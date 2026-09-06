package zulipproto

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
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
		cfg.ResumeRegistration = RegistrationFingerprint(cfg.EventTypes, cfg.Narrow)
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
		cfg.ResumeRegistration = RegistrationFingerprint(cfg.EventTypes, cfg.Narrow)
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

// TestRunWithHandoffArmedButNeverFired: the ordinary life of a
// reload-capable relay is that the reload never comes. The watcher
// goroutine must then exit on the poll context instead of outliving
// Run — which is also what makes that branch deterministically
// covered, rather than won or lost by the scheduler in the
// shutdown-race test above.
func TestRunWithHandoffArmedButNeverFired(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	ss := newScript(t,
		registerOK("q-quiet", 1),
		func(*http.Request) (int, string) {
			cancel()
			return 200, `{"result":"success","msg":"","events":[]}`
		},
	)
	h := newHarness(t, ss, func(context.Context, Event) {},
		func(cfg *RunnerConfig) { cfg.Handoff = make(chan struct{}) })

	if err := h.r.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() = %v, want context.Canceled", err)
	}
	if !hasDelete(ss.calls()) {
		t.Fatalf("shutdown did not delete the queue: %v", ss.calls())
	}
}

// swapTune configures a runner that inherits a queue registered
// WITHOUT reaction events while this image wants them — the production
// defect the fingerprint exists for, and the case that must now swap
// queues losslessly instead of dropping one.
func swapTune(cfg *RunnerConfig) {
	cfg.EventTypes = []string{"message", "reaction"}
	cfg.ResumeQueueID = "q-stale"
	cfg.ResumeLastEventID = 41
	cfg.ResumeRegistration = RegistrationFingerprint([]string{"message"}, cfg.Narrow)
}

// TestRunSwapsAnUnresumableQueueLosslessly is the contract of the
// whole swap: the replacement queue is registered while the old one is
// still alive and buffering, the old one is then drained and
// dispatched, and only THEN deleted. Registering after the delete —
// which is what this used to do — puts everything posted in between
// behind the new cursor, where it is never delivered.
func TestRunSwapsAnUnresumableQueueLosslessly(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var ss *scriptServer
	ss = newScript(t,
		// 1. The replacement is registered FIRST.
		func(r *http.Request) (int, string) {
			if r.Method != http.MethodPost || r.URL.Path != "/api/v1/register" {
				t.Errorf("first call = %s %s, want POST /register", r.Method, r.URL.Path)
			}
			return 200, `{"result":"success","msg":"","queue_id":"q-fresh","last_event_id":900}`
		},
		// 2. The old queue is drained from the inherited cursor. It
		// holds a redelivery (id 41, already seen), a heartbeat, and
		// the message posted during the reload.
		func(r *http.Request) (int, string) {
			if got := r.URL.Query().Get("queue_id"); got != "q-stale" {
				t.Errorf("drain polled queue %q, want q-stale", got)
			}
			if got := r.URL.Query().Get("last_event_id"); got != "41" {
				t.Errorf("drain last_event_id = %q, want 41", got)
			}
			if got := r.URL.Query().Get("dont_block"); got != "true" {
				t.Errorf("drain poll dont_block = %q, want true — an empty result must be the server's answer, not a timeout", got)
			}
			if hasDelete(ss.calls()) {
				t.Error("the old queue was deleted before it was drained")
			}
			return 200, `{"result":"success","msg":"","events":[` +
				`{"id":41,"type":"message","message":{"id":499,"content":"old"}},` +
				`{"id":42,"type":"heartbeat"},` +
				`{"id":43,"type":"message","message":{"id":500,"content":"during the reload"}}]}`
		},
		// 3. Nothing left: that is the drain completing.
		eventsOK(``),
		// 4. Only now is the old queue deleted.
		func(r *http.Request) (int, string) {
			if r.Method != http.MethodDelete {
				t.Errorf("call 4 = %s %s, want DELETE of the drained queue", r.Method, r.URL.Path)
			}
			return 200, `{"result":"success","msg":""}`
		},
		// 5. The replacement is polled last, and redelivers the same
		// message: it was posted inside the overlap, so it landed in
		// both queues.
		func(r *http.Request) (int, string) {
			if got := r.URL.Query().Get("queue_id"); got != "q-fresh" {
				t.Errorf("polled queue %q, want q-fresh", got)
			}
			return 200, `{"result":"success","msg":"","events":[{"id":901,"type":"message","message":{"id":500,"content":"during the reload"}}]}`
		},
		func(*http.Request) (int, string) {
			cancel()
			return 200, `{"result":"success","msg":"","events":[]}`
		},
	)
	var got []int64
	var registered int
	h := newHarness(t, ss, func(_ context.Context, ev Event) { got = append(got, ev.Message.ID) },
		func(cfg *RunnerConfig) {
			swapTune(cfg)
			cfg.OnRegister = func(context.Context) { registered++ }
		})

	if err := h.r.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() = %v, want context.Canceled", err)
	}
	if h.logged("resuming inherited event queue") {
		t.Fatalf("a stale registration was resumed: %v", ss.calls())
	}
	if !h.logged("registration changed (event types +reaction); inherited queue q-stale cannot carry") {
		t.Fatal("mismatch was not narrated")
	}
	if !h.logged("drained 1 event(s) from inherited queue q-stale") {
		t.Fatalf("the drain was not narrated: %v", h.logs)
	}
	if len(got) != 1 || got[0] != 500 {
		t.Fatalf("dispatched message ids %v, want exactly [500] — the reload-window message, once", got)
	}
	if !h.logged("dropping duplicate message event (message:500)") {
		t.Fatal("the overlap redelivery was not reported as a duplicate")
	}
	if registered != 1 {
		t.Fatalf("OnRegister fired %d times, want 1", registered)
	}
}

