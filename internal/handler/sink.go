package handler

import (
	"context"
	"strings"
	"sync"
	"unicode/utf8"

	acp "github.com/coder/acp-go-sdk"

	"github.com/kfet/acp-kit/client"
	"github.com/kfet/zulip-acp/internal/rollover"
	"github.com/kfet/zulip-acp/internal/statusline"
)

// streamingSink converts ACP session updates into Zulip message text
// and feeds them to the rollover splitter.
//
// Surface choices, kept deliberately narrow and aligned with poe-acp
// and slack-acp so one fir agent reads the same everywhere:
//
//   - AgentMessageChunk → appended verbatim (the answer body).
//   - AgentThoughtChunk → italicised one-liner PER LOGICAL THOUGHT,
//     not per delta: the deltas are coalesced (see appendThought), so
//     a model that streams one word at a time does not produce one
//     italic line per word. Suppressed when hideThinking is set.
//   - Plan and ToolCall updates → suppressed. fir emits them
//     constantly on multi-step work and they read as noise on a phone.
//   - dev.acp-kit.status-line/v1 _meta → mood/plan captured and
//     appended once, at the very end of the answer, as an italic
//     footer (see maybeAppendFooter).
//
// The sink performs NO I/O: it only appends to the splitter, which is
// pure. Publishing happens on the coalescing tick. That separation is
// what keeps a slow Zulip edit from back-pressuring the ACP stream.
type streamingSink struct {
	split *rollover.Splitter

	// statusMu guards the status-line state. _meta can arrive
	// concurrently with the chunk path.
	statusMu      sync.Mutex
	status        statusline.Status
	footerEmitted bool
	// notice is the latest out-of-band operational message from the
	// agent, shown on the live placeholder. Latest-wins: a retry
	// notice is only interesting while it is current.
	notice string

	// thoughtMu guards the thought coalescing buffer.
	thoughtMu sync.Mutex
	thought   strings.Builder

	// hideThinking suppresses thought chunks. Read-only after
	// construction.
	hideThinking bool
}

// maxThoughtRunes is the size cap for one coalesced thought line. A
// model that reasons for a page without a newline still gets cut into
// readable lines instead of one wall of italics.
const maxThoughtRunes = 200

func newStreamingSink(split *rollover.Splitter, hideThinking bool) *streamingSink {
	return &streamingSink{split: split, hideThinking: hideThinking}
}

// SetModelInfo records the relay-resolved identity of the model
// servicing this turn: the provider emoji and the short model name,
// which the renderer joins into one segment ("🏛️ opus-4.5"). Either
// half may be empty — an unknown provider or an unnamed model degrades
// to the other half, and both empty drops the segment.
func (s *streamingSink) SetModelInfo(emoji, model string) {
	s.statusMu.Lock()
	s.status.ProviderEmoji = emoji
	s.status.Model = model
	s.statusMu.Unlock()
}

// Status snapshots the current mood/plan, read by the spinner each
// frame so agent-emitted state shows up while the answer is still
// pending.
func (s *streamingSink) Status() statusline.Status {
	s.statusMu.Lock()
	defer s.statusMu.Unlock()
	return s.status
}

// OnNotice implements client.NoticeSink: it receives the agent's
// out-of-band operational messages (provider rate-limit retries,
// failed compaction).
//
// These deliberately do NOT reach the splitter. A notice is not part
// of the answer — the extension exists precisely because delivering
// them as agent message text put them on the same ordered stream as
// the model's tokens, where a background goroutine's notice landed
// mid-sentence inside the user's reply. Here it goes to the live
// placeholder instead, which the spinner redraws on its own tick.
//
// Latest-wins, and best-effort: once the first real chunk lands the
// placeholder is gone and later notices are simply not shown. That is
// the right trade — a status message must never delay, split, or
// mutate an answer already being written.
func (s *streamingSink) OnNotice(_ context.Context, n client.Notice) error {
	if strings.TrimSpace(n.Text) == "" {
		return nil
	}
	s.statusMu.Lock()
	s.notice = n.Text
	s.statusMu.Unlock()
	return nil
}

// Notice snapshots the latest agent notice, read by the spinner each
// frame. Empty when the agent has sent none this turn.
func (s *streamingSink) Notice() string {
	s.statusMu.Lock()
	defer s.statusMu.Unlock()
	return s.notice
}

// OnUpdate implements client.SessionUpdateSink.
//
// Thought chunks do NOT go straight to the splitter. A model that
// streams word-sized reasoning deltas (GLM-style) would otherwise get
// one italic line per token — a hundred lines of `*The*` `* question*`
// above the answer. Thought text is accumulated instead and emitted as
// at most one italic line per logical thought; see appendThought.
//
// Everything else flushes that buffer first, so the reasoning always
// stays above the part of the answer it preceded, and message chunks
// keep going in verbatim with no forced newline per delta.
func (s *streamingSink) OnUpdate(_ context.Context, n acp.SessionNotification) error {
	s.cacheMeta(n)
	if u := n.Update; u.AgentThoughtChunk != nil {
		if s.hideThinking {
			return nil
		}
		s.appendThought(contentBlockText(u.AgentThoughtChunk.Content))
		return nil
	}
	if n.Update == (acp.SessionUpdate{}) {
		// A _meta-only notification (a status-line tick) is not a
		// transition: it carries no content, and cutting the thought
		// in progress on it would split one thought over two lines.
		return nil
	}
	s.flushThought()
	if chunk := renderChunk(n); chunk != "" {
		s.split.Append(chunk)
	}
	return nil
}

