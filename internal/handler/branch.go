// This file is `!branch`: spinning an idea out of the conversation it
// surfaced in, into a topic of its own, without losing the context it
// grew out of.
//
// # The gesture
//
//	!branch <text>
//	!branch #**other-channel** <text>
//
// `<text>` is the intended FIRST MESSAGE of the new topic. It is both
// the opening prompt and — through the existing autotopic naming path
// — the new topic's name, which is exactly why this is a command and
// not an emoji reaction: a reaction cannot carry the user's intent, and
// that intent is the whole value.
//
// # Context is PULLED, never pushed
//
// The relay writes NO summary of the origin. It records a parent
// pointer (channel, topic, and the bare id of the `!branch` message)
// in the journal, states that pointer in the new session's first turn,
// and stops. If the new agent needs the context it fetches it itself,
// with `history(origin: true)` — a lazy fetch cannot be wrong about
// what matters, and a summary composed before anyone knows what the
// branch is about can.
//
// That is also what makes branching work from a topic the relay was
// never engaged in — a human-only thread in an ambient channel — where
// there is no origin session to ask for a summary in the first place.
//
// # The permission model, in one sentence
//
// A session may read its declared parent conversation, clamped to
// messages at or before the branch point, and nothing else. ONE HOP:
// the parent's own parent is not reachable (see journal.Parent), or
// "read my ancestors" would quietly become "read everything".
//
// # Ordering
//
// Nothing is created until every refusal has been checked: the
// destination channel is resolved and confirmed served, the branch is
// confirmed not to land where it started, and the title is chosen
// against the topics already there. Only then is the seed message
// posted — which is what CREATES the topic on Zulip.
//
// After that point exactly one thing can still stop the branch: the
// journal refusing to allocate the conversation. That aborts the turn
// and says so, and it leaves a real Zulip topic behind holding one
// seed message. That is the honest trade — the alternative is deleting
// a message to tidy up after an error — and the user is told to send a
// message in the topic to pick it up. The pointer message, which comes
// after, is the only step that truly degrades: it is logged and
// nothing else changes.
package handler

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kfet/acp-kit/command"
	"github.com/kfet/zulip-acp/internal/autotopic"
	"github.com/kfet/zulip-acp/internal/journal"
	"github.com/kfet/zulip-acp/internal/zulipmcp"
	"github.com/kfet/zulip-acp/internal/zulipproto"
)

const (
	// branchVerb is the typed form. There is deliberately no alias:
	// `!br` would shadow nothing today but reads as an abbreviation of
	// half a dozen plausible words, and a command that creates a topic
	// should be spelled out.
	branchVerb = "branch"
	branchHelp = "- `" + command.DisplaySigil + branchVerb + " [#**channel**] <text>` — spin `<text>` out into a new topic that can read this one\n"

	// branchTimeout bounds the API calls one `!branch` costs: a topic
	// listing, the seed message, and the pointer message. It runs on
	// the event loop, so it must not be able to wedge intake.
	branchTimeout = 60 * time.Second

	// branchMaxSuffix bounds the collision walk. Zulip compares topic
	// names case-insensitively, so " (2)", " (3)", … are tried in turn;
	// if a channel really holds that many topics of one name, the user
	// is better served by an error than by " (33)".
	branchMaxSuffix = 20
)

// branchRequest is a parsed `!branch` command.
type branchRequest struct {
	// Channel is the `#**name**` the user named, or "" for "the
	// channel this was typed in".
	Channel string
	// Text is the first message of the new topic.
	Text string
}

// isBranchCommand reports whether text names `!branch`, returning
// everything after the verb.
//
// Unlike `!archive` this command TAKES an argument, so a bare
// `!branch` is still recognised here and refused below with an
// explanation — telling someone who typed the right command that it
// does not exist would be the worst of both answers.
func isBranchCommand(text string) (string, bool) {
	body, ok := command.StripSigil(strings.TrimSpace(text))
	if !ok {
		return "", false
	}
	verb, rest := body, ""
	if i := strings.IndexAny(body, " \t\n"); i >= 0 {
		verb, rest = body[:i], body[i+1:]
	}
	if !strings.EqualFold(verb, branchVerb) {
		return "", false
	}
	return rest, true
}

