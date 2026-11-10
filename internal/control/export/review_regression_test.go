package export

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"testing"

	"fabric/internal/storetest"
)

// publishedCursor pins both fx1 formats that existing builds emitted. It is
// deliberately independent of the current encoder to exercise upgrades.
func publishedCursor(body string) string {
	sum := sha256.Sum256([]byte("fx1|" + body))
	return "fx1." + base64.RawURLEncoding.EncodeToString([]byte(body)) + "." + hex.EncodeToString(sum[:4])
}

func TestPublishedCursorUpgrade(t *testing.T) {
	for _, suffix := range []string{"", ":b", ":l"} {
		t.Run("shape"+suffix, func(t *testing.T) {
			d, id, h, historic, err := decodeCursor(publishedCursor("0123456789abcdef:42:abcdef123456" + suffix))
			if err != nil || d != "0123456789abcdef" || id != 42 || h != "abcdef123456" || !historic {
				t.Fatalf("published cursor: db=%q id=%d hash=%q historic=%v err=%v", d, id, h, historic, err)
			}
		})
	}
	if got := encodeCursor("0123456789abcdef", 42, "abcdef123456", false); !strings.HasPrefix(got, "fx2.") {
		t.Fatalf("mode-bearing cursor must have a new version: %s", got)
	}
}

func TestBackfillRemainsHistoricWhileLagging(t *testing.T) {
	st, ctx := fresh(t)
	c := conn(t, ctx, storetest.DSN)
	blocker := conn(t, ctx, storetest.DSN)
	if _, err := blocker.Exec(ctx, "BEGIN"); err != nil {
		t.Fatal(err)
	}
	defer blocker.Exec(ctx, "ROLLBACK") //nolint:errcheck
	var xid string
	if err := blocker.QueryRow(ctx, "SELECT pg_current_xact_id()::text").Scan(&xid); err != nil {
		t.Fatal(err)
	}
	seed(t, st, ctx, "historic_lag", 2)
	cur := ""
	for poll := 0; poll < 3; poll++ {
		events, next, result := run(t, ctx, c, cur)
		if len(withoutLag(events)) != 0 || result.More || result.Lagging < 2 {
			t.Fatalf("poll%d: data=%d more=%v lagging=%d", poll, len(withoutLag(events)), result.More, result.Lagging)
		}
		_, _, _, historic, err := decodeCursor(next)
		if err != nil || !historic {
			t.Errorf("poll%d dropped historic mode while lagging: historic=%v err=%v", poll, historic, err)
		}
		for _, event := range lagEvents(events) {
			if event["historic"] != true {
				t.Errorf("backfill lag notice must be historic")
			}
		}
		cur = next // the consumer saves every returned cursor, even when stalled
	}
	if _, err := blocker.Exec(ctx, "ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	events, cur, result := run(t, ctx, c, cur)
	if len(events) != 2 || result.More || result.Lagging != 0 {
		t.Fatalf("released: events=%d more=%v lagging=%d", len(events), result.More, result.Lagging)
	}
	for _, event := range events {
		if event["historic"] != true {
			t.Errorf("historical row lost its mode after blocker released: %v", event["id"])
		}
	}
	seed(t, st, ctx, "new_live", 1)
	events, _, _ = run(t, ctx, c, cur)
	if len(events) != 1 || events[0]["historic"] == true {
		t.Fatalf("new event after catch-up must be live: %v", events)
	}
}

func TestPublishedCursorResumeFromStore(t *testing.T) {
	for _, suffix := range []string{"", ":b", ":l"} {
		t.Run("shape"+suffix, func(t *testing.T) {
			st, ctx := fresh(t)
			c := conn(t, ctx, storetest.DSN)
			seed(t, st, ctx, "upgrade_old", 2)
			_, current, _ := run(t, ctx, c, "")
			d, id, h, _, err := decodeCursor(current)
			if err != nil {
				t.Fatal(err)
			}
			legacy := publishedCursor(d + ":" + itoa(id) + ":" + h + suffix)
			seed(t, st, ctx, "upgrade_pending", 1)
			events, next, result := run(t, ctx, c, legacy)
			if len(events) != 1 || events[0]["historic"] != true || result.Lagging != 0 {
				t.Fatalf("legacy resume must neither replay prior rows nor pulse pending ones: %v", events)
			}
			seed(t, st, ctx, "upgrade_live", 1)
			events, _, _ = run(t, ctx, c, next)
			if len(events) != 1 || events[0]["historic"] == true {
				t.Fatalf("new rows after verified catch-up must be live: %v", events)
			}
		})
	}
}

func TestTicketConsumerGrammar(t *testing.T) {
	for _, tc := range []struct{ name, text, want string }{
		{"space", "GoodQA goodqa#14791", "goodqa#14791"},
		{"digit_project", "1project#5", "1project#5"},
		{"forty", strings.Repeat("p", 40) + "#5", strings.Repeat("p", 40) + "#5"},
		{"forty_one", strings.Repeat("p", 41) + "#5", ""},
		{"eight_digits", "goodqa#12345678", ""},
		{"unicode_prefix", "éproject#5", ""},
		{"unicode_suffix", "project#5é", ""},
		{"underscore", "my_project#5", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ticketRef(tc.text); got != tc.want {
				t.Errorf("ticket reference got%q want%q", got, tc.want)
			}
		})
	}
}

func TestTicketOriginalTextFromStore(t *testing.T) {
	st, ctx := fresh(t)
	c := conn(t, ctx, storetest.DSN)
	seed(t, st, ctx, "ticket_cutoff", 1)
	for _, tc := range []struct{ name, text, want string }{
		{"ordinary", "GoodQA goodqa#14791", "goodqa#14791"},
		{"crossing_number", strings.Repeat(" ", 242) + "goodqa#12345678", ""},
		{"after_cutoff", strings.Repeat(" ", 300) + "goodqa#14791", "goodqa#14791"},
		{"long_context", strings.Repeat("context ", 10000) + "goodqa#14791", "goodqa#14791"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := st.Pool().Exec(ctx, "UPDATE missions SET ticket=$1", tc.text); err != nil {
				t.Fatal(err)
			}
			events, _, _ := run(t, ctx, c, "")
			if len(events) != 1 {
				t.Fatalf("events=%d", len(events))
			}
			got, _ := events[0]["subject"].(map[string]any)["ticket"].(string)
			if got != tc.want {
				t.Errorf("ticket reference got%q want%q", got, tc.want)
			}
		})
	}
}
