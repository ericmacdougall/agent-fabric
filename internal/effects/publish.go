//go:build unix

package effects

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// Publish is the `publish` executor (AC-12): it moves a remote branch from
// expected_head to new_head with one compare-and-swap push, and never
// rewrites history. It is plain git, with no harness helper, so the fabric
// stands alone.
//
//   - Validate: a strict request, an approval required, the repository
//     inside Root, an explicit remote URL (review 1 on 4d98e31: the request
//     hash and so the approval bind where the push goes, not a remote name
//     that configuration resolves), a plain branch name, full object names.
//   - Every call first binds the repository (review 2): its git dir and
//     common dir are inside Root, it has no alternates, and its effective
//     configuration has no key that could redirect or wrap the transport
//     (url.*, credential.*, includes, askpass, proxies, ssh command).
//   - Precondition: the remote head is read with ls-remote <url>. Anything
//     but expected_head is ErrPreconditionConflict, which the broker records
//     as QUARANTINED with nothing sent. new_head must be a local commit that
//     descends from expected_head (a fast-forward), or the effect fails
//     definitely.
//   - Dispatch: git push --force-with-lease=refs/heads/B:expected_head <url>
//     new_head:refs/heads/B. The lease is the compare-and-swap against a
//     move after the precondition; ancestry is checked again first, so the
//     lease can never rewrite history. Only a porcelain fast-forward line
//     is CONFIRMED; anything else is an error, which the broker records as
//     UNKNOWN, and readback decides.
//   - Reconcile: the remote head is new_head (COMMITTED), expected_head
//     (NOT_APPLIED) or anything else (CONFLICT, so QUARANTINED). After a
//     transport timeout the server may still apply a pack it received
//     (review 5); a later Retry then finds head == new_head and quarantines,
//     the safe side. The stale-dispatch window bounds when readback starts.
//
// git runs through RunCommandEnv, whose process group is stopped at the
// deadline, with no global or system configuration and an allow-listed
// environment (review 3). Helpers that could outlive the group (review nit
// 3 on 11145f4) are off: ssh ControlMaster and ControlPersist, credential
// helpers, askpass, fsmonitor, auto gc and maintenance, repository hooks.
// Only the ssh and https transports are allowed, plus file when AllowLocal.
// Residual: a symlink swapped inside Root between the bind check and a git
// call; Root must be writable only by the fabric and its runner.
type Publish struct {
	Root       string // every repository must be strictly inside it; empty refuses all
	Git        string // the git binary; default "git"
	AllowLocal bool   // allow an absolute-path (file transport) URL; tests only
	KnownHosts string // pinned known_hosts file for ssh URLs
}

type publishRequest struct {
	Repo         string `json:"repo"`
	URL          string `json:"url"`
	Branch       string `json:"branch"`
	ExpectedHead string `json:"expected_head"`
	NewHead      string `json:"new_head"`
}

var (
	objectNameRE = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)
	branchNameRE = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._/-]{0,199}$`)
	scpURLRE     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*@[A-Za-z0-9][A-Za-z0-9.-]*:[A-Za-z0-9._~/-]+$`)
	urlPathRE    = regexp.MustCompile(`^/[A-Za-z0-9._~/-]+$`)
)

// publishSubject is the subject a publish request must carry: one live
// effect per destination branch.
func publishSubject(r publishRequest) string { return "branch:" + r.URL + "#" + r.Branch }

// gitSafety precedes every git command: the -c settings apply last.
var gitSafety = []string{
	"-c", "credential.helper=",
	"-c", "core.askPass=",
	"-c", "core.fsmonitor=false",
	"-c", "core.hooksPath=/dev/null",
	"-c", "gc.auto=0",
	"-c", "maintenance.auto=false",
	"-c", "protocol.allow=never",
	"-c", "protocol.ssh.allow=always",
	"-c", "protocol.https.allow=always",
	"-c", "push.recurseSubmodules=no",
	"-c", "push.followTags=false",
	"-c", "push.gpgSign=false",
	// Review H1 on a5b3ac1: the fast-forward check must read parents from
	// the commit objects (whose hashes cover them), never from a
	// commit-graph file in the runner-writable source objects directory.
	"-c", "core.commitGraph=false",
}

// riskyConfig matches configuration keys that could redirect the push or
// the readback, or run a helper; a repository with any is refused. Since
// review N1 on 4571d10 the transport never reads the repository's config
// (see mirror), so this is a second layer that fails closed.
const riskyConfig = `^(url\..*|credential\..*|include\..*|includeif\..*|http\..*|core\.(askpass|gitproxy|sshcommand|worktree|alternaterefscommand))$`

