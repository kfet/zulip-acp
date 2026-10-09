package zulipmcp

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/kfet/zulip-acp/internal/journal"
	"github.com/kfet/zulip-acp/internal/zulipproto"
)

func noKids(string) ([]Child, bool) { return nil, true }

var testKids = []Child{
	{ConvID: "c-a", Key: journal.Channel(4, "✔ alpha"), Link: "#**dev>✔ alpha**"},
	{ConvID: "c-b1", Key: journal.Channel(4, "beta"), Link: "#**dev>beta**"},
	{ConvID: "c-b2", Key: journal.Channel(5, "Beta"), Link: "#**ops>Beta**"},
}

func newKidTools(t *testing.T, c *fakeClient, kids []Child, ok bool) *Tools {
	t.Helper()
	tools, err := NewTools(Config{
		Client:   c,
		ConvKey:  func(string) (journal.Key, bool) { return journal.Channel(4, "parent"), true },
		Origin:   func(string) (journal.Parent, bool) { return journal.Parent{}, false },
		Children: func(string) ([]Child, bool) { return kids, ok },
		Rename:   func(journal.Key, string) (string, error) { return "", nil },
		Branch:   noBranch,
	})
	if err != nil {
		t.Fatal(err)
	}
	return tools
}

func TestHistoryReadsAChildInFull(t *testing.T) {
	c := &fakeClient{msgs: []zulipproto.Message{msg(900, "Alice", "child work")}}
	h := only(t, newKidTools(t, c, testKids, true))
	for _, name := range []string{"c-a", "alpha", "ALPHA"} {
		got, err := h.Handler("s", json.RawMessage(`{"child":"`+name+`","before_id":1000}`))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if c.narrow[1].Operand != "✔ alpha" || c.beforeID != 1000 {
			t.Fatalf("%s: narrow %v before %d", name, c.narrow, c.beforeID)
		}
		if !strings.Contains(got, "#**dev>✔ alpha**") || !strings.Contains(got, `child="c-a" and before_id=900`) {
			t.Fatalf("reply = %s", got)
		}
	}
}

func TestHistoryRefusesWhatIsNotADirectChild(t *testing.T) {
	c := &fakeClient{}
	h := only(t, newKidTools(t, c, testKids, true))
	for _, tc := range []struct{ args, want string }{
		{`{"child":"stranger"}`, "not a topic branched directly"},
		{`{"child":"beta"}`, "more than one"},
		{`{"child":"c-a","origin":true}`, "cannot be combined"},
	} {
		if _, err := h.Handler("s", json.RawMessage(tc.args)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: err = %v", tc.args, err)
		}
	}
	if c.narrow != nil {
		t.Fatal("a refused call must not read anything")
	}
	h = only(t, newKidTools(t, c, nil, false))
	if _, err := h.Handler("s", json.RawMessage(`{"child":"c-a"}`)); err == nil {
		t.Fatal("an unresolvable caller must be refused")
	}
}

func TestListChildren(t *testing.T) {
	c := &fakeClient{msgs: []zulipproto.Message{msg(77, "Bob", "x")}}
	lc := pick(t, newKidTools(t, c, testKids, true), ToolListChildren)
	got, err := lc.Handler("s", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"3 topic(s)", "#**ops>Beta**", "conv_id c-b1", "by Bob (#77)", "history(child="} {
		if !strings.Contains(got, w) {
			t.Fatalf("missing %q in %s", w, got)
		}
	}

	c.msgs = nil
	got, _ = lc.Handler("s", nil)
	if !strings.Contains(got, "last activity: none") {
		t.Fatal(got)
	}
	c.err = errors.New("boom")
	got, _ = lc.Handler("s", nil)
	if !strings.Contains(got, "unknown (boom)") {
		t.Fatal(got)
	}

	m := msg(1, "", "x")
	m.SenderEmail = "e@x"
	c.err, c.msgs = nil, []zulipproto.Message{m}
	got, _ = lc.Handler("s", nil)
	if !strings.Contains(got, "by e@x") {
		t.Fatal(got)
	}

	if got, _ := pick(t, newKidTools(t, c, nil, true), ToolListChildren).Handler("s", nil); !strings.Contains(got, "No topics") {
		t.Fatal(got)
	}
	if _, err := pick(t, newKidTools(t, c, nil, false), ToolListChildren).Handler("s", nil); err == nil {
		t.Fatal("an unresolvable caller must be refused")
	}
}
