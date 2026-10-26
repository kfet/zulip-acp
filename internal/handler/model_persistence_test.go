package handler

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	acp "github.com/coder/acp-go-sdk"
	"github.com/kfet/zulip-acp/internal/journal"
)

func restartModelHandler(t *testing.T, hh *harness) {
	t.Helper()
	waitIdle(t, hh)
	h, err := New(hh.h.cfg)
	if err != nil {
		t.Fatal(err)
	}
	hh.h = h
}

func TestModelChoiceSurvivesRelayRestart(t *testing.T) {
	hh := dmCmdHarness(t, withModels(newAgent("answer"), "a/one", "a/one", "b/two"), nil)
	hh.deliverDM(t, humanID, "hello", humanID, botID)
	conv := hh.j.Convs()[0]
	// Restart before the next turn applies the pending provider/model choice.
	hh.deliverDM(t, humanID, "!model b/two", humanID, botID)
	restartModelHandler(t, hh)
	if got, ok := hh.h.modelOverride(conv.ID); !ok || got != "b/two" {
		t.Fatalf("restored choice = %q, %v; want b/two", got, ok)
	}
	hh.deliverDM(t, humanID, "after restart", humanID, botID)
	want := "sid-" + conv.ID + "=b/two"
	if got := hh.a.selections(); len(got) != 1 || got[0] != want {
		t.Fatalf("selections = %v; want [%s]", got, want)
	}
	// A restart must clear the applied marker, even if the session id stays the same.
	restartModelHandler(t, hh)
	hh.deliverDM(t, humanID, "after another restart", humanID, botID)
	if got := hh.a.selections(); len(got) != 2 || got[1] != want {
		t.Fatalf("selections after second restart = %v", got)
	}
	if err := hh.h.convo.Overrides().Set(conv.ID, ""); err != nil {
		t.Fatal(err)
	}
	restartModelHandler(t, hh)
	if got, ok := hh.h.modelOverride(conv.ID); ok {
		t.Fatalf("cleared choice returned after restart: %q", got)
	}
}

func TestBranchCopiesModelChoice(t *testing.T) {
	for _, mode := range []string{"fork", "fork failure", "no resume", "reaped parent", "swept default"} {
		t.Run(mode, func(t *testing.T) {
			hh := branchHarness(t)
			withModels(hh.a, "a/one", "a/one", "b/two")
			origin := engagedOrigin(t, hh)
			hh.deliver(t, "planning", "!model b/two")
			switch mode {
			case "fork failure":
				hh.a.forkErr = errors.New("fork failed")
			case "no resume":
				hh.a.noResume = true
			case "reaped parent":
				hh.s.mu.Lock()
				delete(hh.s.sessions, origin.ID)
				hh.s.mu.Unlock()
			case "swept default":
				hh.h.cfg.QuickModels = []QuickModel{{Model: "a/one", Emoji: "fish"}}
				if err := hh.h.setDefaultModel("a/one"); err != nil {
					t.Fatal(err)
				}
			}
			hh.branch(t, "planning", "!branch model child")
			child := branchedConv(t, hh, "model child")
			for _, id := range []string{origin.ID, child.ID} {
				if got, ok := hh.h.modelOverride(id); !ok || got != "b/two" {
					t.Fatalf("choice for %s = %q, %v; want b/two", id, got, ok)
				}
			}
			want := "sid-" + child.ID + "=b/two"
			if got := hh.a.selections(); len(got) != 1 || got[0] != want {
				t.Fatalf("child selections = %v; want [%s]", got, want)
			}
			// The copy must survive restart and must not change with its parent.
			if err := hh.h.SetModelOverride(origin.Key.Token(), "a/one"); err != nil {
				t.Fatal(err)
			}
			restartModelHandler(t, hh)
			if got, _ := hh.h.modelOverride(child.ID); got != "b/two" {
				t.Fatalf("child choice changed with parent: %q", got)
			}
		})
	}
}

