package zulipproto

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// scriptServer answers requests from an ordered script. Each step sees
// the request and returns (status, body); the script index advances per
// request, and the last step repeats forever.
type scriptServer struct {
	*httptest.Server
	mu    sync.Mutex
	n     int
	steps []func(r *http.Request) (int, string)
	paths []string
}

func newScript(t *testing.T, steps ...func(r *http.Request) (int, string)) *scriptServer {
	t.Helper()
	ss := &scriptServer{steps: steps}
	ss.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ss.mu.Lock()
		i := ss.n
		ss.n++
		if i >= len(ss.steps) {
			i = len(ss.steps) - 1
		}
		step := ss.steps[i]
		ss.paths = append(ss.paths, r.Method+" "+r.URL.Path)
		ss.mu.Unlock()
		status, body := step(r)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = fmt.Fprint(w, body)
	}))
	t.Cleanup(ss.Close)
	return ss
}

func (ss *scriptServer) calls() []string {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	out := make([]string, len(ss.paths))
	copy(out, ss.paths)
	return out
}

func registerOK(queue string, last int64) func(*http.Request) (int, string) {
	return func(*http.Request) (int, string) {
		return 200, fmt.Sprintf(`{"result":"success","msg":"","queue_id":%q,"last_event_id":%d}`, queue, last)
	}
}

func eventsOK(body string) func(*http.Request) (int, string) {
	return func(*http.Request) (int, string) {
		return 200, `{"result":"success","msg":"","events":[` + body + `]}`
	}
}

// fakeClock returns a monotonic clock the test advances by hand.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// runnerHarness wires a Runner to a script server with injected clock
// and sleep, so nothing in these tests waits on the wall clock.
type runnerHarness struct {
	r      *Runner
	sleeps []time.Duration
	clock  *fakeClock
	logs   []string
	mu     sync.Mutex
}

func newHarness(t *testing.T, ss *scriptServer, handle EventHandler, tune func(*RunnerConfig)) *runnerHarness {
	t.Helper()
	c, err := New(Config{Site: ss.URL, Email: "a", APIKey: "b", HTTPClient: ss.Client()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h := &runnerHarness{clock: &fakeClock{t: time.Unix(1_700_000_000, 0)}}
	cfg := RunnerConfig{
		Client:     c,
		EventTypes: []string{"message"},
		Narrow:     [][2]string{{"channel", "4"}},
		Handle:     handle,
		Now:        h.clock.now,
		Sleep: func(_ context.Context, d time.Duration) error {
			h.mu.Lock()
			h.sleeps = append(h.sleeps, d)
			h.mu.Unlock()
			return nil
		},
		// Deterministic jitter: identity, so backoff growth is exact.
		Jitter: func(d time.Duration) time.Duration { return d },
		Logf: func(format string, args ...any) {
			h.mu.Lock()
			h.logs = append(h.logs, fmt.Sprintf(format, args...))
			h.mu.Unlock()
		},
	}
	if tune != nil {
		tune(&cfg)
	}
	r, err := NewRunner(cfg)
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	h.r = r
	return h
}

func (h *runnerHarness) sleptFor() []time.Duration {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]time.Duration, len(h.sleeps))
	copy(out, h.sleeps)
	return out
}

func (h *runnerHarness) logged(sub string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, l := range h.logs {
		if len(sub) > 0 && contains(l, sub) {
			return true
		}
	}
	return false
}

func contains(s, sub string) bool {
	return len(sub) <= len(s) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}

