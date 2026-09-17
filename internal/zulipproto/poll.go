// This file is the relay's second — and last — coupling to Zulip's
// widget subsystem: the `poll` widget, and the vote submessages it
// produces.
//
// # Why a poll, when zform already exists
//
// zform (zform.go) renders buttons, and it renders them in the WEB app
// only. MEASURED on this deployment, Zulip 12.2: the iOS client draws
// nothing at all for a zform message. Reactions were the first answer
// to that — they render everywhere — but a digit chip row proved
// unreliable in practice too: with nine reactions on the message the
// server held every one of them, and the iOS client simply did not
// draw `:one:` and `:two:`, which is to say it did not draw the CURRENT
// model or the one below it. Cause unknown; not a count limit and not
// a seeding failure.
//
// A poll is the surface that survives all of it. MEASURED: it renders
// AND is votable on iOS. It also fixes the class of bug the chips had,
// rather than one instance of it — a poll option carries its own text
// label, so nothing depends on a glyph rendering or on a positional
// emoji↔option mapping that a reader has to be told about in a footer.
//
// # What a vote looks like on the wire
//
// A vote arrives as a `submessage` event with msg_type "widget" and a
// JSON content of:
//
//	{"type":"vote","key":"canned,0","vote":1}
//
// MEASURED, and measured the hard way. The key for an option the poll
// SHIPPED WITH is the literal string "canned," followed by that
// option's index; it is NOT "<sender_id>,<index>", which is the key
// shape used for an option a PARTICIPANT added later. An earlier note
// in this repo claimed the latter, because it was derived from a
// synthetic POST /api/v1/submessage — and that endpoint accepts an
// arbitrary key string, so the server stored and echoed the guess
// untouched.
//
// The lesson generalises past polls and is why this comment is long:
// THE SERVER ACCEPTING YOUR PROBE INPUT PROVES NOTHING ABOUT WHAT A
// REAL CLIENT SENDS. Anything learned about this subsystem has to be
// learned from a vote cast in a real web or phone client.
//
// `vote` is +1 for a vote and -1 for taking it back — MEASURED
// toggling 1, -1, 1, -1 — so a reader of the poll's state must treat
// the latest POSITIVE vote as the selection. An un-vote means "never
// mind", and must never be read as "no model".
//
// # A BOT CANNOT ATTACH A POLL AS widget_content
//
// MEASURED on Zulip 12.2, and the single most important fact in this
// file: POST /messages with
// `widget_content={"widget_type":"poll",...}` is REFUSED —
// 400 "Widgets: unknown widget type: poll". `widget_content` accepts
// `zform` and nothing else.
//
// A poll is created the way a human creates one: by sending a message
// whose CONTENT is the `/poll` slash command,
//
//	/poll <question>
//	<option>
//	<option>
//
// which Zulip's own markdown processor turns into the poll submessage
// (verified: the resulting message carries exactly the
// `{"widget_type": "poll", "extra_data": {...}}` submessage the server
// built for itself). So PollContent below returns a message BODY, not
// a widget payload, and the caller posts it with an ordinary
// SendMessage.
//
// This is also why the poll message has no separate human-readable
// body: its content IS the slash command. A client that renders
// widgets shows the poll; one that does not shows the literal
// `/poll …` text. The model list a non-widget reader needs therefore
// lives on the PANEL, not here — see internal/handler/opts.go.
//
// # Same edit prohibition as zform
//
// A poll message carries a submessage, so PATCH /messages/<id> is
// refused with 400 "Widgets cannot be edited." exactly as it is for a
// zform. A poll that must reflect new state is re-posted and the old
// one retired; see internal/handler/opts.go.
package zulipproto

import (
	"encoding/json"
	"strings"
)

// WidgetTypePoll is the widget_type Zulip stores a poll submessage
// under. The relay never SENDS it — see the file comment — but it is
// what identifies a poll on a message read back from the server.
const WidgetTypePoll = "poll"

// pollCommand is the slash command that makes Zulip build a poll.
const pollCommand = "/poll "

// WidgetMsgType is the msg_type every widget submessage carries. A
// submessage event with any other msg_type is not a widget interaction
// and is none of the relay's business.
const WidgetMsgType = "widget"

