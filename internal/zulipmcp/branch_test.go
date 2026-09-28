package zulipmcp

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/kfet/zulip-acp/internal/journal"
)

// noBranch is the Branch hook of every test that does not exercise it.
func noBranch(string, []BranchTask) ([]BranchResult, error) { return nil, nil }

// branchTools wires a Tools whose Branch records what it was handed.
func branchTools(t *testing.T, key journal.Key, res []BranchResult, berr error) (*Tools, *[]BranchTask, *string) {
	t.Helper()
	var got []BranchTask
	var session string
	tools, err := NewTools(Config{
		Client:  &fakeClient{},
		ConvKey: func(k string) (journal.Key, bool) { return key, k == "c1" },
		Origin:  func(string) (journal.Parent, bool) { return journal.Parent{}, false },
		Rename:  func(journal.Key, string) (string, error) { return "", nil },
		Branch: func(s string, tasks []BranchTask) ([]BranchResult, error) {
			session, got = s, tasks
			return res, berr
		},
	})
	if err != nil {
		t.Fatalf("NewTools: %v", err)
	}
	return tools, &got, &session
}

// TestBranchToolPassesTheTasksThrough: the tasks reach the relay
// cleaned, under the TOKEN's session key, and the result comes back as
// JSON the agent can parse.
func TestBranchToolPassesTheTasksThrough(t *testing.T) {
	tools, got, session := branchTools(t, journal.Channel(4, "t"),
		[]BranchResult{{Topic: "a", Link: "#**fleet>a**", ConvID: "c9"}, {Error: "nope"}}, nil)
	out, err := pick(t, tools, ToolBranch).Handler("c1",
		json.RawMessage(`{"tasks":[{"title":"  a  b ","seed":"s","from_msg":5},{}]}`))
	if err != nil {
		t.Fatalf("branch: %v", err)
	}
	if *session != "c1" || len(*got) != 2 || (*got)[0] != (BranchTask{Title: "a b", Seed: "s", FromMsg: 5}) {
		t.Fatalf("session %q tasks %+v", *session, *got)
	}
	var res []BranchResult
	if err := json.Unmarshal([]byte(out), &res); err != nil || len(res) != 2 || res[0].ConvID != "c9" || res[1].Error != "nope" {
		t.Fatalf("out = %s (%v)", out, err)
	}
}

// TestBranchToolRefusals: every shape refusal happens BEFORE the relay
// is asked to create anything — including the per-call cap.
func TestBranchToolRefusals(t *testing.T) {
	many := `{"tasks":[` + strings.TrimSuffix(strings.Repeat(`{},`, MaxBranchTasks+1), ",") + `]}`
	cases := []struct {
		name string
		key  journal.Key
		args string
		want string
	}{
		{"no tasks", journal.Channel(4, "t"), `{"tasks":[]}`, "at least one"},
		{"cap", journal.Channel(4, "t"), many, "over the maximum of 10"},
		{"dm", journal.DM([]int64{1, 2}), `{"tasks":[{}]}`, "direct message"},
		{"negative", journal.Channel(4, "t"), `{"tasks":[{"from_msg":-1}]}`, "task 1: from_msg"},
		{"bad title", journal.Channel(4, "t"), `{"tasks":[{},{"title":"` + strings.Repeat("x", 100) + `"}]}`, "task 2:"},
		{"bad json", journal.Channel(4, "t"), `{"tasks":7}`, "invalid params"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tools, got, _ := branchTools(t, tc.key, nil, nil)
			_, err := pick(t, tools, ToolBranch).Handler("c1", json.RawMessage(tc.args))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			if *got != nil {
				t.Fatalf("the relay was asked anyway: %+v", *got)
			}
		})
	}
}

// TestBranchToolReportsTheRelaysError: a whole-call failure is the
// relay's words, verbatim.
func TestBranchToolReportsTheRelaysError(t *testing.T) {
	tools, _, _ := branchTools(t, journal.Channel(4, "t"), nil, errors.New("boom"))
	if _, err := pick(t, tools, ToolBranch).Handler("c1", json.RawMessage(`{"tasks":[{}]}`)); err == nil || err.Error() != "boom" {
		t.Fatalf("err = %v", err)
	}
}
