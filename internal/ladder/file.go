package ladder

// Reading escalation.py state and model_tiers.json from disk for
// `fab ladder status` (goodqa#15324). Inputs are bounded, regular,
// non-symlink files; output carries ids, counts and rung names only.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// MaxInput is escalation.py's MAX_STATE.
const MaxInput = 1 << 20

// ErrInput marks a refused input file.
var ErrInput = errors.New("ladder input refused")

// ReadInput reads a regular, non-symlink file of at most MaxInput bytes.
func ReadInput(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrInput, path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%w: %s is a symlink", ErrInput, path)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: %s is not a regular file", ErrInput, path)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrInput, path, err)
	}
	defer f.Close()
	body, err := io.ReadAll(io.LimitReader(f, MaxInput+1))
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrInput, path, err)
	}
	if len(body) > MaxInput {
		return nil, fmt.Errorf("%w: %s is larger than %d bytes", ErrInput, path, MaxInput)
	}
	return body, nil
}

func decodeInput(path string, body []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("%w: %s: malformed JSON: %v", ErrInput, path, err)
	}
	return nil
}

// StatusFromFiles computes Status from a state file and a policy file.
func StatusFromFiles(statePath, policyPath string) (LadderStatus, error) {
	stateBody, err := ReadInput(statePath)
	if err != nil {
		return LadderStatus{}, err
	}
	policyBody, err := ReadInput(policyPath)
	if err != nil {
		return LadderStatus{}, err
	}
	var st LedgerState
	if err := decodeInput(statePath, stateBody, &st); err != nil {
		return LadderStatus{}, err
	}
	// escalation.py treats a state with another schema, no task or no
	// event list as corrupt and resets it to the lowest rung. A read-only
	// viewer must not guess: it refuses, naming the reason.
	var shape struct {
		Schema json.Number      `json:"schema"`
		Task   *string          `json:"task"`
		Events *json.RawMessage `json:"events"`
	}
	if err := decodeInput(statePath, stateBody, &shape); err != nil {
		return LadderStatus{}, err
	}
	if shape.Schema.String() != "1" || shape.Task == nil || *shape.Task == "" || shape.Events == nil || string(*shape.Events) == "null" {
		return LadderStatus{}, fmt.Errorf("%w: %s: not an escalation.py schema 1 state (schema, task and events are required)", ErrInput, statePath)
	}
	var pol Policy
	if err := decodeInput(policyPath, policyBody, &pol); err != nil {
		return LadderStatus{}, err
	}
	return Status(pol, st)
}

// Format renders a status as text, or as indented JSON.
func Format(s LadderStatus, asJSON bool) (string, error) {
	if asJSON {
		b, err := json.MarshalIndent(s, "", " ")
		if err != nil {
			return "", err
		}
		return string(b) + "\n", nil
	}
	fp := "-"
	if s.Fingerprint != nil {
		fp = *s.Fingerprint
		if len(fp) > 12 {
			fp = fp[:12]
		}
	}
	blocked := "-"
	if len(s.BlockedBy) > 0 {
		blocked = strings.Join(s.BlockedBy, ",")
	}
	return fmt.Sprintf("task %s\nrung %s\nfailures_at_rung %d\nclimbs %d\nfingerprint %s\nblocked_by %s\nneeds_user_permission %t\n",
		s.Task, s.Rung, s.FailuresAtRung, s.Climbs, fp, blocked, s.NeedsUserPermission), nil
}
