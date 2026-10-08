package handler

// The emoji model switcher.
//
// A short, operator-chosen list of models, each bound to one emoji, so
// a model change on a phone is one or two taps and costs no tokens:
//
//   - the menu emoji (default :gear:) on any relay message posts a menu
//     and pre-adds every shortlist emoji to it, so the user taps once;
//   - a shortlist emoji on a relay message switches THIS conversation
//     from its next turn — the same SetModelOverride path `!model`
//     takes — and posts a confirmation with the sweep emoji pre-added;
//   - the sweep emoji (default :www:) on that confirmation applies the
//     same model to every conversation and makes it the default for new
//     ones. An idle session takes it on its next turn, a running one
//     after its current turn ends. Nothing is cancelled.
//
// The relay does all of it. None of these reactions reach the agent,
// and a removal is consumed and does nothing: a bot cannot put a
// reaction back, so un-tapping is not a request.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/kfet/acp-kit/convo"
	"github.com/kfet/zulip-acp/internal/journal"
	"github.com/kfet/zulip-acp/internal/zulipproto"
)

// QuickModel is one shortlist entry.
type QuickModel struct {
	Emoji string
	Model string
	Label string
}

// quickDefaultFile holds the model a sweep made the default for new
// conversations, under the session state directory.
const quickDefaultFile = "quick_default_model"

func (q QuickModel) label() string {
	if q.Label != "" {
		return q.Label
	}
	return q.Model
}

// quickEntry returns the shortlist entry bound to emoji.
func (h *Handler) quickEntry(emoji string) (QuickModel, bool) {
	for _, q := range h.cfg.QuickModels {
		if q.Emoji == emoji {
			return q, true
		}
	}
	return QuickModel{}, false
}

// quickHelp is the `!help` line for the switcher, or "" when it is off.
func (h *Handler) quickHelp() string {
	if len(h.cfg.QuickModels) == 0 {
		return ""
	}
	var em []string
	for _, q := range h.cfg.QuickModels {
		em = append(em, ":"+q.Emoji+":")
	}
	return fmt.Sprintf("- react :%s: on a relay message — model switcher menu; %s switches this topic, then :%s: on the confirm applies it to all sessions\n",
		h.cfg.QuickMenuEmoji, strings.Join(em, " "), h.cfg.QuickSweepEmoji)
}

// isQuickEmoji reports whether emoji is one of the switcher's.
func (h *Handler) isQuickEmoji(emoji string) bool {
	if len(h.cfg.QuickModels) == 0 {
		return false
	}
	_, ok := h.quickEntry(emoji)
	return ok || emoji == h.cfg.QuickMenuEmoji || emoji == h.cfg.QuickSweepEmoji
}

// quickReaction is the switcher's reaction hook. It reports whether it
// consumed the reaction. It only acts on the relay's own messages: m is
// nil when the message was resolved from the relay's own records, and
// set when it had to be fetched, in which case its sender decides.
func (h *Handler) quickReaction(ctx context.Context, conv journal.Conv, ev zulipproto.Event, m *zulipproto.Message) bool {
	if len(h.cfg.QuickModels) == 0 {
		return false
	}
	if m != nil && m.SenderID != h.cfg.BotUserID {
		return false
	}
	add := ev.Op == zulipproto.ReactionAdd
	switch name := ev.EmojiName; {
	case name == h.cfg.QuickMenuEmoji:
		if add {
			h.postQuickMenu(ctx, conv)
		}
		return true
	case name == h.cfg.QuickSweepEmoji:
		// Always consumed on the relay's own messages: the sweep emoji
		// is a control and must never reach the agent, even when the
		// confirmation is not known (any more — the index is in memory).
		if !add {
			return true
		}
		if model, ok := h.quickConfirms.get(ev.MessageID); ok {
			h.quickSweep(ctx, conv, model)
			return true
		}
		h.reply(ctx, conv.Key, fmt.Sprintf(":%s: applies a model to all sessions only on a switch confirmation, and this is none (or it is from before a restart). React :%s: to pick a model again.",
			h.cfg.QuickSweepEmoji, h.cfg.QuickMenuEmoji))
		return true
	default:
		q, ok := h.quickEntry(name)
		if !ok {
			return false
		}
		if add {
			h.quickSwitch(ctx, conv, q)
		}
		return true
	}
}

// postQuick posts body into conv, records it as the relay's own and
// pre-adds emojis to it. It returns the message id, or 0 on failure.
func (h *Handler) postQuick(ctx context.Context, conv journal.Conv, body string, emojis ...string) int64 {
	post := &convPoster{client: h.cfg.Client, key: conv.Key}
	id, err := post.Post(ctx, body)
	if err != nil {
		h.cfg.Logf("handler: posting the model switcher in %s: %v", h.describe(conv.Key), err)
		return 0
	}
	h.rememberOwn(conv.ID, id)
	for _, e := range emojis {
		if err := h.cfg.Client.AddReaction(ctx, id, e); err != nil {
			h.cfg.Logf("handler: adding :%s: to the model switcher in %s: %v", e, h.describe(conv.Key), err)
		}
	}
	return id
}

