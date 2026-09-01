// This file is zulip-acp's half of the relay command surface. The
// broker itself — sigils, classification, the login state machine, the
// passthrough allowlist and all the rendering — lives in
// github.com/kfet/acp-kit/command, shared with poe-acp so the two
// relays cannot drift.
//
// What stays here is what only Zulip knows:
//
//  1. The Controller implementation, over the journal, the session
//     manager and the handler's own inflight bookkeeping.
//  2. The surface pre-filter: Zulip's `/me`, `/poll` and `/todo` are
//     real messages, not client-side slash commands, and must reach the
//     agent untouched.
//  3. The `!!` escape and the unknown-command error, which are this
//     relay's policy about prose that merely starts with a sigil.
//
// Ordering is load-bearing and unchanged from a prompt's: the
// bot-own-message and system-bot guards run first, then
// `allowed_user_ids`, then the engagement gate, and only then is
// anything parsed as a command. Dispatch also runs off Journal.Lookup,
// BEFORE Journal.Ensure, so no command ever allocates a conversation:
// `!help` in a topic the relay has never answered in leaves nothing on
// disk.
package handler

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/kfet/acp-kit/client"
	"github.com/kfet/acp-kit/command"
	"github.com/kfet/zulip-acp/internal/journal"
	"github.com/kfet/zulip-acp/internal/zulipproto"
)

// convsDir is the state-manager subdirectory holding per-conversation
// working directories. It mirrors acp-kit's default cwd layout,
// <StateDir>/convs/<conv-id>, which the relay does not override.
const convsDir = "convs"

// zulipWidgets are Zulip message names that LOOK like slash commands
// and are not.
//
// Zulip's real slash commands (/ping, /dark, /light, …) are handled
// client-side in zcommands.js against the /json/command endpoint and
// never send a message at all, so a bot cannot see them and there is
// no bot slash-command registration API to collide with. But these
// three DO arrive: /me is flagged is_me_message by the markdown
// processor, and /poll and /todo become widgets. They must reach the
// agent byte-for-byte — swallowing a poll because "poll" is not in the
// command list would be indefensible.
//
// This is why "/" is accepted on input but never advertised: the
// broker's DisplaySigil is "!".
var zulipWidgets = map[string]bool{
	"me":   true,
	"poll": true,
	"todo": true,
}

// isWidget reports whether trimmed, mention-stripped text is one of
// Zulip's message-shaped slash commands.
//
// The name is lower-cased before matching, which is deliberately MORE
// permissive than Zulip itself — its markdown processor matches "/me"
// case-sensitively, so "/ME" is not a me-message server-side. The
// over-match is the safe direction and must stay that way: a
// false positive here can only forward a message to the agent
// unchanged, never eat one, and none of "me", "poll" or "todo" is a
// relay command, so nothing is shadowed. Do not "fix" this into a
// case-sensitive match — that trades a harmless pass-through for the
// chance of swallowing a widget.
func isWidget(text string) bool {
	if !strings.HasPrefix(text, "/") {
		return false
	}
	body, _ := command.StripSigil(text)
	name := body
	if i := strings.IndexAny(body, " \t\n"); i >= 0 {
		name = body[:i]
	}
	return zulipWidgets[strings.ToLower(name)]
}

// dispatch classifies text and, when it names a command, hands it to
// the broker.
//
// It returns the prose to forward to the agent and whether the message
// was consumed. handled=true means the relay is done with this message.
func (h *Handler) dispatch(ctx context.Context, m *zulipproto.Message, key journal.Key, text string) (prompt string, handled bool) {
	// Zulip's own widgets win over everything, including a pending
	// login: a /poll must never be eaten as a failed redirect paste.
	if isWidget(text) {
		return text, false
	}
	b := h.cfg.Commands
	if b == nil {
		return text, false
	}
	token := key.Token()

	// A leading "!!" is this relay's escape for prose that genuinely
	// starts with a sigil. Checked before the broker so the broker
	// never sees it — and deliberately BEFORE the pending-login check
	// too: someone typing "!!foo" mid-login is plainly not pasting a
	// redirect URL, so honouring the escape does what they asked and
	// leaves the login pending for the paste that follows. Consuming
	// it as a malformed redirect would abort the login instead.
	if rest, ok := strings.CutPrefix(text, doubleSigil); ok {
		return command.DisplaySigil + rest, false
	}

	// A pasted redirect URL for an in-flight login is not sigil-
	// prefixed, so it can only be recognised by asking the broker.
	if b.HasPending(token) || b.IsCommand(text) {
		out, err := b.Handle(ctx, token, text)
		if err != nil {
			h.cfg.Logf("handler: command %q in %s: %v", text, h.describe(key), err)
			h.reply(ctx, key, fmt.Sprintf("Command failed: %v", err))
			return "", true
		}
		h.reply(ctx, key, mustOutcome(out).Text)
		return "", true
	}

	// An allowlisted agent command is rewritten to its slash form and
	// forwarded through the normal prompt path, so the agent runs it
	// and streams a reply like any other turn.
	if rewritten, ok := b.Passthrough(text); ok {
		return rewritten, false
	}

	// Sigil-prefixed, command-shaped, and nothing recognised it. Say
	// so rather than burning an agent turn on a typo — and name the
	// escape, because the same rule is what makes "!!" necessary.
	if name, ok := unknownCommand(text); ok {
		h.reply(ctx, key, fmt.Sprintf("Unknown command `%s%s`. Send `%shelp` for the list, or `%s%s` to say it as text.",
			command.DisplaySigil, name, command.DisplaySigil, doubleSigil, name))
		return "", true
	}
	return text, false
}

