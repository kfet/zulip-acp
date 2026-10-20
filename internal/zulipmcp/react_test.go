package zulipmcp

import (
	"errors"
	"strings"
	"testing"

	"github.com/kfet/zulip-acp/internal/journal"
)

func noReact(string, int64, string) error { return nil }

type reactCall struct {
	session string
	id      int64
	emoji   string
}

func reactTools(t *testing.T, rerr error) (*Tools, *[]reactCall) {
	t.Helper()
	var calls []reactCall
	tools, err := NewTools(Config{
		Client:   &fakeClient{},
		ConvKey:  func(k string) (journal.Key, bool) { return journal.Channel(4, "t"), k == "c1" },
		Origin:   func(string) (journal.Parent, bool) { return journal.Parent{}, false },
		Children: noKids,
		Rename:   func(journal.Key, string) (string, error) { return "", nil },
		Branch:   noBranch,
		React: func(s string, id int64, emoji string) error {
			calls = append(calls, reactCall{s, id, emoji})
			return rerr
		},
	})
	if err != nil {
		t.Fatalf("NewTools: %v", err)
	}
	return tools, &calls
}

// TestReactPassesTheTokensConversation: the session comes from the
// token, and colons around the emoji name are removed.
func TestReactPassesTheTokensConversation(t *testing.T) {
	tools, calls := reactTools(t, nil)
	out, err := pick(t, tools, ToolReact).Handler("c1", []byte(`{"msg_id":42,"emoji":":one:"}`))
	if err != nil || out != "Added :one: to message 42." {
		t.Fatalf("react = %q, %v", out, err)
	}
	if len(*calls) != 1 || (*calls)[0] != (reactCall{"c1", 42, "one"}) {
		t.Fatalf("calls = %+v", *calls)
	}
}

func TestReactRefusesBadArguments(t *testing.T) {
	tools, calls := reactTools(t, nil)
	h := pick(t, tools, ToolReact).Handler
	for _, args := range []string{
		`{`,
		`{"emoji":"one"}`,
		`{"msg_id":-1,"emoji":"one"}`,
		`{"msg_id":1,"emoji":" :: "}`,
		`{"msg_id":1,"emoji":"two words"}`,
		`{"msg_id":1,"emoji":"` + strings.Repeat("x", MaxEmojiName+1) + `"}`,
	} {
		if _, err := h("c1", []byte(args)); err == nil {
			t.Errorf("%s: want refusal", args)
		}
	}
	if _, err := h("nobody", []byte(`{"msg_id":1,"emoji":"one"}`)); err == nil {
		t.Error("an unknown session must be refused")
	}
	if len(*calls) != 0 {
		t.Fatalf("a refused call reached the relay: %+v", *calls)
	}
}

func TestReactSurfacesTheRelaysRefusal(t *testing.T) {
	tools, _ := reactTools(t, errors.New("message 9 is not in this conversation"))
	if _, err := pick(t, tools, ToolReact).Handler("c1", []byte(`{"msg_id":9,"emoji":"one"}`)); err == nil ||
		!strings.Contains(err.Error(), "not in this conversation") {
		t.Fatalf("err = %v", err)
	}
}
