package harness_test

import (
	"context"
	"encoding/json"
	"testing"

	"fabric/internal/harness"
	"fabric/internal/harness/harnesstest"
)

// The conformance suite must be able to fail. Each mutant is an adapter that
// breaks one clause of the contract; the suite has to report a violation for
// every one, and report none for the unmutated fake.

type sinkMut func(next harness.Sink) harness.Sink

type mutAdapter struct {
	harness.Adapter
	mut sinkMut
}

func (m mutAdapter) Start(ctx context.Context, spec harness.StartSpec, sink harness.Sink) (harness.Session, error) {
	return m.Adapter.Start(ctx, spec, m.mut(sink))
}

type fnSink func(context.Context, harness.Event) error

func (f fnSink) Emit(ctx context.Context, e harness.Event) error { return f(ctx, e) }

func dupFinal(next harness.Sink) harness.Sink {
	return fnSink(func(ctx context.Context, e harness.Event) error {
		if err := next.Emit(ctx, e); err != nil {
			return err
		}
		if e.Kind == harness.KindFinal {
			e.Seq++
			return next.Emit(ctx, e)
		}
		return nil
	})
}

func dropFinal(next harness.Sink) harness.Sink {
	return fnSink(func(ctx context.Context, e harness.Event) error {
		if e.Kind == harness.KindFinal {
			return nil
		}
		return next.Emit(ctx, e)
	})
}

func seqGap(next harness.Sink) harness.Sink {
	return fnSink(func(ctx context.Context, e harness.Event) error {
		if e.Seq >= 2 {
			e.Seq++
		}
		return next.Emit(ctx, e)
	})
}

func afterFinal(next harness.Sink) harness.Sink {
	return fnSink(func(ctx context.Context, e harness.Event) error {
		if err := next.Emit(ctx, e); err != nil {
			return err
		}
		if e.Kind == harness.KindFinal {
			b, _ := json.Marshal(harness.Message{Role: "assistant", Text: "late"})
			return next.Emit(ctx, harness.Event{Version: e.Version, Kind: harness.KindMessage, Harness: e.Harness, Session: e.Session, Seq: e.Seq + 1, Payload: b})
		}
		return nil
	})
}

func swallowSinkErrors(next harness.Sink) harness.Sink {
	return fnSink(func(ctx context.Context, e harness.Event) error {
		_ = next.Emit(ctx, e)
		return nil
	})
}

func TestConformanceCatchesMutants(t *testing.T) {
	base := func(interactive bool, br harnesstest.Break) harnesstest.Case { return fakeCase(interactive, br) }

	if v := harnesstest.Violations(t.TempDir(), base(true, 0)); len(v) != 0 {
		t.Fatalf("the unmutated interactive fake violates the contract:\n%s", harnesstest.Joined(v))
	}
	if v := harnesstest.Violations(t.TempDir(), base(false, 0)); len(v) != 0 {
		t.Fatalf("the unmutated fake violates the contract:\n%s", harnesstest.Joined(v))
	}

	wrapNormal := func(c harnesstest.Case, m sinkMut) harnesstest.Case {
		n := c.Normal
		c.Normal = func(t harnesstest.T) harness.Adapter { return mutAdapter{n(t), m} }
		return c
	}
	wrapBlock := func(c harnesstest.Case, m sinkMut) harnesstest.Case {
		b := c.Block
		c.Block = func(t harnesstest.T) harness.Adapter { return mutAdapter{b(t), m} }
		return c
	}

	cases := []struct {
		name string
		c    harnesstest.Case
	}{
		{"duplicate final", wrapNormal(base(false, 0), dupFinal)},
		{"missing final", wrapNormal(base(false, 0), dropFinal)},
		{"seq gap", wrapNormal(base(false, 0), seqGap)},
		{"event after final", wrapNormal(base(false, 0), afterFinal)},
		{"event after final (block)", wrapBlock(base(true, 0), afterFinal)},
		{"sink error swallowed", func() harnesstest.Case {
			c := base(false, 0)
			n := c.Normal
			// a sink that fails, hidden from the emitter: the run must not
			// then claim a recorded success. Wrapping inside Case.Normal is
			// not enough, so wrap the recorder-visible sink with a failer.
			c.Normal = func(t harnesstest.T) harness.Adapter {
				return mutAdapter{n(t), func(next harness.Sink) harness.Sink {
					return swallowSinkErrors(failAfter(next, 2))
				}}
			}
			return c
		}()},
		{"success on exit without result", base(false, harnesstest.BreakSuccessOnNoFinal)},
		{"drops every second input", base(true, harnesstest.BreakDropInput)},
		{"input accepted after end", base(true, harnesstest.BreakInputAfterEndOK)},
		{"second interrupt errors", base(false, harnesstest.BreakInterruptTwiceErr)},
		{"second stop errors", base(true, harnesstest.BreakStopTwiceErr)},
		{"secret not redacted", func() harnesstest.Case {
			c := base(false, 0)
			c.Secret = func(harnesstest.T) (harness.Adapter, string) {
				return &harnesstest.Fake{AName: "fake", Mode: harnesstest.ModeSecret, Secret: secret, NoRedact: true}, secret
			}
			return c
		}()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if v := harnesstest.Violations(t.TempDir(), tc.c); len(v) == 0 {
				t.Fatal("the conformance suite did not notice this mutant")
			}
		})
	}
}

// failAfter fails every Emit once n events have been passed on.
func failAfter(next harness.Sink, n int) harness.Sink {
	count := 0
	return fnSink(func(ctx context.Context, e harness.Event) error {
		if count >= n {
			return context.DeadlineExceeded
		}
		count++
		return next.Emit(ctx, e)
	})
}