// TestSwapDrainOuterBoundDegradesLoudly: a queue that keeps producing
// must not stall the new image forever. The replacement is already
// registered, so the swap completes on it — but the events left
// undrained are a real gap and the log must say so.
func TestSwapDrainOuterBoundDegradesLoudly(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	ss := newScript(t,
		registerOK("q-fresh", 900),
		func(*http.Request) (int, string) { return 200, `{"result":"success","msg":""}` },
		func(*http.Request) (int, string) {
			cancel()
			return 200, `{"result":"success","msg":"","events":[]}`
		},
	)
	h := newHarness(t, ss, func(context.Context, Event) {}, func(cfg *RunnerConfig) {
		swapTune(cfg)
		// Already spent: the very first drain poll is over budget.
		cfg.DrainBudget = time.Nanosecond
	})

	if err := h.r.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() = %v, want context.Canceled", err)
	}
	if !h.logged("drain of inherited queue q-stale did not finish (context deadline exceeded)") {
		t.Fatalf("the drain gap was not reported: %v", h.logs)
	}
	if !h.logged("NOT delivered") {
		t.Fatal("the gap was not stated in the operator's terms")
	}
	if q, _ := h.r.Cursor(); q != "" {
		t.Fatalf("Cursor() = %q after shutdown, want empty", q)
	}
	if !hasDelete(ss.calls()) {
		t.Fatalf("the drained-out queue was not deleted: %v", ss.calls())
	}
}

// TestSwapDrainTimeoutIsAFaultNotAnEmptyQueue: drain polls are
// non-blocking, so an empty result is the SERVER stating the queue is
// empty. A poll that never answered states nothing — treating it as
// "drained" would delete a queue that still held messages, which is
// exactly the loss this design exists to prevent.
func TestSwapDrainTimeoutIsAFaultNotAnEmptyQueue(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	ss := newScript(t,
		registerOK("q-fresh", 900),
		func(r *http.Request) (int, string) {
			<-r.Context().Done()
			return 200, `{"result":"success","msg":"","events":[]}`
		},
		func(*http.Request) (int, string) { return 200, `{"result":"success","msg":""}` },
		func(*http.Request) (int, string) {
			cancel()
			return 200, `{"result":"success","msg":"","events":[]}`
		},
	)
	h := newHarness(t, ss, func(context.Context, Event) {}, func(cfg *RunnerConfig) {
		swapTune(cfg)
		cfg.DrainPollTimeout = 50 * time.Millisecond
	})

	if err := h.r.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() = %v, want context.Canceled", err)
	}
	if h.logged("drained 0 event(s)") {
		t.Fatalf("a timed-out poll was mistaken for an empty queue: %v", h.logs)
	}
	if !h.logged("drain of inherited queue q-stale failed after 0 event(s)") {
		t.Fatalf("the fault was not reported: %v", h.logs)
	}
}

