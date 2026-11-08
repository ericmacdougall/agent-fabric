package reviewgate

import "encoding/json"

// ApprovalRequestHash extracts only a well-formed durable pause control. Tool
// output that merely resembles JSON cannot synthesize a native resume wait.
func ApprovalRequestHash(result string) (string, bool) {
	var parsed ToolGateResult
	if json.Unmarshal([]byte(result), &parsed) != nil || parsed.Status != "approval_required" || !validHash(parsed.RequestHash) {
		return "", false
	}
	return parsed.RequestHash, true
}
