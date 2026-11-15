package mission

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"fabric/internal/checkout"
	"fabric/internal/domain"
)

// Settings and file-list names inside an attempt directory.
const (
	SettingsFile = "settings.json"
	FilesFile    = "files.json"
	BriefFile    = "brief.md"
	WorktreeDir  = "wt"
	maxBrief     = 1 << 20
)

// Preparer is fabd's M1 mission path: it loads a mission's config before
// START and builds each attempt's directory after it. The attempt
// directory, AttemptRoot/{attempt}, is operator-owned and never inside the
// run directory the worker shares; it holds the worktree, the settings file
// and the archive's file list.
type Preparer struct {
	Loader      Loader
	Git         string // absolute path of git
	AttemptRoot string
	HooksDir    string
	GateSHA     string // sha256 of the running sealed fab binary
}

// Prepared is one attempt's directory.
type Prepared struct {
	Dir, Worktree, Settings string
	// Brief is the attempt's copy of the mission brief: the worker's
	// sandbox reads the attempt directory, not the missions root.
	Brief string
	Files []string // from the archive stream (W2 AC-10)
}

// Load is Loader.Load: the pre-START check.
func (p *Preparer) Load(m domain.MissionID) (*Config, error) { return p.Loader.Load(m) }

// Prepare builds AttemptRoot/{attempt}. On any error nothing is left.
func (p *Preparer) Prepare(ctx context.Context, c *Config, attempt domain.AttemptID) (out Prepared, err error) {
	if _, err := domain.ParseAttemptID(string(attempt)); err != nil || strings.ContainsAny(string(attempt), "/\\") {
		return Prepared{}, fmt.Errorf("%w: attempt id is not a single path component", ErrConfig)
	}
	st, who := p.Loader.Stat, p.Loader.Who
	if err := checkout.CheckAncestors(st, who, p.AttemptRoot, "attempt root"); err != nil {
		return Prepared{}, err
	}
	if err := VerifyGate(st, who, p.HooksDir, p.GateSHA); err != nil {
		return Prepared{}, err
	}
	dir := filepath.Join(p.AttemptRoot, string(attempt))
	if err := os.Mkdir(dir, 0o700); err != nil {
		return Prepared{}, fmt.Errorf("%w: attempt directory not created fresh: %v", checkout.ErrCheckout, err)
	}
	defer func() {
		if err != nil {
			checkout.RemoveTree(dir) //nolint:errcheck // best effort; the launch is refused anyway
		}
	}()
	out = Prepared{Dir: dir, Worktree: filepath.Join(dir, WorktreeDir), Settings: filepath.Join(dir, SettingsFile), Brief: filepath.Join(dir, BriefFile)}
	brief, err := readBrief(c.Brief())
	if err != nil {
		return Prepared{}, err
	}
	if err := writeReadOnly(out.Brief, brief); err != nil {
		return Prepared{}, err
	}
	x := checkout.Extractor{Git: p.Git, Stat: st, Who: who}
	res, err := x.Extract(ctx, c.Repo(), c.SHA(), out.Worktree)
	if err != nil {
		return Prepared{}, err
	}
	out.Files = res.Files
	doc, err := Settings(out.Worktree, p.HooksDir)
	if err != nil {
		return Prepared{}, err
	}
	if err := writeReadOnly(out.Settings, doc); err != nil {
		return Prepared{}, err
	}
	list, err := json.Marshal(res.Files)
	if err != nil {
		return Prepared{}, err
	}
	if err := writeReadOnly(filepath.Join(dir, FilesFile), append(list, '\n')); err != nil {
		return Prepared{}, err
	}
	// Traversable and readable by the worker, writable by nobody.
	if err := os.Chmod(dir, 0o555); err != nil {
		return Prepared{}, err
	}
	if err := checkout.CheckAncestors(st, who, dir, "attempt directory"); err != nil {
		return Prepared{}, err
	}
	return out, nil
}

// readBrief reads the brief Load checked, without following a symlink.
func readBrief(p string) ([]byte, error) {
	f, err := os.OpenFile(p, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("%w: brief cannot be opened", ErrConfig)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: brief is not a regular file", ErrConfig)
	}
	b, err := io.ReadAll(io.LimitReader(f, maxBrief+1))
	if err != nil || len(b) > maxBrief || len(b) == 0 {
		return nil, fmt.Errorf("%w: brief is empty, unreadable or larger than %d bytes", ErrConfig, maxBrief)
	}
	return b, nil
}

// Class is a fixed, value-free name for a Prepare or Load error: the text
// an event or a refusal log may carry (AC-22: config values never reach
// events).
func Class(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, checkout.ErrOwnership):
		return "m1_ownership_refused"
	case errors.Is(err, checkout.ErrCheckout):
		return "m1_checkout_refused"
	case errors.Is(err, ErrConfig):
		return "m1_config_refused"
	}
	return "m1_prepare_failed"
}

func writeReadOnly(path string, b []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o400)
	if err != nil {
		return fmt.Errorf("%w: %s: %v", checkout.ErrCheckout, filepath.Base(path), err)
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Chmod(path, 0o444)
}
