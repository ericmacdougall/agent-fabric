// Package metrics reads the fabric's health from the store and renders it in
// the Prometheus text exposition format (version 0.0.4), with the standard
// library only.
//
// Everything here is computed from the database inside one read-only
// transaction, so `fab metrics` (a separate process) and a scrape see the
// same numbers. Live per-process counters (a broker's executor overruns, its
// dispatch slots in use) are not visible from another process; they arrive
// later with an in-process serve-metrics in the daemon. Their durable
// stand-ins are fabric_dispatch_overdue and fabric_dispatch_inflight.
//
// Labels carry ids and enum values only, through Label: never ticket text,
// prompts, paths, subjects, DSNs or account names.
package metrics

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DefaultTimeout bounds one collection: statement and lock waits both.
const DefaultTimeout = 2 * time.Second

// Cap is a configured spending ceiling exported as a constant gauge.
type Cap struct {
	Scope  string // "month", "job", "live_test"
	Micros int64  // USD micros
}

// Options configures one collection.
type Options struct {
	Timeout       time.Duration // statement_timeout and lock_timeout; DefaultTimeout when 0
	DispatchLimit int           // the configured effect dispatch limit (0: not exported)
	Caps          []Cap
	Version       string
}

// Sample is one line of a family.
type Sample struct {
	Labels [][2]string
	Value  float64
}

// Family is one metric with its help text and type.
type Family struct {
	Name, Help, Type string
	Samples          []Sample
}

// Snapshot is one collection.
type Snapshot struct {
	Families []Family
}

