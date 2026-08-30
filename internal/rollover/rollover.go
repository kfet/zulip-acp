// Package rollover splits an append-only transcript into a chain of
// chat messages, none of which exceeds a budget measured in Unicode
// code points, and drives a dumb poster to publish them.
//
// It exists because Zulip's MAX_MESSAGE_LENGTH is 10000 *code points*
// and Zulip truncates silently: a 10001-character POST returns
// {"result":"success"} and stores 10000 characters with
// "\n[message truncated]" appended. No error is returned at any layer,
// so the relay must count for itself.
//
// # Invariant
//
// At all times:
//
//	concat(raw slices of the sealed messages) + raw slice of the tail
//	    == the full transcript appended so far
//
// and no message payload — decorations included — exceeds Budget code
// points. The only exception is Close on an empty transcript, which
// substitutes EmptyBody so the surface never sees a blank message.
//
// # Two layers
//
// Layer 1 is raw: the transcript, the byte offsets where messages are
// cut, and the fenced-code-block state, all computed over raw text.
// Layer 2 is decoration: the continuation prefix, the reopened fence,
// the closing fence and the seal marker, frozen per sealed message.
// Fence state for split decisions is tracked over the *raw* transcript
// only — synthetic close/reopen fences never feed back into it, which
// is what stops the parity from corrupting at the third message.
//
// # Deliberate non-goals
//
// Inline (single-backtick) code spans are not tracked. A mid-line split
// — only ever reached when one line is longer than the whole budget —
// can break such a span. Cosmetic, rare, and documented rather than
// fixed.
//
// The package imports nothing surface-specific and is a promotion
// candidate for acp-kit (see BACKLOG.md). Keep it that way: HTTP code
// must never make a split decision.
package rollover

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"unicode/utf8"
)

// Defaults. Budget is deliberately below Zulip's 10000 so that a
// server-side change in how the limit is counted cannot cost output.
const (
	DefaultBudget     = 9500
	DefaultSealMarker = "\n\n*(continued below)*"
	DefaultContMarker = "*(continued from above)*\n\n"
	DefaultEmptyBody  = "_(no output)_"
)

// fenceReserve is the exact cost of closing an open fenced block at a
// seal: "\n```". Reserved unconditionally when sealing, before the cut
// point is chosen — reserving it afterwards is the classic off-by-one,
// where a slice fits pre-marker but not post-marker.
//
// The newline is added unconditionally, even when the slice already
// ends in one. The cost is a blank line inside the sealed code block,
// which renders as nothing; the benefit is that the decoration is a
// fixed, exactly-reversible suffix rather than a context-dependent one.
const fenceReserve = 4

// maxLangRunes caps the language tag carried across a seal. An
// unbounded tag would make the continuation prefix unbounded too, and
// with it the arithmetic that guarantees forward progress.
const maxLangRunes = 32

// minSlice is the smallest slice a sealed message may carry. New
// refuses a Budget that cannot guarantee it, so the seal loop always
// makes progress and can never spin.
const minSlice = 64

// Poster is the dumb, surface-specific sink the splitter drives. It
// makes no decisions: it posts the exact content it is handed and
// edits the exact content it is handed.
type Poster interface {
	// Post publishes content as a new message and returns its id.
	Post(ctx context.Context, content string) (int64, error)
	// Edit replaces the content of an existing message.
	Edit(ctx context.Context, id int64, content string) error
}

// Config configures a Splitter.
type Config struct {
	// Poster publishes messages. Required.
	Poster Poster
	// Budget is the maximum code points per message, decorations
	// included. 0 uses DefaultBudget.
	Budget int
	// SealMarker is appended to a message when it is sealed — it can
	// never be edited again, so the marker also doubles as the
	// crash-recovery signal "this message is finished".
	SealMarker string
	// ContMarker prefixes every message after the first.
	ContMarker string
	// EmptyBody is posted by Close when the transcript is empty.
	EmptyBody string
}

