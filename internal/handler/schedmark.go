// This file makes schedule changes visible without a question.
//
// A turn that arms, cancels or is started by a schedule says so in its
// footer, and the reply that armed a schedule carries an :alarm_clock:
// reaction until that schedule is gone. Both are derived from the
// Handler's own command.Scheduler methods, which every path goes
// through — the agent's tools and the `!unschedule` command alike — so
// no change can bypass them.
//
// A turn that changed nothing gets no marker. A footer that always
// talks about schedules is a footer nobody reads.
package handler

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/kfet/acp-kit/command"
	"github.com/kfet/acp-kit/schedule"
	"github.com/kfet/zulip-acp/internal/journal"
)

// alarmEmoji marks a relay reply that armed a schedule still pending.
const alarmEmoji = "alarm_clock"

// schedTurn is what one running turn did to its conversation's
// schedule set.
type schedTurn struct {
	fired     bool
	armed     []schedule.Item
	cancelled int
}

// beginSchedTurn starts recording schedule changes for the turn in
// token's conversation. fired says the turn was started by a schedule.
func (h *Handler) beginSchedTurn(token string, fired bool) *schedTurn {
	st := &schedTurn{fired: fired}
	h.schedMu.Lock()
	if h.schedTurns == nil {
		h.schedTurns = map[string]*schedTurn{}
	}
	h.schedTurns[token] = st
	h.schedMu.Unlock()
	return st
}

// endSchedTurn stops recording for st. A superseded turn can end after
// its successor began, so only st itself is removed.
func (h *Handler) endSchedTurn(token string, st *schedTurn) {
	h.schedMu.Lock()
	if h.schedTurns[token] == st {
		delete(h.schedTurns, token)
	}
	h.schedMu.Unlock()
}

// noteArmed records it against the turn running in its conversation,
// if one is.
func (h *Handler) noteArmed(it schedule.Item) {
	h.schedMu.Lock()
	defer h.schedMu.Unlock()
	if st := h.schedTurns[it.Conv]; st != nil {
		st.armed = append(st.armed, it)
	}
}

// noteCancelled records a cancel against the running turn, if one is.
// A schedule armed and cancelled in the same turn nets out to nothing.
func (h *Handler) noteCancelled(token, id string) {
	h.schedMu.Lock()
	defer h.schedMu.Unlock()
	st := h.schedTurns[token]
	if st == nil {
		return
	}
	for i, it := range st.armed {
		if it.ID == id {
			st.armed = append(st.armed[:i], st.armed[i+1:]...)
			return
		}
	}
	st.cancelled++
}

// schedMarker renders the footer segment for st, or "" when the turn
// changed nothing. pending is what is armed in the conversation now.
//
//	⏰ +1 → <time:…> · 2 pending
//	⏰ −1 · 2 pending
//	⏰ fired · 1 pending
//
// After an arm, the count is of the OTHER pending schedules: the armed
// ones are already on the line.
func (h *Handler) schedMarker(st *schedTurn, pending []schedule.Item) string {
	h.schedMu.Lock()
	fired, armed, cancelled := st.fired, append([]schedule.Item(nil), st.armed...), st.cancelled
	h.schedMu.Unlock()
	if !fired && len(armed) == 0 && cancelled == 0 {
		return ""
	}
	parts := []string{"⏰"}
	if fired {
		parts = append(parts, "fired")
	}
	others := len(pending)
	if len(armed) > 0 {
		earliest := armed[0].At
		for _, it := range armed[1:] {
			if it.At.Before(earliest) {
				earliest = it.At
			}
		}
		parts = append(parts, fmt.Sprintf("+%d → %s", len(armed), zulipTime(earliest)))
		for _, it := range armed {
			if containsItem(pending, it.ID) {
				others--
			}
		}
	}
	if cancelled > 0 {
		parts = append(parts, fmt.Sprintf("−%d", cancelled))
	}
	out := strings.Join(parts[:2], " ")
	for _, p := range parts[2:] {
		out += " · " + p
	}
	if others > 0 {
		out += fmt.Sprintf(" · %d pending", others)
	}
	return out
}

