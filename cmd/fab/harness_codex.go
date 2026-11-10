package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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

// runCodexFileEdit drives one fixed job. The explicit run flag is an operator
// request, not a security waiver: every live metadata gate remains mandatory.
func runCodexFileEdit(ctx context.Context, bin, dir string, wait time.Duration, sink harness.Sink, preflight bool, mcPath string) (result error) {
	if runtime.GOOS != "darwin" || bin != codexDemoBinary {
		return errors.New("codex: reviewed Darwin executable required")
	}
	bounded, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
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
	checkCtx, checkCancel := context.WithTimeout(bounded, 10*time.Second)
	defer checkCancel()
	disabled, source, err := codex.AuditLocalInputs(checkCtx, home, dir)
	if err != nil {
		return err
	}
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
	env := map[string]string{"PATH": "/usr/bin:/bin", "HOME": home, "LANG": "C.UTF-8", "LC_ALL": "C", "SHELL": "/bin/sh", "PYTHONNOUSERSITE": "1", "PYTHONDONTWRITEBYTECODE": "1", "RUST_LOG": "off"}
	featureArgs := append(append([]string(nil), argv[1:len(argv)-1]...), "features", "list")
	featureCmd := exec.CommandContext(checkCtx, bin, featureArgs...)
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
	// The installed API reports a provider ID but cannot attest the endpoint
	// behind managed provider overrides without a full secret-bearing response.
	// Do not turn an operator flag into permission to guess the route.
	if !preflight {
		return errors.New("codex: effective first-party provider route is not attested; model execution refused")
	}
	var out harness.Sink = &boundedCodexSink{next: sink}
	if mcPath != "" {
		mc, err := os.OpenFile(mcPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return errors.New("codex: lifecycle output unavailable")
		}
		defer func() { result = errors.Join(result, mc.Sync(), mc.Close()) }()
		out, err = codex.NewLifecycleSink(mc, out, id)
		if err != nil {
			return err
		}
	}
	a := &codex.Adapter{Command: argv, BaseEnv: env, Control: &codex.Control{Profile: profile, InstructionSources: []string{source}, PreflightOnly: preflight, CatalogOnly: preflight, Home: home}, MaxLine: 1 << 20, MaxTotal: 8 << 20, RequestTimeout: 10 * time.Second, KillGrace: 2 * time.Second}
	prompt := ""
	if !preflight {
		prompt = codex.DemoPrompt
	}
	session, err := a.Start(bounded, harness.StartSpec{SessionID: id, Dir: dir, Prompt: prompt}, out)
	if err != nil {
		return err
	}
	final, err := session.Wait(bounded)
	if err != nil {
		cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		return errors.Join(errors.New("codex: run interrupted; retain owned job evidence; hook cleanup may be uncertain"), session.Stop(cleanup))
	}
	if preflight {
		if final.Outcome != harness.OutcomeStopped || final.Reason != "controlled profile metadata checked; no thread or model turn" {
			return errors.New("codex: controlled preflight refused; no model turn")
		}
		fmt.Println("Codex feature, trusted-hook and permission-profile metadata checked; no thread or model turn")
		return nil
	}
	if final.Outcome != harness.OutcomeDone {
		return errors.New("codex: restricted provider job failed; no model retry or fallback")
	}
	if err := checkCodexArtifact(dir); err != nil {
		return err
	}
	fmt.Printf("Codex owned file verified: session=%s artifact=%s model=%s\n", id, codex.DemoArtifact, codex.DemoModel)
	return nil
}

type boundedCodexSink struct {
	next  harness.Sink
	bytes int
}

func (s *boundedCodexSink) Emit(ctx context.Context, e harness.Event) error {
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	if len(b)+1 > (8<<20)-s.bytes {
		return errors.New("codex: canonical event output limit")
	}
	s.bytes += len(b) + 1
	return s.next.Emit(ctx, e)
}
func checkCodexArtifact(dir string) error {
	files, err := os.ReadDir(dir)
	if err != nil || len(files) != 1 || files[0].Name() != codex.DemoArtifact {
		return errors.New("codex: owned artifact footprint mismatch")
	}
	p := filepath.Join(dir, codex.DemoArtifact)
	st, err := os.Lstat(p)
	if err != nil || !st.Mode().IsRegular() || st.Size() != 7 || st.Mode().Perm()&0111 != 0 {
		return errors.New("codex: artifact type or size mismatch")
	}
	b, err := os.ReadFile(p)
	if err != nil || string(b) != "fabric\n" {
		return errors.New("codex: artifact content mismatch")
	}
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