// message is one chat message in the chain.
type message struct {
	id      int64
	raw     string // the exact transcript slice this message carries
	body    string // desired payload: raw plus decoration
	written string // payload last handed to the Poster
	sealed  bool
}

// Splitter maps an append-only transcript onto a chain of messages.
// Safe for concurrent use.
type Splitter struct {
	cfg Config

	// ioMu serialises every method that talks to the Poster. mu alone
	// is not enough: Flush deliberately releases mu around each
	// network call, so two concurrent Flushes (the coalescing
	// watchdog and Close, say) could both observe the same unposted
	// message and both Post it — a duplicate message on the surface.
	// ioMu is always taken BEFORE mu, never the other way round.
	ioMu sync.Mutex

	mu  sync.Mutex
	raw string
	// sealedEnd is the byte offset in raw up to which text has been
	// frozen into sealed messages.
	sealedEnd int
	// sealedFence is the fenced-block state of the raw transcript at
	// sealedEnd — i.e. the state the current tail starts in.
	sealedFence fence
	msgs        []*message
}

// fence is the fenced-code-block state of a point in the raw
// transcript.
type fence struct {
	open bool
	lang string
}

// New constructs a Splitter, applying defaults and validating that the
// budget leaves room to make progress.
func New(cfg Config) (*Splitter, error) {
	if cfg.Poster == nil {
		return nil, fmt.Errorf("rollover: nil Poster")
	}
	if cfg.Budget == 0 {
		cfg.Budget = DefaultBudget
	}
	if cfg.SealMarker == "" {
		cfg.SealMarker = DefaultSealMarker
	}
	if cfg.ContMarker == "" {
		cfg.ContMarker = DefaultContMarker
	}
	if cfg.EmptyBody == "" {
		cfg.EmptyBody = DefaultEmptyBody
	}
	// Worst-case overhead on a single message: it is a continuation
	// (ContMarker + reopened fence with a max-length tag) *and* it gets
	// sealed (closing fence + SealMarker). Everything below that is
	// available for transcript text, and we insist on minSlice of it.
	overhead := runes(cfg.ContMarker) + 3 + maxLangRunes + 1 + fenceReserve + runes(cfg.SealMarker)
	if cfg.Budget < overhead+minSlice {
		return nil, fmt.Errorf("rollover: budget %d too small; needs at least %d code points for markers plus %d of text",
			cfg.Budget, overhead, minSlice)
	}
	if runes(cfg.EmptyBody) > cfg.Budget {
		return nil, fmt.Errorf("rollover: empty body (%d code points) exceeds budget %d", runes(cfg.EmptyBody), cfg.Budget)
	}
	return &Splitter{cfg: cfg, msgs: []*message{{}}}, nil
}

// Start posts an eager placeholder as the first message, so the user
// sees the relay acknowledge them before the agent produces anything.
// The first Flush carrying real text replaces it. Calling Start after
// any text has been appended, or more than once, is a programming
// error and returns an error rather than orphaning a message.
func (s *Splitter) Start(ctx context.Context, placeholder string) error {
	s.ioMu.Lock()
	defer s.ioMu.Unlock()
	s.mu.Lock()
	if s.raw != "" || len(s.msgs) != 1 || s.msgs[0].id != 0 {
		s.mu.Unlock()
		return fmt.Errorf("rollover: Start called after the chain was opened")
	}
	if runes(placeholder) > s.cfg.Budget {
		s.mu.Unlock()
		return fmt.Errorf("rollover: placeholder exceeds budget")
	}
	s.mu.Unlock()

	id, err := s.cfg.Poster.Post(ctx, placeholder)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.msgs[0].id = id
	s.msgs[0].written = placeholder
	return nil
}

// Append adds delta to the transcript and re-plans the message chain.
// Pure: it performs no I/O. Sealing is decided here, so the split
// points are a function of the transcript prefix alone and are
// identical however the text was chunked.
func (s *Splitter) Append(delta string) {
	if delta == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.raw += delta
	s.replan()
}

