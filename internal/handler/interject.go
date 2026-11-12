package handler

import (
	"context"
	"time"

	"github.com/kfet/acp-kit/interject"
)

// interjectTimeout bounds one side question. The fork prompt has no
// bound of its own.
const interjectTimeout = 60 * time.Second

// interject handles a message that arrives while its conversation's
// main turn runs. It asks a fork of the main session the message, posts
// the short answer, and lets the fork's verdict decide what happens to
// the main turn. It reports false when it does not apply: the caller
// then supersedes the turn, as before.
//
// The fork can take seconds, so it runs off the event loop. When the
// fork fails (an agent without session/fork included), the message
// falls back to the old behaviour.
func (h *Handler) interject(ctx context.Context, r routed, msgID int64) bool {
	ij := h.cfg.Interject
	if ij == nil {
		return false
	}
	key := r.conv.SessionKey()
	sid, _, live := h.cfg.Sessions.Live(key)
	if !live || isSlashPrompt(r.prompt) || !h.turnRunning(key) {
		return false
	}
	h.ops.Add(1)
	go func() {
		defer h.ops.Done()
		release := h.ack(ctx, msgID)
		actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), interjectTimeout)
		rep, err := ij.Ask(actx, key, h.sessionCwd(key), sid, r.prompt)
		cancel()
		release()
		if err != nil {
			h.cfg.Logf("handler: interject in %s failed, superseding the turn: %v", r.conv.ID, err)
			h.startTurn(ctx, r.conv, r.prompt, r.addressed, msgID)
			return
		}
		if rep.Text != "" {
			h.reply(ctx, h.cfg.Journal.Current(r.conv.Key), rep.Text+"\n\n"+rep.Tag())
		}
		switch rep.Action {
		case interject.Inform:
			// Delivered: the running turn already has it. Otherwise
			// supersede the turn, as before; Take carries the side Q&A.
			if !rep.Delivered {
				h.startTurn(ctx, r.conv, r.prompt, r.addressed, msgID)
			}
		case interject.Abort:
			h.convo.Active().Stop(ctx, key)
		}
	}()
	return true
}

// turnRunning reports whether a turn runs for one session key. With
// per-user sessions, another user's turn in the same topic is not it.
func (h *Handler) turnRunning(key string) bool {
	_, ok := h.convo.Active().Get(key)
	return ok
}
