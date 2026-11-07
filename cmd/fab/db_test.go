package main

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"fabric/internal/domain"
	"fabric/internal/store"
)

type fakeCapacity struct {
	setErr   error
	readErr  error
	setCalls int
}

func (f *fakeCapacity) SetCapacity(context.Context, string, string, int64) error {
	f.setCalls++
	return f.setErr
}

func (f *fakeCapacity) Capacity(context.Context, string) (int64, int64, error) {
	return 5, 3, f.readErr
}

// Review F7 (goodqa#15074): SetCapacity's typed refusal (here, a capacity
// below what is held) makes `fab db capacity` exit 3, nothing changed; an
// error of unknown effect still exits 1, and success exits 0.
func TestCapacityExitCodes(t *testing.T) {
	below := fmt.Errorf("%w: resource ra holds 3, more than capacity 2", domain.ErrInvalidTransition)
	cases := []struct {
		name          string
		setErr, rdErr error
		want          int
	}{
		{"below held", below, nil, 3},
		{"unit change", fmt.Errorf("%w: resource ra has a different unit", domain.ErrInvalidTransition), nil, 3},
		{"connection lost", errors.New("conn closed"), nil, 1},
		{"read after set fails", nil, errors.New("conn closed"), 1},
		{"ok", nil, nil, 0},
	}
	for _, c := range cases {
		f := &fakeCapacity{setErr: c.setErr, readErr: c.rdErr}
		err := setCapacity(context.Background(), f, "ra", "slots", 2)
		if got := exitCode(err); got != c.want || f.setCalls != 1 {
			t.Errorf("%s: exit %d (want %d), err %v, set calls %d", c.name, got, c.want, err, f.setCalls)
		}
		if c.want == 3 && !errors.Is(err, domain.ErrInvalidTransition) {
			t.Errorf("%s: refusal lost its cause: %v", c.name, err)
		}
	}
}

func TestExitCode(t *testing.T) {
	ref := &store.Refusal{Response: store.Response{Reason: "x"}}
	for _, c := range []struct {
		err  error
		want int
	}{{nil, 0}, {ref, 3}, {fmt.Errorf("wrapped: %w", ref), 3}, {domain.ErrInvalidTransition, 1}, {errors.New("x"), 1}} {
		if got := exitCode(c.err); got != c.want {
			t.Errorf("exitCode(%v) = %d, want %d", c.err, got, c.want)
		}
	}
}