// Pending reports whether any message's desired payload differs from
// what the Poster has been told. Lets a caller skip a no-op Flush.
func (s *Splitter) Pending() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.msgs {
		if m.dirty() {
			return true
		}
	}
	return false
}

// Flush publishes the current plan: it posts any new messages and
// edits the tail if its content changed. A sealed message is written
// exactly once — the edit that seals it — and never touched again.
//
// One Flush may post several messages: a single large chunk can seal
// more than one.
func (s *Splitter) Flush(ctx context.Context) error {
	s.ioMu.Lock()
	defer s.ioMu.Unlock()
	return s.flushLocked(ctx)
}

// flushLocked is Flush's body. Caller holds ioMu.
func (s *Splitter) flushLocked(ctx context.Context) error {
	s.mu.Lock()
	pending := make([]*message, 0, len(s.msgs))
	for _, m := range s.msgs {
		if m.dirty() {
			pending = append(pending, m)
		}
	}
	s.mu.Unlock()

	for _, m := range pending {
		s.mu.Lock()
		body, id := m.body, m.id
		s.mu.Unlock()
		if id == 0 {
			newID, err := s.cfg.Poster.Post(ctx, body)
			if err != nil {
				return err
			}
			s.mu.Lock()
			m.id, m.written = newID, body
			s.mu.Unlock()
			continue
		}
		if err := s.cfg.Poster.Edit(ctx, id, body); err != nil {
			return err
		}
		s.mu.Lock()
		m.written = body
		s.mu.Unlock()
	}
	return nil
}

// Close appends suffix and flushes. When nothing was ever produced it
// substitutes EmptyBody, so a surface that rejects blank messages
// never sees one and a placeholder posted by Start is always resolved.
func (s *Splitter) Close(ctx context.Context, suffix string) error {
	s.Append(suffix)
	s.ioMu.Lock()
	defer s.ioMu.Unlock()
	s.mu.Lock()
	if strings.TrimSpace(s.raw) == "" {
		s.raw = s.cfg.EmptyBody
		s.replan()
	}
	s.mu.Unlock()
	return s.flushLocked(ctx)
}

// UpdatePlaceholder rewrites the eager placeholder posted by Start
// with a new frame, for an animated "thinking" indicator.
//
// It reports alive=false — and writes nothing — once any real text has
// been appended, so the animation self-disarms the moment the agent
// produces output and can never race a real edit back over it.
func (s *Splitter) UpdatePlaceholder(ctx context.Context, frame string) (alive bool, err error) {
	s.ioMu.Lock()
	defer s.ioMu.Unlock()
	s.mu.Lock()
	id := s.msgs[0].id
	dead := s.raw != "" || id == 0 || len(s.msgs) > 1
	s.mu.Unlock()
	if dead {
		return false, nil
	}
	if runes(frame) > s.cfg.Budget {
		return true, fmt.Errorf("rollover: placeholder frame exceeds budget")
	}
	if err := s.cfg.Poster.Edit(ctx, id, frame); err != nil {
		return true, err
	}
	s.mu.Lock()
	s.msgs[0].written = frame
	s.mu.Unlock()
	return true, nil
}

// IDs returns the ids of every message posted so far, oldest first.
// Unposted planned messages are omitted.
func (s *Splitter) IDs() []int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]int64, 0, len(s.msgs))
	for _, m := range s.msgs {
		if m.id != 0 {
			out = append(out, m.id)
		}
	}
	return out
}

// TailID returns the id of the live (unsealed) message, or 0 if none
// has been posted yet. This is the only message the splitter will ever
// edit again.
func (s *Splitter) TailID() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.msgs[len(s.msgs)-1].id
}

// Transcript returns the raw text appended so far. Tests use it to
// assert the prefix invariant.
func (s *Splitter) Transcript() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.raw
}

