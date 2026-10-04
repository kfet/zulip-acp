package handler

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/kfet/acp-kit/command"

	"github.com/kfet/zulip-acp/internal/journal"
	"github.com/kfet/zulip-acp/internal/zulipproto"
)

// MaxCatchupPerTopic bounds how many missed messages one topic's
// catch-up turn carries. The newest are kept: they are the ones a
// reader still waits on, and the older ones are named in the header.
const MaxCatchupPerTopic = 50

// DefaultCatchupTurns is how many catch-up turns run at once. A long
// outage can leave many topics waiting, and starting all of their turns
// together would be a burst of agent prompts at startup.
const DefaultCatchupTurns = 4

// Mark is the persisted id of the last message the relay processed.
// See internal/catchup.
type Mark interface {
	Get() int64
	Set(id int64) error
}

// catchupRun collects what one catch-up read decided per topic.
type catchupRun struct {
	cutoff int64
	// skipped counts the too-old messages per conversation key, and
	// order keeps the notices in the order the topics were first seen.
	skipped map[string]int
	keys    map[string]journal.Key
	order   []string
}

func (cu *catchupRun) skip(key journal.Key) {
	l := key.Label()
	if _, ok := cu.skipped[l]; !ok {
		cu.keys[l] = key
		cu.order = append(cu.order, l)
	}
	cu.skipped[l]++
}

// catchupTopic is the missed traffic of one conversation.
type catchupTopic struct {
	conv      journal.Conv
	prompts   []string
	first     int64 // timestamp of the first message
	lastID    int64
	addressed bool
}

// catchupText decides what a missed message contributes. A relay
// command and a Zulip widget are skipped: they were typed for a relay
// that was not there, and running `!new` or `!model` minutes later
// would surprise everybody. A `/` message is a widget or an agent slash
// command, and is skipped for the same reason. The `!!` escape still
// means prose.
func catchupText(in string) (prompt string, skip bool) {
	if rest, ok := strings.CutPrefix(in, doubleSigil); ok {
		return command.DisplaySigil + rest, false
	}
	if strings.HasPrefix(in, command.DisplaySigil) || strings.HasPrefix(in, "/") {
		return "", true
	}
	return in, false
}

// CatchUp turns the messages that arrived while the relay was down
// into turns. msgs is oldest first, as internal/catchup reads it.
//
// Every message goes through the same gates as a live one (route).
// Then each conversation's missed messages are COLLAPSED into one
// turn: a topic that had five messages while the relay was down gets
// one answer, not five turns that supersede each other. Messages older
// than maxAge are not answered; each topic that had any gets a
// one-line notice instead. The turns start through startTurn, so the
// session pool and its concurrency apply exactly as for live turns.
func (h *Handler) CatchUp(ctx context.Context, msgs []zulipproto.Message, maxAge time.Duration) {
	cu := &catchupRun{
		cutoff:  h.now().Add(-maxAge).Unix(),
		skipped: map[string]int{},
		keys:    map[string]journal.Key{},
	}
	topics := map[string]*catchupTopic{}
	var order []string
	for i := range msgs {
		m := &msgs[i]
		r, ok := h.route(ctx, m, cu)
		if !ok {
			continue
		}
		t := topics[r.conv.ID]
		if t == nil {
			t = &catchupTopic{conv: r.conv, first: m.Timestamp}
			topics[r.conv.ID] = t
			order = append(order, r.conv.ID)
		}
		t.prompts = append(t.prompts, r.prompt)
		t.lastID = m.ID
		t.addressed = t.addressed || r.addressed
	}
	for _, l := range cu.order {
		n := cu.skipped[l]
		h.reply(ctx, cu.keys[l], fmt.Sprintf("[catch-up] %d %s older than %s arrived while the relay was offline and %s not answered.",
			n, plural(n, "message", "messages"), fmtAge(maxAge), plural(n, "was", "were")))
	}
	if len(order) == 0 {
		return
	}
	turns := make([]*catchupTopic, len(order))
	for i, id := range order {
		turns[i] = topics[id]
	}
	h.catchups.Add(1)
	go h.startCatchups(ctx, turns)
}

// startCatchups starts the catch-up turns, at most CatchupTurns at a
// time. It runs off the event loop, so live messages are not held up
// behind it.
func (h *Handler) startCatchups(ctx context.Context, turns []*catchupTopic) {
	defer h.catchups.Done()
	n := h.cfg.CatchupTurns
	if n <= 0 {
		n = DefaultCatchupTurns
	}
	slots := make(chan struct{}, n)
	for i, t := range turns {
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
			h.cfg.Logf("handler: WARN shutting down; %d catch-up turn(s) not started", len(turns)-i)
			return
		}
		if live, ok := h.liveTurns.Load(t.conv.ID); ok && live.(int64) > t.lastID {
			// A live message started a turn here while this one waited.
			// Starting now would supersede, and so cancel, that newer
			// turn.
			<-slots
			h.cfg.Logf("handler: catch-up turn in %s dropped: a newer message already started a turn", h.describe(t.conv.Key))
			continue
		}
		h.cfg.Logf("handler: catch-up turn in %s for %d message(s)", h.describe(t.conv.Key), len(t.prompts))
		h.startTurnThen(ctx, t.conv, catchupPrompt(t), t.addressed, t.lastID, t.lastID, func() { <-slots })
	}
}

// catchupPrompt builds the one prompt of a topic's catch-up turn.
func catchupPrompt(t *catchupTopic) string {
	n := len(t.prompts)
	var b strings.Builder
	fmt.Fprintf(&b, "[catch-up] %d messages arrived while the relay was offline (first at %s)",
		n, time.Unix(t.first, 0).UTC().Format(time.RFC3339))
	kept := t.prompts
	if n > MaxCatchupPerTopic {
		kept = kept[n-MaxCatchupPerTopic:]
		fmt.Fprintf(&b, "\n[catch-up] Only the newest %d are below; the oldest %d are left out.", MaxCatchupPerTopic, n-MaxCatchupPerTopic)
	}
	for _, p := range kept {
		b.WriteString("\n\n")
		b.WriteString(p)
	}
	return b.String()
}

// fmtAge renders a duration without the zero tails: "24h", not
// "24h0m0s".
func fmtAge(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// advanceMark records that message id has been processed. A failed
// write is logged and not fatal: the worst case is that the next cold
// start reads a few messages again, and the age limit bounds that.
func (h *Handler) advanceMark(id int64) {
	if err := h.cfg.Mark.Set(id); err != nil {
		h.cfg.Logf("handler: WARN catch-up mark: %v", err)
	}
}
