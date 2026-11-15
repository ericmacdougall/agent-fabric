package main

import (
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"

	"fabric/internal/checkout"
	"fabric/internal/dispatch"
	"fabric/internal/m1build"
	"fabric/internal/mission"
)

// m1GitPath is the git fabd runs in the operator's mirror (AC-23).
const m1GitPath = "/usr/bin/git"

// m1MissionOptions are the fab daemon inputs of the M1 mission path.
type m1MissionOptions struct {
	enabled        bool // m1build.Enabled in production
	workRoot       string
	missionsRoot   string
	fabSecretGroup string
	workerUser     string
	profile        string
	hooksDir       string
	self           string // the running fab binary (the Bash gate's pin)
}

// m1MissionPreparer builds the scheduler's mission path for an M1 build.
// A build without M1 returns nil: it never reads mission.json (AC-22). An
// M1 build refuses to start when the path cannot be built safely.
func m1MissionPreparer(o m1MissionOptions) (*mission.Preparer, error) {
	if !o.enabled {
		return nil, nil
	}
	refuse := func(format string, a ...any) error {
		return fmt.Errorf("refusing to start the M1 mission path (goodqa#15966): "+format, a...)
	}
	if o.profile != "P0" {
		return nil, refuse("--profile must be P0 (read-only worktree), not %q", o.profile)
	}
	if o.workerUser == "" {
		return nil, refuse("--worker-user (FABRIC_WORKER_USER) is required")
	}
	if o.hooksDir == "" || !filepath.IsAbs(o.hooksDir) {
		return nil, refuse("FABRIC_HOOKS_DIR must be an absolute path holding %s", mission.BashGateName)
	}
	who, err := m1WorkerPrincipals(o.workerUser)
	if err != nil {
		return nil, err
	}
	grp, err := user.LookupGroup(o.fabSecretGroup)
	if err != nil {
		return nil, refuse("group %q: %v", o.fabSecretGroup, err)
	}
	gid, err := strconv.ParseUint(grp.Gid, 10, 32)
	if err != nil {
		return nil, refuse("group id %q", grp.Gid)
	}
	self, err := filepath.EvalSymlinks(o.self)
	if err != nil {
		return nil, refuse("the running fab binary cannot be resolved: %v", err)
	}
	pin, err := dispatch.FileSHA256(self)
	if err != nil {
		return nil, refuse("the running fab binary cannot be hashed: %v", err)
	}
	if fi, err := os.Stat(m1GitPath); err != nil || !fi.Mode().IsRegular() {
		return nil, refuse("%s is not a regular file", m1GitPath)
	}
	p := &mission.Preparer{
		Loader:      mission.Loader{Root: o.missionsRoot, Stat: checkout.OS{}, Who: who, FabSecretGID: uint32(gid)},
		Git:         m1GitPath,
		AttemptRoot: filepath.Join(o.workRoot, "attempts"),
		HooksDir:    o.hooksDir,
		GateSHA:     pin,
	}
	if err := os.Mkdir(p.AttemptRoot, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, refuse("attempt root: %v", err)
	}
	if err := checkout.CheckAncestors(checkout.OS{}, who, p.AttemptRoot, "attempt root"); err != nil {
		return nil, refuse("%v", err)
	}
	if err := mission.VerifyGate(checkout.OS{}, who, p.HooksDir, p.GateSHA); err != nil {
		return nil, refuse("%v", err)
	}
	return p, nil
}

// bashGateMain runs when fab is invoked as fab-bash-gate (a hook command
// takes no arguments, so the name selects it).
func bashGateMain() int { return mission.RunBashGate(os.Stdin, os.Stderr) }

// m1Enabled is the build fact the daemon wiring reads.
var m1Enabled = m1build.Enabled

// m1ExtraRO are read roots the Linux worker profiles add on the M1 path:
// the attempt root (each attempt's settings, brief copy and worktree; all
// operator-owned and read-only) and the hooks dir (the Bash gate).
var m1ExtraRO []string

func m1ReadRoots(workRoot, hooksDir string) []string {
	out := []string{filepath.Join(filepath.Clean(workRoot), "attempts")}
	if hooksDir != "" && filepath.IsAbs(hooksDir) {
		out = append(out, filepath.Clean(hooksDir))
	}
	return out
}

// m1WorkerPrincipals resolves fabd's uid and the worker account's uid and
// groups, the principals every M1 ownership check uses.
func m1WorkerPrincipals(workerUser string) (checkout.Principals, error) {
	refuse := func(format string, a ...any) (checkout.Principals, error) {
		return checkout.Principals{}, fmt.Errorf("refusing to start the M1 mission path (goodqa#15966): "+format, a...)
	}
	if workerUser == "" {
		return refuse("--worker-user (FABRIC_WORKER_USER) is required")
	}
	wu, err := user.Lookup(workerUser)
	if err != nil {
		return refuse("worker account %q: %v", workerUser, err)
	}
	uid, err := strconv.ParseUint(wu.Uid, 10, 32)
	if err != nil {
		return refuse("worker account uid %q", wu.Uid)
	}
	gids, err := wu.GroupIds()
	if err != nil {
		return refuse("worker account groups: %v", err)
	}
	who := checkout.Principals{OperatorUID: uint32(os.Getuid()), WorkerUID: uint32(uid)}
	for _, g := range gids {
		n, err := strconv.ParseUint(g, 10, 32)
		if err != nil {
			return refuse("worker group id %q", g)
		}
		who.WorkerGIDs = append(who.WorkerGIDs, uint32(n))
	}
	if err := who.Validate(); err != nil {
		return refuse("%v", err)
	}
	return who, nil
}
