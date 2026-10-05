package handler

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	acp "github.com/coder/acp-go-sdk"
	"github.com/kfet/acp-kit/client"
	"github.com/kfet/zulip-acp/internal/journal"
)

// forkBranch gives a new branch conversation a copy of the origin's
// agent session (ACP session/fork), so the branch starts with the
// origin's context instead of an empty one. It reports whether it did.
//
// It is best effort. On any failure — no origin conversation (a
// human-only topic), no origin session, an agent that cannot fork, or a
// fork error — it logs and returns false, and the first turn opens a
// fresh session as before. The journal's origin record and
// history(origin: true) do not change.
//
// # The cwd is the CHILD's
//
// The session manager finds a conversation's session by listing the
// sessions in its directory and resuming the newest
// (state.Manager.tryResume). So the fork is filed in the CHILD's
// directory: that is where the child's first turn — and every relay
// restart after it — finds it. Filed in the parent's directory, it
// would never reach the child, and a restart would resume it as the
// PARENT. For the same reason the agent must list and resume sessions,
// or nothing would ever pick the fork up.
//
// An agent respawn needs nothing here: acp-kit tracks the forked
// session like any other and resumes it on the new process.
func (h *Handler) forkBranch(ctx context.Context, origin journal.Key, child journal.Conv) bool {
	parent, ok := h.cfg.Journal.Lookup(origin)
	if !ok {
		return false
	}
	if caps := h.cfg.Agent.Caps(); !caps.ListSessions || !caps.ResumeSession {
		h.cfg.Logf("handler: branch %s: agent cannot list and resume sessions, so a fork would be lost; starting a fresh session", child.ID)
		return false
	}
	sid, err := h.originSession(ctx, parent.ID)
	if err != nil {
		h.cfg.Logf("handler: branch %s: %v; starting a fresh session", child.ID, err)
		return false
	}
	cwd := h.convDir(child.ID)
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		h.cfg.Logf("handler: branch %s: %v; starting a fresh session", child.ID, err)
		return false
	}
	forked, err := h.cfg.Agent.ForkSession(ctx, cwd, sid, forkPoint(), discardSink{})
	if err != nil {
		if errors.Is(err, client.ErrForkUnsupported) {
			h.cfg.Logf("handler: branch %s: agent cannot fork sessions (%v); starting a fresh session", child.ID, err)
		} else {
			h.cfg.Logf("handler: branch %s: forking session %s failed (%v); starting a fresh session", child.ID, sid, err)
		}
		return false
	}
	h.cfg.Logf("handler: branch %s: forked session %s from %s (%s)", child.ID, forked, sid, parent.ID)
	return true
}

// originSession names the origin conversation's agent session without
// touching it. A live session is read from the manager. A session that
// idle GC has reaped is found the way the manager would resume it: the
// newest in the conversation's directory. The manager is NOT asked to
// resume it — that would rebind the sink of a turn that may have just
// started there, or open an empty session in its place.
func (h *Handler) originSession(ctx context.Context, convID string) (acp.SessionId, error) {
	if sid, _, live := h.cfg.Sessions.Live(convID); live {
		return sid, nil
	}
	list, err := h.cfg.Agent.ListSessions(ctx, h.convDir(convID))
	if err != nil {
		return "", fmt.Errorf("listing the origin's sessions failed (%v)", err)
	}
	if len(list) == 0 {
		return "", fmt.Errorf("the origin has no session to fork")
	}
	return acp.SessionId(list[0].SessionId), nil
}

// convDir is a conversation's working directory, as state.Manager lays
// it out.
func (h *Handler) convDir(convID string) string {
	return filepath.Join(h.cfg.Sessions.StateDir(), convsDir, convID)
}

// forkPoint is the agent entry id the fork is cut at (`_meta.at`).
//
// It is always empty. The relay sees Zulip message ids only; the agent
// never reports which of its session entries a Zulip message became,
// so no mapping exists. Empty forks at the parent's leaf — for `!branch`
// and the tool that is "now", which is the branch point. For a
// :fork_and_knife: on an older message the fork carries more than the
// branch point; history(origin: true) is still clamped at it.
func forkPoint() string { return "" }

// discardSink drops updates. A forked session runs no turn until the
// child's first prompt, and that turn binds its own sink.
type discardSink struct{}

func (discardSink) OnUpdate(context.Context, acp.SessionNotification) error { return nil }
