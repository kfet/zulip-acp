package rollover

import (
	"context"
	"errors"
	"fmt"
	"go/build"
	"strings"
	"testing"
	"unicode/utf8"
)

// fakePoster records every Post/Edit the splitter issues and models the
// surface's message store, so tests can assert on stored state rather
// than on call counts alone.
type fakePoster struct {
	next     int64
	bodies   map[int64]string
	order    []int64
	posts    int
	edits    int
	editsBy  map[int64]int
	postErr  error
	editErr  error
	onEdit   func(id int64, content string)
	maxSeen  int // largest stored message, in code points
	postHook func(content string) error
}

func newFake() *fakePoster {
	return &fakePoster{bodies: map[int64]string{}, editsBy: map[int64]int{}}
}

func (f *fakePoster) Post(_ context.Context, content string) (int64, error) {
	if f.postHook != nil {
		if err := f.postHook(content); err != nil {
			return 0, err
		}
	}
	if f.postErr != nil {
		return 0, f.postErr
	}
	f.next++
	f.bodies[f.next] = content
	f.order = append(f.order, f.next)
	f.posts++
	f.track(content)
	return f.next, nil
}

func (f *fakePoster) Edit(_ context.Context, id int64, content string) error {
	if f.editErr != nil {
		return f.editErr
	}
	if _, ok := f.bodies[id]; !ok {
		return fmt.Errorf("edit of unknown message %d", id)
	}
	f.bodies[id] = content
	f.edits++
	f.editsBy[id]++
	f.track(content)
	if f.onEdit != nil {
		f.onEdit(id, content)
	}
	return nil
}

func (f *fakePoster) track(content string) {
	if n := utf8.RuneCountInString(content); n > f.maxSeen {
		f.maxSeen = n
	}
}

// stored returns the final bodies in post order.
func (f *fakePoster) stored() []string {
	out := make([]string, 0, len(f.order))
	for _, id := range f.order {
		out = append(out, f.bodies[id])
	}
	return out
}

func mustNew(t *testing.T, cfg Config) *Splitter {
	t.Helper()
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

// checkInvariant asserts the two properties the whole package exists
// for: the raw slices reconstruct the transcript exactly, and no
// decorated payload exceeds the budget in CODE POINTS.
func checkInvariant(t *testing.T, s *Splitter, f *fakePoster) {
	t.Helper()
	if got, want := strings.Join(s.RawSlices(), ""), s.Transcript(); got != want {
		t.Fatalf("raw slices do not reconstruct transcript:\n got %d chars\nwant %d chars", len(got), len(want))
	}
	for id, body := range f.bodies {
		if n := utf8.RuneCountInString(body); n > s.cfg.Budget {
			t.Fatalf("message %d is %d code points, over budget %d", id, n, s.cfg.Budget)
		}
	}
}

func TestNewValidation(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("want error on nil Poster")
	}
	if _, err := New(Config{Poster: newFake(), Budget: 10}); err == nil {
		t.Fatal("want error on tiny budget")
	}
	if _, err := New(Config{Poster: newFake(), Budget: 200, EmptyBody: strings.Repeat("x", 300)}); err == nil {
		t.Fatal("want error on oversized empty body")
	}
	s := mustNew(t, Config{Poster: newFake()})
	if s.cfg.Budget != DefaultBudget || s.cfg.SealMarker != DefaultSealMarker ||
		s.cfg.ContMarker != DefaultContMarker || s.cfg.EmptyBody != DefaultEmptyBody {
		t.Fatalf("defaults not applied: %+v", s.cfg)
	}
}

