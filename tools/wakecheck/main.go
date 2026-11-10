// Command wakecheck measures how fast a running fabd reacts to a committed
// command (goodqa#15586 F19a-0): it executes an accepted command (a revise-intent
// on its own mission, one per round), then polls the scheduler cursor every 2 ms until fabd
// has consumed the command's outbox row, and reports the latency. With the
// post-commit wake hint the first reaction after an idle second is a few
// milliseconds; without it fabd only reacts on its backoff timer.
//
// It writes: it creates one mission and runs commands on it, so it refuses to
// run unless the database name starts with fabric_r15299_ (a rehearsal copy) or
// fabtest_ (a scratch database made by wakecheck.sh); it never runs against
// fabric_live. FABRIC_DSN names that database (an app-role DSN). Nothing secret
// is printed.
package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"fabric/internal/domain"
	"fabric/internal/store"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "wakecheck:", err)
		os.Exit(1)
	}
}

func run() error {
	dsn := os.Getenv("FABRIC_DSN")
	if dsn == "" {
		return fmt.Errorf("FABRIC_DSN is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	st, err := store.Open(ctx, dsn)
	if err != nil {
		return err
	}
	defer st.Close()
	var db string
	if err := st.Pool().QueryRow(ctx, "SELECT current_database()").Scan(&db); err != nil {
		return err
	}
	if !strings.HasPrefix(db, "fabric_r15299_") && !strings.HasPrefix(db, "fabtest_") {
		return fmt.Errorf("refusing to run against database %q: only a fabric_r15299_ rehearsal copy or a fabtest_ scratch database", db)
	}
	id := domain.MissionID(fmt.Sprintf("m_wake_%d", time.Now().Unix()))
	if err := st.CreateMission(ctx, id, domain.ProviderClaudeCode, "GoodQA goodqa#15586", "fabric", "main"); err != nil {
		return err
	}
	const rounds = 10
	var lats []time.Duration
	for i := 0; i < rounds; i++ {
		cmd := domain.Command{Kind: domain.CmdReviseIntent, Mission: id, Reason: fmt.Sprintf("wakecheck round %d", i)}
		resp, err := st.Execute(ctx, "wakecheck", domain.CommandID(fmt.Sprintf("wake-%s-%d", id, i)), cmd)
		if err != nil {
			return fmt.Errorf("round %d %s: %w", i, cmd.Kind, err)
		}
		if resp.Replayed || len(resp.Events) == 0 {
			return fmt.Errorf("round %d: the command was replayed or wrote no event, so it proves nothing", i)
		}
		t0 := time.Now()
		var target int64
		if err := st.Pool().QueryRow(ctx, "SELECT COALESCE(max(id), 0) FROM outbox WHERE mission_id = $1", string(id)).Scan(&target); err != nil {
			return err
		}
		deadline := t0.Add(3 * time.Second)
		for {
			var cur int64
			if err := st.Pool().QueryRow(ctx, "SELECT COALESCE((SELECT last_id FROM outbox_cursors WHERE consumer = 'sched'), 0)").Scan(&cur); err != nil {
				return err
			}
			if cur >= target {
				break
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("round %d: fabd did not consume outbox row %d within 3 s (cursor %d): is a daemon running on this copy?", i, target, cur)
			}
			time.Sleep(2 * time.Millisecond)
		}
		lats = append(lats, time.Since(t0))
		time.Sleep(time.Second) // idle longer than the 100 ms hint spacing before the next command
	}
	sort.Slice(lats, func(a, b int) bool { return lats[a] < lats[b] })
	med, p95, max := lats[len(lats)/2], lats[(len(lats)*95+99)/100-1], lats[len(lats)-1]
	fmt.Printf("wakecheck: %d commands, latency from commit to the scheduler consuming its outbox row: median %s p95 %s max %s\n", len(lats), med.Round(time.Millisecond), p95.Round(time.Millisecond), max.Round(time.Millisecond))
	if med > 100*time.Millisecond {
		return fmt.Errorf("median %s exceeds 100 ms: fabd is not woken by the committed command", med)
	}
	fmt.Println("wakecheck: PASS (median within 100 ms)")
	return nil
}
