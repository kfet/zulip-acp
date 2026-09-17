// This file is the POLL PRIMITIVE, and its one caller today: the model
// menu, the half of `!opts` that a phone can actually use.
//
// # The primitive
//
// A poll is a table of (label, reply) pairs. postPoll renders the
// labels as poll options; a vote on option N dispatches reply N
// verbatim (journal.Conv.PollReplies). Nothing here knows about
// models: it knows about commands. Three rules bind anyone adding a
// second kind of poll, and all three are load-bearing:
//
//  1. EVERY REPLY MUST BE RELAY-AUTHORED. Never user text, never agent
//     text. A persisted reply is an arbitrary command replayed later by
//     a tap; the only reason that is safe is that nothing a prompt can
//     influence reaches it. Stated at length on
//     journal.Conv.PollReplies, where the field lives.
//
//  2. THE REPLY MUST REPAINT THE PAIR. This is the primitive's
//     CONTRACT, not the model poll's accident. A poll cannot be edited
//     (400 "Widgets cannot be edited."), so a tick cannot be cleared —
//     two allowlisted users voting differently leaves BOTH ticks
//     showing, and the poll then displays a state that is not the
//     relay's. The model poll self-heals because every model change
//     runs through SetModelOverride, which repaints: the old poll is
//     deleted and a fresh one posted with the new state in its
//     question. A poll whose reply does NOT repaint shows stale ticks
//     forever, and there is no way to fix it after the fact. So: do not
//     add a poll option whose command leaves the pair standing.
//
//     MEASURED caveat, and it bounds the repaint rather than breaking
//     it: message_content_delete_limit_seconds is 600 on this
//     deployment, so a repaint can only DELETE a pair younger than ten
//     minutes. An older poll is refused, cannot be edited either
//     (widgets never can), and stays in the scrollback. It is inert —
//     the vote path matches conv.PollID exactly — so a reader of an
//     idle topic sees two polls after voting, the lower one live.
//
//  3. AN UNHANDLED DISPATCH IS DROPPED, never forwarded to the agent as
//     prose. Generic replies make forwarding more tempting and more
//     wrong: the reply is relay-authored, so a dispatch that does not
//     recognise it is a relay bug, and the correct output of a relay bug
//     is a log line — not a turn spent asking the model what "!model
//     p/x" might have meant.
//
// # Participant-added options are silently ignored — and the question
// # line says so
//
// Zulip lets any viewer add an option to a poll, and there is no server
// setting to forbid it (checked: the poll widget takes no such
// parameter; the ability is part of the widget). An added option is
// keyed by the adder's USER ID, not by a canned index, so ParseVote
// rejects it and nothing the relay does can resolve it. Someone will
// add "gpt-99" and watch nothing happen.
//
// All three available answers are taken, cheapest first:
//
//   - the QUESTION LINE says "vote, don't add" (pollQuestion). A poll
//     shows its question on every client, it costs no round-trip, and
//     it lands before the mistake rather than after it. This is the
//     one that actually helps.
//   - the ADDITION is logged (handleSubmessage), so a confused user's
//     "I added it and nothing happened" is answerable from the log.
//   - nothing is posted in reply. A poll is a control surface, and
//     narrating a mis-tap into the topic would put relay chatter in
//     the transcript the model reads — the same reason a knob change
//     is a reaction and not a message.
//
// # Why a poll and not buttons or chips
//
// Three surfaces were built and measured on this deployment, Zulip
// 12.2:
//
//   - zform buttons (opts.go): render in the WEB app only. The iOS
//     client shows nothing at all.
//   - reaction chips: render everywhere, and broke anyway. With nine
//     reactions present on the panel — every one confirmed held by the
//     server via the API — the iOS client drew the row starting at
//     `three`. `:one:` and `:two:` never appeared, so the CURRENT
//     model (pinned first) and the one after it could not be tapped.
//     Cause unknown; not a count limit, not a seeding failure.
//   - a poll: renders AND is votable on iOS. Proven end to end.
//
// The poll also removes the whole class of bug the chips had rather
// than the one instance: an option carries its own TEXT, so nothing
// depends on a glyph drawing or on a positional emoji↔model mapping
// that a legend has to explain to the reader.
//
// # A vote is a command, and walks the command gates
//
// A vote arrives as a `submessage` event and is routed by
// Handler.pollVote into h.dispatch with the text `!model <id>` — the
// exact string a human could type and the exact string the loopback
// tool's action ends at. There is no second path to model switching,
// which is the same rule a chip and a zform button already obey: one
// parser, N surfaces.
//
// It therefore also walks the same gates, in the same order: the
// relay's own user id, the bot-sender set, the allowlist, and then the
// one call that can recognise a bot created since startup. A vote from
// a user the allowlist does not name changes nothing.
//
// # An un-vote is not a choice
//
// MEASURED: a poll vote toggles, sending vote +1 then -1 then +1. The
// selection is the latest POSITIVE vote; a -1 is dropped entirely.
// Reading it as anything else would let un-ticking an option mean "no
// model", which is not a state the relay has.
package handler

