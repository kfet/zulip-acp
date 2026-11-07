package zulipmcp

import (
	"errors"
	"strings"
	"testing"

	"github.com/kfet/zulip-acp/internal/journal"
)

func stateTools(t *testing.T, fn func(string, string, string) (string, error), inbox func(string) (string, error)) *Tools {
	t.Helper()
	tools, err := NewTools(Config{
		Client:     &fakeClient{},
		ConvKey:    func(k string) (journal.Key, bool) { return journal.Channel(4, "t"), k == "c1" },
		Origin:     func(string) (journal.Parent, bool) { return journal.Parent{}, false },
		Children:   noKids,
		Rename:     func(journal.Key, string) (string, error) { return "", nil },
		Branch:     noBranch,
		React:      noReact,
		TopicState: fn,
		Inbox:      inbox,
	})
	if err != nil {
		t.Fatalf("NewTools: %v", err)
	}
	return tools
}

// TestStateToolsOnlyWhenOffered: without the hooks the agent is not
// offered tools that would do nothing.
func TestStateToolsOnlyWhenOffered(t *testing.T) {
	for _, x := range stateTools(t, nil, nil).Tools() {
		if x.Name == ToolTopicState || x.Name == ToolInbox {
			t.Fatalf("%s offered with no hook", x.Name)
		}
	}
}

func TestTopicStateToolDescriptionIsTheTestedOne(t *testing.T) {
	tool := pick(t, stateTools(t, func(string, string, string) (string, error) { return "", nil }, nil), ToolTopicState)
	for _, want := range []string{
		"Record this topic's state when it changes.",
		"- blocked: you cannot continue, and an answer alone will not fix it",
		"If the state is the same as before, do not call it. The user can override it with !state.",
	} {
		if !strings.Contains(tool.Description, want) {
			t.Errorf("description lacks %q", want)
		}
	}
	props := tool.Schema["properties"].(map[string]any)
	if got := props["state"].(map[string]any)["enum"].([]any); len(got) != 5 {
		t.Fatalf("enum = %v", got)
	}
}

func TestTopicStateToolPassesStateAndReason(t *testing.T) {
	var got string
	tools := stateTools(t, func(s, state, reason string) (string, error) {
		got = s + ":" + state + ":" + reason
		return "ok", nil
	}, nil)
	h := pick(t, tools, ToolTopicState).Handler
	if out, err := h("c1", []byte(`{"state":" fyi ","reason":"  shipped v1 "}`)); err != nil || out != "ok" {
		t.Fatalf("= %q, %v", out, err)
	}
	if got != "c1:fyi:shipped v1" {
		t.Fatalf("call = %q", got)
	}
}

func TestTopicStateToolRefuses(t *testing.T) {
	called := false
	tools := stateTools(t, func(string, string, string) (string, error) { called = true; return "", errors.New("nope") }, nil)
	h := pick(t, tools, ToolTopicState).Handler
	for _, args := range []string{`{`, `{"state":"maybe","reason":"x"}`, `{"state":"done"}`, `{"state":"done","reason":"  "}`} {
		if _, err := h("c1", []byte(args)); err == nil {
			t.Errorf("%s: want refusal", args)
		}
	}
	if _, err := h("nobody", []byte(`{"state":"done","reason":"x"}`)); err == nil {
		t.Error("an unknown session must be refused")
	}
	if called {
		t.Fatal("a refused call reached the relay")
	}
	if _, err := h("c1", []byte(`{"state":"done","reason":"x"}`)); err == nil || err.Error() != "nope" {
		t.Fatalf("relay refusal = %v", err)
	}
}

func TestInboxTool(t *testing.T) {
	tools := stateTools(t, nil, func(s string) (string, error) { return "inbox for " + s, nil })
	h := pick(t, tools, ToolInbox).Handler
	if out, err := h("c1", nil); err != nil || out != "inbox for c1" {
		t.Fatalf("= %q, %v", out, err)
	}
	if _, err := h("nobody", nil); err == nil {
		t.Fatal("an unknown session must be refused")
	}
}
