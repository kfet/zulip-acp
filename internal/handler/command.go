// This file is the Zulip half of the relay's `!command` surface: the
// concrete command list, the dispatch switch, and the Zulip-markdown
// rendering. The grammar — what counts as a command at all — lives in
// internal/command, which knows nothing about Zulip.
//
// Three rules hold for every command here:
//
//  1. A command is handled entirely by the relay and NEVER reaches the
//     agent. It consumes no turn, so it costs nothing and works even
//     while the agent is wedged.
//  2. A command never ALLOCATES a conversation. Dispatch runs off
//     Journal.Lookup, before Ensure, so `!help` in a topic the relay
//     has never answered in leaves no state behind.
//  3. Gating is identical to a prompt's. The bot-own-message and
//     system-bot guards run first, then `allowed_user_ids`, and only
//     then is anything parsed as a command. In a channel a command is
//     honoured exactly when a prompt would be: the message mentions
//     the bot, or the topic is already engaged. Anything else in a
//     channel the relay was never summoned to is none of its business
//     — including `!help`.
//
// # Serialised delivery
//
// Commands read the journal (Lookup) and then act on what they read,
// so they assume Handle is called from ONE goroutine — which it is:
// zulipproto's /events runner is a single long-poll loop. Nothing
// enforces it, so if a second runner is ever added, the Lookup→command
// window becomes a real TOCTOU. The commands that mutate are written
// to fail safe anyway (Retire reports existed=false rather than
// inventing a conversation), but the invariant is worth knowing before
// anyone parallelises the event side.
package handler

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	acp "github.com/coder/acp-go-sdk"

	"github.com/kfet/acp-kit/client"
	"github.com/kfet/zulip-acp/internal/command"
	"github.com/kfet/zulip-acp/internal/journal"
	"github.com/kfet/zulip-acp/internal/zulipproto"
)

// commandSet is the relay's command registry, in help order: read-only
// orientation first, then the two that change something.
//
// It is deliberately short. Every command here either cannot be
// expressed any other way (`!new` in a DM, whose key is the
// participant set and therefore fixed forever) or answers a question
// the chat surface cannot (which conv-id, which state dir, is
// something still running).
var commandSet = command.NewSet(
	command.Spec{
		Name:    "help",
		Summary: "list these commands",
	},
	command.Spec{
		Name:    "status",
		Summary: "where you are, which conversation, which model, and whether a turn is running",
	},
	command.Spec{
		Name:    "id",
		Summary: "the bare conversation id, for finding its state directory",
	},
	command.Spec{
		Name:    "model",
		Args:    "[id]",
		Summary: "list the agent's models, or switch this conversation to one",
	},
	command.Spec{
		Name:    "new",
		Aliases: []string{"reset"},
		Summary: "retire this conversation and start a fresh one",
	},
	command.Spec{
		Name:    "stop",
		Aliases: []string{"cancel"},
		Summary: "interrupt the turn currently running here",
	},
)

// convsDir is the state-manager subdirectory holding per-conversation
// working directories. It mirrors acp-kit's default cwd layout,
// <StateDir>/convs/<conv-id>, which the relay does not override.
const convsDir = "convs"

// dispatch classifies text and, when it names a command, handles it.
//
// It returns the prose to forward to the agent and whether the message
// was consumed. handled=true means the relay is done with this
// message: it is a command, or a command-shaped typo, and it must not
// reach the agent either way.
func (h *Handler) dispatch(ctx context.Context, m *zulipproto.Message, key journal.Key, conv journal.Conv, engaged bool, text string) (prompt string, handled bool) {
	res := commandSet.Parse(text)
	switch res.Kind {
	case command.KindProse:
		return res.Text, false
	case command.KindUnknown:
		h.reply(ctx, key, fmt.Sprintf("Unknown command `%s%s`. Send `%shelp` for the list, or `%s%s` to say it as text.",
			command.Sigil, res.Name, command.Sigil, command.Escape, res.Name))
		return "", true
	}
	h.cfg.Logf("handler: command %s%s in %s", command.Sigil, res.Name, h.describe(key))
	h.reply(ctx, key, h.runCommand(ctx, res, m, key, conv, engaged))
	return "", true
}

