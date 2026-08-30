// Package statusline is the Zulip-markdown renderer for the
// dev.acp-kit.status-line/v1 ACP extension. The wire contract
// (ExtensionID, Status, ParseMeta, ProviderEmoji, Segments) lives in
// github.com/kfet/acp-kit/statusline so poe-acp, slack-acp, zulip-acp
// and the fir agent stay byte-identical on the wire; this package owns
// only the Zulip markup.
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

// Header renders the status header prepended to the first
// user-visible chunk of a turn. Returns "" when every segment is
// empty, so the caller drops the prepend entirely rather than posting
// a bare blockquote.
func Header(s Status) string {
	parts := kit.Segments(s)
	if len(parts) == 0 {
		return ""
	}
	return "> *" + strings.Join(parts, " • ") + "*"
}

// Thinking renders the eager placeholder posted before the agent has
// produced anything. Zulip has no typing indicator, so this is the
// user's only acknowledgement that the relay received them.
func Thinking(s Status) string {
	return Spinner(s, "…")
}

// Spinner renders one live placeholder frame. dots is the animation
// phase appended to "Thinking"; empty defaults to "…" so the frame is
// always visible even with no mood or plan known yet.
func Spinner(s Status, dots string) string {
	if dots == "" {
		dots = "…"
	}
	parts := append(kit.Segments(s), "Thinking"+dots)
	return "> *" + strings.Join(parts, " • ") + "*"
}
