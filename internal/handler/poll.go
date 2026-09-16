// This file is the MODEL POLL: the half of `!opts` that a phone can
// actually use.
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
	"strings"

	"github.com/kfet/acp-kit/command"
	"github.com/kfet/zulip-acp/internal/journal"
	"github.com/kfet/zulip-acp/internal/zulipproto"
)

// pollQuestion is the poll's heading. It names the current model
// because a poll widget shows its question and its options and nothing
// else — the panel's state readout is a different message, and on web
// the panel's markdown is hidden entirely.
func pollQuestion(effective string) string {
	return "⚙️ Model — now: " + modelLabel(effective)
}

// postModelPoll posts the conversation's model poll and returns its
// message id, or 0 when there is none to post.
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
func (h *Handler) postModelPoll(ctx context.Context, post *convPoster, key journal.Key, engaged bool, effective string, choices []zulipproto.ZFormChoice) int64 {
	if !engaged || len(choices) == 0 {
		return 0
	}
	content := zulipproto.PollContent(pollQuestion(effective), pollOptions(choices))
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

// pollModelIDs is what the poll's options MEAN: the model id behind
// each option, by index. It is persisted with the poll's id — see
// journal.Conv.PollModels for why it must not be recomputed.
//
// Derived from the same []ZFormChoice pollOptions renders, so an
// option and its meaning cannot drift — they are two projections of
// one list. The id is recovered from the choice's reply rather than
// carried alongside it, because the reply is the thing actually
// dispatched: if they ever disagreed, the reply would win, so the
// reply is what is read.
func pollModelIDs(choices []zulipproto.ZFormChoice) []string {
	out := make([]string, 0, len(choices))
	for _, c := range choices {
		out = append(out, strings.TrimPrefix(c.Reply, command.DisplaySigil+"model "))
	}
	return out
}

// pollVote resolves a vote on THIS conversation's live model poll into
// the `!model <id>` command it stands for, and runs it down the
// ordinary `!` dispatch path.
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
//     with, which the journal holds (conv.PollModels).
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
	if vote.Option >= len(conv.PollModels) {
		// A vote naming an option the poll does not have. Reachable
		// when the journal write that should have recorded the table
		// failed, and by a hand-crafted submessage. Silence is right:
		// the alternative is switching to whatever happens to sit at
		// that index.
		h.cfg.Logf("handler: vote for option %d on model poll %d, which offers %d",
			vote.Option, conv.PollID, len(conv.PollModels))
		return
	}
	reply := modelReply(conv.PollModels[vote.Option])
	h.cfg.Logf("handler: model poll vote in %s — running %q", h.describe(conv.Key), reply)
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
		// Only reachable if the agent stopped reporting a model it
		// reported when the poll was posted: modelKnob accepts an
		// exact id and nothing else. Say so rather than silently
		// forwarding a bare "!model x" to the agent as prose.
		h.cfg.Logf("handler: model poll produced %q, which is no longer a command", reply)
	}
}
