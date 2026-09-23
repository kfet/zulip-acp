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
//  3. The `!!` escape and the unknown-command answer, which are this
//     relay's policy about prose that merely starts with a sigil.
//  4. `!opts`, the interactive options panel — a Zulip-only surface
//     (zform button widgets), so it cannot live in the shared broker.
//     Everything behind a button is still a broker action; see
//     opts.go.
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
	acp "github.com/coder/acp-go-sdk"
	"github.com/kfet/acp-kit/convo"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/kfet/acp-kit/client"
	"github.com/kfet/acp-kit/command"
	"github.com/kfet/acp-kit/update"
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

// dispatchMeta is the relay context carried on a convo.In through the
// shared Dispatch: the triggering message (nil for a synthesised one)
// and the conversation's key.
type dispatchMeta struct {
	m   *zulipproto.Message
	key journal.Key
}

func metaOf(in *convo.In) dispatchMeta { return in.Meta.(dispatchMeta) }

// dispatch classifies text and, when it names a command, runs it.
//
// It returns the prose to forward to the agent and whether the message
// was consumed. handled=true means the relay is done with this message.
//
// The classification itself is acp-kit's shared convo.Manager.Dispatch
// (broker first, then the passthrough rewrite); this relay contributes
// its rules as filters around it — see beforeFilters and afterFilters,
// whose order is load-bearing and documented there.
func (h *Handler) dispatch(ctx context.Context, m *zulipproto.Message, key journal.Key, text string) (prompt string, handled bool) {
	tok := key.Token()
	res := h.convo.Dispatch(ctx, convo.In{Conv: tok, Token: tok, Text: text, Meta: dispatchMeta{m: m, key: key}})
	return res.Prompt, res.Handled
}

// replySink posts a command's answer where the command arrived.
func (h *Handler) replySink(ctx context.Context, in *convo.In, text string) error {
	h.reply(ctx, metaOf(in).key, text)
	return nil
}

// beforeFilters are this relay's rules that run BEFORE the shared
// broker, in order.
func (h *Handler) beforeFilters() []convo.Filter {
	return []convo.Filter{
		// Zulip's own widgets win over everything, including a pending
		// login: a /poll must never be eaten as a failed redirect paste.
		func(_ context.Context, in *convo.In) convo.Verdict {
			if isWidget(in.Text) || h.cfg.Commands == nil {
				return convo.Forward
			}
			return convo.Pass
		},
		// A leading "!!" is this relay's escape for prose that genuinely
		// starts with a sigil. Checked before the broker so the broker
		// never sees it — and deliberately BEFORE the pending-login check
		// too: someone typing "!!foo" mid-login is plainly not pasting a
		// redirect URL, so honouring the escape does what they asked and
		// leaves the login pending for the paste that follows. Consuming
		// it as a malformed redirect would abort the login instead.
		func(_ context.Context, in *convo.In) convo.Verdict {
			if rest, ok := strings.CutPrefix(in.Text, doubleSigil); ok {
				in.Text = command.DisplaySigil + rest
				return convo.Forward
			}
			return convo.Pass
		},
		// `!opts` is this relay's own: it renders controls that already
		// exist onto a surface only Zulip has (see opts.go), so the shared
		// broker neither knows nor should know about it. Checked before
		// the broker — which would not recognise it — and before the
		// pending-login path, because a pasted redirect URL never carries
		// a sigil and so cannot be mistaken for it.
		func(ctx context.Context, in *convo.In) convo.Verdict {
			if !isOpts(in.Text) {
				return convo.Pass
			}
			h.showPanel(ctx, metaOf(in).key, "")
			return convo.Handled
		},
		// `!archive` is this relay's own for the same reason `!opts` is:
		// it moves a TOPIC between Zulip channels, which is not a thing
		// poe-acp has. It arms exactly what the :wastebasket: reaction
		// arms — one destructive path, two ways to start it (archive.go).
		// Like `!opts` it runs ahead of the pending-login path: a pasted
		// redirect URL never carries a sigil.
		func(ctx context.Context, in *convo.In) convo.Verdict {
			if !isArchiveCommand(in.Text) {
				return convo.Pass
			}
			md := metaOf(in)
			h.archiveCommand(ctx, md.key, senderName(md.m))
			return convo.Handled
		},
		// `!update` (acp-kit/update) authorises by sender, which the broker
		// never sees, so it is dispatched here. The reply is posted BEFORE
		// the reload is triggered.
		func(ctx context.Context, in *convo.In) convo.Verdict {
			if h.cfg.Updater == nil || !update.IsCommand(in.Text) {
				return convo.Pass
			}
			md := metaOf(in)
			token := md.key.Token()
			res := h.cfg.Updater.Handle(ctx, update.Request{
				ConvID: token, Requester: strconv.FormatInt(md.m.SenderID, 10),
				Who: senderName(md.m), Text: in.Text,
				Post: func(s string) error { return h.PostTo(token, s) },
			})
			h.reply(ctx, md.key, res.Text)
			if res.After != nil {
				if err := res.After(); err != nil {
					h.cfg.Logf("handler: !update reload: %v", err)
					h.reply(ctx, md.key, fmt.Sprintf("❌ Reload failed: %v", err))
				}
			}
			return convo.Handled
		},
		// `!branch` is this relay's own for the third time and the same
		// reason: it CREATES a Zulip topic, which poe-acp has no analogue
		// for. Like `!opts` and `!archive` it runs ahead of the
		// pending-login path — a pasted redirect URL never carries a sigil
		// — and, like them, the origin agent never sees the message.
		func(ctx context.Context, in *convo.In) convo.Verdict {
			arg, ok := isBranchCommand(in.Text)
			if !ok {
				return convo.Pass
			}
			md := metaOf(in)
			h.branchCommand(ctx, md.m, md.key, arg)
			return convo.Handled
		},
		h.modelRules,
	}
}

