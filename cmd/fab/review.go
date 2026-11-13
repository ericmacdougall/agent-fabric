package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"fabric/internal/native"
	"fabric/internal/reviewgate"
	"fabric/internal/store"
)

func reviewMain(args []string) error {
	if len(args) == 0 {
		return usageError{errors.New("review list|show|approve|reject|approve-safe-tools")}
	}
	switch args[0] {
	case "list":
		fs := flag.NewFlagSet("review list", flag.ContinueOnError)
		state := fs.String("state", "", "optional durable state, for example AWAITING_HUMAN")
		limit := fs.Int("limit", 100, "metadata page size (1..1000)")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		return withReviewOperator(func(ctx context.Context, op *reviewgate.Operator) error {
			requests, err := op.List(ctx, reviewgate.RequestState(*state), *limit)
			if err != nil {
				return err
			}
			return printReviewSummaries(requests)
		})
	case "show":
		if len(args) != 2 || args[1] == "" {
			return usageError{errors.New("review show REQUEST_HASH")}
		}
		return withReviewOperator(func(ctx context.Context, op *reviewgate.Operator) error {
			request, err := op.Inspect(ctx, args[1])
			if err != nil {
				return err
			}
			return printReviewDetail(os.Stdout, request)
		})
	case "approve":
		fs := flag.NewFlagSet("review approve", flag.ContinueOnError)
		hash := fs.String("hash", "", "exact request hash")
		reason := fs.String("reason", "", "operator reason (required; only its digest is audited)")
		expiresIn := fs.String("expires-in", "", "explicit approval lifetime, for example 30m (maximum 24h)")
		acceptTruncated := fs.Bool("accept-truncated", false, "approve an edit or write whose shown change was cut at the display cap")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		actor, err := reviewOperatorActor()
		if err != nil {
			return err
		}
		if err := validateReviewReason(*reason); err != nil {
			return err
		}
		expires, err := reviewApprovalExpiry(*expiresIn, time.Now())
		if err != nil {
			return err
		}
		if *hash == "" {
			return usageError{errors.New("review approve: --hash is required")}
		}
		return withReviewOperator(func(ctx context.Context, op *reviewgate.Operator) error {
			// Show what is approved before it is: the request must be one the
			// operator could see, and a change cut at the cap needs a flag.
			seen, err := op.Inspect(ctx, *hash)
			if err != nil {
				return err
			}
			if seen.Unbound {
				return usageError{errors.New("review approve: this edit or write predates file bindings and can never run; reject it")}
			}
			if seen.Target != nil && seen.Target.Truncated && !*acceptTruncated {
				return usageError{errors.New("review approve: the change is longer than the display cap (see `fab review show`); " +
					"the whole change is bound by its sha256, but you have not seen all of it. Pass --accept-truncated to approve anyway")}
			}
			decision, err := op.Approve(ctx, reviewgate.Approval{RequestHash: *hash, OperatorID: actor, Reason: *reason, ExpiresAt: expires})
			if err != nil {
				return err
			}
			fmt.Printf("review %s: %s; approval expires %s\n", decision.RequestHash, decision.State, expires.UTC().Format(time.RFC3339))
			return nil
		})
	case "reject":
		fs := flag.NewFlagSet("review reject", flag.ContinueOnError)
		hash := fs.String("hash", "", "exact request hash")
		reason := fs.String("reason", "", "operator reason (required; only its digest is audited)")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		actor, err := reviewOperatorActor()
		if err != nil {
			return err
		}
		if err := validateReviewReason(*reason); err != nil {
			return err
		}
		if *hash == "" {
			return usageError{errors.New("review reject: --hash is required")}
		}
		return withReviewOperator(func(ctx context.Context, op *reviewgate.Operator) error {
			decision, err := op.Reject(ctx, reviewgate.Rejection{RequestHash: *hash, Actor: actor, Reason: *reason})
			if err != nil {
				return err
			}
			fmt.Printf("review %s: %s\n", decision.RequestHash, decision.State)
			return nil
		})
	case "approve-safe-tools":
		return reviewApproveSafeTools(args[1:])
	default:
		return usageError{errors.New("review list|show|approve|reject|approve-safe-tools")}
	}
}

