package handler

// Session settings: per-conversation knobs that the agent exposes as
// ACP session config options, such as `!think`.
//
// A setting is applied the way `!model` is applied. The choice is
// sticky per conversation and is pushed to the ACP session at the start
// of the next turn, at most once per session (convo.Overrides keeps the
// "applied to which session" marker). fir applies a config option to
// the live session in place: the session, its transcript and the
// provider's prompt cache stay as they are. A session that is reaped
// and resumed gets the choice again, because its id changes.
//
// To add a knob, add one row to sessionSettings. A row maps a command
// to the agent's config option id and the values the relay accepts.

import (
	"context"
	"fmt"
	"slices"
	"strings"

	acp "github.com/coder/acp-go-sdk"
	"github.com/kfet/acp-kit/command"
	"github.com/kfet/acp-kit/convo"
	"github.com/kfet/zulip-acp/internal/journal"
	"github.com/kfet/zulip-acp/internal/zulipproto"
)

// sessionSetting is one row of the settings table.
type sessionSetting struct {
	// verb is the command name, without the sigil.
	verb string
	// configID is the agent's ACP config option id.
	configID string
	// label is the name the panel and the replies show.
	label string
	// values are the accepted values, in display order.
	values []string
	// current reads the value the agent reports for a live session.
	// Empty means the agent does not report one.
	current func(h *Handler, sid acp.SessionId) string
}

// thinkVerb is the `!think` command.
const thinkVerb = "think"

// sessionSettings is the table of session knobs.
var sessionSettings = []sessionSetting{
	{
		verb:     thinkVerb,
		configID: "thinking_level",
		label:    "Thinking",
		values:   []string{"off", "minimal", "low", "medium", "high"},
		current: func(h *Handler, sid acp.SessionId) string {
			st, _ := h.cfg.Agent.SessionStats(sid)
			return st.Thinking
		},
	},
}

// settingsHelp is the `!help` text for the settings table.
func settingsHelp() string {
	var sb strings.Builder
	for _, s := range sessionSettings {
		fmt.Fprintf(&sb, "- `%s%s [%s]` — %s level for this conversation (no argument shows it)\n",
			command.DisplaySigil, s.verb, strings.Join(s.values, "|"), strings.ToLower(s.label))
	}
	return sb.String()
}

// settingStore returns the sticky store for setting i.
func (h *Handler) settingStore(i int) *convo.Overrides {
	h.settingsOnce.Do(func() {
		h.settings = make([]*convo.Overrides, len(sessionSettings))
		for j := range h.settings {
			h.settings[j] = mustOverrides(convo.NewOverrides(nil))
		}
	})
	return h.settings[i]
}

// parseSetting reports whether text is a settings command, with the
// row index and the trimmed argument.
func parseSetting(text string) (int, string, bool) {
	body, ok := command.StripSigil(strings.TrimSpace(text))
	if !ok {
		return 0, "", false
	}
	verb, arg, _ := strings.Cut(body, " ")
	for i, s := range sessionSettings {
		if strings.EqualFold(verb, s.verb) {
			return i, strings.ToLower(strings.TrimSpace(arg)), true
		}
	}
	return 0, "", false
}

// settingsFilter is the before-filter for every row of the table.
func (h *Handler) settingsFilter(ctx context.Context, in *convo.In) convo.Verdict {
	i, arg, ok := parseSetting(in.Text)
	if !ok {
		return convo.Pass
	}
	md := metaOf(in)
	h.reply(ctx, md.key, h.settingCommand(ctx, md.key, md.m.ID, i, arg))
	return convo.Handled
}

// settingCommand runs one settings command and returns the reply. An
// accepted change returns "" and is acknowledged with a reaction, the
// same as a `!model` change.
func (h *Handler) settingCommand(ctx context.Context, key journal.Key, msgID int64, i int, arg string) string {
	s := sessionSettings[i]
	conv, engaged := h.cfg.Journal.Lookup(key)
	if arg == "" {
		cur := h.settingValue(i, conv.ID, engaged)
		if cur == "" {
			cur = "agent default"
		}
		return fmt.Sprintf("%s: `%s` · options: `%s`", s.label, cur, strings.Join(s.values, "`, `"))
	}
	if !slices.Contains(s.values, arg) {
		return fmt.Sprintf("Unknown %s level `%s`. Use one of: `%s`.", strings.ToLower(s.label), arg, strings.Join(s.values, "`, `"))
	}
	if !engaged {
		return "There is no conversation here yet — " + startHint(key) + " first."
	}
	if sid, _, live := h.cfg.Sessions.Live(conv.ID); live && s.current(h, sid) == "" {
		return fmt.Sprintf("The agent does not report a %s level, so the relay cannot set one.", strings.ToLower(s.label))
	}
	_ = h.settingStore(i).Set(conv.ID, arg)
	h.cfg.Logf("handler: %s set to %s for %s", s.verb, arg, conv.ID)
	h.reactOnce(ctx, msgID, "check")
	h.refreshPanel(ctx, key)
	return ""
}

// settingValue is the value of setting i for a conversation: the
// sticky choice, else what its live session reports, else "".
func (h *Handler) settingValue(i int, convID string, engaged bool) string {
	if !engaged {
		return ""
	}
	if v, ok := h.settingStore(i).Get(convID); ok {
		return v
	}
	if sid, _, live := h.cfg.Sessions.Live(convID); live {
		return sessionSettings[i].current(h, sid)
	}
	return ""
}

// applySettings pushes every pending sticky setting to session sid. A
// failure is logged and the turn continues, the same as for a model.
func (h *Handler) applySettings(ctx context.Context, convID string, sid acp.SessionId) {
	for i, s := range sessionSettings {
		st := h.settingStore(i)
		v, ok := st.Pending(convID, sid)
		if !ok {
			continue
		}
		if err := h.cfg.Agent.SetConfigOption(ctx, sid, s.configID, v); err != nil {
			h.cfg.Logf("handler: setting %s=%s for %s: %v", s.configID, v, convID, err)
			continue
		}
		st.MarkApplied(convID, v, sid)
	}
}

// carrySettings moves the sticky settings to a fresh conversation id
// on `!new`: like the model, they are the user's choice.
func (h *Handler) carrySettings(from, to string) {
	for i := range sessionSettings {
		_ = h.settingStore(i).Carry(from, to)
	}
}

// renderSettings writes the panel's section for every setting the
// agent reports for this conversation, and returns one button per
// value. Every button's reply is the command a human could type.
func (h *Handler) renderSettings(sb *strings.Builder, key journal.Key) []zulipproto.ZFormChoice {
	conv, engaged := h.cfg.Journal.Lookup(key)
	var buttons []zulipproto.ZFormChoice
	for i, st := range sessionSettings {
		cur := h.settingValue(i, conv.ID, engaged)
		if cur == "" {
			continue
		}
		fmt.Fprintf(sb, "\n**%s**\n", st.label)
		for _, v := range st.values {
			reply := command.DisplaySigil + st.verb + " " + v
			marker := ""
			if v == cur {
				marker = " ←"
			}
			fmt.Fprintf(sb, "- `%s`%s\n", reply, marker)
			buttons = append(buttons, zulipproto.Choice(st.verb+" "+v, st.label+": "+v, reply))
		}
	}
	return buttons
}
