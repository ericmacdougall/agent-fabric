package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

// Preview is what a reviewer is shown before an edit or write runs, computed
// from the file as it is now (goodqa#15864). It is bound into the review
// request, so an approval is for this path, this file state and this change.
type Preview struct {
	Path         string // the cleaned relative path the call would change
	TargetSHA256 string // sha256 of the file it replaces, or "new"
	NewSHA256    string // sha256 of the bytes it would leave
	DiffSHA256   string // sha256 of the full, uncapped diff
	Shown        string // the capped text for the reviewer
	Truncated    bool
}

const (
	// MaxPreviewBytes caps each part of Preview.Shown (the diff, and for a
	// write the new content).
	MaxPreviewBytes = 16 << 10
	fullDiffBytes   = 8 << 20
)

// cut returns s limited to max bytes on a rune boundary, and whether it cut.
func cut(s string, max int) (string, bool) {
	if len(s) <= max {
		return s, false
	}
	s = s[:max]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s, true
}

func marker(what string, shown, total int, digest string) string {
	return fmt.Sprintf("\n[TRUNCATED %s: showing the first %d of %d bytes; the whole is bound into this request by sha256 %s]\n",
		what, shown, total, digest)
}

func buildPreview(rel string, pre snapshot, data []byte, withContent bool) Preview {
	target := "new"
	if pre.exists {
		target = pre.sha
	}
	full := unifiedDiff(rel, pre.data, data, fullDiffBytes)
	p := Preview{Path: rel, TargetSHA256: target, NewSHA256: sha256hex(data), DiffSHA256: sha256hex([]byte(full))}
	var b strings.Builder
	shownDiff, cutDiff := cut(strings.ToValidUTF8(full, "�"), MaxPreviewBytes)
	b.WriteString(shownDiff)
	if cutDiff {
		b.WriteString(marker("diff", len(shownDiff), len(full), p.DiffSHA256))
		p.Truncated = true
	}
	if withContent {
		content := strings.ToValidUTF8(string(data), "�")
		shown, cutContent := cut(content, MaxPreviewBytes)
		b.WriteString("\n--- new content ---\n" + shown)
		if cutContent {
			b.WriteString(marker("new content", len(shown), len(data), p.NewSHA256))
			p.Truncated = true
		}
	}
	p.Shown = b.String()
	return p
}

// Preview computes the preview of the edit in input. It writes nothing and
// creates nothing; it refuses what Run would refuse.
func (e *Edit) Preview(_ context.Context, input json.RawMessage) (Preview, error) {
	var in editIn
	if err := decodeStrict(input, &in); err != nil || in.Path == "" {
		return Preview{}, errors.New(`edit needs {"path": "relative/file", "edits": [{"oldText": "...", "newText": "..."}]}`)
	}
	pl, err := e.plan(in)
	if err != nil {
		return Preview{}, err
	}
	defer pl.release()
	return buildPreview(strings.Join(pl.parts, "/"), pl.pre, pl.result, false), nil
}

// Preview computes the preview of the write in input. A missing file, or a
// missing parent directory, previews as a new file; nothing is created.
func (w *Write) Preview(_ context.Context, input json.RawMessage) (Preview, error) {
	var in writeIn
	if err := decodeStrict(input, &in); err != nil || in.Path == "" || in.Content == nil {
		return Preview{}, errors.New(`write needs {"path": "relative/file", "content": "..."}`)
	}
	data := []byte(*in.Content)
	if len(data) > MaxFileBytes {
		return Preview{}, fmt.Errorf("content is larger than %d bytes", MaxFileBytes)
	}
	if err := validExpect(in.Expect); err != nil {
		return Preview{}, err
	}
	c := &w.core
	parts, err := c.resolve(in.Path)
	if err != nil {
		return Preview{}, err
	}
	var pre snapshot
	parent, made, err := c.parentDir(parts, false)
	switch {
	case errors.Is(err, errNoDir):
		// the directory does not exist yet: the file is new
	case err != nil:
		return Preview{}, err
	default:
		defer func() { unix.Close(parent); releaseDirs(made, false) }()
		if pre, err = loadTarget(parent, parts[len(parts)-1]); err != nil {
			return Preview{}, err
		}
	}
	if in.Expect != "" {
		cur := "new"
		if pre.exists {
			cur = pre.sha
		}
		if in.Expect != cur {
			return Preview{}, fmt.Errorf("expect_sha256 does not match the file (current %s)", cur)
		}
	}
	return buildPreview(strings.Join(parts, "/"), pre, data, true), nil
}
