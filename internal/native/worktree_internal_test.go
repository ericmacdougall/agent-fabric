package native

import (
	"errors"
	"strconv"
	"strings"
	"testing"
)

// goodqa#15283 NEG-6: two spellings that differ only in case are different
// worktrees. The compare is byte-exact, and planExisting refuses, naming both.
func TestWorktreeCompareIsCaseExact(t *testing.T) {
	if !sameWorktree("/srv/wt", "/srv/wt") {
		t.Fatal("control: identical paths compared unequal")
	}
	for _, p := range [][2]string{{"/srv/WT", "/srv/wt"}, {"/Srv/wt", "/srv/wt"}, {"/srv/\u00c9t", "/srv/\u00e9t"}} {
		if sameWorktree(p[0], p[1]) {
			t.Errorf("sameWorktree(%q, %q) folded case", p[0], p[1])
		}
		g := Grant{Mission: "m_case", Attempt: "r_case", Worktree: p[1]}
		stored := p[0]
		_, err := planExisting(g, p[1], &stored, "m_case", "RUNNING", "anthropic-api", "/s/native-locks/r_case.lock", "h_x", 1, true, nil, nil, nil)
		if !errors.Is(err, ErrWorktreeMismatch) || !strings.Contains(err.Error(), strconv.Quote(p[0])) || !strings.Contains(err.Error(), strconv.Quote(p[1])) {
			t.Errorf("planExisting stored %q, grant %q: %v, want ErrWorktreeMismatch naming both", p[0], p[1], err)
		}
	}
}
