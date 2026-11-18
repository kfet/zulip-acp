// This file holds the VIEWS and the NOTIFY entries (config `views` and
// `notify`).
//
// # Views
//
// A view is ONE relay-edited message in a configured "channel/topic".
// It lists the topics of its source channels, grouped by state. Each
// line has a link, the age and the state. A topic in a waiting state
// that is idle longer than StaleAfter shows as stalled; a needs_you
// topic shows as stale. Stale topics go after the fresh ones of their
// group. A view with more than one source is a roll-up of all of them.
//
// # How a view stays current
//
// Every place that changes what a view shows calls viewsDirty:
// recordState, turn start and end (a scheduled turn included),
// moveConv, every Journal.Retire and RetireGone, and a schedule that is armed.
// viewsDirty arms one refresh after ViewDebounce; more calls in that
// time join it. RunViews refreshes at start and every hour, so ages
// and the stale marks move on without activity.
//
// The view is ONE message, edited in place. A new one is posted only
// when the edit fails; the old one is then deleted, or struck through
// when Zulip's delete time limit has passed.
//
// # Notify
//
// An edit does not notify. So a notify entry posts one short message
// when a topic ENTERS one of its states, and deletes that message when
// the topic leaves the state.
//
// Zulip limits how long after posting a message can be deleted. When
// the delete fails, the message is edited to a short struck-through
// line instead (dropMessage). When that fails too, it is logged once
// and forgotten: it is never retried. The relay never resolves a topic: a
// resolve always posts a Notification Bot notice.
package handler

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kfet/zulip-acp/internal/config"
	"github.com/kfet/zulip-acp/internal/journal"
	"github.com/kfet/zulip-acp/internal/zulipproto"
)

// DefaultViewDebounce is Config.ViewDebounce when unset.
const DefaultViewDebounce = 5 * time.Second

// viewRefreshPeriod is how often RunViews refreshes without a trigger.
const viewRefreshPeriod = time.Hour

// viewGroupMax is how many topics one group of a view lists.
const viewGroupMax = 15

// viewMaxRunes bounds a view message under Zulip's limit.
const viewMaxRunes = 9000

// Pseudo-states a view group may list besides the topic states.
const (
	viewRunning = "running"
	viewOpen    = "open"
)

// viewsDirty arms one debounced refresh of the views and notify
// entries. It is a no-op without them, or while a refresh is armed.
func (h *Handler) viewsDirty() {
	if !h.hasViews() || !h.viewArmed.CompareAndSwap(false, true) {
		return
	}
	h.cfg.ViewAfter(h.cfg.ViewDebounce, func() {
		h.viewArmed.Store(false)
		h.RefreshViews(context.Background())
	})
}

func (h *Handler) hasViews() bool {
	return len(h.cfg.Views) > 0 || len(h.cfg.Notifies) > 0 || len(h.cfg.Lifecycle) > 0
}

// RunViews refreshes now, then every hour until ctx ends.
func (h *Handler) RunViews(ctx context.Context) {
	if !h.hasViews() {
		return
	}
	t := time.NewTicker(viewRefreshPeriod)
	defer t.Stop()
	h.viewLoop(ctx, t.C)
}

// viewLoop is the testable core of RunViews; see watchdogLoop.
func (h *Handler) viewLoop(ctx context.Context, tick <-chan time.Time) {
	h.repairGone(ctx)
	h.dropStaleViews(ctx)
	h.RefreshViews(ctx)
	h.runLifecycle(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick:
			h.RefreshViews(ctx)
			h.runLifecycle(ctx)
		}
	}
}

// RefreshViews renders and publishes every view and syncs every notify
// entry. Failures are logged: the next refresh tries again.
func (h *Handler) RefreshViews(ctx context.Context) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), h.cfg.ZulipCallTimeout)
	defer cancel()
	h.viewMu.Lock()
	defer h.viewMu.Unlock()
	convs := h.cfg.Journal.Convs()
	for _, v := range h.cfg.Views {
		rec := h.cfg.Journal.View(v.Key)
		rec.MsgID = h.publishView(ctx, v, rec.MsgID, h.renderView(v, convs))
		h.saveView(v.Key, rec)
	}
	for _, n := range h.cfg.Notifies {
		rec := h.cfg.Journal.View(n.Key)
		rec.Notices = h.syncNotices(ctx, n, rec.Notices, convs)
		h.saveView(n.Key, rec)
	}
}

