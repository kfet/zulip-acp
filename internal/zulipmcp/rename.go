// This file is the second Zulip-only loopback tool: `rename_topic`.
//
// # Why it exists
//
// In an autotopic channel the relay MOVES a general-chat message into a
// topic of its own before answering it (internal/autotopic). The name
// it picks is a pure heuristic over the raw markdown — the opening
// line, stripped and truncated — because at that instant nothing has
// read the message except a `strings.Fields` loop. That is a serviceable
// placeholder and a poor title: it is the QUESTION, verbatim, typos and
// all, and it cannot be anything else.
//
// The agent is the only thing in the system that understands what the
// conversation turned out to be about, and it knows it only AFTER the
// turn. So the title is not guessed harder — it is asked for, here, and
// applied when the turn ends (see Handler.applyRename).
//
// # Why a tool and not a sentinel in the reply
//
// The obvious alternative is a marker line in the answer, like the
// ambient silence sentinel. It does not survive contact with streaming:
// the answer is edited into a live Zulip message chunk by chunk, so a
// trailing marker would be visible in the topic for as long as it took
// the next edit to land. A tool call runs agent→client out of band,
// costs no message text, and cannot leak into what the human reads.
//
// # Identity
//
// Unchanged and absolute, as for `history`: the conversation comes from
// the connection token, never from an argument. An agent can rename the
// topic it is talking in, and there is nowhere to name another one.
package zulipmcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/kfet/zulip-acp/internal/autotopic"
	"github.com/kfet/zulip-acp/internal/journal"
	"github.com/kfet/zulip-acp/internal/zulipproto"
)

// ToolRenameTopic is the tool name exposed to the agent.
const ToolRenameTopic = "rename_topic"

// checkTitle validates a proposed topic title and returns the exact
// string to rename to.
//
// Every rejection here is a message the AGENT reads and can act on, so
// each says what to send instead. The three refusals are the three ways
// a title can be unusable rather than merely bad:
//
//   - empty: there is nothing to rename to;
//   - over MAX_TOPIC_LENGTH: Zulip would truncate it SILENTLY, so the
//     agent must be told rather than shown a mangled topic;
//   - general chat: renaming a topic to Zulip's empty topic would dump
//     the conversation back into the lobby the autotopic move just
//     lifted it out of — the exact inverse of the feature.
func checkTitle(s string) (string, error) {
	title := strings.Join(strings.Fields(s), " ")
	if title == "" {
		return "", errors.New("title must not be empty")
	}
	if n := utf8.RuneCountInString(title); n > zulipproto.MaxTopicLength {
		return "", fmt.Errorf("title is %d characters, over Zulip's maximum of %d — send a shorter one",
			n, zulipproto.MaxTopicLength)
	}
	if autotopic.IsGeneralChat(title) {
		return "", errors.New("a topic cannot be renamed to general chat")
	}
	return title, nil
}

// renameTool is the tool as data. It is a method so the closure can
// reach the configured Rename hook.
func (t *Tools) renameTool() Tool {
	return Tool{
		Name: ToolRenameTopic,
		Description: "Rename the Zulip topic THIS conversation lives in. Use it when the topic's " +
			"name does not describe what the conversation is about — most importantly when the relay " +
			"opened the topic for you and auto-named it after the first line of the opening message, " +
			"which is a placeholder, not a title. Write the title a human scanning the channel would " +
			"want: short, specific, noun-heavy, no trailing punctuation, at most " +
			strconv.Itoa(zulipproto.MaxTopicLength) + " characters. The rename is applied when your turn ends, " +
			"and it moves the whole topic, so the conversation and your session follow it. It renames " +
			"the topic you are in; there is no way to name another one. Call it at most once per turn, " +
			"and do not mention having done it in your reply.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"title": map[string]any{
					"type":        "string",
					"description": "The new topic name.",
				},
			},
			"required": []any{"title"},
		},
		Handler: t.wrap(func(key journal.Key, args json.RawMessage) (string, error) {
			var a struct {
				Title string `json:"title"`
			}
			if err := decode(args, &a); err != nil {
				return "", err
			}
			title, err := checkTitle(a.Title)
			if err != nil {
				return "", err
			}
			if key.IsDM() {
				// A direct message has no topic; there is nothing here
				// to rename and no error the agent could recover from
				// by retrying differently.
				return "", errors.New("a direct message has no topic to rename")
			}
			out, err := t.cfg.Rename(key, title)
			if err != nil {
				return "", err
			}
			t.cfg.Logf("zulipmcp: rename_topic armed %q for %s", title, key.Label())
			return out, nil
		}),
	}
}