// zulipTime renders t as a Zulip global time, which every client shows
// in the reader's own time zone.
func zulipTime(t time.Time) string {
	return "<time:" + t.UTC().Format(time.RFC3339) + ">"
}

func containsItem(items []schedule.Item, id string) bool {
	for _, it := range items {
		if it.ID == id {
			return true
		}
	}
	return false
}

// markArmed puts the :alarm_clock: reaction on msgID, the reply of the
// turn that armed st's schedules, and records which schedules it stands
// for. Schedules that already fired or were cancelled are skipped.
func (h *Handler) markArmed(ctx context.Context, st *schedTurn, pending []schedule.Item, msgID int64) {
	if msgID == 0 {
		return
	}
	h.schedMu.Lock()
	armed := append([]schedule.Item(nil), st.armed...)
	h.schedMu.Unlock()
	marked := false
	for _, it := range armed {
		if !containsItem(pending, it.ID) {
			continue
		}
		if err := h.cfg.Journal.SetAlarm(it.ID, msgID); err != nil {
			h.cfg.Logf("handler: recording the reaction of schedule %s: %v", it.ID, err)
			continue
		}
		marked = true
	}
	if !marked {
		return
	}
	if err := h.cfg.Client.AddReaction(ctx, msgID, alarmEmoji); err != nil {
		h.cfg.Logf("handler: adding :%s: to %d: %v", alarmEmoji, msgID, err)
	}
}

// unmarkAlarm removes the reaction that stood for schedule id, unless
// another pending schedule still shares the message.
func (h *Handler) unmarkAlarm(ctx context.Context, id string) {
	msgID, shared, ok := h.cfg.Journal.TakeAlarm(id)
	if !ok || shared {
		return
	}
	if err := h.cfg.Client.RemoveReaction(ctx, msgID, alarmEmoji); err != nil {
		h.cfg.Logf("handler: removing :%s: from %d: %v", alarmEmoji, msgID, err)
	}
}

// renderSched is the `!sched` answer: the conversation's pending
// schedules, soonest first, with the due time as a Zulip global time and
// the first line of each prompt.
func renderSched(items []schedule.Item) string {
	if len(items) == 0 {
		return "⏰ Nothing is scheduled here."
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "⏰ %d pending:\n\n", len(items))
	for _, it := range items {
		first, _, _ := strings.Cut(strings.TrimSpace(it.Text), "\n")
		if r := []rune(first); len(r) > schedPreviewRunes {
			first = string(r[:schedPreviewRunes]) + "…"
		}
		fmt.Fprintf(&sb, "- `%s` %s", it.ID, zulipTime(it.At))
		if it.Every > 0 {
			fmt.Fprintf(&sb, " (every %s)", it.Every)
		}
		fmt.Fprintf(&sb, " — %s\n", first)
	}
	return sb.String()
}

// schedPreviewRunes caps the prompt preview of one `!sched` line.
const schedPreviewRunes = 120

// isSchedCommand reports whether text is `!sched`.
func isSchedCommand(text string) bool {
	body, ok := command.StripSigil(strings.TrimSpace(text))
	return ok && strings.EqualFold(strings.TrimSpace(body), schedVerb)
}

// schedVerb is the `!sched` command word.
const schedVerb = "sched"

// schedHelp advertises `!sched` in `!help`. Only when scheduling is on:
// a help entry for a command that cannot work teaches the wrong thing.
const schedHelp = "- `" + command.DisplaySigil + schedVerb + "` — list the schedules pending here, with due times\n"

// schedCommand answers `!sched` in key's conversation.
func (h *Handler) schedCommand(ctx context.Context, key journal.Key) {
	items, err := h.cfg.Commands.ScheduleList(key.Token())
	if err != nil {
		h.reply(ctx, key, "❌ "+err.Error()+".")
		return
	}
	h.reply(ctx, key, renderSched(items))
}
