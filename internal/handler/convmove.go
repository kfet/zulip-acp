package handler

import (
	"github.com/kfet/zulip-acp/internal/journal"
)

// moveConv is Journal.Move, and it moves with the conversation all the
// state that the relay keys by the conversation's TOKEN.
//
// # Why the token must follow
//
// The broker, the loopback tools and the schedule store know a
// conversation only by its key token (see journal.Key.Token), and the
// token of a channel conversation holds its topic. A rename changes
// the token. State left under the old token is lost: a schedule armed
// in the same turn as `rename_topic` fired into a conversation that no
// longer existed and was dropped as gone, and a `new_session` from that
// turn was never applied. Worse, a LATER topic with the old name would
// inherit what was left.
//
// So every journal move goes through here, and nothing calls
// Journal.Move or Journal.Rename directly.
//
// When the destination already has a conversation (Move keeps it and
// retires the moved one), the token state goes to the destination too.
// The topic is truth: the messages are there now, and so is the work
// that was armed for them.
func (h *Handler) moveConv(oldStreamID int64, oldTopic string, newStreamID int64, newTopic string) (journal.Conv, bool, error) {
	c, moved, err := h.cfg.Journal.Move(oldStreamID, oldTopic, newStreamID, newTopic)
	// An empty conv-id is "nothing was here to move"; an error has
	// been undone in the journal, so nothing moved either.
	if err != nil || c.ID == "" {
		return c, moved, err
	}
	h.rekey(journal.Channel(oldStreamID, oldTopic).Token(), journal.Channel(newStreamID, newTopic).Token())
	return c, moved, nil
}

// renameConv is moveConv within one channel.
func (h *Handler) renameConv(streamID int64, oldTopic, newTopic string) (journal.Conv, bool, error) {
	return h.moveConv(streamID, oldTopic, streamID, newTopic)
}

// rekey moves the token-keyed state from one token to another. A
// resolve or an unresolve does not change the token (it holds the base
// topic), so it is a no-op then.
//
// A failure to persist the schedules is logged, not returned: the
// journal move has already happened, and the topic is truth. The items
// stay under the old token and are dropped as gone when they fire —
// the state before this fix, and nothing worse.
func (h *Handler) rekey(from, to string) {
	if from == to {
		return
	}
	// The topic's status lock moves too. A markBusy that holds it under
	// the old token and a settleStatus that takes it under the new one
	// must still exclude each other. A destination that has its own
	// lock keeps it.
	if mu, ok := h.statusMu.LoadAndDelete(from); ok {
		h.statusMu.LoadOrStore(to, mu)
	}
	if h.cfg.Schedules != nil {
		n, err := h.cfg.Schedules.Rekey(from, to)
		if err != nil {
			h.cfg.Logf("handler: the topic moved, but its schedules did not follow: %v", err)
		} else if n > 0 {
			h.cfg.Logf("handler: %d schedule(s) follow the moved topic", n)
		}
	}
	if h.cfg.Loopback != nil {
		h.cfg.Loopback.Rekey(from, to)
	}
	if h.cfg.Commands != nil {
		h.cfg.Commands.Rekey(from, to)
	}
}
