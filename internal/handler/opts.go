// This file is `!opts`: the relay's interactive options panel.
//
// # Why it lives here and not in acp-kit/command
//
// Every other `!command` is shared with poe-acp and slack-acp, because
// every relay needs the same controls. `!opts` is the exception on
// purpose: it is a rendering of controls that already exist, onto a
// surface only Zulip has — the `zform` widget, a message-attached
// button form. Nothing behind a button is new. Each one carries a
// `reply` string that Zulip's web client sends as an ORDINARY message
// from the clicking user, so a click walks the same gates, the same
// allowlist and the same `!` parser a typed command does. There is no
// second code path to keep in step, and every action still goes
// through the broker's exported actions.
//
// # The phone is the default reader
//
// zform renders as buttons in the Zulip WEB app and nowhere else — the
// phone app shows the message's plain markdown. So the markdown body
// is the product and the widget is decoration on top of it: the body
// must be complete, current and usable with a thumb, and is written
// first. If the server rejects the widget outright, the panel still
// posts (see postPanel).
//
// # One panel per conversation, and why it is re-posted rather than edited
//
// A widget message CANNOT BE EDITED. Measured on Zulip 12.2: a PATCH
// on a message carrying widget_content comes back 400 "Widgets cannot
// be edited." (see internal/zulipproto/zform.go). So a self-updating
// panel cannot be a PATCHed message — that design is not available at
// any price, and the obvious-looking implementation would fail on the
// first knob change on every server where the widget WORKED.
//
// What is done instead: the panel is re-posted and the old one
// retired. Net effect on the topic is the same — exactly one live
// panel, and no growing pile of stale controls — and it is strictly
// better on a phone, because the panel lands where the reader is
// rather than somewhere above the fold. Retiring means DELETE, the one
// place this relay deletes a message it posted, falling back to
// rewriting the body to a pointer line when the realm forbids deletion
// (a plain, widget-less panel can still be edited) and to leaving it
// alone when neither works. The live panel's id is persisted per
// conversation in the journal, next to the streaming tail.
package handler

import (
	"context"
	"fmt"
	"strings"

	"github.com/kfet/acp-kit/client"
	"github.com/kfet/acp-kit/command"
	"github.com/kfet/zulip-acp/internal/journal"
	"github.com/kfet/zulip-acp/internal/zulipproto"
)

// optsVerb is the command word this file owns.
const optsVerb = "opts"

// optsAckEmoji is the reaction that acknowledges a knob change.
//
// A reaction rather than a reply, because changing a setting is not
// something the topic should have to remember: it is retractable, it
// costs no scrollback, and the panel itself already shows the new
// state. `check` is in Zulip's built-in set; a realm that somehow
// lacks it fails the reaction call, which is logged and swallowed like
// every other decoration.
const optsAckEmoji = "check"

// optsModelCap bounds how many model buttons the panel offers.
//
// An agent can advertise a hundred models and a panel is not a
// catalogue — on a phone it has to fit on one screen. The current
// model is always among them (see modelChoices), and `!model <filter>`
// remains the way to reach the rest.
const optsModelCap = 6

// supersededPanel is what an old panel is rewritten to when it cannot
// be deleted. Only reachable for a panel posted WITHOUT its widget:
// a widget message refuses every edit as well.
const supersededPanel = "*⚙️ (options moved to a newer message)*"

// optsHelpLine advertises `!opts` in `!help`, whose text otherwise
// comes from the shared broker and cannot know about a Zulip-only
// command. Appended by dispatch rather than rendered in acp-kit, which
// is the correct side of the line: poe-acp has no widgets to offer.
const optsHelpLine = "- `" + command.DisplaySigil + optsVerb + "` — options panel (buttons in the Zulip web app)\n"