// modelRules is the relay's `!model` handling ahead of the broker.
//
// Every `!model` shape needs a model list, so an EMPTY catalogue is
// answered here, once, before the shapes are told apart. Without this,
// bare `!model` and `!model <filter>` both fall through to the broker's
// catalogue prose, whose empty-list line can only say "connect a
// provider with `!login`" — it has no way to know whether the agent has
// even been asked yet. This relay does (see emptyModelNote), and one
// wording serves both the panel and this reply so they cannot disagree.
//
// Like `!opts`, all of this runs ahead of the pending-login path and for
// the same reason: a pasted redirect URL never carries a sigil, so a
// sigil-prefixed `!model` mid-login is plainly a question about models
// and not a malformed paste. The login stays pending for the paste that
// follows.
func (h *Handler) modelRules(ctx context.Context, in *convo.In) convo.Verdict {
	md := metaOf(in)
	text := in.Text
	if isModelCommand(text) {
		if models, _ := h.models(); len(models) == 0 {
			h.reply(ctx, md.key, h.emptyModelNote())
			return convo.Handled
		}
	}
	// A knob CHANGE is applied here rather than being rendered as
	// prose: it goes through the broker's exported action exactly as
	// `!model` does, but is acknowledged with a reaction and a
	// repainted panel instead of a new message. Settings chatter
	// belongs in a reaction, not in the topic.
	//
	// Only an exact model id qualifies; a filter or a bare `!model`
	// is a listing and falls through to the broker. A change that
	// FAILS also falls through, so the user hears why — deliberately
	// NOT into the filter branch below: an exact id is a change
	// request, and answering a failed change with a menu would swallow
	// the reason it failed.
	if id, ok := h.modelKnob(text); ok {
		if h.applyModelKnob(ctx, md.key, md.m.ID, id) {
			return convo.Handled
		}
		return convo.Pass
	}
	// `!model <filter>` — an argument that is NOT an exact id — is a
	// narrowing QUERY, and the answer is the control PAIR with its
	// choice list narrowed: the same `!opts` surface, the same single
	// live poll, filtered. The broker's prose answer left the user
	// retyping an exact id by thumb, which is the exact problem the poll
	// exists to solve, in the one place the panel's own "…and N more —
	// `!model <filter>`" line sends them.
	//
	// Bare `!model` is NOT this: it keeps the broker's catalogue prose.
	// See modelFilter for why. A filter matching nothing falls through,
	// so the broker says "(none match …)" and the live pair is left
	// alone. Replacing a working control with an empty poll to answer a
	// typo is the wrong trade.
	if filter, ok := modelFilter(text); ok && h.showFilteredPair(ctx, md.key, filter) {
		return convo.Handled
	}
	return convo.Pass
}

