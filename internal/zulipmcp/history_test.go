package zulipmcp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/kfet/acp-kit/mcphost"
	"github.com/kfet/zulip-acp/internal/journal"
	"github.com/kfet/zulip-acp/internal/zulipproto"
)

// fakeClient records the narrow it was asked for and replays a fixed
// page, so every assertion here is about OUR framing, not Zulip's.
type fakeClient struct {
	narrow   []zulipproto.NarrowTerm
	limit    int
	beforeID int64
	msgs     []zulipproto.Message
	err      error
}

func (f *fakeClient) Messages(_ context.Context, narrow []zulipproto.NarrowTerm, limit int, beforeID int64) ([]zulipproto.Message, error) {
	f.narrow, f.limit, f.beforeID = narrow, limit, beforeID
	return f.msgs, f.err
}

// newTools wires a Tools whose only known conversation is convID.
func newTools(t *testing.T, c *fakeClient, convID string, key journal.Key) *Tools {
	t.Helper()
	tools, err := NewTools(Config{
		Client: c,
		ConvKey: func(k string) (journal.Key, bool) {
			if k != convID {
				return journal.Key{}, false
			}
			return key, true
		},
		Logf: func(string, ...any) {},
	})
	if err != nil {
		t.Fatalf("NewTools: %v", err)
	}
	return tools
}

// only returns the single history tool.
func only(t *testing.T, tools *Tools) Tool {
	t.Helper()
	set := tools.Tools()
	if len(set) != 1 || set[0].Name != ToolHistory {
		t.Fatalf("tool set = %+v", set)
	}
	return set[0]
}

func msg(id int64, who, body string) zulipproto.Message {
	return zulipproto.Message{ID: id, SenderName: who, Content: body, Timestamp: 1756800000}
}

// --- construction --------------------------------------------------------

func TestNewToolsRequiresItsDependencies(t *testing.T) {
	if _, err := NewTools(Config{ConvKey: func(string) (journal.Key, bool) { return journal.Key{}, true }}); err == nil {
		t.Fatal("a Tools with no Client must not construct")
	}
	if _, err := NewTools(Config{Client: &fakeClient{}}); err == nil {
		t.Fatal("a Tools with no ConvKey has no identity and must not construct")
	}
	tools, err := NewTools(Config{Client: &fakeClient{}, ConvKey: func(string) (journal.Key, bool) { return journal.Key{}, true }})
	if err != nil {
		t.Fatalf("NewTools: %v", err)
	}
	if tools.cfg.Timeout != DefaultTimeout {
		t.Fatalf("Timeout = %v; a tool call must never be unbounded", tools.cfg.Timeout)
	}
	tools.cfg.Logf("a nil Logf must be replaced, not called")
}

// TestSchemaTakesNoConversation pins the guarantee the design rests on:
// there is nowhere in the tool's arguments to name a conversation.
func TestSchemaTakesNoConversation(t *testing.T) {
	tools := newTools(t, &fakeClient{}, "c1", journal.Channel(4, "t"))
	props, ok := only(t, tools).Schema["properties"].(map[string]any)
	if !ok {
		t.Fatal("schema has no properties")
	}
	for name := range props {
		if name != "limit" && name != "before_id" {
			t.Fatalf("unexpected parameter %q: a tool must not take a conversation", name)
		}
	}
}

// --- identity ------------------------------------------------------------

// TestUnknownConversationIsRefused: a session key the journal does not
// know reads nothing at all.
func TestUnknownConversationIsRefused(t *testing.T) {
	c := &fakeClient{msgs: []zulipproto.Message{msg(1, "a", "x")}}
	tools := newTools(t, c, "c1", journal.Channel(4, "t"))
	out, err := only(t, tools).Handler("cdeadbeef", nil)
	if err == nil {
		t.Fatalf("an unknown conversation must be refused; got %q", out)
	}
	if c.narrow != nil {
		t.Fatal("the refused call still reached Zulip")
	}
}

// --- narrow --------------------------------------------------------------

