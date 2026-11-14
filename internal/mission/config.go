// Package mission loads an M1 mission config (goodqa#15966 AC-22, AC-24,
// AC-25): /srv/fabric/missions/{mission}/mission.json, owned by the
// operator with group fab-secret and mode 0640. It is read by fabd only in
// an M1 build, never stored in events, and refused before START, with no
// process started, when it is malformed, unsafe or over a cap.
package mission

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"fabric/internal/checkout"
	"fabric/internal/domain"
)

// ErrConfig refuses a mission config. Its messages name fields and caps,
// never field values, so a refusal log cannot echo config content.
var ErrConfig = errors.New("mission config refused")

// Ceilings (AC-25). A config over any of them is refused before launch.
const (
	MaxTurnsCeiling     = 40
	WallCeiling         = 45 * time.Minute
	MaxBudgetUSDCeiling = 2.0
)

// FileName is the config's name inside its mission directory.
const FileName = "mission.json"

// maxSize bounds the file read.
const maxSize = 64 << 10

// PinnedTools and PinnedAllow are the M1 tool surface (AC-24). A config
// must state exactly these; it can never widen them.
var (
	PinnedTools = []string{"Read", "Grep", "Glob", "Bash"}
	PinnedAllow = []string{"Bash(./test.sh)"}
)

var (
	fullSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)
	uuidRe  = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
)

// Caps are the per-mission limits, each within its ceiling.
type Caps struct {
	MaxTurns     int
	Wall         time.Duration
	MaxBudgetUSD float64
}

// Config is a loaded, validated mission config. Fields are reachable only
// through accessors so nothing can widen them after Load.
type Config struct {
	mission domain.MissionID
	issueID string
	brief   string
	repo    string
	sha     string
	caps    Caps
}

func (c *Config) Mission() domain.MissionID { return c.mission }

// IssueID is the GoodQA issue UUID the result comment goes to (W2).
func (c *Config) IssueID() string { return c.issueID }

// Brief is the absolute path of the brief (--append-system-prompt-file).
func (c *Config) Brief() string { return c.brief }

// Repo is the absolute path of the operator-owned bare mirror.
func (c *Config) Repo() string { return c.repo }

// SHA is the full 40-hex commit the worktree is extracted from.
func (c *Config) SHA() string { return c.sha }

func (c *Config) Caps() Caps { return c.caps }

// Tools is the --tools list (a copy).
func (c *Config) Tools() []string { return append([]string(nil), PinnedTools...) }

// Loader reads mission configs from Root (/srv/fabric/missions).
type Loader struct {
	Root         string
	Stat         checkout.Stater
	Who          checkout.Principals
	FabSecretGID uint32
}

// Path is the config path of mission m; m is validated first, so it can
// never name another directory.
func (l Loader) Path(m domain.MissionID) (string, error) {
	if _, err := domain.ParseMissionID(string(m)); err != nil {
		return "", fmt.Errorf("%w: mission id: %v", ErrConfig, err)
	}
	if strings.ContainsAny(string(m), "/\\") || string(m) == "." || string(m) == ".." {
		return "", fmt.Errorf("%w: mission id is not a single path component", ErrConfig)
	}
	if !filepath.IsAbs(l.Root) || filepath.Clean(l.Root) != l.Root {
		return "", fmt.Errorf("%w: missions root is not an absolute clean path", ErrConfig)
	}
	return filepath.Join(l.Root, string(m), FileName), nil
}

// Load opens, checks and parses mission m's config.
func (l Loader) Load(m domain.MissionID) (*Config, error) {
	if err := l.Who.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrConfig, err)
	}
	if l.Who.WorkerInGroup(l.FabSecretGID) {
		return nil, fmt.Errorf("%w: the worker account is in the fab-secret group", ErrConfig)
	}
	p, err := l.Path(m)
	if err != nil {
		return nil, err
	}
	if err := checkout.CheckAncestors(l.Stat, l.Who, filepath.Dir(p), "mission config directory"); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrConfig, err)
	}
	f, err := os.OpenFile(p, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("%w: %s cannot be opened (a symlink is refused): %v", ErrConfig, FileName, errors.Unwrap(err))
	}
	defer f.Close()
	i, err := l.Stat.Fstat(f)
	if err != nil {
		return nil, fmt.Errorf("%w: %s cannot be checked", ErrConfig, FileName)
	}
	if err := l.checkFile(i); err != nil {
		return nil, err
	}
	b, err := io.ReadAll(io.LimitReader(f, maxSize+1))
	if err != nil {
		return nil, fmt.Errorf("%w: %s cannot be read", ErrConfig, FileName)
	}
	if len(b) > maxSize {
		return nil, fmt.Errorf("%w: %s is larger than %d bytes", ErrConfig, FileName, maxSize)
	}
	c, err := Parse(b)
	if err != nil {
		return nil, err
	}
	c.mission = m
	if err := l.checkBrief(c.brief); err != nil {
		return nil, err
	}
	return c, nil
}