// TestSwapDrainStopsWhenTheCursorCannotAdvance: a server that keeps
// returning only events at or behind the cursor would otherwise be
// polled in a tight loop until the drain budget expired.
func TestSwapDrainStopsWhenTheCursorCannotAdvance(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	ss := newScript(t,
		registerOK("q-fresh", 900),
		eventsOK(`{"id":41,"type":"message","message":{"id":499,"content":"already seen"}}`),
		func(*http.Request) (int, string) { return 200, `{"result":"success","msg":""}` },
		func(*http.Request) (int, string) {
			cancel()
			return 200, `{"result":"success","msg":"","events":[]}`
		},
	)
	var dispatched int
	h := newHarness(t, ss, func(context.Context, Event) { dispatched++ }, swapTune)

	if err := h.r.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() = %v, want context.Canceled", err)
	}
	if dispatched != 0 {
		t.Fatalf("dispatched %d events at or behind the inherited cursor", dispatched)
	}
	if !h.logged("drained 0 event(s) from inherited queue q-stale") {
		t.Fatalf("the drain did not stop on a stalled cursor: %v", h.logs)
	}
}

// TestSwapInterruptedMidDrainHandsTheOldQueueBack: a reload signal can
// land while the swap is draining. The replacement holds only the
// overlap; the OLD queue still holds everything the drain never
// reached — so it is the one worth handing to the successor, together
// with ITS registration, or the successor would resume a queue that
// cannot carry its events.
func TestSwapInterruptedMidDrainHandsTheOldQueueBack(t *testing.T) {
	handoff := make(chan struct{})
	var once sync.Once
	ss := newScript(t,
		registerOK("q-fresh", 900),
		eventsOK(`{"id":42,"type":"message","message":{"id":500,"content":"drained"}}`),
		func(r *http.Request) (int, string) {
			once.Do(func() { close(handoff) })
			<-r.Context().Done()
			return 200, `{"result":"success","msg":"","events":[]}`
		},
		func(r *http.Request) (int, string) {
			if r.Method != http.MethodDelete {
				t.Errorf("call 4 = %s %s, want DELETE of the abandoned replacement", r.Method, r.URL.Path)
			}
			body, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(body), "queue_id=q-fresh") {
				t.Errorf("DELETE body = %q, want the replacement q-fresh", body)
			}
			return 200, `{"result":"success","msg":""}`
		},
	)
	var got []int64
	h := newHarness(t, ss, func(_ context.Context, ev Event) { got = append(got, ev.Message.ID) },
		func(cfg *RunnerConfig) {
			swapTune(cfg)
			cfg.Handoff = handoff
		})

	if err := h.r.Run(context.Background()); !errors.Is(err, ErrHandoff) {
		t.Fatalf("Run() = %v, want ErrHandoff", err)
	}
	q, last := h.r.Cursor()
	if q != "q-stale" || last != 42 {
		t.Fatalf("Cursor() = %q, %d; want q-stale, 42 — the queue still holding the undrained events", q, last)
	}
	if want := RegistrationFingerprint([]string{"message"}, [][2]string{{"channel", "4"}}); h.r.Registration() != want {
		t.Fatalf("Registration() = %q, want the inherited queue's own %q", h.r.Registration(), want)
	}
	if len(got) != 1 || got[0] != 500 {
		t.Fatalf("dispatched %v, want the one drained message", got)
	}
	if !h.logged("swap interrupted; handing inherited queue q-stale back on at event 42") {
		t.Fatalf("the interrupted swap was not narrated: %v", h.logs)
	}
}

