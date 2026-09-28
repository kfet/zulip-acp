// This file is the third Zulip-only loopback tool: `branch`.
//
// # What it is
//
// The agent-callable twin of `!branch` and the :fork_and_knife:
// reaction. It spins one or more new topics out of THIS conversation,
// each with its own session that starts working on its seed at once.
// It does not re-implement branching: every task goes through the same
// Handler code path as the two human gestures (performBranch's shared
// half), so the three entry points cannot drift.
//
// # Why here and not in relaytool
//
// A branch creates a Zulip TOPIC, clamps its origin at a Zulip MESSAGE
// id and links it with `#**channel>topic**`. All three are Zulip facts.
// See BRANCH-REUSE.md for which half of this could move to acp-kit.
//
// # Identity
//
// Unchanged: the conversation branched out of is the caller's, resolved
// from the connection token. `from_msg` is only a POSITION in that
// conversation, and the relay refuses an id from any other one.
package zulipmcp

import (
	"encoding/json"
	"errors"
	"fmt"
)

// ToolBranch is the tool name exposed to the agent.
const ToolBranch = "branch"

// MaxBranchTasks caps one `branch` call. It is a sanity guard against a
// runaway loop, not a quota: there is no rate limit across calls.
const MaxBranchTasks = 10

// BranchTask is one branch the agent asks for.
type BranchTask struct {
	// Title is the new topic's name. Empty means "derive it from the
	// seed", as `!branch` does.
	Title string `json:"title,omitempty"`
	// Seed is the new session's first message. Empty means "the text
	// of FromMsg", as the :fork_and_knife: reaction does.
	Seed string `json:"seed,omitempty"`
	// FromMsg is the branch point: a message id in the CALLER's topic.
	// The child's history(origin=true) stops there. Zero means the
	// latest message not sent by the relay.
	FromMsg int64 `json:"from_msg,omitempty"`
}

// BranchResult is what one task produced. Error is set, and the rest is
// empty, when that task failed; the other tasks of the call still ran.
type BranchResult struct {
	Topic  string `json:"topic,omitempty"`
	Link   string `json:"link,omitempty"`
	ConvID string `json:"conv_id,omitempty"`
	Error  string `json:"error,omitempty"`
}

// branchTool is the tool as data.
func (t *Tools) branchTool() Tool {
	return Tool{
		Name: ToolBranch,
		Description: "Spin work out of THIS conversation into new Zulip topics, one per task, in the same " +
			"channel. Each new topic gets its own agent session, which starts working on its seed at once and " +
			"can read this conversation up to the branch point with history(origin=true). Use it to fan out " +
			"independent pieces of work. Per task: `seed` is the new session's first message (omit it to use " +
			"the text of `from_msg`); `title` is the topic name (omit it to derive one from the seed); " +
			"`from_msg` is the id of a message in THIS topic that is the branch point (omit it for the latest " +
			"message not sent by the relay). Get message ids from the `" + ToolHistory + "` tool: each message " +
			"there starts with [#<id> …]. A `from_msg` from another topic is refused. At most " +
			fmt.Sprint(MaxBranchTasks) + " tasks per call. The relay posts the links in this topic itself, so " +
			"do not repeat them. Returns, per task, the topic, its #**channel>topic** link and its conv_id, " +
			"or an error.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"tasks": map[string]any{
					"type":     "array",
					"minItems": 1,
					"maxItems": MaxBranchTasks,
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"title":    map[string]any{"type": "string", "description": "The new topic's name."},
							"seed":     map[string]any{"type": "string", "description": "The new session's first message."},
							"from_msg": map[string]any{"type": "integer", "description": "Branch point: a message id in this topic."},
						},
					},
				},
			},
			"required": []any{"tasks"},
		},
		Handler: t.wrap(func(c caller, args json.RawMessage) (string, error) {
			var a struct {
				Tasks []BranchTask `json:"tasks"`
			}
			if err := decode(args, &a); err != nil {
				return "", err
			}
			if len(a.Tasks) == 0 {
				return "", errors.New("tasks must hold at least one task")
			}
			if len(a.Tasks) > MaxBranchTasks {
				return "", fmt.Errorf("%d tasks is over the maximum of %d per call — split them over several calls", len(a.Tasks), MaxBranchTasks)
			}
			if c.key.IsDM() {
				return "", errors.New("a direct message is in no channel, so there is nowhere to open a topic; ask the user to type `!branch #**channel** <text>` instead")
			}
			for i := range a.Tasks {
				task := &a.Tasks[i]
				if task.FromMsg < 0 {
					return "", fmt.Errorf("task %d: from_msg must not be negative", i+1)
				}
				if task.Title != "" {
					title, err := checkTitle(task.Title)
					if err != nil {
						return "", fmt.Errorf("task %d: %v", i+1, err)
					}
					task.Title = title
				}
			}
			res, err := t.cfg.Branch(c.session, a.Tasks)
			if err != nil {
				return "", err
			}
			t.cfg.Logf("zulipmcp: branch made %d task(s) from %s", len(res), c.key.Label())
			out, err := json.Marshal(res)
			return string(out), err
		}),
	}
}
