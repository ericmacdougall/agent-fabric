package credential

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// Secret directory names (AC-13, AC-18).
const (
	CurrentName  = "claude-oauth"
	VersionPfx   = "claude-oauth.v"
	TmpName      = "claude-oauth.tmp"
	LockName     = ".rotate.lock"
	tokenMode    = 0o640
	maxTokenFile = 4096
)

// Refusal is a definite refusal of the secret directory. Reason is a fixed
// code; Detail never holds a credential byte.
type Refusal struct {
	Reason string
	Detail string
}

func (r *Refusal) Error() string {
	if r.Detail == "" {
		return "credential: " + r.Reason
	}
	return "credential: " + r.Reason + ": " + r.Detail
}

func refuse(reason, format string, a ...any) error {
	return &Refusal{Reason: reason, Detail: fmt.Sprintf(format, a...)}
}

// Refusal reasons.
const (
	RNotRegular   = "token_not_regular"
	RSymlink      = "token_symlink"
	ROwnerWorker  = "token_owner_worker"
	RGroupWorker  = "token_group_worker"
	RWorldBits    = "token_world_bits"
	RUnderRoot    = "token_under_read_root"
	RLinkRule     = "token_link_rule"
	RShape        = "token_shape"
	RDir          = "secret_dir"
	RMissing      = "token_missing"
	RLength       = "credential_length"
	RCaptureExist = "capture_exists"
)

// Policy is the secret directory and the identities its checks compare
// against. Tests pass a temporary directory and synthetic identities.
type Policy struct {
	Dir        string
	WorkerUID  int
	WorkerGIDs []int
	ReadRoots  []string // sandbox read roots; the directory must lie under none
	// OwnerUID and GroupGID are applied to files written by capture and
	// rotation (the operator and fab-secret); -1 keeps the creator's.
	OwnerUID, GroupGID int

	faults *faults // test-only fault injection
}

type faults struct {
	beforeRename func() error
	beforeSync   func() error
}

type statT struct {
	dev, ino, nlink uint64
	mode            uint32
	uid, gid        int
	size            int64
}

func toStat(st *unix.Stat_t) statT {
	return statT{dev: uint64(st.Dev), ino: uint64(st.Ino), nlink: uint64(st.Nlink), mode: uint32(st.Mode),
		uid: int(st.Uid), gid: int(st.Gid), size: int64(st.Size)}
}

func (s statT) typ() uint32 { return s.mode & unix.S_IFMT }

// VersionName is claude-oauth.v{n}.
func VersionName(n int) string { return VersionPfx + strconv.Itoa(n) }

// parseVersion returns n for claude-oauth.v{n} with canonical decimal n.
func parseVersion(name string) (int, bool) {
	s, ok := strings.CutPrefix(name, VersionPfx)
	if !ok || s == "" || (len(s) > 1 && s[0] == '0') {
		return 0, false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(s)
	return n, err == nil && n >= 0
}

// dirHandle is an open secret directory with its listing.
type dirHandle struct {
	fd      int
	entries map[string]statT
}

func (h *dirHandle) close() { unix.Close(h.fd) }

func (p Policy) openDir() (*dirHandle, error) {
	fd, err := unix.Open(p.Dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, refuse(RDir, "open %s: %v", p.Dir, err)
	}
	h := &dirHandle{fd: fd, entries: map[string]statT{}}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		h.close()
		return nil, refuse(RDir, "fstat: %v", err)
	}
	ds := toStat(&st)
	if ds.uid == p.WorkerUID || ds.mode&0o002 != 0 || (ds.mode&0o020 != 0 && p.workerGroup(ds.gid)) {
		h.close()
		return nil, refuse(RDir, "the directory is writable by the worker")
	}
	if err := p.checkReadRoots(); err != nil {
		h.close()
		return nil, err
	}
	dup, err := unix.Dup(fd)
	if err != nil {
		h.close()
		return nil, refuse(RDir, "dup: %v", err)
	}
	f := os.NewFile(uintptr(dup), p.Dir)
	names, err := f.Readdirnames(-1)
	f.Close()
	if err != nil {
		h.close()
		return nil, refuse(RDir, "list: %v", err)
	}
	for _, n := range names {
		var est unix.Stat_t
		if err := unix.Fstatat(fd, n, &est, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			continue // vanished between list and stat
		}
		h.entries[n] = toStat(&est)
	}
	return h, nil
}

