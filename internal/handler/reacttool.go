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
	// The agent may react to a message the relay posted a moment ago.
	// Wait for its echo first, so live clients see the message before
	// the reaction (see reactWhenVisible). This runs on the MCP call's
	// goroutine, never on the event loop.
	if m.SenderID == h.cfg.BotUserID && h.now().Unix()-m.Timestamp < reactFreshSeconds {
		h.awaitEcho(msgID)
	}
	// A fresh deadline: the echo wait must not eat into this call's.
	rctx, rcancel := context.WithTimeout(context.Background(), h.cfg.ZulipCallTimeout)
	defer rcancel()
	if err := h.cfg.Client.AddReaction(rctx, msgID, emoji); err != nil {
		return fmt.Errorf("could not add :%s: to message %d: %v", emoji, msgID, err)
	}
	return nil
}

// reactFreshSeconds is how recent an own message must be for the react
// tool to wait for its echo. An older one is visible already.
const reactFreshSeconds = 10

// sameConv reports whether two keys name one conversation. Zulip
// compares topic names case-insensitively, and the resolve prefix is
// not part of a conversation's identity, so neither is here.
func sameConv(a, b journal.Key) bool {
	if a.IsDM() || b.IsDM() {
		return a.IsDM() && b.IsDM() && a.Token() == b.Token()
	}
	return a.StreamID == b.StreamID && strings.EqualFold(journal.BaseTopic(a.Topic), journal.BaseTopic(b.Topic))
}
