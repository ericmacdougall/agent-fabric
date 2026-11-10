package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"fabric/internal/connref"
	"fabric/internal/host"
	"fabric/internal/native"
	"fabric/internal/store"
	"fabric/internal/tools"
	"fabric/internal/tools/fakehost"
)

func nativeRunMain(args []string) error {
	fs := flag.NewFlagSet("mission run", flag.ContinueOnError)
	prov := fs.String("provider", "", "anthropic")
	grantFile := fs.String("grant", "", "grant file")
	fake := fs.Bool("unconfined-fake-host", false, "tests only: run tools without containment (needs FABRIC_ALLOW_FAKE_HOST=1)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	g, err := native.LoadGrant(*grantFile)
	if err != nil {
		return err
	}
	if *prov != g.Provider {
		return fmt.Errorf("--provider %q does not match the grant (%q)", *prov, g.Provider)
	}
	opts, err := nativeOptions(g, *fake)
	if err != nil {
		return err
	}
	defer opts.Close()
	return withStore(func(_ context.Context, st *store.Store) error {
		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(g.WallSeconds+120)*time.Second)
		defer cancel()
		r, err := native.Run(ctx, st, g, opts)
		if r != nil {
			b, _ := json.MarshalIndent(r, "", "  ")
			fmt.Println(string(b))
		}
		return err
	})
}

// nativeOptions resolves the host boundary, tools and review settings for a
// native run. `fab mission run` and `fab harness run` share it.
func nativeOptions(g native.Grant, fake bool) (result native.Options, rerr error) {
	// The review DSN is always taken out of the environment before any worker
	// can be constructed, flag on or off. Only FABRIC_REVIEW_GATE=1 uses it.
	reviewDSN, credential, err := takeReviewDSN()
	if err != nil {
		return native.Options{}, err
	}
	defer func() {
		if rerr != nil {
			credential.Close()
		}
	}()
	opts := native.Options{ReviewClose: credential.Close, ReviewGate: reviewGateEnabled(), ReviewDSN: reviewDSN, ToolExitCleanup: toolExitCleanupEnabled()}
	if fake {
		if os.Getenv("FABRIC_ALLOW_FAKE_HOST") != "1" {
			return native.Options{}, errors.New("--unconfined-fake-host is refused: it runs tools with no containment")
		}
		fmt.Fprintln(os.Stderr, "WARNING: running tools with NO containment (fake host); this is not a native-boundary run")
		opts.Starter = fakehost.New()
		opts.Boundary = tools.Boundary{Host: "fake-unconfined", Contained: true}
	} else {
		p, err := profileForThisHost()
		if err != nil {
			return native.Options{}, err
		}
		w, ok := p.(interface {
			Wait(host.Handle) (int, error)
		})
		if !ok {
			return native.Options{}, errors.New("host profile cannot wait on children")
		}
		opts.Starter = waitStarter{p, w}
		if err := os.MkdirAll(g.RunDir, 0o700); err != nil {
			return native.Options{}, err
		}
		scratch, err := os.MkdirTemp(g.RunDir, "controls-")
		if err != nil {
			return native.Options{}, err
		}
		results, err := p.NegativeControls(scratch)
		if err != nil {
			return native.Options{}, err
		}
		opts.Boundary = tools.BoundaryFromControlsRequired(p.Name(), results, g.RequiredControls)
	}
	return opts, nil
}

// reviewGateFlag turns on the durable tool review gate for native runs.
// Anything but "1" leaves native runs exactly as they were without the gate.
const reviewGateFlag = "FABRIC_REVIEW_GATE"

// reviewGateEnabled is true only for the exact value "1".
func reviewGateEnabled() bool { return os.Getenv(reviewGateFlag) == "1" }

// toolExitCleanupFlag makes every native tool call stop and verify its
// descendants after the leader exits. Off by default: on Darwin the stop
// signals the leader's process group after it exited, which widens an
// existing PGID-reuse window until the host profile closes it (slice 4).
const toolExitCleanupFlag = "FABRIC_TOOL_EXIT_CLEANUP"

// toolExitCleanupEnabled is true only for the exact value "1".
func toolExitCleanupEnabled() bool { return os.Getenv(toolExitCleanupFlag) == "1" }

// takeReviewDSN removes the app-role credential from the process environment
// before any tool worker can be constructed. Native retains it only in Options.
func takeReviewDSN() (string, *connectionRef, error) {
	dsn, credential, err := connectionDSN("FABRIC_REVIEW_DSN", connref.Review, false)
	// Remove values and reference metadata before any tool is constructed,
	// including on a resolution error. Only Options retains the owned Ref.
	for _, name := range []string{"FABRIC_REVIEW_DSN", "FABRIC_REVIEW_DSN_FILE", "FABRIC_REVIEW_DSN_FILE_KEY", "FABRIC_REVIEW_DSN_PASSWORD_FILE", "FABRIC_REVIEW_DSN_DATABASE"} {
		if unsetErr := os.Unsetenv(name); unsetErr != nil && err == nil {
			err = errors.New("cannot clear review connection environment")
		}
	}
	if err != nil {
		if credential != nil {
			credential.Close()
		}
		return "", nil, err
	}
	return dsn, credential, nil
}

// waitStarter adapts a Profile plus its Wait to tools.Starter.
type waitStarter struct {
	host.Profile
	w interface {
		Wait(host.Handle) (int, error)
	}
}

func (w waitStarter) Wait(h host.Handle) (int, error) { return w.w.Wait(h) }