// gitSSH reads no ssh config file (review N2: ssh finds ~/.ssh/config by
// the passwd entry, not $HOME), never leaves a master connection behind,
// and checks host keys strictly against the pinned file only.
const gitSSH = "ssh -F /dev/null -o BatchMode=yes -o ControlMaster=no -o ControlPath=none -o ControlPersist=no" +
	" -o StrictHostKeyChecking=yes -o GlobalKnownHostsFile=/dev/null -o UserKnownHostsFile="

// knownHostsRE keeps the pinned file's path safe inside GIT_SSH_COMMAND,
// which git runs through a shell.
var knownHostsRE = regexp.MustCompile(`^/[A-Za-z0-9._/-]+$`)

// gitEnvKeep is the allow-list of inherited variables.
var gitEnvKeep = map[string]bool{"PATH": true, "LANG": true, "LC_ALL": true, "LC_CTYPE": true, "TMPDIR": true, "SSH_AUTH_SOCK": true}

func (p *Publish) Kind() string { return "publish" }

func (p *Publish) Validate(spec IntentSpec) error {
	if !spec.NeedsApproval {
		return errors.New("publish: the effect must need an approval")
	}
	_, _, err := p.parse(spec.Request, spec.Subject)
	return err
}

// parse checks a stored or submitted request and its subject and returns
// the request with the repository's resolved directory.
func (p *Publish) parse(raw json.RawMessage, subject string) (publishRequest, string, error) {
	var r publishRequest
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		return r, "", fmt.Errorf("publish: request: %v", err)
	}
	if dec.More() {
		return r, "", errors.New("publish: request: trailing data")
	}
	if err := p.validURL(r.URL); err != nil {
		return r, "", err
	}
	if !validBranch(r.Branch) {
		return r, "", fmt.Errorf("publish: branch %q is not a plain branch name", r.Branch)
	}
	if !objectNameRE.MatchString(r.ExpectedHead) || !objectNameRE.MatchString(r.NewHead) || len(r.ExpectedHead) != len(r.NewHead) {
		return r, "", errors.New("publish: expected_head and new_head must be full lowercase object names")
	}
	if r.ExpectedHead == r.NewHead {
		return r, "", errors.New("publish: new_head equals expected_head")
	}
	if want := publishSubject(r); subject != want {
		return r, "", fmt.Errorf("publish: subject %q, want %q", subject, want)
	}
	dir, err := p.repoDir(r.Repo)
	return r, dir, err
}

// validURL accepts https://host/path and ssh://[user@]host[:port]/path with
// no password, scp-like user@host:path, and an absolute path only when
// AllowLocal. Never an option, a transport helper (::) or credentials.
func (p *Publish) validURL(s string) error {
	bad := fmt.Errorf("publish: url %q is not an allowed remote URL", s)
	if s == "" || len(s) > 512 || strings.HasPrefix(s, "-") || strings.Contains(s, "::") || strings.ContainsAny(s, " \t\r\n\\") {
		return bad
	}
	if strings.HasPrefix(s, "/") {
		if p.AllowLocal && filepath.Clean(s) == s {
			return nil
		}
		return bad
	}
	if scpURLRE.MatchString(s) {
		return p.sshReady()
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" || strings.HasPrefix(u.Host, "-") || u.RawQuery != "" || u.Fragment != "" || !urlPathRE.MatchString(u.Path) {
		return bad
	}
	switch u.Scheme {
	case "https":
		if u.User != nil {
			return errors.New("publish: an https url must not carry credentials")
		}
	case "ssh":
		if _, has := u.User.Password(); has {
			return errors.New("publish: an ssh url must not carry a password")
		}
		if strings.HasPrefix(u.User.Username(), "-") {
			return bad // review N4: never an ssh option
		}
		return p.sshReady()
	default:
		return bad
	}
	return nil
}

// sshReady requires a pinned known_hosts file for an ssh URL (review N2).
func (p *Publish) sshReady() error {
	if p.KnownHosts == "" {
		return errors.New("publish: an ssh url needs a pinned known_hosts file (Publish.KnownHosts)")
	}
	if !knownHostsRE.MatchString(p.KnownHosts) || filepath.Clean(p.KnownHosts) != p.KnownHosts {
		return fmt.Errorf("publish: known_hosts path %q must be an absolute clean plain path", p.KnownHosts)
	}
	return nil
}

