package sweep

import (
	"bytes"
	"encoding/json"

	"fabric/internal/host"
)

// Unit properties for the sweep unit (AC-16 (8)); the worker unit adds
// LimitFSIZE=256M.
var UnitProperties = []string{"RuntimeMaxSec=125", "MemoryMax=256M", "LimitCORE=0"}

// Env is the sweep's fixed environment. It never carries
// CLAUDE_CODE_OAUTH_TOKEN, FABRIC_DSN or the GoodQA key.
var Env = []string{"PATH=/usr/bin:/bin", "LANG=C"}

// Fabd-side incomplete reasons.
const (
	ReasonStopUnverified = "stop_unverified"
	ReasonExit           = "sweep_exit"
	ReasonReport         = "report_malformed"
	ReasonLaunch         = "launch_error"
)

// Launcher runs the sealed fab binary as fab-worker through the worker
// manager with argv, env and stdin, returning stdout and the exit code.
type Launcher func(argv, env []string, stdin []byte) (stdout []byte, exit int, err error)

const maxReport = 1 << 20

// Fabd runs the sweep at settle. It runs only when the stop receipt is
// Verified with no survivors and no escaped processes (W1's rc.Verified);
// otherwise the sweep is incomplete and nothing is launched. Argv carries
// only the run roots; the input travels on stdin.
func Fabd(rc host.StopReceipt, fabBinary string, roots []string, input []byte, launch Launcher) Report {
	bad := func(reason string) Report {
		return Report{Reasons: []string{reason}, Entries: map[string]int{}, Counts: map[string]int{}}
	}
	if !rc.Verified || len(rc.Survivors) > 0 || len(rc.Escaped) > 0 {
		return bad(ReasonStopUnverified)
	}
	argv := append([]string{fabBinary, "__sweep", "--"}, roots...)
	out, code, err := launch(argv, append([]string(nil), Env...), input)
	if err != nil {
		return bad(ReasonLaunch)
	}
	if code != 0 {
		return bad(ReasonExit)
	}
	if len(out) > maxReport {
		return bad(ReasonReport)
	}
	dec := json.NewDecoder(bytes.NewReader(out))
	dec.DisallowUnknownFields()
	var rep Report
	if err := dec.Decode(&rep); err != nil || dec.More() || rep.Entries == nil || rep.Counts == nil {
		return bad(ReasonReport)
	}
	if rep.Complete != (len(rep.Reasons) == 0) {
		return bad(ReasonReport)
	}
	return rep
}

// Exposed reports a non-zero count for any label or the literal.
func (r Report) Exposed() bool {
	for _, n := range r.Counts {
		if n > 0 {
			return true
		}
	}
	return false
}
