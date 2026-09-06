package zulipproto

import (
	"context"
	"errors"
	"math/rand/v2"
	"net"
	"time"
)

// EventHandler consumes one event. It is called synchronously from the
// runner's loop, in queue order — the runner owns ordering, so a
// handler that wants concurrency must arrange it itself.
type EventHandler func(ctx context.Context, ev Event)

// Runner tuning defaults.
const (
	// DefaultSilence is roughly 2x the server's ~90s long-poll window.
	// Zulip emits heartbeat events on an idle queue, so total silence
	// for this long means the connection is wedged even though no
	// error was ever returned.
	DefaultSilence = 180 * time.Second
	// DefaultMaxBackoff caps the reconnect backoff.
	DefaultMaxBackoff = 30 * time.Second
	// baseBackoff is the first retry delay; it doubles from here.
	baseBackoff = 500 * time.Millisecond
	// DefaultDrainPollTimeout bounds ONE non-blocking poll of a queue
	// being swapped out. These polls return whatever the queue holds
	// immediately, so this is a fault bound, not a wait.
	DefaultDrainPollTimeout = 10 * time.Second
	// DefaultDrainBudget bounds the WHOLE drain of a queue being
	// swapped out, so a queue that keeps producing can never stall the
	// startup of a new image indefinitely.
	DefaultDrainBudget = 60 * time.Second
	// swapRegisterAttempts is how many times a swap tries to register
	// the replacement queue before giving up and dropping the
	// inherited one. The inherited queue still holds messages, so a
	// single 5xx must not cost them.
	swapRegisterAttempts = 5
)

// RunnerConfig configures a Runner.
type RunnerConfig struct {
	// Client is the API client. Required.
	Client *Client
	// EventTypes narrows the queue by event type, e.g.
	// {"message", "update_message"}.
	EventTypes []string
	// Narrow narrows the queue to specific channels, as
	// [operator, operand] pairs.
	Narrow [][2]string
	// Handle receives every non-heartbeat event, in order. Required.
	Handle EventHandler
	// OnRegister, if set, is called after every successful queue
	// registration — including re-registrations after a dead queue.
	// Events are lost while a queue is dead, so any state the caller
	// derives from the event stream must be resynced here or it drifts
	// silently. Called from the runner's goroutine, before the first
	// poll of the new queue.
	OnRegister func(ctx context.Context)
	// Logf receives operational messages. Optional.
	Logf func(format string, args ...any)
	// MaxBackoff caps the exponential reconnect backoff. 0 uses
	// DefaultMaxBackoff.
	MaxBackoff time.Duration
	// Silence is how long a queue may produce nothing at all — not even
	// a heartbeat — before it is torn down and re-registered. 0 uses
	// DefaultSilence.
	Silence time.Duration

	// Handoff, when non-nil, stops the loop as soon as it is closed —
	// WITHOUT deleting the event queue. That is the whole point: the
	// queue keeps buffering server-side while the relay re-execs, and
	// the successor process resumes it via ResumeQueueID. Run returns
	// ErrHandoff in that case, and Cursor reports what to hand on.
	Handoff <-chan struct{}
	// ResumeQueueID and ResumeLastEventID seed the loop with a queue
	// inherited from a previous process image instead of registering a
	// fresh one. ResumeQueueID == "" means a cold start.
	//
	// Registering fresh is not a neutral alternative: /register hands
	// back the server's CURRENT last_event_id, so every message posted
	// before that instant is behind the cursor and is never delivered.
	ResumeQueueID     string
	ResumeLastEventID int64
	// ResumeRegistration is the RegistrationFingerprint the inherited
	// queue was REGISTERED with, as recorded by the process image that
	// created it.
	//
	// A queue's event_types and narrow are fixed at /register and
	// cannot be changed afterwards, so an inherited queue is only
	// resumable when its registration is the one this image wants.
	// When it differs — or is absent, which is what an upgrade from an
	// image that never recorded one looks like — the queue is SWAPPED,
	// not simply dropped: a replacement is registered while the old
	// queue is still buffering, the old one is drained and dispatched,
	// and only then deleted. Nothing posted across the change is lost;
	// the overlap both queues saw is de-duplicated on event identity.
	ResumeRegistration string

	// DrainPollTimeout bounds one non-blocking poll of a queue being
	// swapped out. 0 uses DefaultDrainPollTimeout.
	DrainPollTimeout time.Duration
	// DrainBudget bounds the whole drain of a queue being swapped out.
	// If it expires the swap degrades to the lossy behaviour — the
	// replacement queue is used anyway and the gap is logged loudly.
	// 0 uses DefaultDrainBudget.
	DrainBudget time.Duration
	// QueueLifespan asks the server to keep this runner's queue for
	// that long without being polled. It must cover the longest a
	// reload drain can hold the queue unpolled — an agent turn can run
	// for tens of minutes, and the server's default is ten. 0 accepts
	// the server default.
	QueueLifespan time.Duration
	// DedupWindow is how many dispatched event identities to remember
	// so the swap overlap is delivered exactly once. 0 uses
	// DefaultDedupWindow.
	DedupWindow int

	// Now and Sleep are injected by tests so the backoff and liveness
	// logic can be driven without wall-clock waits. Nil uses the real
	// clock.
	Now   func() time.Time
	Sleep func(ctx context.Context, d time.Duration) error
	// Jitter perturbs a backoff delay. Nil uses a uniform 50–100% of d.
	Jitter func(d time.Duration) time.Duration
}

