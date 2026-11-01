// This file is the Zulip-only loopback tool `topic_status`.
//
// # What it is
//
// The relay puts a glyph in front of the topic name to show its status
// (see internal/handler/topicstatus.go). It can see on its own that a
// turn runs. It cannot see that the agent's answer asks the user
// something, so the agent says so with this tool. The state is per
// turn and defaults to done; the relay applies it when the turn ends,
// never mid-turn.
//
// # Identity
//
// Unchanged: the conversation comes from the connection token. The
// tool takes no target.
package zulipmcp

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ToolTopicStatus is the tool name exposed to the agent.
const ToolTopicStatus = "topic_status"

// The states the tool accepts. They match handler.StateDone and
// handler.StateNeedsInput; this package must not import the handler.
const (
	topicStateDone       = "done"
	topicStateNeedsInput = "needs_input"
)

// topicStatusTool is the tool as data.
func (t *Tools) topicStatusTool() Tool {
	return Tool{
		Name: ToolTopicStatus,
		Description: "Set the status this topic shows in the topic list when your turn ends. " +
			"Call it with state=" + topicStateNeedsInput + " before you end a turn that asks the user a question, " +
			"asks them to decide, or offers choice reactions (see `" + ToolReact + "`): the topic then shows that it waits " +
			"for them. Otherwise do not call it: every turn defaults to " + topicStateDone + ". " +
			"The relay applies the status when the turn ends, never during it.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"state": map[string]any{
					"type":        "string",
					"enum":        []any{topicStateNeedsInput, topicStateDone},
					"description": "needs_input when you wait for the user; done (the default) otherwise.",
				},
			},
		},
		Handler: t.wrap(func(c caller, args json.RawMessage) (string, error) {
			var a struct {
				State string `json:"state"`
			}
			if err := decode(args, &a); err != nil {
				return "", err
			}
			state := strings.TrimSpace(a.State)
			if state == "" {
				state = topicStateDone
			}
			if state != topicStateDone && state != topicStateNeedsInput {
				return "", fmt.Errorf("state %q: want %s or %s", a.State, topicStateNeedsInput, topicStateDone)
			}
			out, err := t.cfg.TopicStatus(c.session, state)
			if err != nil {
				return "", err
			}
			t.cfg.Logf("zulipmcp: topic_status %s in %s", state, c.key.Label())
			return out, nil
		}),
	}
}
