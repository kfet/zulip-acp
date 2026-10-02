package handler

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kfet/acp-kit/relaytool"
	"github.com/kfet/zulip-acp/internal/journal"
	"github.com/kfet/zulip-acp/internal/zulipproto"
)

// statusHarness is a loopback harness with the topic status mark on.
// Message ids the fake hands out start at 100, so they never collide
// with the triggering message, which channelEvent always numbers 1.
func statusHarness(t *testing.T) *loopHarness {
	t.Helper()
	lh := newLoopHarness(t, newAgent("done"), func(c *Config) { c.TopicStatus = true })
	lh.z.mu.Lock()
	lh.z.next = 100
	lh.z.mu.Unlock()
	return lh
}

// seedTrigger puts the triggering message where the server holds it:
// the unresolve path reads it back before it moves it.
func (lh *loopHarness) seedTrigger(topic string) {
	lh.z.mu.Lock()
	lh.z.messages[1] = zulipproto.Message{ID: 1, SenderID: humanID, StreamID: 4, Topic: topic}
	lh.z.mu.Unlock()
}

func (lh *loopHarness) clearMoves() {
	lh.z.mu.Lock()
	lh.z.moves = nil
	lh.z.mu.Unlock()
}

func (lh *loopHarness) convTopic(t *testing.T, id string) string {
	t.Helper()
	c, ok := lh.j.LookupID(id)
	if !ok {
		t.Fatalf("conversation %s is gone", id)
	}
	return c.Topic
}

// TestTopicIsResolvedWhenTheTurnEnds: an idle topic carries ✔, moved
// by the relay's own last message with change_all, and the journal
// follows inline so the conversation keeps its id.
func TestTopicIsResolvedWhenTheTurnEnds(t *testing.T) {
	lh := statusHarness(t)
	lh.seedTrigger("work")
	lh.deliverTurn(t, "work", mention("go"))

	moves := lh.z.moved()
	if len(moves) != 1 || !strings.HasSuffix(moves[0], ":✔ work:change_all") || strings.HasPrefix(moves[0], "1:") {
		t.Fatalf("moves = %v, want one resolve by the relay's own message", moves)
	}
	convs := lh.j.Convs()
	if len(convs) != 1 || convs[0].Topic != "✔ work" {
		t.Fatalf("journal = %+v", convs)
	}
}

// TestMessageInAResolvedTopicUnresolvesItFirst: the turn unresolves the
// topic before it posts anything, keeps the same conversation, posts
// the answer into the unresolved topic, and resolves it again at the end.
func TestMessageInAResolvedTopicUnresolvesItFirst(t *testing.T) {
	lh := statusHarness(t)
	lh.seedTrigger("work")
	lh.deliverTurn(t, "work", mention("go"))
	first := lh.j.Convs()[0].ID
	lh.clearMoves()
	lh.z.reset()

	var postedBefore int
	lh.z.moveHook = func() {
		if len(lh.z.moves) == 0 {
			postedBefore = len(lh.z.order)
		}
	}
	lh.seedTrigger("✔ work")
	lh.deliverTurn(t, "✔ work", "again")

	moves := lh.z.moved()
	if len(moves) != 2 || moves[0] != "1:work:change_all" || !strings.HasSuffix(moves[1], ":✔ work:change_all") {
		t.Fatalf("moves = %v, want unresolve by the trigger, then resolve", moves)
	}
	if postedBefore != 0 {
		t.Fatalf("%d messages were posted before the topic was unresolved", postedBefore)
	}
	// The fake moves only the anchor, so the resolve anchor reads the
	// resolved topic; every other message must be in the unresolved one.
	resolveAnchor, _, _ := strings.Cut(moves[1], ":")
	for id, topic := range lh.z.topics {
		if topic != "work" && strconv.FormatInt(id, 10) != resolveAnchor {
			t.Fatalf("message %d (%q) went to %q, want the unresolved topic", id, lh.z.bodies[id], topic)
		}
	}
	convs := lh.j.Convs()
	if len(convs) != 1 || convs[0].ID != first || convs[0].Topic != "✔ work" {
		t.Fatalf("journal = %+v, want the same conversation, resolved again", convs)
	}
}

