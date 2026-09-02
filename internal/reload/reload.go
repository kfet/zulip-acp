// Package reload implements zulip-acp's graceful restart: drain the
// in-flight turns, then replace this process image in place with the
// on-disk binary, carrying the Zulip event-queue cursor across the exec.
//
// # Why in-place exec, and not a master/worker supervisor
//
// poe-acp runs a master/worker supervisor, and copying it here would be
// a mistake. poe-acp is an HTTP SERVER: its scarce resource is a BOUND
// LISTEN SOCKET, and any instant in which that socket is closed is an
// instant in which clients get ECONNREFUSED. The supervisor exists
// solely to hold the socket across worker generations.
//
// zulip-acp holds no socket. It is a long-poll CLIENT, and while it is
// not polling, nothing is lost: incoming messages accumulate
// SERVER-SIDE in the Zulip event queue and are delivered on the next
// GetEvents for that queue id. The buffer we would need a supervisor to
// provide already exists, remotely, for free — and a Zulip queue
// outlives an exec by orders of magnitude (idle GC is minutes; an exec
// is sub-second). So there is nothing for a supervisor to hold, and the
// whole apparatus of control pipes, parent-death pipes, ready signalling
// and drain ordering buys exactly nothing.
//
// # The sequence
//
//  1. SIGHUP arrives (ExecReload=/bin/kill -HUP $MAINPID).
//  2. The zulipproto.Runner stops polling but does NOT delete its
//     queue; queue_id and last_event_id are kept.
//  3. WaitIdle blocks until every in-flight turn has finished posting,
//     bounded by a deadline. THIS is what makes a reply survive a
//     reload: an agent hosted by this relay can run
//     `systemctl --user reload zulip-acp` from inside its own turn,
//     the call returns as soon as the signal is delivered, and the
//     relay waits for that very turn to finish before going anywhere.
//  4. The ACP agent child is shut down cleanly.
//  5. syscall.Exec replaces the image with the on-disk binary, passing
//     the cursor forward in the environment. Same PID — systemd never
//     observes the service go away, so Restart= is never triggered and
//     Type=simple needs no readiness handshake.
//  6. The new image finds the cursor in its environment and RESUMES
//     GetEvents on that queue instead of calling /register. No gap
//     (nothing posted in the window is skipped) and no double delivery
//     (the cursor is exact).
//
// If the drain deadline expires with turns still running, the exec
// happens anyway and the successor's handler.MarkInterrupted annotates
// the truncated messages — the pre-existing behaviour for a hard
// restart, which is strictly the worst case here rather than the normal
// one.
//
// # Wire contract
//
// Two environment variables, set only by Exec and consumed only by
// Inherited:
//
//	ZULIP_ACP_QUEUE_ID        the live Zulip event queue to resume
//	ZULIP_ACP_LAST_EVENT_ID   the id of the last event already dispatched
//
// Both must be present and well-formed or neither is honoured: half a
// cursor is worse than none, because it would silently skip or replay
// events. A stale pair is never inherited by accident — Exec strips
// both from the base environment before appending its own.
// The cursor must never reach the ACP agent. It is a relay capability:
// a queue id plus the bot's credentials is enough to poll the relay's
// own event queue and silently divert its messages. The agent is driven
// by text from people who are not the operator, so both vars are
// scrubbed from the child's environment alongside the API key — see
// config.Config.AgentClientConfig and AgentEnvNames.
//
// The exec itself is unix-only by deployment target (syscall.Exec, see
// exec.go); everything in THIS file — the cursor type and its env round
// trip — is portable, so a package that merely needs to NAME the
// contract does not take on a build constraint.
package reload

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	// EnvQueueID names the env var carrying the live Zulip event queue
	// id across the exec.
	EnvQueueID = "ZULIP_ACP_QUEUE_ID"
	// EnvLastEventID names the env var carrying the cursor into that
	// queue: the id of the last event the previous image dispatched.
	EnvLastEventID = "ZULIP_ACP_LAST_EVENT_ID"
)