// reviewApproveSafeTools records an operator's approval that a mission runs
// with per-call review off for the tools its grant names (goodqa#15866). It
// binds the mission and the digest of the exact policy the grant produces.
func reviewApproveSafeTools(args []string) error {
	fs := flag.NewFlagSet("review approve-safe-tools", flag.ContinueOnError)
	grantPath := fs.String("grant", "", "the grant file whose review_gate.safe_tools is approved")
	reason := fs.String("reason", "", "operator reason (required; only its digest is audited)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	actor, err := reviewOperatorActor()
	if err != nil {
		return err
	}
	if err := validateReviewReason(*reason); err != nil {
		return err
	}
	if *grantPath == "" {
		return usageError{errors.New("review approve-safe-tools: --grant is required")}
	}
	g, err := native.LoadGrant(*grantPath)
	if err != nil {
		return err
	}
	if len(g.ReviewGate.SafeTools) == 0 {
		return usageError{errors.New("review approve-safe-tools: the grant sets no review_gate.safe_tools")}
	}
	digest, err := native.SafeToolsPolicyDigest(g.ReviewGate)
	if err != nil {
		return err
	}
	return withReviewOperator(func(ctx context.Context, op *reviewgate.Operator) error {
		e, err := op.ApproveSafeTools(ctx, reviewgate.SafeToolsApproval{MissionID: g.Mission, PolicyDigest: digest, OperatorID: actor, Reason: *reason})
		if err != nil {
			return err
		}
		fmt.Printf("safe_tools %v approved for mission %s by %s at %s (audit seq %d, policy digest %s)\n",
			g.ReviewGate.SafeTools, g.Mission, e.Actor, e.At.UTC().Format(time.RFC3339), e.Seq, digest)
		return nil
	})
}

func withReviewOperator(f func(context.Context, *reviewgate.Operator) error) error {
	return withStore(func(ctx context.Context, st *store.Store) error {
		op, err := reviewgate.NewOperator(reviewgate.NewPostgresStore(st.Pool()))
		if err != nil {
			return err
		}
		return f(ctx, op)
	})
}

func reviewOperatorActor() (string, error) {
	actor := os.Getenv("FABRIC_ACTOR")
	if !strings.HasPrefix(actor, "op:") || len(actor) <= len("op:") || len(actor) > 256 ||
		!utf8.ValidString(actor) || strings.ContainsAny(actor, "\x00\r\n\t ") {
		return "", usageError{errors.New("review approval/rejection requires FABRIC_ACTOR=op:name")}
	}
	return actor, nil
}

func validateReviewReason(reason string) error {
	if reason == "" || len(reason) > reviewgate.MaxAuditText || !utf8.ValidString(reason) || strings.ContainsRune(reason, '\x00') {
		return usageError{errors.New("review: --reason is required and must be valid UTF-8 of at most 1024 bytes")}
	}
	return nil
}

func reviewApprovalExpiry(value string, now time.Time) (time.Time, error) {
	if value == "" {
		return time.Time{}, usageError{errors.New("review approve: --expires-in is required")}
	}
	duration, err := time.ParseDuration(value)
	if err != nil || duration <= 0 || duration > 24*time.Hour {
		return time.Time{}, usageError{errors.New("review approve: --expires-in must be a positive duration no longer than 24h")}
	}
	return now.Add(duration), nil
}

func printReviewSummaries(requests []reviewgate.RequestSummary) error {
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	for _, request := range requests {
		if err := encoder.Encode(request); err != nil {
			return err
		}
	}
	return nil
}

// displaySafe renders operator-facing text that came from a file or a model:
// control characters (including ESC, which starts terminal escape sequences)
// and bidirectional or other format characters are shown as escapes, so the
// text cannot redraw the screen or reorder what the approver reads.
func displaySafe(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\n' || r == '\t':
			b.WriteRune(r)
		case r == utf8.RuneError || unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Zl, r) || unicode.Is(unicode.Zp, r):
			fmt.Fprintf(&b, "\\u{%04X}", r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// printReviewDetail is `fab review show`: the request summary and, for an edit
// or write, the target path, the file's sha256 and the change, as the
// approver will approve them.
func printReviewDetail(w io.Writer, request reviewgate.RequestSummary) error {
	shown := ""
	if request.Target != nil {
		shown = request.Target.Shown
		t := *request.Target
		t.Shown = ""
		t.Path = displaySafe(t.Path)
		request.Target = &t
	}
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(request); err != nil {
		return err
	}
	if request.Target != nil {
		fmt.Fprintf(w, "\n--- %s %s (target sha256 %s) ---\n%s\n--- end of the change (diff sha256 %s) ---\n",
			request.Tool, request.Target.Path, request.Target.TargetSHA256, displaySafe(shown), request.Target.DiffSHA256)
		if request.Target.Truncated {
			fmt.Fprintln(w, "NOTE: the change above was cut at the display cap; approving it needs --accept-truncated.")
		}
	}
	return nil
}
