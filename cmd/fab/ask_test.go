package main

import (
	"flag"
	"testing"
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
