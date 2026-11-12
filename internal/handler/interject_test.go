package handler

import (
	"context"
	"strings"
	"sync"
	"testing"

	acp "github.com/coder/acp-go-sdk"
	"github.com/kfet/acp-kit/client"
	"github.com/kfet/acp-kit/interject"
	"github.com/kfet/zulip-acp/internal/zulipproto"
)

// ijAgent plays the agent a side question goes to: a fork that answers
// out, or fails with forkErr.
type ijAgent struct {
	mu      sync.Mutex
	out     string
	forkErr error
	sink    client.SessionUpdateSink
	asked   []string
	// during runs inside the fork's prompt.
	during func()
}

func (a *ijAgent) ForkSession(_ context.Context, _ string, parent acp.SessionId, _ string, sink client.SessionUpdateSink) (acp.SessionId, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.forkErr != nil {
		return "", a.forkErr
	}
	a.sink = sink
	return "side-of-" + parent, nil
}

func (a *ijAgent) Prompt(ctx context.Context, _ acp.SessionId, blocks []acp.ContentBlock) (acp.StopReason, error) {
	a.mu.Lock()
	a.asked = append(a.asked, blocks[0].Text.Text)
	sink, out, during := a.sink, a.out, a.during
	a.mu.Unlock()
	if during != nil {
		during()
	}
	_ = sink.OnUpdate(ctx, chunkNotification(out, nil))
	return acp.StopReasonEndTurn, nil
}

func (a *ijAgent) CloseSession(context.Context, acp.SessionId) error { return nil }

func busyMsg(text string) zulipproto.Event {
	return zulipproto.Event{Type: zulipproto.EventMessage, Message: &zulipproto.Message{
		SenderID: humanID, SenderName: "Kfet", Content: text,
		StreamID: 4, Topic: "busy", Type: "stream",
	}}
}

// interjectHarness starts a main turn that blocks, and returns the
// harness with the main turn in flight.
func interjectHarness(t *testing.T, ij *ijAgent) (*harness, *fakeAgent) {
	t.Helper()
	agent := newAgent("main answer")
	agent.block = make(chan struct{})
	hh := newHarness(t, agent, func(c *Config) {
		c.Interject = interject.New(ij, interject.Config{Enabled: true})
	})
	hh.h.Handle(context.Background(), busyMsg(mention("do something slow")))
	<-agent.entered
	return hh, agent
}

func (hh *harness) cancelCount() int {
	hh.s.mu.Lock()
	defer hh.s.mu.Unlock()
	return len(hh.s.cancels)
}

func (a *fakeAgent) promptList() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.prompts...)
}

func TestInterjectAnswersWithoutStoppingTheTurn(t *testing.T) {
	ij := &ijAgent{out: "It is about half done.\n1 9"}
	hh, agent := interjectHarness(t, ij)
	// Wait for the side answer while main still runs.
	ij.during = nil
	hh.h.Handle(context.Background(), busyMsg("how far along?"))
	// ops tracks the side question; main still blocks, so wait on ops.
	hh.h.ops.Wait()
	if !strings.Contains(hh.z.lastBody(), "It is about half done.\n\n*(quick reply · answered)*") {
		t.Fatalf("quick reply not posted: %q", hh.z.lastBody())
	}
	if n := hh.cancelCount(); n != 0 {
		t.Fatalf("main turn cancelled %d times, want 0", n)
	}
	close(agent.block)
	hh.idle(t)

	// The next main prompt carries the side Q&A first.
	hh.deliver(t, "busy", "next")
	p := agent.promptList()
	last := p[len(p)-1]
	if !strings.Contains(last, "[while you were working") || !strings.HasSuffix(last, "next") {
		t.Fatalf("next prompt = %q", last)
	}
}

