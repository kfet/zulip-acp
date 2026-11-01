package handler

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kfet/zulip-acp/internal/journal"
	"github.com/kfet/zulip-acp/internal/zulipproto"
)

// emojiHarness is a loopback harness with the glyph status on.
func emojiHarness(t *testing.T) *loopHarness {
	t.Helper()
	journal.SetStatusPrefixes("⧗", "⁇")
	t.Cleanup(func() { journal.SetStatusPrefixes() })
	lh := newLoopHarness(t, newAgent("done"), func(c *Config) {
		c.TopicStatus = StatusEmoji
		c.StatusBusy, c.StatusQuestion = "⧗", "⁇"
	})
	lh.z.mu.Lock()
	lh.z.next = 100
	lh.z.mu.Unlock()
	return lh
}

// askDuring makes the agent call `topic_status` during its next turn.
func (lh *loopHarness) askDuring(t *testing.T, state string) {
	lh.armDuring(func() {
		id := lh.j.Convs()[0].ID
		if _, err := lh.h.SetTopicStatus(id, state); err != nil {
			t.Errorf("SetTopicStatus: %v", err)
		}
	})
}

// TestEmojiTopicIsBusyThenBare: a turn shows the busy glyph from its
// start, and an idle topic has no prefix. No ✔ is ever set, so
// Notification Bot posts nothing.
func TestEmojiTopicIsBusyThenBare(t *testing.T) {
	lh := emojiHarness(t)
	lh.seedTrigger("work")
	lh.deliverTurn(t, "work", mention("go"))
	m := lh.z.moved()
	if len(m) != 2 || m[0] != "1:⧗ work:change_all" || !strings.HasSuffix(m[1], ":work:change_all") {
		t.Fatalf("moves = %v, want busy by the trigger, then bare", m)
	}
	convs := lh.j.Convs()
	if len(convs) != 1 || convs[0].Topic != "work" {
		t.Fatalf("journal = %+v", convs)
	}
}

// TestEmojiNeedsInputShowsTheQuestion, and the next answered turn
// clears it: the state is per turn and defaults to done.
func TestEmojiNeedsInputShowsTheQuestion(t *testing.T) {
	lh := emojiHarness(t)
	lh.seedTrigger("work")
	lh.askDuring(t, StateNeedsInput)
	lh.deliverTurn(t, "work", mention("go"))
	id := lh.j.Convs()[0].ID
	if got := lh.convTopic(t, id); got != "⁇ work" {
		t.Fatalf("topic = %q, want the question glyph", got)
	}
	lh.clearMoves()
	lh.seedTrigger("⁇ work")
	lh.deliverTurn(t, "⁇ work", "yes")
	if m := lh.z.moved(); len(m) != 2 || m[0] != "1:⧗ work:change_all" {
		t.Fatalf("moves = %v", m)
	}
	if got := lh.convTopic(t, id); got != "work" {
		t.Fatalf("topic = %q, want bare after a done turn", got)
	}
	if n := len(lh.j.Convs()); n != 1 {
		t.Fatalf("%d conversations, want 1", n)
	}
}

// TestEmojiSilentTurnKeepsTheState: a turn that posts nothing changes
// nothing the user sees, so the question stays.
func TestEmojiSilentTurnKeepsTheState(t *testing.T) {
	lh := emojiHarness(t)
	lh.seedTrigger("work")
	lh.askDuring(t, StateNeedsInput)
	lh.deliverTurn(t, "work", mention("go"))
	id := lh.j.Convs()[0].ID

	lh.a.mu.Lock()
	lh.a.chunks = []string{"<<SILENT>>"}
	lh.a.mu.Unlock()
	lh.seedTrigger("⁇ work")
	lh.deliverTurn(t, "⁇ work", "chatter not aimed at the bot")
	if !lh.logged("abstained") {
		t.Fatalf("the turn did not abstain; logs %v", lh.logs)
	}
	if got := lh.convTopic(t, id); got != "⁇ work" {
		t.Fatalf("topic = %q, a silent turn must keep the question", got)
	}
}

// TestEmojiSentinelOnlyAnswerKeepsTheState: the addressed-turn safety
// net retracts a sentinel-only answer; that is a silent turn too.
func TestEmojiSentinelOnlyAnswerKeepsTheState(t *testing.T) {
	lh := emojiHarness(t)
	lh.seedTrigger("work")
	lh.askDuring(t, StateNeedsInput)
	lh.deliverTurn(t, "work", mention("go"))
	id := lh.j.Convs()[0].ID
	lh.a.mu.Lock()
	lh.a.chunks = []string{"<<SILENT>>"}
	lh.a.mu.Unlock()
	lh.seedTrigger("⁇ work")
	lh.deliverTurn(t, "⁇ work", mention("anything?"))
	if got := lh.convTopic(t, id); got != "⁇ work" {
		t.Fatalf("topic = %q, logs %v", got, lh.logs)
	}
}

