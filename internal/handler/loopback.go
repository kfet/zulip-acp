// This file is zulip-acp's half of the agent→relay loopback.
//
// The generic half — the MCP transport, the tool set, and the shared
// session-control actions — lives in acp-kit (mcphost, relaytool,
// command, schedule). What stays here is what only Zulip knows:
//
//  1. Turning a conv-id (the state manager's session key, which is what
//     mcphost binds from the connection token) into the broker's KEY
//     token, and back into a place to post.
//  2. Posting out of band, through the same rollover splitter every
//     agent answer goes through — because Zulip truncates at 10000 code
//     points SILENTLY, and a `post` tool that bypassed the splitter
//     would be a brand-new way to lose output.
//  3. Re-applying the relay's gates when a scheduled prompt fires with
//     no human in the loop.
//
// # The loop hazard
//
// The agent posts, Zulip delivers that message back as an event, and
// the relay must not treat its own words as a new turn. handleMessage's
// FIRST guard is `m.SenderID == h.cfg.BotUserID`, before any allowlist.
// That guard was always correct; the loopback is what makes it
// load-bearing, so it has a test of its own
// (TestLoopbackPostDoesNotFeedItselfBack).
package handler

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/kfet/acp-kit/command"
	"github.com/kfet/acp-kit/schedule"
	"github.com/kfet/zulip-acp/internal/journal"
	"github.com/kfet/zulip-acp/internal/rollover"
)

// scheduledSender is the name a scheduled prompt is attributed to. Turns
// are prefixed with their sender so the agent can tell who is speaking;
// a scheduled turn must not masquerade as the human who armed it.
const scheduledSender = "scheduled"

// ConvToken maps an MCP session key — the conv-id, resolved server-side
// from the connection's token — to the command broker's conversation
// token. It is relaytool's Config.ConvToken.
//
// ok=false rejects the tool call. That happens when the journal has no
// such conv-id at all, which means the caller is not a conversation
// this relay owns.
func (h *Handler) ConvToken(sessionKey string) (string, bool) {
	c, ok := h.cfg.Journal.LookupID(sessionKey)
	if !ok {
		h.cfg.Logf("handler: loopback call from unknown conversation %q", sessionKey)
		return "", false
	}
	return c.Key.Token(), true
}

// PostTo satisfies command.Poster: it sends a message into the
// conversation the tool call came from, out of band.
//
// There is no target parameter anywhere in this path, by design — see
// command.Poster. The key comes from the token, so the worst an agent
// can do is talk in the topic it was already talking in.
func (h *Handler) PostTo(token, text string) error {
	key, err := journal.ParseToken(token)
	if err != nil {
		return err
	}
	post := &convPoster{client: h.cfg.Client, key: key}
	split, err := rollover.New(rollover.Config{
		Poster:     post,
		Budget:     h.cfg.Budget,
		SealMarker: h.cfg.SealMarker,
		ContMarker: h.cfg.ContinuationMarker,
	})
	if err != nil {
		return err
	}
	// Through the splitter, not straight to SendMessage: Zulip
	// truncates past MAX_MESSAGE_LENGTH silently, and an out-of-band
	// post is exactly as capable of being long as an answer is.
	//
	// Close, with no Start: an out-of-band post is complete when it is
	// composed, so there is nothing to hold a placeholder for.
	//
	// Bounded, and detached from any caller: the tool call is answered
	// only when this returns, so an unbounded context would let one
	// wedged Zulip request hang the agent's turn forever.
	ctx, cancel := context.WithTimeout(context.Background(), h.cfg.PromptTimeout)
	defer cancel()
	return split.Close(ctx, text)
}

// --- command.Scheduler ---------------------------------------------------

// CanSchedule satisfies command.Scheduler: scheduling exists only when
// the operator turned the loopback on, and this is what keeps
// `!schedules` and `!unschedule` off the command surface otherwise.
// The Handler implements the interface unconditionally, so without this
// the relay would advertise commands nothing could serve.
func (h *Handler) CanSchedule() bool { return h.cfg.Schedules != nil }

// Schedule satisfies command.Scheduler.
func (h *Handler) Schedule(token, text string, at time.Time, every time.Duration) (schedule.Item, error) {
	if h.cfg.Schedules == nil {
		return schedule.Item{}, errors.New("scheduling is not enabled on this relay")
	}
	if _, err := journal.ParseToken(token); err != nil {
		return schedule.Item{}, err
	}
	return h.cfg.Schedules.Add(token, text, at, every)
}

// Schedules satisfies command.Scheduler.
func (h *Handler) Schedules(token string) []schedule.Item {
	if h.cfg.Schedules == nil {
		return nil
	}
	return h.cfg.Schedules.List(token)
}

// Unschedule satisfies command.Scheduler.
func (h *Handler) Unschedule(token, id string) error {
	if h.cfg.Schedules == nil {
		return errors.New("scheduling is not enabled on this relay")
	}
	return h.cfg.Schedules.Remove(token, id)
}

// --- firing --------------------------------------------------------------

