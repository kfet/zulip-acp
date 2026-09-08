package zulipmcp

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kfet/zulip-acp/internal/journal"
	"github.com/kfet/zulip-acp/internal/zulipproto"
)

// parentOf is a branch point in a channel topic.
func parentOf(streamID int64, topic string, msgID int64) *journal.Parent {
	return &journal.Parent{Key: journal.Channel(streamID, topic), MessageID: msgID}
}

// TestHistoryOriginReadsTheParentTopic: the one cross-conversation
// read the relay permits, narrowed to the DECLARED parent and nothing
// else.
func TestHistoryOriginReadsTheParentTopic(t *testing.T) {
	c := &fakeClient{msgs: []zulipproto.Message{msg(1, "Alice", "how it started")}}
	tools := newBranchedTools(t, c, "c1", journal.Channel(4, "spun out"), parentOf(4, "planning", 949))
	out, err := only(t, tools).Handler("c1", json.RawMessage(`{"origin":true}`))
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(c.narrow) != 2 || c.narrow[1].Operand != "planning" {
		t.Fatalf("narrow = %+v, want the parent topic", c.narrow)
	}
	if !strings.Contains(out, "the origin conversation") {
		t.Fatalf("the reply does not say which conversation it read: %q", out)
	}
	if !strings.Contains(out, "origin=true and before_id=1") {
		t.Fatalf("paging back into the origin is not stated: %q", out)
	}
}

// TestHistoryOriginIsClampedToTheBranchPoint is the permission model:
// a branched session may read what LED to the branch, never what the
// origin went on to say afterwards.
func TestHistoryOriginIsClampedToTheBranchPoint(t *testing.T) {
	c := &fakeClient{msgs: []zulipproto.Message{msg(1, "Alice", "x")}}
	tools := newBranchedTools(t, c, "c1", journal.Channel(4, "spun out"), parentOf(4, "planning", 949))
	h := only(t, tools).Handler

	// No before_id: the clamp IS the anchor. Zulip's anchor is
	// exclusive, so "at or before 949" is "before 950".
	if _, err := h("c1", json.RawMessage(`{"origin":true}`)); err != nil {
		t.Fatalf("history: %v", err)
	}
	if c.beforeID != 950 {
		t.Fatalf("beforeID = %d, want the branch point + 1", c.beforeID)
	}

	// A before_id the agent supplies that reaches PAST the branch
	// point is clamped back to it — otherwise the clamp would be a
	// suggestion.
	if _, err := h("c1", json.RawMessage(`{"origin":true,"before_id":99999}`)); err != nil {
		t.Fatalf("history: %v", err)
	}
	if c.beforeID != 950 {
		t.Fatalf("beforeID = %d, want the branch point + 1", c.beforeID)
	}

	// A before_id inside the allowed range is honoured: paging back is
	// the whole point of the parameter.
	if _, err := h("c1", json.RawMessage(`{"origin":true,"before_id":100}`)); err != nil {
		t.Fatalf("history: %v", err)
	}
	if c.beforeID != 100 {
		t.Fatalf("beforeID = %d, want the agent's own anchor", c.beforeID)
	}
}

// TestHistoryOriginRefusedWithoutAParent: a topic nobody branched has
// no origin, and the refusal says what to do instead.
func TestHistoryOriginRefusedWithoutAParent(t *testing.T) {
	c := &fakeClient{msgs: []zulipproto.Message{msg(1, "Alice", "x")}}
	tools := newTools(t, c, "c1", journal.Channel(4, "ordinary"))
	out, err := only(t, tools).Handler("c1", json.RawMessage(`{"origin":true}`))
	if err == nil {
		t.Fatalf("an unbranched conversation must have no origin to read; got %q", out)
	}
	if !strings.Contains(err.Error(), "no origin conversation to read") {
		t.Fatalf("err = %v", err)
	}
	if c.narrow != nil {
		t.Fatal("the refused call still reached Zulip")
	}
}

