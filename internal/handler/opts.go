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
// phone app shows the message's plain markdown. So the body must be
// complete, current and usable with a thumb, and is written first. If
// the server rejects the widget outright, the panel still posts (see
// postPanel).
//
// What this comment USED to say — that the markdown body is "the
// product" and the widget mere decoration — is false, and was false
// when written. MEASURED on Zulip 12.2: when a message carries
// widget_content the WEB client hides the markdown body ENTIRELY and
// renders only the widget. So the body is the product on every client
// that does not render widgets, and invisible on the one that does.
// Both halves have to stand alone; neither is decoration.
//
// # One panel per conversation — and a poll beside it
//
// `!opts` posts a PAIR of messages: the panel proper, and a model
// poll (poll.go). They are posted together, retired together, and
// recorded together in the journal (journal.SetOpts takes both ids).
//
// The split is forced by the wire: a message carries at most one
// widget_content, and the two controls need different widgets. It is
// also the right split. Choosing a model is the one control with more
// than three options and the one a phone reader actually reaches for,
// and a poll is the only Zulip widget MEASURED to render AND be
// votable on iOS. The panel keeps the session controls, which are
// three fixed actions and fit a chip each.
//
// # Why it is re-posted rather than edited
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
// EVERY client, so the panel seeds its own emoji chips: `new`,
// `octagonal_sign` for stop, and `bar_chart` for status. Tapping a
// chip produces a reaction event, which reaction.go routes back
// through optsReaction below into the SAME `!` dispatch a typed
// command and a zform click walk. Three surfaces, one parser; see
// optsChips.
//
// There used to be `one`..`six` digit chips for the model choices too,
// and they are GONE. Two reasons, either sufficient:
//
//   - They did not work. MEASURED: with all nine chips present on the
//     server (confirmed by API read), the iOS client drew the row
//     starting at `three` — `:one:` and `:two:` never appeared, which
//     is to say the current model and the one below it were
//     unreachable by thumb, on the client the chips existed for. Cause
//     unknown; not a count limit, not a seeding failure.
//   - They are redundant. The model poll is a better surface on every
//     measured axis: it renders and votes on iOS, and each option
//     carries its own TEXT label, so nothing depends on a glyph
//     drawing or on a positional emoji↔model mapping a footer has to
//     explain.
//
// The three session chips survive because the poll has no equivalent
// for them: `!new`, `!stop` and `!status` are actions, not a choice
// among alternatives, and a poll option that fires one would be a
// checkbox pretending to be a button.
//
// Two things about the remaining chips were measured and are
// load-bearing:
//
//   - The chips must be added SEQUENTIALLY. Zulip renders the chip row
//     in first-added order, so a concurrent seed would scramble them
//     against the legend that names them.
//   - A bot CANNOT remove somebody else's reaction. The DELETE
//     succeeds and removes only the bot's own, so an un-tap can never
//     be undone — which is why op=remove is consumed and dropped here
//     and why optsReactionFooter says so out loud. A chip left behind
//     is cosmetic: a model change repaints, and the repaint deletes
//     the whole panel, chips and all. A `!stop` or `!status` tap does
//     not repaint, and its chip simply stays lit.
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

// optsModelCap bounds how many model options the poll offers.
//
// An agent can advertise a hundred models and a menu is not a
// catalogue — on a phone it has to fit on one screen. The current
// model is always among them (see modelChoices), and `!model <filter>`
// remains the way to reach the rest.
const optsModelCap = 6

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

