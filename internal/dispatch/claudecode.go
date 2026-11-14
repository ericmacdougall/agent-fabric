package dispatch

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"fabric/internal/domain"
	"fabric/internal/host"
	"fabric/internal/provider/claude"
)

// StdinFakeSHA256 is the reviewed sha256 of tools/fakeclaude/fakeclaude-stdin.sh.
// Until a recorded probe shows the real claude CLI in stream-json input mode
// passing claude.Parse (goodqa#15373 NEG-11), registry mode runs only a
// provider file with this hash. TestStdinFakePinned keeps it equal to the file.
const StdinFakeSHA256 = "d72cc2aea59c6cc19de5e3ba278d3ee1e3a53ee2424a6dd8f563c2aa70788684"

// RecordedFakeSHA256 is the reviewed sha256 of tools/fakeclaude/fakeclaude.sh,
// the recorded argv fake (installed as fakeclaude-live). The same pin is
// FAKE_SHA in tools/fabd-install/install.sh and DELEGATE_SHA in the stdin
// fake; TestRecordedFakePinned keeps all four equal. With StdinFakeSHA256
// it is the only provider identity a host without a demonstrated worker
// boundary may launch (goodqa#15976 AC-2).
const RecordedFakeSHA256 = "50d4f1d864de7b2b2ab38808d4414a3adf8a2c4b0fc0047571bdb515689d5285"

// PinnedFake reports whether sha is a reviewed fake provider's sha256.
// Anything else, including a file renamed to look like a fake, is real.
func PinnedFake(sha string) bool {
	return sha == StdinFakeSHA256 || sha == RecordedFakeSHA256
}

// ClaudeCode is the claude-code dispatcher. Plan uses claude.StreamArgv, so
// the prompt travels on stdin and in no argv element. Settle is the body
// today's scheduler settled every claude-code stream with.
type ClaudeCode struct {
	// PinSHA256, when set, is checked against the provider file each launch
	// resolved (argv[0]); a mismatch refuses the launch (NEG-11), so a file
	// swapped or a symlink repointed after fabd started cannot run unpinned.
	PinSHA256 string
}

// Name is the provider name START writes as Path.
func (ClaudeCode) Name() string { return string(domain.ProviderClaudeCode) }

// Plan builds the stream-json launch and its one stdin message.
func (c ClaudeCode) Plan(g host.Grant, in Input) (Launch, error) {
	l := in.Launch
	l.Prompt = in.Prompt
	argv, env, err := claude.StreamArgv(g, l)
	if err != nil {
		return Launch{}, err
	}
	if c.PinSHA256 != "" {
		got, err := FileSHA256(argv[0])
		if err != nil {
			return Launch{}, fmt.Errorf("%w: provider pin: %v", host.ErrGrantInvalid, err)
		}
		if got != c.PinSHA256 {
			return Launch{}, fmt.Errorf("%w: provider %s has sha256 %s, not the pinned stdin fake %s", host.ErrGrantInvalid, argv[0], got, c.PinSHA256)
		}
	}
	stdin, err := claude.StreamInput(in.Prompt)
	if err != nil {
		return Launch{}, err
	}
	return Launch{Argv: argv, Env: env, StdinBytes: stdin}, nil
}

// FileSHA256 hashes the file at path.
func FileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Settle decides the finish from the stream and the worker's exit code. A
// read failure is a decision (abandoned, "stream unreadable"), with no
// usage. The cases and reasons are bd56178's settleCommand, unchanged; the
// goldens under internal/sched/testdata/streams pin them.
func (ClaudeCode) Settle(stream io.Reader, code int) (Settlement, error) {
	fin := func(outcome, waitKey, reason string, usage []byte) (Settlement, error) {
		return Settlement{Outcome: outcome, WaitKey: waitKey, Reason: reason, Usage: usage}, nil
	}
	run, perr := claude.Parse(stream, true)
	if perr != nil {
		return fin("abandoned", "", "stream unreadable: "+perr.Error(), nil)
	}
	usage, _ := json.Marshal(map[string]any{"session": run.SessionID, "turns": run.Turns, "context_last_turn": run.LastTurn.Context(), "cost_usd": run.TotalCostUSD, "exit": code})
	switch run.End {
	case claude.EndCheckpoint:
		// goodqa#15082 T1: success needs the process to agree (exit 0) and
		// the stream to be coherent; otherwise the run is abandoned, never
		// verified on the strength of its last event.
		if code != 0 {
			return fin("abandoned", "", fmt.Sprintf("exit %d with a success result", code), usage)
		}
		if len(run.Contradictions) > 0 {
			return fin("abandoned", "", "contradictory stream: "+run.Contradictions[0], usage)
		}
		var cp struct {
			End struct {
				Kind  string `json:"kind"`
				Event string `json:"event"`
			} `json:"end"`
		}
		if err := json.Unmarshal(run.StructuredOutput, &cp); err != nil || cp.End.Kind == "" {
			return fin("abandoned", "", "checkpoint without an end kind", usage)
		}
		switch cp.End.Kind {
		case "wait":
			return fin("wait", cp.End.Event, "", usage)
		case "checkpoint", "ready_for_review", "blocked":
			return fin(cp.End.Kind, "", "", usage)
		default:
			return fin("abandoned", "", "unknown end kind "+cp.End.Kind, usage)
		}
	case claude.EndAbandoned:
		return fin("abandoned", "", "no structured output", usage)
	case claude.EndFailed:
		return fin("abandoned", "", "result "+run.ResultSubtype, usage)
	default:
		// No result event: the host and provider do not agree; the attempt
		// goes UNKNOWN through lease expiry, never to a relaunch (3.4).
		return Settlement{None: true, Usage: usage}, nil
	}
}