// TestPendingScheduleKeepsTheTopicUnresolved, then the fire clears it:
// a schedule armed in a turn is live work, and once the one-shot fires
// and its turn ends the topic is idle again.
func TestPendingScheduleKeepsTheTopicUnresolved(t *testing.T) {
	lh := statusHarness(t)
	lh.run(t)
	lh.seedTrigger("work")
	lh.armDuring(func() { mustSchedule(t, lh.h, chanToken("work"), "wake", fireEpoch.Add(time.Minute)) })
	lh.deliverTurn(t, "work", mention("go"))
	if m := lh.z.moved(); len(m) != 0 {
		t.Fatalf("moves = %v, want none while a schedule is pending", m)
	}

	lh.now = func() time.Time { return fireEpoch.Add(2 * time.Minute) }
	lh.ticks <- fireEpoch
	lh.awaitTurnEnd(t)
	lh.stop()
	<-lh.done
	m := lh.z.moved()
	if len(m) != 1 || !strings.HasSuffix(m[0], ":✔ work:change_all") {
		t.Fatalf("moves = %v, want the resolve after the fire", m)
	}
}

// TestScheduleOutsideATurnFlipsTheMark: arming outside a turn
// unresolves a resolved topic, and removing the last schedule resolves
// it. This is also the override of a human's manual resolve.
func TestScheduleOutsideATurnFlipsTheMark(t *testing.T) {
	lh := statusHarness(t)
	lh.seedTrigger("work")
	lh.deliverTurn(t, "work", mention("go"))
	id := lh.j.Convs()[0].ID
	lh.clearMoves()

	it := mustSchedule(t, lh.h, chanToken("work"), "wake", fireEpoch.Add(time.Hour))
	if got := lh.convTopic(t, id); got != "work" {
		t.Fatalf("topic = %q after arming, want unresolved", got)
	}
	// The token keeps working although the topic string moved.
	if err := lh.h.Unschedule(chanToken("✔ work"), it.ID); err != nil {
		t.Fatalf("Unschedule: %v", err)
	}
	if got := lh.convTopic(t, id); got != "✔ work" {
		t.Fatalf("topic = %q after the last schedule went, want resolved", got)
	}
	if m := lh.z.moved(); len(m) != 2 {
		t.Fatalf("moves = %v", m)
	}
}

// TestManualResolveIsFollowedAndOverridden: a human's resolve arrives
// as update_message; the journal follows it without a new conversation,
// and the next state change puts the mark back to what the work says.
func TestManualResolveIsFollowedAndOverridden(t *testing.T) {
	lh := statusHarness(t)
	lh.seedTrigger("work")
	lh.armDuring(func() { mustSchedule(t, lh.h, chanToken("work"), "wake", fireEpoch.Add(time.Hour)) })
	lh.deliverTurn(t, "work", mention("go"))
	id := lh.j.Convs()[0].ID

	lh.h.Handle(context.Background(), zulipproto.Event{Type: zulipproto.EventUpdateMessage, StreamID: 4, OrigTopic: "work", Topic: "✔ work"})
	if got := lh.convTopic(t, id); got != "✔ work" {
		t.Fatalf("topic = %q, the journal did not follow the human's resolve", got)
	}
	mustSchedule(t, lh.h, chanToken("work"), "second", fireEpoch.Add(2*time.Hour))
	if got := lh.convTopic(t, id); got != "work" {
		t.Fatalf("topic = %q, a pending schedule must override the manual resolve", got)
	}
	if n := len(lh.j.Convs()); n != 1 {
		t.Fatalf("%d conversations, want 1", n)
	}
}

