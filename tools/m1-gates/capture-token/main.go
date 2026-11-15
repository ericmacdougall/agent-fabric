package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/user"
	"strconv"
	"strings"
	"time"

	"fabric/internal/credential"
)

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string, in io.Reader, out io.Writer) error {
	fs := flag.NewFlagSet("capture-token", flag.ContinueOnError)
	claude := fs.String("claude", "claude", "the pinned claude binary")
	dir := fs.String("dir", "/srv/fabric/secret", "secret directory")
	worker := fs.String("worker-user", "fab-worker", "worker account")
	group := fs.String("group", "fab-secret", "group of the token files")
	timeout := fs.Duration("timeout", 10*time.Minute, "how long to wait for sign-in")
	if err := fs.Parse(args); err != nil {
		return err
	}
	p, err := policy(*dir, *worker, *group)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	return capture(ctx, *claude, p, in, out)
}

// capture is the whole acquisition against a policy (tests pass a
// temporary directory and a fake CLI).
func capture(ctx context.Context, claude string, p credential.Policy, in io.Reader, out io.Writer) error {
	s := &scanner{show: func(l string) { fmt.Fprintln(out, l) }}
	if err := runOnPty(ctx, claude, []string{"setup-token"}, cleanEnv(), in, s.Feed); err != nil {
		return fmt.Errorf("capture-token: claude setup-token: %w; nothing written", err)
	}
	tok, err := s.Close()
	if err != nil {
		return err
	}
	n, err := p.Capture([]byte(tok))
	if err != nil {
		return err
	}
	sum := sha256.Sum256([]byte(tok))
	fmt.Fprintf(out, "captured %s len=%d sha256=%s\n", credential.VersionName(n), len(tok), hex.EncodeToString(sum[:4]))
	return nil
}

// cleanEnv is the caller's environment without any credential variable.
func cleanEnv() []string {
	var env []string
	for _, e := range os.Environ() {
		name, _, _ := strings.Cut(e, "=")
		switch name {
		case "ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN", "FABRIC_DSN", "COLUMNS", "LINES", "TERM":
			continue
		}
		env = append(env, e)
	}
	return append(env, "TERM=xterm-256color", "COLUMNS=200", "LINES=50")
}

func policy(dir, worker, group string) (credential.Policy, error) {
	u, err := user.Lookup(worker)
	if err != nil {
		return credential.Policy{}, fmt.Errorf("worker user: %w", err)
	}
	uid, _ := strconv.Atoi(u.Uid)
	gs, err := u.GroupIds()
	if err != nil {
		return credential.Policy{}, err
	}
	var gids []int
	for _, g := range gs {
		n, _ := strconv.Atoi(g)
		gids = append(gids, n)
	}
	g, err := user.LookupGroup(group)
	if err != nil {
		return credential.Policy{}, fmt.Errorf("token group: %w", err)
	}
	gid, _ := strconv.Atoi(g.Gid)
	return credential.Policy{Dir: dir, WorkerUID: uid, WorkerGIDs: gids, OwnerUID: os.Geteuid(), GroupGID: gid}, nil
}

var errNoPty = errors.New("a pty is supported on Linux only")
