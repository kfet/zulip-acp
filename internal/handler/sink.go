package handler

import (
	"context"
	"strings"
	"sync"

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
//   - AgentThoughtChunk → italicised one-liner, so reasoning surfaces
//     without crowding the answer. Suppressed when hideThinking is set.
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

	// hideThinking suppresses thought chunks. Read-only after
	// construction.
	hideThinking bool
}

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

// OnUpdate implements client.SessionUpdateSink.
func (s *streamingSink) OnUpdate(_ context.Context, n acp.SessionNotification) error {
	s.cacheMeta(n)
	chunk := renderChunk(n, s.hideThinking)
	if chunk == "" {
		return nil
	}
	s.split.Append(chunk)
	return nil
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

// --- rendering -----------------------------------------------------------

// renderChunk converts a session update into Zulip-bound text, or ""
// when the update produces nothing user-visible.
func renderChunk(n acp.SessionNotification, hideThinking bool) string {
	u := n.Update
	switch {
	case u.AgentMessageChunk != nil:
		return contentBlockText(u.AgentMessageChunk.Content)
	case u.AgentThoughtChunk != nil:
		if hideThinking {
			return ""
		}
		if t := contentBlockText(u.AgentThoughtChunk.Content); t != "" {
			return "*" + oneLine(t) + "*\n"
		}
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
