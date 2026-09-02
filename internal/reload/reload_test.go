//go:build unix

package reload

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestCursorValidAndString(t *testing.T) {
	var zero Cursor
	if zero.Valid() {
		t.Fatal("zero cursor must not be valid")
	}
	if got := zero.String(); got != "no queue" {
		t.Fatalf("zero String() = %q", got)
	}
	c := Cursor{QueueID: "q1", LastEventID: 42}
	if !c.Valid() {
		t.Fatal("cursor with a queue id must be valid")
	}
	if got := c.String(); got != "queue q1 at event 42" {
		t.Fatalf("String() = %q", got)
	}
}

func TestInherited(t *testing.T) {
	tests := []struct {
		name       string
		queue      *string
		last       *string
		wantCursor Cursor
		wantErr    string
	}{
		{name: "cold start"},
		{
			name: "queue only", queue: ptr("q1"),
			wantErr: "incomplete inherited cursor",
		},
		{
			name: "last only", last: ptr("7"),
			wantErr: "incomplete inherited cursor",
		},
		{
			name: "empty queue", queue: ptr(""), last: ptr("7"),
			wantErr: "is empty",
		},
		{
			name: "unparseable last", queue: ptr("q1"), last: ptr("seven"),
			wantErr: "inherited " + EnvLastEventID,
		},
		{
			name: "resumable", queue: ptr("q1"), last: ptr("7"),
			wantCursor: Cursor{QueueID: "q1", LastEventID: 7},
		},
		{
			// -1 is what Register reports for a brand-new queue, and it
			// must survive the round trip unchanged.
			name: "fresh queue cursor", queue: ptr("q1"), last: ptr("-1"),
			wantCursor: Cursor{QueueID: "q1", LastEventID: -1},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			os.Unsetenv(EnvQueueID)
			os.Unsetenv(EnvLastEventID)
			t.Cleanup(func() {
				os.Unsetenv(EnvQueueID)
				os.Unsetenv(EnvLastEventID)
			})
			if tc.queue != nil {
				t.Setenv(EnvQueueID, *tc.queue)
			}
			if tc.last != nil {
				t.Setenv(EnvLastEventID, *tc.last)
			}
			got, err := Inherited()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Inherited() error = %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Inherited() error = %v, want containing %q", err, tc.wantErr)
			}
			if got != tc.wantCursor {
				t.Fatalf("Inherited() = %+v, want %+v", got, tc.wantCursor)
			}
		})
	}
}

func ptr(s string) *string { return &s }

// TestAgentEnvNamesCoversTheWholeContract: the scrub list and the wire
// contract must not drift — a variable added to one and forgotten in
// the other would hand the relay's event queue to the child agent.
func TestAgentEnvNamesCoversTheWholeContract(t *testing.T) {
	got := AgentEnvNames()
	want := []string{EnvQueueID, EnvLastEventID}
	if !slices.Equal(got, want) {
		t.Fatalf("AgentEnvNames() = %q, want %q", got, want)
	}
	// Environ is the other half of the contract: anything it can SET
	// must be something AgentEnvNames scrubs.
	set := Environ(nil, Cursor{QueueID: "q", LastEventID: 1})
	for _, kv := range set {
		name, _, _ := strings.Cut(kv, "=")
		if !slices.Contains(got, name) {
			t.Errorf("Environ sets %q, which AgentEnvNames does not scrub", name)
		}
	}
}

func TestEnvironStripsStaleCursorAndAppends(t *testing.T) {
	base := []string{
		"PATH=/bin",
		EnvQueueID + "=stale",
		"HOME=/home/x",
		EnvLastEventID + "=999",
	}
	got := Environ(base, Cursor{QueueID: "fresh", LastEventID: 12})
	want := []string{"PATH=/bin", "HOME=/home/x", EnvQueueID + "=fresh", EnvLastEventID + "=12"}
	if !slices.Equal(got, want) {
		t.Fatalf("Environ() = %q, want %q", got, want)
	}
	// An invalid cursor must leave the successor with NO cursor at all
	// rather than a stale one: half a cursor silently skips events.
	got = Environ(base, Cursor{})
	want = []string{"PATH=/bin", "HOME=/home/x"}
	if !slices.Equal(got, want) {
		t.Fatalf("Environ(invalid) = %q, want %q", got, want)
	}
}

// idleFunc adapts a func to IdleWaiter.
type idleFunc func(context.Context) error