// runCommand executes one recognised command and returns the reply.
func (h *Handler) runCommand(ctx context.Context, res command.Result, m *zulipproto.Message, key journal.Key, conv journal.Conv, engaged bool) string {
	switch res.Name {
	case "help":
		return h.cmdHelp()
	case "status":
		return h.cmdStatus(m, key, conv, engaged)
	case "id":
		return h.cmdID(conv, engaged)
	case "model":
		return h.cmdModel(res.Args, conv, engaged)
	case "new":
		return h.cmdNew(ctx, key, conv, engaged)
	default:
		// "stop" — the switch is exhaustive over commandSet, which is
		// a compile-time-constant list, so there is no other arm to
		// reach and no unreachable-default branch to cover.
		return h.cmdStop(ctx, conv, engaged)
	}
}

// reply posts a command's answer where the command arrived — same
// topic, or same DM participant set.
//
// Commands are not turns: there is no placeholder, no streaming, no
// :eyes: lifecycle and no tail tracking, because none of that fits a
// reply that is already complete when it is composed. A reply that
// cannot be posted is logged and dropped; there is nowhere else to put
// it, and unlike an agent answer it costs nothing to re-request.
func (h *Handler) reply(ctx context.Context, key journal.Key, content string) {
	post := &convPoster{client: h.cfg.Client, key: key}
	if _, err := post.Post(ctx, content); err != nil {
		h.cfg.Logf("handler: posting command reply to %s: %v", h.describe(key), err)
	}
}

// --- individual commands -------------------------------------------------

func (h *Handler) cmdHelp() string {
	var sb strings.Builder
	sb.WriteString("**Relay commands** — handled here, never sent to the agent:\n\n")
	for _, sp := range commandSet.Specs() {
		fmt.Fprintf(&sb, "- `%s` — %s", sp.Usage(), sp.Summary)
		if len(sp.Aliases) > 0 {
			alias := make([]string, len(sp.Aliases))
			for i, a := range sp.Aliases {
				alias[i] = "`" + command.Sigil + a + "`"
			}
			fmt.Fprintf(&sb, " (also %s)", strings.Join(alias, ", "))
		}
		sb.WriteString("\n")
	}
	fmt.Fprintf(&sb, "\nAnything else reaches the agent unchanged. To say something that starts with `%s`, double it: `%simportant` arrives as `%simportant`.",
		command.Sigil, command.Escape, command.Sigil)
	return sb.String()
}

func (h *Handler) cmdStatus(m *zulipproto.Message, key journal.Key, conv journal.Conv, engaged bool) string {
	var sb strings.Builder
	sb.WriteString("**Status**\n\n")
	fmt.Fprintf(&sb, "- here: %s\n", h.human(m, key))
	if !engaged {
		fmt.Fprintf(&sb, "- conversation: none yet — your next message starts one\n")
	} else {
		fmt.Fprintf(&sb, "- conversation: `%s`\n", conv.ID)
		fmt.Fprintf(&sb, "- state dir: `%s`\n", filepath.Join(h.cfg.Sessions.StateDir(), convsDir, conv.ID))
	}
	fmt.Fprintf(&sb, "- model: %s\n", h.modelLabel(conv, engaged))
	running := "no"
	if engaged && h.isInflight(conv.ID) {
		running = fmt.Sprintf("yes — `%sstop` interrupts it", command.Sigil)
	}
	fmt.Fprintf(&sb, "- turn running: %s\n", running)
	return sb.String()
}

// human renders the conversation key the way a person would say it.
// In a channel that is the channel name and topic; in a DM it is the
// participant names, taken from the message rather than the key
// because the key holds ids and ids are not human terms.
func (h *Handler) human(m *zulipproto.Message, key journal.Key) string {
	if !key.IsDM() {
		return h.describe(key)
	}
	if names := m.RecipientNames(); len(names) > 0 {
		return "DM with " + strings.Join(names, ", ")
	}
	return key.Label()
}

