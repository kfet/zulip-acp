package handler

import (
	"context"
	"testing"

	acp "github.com/coder/acp-go-sdk"
	"github.com/kfet/acp-kit/client"
)

// countingSink records updates, standing in for the wrapped chain.
type countingSink struct{ updates int }

func (s *countingSink) OnUpdate(context.Context, acp.SessionNotification) error {
	s.updates++
	return nil
}

// TestNoticeRouter_NoticeBypassesTheChain is the point of the whole
// feature: an operational notice must reach the placeholder without
// touching the answer stream or any wrapper that reasons about it.
func TestNoticeRouter_NoticeBypassesTheChain(t *testing.T) {
	sink := newStreamingSink(nil, false)
	next := &countingSink{}
	r := &noticeRouter{next: next, sink: sink}

	err := r.OnNotice(context.Background(), client.Notice{
		Level: client.NoticeLevelWarning, Kind: "provider_retry", Text: "⏳ retrying in 30s",
	})
	if err != nil {
		t.Fatalf("OnNotice: %v", err)
	}
	if next.updates != 0 {
		t.Errorf("notice leaked into the update chain: %d updates", next.updates)
	}
	if got := sink.Notice(); got != "⏳ retrying in 30s" {
		t.Errorf("Notice() = %q", got)
	}
}

func TestNoticeRouter_UpdatesForwarded(t *testing.T) {
	next := &countingSink{}
	r := &noticeRouter{next: next, sink: newStreamingSink(nil, false)}

	if err := r.OnUpdate(context.Background(), acp.SessionNotification{}); err != nil {
		t.Fatalf("OnUpdate: %v", err)
	}
	if next.updates != 1 {
		t.Errorf("expected update forwarded, got %d", next.updates)
	}
}

func TestStreamingSink_OnNotice_LatestWinsAndIgnoresBlank(t *testing.T) {
	s := newStreamingSink(nil, false)
	ctx := context.Background()

	if got := s.Notice(); got != "" {
		t.Errorf("fresh sink Notice() = %q, want empty", got)
	}
	_ = s.OnNotice(ctx, client.Notice{Text: "first"})
	_ = s.OnNotice(ctx, client.Notice{Text: "second"})
	if got := s.Notice(); got != "second" {
		t.Errorf("latest should win, got %q", got)
	}
	// A blank notice must not erase a real one.
	_ = s.OnNotice(ctx, client.Notice{Text: "   "})
	if got := s.Notice(); got != "second" {
		t.Errorf("blank notice clobbered state: %q", got)
	}
}

// TestStreamingSink_ImplementsNoticeSink pins the interface assertion
// acp-kit's dispatch makes; without it notices are silently dropped.
func TestStreamingSink_ImplementsNoticeSink(t *testing.T) {
	var _ client.NoticeSink = (*streamingSink)(nil)
	var _ client.NoticeSink = (*noticeRouter)(nil)
	var _ client.SessionUpdateSink = (*noticeRouter)(nil)
}
