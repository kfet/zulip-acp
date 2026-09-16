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
//
// # The phone cannot tap a zform — so the panel also wears reactions
//
// zform is a web-only surface, which left the phone reader — the
// DEFAULT reader, see above — with markdown they had to retype by
// thumb. Reactions are the one interactive control Zulip renders on
// EVERY client, so the panel seeds its own emoji chips: `one`..`six`
// for the model choices in modelChoices order, `new`, `octagonal_sign`
// for stop, and `bar_chart` for status. Tapping a chip produces a
// reaction event, which reaction.go routes back through optsReaction
// below into the SAME `!` dispatch a typed command and a zform click
// walk. Three surfaces, one parser; see optsChips.
//
// Two things about this were measured and are load-bearing:
//
//   - The chips must be added SEQUENTIALLY. Zulip renders the chip row
//     in first-added order, so a concurrent seed would scramble
//     `one`..`six` against the model list they name.
//   - A bot CANNOT remove somebody else's reaction. The DELETE
//     succeeds and removes only the bot's own, so an un-tap can never
//     be undone — which is why op=remove is consumed and dropped here
//     and why optsReactionFooter says so out loud. A chip left behind
//     is cosmetic: a model-change tap repaints, and the repaint
//     deletes the whole panel, chips and all. A `!stop` or `!status`
//     tap does not repaint, and its chip simply stays lit.
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

// optsDigitEmoji are the chips that stand for the model choices, in
// modelChoices order. There are exactly optsModelCap of them, which is
// not a coincidence and must not become one: the panel may never offer
// a model it cannot name with a chip. `one`..`six` are all in Zulip's
// built-in set (verified against Zulip 12.2 — AddReaction accepted
// every one of them).
var optsDigitEmoji = [optsModelCap]string{"one", "two", "three", "four", "five", "six"}

// The session chips. Each is the exact analogue of the zform button
// below it, and carries the same reply string, so a tap and a click
// cannot mean different things.
//
// Every name here was checked against the live server before it was
// written down, because Zulip answers an unknown emoji with a 400
// "Emoji ... does not exist" and a seed that half-fails leaves a panel
// whose chips do not line up with the list they label. The two names
// that LOOK right and are NOT in Zulip's set: `information_source` and
// `arrows_counterclockwise` — both rejected. So was `mag`.
//
// optsStatusEmoji is `bar_chart` rather than the more obvious `eyes`
// for a second reason: `eyes` is the relay's own in-flight ack (see
// Handler.ack), so a status chip wearing it would be indistinguishable
// from "a turn is running".
const (
	optsNewEmoji    = "new"
	optsStopEmoji   = "octagonal_sign"
	optsStatusEmoji = "bar_chart"
)

// optsChip is one tappable chip: the emoji the bot seeds, and the
// command text a tap on it means.
//
// The reply is the SAME string the matching zform button carries. That
// is the whole design: a chip is not a new capability, it is a second
// way to type a command that already exists.
type optsChip struct {
	emoji string
	reply string
}

// optsReactionFooter is the panel's one-line legend for its chips,
// where models is how many digit chips it actually has.
//
// It says the mapping because a phone reader sees a row of bare emoji
// with nothing to explain them, and it says un-tapping does nothing
// because that is a surprise otherwise: Zulip lets a user remove their
// own reaction, and a bot cannot undo it, so the chip vanishes while
// the relay does nothing at all. Better to say so than to look broken.
//
// The digit range is counted rather than fixed at optsModelCap: an
// agent reporting two models gets a panel that says "1-2", because a
// legend naming chips that are not there is the same lie as a button
// that does nothing.
func optsReactionFooter(models int) string {
	digits := fmt.Sprintf("1-%d models", models)
	switch models {
	case 0:
		digits = "no models"
	case 1:
		digits = "1 model"
	}
	return fmt.Sprintf("*Tap a chip to choose — %s, :%s: new, :%s: stop, :%s: status. (Un-tapping does nothing.)*",
		digits, optsNewEmoji, optsStopEmoji, optsStatusEmoji)
}

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
	id, chips, ok := h.placePanel(ctx, key, note)
	if !ok {
		return
	}
	// Outside the critical section on purpose — see seedChips.
	h.seedChips(ctx, id, chips)
}

