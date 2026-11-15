package main

import (
	"fabric/internal/provider/pin"
)

// pinRealProvider applies goodqa#15966 AC-21 to a real provider (outcome (b)
// of gateDaemonProvider, which it leaves untouched). A fake-only start has
// its own seal (AC-3) and ignores the pin. For a real provider the pin is
// required, checked now, and checked again at each launch (Resolve) and
// immediately before exec (PreExec).
func pinRealProvider(dp *daemonProvider, real bool, wantSHA string) error {
	if !real {
		return nil
	}
	if wantSHA == "" {
		return realProviderRefused{"--provider-sha256 is required for a real provider (goodqa#15966 AC-21)"}
	}
	w, err := pin.Wire(dp.Launch.Binary, wantSHA)
	if err != nil {
		return realProviderRefused{err.Error()}
	}
	dp.Path = w.Path
	dp.Launch.Resolve = w.Resolve
	dp.Launch.PreExec = w.PreExec
	return nil
}