// Runner owns the register + long-poll loop and the last_event_id
// cursor.
//
// Cursor discipline is the whole contract here. Zulip can redeliver
// events across a reconnect, so every event with id <= the cursor is
// dropped, and the cursor only ever moves forward. Losing the cursor
// (a dead queue) is routine and costs at most a re-register.
type Runner struct {
	cfg RunnerConfig

	// queueID is "" whenever a fresh queue must be registered.
	queueID     string
	lastEventID int64
	lastActive  time.Time
	backoff     time.Duration
	// resuming is true until the first poll of an INHERITED queue, so
	// OnRegister still fires exactly once for a resumed queue: the
	// caller's derived state (the followed channel set) would otherwise
	// start empty and stay empty until the queue happened to die.
	resuming bool
	// registration is the fingerprint of what THIS image registers
	// with; an inherited queue is only resumable when it matches.
	registration string
	// queueRegistration is the fingerprint the queue currently HELD
	// was registered with. It is normally registration, and differs
	// only when a swap was interrupted mid-drain and the inherited
	// queue was handed back on: a successor told the wrong fingerprint
	// would resume a queue that cannot carry its events, which is the
	// exact defect this machinery exists to end.
	queueRegistration string
	// seen remembers the identities dispatched during a queue swap, so
	// the window where two queues overlap is delivered exactly once.
	// It is nil except during a swap — see swapQueue and dedup.go.
	seen *seenSet
}

// ErrHandoff is returned by Run when the loop stopped because its
// Handoff channel closed. The event queue is deliberately still alive
// on the server; the caller is expected to pass Cursor() to a successor
// process image and exec it promptly, because an unpolled queue is
// garbage-collected after a few minutes.
var ErrHandoff = errors.New("zulip: event loop handed off (queue left alive)")