func (f idleFunc) WaitIdle(ctx context.Context) error { return f(ctx) }

func TestDrain(t *testing.T) {
	t.Run("graceful", func(t *testing.T) {
		ok, err := Drain(context.Background(), idleFunc(func(context.Context) error { return nil }), time.Minute)
		if !ok || err != nil {
			t.Fatalf("Drain() = %v, %v; want true, nil", ok, err)
		}
	})
	t.Run("non-deadline error is still graceful", func(t *testing.T) {
		sentinel := errors.New("boom")
		ok, err := Drain(context.Background(), idleFunc(func(context.Context) error { return sentinel }), time.Minute)
		if !ok || !errors.Is(err, sentinel) {
			t.Fatalf("Drain() = %v, %v; want true, %v", ok, err, sentinel)
		}
	})
	t.Run("deadline expiry is forced", func(t *testing.T) {
		ok, err := Drain(context.Background(), idleFunc(func(ctx context.Context) error {
			<-ctx.Done()
			return ctx.Err()
		}), time.Millisecond)
		if ok || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Drain() = %v, %v; want false, DeadlineExceeded", ok, err)
		}
	})
	t.Run("a cancelled parent cuts the drain short", func(t *testing.T) {
		// A SIGTERM landing during a long reload drain: the operator
		// asked to stop, and that must beat the 30m reload budget
		// rather than be ignored until systemd SIGKILLs the cgroup.
		parent, cancel := context.WithCancel(context.Background())
		cancel()
		// A short deadline on purpose: the invariant under test is
		// "the parent wins", not "30m is long". With
		// DefaultReloadDrain here a broken cancel propagation would
		// HANG the suite for half an hour instead of failing.
		ok, err := Drain(parent, idleFunc(func(ctx context.Context) error {
			<-ctx.Done()
			return ctx.Err()
		}), time.Minute)
		if ok || !errors.Is(err, context.Canceled) {
			t.Fatalf("Drain() = %v, %v; want false, Canceled", ok, err)
		}
	})
	t.Run("non-positive deadline falls back to the stop default", func(t *testing.T) {
		var seen time.Duration
		ok, err := Drain(context.Background(), idleFunc(func(ctx context.Context) error {
			dl, _ := ctx.Deadline()
			seen = time.Until(dl)
			return nil
		}), 0)
		if !ok || err != nil {
			t.Fatalf("Drain() = %v, %v", ok, err)
		}
		if seen > DefaultStopDrain || seen < DefaultStopDrain-time.Second {
			t.Fatalf("deadline %s, want ~%s", seen, DefaultStopDrain)
		}
	})
}

// withArgs swaps os.Args for the duration of a test.
func withArgs(t *testing.T, argv ...string) {
	t.Helper()
	old := os.Args
	os.Args = argv
	t.Cleanup(func() { os.Args = old })
}

func TestSelfPath(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "zulip-acp")
	if err := os.WriteFile(bin, []byte("#!/bin/true\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Run("os.Executable", func(t *testing.T) {
		stub(t, &osExecutable, func() (string, error) { return bin, nil })
		got, err := SelfPath()
		if err != nil || got != bin {
			t.Fatalf("SelfPath() = %q, %v; want %q", got, err, bin)
		}
	})

	t.Run("strips the (deleted) marker left by an atomic update", func(t *testing.T) {
		// This is the real production case: `mv` onto the running
		// binary unlinks the inode, and /proc/self/exe then reads
		// "<path> (deleted)". Exec'ing that literal path fails.
		stub(t, &osExecutable, func() (string, error) { return bin + " (deleted)", nil })
		got, err := SelfPath()
		if err != nil || got != bin {
			t.Fatalf("SelfPath() = %q, %v; want %q", got, err, bin)
		}
	})

	t.Run("falls back to a relative argv[0]", func(t *testing.T) {
		stub(t, &osExecutable, func() (string, error) { return "", errors.New("no /proc") })
		cwd, err := os.Getwd()
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Chdir(dir); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chdir(cwd) })
		withArgs(t, "./zulip-acp")
		got, err := SelfPath()
		if err != nil {
			t.Fatalf("SelfPath() error = %v", err)
		}
		if resolved, _ := filepath.EvalSymlinks(got); resolved != mustEval(t, bin) {
			t.Fatalf("SelfPath() = %q, want %q", got, bin)
		}
	})

	t.Run("falls back to PATH lookup", func(t *testing.T) {
		stub(t, &osExecutable, func() (string, error) { return "", errors.New("no /proc") })
		stub(t, &lookPath, func(string) (string, error) { return bin, nil })
		withArgs(t, "zulip-acp")
		got, err := SelfPath()
		if err != nil || got != bin {
			t.Fatalf("SelfPath() = %q, %v; want %q", got, err, bin)
		}
	})

	t.Run("a path-shaped argv[0] that does not exist still tries PATH", func(t *testing.T) {
		stub(t, &osExecutable, func() (string, error) { return "", errors.New("no /proc") })
		stub(t, &lookPath, func(string) (string, error) { return bin, nil })
		withArgs(t, filepath.Join(dir, "gone"))
		got, err := SelfPath()
		if err != nil || got != bin {
			t.Fatalf("SelfPath() = %q, %v; want %q", got, err, bin)
		}
	})

	t.Run("unlocatable binary is an error", func(t *testing.T) {
		stub(t, &osExecutable, func() (string, error) { return "", errors.New("no /proc") })
		stub(t, &lookPath, func(string) (string, error) { return "", errors.New("not in PATH") })
		withArgs(t, "zulip-acp")
		if _, err := SelfPath(); err == nil || !strings.Contains(err.Error(), "cannot locate own binary") {
			t.Fatalf("SelfPath() error = %v", err)
		}
	})
}

