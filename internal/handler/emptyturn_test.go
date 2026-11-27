package handler

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"

	"github.com/kfet/zulip-acp/internal/zulipproto"
)

func runEmptyTurn(t *testing.T, agent *fakeAgent) (*harness, []string) {
	t.Helper()
	hh := newHarness(t, agent, nil)
	hh.h.Handle(context.Background(), zulipproto.Event{Type: zulipproto.EventMessage, Message: &zulipproto.Message{
		SenderID: humanID, SenderName: "Kfet", Content: mention("look it up"),
		StreamID: 4, Topic: "empty", Type: "stream",
	}})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := hh.h.WaitIdle(ctx); err != nil {
		t.Fatalf("WaitIdle: %v", err)
	}
	agent.mu.Lock()
	defer agent.mu.Unlock()
	return hh, append([]string(nil), agent.prompts...)
}

// A turn that ends with no text is prompted once more, and the second
// answer is what the topic gets.
func TestEmptyTurnPromptsOnceMore(t *testing.T) {
	agent := newAgent()
	agent.during = func() {
		agent.mu.Lock()
		agent.chunks = []string{"the real answer"}
		agent.mu.Unlock()
	}
	hh, prompts := runEmptyTurn(t, agent)
	if len(prompts) != 2 || prompts[1] != EmptyTurnNote {
		t.Fatalf("prompts = %q", prompts)
	}
	all := strings.Join(hh.z.stored(), "\n")
	if !strings.Contains(all, "the real answer") || strings.Contains(all, "no output") {
		t.Fatalf("stored = %q", all)
	}
}

// Only once: a second empty turn posts "(no output)" as before.
func TestEmptyTurnPromptsOnlyOnce(t *testing.T) {
	hh, prompts := runEmptyTurn(t, newAgent())
	if len(prompts) != 2 {
		t.Fatalf("prompts = %q", prompts)
	}
	if all := strings.Join(hh.z.stored(), "\n"); !strings.Contains(all, "no output") {
		t.Fatalf("stored = %q", all)
	}
}

// A turn with text is never prompted again.
func TestNonEmptyTurnNotPromptedAgain(t *testing.T) {
	_, prompts := runEmptyTurn(t, newAgent("answer"))
	if len(prompts) != 1 {
		t.Fatalf("prompts = %q", prompts)
	}
}

func TestEmptyTurnExemptions(t *testing.T) {
	h := &Handler{}
	sink := newStreamingSink(nil, false)
	dir := t.TempDir()
	if !h.emptyTurn("tok", acp.StopReasonEndTurn, sink, dir) {
		t.Fatal("plain empty turn not detected")
	}
	if h.emptyTurn("tok", acp.StopReasonCancelled, sink, dir) {
		t.Fatal("cancelled turn treated as empty")
	}
	h.outOfBand.Store("tok", true)
	if h.emptyTurn("tok", acp.StopReasonEndTurn, sink, dir) {
		t.Fatal("turn that posted treated as empty")
	}
	if err := os.MkdirAll(filepath.Join(dir, OutboxDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, OutboxDir, ".hidden"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !h.emptyTurn("other", acp.StopReasonEndTurn, sink, dir) {
		t.Fatal("a dotfile in the outbox counted as an answer")
	}
	if err := os.WriteFile(filepath.Join(dir, OutboxDir, "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if h.emptyTurn("other", acp.StopReasonEndTurn, sink, dir) {
		t.Fatal("turn with an outbox file treated as empty")
	}
}