// TestMissedResolveIsTakenFromTheMessage: when the event of a resolve
// was missed, the message's own topic corrects the journal, so the turn
// still posts to the topic as it is.
func TestMissedResolveIsTakenFromTheMessage(t *testing.T) {
	lh := newLoopHarness(t, newAgent("done"), nil)
	lh.deliverTurn(t, "work", mention("go"))
	lh.z.reset()
	lh.deliverTurn(t, "✔ work", "again")
	convs := lh.j.Convs()
	if len(convs) != 1 || convs[0].Topic != "✔ work" {
		t.Fatalf("journal = %+v", convs)
	}
	for _, topic := range lh.z.topics {
		if topic != "✔ work" {
			t.Fatalf("answer went to %q", topic)
		}
	}
}

// TestTopicStatusOffMovesNothing: the option is the whole feature.
func TestTopicStatusOffMovesNothing(t *testing.T) {
	lh := newLoopHarness(t, newAgent("done"), nil)
	lh.deliverTurn(t, "work", mention("go"))
	if m := lh.z.moved(); len(m) != 0 {
		t.Fatalf("moves = %v", m)
	}
	lh.h.settleStatusKey(chanToken("work"), 0)
	if m := lh.z.moved(); len(m) != 0 {
		t.Fatalf("moves = %v", m)
	}
}

// TestTopicStatusSkipsDMsAndGeneralChat: neither has a topic to mark.
func TestTopicStatusSkipsDMsAndGeneralChat(t *testing.T) {
	lh := statusHarness(t)
	lh.h.Handle(context.Background(), dmEvent(humanID, "hi", humanID, botID))
	lh.awaitTurnEnd(t)
	lh.deliverTurn(t, "", mention("hi"))
	if m := lh.z.moved(); len(m) != 0 {
		t.Fatalf("moves = %v", m)
	}
}

// TestStatusMoveFailuresAreLoggedAndHarmless covers every way a move
// can fail to happen; none may cost the turn or the conversation.
func TestStatusMoveFailuresAreLoggedAndHarmless(t *testing.T) {
	t.Run("move refused", func(t *testing.T) {
		lh := statusHarness(t)
		lh.z.moveErr = errors.New("not allowed")
		lh.deliverTurn(t, "work", mention("go"))
		if !lh.logged("for its status mark") || lh.j.Convs()[0].Topic != "work" {
			t.Fatalf("logs %v, journal %+v", lh.logs, lh.j.Convs())
		}
	})
	t.Run("no anchor", func(t *testing.T) {
		lh := statusHarness(t)
		c, err := lh.j.Ensure(journal.Channel(4, "✔ quiet"))
		if err != nil {
			t.Fatal(err)
		}
		lh.z.getErr = errors.New("down")
		got := lh.h.markBusy(context.Background(), c, 1)
		if got.Topic != "✔ quiet" || !lh.logged("no message to move topic") {
			t.Fatalf("conv %+v, logs %v", got, lh.logs)
		}
	})
	t.Run("journal write fails", func(t *testing.T) {
		lh := statusHarness(t)
		lh.seedTrigger("work")
		lh.deliverTurn(t, "work", mention("go"))
		lh.clearMoves()
		lh.breakJournal(t)
		lh.seedTrigger("✔ work")
		lh.h.Handle(context.Background(), channelEvent(humanID, "✔ work", "again"))
		lh.awaitTurnEnd(t)
		if !lh.logged("journal did not follow") {
			t.Fatalf("logs %v", lh.logs)
		}
	})
}

// TestMarkBusyIgnoresUnknownAndRetired: nothing to unresolve.
func TestMarkBusyIgnoresUnknownAndRetired(t *testing.T) {
	lh := statusHarness(t)
	ghost := journal.Conv{ID: "nope", Key: journal.Channel(4, "✔ x")}
	if got := lh.h.markBusy(context.Background(), ghost, 0); got.ID != "nope" {
		t.Fatalf("got %+v", got)
	}
	c, _ := lh.j.Ensure(journal.Channel(4, "✔ x"))
	prev, _, _, _ := lh.j.Retire(c.Key)
	if got := lh.h.markBusy(context.Background(), prev, 0); got.ID != prev.ID {
		t.Fatalf("got %+v", got)
	}
	lh.h.settleStatus("nope", 0)
	lh.h.settleStatusKey("garbage", 0)
	if m := lh.z.moved(); len(m) != 0 {
		t.Fatalf("moves = %v", m)
	}
}

