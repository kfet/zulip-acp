package handler

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	acp "github.com/coder/acp-go-sdk"
	"github.com/kfet/acp-kit/client"
	"github.com/kfet/zulip-acp/internal/journal"
)

// engagedOrigin starts a conversation in "planning" so a branch has an
// origin session to fork, and returns it.
func engagedOrigin(t *testing.T, hh *harness) journal.Conv {
	t.Helper()
	hh.deliver(t, "planning", mention("hello"))
	conv, ok := hh.j.Lookup(journal.Channel(4, "planning"))
	if !ok {
		t.Fatal("origin not engaged")
	}
	return conv
}

func branchedConv(t *testing.T, hh *harness, topic string) journal.Conv {
	t.Helper()
	conv, ok := hh.j.Lookup(journal.Channel(4, topic))
	if !ok {
		t.Fatalf("no branched conversation in %q", topic)
	}
	return conv
}

// TestBranchForksTheOriginSession: the branch forks the origin's live
// session at the leaf, into the CHILD's directory, and keeps the origin
// record in the journal.
func TestBranchForksTheOriginSession(t *testing.T) {
	hh := branchHarness(t)
	origin := engagedOrigin(t, hh)
	hh.branch(t, "planning", "!branch rework the splitter")

	child := branchedConv(t, hh, "rework the splitter")
	forks := hh.a.forked()
	if len(forks) != 1 {
		t.Fatalf("forks = %+v", forks)
	}
	want := forkCall{Cwd: filepath.Join(hh.s.dir, convsDir, child.ID), Parent: acp.SessionId("sid-" + origin.ID)}
	if forks[0] != want {
		t.Fatalf("fork = %+v, want %+v", forks[0], want)
	}
	if fi, err := os.Stat(want.Cwd); err != nil || !fi.IsDir() {
		t.Fatalf("child cwd not created: %v", err)
	}
	if child.Parent == nil || child.Parent.Key.Topic != origin.Key.Topic {
		t.Fatalf("origin record changed: %+v", child.Parent)
	}
	if !hh.logged("forked session fork-of-sid-" + origin.ID) {
		t.Fatalf("logs = %v", hh.logs)
	}
}

// TestBranchFromAnUnengagedTopicDoesNotFork: a human-only topic has no
// session, so the branch starts fresh.
func TestBranchFromAnUnengagedTopicDoesNotFork(t *testing.T) {
	hh := branchHarness(t)
	hh.branch(t, "planning", "!branch rework the splitter")
	branchedConv(t, hh, "rework the splitter")
	if f := hh.a.forked(); len(f) != 0 {
		t.Fatalf("forks = %+v", f)
	}
}

// TestBranchForksAReapedOrigin: an origin that idle GC dropped is found
// on disk, the way the manager would resume it, and forked.
func TestBranchForksAReapedOrigin(t *testing.T) {
	hh := branchHarness(t)
	origin := engagedOrigin(t, hh)
	hh.s.mu.Lock()
	delete(hh.s.sessions, origin.ID)
	hh.s.mu.Unlock()
	hh.a.listed = []client.SessionInfo{{SessionId: "newest"}, {SessionId: "older"}}
	hh.branch(t, "planning", "!branch rework the splitter")

	forks := hh.a.forked()
	if len(forks) != 1 || forks[0].Parent != "newest" {
		t.Fatalf("forks = %+v", forks)
	}
	hh.s.mu.Lock()
	_, revived := hh.s.sessions[origin.ID]
	hh.s.mu.Unlock()
	if revived {
		t.Fatal("the origin session was reopened just to fork it")
	}
	if p := hh.a.prompted(); !strings.Contains(p[len(p)-1], "you already carry its context") {
		t.Fatalf("prompt = %q", p[len(p)-1])
	}
}

// TestBranchFallsBackToAFreshSession covers every failure: each is
// logged, and the branch still runs its first turn.
func TestBranchFallsBackToAFreshSession(t *testing.T) {
	cases := []struct {
		name  string
		setup func(hh *harness, origin journal.Conv)
		log   string
	}{
		{"unsupported", func(hh *harness, _ journal.Conv) {
			hh.a.forkErr = fmt.Errorf("wrapped: %w", client.ErrForkUnsupported)
		}, "agent cannot fork sessions"},
		{"fork error", func(hh *harness, _ journal.Conv) {
			hh.a.forkErr = errors.New("boom")
		}, "failed (boom)"},
		{"no resume", func(hh *harness, _ journal.Conv) {
			hh.a.noResume = true
		}, "cannot list and resume sessions"},
		{"list error", func(hh *harness, origin journal.Conv) {
			delete(hh.s.sessions, origin.ID)
			hh.a.listErr = errors.New("nope")
		}, "listing the origin's sessions failed (nope)"},
		{"nothing listed", func(hh *harness, origin journal.Conv) {
			delete(hh.s.sessions, origin.ID)
		}, "the origin has no session to fork"},
		{"cwd", func(hh *harness, _ journal.Conv) {
			if err := os.WriteFile(filepath.Join(hh.s.dir, convsDir), nil, 0o644); err != nil {
				t.Fatal(err)
			}
		}, "starting a fresh session"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hh := branchHarness(t)
			origin := engagedOrigin(t, hh)
			before := len(hh.a.prompted())
			tc.setup(hh, origin)
			hh.branch(t, "planning", "!branch rework the splitter")
			branchedConv(t, hh, "rework the splitter")
			if !hh.logged(tc.log) {
				t.Fatalf("logs = %v", hh.logs)
			}
			p := hh.a.prompted()
			if len(p) == before {
				t.Fatal("the branch ran no turn")
			}
			if !strings.Contains(p[len(p)-1], "which you have NOT seen") {
				t.Fatalf("prompt = %q", p[len(p)-1])
			}
		})
	}
}

func TestDiscardSinkDropsUpdates(t *testing.T) {
	if err := (discardSink{}).OnUpdate(t.Context(), acp.SessionNotification{}); err != nil {
		t.Fatal(err)
	}
}
