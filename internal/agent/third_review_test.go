package agent_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"fabric/internal/agent"
	"fabric/internal/provider/anthropic/fakeapi"
	"fabric/internal/store"
)

func serverUsageStep(text string, input, output int, uses int64) fakeapi.Step {
	evs := fakeapi.Text(text, input, output)
	evs[0].Data = fmt.Sprintf(`{"type":"message_start","message":{"id":"msg_fake","model":"fake-model","usage":{"input_tokens":%d,"output_tokens":1,"server_tool_use":{"web_search":%d}}}}`, input, uses)
	return fakeapi.Step{Events: evs}
}

func assistantMessageCount(t *testing.T, r *rig) int {
	t.Helper()
	var n int
	if err := r.st.Pool().QueryRow(context.Background(), `SELECT count(*) FROM agent_messages WHERE attempt_id = $1 AND role = 'assistant'`, attemptID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestServerToolMaximumIsIncludedInHold(t *testing.T) {
	r := newRig(t, serverUsageStep("ok", 150, 20, 2))
	r.cfgMod = func(c *agent.Config) { c.MaxServerToolUses = 2 }
	r.srv.Validate = func(body []byte) error {
		reservations, err := r.st.Reservations(context.Background(), attemptID)
		if err != nil {
			return err
		}
		want := testPrices().WorstCase(len(body), 1000, 2)
		for _, reservation := range reservations {
			if reservation.Resource == r.caps[2].Resource && reservation.State == store.ResHeld {
				if reservation.Amount != want {
					return fmt.Errorf("hold amount %d, want server-tool worst case %d", reservation.Amount, want)
				}
				return nil
			}
		}
		return errors.New("run reservation not held at request time")
	}
	out, err, _ := r.life()
	if err != nil || out.Kind != agent.OutcomeDone {
		t.Fatalf("out=%+v err=%v", out, err)
	}
	want := testPrices().Cost(agent.Usage{Input: 150, Output: 20, ServerToolUses: 2})
	if got := r.spent(r.caps[2].Resource); got != want {
		t.Fatalf("spent %d, want %d", got, want)
	}
}

func TestUnboundedServerToolsSettleButFail(t *testing.T) {
	r := newRig(t, serverUsageStep("must not complete", 150, 20, 1))
	out, err, _ := r.life()
	if err != nil || out.Kind != agent.OutcomeBudgetExhausted {
		t.Fatalf("out=%+v err=%v", out, err)
	}
	if n := assistantMessageCount(t, r); n != 0 {
		t.Fatalf("disabled server-tool response committed %d assistant messages", n)
	}
	want := testPrices().Cost(agent.Usage{Input: 150, Output: 20, ServerToolUses: 1})
	if got := r.spent(r.caps[2].Resource); got != want {
		t.Fatalf("spent %d, want full actual %d", got, want)
	}
	if out, err, _ := r.life(); err != nil || out.Kind != agent.OutcomeBudgetExhausted || len(r.srv.Requests()) != 1 {
		t.Fatalf("disabled server tools resumed: out=%+v err=%v requests=%d", out, err, len(r.srv.Requests()))
	}
}

func TestActualOverHoldIsSettledAndBlocksFutureSpend(t *testing.T) {
	// Usage far above max_tokens produces an actual larger than the reservation.
	r := newRig(t, fakeapi.Step{Events: fakeapi.Text("must not complete", 150, 2_000_000)})
	out, err, _ := r.life()
	if err != nil || out.Kind != agent.OutcomeBudgetExhausted {
		t.Fatalf("out=%+v err=%v", out, err)
	}
	if n := assistantMessageCount(t, r); n != 0 {
		t.Fatalf("over-hold response committed %d assistant messages", n)
	}
	actual := testPrices().Cost(agent.Usage{Input: 150, Output: 2_000_000})
	held := testPrices().WorstCase(len(r.srv.Requests()[0].Body), 1000, 0)
	if actual <= held {
		t.Fatalf("test actual %d does not exceed hold %d", actual, held)
	}
	if got := r.spent(r.caps[2].Resource); got != actual {
		t.Fatalf("spent %d, want unclamped actual %d (held %d)", got, actual, held)
	}
	_, err = r.led.Hold(context.Background(), "after-over-hold", 1)
	var budget *agent.ErrBudget
	if !errors.As(err, &budget) {
		t.Fatalf("future hold after over-hold actual = %v, want ErrBudget", err)
	}
}

func TestReflectedKeyNeverReachesAgentMessages(t *testing.T) {
	r := newRig(t, endStep("prefix "+canary+" suffix"))
	_, err, _ := r.life()
	if err == nil || strings.Contains(err.Error(), canary) {
		t.Fatalf("reflected key error = %v", err)
	}
	rows, err := r.st.Pool().Query(context.Background(), `SELECT content::text FROM agent_messages WHERE attempt_id = $1`, attemptID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var content string
		if err := rows.Scan(&content); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(content, canary) {
			t.Fatal("active key persisted in agent_messages")
		}
	}
	if n := assistantMessageCount(t, r); n != 0 {
		t.Fatalf("reflected-key response committed %d assistant messages", n)
	}
}
