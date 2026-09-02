package zulipproto

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// TestRunHandoffLeavesTheQueueAlive is the contract the whole graceful
// reload rests on: on handoff the loop stops polling but must NOT
// delete the queue, because the successor process image resumes it and
// everything posted in between is buffered there.
func TestRunHandoffLeavesTheQueueAlive(t *testing.T) {
	handoff := make(chan struct{})
	var once sync.Once
	ss := newScript(t,
		registerOK("q-live", 7),
		eventsOK(`{"id":8,"type":"message","message":{"id":100,"content":"hi"}}`),
		// Every later poll blocks until the handoff fires, standing in
		// for a long poll that a reload interrupts.
		func(r *http.Request) (int, string) {
			once.Do(func() { close(handoff) })
			<-r.Context().Done()
			return 200, `{"result":"success","msg":"","events":[]}`
		},
	)
	var got []Event
	h := newHarness(t, ss, func(_ context.Context, ev Event) { got = append(got, ev) },
		func(cfg *RunnerConfig) { cfg.Handoff = handoff })

	err := h.r.Run(context.Background())
	if !errors.Is(err, ErrHandoff) {
		t.Fatalf("Run() = %v, want ErrHandoff", err)
	}
	q, last := h.r.Cursor()
	if q != "q-live" || last != 8 {
		t.Fatalf("Cursor() = %q, %d; want q-live, 8", q, last)
	}
	if len(got) != 1 || got[0].ID != 8 {
		t.Fatalf("dispatched %+v, want the one message event", got)
	}
	for _, call := range ss.calls() {
		if strings.HasPrefix(call, "DELETE") {
			t.Fatalf("handoff deleted the queue (%v) — the successor has nothing to resume", ss.calls())
		}
	}
}

// TestRunHandoffDuringShutdownStillDeletesTheQueue: a SIGHUP that
// arrives alongside a SIGTERM is a STOP, not a reload. There is no
// successor to resume the queue, so it must be torn down.
func TestRunHandoffDuringShutdownStillDeletesTheQueue(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	handoff := make(chan struct{})
	var once sync.Once
	ss := newScript(t,
		registerOK("q-doomed", 1),
		func(*http.Request) (int, string) {
			once.Do(func() {
				close(handoff)
				cancel()
			})
			return 200, `{"result":"success","msg":"","events":[]}`
		},
	)
	h := newHarness(t, ss, func(context.Context, Event) {},
		func(cfg *RunnerConfig) { cfg.Handoff = handoff })

	if err := h.r.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() = %v, want context.Canceled", err)
	}
	if !hasDelete(ss.calls()) {
		t.Fatalf("shutdown did not delete the queue: %v", ss.calls())
	}
}

// TestRunResumesInheritedQueue: a resumed runner must skip /register
// entirely (registering would move the cursor past everything posted
// during the reload) and must still fire OnRegister, which is what
// resyncs the followed-channel set.
func TestRunResumesInheritedQueue(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	ss := newScript(t,
		func(r *http.Request) (int, string) {
			if got := r.URL.Query().Get("last_event_id"); got != "41" {
				t.Errorf("resumed poll last_event_id = %q, want 41", got)
			}
			return 200, `{"result":"success","msg":"","events":[{"id":42,"type":"message","message":{"id":1,"content":"x"}}]}`
		},
		func(*http.Request) (int, string) {
			cancel()
			return 200, `{"result":"success","msg":"","events":[]}`
		},
	)
	var resynced int
	h := newHarness(t, ss, func(context.Context, Event) {}, func(cfg *RunnerConfig) {
		cfg.ResumeQueueID = "q-inherited"
		cfg.ResumeLastEventID = 41
		cfg.OnRegister = func(context.Context) { resynced++ }
	})

	if err := h.r.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() = %v", err)
	}
	for _, call := range ss.calls() {
		if strings.Contains(call, "/register") {
			t.Fatalf("a resumed runner registered a fresh queue: %v", ss.calls())
		}
	}
	if resynced != 1 {
		t.Fatalf("OnRegister fired %d times on resume, want 1", resynced)
	}
	if !h.logged("resuming inherited event queue q-inherited") {
		t.Fatal("resume was not logged")
	}
	if _, last := h.r.Cursor(); last != 42 {
		t.Fatalf("cursor = %d, want 42", last)
	}
}

