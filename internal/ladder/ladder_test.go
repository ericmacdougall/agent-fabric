package ladder

import "testing"

func table() Table {
	return Table{FailuresPerRung: 2, Rungs: []Rung{
		{Name: "R0", Writable: true, Reviewer: "R1"},
		{Name: "R1", Writable: true, Reviewer: "R2"},
		{Name: "R2", Writable: true, Reviewer: "R3"},
		{Name: "R3", Writable: true, Reviewer: "R4"},
		{Name: "R4", Writable: false},
		{Name: "R5", Writable: true, Reviewer: "R6", NeedsPermission: true},
		{Name: "R6", Writable: false, NeedsPermission: true},
	}}
}

func TestTwoGenuineFailuresClimbOneRung(t *testing.T) {
	s, err := Resolve(table(), "R0", []Event{{ReviewFail, "a"}, {ReviewFail, "b"}}, nil)
	if err != nil || s.Rung != "R1" || s.Failures != 0 || s.Stopped {
		t.Fatalf("%+v %v", s, err)
	}
	s, _ = Resolve(table(), "R0", []Event{{ReviewFail, "a"}}, nil)
	if s.Rung != "R0" || s.Failures != 1 {
		t.Fatalf("one failure should not climb: %+v", s)
	}
}

func TestInfraNeverCounts(t *testing.T) {
	s, _ := Resolve(table(), "R0", []Event{{Infra, ""}, {Infra, ""}, {Infra, ""}, {ReviewFail, "a"}, {Infra, ""}}, nil)
	if s.Rung != "R0" || s.Failures != 1 {
		t.Fatalf("infra counted: %+v", s)
	}
}

func TestSameHeadIsOneFailure(t *testing.T) {
	s, _ := Resolve(table(), "R0", []Event{{ReviewFail, "a"}, {ReviewFail, "a"}, {TestFail, "a"}}, nil)
	if s.Rung != "R0" || s.Failures != 1 {
		t.Fatalf("repeated failure on one head climbed: %+v", s)
	}
}

func TestNewFindingResets(t *testing.T) {
	s, _ := Resolve(table(), "R0", []Event{{ReviewFail, "a"}, {NewFinding, ""}, {ReviewFail, "b"}}, nil)
	if s.Rung != "R0" || s.Failures != 1 {
		t.Fatalf("new finding did not reset: %+v", s)
	}
}

func TestTopRungNeedsPermission(t *testing.T) {
	fails := []Event{{ReviewFail, "a"}, {ReviewFail, "b"}, {ReviewFail, "c"}, {ReviewFail, "d"}, {ReviewFail, "e"}, {ReviewFail, "f"}, {ReviewFail, "g"}, {ReviewFail, "h"}}
	s, _ := Resolve(table(), "R0", fails, nil)
	if !s.Stopped || s.Rung != "R3" {
		t.Fatalf("expected a stop at R3 before the permissioned rung: %+v", s)
	}
	s, _ = Resolve(table(), "R0", fails, map[string]bool{"R5": true})
	if s.Stopped || s.Rung != "R5" {
		t.Fatalf("with the grant the writer should reach R5: %+v", s)
	}
}

func TestUnknownStartResetsToLowest(t *testing.T) {
	s, _ := Resolve(table(), "R9", nil, nil)
	if s.Rung != "R0" {
		t.Fatalf("unknown start rung should reset to the lowest: %+v", s)
	}
	if r, err := table().Reviewer("R2"); err != nil || r != "R3" {
		t.Fatalf("reviewer of R2 = %q (%v)", r, err)
	}
	if _, err := table().Reviewer("R9"); err == nil {
		t.Fatal("unknown rung accepted")
	}
}