// voteType is the `type` field of a vote submessage. A poll also emits
// "new_option" and "question" submessages, which the relay ignores.
const voteType = "vote"

// cannedKeyPrefix prefixes the vote key of an option the poll was
// CREATED with. An option a participant added later is keyed by their
// user id instead, and the relay's polls are read by index, so only
// canned keys can be resolved.
const cannedKeyPrefix = "canned,"

// PollContent renders a poll as the message CONTENT to send — the
// `/poll` slash command Zulip expands into a poll widget. The caller
// posts it with SendMessage; there is no widget payload (see the file
// comment).
//
// It returns "" for an empty option list, for the same reason ZForm
// does: a poll nobody can vote in is not a degraded control, it is a
// broken one, and sending it would put a literal "/poll" line in the
// topic for no gain.
//
// Newlines are what separate the question from the options, so every
// embedded one is flattened to a space. A model label does not contain
// one today; a question or a label that grew one would otherwise
// silently split into extra poll options — a menu with entries nobody
// wrote.
func PollContent(question string, options []string) string {
	if len(options) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString(pollCommand)
	sb.WriteString(OneLine(question))
	for _, o := range options {
		sb.WriteString("\n")
		sb.WriteString(OneLine(o))
	}
	return sb.String()
}

// OneLine flattens every newline and carriage return to a space.
//
// Exported because the same flattening is needed on the way IN as well
// as on the way out: a poll question or option that grew a newline
// would silently split into extra options here, and a string echoed
// into a markdown list item would break the list. One rule, both
// directions.
func OneLine(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "\r", " "), "\n", " ")
}

// Vote is a decoded vote submessage.
type Vote struct {
	// Option is the index of the canned option voted on.
	Option int
	// Up reports whether this was a vote (+1) rather than a vote
	// being taken back (-1).
	Up bool
}

// voteMsg is the wire shape of a widget submessage.
type voteMsg struct {
	Type string `json:"type"`
	Key  string `json:"key"`
	Vote int    `json:"vote"`
}

// ParseVote decodes a submessage's content as a vote on a CANNED poll
// option, reporting false for anything else.
//
// Everything it rejects is rejected deliberately rather than
// defensively: a submessage of another type (a participant adding an
// option, an author editing the question), a vote on an option that
// was not in the poll as posted (a participant-added option, keyed by
// user id, which no index can resolve), and malformed JSON, which on
// a subsystem documented only in dev docs is a thing to drop quietly
// rather than to reason about.
func ParseVote(msgType, content string) (Vote, bool) {
	if msgType != WidgetMsgType {
		return Vote{}, false
	}
	var m voteMsg
	if err := json.Unmarshal([]byte(content), &m); err != nil {
		return Vote{}, false
	}
	if m.Type != voteType {
		return Vote{}, false
	}
	idx, ok := cannedIndex(m.Key)
	if !ok {
		return Vote{}, false
	}
	return Vote{Option: idx, Up: m.Vote > 0}, true
}

// cannedIndex parses "canned,<n>" into n.
//
// Hand-rolled rather than strconv on a Cut, because the only accepted
// shape is a run of ASCII digits: strconv.Atoi would also accept
// "canned,+1", "canned,-0" and leading whitespace, none of which a
// client sends and all of which would be an index derived from
// something other than what was on the wire.
func cannedIndex(key string) (int, bool) {
	digits, ok := strings.CutPrefix(key, cannedKeyPrefix)
	if !ok || digits == "" {
		return 0, false
	}
	n := 0
	for _, r := range digits {
		if r < '0' || r > '9' {
			return 0, false
		}
		n = n*10 + int(r-'0')
		if n > maxPollOptions {
			// A poll the relay posts has a handful of options, so an
			// index past the ceiling cannot name one of ours. Bailing
			// here also means no amount of digits can overflow n.
			return 0, false
		}
	}
	return n, true
}

// maxPollOptions is the largest option index cannedIndex will parse.
// It is not a protocol limit — it is a sanity bound, several times
// larger than any poll the relay posts, so that a hostile or
// nonsensical key cannot be turned into a huge integer.
const maxPollOptions = 1000