// NewRunner constructs a Runner.
func NewRunner(cfg RunnerConfig) (*Runner, error) {
	if cfg.Client == nil {
		return nil, errors.New("zulip: runner needs a client")
	}
	if cfg.Handle == nil {
		return nil, errors.New("zulip: runner needs a handler")
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	if cfg.MaxBackoff <= 0 {
		cfg.MaxBackoff = DefaultMaxBackoff
	}
	if cfg.Silence <= 0 {
		cfg.Silence = DefaultSilence
	}
	if cfg.DrainPollTimeout <= 0 {
		cfg.DrainPollTimeout = DefaultDrainPollTimeout
	}
	if cfg.DrainBudget <= 0 {
		cfg.DrainBudget = DefaultDrainBudget
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Sleep == nil {
		cfg.Sleep = sleepCtx
	}
	if cfg.Jitter == nil {
		cfg.Jitter = jitter
	}
	r := &Runner{cfg: cfg, lastEventID: -1}
	r.registration = RegistrationFingerprint(cfg.EventTypes, cfg.Narrow)
	r.queueRegistration = r.registration
	if cfg.ResumeQueueID != "" {
		r.queueID = cfg.ResumeQueueID
		r.lastEventID = cfg.ResumeLastEventID
		r.queueRegistration = cfg.ResumeRegistration
		r.resuming = true
	}
	return r, nil
}

// Registration reports the fingerprint of the registration the queue
// this runner is holding was created with — what a successor image
// must match before it may resume that queue. Hand it on beside
// Cursor.
func (r *Runner) Registration() string { return r.queueRegistration }

// LastEventID exposes the cursor, for logging and tests.
func (r *Runner) LastEventID() int64 { return r.lastEventID }

// Cursor reports the queue and position the loop stopped at, for
// handing to a successor process image. QueueID is "" when there is no
// live queue to hand on. Call it only after Run has returned.
func (r *Runner) Cursor() (queueID string, lastEventID int64) {
	return r.queueID, r.lastEventID
}

// Run drives the loop until ctx is cancelled or Handoff fires.
//
// On ctx cancellation it tears the queue down best-effort and returns
// ctx.Err(). On handoff it leaves the queue ALIVE and returns
// ErrHandoff — see Cursor.
func (r *Runner) Run(ctx context.Context) error {
	// The poll context is what a handoff cancels: it aborts the
	// in-flight long poll immediately. Events already fetched are still
	// dispatched under the CALLER's ctx, so a batch retrieved a
	// microsecond before the reload signal is handled in full rather
	// than half-dropped behind an advanced cursor.
	pollCtx, cancelPoll := context.WithCancel(ctx)
	defer cancelPoll()
	if r.cfg.Handoff != nil {
		// Run waits for the watcher before returning. That is not
		// tidiness: without it, whether the watcher ever reaches its
		// pollCtx.Done() branch is a scheduling race, which makes the
		// 100% coverage gate fail at random. The loop below exits only
		// once pollCtx is done, so this can never block.
		watchDone := make(chan struct{})
		defer func() { <-watchDone }()
		go func() {
			defer close(watchDone)
			select {
			case <-r.cfg.Handoff:
				cancelPoll()
			case <-pollCtx.Done():
			}
		}()
	}
	if r.resuming {
		r.resuming = false
		if r.cfg.ResumeRegistration != r.registration {
			// A queue's event_types and narrow are frozen at
			// /register. Resuming one that was registered for a
			// different set means polling a queue that CANNOT carry
			// the events this image asks for — silently, forever,
			// because every subsequent reload resumes it again. Swap
			// it out, without losing what it is holding.
			r.swapQueue(pollCtx, ctx)
		} else {
			r.lastActive = r.cfg.Now()
			r.cfg.Logf("zulip: resuming inherited event queue %s (last_event_id=%d) — nothing posted during the reload is lost", r.queueID, r.lastEventID)
			if r.cfg.OnRegister != nil {
				r.cfg.OnRegister(ctx)
			}
		}
	}
	for pollCtx.Err() == nil {
		if r.queueID == "" {
			if !r.register(pollCtx, ctx) {
				continue
			}
		}
		r.poll(pollCtx, ctx)
	}
	// Distinguish "the caller is shutting us down" from "the caller is
	// about to exec a new image": only the former may delete the queue.
	if ctx.Err() == nil {
		return ErrHandoff
	}
	r.teardown()
	return ctx.Err()
}

// Discard deletes the event queue the loop stopped on, best-effort.
//
// It exists for exactly one case: Run handed off (leaving the queue
// alive for a successor image) and the caller then decided NOT to
// exec — an operator SIGTERM landing during the reload drain. Without
// it that queue would sit unpolled until the server garbage-collects
// it. Calling it after a normal shutdown is a no-op, since teardown
// already cleared the id.
func (r *Runner) Discard() { r.teardown() }

// register creates a fresh queue. pollCtx bounds the request; dispatch
// is the caller's context, handed to OnRegister so a resync started
// here is not cancelled by a handoff mid-flight. Returns false when the
// attempt failed and the caller should loop again (after the backoff it
// has already applied).
func (r *Runner) register(pollCtx, dispatch context.Context) bool {
	res, err := r.cfg.Client.Register(pollCtx, r.cfg.EventTypes, r.cfg.Narrow, r.cfg.QueueLifespan)
	if err != nil {
		r.cfg.Logf("zulip: register failed: %v", err)
		r.wait(pollCtx)
		return false
	}
	r.queueID = res.QueueID
	r.lastEventID = res.LastEventID
	r.queueRegistration = r.registration
	r.lastActive = r.cfg.Now()
	r.backoff = 0
	r.cfg.Logf("zulip: event queue %s registered (last_event_id=%d)", res.QueueID, res.LastEventID)
	r.warnUnsupportedLifespan(res)
	if r.cfg.OnRegister != nil {
		r.cfg.OnRegister(dispatch)
	}
	return true
}

// swapQueue replaces an inherited queue whose registration is not the
// one this image wants — without losing anything posted across the
// change.
//
// The ORDER is the whole design:
//
//  1. Register the replacement FIRST, while the old queue is still
//     alive and still buffering server-side. From that instant nothing
//     new can be missed by both.
//  2. Drain the old queue and dispatch what it holds, in order, before
//     the replacement is ever polled.
//  3. Only then delete it.
//
// Deleting first — which is what this used to do — puts everything
// posted between the predecessor's last poll and /register behind the
// new cursor, where it is never delivered.
//
// Everything posted between steps 1 and 3 lands in BOTH queues. Event
// ids are per-queue and cannot be compared across them, so the overlap
// is de-duplicated on event IDENTITY instead; see dispatch and
// dedup.go.
func (r *Runner) swapQueue(pollCtx, dispatch context.Context) {
	old, oldLast := r.queueID, r.lastEventID
	r.cfg.Logf("zulip: registration changed (%s); inherited queue %s cannot carry this image's events — registering a replacement, then draining it",
		DescribeRegistrationChange(r.cfg.ResumeRegistration, r.registration), old)
	// A single /register failure is not a reason to throw the old
	// queue away: it still holds messages, and a 5xx during a server
	// restart is routine. Retry under the ordinary backoff first.
	for attempt := 1; !r.register(pollCtx, dispatch); attempt++ {
		if pollCtx.Err() != nil {
			// A handoff or a shutdown landed while we were retrying.
			// The inherited queue is still alive and still holds
			// everything posted during the reload — keep it, with its
			// own registration, and let the successor redo the swap.
			r.cfg.Logf("zulip: swap interrupted before a replacement existed; handing inherited queue %s back on", old)
			return
		}
		if attempt >= swapRegisterAttempts {
			// There is nothing to swap TO. Drop the unusable inherited
			// queue and let the loop retry a fresh registration — the
			// one case where the gap is unavoidable.
			r.cfg.Logf("zulip: WARN no replacement queue for inherited queue %s after %d attempts — messages posted during the gap are not delivered", old, attempt)
			r.dropQueue()
			return
		}
	}
	// Arm the dedup window: everything dispatched from the old queue
	// from here on is a candidate to arrive again from the new one.
	r.seen = newSeenSet(r.cfg.DedupWindow)
	fresh, freshLast := r.queueID, r.lastEventID
	drained := r.drain(pollCtx, dispatch, old, oldLast)
	// Note: r.queueID is the replacement from here on.
	if pollCtx.Err() != nil {
		// A handoff (or a shutdown) landed mid-drain. The OLD queue
		// still holds everything the drain did not reach, and the new
		// one holds only the overlap — so keep the old one and throw
		// the replacement away. The successor image redoes the swap
		// from the drained cursor and loses nothing; on a shutdown,
		// teardown deletes the old queue instead.
		r.deleteQueue(fresh)
		r.queueID, r.lastEventID, r.queueRegistration = old, drained, r.cfg.ResumeRegistration
		r.seen = nil
		r.cfg.Logf("zulip: swap interrupted; handing inherited queue %s back on at event %d rather than losing what it still holds", old, drained)
		return
	}
	r.deleteQueue(old)
	r.queueID, r.lastEventID = fresh, freshLast
	// The drain may have taken a while; the replacement has only just
	// been registered, so the silence clock starts now.
	r.lastActive = r.cfg.Now()
}

// drain polls the outgoing queue from the inherited cursor until it
// holds nothing, dispatching everything it has in order. It returns
// the cursor it reached, which is what a swap interrupted mid-drain
// hands back on.
//
// The polls are non-blocking (DrainEvents): an empty result is the
// server stating the queue is empty, so no timeout has to stand in for
// that judgement. The whole drain is bounded anyway — if a queue keeps
// producing, the swap degrades to the old lossy behaviour (the
// replacement is already registered and is used regardless) and says
// so loudly rather than hanging.
func (r *Runner) drain(pollCtx, dispatch context.Context, queueID string, lastEventID int64) int64 {
	budget, cancelBudget := context.WithTimeout(pollCtx, r.cfg.DrainBudget)
	defer cancelBudget()
	n := 0
	for {
		pctx, cancelPoll := context.WithTimeout(budget, r.cfg.DrainPollTimeout)
		evs, err := r.cfg.Client.DrainEvents(pctx, queueID, lastEventID)
		cancelPoll()
		if err != nil {
			r.drainStopped(queueID, n, err, budget.Err(), pollCtx.Err() != nil)
			return lastEventID
		}
		if len(evs) == 0 {
			r.drainDone(queueID, n)
			return lastEventID
		}
		before := lastEventID
		for _, ev := range evs {
			// The outgoing queue may redeliver too; its cursor still
			// only moves forward.
			if ev.ID <= lastEventID {
				continue
			}
			lastEventID = ev.ID
			if ev.Type == EventHeartbeat {
				continue
			}
			n++
			r.dispatch(dispatch, ev)
		}
		if lastEventID == before {
			// Nothing new: polling again would ask the same question
			// with the same cursor, forever.
			r.drainDone(queueID, n)
			return lastEventID
		}
	}
}

// drainStopped narrates why a drain ended on an error: cleanly (the
// server had already collected the queue), or with a gap the operator
// must know about. A timeout is NOT clean here — a non-blocking poll
// that did not answer says nothing about whether the queue was empty.
func (r *Runner) drainStopped(queueID string, n int, err, budgetErr error, interrupted bool) {
	switch {
	case interrupted:
		// A reload or a stop cut the drain short. Nothing is lost by
		// it: the caller hands the un-drained queue back on rather
		// than deleting it.
		r.cfg.Logf("zulip: drain of inherited queue %s interrupted after %d event(s)", queueID, n)
	case budgetErr != nil:
		r.cfg.Logf("zulip: WARN drain of inherited queue %s did not finish (%v) after %d event(s) — anything still buffered there is NOT delivered",
			queueID, budgetErr, n)
	case IsBadEventQueue(err):
		// The queue was collected WITH whatever it still held: this is
		// a loss, not a tidy finish.
		r.cfg.Logf("zulip: WARN inherited queue %s expired before it could be drained (%d event(s) delivered) — anything it still held is NOT delivered",
			queueID, n)
	default:
		r.cfg.Logf("zulip: WARN drain of inherited queue %s failed after %d event(s): %v — anything still buffered there is NOT delivered",
			queueID, n, err)
	}
}

func (r *Runner) drainDone(queueID string, n int) {
	r.cfg.Logf("zulip: drained %d event(s) from inherited queue %s — nothing posted during the reload is lost", n, queueID)
}

// dispatch hands an event to the handler unless the very same event
// has already been delivered by the queue being swapped out.
//
// There are two redeliveries to defend against, and they need
// different mechanisms. WITHIN one queue ids are monotonic, so the
// cursor check in poll and drain covers a reconnect replay. ACROSS two
// queues — the overlap of a swap — ids are incomparable, so identity
// has to come from the event's content; message ids are realm-global.
//
// The identity check is deliberately NOT always on. An event identity
// can legitimately recur: a user who adds a reaction, removes it and
// adds it again produces the same (message, user, emoji, op) twice,
// and a permanent dedup set would silently swallow the second one. It
// is only during a swap that the same event can be DELIVERED twice, so
// that is the only window in which the set is armed — see swapQueue
// and retireDedup.
func (r *Runner) dispatch(ctx context.Context, ev Event) {
	if r.seen != nil {
		if key, ok := eventKey(ev); ok && !r.seen.add(key) {
			r.cfg.Logf("zulip: dropping duplicate %s event (%s) — already delivered by the queue being swapped out", ev.Type, key)
			return
		}
	}
	r.cfg.Handle(ctx, ev)
}

// retireDedup disarms the dedup window after the FIRST successful poll
// of the replacement queue.
//
// One poll is enough because the overlap is closed by then: every
// event that reached both queues was posted before the old queue was
// deleted, so it was already buffered in the new queue when that poll
// was issued, and GET /events returns everything a queue is holding.
func (r *Runner) retireDedup() {
	if r.seen == nil {
		return
	}
	r.seen = nil
	r.cfg.Logf("zulip: queue swap complete; duplicate detection off")
}

// warnUnsupportedLifespan reports a server that took the registration
// but ignored the queue lifespan. Zulip answers "success" either way,
// so without this the relay would believe a reload drain longer than
// the server's ten-minute default is safe when it is not.
func (r *Runner) warnUnsupportedLifespan(res RegisterResult) {
	if r.cfg.QueueLifespan <= 0 {
		return
	}
	for _, p := range res.IgnoredParameters {
		if p == "queue_lifespan_secs" {
			r.cfg.Logf("zulip: WARN this server ignores queue_lifespan_secs — a reload whose drain runs past the server's queue lifespan (10 minutes by default) loses the queue, and the new image will miss anything posted during it")
			return
		}
	}
}

// poll performs one long poll and dispatches whatever it returns.
// pollCtx bounds the HTTP request and is cancelled by a handoff;
// dispatch is the caller's context and is what the handler sees, so a
// batch already in hand is delivered in full.
func (r *Runner) poll(pollCtx, dispatch context.Context) {
	evs, err := r.cfg.Client.GetEvents(pollCtx, r.queueID, r.lastEventID)
	if err != nil {
		r.pollFailed(pollCtx, err)
		return
	}
	r.backoff = 0
	for _, ev := range evs {
		// Reconnect dedup: the server may redeliver, and the cursor
		// must never go backwards.
		if ev.ID <= r.lastEventID {
			continue
		}
		r.lastEventID = ev.ID
		r.lastActive = r.cfg.Now()
		if ev.Type == EventHeartbeat {
			// Liveness only. Advancing the cursor above is the entire
			// point of a heartbeat.
			continue
		}
		r.dispatch(dispatch, ev)
	}
	// One completed poll of the replacement queue closes the swap
	// overlap; anything after it is a genuinely new event.
	r.retireDedup()
	if r.cfg.Now().Sub(r.lastActive) > r.cfg.Silence {
		r.cfg.Logf("zulip: no events (not even a heartbeat) for %s — re-registering queue %s", r.cfg.Silence, r.queueID)
		r.dropQueue()
	}
}

// pollFailed classifies a failed long poll. A dead queue is routine; a
// clean long-poll timeout retries immediately; anything else backs off.
func (r *Runner) pollFailed(ctx context.Context, err error) {
	if ctx.Err() != nil {
		return
	}
	if IsBadEventQueue(err) {
		// Routine, not an error: queues die on server restart and on
		// idle GC. Re-register and carry on.
		r.cfg.Logf("zulip: event queue %s expired, registering a new one", r.queueID)
		r.dropQueue()
		r.backoff = 0
		return
	}
	if isTimeout(err) {
		// The long poll ran its full budget without an event. That is
		// the normal idle outcome, not a fault — reconnect at once.
		return
	}
	r.cfg.Logf("zulip: poll failed: %v", err)
	r.wait(ctx)
}

// dropQueue forgets the current queue so the next loop registers a
// fresh one, resetting the cursor as Zulip requires.
func (r *Runner) dropQueue() {
	r.deleteQueue(r.queueID)
	r.queueID = ""
	r.lastEventID = -1
}

// wait applies the jittered exponential backoff, doubling it for next
// time.
func (r *Runner) wait(ctx context.Context) {
	if r.backoff == 0 {
		r.backoff = baseBackoff
	}
	d := r.cfg.Jitter(r.backoff)
	if r.backoff < r.cfg.MaxBackoff {
		r.backoff *= 2
		if r.backoff > r.cfg.MaxBackoff {
			r.backoff = r.cfg.MaxBackoff
		}
	}
	_ = r.cfg.Sleep(ctx, d)
}

// teardown deletes the queue on shutdown. Best-effort with its own
// short budget, since the caller's ctx is already cancelled.
func (r *Runner) teardown() {
	if r.queueID == "" {
		return
	}
	r.deleteQueue(r.queueID)
	r.queueID = ""
}

func (r *Runner) deleteQueue(id string) {
	if id == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.cfg.Client.DeleteQueue(ctx, id); err != nil {
		r.cfg.Logf("zulip: delete queue %s (ignored): %v", id, err)
	}
}

// isTimeout reports whether err is a clean client-side long-poll
// expiry rather than a real fault.
func isTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// jitter returns a uniformly random 50–100% of d, so a fleet of relays
// reconnecting after a server restart does not do so in lockstep.
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	half := d / 2
	return half + time.Duration(rand.Int64N(int64(half)+1))
}
