package handler

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kfet/zulip-acp/internal/journal"
	"github.com/kfet/zulip-acp/internal/zulipproto"
)

type fakeMark struct {
	id  int64
	err error
}

func (m *fakeMark) Get() int64 { return m.id }
func (m *fakeMark) Set(id int64) error {
	if m.err != nil {
		return m.err
	}
	if id > m.id {
		m.id = id
	}
	return nil
}

var catchupNow = time.Unix(1_800_000_000, 0)

func catchupMsg(id, sender int64, topic, content string, age time.Duration) zulipproto.Message {
	return zulipproto.Message{
		ID: id, SenderID: sender, SenderName: "Kfet", Content: content,
		StreamID: 4, Topic: topic, Type: zulipproto.MessageTypeStream,
		Timestamp: catchupNow.Add(-age).Unix(),
	}
}

func catchupHarness(t *testing.T, mark *fakeMark) *harness {
	return newHarness(t, newAgent("ok"), func(c *Config) {
		c.Now = func() time.Time { return catchupNow }
		if mark != nil {
			c.Mark = mark
		}
	})
}

func (hh *harness) idle(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := hh.h.WaitIdle(ctx); err != nil {
		t.Fatalf("turn did not finish: %v", err)
	}
}

// TestCatchUpCollapsesTopic: the missed messages of one topic become
// ONE turn with the catch-up header; commands, widgets and the bot's
// own messages are left out; the `!!` escape is still prose.
func TestCatchUpCollapsesTopic(t *testing.T) {
	hh := catchupHarness(t, nil)
	msgs := []zulipproto.Message{
		catchupMsg(10, humanID, "t", mention("first"), time.Hour),
		catchupMsg(11, humanID, "t", "!new", 50*time.Minute),
		catchupMsg(12, botID, "t", "my own", 40*time.Minute),
		catchupMsg(13, humanID, "t", "/poll x", 30*time.Minute),
		catchupMsg(14, humanID, "t", "!!bang", 20*time.Minute),
		catchupMsg(15, humanID, "t", "second", 10*time.Minute),
		// Never engaged and not addressed: gated out.
		catchupMsg(16, humanID, "other", "chatter", time.Minute),
	}
	hh.h.CatchUp(context.Background(), msgs, 24*time.Hour)
	hh.idle(t)
	p := hh.a.prompted()
	if len(p) != 1 {
		t.Fatalf("prompts = %d (%q), want 1", len(p), p)
	}
	got := p[0]
	first := catchupNow.Add(-time.Hour).UTC().Format(time.RFC3339)
	for _, want := range []string{
		"[catch-up] 3 messages arrived while the relay was offline (first at " + first + ")",
		"[Kfet] first", "[Kfet] !bang", "[Kfet] second",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("prompt %q lacks %q", got, want)
		}
	}
	for _, bad := range []string{"!new", "my own", "/poll", "chatter"} {
		if strings.Contains(got, bad) {
			t.Fatalf("prompt %q carries %q", got, bad)
		}
	}
	if !hh.logged("catch-up turn in") {
		t.Fatal("catch-up turn not logged")
	}
}

// TestCatchUpTooOld: a message older than the limit is not answered,
// creates nothing, and its topic gets one notice line.
func TestCatchUpTooOld(t *testing.T) {
	hh := catchupHarness(t, nil)
	msgs := []zulipproto.Message{
		catchupMsg(10, humanID, "t", mention("old one"), 48*time.Hour),
		catchupMsg(11, humanID, "t", mention("old two"), 30*time.Hour),
		catchupMsg(12, humanID, "u", mention("old three"), 30*time.Hour),
	}
	hh.h.CatchUp(context.Background(), msgs, 24*time.Hour)
	hh.idle(t)
	if n := len(hh.a.prompted()); n != 0 {
		t.Fatalf("prompts = %d, want 0", n)
	}
	got := strings.Join(hh.z.stored(), "\n")
	for _, want := range []string{
		"[catch-up] 2 messages older than 24h arrived while the relay was offline and were not answered.",
		"[catch-up] 1 message older than 24h arrived while the relay was offline and was not answered.",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("posted %q lacks %q", got, want)
		}
	}
	if _, ok := hh.j.Lookup(catchupKey("t")); ok {
		t.Fatal("a skipped message created a conversation")
	}
}

// TestCatchUpCap: a topic with more than MaxCatchupPerTopic missed
// messages keeps the newest and says how many it left out.
func TestCatchUpCap(t *testing.T) {
	hh := catchupHarness(t, nil)
	var msgs []zulipproto.Message
	n := MaxCatchupPerTopic + 3
	for i := 0; i < n; i++ {
		msgs = append(msgs, catchupMsg(int64(100+i), humanID, "t", mention(fmt.Sprintf("m%03d", i)), time.Hour))
	}
	hh.h.CatchUp(context.Background(), msgs, 24*time.Hour)
	hh.idle(t)
	p := hh.a.prompted()
	if len(p) != 1 {
		t.Fatalf("prompts = %d", len(p))
	}
	if !strings.Contains(p[0], fmt.Sprintf("[catch-up] %d messages", n)) ||
		!strings.Contains(p[0], "the oldest 3 are left out") ||
		strings.Contains(p[0], "m002") || !strings.Contains(p[0], "m003") {
		t.Fatalf("prompt = %q", p[0])
	}
}