// validBranch accepts a plain branch name: no refspec or option syntax, no
// empty, hidden or .lock component, no "..".
func validBranch(b string) bool {
	if !branchNameRE.MatchString(b) || b == "HEAD" || strings.Contains(b, "..") || strings.HasSuffix(b, ".") {
		return false
	}
	for _, c := range strings.Split(b, "/") {
		if c == "" || strings.HasPrefix(c, ".") || strings.HasSuffix(c, ".lock") {
			return false
		}
	}
	return true
}

// within resolves path and requires it strictly inside root (resolved).
func within(root, path string) (string, error) {
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(root, real)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%q is not inside the publish root", path)
	}
	return real, nil
}

// repoDir resolves repo (absolute and clean) and requires it to be strictly
// inside Root after symlinks.
func (p *Publish) repoDir(repo string) (string, error) {
	if p.Root == "" {
		return "", errors.New("publish: no repository root is configured")
	}
	if !filepath.IsAbs(repo) || filepath.Clean(repo) != repo {
		return "", fmt.Errorf("publish: repo %q must be an absolute clean path", repo)
	}
	root, err := filepath.EvalSymlinks(p.Root)
	if err != nil {
		return "", fmt.Errorf("publish: root: %v", err)
	}
	real, err := within(root, repo)
	if err != nil {
		return "", fmt.Errorf("publish: repo: %v", err)
	}
	return real, nil
}

// bind checks, before any transport, that the repository's git dir and
// common dir are inside Root, that it borrows no objects from elsewhere,
// and that no configuration could redirect or wrap the transport. It
// returns the resolved object directory. Its own git calls (rev-parse and
// config) run in the source repository and read its configuration; they
// are a fail-closed check only. Never add a transport or ancestry call
// here: those run in the mirror.
func (p *Publish) bind(ctx context.Context, dir string) (string, error) {
	root, err := filepath.EvalSymlinks(p.Root)
	if err != nil {
		return "", fmt.Errorf("publish: root: %v", err)
	}
	out, err := p.git(ctx, dir, "rev-parse", "--absolute-git-dir", "--git-common-dir")
	if err != nil {
		return "", fmt.Errorf("publish: git rev-parse: %v: %s", err, clip(out))
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) != 2 {
		return "", fmt.Errorf("publish: git rev-parse: unexpected output %q", clip(out))
	}
	common := lines[1]
	if !filepath.IsAbs(common) {
		common = filepath.Join(dir, common)
	}
	for _, d := range []string{lines[0], common} {
		if _, err := within(root, d); err != nil {
			return "", fmt.Errorf("publish: git dir: %v", err)
		}
	}
	objects, err := within(root, filepath.Join(common, "objects"))
	if err != nil {
		return "", fmt.Errorf("publish: object directory: %v", err)
	}
	for _, alt := range []string{"alternates", "http-alternates"} {
		if _, err := os.Lstat(filepath.Join(objects, "info", alt)); err == nil {
			return "", fmt.Errorf("publish: the repository borrows objects (%s); refused", alt)
		}
	}
	// The -c safety settings are read back too (scope "command"); only keys
	// from a configuration file count.
	out, err = p.git(ctx, dir, "config", "--show-scope", "--name-only", "--get-regexp", riskyConfig)
	var ee *exec.ExitError
	if err != nil && !(ctx.Err() == nil && errors.As(err, &ee) && ee.ExitCode() == 1) {
		return "", fmt.Errorf("publish: git config: %v: %s", err, clip(out))
	}
	var risky []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if scope, name, ok := strings.Cut(line, "\t"); ok && scope != "command" {
			risky = append(risky, scope+":"+name)
		} else if !ok && line != "" {
			risky = append(risky, line)
		}
	}
	if len(risky) > 0 {
		return "", fmt.Errorf("publish: repository configuration could redirect the push: %s", clip([]byte(strings.Join(risky, " "))))
	}
	return objects, nil
}

