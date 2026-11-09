package reducer

import (
	"errors"
	"testing"

	"fabric/internal/domain"
)

// goodqa#15283 NEG-7, AC-1: a newly decided native start needs a valid
// worktree and records it on ATTEMPT_STARTED; a claude-code start may not
// carry one.
func TestStartAttemptWorktreeRule(t *testing.T) {
	admitted := func(id domain.MissionID) domain.Mission {
		m, err := Replay(id, views([]domain.Event{{Kind: domain.EvMissionAdmitted, Mission: id, Seq: 1}}))
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	api := Env{Provider: domain.ProviderAnthropicAPI, Cap: 4}
	native := func(wt string) domain.Command {
		return domain.Command{Kind: domain.CmdStartAttempt, Mission: "m_wt", Attempt: "r_wt", Host: "h_t2", Role: "writer",
			Path: domain.ProviderAnthropicAPI, LockPath: "/s/native-locks/r_wt.lock", Worktree: wt}
	}
	for _, wt := range []string{"", "srv/wt", "/", "/srv/../wt", "/srv/wt/", "/srv/w\u202et"} {
		if _, err := Decide(admitted("m_wt"), native(wt), api); !errors.Is(err, domain.ErrInvalidTransition) {
			t.Errorf("native start with worktree %q: %v, want ErrInvalidTransition", wt, err)
		}
	}
	evs, err := Decide(admitted("m_wt"), native("/srv/wt"), api)
	if err != nil || len(evs) != 1 || evs[0].Kind != domain.EvAttemptStarted || evs[0].Worktree != "/srv/wt" {
		t.Fatalf("native start: %v %+v, want ATTEMPT_STARTED with worktree /srv/wt", err, evs)
	}
	cc := domain.Command{Kind: domain.CmdStartAttempt, Mission: "m_cc", Attempt: "r_cc", Host: "h_t2", Role: "writer",
		Path: domain.ProviderClaudeCode, Worktree: "/srv/wt"}
	if _, err := Decide(admitted("m_cc"), cc, Env{Provider: domain.ProviderClaudeCode, Cap: 4}); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Errorf("claude-code start with a worktree: %v, want ErrInvalidTransition", err)
	}
	cc.Worktree = ""
	if _, err := Decide(admitted("m_cc"), cc, Env{Provider: domain.ProviderClaudeCode, Cap: 4}); err != nil {
		t.Errorf("control: claude-code start without a worktree: %v", err)
	}
}

// goodqa#15283 AC-9: a stored pre-0038 native ATTEMPT_STARTED, which has no
// worktree, replays unchanged: no error, and the attempt is live. The bytes
// and hash of that event are pinned by internal/domain's golden test.
func TestReplayPre0038NativeStartWithoutWorktree(t *testing.T) {
	log := []domain.Event{
		{Kind: domain.EvMissionAdmitted, Mission: "m_old", Seq: 1},
		{Kind: domain.EvAttemptStarted, Mission: "m_old", Seq: 2, Attempt: "r_old", Fence: 1, Host: "h_t2", Role: "writer",
			Path: domain.ProviderAnthropicAPI, LockPath: "/s/native-locks/r_old.lock", AttemptTo: domain.AttemptLeased},
		{Kind: domain.EvAttemptAcked, Mission: "m_old", Seq: 3, Attempt: "r_old", Fence: 1, AttemptTo: domain.AttemptRunning},
	}
	m, err := Replay("m_old", views(log))
	if err != nil {
		t.Fatalf("replay of a pre-0038 native start: %v", err)
	}
	if a, ok := m.Attempts["r_old"]; !ok || a.State != domain.AttemptRunning {
		t.Fatalf("replayed attempt %+v, want RUNNING", m.Attempts["r_old"])
	}
	if m.Seq != 3 {
		t.Fatalf("replay stopped at seq %d", m.Seq)
	}
}
