package ingest

import (
	"context"
	"fmt"
	"io"
	"sort"
)

// maxRejectLines caps the per-line rejection report on stderr.
const maxRejectLines = 20

// Result is what one Run did.
type Result struct {
	Batch  Batch
	Stored Stored
}

// Run is one `fab control ingest` call: parse stdin under the limits, store the
// accepted events, and report counts on stderr. A call over a limit is refused
// whole (ErrCallTooLarge) before anything is written; a bad line is rejected on
// its own and the rest continue. Reasons printed are fixed codes, never input text.
func Run(ctx context.Context, db DB, in io.Reader, source string, stderr io.Writer) (Result, error) {
	b, err := Parse(in, source)
	if err != nil {
		return Result{}, err
	}
	st, err := Store(ctx, db, b.Accepted)
	if err != nil {
		return Result{}, err
	}
	res := Result{Batch: b, Stored: st}
	fmt.Fprintf(stderr, "ingest: source=%s lines=%d accepted=%d inserted=%d existing=%d duplicate-in-input=%d rejected=%d\n",
		source, b.Lines, len(b.Accepted), st.Inserted, st.Existing, b.Dups, len(b.Rejected))
	byReason := map[string]int{}
	for _, r := range b.Rejected {
		byReason[r.Reason]++
	}
	reasons := make([]string, 0, len(byReason))
	for r := range byReason {
		reasons = append(reasons, r)
	}
	sort.Strings(reasons)
	for _, r := range reasons {
		fmt.Fprintf(stderr, "ingest: rejected %d: %s\n", byReason[r], r)
	}
	for i, r := range b.Rejected {
		if i == maxRejectLines {
			fmt.Fprintf(stderr, "ingest: ... %d more rejected lines not listed\n", len(b.Rejected)-maxRejectLines)
			break
		}
		fmt.Fprintf(stderr, "ingest: line %d rejected: %s\n", r.Line, r.Reason)
	}
	return res, nil
}