// appendThought accumulates a thought delta and emits the complete
// thoughts it now holds. A thought ends on a BLANK line — a paragraph
// break, which is where models separate one reasoning step from the
// next — and, for a model that never emits one, at the size cap, cut
// at the last sentence boundary so the line breaks where the
// reasoning does. Single newlines inside one thought are collapsed by
// oneLine, so a thought stays one italic line on a phone.
func (s *streamingSink) appendThought(delta string) {
	if delta == "" {
		return
	}
	s.thoughtMu.Lock()
	s.thought.WriteString(delta)
	buf := s.thought.String()
	var out strings.Builder
	for {
		line, rest, ok := cutThought(buf)
		if !ok {
			break
		}
		buf = rest
		writeThoughtLine(&out, line)
	}
	s.thought.Reset()
	s.thought.WriteString(buf)
	// The append stays under thoughtMu so a flush from another
	// goroutine can never overtake the thought it follows. The
	// splitter has its own lock and takes no lock of ours, so there
	// is no cycle.
	if out.Len() > 0 {
		s.split.Append(out.String())
	}
	s.thoughtMu.Unlock()
}

// cutThought takes the next complete thought off buf, reporting false
// when buf holds no complete thought yet.
func cutThought(buf string) (line, rest string, ok bool) {
	if i := strings.Index(buf, "\n\n"); i >= 0 {
		return buf[:i], buf[i+2:], true
	}
	r := []rune(buf)
	if len(r) < maxThoughtRunes {
		return "", buf, false
	}
	head := string(r[:maxThoughtRunes])
	if i := lastSentenceEnd(head); i > 0 {
		return head[:i], string(r[utf8.RuneCountInString(head[:i]):]), true
	}
	return head, string(r[maxThoughtRunes:]), true
}

// lastSentenceEnd reports the index just past the last sentence
// terminator in s, or 0 when it holds none.
func lastSentenceEnd(s string) int {
	if i := strings.LastIndexAny(s, ".!?"); i >= 0 {
		return i + 1
	}
	return 0
}

// flushThought emits whatever partial thought is still buffered. It is
// called on any non-thought update and at the end of the turn, so a
// reasoning run that never ended in a newline is not lost.
func (s *streamingSink) flushThought() {
	s.thoughtMu.Lock()
	defer s.thoughtMu.Unlock()
	line := s.thought.String()
	s.thought.Reset()
	var out strings.Builder
	writeThoughtLine(&out, line)
	if out.Len() > 0 {
		s.split.Append(out.String())
	}
}

// writeThoughtLine renders one italic thought line, dropping a line
// that holds only space.
func writeThoughtLine(out *strings.Builder, line string) {
	if t := strings.TrimSpace(line); t != "" {
		out.WriteString("*" + oneLine(t) + "*\n")
	}
}

// cacheMeta keeps the latest mood/plan warm. The status line is
// rendered from this snapshot twice: live, by every spinner frame, and
// finally by maybeAppendFooter at the end of the turn.
func (s *streamingSink) cacheMeta(n acp.SessionNotification) {
	if mood, plan, ok := statusline.ParseMeta(n.Meta); ok {
		s.statusMu.Lock()
		s.status.Mood = mood
		s.status.Plan = plan
		s.statusMu.Unlock()
	}
}

// maybeAppendFooter appends the status line to the transcript as the
// LAST thing in the answer — a blank line, then the line in italics:
//
//	\n\n*🏛️ opus-4.5 • steady • 2/5*
//
// Called once, by handler.run, after the outbox links and any
// "(stopped: …)" note have been appended and BEFORE split.Close
// flushes. It is a footer rather than a header because mood and plan
// are agent-supplied and normally arrive mid-turn: a line rendered on
// the first chunk showed a status the agent had not published yet.
// Here the snapshot is final, which is the whole point of the move.
//
// It is suppressed when:
//
//   - the turn produced no user-visible content. The check is on the
//     TRANSCRIPT, not on what has been posted: the sink performs no
//     I/O, so on a fast turn the entire answer can still be sitting
//     unflushed in the splitter when the turn ends — an answer that is
//     about to be written is an answer. Conversely, signing an empty
//     turn would defeat Close's EmptyBody substitution, which only
//     triggers on a blank transcript;
//   - the rendered line is empty (unknown provider, no model, no agent
//     _meta) — nothing is appended, not even the blank line.
//
// Error turns never reach here: handler.failTurn closes the splitter
// on its own path and does not sign a failure.
//
// Zulip-specific hazards it is safe against, both pinned by tests:
//
//   - the animated placeholder. spinner/UpdatePlaceholder edits the
//     first message only while the transcript is empty and
//     self-disarms as soon as any text is appended, so no spinner
//     frame can overwrite the footer.
//   - the end-of-turn repost. RepostOnClose deletes the streamed chain
//     and re-posts it as NEW messages, copying each message's last
//     WRITTEN body. Appending the footer before Close's flush is what
//     puts it in that body: appended after, it would be lost by the
//     repost; appended by the repost, it would be doubled.
func (s *streamingSink) maybeAppendFooter() {
	s.flushThought()
	s.statusMu.Lock()
	if s.footerEmitted {
		s.statusMu.Unlock()
		return
	}
	s.footerEmitted = true
	footer := statusline.Footer(s.status)
	s.statusMu.Unlock()
	if footer == "" || strings.TrimSpace(s.split.Transcript()) == "" {
		return
	}
	s.split.Append(footer)
}