// mirror makes a fabric-owned bare repository, outside Root, that borrows
// the source repository's objects through alternates (review N1 on
// 4571d10). Every later git call runs in it, so configuration written into
// the source after bind cannot redirect the readback or the push. Objects
// are content addressed: whatever the source holds, the push sends exactly
// new_head's history. The caller runs cleanup.
func (p *Publish) mirror(ctx context.Context, objects string) (string, func(), error) {
	dir, err := os.MkdirTemp("", "fabric-publish-")
	if err != nil {
		return "", func() {}, fmt.Errorf("publish: mirror: %v", err)
	}
	cleanup := func() { os.RemoveAll(dir) } //nolint:errcheck
	if root, err := filepath.EvalSymlinks(p.Root); err == nil {
		if _, outside := within(root, dir); outside == nil {
			cleanup()
			return "", func() {}, errors.New("publish: the temporary directory is inside the publish root")
		}
	}
	if out, err := p.git(ctx, filepath.Dir(dir), "init", "-q", "--bare", "--template=", dir); err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("publish: mirror: git init: %v: %s", err, clip(out))
	}
	if err := os.WriteFile(filepath.Join(dir, "objects", "info", "alternates"), []byte(objects+"\n"), 0o600); err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("publish: mirror: %v", err)
	}
	return dir, cleanup, nil
}

// git runs one git command in dir with the safety settings and a scrubbed
// environment.
func (p *Publish) git(ctx context.Context, dir string, args ...string) ([]byte, error) {
	bin := p.Git
	if bin == "" {
		bin = "git"
	}
	pre := append([]string{}, gitSafety...)
	if p.AllowLocal {
		pre = append(pre, "-c", "protocol.file.allow=always")
	}
	return RunCommandEnv(ctx, dir, p.gitEnv(dir), bin, append(pre, args...)...)
}

// gitEnv is the allow-listed environment: no GIT_* or other inherited
// variables, no global or system configuration, no prompts, discovery kept
// to dir, and the ssh command above with the pinned known_hosts file (none
// configured: /dev/null, so strict checking refuses every host).
func (p *Publish) gitEnv(dir string) []string {
	var env []string
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); gitEnvKeep[k] {
			env = append(env, kv)
		}
	}
	kh := "/dev/null"
	if p.sshReady() == nil {
		kh = p.KnownHosts
	}
	return append(env, "HOME=/nonexistent", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0", "GIT_SSH_COMMAND="+gitSSH+kh, "GIT_CEILING_DIRECTORIES="+filepath.Dir(dir))
}

// remoteHead reads the remote branch head; found is false when the remote
// has no such branch.
func (p *Publish) remoteHead(ctx context.Context, dir string, r publishRequest) (head string, found bool, err error) {
	ref := "refs/heads/" + r.Branch
	out, err := p.git(ctx, dir, "ls-remote", "--heads", r.URL, ref)
	if err != nil {
		return "", false, fmt.Errorf("git ls-remote: %v: %s", err, clip(out))
	}
	for _, line := range strings.Split(string(out), "\n") {
		sha, name, ok := strings.Cut(strings.TrimRight(line, "\r"), "\t")
		if !ok || name != ref {
			continue
		}
		if !objectNameRE.MatchString(sha) {
			return "", false, fmt.Errorf("git ls-remote: unexpected line %q", clip([]byte(line)))
		}
		return sha, true, nil
	}
	return "", false, nil
}

// fastForward returns "" when new_head is a local commit that descends from
// expected_head, or a definite reason why not. An error is not definite.
func (p *Publish) fastForward(ctx context.Context, dir string, r publishRequest) (string, error) {
	if out, err := p.git(ctx, dir, "cat-file", "-e", r.NewHead+"^{commit}"); err != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("git cat-file: %v: %s", err, clip(out))
		}
		return "new_head " + r.NewHead + " is not a commit in the repository", nil
	}
	out, err := p.git(ctx, dir, "merge-base", "--is-ancestor", r.ExpectedHead, r.NewHead)
	if err == nil {
		return "", nil
	}
	if ctx.Err() != nil {
		return "", fmt.Errorf("git merge-base: %v: %s", err, clip(out))
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == 1 {
		return "new_head does not descend from expected_head: not a fast-forward", nil
	}
	return "ancestry check failed (is expected_head fetched?): " + clip(out), nil
}

// load parses the stored request, binds its repository and returns a
// fabric-owned mirror to run every later git call in (review N1). The
// caller runs cleanup.
func (p *Publish) load(ctx context.Context, e Effect) (publishRequest, string, func(), error) {
	r, dir, err := p.parse(e.Request, e.Subject)
	if err != nil {
		return r, "", func() {}, err
	}
	objects, err := p.bind(ctx, dir)
	if err != nil {
		return r, "", func() {}, err
	}
	m, cleanup, err := p.mirror(ctx, objects)
	return r, m, cleanup, err
}

