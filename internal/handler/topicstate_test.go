package handler

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kfet/zulip-acp/internal/channels"
	"github.com/kfet/zulip-acp/internal/journal"
)

// TestStateCommand: bare `!state` shows the state, `!state <s> [reason]`
// sets it as the user, a repeat changes nothing, and a bad word is
// refused with the usage.
func TestStateCommand(t *testing.T) {
	hh := cmdHarness(t, newAgent("ok"), nil)
	if got := hh.h.stateCommand(journal.Channel(4, "never"), ""); !strings.Contains(got, "no conversation yet") {
		t.Fatalf("unknown topic = %q", got)
	}
	hh.deliver(t, "work", mention("hello"))
	for _, step := range []struct{ in, want string }{
		{"!state", "No state yet. Set one: `!state <needs_you|fyi|waiting|blocked|done> [reason]`."},
		{"!state maybe", "❌ `maybe` is not a state. Use `!state <needs_you|fyi|waiting|blocked|done> [reason]`."},
		{"!state FYI read the summary", "State: **fyi** — read the summary (by user, 0m ago)"},
		{"!state fyi read the summary", "State is already **fyi**."},
		{"!STATE", "State: **fyi** — read the summary (by user, 0m ago)"},
		{"!state\tdone\nall merged", "State: **done** — all merged (by user, 0m ago)"},
		{"!stateless", "Unknown command"},
	} {
		hh.z.reset()
		hh.deliver(t, "work", step.in)
		if got := strings.Join(hh.z.stored(), "\n"); got != step.want && !(step.want == "Unknown command" && strings.Contains(got, step.want)) {
			t.Fatalf("%s = %q, want %q", step.in, got, step.want)
		}
	}
	c, _ := hh.j.Lookup(journal.Channel(4, "work"))
	if c.State == nil || c.State.SetBy != journal.SetByUser || c.State.State != journal.StateDone {
		t.Fatalf("state = %+v", c.State)
	}
	for _, p := range hh.a.prompted() {
		if strings.Contains(p, "!state") {
			t.Fatalf("the agent saw the command: %q", p)
		}
	}
}

// TestStateCommandReportsAWriteFailure: a journal that cannot be
// written is said, not hidden.
func TestStateCommandReportsAWriteFailure(t *testing.T) {
	hh := cmdHarness(t, newAgent("ok"), nil)
	hh.deliver(t, "work", mention("hello"))
	if err := os.Chmod(hh.jdir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(hh.jdir, 0o700) })
	if got := hh.h.stateCommand(journal.Channel(4, "work"), "done"); !strings.HasPrefix(got, "❌ ") {
		t.Fatalf("reply = %q", got)
	}
}

func TestStateHelpIsAdvertised(t *testing.T) {
	hh := cmdHarness(t, newAgent("ok"), nil)
	hh.deliver(t, "work", mention("hello"))
	hh.z.reset()
	hh.deliver(t, "work", "!help")
	got := hh.only(t)
	if !strings.Contains(got, "`!state [needs_you|fyi|waiting|blocked|done] [reason]`") || !strings.Contains(got, "`!inbox`") {
		t.Fatalf("help = %q", got)
	}
}

func TestAge(t *testing.T) {
	now := time.UnixMilli(1_000_000_000_000)
	for ms, want := range map[int64]string{
		0:                                      "?",
		now.UnixMilli() + 5000:                 "0m",
		now.Add(-59 * time.Minute).UnixMilli(): "59m",
		now.Add(-47 * time.Hour).UnixMilli():   "47h",
		now.Add(-72 * time.Hour).UnixMilli():   "3d",
	} {
		if got := age(now, ms); got != want {
			t.Errorf("age(%d) = %q, want %q", ms, got, want)
		}
	}
}