func (h *Handler) saveView(key string, rec journal.ViewRecord) {
	if err := h.cfg.Journal.SetView(key, rec); err != nil {
		h.cfg.Logf("handler: recording %s: %v", key, err)
	}
}

// publishView edits the view message msgID to text, and posts a new
// one only when there is none or the edit fails. A view is ONE
// long-lived message: Zulip limits how late a message can be deleted,
// so a view that moved on each post left old views behind that the
// relay could no longer remove. It returns the view message id now.
func (h *Handler) publishView(ctx context.Context, v config.ResolvedView, msgID int64, text string) int64 {
	if msgID != 0 {
		if h.viewText[v.Key] == text {
			return msgID
		}
		err := h.cfg.Client.EditMessage(ctx, msgID, text)
		if err == nil {
			h.viewText[v.Key] = text
			return msgID
		}
		h.cfg.Logf("handler: editing %s message %d: %v; posting a new one", v.Key, msgID, err)
	}
	id, err := h.cfg.Client.SendMessage(ctx, v.StreamID, v.Topic, text)
	if err != nil {
		h.cfg.Logf("handler: posting %s: %v", v.Key, err)
		return msgID
	}
	h.viewText[v.Key] = text
	if msgID != 0 {
		h.dropMessage(ctx, msgID, "old "+v.Key+" message", viewGone(v))
	}
	return id
}

// staleViewScan is how many of its own newest messages dropStaleViews
// reads in a view topic.
const staleViewScan = 50

// dropStaleViews drops, once at start, every view message the relay
// left in a view topic besides the current one, and every notice it
// no longer tracks. Older releases posted
// a new view when another message came after it, and could not delete
// the old one after Zulip's time limit.
func (h *Handler) dropStaleViews(ctx context.Context) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), h.cfg.ZulipCallTimeout)
	defer cancel()
	for _, v := range h.cfg.Views {
		cur := h.cfg.Journal.View(v.Key).MsgID
		narrow := append(zulipproto.TopicNarrow(v.StreamID, v.Topic), zulipproto.SenderNarrow(h.cfg.BotUserID))
		msgs, err := h.cfg.Client.Messages(ctx, narrow, staleViewScan, 0)
		if err != nil {
			h.cfg.Logf("handler: reading old %s messages: %v", v.Key, err)
			continue
		}
		for _, m := range msgs {
			if m.ID != cur && strings.HasPrefix(m.Content, viewMark) {
				h.dropMessage(ctx, m.ID, "old "+v.Key+" message", viewGone(v))
			}
		}
	} // A notice no notify entry tracks is stale: an older release
	// forgot it when Zulip refused the delete. The next refresh
	// reconciles the tracked ones against the current states.
	tracked := map[int64]bool{}
	for _, n := range h.cfg.Notifies {
		for _, id := range h.cfg.Journal.View(n.Key).Notices {
			tracked[id] = true
		}
	}
	for _, n := range h.cfg.Notifies {
		narrow := append(zulipproto.TopicNarrow(n.StreamID, n.Topic), zulipproto.SenderNarrow(h.cfg.BotUserID))
		msgs, err := h.cfg.Client.Messages(ctx, narrow, staleViewScan, 0)
		if err != nil {
			h.cfg.Logf("handler: reading old %s notices: %v", n.Key, err)
			continue
		}
		for _, m := range msgs {
			if !tracked[m.ID] && strings.HasPrefix(m.Content, noticeMark) {
				tracked[m.ID] = true // two entries may share a topic
				h.dropMessage(ctx, m.ID, n.Key+" notice", noticeGone)
			}
		}
	}
}

// noticeMark starts every notify notice.
const noticeMark = "🔔 "

// noticeGone is the text a notice is edited to when it cannot be
// deleted.
const noticeGone = "✔ ~~notice~~ — handled"

// viewMark starts every view message.
const viewMark = "📋 **"

// viewGone is the text an old view message is edited to.
func viewGone(v config.ResolvedView) string {
	return "~~" + v.Topic + "~~ — moved to a newer message below"
}

