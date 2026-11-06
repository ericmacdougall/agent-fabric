//go:build linux

package host

import (
	"os"
	"path/filepath"
	"testing"
)

func testProfile(t *testing.T) Profile {
	t.Helper()
	self := os.Getenv("FABRIC_FAB_BIN")
	if self == "" {
		t.Fatal("FABRIC_FAB_BIN must point at a built fab binary (for __sandbox)")
	}
	return &Linux{Self: self, ExtraRW: []string{filepath.Join(os.Getenv("HOME"), ".cache")}}
}

func darwinProfile(t *testing.T) interface {
	Start(Grant, []string, []string, *os.File, *os.File) (Handle, error)
	Wait(Handle) (int, error)
} {
	t.Skip("darwin only")
	return nil
}