func mustEval(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestExec(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "zulip-acp")
	if err := os.WriteFile(bin, []byte("#!/bin/true\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Run("passes the cursor and preserves argv", func(t *testing.T) {
		stub(t, &osExecutable, func() (string, error) { return bin, nil })
		withArgs(t, "/whatever/zulip-acp", "--config", "/etc/z.json")
		os.Unsetenv(EnvQueueID)
		os.Unsetenv(EnvLastEventID)
		var gotPath string
		var gotArgv, gotEnv []string
		stub(t, &execFn, func(p string, argv, env []string) error {
			gotPath, gotArgv, gotEnv = p, argv, env
			return nil
		})
		if err := Exec(Cursor{QueueID: "q9", LastEventID: 5}); err != nil {
			t.Fatalf("Exec() = %v", err)
		}
		if gotPath != bin {
			t.Fatalf("exec path = %q, want %q", gotPath, bin)
		}
		// argv[0] is the resolved binary, not the stale one we were
		// invoked as, so ps and /proc agree with reality.
		want := []string{bin, "--config", "/etc/z.json"}
		if !slices.Equal(gotArgv, want) {
			t.Fatalf("argv = %q, want %q", gotArgv, want)
		}
		if !slices.Contains(gotEnv, EnvQueueID+"=q9") || !slices.Contains(gotEnv, EnvLastEventID+"=5") {
			t.Fatalf("env missing the cursor: %q", gotEnv)
		}
	})

	t.Run("reports a failed exec", func(t *testing.T) {
		stub(t, &osExecutable, func() (string, error) { return bin, nil })
		withArgs(t, bin)
		stub(t, &execFn, func(string, []string, []string) error { return syscall.ENOEXEC })
		err := Exec(Cursor{QueueID: "q9"})
		if err == nil || !errors.Is(err, syscall.ENOEXEC) {
			t.Fatalf("Exec() = %v, want ENOEXEC", err)
		}
	})

	t.Run("reports an unlocatable binary without exec'ing", func(t *testing.T) {
		stub(t, &osExecutable, func() (string, error) { return "", errors.New("no /proc") })
		stub(t, &lookPath, func(string) (string, error) { return "", errors.New("not in PATH") })
		withArgs(t, "zulip-acp")
		stub(t, &execFn, func(string, []string, []string) error {
			t.Fatal("must not exec when the binary cannot be located")
			return nil
		})
		if err := Exec(Cursor{}); err == nil {
			t.Fatal("Exec() = nil, want an error")
		}
	})
}

// stub swaps a package-level seam for the duration of a test.
func stub[T any](t *testing.T, target *T, replacement T) {
	t.Helper()
	old := *target
	*target = replacement
	t.Cleanup(func() { *target = old })
}

// finishHarness records the log narration and drives Idle by hand.
type finishHarness struct {
	logs      []string
	discarded int
}

func (h *finishHarness) logf(format string, args ...any) {
	h.logs = append(h.logs, fmt.Sprintf(format, args...))
}

func (h *finishHarness) logged(sub string) bool {
	for _, l := range h.logs {
		if strings.Contains(l, sub) {
			return true
		}
	}
	return false
}