// modelLabel renders the effective model for a conversation: the
// sticky choice made with `!model <id>` if there is one, otherwise
// whatever the agent last reported as current.
func (h *Handler) modelLabel(conv journal.Conv, engaged bool) string {
	if engaged {
		if id, ok := h.modelOverride(conv.ID); ok {
			return fmt.Sprintf("`%s` (set with `%smodel`)", id, command.Sigil)
		}
	}
	if _, current := h.cfg.Agent.Models(); current != "" {
		return "`" + current + "`"
	}
	return "not reported yet — the agent publishes it once a session exists"
}

func (h *Handler) cmdID(conv journal.Conv, engaged bool) string {
	if !engaged {
		return "No conversation here yet — your next message starts one."
	}
	return "`" + conv.ID + "`"
}

// cmdNew retires the conversation and allocates a fresh conv-id.
//
// The old state/convs/<id>/ directory is left exactly where it is:
// retiring a conversation is not deleting work, and the id in the
// reply is what makes the old directory findable afterwards.
//
// A turn still running in the retired conversation is cancelled first.
// Leaving it alive would let it keep streaming into a conversation the
// user has just declared finished.
func (h *Handler) cmdNew(ctx context.Context, key journal.Key, conv journal.Conv, engaged bool) string {
	if !engaged {
		return "Nothing to retire — there is no conversation here yet, so your next message already starts a fresh one."
	}
	h.cancelInflight(ctx, conv.ID)
	prev, fresh, existed, err := h.cfg.Journal.Retire(key)
	if err != nil {
		h.cfg.Logf("handler: retiring conversation %s: %v", conv.ID, err)
		return fmt.Sprintf("Couldn't start a new conversation: %v", err)
	}
	if !existed {
		// Raced with a rename or another !new between Lookup and
		// Retire. Nothing was retired, and the key already resolves
		// to something fresh enough.
		return "Nothing to retire — there is no conversation here any more, so your next message already starts a fresh one."
	}
	// The model choice is the user's, not the conversation's: carry it
	// across so `!new` clears context without silently reverting it.
	h.carryModelOverride(prev.ID, fresh.ID)
	return fmt.Sprintf("🧹 Fresh conversation `%s` — your next message starts it with no history.\n\n"+
		"The old conversation `%s` is retired, not deleted; its files are still in `%s`.",
		fresh.ID, prev.ID, filepath.Join(h.cfg.Sessions.StateDir(), convsDir, prev.ID))
}

func (h *Handler) cmdStop(ctx context.Context, conv journal.Conv, engaged bool) string {
	if !engaged || !h.cancelInflight(ctx, conv.ID) {
		return "Nothing is running here."
	}
	return "🛑 Interrupted."
}

// modelsListCap bounds how many models one reply prints. A relay with
// a broad provider list can advertise hundreds, and a wall of ids is
// unreadable on a phone — and would hit the 10k rollover for no gain.
const modelsListCap = 40

// cmdModel shows the agent's model list, or makes a sticky per-
// conversation choice.
//
// The switch is recorded here and applied to the ACP session at the
// start of the next turn (see applyModel). It is deliberately NOT
// applied immediately: doing so would need a live session, and calling
// GetOrCreate outside a turn would spawn one — and re-register a sink
// — as a side effect of a read-ish command.
//
// Only an exact id switches. A near-match is treated as a filter, so
// mistyping an id lists candidates instead of silently picking one.
func (h *Handler) cmdModel(arg string, conv journal.Conv, engaged bool) string {
	models, current := h.cfg.Agent.Models()
	if len(models) == 0 {
		return "The agent has not reported its models yet — send it a message first, then ask again."
	}
	if arg == "" {
		return renderModels(models, current, "", h.effectiveOverride(conv, engaged))
	}
	for _, m := range models {
		if m.ID == arg {
			if !engaged {
				return fmt.Sprintf("There is no conversation here yet to set a model on. Send a message first, then `%smodel %s`.", command.Sigil, arg)
			}
			h.setModelOverride(conv.ID, arg)
			return fmt.Sprintf("✅ Model set to `%s` for this conversation — it applies from your next message.", arg)
		}
	}
	return renderModels(models, current, arg, h.effectiveOverride(conv, engaged))
}

