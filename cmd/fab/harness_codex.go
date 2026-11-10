package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"fabric/internal/harness"
	"fabric/internal/harness/codex"
)

const codexDemoBinarySHA = "a636331ed87605efc4cf84028ac2884a75363f9d10ead63cffb57ea0016d486e"
const codexDemoBinary = "/Applications/ChatGPT.app/Contents/Resources/codex-cli/CodexCLI.app/Contents/MacOS/codex"

// runCodexPreflight cannot call a model: the adapter stops after config and
// thread permission checks. Real inference is a separate reviewed runtime gate.
func runCodexPreflight(ctx context.Context, bin, dir string, wait time.Duration, sink harness.Sink) error {
	if runtime.GOOS != "darwin" {
		return errors.New("codex: reviewed runtime is Darwin only")
	}
	if bin != codexDemoBinary {
		return errors.New("codex: executable is not the reviewed runtime")
	}
	f, err := os.Open(bin)
	if err != nil {
		return errors.New("codex: runtime unavailable")
	}
	h := sha256.New()
	_, err = io.Copy(h, io.LimitReader(f, 300<<20))
	_ = f.Close()
	if err != nil || hex.EncodeToString(h.Sum(nil)) != codexDemoBinarySHA {
		return errors.New("codex: runtime identity mismatch")
	}
	if err := ownedCodexDir(dir); err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return errors.New("codex: home unavailable")
	}
	source := filepath.Join(home, ".codex", "AGENTS.md")
	if st, e := os.Lstat(source); e != nil || !st.Mode().IsRegular() {
		return errors.New("codex: required user instructions unavailable")
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	id := hex.EncodeToString(nonce[:])
	profile := "fabric_demo_" + id
	argv, err := codex.FileEditCommand(bin, profile, dir)
	if err != nil {
		return err
	}
	a := &codex.Adapter{Command: argv, BaseEnv: map[string]string{"PATH": "/usr/bin:/bin", "HOME": home, "LANG": "C.UTF-8", "LC_ALL": "C"}, Control: &codex.Control{Profile: profile, InstructionSources: []string{source}, PreflightOnly: true}, MaxLine: 1 << 20, MaxTotal: 8 << 20, RequestTimeout: 10 * time.Second, KillGrace: 2 * time.Second}
	s, err := a.Start(ctx, harness.StartSpec{SessionID: id, Dir: dir}, sink)
	if err != nil {
		return err
	}
	bound, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	final, err := s.Wait(bound)
	if err != nil {
		cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		return errors.Join(err, s.Stop(cleanup))
	}
	if final.Outcome != harness.OutcomeStopped || final.Reason != "controlled preflight complete; no model turn" {
		return errors.New("codex: controlled preflight refused; no model turn")
	}
	fmt.Println("Codex controlled preflight complete; model execution remains gated")
	return nil
}

func ownedCodexDir(dir string) error {
	if !filepath.IsAbs(dir) || filepath.Clean(dir) != dir {
		return usageErr("--codex-dir must be an absolute clean path")
	}
	for p := dir; ; p = filepath.Dir(p) {
		st, err := os.Lstat(p)
		if err != nil || !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
			return errors.New("codex: workspace ancestry must contain only real directories")
		}
		if p == dir && (st.Mode().Perm() != 0700 || st.Sys().(*syscall.Stat_t).Uid != uint32(os.Getuid())) {
			return errors.New("codex: workspace must have mode 0700")
		}
		if p == filepath.Dir(p) {
			break
		}
	}
	files, err := os.ReadDir(dir)
	if err != nil || len(files) != 0 {
		return errors.New("codex: preflight requires a new empty owned workspace")
	}
	if strings.ContainsRune(dir, 0) {
		return errors.New("codex: invalid workspace")
	}
	return nil
}
