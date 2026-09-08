package zulipmcp

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/kfet/zulip-acp/internal/journal"
	"github.com/kfet/zulip-acp/internal/zulipproto"
)

// renameTools wires a Tools whose Rename records what it was asked for
// and replays a fixed answer.
type renameCall struct {
	key   journal.Key
	title string
}

func renameTools(t *testing.T, key journal.Key, reply string, rerr error) (*Tools, *[]renameCall) {
	t.Helper()
	var calls []renameCall
	tools, err := NewTools(Config{
		Client:  &fakeClient{},
		ConvKey: func(k string) (journal.Key, bool) { return key, k == "c1" },
		Origin:  func(string) (journal.Parent, bool) { return journal.Parent{}, false },
		Rename: func(k journal.Key, title string) (string, error) {
			calls = append(calls, renameCall{k, title})
			return reply, rerr
		},
		Logf: func(string, ...any) {},
	})
	if err != nil {
		t.Fatalf("NewTools: %v", err)
	}
	return tools, &calls
}

func renameArgs(title string) json.RawMessage {
	b, err := json.Marshal(map[string]string{"title": title})
	if err != nil {
		panic(err)
	}
	return b
}

// TestRenameTopicArmsTheRename: the happy path passes the conversation
// resolved from the token — never from an argument — and returns what
// the relay said.
func TestRenameTopicArmsTheRename(t *testing.T) {
	key := journal.Channel(4, "auto named from the first line")
	tools, calls := renameTools(t, key, "armed", nil)
	out, err := pick(t, tools, ToolRenameTopic).Handler("c1", renameArgs("  Topic  rename  design "))
	if err != nil {
		t.Fatalf("rename: %v", err)
	}
	if out != "armed" {
		t.Fatalf("reply = %q, want the relay's own answer verbatim", out)
	}
	if len(*calls) != 1 {
		t.Fatalf("calls = %+v", *calls)
	}
	if got := (*calls)[0].title; got != "Topic rename design" {
		t.Fatalf("title = %q, want the whitespace squashed", got)
	}
	if (*calls)[0].key.Label() != key.Label() {
		t.Fatalf("key = %+v, want the one bound to the connection", (*calls)[0].key)
	}
}

// TestRenameTopicRefusesUnusableTitles: each refusal is prose the AGENT
// reads, so each must name what to send instead.
func TestRenameTopicRefusesUnusableTitles(t *testing.T) {
	tools, calls := renameTools(t, journal.Channel(4, "t"), "armed", nil)
	cases := []struct {
		name, title, want string
	}{
		{"empty", "   \n\t ", "must not be empty"},
		{"too long", strings.Repeat("é", zulipproto.MaxTopicLength+1), "over Zulip's maximum"},
		{"empty topic", "", "must not be empty"},
		{"general chat", " General Chat ", "general chat"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := pick(t, tools, ToolRenameTopic).Handler("c1", renameArgs(tc.title))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one mentioning %q", err, tc.want)
			}
		})
	}
	if len(*calls) != 0 {
		t.Fatalf("a rejected title reached the relay: %+v", *calls)
	}
}

// TestRenameTopicAcceptsAMaximalTitle: MAX_TOPIC_LENGTH is counted in
// CODE POINTS, so a 60-character non-ASCII title is legal and must not
// be refused as if it were bytes.
func TestRenameTopicAcceptsAMaximalTitle(t *testing.T) {
	tools, calls := renameTools(t, journal.Channel(4, "t"), "armed", nil)
	if _, err := pick(t, tools, ToolRenameTopic).Handler("c1", renameArgs(strings.Repeat("é", zulipproto.MaxTopicLength))); err != nil {
		t.Fatalf("a title of exactly MAX_TOPIC_LENGTH code points was refused: %v", err)
	}
	if len(*calls) != 1 {
		t.Fatalf("calls = %+v", *calls)
	}
}

// TestRenameTopicRefusesADM: a direct message has no topic, and the
// refusal must come before anything is armed.
func TestRenameTopicRefusesADM(t *testing.T) {
	tools, calls := renameTools(t, journal.DM([]int64{1, 2}), "armed", nil)
	_, err := pick(t, tools, ToolRenameTopic).Handler("c1", renameArgs("a title"))
	if err == nil || !strings.Contains(err.Error(), "direct message") {
		t.Fatalf("err = %v, want a DM refusal", err)
	}
	if len(*calls) != 0 {
		t.Fatalf("a DM reached the relay: %+v", *calls)
	}
}

// TestRenameTopicSurfacesTheRelaysRefusal: whatever the handler refuses
// — an unknown conversation, a turn with no anchor — reaches the agent
// as the tool's error rather than as a silent no-op.
func TestRenameTopicSurfacesTheRelaysRefusal(t *testing.T) {
	tools, _ := renameTools(t, journal.Channel(4, "t"), "", errors.New("no anchor here"))
	if _, err := pick(t, tools, ToolRenameTopic).Handler("c1", renameArgs("a title")); err == nil ||
		!strings.Contains(err.Error(), "no anchor here") {
		t.Fatalf("err = %v, want the relay's refusal", err)
	}
}

// TestRenameTopicRejectsAStrangerAndBadArgs: identity comes from the
// connection, and malformed arguments are the agent's problem to fix.
func TestRenameTopicRejectsAStrangerAndBadArgs(t *testing.T) {
	tools, calls := renameTools(t, journal.Channel(4, "t"), "armed", nil)
	tool := pick(t, tools, ToolRenameTopic)
	if _, err := tool.Handler("someone-else", renameArgs("a title")); err == nil {
		t.Fatal("a session key the relay does not own must be refused")
	}
	if _, err := tool.Handler("c1", json.RawMessage(`{"title":42}`)); err == nil {
		t.Fatal("a non-string title must be refused")
	}
	if len(*calls) != 0 {
		t.Fatalf("calls = %+v", *calls)
	}
}

// TestRenameTopicSchemaNamesItsArgument: the schema is the only thing
// the agent sees before calling, so `title` must be declared required.
func TestRenameTopicSchemaNamesItsArgument(t *testing.T) {
	tool := pick(t, renameToolsOnly(t), ToolRenameTopic)
	props, ok := tool.Schema["properties"].(map[string]any)
	if !ok || props["title"] == nil {
		t.Fatalf("schema = %+v", tool.Schema)
	}
	req, ok := tool.Schema["required"].([]any)
	if !ok || len(req) != 1 || req[0] != "title" {
		t.Fatalf("required = %+v, want [title]", tool.Schema["required"])
	}
	if !strings.Contains(tool.Description, "turn ends") {
		t.Fatalf("description does not say WHEN the rename lands: %q", tool.Description)
	}
}

func renameToolsOnly(t *testing.T) *Tools {
	t.Helper()
	tools, _ := renameTools(t, journal.Channel(4, "t"), "armed", nil)
	return tools
}