// doubleSigil is the escape a human types to send prose beginning with
// the display sigil: "!!new" reaches the agent as "!new".
const doubleSigil = command.DisplaySigil + command.DisplaySigil

// unknownCommand reports whether text is command-SHAPED but names
// nothing, returning the offending name.
//
// Command-shaped means the sigil is followed by an ASCII letter and
// then letters, digits, "_" or "-". The check is deliberately strict:
// it is the only thing standing between "!important: fix this" and a
// swallowed message, and eating a user's prose is far worse than
// missing a typo'd command.
//
// Only the display sigil counts here. "/" is excluded because it is
// Zulip's own namespace — an unrecognised "/foo" is far more likely to
// be a Zulip feature than a mistyped relay command — and "." because a
// message starting with a full stop is ordinary punctuation.
func unknownCommand(text string) (string, bool) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(text), command.DisplaySigil)
	if !ok {
		return "", false
	}
	name := rest
	if i := strings.IndexAny(rest, " \t\n"); i >= 0 {
		name = rest[:i]
	}
	if !commandShaped(name) {
		return "", false
	}
	return strings.ToLower(name), true
}

// commandShaped reports whether tok looks like a command name.
func commandShaped(tok string) bool {
	if tok == "" {
		return false
	}
	for i := 0; i < len(tok); i++ {
		c := tok[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
		case i > 0 && (c >= '0' && c <= '9' || c == '_' || c == '-'):
		default:
			return false
		}
	}
	return true
}

// reply posts a command's answer where the command arrived — same
// topic, or same DM participant set.
//
// Commands are not turns: no placeholder, no streaming, no :eyes:
// lifecycle and no tail tracking, because none of that fits a reply
// that is already complete when it is composed. A reply that cannot be
// posted is logged and dropped; unlike agent output it costs nothing
// to ask for again, so the rescue path does not apply.
func (h *Handler) reply(ctx context.Context, key journal.Key, content string) {
	if strings.TrimSpace(content) == "" {
		return
	}
	post := &convPoster{client: h.cfg.Client, key: key}
	if _, err := post.Post(ctx, content); err != nil {
		h.cfg.Logf("handler: posting command reply to %s: %v", h.describe(key), err)
	}
}

// rememberDMNames records a DM's participant display names, so
// `!status` can say who is in it rather than reciting user ids.
func (h *Handler) rememberDMNames(key journal.Key, names []string) {
	if len(names) == 0 {
		return
	}
	h.dmMu.Lock()
	h.dmNames[key.Token()] = names
	h.dmMu.Unlock()
}

// whereFor renders a conversation key the way a person would say it.
func (h *Handler) whereFor(key journal.Key) string {
	if !key.IsDM() {
		if key.Topic == "" && key.StreamID == 0 {
			return ""
		}
		return h.describe(key)
	}
	h.dmMu.Lock()
	names := h.dmNames[key.Token()]
	h.dmMu.Unlock()
	if len(names) > 0 {
		return "DM with " + strings.Join(names, ", ")
	}
	return key.Label()
}

// --- command.Controller --------------------------------------------------
//
// The broker identifies a conversation by an opaque token it hands
// straight back. This relay passes the KEY's token, never the conv-id:
// `!new` replaces the conv-id, so a broker holding one would be holding
// a stale identity. See journal.Key.Token.

// AvailableModels satisfies command.Controller.
func (h *Handler) AvailableModels() (models []client.ModelInfo, currentID string) {
	return h.cfg.Agent.Models()
}

// AgentCommands satisfies command.Controller.
func (h *Handler) AgentCommands() []client.CommandInfo {
	return h.cfg.Agent.AvailableCommands()
}

