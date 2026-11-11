package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"fabric/internal/metrics"
	"fabric/internal/native"
	"fabric/internal/tui"
)

// tuiDefaultTimeout bounds one refresh (statement and lock waits).
const tuiDefaultTimeout = time.Second

// tuiFlags are fab tui's options, parsed apart from the terminal so they
// can be tested.
type tuiFlags struct {
	interval, timeout time.Duration
	opts              tui.Opts
}

func parseTUIFlags(args []string, getenv func(string) string) (tuiFlags, error) {
	fs := flag.NewFlagSet("tui", flag.ContinueOnError)
	interval := fs.Duration("interval", 2*time.Second, "refresh interval (at least 250ms)")
	timeout := fs.Duration("timeout", tuiDefaultTimeout, "statement and lock timeout per refresh")
	noColor := fs.Bool("no-color", false, "no colour or other text attributes (also when NO_COLOR is set)")
	keys := fs.String("keys", "on", "single-character shortcuts j, k, q and /: on or off")
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), `usage: fab tui [--interval 2s] [--timeout 1s] [--no-color] [--keys on|off]

Read-only view of missions, attempts, effects and asks.
Keys: Up/Down or j/k move, PgUp/PgDn page, Tab/Shift-Tab change pane,
Enter open the selected row's mission, Esc back (or clear the filter),
/ or Ctrl-F filter, q or Ctrl-C quit.
--keys=off disables the single-character shortcuts j, k, q and /; the
arrows, Enter, Esc, Tab, Ctrl-F and Ctrl-C keep working.
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return tuiFlags{}, err
	}
	if fs.NArg() > 0 {
		return tuiFlags{}, fmt.Errorf("tui: unexpected argument %q", fs.Arg(0))
	}
	if *interval < 250*time.Millisecond {
		return tuiFlags{}, errors.New("tui: --interval must be at least 250ms")
	}
	if *timeout <= 0 {
		return tuiFlags{}, errors.New("tui: --timeout must be positive")
	}
	var on bool
	switch *keys {
	case "on":
		on = true
	case "off":
	default:
		return tuiFlags{}, errors.New("tui: --keys must be on or off")
	}
	return tuiFlags{interval: *interval, timeout: *timeout,
		opts: tui.Opts{Color: !*noColor && getenv("NO_COLOR") == "", KeysOn: on}}, nil
}

// fab tui: the read-only terminal view.
func tuiMain(args []string) error {
	f, err := parseTUIFlags(args, os.Getenv)
	if err != nil {
		return err
	}
	dsn, credential, err := appConnection()
	if err != nil {
		return err
	}
	defer credential.Close()
	pool, err := tui.OpenPool(dsn)
	if err != nil {
		return err
	}
	defer pool.Close()
	// No store is opened, so a down database still starts the view. The
	// dispatch limit shown is the default store's (store.Open: 16 conns,
	// half of them dispatch slots), as fab metrics reports it.
	o := metrics.Options{Timeout: f.timeout, DispatchLimit: 8, Version: buildVersion(), Caps: spendCaps()}
	s := &tui.Session{Src: &tui.Source{Pool: pool, Timeout: f.timeout, Metrics: o}, M: tui.Model{JobCap: native.JobCapMicros}}
	return tui.Run(context.Background(), s, os.Stdin, os.Stdout, f.interval, f.opts)
}

// fab version: the build revision, as in fabric_build_info.
func versionMain(args []string) error {
	if len(args) > 0 {
		return fmt.Errorf("version: unexpected argument %q", args[0])
	}
	fmt.Println(buildVersion())
	return nil
}
