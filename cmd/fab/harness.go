package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"sort"
	"time"

	"fabric/internal/harness"
	"fabric/internal/native"
	"fabric/internal/store"
)

// harnessMain handles `fab harness ...`. Only `run` exists, and only the
// native adapter is wired; the other adapters land in their own PRs.
func harnessMain(args []string) error {
	if len(args) == 0 || args[0] != "run" {
		return errors.New("usage: fab harness run --adapter native --grant FILE --events FILE")
	}
	return harnessRunMain(args[1:])
}

// harnessRunMain runs one native mission through the harness seam: the same
// run `fab mission run` does, with its events recorded to a JSONL trace that
// is then checked against the adapter contract.
func harnessRunMain(args []string) error {
	fs := flag.NewFlagSet("harness run", flag.ContinueOnError)
	adapter := fs.String("adapter", "", "harness adapter (native)")
	grantFile := fs.String("grant", "", "grant file")
	events := fs.String("events", "", "new file to write the event trace to (JSONL, mode 0600)")
	fake := fs.Bool("unconfined-fake-host", false, "tests only: run tools without containment (needs FABRIC_ALLOW_FAKE_HOST=1)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *adapter != "native" {
		return fmt.Errorf("--adapter %q is not wired into fab harness run (only native)", *adapter)
	}
	if *events == "" {
		return errors.New("--events is required")
	}
	g, err := native.LoadGrant(*grantFile)
	if err != nil {
		return err
	}
	opts, err := nativeOptions(g, *fake)
	if err != nil {
		return err
	}
	sink, err := harness.NewJSONLSink(*events)
	if err != nil {
		return err
	}
	opts.Sink = sink
	runErr := withStore(func(_ context.Context, st *store.Store) error {
		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(g.WallSeconds+120)*time.Second)
		defer cancel()
		r, err := native.Run(ctx, st, g, opts)
		if r != nil {
			b, _ := json.MarshalIndent(r, "", "  ")
			fmt.Println(string(b))
		}
		return err
	})
	if cerr := sink.Close(); cerr != nil && runErr == nil {
		runErr = cerr
	}
	evs, rerr := harness.ReadJSONL(*events)
	if rerr != nil {
		return errors.Join(runErr, rerr)
	}
	counts := map[harness.Kind]int{}
	for _, e := range evs {
		counts[e.Kind]++
	}
	kinds := make([]string, 0, len(counts))
	for k, n := range counts {
		kinds = append(kinds, fmt.Sprintf("%s=%d", k, n))
	}
	sort.Strings(kinds)
	conf := harness.CheckStream(evs)
	fmt.Printf("trace: %d events (%v) conforms=%v\n", len(evs), kinds, conf == nil)
	if len(evs) > 0 {
		last := evs[len(evs)-1]
		var f harness.Final
		if last.Kind == harness.KindFinal && json.Unmarshal(last.Payload, &f) == nil {
			fmt.Printf("final: %s\n", f.Outcome)
		}
	}
	if conf != nil {
		return errors.Join(runErr, fmt.Errorf("trace does not conform: %w", conf))
	}
	return runErr
}