func TestNewRunnerValidation(t *testing.T) {
	if _, err := NewRunner(RunnerConfig{}); err == nil {
		t.Fatal("want error on nil client")
	}
	c, err := New(Config{Site: "https://z", Email: "a", APIKey: "b"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := NewRunner(RunnerConfig{Client: c}); err == nil {
		t.Fatal("want error on nil handler")
	}
	r, err := NewRunner(RunnerConfig{Client: c, Handle: func(context.Context, Event) {}})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	if r.cfg.MaxBackoff != DefaultMaxBackoff || r.cfg.Silence != DefaultSilence ||
		r.cfg.Now == nil || r.cfg.Sleep == nil || r.cfg.Jitter == nil || r.cfg.Logf == nil {
		t.Fatalf("defaults not applied: %+v", r.cfg)
	}
	r.cfg.Logf("smoke %d", 1) // the no-op default must be callable
	if r.LastEventID() != -1 {
		t.Fatalf("initial cursor = %d", r.LastEventID())
	}
}

// TestRunDeliversEventsAndSkipsHeartbeats is the happy path: the
// cursor advances over every event including heartbeats, but only
// non-heartbeat events reach the handler.
func TestRunDeliversEventsAndSkipsHeartbeats(t *testing.T) {
	ss := newScript(t,
		registerOK("q1", -1),
		eventsOK(`{"id":0,"type":"heartbeat"},{"id":1,"type":"message","message":{"id":33,"content":"one","sender_id":5,"stream_id":4,"subject":"t"}}`),
		eventsOK(`{"id":2,"type":"heartbeat"},{"id":3,"type":"update_message","message_id":33,"stream_id":4,"subject":"new","orig_subject":"t"}`),
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var got []Event
	h := newHarness(t, ss, func(_ context.Context, ev Event) {
		got = append(got, ev)
		if len(got) == 2 {
			cancel()
		}
	}, nil)

	if err := h.r.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("handled %d events, want 2", len(got))
	}
	if got[0].Type != EventMessage || got[0].Message.Content != "one" {
		t.Fatalf("event 0 = %+v", got[0])
	}
	if got[1].Type != EventUpdateMessage || got[1].Topic != "new" || got[1].OrigTopic != "t" {
		t.Fatalf("event 1 = %+v", got[1])
	}
	if h.r.LastEventID() < 3 {
		t.Fatalf("cursor = %d, want >= 3", h.r.LastEventID())
	}
	// Shutdown deletes the queue.
	calls := ss.calls()
	if last := calls[len(calls)-1]; last != "DELETE /api/v1/events" {
		t.Fatalf("last call = %q", last)
	}
}

// TestRedeliveredEventsAreDropped pins the reconnect dedup rule: an
// event id at or below the cursor is skipped.
func TestRedeliveredEventsAreDropped(t *testing.T) {
	ss := newScript(t,
		registerOK("q1", 10),
		// The server redelivers 5 and 10 (both <= cursor) plus a new 11.
		eventsOK(`{"id":5,"type":"message","message":{"content":"old"}},{"id":10,"type":"message","message":{"content":"boundary"}},{"id":11,"type":"message","message":{"content":"new"}}`),
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var got []string
	h := newHarness(t, ss, func(_ context.Context, ev Event) {
		got = append(got, ev.Message.Content)
		cancel()
	}, nil)
	_ = h.r.Run(ctx)
	if len(got) != 1 || got[0] != "new" {
		t.Fatalf("handled %v, want only the new event", got)
	}
	if h.r.LastEventID() != 11 {
		t.Fatalf("cursor = %d", h.r.LastEventID())
	}
}

// TestBadEventQueueReregisters pins that a dead queue is ROUTINE: it
// re-registers, resets the cursor, and does not back off.
func TestBadEventQueueReregisters(t *testing.T) {
	ss := newScript(t,
		registerOK("q1", 7),
		func(*http.Request) (int, string) {
			return 400, `{"result":"error","msg":"Bad event queue ID: q1","code":"BAD_EVENT_QUEUE_ID"}`
		},
		registerOK("q2", 0),
		eventsOK(`{"id":1,"type":"message","message":{"content":"after"}}`),
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var got string
	h := newHarness(t, ss, func(_ context.Context, ev Event) {
		got = ev.Message.Content
		cancel()
	}, nil)
	_ = h.r.Run(ctx)
	if got != "after" {
		t.Fatalf("got %q", got)
	}
	if s := h.sleptFor(); len(s) != 0 {
		t.Fatalf("a dead queue must not back off, slept %v", s)
	}
	if !h.logged("expired") {
		t.Fatal("expected an info log about the expired queue")
	}
	// The old queue is deleted before a new one is registered.
	calls := ss.calls()
	if calls[2] != "DELETE /api/v1/events" || calls[3] != "POST /api/v1/register" {
		t.Fatalf("calls = %v", calls)
	}
}

// TestRegisterBacksOff drives the exponential backoff deterministically
// through the injected Sleep.
func TestRegisterBacksOff(t *testing.T) {
	ss := newScript(t,
		func(*http.Request) (int, string) { return 500, `{"result":"error","msg":"down"}` },
		func(*http.Request) (int, string) { return 500, `{"result":"error","msg":"down"}` },
		func(*http.Request) (int, string) { return 500, `{"result":"error","msg":"down"}` },
		registerOK("q1", 0),
		eventsOK(`{"id":1,"type":"message","message":{"content":"up"}}`),
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := newHarness(t, ss, func(context.Context, Event) { cancel() }, nil)
	_ = h.r.Run(ctx)
	want := []time.Duration{baseBackoff, 2 * baseBackoff, 4 * baseBackoff}
	got := h.sleptFor()
	if len(got) != len(want) {
		t.Fatalf("slept %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("slept %v, want %v", got, want)
		}
	}
}

// TestBackoffCaps pins that the delay saturates at MaxBackoff instead
// of doubling forever.
func TestBackoffCaps(t *testing.T) {
	ss := newScript(t, func(*http.Request) (int, string) { return 500, `{"result":"error","msg":"down"}` })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := newHarness(t, ss, func(context.Context, Event) {}, func(c *RunnerConfig) {
		c.MaxBackoff = 3 * baseBackoff
		var n int
		inner := c.Sleep
		c.Sleep = func(ctx context.Context, d time.Duration) error {
			n++
			if n >= 6 {
				cancel()
			}
			return inner(ctx, d)
		}
	})
	_ = h.r.Run(ctx)
	got := h.sleptFor()
	if len(got) < 6 {
		t.Fatalf("slept %v", got)
	}
	for _, d := range got {
		if d > 3*baseBackoff {
			t.Fatalf("delay %s exceeds cap; sleeps %v", d, got)
		}
	}
	if got[len(got)-1] != 3*baseBackoff {
		t.Fatalf("delay did not saturate at the cap: %v", got)
	}
}

// TestPollErrorBacksOffThenRecovers covers a transient non-queue poll
// failure.
func TestPollErrorBacksOffThenRecovers(t *testing.T) {
	ss := newScript(t,
		registerOK("q1", 0),
		func(*http.Request) (int, string) { return 500, `{"result":"error","msg":"tornado wobble"}` },
		eventsOK(`{"id":1,"type":"message","message":{"content":"ok"}}`),
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := newHarness(t, ss, func(context.Context, Event) { cancel() }, nil)
	_ = h.r.Run(ctx)
	if s := h.sleptFor(); len(s) != 1 || s[0] != baseBackoff {
		t.Fatalf("slept %v", s)
	}
	if !h.logged("poll failed") {
		t.Fatal("expected a poll-failure log")
	}
}

// TestCleanTimeoutRetriesImmediately: a long poll that runs its full
// client budget is the normal idle outcome and must not back off.
func TestCleanTimeoutRetriesImmediately(t *testing.T) {
	block := make(chan struct{})
	released := false
	ss := newScript(t,
		registerOK("q1", 0),
		func(*http.Request) (int, string) {
			<-block // held open until the client times out
			return 200, `{"result":"success","msg":"","events":[]}`
		},
		eventsOK(`{"id":1,"type":"message","message":{"content":"after timeout"}}`),
	)
	// A short client timeout makes the blocked long poll expire.
	c, err := New(Config{Site: ss.URL, Email: "a", APIKey: "b",
		HTTPClient: &http.Client{Timeout: 150 * time.Millisecond}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var slept []time.Duration
	var got string
	r, err := NewRunner(RunnerConfig{
		Client: c,
		Handle: func(_ context.Context, ev Event) {
			got = ev.Message.Content
			cancel()
		},
		Sleep: func(_ context.Context, d time.Duration) error {
			slept = append(slept, d)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	done := make(chan struct{})
	go func() { defer close(done); _ = r.Run(ctx) }()
	<-done
	if !released {
		close(block)
		released = true
	}
	if got != "after timeout" {
		t.Fatalf("got %q", got)
	}
	if len(slept) != 0 {
		t.Fatalf("a clean long-poll timeout must not back off, slept %v", slept)
	}
}

// TestSilenceForcesReregister: total silence — no events, not even a
// heartbeat — for longer than Silence tears the queue down.
func TestSilenceForcesReregister(t *testing.T) {
	ss := newScript(t,
		registerOK("q1", 0),
		eventsOK(``), // empty batch: no heartbeat, nothing
		registerOK("q2", 0),
		eventsOK(`{"id":1,"type":"message","message":{"content":"revived"}}`),
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var got string
	h := newHarness(t, ss, func(_ context.Context, ev Event) {
		got = ev.Message.Content
		cancel()
	}, nil)
	// Every clock read jumps a full silence window forward, so the
	// first poll that returns nothing at all trips the liveness check.
	base := h.clock.now
	var n int
	h.r.cfg.Now = func() time.Time {
		n++
		return base().Add(time.Duration(n) * (DefaultSilence + time.Second))
	}
	_ = h.r.Run(ctx)
	if got != "revived" {
		t.Fatalf("got %q", got)
	}
	if !h.logged("not even a heartbeat") {
		t.Fatal("expected a silence log")
	}
	calls := ss.calls()
	if calls[2] != "DELETE /api/v1/events" || calls[3] != "POST /api/v1/register" {
		t.Fatalf("calls = %v", calls)
	}
}

// TestHeartbeatKeepsQueueAlive is the mirror image: heartbeats reset
// the liveness timer, so a quiet-but-healthy queue is never dropped.
func TestHeartbeatKeepsQueueAlive(t *testing.T) {
	ss := newScript(t,
		registerOK("q1", 0),
		eventsOK(`{"id":1,"type":"heartbeat"}`),
		eventsOK(`{"id":2,"type":"message","message":{"content":"still here"}}`),
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := newHarness(t, ss, func(context.Context, Event) { cancel() }, nil)
	// The clock advances between polls, but each heartbeat resets the
	// window, so it never trips.
	base := h.clock.now
	var n int
	h.r.cfg.Now = func() time.Time {
		n++
		return base().Add(time.Duration(n) * (DefaultSilence / 2))
	}
	_ = h.r.Run(ctx)
	for _, c := range ss.calls()[:3] {
		if c == "DELETE /api/v1/events" {
			t.Fatalf("healthy queue was dropped: %v", ss.calls())
		}
	}
}

// TestTeardownDeleteFailureIsLogged: a failed best-effort delete on
// shutdown must not escalate.
func TestTeardownDeleteFailureIsLogged(t *testing.T) {
	ss := newScript(t,
		registerOK("q1", 0),
		func(r *http.Request) (int, string) {
			if r.Method == http.MethodDelete {
				return 500, `{"result":"error","msg":"cannot delete"}`
			}
			return 200, `{"result":"success","msg":"","events":[{"id":1,"type":"message","message":{"content":"x"}}]}`
		},
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := newHarness(t, ss, func(context.Context, Event) { cancel() }, nil)
	_ = h.r.Run(ctx)
	if !h.logged("delete queue") {
		t.Fatal("expected a delete-failure log")
	}
}

// TestTeardownWithoutQueueIsNoop covers the shutdown path when the
// runner never got a queue.
func TestTeardownWithoutQueueIsNoop(t *testing.T) {
	ss := newScript(t, func(*http.Request) (int, string) { return 500, `{"result":"error","msg":"down"}` })
	ctx, cancel := context.WithCancel(context.Background())
	h := newHarness(t, ss, func(context.Context, Event) {}, func(c *RunnerConfig) {
		inner := c.Sleep
		c.Sleep = func(ctx context.Context, d time.Duration) error {
			cancel()
			return inner(ctx, d)
		}
	})
	if err := h.r.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v", err)
	}
	for _, c := range ss.calls() {
		if c == "DELETE /api/v1/events" {
			t.Fatal("nothing to delete, but a delete was issued")
		}
	}
	// dropQueue with an empty id is a no-op too.
	h.r.deleteQueue("")
}

// TestPollFailureDuringShutdownIsSilent: once ctx is cancelled, a
// failing in-flight poll must not log or back off.
func TestPollFailureDuringShutdownIsSilent(t *testing.T) {
	ss := newScript(t, registerOK("q1", 0))
	ctx, cancel := context.WithCancel(context.Background())
	h := newHarness(t, ss, func(context.Context, Event) {}, nil)
	cancel()
	h.r.queueID = "q1"
	h.r.pollFailed(ctx, errors.New("connection reset"))
	if len(h.sleptFor()) != 0 || h.logged("poll failed") {
		t.Fatal("shutdown-time poll failure must be silent")
	}
}

func TestIsTimeout(t *testing.T) {
	if !isTimeout(context.DeadlineExceeded) {
		t.Fatal("deadline exceeded is a timeout")
	}
	if !isTimeout(fmt.Errorf("wrapped: %w", &net.OpError{Err: &timeoutErr{}})) {
		t.Fatal("net timeout not recognised")
	}
	if isTimeout(errors.New("connection reset")) {
		t.Fatal("plain error is not a timeout")
	}
}

type timeoutErr struct{}

func (*timeoutErr) Error() string { return "i/o timeout" }
func (*timeoutErr) Timeout() bool { return true }

func TestSleepCtx(t *testing.T) {
	if err := sleepCtx(context.Background(), time.Nanosecond); err != nil {
		t.Fatalf("sleepCtx: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sleepCtx(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("sleepCtx cancelled = %v", err)
	}
}

func TestJitter(t *testing.T) {
	if got := jitter(0); got != 0 {
		t.Fatalf("jitter(0) = %s", got)
	}
	if got := jitter(-time.Second); got != 0 {
		t.Fatalf("jitter(negative) = %s", got)
	}
	for i := 0; i < 200; i++ {
		d := jitter(time.Second)
		if d < 500*time.Millisecond || d > time.Second {
			t.Fatalf("jitter out of range: %s", d)
		}
	}
}

// TestOnRegisterFiresOnEveryRegistration pins the drift repair: a dead
// queue loses events, so the hook must run again on re-registration,
// not only at startup.
func TestOnRegisterFiresOnEveryRegistration(t *testing.T) {
	ss := newScript(t,
		registerOK("q1", 7),
		func(*http.Request) (int, string) {
			return 400, `{"result":"error","msg":"Bad event queue ID: q1","code":"BAD_EVENT_QUEUE_ID"}`
		},
		registerOK("q2", 0),
		eventsOK(`{"id":1,"type":"message","message":{"content":"after"}}`),
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var registrations int
	h := newHarness(t, ss, func(context.Context, Event) { cancel() }, func(cfg *RunnerConfig) {
		cfg.OnRegister = func(ctx context.Context) {
			if ctx == nil {
				t.Error("OnRegister must receive the runner's context")
			}
			registrations++
		}
	})
	_ = h.r.Run(ctx)
	if registrations != 2 {
		t.Fatalf("OnRegister called %d times, want 2", registrations)
	}
}