func TestInterjectFallsBackWhenForkFails(t *testing.T) {
	ij := &ijAgent{forkErr: client.ErrForkUnsupported}
	hh, agent := interjectHarness(t, ij)
	agent.mu.Lock()
	agent.block = nil
	agent.mu.Unlock()
	hh.h.Handle(context.Background(), busyMsg("actually, do this instead"))
	hh.idle(t)
	if n := hh.cancelCount(); n != 1 {
		t.Fatalf("cancels = %d, want 1", n)
	}
	p := agent.promptList()
	if !strings.HasSuffix(p[len(p)-1], "actually, do this instead") {
		t.Fatalf("fallback prompt = %q", p[len(p)-1])
	}
}

func TestInterjectSteerRepromptsMain(t *testing.T) {
	ij := &ijAgent{out: "Switching.\n3 9"}
	hh, agent := interjectHarness(t, ij)
	agent.mu.Lock()
	agent.block = nil
	agent.mu.Unlock()
	hh.h.Handle(context.Background(), busyMsg("do B instead"))
	hh.idle(t)
	if n := hh.cancelCount(); n != 1 {
		t.Fatalf("cancels = %d, want 1", n)
	}
	p := agent.promptList()
	if len(p) != 2 || !strings.HasSuffix(p[1], "do B instead") {
		t.Fatalf("prompts = %q", p)
	}
}

func TestInterjectAbortStopsMainOnly(t *testing.T) {
	ij := &ijAgent{out: "4 9"}
	hh, agent := interjectHarness(t, ij)
	hh.h.Handle(context.Background(), busyMsg("stop that"))
	hh.idle(t)
	if n := hh.cancelCount(); n != 1 {
		t.Fatalf("cancels = %d, want 1", n)
	}
	if p := agent.promptList(); len(p) != 1 {
		t.Fatalf("prompts = %q, want only the main one", p)
	}
	if strings.Contains(hh.z.lastBody(), "quick reply") {
		t.Fatalf("empty answer posted: %q", hh.z.lastBody())
	}
}

func TestInterjectQueueWhileMainRuns(t *testing.T) {
	ij := &ijAgent{out: "Will do after.\n2 9"}
	hh, agent := interjectHarness(t, ij)
	hh.h.Handle(context.Background(), busyMsg("also add tests"))
	hh.h.ops.Wait()
	close(agent.block)
	hh.idle(t)
	if p := agent.promptList(); len(p) != 1 {
		t.Fatalf("prompts = %q, want only the main one", p)
	}
	hh.deliver(t, "busy", "go on")
	p := agent.promptList()
	if !strings.Contains(p[1], "[queued messages from the user]\nalso add tests") {
		t.Fatalf("queued message not delivered: %q", p[1])
	}
}

func TestInterjectQueueAfterMainEnded(t *testing.T) {
	ij := &ijAgent{out: "Will do after.\n2 9"}
	hh, agent := interjectHarness(t, ij)
	ij.during = func() {
		// The main turn ends while the fork answers.
		close(agent.block)
		ctx, cancel := context.WithTimeout(context.Background(), turnWait)
		defer cancel()
		_ = hh.h.convo.Active().WaitIdle(ctx)
		hh.h.endWG.Wait()
	}
	hh.h.Handle(context.Background(), busyMsg("also add tests"))
	hh.idle(t)
	p := agent.promptList()
	if len(p) != 2 || !strings.Contains(p[1], "also add tests") {
		t.Fatalf("prompts = %q", p)
	}
}

func TestInterjectSkipsIdleConversation(t *testing.T) {
	ij := &ijAgent{out: "x\n1 9"}
	agent := newAgent("ok")
	hh := newHarness(t, agent, func(c *Config) {
		c.Interject = interject.New(ij, interject.Config{Enabled: true})
	})
	hh.deliver(t, "busy", mention("one"))
	hh.deliver(t, "busy", "two")
	if len(ij.asked) != 0 || len(agent.promptList()) != 2 {
		t.Fatalf("asked %q, prompts %q", ij.asked, agent.promptList())
	}
}
