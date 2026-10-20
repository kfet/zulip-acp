package handler

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/kfet/zulip-acp/internal/journal"
)

// ReactTo is zulipmcp.Config.React: it adds emoji, as the bot, to
// message msgID in the conversation sessionKey names.
//
// The message must belong to the CALLER's conversation, as a branch
// point must. Any other id gets one refusal, whether it is unreadable
// or elsewhere: two would let an agent probe which ids exist in
// other channels. The check compares// conversation keys the way Zulip does: a resolved or case-shifted
// topic name still matches.
//
// The relay drops reaction events the bot sends itself, so a reaction
// added here never comes back to the agent as a turn.
func (h *Handler) ReactTo(sessionKey string, msgID int64, emoji string) error {
	caller, ok := h.cfg.Journal.LookupID(sessionKey)
	if !ok || caller.Retired {
		return errors.New("this conversation is no longer active")
	}
	ctx, cancel := context.WithTimeout(context.Background(), h.cfg.ZulipCallTimeout)
	defer cancel()
	refused := fmt.Errorf("message %d is not in this conversation; msg_id must name a message here", msgID)
	m, err := h.cfg.Client.GetMessage(ctx, msgID)
	if err != nil {
		h.cfg.Logf("handler: react target %d unreadable: %v", msgID, err)
		return refused
	}
	key, ok := h.keyForMessage(m)
	if !ok {
		return refused
	}
	if !sameConv(key, caller.Key) {
		return refused
	}
	if err := h.cfg.Client.AddReaction(ctx, msgID, emoji); err != nil {
		return fmt.Errorf("could not add :%s: to message %d: %v", emoji, msgID, err)
	}
	return nil
}

// sameConv reports whether two keys name one conversation. Zulip
// compares topic names case-insensitively, and the resolve prefix is
// not part of a conversation's identity, so neither is here.
func sameConv(a, b journal.Key) bool {
	if a.IsDM() || b.IsDM() {
		return a.IsDM() && b.IsDM() && a.Token() == b.Token()
	}
	return a.StreamID == b.StreamID && strings.EqualFold(journal.BaseTopic(a.Topic), journal.BaseTopic(b.Topic))
}
