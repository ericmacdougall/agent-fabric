// Package records writes the M1 attempt records (spec AC-29): delivery_blocked,
// delivered_earlier, credential_exposure, sweep_incomplete and late_write.
// They are JSON files in the fabd-only run area, {run area}/records/{attempt}.json,
// mode 0600, one JSON array per attempt. A record names the mission, the
// attempt, its kind, a class code and the time; late_write also names the
// remote comment id. A record never holds body bytes or credential bytes:
// every field is checked against a closed grammar, except the operator's
// release reason, which is one bounded line of printable ASCII refused on
// any credential-scan hit. No column, event kind or migration is involved (I-4).
package records

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"fabric/internal/credential"
	"fabric/internal/domain"
)

// Kind is one of the AC-29 record kinds.
type Kind string

const (
	DeliveryBlocked    Kind = "delivery_blocked"
	DeliveredEarlier   Kind = "delivered_earlier"
	CredentialExposure Kind = "credential_exposure"
	SweepIncomplete    Kind = "sweep_incomplete"
	LateWrite          Kind = "late_write"
	// MissionHold and MissionRelease record an operator hold (goodqa#15966
	// owner ruling R1) and its release; Class is the reason class.
	MissionHold Kind = "mission_hold"
	// ReleaseAuthorized is the operator's release of one hold generation
	// (spec v16 AC-34): written durably while the hold row is locked and
	// before the row is deleted, so a release is never unrecorded. It
	// carries Generation, Reason and AckUnswept. On its own it releases
	// nothing.
	ReleaseAuthorized Kind = "release_authorized"
)

var kinds = map[Kind]bool{DeliveryBlocked: true, DeliveredEarlier: true, CredentialExposure: true, SweepIncomplete: true, LateWrite: true, MissionHold: true, ReleaseAuthorized: true}

// Record is one AC-29 record.
type Record struct {
	Mission domain.MissionID `json:"mission"`
	Attempt domain.AttemptID `json:"attempt"`
	Kind    Kind             `json:"kind"`
	// Class is a fixed code (for example lease_budget, scan_token_form,
	// m1_second_effect), never a value or a message.
	Class string    `json:"class,omitempty"`
	Time  time.Time `json:"time"`
	// CommentID is the remote comment id of a late_write (AC-31).
	CommentID string `json:"comment_id,omitempty"`
	// Effect is the effect id the record is about, when there is one.
	Effect string `json:"effect,omitempty"`
	// Generation, Reason and AckUnswept belong to release_authorized only.
	// Reason is the operator's text: one line of printable ASCII, at most
	// 200 bytes, refused when the unbound credential scan hits.
	Generation int64  `json:"generation,omitempty"`
	Reason     string `json:"reason,omitempty"`
	AckUnswept bool   `json:"ack_unswept,omitempty"`
}