// TestInbox: open topics only, grouped needs_you, blocked, fyi,
// waiting, then no state; done, retired, DM and unserved topics are
// left out; a state with three turns since is flagged.
func TestInbox(t *testing.T) {
	hh := cmdHarness(t, newAgent("ok"), nil)
	j := hh.j
	set := func(topic, state, reason string, turns int) journal.Conv {
		c, _ := j.Ensure(journal.Channel(4, topic))
		if state != "" {
			if _, _, err := j.SetState(c.ID, journal.TopicState{State: state, Reason: reason}); err != nil {
				t.Fatal(err)
			}
		}
		for range turns {
			_ = j.NoteActivity(c.ID, true)
		}
		return c
	}
	if got := hh.h.renderInbox(0); got != "📭 Inbox empty: no open topics." {
		t.Fatalf("empty = %q", got)
	}
	set("ask", journal.StateNeedsYou, "pick A or B", 3)
	set("fix", journal.StateBlocked, "token expired", 0)
	set("report", journal.StateFYI, "", 0)
	set("build", journal.StateWaiting, "CI", 0)
	set("chat", "", "", 0)
	set("over", journal.StateDone, "shipped", 0)
	old := set("replaced", journal.StateNeedsYou, "x", 0)
	_, _, _, _ = j.Retire(old.Key)
	_, _, _ = j.SetState(mustLookup(t, j, journal.Channel(4, "replaced")).ID, journal.TopicState{State: journal.StateDone})
	dm, _ := j.Ensure(journal.DM([]int64{1, 2}))
	_, _, _ = j.SetState(dm.ID, journal.TopicState{State: journal.StateNeedsYou})
	_, _ = j.Ensure(journal.Channel(99, "elsewhere"))

	got := hh.h.renderInbox(0)
	want := []string{
		"📥 5 open topics",
		"**❓ needs you** (1)",
		"- #**fleet>ask** · 0m · pick A or B · ⚠ state may be stale",
		"**⛔ blocked** (1)",
		"- #**fleet>fix** · 0m · token expired",
		"**📬 fyi** (1)",
		"- #**fleet>report** · 0m",
		"**⏳ waiting** (1)",
		"- #**fleet>build** · 0m · CI",
		"**· open (no state)** (1)",
		"- #**fleet>chat** · 0m",
	}
	last := -1
	for _, w := range want {
		i := strings.Index(got, w)
		if i <= last {
			t.Fatalf("inbox lacks %q in order:\n%s", w, got)
		}
		last = i
	}
	for _, no := range []string{"over", "replaced", "elsewhere", "DM"} {
		if strings.Contains(got, no) {
			t.Fatalf("inbox shows %q:\n%s", no, got)
		}
	}

	hh.z.reset()
	hh.deliver(t, "chat", mention("hi"))
	hh.z.reset()
	hh.deliver(t, "chat", "!inbox")
	if got := hh.only(t); !strings.HasPrefix(got, "📥 5 open topics in #**fleet**\n") {
		t.Fatalf("!inbox = %q", got)
	}
}

func mustLookup(t *testing.T, j *journal.Journal, k journal.Key) journal.Conv {
	t.Helper()
	c, ok := j.Lookup(k)
	if !ok {
		t.Fatalf("no conversation at %s", k.Label())
	}
	return c
}

// TestLastSeenFallsBack: a topic with no activity yet uses its state time, then its creation time.
func TestLastSeenFallsBack(t *testing.T) {
	c := journal.Conv{Created: 1}
	if lastSeen(c) != 1 {
		t.Fatal("created")
	}
	c.State = &journal.TopicState{SetAt: 2}
	if lastSeen(c) != 2 {
		t.Fatal("set_at")
	}
	c.LastActivity = 3
	if lastSeen(c) != 3 {
		t.Fatal("last activity")
	}
}

// TestInboxIsBounded: a long inbox stays under Zulip's message limit
// and says how many it left out.
func TestInboxIsBounded(t *testing.T) {
	hh := cmdHarness(t, newAgent("ok"), nil)
	for i := range 200 {
		c, _ := hh.j.Ensure(journal.Channel(4, fmt.Sprintf("%03d %s", i, strings.Repeat("x", 50))))
		_, _, _ = hh.j.SetState(c.ID, journal.TopicState{State: journal.StateFYI, Reason: strings.Repeat("r", 40)})
	}
	got := hh.h.renderInbox(0)
	if n := len([]rune(got)); n > inboxMaxRunes+50 {
		t.Fatalf("inbox has %d runes", n)
	}
	if !strings.Contains(got, " more.") {
		t.Fatalf("inbox does not say what it left out: %q", got[len(got)-80:])
	}
}

// TestMergeBack: a branch that delivers posts one line into its parent
// topic; a second delivered state does not, a new one after a
// non-delivered state does.
func TestMergeBack(t *testing.T) {
	hh := branchHarness(t)
	hh.branch(t, "planning", "!branch rework the splitter")
	child := mustLookup(t, hh.j, journal.Channel(4, "rework the splitter"))
	parentPosts := func() []string {
		var out []string
		for _, id := range hh.z.order {
			if hh.topicOf(id) == "planning" && strings.HasPrefix(hh.z.body(id), "⤷") {
				out = append(out, hh.z.body(id))
			}
		}
		return out
	}
	if _, err := hh.h.SetTopicState(child.ID, journal.StateFYI, "splitter fixed @**all**\nsecond line"); err != nil {
		t.Fatal(err)
	}
	// The reason as stored: one line, its mention silenced.
	want := "⤷ branch result: #**fleet>rework the splitter** — splitter fixed @_**all**"
	if got := parentPosts(); len(got) != 1 || got[0] != want {
		t.Fatalf("parent posts = %q", got)
	}
	_, _ = hh.h.SetTopicState(child.ID, journal.StateDone, "merged")
	if got := parentPosts(); len(got) != 1 {
		t.Fatalf("fyi → done posted again: %q", got)
	}
	_ = hh.h.stateCommand(child.Key, "needs_you")
	_ = hh.h.stateCommand(child.Key, "done")
	if got := parentPosts(); len(got) != 2 || got[1] != "⤷ branch result: #**fleet>rework the splitter**" {
		t.Fatalf("parent posts = %q", got)
	}
}

