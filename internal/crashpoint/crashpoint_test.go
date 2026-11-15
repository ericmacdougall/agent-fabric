package crashpoint

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// The variable arms a point only inside a test binary and only in the
// exact <name>:before|after form.
func TestParseArmsOnlyInATestBinary(t *testing.T) {
	cases := []struct {
		v    string
		test bool
		want arm
	}{
		{"settle.mark:before", true, arm{"settle.mark", "before", 1}},
		{"settle.mark:after", true, arm{"settle.mark", "after", 1}},
		{"settle.record:after:3", true, arm{"settle.record", "after", 3}},
		{"settle.mark:before", false, arm{}},
		{"settle.mark", true, arm{}},
		{"settle.mark:during", true, arm{}},
		{"settle.mark:before:0", true, arm{}},
		{"settle.mark:before:x", true, arm{}},
		{":before", true, arm{}},
		{"", true, arm{}},
		{"trace", true, arm{Trace, Trace, 0}},
		{"trace", false, arm{}},
	}
	for _, c := range cases {
		if got := parse(c.v, c.test); got != c.want {
			t.Errorf("parse(%q, %v) = %+v; want %+v", c.v, c.test, got, c.want)
		}
	}
}

// A child armed at a point exits 137 there with the marker line, and
// runs nothing after it; unarmed, Around only calls through.
func TestAroundKillsTheArmedChild(t *testing.T) {
	if os.Getenv("CRASHPOINT_SELFTEST") == "1" {
		for i := 1; i <= 2; i++ {
			Around("selftest.write", func() error { os.Stdout.WriteString("WROTE" + string(rune('0'+i)) + "\n"); return nil }) //nolint:errcheck
		}
		os.Stdout.WriteString("CONTINUED\n")
		return
	}
	for _, c := range []struct{ arm, wrote string }{{"before", ""}, {"after", "WROTE1"}, {"before:2", "WROTE1"}, {"after:2", "WROTE1 WROTE2"}} {
		cmd := exec.Command(os.Args[0], "-test.run=^TestAroundKillsTheArmedChild$", "-test.count=1")
		cmd.Env = append(os.Environ(), "CRASHPOINT_SELFTEST=1", Env+"=selftest.write:"+c.arm)
		var out, errb strings.Builder
		cmd.Stdout, cmd.Stderr = &out, &errb
		err := cmd.Run()
		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.ExitCode() != ExitCode {
			t.Fatalf("%s: exit %v; want %d", c.arm, err, ExitCode)
		}
		if !strings.Contains(errb.String(), Marker+"selftest.write:") {
			t.Fatalf("%s: no marker on stderr", c.arm)
		}
		if got := strings.Join(strings.Fields(strings.ReplaceAll(strings.ReplaceAll(out.String(), "PASS", ""), "CONTINUED", "CONTINUED")), " "); got != c.wrote {
			t.Fatalf("%s: stdout %q; want %q", c.arm, got, c.wrote)
		}
	}
	trace := exec.Command(os.Args[0], "-test.run=^TestAroundKillsTheArmedChild$", "-test.count=1")
	trace.Env = append(os.Environ(), "CRASHPOINT_SELFTEST=1", Env+"="+Trace)
	var terr strings.Builder
	trace.Stderr = &terr
	if err := trace.Run(); err != nil {
		t.Fatalf("trace run: %v", err)
	}
	for _, want := range []string{"selftest.write:before:1", "selftest.write:after:2"} {
		if !strings.Contains(terr.String(), TraceMarker+want) {
			t.Fatalf("trace lacks %s", want)
		}
	}
	if err := Around("selftest.write", func() error { return errors.New("x") }); err == nil || err.Error() != "x" {
		t.Fatalf("unarmed Around: %v", err)
	}
}