// isOpts reports whether text is the bare `!opts` command.
//
// Strict on purpose: an argument means the user meant something else,
// and forwarding "!opts why is this slow" to the agent as prose is a
// better failure than eating it. `.opts` and `/opts` are accepted for
// the same reason the broker accepts them — the sigil set is shared —
// but only `!` is ever advertised.
func isOpts(text string) bool {
	body, ok := command.StripSigil(strings.TrimSpace(text))
	if !ok {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(body), optsVerb)
}

// modelKnob reports whether text is `!model <id>` naming a model the
// agent actually has, i.e. a knob CHANGE rather than a listing.
//
// Only an exact id counts. `!model opus` is a filter query and belongs
// to the broker's listing path, which answers with prose; treating it
// as a change would silently switch models off an approximate match.
func (h *Handler) modelKnob(text string) (string, bool) {
	body, ok := command.StripSigil(strings.TrimSpace(text))
	if !ok {
		return "", false
	}
	verb, arg, found := strings.Cut(strings.TrimSpace(body), " ")
	if !found || !strings.EqualFold(verb, "model") {
		return "", false
	}
	arg = strings.TrimSpace(arg)
	models, _ := h.cfg.Agent.Models()
	for _, m := range models {
		if m.ID == arg {
			return arg, true
		}
	}
	return "", false
}

// applyModelKnob performs a knob change end to end: apply, then
// acknowledge with a reaction. It returns false when the change could
// not be applied, leaving the caller to fall through to the broker so
// the user gets a spoken reason rather than silence — the broker runs
// the same action again and renders its error, which is the one place
// that prose lives.
//
// The panel is NOT repainted here. That happens in SetModelOverride,
// the single point every model change passes through, so a change the
// AGENT makes through its loopback tool updates the panel too.
func (h *Handler) applyModelKnob(ctx context.Context, key journal.Key, msgID int64, modelID string) bool {
	// Through the broker's exported action, never past it: `!model`
	// typed, a button clicked and the loopback tool must all be the
	// same call, or they drift.
	if err := h.cfg.Commands.SelectModel(key.Token(), modelID); err != nil {
		return false
	}
	h.reactOnce(ctx, msgID, optsAckEmoji)
	return true
}

// reactOnce places a one-off reaction and leaves it there. Unlike the
// in-flight ack it is never retracted: it is the durable record that a
// setting was applied, and it is the only thing the topic keeps.
func (h *Handler) reactOnce(ctx context.Context, msgID int64, emoji string) {
	if err := h.cfg.Client.AddReaction(ctx, msgID, emoji); err != nil {
		h.cfg.Logf("handler: adding :%s: to message %d: %v", emoji, msgID, err)
	}
}

// refreshPanel re-posts the panel a conversation ALREADY has, so it
// shows the state it has just moved to.
//
// It does nothing when there is no panel: a state change is not a
// reason to start posting one at somebody who never asked for it.
func (h *Handler) refreshPanel(ctx context.Context, key journal.Key) {
	if conv, ok := h.cfg.Journal.Lookup(key); !ok || conv.OptsID == 0 {
		return
	}
	h.showPanel(ctx, key, "")
}

// showPanel posts a fresh panel at the bottom of the conversation and
// retires whatever panel it replaces. note is optional prose shown
// above it — it is how an unknown command explains itself.
//
// Serialised across the whole relay by optsMu, because post → retire →
// remember is a read-modify-write over one message id and there are two
// callers on different goroutines: a human's command on the event loop,
// and the agent's own `select_model` tool mid-turn. Interleaved, both
// would delete the same old panel and only one of the two NEW panels
// would be remembered — leaving a live panel nothing will ever retire.
// The section is three HTTP calls long and uncontended in practice.
func (h *Handler) showPanel(ctx context.Context, key journal.Key, note string) {
	h.optsMu.Lock()
	defer h.optsMu.Unlock()

	body, widget := h.renderPanel(key, note)
	post := &convPoster{client: h.cfg.Client, key: key}
	id, err := h.postPanel(ctx, post, body, widget)
	if err != nil {
		h.cfg.Logf("handler: posting options panel to %s: %v", h.describe(key), err)
		return
	}
	h.retirePrevious(ctx, key)
	h.rememberPanel(key, id)
}