func TestHistoryNarrowsToTheTopic(t *testing.T) {
	c := &fakeClient{msgs: []zulipproto.Message{msg(1, "Alice", "hello")}}
	tools := newTools(t, c, "c1", journal.Channel(4, "sess"))
	out, err := only(t, tools).Handler("c1", nil)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(c.narrow) != 2 || c.narrow[0].Operator != "channel" || c.narrow[1].Operand != "sess" {
		t.Fatalf("narrow = %+v", c.narrow)
	}
	if c.limit != DefaultLimit || c.beforeID != 0 {
		t.Fatalf("limit = %d, beforeID = %d", c.limit, c.beforeID)
	}
	if !strings.Contains(out, "#1") || !strings.Contains(out, "Alice") || !strings.Contains(out, "hello") {
		t.Fatalf("output = %q", out)
	}
}

func TestHistoryNarrowsToTheDM(t *testing.T) {
	c := &fakeClient{msgs: []zulipproto.Message{msg(1, "Alice", "hello")}}
	tools := newTools(t, c, "c1", journal.DM([]int64{4, 9}))
	if _, err := only(t, tools).Handler("c1", nil); err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(c.narrow) != 1 || c.narrow[0].Operator != "dm" {
		t.Fatalf("narrow = %+v", c.narrow)
	}
}

// --- arguments -----------------------------------------------------------

func TestHistoryArguments(t *testing.T) {
	c := &fakeClient{msgs: []zulipproto.Message{msg(1, "Alice", "hello")}}
	tools := newTools(t, c, "c1", journal.Channel(4, "sess"))
	h := only(t, tools).Handler

	if _, err := h("c1", json.RawMessage(`{"limit":5,"before_id":42}`)); err != nil {
		t.Fatalf("history: %v", err)
	}
	if c.limit != 5 || c.beforeID != 42 {
		t.Fatalf("limit = %d, beforeID = %d", c.limit, c.beforeID)
	}

	out, err := h("c1", json.RawMessage(`{"limit":100000}`))
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if c.limit != MaxLimit || !strings.Contains(out, "clamped") {
		t.Fatalf("limit = %d, out = %q", c.limit, out)
	}

	if _, err := h("c1", json.RawMessage(`{"limit":-1}`)); err == nil {
		t.Fatal("a negative limit must be refused")
	}
	if _, err := h("c1", json.RawMessage(`{"before_id":-1}`)); err == nil {
		t.Fatal("a negative before_id must be refused")
	}
	if _, err := h("c1", json.RawMessage(`{"limit":`)); err == nil {
		t.Fatal("malformed params must be refused")
	}
}

func TestHistoryPropagatesClientErrors(t *testing.T) {
	c := &fakeClient{err: errors.New("zulip is down")}
	tools := newTools(t, c, "c1", journal.Channel(4, "sess"))
	if _, err := only(t, tools).Handler("c1", nil); err == nil {
		t.Fatal("a failed fetch must surface as a tool error")
	}
}

