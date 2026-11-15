package main

import (
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"syscall"

	"fabric/internal/checkout"
	"fabric/internal/credential"
	"fabric/internal/sched"
)

// The fabd-only run area (goodqa#15966 AC-15, AC-18, AC-29). It holds the
// raw and redacted streams, the binding records, the AC-29 records and the
// id key. It is 0700 and fabd-owned, no ancestor is writable by the worker,
// and no worker grant, variable or argument names it.
const (
	runAreaStreams  = "streams"
	runAreaBindings = "bindings"
	runAreaRecords  = "records"
	runAreaIDKey    = "credential-id.key"
	idKeyLen        = 32
)

// m1DefaultRunArea is the run area under the work root. It is a sibling of
// the worker's run directories ({work-root}/run), never inside one.
func m1DefaultRunArea(workRoot string) string { return filepath.Join(workRoot, "run-area") }

// m1RunArea is the checked run area and its parts.
type m1RunArea struct {
	Root, Streams, Bindings, Records string
}

// prepareRunArea creates the run area (0700) and its parts when missing and
// refuses to start when the area is not a fabd-owned 0700 directory or any
// ancestor is writable by the worker account.
func prepareRunArea(root string, who checkout.Principals, st checkout.Stater) (m1RunArea, error) {
	refuse := func(format string, a ...any) (m1RunArea, error) {
		return m1RunArea{}, fmt.Errorf("refusing to start the M1 run area (goodqa#15966): "+format, a...)
	}
	if !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return refuse("--run-area %q is not an absolute clean path", root)
	}
	if err := os.Mkdir(root, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return refuse("%v", err)
	}
	ra := m1RunArea{Root: root, Streams: filepath.Join(root, runAreaStreams), Bindings: filepath.Join(root, runAreaBindings), Records: filepath.Join(root, runAreaRecords)}
	for _, d := range []string{root, ra.Streams, ra.Bindings, ra.Records} {
		if d != root {
			if err := os.Mkdir(d, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
				return refuse("%v", err)
			}
		}
		i, err := st.Lstat(d)
		if err != nil {
			return refuse("%s cannot be checked: %v", d, err)
		}
		if i.Mode&os.ModeSymlink != 0 || !i.Mode.IsDir() {
			return refuse("%s is not a directory", d)
		}
		if i.UID != who.OperatorUID {
			return refuse("%s is owned by uid %d, not fabd's uid %d", d, i.UID, who.OperatorUID)
		}
		if i.Mode.Perm() != 0o700 {
			return refuse("%s has mode %v, want 0700", d, i.Mode.Perm())
		}
	}
	if err := checkout.CheckAncestors(st, who, filepath.Dir(root), "run area parent"); err != nil {
		return refuse("%v", err)
	}
	return ra, nil
}

// loadIDKey reads the 32-byte version-id key (AC-18), creating it once with
// O_EXCL, mode 0600. An existing key must be a fabd-owned 0600 regular file
// of exactly 32 bytes.
func loadIDKey(root string) ([]byte, error) {
	path := filepath.Join(root, runAreaIDKey)
	key := make([]byte, idKeyLen)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err == nil {
		_, err = f.Write(key)
		if err == nil {
			err = f.Sync()
		}
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return nil, fmt.Errorf("id key not written: %w", err)
		}
		return key, nil
	}
	if !errors.Is(err, os.ErrExist) {
		return nil, fmt.Errorf("id key: %w", err)
	}
	f, err = os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fmt.Errorf("id key: %w", err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("id key: %w", err)
	}
	sys, ok := fi.Sys().(*syscall.Stat_t)
	if !fi.Mode().IsRegular() || fi.Mode().Perm() != 0o600 || !ok || int(sys.Uid) != os.Geteuid() || fi.Size() != idKeyLen {
		return nil, errors.New("id key: must be a fabd-owned 0600 regular file of 32 bytes")
	}
	if _, err := io.ReadFull(f, key); err != nil {
		return nil, fmt.Errorf("id key: %w", err)
	}
	return key, nil
}

// m1OthersFunc returns the credentials fabd holds besides the token, read
// fresh at launch and at settle so a mid-attempt change is detected
// (AC-18): the GoodQA key from its existing key file, and the runner DSN
// password when the DSN carries one.
func m1OthersFunc(goodqaKeyFile, runnerDSN string) func() []credential.OtherCredential {
	var runnerPW []byte
	if u, err := url.Parse(runnerDSN); err == nil && u.User != nil {
		if pw, ok := u.User.Password(); ok && pw != "" {
			runnerPW = []byte(pw)
		}
	}
	return func() []credential.OtherCredential {
		var out []credential.OtherCredential
		if b, err := os.ReadFile(goodqaKeyFile); err == nil {
			if v := bytes.TrimSpace(b); len(v) > 0 {
				out = append(out, credential.OtherCredential{Kind: credential.KindGoodQA, Name: "goodqa", Value: v})
			}
		}
		if runnerPW != nil {
			out = append(out, credential.OtherCredential{Kind: credential.KindDSN, Name: "runner", Value: append([]byte(nil), runnerPW...)})
		}
		return out
	}
}

// m1CredentialOptions are the fab daemon inputs of the subscription-token
// launch.
type m1CredentialOptions struct {
	runArea, secretDir, workerUser, secretGroup, providerSHA, goodqaKeyFile, runnerDSN string
	who                                                                                checkout.Principals
}

// m1CredentialLaunch builds the scheduler's credential launch for an M1
// build that dispatches, and the run area it lives in.
func m1CredentialLaunch(o m1CredentialOptions) (*sched.CredentialLaunch, m1RunArea, error) {
	if o.providerSHA == "" {
		return nil, m1RunArea{}, errors.New("refusing to start the M1 token launch (goodqa#15966): --provider-sha256 is required")
	}
	ra, err := prepareRunArea(o.runArea, o.who, checkout.OS{})
	if err != nil {
		return nil, m1RunArea{}, err
	}
	key, err := loadIDKey(ra.Root)
	if err != nil {
		return nil, m1RunArea{}, fmt.Errorf("refusing to start the M1 token launch (goodqa#15966): %w", err)
	}
	pol, err := secretPolicy(o.secretDir, o.workerUser, o.secretGroup)
	if err != nil {
		return nil, m1RunArea{}, fmt.Errorf("refusing to start the M1 token launch (goodqa#15966): %w", err)
	}
	return &sched.CredentialLaunch{Policy: pol, BindingsDir: ra.Bindings, IDKey: key, PinnedSHA: o.providerSHA,
		Others: m1OthersFunc(o.goodqaKeyFile, o.runnerDSN)}, ra, nil
}