// Precondition holds the effect lock and its transaction across one
// ls-remote, so the session idles in a transaction for up to the transport
// timeout (review 6); idle_in_transaction_session_timeout must exceed it.
func (p *Publish) Precondition(ctx context.Context, e Effect) (bool, string, error) {
	r, dir, cleanup, err := p.load(ctx, e)
	defer cleanup()
	if err != nil {
		return false, "", err
	}
	head, found, err := p.remoteHead(ctx, dir, r)
	if err != nil {
		return false, "", err
	}
	if !found {
		return false, "", fmt.Errorf("%w: %s has no branch %s (expected %s)", ErrPreconditionConflict, r.URL, r.Branch, r.ExpectedHead)
	}
	if head != r.ExpectedHead {
		return false, "", fmt.Errorf("%w: %s %s is at %s, expected %s", ErrPreconditionConflict, r.URL, r.Branch, head, r.ExpectedHead)
	}
	detail, err := p.fastForward(ctx, dir, r)
	if err != nil {
		return false, "", err
	}
	return detail == "", detail, nil
}

func (p *Publish) Dispatch(ctx context.Context, sub Submission) (Receipt, error) {
	r, dir, cleanup, err := p.load(ctx, sub.Effect)
	defer cleanup()
	if err != nil {
		return Receipt{}, err
	}
	// Ancestry again: the lease alone would let a non-fast-forward through.
	detail, err := p.fastForward(ctx, dir, r)
	if err != nil {
		return Receipt{}, err
	}
	if detail != "" {
		return Receipt{Outcome: OutcomeFailedDefinite, Detail: detail}, nil
	}
	ref := "refs/heads/" + r.Branch
	out, err := p.git(ctx, dir, "push", "--porcelain", "--force-with-lease="+ref+":"+r.ExpectedHead, r.URL, r.NewHead+":"+ref)
	if err != nil {
		return Receipt{}, fmt.Errorf("git push: %v: %s", err, clip(out))
	}
	if !fastForwarded(out, ref) {
		return Receipt{}, fmt.Errorf("git push reported no fast-forward of %s: %s", ref, clip(out))
	}
	// Review N1: CONFIRMED needs the remote itself to show new_head; any
	// doubt is an error, so the effect goes UNKNOWN and readback decides.
	if head, found, err := p.remoteHead(ctx, dir, r); err != nil || !found || head != r.NewHead {
		return Receipt{}, fmt.Errorf("git push: readback does not show new_head (found=%v head=%s err=%v)", found, head, err)
	}
	remote, _ := json.Marshal(map[string]string{"url": r.URL, "ref": ref, "old": r.ExpectedHead, "new": r.NewHead})
	return Receipt{Outcome: OutcomeConfirmed, Detail: "fast-forwarded " + ref, Remote: remote}, nil
}

// fastForwarded reports whether porcelain push output has a fast-forward
// line (flag ' ') for ref.
func fastForwarded(out []byte, ref string) bool {
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Split(line, "\t")
		if len(f) >= 3 && f[0] == " " && strings.HasSuffix(f[1], ":"+ref) {
			return true
		}
	}
	return false
}

func (p *Publish) Reconcile(ctx context.Context, e Effect) (Observation, string, error) {
	r, dir, cleanup, err := p.load(ctx, e)
	defer cleanup()
	if err != nil {
		return ObsUnknown, "", err
	}
	head, found, err := p.remoteHead(ctx, dir, r)
	if err != nil {
		return ObsUnknown, "", err
	}
	switch {
	case !found:
		return ObsConflict, "the remote has no branch " + r.Branch, nil
	case head == r.NewHead:
		return ObsCommitted, r.Branch + " is at new_head", nil
	case head == r.ExpectedHead:
		return ObsNotApplied, r.Branch + " is still at expected_head", nil
	default:
		return ObsConflict, r.Branch + " is at " + head + ", neither expected_head nor new_head", nil
	}
}

// userinfoRE matches credentials embedded in a URL.
var userinfoRE = regexp.MustCompile(`://[^/@\s]+@`)

// clip keeps git output short and free of credentials for receipts: the
// "To <url>" line is dropped, URL user info redacted, and the rest capped.
func clip(out []byte) string {
	var keep []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if strings.HasPrefix(line, "To ") || strings.TrimSpace(line) == "" {
			continue
		}
		keep = append(keep, strings.TrimSpace(line))
	}
	s := userinfoRE.ReplaceAllString(strings.Join(keep, " | "), "://***@")
	if len(s) > 400 {
		s = s[:400] + "..."
	}
	return s
}
