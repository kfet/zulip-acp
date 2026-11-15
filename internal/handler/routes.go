// This file holds the ROUTES (config `routes`): a new topic in one
// channel is moved to another channel, where the relay answers it.
//
// A route from a channel to itself is autotopic, and main merges it
// there; Config.Routes holds only routes between two channels.
//
// A route applies to a message in its from channel in general chat,
// or in a topic that has no conversation yet. Such a message is
// addressed with no @-mention. It moves to the to channel with
// propagate_mode change_one, under its own topic, or under a topic
// named after it when it was posted in general chat. A topic name that
// is taken there gets the message id, as autotopic does. Any failure
// is logged, and the relay answers in place. A post in the topic of a
// view or notify entry is not moved.
package handler

import (
	"context"
	"strings"

	"github.com/kfet/zulip-acp/internal/autotopic"
	"github.com/kfet/zulip-acp/internal/journal"
	"github.com/kfet/zulip-acp/internal/zulipproto"
)

// Route is where a new topic in a channel goes. See Config.Routes.
type Route struct {
	// To is the destination channel.
	To int64
	// LinkBack posts a link to the moved topic in the source topic.
	LinkBack bool
}

// routeFor returns the route that applies to m in the conversation
// key, if one does.
func (h *Handler) routeFor(m *zulipproto.Message, key journal.Key) (Route, bool) {
	if m.IsDM() {
		return Route{}, false
	}
	r, ok := h.cfg.Routes[m.StreamID]
	if !ok || h.isViewTopic(m.StreamID, m.Topic) {
		return Route{}, false
	}
	// General chat is a lobby, as for autotopic: a conversation the
	// journal holds there must not stop the route.
	if autotopic.IsGeneralChat(m.Topic) {
		return r, true
	}
	_, engaged := h.cfg.Journal.Lookup(key)
	return r, !engaged
}

// routeMove moves m along r, and returns the key the conversation is
// allocated under: the original key when the move fails.
func (h *Handler) routeMove(ctx context.Context, m *zulipproto.Message, key journal.Key, text string, r Route) journal.Key {
	topic := m.Topic
	if autotopic.IsGeneralChat(topic) {
		topic = autotopic.NameAt(h.promptText(text), h.now())
	}
	if h.topicTaken(ctx, r.To, topic) {
		topic = autotopic.Disambiguate(topic, m.ID)
	}
	if err := h.cfg.Client.MoveMessageToChannel(ctx, m.ID, r.To, topic, propagateOne); err != nil {
		h.cfg.Logf("handler: route: moving message %d to channel %d failed (%v) — answering in place", m.ID, r.To, err)
		return key
	}
	h.cfg.Logf("handler: route: moved message %d to channel %d topic %q", m.ID, r.To, topic)
	if r.LinkBack {
		name, _ := h.cfg.Channels.Name(r.To)
		if _, err := h.cfg.Client.SendMessage(ctx, m.StreamID, m.Topic, "↪ moved to "+topicMention(name, topic)); err != nil {
			h.cfg.Logf("handler: route: posting the link back for message %d: %v", m.ID, err)
		}
	}
	return journal.Channel(r.To, topic)
}

// topicTaken reports whether topic has a conversation in channel id,
// or already exists there. A failed topic list counts as not taken.
func (h *Handler) topicTaken(ctx context.Context, id int64, topic string) bool {
	if _, ok := h.cfg.Journal.Lookup(journal.Channel(id, topic)); ok {
		return true
	}
	have, err := h.cfg.Client.Topics(ctx, id)
	if err != nil {
		h.cfg.Logf("handler: route: listing the topics of channel %d: %v", id, err)
		return false
	}
	for _, t := range have {
		if strings.EqualFold(t, topic) {
			return true
		}
	}
	return false
}

// isViewTopic reports whether a view or notify entry posts in topic of
// channel id. A human reply there is about the view, not a new task.
func (h *Handler) isViewTopic(id int64, topic string) bool {
	for _, v := range h.cfg.Views {
		if v.StreamID == id && strings.EqualFold(v.Topic, topic) {
			return true
		}
	}
	for _, n := range h.cfg.Notifies {
		if n.StreamID == id && strings.EqualFold(n.Topic, topic) {
			return true
		}
	}
	return false
}