// parseBranch splits the argument into an optional channel and the
// text. It is pure, so the whole surface is table-testable.
//
// Only Zulip's own `#**name**` mention syntax counts as a channel. A
// bare `#name` deliberately does NOT: Zulip's markdown does not
// linkify it either, and "#42 is broken" opening a branch would be a
// message eaten to satisfy a guess.
func parseBranch(arg string) (branchRequest, error) {
	s := strings.TrimLeft(arg, " \t")
	var req branchRequest
	if rest, ok := strings.CutPrefix(s, "#**"); ok {
		end := strings.Index(rest, "**")
		if end < 0 {
			return branchRequest{}, fmt.Errorf("that channel mention is not closed — write it as `#**channel name**`")
		}
		name := strings.TrimSpace(rest[:end])
		if name == "" {
			return branchRequest{}, fmt.Errorf("that channel mention names no channel — write it as `#**channel name**`")
		}
		// A `#**channel>topic**` mention names a topic, not a channel,
		// and `!branch` creates its own topic: honouring the channel
		// half and silently dropping the topic would post somewhere
		// the user did not ask for.
		if i := strings.IndexByte(name, '>'); i >= 0 {
			return branchRequest{}, fmt.Errorf("`#**%s**` names a topic; `%s%s` creates its own topic, so name only the channel",
				name, command.DisplaySigil, branchVerb)
		}
		req.Channel = name
		s = rest[end+2:]
	}
	req.Text = strings.TrimSpace(s)
	if req.Text == "" {
		return branchRequest{}, fmt.Errorf("say what the new topic is about: `%s%s [#**channel**] <text>`. The text is the first message of the new conversation, and its first line names the topic",
			command.DisplaySigil, branchVerb)
	}
	return req, nil
}

// branchCommand is the typed entry point, called from dispatch with
// the message that carried the command.
func (h *Handler) branchCommand(ctx context.Context, m *zulipproto.Message, key journal.Key, arg string) {
	req, err := parseBranch(arg)
	if err != nil {
		h.reply(ctx, key, err.Error())
		return
	}
	dest, name, err := h.branchDestination(key, req.Channel)
	if err != nil {
		h.reply(ctx, key, err.Error())
		return
	}
	// Detached and bounded: the branch outlives the event that started
	// it (it posts twice and starts a turn), and a wedged Zulip request
	// must not hold the poll loop.
	bctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), branchTimeout)
	defer cancel()

	title, err := h.branchTitle(bctx, dest, req.Text)
	if err != nil {
		h.reply(ctx, key, err.Error())
		return
	}
	// A branch that lands where it started is not a branch. It is
	// reachable — `!branch planning`, typed in the topic "planning",
	// generates exactly that title — and it must be refused BEFORE the
	// seed message, or the relay would post an opening message into
	// the very conversation it was spinning out of.
	if !key.IsDM() && key.StreamID == dest && strings.EqualFold(key.Topic, title) {
		h.reply(ctx, key, fmt.Sprintf("That would branch this topic into itself: the new topic would be called %q, which is where you already are. Give it a different opening line.", title))
		return
	}

	// The seed message CREATES the topic, and it is the first thing
	// anyone sees there. It @-mentions the person who branched,
	// because after typing `!branch` they are still reading the origin
	// topic — on a phone the mention is the only thing that surfaces
	// the new one.
	newKey := journal.Channel(dest, title)
	seed, err := (&convPoster{client: h.cfg.Client, key: newKey}).Post(bctx, branchSeed(m, h.describe(key)))
	if err != nil {
		h.cfg.Logf("handler: branching %s into #%s: the opening message failed (%v) — nothing was created", h.describe(key), name, err)
		h.reply(ctx, key, fmt.Sprintf("I could not open a topic in #%s (%v). Nothing was created.", name, err))
		return
	}

	// The conversation and its origin are one atomic write: a branched
	// conversation must never exist without the pointer that says what
	// it may read.
	parent := journal.Parent{Key: key, MessageID: m.ID}
	conv, err := h.cfg.Journal.Branch(newKey, parent)
	if err != nil {
		// The topic exists and the seed message is in it, so the user
		// is not left with nothing — but without a conversation there
		// is nothing to run the turn in.
		h.cfg.Logf("handler: branching into %s: %v", h.describe(newKey), err)
		h.reply(ctx, key, fmt.Sprintf("I opened #**%s>%s** but could not start a conversation there (%v). Send a message in it to try again.", name, title, err))
		return
	}
	h.rememberOwn(conv.ID, seed)

	// The pointer line in the ORIGIN topic. Posted after the branch is
	// real, so it never points at a topic that was not created; a
	// failure is logged and swallowed, because by then the branch has
	// happened and saying so is the only thing left.
	if _, err := (&convPoster{client: h.cfg.Client, key: key}).Post(bctx, "branched → #**"+name+">"+title+"**"); err != nil {
		h.cfg.Logf("handler: branched into %s but could not say so in %s: %v", h.describe(newKey), h.describe(key), err)
	}

	h.cfg.Logf("handler: %s branched %s (message %d) into %s as %s", senderName(m), h.describe(key), m.ID, h.describe(newKey), conv.ID)

	// Addressed, so the answer streams with a placeholder. The ack
	// reaction goes on the `!branch` message, which is where the user
	// is looking; the rename anchor is the seed message, which is the
	// one in the topic being renamed. See startTurnAnchored.
	h.startTurnAnchored(ctx, conv, h.branchPrompt(m, parent, req.Text, title), true, m.ID, seed)
}

