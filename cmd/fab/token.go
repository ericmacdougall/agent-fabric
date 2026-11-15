package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/user"
	"strconv"

	"fabric/internal/credential"
	"fabric/internal/credential/sweep"
)

// tokenMain is `fab provider token rotate` (goodqa#15966 AC-18). The new
// token arrives on stdin, never in argv or the environment, and only its
// length and an 8-hex sha256 prefix are printed.
func tokenMain(args []string) error {
	if len(args) == 0 || args[0] != "rotate" {
		return errors.New("provider token rotate --dir /srv/fabric/secret --bindings DIR --worker-user fab-worker --group fab-secret < token")
	}
	fs := flag.NewFlagSet("provider token rotate", flag.ContinueOnError)
	dir := fs.String("dir", "/srv/fabric/secret", "secret directory")
	bindings := fs.String("bindings", "", "fabd bindings directory ({run area}/bindings)")
	worker := fs.String("worker-user", "fab-worker", "worker account")
	group := fs.String("group", "fab-secret", "group of the token files")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *bindings == "" {
		return errors.New("provider token rotate: --bindings is required")
	}
	p, err := secretPolicy(*dir, *worker, *group)
	if err != nil {
		return err
	}
	value, err := readTokenStdin()
	if err != nil {
		return err
	}
	n, err := p.Rotate(value, *bindings)
	if err != nil {
		return err
	}
	fmt.Printf("rotated to %s len=%d sha256=%s\n", credential.VersionName(n), len(value), shaPrefix(value))
	return nil
}

func secretPolicy(dir, worker, group string) (credential.Policy, error) {
	u, err := user.Lookup(worker)
	if err != nil {
		return credential.Policy{}, fmt.Errorf("worker user: %w", err)
	}
	uid, _ := strconv.Atoi(u.Uid)
	var gids []int
	gs, err := u.GroupIds()
	if err != nil {
		return credential.Policy{}, fmt.Errorf("worker groups: %w", err)
	}
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

func readTokenStdin() ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(os.Stdin, credential.MaxTokenLen+2))
	if err != nil {
		return nil, err
	}
	b = bytes.TrimSuffix(b, []byte("\n"))
	if err := credential.CheckTokenValue(b); err != nil {
		return nil, err
	}
	return b, nil
}

func shaPrefix(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:4])
}

// sweepMain is `fab __sweep -- ROOT...` (AC-16), run as fab-worker through
// the worker manager. Its input is stdin; it prints only the count report.
func sweepMain(args []string) error {
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	}
	in, err := io.ReadAll(io.LimitReader(os.Stdin, credential.SweepMaxInput+1))
	if err != nil {
		in = nil
	}
	rep := sweep.Run(in, args, sweep.DefaultLimits, nil)
	return json.NewEncoder(os.Stdout).Encode(rep)
}
