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
// — the new topic's name.
//
// # The second entry point: :fork_and_knife:
//
// A reaction cannot carry intent, which is why the typed form exists
// and why it takes an argument. But there is one case where the intent
// is already written down: the message being reacted to. Tapping
// :fork_and_knife: on a message means "spin THIS message out into its
// own topic" — the reacted-to message's body becomes the seed text,
// the topic title and the new session's first prompt, exactly as
// `<text>` does above.
//
// Everything after that is the same code (see performBranch): one
// branch, two ways to start it, exactly as `!archive` and the
// :wastebasket: reaction are one archive. The differences are only at
// the edges and each is forced:
//
//   - the branching user is the REACTOR, not the reacted-to message's
//     sender — a third party may well be the one spinning somebody
//     else's message out, and the seed's @-mention must reach whoever
//     is waiting for the answer;
//   - the destination is the origin's own channel, because a reaction
//     has nowhere to name one — which is also why a DM is not branched
//     by reaction at all (see BranchReaction);
//   - the branch point is the REACTED-TO message, not "now": the
//     pointer anchors and clamps there, so the branched session reads
//     the origin up to the message it was spun out of and no further.
//
// A second person tapping the same message must not open a second
// topic, so branched message ids are remembered and a repeat tap
// replies with a link to the topic that already exists.
//
// # Context is PULLED, never pushed
//
// The relay writes NO summary of the origin. It records a parent
// pointer (channel, topic, and the bare id of the message that
// triggered the branch — the `!branch` message, or the message the
// :fork_and_knife: landed on) in the journal, states that pointer in
// the new session's first turn, and stops. If the new agent needs the context it fetches it itself,
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
	branchHelp = "- `" + command.DisplaySigil + branchVerb + " [#**channel**] <text>` — spin `<text>` out into a new topic that can read this one (or react :" + branchEmoji + ": on a message to spin that message out)\n"

	// branchEmoji is the reaction form. A fork in the road, near
	// enough — Zulip has no :fork: — and, unlike :wastebasket:, it
	// needs no confirmation cycle: branching destroys nothing, and the
	// worst case is one extra topic holding one message.
	branchEmoji = "fork_and_knife"

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
	h.performBranch(ctx, branchPlan{
		Origin:  key,
		Dest:    dest,
		Channel: name,
		Actor:   m,
		// The branch point is the `!branch` message itself: the
		// pointer anchors there, and history(origin: true) is clamped
		// there.
		Parent:  journal.Parent{Key: key, MessageID: m.ID},
		Text:    req.Text,
		Ack:     m.ID,
		Gesture: command.DisplaySigil + branchVerb,
	})
}

// branchPlan is everything both entry points have settled before the
// shared half runs: where the branch is going, who asked for it, what
// it says, and which message the branch hangs off.
//
// It exists so that `!branch` and :fork_and_knife: cannot drift. The
// two entry points differ only in how these fields are FILLED — parsed
// from a command, or read off a reacted-to message — and everything
// that actually creates a topic happens once, in performBranch.
type branchPlan struct {
	// Origin is the conversation being branched out of: where the
	// refusals are said and where the pointer message goes.
	Origin journal.Key
	// Dest and Channel are the resolved destination channel, already
	// confirmed served by branchDestination.
	Dest    int64
	Channel string
	// Actor is the person branching — the `!branch` sender, or the
	// REACTOR for the reaction form. It supplies the seed's @-mention,
	// the prompt's name prefix and the log line; it is NOT necessarily
	// the sender of the message the branch hangs off.
	Actor *zulipproto.Message
	// Author is who WROTE Text, when that is not the Actor. Empty for
	// the typed form, where the person branching typed the text
	// themselves.
	//
	// It is load-bearing rather than decorative. The reaction form
	// ships somebody else's words into a fresh session under the
	// reactor's name, and an agent told "Ada said X" when X is
	// Grace's is being lied to about provenance — the more so because
	// a message the allowlist would never have delivered can be
	// branched in by one allowlisted tap.
	Author string
	// Parent is the pointer written into the journal: the origin key,
	// and the message id the branched session's history is clamped at.
	Parent journal.Parent
	// Text is the first message of the new topic, and what its name is
	// derived from.
	Text string
	// Ack is the message the in-flight acknowledgement reaction goes
	// on — whichever message the user is looking at.
	Ack int64
	// Gesture names the entry point in the log line, so an operator
	// reading the journal can tell a typed branch from a tapped one.
	Gesture string
}

