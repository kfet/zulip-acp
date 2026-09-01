package channels

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/kfet/zulip-acp/internal/zulipproto"
)

// recorder collects log lines so the join/leave announcements — the
// only way an operator learns the served set moved — can be asserted.
type recorder struct {
	mu    sync.Mutex
	lines []string
}

func (r *recorder) logf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, fmt.Sprintf(format, args...))
}

func (r *recorder) saw(sub string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, l := range r.lines {
		if strings.Contains(l, sub) {
			return true
		}
	}
	return false
}

func mustName(t *testing.T, s *Set, id int64) string {
	t.Helper()
	name, ok := s.Name(id)
	if !ok {
		t.Fatalf("stream %d not served", id)
	}
	return name
}

func mustNotServe(t *testing.T, s *Set, id int64) {
	t.Helper()
	if name, ok := s.Name(id); ok {
		t.Fatalf("stream %d unexpectedly served as %q", id, name)
	}
}

func subEvent(op string, streams ...zulipproto.Stream) zulipproto.Event {
	return zulipproto.Event{Type: zulipproto.EventSubscription, Op: op, Subscriptions: streams}
}

// --- static half ---------------------------------------------------------

func TestStaticSetIgnoresEvents(t *testing.T) {
	rec := &recorder{}
	s := New(Config{Explicit: map[int64]string{4: "fleet"}, Logf: rec.logf})
	if got := mustName(t, s, 4); got != "fleet" {
		t.Fatalf("name = %q", got)
	}
	mustNotServe(t, s, 5)

	// Neither a subscription event nor a resync may widen a set the
	// operator listed explicitly.
	s.Apply(subEvent("add", zulipproto.Stream{StreamID: 5, Name: "ops"}))
	s.Sync([]zulipproto.Stream{{StreamID: 5, Name: "ops"}})
	mustNotServe(t, s, 5)
	if len(rec.lines) != 0 {
		t.Fatalf("static set logged %v", rec.lines)
	}
	if s.Len() != 1 {
		t.Fatalf("len = %d", s.Len())
	}
}

func TestNewCopiesExplicitAndDefaultsLogf(t *testing.T) {
	explicit := map[int64]string{4: "fleet"}
	s := New(Config{Explicit: explicit, Follow: true})
	delete(explicit, 4)
	if got := mustName(t, s, 4); got != "fleet" {
		t.Fatalf("name = %q", got)
	}
	// The nil logger must be callable — this would panic if New left
	// it nil.
	s.Apply(subEvent("add", zulipproto.Stream{StreamID: 5, Name: "ops"}))
}

// --- following the bot's subscriptions -----------------------------------

func TestSyncSeedsAndDiffs(t *testing.T) {
	rec := &recorder{}
	s := New(Config{Explicit: map[int64]string{4: "fleet"}, Follow: true, Logf: rec.logf})

	s.Sync([]zulipproto.Stream{{StreamID: 4, Name: "fleet"}, {StreamID: 3, Name: "general"}})
	if got := mustName(t, s, 3); got != "general" {
		t.Fatalf("name = %q", got)
	}
	if !rec.saw("now serving #general (3)") {
		t.Fatalf("no join line: %v", rec.lines)
	}
	// An explicitly configured channel is not a runtime join.
	if rec.saw("#fleet") {
		t.Fatalf("explicit channel announced as a join: %v", rec.lines)
	}
	if s.Len() != 2 {
		t.Fatalf("len = %d", s.Len())
	}
	if got := s.Names(); len(got) != 2 || got[0] != "fleet" || got[1] != "general" {
		t.Fatalf("names = %v", got)
	}

	// A resync that drops general must stop serving it, and must not
	// touch the explicit half.
	s.Sync([]zulipproto.Stream{{StreamID: 4, Name: "fleet"}})
	mustNotServe(t, s, 3)
	if !rec.saw("no longer serving #general (3)") {
		t.Fatalf("no leave line: %v", rec.lines)
	}
	if _, ok := s.Name(4); !ok {
		t.Fatal("explicit channel lost on resync")
	}
}