// convFor resolves a broker token to its conversation, if one exists.
// A token that does not parse is a programming error on the relay
// side, not user input, so it is logged rather than surfaced.
func (h *Handler) convFor(token string) (journal.Key, journal.Conv, bool) {
	key, err := journal.ParseToken(token)
	if err != nil {
		h.cfg.Logf("handler: %v", err)
		return journal.Key{}, journal.Conv{}, false
	}
	c, ok := h.cfg.Journal.Lookup(key)
	return key, c, ok
}

// StatusFor satisfies command.Controller.
func (h *Handler) StatusFor(token string) command.SessionStatus {
	key, conv, engaged := h.convFor(token)
	// One call, not two: separate reads could straddle a model-state
	// update and report a current model that is not in the list.
	models, current := h.cfg.Agent.Models()
	st := command.SessionStatus{
		EffectiveModel:  current,
		DefaultModel:    current,
		HasSession:      engaged,
		ModelsAvailable: len(models),
		Where:           h.whereFor(key),
	}
	if engaged {
		st.ConvID = conv.ID
		st.StateDir = filepath.Join(h.cfg.Sessions.StateDir(), convsDir, conv.ID)
		st.TurnRunning = h.isInflight(conv.ID)
		if id, ok := h.modelOverride(conv.ID); ok {
			st.OverrideModel, st.EffectiveModel = id, id
		}
	}
	return st
}

// RelayInfo satisfies command.Controller.
func (h *Handler) RelayInfo(token string) command.RelayInfo {
	_, conv, engaged := h.convFor(token)
	models, _ := h.cfg.Agent.Models()
	ri := command.RelayInfo{
		Version:         h.cfg.Version,
		AgentCmd:        h.cfg.AgentCmd,
		ModelsAvailable: len(models),
		ActiveSessions:  h.cfg.Journal.ActiveCount(),
	}
	if !h.cfg.StartTime.IsZero() {
		ri.Uptime = h.now().Sub(h.cfg.StartTime).Round(time.Second).String()
	}
	if engaged {
		ri.SessionID = conv.ID
	}
	return ri
}

// SetModelOverride satisfies command.Controller. The choice is sticky
// per conversation and applied to the ACP session at the start of the
// next turn — see applyModel.
func (h *Handler) SetModelOverride(token, modelID string) error {
	_, conv, engaged := h.convFor(token)
	if !engaged {
		return fmt.Errorf("there is no conversation here yet — send a message first")
	}
	models, _ := h.cfg.Agent.Models()
	found := len(models) == 0
	for _, m := range models {
		if m.ID == modelID {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("unknown model %q", modelID)
	}
	h.setModelOverride(conv.ID, modelID)
	return nil
}

// ResetSession satisfies command.Controller: it is what `!new` calls.
//
// On Zulip this RETIRES the journal entry and allocates a fresh
// conv-id. The old state/convs/<id>/ directory is left exactly where it
// is — retiring a conversation is not deleting work. The broker never
// learns the id changed, which is precisely why it is handed a key
// token rather than a conv-id.
func (h *Handler) ResetSession(token string) error {
	key, conv, engaged := h.convFor(token)
	if engaged {
		// A turn still running in the retired conversation would keep
		// streaming into a conversation the user has just declared
		// over.
		h.cancelInflight(context.Background(), conv.ID)
	}
	// Retire is the single source of truth for whether there WAS a
	// conversation. Pre-checking `engaged` and erroring on it
	// separately would split one answer across two branches, the
	// second of which nothing can reach.
	prev, fresh, existed, err := h.cfg.Journal.Retire(key)
	if err != nil {
		h.cfg.Logf("handler: retiring conversation in %s: %v", h.describe(key), err)
		return err
	}
	if !existed {
		return fmt.Errorf("there is no conversation here yet, so your next message already starts a fresh one")
	}
	// The model choice is the user's, not the conversation's: carry it
	// across so `!new` clears context without silently reverting it.
	h.carryModelOverride(prev.ID, fresh.ID)
	h.cfg.Logf("handler: %s retired for fresh conversation %s in %s", prev.ID, fresh.ID, h.describe(key))
	return nil
}

// StopTurn satisfies command.TurnStopper, which is what enables
// `!stop`. poe-acp deliberately does not implement it: it answers one
// HTTP request per turn and has no in-flight turn a later message
// could reach. This relay streams into an editable message and does.
func (h *Handler) StopTurn(token string) bool {
	_, conv, engaged := h.convFor(token)
	if !engaged {
		return false
	}
	// context.Background: see ResetSession.
	return h.cancelInflight(context.Background(), conv.ID)
}

// Compile-time proof that the Handler satisfies the broker's
// interfaces. Without these, a signature drift in acp-kit would surface
// as `!status` silently reporting "Session control is unavailable" at
// runtime instead of as a build failure.
var (
	_ command.Controller  = (*Handler)(nil)
	_ command.TurnStopper = (*Handler)(nil)
)