// TestHistoryOriginIsOneHop: the parent's own parent is not reachable.
// There is no argument for it and no chaining in the resolver — the
// tool asks for the CALLER's origin, once. Otherwise "read my
// ancestors" would quietly become "read everything".
//
// It also pins that the origin is resolved LAZILY: an ordinary history
// call must not pay for a parent it is not asking about.
func TestHistoryOriginIsOneHop(t *testing.T) {
	// A grandchild whose parent is the child. The Origin hook is asked
	// for "c-grandchild" and answers with the child; nothing ever asks
	// for the child's own origin.
	var asked []string
	c := &fakeClient{msgs: []zulipproto.Message{msg(1, "Alice", "x")}}
	tools, err := NewTools(Config{
		Client:  c,
		ConvKey: func(string) (journal.Key, bool) { return journal.Channel(4, "grandchild"), true },
		Origin: func(k string) (journal.Parent, bool) {
			asked = append(asked, k)
			return journal.Parent{Key: journal.Channel(4, "child"), MessageID: 500}, true
		},
		Rename: func(journal.Key, string) (string, error) { return "", nil },
		Logf:   func(string, ...any) {},
	})
	if err != nil {
		t.Fatalf("NewTools: %v", err)
	}
	if _, err := only(t, tools).Handler("c-grandchild", json.RawMessage(`{"origin":true}`)); err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(asked) != 1 || asked[0] != "c-grandchild" {
		t.Fatalf("Origin was asked %v; a second hop would be one entry more", asked)
	}
	if c.narrow[1].Operand != "child" {
		t.Fatalf("narrow = %+v, want the PARENT, never the grandparent", c.narrow)
	}
	if _, err := only(t, tools).Handler("c-grandchild", nil); err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(asked) != 1 {
		t.Fatalf("a plain history call resolved the origin anyway: %v", asked)
	}
}

// TestHistoryOriginReadsADMParent: a branch out of a direct message
// records the DM as its parent, and the narrow must follow — a channel
// narrow would match nothing there, silently.
func TestHistoryOriginReadsADMParent(t *testing.T) {
	c := &fakeClient{msgs: []zulipproto.Message{msg(1, "Alice", "x")}}
	parent := &journal.Parent{Key: journal.DM([]int64{4, 9}), MessageID: 12}
	tools := newBranchedTools(t, c, "c1", journal.Channel(4, "spun out"), parent)
	if _, err := only(t, tools).Handler("c1", json.RawMessage(`{"origin":true}`)); err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(c.narrow) != 1 || c.narrow[0].Operator != "dm" {
		t.Fatalf("narrow = %+v", c.narrow)
	}
}

// TestHistoryWithoutOriginStillReadsHere: having a parent does not
// change the default. A branched session reads its OWN topic unless it
// asks for the other one.
func TestHistoryWithoutOriginStillReadsHere(t *testing.T) {
	c := &fakeClient{msgs: []zulipproto.Message{msg(1, "Alice", "x")}}
	tools := newBranchedTools(t, c, "c1", journal.Channel(4, "spun out"), parentOf(4, "planning", 949))
	out, err := only(t, tools).Handler("c1", nil)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if c.narrow[1].Operand != "spun out" || c.beforeID != 0 {
		t.Fatalf("narrow = %+v beforeID = %d", c.narrow, c.beforeID)
	}
	if !strings.Contains(out, "this conversation") || strings.Contains(out, "origin=true and") {
		t.Fatalf("out = %q", out)
	}
}

// TestRenderNamesTheConversationItRead: the empty page has to say WHICH
// conversation was empty, or an agent reading its origin cannot tell
// "nothing there" from "wrong place".
func TestRenderNamesTheConversationItRead(t *testing.T) {
	if got := render(nil, false, true); !strings.Contains(got, "the origin conversation") {
		t.Fatalf("render = %q", got)
	}
}
