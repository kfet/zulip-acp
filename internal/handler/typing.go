package handler

import (
	"context"
	"time"

	"github.com/kfet/zulip-acp/internal/journal"
	"github.com/kfet/zulip-acp/internal/zulipproto"
)

// Quiet mode's liveness signal.
//
// In streaming mode the relay posts an eager "Thinking…" placeholder
// and edits the answer into it. That placeholder is a real message, and
// Zulip pushes on message CREATION and never on an edit — so it costs
// the user a push notification whose body is the word "Thinking", and
// then a second one when the finished chain is re-posted. Quiet mode
// (`"stream_edits": false`) exists to make ONE push carrying the real
// answer, so it must not post a placeholder at all.
//
// What replaces it is Zulip's typing indicator: POST /typing with
// op=start. It renders as "the bot is typing…" in the conversation,
// generates no message, no unread and no push. A `start` expires
// server-side after server_typing_started_expiry_period_milliseconds
// (15s stock), so it is refreshed on a cadence read from the realm,
// and stopped on every turn exit path.
//
// (The source comment that used to justify the placeholder said "Zulip
// has no typing indicator". That was true when it was written; the
// channel-typing endpoint has since landed — verified on Zulip 12.2,
// feature level 500.)

// defaultTypingInterval is the cadence used when a Handler in quiet
// mode is given no explicit one: two thirds of Zulip's stock 15s
// expiry.
const defaultTypingInterval = 10 * time.Second

// TypingIntervalFor converts a server's typing expiry period into a
// refresh cadence with room to spare — two thirds of it, never less
// than a second.
//
// Exported because the cadence is settled at startup, from the realm
// snapshot, and handed to the Handler in its Config.
func TypingIntervalFor(expiry time.Duration) time.Duration {
	d := expiry * 2 / 3
	if d < time.Second {
		return time.Second
	}
	return d
}

// startTyping raises the typing indicator for a conversation and keeps
// it up until ctx is done, at which point it is lowered.
//
// It is the quiet-mode counterpart of startSpinner, and like the
// spinner it is decoration: every failure is logged and swallowed.
func (h *Handler) startTyping(ctx context.Context, key journal.Key) {
	period := h.cfg.TypingInterval
	if period <= 0 {
		return
	}
	t := time.NewTicker(period)
	go func() {
		defer t.Stop()
		typingLoop(ctx, h.typingSetter(key), t.C, h.cfg.ZulipCallTimeout)
	}()
}

// typingLoop is the testable core; see spinnerLoop.
//
// The stop is deferred, so it runs on cancellation, on a failed turn
// and on a turn that ended normally alike — there is no exit path that
// leaves the indicator stuck up. It is sent on a context detached from
// the turn's, because the turn's is precisely what has just been
// cancelled.
//
// A tick that races a cancellation may send one last `start` on a
// context that is already done. That costs a log line and nothing
// else — the deferred stop still runs — and the alternative, a
// pre-flight ctx.Err() check, is a branch no test can reach
// deterministically.
//
// The stop is bounded by stopTimeout rather than left open-ended: it
// runs detached from the turn, so a server that never answers would
// otherwise pin this goroutine for as long as the transport allows.
func typingLoop(ctx context.Context, set func(context.Context, string), tick <-chan time.Time, stopTimeout time.Duration) {
	defer func() {
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), stopTimeout)
		defer cancel()
		set(sctx, zulipproto.TypingStop)
	}()
	set(ctx, zulipproto.TypingStart)
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick:
			set(ctx, zulipproto.TypingStart)
		}
	}
}

// typingSetter binds one conversation to the typing endpoint. A DM is
// the `direct` form and a topic the `channel` form; the key is what
// knows which.
func (h *Handler) typingSetter(key journal.Key) func(context.Context, string) {
	return func(ctx context.Context, op string) {
		if err := h.cfg.Client.SetTyping(ctx, op, key.StreamID, key.Topic, key.UserIDs); err != nil {
			h.cfg.Logf("handler: typing %s for %s: %v", op, key.Label(), err)
		}
	}
}
