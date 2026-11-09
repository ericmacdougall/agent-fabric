package proc

import (
	"strings"
	"testing"
)

func TestTailDropsThePartialLine(t *testing.T) {
	tl := &tail{n: 106} // the window starts three bytes into the line
	tl.Write([]byte("Bearer " + strings.Repeat("Z9", 20) + "\n" + strings.Repeat("y", 60) + "\n"))
	if strings.Contains(tl.String(), "Z9Z9Z9") {
		t.Fatalf("tail keeps the cut line: %q", tl.String())
	}
}
