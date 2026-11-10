package codex

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strings"
)

//go:embed local_inputs.py
var localInputsScript string

const inputPython = "/opt/homebrew/bin/python3"
const inputPythonSHA = "1955899cb535256cfa477d926d016819e52e747984722c44f76e39a4950fee0e"

// AuditLocalInputs returns only disable IDs and the required AGENTS source.
// First-party managed policy remains an explicit trusted dependency reviewed
// separately; it does not grant tools or override OS permissions in this mode.
func AuditLocalInputs(ctx context.Context, home, workspace string) ([]DisabledIntegration, string, error) {
	b, err := os.ReadFile(inputPython)
	if err != nil {
		return nil, "", errors.New("codex: input checker unavailable")
	}
	sum := sha256.Sum256(b)
	if hex.EncodeToString(sum[:]) != inputPythonSHA {
		return nil, "", errors.New("codex: input checker identity changed")
	}
	cmd := exec.CommandContext(ctx, inputPython, "-I", "-S", "-c", localInputsScript, workspace)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + home, "LC_ALL=C", "PYTHONDONTWRITEBYTECODE=1", "PYTHONNOUSERSITE=1"}
	cmd.Dir = workspace
	out, err := cmd.Output()
	if err != nil || len(out) > 16384 {
		return nil, "", errors.New("codex: local input closure refused")
	}
	var result struct {
		Disabled     []DisabledIntegration `json:"disabled"`
		GlobalAgents string                `json:"global_agents"`
	}
	dec := json.NewDecoder(strings.NewReader(string(out)))
	dec.DisallowUnknownFields()
	if dec.Decode(&result) != nil || result.GlobalAgents == "" {
		return nil, "", errors.New("codex: local input metadata unavailable")
	}
	return result.Disabled, result.GlobalAgents, nil
}
