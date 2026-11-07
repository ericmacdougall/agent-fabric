//go:build unix

package effects

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
//     inside Root, a remote name (not a URL or an option), a plain branch
//     name, and full object names.
//   - Precondition: the remote head is read with ls-remote. Anything but
//     expected_head is ErrPreconditionConflict, which the broker records as
//     QUARANTINED with nothing sent. new_head must be a local commit that
//     descends from expected_head (a fast-forward), or the effect fails
//     definitely.
//   - Dispatch: git push --force-with-lease=refs/heads/B:expected_head
//     new_head:refs/heads/B. The lease is the compare-and-swap against a
//     move after the precondition; ancestry is checked again first, so the
//     lease can never rewrite history. Only a porcelain fast-forward line
//     is CONFIRMED; anything else is an error, which the broker records as
//     UNKNOWN, and readback decides.
//   - Reconcile: the remote head is new_head (COMMITTED), expected_head
//     (NOT_APPLIED) or anything else (CONFLICT, so QUARANTINED).
//
// git runs through RunCommandEnv, whose process group is stopped at the
// deadline. Helpers that could outlive the group (review nit 3 on 11145f4)
// are disabled: ssh ControlMaster and ControlPersist, credential helpers,
// fsmonitor, auto gc and maintenance, and repository hooks. The ext::
// transport, prompts, submodule pushes and followed tags are off too.
type Publish struct {
	Root string // every repository must be strictly inside it; empty refuses all
	Git  string // the git binary; default "git"
}

type publishRequest struct {
	Repo         string `json:"repo"`
	Remote       string `json:"remote"`
	Branch       string `json:"branch"`
	ExpectedHead string `json:"expected_head"`
	NewHead      string `json:"new_head"`
}

var (
	objectNameRE = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)
	remoteNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	branchNameRE = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._/-]{0,199}$`)
)

// gitSafety precedes every git command: the -c settings apply last, so they
// override repository and user configuration.
var gitSafety = []string{
	"-c", "credential.helper=",
	"-c", "core.fsmonitor=false",
	"-c", "core.hooksPath=/dev/null",
	"-c", "gc.auto=0",
	"-c", "maintenance.auto=false",
	"-c", "protocol.ext.allow=never",
	"-c", "push.recurseSubmodules=no",
	"-c", "push.followTags=false",
}

// gitSSH keeps ssh from leaving a master connection behind; GIT_SSH_COMMAND
// overrides core.sshCommand.
const gitSSH = "ssh -o BatchMode=yes -o ControlMaster=no -o ControlPath=none -o ControlPersist=no"

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
	if !remoteNameRE.MatchString(r.Remote) {
		return r, "", fmt.Errorf("publish: remote %q is not a remote name", r.Remote)
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
	if want := "branch:" + r.Remote + "/" + r.Branch; subject != want {
		return r, "", fmt.Errorf("publish: subject %q, want %q", subject, want)
	}
	dir, err := p.repoDir(r.Repo)
	return r, dir, err
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
	real, err := filepath.EvalSymlinks(repo)
	if err != nil {
		return "", fmt.Errorf("publish: repo: %v", err)
	}
	rel, err := filepath.Rel(root, real)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("publish: repo %q is not inside the publish root", repo)
	}
	return real, nil
}

// git runs one git command in dir with the safety settings and a scrubbed
// environment.
func (p *Publish) git(ctx context.Context, dir string, args ...string) ([]byte, error) {
	bin := p.Git
	if bin == "" {
		bin = "git"
	}
	return RunCommandEnv(ctx, dir, gitEnv(), bin, append(append([]string{}, gitSafety...), args...)...)
}

// gitEnv is the process environment without GIT_* overrides or an ssh
// askpass, plus no prompts and the ssh command above.
func gitEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "GIT_") || k == "SSH_ASKPASS" {
			continue
		}
		env = append(env, kv)
	}
	return append(env, "GIT_TERMINAL_PROMPT=0", "GIT_SSH_COMMAND="+gitSSH)
}

// remoteHead reads the remote branch head; found is false when the remote
// has no such branch.
func (p *Publish) remoteHead(ctx context.Context, dir string, r publishRequest) (head string, found bool, err error) {
	ref := "refs/heads/" + r.Branch
	out, err := p.git(ctx, dir, "ls-remote", "--heads", r.Remote, ref)
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

func (p *Publish) Precondition(ctx context.Context, e Effect) (bool, string, error) {
	r, dir, err := p.parse(e.Request, e.Subject)
	if err != nil {
		return false, "", err
	}
	head, found, err := p.remoteHead(ctx, dir, r)
	if err != nil {
		return false, "", err
	}
	if !found {
		return false, "", fmt.Errorf("%w: %s has no branch %s (expected %s)", ErrPreconditionConflict, r.Remote, r.Branch, r.ExpectedHead)
	}
	if head != r.ExpectedHead {
		return false, "", fmt.Errorf("%w: %s/%s is at %s, expected %s", ErrPreconditionConflict, r.Remote, r.Branch, head, r.ExpectedHead)
	}
	detail, err := p.fastForward(ctx, dir, r)
	if err != nil {
		return false, "", err
	}
	return detail == "", detail, nil
}

func (p *Publish) Dispatch(ctx context.Context, sub Submission) (Receipt, error) {
	r, dir, err := p.parse(sub.Effect.Request, sub.Effect.Subject)
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
	out, err := p.git(ctx, dir, "push", "--porcelain", "--force-with-lease="+ref+":"+r.ExpectedHead, r.Remote, r.NewHead+":"+ref)
	if err != nil {
		return Receipt{}, fmt.Errorf("git push: %v: %s", err, clip(out))
	}
	if !fastForwarded(out, ref) {
		return Receipt{}, fmt.Errorf("git push reported no fast-forward of %s: %s", ref, clip(out))
	}
	remote, _ := json.Marshal(map[string]string{"ref": ref, "old": r.ExpectedHead, "new": r.NewHead})
	return Receipt{Outcome: OutcomeConfirmed, Detail: "fast-forwarded " + r.Remote + "/" + r.Branch, Remote: remote}, nil
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
	r, dir, err := p.parse(e.Request, e.Subject)
	if err != nil {
		return ObsUnknown, "", err
	}
	head, found, err := p.remoteHead(ctx, dir, r)
	if err != nil {
		return ObsUnknown, "", err
	}
	switch {
	case !found:
		return ObsConflict, r.Remote + " has no branch " + r.Branch, nil
	case head == r.NewHead:
		return ObsCommitted, r.Remote + "/" + r.Branch + " is at new_head", nil
	case head == r.ExpectedHead:
		return ObsNotApplied, r.Remote + "/" + r.Branch + " is still at expected_head", nil
	default:
		return ObsConflict, r.Remote + "/" + r.Branch + " is at " + head + ", neither expected_head nor new_head", nil
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
