//go:build !linux

package connref

import (
	"net/url"
	"os"
)

// Fixture preserves only the pre-existing non-Linux raw-env test behavior.
// New FILE inputs never fall back to a password-bearing URL on this platform.
type Fixture struct{ ref *Ref }

func NewFixture(base string, purpose Purpose, user, password, _ string) (*Fixture, error) {
	if os.Getenv("FABRIC_TEST_DSN_FILE") != "" || purpose.field() == "" {
		return nil, ErrUnsupported
	}
	u, err := url.Parse(base)
	if err != nil || u.Query().Get("passfile") != "" {
		return nil, ErrUnsupported
	}
	u.User = url.UserPassword(user, password) // existing raw-env non-Linux compatibility only
	q := u.Query()
	for _, key := range []string{"role", "session_authorization", "options", "user", "password"} {
		q.Del(key)
	}
	u.RawQuery = q.Encode()
	return &Fixture{ref: &Ref{dsn: u.String(), purpose: purpose}}, nil
}
func (f *Fixture) DSN() (string, error) {
	if f == nil {
		return "", errReference
	}
	return f.ref.DSN()
}
func (f *Fixture) Close() error {
	if f == nil {
		return nil
	}
	return f.ref.Close()
}

// ForTestDatabase preserves URL path derivation only for pre-existing raw-env
// tests. FILE mode errors in ResolveEnv and must never fall back here.
func ForTestDatabase(ref *Ref, database string) (*Ref, error) {
	if os.Getenv("FABRIC_TEST_DSN_FILE") != "" {
		return nil, ErrUnsupported
	}
	dsn, err := ref.DSN()
	if err != nil {
		return nil, err
	}
	u, err := url.Parse(dsn)
	if err != nil || u.Query().Get("passfile") != "" {
		return nil, ErrUnsupported
	}
	u.Path = "/" + database
	u.RawPath = ""
	return &Ref{dsn: u.String(), purpose: TestAdmin}, nil
}
