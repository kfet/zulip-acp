// This file shows in the topic list whether agent work is live.
//
// There are two ways to show it (TopicStatus), and both change only the
// topic NAME, because a topic move is the only per-topic state a bot
// can change without posting a message.
//
// # StatusEmoji (the default)
//
// A glyph in front of the name: StatusBusy while a turn runs,
// StatusQuestion while the agent waits for the user, and no glyph while
// the topic is idle. A pending schedule alone shows no glyph: a topic
// that a check will look at later is idle until the check runs.
//
// The agent says that it waits for the user with the `topic_status`
// loopback tool (SetTopicStatus). The state is per turn: a turn that
// does not call the tool is "done". A turn that posts no answer (the
// silent sentinel) keeps the state the topic had, because nothing the
// user sees has changed.
//
// A plain topic move with send_notification_to_old/new_thread=false
// posts no notice and keeps every user's follow/mute setting —
// measured on Zulip 12.2. That is why this mode is the default.
//
// # StatusResolve
//
// Zulip's own resolve mark (✔): resolved when no turn runs and nothing
// is scheduled, unresolved otherwise. Every client draws it, filters on
// it (is:resolved) and sorts it. But Zulip's Notification Bot posts
// "marked this topic as resolved" for every resolve, and no API flag
// suppresses it (measured, Zulip 12.2). Keep it for users who depend on
// is:resolved.
//
// # When the name moves
//
//   - markBusy, at the start of every turn, before anything is posted:
//     the topic shows "busy" (emoji) or is unresolved (resolve).
//   - settleStatus, wherever the topic may have gone idle (a turn ended,
//     a schedule fired, was armed or was removed): the topic shows its
//     idle state. A human who changed the name while work was live is
//     overridden here, at the next change.
//
// settleStatus does nothing while a turn runs. Moving the topic under a
// running turn splits the turn's own answer in two (see rename.go), and
// the turn's end settles the topic anyway. For the same reason the
// `topic_status` tool only records; endTurn applies it.
//
// # Why a lock per topic
//
// A turn can start in the gap between settleStatus deciding "idle" and
// its move landing. Both paths hold the topic's lock (statusLock)
// across decide-and-move and re-read the conversation from the journal
// inside it, so the turn's markBusy runs after the idle move and undoes
// it, never before it. The lock is per topic, not global, because it is
// held across Zulip requests: a slow move in one topic must not delay
// the start of a turn in another.
//
// # The server is truth
//
// markBusy reads the triggering message back from the server and takes
// its topic as the topic's current name. That covers a move whose
// event was missed, and a message sent just before the relay's own
// move, whose event still names the old topic.
//
// # Identity
//
// The journal index ignores every status prefix (journal.BaseTopic,
// journal.SetStatusPrefixes), so "foo", "✔ foo" and "⧗ foo" are one
// conversation and a status change never costs a session. The journal
// is updated inline after each move, as applyRename does, so a message
// in the moved topic cannot miss the conversation while the echoed
// update_message event is still on its way.
package handler

import (
	"context"
	"errors"
	"sync"
	"unicode/utf8"

	"github.com/kfet/zulip-acp/internal/journal"
	"github.com/kfet/zulip-acp/internal/zulipproto"
)

// StatusMode is how a topic shows whether agent work is live in it.
type StatusMode int

const (
	// StatusOff leaves topic names alone.
	StatusOff StatusMode = iota
	// StatusResolve uses Zulip's resolve mark (✔).
	StatusResolve
	// StatusEmoji puts a glyph in front of the topic name.
	StatusEmoji
)

// The states the `topic_status` tool accepts.
const (
	// StateDone: the turn needs nothing from the user. The default.
	StateDone = "done"
	// StateNeedsInput: the turn asked the user a question or offered a
	// choice, and the agent waits for the answer.
	StateNeedsInput = "needs_input"
)

