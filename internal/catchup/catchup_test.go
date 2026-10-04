package catchup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kfet/zulip-acp/internal/zulipproto"
)

type fakeSource struct {
	msgs []zulipproto.Message
	// failures makes that many reads fail before any succeeds.
	failures int
	afterErr error
	calls    []int64
	// forceMore makes every page claim found_newest=false.
	forceMore bool
}

func (f *fakeSource) MessagesAfter(_ context.Context, _ []zulipproto.NarrowTerm, after int64, limit int) ([]zulipproto.Message, bool, error) {
	f.calls = append(f.calls, after)
	if f.afterErr != nil {
		return nil, false, f.afterErr
	}
	if f.failures > 0 {
		f.failures--
		return nil, false, errors.New("flaky")
	}
	var out []zulipproto.Message
	rest := 0
	for _, m := range f.msgs {
		if m.ID <= after {
			continue
		}
		if len(out) < limit {
			out = append(out, m)
		} else {
			rest++
		}
	}
	return out, rest == 0 && !f.forceMore, nil
}

func msgs(ids ...int64) []zulipproto.Message {
	out := make([]zulipproto.Message, len(ids))
	for i, id := range ids {
		out[i] = zulipproto.Message{ID: id}
	}
	return out
}

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "sub", "catchup.mark"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestStoreRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mark")
	s, err := Open(path)
	if err != nil || s.Get() != 0 {
		t.Fatalf("open: %v, %d", err, s.Get())
	}
	if err := s.Set(10); err != nil {
		t.Fatal(err)
	}
	if err := s.Set(5); err != nil || s.Get() != 10 {
		t.Fatalf("mark moved back: %d, %v", s.Get(), err)
	}
	s2, err := Open(path)
	if err != nil || s2.Get() != 10 {
		t.Fatalf("reopen: %v, %d", err, s2.Get())
	}
}

