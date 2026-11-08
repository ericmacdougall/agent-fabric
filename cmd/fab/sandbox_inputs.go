package main

import (
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// workerTCPPorts is the outbound TCP allow-list for a worker: 443 plus the
// database port of fabd's DSN and of the runner DSN the worker actually
// uses (goodqa#15161 review F9). A postgres URL without a port means 5432;
// every host of a multi-host URL counts; ports outside 1-65535 are dropped.
// A unix-socket DSN adds nothing (the sandboxes deny unix sockets).
func workerTCPPorts() []int {
	ports := []int{443}
	add := func(s string) {
		p, err := strconv.Atoi(s)
		if err != nil || p < 1 || p > 65535 {
			return
		}
		for _, q := range ports {
			if q == p {
				return
			}
		}
		ports = append(ports, p)
	}
	runnerEnv := os.Getenv("FABRIC_RUNNER_DSN_ENV")
	if runnerEnv == "" {
		runnerEnv = "FABRIC_RUNNER_DSN"
	}
	for _, dsn := range []string{os.Getenv("FABRIC_DSN"), os.Getenv(runnerEnv)} {
		for _, p := range dsnPorts(dsn) {
			add(p)
		}
	}
	return ports
}

// dsnPorts returns the TCP ports a libpq DSN (URL or key=value form) names.
func dsnPorts(dsn string) []string {
	dsn = strings.TrimSpace(dsn)
	if dsn == "" {
		return nil
	}
	if u, err := url.Parse(dsn); err == nil && (u.Scheme == "postgres" || u.Scheme == "postgresql") {
		if u.Host == "" || strings.HasPrefix(u.Query().Get("host"), "/") {
			return nil
		}
		var out []string
		for _, h := range strings.Split(u.Host, ",") {
			if _, p, err := net.SplitHostPort(h); err == nil {
				out = append(out, p)
			} else {
				out = append(out, "5432")
			}
		}
		return out
	}
	var host, port string
	for _, f := range strings.Fields(dsn) {
		if k, v, ok := strings.Cut(f, "="); ok {
			switch k {
			case "host":
				host = v
			case "port":
				port = v
			}
		}
	}
	if strings.HasPrefix(host, "/") {
		return nil
	}
	if port == "" {
		port = "5432"
	}
	return strings.Split(port, ",")
}

// providerPath is the provider binary as an absolute path with symlinks
// resolved ("" when it cannot be found or its symlinks cannot be
// resolved). The launch line runs this file: a symlink such as
// ~/.local/bin/claude sits in a directory the Darwin profile denies, so
// exec through it fails (review 716fc04 C). A symlink that LookPath saw
// but EvalSymlinks cannot follow (dangling, or swapped by an updater in
// between) is "", never the symlink path itself (goodqa#15161 R4).
func providerPath(provider string) string {
	path, err := exec.LookPath(provider)
	if err != nil {
		return ""
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return ""
	}
	r, err := evalSymlinks(abs)
	if err != nil {
		return ""
	}
	return r
}

// evalSymlinks is filepath.EvalSymlinks; a test swaps it to replay an
// updater that removes the symlink between LookPath and the resolution.
var evalSymlinks = filepath.EvalSymlinks

// launchInputs lists the read-only inputs a real launch needs that may sit
// under a home the Darwin profile denies (goodqa#15161 review F4): the
// provider's installed directory, the hooks directory, the schema file,
// plus any operator-reviewed FABRIC_DARWIN_EXTRA_RO entries. Relative
// entries are dropped; the profile refuses an entry that exposes a home.
func launchInputs(provider, hooksDir, schema string) []string {
	var out []string
	add := func(p string) {
		if p != "" && filepath.IsAbs(p) {
			out = append(out, filepath.Clean(p))
		}
	}
	if p := providerPath(provider); p != "" {
		add(filepath.Dir(p))
	}
	add(hooksDir)
	add(schema)
	for _, p := range filepath.SplitList(os.Getenv("FABRIC_DARWIN_EXTRA_RO")) {
		add(p)
	}
	return out
}