// TestStatusAnchorMustBeInTheTopic: a trigger outside the topic (a
// `!branch` in the origin topic) must never be the anchor, or change_all
// would move the origin topic.
func TestStatusAnchorMustBeInTheTopic(t *testing.T) {
	lh := statusHarness(t)
	lh.seedTrigger("origin")
	c, _ := lh.j.Ensure(journal.Channel(4, "✔ branch"))
	lh.h.rememberOwn(c.ID, 7)
	got := lh.h.markBusy(context.Background(), c, 1)
	if m := lh.z.moved(); !slices.Equal(m, []string{"7:branch:change_all"}) || got.Topic != "branch" {
		t.Fatalf("moves %v, conv %+v", m, got)
	}
}

// TestNewSessionTopicIsResolvedToo: `new_session` swaps the
// conversation at the end of the turn; the fresh one is idle, and the
// topic is resolved by the old conversation's last message.
func TestNewSessionTopicIsResolvedToo(t *testing.T) {
	lh := statusHarness(t)
	lh.seedTrigger("work")
	lh.deliverTurn(t, "work", mention("go"))
	conv := lh.j.Convs()[0]
	if _, err := callTool(t, lh.tools, relaytool.ToolNewSession, conv.ID, `{}`); err != nil {
		t.Fatalf("new_session: %v", err)
	}
	lh.clearMoves()
	lh.seedTrigger("✔ work")
	lh.deliverTurn(t, "✔ work", "carry on")
	fresh, _ := lh.j.Lookup(journal.Channel(4, "work"))
	if fresh.ID == conv.ID || fresh.Topic != "✔ work" {
		t.Fatalf("fresh = %+v, moves %v, logs %v", fresh, lh.z.moved(), lh.logs)
	}
}

// TestMarkBusyTakesTheTopicFromTheServer: the triggering message, read
// back from the server, names the topic as it is now — whatever the
// journal or the message event say.
func TestMarkBusyTakesTheTopicFromTheServer(t *testing.T) {
	t.Run("missed resolve", func(t *testing.T) {
		lh := statusHarness(t)
		c, _ := lh.j.Ensure(journal.Channel(4, "work"))
		lh.seedTrigger("✔ work")
		got := lh.h.markBusy(context.Background(), c, 1)
		if m := lh.z.moved(); !slices.Equal(m, []string{"1:work:change_all"}) || got.Topic != "work" {
			t.Fatalf("moves %v, conv %+v", m, got)
		}
	})
	t.Run("already unresolved", func(t *testing.T) {
		lh := statusHarness(t)
		c, _ := lh.j.Ensure(journal.Channel(4, "✔ work"))
		lh.seedTrigger("work")
		got := lh.h.markBusy(context.Background(), c, 1)
		if m := lh.z.moved(); len(m) != 0 || got.Topic != "work" {
			t.Fatalf("moves %v, conv %+v", m, got)
		}
	})
}

// TestSettleChecksItsAnchorToo: the settle step's candidate is checked
// like markBusy's, and a message outside the topic is never moved.
func TestSettleChecksItsAnchorToo(t *testing.T) {
	lh := statusHarness(t)
	lh.seedTrigger("origin")
	c, _ := lh.j.Ensure(journal.Channel(4, "work"))
	lh.h.rememberOwn(c.ID, 7)
	lh.h.settleStatus(c.ID, 1)
	if m := lh.z.moved(); !slices.Equal(m, []string{"7:✔ work:change_all"}) {
		t.Fatalf("moves %v", m)
	}
}