// markBusy marks conv's topic busy at the start of a turn and returns
// the conversation as the journal now holds it. anchor is the message
// that triggered the turn, or 0 to use the relay's own last message.
//
// Every failure is logged and the turn goes on in the topic as it is: a
// status mark is cosmetic, and an answer is not.
func (h *Handler) markBusy(ctx context.Context, conv journal.Conv, anchor int64) journal.Conv {
	if !h.statusApplies(conv) {
		return conv
	}
	defer h.statusLock(conv.Key)()
	cur, ok := h.cfg.Journal.LookupID(conv.ID)
	if !ok || cur.Retired {
		return conv
	}
	if anchor != 0 {
		m, err := h.cfg.Client.GetMessage(ctx, anchor)
		switch {
		case err != nil || !h.inTopic(m, cur):
			anchor = 0
		case m.Topic != cur.Topic:
			cur = h.followTopic(cur, m.Topic)
		}
	}
	want := journal.BaseTopic(cur.Topic)
	if h.cfg.TopicStatus == StatusEmoji {
		want = journal.StatusPrefix(h.cfg.StatusBusy) + want
	}
	if want == cur.Topic {
		return cur
	}
	return h.moveStatus(ctx, cur, want, anchor)
}

// statusLock takes the status lock of the topic k names and returns its
// release. Keyed by token, which ignores the prefix and survives
// `new_session`.
func (h *Handler) statusLock(k journal.Key) func() {
	v, _ := h.statusMu.LoadOrStore(k.Token(), new(sync.Mutex))
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// inTopic reports whether message m is in conv's topic, under either
// name.
func (h *Handler) inTopic(m zulipproto.Message, conv journal.Conv) bool {
	return m.StreamID == conv.StreamID && journal.BaseTopic(m.Topic) == journal.BaseTopic(conv.Topic)
}

// followTopic makes the journal hold topic as conv's current name and
// returns the conversation as the journal then holds it.
func (h *Handler) followTopic(conv journal.Conv, topic string) journal.Conv {
	if _, _, err := h.cfg.Journal.Rename(conv.StreamID, conv.Topic, topic); err != nil {
		h.cfg.Logf("handler: topic %q is now %q but the journal did not follow: %v", conv.Topic, topic, err)
	}
	cur, _ := h.cfg.Journal.LookupID(conv.ID)
	return cur
}

// settleStatus sets the name of the conversation convID's topic to
// what its work says (idleTopic).
// It does nothing while a turn runs; the turn's end settles it.
// anchor is a message to try first (see moveStatus), or 0.
func (h *Handler) settleStatus(convID string, anchor int64) {
	c, ok := h.cfg.Journal.LookupID(convID)
	if !ok || !h.statusApplies(c) {
		return
	}
	defer h.statusLock(c.Key)()
	cur, ok := h.cfg.Journal.LookupID(convID)
	if !ok || cur.Retired || h.isInflight(convID) {
		return
	}
	want := h.idleTopic(cur)
	if want == cur.Topic {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), h.cfg.ZulipCallTimeout)
	defer cancel()
	if anchor != 0 {
		if m, err := h.cfg.Client.GetMessage(ctx, anchor); err != nil || !h.inTopic(m, cur) {
			anchor = 0
		}
	}
	h.moveStatus(ctx, cur, want, anchor)
}

// settleStatusKey is settleStatus for the conversation a broker token
// names NOW — after `new_session` that is the fresh one.
func (h *Handler) settleStatusKey(token string, anchor int64) {
	if h.cfg.TopicStatus == StatusOff {
		return
	}
	if _, conv, ok := h.convFor(token); ok {
		h.settleStatus(conv.ID, anchor)
	}
}

// idleTopic is the name conv's topic has while no turn runs in it.
//
// StatusResolve: resolved, unless a schedule is pending.
// StatusEmoji: the question glyph if the last answered turn waits for
// the user, else the bare name. A schedule does not count.
func (h *Handler) idleTopic(conv journal.Conv) string {
	base := journal.BaseTopic(conv.Topic)
	token := conv.Key.Token()
	if h.cfg.TopicStatus == StatusEmoji {
		if _, ok := h.waiting.Load(token); ok {
			return journal.StatusPrefix(h.cfg.StatusQuestion) + base
		}
		return base
	}
	if len(h.Schedules(token)) > 0 {
		return base
	}
	return journal.ResolvedPrefix + base
}

