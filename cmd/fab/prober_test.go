package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRunUnitName(t *testing.T) {
	if got := runUnit("r_abc_1"); got != "fab-r-abc-1.service" {
		t.Fatalf("runUnit = %s", got)
	}
}

// G1 readback: present unit = alive; absent unit under a readable slice =
// gone; no slice = no answer (never "gone").
func TestCgroupProber(t *testing.T) {
	root := t.TempDir()
	p := cgroupProber{root: root, uid: 1234}
	if _, known := p.Alive("r_x_1"); known {
		t.Fatal("an unreadable tree gave an answer")
	}
	slice := filepath.Join(root, "user.slice", "user-1234.slice", "user@1234.service", "app.slice")
	if err := os.MkdirAll(filepath.Join(slice, runUnit("r_live_1")), 0o755); err != nil {
		t.Fatal(err)
	}
	if alive, known := p.Alive("r_live_1"); !alive || !known {
		t.Fatalf("live unit read as alive=%v known=%v", alive, known)
	}
	if alive, known := p.Alive("r_gone_1"); alive || !known {
		t.Fatalf("collected unit read as alive=%v known=%v", alive, known)
	}
}