// repairGone retires, once at start, each conversation whose topic has
// left its channel but which an older release kept live. Those
// releases gave an archived topic a replacement conversation that kept
// the topic state, so a view still listed it. A candidate is a live
// channel conversation that shares its key with a retired one; it is
// retired when its topic (resolved or not) holds no message.
func (h *Handler) repairGone(ctx context.Context) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), h.cfg.ZulipCallTimeout)
	defer cancel()
	convs := h.cfg.Journal.Convs()
	retired := map[string]bool{}
	for _, c := range convs {
		if c.Retired {
			retired[c.Key.Token()] = true
		}
	}
	for _, c := range convs {
		if c.Retired || c.IsDM() || !retired[c.Key.Token()] || h.topicHasMessages(ctx, c.Key) {
			continue
		}
		if _, existed, err := h.cfg.Journal.RetireGone(c.Key); err != nil || !existed {
			h.cfg.Logf("handler: repairing %s: retire failed (existed=%v, %v)", c.ID, existed, err)
			continue
		}
		h.cfg.Logf("handler: repaired %s: its topic %s has left the channel, so it is retired", c.ID, h.describe(c.Key))
	}
}

// topicHasMessages reports whether topic k, or its resolved form, holds
// a message. A failed read counts as yes: repair only on evidence.
func (h *Handler) topicHasMessages(ctx context.Context, k journal.Key) bool {
	base := strings.TrimPrefix(k.Topic, journal.ResolvedPrefix)
	for _, t := range []string{base, journal.ResolvedPrefix + base} {
		_, ok, err := h.cfg.Client.OldestMessage(ctx, zulipproto.TopicNarrow(k.StreamID, t))
		if err != nil || ok {
			return true
		}
	}
	return false
}

// inSources reports whether channel id is in src. A nil src is all.
func inSources(src []int64, id int64) bool {
	return src == nil || slices.Contains(src, id)
}

// viewable reports whether a view or notify entry may show conv c.
func viewable(c journal.Conv, src []int64) bool {
	return !c.Retired && !c.IsDM() && inSources(src, c.StreamID)
}

// syncNotices posts a notice for each conv that is in one of n's
// states and has none, and deletes the notice of each conv that left
// them. It returns the notices that exist now.
func (h *Handler) syncNotices(ctx context.Context, n config.ResolvedNotify, have map[string]int64, convs []journal.Conv) map[string]int64 {
	out := map[string]int64{}
	for _, c := range convs {
		if !viewable(c, n.Sources) || c.State == nil || !slices.Contains(n.OnEnter, c.State.State) {
			continue
		}
		if id, ok := have[c.ID]; ok {
			out[c.ID] = id
			continue
		}
		line := noticeMark + topicMention(h.channelName(c.StreamID), c.Topic) + " is " + c.State.State
		if c.State.Reason != "" {
			line += " — " + c.State.Reason
		}
		id, err := h.cfg.Client.SendMessage(ctx, n.StreamID, n.Topic, line)
		if err != nil {
			h.cfg.Logf("handler: posting %s notice for %s: %v", n.Key, c.ID, err)
			continue
		}
		out[c.ID] = id
	}
	for convID, id := range have {
		if _, ok := out[convID]; ok {
			continue
		}
		h.dropMessage(ctx, id, n.Key+" notice", noticeGone)
	}
	return out
}

// dropMessage deletes a stale view or notice message. When Zulip
// refuses the delete (its time limit has passed), it edits the message
// to the short line gone instead. When the edit fails too, it logs once
// and gives up: the caller forgets the message either way.
func (h *Handler) dropMessage(ctx context.Context, id int64, what, gone string) {
	derr := h.cfg.Client.DeleteMessage(ctx, id)
	if derr == nil {
		return
	}
	if err := h.cfg.Client.EditMessage(ctx, id, gone); err != nil {
		h.cfg.Logf("handler: dropping %s %d: delete failed (%v), edit failed (%v); forgetting it", what, id, derr, err)
	}
}

// viewGroup is the index of the group of v that conv c goes in, or -1.
// A conv in a running turn goes to a group that lists "running", if
// one does; else to the first group that lists its state ("open" when
// it has none).
func (h *Handler) viewGroup(v config.ResolvedView, c journal.Conv) int {
	if h.isInflight(c.ID) {
		if g := groupOf(v.Groups, viewRunning); g >= 0 {
			return g
		}
	}
	state := viewOpen
	if c.State != nil {
		state = c.State.State
	}
	return groupOf(v.Groups, state)
}