// performBranch is the half both entry points share: choose the title,
// refuse a branch that would land where it started, create the topic,
// record the origin, point at it, and start the turn.
//
// It reports the conversation it created and whether the branch
// happened at all — the reaction path remembers the conv-id, so a
// second tap on the same message links the topic that exists instead of
// opening another one. The typed path wants neither and ignores both.
//
// Nothing is created until every refusal has been checked; see the file
// comment for why that ordering is the whole feature.
func (h *Handler) performBranch(ctx context.Context, p branchPlan) (journal.Conv, bool) {
	// Detached and bounded: the branch outlives the event that started
	// it (it posts twice and starts a turn), and a wedged Zulip request
	// must not hold the poll loop.
	bctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), branchTimeout)
	defer cancel()

	title, err := h.branchTitle(bctx, p.Dest, p.Text)
	if err != nil {
		h.reply(ctx, p.Origin, err.Error())
		return journal.Conv{}, false
	}
	// A branch that lands where it started is not a branch. It is
	// reachable — `!branch planning`, typed in the topic "planning",
	// generates exactly that title, and so does a :fork_and_knife: on
	// the message that named the topic — and it must be refused BEFORE
	// the seed message, or the relay would post an opening message
	// into the very conversation it was spinning out of.
	if !p.Origin.IsDM() && p.Origin.StreamID == p.Dest && strings.EqualFold(p.Origin.Topic, title) {
		h.reply(ctx, p.Origin, fmt.Sprintf("That would branch this topic into itself: the new topic would be called %q, which is where you already are. Give it a different opening line.", title))
		return journal.Conv{}, false
	}

	// The seed message CREATES the topic, and it is the first thing
	// anyone sees there. It @-mentions the person who branched,
	// because they are still reading the origin topic — on a phone the
	// mention is the only thing that surfaces the new one.
	newKey := journal.Channel(p.Dest, title)
	seed, err := (&convPoster{client: h.cfg.Client, key: newKey}).Post(bctx, branchSeed(p.Actor, h.describe(p.Origin)))
	if err != nil {
		h.cfg.Logf("handler: branching %s into #%s: the opening message failed (%v) — nothing was created", h.describe(p.Origin), p.Channel, err)
		h.reply(ctx, p.Origin, fmt.Sprintf("I could not open a topic in #%s (%v). Nothing was created.", p.Channel, err))
		return journal.Conv{}, false
	}

	link := topicMention(p.Channel, title)

	// The conversation and its origin are one atomic write: a branched
	// conversation must never exist without the pointer that says what
	// it may read.
	conv, err := h.cfg.Journal.Branch(newKey, p.Parent)
	if err != nil {
		// The topic exists and the seed message is in it, so the user
		// is not left with nothing — but without a conversation there
		// is nothing to run the turn in.
		h.cfg.Logf("handler: branching into %s: %v", h.describe(newKey), err)
		h.reply(ctx, p.Origin, fmt.Sprintf("I opened %s but could not start a conversation there (%v). Send a message in it to try again.", link, err))
		return journal.Conv{}, false
	}
	h.rememberOwn(conv.ID, seed)

	// The pointer line in the ORIGIN topic. Posted after the branch is
	// real, so it never points at a topic that was not created; a
	// failure is logged and swallowed, because by then the branch has
	// happened and saying so is the only thing left.
	//
	// It is also the ONLY write this makes to the origin conversation:
	// no turn is started there, no claim is taken, and whatever the
	// origin's agent is doing carries on undisturbed.
	if _, err := (&convPoster{client: h.cfg.Client, key: p.Origin}).Post(bctx, "branched → "+link); err != nil {
		h.cfg.Logf("handler: branched into %s but could not say so in %s: %v", h.describe(newKey), h.describe(p.Origin), err)
	}

	h.cfg.Logf("handler: %s branched %s (%s on message %d) into %s as %s",
		senderName(p.Actor), h.describe(p.Origin), p.Gesture, p.Parent.MessageID, h.describe(newKey), conv.ID)

	// Addressed, so the answer streams with a placeholder. The ack
	// reaction goes on the message the user is looking at — the
	// `!branch` message, or the one they tapped — while the rename
	// anchor is the seed message, which is the one in the topic being
	// renamed. See startTurnAnchored.
	h.startTurnAnchored(ctx, conv, h.branchPrompt(p, title), true, p.Ack, seed)
	return conv, true
}

