package dispatch

import (
	"strings"
	"testing"
)

// goodqa#15966 AC-19: authentication_failed or a 401 settles definite with
// fixed text, even when the stream has no result event.
func TestClaudeCodeSettleRefusedToken(t *testing.T) {
	init := `{"type":"system","subtype":"init","session_id":"s1","model":"m"}`
	for _, stream := range []string{
		init + "\n" + `{"type":"assistant","session_id":"s1","error":"authentication_failed","message":{"usage":{}}}` + "\n" +
			`{"type":"result","subtype":"success","session_id":"s1","is_error":true,"result":"CLI-TEXT"}`,
		init + "\n" + `{"type":"system","subtype":"api_retry","session_id":"s1","error":"CLI-TEXT","error_status":401}`,
	} {
		st, err := ClaudeCode{}.Settle(strings.NewReader(stream+"\n"), 1)
		if err != nil || st.None || st.Outcome != "abandoned" || !strings.HasPrefix(st.Reason, "credential_rejected") || strings.Contains(st.Reason, "CLI-TEXT") {
			t.Fatalf("settlement %+v err %v", st, err)
		}
	}
	// A 429 retry is not a refused token.
	st, _ := ClaudeCode{}.Settle(strings.NewReader(init+"\n"+`{"type":"system","subtype":"api_retry","session_id":"s1","error":"rate_limit","error_status":429}`+"\n"), 1)
	if !st.None {
		t.Fatalf("a 429 retry settled: %+v", st)
	}
}