func TestRuntimeAddAndRemove(t *testing.T) {
	rec := &recorder{}
	s := New(Config{Follow: true, Logf: rec.logf})
	mustNotServe(t, s, 2)

	s.Apply(subEvent("add", zulipproto.Stream{StreamID: 2, Name: "sandbox"}))
	if got := mustName(t, s, 2); got != "sandbox" {
		t.Fatalf("name = %q", got)
	}
	if !rec.saw("now serving #sandbox (2)") {
		t.Fatalf("no join line: %v", rec.lines)
	}

	// Re-delivery of the same add must not announce twice.
	before := len(rec.lines)
	s.Apply(subEvent("add", zulipproto.Stream{StreamID: 2, Name: "sandbox"}))
	if len(rec.lines) != before {
		t.Fatalf("duplicate add logged: %v", rec.lines)
	}

	s.Apply(subEvent("remove", zulipproto.Stream{StreamID: 2}))
	mustNotServe(t, s, 2)
	if !rec.saw("no longer serving #sandbox (2)") {
		t.Fatalf("no leave line (name must fall back to the known one): %v", rec.lines)
	}
	// Removing what was never served is silent.
	before = len(rec.lines)
	s.Apply(subEvent("remove", zulipproto.Stream{StreamID: 99, Name: "nope"}))
	if len(rec.lines) != before {
		t.Fatalf("spurious leave logged: %v", rec.lines)
	}
}

func TestExplicitChannelSurvivesUnsubscribe(t *testing.T) {
	rec := &recorder{}
	s := New(Config{Explicit: map[int64]string{4: "fleet"}, Follow: true, Logf: rec.logf})
	s.Apply(subEvent("add", zulipproto.Stream{StreamID: 4, Name: "fleet"}))
	s.Apply(subEvent("remove", zulipproto.Stream{StreamID: 4, Name: "fleet"}))
	// Config is authoritative: the operator asked for #fleet.
	if _, ok := s.Name(4); !ok {
		t.Fatal("explicit channel dropped by an unsubscribe event")
	}
	if len(rec.lines) != 0 {
		t.Fatalf("explicit channel announced: %v", rec.lines)
	}
}

func TestApplyIgnoresIrrelevantEvents(t *testing.T) {
	s := New(Config{Follow: true})
	s.Apply(subEvent("add", zulipproto.Stream{StreamID: 2, Name: "sandbox"}))
	for _, ev := range []zulipproto.Event{
		{Type: zulipproto.EventMessage, Message: &zulipproto.Message{StreamID: 2}},
		// peer_add is about OTHER users' subscriptions.
		subEvent("peer_add", zulipproto.Stream{StreamID: 7, Name: "other"}),
		{Type: zulipproto.EventStream, Op: "create", Streams: []zulipproto.Stream{{StreamID: 7, Name: "other"}}},
		{Type: zulipproto.EventStream, Op: "update", StreamID: 2, Property: "description", Value: json.RawMessage(`"hi"`)},
	} {
		s.Apply(ev)
	}
	if got := s.Names(); len(got) != 1 || got[0] != "sandbox" {
		t.Fatalf("names = %v", got)
	}
}

func TestStreamRenameAndArchive(t *testing.T) {
	rec := &recorder{}
	s := New(Config{Explicit: map[int64]string{4: "fleet"}, Follow: true, Logf: rec.logf})
	s.Apply(subEvent("add", zulipproto.Stream{StreamID: 2, Name: "sandbox"}))

	rename := func(id int64, to string) zulipproto.Event {
		return zulipproto.Event{
			Type: zulipproto.EventStream, Op: "update", StreamID: id,
			Property: "name", Value: json.RawMessage(`"` + to + `"`),
		}
	}
	s.Apply(rename(2, "playground"))
	s.Apply(rename(4, "fleet-ops"))
	s.Apply(rename(99, "ghost")) // unknown channel: ignored
	if got := mustName(t, s, 2); got != "playground" {
		t.Fatalf("followed rename: %q", got)
	}
	if got := mustName(t, s, 4); got != "fleet-ops" {
		t.Fatalf("explicit rename: %q", got)
	}
	mustNotServe(t, s, 99)

	// Archiving a channel takes its subscription with it.
	s.Apply(zulipproto.Event{
		Type: zulipproto.EventStream, Op: "delete",
		Streams: []zulipproto.Stream{{StreamID: 2, Name: "playground"}},
	})
	mustNotServe(t, s, 2)
	if !rec.saw("no longer serving #playground (2)") {
		t.Fatalf("no leave line: %v", rec.lines)
	}
}

// TestConcurrentReadWrite is the -race witness: the handler reads the
// set on turn goroutines while the event loop writes it.
func TestConcurrentReadWrite(t *testing.T) {
	s := New(Config{Follow: true})
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				s.Name(2)
				s.Names()
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				s.Apply(subEvent("add", zulipproto.Stream{StreamID: 2, Name: "sandbox"}))
				s.Sync([]zulipproto.Stream{{StreamID: 3, Name: "general"}})
				s.Apply(subEvent("remove", zulipproto.Stream{StreamID: 3}))
			}
		}()
	}
	wg.Wait()
}