// TestEmojiPendingScheduleShowsNoGlyph: unlike the resolve mode, a
// schedule alone is not shown.
func TestEmojiPendingScheduleShowsNoGlyph(t *testing.T) {
	lh := emojiHarness(t)
	lh.seedTrigger("work")
	lh.deliverTurn(t, "work", mention("go"))
	id := lh.j.Convs()[0].ID
	lh.clearMoves()
	mustSchedule(t, lh.h, chanToken("work"), "wake", fireEpoch.Add(time.Hour))
	if got := lh.convTopic(t, id); got != "work" {
		t.Fatalf("topic = %q", got)
	}
	if m := lh.z.moved(); len(m) != 0 {
		t.Fatalf("moves = %v", m)
	}
}

// TestEmojiMarkBusyReadsTheServer: the read-back strips every glyph, so
// a topic already busy on the server is not moved again, and one shown
// as waiting is.
func TestEmojiMarkBusyReadsTheServer(t *testing.T) {
	lh := emojiHarness(t)
	c, _ := lh.j.Ensure(journal.Channel(4, "work"))
	lh.seedTrigger("⧗ work")
	if got := lh.h.markBusy(context.Background(), c, 1); got.Topic != "⧗ work" || len(lh.z.moved()) != 0 {
		t.Fatalf("conv %+v, moves %v", got, lh.z.moved())
	}
	lh.seedTrigger("⁇ work")
	got := lh.h.markBusy(context.Background(), c, 1)
	if m := lh.z.moved(); !slices.Equal(m, []string{"1:⧗ work:change_all"}) || got.Topic != "⧗ work" {
		t.Fatalf("moves %v, conv %+v", m, got)
	}
}

// TestSetTopicStatusRefusals: the tool records only for a live turn of
// a live conversation, in emoji mode, with a known state.
func TestSetTopicStatusRefusals(t *testing.T) {
	lh := emojiHarness(t)
	c, _ := lh.j.Ensure(journal.Channel(4, "work"))
	for _, tc := range []struct{ id, state, want string }{
		{c.ID, "maybe", "state must be"},
		{"nope", StateDone, "no longer active"},
		{c.ID, StateDone, "no turn is running"},
	} {
		if _, err := lh.h.SetTopicStatus(tc.id, tc.state); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%+v: err = %v", tc, err)
		}
	}
	prev, _, _, _ := lh.j.Retire(c.Key)
	if _, err := lh.h.SetTopicStatus(prev.ID, StateDone); err == nil {
		t.Error("a retired conversation must be refused")
	}

	off := statusHarness(t)
	if _, err := off.h.SetTopicStatus(c.ID, StateDone); err != errStatusOff {
		t.Fatalf("resolve mode: err = %v", err)
	}
}

// TestSetTopicStatusTellsTheAgent: the reply says nothing moved yet.
func TestSetTopicStatusTellsTheAgent(t *testing.T) {
	lh := emojiHarness(t)
	lh.seedTrigger("work")
	var out string
	lh.armDuring(func() { out, _ = lh.h.SetTopicStatus(lh.j.Convs()[0].ID, StateDone) })
	lh.deliverTurn(t, "work", mention("go"))
	if !strings.Contains(out, "when this turn ends") {
		t.Fatalf("out = %q", out)
	}
}

// TestMarkQuietWithoutATurn is a no-op, not a panic.
func TestMarkQuietWithoutATurn(t *testing.T) {
	lh := emojiHarness(t)
	lh.h.markQuiet("nope")
}

// TestNoteTurnStatusWithoutAnEntry: endTurn can run with no entry.
func TestNoteTurnStatusWithoutAnEntry(t *testing.T) {
	lh := emojiHarness(t)
	c, _ := lh.j.Ensure(journal.Channel(4, "work"))
	lh.h.waiting.Store(c.Key.Token(), struct{}{})
	lh.h.noteTurnStatus(c, nil)
	if _, ok := lh.h.waiting.Load(c.Key.Token()); !ok {
		t.Fatal("a nil entry changed the state")
	}
}

// TestStatusSkipsATopicTooLongForThePrefix: Zulip refuses a topic over
// MaxTopicLength, so a topic the prefix would push over it gets no
// status and no refused move every turn.
func TestStatusSkipsATopicTooLongForThePrefix(t *testing.T) {
	for _, tc := range []struct {
		name string
		h    func(*testing.T) *loopHarness
	}{{"emoji", emojiHarness}, {"resolve", statusHarness}} {
		t.Run(tc.name, func(t *testing.T) {
			lh := tc.h(t)
			long := strings.Repeat("x", zulipproto.MaxTopicLength-1)
			lh.seedTrigger(long)
			lh.deliverTurn(t, long, mention("go"))
			if m := lh.z.moved(); len(m) != 0 || lh.logged("status mark") {
				t.Fatalf("moves %v, logs %v", m, lh.logs)
			}
			fits := strings.Repeat("x", zulipproto.MaxTopicLength-2)
			if !lh.h.statusApplies(journal.Conv{Key: journal.Channel(4, fits)}) {
				t.Fatal("a topic that fits with the prefix must get the status")
			}
		})
	}
}