// TestSwapDrainOfADeadQueueIsAReportedLoss: the inherited queue can
// already be gone (a server restart during the exec). It was collected
// WITH whatever it still held, so this is a loss to name — not a tidy
// finish to log as "nothing lost".
func TestSwapDrainOfADeadQueueIsAReportedLoss(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	ss := newScript(t,
		registerOK("q-fresh", 900),
		func(*http.Request) (int, string) {
			return 400, `{"result":"error","msg":"Bad event queue id","code":"BAD_EVENT_QUEUE_ID","queue_id":"q-stale"}`
		},
		func(*http.Request) (int, string) { return 200, `{"result":"success","msg":""}` },
		func(*http.Request) (int, string) {
			cancel()
			return 200, `{"result":"success","msg":"","events":[]}`
		},
	)
	h := newHarness(t, ss, func(context.Context, Event) {}, swapTune)

	if err := h.r.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() = %v, want context.Canceled", err)
	}
	if !h.logged("inherited queue q-stale expired before it could be drained") {
		t.Fatalf("the expired queue was not reported as a loss: %v", h.logs)
	}
	if h.logged("nothing posted during the reload is lost") {
		t.Fatal("a lost queue was narrated as lossless")
	}
}

// TestSwapDrainFailureIsReported: any other fault ends the drain, and
// whatever the old queue still held is a gap the operator must see.
func TestSwapDrainFailureIsReported(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	ss := newScript(t,
		registerOK("q-fresh", 900),
		func(*http.Request) (int, string) {
			return 500, `{"result":"error","msg":"Internal server error"}`
		},
		func(*http.Request) (int, string) { return 200, `{"result":"success","msg":""}` },
		func(*http.Request) (int, string) {
			cancel()
			return 200, `{"result":"success","msg":"","events":[]}`
		},
	)
	h := newHarness(t, ss, func(context.Context, Event) {}, swapTune)

	if err := h.r.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() = %v, want context.Canceled", err)
	}
	if !h.logged("drain of inherited queue q-stale failed after 0 event(s)") {
		t.Fatalf("the failed drain was not reported: %v", h.logs)
	}
}

// TestSwapWithoutAReplacementFallsBack: if /register itself fails
// there is nothing to swap TO. The unusable queue is dropped and the
// loop retries a fresh registration — the one case where the gap is
// unavoidable, and it is logged as such.
func TestSwapWithoutAReplacementFallsBack(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var registerAttempts int
	ss := newScript(t,
		func(r *http.Request) (int, string) {
			switch {
			case strings.Contains(r.URL.Path, "/register"):
				registerAttempts++
				if registerAttempts <= swapRegisterAttempts {
					return 500, `{"result":"error","msg":"Internal server error"}`
				}
				return 200, `{"result":"success","msg":"","queue_id":"q-second-try","last_event_id":10}`
			case r.Method == http.MethodDelete:
				return 200, `{"result":"success","msg":""}`
			}
			cancel()
			return 200, `{"result":"success","msg":"","events":[]}`
		},
	)
	h := newHarness(t, ss, func(context.Context, Event) {}, swapTune)

	if err := h.r.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() = %v, want context.Canceled", err)
	}
	if registerAttempts != swapRegisterAttempts+1 {
		t.Fatalf("tried /register %d times, want %d — one 5xx must not cost the messages the old queue holds",
			registerAttempts, swapRegisterAttempts+1)
	}
	if !h.logged("no replacement queue for inherited queue q-stale after 5 attempts") {
		t.Fatalf("the unavoidable gap was not reported: %v", h.logs)
	}
	if !h.logged("messages posted during the gap are not delivered") {
		t.Fatal("the cost was not stated")
	}
	if !hasDelete(ss.calls()) {
		t.Fatalf("the unusable queue was not dropped: %v", ss.calls())
	}
}

