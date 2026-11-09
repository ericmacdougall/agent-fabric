package main

import (
	"os"
	"strings"
	"testing"
)

// goodqa#15587: the daemon's --no-dispatch flag must reach the scheduler
// config; a flag that parses but is never copied would leave the live fabd
// dispatching. A source guard in the style of TestDaemonMainUsesTheServiceContext.
func TestDaemonNoDispatchFlagReachesConfig(t *testing.T) {
	b, err := os.ReadFile("job.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	if !strings.Contains(src, `fs.Bool("no-dispatch"`) {
		t.Fatal("fab daemon has no --no-dispatch flag")
	}
	if !strings.Contains(src, "cfg.NoDispatch = *noDispatch") {
		t.Fatal("the --no-dispatch flag is not copied into the scheduler config")
	}
}