// placePanel is showPanel's locked half: post the new panel, retire the
// old one, and record which message is now the live panel. It returns
// the new panel's id and the chips it should wear.
func (h *Handler) placePanel(ctx context.Context, key journal.Key, note string) (int64, []optsChip, bool) {
	h.optsMu.Lock()
	defer h.optsMu.Unlock()

	body, widget, chips := h.renderPanel(key, note)
	post := &convPoster{client: h.cfg.Client, key: key}
	id, err := h.postPanel(ctx, post, body, widget)
	if err != nil {
		h.cfg.Logf("handler: posting options panel to %s: %v", h.describe(key), err)
		return 0, nil, false
	}
	h.retirePrevious(ctx, key)
	if !h.rememberPanel(key, id) {
		// Nothing recorded this message as THE panel, so no tap on it
		// can ever be honoured (optsReaction matches conv.OptsID
		// exactly) — and there is no retirement coming that would drop
		// a pinned chip table again. Post it, seed nothing, hold
		// nothing. renderPanel has already withheld the chips and the
		// legend for the same reason.
		return id, nil, true
	}
	// The chip table is recorded BEFORE the caller starts seeding, and
	// before the panel's id is reachable by a tap, because a tap is
	// only honoured on the id the journal currently calls the panel
	// (see optsReaction). Seeding is a chip per HTTP call, so the
	// window in which an eager thumb hits a chip the relay has not
	// finished placing is not theoretical.
	h.rememberChips(id, chips)
	return id, chips, true
}

// rememberChips records what a panel's chips MEAN, and forgets the
// panel they replace.
//
// This is not a cache, it is the answer. Recomputing the table at tap
// time was the obvious implementation and is wrong: the mapping also
// depends on the agent's model LIST, which can be re-probed — after
// `!login`, or when a provider is connected — without anything
// repainting the panel. A list that reordered between seeding and the
// tap would leave `two` pointing at a model other than the one written
// on the line beside it, on a panel that is still live. So the table
// is pinned to the message it was seeded on.
//
// Bounded by construction: one entry per conversation, replaced when
// its panel is. It is in memory only, so a restart loses it — see
// chipsFor for what happens then.
func (h *Handler) rememberChips(id int64, chips []optsChip) {
	h.chipMu.Lock()
	defer h.chipMu.Unlock()
	h.panelChips[id] = chips
}

// forgetChips drops a retired panel's chip table.
func (h *Handler) forgetChips(id int64) {
	h.chipMu.Lock()
	defer h.chipMu.Unlock()
	delete(h.panelChips, id)
}

// chipsFor returns what the chips on panel id mean.
//
// On a miss — the relay restarted since the panel was posted, and the
// journal remembers the id where memory does not — it falls back to
// recomputing from the panel's current state. That can only be wrong
// in the narrow way rememberChips describes, and the alternative is a
// panel that goes dead across a reload, which is worse: the id is
// still live, the chips are still on the message, and a thumb has no
// way to know the relay has forgotten them.
func (h *Handler) chipsFor(key journal.Key, id int64) []optsChip {
	h.chipMu.Lock()
	chips, ok := h.panelChips[id]
	h.chipMu.Unlock()
	if ok {
		return chips
	}
	_, _, _, choices := h.panelState(key)
	return h.optsChips(choices)
}

// seedChips places the panel's tappable chips, in order, one call each.
//
// SEQUENTIAL is the requirement, not an implementation detail: Zulip
// renders a message's reactions in the order they were added, so
// firing these concurrently would draw the row out of order — `three`
// sitting where the reader expects `one`, above a list that says
// otherwise. A tap is resolved by emoji NAME, so a scrambled row is a
// readability failure rather than a wrong command, which is exactly
// why it has to be fixed here and cannot be papered over later. The
// cost is a handful of serial round-trips after the panel is already
// visible and its zform already tappable.
//
// Deliberately called with optsMu RELEASED. The lock exists for the
// post → retire → remember read-modify-write over one message id;
// seeding touches none of that, and holding it across nine round-trips
// would block the agent's own `select_model` for no correctness gain.
// If a second panel replaces this one mid-seed the remaining calls
// simply fail against a deleted message and are logged.
//
// A refusal is logged and the rest still go: a realm missing one emoji
// should lose one chip, not the whole menu. Nothing here is retried,
// for the same reason nothing else in this file is — a panel is
// decoration over a command surface that a human can always type.
func (h *Handler) seedChips(ctx context.Context, msgID int64, chips []optsChip) {
	for _, c := range chips {
		if err := h.cfg.Client.AddReaction(ctx, msgID, c.emoji); err != nil {
			h.cfg.Logf("handler: seeding :%s: on options panel %d: %v", c.emoji, msgID, err)
		}
	}
}