// BranchReaction is the reaction entry point, wired alongside
// ArchiveReaction as a second consumer of Config.ReactionTrigger. It
// reports whether it consumed the reaction.
//
// Everything it relies on has already been checked by handleReaction:
// the reaction is not the relay's own, not another bot's, the user is
// on the allowlist, and conv is a conversation the relay is engaged in
// and still serves. What is left is this file's own policy.
//
// It never touches the origin conversation's turn. There is no
// cancelInflight, no claim, and no prompt delivered there: the only
// write to the origin is the pointer message performBranch posts, so
// tapping :fork_and_knife: while the origin agent is mid-answer costs
// that answer nothing.
func (h *Handler) BranchReaction(ctx context.Context, conv journal.Conv, ev zulipproto.Event, m *zulipproto.Message) bool {
	if ev.EmojiName != branchEmoji {
		return false
	}
	// Un-reacting is not an action. A branch has created a topic, a
	// conversation and a turn; retracting the emoji cannot undo any of
	// it, so "un-branch" is not a thing that could be honoured. It is
	// not consumed either — the removal stays ordinary signal and
	// reaches the agent, exactly as any other retraction does.
	if ev.Op != zulipproto.ReactionAdd {
		return false
	}
	// A direct message is in no channel, so a branch out of one needs
	// its destination NAMED — which is precisely what a reaction
	// cannot carry, and inventing one would be the relay choosing
	// where to publish the contents of a private conversation. The
	// typed form still works here and says so. The reaction is left
	// alone: it falls through as ordinary signal and reaches the
	// agent.
	if conv.Key.IsDM() {
		return false
	}
	// Only now is a name worth an API call — and it is also the last
	// bot check: BotSenderIDs is a startup snapshot, and a bot that
	// appeared since must not be able to create topics.
	who, isBot := h.reactor(ctx, ev.UserID)
	if isBot {
		return false
	}
	// Two people reading the same message will tap the same emoji on
	// it. The second tap must not open "… (2)" next door: it is
	// answered with a link to the topic the first tap created.
	if link, ok := h.alreadyBranched(conv.ID, ev.MessageID); ok {
		h.reply(ctx, conv.Key, "That message has already been branched → "+link)
		return true
	}
	// The reaction carries no intent text, so the REACTED-TO MESSAGE
	// supplies it: its body is the seed, the title and the new
	// session's first prompt. convForReaction only hands the message
	// over when it had to fetch it anyway — resolved from the relay's
	// own index it returns nil — so this is the one read the gesture
	// costs.
	if m == nil {
		got, err := h.cfg.Client.GetMessage(ctx, ev.MessageID)
		if err != nil {
			h.cfg.Logf("handler: not branching message %d in %s: reading it back failed (%v)", ev.MessageID, h.describe(conv.Key), err)
			h.reply(ctx, conv.Key, fmt.Sprintf("I could not read that message back (%v), so I did not branch it. Nothing was created.", err))
			return true
		}
		m = &got
	}
	text := strings.TrimSpace(m.Content)
	if text == "" {
		// An attachment-only message, or one whose body has since been
		// edited away. There is nothing to name a topic after and
		// nothing to prompt with.
		h.reply(ctx, conv.Key, fmt.Sprintf("There is no text in that message to branch. Say what the new topic is about instead: `%s%s <text>`.",
			command.DisplaySigil, branchVerb))
		return true
	}
	dest, name, err := h.branchDestination(conv.Key, "")
	if err != nil {
		h.reply(ctx, conv.Key, err.Error())
		return true
	}
	// The branching user is the REACTOR, not the reacted-to message's
	// sender: they are the one waiting for an answer, so they are who
	// the seed @-mentions and who the log line names. A synthetic
	// message is the honest way to say that, because everything
	// downstream asks a message who sent it.
	actor := &zulipproto.Message{SenderID: ev.UserID, SenderName: who}
	branched, ok := h.performBranch(ctx, branchPlan{
		Origin:  conv.Key,
		Dest:    dest,
		Channel: name,
		Actor:   actor,
		Author:  h.authorOf(m),
		// Anchored AND clamped at the reacted-to message: that message
		// IS the branch point, so history(origin: true) reads the
		// origin up to it and no further. Clamping at "now" would hand
		// the branched session whatever the origin has said since,
		// which is not what was spun out.
		Parent:  journal.Parent{Key: conv.Key, MessageID: ev.MessageID},
		Text:    text,
		Ack:     ev.MessageID,
		Gesture: ":" + branchEmoji + ":",
	})
	if !ok {
		// performBranch has already said why in the origin topic. The
		// reaction is still consumed: it was plainly aimed at the
		// relay, and passing it to the agent as ambient signal on top
		// of a refusal would be noise.
		return true
	}
	// Remembered only on success, so a branch that failed can be
	// retried by tapping again.
	h.rememberBranched(conv.ID, ev.MessageID, branched.ID)
	return true
}

