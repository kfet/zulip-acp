// This file is the relay half of `rename_topic` (internal/zulipmcp).
//
// # Why the rename is deferred
//
// A turn posts into a topic while it runs: the placeholder, every
// streaming edit, each rollover message, the end-of-turn repost. The
// poster is built from the conversation's key as it was at the START of
// the turn (see convPoster), so a topic that moves underneath a running
// turn splits its own answer in half — the messages already posted
// travel with the move, and every message posted after it lands back in
// a topic named after a question nobody is asking any more.
//
// So the tool call does not rename anything. It ARMS a rename, and
// Handler.applyRename performs it from endTurn, after the turn has left
// the inflight map and every byte of the answer is posted. This is the
// same rule `new_session` follows, and for the same reason: a loopback
// tool must never destroy the turn that is calling it.
//
// # Why the arm belongs to the TURN, not the conversation
//
// A turn can be superseded: a follow-up cancels whatever is running and
// starts its own turn in the same conversation. The cancelled turn then
// unwinds asynchronously, so two turns are briefly alive at once. An arm
// keyed on the conversation would let the dying turn apply the LIVE
// turn's title — moving the topic out from under a turn that is still
// streaming into it, which is exactly what the deferral exists to
// prevent. So the arm hangs off the turn's own inflightEntry, and
// applyRename refuses to act once another turn holds the conversation.
//
// # Why the journal is renamed here rather than on the event
//
// Zulip echoes the move back as an update_message event, and
// handleUpdate migrates the journal when it arrives. That is correct and
// stays — but it is not immediate, and in the window between the PATCH
// and the event a message in the NEW topic would miss the conversation
// and allocate a second one. Renaming the journal inline closes the
// window; the echoed event then finds nothing left to move and says so.
package handler

import (
	"context"
	"errors"
	"time"

	"github.com/kfet/zulip-acp/internal/journal"
)

// propagateAll is Zulip's propagate_mode for "move this message and
// every other message in its topic" — what a topic RENAME is on the
// wire. It is the opposite of the autotopic move's propagateOne: there
// the other messages in general chat belong to other people, here they
// are the conversation itself.
const propagateAll = "change_all"

// renameTimeout bounds the two requests a rename costs. Deliberately
// short and NOT PromptTimeout: this runs in endTurn, after the turn has
// left the inflight map, where a wedged request would hold up the
// deferred loopback drain and — during a graceful reload — the exec
// itself. A rename is cosmetic; it is not worth ten minutes of anyone's
// time. A reload that cuts it short loses the rename and nothing else:
// the topic keeps its placeholder name and the journal still matches it.
const renameTimeout = 30 * time.Second

// The refusals the agent can see from RenameTopic. Each is stated as
// something it can act on rather than as an internal fault: a tool error
// is prose the model reads, and "unknown conversation" would invite a
// retry that cannot work.
var (
	errUnknownConv = errors.New("this conversation is no longer active")
	errNoAnchor    = errors.New("this turn has no message to anchor a topic move on, so the topic cannot be renamed from here")
	errTopicTaken  = errors.New("another conversation already lives in a topic of that name — choose a different one")
)

// pendingRename is a rename the agent asked for during a turn.
//
// anchor is the message the PATCH is addressed to. Zulip has no
// rename-topic endpoint: a rename is an edit of some message IN the
// topic with propagate_mode=change_all, so a rename needs a message to
// speak through, and the turn's triggering message is the one certain
// to be there.
type pendingRename struct {
	anchor int64
	title  string
}

// RenameTopic is zulipmcp.Config.Rename: it arms a topic rename on the
// turn currently running in the conversation that key names.
//
// It only records. Nothing is sent to Zulip until the turn ends — see
// applyRename — and the string returned is what the agent is told, so it
// says exactly that rather than implying the topic has moved.
func (h *Handler) RenameTopic(key journal.Key, title string) (string, error) {
	conv, ok := h.cfg.Journal.Lookup(key)
	if !ok {
		return "", errUnknownConv
	}
	// Renaming ONTO a live conversation is refused rather than
	// attempted. Zulip would merge the two topics, and the journal
	// resolves the collision by keeping the other conversation and
	// orphaning this one — the agent would lose the session it is
	// running in, to make a cosmetic change.
	// Renaming to the name it already has is not a collision with
	// itself: applyRename simply has nothing to do.
	if c, taken := h.cfg.Journal.Lookup(journal.Channel(key.StreamID, title)); taken && c.ID != conv.ID {
		return "", errTopicTaken
	}
	h.inflightMu.Lock()
	defer h.inflightMu.Unlock()
	e := h.inflight[conv.ID]
	if e == nil || e.rename == nil || e.rename.anchor == 0 {
		// Either no turn holds the conversation any more, or the one
		// that does is a scheduled prompt — which has no triggering
		// message to address the edit to. Renaming would need a message
		// id fetched from the topic, and inventing one for the rarest
		// caller is not worth the failure mode it adds.
		return "", errNoAnchor
	}
	e.rename.title = title
	return "Topic rename to " + title + " armed; the relay applies it when this turn ends.", nil
}

// applyRename performs the rename armed on the turn that has just
// finished, if any. Called from endTurn with that turn's own entry.
//
// Every failure is logged and swallowed. A rename is cosmetic: the realm
// may refuse the move outright (it is the same
// can_move_messages_between_topics_group policy the autotopic move
// depends on), and a turn that answered correctly must not be reported
// as failed because its topic kept the placeholder name.
func (h *Handler) applyRename(conv journal.Conv, e *inflightEntry) {
	if e == nil || e.rename == nil {
		return
	}
	h.inflightMu.Lock()
	title := e.rename.title
	anchor := e.rename.anchor
	// A turn is only allowed to move the topic it had to itself. By the
	// time endTurn runs this turn's entry is gone from the map (see
	// clearInflight); anything still there is a SUCCESSOR that is
	// already posting into the old name. Dropping the rename is the
	// fail-safe direction — the topic keeps a name that is merely
	// wrong, instead of moving under a live turn — and the successor
	// can ask for its own.
	superseded := h.inflight[conv.ID] != nil
	h.inflightMu.Unlock()
	if title == "" || title == conv.Key.Topic {
		return
	}
	if superseded {
		h.cfg.Logf("handler: dropping the rename of topic %q to %q: a newer turn holds %s", conv.Key.Topic, title, conv.ID)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), renameTimeout)
	defer cancel()
	// The anchor is only an anchor while it is still IN this topic. A
	// human moving the triggering message elsewhere mid-turn is rare,
	// and renaming whatever topic it landed in would be worse than not
	// renaming at all.
	if m, err := h.cfg.Client.GetMessage(ctx, anchor); err != nil || m.Topic != conv.Key.Topic {
		h.cfg.Logf("handler: not renaming topic %q to %q: message %d is no longer in it (%v)", conv.Key.Topic, title, anchor, err)
		return
	}
	if err := h.cfg.Client.MoveMessage(ctx, anchor, title, propagateAll); err != nil {
		h.cfg.Logf("handler: renaming topic %q to %q failed (%v) — the topic keeps its name", conv.Key.Topic, title, err)
		return
	}
	if _, moved, err := h.cfg.Journal.Rename(conv.Key.StreamID, conv.Key.Topic, title); err != nil {
		h.cfg.Logf("handler: topic renamed to %q but the journal did not follow: %v", title, err)
	} else if !moved {
		h.cfg.Logf("handler: topic renamed to %q but the journal had already moved on", title)
	} else {
		h.cfg.Logf("handler: agent renamed topic %q to %q, session %s follows it", conv.Key.Topic, title, conv.ID)
	}
}