// postPanel posts the panel, degrading gracefully if the widget is
// refused.
//
// A server with widgets disabled, or older than the parameter, ignores
// widget_content and posts anyway — the failure this guards against is
// the loud one, a server that REFUSES the message because of it. The
// retry is therefore only for a refusal (a 4xx): a transport failure or
// a cancelled context would fail identically without the widget, and
// retrying would just post a worse panel twice as slowly.
//
// widget is always non-empty here: renderPanel's session buttons exist
// whatever the agent reports, so there is no such thing as a panel with
// nothing to tap.
func (h *Handler) postPanel(ctx context.Context, post *convPoster, body, widget string) (int64, error) {
	id, err := post.PostWidget(ctx, body, widget)
	if err == nil || !zulipproto.RejectedByServer(err) {
		return id, err
	}
	h.cfg.Logf("handler: widget refused (%v) — posting the options panel as plain markdown", err)
	return post.Post(ctx, body)
}

// retirePrevious removes the panel this conversation had before the one
// just posted, so exactly one is ever live.
//
// DELETE first, because a panel carrying a widget cannot be edited at
// all. Deleting one's own message is a realm policy and is time-limited
// (message_content_delete_limit_seconds), so a refusal is expected
// rather than exceptional: fall back to rewriting the body to a pointer
// line, which works for a panel posted without its widget. If both are
// refused the old panel simply stays — stale, but harmless, since every
// button on it is still a valid command.
func (h *Handler) retirePrevious(ctx context.Context, key journal.Key) {
	conv, ok := h.cfg.Journal.Lookup(key)
	if !ok || conv.OptsID == 0 {
		return
	}
	err := h.cfg.Client.DeleteMessage(ctx, conv.OptsID)
	switch {
	case err == nil:
		return
	case zulipproto.IsMissing(err):
		// Already gone — a human deleted it, or the topic moved.
		// Nothing to retire and nothing to say.
		return
	case !zulipproto.RejectedByServer(err):
		// Not a refusal: the server could not be reached, so the edit
		// would fail identically.
		h.cfg.Logf("handler: deleting options panel %d in %s: %v", conv.OptsID, h.describe(key), err)
		return
	}
	if err := h.cfg.Client.EditMessage(ctx, conv.OptsID, supersededPanel); err != nil {
		h.cfg.Logf("handler: retiring options panel %d in %s: %v", conv.OptsID, h.describe(key), err)
	}
}

// rememberPanel persists the panel's message id.
//
// A conversation the relay has never answered in has no journal entry,
// and commands deliberately do not allocate one — `!opts` in a fresh
// topic must leave nothing on disk. The panel still posts; it is
// simply a one-off that the first real turn's panel replaces.
func (h *Handler) rememberPanel(key journal.Key, id int64) {
	conv, ok := h.cfg.Journal.Lookup(key)
	if !ok {
		return
	}
	if err := h.cfg.Journal.SetOpts(conv.ID, id); err != nil {
		h.cfg.Logf("handler: recording options panel for %s: %v", conv.ID, err)
	}
}