// alreadyBranched reports the topic a previous :fork_and_knife: on this
// message opened, if there was one.
//
// The pair is (conversation, message) rather than the message alone:
// message ids are realm-unique, but a conversation can be retired and
// re-minted under the same key by `!new`, and the branch that belonged
// to the old one is not a reason to refuse the new one.
//
// Like every other cache in reaction.go this is a bounded HINT. Losing
// an entry to eviction costs at most one extra topic, never
// correctness.
func (h *Handler) alreadyBranched(convID string, msgID int64) (string, bool) {
	v, ok := h.branchedMsgs.get(msgID)
	if !ok {
		return "", false
	}
	owner, branchedID, _ := strings.Cut(v, "\x00")
	if owner != convID {
		return "", false
	}
	// The conv-id is stored rather than the rendered link, and the
	// location is resolved FROM it — the same reason the parent pointer
	// is a bare message id. The branched agent is told to rename its
	// topic as soon as it knows what the conversation is about, so a
	// link captured at branch time goes stale almost immediately.
	//
	// Three ways there is nothing left to point at, and all of them
	// mean the same thing: a fresh tap is a fresh branch, not a refusal
	// linking a topic the relay no longer answers in. Asking the
	// channel set about a zero key is harmless — it simply says no.
	c, live := h.cfg.Journal.LookupID(branchedID)
	name, served := h.cfg.Channels.Name(c.Key.StreamID)
	if !live || c.Retired || !served {
		return "", false
	}
	return topicMention(name, c.Key.Topic), true
}

// rememberBranched records that a message has been branched, and into
// which conversation.
func (h *Handler) rememberBranched(convID string, msgID int64, branchedID string) {
	h.branchedMsgs.put(msgID, convID+"\x00"+branchedID)
}

// authorOf names who WROTE a message, for branchPlan.Author.
//
// The relay's own messages are named explicitly rather than left to
// senderName: the agent should be told it is looking at its own earlier
// words, not at some third party's.
func (h *Handler) authorOf(m *zulipproto.Message) string {
	if m != nil && m.SenderID == h.cfg.BotUserID {
		return h.cfg.BotFullName
	}
	return senderName(m)
}

// topicMention is Zulip's `#**channel>topic**` syntax, in one place so
// the two callers cannot spell it differently.
func topicMention(channel, topic string) string {
	return "#**" + channel + ">" + topic + "**"
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
func (h *Handler) branchPrompt(p branchPlan, title string) string {
	m, parent, text := p.Actor, p.Parent, p.Text
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
	prompt := fmt.Sprintf("[%s] [branched from %s]%s\n%s", senderName(m), origin, h.branchAttribution(p), text)
	if h.cfg.Loopback != nil {
		prompt += "\n\n[relay] This topic was branched out of another conversation, which you have NOT seen. " +
			"Call the relay `" + zulipmcp.ToolHistory + "` tool with origin=true to read it — that reads the origin " +
			"conversation up to the moment of the branch, and it is the only other conversation you can read. Do it " +
			"when the message above leans on context you do not have, not reflexively." +
			renameHint(title)
	}
	return prompt
}

// branchAttribution is the clause that keeps the prompt honest about
// who wrote the text below it.
//
// The `[name]` prefix of every relay prompt means "the person whose
// turn this is", and for a branch that is the person who branched. In
// the reaction form the TEXT is somebody else's, and saying nothing
// would tell the agent that the reactor said words they never typed.
// That is not a nicety: `allowed_user_ids` gates who may put words in
// front of the model, and one allowlisted tap can spin in a message
// from somebody the allowlist would never have delivered.
//
// Empty for the typed form, and for a reactor branching their own
// message — there is nothing to correct, and a clause that fires on
// every branch would be noise the model learns to skip.
func (h *Handler) branchAttribution(p branchPlan) string {
	if p.Author == "" || p.Author == senderName(p.Actor) {
		return ""
	}
	if p.Author == h.cfg.BotFullName {
		return fmt.Sprintf(" [the text below is an earlier message of your own, which %s asked to spin out]", senderName(p.Actor))
	}
	return fmt.Sprintf(" [the text below was written by %s, not by %s]", p.Author, senderName(p.Actor))
}