// TestRunResumeOfADeadQueueFallsBackToRegister: an inherited queue can
// still be gone (a server restart during the exec). BAD_EVENT_QUEUE_ID
// is routine, and the runner must recover by registering fresh rather
// than wedging.
func TestRunResumeOfADeadQueueFallsBackToRegister(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	ss := newScript(t,
		func(*http.Request) (int, string) {
			return 400, `{"result":"error","msg":"Bad event queue id","code":"BAD_EVENT_QUEUE_ID","queue_id":"q-inherited"}`
		},
		registerOK("q-fresh", 0),
		func(*http.Request) (int, string) {
			cancel()
			return 200, `{"result":"success","msg":"","events":[]}`
		},
	)
	h := newHarness(t, ss, func(context.Context, Event) {}, func(cfg *RunnerConfig) {
		cfg.ResumeQueueID = "q-inherited"
		cfg.ResumeLastEventID = 41
	})
	if err := h.r.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() = %v", err)
	}
	if q, _ := h.r.Cursor(); q != "" {
		t.Fatalf("Cursor() = %q after shutdown, want empty", q)
	}
}

// TestHandoffDoesNotCancelDispatch: events already fetched when the
// reload signal lands must reach the handler with a LIVE context. The
// cursor has already advanced past them, so a handler that saw a
// cancelled context would drop work nobody will ever redeliver.
func TestHandoffDoesNotCancelDispatch(t *testing.T) {
	handoff := make(chan struct{})
	var once sync.Once
	ss := newScript(t,
		registerOK("q", 0),
		eventsOK(`{"id":1,"type":"message","message":{"id":9,"content":"a"}}`),
		func(r *http.Request) (int, string) {
			once.Do(func() { close(handoff) })
			<-r.Context().Done()
			return 200, `{"result":"success","msg":"","events":[]}`
		},
	)
	var sawErr error
	h := newHarness(t, ss, func(ctx context.Context, _ Event) { sawErr = ctx.Err() },
		func(cfg *RunnerConfig) { cfg.Handoff = handoff })
	if err := h.r.Run(context.Background()); !errors.Is(err, ErrHandoff) {
		t.Fatalf("Run() = %v", err)
	}
	if sawErr != nil {
		t.Fatalf("handler saw ctx.Err() = %v, want nil", sawErr)
	}
}

// TestDiscardTearsDownAHandedOffQueue: if a SIGTERM lands during the
// reload drain the caller abandons the re-exec, and the queue it was
// holding for a successor that will never exist must not be left
// unpolled for the server to collect.
func TestDiscardTearsDownAHandedOffQueue(t *testing.T) {
	handoff := make(chan struct{})
	var once sync.Once
	ss := newScript(t,
		registerOK("q-abandoned", 3),
		func(r *http.Request) (int, string) {
			once.Do(func() { close(handoff) })
			<-r.Context().Done()
			return 200, `{"result":"success","msg":"","events":[]}`
		},
		// Serves the DELETE that Discard issues. Without a step after
		// the blocking poll the script would repeat it and wedge.
		func(*http.Request) (int, string) { return 200, `{"result":"success","msg":""}` },
	)
	h := newHarness(t, ss, func(context.Context, Event) {},
		func(cfg *RunnerConfig) { cfg.Handoff = handoff })
	if err := h.r.Run(context.Background()); !errors.Is(err, ErrHandoff) {
		t.Fatalf("Run() = %v, want ErrHandoff", err)
	}
	if hasDelete(ss.calls()) {
		t.Fatal("the handoff itself deleted the queue")
	}
	h.r.Discard()
	if !hasDelete(ss.calls()) {
		t.Fatalf("Discard did not delete the queue: %v", ss.calls())
	}
	if q, _ := h.r.Cursor(); q != "" {
		t.Fatalf("Cursor() = %q after Discard, want empty", q)
	}
	// Idempotent: a second Discard (or one after a plain shutdown) is a
	// no-op rather than a spurious DELETE for an id we no longer hold.
	before := len(ss.calls())
	h.r.Discard()
	if len(ss.calls()) != before {
		t.Fatal("a second Discard issued another request")
	}
}

func hasDelete(calls []string) bool {
	for _, c := range calls {
		if strings.HasPrefix(c, "DELETE") {
			return true
		}
	}
	return false
}

// TestNewRunnerIgnoresAnEmptyResume keeps a cold start cold: an unset
// ResumeQueueID must not put the runner into resume mode with a bogus
// cursor.
func TestNewRunnerIgnoresAnEmptyResume(t *testing.T) {
	r, err := NewRunner(RunnerConfig{
		Client:            &Client{},
		Handle:            func(context.Context, Event) {},
		ResumeLastEventID: 99,
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	if q, last := r.Cursor(); q != "" || last != -1 {
		t.Fatalf("Cursor() = %q, %d; want \"\", -1", q, last)
	}
}
