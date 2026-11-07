// This file is the topic STATE: what the user must do with a topic.
//
// # Why it exists
//
// A user with a hundred topics cannot tell which are finished and
// which need a look. Zulip's resolve mark does not mean "done", and
// who wrote last does not help: the agent writes last almost always.
// So the relay keeps one persistent fact per topic (journal.TopicState)
// and renders it two ways: the ⁇ glyph on a needs_you topic
// (topicstatus.go), and the `!inbox` list.
//
// # Who records it
//
//   - The agent, with the `topic_state` loopback tool, ONLY when the
//     state changes. It is not asked to state it every turn.
//   - The user, with `!state`, which overrides the agent.
//   - `zulip-acp import-states`, once, for topics with no state yet.
//
// # Merge-back
//
// When a BRANCHED topic first delivers (moves into done or fyi from
// any other state), the relay posts one line into the parent topic
// with a link to the branch and its reason. It moves no message
// between topics. An import never posts one: it is a backfill, not
// news.
package handler

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kfet/acp-kit/command"
	"github.com/kfet/acp-kit/convo"
	"github.com/kfet/zulip-acp/internal/journal"
)

// Verbs of the two commands.
const (
	stateVerb = "state"
	inboxVerb = "inbox"
)

// staleTurns is how many turns since the state was recorded make the
// inbox flag it as possibly stale.
const staleTurns = 3

// inboxMaxRunes bounds the `!inbox` message, under Zulip's 10000
// code-point limit, with room for the "more" line.
const inboxMaxRunes = 9000

// stateHelp advertises `!state` and `!inbox` in `!help`.
const stateHelp = "- `" + command.DisplaySigil + stateVerb + " [needs_you|fyi|waiting|blocked|done] [reason]` — show or set what you must do with this topic\n" +
	"- `" + command.DisplaySigil + inboxVerb + "` — list the open topics of this channel, grouped by state\n"

// noteActivity records activity in convID; a failure is only logged,
// because the fact is cosmetic.
func (h *Handler) noteActivity(convID string, turn bool) {
	if err := h.cfg.Journal.NoteActivity(convID, turn); err != nil {
		h.cfg.Logf("handler: recording activity in %s: %v", convID, err)
	}
}

// SetTopicState is zulipmcp.Config.TopicState: the agent records the
// state of the conversation sessionKey names.
func (h *Handler) SetTopicState(sessionKey, state, reason string) (string, error) {
	conv, ok := h.cfg.Journal.LookupID(sessionKey)
	if !ok || conv.Retired {
		return "", errors.New("this conversation is no longer active")
	}
	changed, err := h.recordState(conv, journal.TopicState{State: state, Reason: reason, SetBy: journal.SetByAgent})
	if err != nil {
		return "", err
	}
	if !changed {
		return "The state is already " + state + " with that reason; nothing changed.", nil
	}
	return "State " + state + " recorded.", nil
}

// recordState writes s for conv, posts the merge-back line when a
// branch delivers, and settles the topic glyph. While a turn runs in
// conv, settleStatus does nothing and endTurn applies the glyph.
func (h *Handler) recordState(conv journal.Conv, s journal.TopicState) (bool, error) {
	prev, changed, err := h.cfg.Journal.SetState(conv.ID, s)
	if err != nil || !changed {
		return false, err
	}
	h.cfg.Logf("handler: state of %s: %q → %q by %s", conv.ID, prev.State, s.State, s.SetBy)
	if delivered(s.State) && !delivered(prev.State) {
		// The reason as the journal stored it: cut to one line, with
		// its mentions silenced.
		cur, _ := h.cfg.Journal.LookupID(conv.ID)
		h.mergeBack(conv, cur.State.Reason)
	}
	h.settleStatus(conv.ID, 0)
	return true, nil
}

// delivered reports whether a state means the topic has a result.
func delivered(state string) bool {
	return state == journal.StateDone || state == journal.StateFYI
}

