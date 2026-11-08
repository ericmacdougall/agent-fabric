package importer

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"fabric/internal/domain"
	"fabric/internal/storetest"
)

func TestMalformedQueuedProviderIsRejected(t *testing.T) {
	for _, raw := range []string{
		`{"name":"q","provider":42}`,
		`{"name":"q","provider":true}`,
		`{"name":"q","provider":{"name":"claude-code"}}`,
		`{"name":"q","provider":["claude-code"]}`,
		`42`,
		`true`,
		`null`,
		`["legacy"]`,
	} {
		t.Run(raw, func(t *testing.T) {
			st := storetest.Open(t)
			var sf SlotsFile
			if err := json.Unmarshal([]byte(`{"queued_in_order":[`+raw+`]}`), &sf); err != nil {
				t.Fatal(err)
			}
			_, err := Run(context.Background(), st, nil, fixture(t), sf, QueueFile{})
			if !errors.Is(err, domain.ErrInvalidProvider) {
				t.Fatalf("malformed provider defaulted instead of failing: %v", err)
			}
			var n int
			if err := st.Pool().QueryRow(context.Background(), `SELECT count(*) FROM missions`).Scan(&n); err != nil || n != 0 {
				t.Fatalf("malformed row left %d missions (%v)", n, err)
			}
		})
	}
}
