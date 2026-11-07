package effects

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Fake is an in-memory remote with GoodQA-like semantics: a comment is
// deduplicated by its remote key, and readback can list what was applied.
// Fault switches reproduce the N2 cutpoints: a response lost after the remote
// applied the write, and a request lost before it reached the remote.
type Fake struct {
	mu        sync.Mutex
	applied   map[string]json.RawMessage // remote key -> body
	order     []string
	LoseAfter bool // apply, then return UNKNOWN (response lost)
	LoseBefor bool // do not apply, return UNKNOWN (request lost)
	Reject    bool // definite 4xx before any write
	// Hang blocks every call until its context ends (a remote that never
	// answers, for the Executor deadline contract). Stall sleeps that long
	// ignoring the context: an executor that breaks the contract.
	Hang  bool
	Stall time.Duration
}

// wait applies Hang and Stall before a call.
func (f *Fake) wait(ctx context.Context) error {
	if f.Stall > 0 {
		time.Sleep(f.Stall)
	}
	if f.Hang {
		<-ctx.Done()
		return ctx.Err()
	}
	return nil
}

func NewFake() *Fake { return &Fake{applied: map[string]json.RawMessage{}} }

func (f *Fake) Kind() string { return "ticket_comment" }

func (f *Fake) Validate(spec IntentSpec) error {
	var body struct {
		Ticket string `json:"ticket"`
		Body   string `json:"body"`
	}
	if err := json.Unmarshal(spec.Request, &body); err != nil || body.Ticket == "" || body.Body == "" {
		return errors.New("ticket_comment needs ticket and body")
	}
	return nil
}

func (f *Fake) Precondition(ctx context.Context, e Effect) (bool, string, error) {
	if err := f.wait(ctx); err != nil {
		return false, "", err
	}
	return true, "", nil
}

func (f *Fake) Dispatch(ctx context.Context, sub Submission) (Receipt, error) {
	if err := f.wait(ctx); err != nil {
		return Receipt{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Reject {
		return Receipt{Outcome: OutcomeFailedDefinite, Detail: "remote refused before any write (4xx)"}, nil
	}
	if f.LoseBefor {
		return Receipt{}, errors.New("connection dropped before the request was sent")
	}
	if _, dup := f.applied[sub.RemoteKey]; !dup {
		f.applied[sub.RemoteKey] = sub.Effect.Request
		f.order = append(f.order, sub.RemoteKey)
	}
	if f.LoseAfter {
		return Receipt{}, errors.New("connection dropped after the request was sent")
	}
	return Receipt{Outcome: OutcomeConfirmed, Remote: json.RawMessage(fmt.Sprintf(`{"comment_id":"c_%s"}`, sub.RemoteKey))}, nil
}

func (f *Fake) Reconcile(ctx context.Context, e Effect) (Observation, string, error) {
	if err := f.wait(ctx); err != nil {
		return ObsUnknown, "", err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	body, ok := f.applied[e.IdemKey]
	if !ok {
		return ObsNotApplied, "no remote entry for the key", nil
	}
	if string(body) != string(e.Request) {
		return ObsConflict, "remote entry differs from the request", nil
	}
	return ObsCommitted, "remote entry matches the key and body", nil
}

// Count is the number of distinct writes the remote accepted.
func (f *Fake) Count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.applied)
}