import (
	"context"

	"github.com/kfet/zulip-acp/internal/journal"
	"github.com/kfet/zulip-acp/internal/zulipproto"
)

// pollQuestion is the poll's heading. It names the current model
// because a poll widget shows its question and its options and nothing
// else — the panel's state readout is a different message, and on web
// the panel's markdown is hidden entirely.
//
// filter, when set, is named too. Under a filter the options are a
// SUBSET, so the current model may not be among them and "option 0 is
// the current one" stops holding — a reader has to be told which
// question they are answering, and `now:` still tells them the state
// either way. The filter is quoted, never interpolated into any
// option's reply: see journal.Conv.PollReplies.
//
// It also carries the "don't add options" note, which is the cheapest
// and earliest of the three answers to participant-added options (see
// the file comment). A question is shown on every client and costs no
// round-trip.
func pollQuestion(effective, filter string) string {
	q := "⚙️ Model"
	if filter != "" {
		q += " matching “" + filter + "”"
	}
	return q + " — now: " + modelLabel(effective) + " · vote, don't add options"
}

// postPoll posts a conversation's poll and returns its message id, or 0
// when there is none to post.
//
// Three reasons there may be none, all of them deliberate:
//
//   - no models. An agent with nothing to offer gets no menu; the
//     panel already says to connect a provider.
//   - the conversation does not exist yet. A vote is resolved against
//     conv.PollID, and a place with no journal entry has no conv at
//     all — `!opts` in a fresh topic must leave nothing on disk. A
//     poll nobody could act on is worse than no poll, because unlike a
//     zform button it looks live on every client. The panel's hint
//     says to start a conversation first.
//   - the post failed. Logged, and the panel stands on its own.
//
// There is no widget-less fallback and there cannot be one: the
// message's CONTENT is the `/poll` slash command, so a server that
// does not expand it leaves a literal "/poll …" line in the topic and
// nothing else. That is why the panel — not this message — carries the
// `!model <id>` list a non-widget reader needs.
func (h *Handler) postModelPoll(ctx context.Context, post *convPoster, key journal.Key, engaged bool, effective, filter string, choices []zulipproto.ZFormChoice) int64 {
	if !engaged || len(choices) == 0 {
		return 0
	}
	content := zulipproto.PollContent(pollQuestion(effective, filter), pollOptions(choices))
	id, err := post.Post(ctx, content)
	if err != nil {
		h.cfg.Logf("handler: posting model poll to %s: %v", h.describe(key), err)
		return 0
	}
	return id
}

// pollOptions is what the poll's options SAY. The short label, because
// a full provider-qualified id does not fit a phone's poll row and the
// panel carries the exact command anyway.
func pollOptions(choices []zulipproto.ZFormChoice) []string {
	out := make([]string, 0, len(choices))
	for _, c := range choices {
		out = append(out, c.ShortName)
	}
	return out
}