// checkFile is the AC-22 rule on the open descriptor: a regular file, mode
// exactly 0640, owned by the operator, group fab-secret.
func (l Loader) checkFile(i checkout.Info) error {
	switch {
	case !i.Mode.IsRegular():
		return fmt.Errorf("%w: %s is not a regular file", ErrConfig, FileName)
	case i.Mode.Perm() != 0o640 || i.Mode&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0:
		return fmt.Errorf("%w: %s has mode %v, want 0640", ErrConfig, FileName, i.Mode.Perm())
	case i.UID != l.Who.OperatorUID:
		return fmt.Errorf("%w: %s is owned by uid %d, not the operator", ErrConfig, FileName, i.UID)
	case i.GID != l.FabSecretGID:
		return fmt.Errorf("%w: %s has group %d, not fab-secret", ErrConfig, FileName, i.GID)
	}
	return nil
}

// checkBrief: the brief is read by the claude process as fab-worker, so it
// must be readable by it, and must not be changeable by it.
func (l Loader) checkBrief(p string) error {
	// The ancestor error names directories; the brief's are config values.
	if err := checkout.CheckAncestors(l.Stat, l.Who, filepath.Dir(p), "brief directory"); err != nil {
		return fmt.Errorf("%w: brief: a directory above it is not operator- or root-owned, or is worker-writable", ErrConfig)
	}
	i, err := l.Stat.Lstat(p)
	if err != nil {
		return fmt.Errorf("%w: brief cannot be checked", ErrConfig)
	}
	if !i.Mode.IsRegular() {
		return fmt.Errorf("%w: brief is not a regular file (a symlink is refused)", ErrConfig)
	}
	if err := l.Who.CheckOwned(i, "brief"); err != nil {
		return fmt.Errorf("%w: %v", ErrConfig, err)
	}
	if !l.Who.ReadableByWorker(i) {
		return fmt.Errorf("%w: brief is not readable by the worker account", ErrConfig)
	}
	return nil
}

// Parse validates the JSON document. Keys are exact and case-sensitive;
// unknown, duplicate and missing keys refuse.
func Parse(b []byte) (*Config, error) {
	if !utf8.Valid(b) {
		return nil, fmt.Errorf("%w: not valid UTF-8", ErrConfig)
	}
	top, err := strictObject(b, "mission config", []string{"version", "issue_id", "brief", "repo", "sha", "caps", "tools", "allow"})
	if err != nil {
		return nil, err
	}
	var version int
	if err := num(top["version"], "version", &version); err != nil || version != 1 {
		return nil, fmt.Errorf("%w: version must be 1", ErrConfig)
	}
	c := &Config{}
	if err := str(top["issue_id"], "issue_id", &c.issueID); err != nil {
		return nil, err
	}
	if !uuidRe.MatchString(c.issueID) {
		return nil, fmt.Errorf("%w: issue_id is not a lower-case UUID", ErrConfig)
	}
	if err := str(top["brief"], "brief", &c.brief); err != nil {
		return nil, err
	}
	if err := str(top["repo"], "repo", &c.repo); err != nil {
		return nil, err
	}
	for name, p := range map[string]string{"brief": c.brief, "repo": c.repo} {
		if !filepath.IsAbs(p) || filepath.Clean(p) != p || p == "/" {
			return nil, fmt.Errorf("%w: %s is not an absolute clean path", ErrConfig, name)
		}
	}
	if err := str(top["sha"], "sha", &c.sha); err != nil {
		return nil, err
	}
	if !fullSHA.MatchString(c.sha) {
		return nil, fmt.Errorf("%w: sha is not a full lower-case 40-hex commit id", ErrConfig)
	}
	caps, err := strictObject(top["caps"], "caps", []string{"max_turns", "wall_minutes", "max_budget_usd"})
	if err != nil {
		return nil, err
	}
	var turns, wall int
	var budget float64
	if err := num(caps["max_turns"], "caps.max_turns", &turns); err != nil {
		return nil, err
	}
	if err := num(caps["wall_minutes"], "caps.wall_minutes", &wall); err != nil {
		return nil, err
	}
	if err := num(caps["max_budget_usd"], "caps.max_budget_usd", &budget); err != nil {
		return nil, err
	}
	c.caps = Caps{MaxTurns: turns, Wall: time.Duration(wall) * time.Minute, MaxBudgetUSD: budget}
	// Clamped before the multiplication can overflow.
	switch {
	case wall > int(WallCeiling/time.Minute):
		c.caps.Wall = WallCeiling + time.Minute
	case wall < 0:
		c.caps.Wall = -1
	}
	if err := CheckCaps(c.caps); err != nil {
		return nil, err
	}
	var tools, allow []string
	if err := strs(top["tools"], "tools", &tools); err != nil {
		return nil, err
	}
	if err := strs(top["allow"], "allow", &allow); err != nil {
		return nil, err
	}
	if !equal(tools, PinnedTools) {
		return nil, fmt.Errorf("%w: tools must be exactly %s", ErrConfig, strings.Join(PinnedTools, ","))
	}
	if !equal(allow, PinnedAllow) {
		return nil, fmt.Errorf("%w: allow must be exactly %s", ErrConfig, strings.Join(PinnedAllow, ","))
	}
	return c, nil
}

