package credential

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Settle failure classes (AC-18), recorded as delivery_blocked classes.
const (
	ClassMaterialUnavailable = "credential_material_unavailable"
	ClassRotatedMidAttempt   = "credential_rotated_mid_attempt"
)

// SettleError is a fail-closed settle refusal. Class is the
// delivery_blocked class; the error text never holds a credential byte.
type SettleError struct {
	Class  string
	Detail string
}

func (e *SettleError) Error() string { return "credential: " + e.Class + ": " + e.Detail }

func settleErr(class, format string, a ...any) error {
	return &SettleError{Class: class, Detail: fmt.Sprintf(format, a...)}
}

var attemptRE = regexp.MustCompile(`^[A-Za-z0-9_-][A-Za-z0-9_.-]{0,127}$`)

const (
	idKeyName    = "credential-id.key"
	maxBinding   = 64 << 10
	retainedName = "retained.json"
)

// LoadIDKey returns the 32-byte version-id key in stateDir, creating it
// (mode 0600, O_EXCL) when absent. It is used for nothing else.
func LoadIDKey(stateDir string) ([]byte, error) {
	path := filepath.Join(stateDir, idKeyName)
	f, err := os.OpenFile(path, os.O_RDONLY|oNoFollow, 0)
	if errors.Is(err, os.ErrNotExist) {
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, err
		}
		if err := writeDurable(path, key, false); err != nil {
			if errors.Is(err, os.ErrExist) {
				return LoadIDKey(stateDir)
			}
			return nil, fmt.Errorf("credential: id key: %w", err)
		}
		return key, nil
	}
	if err != nil {
		return nil, fmt.Errorf("credential: id key: %w", err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Mode().Perm() != 0o600 || st.Size() != 32 {
		return nil, errors.New("credential: id key must be a 32-byte regular file of mode 0600")
	}
	key := make([]byte, 32)
	if _, err := io.ReadFull(f, key); err != nil {
		return nil, fmt.Errorf("credential: id key: %w", err)
	}
	return key, nil
}

// VersionID is the version id of an unversioned credential: the first 16
// hex of HMAC-SHA256(id key, value).
func VersionID(idKey, value []byte) string {
	m := hmac.New(sha256.New, idKey)
	m.Write(value)
	return hex.EncodeToString(m.Sum(nil))[:16]
}

// OtherCredential names one unversioned credential fabd holds, with its
// current value (the GoodQA key or a DSN password).
type OtherCredential struct {
	Kind  Kind
	Name  string // e.g. "runner", "app"; unique per kind
	Value []byte
}

// BoundID is one version id in a binding record.
type BoundID struct {
	Kind Kind   `json:"kind"`
	Name string `json:"name"`
	ID   string `json:"id"`
}

// Binding is the per-attempt binding record (AC-18). It holds version ids
// only, never a value or a representation of one.
type Binding struct {
	Attempt      string    `json:"attempt"`
	Mission      string    `json:"mission"`
	TokenVersion int       `json:"token_version"`
	Others       []BoundID `json:"others"`
}

// Durable is the handle the durable binding write returns. Only Launch
// accepts it, so no code path starts a worker without the record.
type Durable struct {
	path    string
	binding Binding
	token   Secret
}

// Binding returns the durable record.
func (d *Durable) Binding() Binding { return d.binding }

// Token returns the bound token version for the claude launch environment.
func (d *Durable) Token() Secret { return d.token }

// ErrNoBinding refuses a launch without a durable binding record.
var ErrNoBinding = errors.New("credential: launch refused: no durable binding record")

// Launch runs start only for a durable binding whose record is still on
// disk.
func Launch(d *Durable, start func() error) error {
	if d == nil || d.path == "" {
		return ErrNoBinding
	}
	if _, err := os.Lstat(d.path); err != nil {
		return ErrNoBinding
	}
	return start()
}

func ids(idKey []byte, others []OtherCredential) ([]BoundID, error) {
	out := make([]BoundID, 0, len(others))
	seen := map[string]bool{}
	for _, o := range others {
		if o.Kind == KindToken || len(o.Value) == 0 || o.Name == "" {
			return nil, fmt.Errorf("credential: invalid bound credential %s:%s", o.Kind, o.Name)
		}
		k := string(o.Kind) + "/" + o.Name
		if seen[k] {
			return nil, fmt.Errorf("credential: duplicate bound credential %s", k)
		}
		seen[k] = true
		out = append(out, BoundID{Kind: o.Kind, Name: o.Name, ID: VersionID(idKey, o.Value)})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Kind < out[j].Kind || out[i].Kind == out[j].Kind && out[i].Name < out[j].Name
	})
	return out, nil
}

