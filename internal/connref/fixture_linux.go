//go:build linux

package connref

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/sys/unix"
)

// Fixture owns a disposable test credential file. Operational credentials must
// use ResolveEnv's protected sources and sealed RAM transport instead. A caller
// must close every pool/child using the borrowed DSN before closing the fixture.
// The caller's private directory cleanup is an additional recovery barrier.
type Fixture struct {
	mu        sync.Mutex
	ref       *Ref
	file      *os.File
	directory *os.File
	name      string
	closed    bool
}

// NewFixture replaces only user/authentication for a disposable test role. It
// never constructs a password-bearing URL; the exact credential stays in the
// private passfile. This function does not create or authorize database roles.
func NewFixture(baseDSN string, purpose Purpose, user, password, dir string) (*Fixture, error) {
	if purpose.field() == "" || !validField(user) || strings.ContainsAny(password, "\x00\r\n") {
		return nil, errReference
	}
	u, e := url.Parse(baseDSN)
	if e != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") || u.Fragment != "" || !validField(u.Hostname()) {
		return nil, errReference
	}
	db := strings.TrimPrefix(u.Path, "/")
	if !validField(db) || len(db) > 63 {
		return nil, errReference
	}
	port := u.Port()
	if port == "" {
		port = "5432"
	}
	pn, e := strconv.Atoi(port)
	if e != nil || pn < 1 || pn > 65535 {
		return nil, errReference
	}
	q, e := url.ParseQuery(u.RawQuery)
	if e != nil {
		return nil, errReference
	}
	for key, values := range q {
		if len(values) != 1 {
			return nil, errReference
		}
		switch key {
		case "passfile": // replace the parent's reference; never give it to a worker
			q.Del(key)
		case "sslmode":
			switch values[0] {
			case "disable", "allow", "prefer", "require", "verify-ca", "verify-full":
			default:
				return nil, errReference
			}
		case "connect_timeout":
			n, e := strconv.Atoi(values[0])
			if e != nil || n < 1 || n > 120 {
				return nil, errReference
			}
		case "application_name":
			switch values[0] {
			case "fabd", "fabd-install", "fab-tui":
			default:
				return nil, errReference
			}
		default:
			return nil, errReference
		}
	}
	directory, e := openFixtureDirectory(dir)
	if e != nil {
		return nil, errReference
	}
	keep := false
	var file *os.File
	var name string
	defer func() {
		if !keep {
			if name != "" {
				unix.Unlinkat(int(directory.Fd()), name, 0)
			}
			if file != nil {
				file.Close()
			}
			directory.Close()
		}
	}()
	for attempt := 0; attempt < 8; attempt++ {
		var random [12]byte
		if _, e = rand.Read(random[:]); e != nil {
			return nil, errReference
		}
		name = "credential-" + hex.EncodeToString(random[:])
		fd, e := unix.Openat(int(directory.Fd()), name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
		if e == unix.EEXIST {
			name = ""
			continue
		}
		if e != nil {
			return nil, errReference
		}
		file = os.NewFile(uintptr(fd), "disposable-test-credential")
		break
	}
	if file == nil {
		return nil, errReference
	}
	if e = file.Chmod(0600); e != nil {
		return nil, errReference
	}
	record := fmt.Sprintf("%s:%s:%s:%s:%s\n", pgpassEscape(u.Hostname()), port, pgpassEscape(db), pgpassEscape(user), pgpassEscape(password))
	if n, e := file.WriteString(record); e != nil || n != len(record) {
		return nil, errReference
	}
	u.Host = net.JoinHostPort(u.Hostname(), port)
	u.User = url.User(user)
	u.RawPath = ""
	q.Set("passfile", filepath.Join(dir, name))
	u.RawQuery = q.Encode()
	keep = true
	return &Fixture{ref: &Ref{dsn: u.String(), purpose: purpose}, file: file, directory: directory, name: name}, nil
}

func openFixtureDirectory(path string) (*os.File, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, errReference
	}
	fd, e := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if e != nil {
		return nil, errReference
	}
	for _, part := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
		if part == "" || part == "." || part == ".." {
			unix.Close(fd)
			return nil, errReference
		}
		next, e := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		unix.Close(fd)
		if e != nil {
			return nil, errReference
		}
		fd = next
		var st unix.Stat_t
		if unix.Fstat(fd, &st) != nil || (st.Uid != 0 && st.Uid != uint32(os.Geteuid())) || (st.Mode&0022 != 0 && !(st.Uid == 0 && st.Mode&unix.S_ISVTX != 0)) {
			unix.Close(fd)
			return nil, errReference
		}
	}
	var st unix.Stat_t
	if unix.Fstat(fd, &st) != nil || st.Uid != uint32(os.Geteuid()) || st.Mode&0077 != 0 {
		unix.Close(fd)
		return nil, errReference
	}
	return os.NewFile(uintptr(fd), "disposable-test-credential-directory"), nil
}

func (f *Fixture) DSN() (string, error) {
	if f == nil {
		return "", errReference
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return "", errClosed
	}
	return f.ref.DSN()
}

func (f *Fixture) Close() error {
	if f == nil {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return nil
	}
	f.closed = true
	f.ref.Close()
	e1 := unix.Unlinkat(int(f.directory.Fd()), f.name, 0)
	e2 := f.file.Close()
	e3 := f.directory.Close()
	if (e1 != nil && e1 != unix.ENOENT) || e2 != nil || e3 != nil {
		return errReference
	}
	return nil
}

// ForTestDatabase keeps the platform decision explicit for disposable test
// helpers. Authorized Linux jobs use FILE inputs and exact sealed records.
func ForTestDatabase(ref *Ref, database string) (*Ref, error) { return ref.ForDatabase(database) }
