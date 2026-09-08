package handler

import (
	"context"
	"strings"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"
)

// toolNotification is a tool_call_update: the ACP evidence that a
// legitimately long-running tool is still working.
func toolNotification() acp.SessionNotification {
	return acp.SessionNotification{
		Update: acp.SessionUpdate{
			ToolCallUpdate: &acp.SessionToolCallUpdate{ToolCallId: "t1"},
		},
	}
}

// TestWedgedTurnIsCutAndSaysSo is the regression for the bug this whole
// mechanism exists for: a turn that stops making progress must be cut,
// the partial answer must survive, and the message must say what
// happened instead of the bare "*error: context deadline exceeded*"
// that told the user nothing.
func TestWedgedTurnIsCutAndSaysSo(t *testing.T) {
	agent := newAgent("half an ans")
	agent.hold = make(chan struct{}) // never closed: the agent wedges
	hh := newHarness(t, agent, func(c *Config) {
		c.NoProgressTimeout = 150 * time.Millisecond
	})
	hh.deliver(t, "t", mention("hi"))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := hh.h.WaitIdle(ctx); err != nil {
		t.Fatalf("the wedged turn was never cut: %v", err)
	}
	body := hh.z.lastBody()
	if !strings.Contains(body, "half an ans") {
		t.Fatalf("the partial answer was lost: %q", body)
	}
	if !strings.Contains(body, "no tool activity") {
		t.Fatalf("the cut must name its reason, got %q", body)
	}
	if strings.Contains(body, "deadline exceeded") {
		t.Fatalf("the bare deadline error is back: %q", body)
	}
}

// TestToolActivityKeepsATurnAlive is the other half of the contract: a
// turn far longer than the no-progress window is NOT cut, because tool
// calls keep landing. Without this the fix would just be a shorter
// version of the bug.
func TestToolActivityKeepsATurnAlive(t *testing.T) {
	agent := newAgent("done")
	release := make(chan struct{})
	agent.hold = release
	window := 100 * time.Millisecond
	agent.during = func() {
		go func() {
			// Tool activity spanning several windows, then release.
			for i := 0; i < 8; i++ {
				time.Sleep(window / 4)
				agent.mu.Lock()
				sink := agent.sink
				agent.mu.Unlock()
				_ = sink.OnUpdate(context.Background(), toolNotification())
			}
			close(release)
		}()
	}
	hh := newHarness(t, agent, func(c *Config) { c.NoProgressTimeout = window })
	hh.deliver(t, "t", mention("hi"))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := hh.h.WaitIdle(ctx); err != nil {
		t.Fatalf("turn did not finish: %v", err)
	}
	if body := hh.z.lastBody(); !strings.Contains(body, "done") || strings.Contains(body, "wedged") {
		t.Fatalf("a working turn was cut: %q", body)
	}
}

// TestTurnCeilingIsOptIn: with prompt_timeout_seconds unset there is no
// absolute cap, so a turn that keeps working is never cut by one.
func TestTurnCeilingIsOptIn(t *testing.T) {
	agent := newAgent("done")
	hh := newHarness(t, agent, nil)
	hh.deliver(t, "t", mention("hi"))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := hh.h.WaitIdle(ctx); err != nil {
		t.Fatalf("turn did not finish: %v", err)
	}
	if hh.h.cfg.TurnCeiling != 0 {
		t.Fatalf("turn ceiling = %s, want none unless configured", hh.h.cfg.TurnCeiling)
	}
}

// TestTurnCeilingCutsSaysSo: when an operator DOES set the ceiling it
// fires despite progress, and names itself so the difference from a
// wedge is visible in the topic.
func TestTurnCeilingCutsSaysSo(t *testing.T) {
	agent := newAgent("partial")
	agent.hold = make(chan struct{})
	hh := newHarness(t, agent, func(c *Config) {
		c.NoProgressTimeout = time.Hour
		c.TurnCeiling = 150 * time.Millisecond
	})
	hh.deliver(t, "t", mention("hi"))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := hh.h.WaitIdle(ctx); err != nil {
		t.Fatalf("the ceiling never fired: %v", err)
	}
	body := hh.z.lastBody()
	if !strings.Contains(body, "ceiling") || !strings.Contains(body, "partial") {
		t.Fatalf("body = %q", body)
	}
}