// PrepareLaunch is the production launch-side loader: under the rotate lock
// it loads the current token (every AC-13 check, the length rule), writes
// the attempt's binding record durably, and only then releases the lock
// (AC-18), so GC always sees the binding. A failure starts nothing.
func (p Policy) PrepareLaunch(bindingsDir string, idKey []byte, mission, attempt string, others []OtherCredential) (*Durable, error) {
	if !attemptRE.MatchString(attempt) {
		return nil, errors.New("credential: invalid attempt id")
	}
	lk, err := p.LockRotate()
	if err != nil {
		return nil, err
	}
	defer lk.Unlock()
	tok, err := p.LoadCurrent()
	if err != nil {
		return nil, err
	}
	oids, err := ids(idKey, others)
	if err != nil {
		return nil, err
	}
	rec := Binding{Attempt: attempt, Mission: mission, TokenVersion: tok.Version, Others: oids}
	data, err := json.Marshal(rec)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(bindingsDir, attempt+".json")
	if err := p.writeBinding(path, data); err != nil {
		return nil, fmt.Errorf("credential: binding record not durable, launch refused: %w", err)
	}
	return &Durable{path: path, binding: rec, token: tok.Secret}, nil
}

// writeBinding writes under a temporary name, fsyncs, renames and fsyncs the
// directory, with the test fault seams.
func (p Policy) writeBinding(path string, data []byte) error {
	tmp := path + ".tmp"
	os.Remove(tmp)
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|oNoFollow, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil && p.faults != nil && p.faults.beforeSync != nil {
		err = p.faults.beforeSync()
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && p.faults != nil && p.faults.beforeRename != nil {
		err = p.faults.beforeRename()
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	return syncDir(filepath.Dir(path))
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// writeDurable writes a new file (O_EXCL unless replace), fsyncs it and the
// directory. replace writes a temporary and renames it over path.
func writeDurable(path string, data []byte, replace bool) error {
	target := path
	if replace {
		target = path + ".tmp"
		os.Remove(target)
	}
	f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL|oNoFollow, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && replace {
		err = os.Rename(target, path)
	}
	if err != nil {
		os.Remove(target)
		return err
	}
	return syncDir(filepath.Dir(path))
}

// ReadBinding reads and strictly parses an attempt's binding record.
func ReadBinding(bindingsDir, attempt string) (Binding, error) {
	if !attemptRE.MatchString(attempt) {
		return Binding{}, settleErr(ClassMaterialUnavailable, "invalid attempt id")
	}
	f, err := os.OpenFile(filepath.Join(bindingsDir, attempt+".json"), os.O_RDONLY|oNoFollow, 0)
	if err != nil {
		return Binding{}, settleErr(ClassMaterialUnavailable, "binding record unreadable")
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxBinding+1))
	if err != nil || len(data) > maxBinding {
		return Binding{}, settleErr(ClassMaterialUnavailable, "binding record unreadable")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var b Binding
	if err := dec.Decode(&b); err != nil || dec.More() {
		return Binding{}, settleErr(ClassMaterialUnavailable, "binding record malformed")
	}
	if b.Attempt != attempt || b.TokenVersion < 0 {
		return Binding{}, settleErr(ClassMaterialUnavailable, "binding record malformed")
	}
	for _, o := range b.Others {
		if len(o.ID) != 16 || o.Kind == KindToken || o.Name == "" {
			return Binding{}, settleErr(ClassMaterialUnavailable, "binding record malformed")
		}
	}
	return b, nil
}

// LoadBound is the settle-side loader (AC-18): it reads the binding record,
// rechecks the unversioned credentials fabd now holds against their bound
// version ids, opens the bound token version by id with every AC-13 check,
// and builds the matcher. Any failure is a *SettleError with the class
// credential_material_unavailable or credential_rotated_mid_attempt.
func (p Policy) LoadBound(bindingsDir, attempt string, idKey []byte, current []OtherCredential) (*Bound, error) {
	rec, err := ReadBinding(bindingsDir, attempt)
	if err != nil {
		return nil, err
	}
	now, err := ids(idKey, current)
	if err != nil {
		return nil, settleErr(ClassMaterialUnavailable, "current credentials invalid")
	}
	if len(now) != len(rec.Others) {
		return nil, settleErr(ClassRotatedMidAttempt, "the set of held credentials changed")
	}
	for i := range now {
		if now[i] != rec.Others[i] {
			return nil, settleErr(ClassRotatedMidAttempt, "%s:%s changed during the attempt", now[i].Kind, now[i].Name)
		}
	}
	tok, err := p.LoadVersion(rec.TokenVersion)
	if err != nil {
		var r *Refusal
		reason := "check failed"
		if errors.As(err, &r) {
			reason = r.Reason
		}
		return nil, settleErr(ClassMaterialUnavailable, "bound token %s: %s", VersionName(rec.TokenVersion), reason)
	}
	secrets := make([]Secret, 0, len(current))
	for _, o := range current {
		secrets = append(secrets, NewSecret(o.Kind, VersionID(idKey, o.Value), o.Value))
	}
	b, err := NewBound(tok, secrets...)
	if err != nil {
		return nil, settleErr(ClassMaterialUnavailable, "bound token unusable")
	}
	return b, nil
}

// MarkSettled records that the attempt's settle has finished (redaction, the
// pre-submit scan and the sweep are recorded, or settle failed closed), so
// GC may delete its token version.
func MarkSettled(bindingsDir, attempt string) error {
	if !attemptRE.MatchString(attempt) {
		return errors.New("credential: invalid attempt id")
	}
	err := writeDurable(filepath.Join(bindingsDir, attempt+".settled"), []byte("settled\n"), false)
	if errors.Is(err, os.ErrExist) {
		return nil
	}
	return err
}

// SaveMaterial keeps an incomplete sweep's input (keys and fingerprints, no
// value) as {attempt}.material, mode 0600 (AC-18).
func SaveMaterial(bindingsDir, attempt string, data []byte) error {
	if !attemptRE.MatchString(attempt) {
		return errors.New("credential: invalid attempt id")
	}
	return writeDurable(filepath.Join(bindingsDir, attempt+".material"), data, true)
}

// Retained is one entry of retained.json.
type Retained struct {
	Attempt  string `json:"attempt"`
	Material string `json:"material"`
}

// GCReport says what GC kept and removed.
type GCReport struct {
	Removed  []int
	Kept     []int
	Retained []Retained
}

// GCLocked deletes every claude-oauth.v{n} that is neither current nor bound
// to an attempt whose settle has not finished, and lists retained sweep
// material in retained.json (AC-18). A malformed binding record makes GC
// delete nothing.
func (p Policy) GCLocked(lk *Lock, bindingsDir string) (GCReport, error) {
	var rep GCReport
	if lk == nil || lk.fd < 0 {
		return rep, errors.New("credential: gc needs the rotate lock")
	}
	ents, err := os.ReadDir(bindingsDir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return rep, err
	}
	settled := map[string]bool{}
	for _, e := range ents {
		if a, ok := strings.CutSuffix(e.Name(), ".settled"); ok {
			settled[a] = true
		}
	}
	keep := map[int]bool{}
	for _, e := range ents {
		name := e.Name()
		if a, ok := strings.CutSuffix(name, ".material"); ok {
			rep.Retained = append(rep.Retained, Retained{Attempt: a, Material: name})
			continue
		}
		a, ok := strings.CutSuffix(name, ".json")
		if !ok || name == retainedName || strings.HasSuffix(name, ".tmp") {
			continue
		}
		if settled[a] {
			continue
		}
		b, err := ReadBinding(bindingsDir, a)
		if err != nil {
			return rep, fmt.Errorf("credential: gc: binding %s unreadable; nothing deleted", a)
		}
		keep[b.TokenVersion] = true
	}
	h, err := p.openDir()
	if err != nil {
		return rep, err
	}
	defer h.close()
	cur, hasCur := h.entries[CurrentName]
	for _, n := range h.versions() {
		st := h.entries[VersionName(n)]
		if hasCur && st.dev == cur.dev && st.ino == cur.ino || keep[n] {
			rep.Kept = append(rep.Kept, n)
			continue
		}
		if err := unixUnlinkat(h.fd, VersionName(n)); err != nil {
			return rep, err
		}
		rep.Removed = append(rep.Removed, n)
	}
	if err := unixFsync(h.fd); err != nil {
		return rep, err
	}
	sort.Slice(rep.Retained, func(i, j int) bool { return rep.Retained[i].Attempt < rep.Retained[j].Attempt })
	if len(ents) > 0 || len(rep.Retained) > 0 {
		data, _ := json.Marshal(struct {
			Retained []Retained `json:"retained"`
		}{append([]Retained{}, rep.Retained...)})
		if err := writeDurable(filepath.Join(bindingsDir, retainedName), data, true); err != nil {
			return rep, err
		}
	}
	return rep, nil
}

// GC takes the rotate lock and runs GCLocked (after every settle).
func (p Policy) GC(bindingsDir string) (GCReport, error) {
	lk, err := p.LockRotate()
	if err != nil {
		return GCReport{}, err
	}
	defer lk.Unlock()
	return p.GCLocked(lk, bindingsDir)
}

// Unsettled reports whether attempt has a binding record and no settled
// marker (spec v16 prior_attempt_unsettled). An attempt that never got a
// binding (refused or abandoned before it, or a pre-M1 attempt) is not
// unsettled: no token version was bound to it. Lstat, so a planted link
// counts as present and fails closed.
func Unsettled(bindingsDir, attempt string) (bool, error) {
	if _, err := os.Lstat(filepath.Join(bindingsDir, attempt+".json")); errors.Is(err, os.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return true, err
	}
	if _, err := os.Lstat(filepath.Join(bindingsDir, attempt+".settled")); errors.Is(err, os.ErrNotExist) {
		return true, nil
	} else if err != nil {
		return true, err
	}
	return false, nil
}