// labelRE is the allow-list for label values: ids and enum values only.
var labelRE = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,64}$`)

// Label returns v when it is a plain id or enum value and "other" otherwise,
// so a hostile or free-text column value can never reach the output.
func Label(v string) string {
	if labelRE.MatchString(v) {
		return v
	}
	return "other"
}

// afterQueries is a test cutpoint: it runs inside the collection
// transaction after every query, before commit.
var afterQueries func()

// liveEffectStates are the effect states whose oldest age is exported.
var liveEffectStates = []string{"QUARANTINED", "RECONCILING", "UNKNOWN"}

// Collect reads one snapshot in a single read-only transaction whose
// statement_timeout and lock_timeout are both Options.Timeout, so it can
// neither wait long on a lock nor hold one: it takes no row locks, and a
// dispatch never waits for it.
func Collect(ctx context.Context, pool *pgxpool.Pool, o Options) (*Snapshot, error) {
	timeout := o.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout+time.Second)
	defer cancel()
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly, IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return nil, fmt.Errorf("metrics: begin: %w", err)
	}
	defer tx.Rollback(context.Background()) //nolint:errcheck // read-only
	ms := strconv.FormatInt(timeout.Milliseconds(), 10)
	for _, q := range []string{"SET LOCAL statement_timeout = " + ms, "SET LOCAL lock_timeout = " + ms} {
		if _, err := tx.Exec(ctx, q); err != nil {
			return nil, fmt.Errorf("metrics: %w", err)
		}
	}
	c := &collector{ctx: ctx, tx: tx}
	s := &Snapshot{}
	add := func(f Family) { s.Families = append(s.Families, f) }

	add(Family{Name: "fabric_build_info", Help: "Build version of the reporting binary (always 1).", Type: "gauge",
		Samples: []Sample{{Labels: [][2]string{{"version", Label(o.Version)}}, Value: 1}}})

	var applied int64
	var latest string
	c.row(`SELECT count(*), COALESCE(max(name), '') FROM fabric_schema`, &applied, &latest)
	add(gauge("fabric_schema_migrations_applied", "Migrations recorded in fabric_schema.", float64(applied)))
	add(gauge("fabric_schema_level", "Numeric prefix of the latest applied migration.", float64(migrationLevel(latest))))

	add(c.grouped("fabric_missions", "Missions by execution and terminal state.", "gauge",
		`SELECT execution, terminal, count(*) FROM missions GROUP BY 1, 2`, "execution", "terminal"))
	add(c.grouped("fabric_attempts_active", "Active attempts by state and lease freshness (live: lease unexpired).", "gauge",
		`SELECT state, CASE WHEN lease_until > now() THEN 'live' ELSE 'stale' END, count(*)
		   FROM attempts WHERE state IN ('LEASED', 'PREPARING', 'RUNNING') GROUP BY 1, 2`, "state", "lease"))
	add(c.grouped("fabric_effects", "Effects by kind and state.", "gauge",
		`SELECT kind, state, count(*) FROM effects GROUP BY 1, 2`, "kind", "state"))

	ages := map[string]float64{}
	for _, st := range liveEffectStates {
		ages[st] = 0
	}
	c.each(`SELECT state, floor(EXTRACT(EPOCH FROM now() - min(updated_at)))::bigint FROM effects
		WHERE state = ANY($1) GROUP BY 1`, func(r pgx.Rows) error {
		var st string
		var age int64
		if err := r.Scan(&st, &age); err != nil {
			return err
		}
		ages[st] = float64(age)
		return nil
	}, liveEffectStates)
	ageFam := Family{Name: "fabric_effect_oldest_age_seconds", Help: "Age of the oldest effect in a state needing attention (0: none).", Type: "gauge"}
	for _, st := range liveEffectStates {
		ageFam.Samples = append(ageFam.Samples, Sample{Labels: [][2]string{{"state", st}}, Value: ages[st]})
	}
	add(ageFam)

	var inflight, overdue int64
	c.row(`SELECT count(*) FROM effects WHERE state = 'DISPATCHED'`, &inflight)
	c.row(`SELECT count(*) FROM effects e JOIN effect_dispatches d ON d.effect_id = e.id AND d.dispatch = e.dispatches
		WHERE e.state = 'DISPATCHED' AND d.deadline < now()`, &overdue)
	add(gauge("fabric_dispatch_inflight", "Effects in DISPATCHED (a transport may be running).", float64(inflight)))
	add(gauge("fabric_dispatch_overdue", "DISPATCHED effects past the transport deadline stamped at dispatch.", float64(overdue)))
	if o.DispatchLimit > 0 {
		add(gauge("fabric_dispatch_limit", "Configured concurrent effect dispatches per store.", float64(o.DispatchLimit)))
	}

	add(c.grouped("fabric_sched_counter_total", "Durable scheduler counters (sched_counters).", "counter",
		`SELECT name, value FROM sched_counters`, "name"))

	add(c.grouped("fabric_resource_capacity", "Configured capacity per resource.", "gauge",
		`SELECT resource, unit, capacity FROM resource_capacities`, "resource", "unit"))
	add(c.grouped("fabric_resource_held", "Amount currently held per resource.", "gauge",
		`SELECT resource, unit, held FROM resource_capacities`, "resource", "unit"))
	add(c.grouped("fabric_resource_spent_month", "Settled usage this UTC calendar month per resource.", "gauge",
		`SELECT r.resource, r.unit, COALESCE(sum(u.actual), 0) FROM usage_receipts u
		   JOIN reservations r ON r.id = u.reservation_id
		  WHERE u.at >= date_trunc('month', now() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC'
		  GROUP BY 1, 2`, "resource", "unit"))

	var spent int64
	c.row(`SELECT COALESCE(sum(cost_micros), 0) FROM agent_messages
		WHERE created_at >= date_trunc('month', now() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC'`, &spent)
	add(gauge("fabric_native_spent_month_usd_micros", "Native provider cost recorded this UTC calendar month, in USD micros.", float64(spent)))
	capFam := Family{Name: "fabric_budget_cap_usd_micros", Help: "Configured spending ceilings in USD micros (constants).", Type: "gauge"}
	for _, cp := range o.Caps {
		capFam.Samples = append(capFam.Samples, Sample{Labels: [][2]string{{"scope", Label(cp.Scope)}}, Value: float64(cp.Micros)})
	}
	if len(capFam.Samples) > 0 {
		add(capFam)
	}

	add(c.grouped("fabric_asks_open", "Open asks by kind.", "gauge",
		`SELECT kind, count(*) FROM asks WHERE state = 'open' GROUP BY 1`, "kind"))
	var oldestAsk int64
	c.row(`SELECT COALESCE(floor(EXTRACT(EPOCH FROM now() - min(created_at)))::bigint, 0) FROM asks WHERE state = 'open'`, &oldestAsk)
	add(gauge("fabric_asks_open_oldest_age_seconds", "Age of the oldest open ask (0: none).", float64(oldestAsk)))

	if c.err != nil {
		return nil, fmt.Errorf("metrics: %w", c.err)
	}
	if afterQueries != nil {
		afterQueries()
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("metrics: commit: %w", err)
	}
	return s, nil
}

// collector runs queries until the first error, which it keeps.
type collector struct {
	ctx context.Context
	tx  pgx.Tx
	err error
}

func (c *collector) row(q string, dest ...any) {
	if c.err != nil {
		return
	}
	c.err = c.tx.QueryRow(c.ctx, q).Scan(dest...)
}

func (c *collector) each(q string, f func(pgx.Rows) error, args ...any) {
	if c.err != nil {
		return
	}
	rows, err := c.tx.Query(c.ctx, q, args...)
	if err != nil {
		c.err = err
		return
	}
	defer rows.Close()
	for rows.Next() {
		if err := f(rows); err != nil {
			c.err = err
			return
		}
	}
	c.err = rows.Err()
}

// grouped reads rows of (label values..., number). Values that sanitize to
// the same label set are summed, so "other" never appears twice.
func (c *collector) grouped(name, help, typ, q string, labels ...string) Family {
	f := Family{Name: name, Help: help, Type: typ}
	index := map[string]int{}
	c.each(q, func(r pgx.Rows) error {
		vals := make([]string, len(labels))
		dest := make([]any, 0, len(labels)+1)
		for i := range vals {
			dest = append(dest, &vals[i])
		}
		var n int64
		dest = append(dest, &n)
		if err := r.Scan(dest...); err != nil {
			return err
		}
		ls := make([][2]string, len(labels))
		for i, l := range labels {
			ls[i] = [2]string{l, Label(vals[i])}
		}
		key := labelString(ls)
		if i, ok := index[key]; ok {
			f.Samples[i].Value += float64(n)
			return nil
		}
		index[key] = len(f.Samples)
		f.Samples = append(f.Samples, Sample{Labels: ls, Value: float64(n)})
		return nil
	})
	return f
}

func gauge(name, help string, v float64) Family {
	return Family{Name: name, Help: help, Type: "gauge", Samples: []Sample{{Value: v}}}
}

// migrationLevel is the numeric prefix of a migration file name, which the
// store records with its directory ("migrations/0024_x.sql").
func migrationLevel(name string) int64 {
	digits := name[strings.LastIndex(name, "/")+1:]
	if i := strings.IndexFunc(digits, func(r rune) bool { return r < '0' || r > '9' }); i >= 0 {
		digits = digits[:i]
	}
	n, err := strconv.ParseInt(digits, 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// escapeLabel applies the exposition format's escaping. Label already
// forbids these characters; this keeps the writer safe on its own.
var escapeLabel = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace

func labelString(ls [][2]string) string {
	if len(ls) == 0 {
		return ""
	}
	parts := make([]string, len(ls))
	for i, l := range ls {
		parts[i] = l[0] + `="` + escapeLabel(l[1]) + `"`
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// ErrWrite wraps a failed write of the exposition.
var ErrWrite = errors.New("metrics: write")

// Write renders the snapshot: families in the order collected, samples
// sorted by label set, values without exponents.
func (s *Snapshot) Write(w io.Writer) error {
	var b strings.Builder
	for _, f := range s.Families {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n", f.Name, f.Help, f.Name, f.Type)
		lines := make([]string, 0, len(f.Samples))
		for _, sm := range f.Samples {
			lines = append(lines, f.Name+labelString(sm.Labels)+" "+strconv.FormatFloat(sm.Value, 'f', -1, 64))
		}
		sort.Strings(lines)
		for _, l := range lines {
			b.WriteString(l)
			b.WriteByte('\n')
		}
	}
	if _, err := io.WriteString(w, b.String()); err != nil {
		return fmt.Errorf("%w: %v", ErrWrite, err)
	}
	return nil
}
