//go:build darwin

package main

import "fabric/internal/host"

// realProviderGate (goodqa#15976 AC-2): M1 refuses a real provider on macOS
// outright. Only a pinned fake runs there (daemonProviderGate).
func realProviderGate(host.Profile) error {
	return realProviderRefused{"a real provider is refused on macOS in M1 builds"}
}
