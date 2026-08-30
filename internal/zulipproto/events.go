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
	// Logf receives operational messages. Optional.
	Logf func(format string, args ...any)
	// MaxBackoff caps the exponential reconnect backoff. 0 uses
	// DefaultMaxBackoff.
	MaxBackoff time.Duration
	// Silence is how long a queue may produce nothing at all — not even
	// a heartbeat — before it is torn down and re-registered. 0 uses
	// DefaultSilence.
	Silence time.Duration

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
}

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
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Sleep == nil {
		cfg.Sleep = sleepCtx
	}
	if cfg.Jitter == nil {
		cfg.Jitter = jitter
	}
	return &Runner{cfg: cfg, lastEventID: -1}, nil
}

// LastEventID exposes the cursor, for logging and tests.
func (r *Runner) LastEventID() int64 { return r.lastEventID }

// Run drives the loop until ctx is cancelled, then tears the queue
// down best-effort. It returns ctx.Err().
func (r *Runner) Run(ctx context.Context) error {
	defer r.teardown()
	for ctx.Err() == nil {
		if r.queueID == "" {
			if !r.register(ctx) {
				continue
			}
		}
		r.poll(ctx)
	}
	return ctx.Err()
}

// register creates a fresh queue. Returns false when the attempt
// failed and the caller should loop again (after the backoff it has
// already applied).
func (r *Runner) register(ctx context.Context) bool {
	res, err := r.cfg.Client.Register(ctx, r.cfg.EventTypes, r.cfg.Narrow)
	if err != nil {
		r.cfg.Logf("zulip: register failed: %v", err)
		r.wait(ctx)
		return false
	}
	r.queueID = res.QueueID
	r.lastEventID = res.LastEventID
	r.lastActive = r.cfg.Now()
	r.backoff = 0
	r.cfg.Logf("zulip: event queue %s registered (last_event_id=%d)", res.QueueID, res.LastEventID)
	return true
}

// poll performs one long poll and dispatches whatever it returns.
func (r *Runner) poll(ctx context.Context) {
	evs, err := r.cfg.Client.GetEvents(ctx, r.queueID, r.lastEventID)
	if err != nil {
		r.pollFailed(ctx, err)
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
		r.cfg.Handle(ctx, ev)
	}
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
