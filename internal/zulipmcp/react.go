// This file is the Zulip-only loopback tool `react`.
//
// # What it is
//
// It adds an emoji reaction, as the bot, to one message in THIS
// conversation. Its main use is a choice: the agent posts a question
// with `post`, which returns the message id, then reacts with one
// emoji per option. The user taps an existing reaction instead of
// typing, and that tap reaches the agent as a reaction line.
//
// # Identity
//
// Unchanged: the conversation comes from the connection token.
// `msg_id` is only a POSITION in that conversation, and the relay
// refuses an id from any other one, as it does for branch's from_msg.
package zulipmcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ToolReact is the tool name exposed to the agent.
const ToolReact = "react"

// MaxEmojiName bounds an emoji name. Zulip's own names are far
// shorter; this only stops a junk argument before it costs a request.
const MaxEmojiName = 60

// checkEmoji normalises an emoji name: it removes surrounding colons
// and refuses a name Zulip cannot hold.
func checkEmoji(s string) (string, error) {
	name := strings.Trim(strings.TrimSpace(s), ":")
	if name == "" {
		return "", errors.New("emoji must not be empty; give a Zulip emoji name such as one or thumbs_up")
	}
	if len(name) > MaxEmojiName || strings.ContainsAny(name, " \t\n:") {
		return "", fmt.Errorf("%q is not a Zulip emoji name; give one such as one or thumbs_up", s)
	}
	return name, nil
}

// reactTool is the tool as data.
func (t *Tools) reactTool() Tool {
	return Tool{
		Name:        ToolReact,
		Description: toolText(ToolReact),
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"msg_id": map[string]any{"type": "integer", "description": paramText(ToolReact, "msg_id")},
				"emoji":  map[string]any{"type": "string", "description": paramText(ToolReact, "emoji")},
			},
			"required": []any{"msg_id", "emoji"},
		},
		Handler: t.wrap(func(c caller, args json.RawMessage) (string, error) {
			var a struct {
				MsgID int64  `json:"msg_id"`
				Emoji string `json:"emoji"`
			}
			if err := decode(args, &a); err != nil {
				return "", err
			}
			if a.MsgID <= 0 {
				return "", errors.New("msg_id must be a positive message id")
			}
			emoji, err := checkEmoji(a.Emoji)
			if err != nil {
				return "", err
			}
			if err := t.cfg.React(c.session, a.MsgID, emoji); err != nil {
				return "", err
			}
			t.cfg.Logf("zulipmcp: react :%s: on %d in %s", emoji, a.MsgID, c.key.Label())
			return fmt.Sprintf("Added :%s: to message %d.", emoji, a.MsgID), nil
		}),
	}
}
