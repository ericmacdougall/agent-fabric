//go:build linux

package host

import (
	"errors"
	"strings"
	"testing"
)

func TestLinuxStartArgsInjectProxyWithoutDirectEgress(t *testing.T) {
	policy := validLinuxEgressPolicy()
	l := &LinuxManager{Self: "/srv/fabric/bin/fab"}
	args, pairs, err := l.startArgsEnvWithPolicy(controlGrant(t.TempDir()), "fab-proxy.service", "/run/ef", []string{"env"}, nil, &policy)
	if err != nil {
		t.Fatal(err)
	}
	// Values travel only in the environment file (goodqa#15177); every -E
	// argument must be a blank.
	values := map[string]string{}
	for _, p := range pairs {
		name, value, _ := strings.Cut(p, "=")
		values[name] = value
	}
	keep := ""
	var ports []string
	sawSandbox := false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "__sandbox":
			sawSandbox = true
		case "-E":
			if !sawSandbox && i+1 < len(args) {
				if _, value, _ := strings.Cut(args[i+1], "="); value != "" {
					t.Errorf("-E %s carries a value on the command line", args[i+1])
				}
				i++
			}
		case "--env-keep":
			if i+1 < len(args) {
				keep = args[i+1]
				i++
			}
		case "--tcp":
			if i+1 < len(args) {
				ports = append(ports, args[i+1])
				i++
			}
		}
	}
	if len(ports) != 1 || ports[0] != "18080" {
		t.Fatalf("worker Landlock TCP rules = %v, want only 18080", ports)
	}
	for _, name := range linuxProxyEnvironmentNames {
		want := policy.ProxyURL
		if name == "NO_PROXY" || name == "no_proxy" {
			want = ""
		}
		if values[name] != want {
			t.Errorf("%s = %q, want %q", name, values[name], want)
		}
		if !strings.Contains(keep, name) {
			t.Errorf("--env-keep %q omits %s", keep, name)
		}
	}
	joined := strings.Join(args, " ")
	for _, property := range []string{"IPAddressDeny=any", "IPAddressAllow=127.0.0.1/32", "InaccessiblePaths=", "RestrictAddressFamilies=~AF_UNIX", "PrivatePIDs=yes", "ProtectProc=invisible", "ProcSubset=pid"} {
		if !strings.Contains(joined, property) {
			t.Errorf("worker launch omits %s", property)
		}
	}
	if strings.Contains(joined, "192.0.2.10") {
		t.Fatal("mediator-only provider address was copied into worker launch")
	}
	if _, err := l.startArgsWithPolicy(controlGrant(t.TempDir()), "fab-proxy.service", []string{"env"}, []string{"HTTPS_PROXY=http://127.0.0.1:1"}, &policy); !errors.Is(err, ErrGrantInvalid) {
		t.Fatalf("caller proxy override was accepted: %v", err)
	}
}
