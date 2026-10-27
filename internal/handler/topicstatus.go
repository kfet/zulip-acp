// This file shows in the topic list whether agent work is live.
//
// A channel topic WITHOUT Zulip's resolved mark (✔) has work in it: a
// turn is running, or a schedule is pending. A topic WITH the mark is
// idle. The relay uses the native mark rather than a label of its own
// because every client already draws it, filters on it (is:resolved)
// and sorts it, and because a topic move is the only per-topic state a
// bot can change without posting a message.
//
// # When the mark moves
//
//   - markBusy, at the start of every turn, before anything is posted:
//     the topic is unresolved. A message sent to a resolved topic thus
//     unresolves it when its turn starts.
//   - settleStatus, wherever the topic may have gone idle (a turn ended,
//     a schedule fired, was armed or was removed): the topic is resolved
//     when no turn runs and nothing is scheduled, and unresolved
//     otherwise. A human who resolved the topic while work was live is
//     overridden here, at the next change.
//
// settleStatus does nothing while a turn runs. Moving the topic under a
// running turn splits the turn's own answer in two (see rename.go), and
// the turn's end settles the topic anyway.
//
// # Why a lock per topic
//
// A turn can start in the gap between settleStatus deciding "idle" and
// its move landing. Both paths hold the topic's lock (statusLock)
// across decide-and-move and re-read the conversation from the journal
// inside it, so the turn's markBusy runs after the resolve and undoes
// it, never before it. The lock is per topic, not global, because it is
// held across Zulip requests: a slow move in one topic must not delay
// the start of a turn in another.
//
// # The server is truth
//
// markBusy reads the triggering message back from the server and takes
// its topic as the topic's current name. That covers a resolve whose
// event was missed, and a message sent just before the relay's own
// move, whose event still names the old topic.
//
// # Identity
//
// The journal index ignores the prefix (journal.BaseTopic), so "foo" and
// "✔ foo" are one conversation and toggling never costs a session. The
// journal is updated inline after each move, as applyRename does, so a
// message in the moved topic cannot miss the conversation while the
// echoed update_message event is still on its way.
//
// # The notice
//
// Zulip's Notification Bot posts "marked this topic as resolved" for
// every resolve. send_notification_to_old_thread/new_thread=false (which
// MoveMessage always sends) does NOT suppress it — measured on Zulip
// 12.2. The relay never answers it: it comes from a system bot. Users
// can have such notices marked read automatically
// (resolved_topic_notice_auto_read_policy), and an operator who does
// not want them sets "topic_status": false.
package handler

import (
	"context"
	"sync"

	"github.com/kfet/zulip-acp/internal/journal"
	"github.com/kfet/zulip-acp/internal/zulipproto"
)

// markBusy unresolves conv's topic at the start of a turn and returns
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
	if !journal.IsResolved(cur.Topic) {
		return cur
	}
	return h.moveStatus(ctx, cur, journal.BaseTopic(cur.Topic), anchor)
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

// settleStatus sets the mark of the conversation convID to what its
// work says: resolved when idle, unresolved when a schedule is pending.
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
	base := journal.BaseTopic(cur.Topic)
	want := journal.ResolvedPrefix + base
	if len(h.Schedules(cur.Key.Token())) > 0 {
		want = base
	}
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
	if !h.cfg.TopicStatus {
		return
	}
	if _, conv, ok := h.convFor(token); ok {
		h.settleStatus(conv.ID, anchor)
	}
}

// statusApplies reports whether conv's topic can carry the mark. A DM
// has no topic, and the empty topic ("general chat") cannot be
// resolved.
func (h *Handler) statusApplies(conv journal.Conv) bool {
	return h.cfg.TopicStatus && !conv.IsDM() && journal.BaseTopic(conv.Topic) != ""
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