// RawSlices returns, per planned message, the exact slice of the
// transcript that message carries, with every decoration removed.
//
//	strings.Join(s.RawSlices(), "") == s.Transcript()
//
// is the splitter's core invariant. It is exported so that callers —
// and tests — can assert it directly rather than reverse-engineering
// it from the decorated payloads, where a synthetic closing fence is
// genuinely indistinguishable from one the agent wrote.
func (s *Splitter) RawSlices() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.msgs))
	for _, m := range s.msgs {
		out = append(out, m.raw)
	}
	return out
}

func (m *message) dirty() bool {
	return m.body != m.written && strings.TrimSpace(m.body) != ""
}

func (s *Splitter) tail() *message { return s.msgs[len(s.msgs)-1] }

// replan seals as many messages as the transcript now requires and
// recomputes the tail. Called with s.mu held.
//
// Sealing is greedy and monotone: once a message is sealed its raw
// slice and its decorated payload are frozen, and only the remainder
// after sealedEnd is ever reconsidered. Because the transcript is
// append-only, the first Budget code points of that remainder never
// change, so the cut point does not depend on how the text arrived.
func (s *Splitter) replan() {
	for {
		rem := s.raw[s.sealedEnd:]
		pre := s.prefix()
		preN := runes(pre)
		if preN+runes(rem) <= s.cfg.Budget {
			break
		}
		// Must seal. Reserve the seal marker and the worst-case
		// closing fence *before* choosing the cut.
		maxSlice := s.cfg.Budget - preN - runes(s.cfg.SealMarker) - fenceReserve
		cut := cutPoint(rem, maxSlice)
		slice := rem[:cut]
		end := scanFence(s.sealedFence, slice)

		body := pre + slice
		if end.open {
			body += "\n```"
		}
		body += s.cfg.SealMarker

		t := s.tail()
		t.raw = slice
		t.body = body
		t.sealed = true
		s.msgs = append(s.msgs, &message{})
		s.sealedEnd += cut
		s.sealedFence = end
	}
	t := s.tail()
	t.raw = s.raw[s.sealedEnd:]
	t.body = s.prefix() + t.raw
}

// prefix is the decoration that opens the current tail message: the
// continuation marker (every message but the first) and, if the raw
// transcript was inside a fenced block at the cut, that fence reopened
// with its language tag. Called with s.mu held.
func (s *Splitter) prefix() string {
	if s.sealedEnd == 0 {
		return ""
	}
	p := s.cfg.ContMarker
	if s.sealedFence.open {
		p += "```" + capRunes(s.sealedFence.lang, maxLangRunes) + "\n"
	}
	return p
}

// cutPoint returns the byte offset at which to cut s so the slice is
// at most maxRunes code points. It prefers the last line boundary in
// the window (the newline goes with the sealed message), and falls
// back to the exact rune boundary only when the window holds no
// newline — i.e. when a single line is longer than the budget.
func cutPoint(s string, maxRunes int) int {
	r := []rune(s)
	if len(r) <= maxRunes {
		return len(s)
	}
	window := string(r[:maxRunes])
	if i := strings.LastIndexByte(window, '\n'); i >= 0 {
		return i + 1
	}
	return len(window)
}

// scanFence advances fenced-code-block state across s. An opening
// fence is any line whose trimmed form starts with three backticks;
// the rest of that line is the language tag. Only a line that is
// exactly three backticks closes it, matching CommonMark closely
// enough for the decision this drives.
func scanFence(st fence, s string) fence {
	for _, line := range strings.Split(s, "\n") {
		t := strings.TrimSpace(line)
		if !st.open {
			if strings.HasPrefix(t, "```") {
				st.open = true
				st.lang = strings.TrimSpace(t[3:])
			}
			continue
		}
		if t == "```" {
			st.open = false
			st.lang = ""
		}
	}
	return st
}

func runes(s string) int { return utf8.RuneCountInString(s) }

// capRunes truncates s to at most n code points, never splitting one.
func capRunes(s string, n int) string {
	if runes(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}