// statusApplies reports whether conv's topic can carry the mark. A DM
// has no topic, and the empty topic ("general chat") cannot be
// resolved or carry a prefix.
//
// A topic the widest prefix would push over MaxTopicLength gets no
// status either: Zulip refuses the move, and it would be refused on
// every turn.
func (h *Handler) statusApplies(conv journal.Conv) bool {
	if h.cfg.TopicStatus == StatusOff || conv.IsDM() {
		return false
	}
	base := journal.BaseTopic(conv.Topic)
	return base != "" && utf8.RuneCountInString(base)+h.widestPrefix() <= zulipproto.MaxTopicLength
}

// widestPrefix is the length, in code points, of the longest prefix
// the mode can put in front of a topic.
func (h *Handler) widestPrefix() int {
	if h.cfg.TopicStatus == StatusResolve {
		return utf8.RuneCountInString(journal.ResolvedPrefix)
	}
	return max(utf8.RuneCountInString(journal.StatusPrefix(h.cfg.StatusBusy)),
		utf8.RuneCountInString(journal.StatusPrefix(h.cfg.StatusQuestion)))
}

// moveStatus moves the whole topic of conv to topic and makes the
// journal follow. Caller holds the topic's statusLock. Returns the
// conversation as the journal holds it afterwards.
//
// anchor must be a message the caller has CHECKED is in the topic, or
// 0 for the relay's own last message. Never trust a candidate: `!branch`
// triggers a turn in a new topic from a message in the origin one, and
// moving that with change_all would move the ORIGIN topic.
func (h *Handler) moveStatus(ctx context.Context, conv journal.Conv, topic string, anchor int64) journal.Conv {
	if anchor == 0 {
		anchor = h.lastOwnMessage(ctx, conv)
	}
	if anchor == 0 {
		h.cfg.Logf("handler: no message to move topic %q by, so its status mark stays", conv.Topic)
		return conv
	}
	if err := h.cfg.Client.MoveMessage(ctx, anchor, topic, propagateAll); err != nil {
		h.cfg.Logf("handler: moving topic %q to %q for its status mark: %v", conv.Topic, topic, err)
		return conv
	}
	return h.followTopic(conv, topic)
}

// markQuiet records that the turn running in convID posts no answer,
// so endTurn keeps the topic's state as it was.
func (h *Handler) markQuiet(convID string) {
	h.renameMu.Lock()
	defer h.renameMu.Unlock()
	if e := h.inflightOf(convID); e != nil {
		e.quiet = true
	}
}

// noteTurnStatus records the state the turn that has just ended left
// its topic in: "waits for the user" when it called `topic_status` with
// StateNeedsInput, "done" otherwise. A quiet turn changes nothing.
// Called from endTurn, before settleStatus reads the state.
func (h *Handler) noteTurnStatus(conv journal.Conv, e *inflightEntry) {
	if h.cfg.TopicStatus != StatusEmoji || e == nil {
		return
	}
	h.renameMu.Lock()
	status, quiet := e.status, e.quiet
	h.renameMu.Unlock()
	if quiet {
		return
	}
	token := conv.Key.Token()
	if status == StateNeedsInput {
		h.waiting.Store(token, struct{}{})
		return
	}
	h.waiting.Delete(token)
}

// errStatusOff is the refusal when the relay shows no glyph status.
var errStatusOff = errors.New("topic status glyphs are off in this relay; nothing to set")

// SetTopicStatus is zulipmcp.Config.TopicStatus: it records state for
// the turn running in the conversation sessionKey names. Nothing moves
// until the turn ends (endTurn), for the reason rename.go gives.
func (h *Handler) SetTopicStatus(sessionKey, state string) (string, error) {
	if h.cfg.TopicStatus != StatusEmoji {
		return "", errStatusOff
	}
	if state != StateDone && state != StateNeedsInput {
		return "", errors.New("state must be " + StateNeedsInput + " or " + StateDone)
	}
	conv, ok := h.cfg.Journal.LookupID(sessionKey)
	if !ok || conv.Retired {
		return "", errors.New("this conversation is no longer active")
	}
	h.renameMu.Lock()
	defer h.renameMu.Unlock()
	e := h.inflightOf(conv.ID)
	if e == nil {
		return "", errors.New("no turn is running in this conversation")
	}
	e.status = state
	return "Topic status " + state + " recorded; the relay shows it when this turn ends.", nil
}