func groupOf(groups []config.ViewGroup, state string) int {
	return slices.IndexFunc(groups, func(g config.ViewGroup) bool { return slices.Contains(g.States, state) })
}

// renderView renders view v over convs.
func (h *Handler) renderView(v config.ResolvedView, convs []journal.Conv) string {
	groups := make([][]journal.Conv, len(v.Groups))
	n := 0
	for _, c := range convs {
		if !viewable(c, v.Sources) {
			continue
		}
		if g := h.viewGroup(v, c); g >= 0 {
			groups[g] = append(groups[g], c)
			n++
		}
	}
	var sb strings.Builder
	sb.WriteString(viewMark + v.Topic + "** —")
	for _, id := range v.Sources {
		sb.WriteString(" #**" + h.channelName(id) + "**")
	}
	sb.WriteString("\n")
	if n == 0 {
		sb.WriteString("\nNo topics.")
		return sb.String()
	}
	now := h.now()
	// Folded groups go last, each in a spoiler block.
	order := make([]int, 0, len(groups))
	var folded []int
	for g := range groups {
		if h.folds(v, g) {
			folded = append(folded, g)
		} else {
			order = append(order, g)
		}
	}
	for _, g := range append(order, folded...) {
		cs := groups[g]
		if len(cs) == 0 {
			continue
		}
		// Fresh topics first, then stale ones; each part newest first.
		sort.SliceStable(cs, func(a, b int) bool {
			if sa, sb := isStale(v, cs[a], now), isStale(v, cs[b], now); sa != sb {
				return sb
			}
			return lastSeen(cs[a]) > lastSeen(cs[b])
		})
		limit, fold := viewGroupMax, h.folds(v, g)
		if fold {
			limit = v.CollapseShow
			fmt.Fprintf(&sb, "\n```spoiler %s (%d)\n", v.Groups[g].Title, len(cs))
		} else {
			fmt.Fprintf(&sb, "\n**%s** (%d)\n", v.Groups[g].Title, len(cs))
		}
		for i, c := range cs {
			if i == limit {
				fmt.Fprintf(&sb, "- …and %d more\n", len(cs)-i)
				break
			}
			line := h.viewLine(v, c, now)
			if utf8.RuneCountInString(sb.String())+utf8.RuneCountInString(line) > viewMaxRunes {
				if fold {
					sb.WriteString("```\n")
				}
				sb.WriteString("\n…the view is full.")
				return sb.String()
			}
			sb.WriteString(line)
		}
		if fold {
			sb.WriteString("```\n")
		}
	}
	return sb.String()
}

// folds reports whether group g of v folds: it lists a collapse state.
func (h *Handler) folds(v config.ResolvedView, g int) bool {
	return slices.ContainsFunc(v.Groups[g].States, func(st string) bool { return slices.Contains(v.Collapse, st) })
}

// staleLabel is the label of each state that can go stale.
var staleLabel = map[string]string{journal.StateWaiting: "stalled", journal.StateNeedsYou: "stale"}

// isStale reports whether c is in a state that can go stale and is idle
// longer than v.StaleAfter.
func isStale(v config.ResolvedView, c journal.Conv, now time.Time) bool {
	if c.State == nil || v.StaleAfter <= 0 {
		return false
	}
	_, ok := staleLabel[c.State.State]
	return ok && now.Sub(time.UnixMilli(lastSeen(c))) > v.StaleAfter
}

// viewLine renders one topic of a view: link, age, state, reason.
func (h *Handler) viewLine(v config.ResolvedView, c journal.Conv, now time.Time) string {
	seen := lastSeen(c)
	line := "- " + topicMention(h.channelName(c.StreamID), c.Topic) + " · " + age(now, seen)
	if h.isInflight(c.ID) {
		line += " · " + viewRunning
	}
	if c.State == nil {
		return line + "\n"
	}
	state := c.State.State
	if isStale(v, c, now) {
		state = staleLabel[state]
	}
	line += " · " + state
	if c.State.SetBy == journal.SetByImport {
		line += " (guessed)"
	}
	if c.State.Reason != "" {
		line += " — " + c.State.Reason
	}
	return line + "\n"
}

// channelName is the name of served channel id, or its id when the
// relay does not serve it.
func (h *Handler) channelName(id int64) string {
	if name, ok := h.cfg.Channels.Name(id); ok {
		return name
	}
	return strconv.FormatInt(id, 10)
}
