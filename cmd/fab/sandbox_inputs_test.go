package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// goodqa#15161 review F9: the worker's TCP allow-list covers the runner DSN
// the worker uses, defaults a postgres URL without a port to 5432, keeps
// every host of a multi-host URL and drops out-of-range ports.
func TestWorkerTCPPorts(t *testing.T) {
	cases := []struct {
		dsn, runner string
		want        []int
	}{
		{"", "", []int{443}},
		{"postgres://u@db/x", "", []int{443, 5432}},
		{"postgres://u@db:6543/x", "postgres://r@pgb:6432/x", []int{443, 6543, 6432}},
		{"postgresql://u@h1:5433,h2:5434/x", "", []int{443, 5433, 5434}},
		{"postgres://u@db:99999/x", "", []int{443}},
		{"host=db port=7000 dbname=x", "", []int{443, 7000}},
		{"host=db dbname=x", "", []int{443, 5432}},
		{"host=/var/run/postgresql dbname=x", "", []int{443}},
		{"postgres:///x?host=/tmp", "", []int{443}},
	}
	for _, c := range cases {
		t.Setenv("FABRIC_DSN", c.dsn)
		t.Setenv("FABRIC_RUNNER_DSN_ENV", "FAB_TEST_RUNNER_DSN")
		t.Setenv("FAB_TEST_RUNNER_DSN", c.runner)
		if got := workerTCPPorts(); !slices.Equal(got, c.want) {
			t.Errorf("dsn=%q runner=%q: got %v want %v", c.dsn, c.runner, got, c.want)
		}
	}
}

// goodqa#15161 review F4: the launch inputs are the provider's resolved
// directory, the hooks dir, the schema and the operator's extra entries;
// relative entries are dropped.
func TestLaunchInputs(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "versions")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(real, "provider")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "bin", "provider")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(bin, link); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FABRIC_DARWIN_EXTRA_RO", "/opt/node"+string(os.PathListSeparator)+"relative/x")
	got := launchInputs(link, "/srv/hooks", "/srv/wr/checkpoint.schema.json")
	realDir, _ := filepath.EvalSymlinks(real)
	want := []string{realDir, "/srv/hooks", "/srv/wr/checkpoint.schema.json", "/opt/node"}
	if !slices.Equal(got, want) {
		t.Errorf("got %v want %v", got, want)
	}
}