func (p Policy) workerGroup(gid int) bool {
	for _, g := range p.WorkerGIDs {
		if g == gid {
			return true
		}
	}
	return false
}

func (p Policy) checkReadRoots() error {
	dir, err := filepath.EvalSymlinks(p.Dir)
	if err != nil {
		return refuse(RDir, "resolve: %v", err)
	}
	for _, r := range p.ReadRoots {
		root, err := filepath.EvalSymlinks(r)
		if err != nil {
			root = filepath.Clean(r)
		}
		rel, err := filepath.Rel(root, dir)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, "../") {
			return refuse(RUnderRoot, "the secret directory lies under a sandbox read root")
		}
	}
	return nil
}

// checkFile applies AC-13 (a)-(d) to an open descriptor's stat.
func (p Policy) checkFile(name string, st statT) error {
	if st.typ() != unix.S_IFREG {
		return refuse(RNotRegular, "%s is not a regular file", name)
	}
	if st.uid == p.WorkerUID {
		return refuse(ROwnerWorker, "%s is owned by the worker uid", name)
	}
	if p.workerGroup(st.gid) {
		return refuse(RGroupWorker, "%s has a group the worker belongs to", name)
	}
	if st.mode&0o007 != 0 {
		return refuse(RWorldBits, "%s has world permission bits", name)
	}
	return nil
}

// openChecked opens name relative to the directory with O_NOFOLLOW and
// checks the descriptor. A symlink fails the open.
func (p Policy) openChecked(h *dirHandle, name string) (int, statT, error) {
	fd, err := unix.Openat(h.fd, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.EMLINK) {
			return -1, statT{}, refuse(RSymlink, "%s is a symlink", name)
		}
		if errors.Is(err, unix.ENOENT) {
			return -1, statT{}, refuse(RMissing, "%s does not exist", name)
		}
		return -1, statT{}, refuse(RNotRegular, "open %s: %v", name, err)
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		unix.Close(fd)
		return -1, statT{}, refuse(RNotRegular, "fstat %s: %v", name, err)
	}
	s := toStat(&st)
	if err := p.checkFile(name, s); err != nil {
		unix.Close(fd)
		return -1, statT{}, err
	}
	return fd, s, nil
}

// checkLinks applies the AC-13 link rules to the whole directory, given the
// current token's open stat. It returns the version whose inode is current.
func (p Policy) checkLinks(h *dirHandle, cur statT) (int, error) {
	version := -1
	for name, st := range h.entries {
		if st.typ() == unix.S_IFLNK {
			return -1, refuse(RSymlink, "the secret directory holds a symlink")
		}
		same := st.dev == cur.dev && st.ino == cur.ino
		if name == CurrentName {
			if !same {
				return -1, refuse(RLinkRule, "claude-oauth changed while it was checked")
			}
			continue
		}
		n, isVersion := parseVersion(name)
		if same {
			if !isVersion || version >= 0 {
				return -1, refuse(RLinkRule, "claude-oauth has an extra link")
			}
			version = n
		}
	}
	if cur.nlink != 2 || version < 0 {
		return -1, refuse(RLinkRule, "claude-oauth must have exactly 2 links, the second a version file (has %d)", cur.nlink)
	}
	return version, nil
}

