package handler

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kfet/zulip-acp/internal/journal"
	"github.com/kfet/zulip-acp/internal/zulipmcp"
	"github.com/kfet/zulip-acp/internal/zulipproto"
)

// toolHarness is branchReactHarness's shape for the `branch` tool: one
// engaged conversation in #fleet > "planning", returned with it.
func toolHarness(t *testing.T) (*harness, journal.Conv) {
	t.Helper()
	hh := branchHarness(t)
	hh.deliver(t, "planning", mention("hi"))
	origin, ok := hh.j.Lookup(journal.Channel(4, "planning"))
	if !ok {
		t.Fatal("no origin conversation")
	}
	return hh, origin
}

// callBranch runs the tool's relay half and waits for every turn it
// started.
func (hh *harness) callBranch(t *testing.T, convID string, tasks ...zulipmcp.BranchTask) []zulipmcp.BranchResult {
	t.Helper()
	res, err := hh.h.BranchTasks(convID, tasks)
	if err != nil {
		t.Fatalf("BranchTasks: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := hh.h.WaitIdle(ctx); err != nil {
		t.Fatalf("branched turns did not finish: %v", err)
	}
	return res
}

// postedIn returns every body the relay posted into a topic.
func (hh *harness) postedIn(topic string) []string {
	var out []string
	for _, id := range hh.z.order {
		if hh.topicOf(id) == topic {
			out = append(out, hh.z.body(id))
		}
	}
	return out
}

// TestBranchToolSeedsFromTheBranchPoint: with no seed, the from_msg
// text is the child's first message; the child's origin is clamped at
// from_msg; the journal holds the audit record; and ONE notification
// lists the link and its goal in the caller's topic.
func TestBranchToolSeedsFromTheBranchPoint(t *testing.T) {
	hh, origin := toolHarness(t)
	id := hh.plant("planning", "research the cache layer\nand the eviction policy", 99, "Grace Hopper")

	res := hh.callBranch(t, origin.ID, zulipmcp.BranchTask{FromMsg: id})

	if len(res) != 1 || res[0].Error != "" || res[0].Topic != "research the cache layer" ||
		res[0].Link != "#**fleet>research the cache layer**" || res[0].ConvID == "" {
		t.Fatalf("results = %+v", res)
	}
	child, ok := hh.j.LookupID(res[0].ConvID)
	if !ok || child.Parent == nil {
		t.Fatalf("child = %+v", child)
	}
	p := *child.Parent
	if p.MessageID != id || p.Key.Topic != "planning" {
		t.Fatalf("origin = %+v, want the cut-off at message %d", p, id)
	}
	if p.By != botName || p.At == 0 || !strings.HasPrefix(p.Seed, "research the cache layer") {
		t.Fatalf("audit record = %+v", p)
	}
	prompt := hh.lastPrompt()
	if !strings.Contains(prompt, "and the eviction policy") || !strings.Contains(prompt, "written by Grace Hopper") {
		t.Fatalf("prompt = %q", prompt)
	}
	notes := hh.postedIn("planning")
	last := notes[len(notes)-1]
	if !strings.Contains(last, "- #**fleet>research the cache layer** — research the cache layer") {
		t.Fatalf("notification = %q", last)
	}
	for _, b := range notes {
		if strings.HasPrefix(b, "branched → ") {
			t.Fatalf("the per-branch pointer must not be posted by the tool: %q", b)
		}
	}
	if !hh.logged("(the branch tool on message") {
		t.Fatalf("logs = %v", hh.logs)
	}
}

// TestBranchToolExplicitSeedAndTitle: both override the defaults, and
// the default branch point is the newest message the relay did NOT
// send — the newest one is usually its own in-flight answer.
func TestBranchToolExplicitSeedAndTitle(t *testing.T) {
	hh, origin := toolHarness(t)
	hh.z.setHistory(
		zulipproto.Message{ID: 40, SenderID: humanID},
		zulipproto.Message{ID: 41, SenderID: botID},
	)
	res := hh.callBranch(t, origin.ID, zulipmcp.BranchTask{Title: "Cache study", Seed: "study the cache"})
	if res[0].Topic != "Cache study" {
		t.Fatalf("results = %+v", res)
	}
	child, _ := hh.j.LookupID(res[0].ConvID)
	if child.Parent.MessageID != 40 {
		t.Fatalf("branch point = %d, want the newest non-relay message", child.Parent.MessageID)
	}
	if prompt := hh.lastPrompt(); !strings.Contains(prompt, "study the cache") || strings.Contains(prompt, "written by") {
		t.Fatalf("prompt = %q", prompt)
	}
}

// TestBranchToolRejectsAnotherTopicsMessage: a from_msg outside the
// caller's topic is refused per task, and the other tasks still run.
func TestBranchToolRejectsAnotherTopicsMessage(t *testing.T) {
	hh, origin := toolHarness(t)
	foreign := hh.plant("elsewhere", "secret plans", humanID, "Kfet")
	ok := hh.plant("planning", "fine work", humanID, "Kfet")

	res := hh.callBranch(t, origin.ID, zulipmcp.BranchTask{FromMsg: foreign}, zulipmcp.BranchTask{FromMsg: ok})

	if !strings.Contains(res[0].Error, "not in this topic") || res[0].ConvID != "" {
		t.Fatalf("foreign result = %+v", res[0])
	}
	if res[1].Error != "" {
		t.Fatalf("the good task failed too: %+v", res[1])
	}
	if _, found := hh.j.Lookup(journal.Channel(4, "secret plans")); found {
		t.Fatal("a branch was cut from another topic")
	}
}

// TestBranchToolPerTaskFailures: every way one task can fail is its
// own result, never the call's.
func TestBranchToolPerTaskFailures(t *testing.T) {
	hh, origin := toolHarness(t)
	empty := hh.plant("planning", "   ", humanID, "Kfet")
	hh.z.setHistory(zulipproto.Message{ID: 41, SenderID: botID})

	res := hh.callBranch(t, origin.ID,
		zulipmcp.BranchTask{FromMsg: 12345},
		zulipmcp.BranchTask{FromMsg: empty},
		zulipmcp.BranchTask{Seed: "x"},
	)
	for i, want := range []string{"message 12345 is not in this topic", "no text", "anybody but the relay"} {
		if !strings.Contains(res[i].Error, want) {
			t.Fatalf("task %d = %+v, want %q", i+1, res[i], want)
		}
	}
	// Nothing was made, so nothing is announced.
	for _, b := range hh.postedIn("planning") {
		if strings.HasPrefix(b, "🌿") {
			t.Fatalf("a notification for nothing: %q", b)
		}
	}

	hh.z.mu.Lock()
	hh.z.topicsErr = errors.New("down")
	hh.z.mu.Unlock()
	res = hh.callBranch(t, origin.ID, zulipmcp.BranchTask{FromMsg: empty, Seed: "x"})
	if !strings.Contains(res[0].Error, "could not check which topics") {
		t.Fatalf("result = %+v", res[0])
	}

	hh.z.failHistory(errors.New("down"))
	res = hh.callBranch(t, origin.ID, zulipmcp.BranchTask{Seed: "x"})
	if !strings.Contains(res[0].Error, "could not read this topic") {
		t.Fatalf("result = %+v", res[0])
	}
}

// TestBranchToolWholeCallRefusals: a caller that does not resolve, or
// has no channel, gets nothing created.
func TestBranchToolWholeCallRefusals(t *testing.T) {
	hh, _ := toolHarness(t)
	if _, err := hh.h.BranchTasks("cdeadbeef", []zulipmcp.BranchTask{{Seed: "x"}}); err == nil {
		t.Fatal("an unknown conversation must be refused")
	}
	dm, err := hh.j.Ensure(journal.DM([]int64{humanID, botID}))
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	old, _ := hh.j.Lookup(journal.Channel(4, "planning"))
	if _, _, _, err := hh.j.Retire(journal.Channel(4, "planning")); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if _, err := hh.h.BranchTasks(old.ID, []zulipmcp.BranchTask{{Seed: "x"}}); err == nil {
		t.Fatal("a retired conversation must not branch")
	}
	if _, err := hh.h.BranchTasks(dm.ID, []zulipmcp.BranchTask{{Seed: "x"}}); err == nil || !strings.Contains(err.Error(), "direct message") {
		t.Fatalf("err = %v", err)
	}
}

// TestBranchToolNestedNotifiesTheRoot: a child that branches again is
// announced in its own topic AND in the root of the tree.
func TestBranchToolNestedNotifiesTheRoot(t *testing.T) {
	hh, origin := toolHarness(t)
	id := hh.plant("planning", "research the cache layer", humanID, "Kfet")
	res := hh.callBranch(t, origin.ID, zulipmcp.BranchTask{FromMsg: id})
	child := res[0]

	sub := hh.plant(child.Topic, "measure eviction", humanID, "Kfet")
	res = hh.callBranch(t, child.ConvID, zulipmcp.BranchTask{FromMsg: sub})
	if res[0].Error != "" {
		t.Fatalf("nested branch = %+v", res[0])
	}
	grand, _ := hh.j.LookupID(res[0].ConvID)
	if grand.Parent.Key.Topic != child.Topic {
		t.Fatalf("grandchild origin = %+v, want its direct caller only", grand.Parent)
	}
	if got := hh.postedIn(child.Topic); !strings.Contains(got[len(got)-1], "#**fleet>measure eviction**") {
		t.Fatalf("child topic = %q", got)
	}
	want := fmt.Sprintf("🌿 %s branched → #**fleet>measure eviction**", child.Link)
	root := hh.postedIn("planning")
	if root[len(root)-1] != want {
		t.Fatalf("root = %q, want %q", root[len(root)-1], want)
	}
}

// TestBranchToolNotificationFailuresAreLogged: the branches exist, so a
// notification that cannot be posted is logged and nothing else.
func TestBranchToolNotificationFailuresAreLogged(t *testing.T) {
	hh, origin := toolHarness(t)
	id := hh.plant("planning", "research the cache layer", humanID, "Kfet")
	child := hh.callBranch(t, origin.ID, zulipmcp.BranchTask{FromMsg: id})[0]

	hh.z.sendHook = func(content string) error {
		if strings.HasPrefix(content, "🌿 ") && !strings.HasPrefix(content, "🌱") {
			return errors.New("nope")
		}
		return nil
	}
	sub := hh.plant(child.Topic, "measure eviction", humanID, "Kfet")
	res := hh.callBranch(t, child.ConvID, zulipmcp.BranchTask{FromMsg: sub})
	if res[0].Error != "" {
		t.Fatalf("result = %+v", res[0])
	}
	if !hh.logged("could not say so there") || !hh.logged("could not say so in the root") {
		t.Fatalf("logs = %v", hh.logs)
	}
}

func TestBranchGoalIsOneBoundedLine(t *testing.T) {
	if got := branchGoal("\n  first line \nsecond"); got != "first line" {
		t.Fatalf("goal = %q", got)
	}
	if got := branchGoal(strings.Repeat("é", branchGoalRunes+5)); got != strings.Repeat("é", branchGoalRunes)+"…" {
		t.Fatalf("goal = %q", got)
	}
}

// TestBranchToolRootMayBeAHumanOnlyTopic: a topic the relay was never
// engaged in, but was branched out of, is the root of its tree.
func TestBranchToolRootMayBeAHumanOnlyTopic(t *testing.T) {
	hh, _ := toolHarness(t)
	point := hh.plant("humans only", "spin this out", humanID, "Kfet")
	child, err := hh.j.Branch(journal.Channel(4, "spun"), journal.Parent{Key: journal.Channel(4, "humans only"), MessageID: point})
	if err != nil {
		t.Fatalf("Branch: %v", err)
	}
	sub := hh.plant("spun", "go deeper", humanID, "Kfet")
	res := hh.callBranch(t, child.ID, zulipmcp.BranchTask{FromMsg: sub})
	if res[0].Error != "" {
		t.Fatalf("result = %+v", res[0])
	}
	root := hh.postedIn("humans only")
	if len(root) != 1 || !strings.HasPrefix(root[0], "🌿 #**fleet>spun** branched → ") {
		t.Fatalf("root = %q", root)
	}
}