func TestBranchCopiesSessionModelWithoutAnOverride(t *testing.T) {
	hh := branchHarness(t)
	withModels(hh.a, "a/one", "a/one", "b/two")
	origin := engagedOrigin(t, hh)
	if err := hh.a.SetModel(context.Background(), acp.SessionId("sid-"+origin.ID), "b/two"); err != nil {
		t.Fatal(err)
	}
	hh.branch(t, "planning", "!branch model child")
	child := branchedConv(t, hh, "model child")
	if got, ok := hh.h.modelOverride(child.ID); !ok || got != "b/two" {
		t.Fatalf("child choice = %q, %v; want the origin session's model", got, ok)
	}
	if _, ok := hh.h.modelOverride(origin.ID); ok {
		t.Fatal("branch changed the origin's override")
	}
}

func TestModelStoreLoadFailureIsReturned(t *testing.T) {
	hh := cmdHarness(t, newAgent("answer"), nil)
	path := filepath.Join(hh.s.dir, "model_overrides.json")
	if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(hh.h.cfg); err == nil || !strings.Contains(err.Error(), "load overrides") {
		t.Fatalf("New error = %v; want a store load error", err)
	}
}

func TestBranchChoiceStillAppliesWhenStoreWriteFails(t *testing.T) {
	hh := branchHarness(t)
	withModels(hh.a, "a/one", "a/one", "b/two")
	engagedOrigin(t, hh)
	hh.deliver(t, "planning", "!model b/two")
	if err := os.Mkdir(filepath.Join(hh.s.dir, "model_overrides.json.tmp"), 0o700); err != nil {
		t.Fatal(err)
	}
	hh.branch(t, "planning", "!branch model child")
	child := branchedConv(t, hh, "model child")
	if !hh.logged("persisting branch model") {
		t.Fatalf("missing branch persistence warning: %v", hh.logs)
	}
	if got, ok := hh.h.modelOverride(child.ID); !ok || got != "b/two" {
		t.Fatalf("child choice = %q, %v", got, ok)
	}
}

func TestNewModelChoiceSurvivesRelayRestart(t *testing.T) {
	hh := dmCmdHarness(t, withModels(newAgent("answer"), "a/one", "a/one", "b/two"), nil)
	hh.deliverDM(t, humanID, "hello", humanID, botID)
	previous := hh.j.Convs()[0]
	hh.deliverDM(t, humanID, "!model b/two", humanID, botID)
	hh.deliverDM(t, humanID, "!new", humanID, botID)
	fresh, _ := hh.j.Lookup(journal.DM([]int64{humanID, botID}))
	restartModelHandler(t, hh)
	if _, ok := hh.h.modelOverride(previous.ID); ok {
		t.Fatal("the retired conversation still has a choice")
	}
	if got, ok := hh.h.modelOverride(fresh.ID); !ok || got != "b/two" {
		t.Fatalf("fresh choice = %q, %v", got, ok)
	}
}

func TestModelChoiceStillAppliesWhenStoreWriteFails(t *testing.T) {
	hh := cmdHarness(t, withModels(newAgent("answer"), "a/one", "a/one", "b/two"), nil)
	hh.deliver(t, "planning", mention("hello"))
	if err := os.Mkdir(filepath.Join(hh.s.dir, "model_overrides.json.tmp"), 0o700); err != nil {
		t.Fatal(err)
	}
	hh.deliver(t, "planning", "!model b/two")
	if !hh.logged("persisting model override") {
		t.Fatalf("missing persistence warning: %v", hh.logs)
	}
	hh.deliver(t, "planning", "next")
	conv, _ := hh.j.Lookup(journal.Channel(4, "planning"))
	want := "sid-" + conv.ID + "=b/two"
	if got := hh.a.selections(); len(got) != 1 || got[0] != want {
		t.Fatalf("selections = %v; want [%s]", got, want)
	}
}
