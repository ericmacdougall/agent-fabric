package native_test

import (
	"context"
	"errors"
	"testing"

	"fabric/internal/domain"
	"fabric/internal/native"
	"fabric/internal/provider/anthropic/fakeapi"
	"fabric/internal/storetest"
)

// goodqa#15324 S1b: `fab mission run` must never start a ladder mission. The
// refusal comes before SelfTest, the key command and any write, the same as
// the other start refusals, whether or not the mission row exists.
func TestNativeRefusesLadderMission(t *testing.T) {
	ctx := context.Background()
	stateDir(t)
	for _, c := range []string{"existing ladder mission", "ladder id with no row yet"} {
		t.Run(c, func(t *testing.T) {
			st := storetest.Open(t)
			e := newEnv(t, fakeapi.Step{Events: fakeapi.Text("never", 1, 1)})
			marker := selfTestMarker(t, e)
			e.g.Mission = "m_ladder-t1"
			if c == "existing ladder mission" {
				h := "abcdef0"
				if _, err := st.RecordLadderReceipt(ctx, "t1", domain.LadderRecord{Class: "CLAIM", Step: "claim", Commit: &h}); err != nil {
					t.Fatal(err)
				}
			}
			cmds := count(t, st, "SELECT count(*) FROM commands")
			missions := count(t, st, "SELECT count(*) FROM missions")
			attempts := count(t, st, "SELECT count(*) FROM attempts")
			slots := count(t, st, "SELECT count(*) FROM missions WHERE execution <> 'IDLE'")
			_, err := native.Run(ctx, st, e.g, opts(true))
			if !errors.Is(err, native.ErrStartRefused) {
				t.Fatalf("native run on a ladder mission: %v, want ErrStartRefused", err)
			}
			if exists(marker) || exists(e.marker) || len(e.srv.Requests()) != 0 {
				t.Fatalf("selftest ran %v, key read %v, %d requests; want none", exists(marker), exists(e.marker), len(e.srv.Requests()))
			}
			for name, q := range map[string]struct {
				sql  string
				want int
			}{
				"commands": {"SELECT count(*) FROM commands", cmds}, "missions": {"SELECT count(*) FROM missions", missions},
				"attempts": {"SELECT count(*) FROM attempts", attempts},
				"non-idle": {"SELECT count(*) FROM missions WHERE execution <> 'IDLE'", slots},
			} {
				if n := count(t, st, q.sql); n != q.want {
					t.Fatalf("%s %d -> %d after a refusal", name, q.want, n)
				}
			}
		})
	}
}