// AgentEnvNames lists the reload contract variables that must be
// scrubbed from the ACP agent's environment. A queue id is a relay
// capability, not configuration: combined with credentials it lets its
// holder poll the relay's own event queue and take delivery of messages
// meant for the relay. The agent is driven by untrusted text, so it
// gets the same treatment as the bot API key.
func AgentEnvNames() []string { return []string{EnvQueueID, EnvLastEventID} }

// Cursor is a position in a Zulip event queue: the queue to poll and
// the id of the last event already dispatched from it. The zero value
// means "no live queue"; Valid reports that.
type Cursor struct {
	QueueID     string
	LastEventID int64
}

// Valid reports whether c names a queue that can be resumed.
func (c Cursor) Valid() bool { return c.QueueID != "" }

func (c Cursor) String() string {
	if !c.Valid() {
		return "no queue"
	}
	return fmt.Sprintf("queue %s at event %d", c.QueueID, c.LastEventID)
}

// Inherited returns the cursor this process was exec'd with, or the
// zero Cursor on a cold start.
//
// A partial or malformed pair returns the zero Cursor AND an error: the
// caller must register a fresh queue and say so loudly, because
// guessing at half a cursor is how events get silently skipped.
func Inherited() (Cursor, error) {
	q, qok := os.LookupEnv(EnvQueueID)
	l, lok := os.LookupEnv(EnvLastEventID)
	switch {
	case !qok && !lok:
		return Cursor{}, nil
	case !qok || !lok:
		return Cursor{}, fmt.Errorf("reload: incomplete inherited cursor (%s set: %t, %s set: %t)", EnvQueueID, qok, EnvLastEventID, lok)
	case q == "":
		return Cursor{}, fmt.Errorf("reload: inherited %s is empty", EnvQueueID)
	}
	last, err := strconv.ParseInt(l, 10, 64)
	if err != nil {
		return Cursor{}, fmt.Errorf("reload: inherited %s=%q: %w", EnvLastEventID, l, err)
	}
	return Cursor{QueueID: q, LastEventID: last}, nil
}

// Environ returns base with any pre-existing cursor variables removed
// and c's appended when c is valid. Stripping first is what stops a
// stale cursor from an earlier reload surviving into a generation that
// has no live queue to hand on.
func Environ(base []string, c Cursor) []string {
	out := make([]string, 0, len(base)+2)
	for _, kv := range base {
		if strings.HasPrefix(kv, EnvQueueID+"=") || strings.HasPrefix(kv, EnvLastEventID+"=") {
			continue
		}
		out = append(out, kv)
	}
	if c.Valid() {
		out = append(out,
			EnvQueueID+"="+c.QueueID,
			EnvLastEventID+"="+strconv.FormatInt(c.LastEventID, 10),
		)
	}
	return out
}

// ---------------------------------------------------------------------------
// Draining
// ---------------------------------------------------------------------------

// DefaultReloadDrain bounds the wait for in-flight turns on a RELOAD.
// Nothing external is waiting on this: systemd's ExecReload is just a
// kill(1) that has already returned, and the relay is not "down" while
// it drains — the Zulip queue is buffering for it. Agent turns
// legitimately run tens of minutes, and prompt_timeout is what bounds a
// turn as work, so this is a leak backstop rather than a working bound.
const DefaultReloadDrain = 30 * time.Minute

// DefaultStopDrain bounds the wait for in-flight turns on a SERVICE
// STOP. Something external IS waiting here — systemd SIGTERMs the
// cgroup and SIGKILLs it at TimeoutStopSec (90s by default) — so the
// budget must stay comfortably underneath it.
const DefaultStopDrain = 30 * time.Second

// IdleWaiter is the subset of *handler.Handler that Drain drives: a
// wait that returns when no turn is in flight, or when ctx expires.
type IdleWaiter interface {
	WaitIdle(ctx context.Context) error
}