// TestMarkDedupAndAdvance: a live message at or below the mark is
// dropped as already handled; a newer one is handled and moves the
// mark. A failed mark write is logged, not fatal.
func TestMarkDedupAndAdvance(t *testing.T) {
	mark := &fakeMark{id: 5}
	hh := catchupHarness(t, mark)
	ev := channelEvent(humanID, "t", mention("hi"))
	ev.Message.ID = 5
	hh.h.Handle(context.Background(), ev)
	hh.idle(t)
	if len(hh.a.prompted()) != 0 {
		t.Fatal("a message at the mark was handled again")
	}
	ev = channelEvent(humanID, "t", mention("hi"))
	ev.Message.ID = 6
	hh.h.Handle(context.Background(), ev)
	hh.idle(t)
	if len(hh.a.prompted()) != 1 || mark.id != 6 {
		t.Fatalf("prompts = %d, mark = %d", len(hh.a.prompted()), mark.id)
	}
	mark.err = errors.New("disk full")
	ev = channelEvent(humanID, "t", "ignored? no, engaged")
	ev.Message.ID = 7
	hh.h.Handle(context.Background(), ev)
	hh.idle(t)
	if !hh.logged("catch-up mark: disk full") {
		t.Fatal("mark failure not logged")
	}
}

func TestCatchupHelpers(t *testing.T) {
	if fmtAge(24*time.Hour) != "24h" || fmtAge(90*time.Second) != "1m30s" || fmtAge(time.Minute) != "1m" {
		t.Fatalf("fmtAge: %s %s %s", fmtAge(24*time.Hour), fmtAge(90*time.Second), fmtAge(time.Minute))
	}
	if p, skip := catchupText("hello"); skip || p != "hello" {
		t.Fatalf("catchupText(hello) = %q, %v", p, skip)
	}
}

func catchupKey(topic string) journal.Key { return journal.Channel(4, topic) }

// TestCatchUpBoundedTurns: with one slot, the second topic's turn
// starts only after the first finishes, and WaitIdle counts the
// waiting turn as in flight.
func TestCatchUpBoundedTurns(t *testing.T) {
	agent := newAgent("ok")
	agent.block = make(chan struct{})
	hh := newHarness(t, agent, func(c *Config) {
		c.Now = func() time.Time { return catchupNow }
		c.CatchupTurns = 1
	})
	msgs := []zulipproto.Message{
		catchupMsg(10, humanID, "a", mention("one"), time.Hour),
		catchupMsg(11, humanID, "b", mention("two"), time.Hour),
	}
	hh.h.CatchUp(context.Background(), msgs, 24*time.Hour)
	<-agent.entered
	done, cancel := context.WithCancel(context.Background())
	cancel()
	if err := hh.h.WaitIdle(done); err == nil {
		t.Fatal("WaitIdle returned while a catch-up turn was waiting for a slot")
	}
	close(agent.block)
	hh.idle(t)
	if n := len(agent.prompted()); n != 2 {
		t.Fatalf("prompts = %d, want 2", n)
	}
}

// TestCatchUpShutdown: a shutdown while turns wait for a slot starts
// no more of them, and says how many were left.
func TestCatchUpShutdown(t *testing.T) {
	agent := newAgent("ok")
	agent.block = make(chan struct{})
	hh := newHarness(t, agent, func(c *Config) {
		c.Now = func() time.Time { return catchupNow }
		c.CatchupTurns = 1
	})
	ctx, cancel := context.WithCancel(context.Background())
	msgs := []zulipproto.Message{
		catchupMsg(10, humanID, "a", mention("one"), time.Hour),
		catchupMsg(11, humanID, "b", mention("two"), time.Hour),
	}
	hh.h.CatchUp(ctx, msgs, 24*time.Hour)
	<-agent.entered
	cancel()
	hh.h.catchups.Wait()
	close(agent.block)
	hh.idle(t)
	if n := len(agent.prompted()); n != 1 {
		t.Fatalf("prompts = %d, want 1", n)
	}
	if !hh.logged("1 catch-up turn(s) not started") {
		t.Fatal("unstarted turns not logged")
	}
}

// TestCatchUpYieldsToLive: a catch-up turn that waited for a slot is
// dropped when a live message in its topic started a turn meanwhile —
// starting it would cancel the live answer.
func TestCatchUpYieldsToLive(t *testing.T) {
	agent := newAgent("ok")
	agent.block = make(chan struct{})
	hh := newHarness(t, agent, func(c *Config) {
		c.Now = func() time.Time { return catchupNow }
		c.CatchupTurns = 1
	})
	msgs := []zulipproto.Message{
		catchupMsg(10, humanID, "a", mention("one"), time.Hour),
		catchupMsg(11, humanID, "b", mention("two"), time.Hour),
	}
	hh.h.CatchUp(context.Background(), msgs, 24*time.Hour)
	<-agent.entered
	ev := channelEvent(humanID, "b", mention("live"))
	ev.Message.ID = 20
	hh.h.Handle(context.Background(), ev)
	<-agent.entered
	close(agent.block)
	hh.idle(t)
	p := agent.prompted()
	if len(p) != 2 || !strings.Contains(p[1], "live") {
		t.Fatalf("prompts = %q", p)
	}
	if !hh.logged("dropped: a newer message already started a turn") {
		t.Fatal("drop not logged")
	}
}
