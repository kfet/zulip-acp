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

// askDuring makes the agent call `topic_state` during its next turn.
func (lh *loopHarness) askDuring(t *testing.T, state string) {
	lh.armDuring(func() {
		id := lh.j.Convs()[0].ID
		if _, err := lh.h.SetTopicState(id, state, "why"); err != nil {
			t.Errorf("SetTopicState: %v", err)
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

// TestEmojiNeedsYouShowsTheQuestion: the state lasts across turns. A
// turn that records nothing keeps the question; a turn that records
// fyi clears it.
func TestEmojiNeedsYouShowsTheQuestion(t *testing.T) {
	lh := emojiHarness(t)
	lh.seedTrigger("work")
	lh.askDuring(t, journal.StateNeedsYou)
	lh.deliverTurn(t, "work", mention("go"))
	id := lh.j.Convs()[0].ID
	if got := lh.convTopic(t, id); got != "⁇ work" {
		t.Fatalf("topic = %q, want the question glyph", got)
	}
	lh.clearMoves()
	lh.seedTrigger("⁇ work")
	lh.deliverTurn(t, "⁇ work", "a follow-up")
	if got := lh.convTopic(t, id); got != "⁇ work" {
		t.Fatalf("topic = %q, a turn that records nothing must keep the state", got)
	}
	lh.clearMoves()
	lh.askDuring(t, journal.StateFYI)
	lh.seedTrigger("⁇ work")
	lh.deliverTurn(t, "⁇ work", "yes")
	if m := lh.z.moved(); len(m) != 2 || m[0] != "1:⧗ work:change_all" {
		t.Fatalf("moves = %v", m)
	}
	if got := lh.convTopic(t, id); got != "work" {
		t.Fatalf("topic = %q, want bare after fyi", got)
	}
	if n := len(lh.j.Convs()); n != 1 {
		t.Fatalf("%d conversations, want 1", n)
	}
	c, _ := lh.j.LookupID(id)
	if c.ActivitySince != 0 || c.LastActivity == 0 {
		t.Fatalf("activity = %d since, last %d", c.ActivitySince, c.LastActivity)
	}
}

// TestEmojiSilentTurnKeepsTheState: a turn that posts nothing changes
// nothing the user sees, so the question stays.
func TestEmojiSilentTurnKeepsTheState(t *testing.T) {
	lh := emojiHarness(t)
	lh.seedTrigger("work")
	lh.askDuring(t, journal.StateNeedsYou)
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
	lh.askDuring(t, journal.StateNeedsYou)
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

// TestSetTopicStateRefusals: the tool records only for a live
// conversation, with a known state.
func TestSetTopicStateRefusals(t *testing.T) {
	lh := emojiHarness(t)
	c, _ := lh.j.Ensure(journal.Channel(4, "work"))
	for _, tc := range []struct{ id, state, want string }{
		{c.ID, "maybe", "not a topic state"},
		{"nope", journal.StateDone, "no longer active"},
	} {
		if _, err := lh.h.SetTopicState(tc.id, tc.state, "r"); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%+v: err = %v", tc, err)
		}
	}
	prev, _, _, _ := lh.j.Retire(c.Key)
	if _, err := lh.h.SetTopicState(prev.ID, journal.StateDone, "r"); err == nil {
		t.Error("a retired conversation must be refused")
	}
}

// TestSetTopicStateTellsTheAgent, and says so when nothing changed.
func TestSetTopicStateTellsTheAgent(t *testing.T) {
	lh := emojiHarness(t)
	lh.seedTrigger("work")
	var out, again string
	lh.armDuring(func() {
		id := lh.j.Convs()[0].ID
		out, _ = lh.h.SetTopicState(id, journal.StateDone, "r")
		again, _ = lh.h.SetTopicState(id, journal.StateDone, "r")
	})
	lh.deliverTurn(t, "work", mention("go"))
	if out != "State done recorded." || !strings.Contains(again, "nothing changed") {
		t.Fatalf("out = %q, again = %q", out, again)
	}
}

// TestUserStateSettlesAnIdleTopic: `!state needs_you` on an idle topic
// moves it to the question glyph at once.
func TestUserStateSettlesAnIdleTopic(t *testing.T) {
	lh := emojiHarness(t)
	lh.seedTrigger("work")
	lh.deliverTurn(t, "work", mention("go"))
	c := lh.j.Convs()[0]
	lh.clearMoves()
	if got := lh.h.stateCommand(c.Key, "needs-you check the diff"); !strings.Contains(got, "**needs_you** — check the diff (by user") {
		t.Fatalf("reply = %q", got)
	}
	if got := lh.convTopic(t, c.ID); got != "⁇ work" {
		t.Fatalf("topic = %q", got)
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
