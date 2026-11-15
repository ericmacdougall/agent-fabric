package crashpoint

import (
	"fmt"
	"strings"
)

// Point is one crash point: a durable write on an M1 path.
type Point struct {
	Name      string
	Path      string // launch, hold, finish, settle, release or recovery
	Order     int    // its place in the path
	Kind      string // db, file, remove, rename or marker
	Effect    string // what becomes durable
	Invariant string // the invariants (I1..I6) a crash at this point tests
}

// Table is every crash point (goodqa#15966 crash-point brief section 2).
// TestEveryDurableWriteIsListed keeps it and the code in step.
var Table = []Point{
	{"launch.start", "launch", 1, "db", "START_ATTEMPT: the attempt row, LEASED", "I2"},
	{"launch.binding", "launch", 2, "file", "<attempt>.json: the binding pins a token version, before any runner byte (AC-14)", "I2, I6"},
	{"launch.ack", "launch", 3, "db", "ACK_START: the launch boundary; no ACK proves no launch", "I2, I6"},
	{"settle.redact.clear", "settle", 1, "remove", "a partial redacted copy is discarded before a redo", "I4, I6"},
	{"settle.redact.done", "settle", 2, "marker", "redacted.done: both redacted copies are durable; the summary decides the class", "I1, I4, I6"},
	{"settle.usage", "settle", 3, "file", "usage.json beside the stream (token counts only)", "I4"},
	{"settle.sweep.record", "settle", 4, "file", "the sweep record (counts only)", "I4, I6"},
	{"settle.record", "settle", 5, "file", "a delivery_blocked, credential_exposure or sweep_incomplete record", "I1, I4"},
	{"settle.material", "settle", 6, "file", "<attempt>.material: sweep keys and fingerprints for a re-sweep", "I6"},
	{"settle.raw.remove", "settle", 7, "remove", "a raw stream file is removed after a clean redaction", "I4, I6"},
	{"settle.mark", "settle", 8, "marker", "<attempt>.settled: the binding no longer pins its token version", "I1, I4, I6"},
	{"settle.gc", "settle", 9, "remove", "token versions no unsettled binding pins are removed", "I6"},
	{"settle.unstarted.mark", "settle", 10, "marker", "<attempt>.settled for a binding whose worker never started", "I6"},
	{"settle.unstarted.gc", "settle", 11, "remove", "GC after an unstarted binding", "I6"},
	{"hold.operator", "hold", 1, "db", "the sched.ophold row (AC-34 operator hold), before FINISH", "I1, I2, I4"},
	{"hold.record", "hold", 2, "file", "the mission_hold record with its class", "I1, I3"},
	{"finish.m1", "finish", 1, "db", "FINISH_ATTEMPT (with the effect prefix) via finishRetry", "I1, I2, I5"},
	{"finish.hold.w3", "finish", 2, "db", "W3's mission hold after FINISH on a fail-closed settle", "I1, I2"},
	{"finish.prelaunch", "finish", 3, "db", "FINISH_ATTEMPT abandoned before the runner started (then the unstarted settle)", "I2, I6"},
	{"release.record", "release", 1, "file", "release_authorized for generation G, under the row lock", "I3"},
	{"release.commit", "release", 2, "db", "the hold row of generation G is deleted and the transaction commits", "I2, I3"},
	{"recovery.expire", "recovery", 1, "db", "EXPIRE_LEASE: a lapsed lease becomes UNKNOWN", "I1, I2"},
	{"recovery.quarantine", "recovery", 2, "db", "RECONCILE_ATTEMPT QUARANTINED: the runner is gone, outcome unverified", "I1, I2"},
	{"recovery.unverified.record", "recovery", 3, "file", "sweep_incomplete (restart_unverified)", "I1, I6"},
	{"recovery.material", "recovery", 4, "file", "<attempt>.material for the unverified attempt", "I6"},
	{"recovery.hold", "recovery", 5, "db", "the operator hold placed by restart recovery, before MarkSettled", "I1, I2, I4"},
	{"recovery.hold.record", "recovery", 6, "file", "the recovery's mission_hold record", "I1, I3"},
}

// Markdown renders the table for the PR evidence.
func Markdown() string {
	var b strings.Builder
	b.WriteString("| Point | Path | Order | Kind | Effect | Invariants |\n|---|---|---|---|---|---|\n")
	for _, p := range Table {
		fmt.Fprintf(&b, "| `%s` | %s | %d | %s | %s | %s |\n", p.Name, p.Path, p.Order, p.Kind, p.Effect, p.Invariant)
	}
	return b.String()
}
