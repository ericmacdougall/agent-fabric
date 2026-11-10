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
	"os/exec"
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

// runCodexPreflight stops after feature and permission-profile metadata. It
// requests neither full configuration nor thread/model execution.
func runCodexPreflight(ctx context.Context, bin, dir string, wait time.Duration, sink harness.Sink, disabled []codex.DisabledIntegration) error {
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
	argv, err := codex.FileEditCommand(bin, profile, dir, disabled...)
	if err != nil {
		return err
	}
	env := map[string]string{"PATH": "/usr/bin:/bin", "HOME": home, "LANG": "C.UTF-8", "LC_ALL": "C", "SHELL": "/bin/sh", "PYTHONNOUSERSITE": "1", "PYTHONDONTWRITEBYTECODE": "1"}
	featureCtx, featureCancel := context.WithTimeout(ctx, 10*time.Second)
	defer featureCancel()
	featureArgs := append(append([]string(nil), argv[1:len(argv)-1]...), "features", "list")
	featureCmd := exec.CommandContext(featureCtx, bin, featureArgs...)
	featureCmd.Dir = dir
	featureCmd.Env = []string{}
	for k, v := range env {
		featureCmd.Env = append(featureCmd.Env, k+"="+v)
	}
	metadata, err := featureCmd.Output()
	if err != nil {
		return errors.New("codex: feature metadata unavailable")
	}
	if err := codex.CheckFileEditFeatures(metadata); err != nil {
		return err
	}
	a := &codex.Adapter{Command: argv, BaseEnv: env, Control: &codex.Control{Profile: profile, InstructionSources: []string{source}, PreflightOnly: true, CatalogOnly: true}, MaxLine: 1 << 20, MaxTotal: 8 << 20, RequestTimeout: 10 * time.Second, KillGrace: 2 * time.Second}
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
	if final.Outcome != harness.OutcomeStopped || final.Reason != "controlled profile metadata checked; no thread or model turn" {
		return errors.New("codex: controlled preflight refused; no model turn")
	}
	fmt.Println("Codex feature and permission-profile metadata checked; thread and model execution remain gated")
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

// Restrictive registration flags cannot grant tools or replace commands/env.
type codexDisabledIDs struct {
	Kind   string
	Values []codex.DisabledIntegration
}

func (v *codexDisabledIDs) String() string { return "disabled registration IDs" }
func (v *codexDisabledIDs) Set(id string) error {
	if id == "" || len(id) > 200 || strings.ContainsAny(id, "\x00\n\r") {
		return errors.New("invalid Codex registration ID")
	}
	v.Values = append(v.Values, codex.DisabledIntegration{Kind: v.Kind, ID: id})
	return nil
}
