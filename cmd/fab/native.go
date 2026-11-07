package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

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
	var opts native.Options
	if *fake {
		if os.Getenv("FABRIC_ALLOW_FAKE_HOST") != "1" {
			return errors.New("--unconfined-fake-host is refused: it runs tools with no containment")
		}
		fmt.Fprintln(os.Stderr, "WARNING: running tools with NO containment (fake host); this is not a native-boundary run")
		opts.Starter = fakehost.New()
		opts.Boundary = tools.Boundary{Host: "fake-unconfined", Contained: true}
	} else {
		p, err := profileForThisHost()
		if err != nil {
			return err
		}
		w, ok := p.(interface {
			Wait(host.Handle) (int, error)
		})
		if !ok {
			return errors.New("host profile cannot wait on children")
		}
		opts.Starter = waitStarter{p, w}
		if err := os.MkdirAll(g.RunDir, 0o700); err != nil {
			return err
		}
		scratch, err := os.MkdirTemp(g.RunDir, "controls-")
		if err != nil {
			return err
		}
		results, err := p.NegativeControls(scratch)
		if err != nil {
			return err
		}
		opts.Boundary = tools.BoundaryFromControlsRequired(p.Name(), results, g.RequiredControls)
	}
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

// waitStarter adapts a Profile plus its Wait to tools.Starter.
type waitStarter struct {
	host.Profile
	w interface {
		Wait(host.Handle) (int, error)
	}
}

func (w waitStarter) Wait(h host.Handle) (int, error) { return w.w.Wait(h) }