// renderPanel builds the panel's markdown body and its widget payload.
//
// The body comes first and stands alone, because most readers never
// see the widget. The header doubles as the current-state readout, so
// the panel is the menu and the status line at once.
func (h *Handler) renderPanel(key journal.Key, note string) (body, widget string) {
	models, current := h.cfg.Agent.Models()
	conv, engaged := h.cfg.Journal.Lookup(key)
	effective := current
	if engaged {
		if id, set := h.modelOverride(conv.ID); set {
			effective = id
		}
	}
	choices := modelChoices(models, effective)

	var sb strings.Builder
	if note != "" {
		sb.WriteString(note + "\n\n")
	}
	s := command.DisplaySigil
	fmt.Fprintf(&sb, "**⚙️ %s**\n", modelLabel(effective))
	fmt.Fprintf(&sb, "*`%s%s` to change · `%shelp` for everything*\n", s, optsVerb, s)

	if len(choices) > 0 {
		sb.WriteString("\n**Model**\n")
		for _, c := range choices {
			marker := ""
			if c.Reply == modelReply(effective) {
				marker = " ←"
			}
			fmt.Fprintf(&sb, "- `%s`%s\n", c.Reply, marker)
		}
		if n := len(models) - len(choices); n > 0 {
			fmt.Fprintf(&sb, "- …and %d more — `%smodel <filter>`\n", n, s)
		}
	} else {
		fmt.Fprintf(&sb, "\nNo models available — connect a provider with `%slogin`.\n", s)
	}

	sb.WriteString("\n**Session**\n")
	fmt.Fprintf(&sb, "- `%snew` — fresh context, same model\n", s)
	fmt.Fprintf(&sb, "- `%sstop` — interrupt the running turn\n", s)
	fmt.Fprintf(&sb, "- `%sstatus` — full detail\n", s)

	// A panel can be asked for before this place HAS a conversation —
	// commands never allocate one. Say so, because until it exists
	// every control here answers "there is no conversation here yet",
	// and in a channel a button's reply would not even be answered:
	// an unengaged topic ignores a message that does not mention the
	// bot. Better to be told than to tap something that does nothing.
	if !engaged {
		sb.WriteString("\n*No conversation here yet — " + startHint(key) + " first; these controls need one.*\n")
	}

	// The session buttons repeat the lines above so the web reader can
	// tap them; the markdown reader has already read them.
	choices = append(choices,
		zulipproto.Choice("new", "Fresh context", s+"new"),
		zulipproto.Choice("stop", "Interrupt the turn", s+"stop"),
		zulipproto.Choice("status", "Full detail", s+"status"),
	)
	return sb.String(), zulipproto.ZForm("⚙️ Options", choices)
}

// startHint says how to start a conversation where the panel is
// showing. A DM is addressed to the bot by construction; a channel
// topic has to summon it.
func startHint(key journal.Key) string {
	if key.IsDM() {
		return "send a message"
	}
	return "@-mention me"
}

// modelChoices builds the model buttons.
//
// Buttons are drawn ONLY from what the agent reported, so a click can
// never ask for a model the agent does not have. The current model is
// pinned first and the rest follow in the agent's own order, which is
// the order the agent considers useful.
func modelChoices(models []client.ModelInfo, current string) []zulipproto.ZFormChoice {
	ordered := make([]client.ModelInfo, 0, len(models))
	for _, m := range models {
		if m.ID == current {
			ordered = append(ordered, m)
		}
	}
	for _, m := range models {
		if m.ID != current {
			ordered = append(ordered, m)
		}
	}
	if len(ordered) > optsModelCap {
		ordered = ordered[:optsModelCap]
	}
	out := make([]zulipproto.ZFormChoice, 0, len(ordered))
	for _, m := range ordered {
		long := m.Name
		if long == "" {
			long = m.ID
		}
		out = append(out, zulipproto.Choice(modelLabel(m.ID), long, modelReply(m.ID)))
	}
	return out
}

// modelReply is the message a model button sends.
func modelReply(id string) string { return command.DisplaySigil + "model " + id }

// modelLabel shortens a model id to something that fits on a button:
// "anthropic/claude-opus-4-5" reads as "claude-opus-4-5". The full id
// is still in the button's reply and in the markdown line, so nothing
// is hidden — this is only what a thumb has to hit.
func modelLabel(id string) string {
	if id == "" {
		return "no model"
	}
	if i := strings.LastIndexByte(id, '/'); i >= 0 && i < len(id)-1 {
		return id[i+1:]
	}
	return id
}