// pollReplies is what the poll's options MEAN: the `!command` behind
// each option, by index, taken VERBATIM from the choice's reply. It is
// persisted with the poll's id — see journal.Conv.PollReplies for why
// it must not be recomputed, and for the rule that only
// relay-authored replies may go in.
//
// Derived from the same []ZFormChoice pollOptions renders, so an
// option and its meaning cannot drift — they are two projections of
// one list. The reply is stored as-is rather than being decomposed and
// reassembled at vote time, because the reply IS the canonical form of
// a choice: it is the string a human could type, the string the zform
// button beside it carries, and the string dispatch parses. Anything
// stored narrower would make the poll's meaning depend on a rebuilder,
// and a second kind of poll would then need a second rebuilder.
func pollReplies(choices []zulipproto.ZFormChoice) []string {
	out := make([]string, 0, len(choices))
	for _, c := range choices {
		out = append(out, c.Reply)
	}
	return out
}

// pollVote resolves a vote on THIS conversation's live poll into the
// `!command` it stands for, and runs it down the ordinary `!` dispatch
// path.
//
// The caller has already established that the submessage landed on
// conv.PollID EXACTLY, and that the voter passed every gate a typed
// command walks (handleSubmessage). The exact-id match is what keeps a
// retired poll inert: deleting it usually takes it out of the topic,
// but a realm that forbids deletion leaves the whole message sitting
// there, still votable, and a vote in a month-old menu must not
// reconfigure a live conversation.
//
// What is left to decide here:
//
//   - the submessage must decode as a vote on a CANNED option
//     (zulipproto.ParseVote). A participant-added option is keyed by
//     user id and names no index of ours; a "new_option" or "question"
//     submessage is not a selection at all.
//   - vote must be positive. An un-vote is dropped: see the file
//     comment.
//   - the option index must be inside the table this poll was posted
//     with, which the journal holds (conv.PollReplies).
//
// Everything it rejects is dropped rather than passed on. A
// submessage is not conversational signal — it is a control surface
// the relay owns — and there is nothing downstream of here for one to
// fall through to.
func (h *Handler) pollVote(ctx context.Context, conv journal.Conv, ev zulipproto.Event) {
	vote, ok := zulipproto.ParseVote(ev.MsgType, ev.Content)
	if !ok || !vote.Up {
		return
	}
	if vote.Option >= len(conv.PollReplies) {
		// A vote naming an option the poll does not have. Reachable
		// when the journal write that should have recorded the table
		// failed, by a poll posted before the table's on-disk key
		// changed, and by a hand-crafted submessage. Silence is right:
		// the alternative is running whatever command happens to sit
		// at that index.
		h.cfg.Logf("handler: vote for option %d on poll %d, which offers %d",
			vote.Option, conv.PollID, len(conv.PollReplies))
		return
	}
	reply := conv.PollReplies[vote.Option]
	h.cfg.Logf("handler: poll vote in %s — running %q", h.describe(conv.Key), reply)
	// Through dispatch, never past it. A vote, a chip, a zform button
	// and a typed command must be one code path or they drift — the
	// whole justification for putting a menu on a poll at all is that
	// it adds no capability.
	//
	// The synthetic message carries the POLL's id, which is what a
	// model change reacts `check` onto — the same message the user
	// just voted in, so the acknowledgement lands where they are
	// looking. A model change also repaints, which retires this very
	// poll and posts a fresh one showing the new state.
	m := &zulipproto.Message{ID: ev.MessageID, SenderID: ev.SenderID}
	if _, handled := h.dispatch(ctx, m, conv.Key, reply); !handled {
		// Only reachable if the reply stopped being a command — for
		// the model poll, if the agent stopped reporting a model it
		// reported when the poll was posted, since modelKnob accepts
		// an exact id and nothing else. DROPPED, not forwarded: see
		// the file comment's rule about a generic reply.
		h.cfg.Logf("handler: poll vote produced %q, which is no longer a command", reply)
	}
}
