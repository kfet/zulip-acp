package handler

import (
	"context"
	"time"
	"unicode/utf8"

	"errors"
	"github.com/kfet/acp-kit/schedule"
	"strings"
	"testing"

	"github.com/kfet/zulip-acp/internal/journal"
	"github.com/kfet/zulip-acp/internal/zulipproto"
)

func repeatLoop(t *testing.T, chunk string, err error) *loopHarness {
	lh := silentLoop(t)
	lh.a.mu.Lock()
	lh.a.chunks = []string{chunk}
	lh.a.err = err
	lh.a.mu.Unlock()
	lh.z.mu.Lock()
	lh.z.liveHistory = true
	lh.z.mu.Unlock()
	return lh
}

// fireAny is fire for a turn that may fail.
func (lh *loopHarness) fireAny(id string) {
	_ = lh.h.FireSchedule(context.Background(),
		schedule.Item{ID: id, Conv: chanToken("hb"), Text: "check", Depth: 1, Every: time.Minute})
}

func TestRepeatHashNormalises(t *testing.T) {
	a := repeatHash("402 Budget exceeded at 03:10 (request-id: abc) 0b8e2c4a-1111-2222-3333-444455556666")
	b := repeatHash("402 Budget  exceeded at 4:22:01 (request-id: xyz) 9f8e2c4a-1111-2222-3333-444455556666")
	if a != b {
		t.Fatal("hashes differ")
	}
	if a == repeatHash("402 Budget exceeded at 03:10 (request-id: abc) more") {
		t.Fatal("hash ignores content")
	}
}

// TestScheduledRepeatCollapses is the regression: a schedule that gave
// the same answer each fire posted a new message each time.
func TestScheduledRepeatCollapses(t *testing.T) {
	lh := repeatLoop(t, "build red (request-id: r1)", nil)
	lh.fire(t, "hb", "s1", "check")
	if n := len(lh.z.stored()); n != 1 {
		t.Fatalf("first fire stored %d", n)
	}
	id := lh.z.order[0]
	hb, _ := lh.j.Heartbeat("s1")
	if hb.MsgID != id || hb.Count != 1 || hb.Hash == "" {
		t.Fatalf("journal = %+v", hb)
	}

	lh.a.mu.Lock()
	lh.a.chunks = []string{"build red (request-id: r2)"}
	lh.a.leaf = "L2"
	lh.a.mu.Unlock()
	lh.fire(t, "hb", "s1", "check")
	got := lh.z.stored()
	if len(got) != 1 || !strings.Contains(got[0], "r2") || !strings.Contains(got[0], "×2 · last 09:05") {
		t.Fatalf("stored = %q", got)
	}
	if hb, _ := lh.j.Heartbeat("s1"); hb.MsgID != id || hb.Count != 2 {
		t.Fatalf("journal = %+v", hb)
	}
	// The kept message keys the newer turn, so a branch from it forks
	// at the turn that wrote its text.
	if c, _ := lh.j.Lookup(journal.Channel(4, "hb")); lh.j.TurnLeaf(c.ID, id) != "L2" {
		t.Fatal("kept message does not key the newer turn")
	}

	// A heartbeat never overwrites an answer record's message.
	lh.a.mu.Lock()
	lh.a.chunks = []string{"<<SILENT>>"}
	lh.a.mu.Unlock()
	lh.fire(t, "hb", "s1", "check")
	if got := lh.z.stored(); len(got) != 2 || !strings.Contains(got[0], "×2") {
		t.Fatalf("stored = %q", got)
	}
}

func TestScheduledRepeatNotNewestOrEditFails(t *testing.T) {
	lh := repeatLoop(t, "same", nil)
	lh.fire(t, "hb", "s1", "check")
	id := lh.z.order[0]
	// Somebody posted after it: a new message.
	lh.z.mu.Lock()
	lh.z.liveHistory = false
	lh.z.mu.Unlock()
	lh.z.setHistory(zulipproto.Message{ID: id + 100})
	lh.fire(t, "hb", "s1", "check")
	lh.z.mu.Lock()
	lh.z.liveHistory = true
	lh.z.mu.Unlock()
	if n := len(lh.z.stored()); n != 2 {
		t.Fatalf("stored %d", n)
	}
	id2 := lh.z.order[1]
	if hb, _ := lh.j.Heartbeat("s1"); hb.MsgID != id2 || hb.Count != 1 {
		t.Fatalf("journal = %+v", hb)
	}
	// The edit fails: a new message.
	lh.z.mu.Lock()
	lh.z.editErrFor = id2
	lh.z.mu.Unlock()
	lh.fireAny("s1")
	if !lh.logged("editing repeated answer") {
		t.Fatal("edit failure not logged")
	}
}

func TestScheduledRepeatErrorCollapses(t *testing.T) {
	lh := repeatLoop(t, "", errors.New("402 Budget exceeded (request-id: 0b8e2c4a-1111-2222-3333-444455556666)"))
	lh.fireAny("s1")
	if n := len(lh.z.stored()); n != 1 {
		t.Fatalf("stored %d", n)
	}
	id := lh.z.order[0]
	lh.a.mu.Lock()
	lh.a.err = errors.New("402 Budget exceeded (request-id: 1b8e2c4a-1111-2222-3333-444455556666)")
	lh.a.mu.Unlock()
	lh.fireAny("s1")
	got := lh.z.stored()
	if len(got) != 1 || lh.z.order[0] != id || !strings.Contains(got[0], "×2") {
		t.Fatalf("stored = %q", got)
	}
}

// TestScheduledRepeatOverBudgetPosts: the "×N" note must never push an
// edited answer past the message limit, which Zulip truncates silently.
func TestScheduledRepeatOverBudgetPosts(t *testing.T) {
	lh := repeatLoop(t, strings.Repeat("same ", 60), nil)
	lh.h.cfg.Budget = utf8.RuneCountInString(lh.firstBody(t)) + 3
	lh.fire(t, "hb", "s1", "check")
	lh.fire(t, "hb", "s1", "check")
	if !lh.logged("over the message limit") {
		t.Fatalf("stored = %q", lh.z.stored())
	}
}

// firstBody fires once in a scratch topic and returns the answer body.
func (lh *loopHarness) firstBody(t *testing.T) string {
	lh.engage(t, "scratch")
	lh.fire(t, "scratch", "s0", "check")
	got := lh.z.stored()
	return got[len(got)-1]
}

// TestIsNewestAllSkipped: a read that holds only skipped messages has
// no newest message to compare.
func TestIsNewestAllSkipped(t *testing.T) {
	lh := silentLoop(t)
	lh.z.setHistory(zulipproto.Message{ID: 7})
	if lh.h.isNewest(context.Background(), journal.Channel(4, "hb"), 7, 7) {
		t.Fatal("skipped message read as newest")
	}
}