// --- sentinel watch ------------------------------------------------------

// sentinelWatch sits ABOVE acp-kit's ValidatingSink on the ambient
// path and answers one question early: is a reply coming at all?
//
// The abstain verdict is normally only known at the end of the turn,
// because PromptAbstainable compares the COMPLETE message against the
// sentinel. But the negative verdict is knowable far sooner: once the
// accumulated text is non-empty and is no longer a prefix of the
// sentinel, no continuation of the stream can ever equal it, so a
// reply is certain. That is the moment onCommit fires — exactly once
// per turn — and the relay can put its "Thinking…" placeholder up
// instead of leaving the topic silent for minutes.
//
// It only observes. Every update is delegated to next unchanged, and
// the buffered answer still lands via the normal end-of-turn commit:
// calling Commit here would reset the ValidatingSink's text and make
// PromptAbstainable declare a false abstain.
type sentinelWatch struct {
	next     client.SessionUpdateSink
	sentinel string
	// onCommit runs on the sink goroutine, at most once per turn.
	onCommit func()

	mu    sync.Mutex
	acc   strings.Builder
	fired bool
}

// OnUpdate implements client.SessionUpdateSink.
func (w *sentinelWatch) OnUpdate(ctx context.Context, n acp.SessionNotification) error {
	if c := n.Update.AgentMessageChunk; c != nil && c.Content.Text != nil {
		w.observe(c.Content.Text.Text)
	}
	return w.next.OnUpdate(ctx, n)
}

// observe accumulates message text and reports divergence from the
// sentinel. The comparison is on TRIMMED text so the leading newlines
// some agents emit before the sentinel do not read as a reply.
func (w *sentinelWatch) observe(delta string) {
	w.mu.Lock()
	if w.fired {
		w.mu.Unlock()
		return
	}
	w.acc.WriteString(delta)
	t := strings.TrimSpace(w.acc.String())
	diverged := t != "" && !strings.HasPrefix(strings.TrimSpace(w.sentinel), t)
	w.fired = diverged
	w.mu.Unlock()
	if diverged && w.onCommit != nil {
		w.onCommit()
	}
}

// --- notice routing ------------------------------------------------------

// noticeRouter is the OUTERMOST sink wrapper. It forwards session
// updates untouched and short-circuits notices straight to the
// streaming sink.
//
// It exists because a notice must not traverse the sink chain. The
// chain — liveness, sentinel watch, the abstain ValidatingSink — is
// built to reason about the ANSWER: it buffers it, times it, and
// compares it against the silence sentinel. A notice is none of those
// things, and the intermediate wrappers do not implement NoticeSink,
// so a notice sent down the chain would simply be dropped. Routing it
// around the chain keeps both jobs honest: liveness never counts a
// notice as progress, and the sentinel never mistakes one for a reply.
type noticeRouter struct {
	next client.SessionUpdateSink
	sink *streamingSink
}

// OnUpdate implements client.SessionUpdateSink.
func (r *noticeRouter) OnUpdate(ctx context.Context, n acp.SessionNotification) error {
	return r.next.OnUpdate(ctx, n)
}

// OnNotice implements client.NoticeSink.
func (r *noticeRouter) OnNotice(ctx context.Context, n client.Notice) error {
	return r.sink.OnNotice(ctx, n)
}

// --- rendering -----------------------------------------------------------

// renderChunk converts a non-thought session update into Zulip-bound
// text, or "" when the update produces nothing user-visible. Thought
// chunks never come here: they are coalesced in the sink, which holds
// the state one delta cannot see.
func renderChunk(n acp.SessionNotification) string {
	if c := n.Update.AgentMessageChunk; c != nil {
		return contentBlockText(c.Content)
	}
	return ""
}

func contentBlockText(c acp.ContentBlock) string {
	if c.Text != nil {
		return c.Text.Text
	}
	return ""
}

// oneLine collapses a thought into a single line capped at 200 runes,
// never splitting a code point.
func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	const maxRunes = 200
	r := []rune(s)
	if len(r) > maxRunes {
		s = string(r[:maxRunes]) + "…"
	}
	return s
}