func TestStoreErrors(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad")
	if err := os.WriteFile(bad, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(bad); err == nil {
		t.Fatal("want malformed error")
	}
	if _, err := Open(dir); err == nil {
		t.Fatal("want read error for a directory")
	}
	// mkdir fails: the parent is a file.
	s := &Store{path: filepath.Join(bad, "x", "mark")}
	if err := s.Set(1); err == nil || !strings.Contains(err.Error(), "mkdir") {
		t.Fatalf("Set = %v", err)
	}
	// write fails: the tmp path is a directory.
	wdir := t.TempDir()
	if err := os.Mkdir(filepath.Join(wdir, "mark.tmp"), 0o755); err != nil {
		t.Fatal(err)
	}
	s = &Store{path: filepath.Join(wdir, "mark")}
	if err := s.Set(1); err == nil || !strings.Contains(err.Error(), "write") {
		t.Fatalf("Set = %v", err)
	}
	// rename fails: the target is a non-empty directory.
	rdir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(rdir, "mark", "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	s = &Store{path: filepath.Join(rdir, "mark")}
	if err := s.Set(1); err == nil || !strings.Contains(err.Error(), "commit") {
		t.Fatalf("Set = %v", err)
	}
}

// TestRunFirstRun: with no mark the registration's max_message_id is
// stored and nothing is replayed.
func TestRunFirstRun(t *testing.T) {
	s := newStore(t)
	src := &fakeSource{msgs: msgs(1, 2)}
	err := Run(context.Background(), Config{Source: src, Store: s, Newest: 77, Deliver: func(context.Context, []zulipproto.Message) {
		t.Fatal("first run must replay nothing")
	}})
	if err != nil || s.Get() != 77 || len(src.calls) != 0 {
		t.Fatalf("err=%v mark=%d calls=%v", err, s.Get(), src.calls)
	}
}

// TestRunPages: everything above the mark is read across pages,
// delivered once, and the mark moves only after delivery.
func TestRunPages(t *testing.T) {
	s := newStore(t)
	_ = s.Set(3)
	src := &fakeSource{msgs: msgs(1, 2, 3, 4, 5, 6, 7, 8)}
	var got []int64
	err := Run(context.Background(), Config{Source: src, Store: s, PageSize: 2, Logf: t.Logf,
		Deliver: func(_ context.Context, ms []zulipproto.Message) {
			if s.Get() != 3 {
				t.Fatalf("mark moved before delivery: %d", s.Get())
			}
			for _, m := range ms {
				got = append(got, m.ID)
			}
		}})
	if err != nil || s.Get() != 8 {
		t.Fatalf("err=%v mark=%d", err, s.Get())
	}
	if len(got) != 5 || got[0] != 4 || got[4] != 8 {
		t.Fatalf("got %v", got)
	}
}

func TestRunNothingNew(t *testing.T) {
	s := newStore(t)
	_ = s.Set(9)
	src := &fakeSource{msgs: msgs(9)}
	err := Run(context.Background(), Config{Source: src, Store: s, Deliver: func(context.Context, []zulipproto.Message) {
		t.Fatal("nothing to deliver")
	}})
	if err != nil || s.Get() != 9 {
		t.Fatalf("err=%v mark=%d", err, s.Get())
	}
}

// TestRunLimit: a backlog over MaxMessages stops early and still
// delivers what was read.
func TestRunLimit(t *testing.T) {
	s := newStore(t)
	_ = s.Set(1)
	src := &fakeSource{msgs: msgs(2, 3, 4, 5, 6)}
	var n int
	err := Run(context.Background(), Config{Source: src, Store: s, PageSize: 2, MaxMessages: 2,
		Deliver: func(_ context.Context, ms []zulipproto.Message) { n = len(ms) }})
	if err != nil || n != 2 || s.Get() != 3 {
		t.Fatalf("err=%v n=%d mark=%d", err, n, s.Get())
	}
}

// TestRunEmptyPageStops: a page with no messages ends the read even
// if the server did not say found_newest.
func TestRunEmptyPageStops(t *testing.T) {
	s := newStore(t)
	_ = s.Set(1)
	src := &fakeSource{msgs: msgs(2), forceMore: true}
	var n int
	err := Run(context.Background(), Config{Source: src, Store: s,
		Deliver: func(_ context.Context, ms []zulipproto.Message) { n = len(ms) }})
	if err != nil || n != 1 || len(src.calls) != 2 {
		t.Fatalf("err=%v n=%d calls=%v", err, n, src.calls)
	}
}

func noSleep(context.Context, time.Duration) error { return nil }

// TestRunReadError: a read that keeps failing is retried, then given
// up on with the mark untouched.
func TestRunReadError(t *testing.T) {
	s := newStore(t)
	_ = s.Set(1)
	src := &fakeSource{afterErr: errors.New("boom")}
	var waits []time.Duration
	err := Run(context.Background(), Config{Source: src, Store: s, Attempts: 3,
		Sleep: func(_ context.Context, d time.Duration) error { waits = append(waits, d); return nil }})
	if err == nil || len(src.calls) != 3 || s.Get() != 1 {
		t.Fatalf("err=%v calls=%v mark=%d", err, src.calls, s.Get())
	}
	if len(waits) != 2 || waits[1] != 2*waits[0] {
		t.Fatalf("waits = %v, want doubling", waits)
	}
}

// TestRunRetrySucceeds: a transient failure costs one retry, not the
// gap.
func TestRunRetrySucceeds(t *testing.T) {
	s := newStore(t)
	_ = s.Set(1)
	src := &fakeSource{msgs: msgs(2), failures: 1}
	var n int
	err := Run(context.Background(), Config{Source: src, Store: s, Sleep: noSleep,
		Deliver: func(_ context.Context, ms []zulipproto.Message) { n = len(ms) }})
	if err != nil || n != 1 || s.Get() != 2 {
		t.Fatalf("err=%v n=%d mark=%d", err, n, s.Get())
	}
}

// TestRunRetryCancelled: shutdown during the wait stops the retries.
func TestRunRetryCancelled(t *testing.T) {
	s := newStore(t)
	_ = s.Set(1)
	src := &fakeSource{afterErr: errors.New("boom")}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Run(ctx, Config{Source: src, Store: s}); err == nil || len(src.calls) != 1 {
		t.Fatalf("err=%v calls=%v", err, src.calls)
	}
}

func TestSleepCtx(t *testing.T) {
	if err := sleepCtx(context.Background(), time.Nanosecond); err != nil {
		t.Fatal(err)
	}
}