// optsChips is the panel's chip table: which emoji means which command,
// in the order they are seeded and therefore rendered.
//
// It is derived from exactly what renderPanel shows, and it is the ONE
// place the mapping exists — both the seeding side and the tap side
// call it, so a chip can never come to mean something other than the
// line above it.
//
// Empty when reactions are off. A chip is only half a control: the
// other half is the reaction event, and with "reactions": false the
// relay does not even subscribe to those (see cmd/zulip-acp/main.go).
// Seeding a row of buttons that provably cannot do anything is worse
// than showing none.
//
// It is built at RENDER time and then pinned to the message it was
// seeded on (rememberChips), never recomputed to answer a tap. The
// mapping depends on the agent's model list, which can be re-probed
// without anything repainting the panel; chipsFor falls back to
// calling this again only when a restart has lost the pinned table.
func (h *Handler) optsChips(choices []zulipproto.ZFormChoice) []optsChip {
	if !h.cfg.Reactions {
		return nil
	}
	chips := make([]optsChip, 0, len(choices))
	for i, c := range choices {
		if i >= len(optsDigitEmoji) {
			// Unreachable while optsModelCap bounds the choices, and
			// kept anyway: the alternative to a dropped chip is a
			// panic or a chip with no emoji.
			break
		}
		chips = append(chips, optsChip{emoji: optsDigitEmoji[i], reply: c.Reply})
	}
	s := command.DisplaySigil
	return append(chips,
		optsChip{emoji: optsNewEmoji, reply: s + "new"},
		optsChip{emoji: optsStopEmoji, reply: s + "stop"},
		optsChip{emoji: optsStatusEmoji, reply: s + "status"},
	)
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
	// Unconditionally, before any of the ways this can fail: the old
	// panel stops being THE panel here regardless of whether its
	// message could be removed, and a chip table that outlived its
	// panel is memory held for a message nothing will ever honour a
	// tap on.
	h.forgetChips(conv.OptsID)
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

// rememberPanel persists the panel's message id, and reports whether
// anything now calls this message the conversation's panel.
//
// A conversation the relay has never answered in has no journal entry,
// and commands deliberately do not allocate one — `!opts` in a fresh
// topic must leave nothing on disk. The panel still posts; it is
// simply a one-off that the first real turn's panel replaces. The
// false it returns is what stops the caller pinning an in-memory chip
// table for a panel that nothing will ever retire.
func (h *Handler) rememberPanel(key journal.Key, id int64) bool {
	conv, ok := h.cfg.Journal.Lookup(key)
	if !ok {
		return false
	}
	if err := h.cfg.Journal.SetOpts(conv.ID, id); err != nil {
		// Still true: the id did not reach disk, but the journal's
		// in-memory view has it, so a tap resolves and the eventual
		// retirement drops the chips. Only a restart loses the panel,
		// and chipsFor already handles that.
		h.cfg.Logf("handler: recording options panel for %s: %v", conv.ID, err)
	}
	return true
}

// renderPanel builds the panel's markdown body and its widget payload.
//
// The body comes first and stands alone, because most readers never
// see the widget. The header doubles as the current-state readout, so
// the panel is the menu and the status line at once.
func (h *Handler) renderPanel(key journal.Key, note string) (body, widget string, chips []optsChip) {
	models, effective, engaged, choices := h.panelState(key)

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

	// The chip table is built from the MODEL choices alone: the three
	// session chips are constant and optsChips appends them itself.
	// Built before the session buttons are appended below, which is
	// why that append cannot be hoisted.
	chips = h.optsChips(choices)
	if !engaged {
		// No conversation here yet, so every control on this panel
		// answers "there is none" — and in a channel a button's reply
		// would not even be answered, since an unengaged topic ignores
		// a message that does not mention the bot. The buttons are
		// rendered anyway because they cost nothing; a chip costs an
		// HTTP call each and would sit there looking live. The hint
		// below says what to do instead.
		chips = nil
	}

	// The footer names the chip mapping, and only when there are
	// chips. On a phone the reaction row is the sole tappable thing on
	// the message, and a row of bare digits with nothing to explain it
	// is a puzzle; with reactions off there is no row and the line
	// would be a promise the relay does not keep.
	if len(chips) > 0 {
		sb.WriteString("\n" + optsReactionFooter(len(choices)) + "\n")
	}

	// The session buttons repeat the lines above so the web reader can
	// tap them; the markdown reader has already read them.
	choices = append(choices,
		zulipproto.Choice("new", "Fresh context", s+"new"),
		zulipproto.Choice("stop", "Interrupt the turn", s+"stop"),
		zulipproto.Choice("status", "Full detail", s+"status"),
	)
	return sb.String(), zulipproto.ZForm("⚙️ Options", choices), chips
}

// panelState reads everything the panel is a rendering of: the agent's
// models, the model this conversation would actually use, whether
// there is a conversation at all, and the model buttons that follow.
//
// Factored out because the chip table has to be derivable from the
// same facts WITHOUT re-rendering the panel — optsReaction resolves a
// tap this way. Two copies of this arithmetic is exactly how a chip
// starts pointing at a different model from the line it sits under.
func (h *Handler) panelState(key journal.Key) (models []client.ModelInfo, effective string, engaged bool, choices []zulipproto.ZFormChoice) {
	models, effective = h.cfg.Agent.Models()
	conv, engaged := h.cfg.Journal.Lookup(key)
	if engaged {
		if id, set := h.modelOverride(conv.ID); set {
			effective = id
		}
	}
	return models, effective, engaged, modelChoices(models, effective)
}

// optsReaction resolves a reaction on THIS conversation's live options
// panel into the command the chip stands for, and runs it down the
// ordinary `!` dispatch path. It reports whether it consumed the
// reaction, so reaction.go can stop before handing it to the agent.
//
// The gates, in the order the code applies them and all of them
// necessary:
//
//   - message_id == conv.OptsID EXACTLY. A retired panel must be
//     inert: its chips still sit there in the scrollback (deleting the
//     panel usually takes them, but a realm that forbids deletion
//     leaves the whole message), and a tap on a month-old menu must
//     not reconfigure a live conversation.
//   - the emoji must be in the chip table this panel WAS seeded with.
//     Anything else — a human's own :+1: on the panel — is not ours
//     and falls through to the agent as ordinary ambient signal.
//   - only then, op=add. A removal is consumed and DROPPED rather than
//     passed on: a bot cannot remove another user's reaction (the
//     DELETE succeeds and takes only its own), so an un-tap is
//     un-undoable, and narrating it to the agent would be noise about
//     a control the user has already finished with. The footer says as
//     much.
//
// Not gated here, because reaction.go has already done it before
// calling: the relay's own user id, the bot-sender set, and the
// allowlist. That is the same gate order a typed message walks, which
// is the point — a chip may not be a way around the allowlist.
//
// Acknowledgement is whatever the dispatched command already does: a
// `check` reaction and a repainted panel for a model change, a posted
// reply for the rest. The repaint re-posts the panel and deletes the
// old one, which disposes of the stale chips.
func (h *Handler) optsReaction(ctx context.Context, conv journal.Conv, ev zulipproto.Event) bool {
	if conv.OptsID == 0 || ev.MessageID != conv.OptsID {
		return false
	}
	reply := ""
	for _, c := range h.chipsFor(conv.Key, conv.OptsID) {
		if c.emoji == ev.EmojiName {
			reply = c.reply
			break
		}
	}
	if reply == "" {
		return false
	}
	if ev.Op != zulipproto.ReactionAdd {
		// Consumed and dropped. Un-tapping a chip is not signal: it is
		// a user tidying up after a control they already used, and a
		// bot cannot put it back. Narrating "kfet removed :two: from
		// your own message" to the agent would be noise it then tries
		// to interpret — so the removal is swallowed here rather than
		// falling through to the ambient path.
		return true
	}
	h.cfg.Logf("handler: :%s: on the options panel in %s — running %q", ev.EmojiName, h.describe(conv.Key), reply)
	// Through dispatch, never past it. A chip, a zform button and a
	// typed command must be one code path or they drift — the whole
	// justification for putting a menu on reactions at all is that it
	// adds no capability.
	//
	// The synthetic message carries the PANEL's id, which is what a
	// model change reacts `check` onto — the same message the user
	// just tapped, so the acknowledgement lands where they are
	// looking. Nothing in the chip table reaches a dispatch branch
	// that reads anything else off it.
	m := &zulipproto.Message{ID: ev.MessageID, SenderID: ev.UserID}
	if _, handled := h.dispatch(ctx, m, conv.Key, reply); !handled {
		// Only reachable if a chip's reply stopped being a command —
		// i.e. someone changed the table without changing the parser.
		// Say so rather than silently forwarding a bare "!stop" to the
		// agent as prose.
		h.cfg.Logf("handler: options chip :%s: produced %q, which is no longer a command", ev.EmojiName, reply)
	}
	return true
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