// TestSwapInterruptedBeforeAReplacementKeepsTheOldQueue: a reload
// signal during the /register retries must not cost the inherited
// queue. It is still alive and still holds everything posted during
// the reload, so it is handed back on rather than deleted.
func TestSwapInterruptedBeforeAReplacementKeepsTheOldQueue(t *testing.T) {
	handoff := make(chan struct{})
	var once sync.Once
	// Closed before the server is torn down (a defer runs ahead of
	// t.Cleanup), so a handler still holding its request cannot wedge
	// the shutdown.
	stop := make(chan struct{})
	defer close(stop)
	ss := newScript(t,
		func(r *http.Request) (int, string) {
			if r.Method == http.MethodDelete {
				t.Errorf("the inherited queue was deleted: %s %s", r.Method, r.URL.Path)
			}
			once.Do(func() { close(handoff) })
			// Hold the request until the handoff has actually
			// cancelled the poll context: the assertion is about what
			// happens THEN, not about winning a scheduling race.
			select {
			case <-r.Context().Done():
			case <-stop:
			}
			return 500, `{"result":"error","msg":"Internal server error"}`
		},
	)
	h := newHarness(t, ss, func(context.Context, Event) {}, func(cfg *RunnerConfig) {
		swapTune(cfg)
		cfg.Handoff = handoff
	})

	if err := h.r.Run(context.Background()); !errors.Is(err, ErrHandoff) {
		t.Fatalf("Run() = %v, want ErrHandoff", err)
	}
	q, last := h.r.Cursor()
	if q != "q-stale" || last != 41 {
		t.Fatalf("Cursor() = %q, %d; want the inherited q-stale at 41", q, last)
	}
	if want := RegistrationFingerprint([]string{"message"}, [][2]string{{"channel", "4"}}); h.r.Registration() != want {
		t.Fatalf("Registration() = %q, want the inherited queue's own", h.r.Registration())
	}
	if !h.logged("swap interrupted before a replacement existed") {
		t.Fatalf("the interruption was not narrated: %v", h.logs)
	}
}

// TestSwapDedupRetiresAfterTheOverlap: duplicate detection covers the
// swap window and NOTHING else. An event identity can legitimately
// recur — a user removes a reaction and adds it back — so a permanent
// dedup set would silently swallow the second one.
func TestSwapDedupRetiresAfterTheOverlap(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	const reaction = `{"type":"reaction","op":"add","message_id":700,"user_id":12,"emoji_name":"wastebasket"}`
	ss := newScript(t,
		registerOK("q-fresh", 900),
		eventsOK(`{"id":42,`+reaction[1:]),
		eventsOK(``),
		func(*http.Request) (int, string) { return 200, `{"result":"success","msg":""}` },
		// The overlap sighting of the very same reaction: dropped.
		eventsOK(`{"id":901,`+reaction[1:]),
		// A later poll: the user really did react again, and this one
		// must get through.
		eventsOK(`{"id":902,`+reaction[1:]),
		func(*http.Request) (int, string) {
			cancel()
			return 200, `{"result":"success","msg":"","events":[]}`
		},
	)
	var reactions int
	h := newHarness(t, ss, func(_ context.Context, ev Event) {
		if ev.Type == EventReaction {
			reactions++
		}
	}, swapTune)

	if err := h.r.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() = %v, want context.Canceled", err)
	}
	if !h.logged("dropping duplicate reaction event (reaction:700:12:wastebasket:add)") {
		t.Fatalf("the overlap sighting was not dropped: %v", h.logs)
	}
	if !h.logged("queue swap complete; duplicate detection off") {
		t.Fatal("the dedup window was never retired")
	}
	if reactions != 2 {
		t.Fatalf("dispatched %d reactions, want 2 — the drained one and the genuine re-add", reactions)
	}
}

