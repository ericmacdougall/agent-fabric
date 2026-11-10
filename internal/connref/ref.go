// Package connref loads database credentials inside the client. File references
// produce connection strings with exact-match passfiles rather than passwords.
// A caller owns each Ref until every pool, child and deferred cleanup using it
// has finished. Matching a passfile record is not database authorization.
package connref

import (
	"errors"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
)

type Purpose uint8

const (
	App Purpose = iota
	Runner
	TestAdmin
	Review
)

var configName = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,127}$`)
var ErrUnsupported = errors.New("database credential file references are not supported on this platform")
var errReference = errors.New("database credential reference is invalid or unavailable")
var errClosed = errors.New("database credential reference is closed")

// Ref is an owned connection reference. DSN returns a borrowed string: Close
// must not race with use of that string by a connection, pool or child.
// ForDatabase and Close themselves are safe to call concurrently.
type Ref struct {
	purpose  Purpose
	mu       sync.Mutex
	closed   bool
	dsn      string
	file     *os.File
	material *material
}
type material struct {
	purpose  Purpose
	base     url.URL
	password string
	owners   atomic.Int64
}

func (p Purpose) field() string {
	switch p {
	case App:
		return "FABRIC_DSN"
	case Runner:
		return "FABRIC_RUNNER_DSN"
	case TestAdmin:
		return "FABRIC_TEST_DSN"
	case Review:
		return "FABRIC_REVIEW_DSN"
	}
	return ""
}

// HasReferenceConfig distinguishes an explicitly configured empty reference
// from an absent mode. Optional tests and compatibility paths must use presence.
func HasReferenceConfig(name string) bool {
	for _, suffix := range []string{"_FILE", "_PASSWORD_FILE", "_FILE_KEY", "_DATABASE"} {
		if _, set := os.LookupEnv(name + suffix); set {
			return true
		}
	}
	return false
}

// ResolveEnv accepts one source mode. Any explicit reference configuration
// is presence-aware and cannot fall back to a raw variable after failure.
func ResolveEnv(name string, purpose Purpose) (*Ref, error) {
	if !configName.MatchString(name) || purpose.field() == "" {
		return nil, errReference
	}
	raw, rawSet := os.LookupEnv(name)
	path, fileSet := os.LookupEnv(name + "_FILE")
	passwordPath, passwordSet := os.LookupEnv(name + "_PASSWORD_FILE")
	key, keySet := os.LookupEnv(name + "_FILE_KEY")
	database, targetSet := os.LookupEnv(name + "_DATABASE")
	if !fileSet && !passwordSet && !keySet && !targetSet {
		return &Ref{dsn: raw, purpose: purpose}, nil // pre-existing raw-variable compatibility only
	}
	// Explicitly empty modes never become legacy raw inputs. A key is only
	// meaningful with a full DSN file; password mode takes nonsecret metadata.
	if (targetSet && ((!fileSet && !passwordSet) || !validField(database) || len(database) > 63)) ||
		(fileSet && (path == "" || rawSet || passwordSet)) ||
		(passwordSet && (passwordPath == "" || !rawSet || raw == "" || fileSet || keySet)) ||
		(keySet && (!fileSet || key == "" || key != purpose.field())) {
		return nil, errReference
	}
	for _, pair := range os.Environ() {
		envName, _, _ := strings.Cut(pair, "=")
		if strings.HasPrefix(envName, "PG") {
			return nil, errReference
		}
	}
	if passwordSet {
		// Validate metadata before reading any secret, and never reconstruct a
		// password URL. The exact same parser/static metadata contract is shared.
		m, err := parsePasswordMaterial(raw, "")
		if err != nil {
			return nil, err
		}
		body, err := readProtected(passwordPath)
		if err != nil {
			return nil, err
		}
		password := strings.TrimSpace(string(body)) // matches the roles CLI loader
		if password == "" || strings.ContainsAny(password, "\x00\r\n") {
			return nil, errReference
		}
		m.password = password
		m.purpose = purpose
		ref, err := makeRef(m, strings.TrimPrefix(m.base.Path, "/"))
		if err != nil {
			return nil, err
		}
		return selectedTarget(ref, database, targetSet)
	}
	body, err := readProtected(path)
	if err != nil {
		return nil, err
	}
	dsn, err := sourceDSN(body, key)
	if err != nil {
		return nil, errReference
	}
	ref, err := fromDSN(dsn, purpose)
	if err != nil {
		return nil, err
	}
	return selectedTarget(ref, database, targetSet)
}

// The source reference is never connected. The selected child owns the
// material before its parent is closed, and lives through caller cleanup.
func selectedTarget(ref *Ref, database string, declared bool) (*Ref, error) {
	if !declared {
		return ref, nil
	}
	child, err := ref.ForDatabase(database)
	closeErr := ref.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		child.Close()
		return nil, closeErr
	}
	return child, nil
}

func sourceDSN(body []byte, key string) (string, error) {
	if len(body) == 0 || strings.ContainsAny(string(body), "\x00\r") {
		return "", errReference
	}
	if key == "" {
		return strings.TrimSuffix(string(body), "\n"), nil
	}
	seen := map[string]bool{}
	selected := ""
	for _, line := range strings.Split(string(body), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, value, ok := strings.Cut(line, "=")
		if !ok || !configName.MatchString(name) || seen[name] || strings.ContainsAny(value, "`$\"'\t ") {
			return "", errReference
		}
		seen[name] = true
		if name == key {
			selected = value
		}
	}
	if selected == "" {
		return "", errReference
	}
	return selected, nil
}

func (r *Ref) DSN() (string, error) {
	if r == nil {
		return "", errReference
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return "", errClosed
	}
	return r.dsn, nil
}

// ForDatabase makes a fresh exact record and independently owned descriptor.
// It never edits a returned DSN's path while reusing a differently scoped file.
func (r *Ref) ForDatabase(name string) (*Ref, error) {
	if r == nil || !validField(name) {
		return nil, errReference
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, errClosed
	}
	if r.material != nil {
		return makeRef(r.material, name)
	}
	// Legacy inputs stay unchanged until a caller explicitly requests derivation.
	// Parse inside the client, without reconstructing a password-bearing URL.
	m, err := parseMaterial(r.dsn)
	if err != nil {
		return nil, err
	}
	m.purpose = r.purpose
	return makeRef(m, name)
}

// Close is terminal even if the underlying close reports an error. Never retry
// a released integer FD: it could already identify an unrelated resource.
func (r *Ref) Close() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	r.closed = true
	r.dsn = ""
	var err error
	if r.file != nil {
		err = r.file.Close()
		r.file = nil
	}
	if r.material != nil {
		if r.material.owners.Add(-1) == 0 {
			r.material.password = ""
		}
		r.material = nil
	}
	if err != nil {
		return errors.New("database credential reference close failed")
	}
	return nil
}

func validField(s string) bool {
	return s != "" && len(s) <= 1024 && !strings.ContainsAny(s, "*\x00\r\n")
}

func fromDSN(dsn string, purpose Purpose) (*Ref, error) {
	m, err := parseMaterial(dsn)
	if err != nil {
		return nil, err
	}
	m.purpose = purpose
	return makeRef(m, strings.TrimPrefix(m.base.Path, "/"))
}
