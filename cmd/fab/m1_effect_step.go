package main

import (
	"context"
	"path/filepath"

	"fabric/internal/domain"
	"fabric/internal/m1comment"
	"fabric/internal/m1effect"
	"fabric/internal/mission"
	"fabric/internal/sched"
	"fabric/internal/store"
)

// m1EffectStep is sched.Config.EffectStep on the M1 path (goodqa#15966
// AC-7, seam 3). Each call runs the pre-submit step with a
// m1effect.RedactedSource for that attempt: its RunDir is the redacted copy
// that settleM1 hands over, and its Creds are the attempt's bound values.
func m1EffectStep(step *m1effect.Step, base m1effect.RedactedSource) func(context.Context, sched.EffectAttempt, func(context.Context) error) string {
	return func(ctx context.Context, a sched.EffectAttempt, hb func(context.Context) error) string {
		src := base
		src.Creds = a.Creds
		st := *step
		st.Source = src
		return st.Run(ctx, m1effect.Attempt{Mission: a.Mission, Attempt: a.Attempt, Fence: a.Fence, RunDir: a.RunDir, Outcome: a.Outcome}, hb).Prefix()
	}
}

// m1SourceFor builds the RedactedSource from the daemon's own state: the
// mission config, the missions row and the attempt's files.json.
func m1SourceFor(st *store.Store, p *mission.Preparer, pinnedSHA, model string) m1effect.RedactedSource {
	return m1effect.RedactedSource{
		IssueID: func(m domain.MissionID) (string, error) {
			c, err := p.Load(m)
			if err != nil {
				return "", err
			}
			return c.IssueID(), nil
		},
		Ticket: func(ctx context.Context, m domain.MissionID) (string, error) {
			var t string
			err := st.Pool().QueryRow(ctx, `SELECT COALESCE(ticket, '') FROM missions WHERE id = $1`, string(m)).Scan(&t)
			return t, err
		},
		FilesJSON: func(a domain.AttemptID) string { return filepath.Join(p.AttemptRoot, string(a), mission.FilesFile) },
		Fields:    m1comment.Fields{Provider: string(domain.ProviderClaudeCode), ClaudeSHA: pinnedSHA, Model: model},
	}
}