// effectiveOverride returns the conversation's sticky model, if any.
func (h *Handler) effectiveOverride(conv journal.Conv, engaged bool) string {
	if !engaged {
		return ""
	}
	id, _ := h.modelOverride(conv.ID)
	return id
}

// renderModels lists models, optionally narrowed by a substring.
func renderModels(models []client.ModelInfo, current, filter, override string) string {
	var sb strings.Builder
	marked := override
	if marked == "" {
		marked = current
	}
	matched := models[:0:0]
	lower := strings.ToLower(filter)
	for _, m := range models {
		if lower == "" || strings.Contains(strings.ToLower(m.ID), lower) {
			matched = append(matched, m)
		}
	}
	if filter == "" {
		fmt.Fprintf(&sb, "**%d models** — `%smodel <id>` switches this conversation:\n\n", len(models), command.Sigil)
	} else if len(matched) == 0 {
		return fmt.Sprintf("No model id matches %q. Send `%smodel` for the full list of %d.", filter, command.Sigil, len(models))
	} else {
		fmt.Fprintf(&sb, "**%d of %d models** match %q:\n\n", len(matched), len(models), filter)
	}
	for i, m := range matched {
		if i >= modelsListCap {
			fmt.Fprintf(&sb, "- …and %d more — narrow it with `%smodel <substring>`.\n", len(matched)-modelsListCap, command.Sigil)
			break
		}
		suffix := ""
		if m.ID == marked {
			suffix = " ← current"
		}
		fmt.Fprintf(&sb, "- `%s`%s\n", m.ID, suffix)
	}
	return sb.String()
}

// --- sticky model overrides ----------------------------------------------

// modelChoice is a conversation's sticky model and the session it was
// last pushed to. Recording the session id is what makes the choice
// survive idle GC: a conversation whose session was reaped comes back
// with a new session id, and the mismatch is the signal to re-apply.
type modelChoice struct {
	id      string
	applied acp.SessionId
}

func (h *Handler) modelOverride(convID string) (string, bool) {
	h.modelMu.Lock()
	defer h.modelMu.Unlock()
	c, ok := h.modelChoices[convID]
	if !ok {
		return "", false
	}
	return c.id, true
}

func (h *Handler) setModelOverride(convID, modelID string) {
	h.modelMu.Lock()
	defer h.modelMu.Unlock()
	h.modelChoices[convID] = modelChoice{id: modelID}
}

// carryModelOverride moves a sticky choice from a retired conversation
// to the fresh one that replaced it, dropping the applied marker so
// the new session gets its own push.
func (h *Handler) carryModelOverride(from, to string) {
	h.modelMu.Lock()
	defer h.modelMu.Unlock()
	if c, ok := h.modelChoices[from]; ok {
		delete(h.modelChoices, from)
		h.modelChoices[to] = modelChoice{id: c.id}
	}
}

// applyModel pushes a conversation's sticky model to its ACP session,
// at most once per session.
//
// A failure is logged and the turn continues: the agent answers on
// whatever model it already had, which is a far better outcome than
// refusing to answer at all over a preference.
func (h *Handler) applyModel(ctx context.Context, convID string, sid acp.SessionId) {
	h.modelMu.Lock()
	c, ok := h.modelChoices[convID]
	if !ok || c.applied == sid {
		h.modelMu.Unlock()
		return
	}
	h.modelMu.Unlock()

	if err := h.cfg.Agent.SetModel(ctx, sid, c.id); err != nil {
		h.cfg.Logf("handler: selecting model %s for %s: %v", c.id, convID, err)
		return
	}
	h.modelMu.Lock()
	if cur, still := h.modelChoices[convID]; still && cur.id == c.id {
		h.modelChoices[convID] = modelChoice{id: c.id, applied: sid}
	}
	h.modelMu.Unlock()
}
