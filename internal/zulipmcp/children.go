package zulipmcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kfet/zulip-acp/internal/journal"
	"github.com/kfet/zulip-acp/internal/zulipproto"
)

// ToolListChildren is the tool name exposed to the agent.
const ToolListChildren = "list_children"

// MaxChildren bounds how many children one call resolves. Each costs
// Zulip round-trips, under a rate limit of 200 a minute.
const MaxChildren = 20

// child resolves name — a conv-id or a topic — to one of the caller's
// direct children, or refuses.
func (c caller) child(name string) (Child, error) {
	kids, ok := c.children()
	if !ok {
		return Child{}, errors.New("this conversation is no longer active")
	}
	base := journal.BaseTopic(name)
	var match []Child
	for _, k := range kids {
		if k.ConvID == name {
			return k, nil
		}
		if strings.EqualFold(journal.BaseTopic(k.Key.Topic), base) {
			match = append(match, k)
		}
	}
	switch len(match) {
	case 1:
		return match[0], nil
	case 0:
		return Child{}, fmt.Errorf("%q is not a topic branched directly out of this conversation; "+
			"call %s to see the ones that are", name, ToolListChildren)
	default:
		return Child{}, fmt.Errorf("%q names more than one child topic; give its conv_id instead (see %s)", name, ToolListChildren)
	}
}

// listChildrenTool is the `list_children` tool.
func (t *Tools) listChildrenTool() Tool {
	return Tool{
		Name: ToolListChildren,
		Description: "List the topics branched DIRECTLY out of this conversation — by the `branch` tool, " +
			"`!branch` or the fork reaction — with each one's topic, conv_id, #channel>topic link and last " +
			"activity. Read one in full with history(child=<conv_id>). A child's own children are not listed. " +
			fmt.Sprintf("At most the %d most recently active children are shown.", MaxChildren),
		Schema: map[string]any{"type": "object", "properties": map[string]any{}},
		Handler: t.wrap(func(c caller, _ json.RawMessage) (string, error) {
			kids, ok := c.children()
			if !ok {
				return "", errors.New("this conversation is no longer active")
			}
			return t.listChildren(kids), nil
		}),
	}
}

// listChildren renders the children, newest activity read live.
func (t *Tools) listChildren(kids []Child) string {
	if len(kids) == 0 {
		return "No topics have been branched out of this conversation."
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "%d topic(s) branched out of this conversation:\n", len(kids))
	for _, k := range kids {
		fmt.Fprintf(&sb, "- %s — topic %q, conv_id %s, last activity: %s\n", k.Link, k.Key.Topic, k.ConvID, t.lastActivity(k.Key))
	}
	sb.WriteString("Read one with history(child=<conv_id>).")
	return sb.String()
}

// lastActivity describes the newest message in a child topic.
func (t *Tools) lastActivity(k journal.Key) string {
	ctx, cancel := context.WithTimeout(context.Background(), t.cfg.Timeout)
	defer cancel()
	msgs, err := t.cfg.Client.Messages(ctx, zulipproto.TopicNarrow(k.StreamID, k.Topic), 1, 0)
	if err != nil {
		t.cfg.Logf("zulipmcp: list_children could not read %s: %v", k.Label(), err)
		return "unknown (" + err.Error() + ")"
	}
	if len(msgs) == 0 {
		return "none"
	}
	m := msgs[len(msgs)-1]
	who := m.SenderName
	if who == "" {
		who = m.SenderEmail
	}
	return fmt.Sprintf("%s by %s (#%d)", time.Unix(m.Timestamp, 0).UTC().Format(time.RFC3339), who, m.ID)
}