// mergeBack posts one line into the parent topic of a branched conv.
// Any failure is logged: the branch's own state is already recorded.
func (h *Handler) mergeBack(conv journal.Conv, reason string) {
	parent, ok := h.resolveOrigin(conv)
	if !ok {
		return
	}
	name, ok := h.cfg.Channels.Name(conv.StreamID)
	if conv.IsDM() || !ok {
		return
	}
	// The base name: a done or fyi topic carries no glyph once the turn
	// that recorded it ends, so this is the name the link will find.
	line := "⤷ branch result: " + topicMention(name, journal.BaseTopic(conv.Topic))
	if reason != "" {
		line += " — " + reason
	}
	if err := h.PostTo(parent.Key.Token(), line); err != nil {
		h.cfg.Logf("handler: merge-back from %s: %v", conv.ID, err)
	}
}

// stateArgs reports whether text is `!state`, and returns what follows
// the verb.
func stateArgs(text string) (string, bool) {
	body, ok := command.StripSigil(strings.TrimSpace(text))
	if !ok {
		return "", false
	}
	body = strings.TrimSpace(body)
	verb, rest := body, ""
	if i := strings.IndexAny(body, " \t\n"); i >= 0 {
		verb, rest = body[:i], body[i:]
	}
	if !strings.EqualFold(verb, stateVerb) {
		return "", false
	}
	return strings.TrimSpace(rest), true
}

// isInboxCommand reports whether text is `!inbox`.
func isInboxCommand(text string) bool {
	body, ok := command.StripSigil(strings.TrimSpace(text))
	return ok && strings.EqualFold(strings.TrimSpace(body), inboxVerb)
}

// stateFilter runs `!state` and `!inbox` ahead of the broker, as
// `!sched` does. A pasted login redirect never carries a sigil, so
// neither can be mistaken for one.
func (h *Handler) stateFilter(ctx context.Context, in *convo.In) convo.Verdict {
	md := metaOf(in)
	if isInboxCommand(in.Text) {
		h.reply(ctx, md.key, h.inboxFor(md.key, md.m.SenderID))
		return convo.Handled
	}
	args, ok := stateArgs(in.Text)
	if !ok {
		return convo.Pass
	}
	h.reply(ctx, md.key, h.stateCommand(md.key, args))
	return convo.Handled
}

// stateUsage is the shape of `!state`.
var stateUsage = "`" + command.DisplaySigil + stateVerb + " <" + strings.Join(journal.States, "|") + "> [reason]`"

// stateCommand answers `!state args` in key's conversation.
func (h *Handler) stateCommand(key journal.Key, args string) string {
	conv, ok := h.cfg.Journal.Lookup(key)
	if !ok {
		return "This topic has no conversation yet, so it has no state."
	}
	if args == "" {
		if conv.State == nil {
			return "No state yet. Set one: " + stateUsage + "."
		}
		return "State: " + describeState(*conv.State, h.cfg.Now())
	}
	word, reason := args, ""
	if i := strings.IndexAny(args, " \t\n"); i >= 0 {
		word, reason = args[:i], args[i:]
	}
	state := strings.ReplaceAll(strings.ToLower(word), "-", "_")
	if !journal.ValidState(state) {
		return fmt.Sprintf("❌ `%s` is not a state. Use %s.", word, stateUsage)
	}
	s := journal.TopicState{State: state, Reason: strings.TrimSpace(reason), SetBy: journal.SetByUser}
	changed, err := h.recordState(conv, s)
	if err != nil {
		return "❌ " + err.Error() + "."
	}
	if !changed {
		return "State is already **" + state + "**."
	}
	cur, _ := h.cfg.Journal.LookupID(conv.ID)
	return "State: " + describeState(*cur.State, h.cfg.Now())
}

// describeState renders a state for `!state`.
func describeState(s journal.TopicState, now time.Time) string {
	out := "**" + s.State + "**"
	if s.Reason != "" {
		out += " — " + s.Reason
	}
	return out + fmt.Sprintf(" (by %s, %s ago)", s.SetBy, age(now, s.SetAt))
}

// age renders the time from ms (Unix milliseconds) to now, short.
func age(now time.Time, ms int64) string {
	if ms == 0 {
		return "?"
	}
	d := max(now.Sub(time.UnixMilli(ms)), 0)
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	}
	return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
}

// inboxGroups is the order of the inbox, and the heading of each
// group. "" is a topic with no state.
var inboxGroups = []struct{ state, title string }{
	{journal.StateNeedsYou, "❓ needs you"},
	{journal.StateBlocked, "⛔ blocked"},
	{journal.StateFYI, "📬 fyi"},
	{journal.StateWaiting, "⏳ waiting"},
	{"", "· open (no state)"},
}