// TestMergeBackOnlyForAResolvableBranch: no parent, an unreadable
// branch point, or an unserved channel post nothing.
func TestMergeBackOnlyForAResolvableBranch(t *testing.T) {
	hh := branchHarness(t)
	plain, _ := hh.j.Ensure(journal.Channel(4, "plain"))
	hh.h.mergeBack(plain, "x")
	gone, _ := hh.j.Branch(journal.Channel(4, "orphan"), journal.Parent{Key: journal.Channel(4, "p"), MessageID: 999})
	hh.h.mergeBack(gone, "x")
	hh.z.mu.Lock()
	hh.z.messages[1] = *channelEvent(humanID, "planning", "x").Message
	hh.z.mu.Unlock()
	away := journal.Conv{ID: "x", Key: journal.Channel(77, "t"), Parent: &journal.Parent{Key: journal.Channel(4, "planning"), MessageID: 1}}
	hh.h.mergeBack(away, "x")
	// A failed post is logged; the state stays recorded.
	hh.z.sendErr = errors.New("down")
	child := journal.Conv{ID: "y", Key: journal.Channel(4, "t"), Parent: away.Parent}
	hh.h.mergeBack(child, "x")
	if !hh.logged("merge-back from y") {
		t.Fatalf("logs %v", hh.logs)
	}
	hh.z.sendErr = nil
	if got := hh.z.stored(); len(got) != 0 {
		t.Fatalf("posted %q", got)
	}
}

// TestInboxScope: a listing is a read, so it stays in its channel. A
// user sees the channel they ask in, an owner sees every channel, a
// DM non-owner is told where to ask, and the tool lists the caller's
// channel only.
func TestInboxScope(t *testing.T) {
	hh := cmdHarness(t, newAgent("ok"), func(c *Config) {
		c.Channels = channels.New(channels.Config{Explicit: map[int64]string{4: "fleet", 5: "private"}})
		c.OpsOwners = []int64{42}
	})
	here, _ := hh.j.Ensure(journal.Channel(4, "here"))
	_, _ = hh.j.Ensure(journal.Channel(5, "secret"))
	dm, _ := hh.j.Ensure(journal.DM([]int64{botID, humanID}))

	user := hh.h.inboxFor(here.Key, humanID)
	if !strings.Contains(user, "here") || strings.Contains(user, "secret") {
		t.Fatalf("user inbox = %q", user)
	}
	owner := hh.h.inboxFor(here.Key, 42)
	if !strings.HasPrefix(owner, "📥 2 open topics\n") || !strings.Contains(owner, "#**private>secret**") {
		t.Fatalf("owner inbox = %q", owner)
	}
	if got := hh.h.inboxFor(dm.Key, humanID); !strings.HasPrefix(got, "❌ a direct message is in no channel") {
		t.Fatalf("DM inbox = %q", got)
	}
	if got := hh.h.inboxFor(dm.Key, 42); !strings.Contains(got, "secret") {
		t.Fatalf("owner DM inbox = %q", got)
	}

	if got, err := hh.h.Inbox(here.ID); err != nil || strings.Contains(got, "secret") || !strings.Contains(got, "here") {
		t.Fatalf("tool = %q, %v", got, err)
	}
	if _, err := hh.h.Inbox(dm.ID); err != errInboxDM {
		t.Fatalf("tool in DM: %v", err)
	}
	if _, err := hh.h.Inbox("nope"); err == nil {
		t.Fatal("unknown caller must be refused")
	}
}

// TestActivityCountsCommandsAndPosts: a command in a known topic and an
// out-of-band post both count as activity, but not as a turn.
func TestActivityCountsCommandsAndPosts(t *testing.T) {
	hh := cmdHarness(t, newAgent("ok"), nil)
	cmd, _ := hh.j.Ensure(journal.Channel(4, "cmd"))
	hh.deliver(t, "cmd", "!state")
	if got := mustLookup(t, hh.j, cmd.Key); got.LastActivity == 0 || got.ActivitySince != 0 {
		t.Fatalf("after a command: %+v", got)
	}
	quiet, _ := hh.j.Ensure(journal.Channel(4, "quiet"))
	if err := hh.h.PostTo(quiet.Key.Token(), "progress"); err != nil {
		t.Fatal(err)
	}
	if got := mustLookup(t, hh.j, quiet.Key); got.LastActivity == 0 || got.ActivitySince != 0 {
		t.Fatalf("after a post: %+v", got)
	}
	hh.deliver(t, "quiet", mention("go"))
	if got := mustLookup(t, hh.j, quiet.Key); got.ActivitySince != 1 {
		t.Fatalf("after a turn: %+v", got)
	}
}
