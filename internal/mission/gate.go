package mission

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"fabric/internal/checkout"
)

// VerifyGate checks the Bash gate executable before a settings file that
// names it is written: hooksDir/fab-bash-gate must be a regular file, not a
// symlink, owned by the operator or root, not writable by the worker, in a
// directory whose every ancestor passes checkout.CheckAncestors, and its
// sha256 must equal wantSHA, the running sealed fab binary's.
func VerifyGate(st checkout.Stater, who checkout.Principals, hooksDir, wantSHA string) error {
	if len(wantSHA) != 64 {
		return fmt.Errorf("%w: no pinned fab sha for the Bash gate", ErrConfig)
	}
	if err := checkout.CheckAncestors(st, who, hooksDir, "hooks dir"); err != nil {
		return fmt.Errorf("%w: Bash gate: %v", ErrConfig, err)
	}
	p := filepath.Join(hooksDir, BashGateName)
	i, err := st.Lstat(p)
	if err != nil {
		return fmt.Errorf("%w: Bash gate cannot be checked", ErrConfig)
	}
	if !i.Mode.IsRegular() {
		return fmt.Errorf("%w: Bash gate is not a regular file (a symlink is refused)", ErrConfig)
	}
	if err := who.CheckOwned(i, "Bash gate"); err != nil {
		return fmt.Errorf("%w: %v", ErrConfig, err)
	}
	f, err := os.OpenFile(p, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return fmt.Errorf("%w: Bash gate cannot be opened", ErrConfig)
	}
	defer f.Close()
	fi, err := st.Fstat(f)
	if err != nil || !fi.Mode.IsRegular() {
		return fmt.Errorf("%w: Bash gate is not a regular file", ErrConfig)
	}
	if err := who.CheckOwned(fi, "Bash gate"); err != nil {
		return fmt.Errorf("%w: %v", ErrConfig, err)
	}
	if fi.Mode.Perm()&0o111 == 0 {
		return fmt.Errorf("%w: Bash gate is not executable", ErrConfig)
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return fmt.Errorf("%w: Bash gate cannot be read", ErrConfig)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != wantSHA {
		return fmt.Errorf("%w: Bash gate is not the sealed fab binary (sha mismatch)", ErrConfig)
	}
	return nil
}