func TestFinish(t *testing.T) {
	idleNow := idleFunc(func(context.Context) error { return nil })
	blocked := idleFunc(func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() })

	t.Run("a clean shutdown does not re-exec", func(t *testing.T) {
		h := &finishHarness{}
		if reexec := Finish(context.Background(), FinishConfig{
			Idle: idleNow, StopDeadline: time.Minute, Logf: h.logf,
		}); reexec {
			t.Fatal("Finish() = true on a shutdown")
		}
		if len(h.logs) != 0 {
			t.Fatalf("a clean drain narrated %q", h.logs)
		}
	})

	t.Run("a clean reload re-execs", func(t *testing.T) {
		h := &finishHarness{}
		if reexec := Finish(context.Background(), FinishConfig{
			Reloading: true, Idle: idleNow, ReloadDeadline: time.Minute, Logf: h.logf,
		}); !reexec {
			t.Fatal("Finish() = false on a clean reload")
		}
	})

	t.Run("a non-deadline drain error is reported, not swallowed", func(t *testing.T) {
		h := &finishHarness{}
		boom := errors.New("boom")
		if reexec := Finish(context.Background(), FinishConfig{
			Reloading: true, ReloadDeadline: time.Minute, Logf: h.logf,
			Idle: idleFunc(func(context.Context) error { return boom }),
		}); !reexec {
			t.Fatal("a non-deadline error must not cancel the re-exec")
		}
		if !h.logged("reload drain finished with boom") {
			t.Fatalf("error not reported: %q", h.logs)
		}
	})

	t.Run("a forced reload drain still re-execs, loudly", func(t *testing.T) {
		h := &finishHarness{}
		if reexec := Finish(context.Background(), FinishConfig{
			Reloading: true, Idle: blocked, ReloadDeadline: time.Millisecond, Logf: h.logf,
		}); !reexec {
			t.Fatal("a forced drain must still re-exec; the successor annotates the tails")
		}
		if !h.logged("WARN reload drain hit its") {
			t.Fatalf("forced drain not warned: %q", h.logs)
		}
	})

	t.Run("a forced shutdown drain warns and does not re-exec", func(t *testing.T) {
		h := &finishHarness{}
		if reexec := Finish(context.Background(), FinishConfig{
			Idle: blocked, StopDeadline: time.Millisecond, Logf: h.logf,
		}); reexec {
			t.Fatal("Finish() = true on a shutdown")
		}
		if !h.logged("WARN shutdown drain hit its") {
			t.Fatalf("forced drain not warned: %q", h.logs)
		}
	})

	// The regression this exists for: before, a reload drain waited on a
	// Background context, so a SIGTERM during it was ignored for up to
	// 30 minutes and systemd eventually SIGKILLed the cgroup.
	t.Run("a stop landing mid-reload pre-empts the re-exec", func(t *testing.T) {
		h := &finishHarness{}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		reexec := Finish(ctx, FinishConfig{
			Reloading: true, Idle: idleNow,
			ReloadDeadline: time.Minute, StopDeadline: time.Minute,
			DiscardQueue: func() { h.discarded++ },
			Logf:         h.logf,
		})
		if reexec {
			t.Fatal("a stop must beat a pending reload")
		}
		if !h.logged("abandoning the re-exec") {
			t.Fatalf("pre-emption not narrated: %q", h.logs)
		}
		if h.discarded != 1 {
			t.Fatalf("queue discarded %d times, want 1 — an abandoned queue is left unpolled", h.discarded)
		}
		// The drain came out clean here (nothing was in flight). That
		// must NOT be read as "the reload may proceed": a SIGTERM
		// arriving on an idle relay would otherwise re-exec in place
		// and then be SIGKILLed at TimeoutStopSec.
		if h.logged("WARN") {
			t.Fatalf("a clean pre-empted drain warned: %q", h.logs)
		}
	})

	t.Run("a pre-empted reload whose turns then wedge warns, and tolerates no discard hook", func(t *testing.T) {
		h := &finishHarness{}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if reexec := Finish(ctx, FinishConfig{
			Reloading: true, Idle: blocked,
			ReloadDeadline: time.Minute, StopDeadline: time.Millisecond,
			Logf: h.logf,
		}); reexec {
			t.Fatal("a stop must beat a pending reload")
		}
		if !h.logged("WARN shutdown drain hit its") {
			t.Fatalf("wedged post-preemption drain not warned: %q", h.logs)
		}
	})
}