// FireSchedule is schedule.Config.Fire: it runs one scheduled prompt as
// an ordinary turn in the conversation it was armed in.
//
// It BLOCKS for the whole turn. That is not incidental — acp-kit's
// store derives a schedule's recursion depth from whatever is firing in
// the conversation right now, so returning early would make every
// schedule the turn arms look like a fresh depth-1 one and the depth cap
// would stop bounding anything.
//
// # No human in the loop
//
// A scheduled turn has nobody watching it, so every gate an interactive
// turn passes is re-applied HERE, at fire time rather than at arm time:
//
//   - the channel must still be served (a channel unsubscribed since the
//     schedule was armed stops firing);
//   - direct messages must still be enabled;
//   - the conversation must still exist in the journal.
//
// Any of those failing returns schedule.ErrGone, which disarms the item
// rather than retrying it forever. The one gate with no analogue is
// `allowed_user_ids`: a scheduled prompt has no sender. It needs none —
// it can only exist because an allowed user drove a turn that armed it,
// and it re-enters that same conversation, so it can never reach
// anywhere its author could not.
func (h *Handler) FireSchedule(ctx context.Context, it schedule.Item) error {
	key, err := journal.ParseToken(it.Conv)
	if err != nil {
		return fmt.Errorf("%w: %v", schedule.ErrGone, err)
	}
	if key.IsDM() {
		if !h.cfg.DMs {
			return fmt.Errorf("%w: direct messages are no longer served", schedule.ErrGone)
		}
	} else if _, ok := h.cfg.Channels.Name(key.StreamID); !ok {
		return fmt.Errorf("%w: channel %d is no longer served", schedule.ErrGone, key.StreamID)
	}
	conv, engaged := h.cfg.Journal.Lookup(key)
	if !engaged {
		return fmt.Errorf("%w: no conversation in %s", schedule.ErrGone, h.describe(key))
	}

	pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), h.cfg.PromptTimeout)
	defer cancel()
	// A human turn must never be superseded by a scheduled one, so wait
	// for the conversation to go idle instead of cancelling what is
	// running — and claim it in the same critical section, so a message
	// arriving in the gap cannot have its own turn silently displaced.
	// The store fires each item in its own goroutine, so waiting here
	// holds nothing else up.
	entry := &inflightEntry{cancel: cancel}
	if err := h.claimConvIdle(ctx, conv.ID, entry); err != nil {
		return err
	}
	// See handleMessage: endTurn runs after the turn has left the
	// inflight map, never before.
	defer h.endTurn(conv)
	defer h.clearInflight(conv.ID, entry)

	h.cfg.Logf("handler: firing schedule %s in %s (depth %d)", it.ID, h.describe(key), it.Depth)
	// Addressed, so the answer streams with a placeholder: there is no
	// triggering message to react to, and a scheduled turn that decided
	// to abstain would leave the user with no sign anything happened.
	// msgID 0 skips the :eyes: acknowledgement for the same reason.
	return h.run(pctx, conv, "["+scheduledSender+"] "+it.Text, true, 0)
}

// claimConvIdle blocks until no turn is in flight for convID and then
// installs e as the conversation's inflight turn, without releasing the
// lock in between. Returns ctx.Err() if it gives up first, in which case
// nothing is claimed and the caller must not clear anything.
//
// The wait reuses the inflight condition variable, so neither this nor
// any test polls a clock.
func (h *Handler) claimConvIdle(ctx context.Context, convID string, e *inflightEntry) error {
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
		case <-stop:
			return
		}
		h.inflightMu.Lock()
		h.inflightCond.Broadcast()
		h.inflightMu.Unlock()
	}()
	h.inflightMu.Lock()
	defer h.inflightMu.Unlock()
	for ctx.Err() == nil {
		if _, busy := h.inflight[convID]; !busy {
			h.inflight[convID] = e
			return nil
		}
		if h.cfg.OnWaitForConv != nil {
			h.cfg.OnWaitForConv(convID)
		}
		h.inflightCond.Wait()
	}
	return ctx.Err()
}

// endTurn applies whatever the agent deferred during the turn that has
// just finished — today, only `new_session`. Called on every completed
// turn, including a scheduled one: a loopback capability must not
// depend on a human having started the turn.
//
// It resolves the token from the conversation as it was at the START of
// the turn, so a topic renamed mid-turn drops the deferred action. That
// is the fail-safe direction — nothing is reset — and the user's next
// `!new` does what they wanted anyway; carrying a rename through would
// mean tracking identity across a turn for one vanishingly rare case.
func (h *Handler) endTurn(conv journal.Conv) {
	if h.cfg.Loopback == nil {
		return
	}
	h.cfg.Loopback.EndTurn(conv.Key.Token())
}

// Compile-time proof that the Handler satisfies the loopback
// capabilities. Without these, a signature drift in acp-kit would
// surface as the `post` and `schedule` tools silently vanishing from
// the agent's tool list at runtime instead of as a build failure.
var (
	_ command.Poster    = (*Handler)(nil)
	_ command.Scheduler = (*Handler)(nil)
)
