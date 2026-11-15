// Package sweep is the fab-worker side of the AC-16 sweep (`fab __sweep`):
// it traverses the worker-writable run roots without following any link,
// counts keyed matches of the bound token's patterns, and removes the
// worker's files only when the sweep is complete. It receives no
// credential byte: only an HMAC key, a rolling base and per-pattern
// fingerprints and MACs (credential.SweepInput).
package sweep

import (
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"hash"
	"io"
	"os"
	"sort"
	"time"

	"fabric/internal/credential"

	"golang.org/x/sys/unix"
)

// Limits (AC-16 (8)).
type Limits struct {
	Entries       int
	Bytes         int64
	Wall          time.Duration
	Confirmations int
}

// DefaultLimits are the spec's ceilings.
var DefaultLimits = Limits{Entries: 20000, Bytes: 512 << 20, Wall: 120 * time.Second, Confirmations: 1_000_000}

// Incomplete reasons (fixed codes).
const (
	ReasonInput         = "input_malformed"
	ReasonShortPattern  = "input_short_pattern"
	ReasonEntries       = "limit_entries"
	ReasonBytes         = "limit_bytes"
	ReasonWall          = "limit_wall"
	ReasonConfirmations = "limit_confirmations"
	ReasonOpen          = "open_error"
	ReasonHardlink      = "anomaly_hardlink"
	ReasonSpecial       = "anomaly_special"
	ReasonSwapped       = "anomaly_swapped"
	ReasonRemove        = "remove_error"
	ReasonRoot          = "root_error"
)

// Report is the sweep's only output: counts, never content.
type Report struct {
	Complete   bool           `json:"complete"`
	Reasons    []string       `json:"reasons"`
	Entries    map[string]int `json:"entries"` // by kind
	Files      int            `json:"files"`
	Bytes      int64          `json:"bytes"`
	Candidates int            `json:"candidates"`
	Confirmed  int            `json:"confirmed"`
	Counts     map[string]int `json:"counts"` // per label
	Literal    int            `json:"literal"`
}

func (r *Report) fail(reason string) {
	for _, x := range r.Reasons {
		if x == reason {
			return
		}
	}
	r.Reasons = append(r.Reasons, reason)
}

// errStop aborts the traversal after a limit.
var errStop = errors.New("sweep: stop")

const ringSize = 4096 // >= SweepMaxLen + SweepWindow, a power of two
const ringMask = ringSize - 1

type pending struct {
	deadline uint64
	start    uint64
	entry    int
}

// matcher is the per-sweep state built from the input.
type matcher struct {
	in      credential.SweepInput
	macs    [][]byte
	mac     hash.Hash
	bw1     uint64 // B^(W-1) mod p
	table   [1 << credential.SweepFPBits / 64]uint64
	byFP    map[uint32][]int
	hasNorm map[credential.Norm]bool
}

func newMatcher(in credential.SweepInput, key []byte, macs [][]byte) *matcher {
	m := &matcher{in: in, macs: macs, mac: hmac.New(sha256.New, key), byFP: map[uint32][]int{}, hasNorm: map[credential.Norm]bool{}}
	m.bw1 = 1
	for i := 0; i < in.Window-1; i++ {
		m.bw1 = credential.MulMod(m.bw1, in.Base)
	}
	for i, e := range in.Entries {
		m.table[e.FP>>6] |= 1 << (e.FP & 63)
		m.byFP[e.FP] = append(m.byFP[e.FP], i)
		m.hasNorm[e.Norm] = true
	}
	return m
}

// stream is the rolling state of one file under one normalisation.
type stream struct {
	m       *matcher
	norm    credential.Norm
	n       uint64
	h       uint64
	ring    [ringSize]byte
	buckets [ringSize][]pending
	nz      credential.Normalizer
	lit     int // literal progress (NormWS only)
}

func (s *stream) reset() {
	s.n, s.h, s.lit = 0, 0, 0
	for i := range s.buckets {
		s.buckets[i] = s.buckets[i][:0]
	}
	s.nz = credential.Normalizer{Mode: s.norm}
}

// Scanner runs one sweep.
type Scanner struct {
	Limits  Limits
	Now     func() time.Time // monotonic clock; injectable
	m       *matcher
	rep     *Report
	start   time.Time
	euid    int
	ws      *stream
	esc     *stream
	stop    bool
	entries int
}

