// This file is link auto-hydration: when a human pastes a link to
// another Zulip message, the relay fetches that message and puts it in
// front of the agent, so "see this" means something.
//
// # Why the relay does it and not the agent
//
// It is the same idea as `!branch`, at a smaller scale: context is
// PULLED, on demand, by the thing that needs it. The difference is
// that here the human has already named exactly which message they
// mean, so there is nothing left to decide — an agent asking a tool
// for a message id it can already see would be a round-trip for a
// value nobody is uncertain about.
//
// # What counts as a link
//
// Two spellings, both of which Zulip's composer produces:
//
//   - `#**channel>topic@949**`, the message-link mention;
//   - any URL containing `/near/949`, which is what "Copy link to
//     message" puts on the clipboard.
//
// They are read out of the RAW markdown. The relay registers its event
// queue with apply_markdown=false, so the message content it holds is
// what the human typed, mention syntax intact — there is no rendered
// HTML to parse and no `topic_links` field to consult (that field
// carries linkifier matches from the TOPIC string, which is a
// different feature entirely).
//
// # Bounds, all of them mandatory
//
//   - At most maxLinks per message. A message pasting forty links must
//     not be able to spend forty API calls or forty thousand tokens.
//   - Each body truncated to maxLinkRunes.
//   - THE SAME CHANNEL ONLY, which is a stricter rule than the channel
//     allowlist and has to be. `GET /messages/{id}` runs with the
//     BOT's permissions, and the bot is subscribed to every channel it
//     serves — so "served" alone would let anyone in one served
//     channel paste a `/near/` link into another and have the relay
//     read out a channel they cannot see. That is privilege
//     escalation, not sharing. Restricting hydration to the channel
//     the linking message is itself in makes the check free and exact:
//     whoever posted there can read there. A DM is never hydrated at
//     all, in either direction.
//   - Deduped per conversation, so re-pasting the same link in a
//     follow-up does not re-inject the same block turn after turn.
package handler

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kfet/zulip-acp/internal/zulipproto"
)

const (
	// maxLinks bounds how many linked messages one incoming message
	// can pull in.
	maxLinks = 3
	// maxLinkRunes bounds ONE hydrated body, in code points. Smaller
	// than the `history` tool's per-message bound on purpose:
	// hydration is unasked-for, so it must cost less than something
	// the agent chose to fetch.
	maxLinkRunes = 1500
	// linkTimeout bounds the fetches one message costs. It runs on the
	// event loop, ahead of the turn, so it must not be able to wedge
	// intake.
	linkTimeout = 20 * time.Second
	// linkIndexSize bounds the per-conversation dedupe memory. It is a
	// hint: an evicted entry costs one re-injection, never
	// correctness.
	linkIndexSize = 512
)

// messageLinks extracts the message ids a raw markdown body links to,
// in order of appearance, deduped, and capped at maxLinks.
//
// Pure, so the whole grammar is table-testable. It is deliberately
// strict — an id must be a run of digits terminated by the syntax that
// opened it — because a false positive costs an API call and injects a
// message nobody pointed at.
func messageLinks(text string) []int64 {
	var out []int64
	seen := map[int64]struct{}{}
	add := func(id int64) bool {
		if _, dup := seen[id]; dup || id <= 0 {
			return true
		}
		seen[id] = struct{}{}
		out = append(out, id)
		return len(out) < maxLinks
	}
	// `#**channel>topic@949**`. The topic may itself contain '@' and
	// '>', so the id is the run of digits immediately before the
	// CLOSING '**' — anything else is part of the topic name.
	rest := text
	for {
		i := strings.Index(rest, "#**")
		if i < 0 {
			break
		}
		rest = rest[i+3:]
		end := strings.Index(rest, "**")
		if end < 0 {
			break
		}
		body := rest[:end]
		rest = rest[end+2:]
		at := strings.LastIndexByte(body, '@')
		if at < 0 || !strings.Contains(body[:at], ">") {
			continue
		}
		if id, ok := parseID(body[at+1:]); ok && !add(id) {
			return out
		}
	}
	// `…/near/949`. Terminated by anything that is not a digit, which
	// covers the trailing punctuation of a sentence and the closing
	// paren of a markdown link alike.
	rest = text
	for {
		i := strings.Index(rest, "/near/")
		if i < 0 {
			break
		}
		rest = rest[i+len("/near/"):]
		n := 0
		for n < len(rest) && rest[n] >= '0' && rest[n] <= '9' {
			n++
		}
		if id, ok := parseID(rest[:n]); ok && !add(id) {
			return out
		}
		rest = rest[n:]
	}
	return out
}

// parseID reads a bare positive message id.
func parseID(s string) (int64, bool) {
	if s == "" {
		return 0, false
	}
	id, err := strconv.ParseInt(s, 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

// hydrateLinks returns the relay-written blocks to append to a prompt
// for the messages m links to, or "" when there are none.
//
// Every failure is silent to the user and logged: hydration is a
// convenience, and a turn must never fail — or grow an apology — because
// a linked message could not be read.
func (h *Handler) hydrateLinks(ctx context.Context, convID string, m *zulipproto.Message) string {
	// A direct message has no channel to measure a link against, and
	// the reader-can-see-it check below is entirely a channel
	// comparison. Rather than invent a weaker rule for DMs, there is
	// no hydration in one.
	if m.IsDM() || m.StreamID == 0 {
		return ""
	}
	ids := messageLinks(m.Content)
	if len(ids) == 0 {
		return ""
	}
	name, served := h.cfg.Channels.Name(m.StreamID)
	if !served {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), linkTimeout)
	defer cancel()
	var blocks []string
	for _, id := range ids {
		if v, seen := h.linkMsgs.get(id); seen && v == convID {
			continue
		}
		linked, err := h.cfg.Client.GetMessage(ctx, id)
		if err != nil {
			h.cfg.Logf("handler: not hydrating linked message %d: %v", id, err)
			continue
		}
		// The one check that matters, and the reason it is this one:
		// see the file comment. Anything not in the channel the link
		// was posted in is refused, whoever posted it.
		if linked.IsDM() || linked.StreamID != m.StreamID {
			h.cfg.Logf("handler: not hydrating linked message %d: it is not in #%s, where it was linked", id, name)
			continue
		}
		// Recorded only once it has actually been injected, so a
		// transient fetch failure does not permanently suppress the
		// link.
		//
		// The index is keyed on the message id and holds the
		// conversation, so the same link pasted in two topics is
		// hydrated in both — which is right: they are different
		// agents, each seeing it for the first time. The cost is that
		// the second evicts the first's record, so a re-paste in the
		// first would hydrate again: a bounded, harmless duplicate,
		// and it buys a dedupe with no per-conversation map to grow
		// and garbage-collect.
		h.linkMsgs.put(id, convID)
		blocks = append(blocks, linkBlock(linked, name))
	}
	if len(blocks) == 0 {
		return ""
	}
	return "\n\n" + strings.Join(blocks, "\n\n")
}

// linkBlock renders one hydrated message.
func linkBlock(m zulipproto.Message, channel string) string {
	who := m.SenderName
	if who == "" {
		who = m.SenderEmail
	}
	when := time.Unix(m.Timestamp, 0).UTC().Format(time.RFC3339)
	body := m.Content
	if utf8.RuneCountInString(body) > maxLinkRunes {
		body = string([]rune(body)[:maxLinkRunes]) + "… [truncated]"
	}
	return fmt.Sprintf("[linked] #**%s>%s@%d** — %s, %s:\n%s", channel, m.Topic, m.ID, who, when, body)
}