func TestSingleMessageNoRollover(t *testing.T) {
	f := newFake()
	s := mustNew(t, Config{Poster: f, Budget: 500})
	s.Append("hello ")
	s.Append("world")
	if !s.Pending() {
		t.Fatal("want pending after Append")
	}
	if err := s.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if s.Pending() {
		t.Fatal("want not pending after Flush")
	}
	if got := f.stored(); len(got) != 1 || got[0] != "hello world" {
		t.Fatalf("stored = %q", got)
	}
	// A second Flush with no new text must issue no calls.
	posts, edits := f.posts, f.edits
	if err := s.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if f.posts != posts || f.edits != edits {
		t.Fatalf("idle Flush issued calls: posts %d→%d edits %d→%d", posts, f.posts, edits, f.edits)
	}
	if err := s.Close(context.Background(), "\n_done_"); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := f.bodies[1]; got != "hello world\n_done_" {
		t.Fatalf("after Close = %q", got)
	}
	checkInvariant(t, s, f)
}

// TestAppendEmptyIsNoop pins that a zero-length delta changes nothing.
func TestAppendEmptyIsNoop(t *testing.T) {
	f := newFake()
	s := mustNew(t, Config{Poster: f, Budget: 500})
	s.Append("")
	if s.Pending() {
		t.Fatal("empty Append must not mark the chain dirty")
	}
	if s.Transcript() != "" {
		t.Fatalf("transcript = %q", s.Transcript())
	}
}

func TestStreamingEditsSameMessage(t *testing.T) {
	f := newFake()
	s := mustNew(t, Config{Poster: f, Budget: 500})
	for i := 0; i < 5; i++ {
		s.Append(fmt.Sprintf("chunk%d ", i))
		if err := s.Flush(context.Background()); err != nil {
			t.Fatalf("Flush: %v", err)
		}
	}
	if f.posts != 1 {
		t.Fatalf("want 1 post, got %d", f.posts)
	}
	if f.edits != 4 {
		t.Fatalf("want 4 edits, got %d", f.edits)
	}
	if s.TailID() != 1 {
		t.Fatalf("TailID = %d", s.TailID())
	}
	if ids := s.IDs(); len(ids) != 1 || ids[0] != 1 {
		t.Fatalf("IDs = %v", ids)
	}
	checkInvariant(t, s, f)
}