// Run sweeps roots with the stdin input. It never returns an error: every
// failure is an incomplete report.
func Run(input []byte, roots []string, lim Limits, now func() time.Time) Report {
	rep := Report{Entries: map[string]int{}, Counts: map[string]int{}}
	in, key, macs, short, err := credential.ParseSweepInput(input)
	if err != nil {
		rep.fail(ReasonInput)
		return rep
	}
	if short {
		rep.fail(ReasonShortPattern)
		return rep
	}
	if now == nil {
		now = time.Now
	}
	s := &Scanner{Limits: lim, Now: now, m: newMatcher(in, key, macs), rep: &rep, euid: os.Geteuid()}
	s.ws = &stream{m: s.m, norm: credential.NormWS}
	s.esc = &stream{m: s.m, norm: credential.NormEsc}
	s.start = now()
	if len(roots) == 0 {
		rep.fail(ReasonRoot)
	}
	for _, root := range roots {
		if s.stop {
			break
		}
		fd, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			if errors.Is(err, unix.ENOENT) {
				continue // a root the attempt never created
			}
			rep.fail(ReasonRoot)
			continue
		}
		s.walk(fd, true)
		unix.Close(fd)
	}
	if len(rep.Reasons) == 0 {
		for _, root := range roots {
			fd, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
			if err != nil {
				if !errors.Is(err, unix.ENOENT) {
					rep.fail(ReasonRemove)
				}
				continue
			}
			if err := s.remove(fd); err != nil {
				rep.fail(ReasonRemove)
			}
			unix.Close(fd)
		}
	}
	sort.Strings(rep.Reasons)
	rep.Complete = len(rep.Reasons) == 0
	return rep
}

func (s *Scanner) overTime() bool {
	if s.Now().Sub(s.start) > s.Limits.Wall {
		s.rep.fail(ReasonWall)
		s.stop = true
		return true
	}
	return false
}

func listNames(dirfd int) ([]string, error) {
	dup, err := unix.Dup(dirfd)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(dup), "dir")
	defer f.Close()
	names, err := f.Readdirnames(-1)
	sort.Strings(names) // deterministic order
	return names, err
}

// walk scans every entry under dirfd. It opens each entry relative to its
// parent with O_NOFOLLOW, never follows a link, and never leaves the root.
func (s *Scanner) walk(dirfd int, _ bool) {
	names, err := listNames(dirfd)
	if err != nil {
		s.rep.fail(ReasonOpen)
		return
	}
	for _, name := range names {
		if s.stop || s.overTime() {
			return
		}
		if s.entries >= s.Limits.Entries {
			s.rep.fail(ReasonEntries)
			s.stop = true
			return
		}
		s.entries++
		var st unix.Stat_t
		if err := unix.Fstatat(dirfd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			s.rep.Entries["error"]++
			s.rep.fail(ReasonOpen)
			continue
		}
		switch uint32(st.Mode) & unix.S_IFMT {
		case unix.S_IFDIR:
			s.rep.Entries["dir"]++
			if int(st.Uid) == s.euid && uint32(st.Mode)&0o700 != 0o700 {
				unix.Fchmodat(dirfd, name, uint32(st.Mode)&0o7777|0o700, 0)
			}
			fd, err := unix.Openat(dirfd, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
			if err != nil {
				if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR) {
					s.rep.fail(ReasonSwapped)
				} else {
					s.rep.fail(ReasonOpen)
				}
				continue
			}
			var fst unix.Stat_t
			if unix.Fstat(fd, &fst) != nil || fst.Ino != st.Ino || fst.Dev != st.Dev {
				s.rep.fail(ReasonSwapped)
				unix.Close(fd)
				continue
			}
			s.walk(fd, false)
			unix.Close(fd)
		case unix.S_IFREG:
			s.rep.Entries["regular"]++
			if st.Nlink > 1 {
				s.rep.Entries["hardlink"]++
				s.rep.fail(ReasonHardlink)
				continue
			}
			s.scanFile(dirfd, name, &st)
		default:
			kind := map[uint32]string{unix.S_IFLNK: "symlink", unix.S_IFIFO: "fifo", unix.S_IFSOCK: "socket",
				unix.S_IFCHR: "device", unix.S_IFBLK: "device"}[uint32(st.Mode)&unix.S_IFMT]
			if kind == "" {
				kind = "other"
			}
			s.rep.Entries[kind]++
			s.rep.fail(ReasonSpecial)
			// Counted by kind and unlinked without being opened.
			if err := unix.Unlinkat(dirfd, name, 0); err != nil && !errors.Is(err, unix.ENOENT) {
				s.rep.fail(ReasonRemove)
			}
		}
	}
}

