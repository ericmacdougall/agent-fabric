//go:build linux

package host

import "testing"

// workerArgs returns the systemd-run argument list for a worker.
func workerArgs(t *testing.T, l *Linux, g Grant, env []string) []string {
	t.Helper()
	args, _, err := l.startArgs(g, "fab-x.service", envFile(g), []string{"env"}, env)
	if err != nil {
		t.Fatal(err)
	}
	return args
}