func TestStartPlaceholderThenReplaced(t *testing.T) {
	f := newFake()
	s := mustNew(t, Config{Poster: f, Budget: 500})
	if err := s.Start(context.Background(), "_thinking…_"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if f.bodies[1] != "_thinking…_" {
		t.Fatalf("placeholder = %q", f.bodies[1])
	}
	// Flush before any text must not blank the placeholder.
	if err := s.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if f.bodies[1] != "_thinking…_" {
		t.Fatalf("placeholder clobbered: %q", f.bodies[1])
	}
	s.Append("real answer")
	if err := s.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if f.bodies[1] != "real answer" {
		t.Fatalf("body = %q", f.bodies[1])
	}
	if f.posts != 1 {
		t.Fatalf("want 1 post, got %d", f.posts)
	}
}

func TestStartErrors(t *testing.T) {
	f := newFake()
	s := mustNew(t, Config{Poster: f, Budget: 500})
	if err := s.Start(context.Background(), strings.Repeat("x", 600)); err == nil {
		t.Fatal("want error on oversized placeholder")
	}
	f.postErr = errors.New("boom")
	if err := s.Start(context.Background(), "hi"); err == nil {
		t.Fatal("want post error")
	}
	f.postErr = nil
	if err := s.Start(context.Background(), "hi"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := s.Start(context.Background(), "hi again"); err == nil {
		t.Fatal("want error on second Start")
	}
	s2 := mustNew(t, Config{Poster: f, Budget: 500})
	s2.Append("text first")
	if err := s2.Start(context.Background(), "hi"); err == nil {
		t.Fatal("want error on Start after Append")
	}
}

func TestCloseEmptyTranscriptSubstitutesEmptyBody(t *testing.T) {
	f := newFake()
	s := mustNew(t, Config{Poster: f, Budget: 500})
	if err := s.Close(context.Background(), "   \n "); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := f.stored(); len(got) != 1 || got[0] != DefaultEmptyBody {
		t.Fatalf("stored = %q", got)
	}
	checkInvariant(t, s, f)
}

func TestCloseEmptyAfterPlaceholder(t *testing.T) {
	f := newFake()
	s := mustNew(t, Config{Poster: f, Budget: 500})
	if err := s.Start(context.Background(), "_thinking…_"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := s.Close(context.Background(), ""); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if f.bodies[1] != DefaultEmptyBody {
		t.Fatalf("placeholder not resolved: %q", f.bodies[1])
	}
}

func TestFlushPostError(t *testing.T) {
	f := newFake()
	f.postErr = errors.New("post failed")
	s := mustNew(t, Config{Poster: f, Budget: 500})
	s.Append("body")
	if err := s.Flush(context.Background()); err == nil {
		t.Fatal("want post error")
	}
	// Recovery: the chain is still dirty and a later Flush succeeds.
	f.postErr = nil
	if err := s.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if f.bodies[1] != "body" {
		t.Fatalf("body = %q", f.bodies[1])
	}
}

func TestFlushEditError(t *testing.T) {
	f := newFake()
	s := mustNew(t, Config{Poster: f, Budget: 500})
	s.Append("one")
	if err := s.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	f.editErr = errors.New("edit failed")
	s.Append(" two")
	if err := s.Flush(context.Background()); err == nil {
		t.Fatal("want edit error")
	}
}

// --- rollover behaviour -------------------------------------------------

func TestRolloverTwoMessages(t *testing.T) {
	f := newFake()
	const budget = 300
	s := mustNew(t, Config{Poster: f, Budget: budget})
	// 40 lines of 20 chars = 840 chars: comfortably 4 messages.
	var sb strings.Builder
	for i := 0; i < 40; i++ {
		fmt.Fprintf(&sb, "line %014d\n", i)
	}
	s.Append(sb.String())
	if err := s.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if f.posts < 3 {
		t.Fatalf("want 3+ messages, got %d", f.posts)
	}
	checkInvariant(t, s, f)
	// Sealed messages carry the marker; the tail does not.
	bodies := f.stored()
	for i, b := range bodies[:len(bodies)-1] {
		if !strings.HasSuffix(b, DefaultSealMarker) {
			t.Fatalf("message %d not sealed: %q", i, b)
		}
	}
	if strings.HasSuffix(bodies[len(bodies)-1], DefaultSealMarker) {
		t.Fatal("tail must not be sealed")
	}
	for i, b := range bodies[1:] {
		if !strings.HasPrefix(b, DefaultContMarker) {
			t.Fatalf("message %d missing continuation marker: %q", i+1, b)
		}
	}
	// Splits landed on line boundaries.
	for _, raw := range s.RawSlices()[:len(bodies)-1] {
		if !strings.HasSuffix(raw, "\n") {
			t.Fatalf("split not on a line boundary: %q", raw[len(raw)-10:])
		}
	}
}

// TestSealedMessagesAreImmutable is the load-bearing test: once a
// message carries the seal marker it must never be edited again, no
// matter how much more text arrives.
func TestSealedMessagesAreImmutable(t *testing.T) {
	f := newFake()
	const budget = 300
	s := mustNew(t, Config{Poster: f, Budget: budget})
	sealedAt := map[int64]string{}
	f.onEdit = func(id int64, content string) {
		if want, ok := sealedAt[id]; ok {
			t.Errorf("sealed message %d edited again:\n was %q\nnow %q", id, want, content)
		}
	}
	for i := 0; i < 200; i++ {
		s.Append(fmt.Sprintf("row %04d of the transcript\n", i))
		if err := s.Flush(context.Background()); err != nil {
			t.Fatalf("Flush: %v", err)
		}
		// Everything before the tail is sealed as of now.
		ids := s.IDs()
		for _, id := range ids[:len(ids)-1] {
			if _, ok := sealedAt[id]; !ok {
				sealedAt[id] = f.bodies[id]
			}
		}
	}
	if err := s.Close(context.Background(), ""); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if f.posts < 15 {
		t.Fatalf("expected many messages, got %d", f.posts)
	}
	checkInvariant(t, s, f)
	// Every sealed body is still exactly what it was when sealed.
	for id, want := range sealedAt {
		if f.bodies[id] != want {
			t.Fatalf("sealed message %d changed", id)
		}
	}
}

// TestChunkingDoesNotChangeSplits pins that the split points are a
// function of the transcript alone, not of how it was chunked.
func TestChunkingDoesNotChangeSplits(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < 120; i++ {
		fmt.Fprintf(&sb, "line %d: %s\n", i, strings.Repeat("abc", i%17))
	}
	full := sb.String()

	plan := func(chunk int) []string {
		f := newFake()
		s := mustNew(t, Config{Poster: f, Budget: 400})
		for i := 0; i < len(full); i += chunk {
			end := i + chunk
			if end > len(full) {
				end = len(full)
			}
			s.Append(full[i:end])
		}
		return s.RawSlices()
	}
	want := plan(len(full))
	for _, chunk := range []int{1, 7, 13, 64, 501, 4096} {
		got := plan(chunk)
		if len(got) != len(want) {
			t.Fatalf("chunk=%d: %d messages, want %d", chunk, len(got), len(want))
		}
		for i := range got {
			if got[i] != want[i] {
				t.Fatalf("chunk=%d: message %d differs", chunk, i)
			}
		}
	}
}

// TestBoundaryExact walks the exact-fit boundary: a transcript of
// budget code points is one message; budget+1 rolls over. This is the
// 9999 / 10000 / 10001 case in miniature, and it is run at Zulip's
// real numbers too.
func TestBoundaryExact(t *testing.T) {
	for _, budget := range []int{300, 9500, 10000} {
		for _, delta := range []int{-1, 0, 1} {
			n := budget + delta
			f := newFake()
			s := mustNew(t, Config{Poster: f, Budget: budget})
			s.Append(strings.Repeat("x", n))
			if err := s.Flush(context.Background()); err != nil {
				t.Fatalf("Flush: %v", err)
			}
			checkInvariant(t, s, f)
			wantMsgs := 1
			if delta > 0 {
				wantMsgs = 2
			}
			if f.posts != wantMsgs {
				t.Fatalf("budget=%d n=%d: %d messages, want %d", budget, n, f.posts, wantMsgs)
			}
			if delta == 0 && utf8.RuneCountInString(f.bodies[1]) != budget {
				t.Fatalf("budget=%d: exact-fit message is %d code points", budget, utf8.RuneCountInString(f.bodies[1]))
			}
		}
	}
}

// TestSealMarkerLandsExactlyAtBudget is the off-by-one the v1 design calls
// out: content that fits before the seal marker is added but not
// after. The headroom must be reserved BEFORE the cut is chosen.
func TestSealMarkerLandsExactlyAtBudget(t *testing.T) {
	const budget = 400
	seal := "\n\n*(cont)*"
	cont := "*(from above)*\n"
	// Sweep every length in a window straddling the point where the
	// decorated first message would exactly hit the budget.
	for n := budget - 40; n <= budget+40; n++ {
		f := newFake()
		s := mustNew(t, Config{Poster: f, Budget: budget, SealMarker: seal, ContMarker: cont})
		s.Append(strings.Repeat("y", n))
		if err := s.Close(context.Background(), ""); err != nil {
			t.Fatalf("n=%d Close: %v", n, err)
		}
		checkInvariant(t, s, f)
	}
}

func TestSingleLineLongerThanBudget(t *testing.T) {
	f := newFake()
	const budget = 500
	s := mustNew(t, Config{Poster: f, Budget: budget})
	// One 12000-char line with no newline anywhere.
	s.Append(strings.Repeat("z", 12000))
	if err := s.Close(context.Background(), ""); err != nil {
		t.Fatalf("Close: %v", err)
	}
	checkInvariant(t, s, f)
	if f.posts < 25 {
		t.Fatalf("want many messages for a 12k line, got %d", f.posts)
	}
}

func TestNonASCIIAtSplitPoint(t *testing.T) {
	for _, unit := range []string{"🙂", "漢", "é", "👨‍👩‍👧"} {
		for n := 90; n <= 130; n++ {
			f := newFake()
			s := mustNew(t, Config{Poster: f, Budget: 200})
			s.Append(strings.Repeat(unit, n))
			if err := s.Close(context.Background(), ""); err != nil {
				t.Fatalf("unit=%q n=%d Close: %v", unit, n, err)
			}
			checkInvariant(t, s, f)
			for id, body := range f.bodies {
				if !utf8.ValidString(body) {
					t.Fatalf("message %d is not valid UTF-8", id)
				}
			}
		}
	}
}

// TestMidFenceSplit is the #1 subtle bug: sealing inside a fenced code
// block must close the fence in the sealed message and reopen it, with
// the same language tag, in the continuation.
func TestMidFenceSplit(t *testing.T) {
	f := newFake()
	const budget = 300
	s := mustNew(t, Config{Poster: f, Budget: budget})
	var sb strings.Builder
	sb.WriteString("Here is some code:\n\n```go\n")
	for i := 0; i < 60; i++ {
		fmt.Fprintf(&sb, "\tfmt.Println(%d)\n", i)
	}
	sb.WriteString("```\n\nand that is all.\n")
	s.Append(sb.String())
	if err := s.Close(context.Background(), ""); err != nil {
		t.Fatalf("Close: %v", err)
	}
	checkInvariant(t, s, f)

	bodies := f.stored()
	if len(bodies) < 3 {
		t.Fatalf("want 3+ messages, got %d", len(bodies))
	}
	// Every message must render with balanced fences: an even number of
	// fence lines, so nothing leaks out of (or into) a code block.
	for i, b := range bodies {
		if n := countFenceLines(b); n%2 != 0 {
			t.Fatalf("message %d has %d fence lines (unbalanced):\n%s", i, n, b)
		}
	}
	// Messages 1..k that continue inside the block must reopen ```go.
	reopened := 0
	for i, b := range bodies[1:] {
		rest := strings.TrimPrefix(b, DefaultContMarker)
		if strings.HasPrefix(rest, "```go\n") {
			reopened++
			continue
		}
		// Only the message after the block closed may skip the reopen.
		if i != len(bodies)-2 {
			t.Fatalf("message %d did not reopen the fence:\n%s", i+1, b)
		}
	}
	if reopened == 0 {
		t.Fatal("no continuation reopened the fence")
	}
}

// TestFenceStateDoesNotDriftAcrossThreeMessages guards the trap the
// two-layer design exists for: synthetic close/reopen fences must not
// feed back into the raw fence parity, or message 3+ inverts.
func TestFenceStateDoesNotDriftAcrossThreeMessages(t *testing.T) {
	f := newFake()
	s := mustNew(t, Config{Poster: f, Budget: 250})
	var sb strings.Builder
	sb.WriteString("```python\n")
	for i := 0; i < 80; i++ {
		fmt.Fprintf(&sb, "x%d = %d\n", i, i)
	}
	sb.WriteString("```\n")
	s.Append(sb.String())
	if err := s.Close(context.Background(), ""); err != nil {
		t.Fatalf("Close: %v", err)
	}
	checkInvariant(t, s, f)
	bodies := f.stored()
	if len(bodies) < 4 {
		t.Fatalf("want 4+ messages, got %d", len(bodies))
	}
	for i, b := range bodies {
		if n := countFenceLines(b); n%2 != 0 {
			t.Fatalf("message %d unbalanced (%d fence lines):\n%s", i, n, b)
		}
		if i > 0 && !strings.Contains(b, "```python") && i < len(bodies)-1 {
			t.Fatalf("message %d lost the language tag:\n%s", i, b)
		}
	}
}

// TestLongLanguageTagIsCapped keeps the continuation prefix bounded, so
// the arithmetic that guarantees forward progress holds.
func TestLongLanguageTagIsCapped(t *testing.T) {
	f := newFake()
	s := mustNew(t, Config{Poster: f, Budget: 250})
	s.Append("```" + strings.Repeat("q", 500) + "\n" + strings.Repeat("d\n", 200))
	if err := s.Close(context.Background(), ""); err != nil {
		t.Fatalf("Close: %v", err)
	}
	checkInvariant(t, s, f)
	for i, b := range f.stored()[1:] {
		rest := strings.TrimPrefix(b, DefaultContMarker)
		if !strings.HasPrefix(rest, "```") {
			continue
		}
		first := strings.SplitN(rest, "\n", 2)[0]
		if n := utf8.RuneCountInString(first); n > 3+maxLangRunes {
			t.Fatalf("continuation %d reopened with an uncapped tag (%d runes): %q", i+1, n, first)
		}
	}
}

// TestTailFenceLeftOpen pins the deliberate choice not to auto-close a
// still-streaming fence: Zulip renders an unterminated fence as
// code-to-end-of-message, which is the right look mid-stream.
func TestTailFenceLeftOpen(t *testing.T) {
	f := newFake()
	s := mustNew(t, Config{Poster: f, Budget: 500})
	s.Append("```go\nfmt.Print")
	if err := s.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if f.bodies[1] != "```go\nfmt.Print" {
		t.Fatalf("tail was decorated: %q", f.bodies[1])
	}
}

func TestScanFence(t *testing.T) {
	cases := []struct {
		in       string
		wantOpen bool
		wantLang string
	}{
		{"plain text", false, ""},
		{"```go\nx", true, "go"},
		{"```go\nx\n```", false, ""},
		{"```\nx", true, ""},
		{"  ```rust  \n", true, "rust"},
		{"```go\n```rust\n", true, "go"}, // inner line is not a close
		{"```go\nx\n```\n```sh\n", true, "sh"},
	}
	for _, c := range cases {
		got := scanFence(fence{}, c.in)
		if got.open != c.wantOpen || got.lang != c.wantLang {
			t.Fatalf("scanFence(%q) = %+v, want open=%v lang=%q", c.in, got, c.wantOpen, c.wantLang)
		}
	}
}

func TestCutPointFallbacks(t *testing.T) {
	if got := cutPoint("short", 100); got != 5 {
		t.Fatalf("cutPoint short = %d", got)
	}
	if got := cutPoint("abcdefghij", 4); got != 4 {
		t.Fatalf("cutPoint no newline = %d", got)
	}
	if got := cutPoint("ab\ncdefgh", 6); got != 3 {
		t.Fatalf("cutPoint newline = %d", got)
	}
	// A leading newline still makes progress.
	if got := cutPoint("\nabcdefgh", 4); got != 1 {
		t.Fatalf("cutPoint leading newline = %d", got)
	}
}

func TestCapRunes(t *testing.T) {
	if got := capRunes("héllo", 10); got != "héllo" {
		t.Fatalf("capRunes = %q", got)
	}
	if got := capRunes("héllo", 2); got != "hé" {
		t.Fatalf("capRunes = %q", got)
	}
}

func countFenceLines(s string) int {
	n := 0
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			n++
		}
	}
	return n
}

func TestUpdatePlaceholder(t *testing.T) {
	f := newFake()
	s := mustNew(t, Config{Poster: f, Budget: 500})
	ctx := context.Background()

	// Before Start there is no message to animate.
	if alive, err := s.UpdatePlaceholder(ctx, "frame"); alive || err != nil {
		t.Fatalf("UpdatePlaceholder before Start = %v, %v", alive, err)
	}
	if err := s.Start(ctx, "_thinking…_"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if alive, err := s.UpdatePlaceholder(ctx, "_thinking._"); !alive || err != nil {
		t.Fatalf("UpdatePlaceholder = %v, %v", alive, err)
	}
	if f.bodies[1] != "_thinking._" {
		t.Fatalf("frame not written: %q", f.bodies[1])
	}
	// An oversized frame is refused but keeps the animation alive.
	if alive, err := s.UpdatePlaceholder(ctx, strings.Repeat("x", 600)); !alive || err == nil {
		t.Fatalf("oversized frame = %v, %v", alive, err)
	}
	// A failing edit surfaces but does not disarm.
	f.editErr = errors.New("edit failed")
	if alive, err := s.UpdatePlaceholder(ctx, "_thinking.._"); !alive || err == nil {
		t.Fatalf("failing edit = %v, %v", alive, err)
	}
	f.editErr = nil
	// Once real text lands the placeholder window is closed for good.
	s.Append("answer")
	if alive, err := s.UpdatePlaceholder(ctx, "_thinking..._"); alive || err != nil {
		t.Fatalf("UpdatePlaceholder after text = %v, %v", alive, err)
	}
	if err := s.Flush(ctx); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if f.bodies[1] != "answer" {
		t.Fatalf("body = %q", f.bodies[1])
	}
}

// TestUpdatePlaceholderAfterRollover: once the chain has more than one
// message the first is sealed, so the animation must be disarmed.
func TestUpdatePlaceholderAfterRollover(t *testing.T) {
	f := newFake()
	s := mustNew(t, Config{Poster: f, Budget: 200})
	ctx := context.Background()
	if err := s.Start(ctx, "_thinking…_"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	s.Append(strings.Repeat("a\n", 300))
	if err := s.Flush(ctx); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if alive, _ := s.UpdatePlaceholder(ctx, "frame"); alive {
		t.Fatal("placeholder must be dead after rollover")
	}
}

// TestConcurrentFlushDoesNotDoublePost is the regression test for the
// race ioMu exists to close: Flush releases the plan mutex around each
// network call, so two concurrent flushes could otherwise both see the
// same unposted message and both Post it.
func TestConcurrentFlushDoesNotDoublePost(t *testing.T) {
	f := newFake()
	// Block inside Post until both flushers are known to be in flight.
	entered := make(chan struct{}, 8)
	release := make(chan struct{})
	f.postHook = func(string) error {
		entered <- struct{}{}
		<-release
		return nil
	}
	s := mustNew(t, Config{Poster: f, Budget: 500})
	s.Append("the one and only message")

	done := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() { done <- s.Flush(context.Background()) }()
	}
	// Exactly one flusher may be inside Post; the other must be
	// serialised behind ioMu.
	<-entered
	close(release)
	for i := 0; i < 2; i++ {
		if err := <-done; err != nil {
			t.Fatalf("Flush: %v", err)
		}
	}
	if f.posts != 1 {
		t.Fatalf("posted %d times, want 1", f.posts)
	}
	checkInvariant(t, s, f)
}

// TestConcurrentAppendAndFlush exercises the lock discipline under the
// race detector with a live streaming shape.
func TestConcurrentAppendAndFlush(t *testing.T) {
	f := newFake()
	s := mustNew(t, Config{Poster: f, Budget: 400})
	ctx := context.Background()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 300; i++ {
			s.Append(fmt.Sprintf("line %d\n", i))
		}
	}()
	for i := 0; i < 50; i++ {
		if err := s.Flush(ctx); err != nil {
			t.Errorf("Flush: %v", err)
		}
	}
	<-done
	if err := s.Close(ctx, ""); err != nil {
		t.Fatalf("Close: %v", err)
	}
	checkInvariant(t, s, f)
	if got := strings.Join(s.RawSlices(), ""); got != s.Transcript() {
		t.Fatal("invariant broken under concurrency")
	}
}

// TestNoZulipImports enforces the rule in the package doc: the
// splitter must never import anything Zulip-specific, so that HTTP
// code can never make a split decision and the package stays a
// promotion candidate for acp-kit/chunker. See BACKLOG.md.
func TestNoZulipImports(t *testing.T) {
	p, err := build.ImportDir(".", 0)
	if err != nil {
		t.Fatalf("ImportDir: %v", err)
	}
	for _, imp := range append(append([]string{}, p.Imports...), p.TestImports...) {
		if strings.Contains(imp, "zulip-acp/") {
			t.Fatalf("internal/rollover imports %q; it must stay Zulip-free", imp)
		}
	}
}