// branchDestination resolves the channel a branch lands in, or says
// why it cannot.
//
// A direct message has no channel of its own, so branching out of one
// requires the destination to be named. Inventing one — the first
// served channel, the busiest, the one the user last spoke in — would
// be the relay guessing where to publish the contents of a private
// conversation.
func (h *Handler) branchDestination(key journal.Key, named string) (int64, string, error) {
	if named == "" {
		if key.IsDM() {
			return 0, "", fmt.Errorf("a direct message is in no channel, so I do not know where to put the new topic. Name one: `%s%s #**channel** <text>`",
				command.DisplaySigil, branchVerb)
		}
		name, ok := h.cfg.Channels.Name(key.StreamID)
		if !ok {
			// Only reachable if the channel left the served set between
			// the message arriving and the command being dispatched.
			return 0, "", fmt.Errorf("I no longer serve this channel, so I cannot open a topic in it")
		}
		return key.StreamID, name, nil
	}
	id, ok := h.cfg.Channels.ID(named)
	if !ok {
		// One refusal for "no such channel" and "a channel I am not in",
		// because from here they are the same fact: the relay is not
		// subscribed, so it can neither post there nor answer there.
		return 0, "", fmt.Errorf("I do not serve a channel called #**%s** — I can only branch into a channel I am subscribed to", named)
	}
	name, _ := h.cfg.Channels.Name(id)
	return id, name, nil
}