// renderQuickMenu is the menu text for a conversation whose effective
// model is current. An entry the agent does not offer is marked, so
// the user sees why a tap on it will be refused.
func (h *Handler) renderQuickMenu(current string) string {
	models, _ := h.models()
	var sb strings.Builder
	now := "`" + current + "`"
	for _, q := range h.cfg.QuickModels {
		if q.Model == current {
			now = ":" + q.Emoji + ": " + q.label()
			break
		}
	}
	if current == "" {
		now = "agent default"
	}
	fmt.Fprintf(&sb, "**Switch model** · now: %s", now)
	for _, q := range h.cfg.QuickModels {
		fmt.Fprintf(&sb, "\n:%s: %s", q.Emoji, q.label())
		if convo.ValidateModel(models, q.Model) != nil {
			sb.WriteString(" *(not offered by the agent)*")
			h.cfg.Logf("handler: WARN quick_models :%s: names %q, which the agent does not offer", q.Emoji, q.Model)
		}
	}
	fmt.Fprintf(&sb, "\n*Tap once: this topic. Tap :%s: on the confirm: all sessions.*", h.cfg.QuickSweepEmoji)
	return sb.String()
}

func (h *Handler) postQuickMenu(ctx context.Context, conv journal.Conv) {
	emojis := make([]string, 0, len(h.cfg.QuickModels))
	for _, q := range h.cfg.QuickModels {
		emojis = append(emojis, q.Emoji)
	}
	h.postQuick(ctx, conv, h.renderQuickMenu(h.convo.EffectiveModel(conv.ID)), emojis...)
}

// quickSwitch switches one conversation, through the same controller
// action `!model` uses, and posts the confirmation the sweep hangs off.
func (h *Handler) quickSwitch(ctx context.Context, conv journal.Conv, q QuickModel) {
	if err := h.SetModelOverride(conv.Key.Token(), q.Model); err != nil {
		h.reply(ctx, conv.Key, fmt.Sprintf(":%s: cannot switch to **%s**: %v", q.Emoji, q.label(), err))
		return
	}
	h.cfg.Logf("handler: :%s: switched %s to %s", q.Emoji, conv.ID, q.Model)
	body := fmt.Sprintf(":%s: → **%s** for this topic, from the next turn.\n*Tap :%s: to apply to all sessions.*",
		q.Emoji, q.label(), h.cfg.QuickSweepEmoji)
	if id := h.postQuick(ctx, conv, body, h.cfg.QuickSweepEmoji); id != 0 {
		h.quickConfirms.put(id, q.Model)
	}
}

// quickSweep applies model to every live conversation and makes it the
// default for new ones.
//
// The overrides are applied lazily at the start of each conversation's
// next turn (convo.Manager.ApplyModel), which is exactly the wanted
// behaviour: an idle session switches on its next turn and a running
// one after its current turn ends. Nothing is cancelled. Only THIS
// conversation goes through SetModelOverride; the others are set
// directly, because that path repaints the options panel and a sweep
// must not re-post a panel into every topic that has one.
func (h *Handler) quickSweep(ctx context.Context, conv journal.Conv, model string) {
	if err := h.SetModelOverride(conv.Key.Token(), model); err != nil {
		h.reply(ctx, conv.Key, fmt.Sprintf("Cannot apply to all sessions: %v", err))
		return
	}
	n := 1
	for _, c := range h.cfg.Journal.Convs() {
		if c.Retired || c.ID == conv.ID {
			continue
		}
		// Set keeps the choice in memory even when persisting it
		// fails, and convo already logs that failure.
		_ = h.convo.Overrides().Set(c.ID, model)
		n++
	}
	if err := h.setDefaultModel(model); err != nil {
		h.cfg.Logf("handler: persisting the default model: %v", err)
	}
	label := model
	emoji := h.cfg.QuickSweepEmoji
	for _, q := range h.cfg.QuickModels {
		if q.Model == model {
			label, emoji = q.label(), q.Emoji
			break
		}
	}
	h.cfg.Logf("handler: swept %d conversation(s) to %s", n, model)
	h.reply(ctx, conv.Key, fmt.Sprintf(":%s: → **%s** for all %d session(s) and new topics; a running turn switches when it ends.", emoji, label, n))
}

// defaultModelPath is where the swept default is kept.
func (h *Handler) defaultModelPath() string {
	return filepath.Join(h.cfg.Sessions.StateDir(), quickDefaultFile)
}

// defaultModel returns the swept default, or "" when there is none.
func (h *Handler) defaultModel() string {
	b, err := os.ReadFile(h.defaultModelPath())
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			h.cfg.Logf("handler: reading the default model: %v", err)
		}
		return ""
	}
	return strings.TrimSpace(string(b))
}

func (h *Handler) setDefaultModel(model string) error {
	tmp := h.defaultModelPath() + ".tmp"
	if err := os.WriteFile(tmp, []byte(model+"\n"), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, h.defaultModelPath())
}

// applyDefaultModel gives a conversation with no model choice of its
// own the swept default. It runs at the start of each turn, before the
// override is pushed to the session, so a new topic starts on it.
func (h *Handler) applyDefaultModel(convID string) {
	if len(h.cfg.QuickModels) == 0 {
		return
	}
	if _, ok := h.modelOverride(convID); ok {
		return
	}
	def := h.defaultModel()
	if def == "" {
		return
	}
	// The agent may have stopped offering it since the sweep. A dead
	// override would fail on every turn of every new topic.
	models, _ := h.models()
	if err := convo.ValidateModel(models, def); err != nil {
		h.cfg.Logf("handler: WARN the swept default model is not usable: %v", err)
		return
	}
	_ = h.convo.Overrides().Set(convID, def)
}
