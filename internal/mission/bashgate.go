package mission

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

// TestCommand is the one Bash command an M1 worker may run (AC-24).
const TestCommand = "./test.sh"

// maxHookInput bounds the hook's stdin.
const maxHookInput = 1 << 20

// BashGate decides a PreToolUse hook call (the JSON on the hook's stdin).
// It allows only a Bash call whose command is exactly "./test.sh" and
// refuses everything else: another tool, any other command string
// (arguments, separators, substitutions, an environment prefix, extra
// whitespace), a background run, malformed or oversized input, a missing
// field, or a repeated key. It never fails open.
func BashGate(in []byte) (allow bool, reason string) {
	if len(in) > maxHookInput {
		return false, "hook input too large"
	}
	if err := noDuplicateKeys(in); err != nil {
		return false, "malformed hook input"
	}
	// Exact, case-sensitive keys: Go's struct decoding would also accept
	// "Tool_Name" for tool_name.
	var top map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(in))
	if err := dec.Decode(&top); err != nil || top == nil {
		return false, "malformed hook input"
	}
	if _, err := dec.Token(); err != io.EOF {
		return false, "malformed hook input"
	}
	var event, tool, command string
	if json.Unmarshal(top["hook_event_name"], &event) != nil || event != "PreToolUse" {
		return false, "not a PreToolUse call"
	}
	if json.Unmarshal(top["tool_name"], &tool) != nil || tool != "Bash" {
		return false, "not a Bash call"
	}
	var input map[string]json.RawMessage
	if json.Unmarshal(top["tool_input"], &input) != nil || input == nil {
		return false, "no tool input"
	}
	if json.Unmarshal(input["command"], &command) != nil {
		return false, "no command"
	}
	if raw, ok := input["run_in_background"]; ok {
		var bg bool
		if json.Unmarshal(raw, &bg) != nil || bg {
			return false, "background runs are refused"
		}
	}
	if command != TestCommand {
		return false, "only " + TestCommand + " may run"
	}
	return true, ""
}

// RunBashGate is the hook process: exit 0 with no output allows (the
// permission rules still apply), exit 2 with a reason on stderr blocks the
// call. A panic is recovered into a block.
func RunBashGate(stdin io.Reader, stderr io.Writer) (code int) {
	defer func() {
		if recover() != nil {
			fmt.Fprintln(stderr, "fab-bash-gate: refused: internal error")
			code = 2
		}
	}()
	b, err := io.ReadAll(io.LimitReader(stdin, maxHookInput+1))
	if err != nil {
		fmt.Fprintln(stderr, "fab-bash-gate: refused: input unreadable")
		return 2
	}
	if ok, reason := BashGate(b); !ok {
		fmt.Fprintln(stderr, "fab-bash-gate: refused: "+reason)
		return 2
	}
	return 0
}

// noDuplicateKeys walks a JSON document and refuses any object that
// repeats a key (Go would keep the last one silently, and another parser
// might keep the first).
func noDuplicateKeys(b []byte) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	var walk func() error
	walk = func() error {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		d, ok := tok.(json.Delim)
		if !ok {
			return nil
		}
		switch d {
		case '{':
			seen := map[string]bool{}
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return err
				}
				k, _ := kt.(string)
				if seen[k] {
					return fmt.Errorf("duplicate key")
				}
				seen[k] = true
				if err := walk(); err != nil {
					return err
				}
			}
		case '[':
			for dec.More() {
				if err := walk(); err != nil {
					return err
				}
			}
		}
		_, err = dec.Token() // the closing delimiter
		return err
	}
	return walk()
}
