package handler

import (
	"errors"
	"testing"

	"github.com/kfet/zulip-acp/internal/journal"
)

// TestTurnRecordsItsLeaf: a finished turn files the agent's leaf id
// under its prompt and first reply message.
func TestTurnRecordsItsLeaf(t *testing.T) {
	agent := newAgent("hello")
	agent.leaf = "leaf-1"
	hh := newHarness(t, agent, nil)
	hh.deliver(t, "t", mention("hi"))
	conv, _ := hh.j.Lookup(journal.Channel(4, "t"))
	if len(conv.Turns) != 1 {
		t.Fatalf("turns = %+v", conv.Turns)
	}
	got := conv.Turns[0]
	if got.PromptID != 1 || got.ReplyID == 0 || got.Leaf != "leaf-1" {
		t.Fatalf("turn = %+v", got)
	}
}

// TestAbstainedTurnRecordsItsLeaf: the prompt reached the session, so
// the turn has a leaf even though nothing was posted.
func TestAbstainedTurnRecordsItsLeaf(t *testing.T) {
	agent := newAgent("hello")
	hh := newHarness(t, agent, nil)
	hh.deliver(t, "t", mention("hi"))
	agent.mu.Lock()
	agent.chunks = []string{"<<SILENT>>"}
	agent.leaf = "leaf-quiet"
	agent.mu.Unlock()
	hh.deliver(t, "t", "chatter")
	conv, _ := hh.j.Lookup(journal.Channel(4, "t"))
	if n := len(conv.Turns); n != 1 || conv.Turns[0] != (journal.Turn{PromptID: 1, Leaf: "leaf-quiet"}) {
		t.Fatalf("turns = %+v", conv.Turns)
	}
}

// TestRecordTurnFailureIsLogged: a journal write that fails costs only
// the fork point, and says so.
func TestRecordTurnFailureIsLogged(t *testing.T) {
	hh := newHarness(t, newAgent("x"), nil)
	hh.h.recordTurn("missing", 1, 2, "leaf")
	if !hh.logged("recording turn leaf for missing") {
		t.Fatalf("logs = %v", hh.logs)
	}
}

// TestBranchForksAtTheTurnOfTheMessage: a :fork_and_knife: on an
// earlier message forks the origin at the leaf of the turn that holds
// it — and a message between two turns maps to the earlier one.
func TestBranchForksAtTheTurnOfTheMessage(t *testing.T) {
	for _, tc := range []struct {
		name string
		pick func(first, between, second int64) int64
		want string
	}{
		{"prompt of the first turn", func(f, _, _ int64) int64 { return f }, "L1"},
		{"message after the first turn", func(_, b, _ int64) int64 { return b }, "L1"},
		{"prompt of the second turn", func(_, _, s int64) int64 { return s }, "L2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hh := branchReactHarness(t)
			origin, _ := hh.j.Lookup(journal.Channel(4, "planning"))
			first := hh.plant("planning", "first idea", 99, "Grace Hopper")
			if err := hh.j.RecordTurn(origin.ID, journal.Turn{PromptID: first, ReplyID: first + 1, Leaf: "L1"}); err != nil {
				t.Fatal(err)
			}
			hh.plant("planning", "reply", botID, "bot")
			between := hh.plant("planning", "an aside", 99, "Grace Hopper")
			second := hh.plant("planning", "second idea", 99, "Grace Hopper")
			if err := hh.j.RecordTurn(origin.ID, journal.Turn{PromptID: second, Leaf: "L2"}); err != nil {
				t.Fatal(err)
			}
			hh.fork(t, humanID, tc.pick(first, between, second))
			forks := hh.a.forked()
			if len(forks) != 1 || forks[0].At != tc.want {
				t.Fatalf("forks = %+v, want at %q", forks, tc.want)
			}
		})
	}
}

// TestBranchBeforeAnyKnownTurnForksAtTheLeaf: with no turn at or before
// the message, the fork falls back to an empty at.
func TestBranchBeforeAnyKnownTurnForksAtTheLeaf(t *testing.T) {
	hh := branchReactHarness(t)
	origin, _ := hh.j.Lookup(journal.Channel(4, "planning"))
	early := hh.plant("planning", "early idea", 99, "Grace Hopper")
	if err := hh.j.RecordTurn(origin.ID, journal.Turn{PromptID: early + 100, Leaf: "later"}); err != nil {
		t.Fatal(err)
	}
	hh.fork(t, humanID, early)
	if forks := hh.a.forked(); len(forks) != 1 || forks[0].At != "" {
		t.Fatalf("forks = %+v", forks)
	}
}

// TestBranchRetriesAtTheLeafWhenTheAtIsRejected: a leaf from an earlier
// session of the origin is refused; the fork is retried at the session
// leaf instead of being dropped.
func TestBranchRetriesAtTheLeafWhenTheAtIsRejected(t *testing.T) {
	hh := branchReactHarness(t)
	hh.a.forkAtErr = errors.New("unknown entry")
	origin, _ := hh.j.Lookup(journal.Channel(4, "planning"))
	id := hh.plant("planning", "an idea", 99, "Grace Hopper")
	if err := hh.j.RecordTurn(origin.ID, journal.Turn{PromptID: id, Leaf: "stale"}); err != nil {
		t.Fatal(err)
	}
	hh.fork(t, humanID, id)
	forks := hh.a.forked()
	if len(forks) != 2 || forks[0].At != "stale" || forks[1].At != "" {
		t.Fatalf("forks = %+v", forks)
	}
	if !hh.logged("at stale failed (unknown entry); forking at its leaf") || !hh.logged("forked session") {
		t.Fatalf("logs = %v", hh.logs)
	}
}