// afterFilters run after the broker and the passthrough rewrite.
//
// Sigil-prefixed, command-shaped, and nothing recognised it. The panel
// IS the answer: an unknown command is the moment a user is most in
// need of the menu, and a failure that teaches is worth more than a
// line of apology. Nothing is forwarded to the agent — a typo must not
// burn a turn, and config chatter must not enter the transcript the
// model reads.
func (h *Handler) afterFilters() []convo.Filter {
	return []convo.Filter{func(ctx context.Context, in *convo.In) convo.Verdict {
		name, ok := unknownCommand(in.Text)
		if !ok {
			return convo.Pass
		}
		note := fmt.Sprintf("Unknown command `%s%s` — here is what this relay can do. Send `%s%s` to say it as text.",
			command.DisplaySigil, name, doubleSigil, name)
		h.showPanel(ctx, metaOf(in).key, note)
		return convo.Handled
	}}
}

// decorate appends the relay's own commands to a broker-rendered
// reply.
//
// `!help` is composed in acp-kit, which cannot know about `!opts`: the
// panel is a Zulip-only surface and poe-acp has no widgets to render.
// Rather than fork the shared help text, the one extra line is added
// on the way out, here, where the Zulip-specific knowledge lives.
func (h *Handler) decorate(text, out string) string {
	body, ok := command.StripSigil(strings.TrimSpace(text))
	if !ok || !strings.EqualFold(strings.TrimSpace(body), "help") || out == "" {
		return out
	}
	extra := optsHelpLine + branchHelp
	if h.archiveEnabled() {
		// Advertised only when it can actually work: a help entry for
		// a command that answers "not configured here" teaches the
		// wrong thing.
		extra += archiveHelp
	}
	// Inserted right after the `!help` bullet rather than appended:
	// the broker's help ends with an optional "Agent commands:"
	// section, and a relay command filed under that heading would be a
	// plain lie about who runs it.
	const anchor = "- `" + command.DisplaySigil + "help`"
	i := strings.Index(out, anchor)
	if i < 0 {
		return strings.TrimRight(out, "\n") + "\n" + extra
	}
	j := strings.IndexByte(out[i:], '\n')
	if j < 0 {
		return strings.TrimRight(out, "\n") + "\n" + extra
	}
	cut := i + j + 1
	return out[:cut] + extra + out[cut:]
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

// The Controller itself is acp-kit's shared convo.Manager (h.convo):
// the sticky model table, validation, `!stop` and the status snapshots
// live there. What follows are this relay's hooks into it, and the
// Handler's own Controller methods, which delegate so every existing
// caller (the options panel, the loopback tools) keeps one handle.

// newConvo builds the Handler's convo Manager and wires it into the
// broker as its Controller.
func (h *Handler) newConvo() (*convo.Manager, error) {
	cfg := h.cfg
	if cfg.Commands != nil && cfg.Updater != nil {
		cfg.Commands.AddHelp(update.HelpLine)
	}
	return convo.New(convo.Config{
		Agent:      zulipAgent{h},
		Sessions:   sessionsLen{cfg.Sessions, cfg.Journal},
		Broker:     cfg.Commands,
		NoCommands: cfg.Commands == nil,
		Poster:     h,
		Scheduler:  h,
		Mode:       convo.Supersede,
		Liveness: convo.ProgressClock{
			NoProgressTimeout: cfg.NoProgressTimeout,
			MaxTurnDuration:   cfg.TurnCeiling,
		},
		Sink:    convo.SinkFunc(h.replySink),
		Before:  h.beforeFilters(),
		After:   h.afterFilters(),
		Logf:    cfg.Logf,
		OnError: func(conv string, err error) { cfg.Logf("handler: turn for %s failed: %v", conv, err) },
		Version: cfg.Version, AgentCmd: cfg.AgentCmd, StartTime: cfg.StartTime, Now: cfg.Now,
		Hooks: convo.Hooks{
			Resolve:      h.resolve,
			Reset:        func(_ context.Context, token string) error { return h.resetSession(token) },
			Status:       h.decorateStatus,
			RelayInfo:    h.decorateRelayInfo,
			ModelChanged: func(token, _, _ string) { h.refreshPanel(context.Background(), mustKey(token)) },
			Decorate:     h.decorate,
		},
	})
}

// zulipAgent routes the Manager's model reads through h.models, which
// records that a model list has been seen (see emptyModelNote).
type zulipAgent struct{ h *Handler }

func (a zulipAgent) Models() ([]client.ModelInfo, string) { return a.h.models() }
func (a zulipAgent) AvailableCommands() []client.CommandInfo {
	return a.h.cfg.Agent.AvailableCommands()
}
func (a zulipAgent) SetModel(ctx context.Context, sid acp.SessionId, id string) error {
	return a.h.cfg.Agent.SetModel(ctx, sid, id)
}
func (a zulipAgent) SessionStats(sid acp.SessionId) (client.SessionStats, bool) {
	return a.h.cfg.Agent.SessionStats(sid)
}

// sessionsLen gives the session manager the Len convo.Sessions wants:
// on Zulip the count `!status` reports is the journal's active
// conversations, not live ACP sessions.
type sessionsLen struct {
	Sessions
	j *journal.Journal
}

func (s sessionsLen) Len() int { return s.j.ActiveCount() }

// resolve maps a broker token to the conversation's id. This relay
// passes the KEY's token, never the conv-id: `!new` replaces the
// conv-id, so a broker holding one would be holding a stale identity.
func (h *Handler) resolve(token string) (string, bool) {
	_, conv, ok := h.convFor(token)
	return conv.ID, ok
}

func (h *Handler) ctl() command.Controller { return h.convo.Controller() }

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
func (h *Handler) StatusFor(token string) command.SessionStatus { return h.ctl().StatusFor(token) }

// decorateStatus adds what only Zulip knows: where the conversation is,
// its id and directory, and that on Zulip "has a session" means the
// topic is engaged (the ACP session behind it may have been reaped).
func (h *Handler) decorateStatus(token, convID string, engaged bool, st command.SessionStatus) command.SessionStatus {
	key, _ := journal.ParseToken(token)
	st.Where = h.whereFor(key)
	st.HasSession = engaged
	if engaged {
		st.ConvID = convID
		st.StateDir = filepath.Join(h.cfg.Sessions.StateDir(), convsDir, convID)
	}
	return st
}

// RelayInfo satisfies command.Controller.
func (h *Handler) RelayInfo(token string) command.RelayInfo { return h.ctl().RelayInfo(token) }

func (h *Handler) decorateRelayInfo(_, convID string, engaged bool, ri command.RelayInfo) command.RelayInfo {
	if ai := h.cfg.Agent.AgentInfo(); ai.Name != "" {
		ri.AgentName, ri.AgentVersion = ai.Name, ai.Version
	} else if h.cfg.AgentVersion != nil {
		ri.AgentName = h.cfg.AgentVersion()
	}
	ri.SessionID, ri.EffectiveModel = "", ""
	if engaged {
		ri.SessionID = convID
	}
	return ri
}

// SetModelOverride satisfies command.Controller. The choice is sticky
// per conversation and applied to the ACP session at the start of the
// next turn — see convo.Manager.ApplyModel. Every model change passes
// through here — typed, tapped, or made by the agent through its
// loopback tool — so the ModelChanged hook is the one place that keeps
// the options panel honest.
func (h *Handler) SetModelOverride(token, modelID string) error {
	return h.ctl().SetModelOverride(token, modelID)
}

// ResetSession satisfies command.Controller: it is what `!new` calls.
func (h *Handler) ResetSession(token string) error { return h.ctl().ResetSession(token) }

// resetSession is the Reset hook. On Zulip this RETIRES the journal
// entry and allocates a fresh conv-id. The old state/convs/<id>/
// directory is left exactly where it is — retiring a conversation is
// not deleting work. The broker never learns the id changed, which is
// precisely why it is handed a key token rather than a conv-id.
func (h *Handler) resetSession(token string) error {
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
	_ = h.convo.Overrides().Carry(prev.ID, fresh.ID)
	// The retired conversation owns no message any more. Retire has
	// already cleared the persisted record; this drops the in-memory
	// one, which would otherwise outlive the conversation it names.
	h.forgetOwn(prev.ID)
	h.cfg.Logf("handler: %s retired for fresh conversation %s in %s", prev.ID, fresh.ID, h.describe(key))
	return nil
}

// StopTurn satisfies command.TurnStopper, which is what enables
// `!stop`. poe-acp deliberately does not implement it: it answers one
// HTTP request per turn and has no in-flight turn a later message
// could reach. This relay streams into an editable message and does.
func (h *Handler) StopTurn(token string) bool {
	return h.ctl().(command.TurnStopper).StopTurn(token)
}