// # Who sees which topics
//
// A listing is a read, and it follows the rule link.go states for
// hydration: the bot is subscribed to every channel it serves, so a
// list of all of them would show a user in one channel the topics and
// reasons of another channel, maybe a private one. So `!inbox` lists the
// channel it is sent in. An owner (Config.OpsOwners) sees every served
// channel, from anywhere. The `inbox` tool lists the caller's channel.

// errInboxDM is the answer to an `inbox` read from a DM.
var errInboxDM = errors.New("a direct message is in no channel; " +
	"send `" + command.DisplaySigil + inboxVerb + "` in a channel: it lists that channel's topics")

// inboxFor renders `!inbox` as sender, in key's conversation.
func (h *Handler) inboxFor(key journal.Key, sender int64) string {
	switch {
	case slices.Contains(h.cfg.OpsOwners, sender):
		return h.renderInbox(0)
	case key.IsDM():
		return "❌ " + errInboxDM.Error() + "."
	}
	return h.renderInbox(key.StreamID)
}

// Inbox is zulipmcp.Config.Inbox: the inbox of the caller's channel.
func (h *Handler) Inbox(sessionKey string) (string, error) {
	k, ok := h.ConvKey(sessionKey)
	if !ok {
		return "", errors.New("this conversation is no longer active")
	}
	if k.IsDM() {
		return "", errInboxDM
	}
	return h.renderInbox(k.StreamID), nil
}

// lastSeen is when conv last saw activity, or the best fact the
// journal has instead.
func lastSeen(c journal.Conv) int64 {
	if c.LastActivity != 0 {
		return c.LastActivity
	}
	if c.State != nil {
		return c.State.SetAt
	}
	return c.Created
}

// renderInbox lists every live topic of channel streamID (0: of every
// served channel) that is not done, grouped by state, newest activity
// first.
func (h *Handler) renderInbox(streamID int64) string {
	groups := map[string][]journal.Conv{}
	total := 0
	for _, c := range h.cfg.Journal.Convs() {
		if c.Retired || c.IsDM() {
			continue
		}
		if _, ok := h.cfg.Channels.Name(c.StreamID); !ok || (streamID != 0 && c.StreamID != streamID) {
			continue
		}
		state := ""
		if c.State != nil {
			state = c.State.State
		}
		if state == journal.StateDone {
			continue
		}
		groups[state] = append(groups[state], c)
		total++
	}
	if total == 0 {
		return "📭 Inbox empty: no open topics."
	}
	now := h.cfg.Now()
	var sb strings.Builder
	fmt.Fprintf(&sb, "📥 %d open topics", total)
	if name, ok := h.cfg.Channels.Name(streamID); ok {
		fmt.Fprintf(&sb, " in #**%s**", name)
	}
	sb.WriteString("\n")
	shown := 0
	for _, g := range inboxGroups {
		cs := groups[g.state]
		if len(cs) == 0 {
			continue
		}
		sort.SliceStable(cs, func(a, b int) bool { return lastSeen(cs[a]) > lastSeen(cs[b]) })
		head := fmt.Sprintf("\n**%s** (%d)\n", g.title, len(cs))
		for i, c := range cs {
			line := h.inboxLine(c, now)
			if i == 0 {
				line = head + line
			}
			if utf8.RuneCountInString(sb.String())+utf8.RuneCountInString(line) > inboxMaxRunes {
				fmt.Fprintf(&sb, "\n…and %d more.", total-shown)
				return sb.String()
			}
			sb.WriteString(line)
			shown++
		}
	}
	return sb.String()
}

// inboxLine renders one topic of the inbox.
func (h *Handler) inboxLine(c journal.Conv, now time.Time) string {
	name, _ := h.cfg.Channels.Name(c.StreamID)
	line := "- " + topicMention(name, c.Topic) + " · " + age(now, lastSeen(c))
	if c.State != nil && c.State.Reason != "" {
		line += " · " + c.State.Reason
	}
	if c.State != nil && c.ActivitySince >= staleTurns {
		line += " · ⚠ state may be stale"
	}
	return line + "\n"
}
