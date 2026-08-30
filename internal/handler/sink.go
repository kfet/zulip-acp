package handler

import (
	"context"
	"strings"
	"sync"

	acp "github.com/coder/acp-go-sdk"

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
//     prepended once, on the first user-visible chunk, as a header.
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
	headerEmitted bool

	// hideThinking suppresses thought chunks. Read-only after
	// construction.
	hideThinking bool
}

func newStreamingSink(split *rollover.Splitter, hideThinking bool) *streamingSink {
	return &streamingSink{split: split, hideThinking: hideThinking}
}

// SetProviderEmoji records the relay-resolved provider emoji for this
// turn. Empty means unknown, and the renderer drops the segment.
func (s *streamingSink) SetProviderEmoji(emoji string) {
	s.statusMu.Lock()
	s.status.ProviderEmoji = emoji
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
	s.split.Append(s.maybePrependHeader(chunk))
	return nil
}

// cacheMeta keeps the latest mood/plan warm. Header rendering is lazy,
// on the first user-visible chunk.
func (s *streamingSink) cacheMeta(n acp.SessionNotification) {
	if mood, plan, ok := statusline.ParseMeta(n.Meta); ok {
		s.statusMu.Lock()
		s.status.Mood = mood
		s.status.Plan = plan
		s.statusMu.Unlock()
	}
}

// maybePrependHeader injects the status header in front of the first
// user-visible write, exactly once.
func (s *streamingSink) maybePrependHeader(t string) string {
	s.statusMu.Lock()
	defer s.statusMu.Unlock()
	if s.headerEmitted {
		return t
	}
	s.headerEmitted = true
	h := statusline.Header(s.status)
	if h == "" {
		return t
	}
	return h + "\n" + t
}

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
