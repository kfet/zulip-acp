// Package statusline is the Zulip-markdown renderer for the
// dev.acp-kit.status-line/v1 ACP extension. The wire contract
// (ExtensionID, Status, ParseMeta, ProviderEmoji, ShortModelName,
// Segments) lives in github.com/kfet/acp-kit/statusline so poe-acp,
// slack-acp, zulip-acp and the fir agent stay byte-identical on the
// wire; this package owns only the Zulip markup.
//
// Two surfaces, one line:
//
//   - Spinner/Thinking — the live placeholder, a blockquoted italic
//     line ending in "Thinking…", edited in place while the turn runs.
//   - Footer — the same segments appended in italics under the
//     FINISHED answer, after a blank line.
//
// Footer, not header: mood and plan are agent-supplied and normally
// arrive mid-turn, so a line rendered on the first chunk showed a
// status the agent had not published yet — usually the emoji alone.
// Rendered at the end, it always carries the latest snapshot.
//
// Zulip's markdown is CommonMark-ish: `*text*` is italic, `**text**`
// is bold, and `> ` opens a blockquote. That is deliberately the same
// shape slack-acp renders in mrkdwn, so one agent reads the same on
// both surfaces.
package statusline

import (
	"strings"

	kit "github.com/kfet/acp-kit/statusline"
)

// ExtensionID is the _meta key both sides use to advertise support and
// to carry per-update mood/plan payloads.
const ExtensionID = kit.ExtensionID

// Status is the renderable state of one status header.
type Status = kit.Status

// ParseMeta extracts the v1 mood/plan fields from a session/update
// _meta map.
var ParseMeta = kit.ParseMeta

// ProviderEmojiForModel resolves the provider emoji from a fully
// qualified "<provider>/<model>" id.
var ProviderEmojiForModel = kit.ProviderEmojiForModel

// ShortModelName derives the compact display name shown next to the
// provider emoji from a fully qualified model id
// ("anthropic/claude-opus-4-5-20251001" → "opus-4.5"). Lossy by
// design: it is a label, never an id to send back on the wire.
var ShortModelName = kit.ShortModelName

// line joins the non-empty status segments with " • ". Returns "" when
// there is nothing to show. The provider emoji and the model name share
// the first segment ("🏛️ opus-4.5"): they name one thing.
func line(s Status) string {
	return strings.Join(kit.Segments(s), " • ")
}

// Footer renders the status line as it is appended to the END of a
// finished answer: a blank line, then the line in Zulip italics.
//
//	\n\n*🏛️ opus-4.5 • steady • 2/5*
//
// Returns "" when every segment is empty (unknown provider, no model,
// no agent _meta), so the caller appends nothing at all rather than a
// stray blank line or an empty pair of asterisks.
//
// The leading blank line is part of the footer, not the caller's job:
// it is what stops Zulip's markdown renderer folding the italic line
// into the answer's last paragraph.
func Footer(s Status) string {
	l := line(s)
	if l == "" {
		return ""
	}
	return "\n\n*" + l + "*"
}

// Thinking renders the eager placeholder posted before the agent has
// produced anything. Zulip has no typing indicator, so this is the
// user's only acknowledgement that the relay received them.
func Thinking(s Status) string {
	return Spinner(s, "…")
}

// Spinner renders one live placeholder frame. dots is the animation
// phase appended to "Thinking"; empty defaults to "…" so the frame is
// always visible even with no mood or plan known yet. The segments
// include the model identity ("🏛️ opus-4.5"), so the live line names
// the model servicing the turn just as the final footer does.
func Spinner(s Status, dots string) string {
	if dots == "" {
		dots = "…"
	}
	parts := append(kit.Segments(s), "Thinking"+dots)
	return "> *" + strings.Join(parts, " • ") + "*"
}