// optsReactionFooter is the panel's one-line legend for its chips.
//
// It says the mapping because a phone reader sees a row of bare emoji
// with nothing to explain them, and it says un-tapping does nothing
// because that is a surprise otherwise: Zulip lets a user remove their
// own reaction, and a bot cannot undo it, so the chip vanishes while
// the relay does nothing at all. Better to say so than to look broken.
//
// It no longer takes a model count: the models live in the poll, whose
// options label themselves.
func optsReactionFooter() string {
	return fmt.Sprintf("*Tap a chip — :%s: new, :%s: stop, :%s: status. (Un-tapping does nothing.)*",
		optsNewEmoji, optsStopEmoji, optsStatusEmoji)
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

// showPanel posts a fresh control pair at the bottom of the
// conversation and retires whatever pair it replaces. note is optional
// prose shown above it — it is how an unknown command explains itself.
//
// Serialised across the whole relay by optsMu, because post → retire →
// remember is a read-modify-write over the recorded message ids and
// there are two callers on different goroutines: a human's command on
// the event loop, and the agent's own `select_model` tool mid-turn.
// Interleaved, both would delete the same old pair and only one of the
// two NEW ones would be remembered — leaving a live panel nothing will
// ever retire.
func (h *Handler) showPanel(ctx context.Context, key journal.Key, note string) {
	id, chips, ok := h.placePanel(ctx, key, note)
	if !ok {
		return
	}
	// Outside the critical section on purpose — see seedChips.
	h.seedChips(ctx, id, chips)
}

// placePanel is showPanel's locked half: post the model poll and the
// new panel, retire the old pair, and record which messages are now
// live. It returns the new panel's id and the chips it should wear.
//
// The POLL is posted first and the panel under it, which is the
// opposite of the reading order you would guess and is load-bearing
// twice over. The panel is the state readout, so it belongs nearest
// the reader — the same reason the pair is re-posted at the bottom
// rather than edited in place. And the panel's own text says whether
// there is a poll to vote in, which cannot be known until the poll has
// actually gone up: rendering the panel first would mean pointing at a
// control that the next call then fails to post, which is exactly the
// kind of lie a panel that doubles as a status line must not tell.
//
// If the panel fails, the poll just posted is deleted rather than left
// behind. An orphan poll is not a degraded control: the old pair is
// still live and still recorded, so the topic would show two polls and
// only one of them would resolve a vote.
func (h *Handler) placePanel(ctx context.Context, key journal.Key, note string) (int64, []optsChip, bool) {
	h.optsMu.Lock()
	defer h.optsMu.Unlock()

	models, effective, engaged, choices := h.panelState(key)
	post := &convPoster{client: h.cfg.Client, key: key}
	pollID := h.postModelPoll(ctx, post, key, engaged, effective, choices)

	body, widget, chips := h.renderPanel(key, note, models, effective, engaged, pollID != 0, choices)
	id, err := h.postPanel(ctx, post, body, widget)
	if err != nil {
		h.cfg.Logf("handler: posting options panel to %s: %v", h.describe(key), err)
		if pollID != 0 {
			h.retireMessage(ctx, key, pollID, "orphaned model poll")
		}
		return 0, nil, false
	}
	h.retirePrevious(ctx, key)
	if !h.rememberPanel(key, id, pollID, pollModelIDs(choices)) {
		// Nothing recorded these messages as THE control pair, so no
		// tap and no vote on them can ever be honoured (optsReaction
		// and pollVote match conv.OptsID / conv.PollID exactly). Post
		// them and seed nothing. renderPanel has already withheld the
		// chips and the legend, and postModelPoll the poll, for the
		// same reason.
		return id, nil, true
	}
	return id, chips, true
}

// seedChips places the panel's tappable chips, in order, one call each.
//
// SEQUENTIAL is the requirement, not an implementation detail: Zulip
// renders a message's reactions in the order they were added, so
// firing these concurrently would draw the row out of order — `stop`
// sitting where the reader expects `new`, above a legend that says
// otherwise. A tap is resolved by emoji NAME, so a scrambled row is a
// readability failure rather than a wrong command, which is exactly
// why it has to be fixed here and cannot be papered over later. The
// cost is three serial round-trips after the panel is already visible
// and its zform already tappable.
//
// Deliberately called with optsMu RELEASED. The lock exists for the
// post → retire → remember read-modify-write over the recorded message
// ids; seeding touches none of that, and holding it across the
// round-trips would block the agent's own `select_model` for no
// correctness gain. If a second panel replaces this one mid-seed the
// remaining calls simply fail against a deleted message and are
// logged.
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

// optsChips is the panel's chip table: which emoji means which
// command, in the order they are seeded and therefore rendered.
//
// It is CONSTANT, and that is the point of having moved the models to
// a poll. The table used to depend on the agent's model list, which
// can be re-probed — after `!login`, or when a provider is connected —
// without anything repainting the panel, so it had to be pinned to the
// message it was seeded on and recomputed only as a post-restart
// fallback. `!new`, `!stop` and `!status` depend on nothing, so the
// pinning, its lock, its map and its restart fallback are all gone.
//
// Empty when reactions are off. A chip is only half a control: the
// other half is the reaction event, and with "reactions": false the
// relay does not even subscribe to those (see cmd/zulip-acp/main.go).
// Seeding a row of buttons that provably cannot do anything is worse
// than showing none.
func (h *Handler) optsChips() []optsChip {
	if !h.cfg.Reactions {
		return nil
	}
	s := command.DisplaySigil
	return []optsChip{
		{emoji: optsNewEmoji, reply: s + "new"},
		{emoji: optsStopEmoji, reply: s + "stop"},
		{emoji: optsStatusEmoji, reply: s + "status"},
	}
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

// retirePrevious removes the control pair this conversation had before
// the one just posted, so exactly one panel and one poll are ever live.
//
// DELETE first, because a message carrying a widget cannot be edited
// at all. Deleting one's own message is a realm policy and is
// time-limited (message_content_delete_limit_seconds), so a refusal is
// expected rather than exceptional: fall back to rewriting the body to
// a pointer line, which works for a message posted without its widget.
// It never works for the POLL — a poll always carries a submessage, so
// it is sealed by construction — which is why a realm that forbids
// deletion simply leaves the old poll in the scrollback. That is safe
// rather than merely tolerable: the vote path matches conv.PollID
// exactly, so a poll that is no longer the live one is inert.
// If both are refused the old message simply stays — stale, but
// harmless, since every button on it is still a valid command.
func (h *Handler) retirePrevious(ctx context.Context, key journal.Key) {
	conv, ok := h.cfg.Journal.Lookup(key)
	if !ok {
		return
	}
	// The poll goes first: a stale poll is the more misleading of the
	// two, because its options still look votable.
	if conv.PollID != 0 {
		h.retireMessage(ctx, key, conv.PollID, "model poll")
	}
	if conv.OptsID != 0 {
		h.retireMessage(ctx, key, conv.OptsID, "options panel")
	}
}

// retireMessage deletes one retired control message, falling back to
// an edit and then to leaving it alone. what names it in the log.
func (h *Handler) retireMessage(ctx context.Context, key journal.Key, id int64, what string) {
	err := h.cfg.Client.DeleteMessage(ctx, id)
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
		h.cfg.Logf("handler: deleting %s %d in %s: %v", what, id, h.describe(key), err)
		return
	}
	if err := h.cfg.Client.EditMessage(ctx, id, supersededPanel); err != nil {
		h.cfg.Logf("handler: retiring %s %d in %s: %v", what, id, h.describe(key), err)
	}
}

// rememberPanel persists the control pair's message ids, and reports
// whether anything now calls these messages the conversation's
// controls.
//
// A conversation the relay has never answered in has no journal entry,
// and commands deliberately do not allocate one — `!opts` in a fresh
// topic must leave nothing on disk. The panel still posts; it is
// simply a one-off that the first real turn's panel replaces. The
// false it returns is what stops the caller pinning an in-memory
// option table for a poll that nothing will ever retire.
func (h *Handler) rememberPanel(key journal.Key, id, pollID int64, pollModels []string) bool {
	conv, ok := h.cfg.Journal.Lookup(key)
	if !ok {
		return false
	}
	if pollID == 0 {
		// No poll went up, so nothing means anything: recording an
		// option table for a poll that does not exist would leave the
		// PREVIOUS poll's meaning attached to a conversation that has
		// none.
		pollModels = nil
	}
	if err := h.cfg.Journal.SetOpts(conv.ID, id, pollID, pollModels); err != nil {
		// Journal.commit ROLLS BACK its in-memory state when the write
		// fails, so this pair is not recorded anywhere: no tap and no
		// vote on it will resolve, and the next `!opts` replaces it.
		// That is the safe direction and the reason this is logged
		// rather than retried — the alternative to an inert control is
		// resolving a vote against a table nobody wrote down.
		h.cfg.Logf("handler: recording options panel for %s: %v", conv.ID, err)
	}
	return true
}

// renderPanel builds the panel's markdown body and its widget payload.
//
// Both halves have to stand alone: a client that renders widgets shows
// ONLY the widget, and a client that does not shows only the markdown.
// The header doubles as the current-state readout, so the panel is the
// menu and the status line at once.
//
// The MODELS are not here. They are the poll posted underneath (see
// postModelPoll) — one control, one surface, and a model name that a
// phone can read and tap.
func (h *Handler) renderPanel(key journal.Key, note string, models []client.ModelInfo, effective string, engaged, poll bool, choices []zulipproto.ZFormChoice) (body, widget string, chips []optsChip) {
	var sb strings.Builder
	if note != "" {
		sb.WriteString(note + "\n\n")
	}
	s := command.DisplaySigil
	fmt.Fprintf(&sb, "**⚙️ %s**\n", modelLabel(effective))
	fmt.Fprintf(&sb, "*`%s%s` to change · `%shelp` for everything*\n", s, optsVerb, s)

	// The model list lives HERE, not on the poll, and that is forced
	// by the wire: a poll message's content IS the `/poll` slash
	// command (see zulipproto.PollContent), so a client that does not
	// render widgets shows the reader a literal "/poll …" line and
	// nothing they can use. The panel is the only place a typable list
	// can go.
	//
	// Whether it points at the poll depends on whether the poll ABOVE
	// actually went up, which is why the poll is posted first: a panel
	// that doubles as a status line may not point at a control that is
	// not there.
	if len(choices) == 0 {
		fmt.Fprintf(&sb, "\nNo models available — connect a provider with `%slogin`.\n", s)
	} else {
		sb.WriteString("\n**Model**")
		if poll {
			sb.WriteString(" — vote in the poll above, or:")
		}
		sb.WriteString("\n")
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

	chips = h.optsChips()
	if !engaged {
		// No conversation here yet, so every control on this panel
		// answers "there is none" — and in a channel a button's reply
		// would not even be answered, since an unengaged topic ignores
		// a message that does not mention the bot. The buttons are
		// rendered anyway because they cost nothing; a chip costs an
		// HTTP call each and would sit there looking live. The hint
		// above says what to do instead.
		chips = nil
	}

	// The footer names the chip mapping, and only when there are
	// chips. On a phone the reaction row is the sole tappable thing on
	// the message, and a row of bare emoji with nothing to explain it
	// is a puzzle; with reactions off there is no row and the line
	// would be a promise the relay does not keep.
	if len(chips) > 0 {
		sb.WriteString("\n" + optsReactionFooter() + "\n")
	}

	// The session buttons repeat the lines above so the web reader can
	// tap them — and on web they are all the reader gets, since the
	// widget hides the body entirely.
	buttons := []zulipproto.ZFormChoice{
		zulipproto.Choice("new", "Fresh context", s+"new"),
		zulipproto.Choice("stop", "Interrupt the turn", s+"stop"),
		zulipproto.Choice("status", "Full detail", s+"status"),
	}
	return sb.String(), zulipproto.ZForm("⚙️ "+modelLabel(effective), buttons), chips
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
//   - the emoji must be in the chip table (optsChips). Anything else —
//     a human's own :+1: on the panel — is not ours and falls through
//     to the agent as ordinary ambient signal.
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
// posted reply. The chips are `!new`, `!stop` and `!status` — none of
// them repaints the panel, so their chips simply stay lit.
func (h *Handler) optsReaction(ctx context.Context, conv journal.Conv, ev zulipproto.Event) bool {
	if conv.OptsID == 0 || ev.MessageID != conv.OptsID {
		return false
	}
	reply := ""
	for _, c := range h.optsChips() {
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
