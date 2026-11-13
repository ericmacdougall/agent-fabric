// Package host is the fab-host side of the fabric: it turns a grant into a
// confined worker process on this machine, inventories and stops that
// process tree, and proves each confinement boundary with negative controls
// before advertising it (DESIGN.md 3.5, 3.7; R19, R23).
package host

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"fabric/internal/domain"
)

// Grant is the daemon's authorisation for one run. The model and effort are
// resolved by policy upstream; a host refuses to launch anything the grant
// does not name (R23, AC-15).
type Grant struct {
	Run          domain.AttemptID `json:"run"`
	Mission      domain.MissionID `json:"mission"`
	Fence        int64            `json:"fence"`
	Model        string           `json:"model"`
	Effort       string           `json:"effort"`
	Rung         string           `json:"rung"`
	TierTableSHA string           `json:"tier_table_sha"`
	ConfigSHA    string           `json:"config_sha"`
	Profile      string           `json:"profile"` // P0 | P1
	MaxTurns     int              `json:"max_turns"`
	MaxBudgetUSD float64          `json:"max_budget_usd"`
	Wall         time.Duration    `json:"wall"`
	Worktree     string           `json:"worktree"`
	RunDir       string           `json:"run_dir"`
	Token        string           `json:"token"` // per-run token in the worker's environment (FAB_RUN)
}

var ErrGrantInvalid = errors.New("grant refused")

// Validate fails closed: every field a launch depends on must be present and
// in range (R25). It never fills a default for the model.
func (g Grant) Validate() error {
	var problems []string
	if _, err := domain.ParseAttemptID(string(g.Run)); err != nil {
		problems = append(problems, "run id")
	}
	if _, err := domain.ParseMissionID(string(g.Mission)); err != nil {
		problems = append(problems, "mission id")
	}
	if g.Fence <= 0 {
		problems = append(problems, "fence must be positive")
	}
	if strings.TrimSpace(g.Model) == "" {
		problems = append(problems, "no policy-resolved model (R23)")
	}
	if g.Effort == "" {
		problems = append(problems, "no effort")
	}
	if g.Rung == "" || g.TierTableSHA == "" {
		problems = append(problems, "no rung or tier table digest")
	}
	if g.Profile != "P0" && g.Profile != "P1" {
		problems = append(problems, fmt.Sprintf("profile %q not offered by this host", g.Profile))
	}
	if g.MaxTurns <= 0 || g.MaxTurns > 1000 {
		problems = append(problems, "max_turns out of range")
	}
	if g.MaxBudgetUSD <= 0 || g.MaxBudgetUSD > 1000 {
		problems = append(problems, "max_budget_usd out of range")
	}
	if g.Wall <= 0 || g.Wall > 24*time.Hour {
		problems = append(problems, "wall out of range")
	}
	for name, p := range map[string]string{"worktree": g.Worktree, "run_dir": g.RunDir} {
		if !filepath.IsAbs(p) {
			problems = append(problems, name+" must be absolute")
			continue
		}
		if st, err := os.Stat(p); err != nil || !st.IsDir() {
			problems = append(problems, name+" is not a directory")
		}
	}
	if len(g.Token) < 16 {
		problems = append(problems, "token too short")
	}
	if len(problems) > 0 {
		return fmt.Errorf("%w: %s", ErrGrantInvalid, strings.Join(problems, "; "))
	}
	return nil
}

// Inventory is one live process attributed to a run, with the identity
// fields a stop must re-check before signalling (pid reuse).
type Process struct {
	PID     int
	PPID    int
	PGID    int
	Start   string // as the OS reports it; compared verbatim
	Command string
	Escaped bool // attributed to the run but found outside its unit or session
}

// StopReceipt is the evidence a stop leaves behind (R19).
type StopReceipt struct {
	Run        domain.AttemptID
	Reason     string
	Signalled  []int
	Survivors  []Process // non-empty is an alert (R28), never silence
	Escaped    []Process // processes found outside the unit or session: an alert even when reaped
	Verified   bool
	StartedAt  time.Time
	FinishedAt time.Time
}

// Profile is the per-OS execution profile contract (Kimi's ExecutionProfile,
// DESIGN.md 3.7). A profile may not advertise a capability its negative
// controls have not demonstrated on this host.
type Profile interface {
	// Name is the OS label the host advertises, e.g. linux-cgroup, darwin-token.
	Name() string
	// Start launches argv for the grant inside the profile's confinement and
	// returns once the process is running. The returned handle identifies the
	// run to Inventory and Stop.
	Start(g Grant, argv []string, env []string, stdout, stderr *os.File) (Handle, error)
	// Inventory lists every live process attributed to the run.
	Inventory(h Handle) ([]Process, error)
	// Stop ends every process of the run, verifies none remain and writes
	// the receipt. It never signals a process it cannot attribute.
	Stop(h Handle, reason string, grace time.Duration) (StopReceipt, error)
	// NegativeControls runs the boundary proofs for this host and returns
	// one result per control.
	NegativeControls(scratch string) ([]ControlResult, error)
}

// StdinStarter is a Profile whose Start can also connect the worker's stdin,
// for harness adapters that speak a protocol on stdio (goodqa#15373 D1). The
// confinement is Start's; only stdin differs. A nil stdin is Start.
type StdinStarter interface {
	StartIO(g Grant, argv []string, env []string, stdin, stdout, stderr *os.File) (Handle, error)
}

// Handle names a started run on this host.
// ErrWorkerManagerBusy reports definite prelaunch contention for the Linux
// worker manager: no unit or process was started (goodqa#15235 S5b).
var ErrWorkerManagerBusy = errors.New("worker manager busy")

type Handle struct {
	Run   domain.AttemptID
	Token string
	PID   int
	PGID  int
	Unit  string // Linux transient unit name
	// UnitSerial is the worker manager's run generation, so a stale handle
	// cannot stop a reused unit (goodqa#15235 S5b). Zero for other hosts.
	UnitSerial uint64
	// CGroup is the unit's cgroup path in sysfs, resolved once at Start so
	// inventory and stop never depend on the user bus (review H4).
	CGroup string
	// RunFile is a per-run file whose inherited descriptor attributes
	// descendants on macOS, where Apple platform binaries hide their
	// environment from other processes.
	RunFile string
}

// ControlResult is the outcome of one negative control.
type ControlResult struct {
	Name   string
	Passed bool
	Detail string
}