// Drain waits for w's in-flight turns to finish, bounded by deadline
// AND by parent. It reports whether the drain completed gracefully;
// false means it was cut short — either the deadline expired with turns
// still running, or parent was cancelled.
//
// parent matters on the RELOAD path, where the deadline is deliberately
// long (DefaultReloadDrain) because nothing external is waiting. Pass
// the process's signal context there: a SIGTERM arriving mid-reload is
// an operator saying "stop now", and it must win over a 30-minute
// drain rather than be ignored until systemd SIGKILLs the cgroup. On
// the STOP path the signal context is already cancelled, so the caller
// passes context.Background() — otherwise the drain would return
// instantly and never drain at all.
//
// deadline <= 0 means DefaultStopDrain: there is deliberately no "wait
// forever" value, because an agent child that wedges must not be able
// to make the relay unstoppable.
func Drain(parent context.Context, w IdleWaiter, deadline time.Duration) (bool, error) {
	if deadline <= 0 {
		deadline = DefaultStopDrain
	}
	ctx, cancel := context.WithTimeout(parent, deadline)
	defer cancel()
	err := w.WaitIdle(ctx)
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return false, err
	}
	return true, err
}

// ---------------------------------------------------------------------------
// Finishing the run
// ---------------------------------------------------------------------------

// FinishConfig describes one end-of-run drain decision.
type FinishConfig struct {
	// Reloading is true when the event loop stopped on a handoff
	// (zulipproto.ErrHandoff) rather than on a cancelled context.
	Reloading bool
	// Idle is what the drain waits on — the Handler.
	Idle IdleWaiter
	// ReloadDeadline and StopDeadline bound the two drains.
	ReloadDeadline, StopDeadline time.Duration
	// DiscardQueue tears down the event queue that was being held for a
	// successor image. Called only when a stop pre-empts a reload, so
	// the queue is not left unpolled for the server to collect.
	DiscardQueue func()
	// Logf receives the operator-facing narration. Required.
	Logf func(format string, args ...any)
}

// Finish performs the end-of-run drain and reports whether the caller
// should re-exec.
//
// It exists as a function rather than inline in main because it encodes
// a real decision — which of two very different contracts this drain
// falls under, and whether a stop has pre-empted a reload — and main is
// excluded from the coverage gate.
//
// ctx is the process signal context. It is passed to a RELOAD drain, so
// an operator SIGTERM cuts a 30-minute wait short; it is deliberately
// NOT passed to a stop drain, where it is already cancelled and would
// make the drain a no-op.
func Finish(ctx context.Context, cfg FinishConfig) (reexec bool) {
	deadline, what, parent := cfg.StopDeadline, "shutdown", context.Background()
	if cfg.Reloading {
		deadline, what, parent = cfg.ReloadDeadline, "reload", ctx
	}
	// A drain that ends because the deadline expired leaves half-written
	// messages behind; the next start annotates them (MarkInterrupted).
	warnForced := func(kind string, d time.Duration, err error) {
		cfg.Logf("zulip-acp: WARN %s drain hit its %s deadline with turns still running (%v); their messages will be marked interrupted", kind, d, err)
	}

	ok, err := Drain(parent, cfg.Idle, deadline)
	// A stop landing mid-reload pre-empts the re-exec — and it does so
	// whether or not the drain itself came out clean. Checking `ok`
	// here instead would mean a SIGTERM that arrives while nothing is
	// in flight re-execs anyway, so `systemctl stop` would silently
	// restart the relay in place and then be SIGKILLed at
	// TimeoutStopSec. The operator asked to stop; that beats a pending
	// upgrade.
	preempted := cfg.Reloading && ctx.Err() != nil

	if ok && err != nil {
		cfg.Logf("zulip-acp: %s drain finished with %v", what, err)
	} else if !ok && !preempted {
		warnForced(what, deadline, err)
	}
	if !preempted {
		return cfg.Reloading
	}

	cfg.Logf("zulip-acp: stop requested during the reload drain — abandoning the re-exec")
	// Only re-drain if turns are actually still running: the reload
	// drain above was almost certainly cut by the cancelled context
	// rather than by its own (30m) deadline, so they have had no stop
	// budget yet. In the vanishing case where both landed together this
	// grants one extra StopDeadline — cheaper than distinguishing them.
	if !ok {
		if ok, err := Drain(context.Background(), cfg.Idle, cfg.StopDeadline); !ok {
			warnForced("shutdown", cfg.StopDeadline, err)
		}
	}
	// Hand the queue back rather than leave it unpolled for the server
	// to collect: no successor image is coming for it.
	if cfg.DiscardQueue != nil {
		cfg.DiscardQueue()
	}
	return false
}