// checkVersionFile applies (a)-(e) and the version-file link rule.
func (p Policy) checkVersionFile(h *dirHandle, name string, cur *statT) (int, statT, error) {
	fd, st, err := p.openChecked(h, name)
	if err != nil {
		return -1, st, err
	}
	isCur := cur != nil && st.dev == cur.dev && st.ino == cur.ino
	switch {
	case st.nlink == 1 && !isCur:
	case st.nlink == 2 && isCur:
	default:
		unix.Close(fd)
		return -1, st, refuse(RLinkRule, "%s has %d links", name, st.nlink)
	}
	return fd, st, nil
}

func readToken(fd int, name string) ([]byte, error) {
	f := os.NewFile(uintptr(fd), name)
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxTokenFile+1))
	if err != nil {
		return nil, refuse(RShape, "read %s: %v", name, err)
	}
	if err := CheckTokenValue(b); err != nil {
		return nil, err
	}
	return b, nil
}

// CheckTokenValue applies the length rule and a printable-ASCII shape rule.
// Neither error carries a byte of the value.
func CheckTokenValue(b []byte) error {
	if err := CheckTokenLength(len(b)); err != nil {
		return &Refusal{Reason: RLength, Detail: err.Error()}
	}
	for _, c := range b {
		if c < 0x21 || c > 0x7e {
			return refuse(RShape, "the token holds a byte outside printable ASCII")
		}
	}
	return nil
}

// Token is the loaded current token and its version id.
type Token struct {
	Version int
	Secret  Secret
}

// LoadCurrent opens claude-oauth with O_NOFOLLOW, applies every AC-13 check
// to the descriptor and the link rules to the directory, identifies the
// version by inode (AC-18), checks every version file, and applies the
// length rule. It is also the AC-17 admission check.
func (p Policy) LoadCurrent() (Token, error) {
	h, err := p.openDir()
	if err != nil {
		return Token{}, err
	}
	defer h.close()
	fd, cur, err := p.openChecked(h, CurrentName)
	if err != nil {
		return Token{}, err
	}
	version, err := p.checkLinks(h, cur)
	if err != nil {
		unix.Close(fd)
		return Token{}, err
	}
	for _, name := range sortedNames(h.entries) {
		if _, ok := parseVersion(name); !ok {
			continue
		}
		vfd, _, err := p.checkVersionFile(h, name, &cur)
		if err != nil {
			unix.Close(fd)
			return Token{}, err
		}
		unix.Close(vfd)
	}
	val, err := readToken(fd, CurrentName)
	if err != nil {
		return Token{}, err
	}
	return Token{Version: version, Secret: NewSecret(KindToken, VersionName(version), val)}, nil
}

// LoadVersion reads claude-oauth.v{n} by version id with every AC-13 check
// (used at settle, AC-18).
func (p Policy) LoadVersion(n int) (Secret, error) {
	h, err := p.openDir()
	if err != nil {
		return Secret{}, err
	}
	defer h.close()
	for _, st := range h.entries {
		if st.typ() == unix.S_IFLNK {
			return Secret{}, refuse(RSymlink, "the secret directory holds a symlink")
		}
	}
	var cur *statT
	if st, ok := h.entries[CurrentName]; ok {
		cur = &st
	}
	fd, _, err := p.checkVersionFile(h, VersionName(n), cur)
	if err != nil {
		return Secret{}, err
	}
	val, err := readToken(fd, VersionName(n))
	if err != nil {
		return Secret{}, err
	}
	return NewSecret(KindToken, VersionName(n), val), nil
}

// CheckAdmission is the AC-17 admission rule: claude-oauth is a hard link to
// an existing version file and passes every check.
func (p Policy) CheckAdmission() error {
	_, err := p.LoadCurrent()
	return err
}

func sortedNames(m map[string]statT) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// versions lists the version numbers present.
func (h *dirHandle) versions() []int {
	var out []int
	for name := range h.entries {
		if n, ok := parseVersion(name); ok {
			out = append(out, n)
		}
	}
	sort.Ints(out)
	return out
}
