package agent_test

import (
	"os"
	"regexp"
	"testing"
)

// 0022 must commute with the effects migrations (0023 and later): a database
// that already ran 0023 applies 0022 after it, a fresh one before it. That holds
// while 0022 only creates its own agent_* objects and touches nothing else but
// the attempts key it references.
func TestMigration0022Commutes(t *testing.T) {
	b, err := os.ReadFile("../store/migrations/0022_agent_turns.sql")
	if err != nil {
		t.Fatal(err)
	}
	// Judge the statements, not the commentary.
	sql := regexp.MustCompile(`(?m)^\s*--.*$`).ReplaceAllString(string(b), "")
	for _, re := range []string{`(?i)effect_`, `(?i)\bGRANT\b`, `(?i)\bREVOKE\b`, `(?i)\bCREATE\s+ROLE\b`, `(?i)\bALTER\s+(TABLE|ROLE|DEFAULT)`, `(?i)\bDROP\b`, `(?i)fabric_is_scheduler`} {
		if regexp.MustCompile(re).MatchString(sql) {
			t.Errorf("0022 matches %s: it no longer provably commutes with the effects migrations", re)
		}
	}
	// Every object it creates is agent_-prefixed.
	for _, m := range regexp.MustCompile(`(?i)CREATE\s+(?:TABLE|FUNCTION|TRIGGER|INDEX)\s+(\w+)`).FindAllStringSubmatch(sql, -1) {
		if len(m[1]) < 6 || m[1][:6] != "agent_" {
			t.Errorf("0022 creates %q outside the agent_ namespace", m[1])
		}
	}
	// It may reference only the attempts table of the earlier schema.
	for _, m := range regexp.MustCompile(`(?i)REFERENCES\s+(\w+)`).FindAllStringSubmatch(sql, -1) {
		if m[1] != "attempts" && m[1][:6] != "agent_" {
			t.Errorf("0022 references %q", m[1])
		}
	}
}
