package zulipmcp

import (
	"errors"
	"slices"
	"testing"

	"github.com/kfet/zulip-acp/internal/journal"
)

func statusTools(t *testing.T, fn func(string, string) (string, error)) *Tools {
	t.Helper()
	tools, err := NewTools(Config{
		Client:      &fakeClient{},
		ConvKey:     func(k string) (journal.Key, bool) { return journal.Channel(4, "t"), k == "c1" },
		Origin:      func(string) (journal.Parent, bool) { return journal.Parent{}, false },
		Children:    noKids,
		Rename:      func(journal.Key, string) (string, error) { return "", nil },
		Branch:      noBranch,
		React:       noReact,
		TopicStatus: fn,
	})
	if err != nil {
		t.Fatalf("NewTools: %v", err)
	}
	return tools
}

// TestTopicStatusToolOnlyWhenOffered: without the hook the agent is not
// offered a tool that would do nothing.
func TestTopicStatusToolOnlyWhenOffered(t *testing.T) {
	for _, x := range statusTools(t, nil).Tools() {
		if x.Name == ToolTopicStatus {
			t.Fatal("topic_status offered with no hook")
		}
	}
}

// TestTopicStatusToolPassesState: the session comes from the token,
// and an empty state means done.
func TestTopicStatusToolPassesState(t *testing.T) {
	var got []string
	tools := statusTools(t, func(s, state string) (string, error) {
		got = append(got, s+":"+state)
		return "ok " + state, nil
	})
	h := pick(t, tools, ToolTopicStatus).Handler
	for args, want := range map[string]string{
		`{"state":"needs_input"}`: "ok needs_input",
		`{"state":" done "}`:      "ok done",
		`{}`:                      "ok done",
	} {
		if out, err := h("c1", []byte(args)); err != nil || out != want {
			t.Errorf("%s = %q, %v", args, out, err)
		}
	}
	if len(got) != 3 || !slices.Contains(got, "c1:needs_input") || !slices.Contains(got, "c1:done") {
		t.Fatalf("calls = %v", got)
	}
}

func TestTopicStatusToolRefuses(t *testing.T) {
	called := false
	tools := statusTools(t, func(string, string) (string, error) { called = true; return "", errors.New("no turn") })
	h := pick(t, tools, ToolTopicStatus).Handler
	for _, args := range []string{`{`, `{"state":"maybe"}`} {
		if _, err := h("c1", []byte(args)); err == nil {
			t.Errorf("%s: want refusal", args)
		}
	}
	if _, err := h("nobody", []byte(`{}`)); err == nil {
		t.Error("an unknown session must be refused")
	}
	if called {
		t.Fatal("a refused call reached the relay")
	}
	if _, err := h("c1", []byte(`{"state":"done"}`)); err == nil || err.Error() != "no turn" {
		t.Fatalf("relay refusal = %v", err)
	}
}