func (s *Scanner) scanFile(dirfd int, name string, st *unix.Stat_t) {
	if int(st.Uid) == s.euid && uint32(st.Mode)&0o400 == 0 {
		// The worker is stopped (verified), so nothing can swap the entry
		// between the fstatat above and this chmod.
		unix.Fchmodat(dirfd, name, uint32(st.Mode)&0o7777|0o400, 0)
	}
	fd, err := unix.Openat(dirfd, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, unix.ELOOP) {
			s.rep.fail(ReasonSwapped)
		} else {
			s.rep.fail(ReasonOpen)
		}
		return
	}
	f := os.NewFile(uintptr(fd), name)
	defer f.Close()
	var fst unix.Stat_t
	if unix.Fstat(fd, &fst) != nil || uint32(fst.Mode)&unix.S_IFMT != unix.S_IFREG || fst.Ino != st.Ino || fst.Nlink > 1 {
		s.rep.fail(ReasonSwapped)
		return
	}
	s.rep.Files++
	s.ws.reset()
	s.esc.reset()
	buf := make([]byte, 64<<10)
	for !s.stop {
		n, err := f.Read(buf)
		if n > 0 {
			s.rep.Bytes += int64(n)
			if s.rep.Bytes > s.Limits.Bytes {
				s.rep.fail(ReasonBytes)
				s.stop = true
				return
			}
			s.feed(s.ws, buf[:n])
			s.feed(s.esc, buf[:n])
			if s.overTime() {
				return
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			s.rep.fail(ReasonOpen)
			return
		}
	}
	if s.stop {
		return
	}
	s.flush(s.ws)
	s.flush(s.esc)
}

func (s *Scanner) feed(st *stream, b []byte) {
	if !s.m.hasNorm[st.norm] && st.norm != credential.NormWS {
		return
	}
	st.nz.Feed(b, func(c byte, _ int64) { s.push(st, c) })
}

func (s *Scanner) flush(st *stream) {
	st.nz.Flush(func(c byte, _ int64) { s.push(st, c) })
}

func (s *Scanner) push(st *stream, c byte) {
	if s.stop {
		return
	}
	m := s.m
	w := uint64(m.in.Window)
	if st.norm == credential.NormWS {
		lit := m.in.Literal
		switch {
		case c == lit[st.lit]:
			st.lit++
		case c == lit[0]:
			st.lit = 1
		default:
			st.lit = 0
		}
		if st.lit == len(lit) {
			s.rep.Literal++
			st.lit = 0
		}
	}
	if !m.hasNorm[st.norm] {
		return
	}
	st.ring[st.n&ringMask] = c
	if st.n >= w {
		out := uint64(st.ring[(st.n-w)&ringMask])
		st.h = st.h + credential.MersenneP - credential.MulMod(out, m.bw1)
		if st.h >= credential.MersenneP {
			st.h -= credential.MersenneP
		}
	}
	st.h = credential.MulMod(st.h, m.in.Base) + uint64(c)
	if st.h >= credential.MersenneP {
		st.h -= credential.MersenneP
	}
	st.n++
	if st.n >= w {
		fp := uint32(st.h & (1<<credential.SweepFPBits - 1))
		if m.table[fp>>6]&(1<<(fp&63)) != 0 {
			start := st.n - w
			for _, i := range m.byFP[fp] {
				e := m.in.Entries[i]
				if e.Norm != st.norm {
					continue
				}
				s.rep.Candidates++
				d := start + uint64(e.Len)
				st.buckets[d&ringMask] = append(st.buckets[d&ringMask], pending{deadline: d, start: start, entry: i})
			}
		}
	}
	b := &st.buckets[st.n&ringMask]
	if len(*b) == 0 {
		return
	}
	keep := (*b)[:0]
	for _, p := range *b {
		if p.deadline != st.n {
			keep = append(keep, p)
			continue
		}
		if s.rep.Confirmed >= s.Limits.Confirmations {
			s.rep.fail(ReasonConfirmations)
			s.stop = true
			return
		}
		s.rep.Confirmed++
		e := m.in.Entries[p.entry]
		m.mac.Reset()
		a := p.start & ringMask
		z := (p.start + uint64(e.Len)) & ringMask
		if a < z || e.Len == 0 {
			m.mac.Write(st.ring[a:z])
		} else {
			m.mac.Write(st.ring[a:])
			m.mac.Write(st.ring[:z])
		}
		if hmac.Equal(m.mac.Sum(nil), m.macs[p.entry]) {
			s.rep.Counts[e.Label]++
		}
	}
	*b = keep
}

// remove deletes every entry under dirfd (the complete-sweep removal pass),
// never following a link.
func (s *Scanner) remove(dirfd int) error {
	names, err := listNames(dirfd)
	if err != nil {
		return err
	}
	var first error
	for _, name := range names {
		var st unix.Stat_t
		if err := unix.Fstatat(dirfd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			first = errors.Join(first, err)
			continue
		}
		if uint32(st.Mode)&unix.S_IFMT == unix.S_IFDIR {
			fd, err := unix.Openat(dirfd, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
			if err != nil {
				first = errors.Join(first, err)
				continue
			}
			first = errors.Join(first, s.remove(fd))
			unix.Close(fd)
			first = errors.Join(first, unix.Unlinkat(dirfd, name, unix.AT_REMOVEDIR))
			continue
		}
		first = errors.Join(first, unix.Unlinkat(dirfd, name, 0))
	}
	return first
}
