package m1effect

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"

	"fabric/internal/domain"
	"fabric/internal/m1comment"
)

// Redacted-copy layout (sched's settle writes it; goodqa#15966 AC-15).
const (
	redactedDir  = "redacted"
	redactedDone = "redacted.done"
	streamFile   = "stream.jsonl"
	// maxStream is the most fabd reads of a stream (AC-16 (8)).
	maxStream = 64 << 20
	maxLine   = 16 << 20
)

// Source error classes (delivery_blocked).
const (
	ClassNotRedacted  = "source_not_redacted"
	ClassNoResult     = "source_no_result"
	ClassTicket       = "source_ticket"
	ClassFiles        = "source_files"
	ClassIssue        = "source_issue"
	ClassNoBoundCreds = "credential_material_unavailable"
)

var ticketRE = regexp.MustCompile(`(?i)^(?:goodqa )?goodqa#([1-9][0-9]{0,9})$`)

// RedactedSource is the production Source (goodqa#15966 seam 3). Every part
// of the Delivery comes from fabd, never from the raw stream:
//   - IssueID: the mission config (mission.json issue_id, AC-35(e));
//   - Ticket: missions.ticket, as "GoodQA goodqa#N";
//   - Result: the final result text of the redacted copy only. RunDir must
//     be the attempt's redacted directory, with its redaction finished;
//   - Allow: the extract's files.json plus the mission and attempt ids
//     (AC-35(a));
//   - Creds: the attempt's bound credentials (AC-18).
type RedactedSource struct {
	IssueID func(domain.MissionID) (string, error)
	Ticket  func(context.Context, domain.MissionID) (string, error)
	// FilesJSON is the path of the attempt's files.json (fabd's own git
	// archive listing in the operator-owned attempt directory).
	FilesJSON func(domain.AttemptID) string
	Fields    m1comment.Fields // Provider, ClaudeSHA, Model; Mission is set per call
	Creds     m1comment.BoundCredentials
}

func classErr(class string, err error) error { return &ClassError{Class: class, Err: err} }

// Prepare is Source.Prepare.
func (s RedactedSource) Prepare(ctx context.Context, a Attempt) (Delivery, error) {
	if s.Creds == nil {
		return Delivery{}, classErr(ClassNoBoundCreds, errors.New("no bound credentials"))
	}
	text, err := redactedResult(a.RunDir)
	if err != nil {
		return Delivery{}, err
	}
	issue, err := s.IssueID(a.Mission)
	if err != nil || issue == "" {
		return Delivery{}, classErr(ClassIssue, fmt.Errorf("issue id: %v", err))
	}
	raw, err := s.Ticket(ctx, a.Mission)
	if err != nil {
		return Delivery{}, classErr(ClassTicket, err)
	}
	m := ticketRE.FindStringSubmatch(raw)
	if m == nil {
		return Delivery{}, classErr(ClassTicket, errors.New("mission ticket is not goodqa#N"))
	}
	paths, err := readFiles(s.FilesJSON(a.Attempt))
	if err != nil {
		return Delivery{}, classErr(ClassFiles, err)
	}
	f := s.Fields
	f.Mission = string(a.Mission)
	return Delivery{IssueID: issue, Ticket: "GoodQA goodqa#" + m[1], Fields: f, Result: text,
		Allow: m1comment.Allow{Paths: paths, Mission: string(a.Mission), Attempt: string(a.Attempt)}, Creds: s.Creds}, nil
}

// redactedResult reads the final result text from dir/stream.jsonl. dir
// must be a finished redacted copy: named "redacted", with the sibling
// redacted.done that settle writes only after both copies are durable.
// Anything else (the raw stream directory included) is refused.
func redactedResult(dir string) (string, error) {
	if filepath.Base(dir) != redactedDir {
		return "", classErr(ClassNotRedacted, errors.New("effect source is not the redacted copy"))
	}
	if _, err := os.Lstat(filepath.Join(filepath.Dir(dir), redactedDone)); err != nil {
		return "", classErr(ClassNotRedacted, errors.New("redaction not finished"))
	}
	f, err := os.Open(filepath.Join(dir, streamFile))
	if err != nil {
		return "", classErr(ClassNoResult, err)
	}
	defer f.Close()
	sc := bufio.NewScanner(io.LimitReader(f, maxStream))
	sc.Buffer(make([]byte, 64<<10), maxLine)
	text, found := "", false
	for sc.Scan() {
		var e struct {
			Type   string  `json:"type"`
			Result *string `json:"result"`
		}
		if json.Unmarshal(sc.Bytes(), &e) != nil || e.Type != "result" {
			continue
		}
		if found {
			return "", classErr(ClassNoResult, errors.New("second result event"))
		}
		found = true
		if e.Result != nil {
			text = *e.Result
		}
	}
	if err := sc.Err(); err != nil {
		return "", classErr(ClassNoResult, err)
	}
	if !found || text == "" {
		return "", classErr(ClassNoResult, errors.New("no result text"))
	}
	return text, nil
}

// readFiles reads files.json: a JSON array of the extract's paths.
func readFiles(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var paths []string
	if err := json.Unmarshal(data, &paths); err != nil {
		return nil, fmt.Errorf("files.json: %w", err)
	}
	return paths, nil
}
