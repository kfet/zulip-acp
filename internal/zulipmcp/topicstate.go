// This file holds the Zulip-only loopback tools `topic_state` and
// `inbox`.
//
// # What they are
//
// `topic_state` records what the user must do with THIS topic. The
// state lasts across turns (journal.TopicState) and the agent records
// it only when it changes. It feeds the topic-name glyph and the
// user's `!inbox`. It replaces the per-turn `topic_status` tool: two
// tools for one fact would confuse the model.
//
// `inbox` is a read-only copy of `!inbox`, so an agent can see the
// same list the user sees.
//
// # Identity
//
// Unchanged: the conversation comes from the connection token. Neither
// tool takes a target. `inbox` lists topics, but it changes nothing,
// and it shows only the caller's own channel: a listing of another
// channel would leak it (see the scope note in the handler).
package zulipmcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/kfet/zulip-acp/internal/journal"
)

// Tool names exposed to the agent.
const (
	ToolTopicState = "topic_state"
	ToolInbox      = "inbox"
)

// topicStateTool is the tool as data.
func (t *Tools) topicStateTool() Tool {
	enum := make([]any, len(journal.States))
	for i, s := range journal.States {
		enum[i] = s
	}
	return Tool{
		Name:        ToolTopicState,
		Description: toolText(ToolTopicState),
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"state": map[string]any{"type": "string", "enum": enum},
				"reason": map[string]any{
					"type":        "string",
					"description": paramText(ToolTopicState, "reason"),
				},
			},
			"required": []any{"state", "reason"},
		},
		Handler: t.wrap(func(c caller, args json.RawMessage) (string, error) {
			var a struct {
				State  string `json:"state"`
				Reason string `json:"reason"`
			}
			if err := decode(args, &a); err != nil {
				return "", err
			}
			state := strings.TrimSpace(a.State)
			if !journal.ValidState(state) {
				return "", fmt.Errorf("state %q: want one of %s", a.State, strings.Join(journal.States, ", "))
			}
			reason := strings.TrimSpace(a.Reason)
			if reason == "" {
				return "", errors.New("reason must not be empty: give one line for the inbox")
			}
			out, err := t.cfg.TopicState(c.session, state, reason)
			if err != nil {
				return "", err
			}
			t.cfg.Logf("zulipmcp: topic_state %s in %s", state, c.key.Label())
			return out, nil
		}),
	}
}

// inboxTool is the tool as data.
func (t *Tools) inboxTool() Tool {
	return Tool{
		Name:        ToolInbox,
		Description: toolText(ToolInbox),
		Schema:      map[string]any{"type": "object", "properties": map[string]any{}},
		Handler: t.wrap(func(c caller, _ json.RawMessage) (string, error) {
			return t.cfg.Inbox(c.session)
		}),
	}
}