// CheckCaps refuses caps that are not positive or exceed a ceiling (AC-25).
func CheckCaps(c Caps) error {
	switch {
	case c.MaxTurns <= 0:
		return fmt.Errorf("%w: caps.max_turns must be positive", ErrConfig)
	case c.MaxTurns > MaxTurnsCeiling:
		return fmt.Errorf("%w: caps.max_turns exceeds the ceiling of %d", ErrConfig, MaxTurnsCeiling)
	case c.Wall <= 0:
		return fmt.Errorf("%w: caps.wall_minutes must be positive", ErrConfig)
	case c.Wall > WallCeiling:
		return fmt.Errorf("%w: caps.wall_minutes exceeds the ceiling of %d", ErrConfig, int(WallCeiling/time.Minute))
	case math.IsNaN(c.MaxBudgetUSD) || math.IsInf(c.MaxBudgetUSD, 0) || c.MaxBudgetUSD < 0.01:
		// The launch prints two decimals; less than a cent would read 0.00.
		return fmt.Errorf("%w: caps.max_budget_usd must be at least 0.01", ErrConfig)
	case c.MaxBudgetUSD > MaxBudgetUSDCeiling:
		return fmt.Errorf("%w: caps.max_budget_usd exceeds the ceiling of %.2f", ErrConfig, MaxBudgetUSDCeiling)
	}
	return nil
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// strictObject decodes one JSON object whose keys are exactly want: no
// unknown, duplicate (Go keeps the last silently) or missing key, and
// nothing after it.
func strictObject(raw []byte, label string, want []string) (map[string]json.RawMessage, error) {
	if raw == nil {
		return nil, fmt.Errorf("%w: %s is missing", ErrConfig, label)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if d, ok := tok.(json.Delim); err != nil || !ok || d != '{' {
		return nil, fmt.Errorf("%w: %s is not a JSON object", ErrConfig, label)
	}
	allowed := map[string]bool{}
	for _, k := range want {
		allowed[k] = true
	}
	out := map[string]json.RawMessage{}
	for dec.More() {
		tok, err := dec.Token()
		k, ok := tok.(string)
		if err != nil || !ok {
			return nil, fmt.Errorf("%w: %s is not a JSON object", ErrConfig, label)
		}
		if !allowed[k] {
			return nil, fmt.Errorf("%w: %s has a key outside (%s)", ErrConfig, label, strings.Join(want, ", "))
		}
		if _, dup := out[k]; dup {
			return nil, fmt.Errorf("%w: %s repeats the key %s", ErrConfig, label, k)
		}
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, fmt.Errorf("%w: %s.%s is not valid JSON", ErrConfig, label, k)
		}
		out[k] = v
	}
	if _, err := dec.Token(); err != nil {
		return nil, fmt.Errorf("%w: %s is not a JSON object", ErrConfig, label)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("%w: %s has trailing data", ErrConfig, label)
	}
	for _, k := range want {
		if _, ok := out[k]; !ok {
			return nil, fmt.Errorf("%w: %s is missing %s", ErrConfig, label, k)
		}
	}
	return out, nil
}

func str(raw json.RawMessage, name string, dst *string) error {
	if err := json.Unmarshal(raw, dst); err != nil || *dst == "" {
		return fmt.Errorf("%w: %s must be a non-empty string", ErrConfig, name)
	}
	return nil
}

func strs(raw json.RawMessage, name string, dst *[]string) error {
	if err := json.Unmarshal(raw, dst); err != nil || *dst == nil {
		return fmt.Errorf("%w: %s must be a list of strings", ErrConfig, name)
	}
	return nil
}

// num decodes a JSON number into an int or a float64; an int must be
// integral and in range.
func num(raw json.RawMessage, name string, dst any) error {
	// json.Number also accepts a quoted number; only a bare number counts.
	if t := bytes.TrimSpace(raw); len(t) == 0 || !(t[0] == '-' || (t[0] >= '0' && t[0] <= '9')) {
		return fmt.Errorf("%w: %s must be a number", ErrConfig, name)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var n json.Number
	if err := dec.Decode(&n); err != nil {
		return fmt.Errorf("%w: %s must be a number", ErrConfig, name)
	}
	switch d := dst.(type) {
	case *int:
		v, err := n.Int64()
		if err != nil || v > math.MaxInt32 || v < math.MinInt32 {
			return fmt.Errorf("%w: %s must be an integer", ErrConfig, name)
		}
		*d = int(v)
	case *float64:
		v, err := n.Float64()
		if err != nil {
			return fmt.Errorf("%w: %s must be a number", ErrConfig, name)
		}
		*d = v
	}
	return nil
}
