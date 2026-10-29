package handler

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/kfet/zulip-acp/internal/zulipproto"
)

// echoTimer makes the echo wait end only when the test fires it.
func echoTimer(c *Config) chan time.Time {
	ch := make(chan time.Time)
	c.EchoAfter = func(d time.Duration) <-chan time.Time {
		if d != reactEchoTimeout {
			panic(fmt.Sprintf("echo wait of %s", d))
		}
		return ch
	}
	return ch
}

// echo feeds the relay's own message id back as a live event.
func (hh *harness) echo(id int64) {
	hh.h.Handle(context.Background(), zulipproto.Event{
		Type: zulipproto.EventMessage,
		Message: &zulipproto.Message{
			ID: id, SenderID: botID, Content: "x",
			StreamID: 4, Topic: "t", Type: zulipproto.MessageTypeStream,
		},
	})
}

func echoHarness(t *testing.T) (*harness, chan time.Time) {
	t.Helper()
	var fire chan time.Time
	hh := newHarness(t, newAgent("hello"), func(c *Config) { fire = echoTimer(c) })
	return hh, fire
}

// TestReactWhenVisibleWaitsForTheEcho: nothing is added before the
// echo; after it, every emoji is added in order.
func TestReactWhenVisibleWaitsForTheEcho(t *testing.T) {
	hh, _ := echoHarness(t)
	hh.h.reactWhenVisible(context.Background(), 77, "test", nil, "one", "two", "three")
	if added, _ := hh.z.reactions(); len(added) != 0 {
		t.Fatalf("added before the echo: %v", added)
	}
	hh.echo(77)
	hh.h.echoWG.Wait()
	added, _ := hh.z.reactions()
	if want := []string{"77:one", "77:two", "77:three"}; !slices.Equal(added, want) {
		t.Fatalf("added %v, want %v", added, want)
	}
}

// TestReactWhenVisibleTimesOut: with no echo the reactions still land
// once the timer fires.
func TestReactWhenVisibleTimesOut(t *testing.T) {
	hh, fire := echoHarness(t)
	hh.h.reactWhenVisible(context.Background(), 5, "test", nil, "one")
	fire <- time.Time{}
	hh.h.echoWG.Wait()
	if added, _ := hh.z.reactions(); !slices.Equal(added, []string{"5:one"}) {
		t.Fatalf("added %v", added)
	}
	if len(hh.h.ownEcho.waiters) != 0 {
		t.Fatalf("waiter left behind: %v", hh.h.ownEcho.waiters)
	}
}

// TestReactWhenVisibleEchoFirst: an echo seen before the wait starts
// resolves it at once, without the timer.
func TestReactWhenVisibleEchoFirst(t *testing.T) {
	hh, _ := echoHarness(t)
	hh.echo(9)
	hh.echo(9) // a repeat changes nothing
	hh.h.reactWhenVisible(context.Background(), 9, "test", nil, "one")
	hh.h.echoWG.Wait()
	if added, _ := hh.z.reactions(); !slices.Equal(added, []string{"9:one"}) {
		t.Fatalf("added %v", added)
	}
}

// TestOwnEchoWaitersAndEviction: two waiters on one id are both
// released; cancelling one leaves the other; the recent set is bounded.
func TestOwnEchoWaitersAndEviction(t *testing.T) {
	e := newOwnEcho()
	a, cancelA := e.wait(1)
	b, cancelB := e.wait(1)
	cancelA()
	if len(e.waiters[1]) != 1 {
		t.Fatalf("waiters = %d", len(e.waiters[1]))
	}
	e.seen(1)
	<-b
	select {
	case <-a:
		t.Fatal("cancelled waiter released")
	default:
	}
	cancelB()
	for i := int64(2); i <= ownEchoRecent+1; i++ {
		e.seen(i)
	}
	if _, ok := e.recent[1]; ok || len(e.recent) != ownEchoRecent {
		t.Fatalf("recent = %d, oldest kept: %v", len(e.recent), ok)
	}
}

// TestEchoAfterDefaultsToTimeAfter: production wires no echo timer.
func TestEchoAfterDefaultsToTimeAfter(t *testing.T) {
	h := &Handler{}
	if h.echoAfter(time.Nanosecond) == nil {
		t.Fatal("nil timer")
	}
}

// TestArchiveConfirmedDuringEchoWaitIsNotPreReacted: an arm that is
// gone when the echo wait ends gets no pre-added answers.
func TestArchiveConfirmedDuringEchoWaitIsNotPreReacted(t *testing.T) {
	var fire chan time.Time
	hh, own := archiveHarness(t, func(c *Config) { fire = echoTimer(c) })
	conv := archiveConv(t, hh)
	hh.h.Handle(context.Background(), reactionEvent(humanID, own, archiveEmoji, zulipproto.ReactionAdd))
	prompt, ok := hh.armed(conv.ID)
	if !ok {
		t.Fatal("nothing armed")
	}
	hh.h.Handle(context.Background(), reactionEvent(humanID, prompt, archiveEmoji, zulipproto.ReactionAdd))
	if _, still := hh.armed(conv.ID); still {
		t.Fatal("confirmation did not consume the arm")
	}
	fire <- time.Time{}
	hh.h.echoWG.Wait()
	added, _ := hh.z.reactions()
	for _, e := range []string{archiveEmoji, defaultDeleteEmoji} {
		if slices.Contains(added, fmt.Sprintf("%d:%s", prompt, e)) {
			t.Fatalf("pre-added :%s: to a spent arm: %v", e, added)
		}
	}
}

// TestReactToWaitsForAFreshOwnMessage: the react tool on a message the
// relay posted a moment ago waits for its echo.
func TestReactToWaitsForAFreshOwnMessage(t *testing.T) {
	var fire chan time.Time
	hh, origin := toolHarness(t)
	fire = echoTimer(&hh.h.cfg)
	id := hh.plant("Planning", "pick one", botID, botName)
	hh.z.mu.Lock()
	m := hh.z.messages[id]
	m.Timestamp = hh.h.now().Unix()
	hh.z.messages[id] = m
	hh.z.mu.Unlock()
	done := make(chan error)
	go func() { done <- hh.h.ReactTo(origin.ID, id, "one") }()
	fire <- time.Time{}
	if err := <-done; err != nil {
		t.Fatalf("ReactTo: %v", err)
	}
	if got, want := hh.lastReaction(), fmt.Sprintf("%d:one", id); got != want {
		t.Fatalf("reaction = %q, want %q", got, want)
	}
}

// TestReactWhenVisibleSettles: after the echo, no reaction is added
// until the settle timer fires.
func TestReactWhenVisibleSettles(t *testing.T) {
	settle := make(chan time.Time)
	asked := make(chan time.Duration, 1)
	hh := newHarness(t, newAgent("hello"), func(c *Config) {
		c.ReactSettle = 7 * time.Millisecond
		c.EchoAfter = func(d time.Duration) <-chan time.Time {
			if d == reactEchoTimeout {
				return make(chan time.Time)
			}
			asked <- d
			return settle
		}
	})
	hh.h.reactWhenVisible(context.Background(), 3, "test", nil, "one")
	hh.echo(3)
	if d := <-asked; d != 7*time.Millisecond {
		t.Fatalf("settle of %s", d)
	}
	if added, _ := hh.z.reactions(); len(added) != 0 {
		t.Fatalf("added before the settle: %v", added)
	}
	settle <- time.Time{}
	hh.h.echoWG.Wait()
	if added, _ := hh.z.reactions(); !slices.Equal(added, []string{"3:one"}) {
		t.Fatalf("added %v", added)
	}
}