// branchTitle picks the new topic's name: the existing autotopic
// heuristic over the first line, made unique against the topics
// already in the destination channel.
//
// A branch that silently appended into a live session's topic would
// drop two conversations into one agent session; a branch that
// appended into a human's topic would be worse still.
func (h *Handler) branchTitle(ctx context.Context, streamID int64, text string) (string, error) {
	base := autotopic.NameAt(text, h.now())
	existing, err := h.cfg.Client.Topics(ctx, streamID)
	if err != nil {
		// Refuse rather than post blind. Creating a topic is a write
		// that cannot be taken back, and "which topics are there" is
		// the one question that decides whether this write lands in
		// somebody else's conversation.
		return "", fmt.Errorf("I could not check which topics are already in that channel (%v), so I did not create one", err)
	}
	taken := make(map[string]struct{}, len(existing))
	for _, t := range existing {
		taken[strings.ToLower(t)] = struct{}{}
	}
	// The journal is unioned in, not consulted instead. Zulip's
	// listing is the authority on what a human would see, but a live
	// conversation whose messages have all been deleted has left that
	// listing while still holding an agent session — and branching
	// into it would hand two conversations to one session.
	free := func(name string) bool {
		if _, clash := taken[strings.ToLower(name)]; clash {
			return false
		}
		_, engaged := h.cfg.Journal.Lookup(journal.Channel(streamID, name))
		return !engaged
	}
	if free(base) {
		return base, nil
	}
	for n := 2; n <= branchMaxSuffix; n++ {
		if cand := suffixTopic(base, n); free(cand) {
			return cand, nil
		}
	}
	return "", fmt.Errorf("there are already %d topics called %q in that channel, so I did not create another. Give the branch a different opening line", branchMaxSuffix, base)
}

// suffixTopic appends " (n)" to a topic name, trimming the base so the
// result still fits Zulip's MAX_TOPIC_LENGTH — which the server
// enforces by SILENT truncation, so a name that is merely too long
// comes back mangled rather than refused.
func suffixTopic(base string, n int) string {
	suffix := " (" + strconv.Itoa(n) + ")"
	room := autotopic.MaxLen - utf8.RuneCountInString(suffix)
	r := []rune(base)
	if len(r) > room {
		r = r[:room]
	}
	return strings.TrimRight(string(r), " ") + suffix
}

// branchSeed is the message that creates the new topic.
//
// It is written for the human, not the agent: the agent gets the same
// facts in its prompt (see branchPrompt), and a bot message that
// merely repeats a machine-readable header teaches a reader nothing.
// The @-mention is load-bearing — see branchCommand.
func branchSeed(m *zulipproto.Message, origin string) string {
	return fmt.Sprintf("🌱 %s branched this out of %s. Working on it — the answer follows in this topic.",
		mentionOf(m), origin)
}

// mentionOf renders an @-mention of a message's sender.
//
// The `|id` form is not decoration: Zulip refuses to LINK a bare
// `@**Name**` when two users in the realm share that display name, and
// the mention is the only thing that surfaces the new topic on the
// branching user's phone. A message with no sender id — there is no
// such thing on the wire, but the zero value exists — degrades to the
// plain name rather than mentioning user 0.
func mentionOf(m *zulipproto.Message) string {
	if m == nil || m.SenderID == 0 {
		return senderName(m)
	}
	return fmt.Sprintf("@**%s|%d**", senderName(m), m.SenderID)
}

// branchPrompt is the new session's first turn: the back-link header,
// then the user's text verbatim.
//
// The header is a real Zulip message link — `#**channel>topic@id**` —
// so a human reading the agent's transcript can click it, and the
// agent is told in plain words that it may read that topic and how.
// The pointer the tool actually uses is the journal's, not this
// string; this is documentation, and the journal is truth.
func (h *Handler) branchPrompt(m *zulipproto.Message, parent journal.Parent, text, title string) string {
	var origin string
	if parent.Key.IsDM() {
		origin = "a direct message"
	} else {
		originChannel, ok := h.cfg.Channels.Name(parent.Key.StreamID)
		if !ok {
			originChannel = strconv.FormatInt(parent.Key.StreamID, 10)
		}
		origin = fmt.Sprintf("#**%s>%s@%d**", originChannel, parent.Key.Topic, parent.MessageID)
	}
	prompt := fmt.Sprintf("[%s] [branched from %s]\n%s", senderName(m), origin, text)
	if h.cfg.Loopback != nil {
		prompt += "\n\n[relay] This topic was branched out of another conversation, which you have NOT seen. " +
			"Call the relay `" + zulipmcp.ToolHistory + "` tool with origin=true to read it — that reads the origin " +
			"conversation up to the moment of the branch, and it is the only other conversation you can read. Do it " +
			"when the message above leans on context you do not have, not reflexively." +
			renameHint(title)
	}
	return prompt
}
