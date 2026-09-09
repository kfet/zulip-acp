package handler

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kfet/zulip-acp/internal/journal"
)

// TestTypingIntervalFor: the cadence must sit comfortably inside the
// realm's expiry, and never collapse to a busy loop on an absurd one.
func TestTypingIntervalFor(t *testing.T) {
	cases := []struct{ expiry, want time.Duration }{
		{15 * time.Second, 10 * time.Second},
		{30 * time.Second, 20 * time.Second},
		{time.Second, time.Second},
		{0, time.Second},
	}
	for _, tc := range cases {
		if got := TypingIntervalFor(tc.expiry); got != tc.want {
			t.Fatalf("TypingIntervalFor(%s) = %s, want %s", tc.expiry, got, tc.want)
		}
	}
}

// TestTypingLoopRefreshesAndStops drives the loop on an injected tick:
// a `start` goes up at once, every tick refreshes it before the server
// would expire it, and cancellation lowers it exactly once.
func TestTypingLoopRefreshesAndStops(t *testing.T) {
	z := newZulip()
	h := &Handler{cfg: Config{Client: z, Logf: func(string, ...any) {}}}
	ctx, cancel := context.WithCancel(context.Background())
	tick := make(chan time.Time)
	done := make(chan struct{})
	go func() {
		defer close(done)
		typingLoop(ctx, h.typingSetter(journal.Channel(4, "t")), tick, time.Minute)
	}()
	<-z.typed
	tick <- time.Time{}
	<-z.typed
	cancel()
	<-done
	want := []string{"start:4:t", "start:4:t", "stop:4:t"}
	got := z.typingOps()
	if len(got) != len(want) {
		t.Fatalf("typing ops = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("typing ops = %q, want %q", got, want)
		}
	}
}

// TestTypingSetterDM pins the DM shape: the key's user ids select the
// `direct` form, so a quiet-mode DM shows liveness instead of
// crashing on a stream id it does not have.
func TestTypingSetterDM(t *testing.T) {
	z := newZulip()
	h := &Handler{cfg: Config{Client: z, Logf: func(string, ...any) {}}}
	h.typingSetter(journal.DM([]int64{7, 9}))(context.Background(), "start")
	if got := z.typingOps(); len(got) != 1 || got[0] != "start:dm[7 9]" {
		t.Fatalf("typing ops = %q", got)
	}
}

// TestTypingFailureIsSwallowed: a server that refuses typing
// notifications must cost a log line, not a turn.
func TestTypingFailureIsSwallowed(t *testing.T) {
	z := newZulip()
	z.typingErr = errors.New("nope")
	var logged int
	h := &Handler{cfg: Config{Client: z, Logf: func(string, ...any) { logged++ }}}
	h.typingSetter(journal.Channel(4, "t"))(context.Background(), "start")
	if logged != 1 {
		t.Fatalf("logged %d lines, want 1", logged)
	}
}

// TestStartTypingDisabled pins the off switch: a non-positive interval
// starts no goroutine and sends nothing at all.
func TestStartTypingDisabled(t *testing.T) {
	z := newZulip()
	h := &Handler{cfg: Config{Client: z, TypingInterval: -1, Logf: func(string, ...any) {}}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.startTyping(ctx, journal.Channel(4, "t"))
	if got := z.typingOps(); len(got) != 0 {
		t.Fatalf("typing ops = %q, want none", got)
	}
}

// TestNewDefaultsTypingIntervalInQuietMode: quiet mode without an
// explicit cadence still shows liveness, on the stock-realm default.
func TestNewDefaultsTypingIntervalInQuietMode(t *testing.T) {
	h := newHarness(t, newAgent("x"), func(c *Config) { c.BatchEdits = true; c.TypingInterval = 0 }).h
	if h.cfg.TypingInterval != defaultTypingInterval {
		t.Fatalf("TypingInterval = %s, want %s", h.cfg.TypingInterval, defaultTypingInterval)
	}
}