func TestHistoryTimesOut(t *testing.T) {
	blocked := make(chan struct{})
	tools, err := NewTools(Config{
		Client:  clientFunc(func(ctx context.Context) error { <-ctx.Done(); close(blocked); return ctx.Err() }),
		ConvKey: func(string) (journal.Key, bool) { return journal.Channel(4, "t"), true },
		Timeout: time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewTools: %v", err)
	}
	if _, err := only(t, tools).Handler("c1", nil); err == nil {
		t.Fatal("a wedged fetch must not hang the turn forever")
	}
	<-blocked
}

// clientFunc is a Client whose fetch runs an arbitrary body — used to
// drive the timeout branch without a clock or a sleep.
type clientFunc func(ctx context.Context) error

func (f clientFunc) Messages(ctx context.Context, _ []zulipproto.NarrowTerm, _ int, _ int64) ([]zulipproto.Message, error) {
	return nil, f(ctx)
}

// --- registration --------------------------------------------------------

// TestRegisterInstallsOnAHost: the tool set reaches a real mcphost.Host
// under the relay's own identity — the wiring main.go depends on.
func TestRegisterInstallsOnAHost(t *testing.T) {
	h, err := mcphost.New(HostConfig())
	if err != nil {
		t.Fatalf("mcphost.New: %v", err)
	}
	t.Cleanup(func() { _ = h.Close() })
	newTools(t, &fakeClient{}, "c1", journal.Channel(4, "t")).Register(h)
	got := h.ServerConfigForSession("c1")
	if len(got) != 1 || got[0].Stdio == nil || got[0].Stdio.Name != ServerName {
		t.Fatalf("server config = %+v", got)
	}
}

// --- rendering -----------------------------------------------------------

func TestRenderEmptyPage(t *testing.T) {
	if got := render(nil, false); !strings.Contains(got, "No earlier messages") {
		t.Fatalf("render = %q", got)
	}
}

// TestRenderIsOldestFirstAndNamesThePagingAnchor: the agent reads a
// conversation in the order it happened, and is told how to go further
// back without having to guess an id.
func TestRenderIsOldestFirst(t *testing.T) {
	got := render([]zulipproto.Message{msg(1, "Alice", "first"), msg(2, "bot", "second")}, false)
	if strings.Index(got, "first") > strings.Index(got, "second") {
		t.Fatalf("not oldest first: %q", got)
	}
	if !strings.Contains(got, "before_id=1") {
		t.Fatalf("no paging anchor: %q", got)
	}
	if !strings.Contains(got, "2025-09-02T") {
		t.Fatalf("no timestamp: %q", got)
	}
}

// TestRenderFallsBackToTheSenderEmail: Zulip always sends a full name,
// but a message with none must still be attributable.
func TestRenderFallsBackToTheSenderEmail(t *testing.T) {
	m := msg(1, "", "hi")
	m.SenderEmail = "bot@example.com"
	if got := render([]zulipproto.Message{m}, false); !strings.Contains(got, "bot@example.com") {
		t.Fatalf("render = %q", got)
	}
}

// TestRenderTruncatesOneLongMessage: a single maximal Zulip message is
// 10000 code points; the reply says when it cut one.
func TestRenderTruncatesOneLongMessage(t *testing.T) {
	got := render([]zulipproto.Message{msg(1, "Alice", strings.Repeat("é", MaxMessageRunes+50))}, false)
	if !strings.Contains(got, "[truncated]") || !strings.Contains(got, "were truncated") {
		t.Fatalf("render = %q", got)
	}
	if n := utf8.RuneCountInString(got); n > MaxMessageRunes+400 {
		t.Fatalf("truncation did not bound the body: %d runes", n)
	}
}

// TestRenderDropsTheOldestWhenTheTotalBinds: the newest end of the
// conversation is what survives, and before_id names the oldest that
// did — so paging back reaches what was dropped.
func TestRenderDropsTheOldestWhenTheTotalBinds(t *testing.T) {
	var msgs []zulipproto.Message
	for i := int64(1); i <= 40; i++ {
		msgs = append(msgs, msg(i, "Alice", strings.Repeat("x", MaxMessageRunes)))
	}
	got := render(msgs, false)
	if n := utf8.RuneCountInString(got); n > MaxTotalRunes+500 {
		t.Fatalf("reply not bounded: %d runes", n)
	}
	if !strings.Contains(got, "were dropped") {
		t.Fatalf("the drop was not reported: %q", got[:200])
	}
	if strings.Contains(got, "[#1 ") {
		t.Fatal("the oldest message survived a binding total budget")
	}
	if !strings.Contains(got, "[#40 ") {
		t.Fatal("the newest message was dropped")
	}
}

// TestRenderKeepsOneOversizeMessage: a single message bigger than the
// whole budget must still come back, or the tool would answer nothing.
func TestRenderKeepsOneOversizeMessage(t *testing.T) {
	got := render([]zulipproto.Message{msg(9, "Alice", strings.Repeat("x", MaxTotalRunes*2))}, false)
	if !strings.Contains(got, "[#9 ") {
		t.Fatalf("render = %q", got)
	}
}