// TestRunDoesNotResumeAnUnrecordedRegistration covers the upgrade that
// installs this fix: the predecessor image never recorded a
// registration, so the inherited queue's shape is UNKNOWN. Unknown
// must mean different — otherwise the very reload that ships the fix
// resumes the broken queue and the defect survives it.
func TestRunDoesNotResumeAnUnrecordedRegistration(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	ss := newScript(t,
		registerOK("q-fresh", 5),
		eventsOK(``),
		func(*http.Request) (int, string) { return 200, `{"result":"success","msg":""}` },
		func(*http.Request) (int, string) {
			cancel()
			return 200, `{"result":"success","msg":"","events":[]}`
		},
	)
	h := newHarness(t, ss, func(context.Context, Event) {}, func(cfg *RunnerConfig) {
		cfg.ResumeQueueID = "q-unknown"
		cfg.ResumeLastEventID = 41
	})

	if err := h.r.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() = %v, want context.Canceled", err)
	}
	if h.logged("resuming inherited event queue") {
		t.Fatal("an unrecorded registration was resumed")
	}
	if !h.logged("recorded no registration") {
		t.Fatal("the unknown registration was not narrated")
	}
	if !hasDelete(ss.calls()) {
		t.Fatalf("the unresumable queue was not deleted: %v", ss.calls())
	}
}

// TestRunnerRegistrationIsWhatItRegisteredWith: the fingerprint handed
// to a successor must describe THIS image's registration, so the
// successor can compare it against its own.
func TestRunnerRegistrationIsWhatItRegisteredWith(t *testing.T) {
	r, err := NewRunner(RunnerConfig{
		Client:     &Client{},
		Handle:     func(context.Context, Event) {},
		EventTypes: []string{"update_message", "message"},
		Narrow:     [][2]string{{"stream", "fleet"}},
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	want := RegistrationFingerprint([]string{"message", "update_message"}, [][2]string{{"stream", "fleet"}})
	if got := r.Registration(); got != want {
		t.Fatalf("Registration() = %q, want %q", got, want)
	}
}

// TestRegisterWarnsWhenTheServerIgnoresTheQueueLifespan: Zulip answers
// "success" to a request carrying a parameter it does not understand
// and lists it in ignored_parameters_unsupported. Without this warning
// the relay would believe a long reload drain is safe when the queue
// will in fact be collected out from under it.
func TestRegisterWarnsWhenTheServerIgnoresTheQueueLifespan(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	ss := newScript(t,
		func(r *http.Request) (int, string) {
			body, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(body), "queue_lifespan_secs=2100") {
				t.Errorf("register body = %q, want queue_lifespan_secs=2100", body)
			}
			return 200, `{"result":"success","msg":"","queue_id":"q","last_event_id":0,"ignored_parameters_unsupported":["queue_lifespan_secs"]}`
		},
		func(*http.Request) (int, string) {
			cancel()
			return 200, `{"result":"success","msg":"","events":[]}`
		},
	)
	h := newHarness(t, ss, func(context.Context, Event) {}, func(cfg *RunnerConfig) {
		cfg.QueueLifespan = 35 * time.Minute
	})
	if err := h.r.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() = %v", err)
	}
	if !h.logged("WARN this server ignores queue_lifespan_secs") {
		t.Fatalf("an ignored lifespan was not reported: %v", h.logs)
	}
}

// TestRegisterAcceptsTheQueueLifespanQuietly: the normal case says
// nothing. A server that honours the parameter does not list it.
func TestRegisterAcceptsTheQueueLifespanQuietly(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	ss := newScript(t,
		func(*http.Request) (int, string) {
			return 200, `{"result":"success","msg":"","queue_id":"q","last_event_id":0,"ignored_parameters_unsupported":["something_else"]}`
		},
		func(*http.Request) (int, string) {
			cancel()
			return 200, `{"result":"success","msg":"","events":[]}`
		},
	)
	h := newHarness(t, ss, func(context.Context, Event) {}, func(cfg *RunnerConfig) {
		cfg.QueueLifespan = 35 * time.Minute
	})
	if err := h.r.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() = %v", err)
	}
	if h.logged("WARN this server ignores") {
		t.Fatalf("a honoured lifespan warned: %v", h.logs)
	}
}
