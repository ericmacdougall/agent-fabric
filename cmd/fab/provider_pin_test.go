package main

import (
	"errors"
	"strings"
	"testing"

	"fabric/internal/provider/claude"
)

// goodqa#15966 AC-21 wiring. The file checks themselves are tested in
// internal/provider/pin.

func TestPinRequiresTheFlagForARealProvider(t *testing.T) {
	for _, sha := range []string{"", "ABC", strings.Repeat("A", 64), strings.Repeat("a", 63)} {
		dp := &daemonProvider{Launch: claude.Launch{Binary: "/bin/sh"}}
		err := pinRealProvider(dp, true, sha)
		var rp realProviderRefused
		if !errors.As(err, &rp) {
			t.Fatalf("sha %q: want a realProviderRefused, got %v", sha, err)
		}
		if dp.Launch.PreExec != nil || dp.Launch.Resolve != nil {
			t.Fatalf("sha %q: a refused pin must not install launch hooks", sha)
		}
	}
}

func TestPinIsWiredIntoEveryRealLaunch(t *testing.T) {
	// /bin/sh is not root-writable-safe to assume, but the wiring only needs
	// a refusal path: the sha of /bin/sh is not this one.
	dp := &daemonProvider{Launch: claude.Launch{Binary: "/bin/sh"}}
	err := pinRealProvider(dp, true, strings.Repeat("0", 64))
	var rp realProviderRefused
	if !errors.As(err, &rp) {
		t.Fatalf("a wrong pin must be a realProviderRefused, got %v", err)
	}
}

func TestPinIgnoredForAFakeOnlyStart(t *testing.T) {
	dp := &daemonProvider{Launch: claude.Launch{Binary: "/bin/sh"}}
	if err := pinRealProvider(dp, false, ""); err != nil || dp.Launch.PreExec != nil {
		t.Fatalf("a fake-only start keeps its own seal and ignores the pin: %v", err)
	}
}
