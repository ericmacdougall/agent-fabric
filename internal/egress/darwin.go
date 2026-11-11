package egress

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// DarwinWorkerProfile compiles only the worker's network boundary. It must
// wrap the native provider and its descendants, never the mediator. This does
// not provide a filesystem/process containment claim.
func DarwinWorkerProfile(p PolicyDescriptor) (string, error) {
	if p.Version != 1 || !p.DefaultDeny || len(p.WorkerRules) != 1 {
		return "", errors.New("egress: invalid worker policy")
	}
	r := p.WorkerRules[0]
	u, err := url.Parse(p.ProxyURL)
	if err != nil || r.Action != "allow" || r.Protocol != "tcp" || r.Address != "127.0.0.1" || r.Port == 0 || r.Host != "" || u.Scheme != "http" || u.Host != "127.0.0.1:"+strconv.Itoa(int(r.Port)) || u.Path != "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("egress: invalid loopback proxy policy")
	}
	// Apple's native compiler accepts only localhost or * in this filter.
	// tcp4 keeps the compiler alias restricted to the numeric IPv4 listener;
	// it must not admit an unrelated IPv6 listener sharing the same port.
	return fmt.Sprintf("(version 1)\n(allow default)\n(deny network*)\n(allow network-outbound (remote tcp4 \"localhost:%d\"))\n", r.Port), nil
}

// DarwinCodexHistory prevents native startup backfill from reading old rollout
// contents. Metadata validation never enumerates or opens either rollout root.
// Unsupported home/root aliases refuse before a native process is started.
func DarwinCodexHistory(profile, home string) (string, error) {
	if !strings.HasPrefix(profile, "(version 1)\n") || !filepath.IsAbs(home) || filepath.Clean(home) != home || home == "/" || strings.ContainsAny(home, "\x00\r\n\"\\") {
		return "", errors.New("egress: invalid Codex history boundary")
	}
	resolved, err := filepath.EvalSymlinks(home)
	if err != nil || resolved != home {
		return "", errors.New("egress: aliased Codex home refused")
	}
	for _, path := range []string{home, filepath.Join(home, ".codex"), filepath.Join(home, ".codex/sessions"), filepath.Join(home, ".codex/archived_sessions")} {
		st, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) && path != home && path != filepath.Join(home, ".codex") {
			continue // Future roots are covered by the same inherited deny rule.
		}
		if err != nil || !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("egress: unsupported Codex history layout")
		}
		owner, ok := st.Sys().(*syscall.Stat_t)
		if !ok || owner.Uid != uint32(os.Getuid()) {
			return "", errors.New("egress: Codex history owner mismatch")
		}
	}
	for _, suffix := range []string{"sessions", "archived_sessions"} {
		profile += fmt.Sprintf("(deny file-read* (subpath %q))\n", filepath.Join(home, ".codex", suffix))
	}
	return profile, nil
}

// DarwinPreserveLogs prevents this native client and its descendants from
// unlinking or renaming away existing transcript/log/semantic-WAL entries.
// It deliberately leaves credential refresh paths and new artifact writes
// alone; it is not a general filesystem containment policy.
func DarwinPreserveLogs(profile, home string) (string, error) {
	if !filepath.IsAbs(home) || filepath.Clean(home) != home || home == "/" || strings.ContainsAny(home, "\x00\r\n\"\\") {
		return "", errors.New("egress: invalid log home")
	}
	if !strings.HasPrefix(profile, "(version 1)\n") {
		return "", errors.New("egress: invalid native profile")
	}
	for _, suffix := range []string{".claude/projects", ".claude/wals", ".claude/debug", ".claude/history.jsonl", ".codex/sessions", ".codex/log", ".codex/logs_2.sqlite", ".agent-harness/codex-wals", ".agent-harness/logs"} {
		profile += fmt.Sprintf("(deny file-write-unlink (subpath %q))\n", filepath.Join(home, suffix))
	}
	// Codex's immediate maintenance deletes SQL rows without unlinking the
	// database. Protect both existing current-log subtree and the legacy root
	// database, including WAL/SHM/rollback journals, against ALL writes. New
	// session SQLite/log directories are separate, fresh and outside these roots.
	for _, suffix := range []string{".codex/log", ".codex/logs_2.sqlite", ".codex/logs_2.sqlite-wal", ".codex/logs_2.sqlite-shm", ".codex/logs_2.sqlite-journal"} {
		profile += fmt.Sprintf("(deny file-write* (subpath %q))\n", filepath.Join(home, suffix))
	}
	return profile, nil
}
