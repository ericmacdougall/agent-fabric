package sched

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"fabric/internal/store"
	"fabric/internal/storetest"
)

func counterValue(t *testing.T, st *store.Store, name string) int64 {
	t.Helper()
	var v int64
	err := st.Pool().QueryRow(context.Background(), `SELECT coalesce((SELECT value FROM sched_counters WHERE name = $1), 0)`, name).Scan(&v)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// goodqa#15087 adversarial L2: an operator pass logs one line naming the
// host, FABRIC_ACTOR (quoted, so it cannot forge a second line), the pid
// and the counts, with no DSN or password in it, and bumps
// reconcile_operator_passes_total. A refused pass (the lock is held) logs
// no pass line and leaves the counter unmoved.
func TestOperatorReconcileLogAndCounter(t *testing.T) {
	ctx := context.Background()
	st := storetest.Open(t)
	lapsedAttempt(t, st, "m_op", "r_op", hostA)
	ext, err := st.TryReconcileLock(ctx, hostA)
	if err != nil {
		t.Fatal(err)
	}
	var log bytes.Buffer
	if _, err := OperatorReconcile(ctx, st, hostA, hostA, goneProber(), nil, "op:alice", &log); !errors.Is(err, store.ErrReconcileBusy) {
		t.Fatalf("pass under a held lock: %v, want ErrReconcileBusy", err)
	}
	if log.Len() != 0 || counterValue(t, st, OperatorPassCounter) != 0 {
		t.Fatalf("refused pass logged %q, counter %d; want nothing", log.String(), counterValue(t, st, OperatorPassCounter))
	}
	ext.Release()

	actor := "op:alice\nfab reconcile: operator pass host=forged"
	r, err := OperatorReconcile(ctx, st, hostA, hostA, goneProber(), nil, actor, &log)
	if err != nil {
		t.Fatal(err)
	}
	line := log.String()
	if strings.Count(line, "\n") != 1 || !strings.HasSuffix(line, "\n") {
		t.Fatalf("log %q, want exactly one line", line)
	}
	for _, want := range []string{
		"fab reconcile: operator pass host=" + string(hostA),
		fmt.Sprintf("actor=%q", actor),
		fmt.Sprintf("pid=%d", os.Getpid()),
		fmt.Sprintf("expired=%d", len(r.Expired)),
		fmt.Sprintf("quarantined=%d", r.Quarantined),
	} {
		if !strings.Contains(line, want) {
			t.Fatalf("log %q lacks %q", line, want)
		}
	}
	if len(r.Expired) != 1 {
		t.Fatalf("expired %d, want the lapsed attempt", len(r.Expired))
	}
	for _, secret := range []string{storetest.DSN, "postgres://", "password"} {
		if secret != "" && strings.Contains(line, secret) {
			t.Fatal("the pass line carries connection details")
		}
	}
	if v := counterValue(t, st, OperatorPassCounter); v != 1 {
		t.Fatalf("%s = %d after one pass, want 1", OperatorPassCounter, v)
	}
}