var (
	classRE   = regexp.MustCompile(`^[a-z0-9][a-z0-9_.:-]{0,79}$`)
	remoteRE  = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,80}$`)
	effectRE  = regexp.MustCompile(`^e_[0-9a-f]{24}$`)
	ErrRecord = errors.New("invalid record")
)

// Validate checks every field against its grammar.
func (r Record) Validate() error {
	if _, err := domain.ParseMissionID(string(r.Mission)); err != nil {
		return fmt.Errorf("%w: mission: %v", ErrRecord, err)
	}
	if _, err := domain.ParseAttemptID(string(r.Attempt)); err != nil {
		return fmt.Errorf("%w: attempt: %v", ErrRecord, err)
	}
	if !kinds[r.Kind] {
		return fmt.Errorf("%w: unknown kind", ErrRecord)
	}
	if r.Class != "" && !classRE.MatchString(r.Class) {
		return fmt.Errorf("%w: class must match %s", ErrRecord, classRE)
	}
	if (r.Kind == DeliveryBlocked || r.Kind == MissionHold || r.Kind == ReleaseAuthorized) && r.Class == "" {
		return fmt.Errorf("%w: %s needs a class", ErrRecord, r.Kind)
	}
	if r.Kind == ReleaseAuthorized {
		if r.Generation <= 0 {
			return fmt.Errorf("%w: release_authorized needs the hold generation", ErrRecord)
		}
		if err := checkReason(r.Reason); err != nil {
			return err
		}
	} else if r.Generation != 0 || r.Reason != "" || r.AckUnswept {
		return fmt.Errorf("%w: generation, reason and ack belong to release_authorized only", ErrRecord)
	}
	if r.CommentID != "" && !remoteRE.MatchString(r.CommentID) {
		return fmt.Errorf("%w: comment id must match %s", ErrRecord, remoteRE)
	}
	if r.Kind == LateWrite && r.CommentID == "" {
		return fmt.Errorf("%w: late_write needs the comment id", ErrRecord)
	}
	if r.Effect != "" && !effectRE.MatchString(r.Effect) {
		return fmt.Errorf("%w: effect id", ErrRecord)
	}
	return nil
}

// checkReason admits an operator reason: one line of printable ASCII, at
// most 200 bytes, with no credential shape in it.
func checkReason(reason string) error {
	if reason == "" || len(reason) > 200 || strings.TrimSpace(reason) == "" {
		return fmt.Errorf("%w: reason must be 1-200 bytes", ErrRecord)
	}
	for i := 0; i < len(reason); i++ {
		if c := reason[i]; c < 0x20 || c > 0x7e {
			return fmt.Errorf("%w: reason must be one line of printable ASCII", ErrRecord)
		}
	}
	if hit, class := credential.ScanUnbound(reason); hit {
		return fmt.Errorf("%w: reason refused by the credential scan (%s)", ErrRecord, class)
	}
	return nil
}

// Path is the record file of one attempt under dir ({run area}/records).
func Path(dir string, attempt domain.AttemptID) string {
	return filepath.Join(dir, string(attempt)+".json")
}

// Append adds r to its attempt's file. It is a locked read-modify-write: an
// exclusive flock on dir/.lock, a read of the current array, a new file
// written under a temporary name (O_EXCL, 0600) and fsynced, renamed over the
// old one, then the directory fsynced. Concurrent Appends from any process
// that uses this function all survive. A record identical to one already in
// the file (same kind, class, effect and comment id) is not added twice, so a
// retried settle or loop pass is idempotent.
func Append(dir string, r Record) error {
	if r.Time.IsZero() {
		r.Time = time.Now().UTC()
	}
	r.Time = r.Time.UTC().Truncate(time.Millisecond)
	if err := r.Validate(); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	unlock, err := lock(dir)
	if err != nil {
		return err
	}
	defer unlock()
	cur, err := read(Path(dir, r.Attempt))
	if err != nil {
		return err
	}
	for _, x := range cur {
		if x.Kind == r.Kind && x.Class == r.Class && x.Effect == r.Effect && x.CommentID == r.CommentID && x.Mission == r.Mission && x.Generation == r.Generation {
			return nil
		}
	}
	cur = append(cur, r)
	data, err := json.MarshalIndent(cur, "", " ")
	if err != nil {
		return err
	}
	return writeAtomic(dir, Path(dir, r.Attempt), append(data, '\n'))
}

// Read returns the records of one attempt (none when the file is absent).
func Read(dir string, attempt domain.AttemptID) ([]Record, error) {
	if _, err := domain.ParseAttemptID(string(attempt)); err != nil {
		return nil, fmt.Errorf("%w: attempt: %v", ErrRecord, err)
	}
	return read(Path(dir, attempt))
}

// List returns every record of kind k under dir (all kinds when k is ""),
// ordered by time. `fab effect unresolved` lists delivery_blocked this way.
func List(dir string, k Kind) ([]Record, error) {
	ents, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Record
	for _, e := range ents {
		name := e.Name()
		if !e.Type().IsRegular() || !strings.HasSuffix(name, ".json") {
			continue
		}
		if _, err := domain.ParseAttemptID(strings.TrimSuffix(name, ".json")); err != nil {
			continue
		}
		rs, err := read(filepath.Join(dir, name))
		if err != nil {
			return nil, err
		}
		for _, r := range rs {
			if k == "" || r.Kind == k {
				out = append(out, r)
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Time.Before(out[j].Time) })
	return out, nil
}

func read(path string) ([]Record, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: %s is not a regular file", ErrRecord, filepath.Base(path))
	}
	var rs []Record
	if err := json.NewDecoder(f).Decode(&rs); err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrRecord, filepath.Base(path), err)
	}
	return rs, nil
}

// lock is lockDir; a test swaps it to show the concurrency test catches lost updates.
var lock = lockDir

func lockDir(dir string) (func(), error) {
	f, err := os.OpenFile(filepath.Join(dir, ".lock"), os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN) //nolint:errcheck
		f.Close()
	}, nil
}

// Fault points of writeAtomic, for tests.
const (
	FaultWrite  = "write"
	FaultSync   = "fsync"
	FaultRename = "rename"
)

var testFault func(point string) error

// SetTestFault installs a failure at a writeAtomic fault point; nil removes
// it. Tests only: production never sets it.
func SetTestFault(f func(point string) error) { testFault = f }

func fault(point string) error {
	if testFault != nil {
		return testFault(point)
	}
	return nil
}

func writeAtomic(dir, path string, data []byte) error {
	tmp := path + ".tmp"
	os.Remove(tmp) //nolint:errcheck // a leftover from a crash; we hold the lock
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = fault(FaultWrite)
	}
	if err != nil {
		f.Close()
		os.Remove(tmp) //nolint:errcheck
		return err
	}
	err = f.Sync()
	if err == nil {
		err = fault(FaultSync)
	}
	if err != nil {
		f.Close()
		os.Remove(tmp) //nolint:errcheck
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp) //nolint:errcheck
		return err
	}
	err = fault(FaultRename)
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		os.Remove(tmp) //nolint:errcheck
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
