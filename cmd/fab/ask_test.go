package main

import (
	"errors"
	"flag"
	"testing"

	"fabric/internal/domain"
	"fabric/internal/store"
)

// The id may come first or last; flags on either side of it are parsed, and
// a stray second positional is refused rather than ignored.
func TestParseWithID(t *testing.T) {
	cases := []struct {
		args    []string
		id, ans string
		bad     bool
	}{
		{args: []string{"a_1", "--answer", "yes"}, id: "a_1", ans: "yes"},
		{args: []string{"--answer", "yes", "a_1"}, id: "a_1", ans: "yes"},
		{args: []string{"--answer", "yes"}, bad: true},
		{args: []string{"a_1", "a_2"}, bad: true},
		{args: []string{"a_1", "--answer", "yes", "a_2"}, bad: true},
		{args: []string{}, bad: true},
	}
	for _, c := range cases {
		fs := flag.NewFlagSet("t", flag.ContinueOnError)
		ans := fs.String("answer", "", "")
		id, err := parseWithID(fs, c.args, "t")
		if c.bad {
			if err == nil {
				t.Errorf("%v: accepted as id=%q", c.args, id)
			}
			continue
		}
		if err != nil || id != c.id || *ans != c.ans {
			t.Errorf("%v: id=%q answer=%q err=%v", c.args, id, *ans, err)
		}
	}
}

// Review F8: a stored answer whose wake failed is not mapped to a refusal
// (exit 3, "nothing changed"), even when the wake's cause is a refusal kind.
func TestWakePendingIsNotARefusal(t *testing.T) {
	err := refusal(&store.WakePendingError{Ask: "a_1", Mission: "m_1", Cause: domain.ErrInvalidTransition})
	var ref *store.Refusal
	if errors.As(err, &ref) {
		t.Fatalf("a pending wake was reported as a refusal: %v", err)
	}
}
