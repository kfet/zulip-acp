package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kfet/acp-kit/command"
	"github.com/kfet/acp-kit/relaytool"
	"github.com/kfet/zulip-acp/internal/channels"
	"github.com/kfet/zulip-acp/internal/journal"
	"github.com/kfet/zulip-acp/internal/zulipmcp"
	"github.com/kfet/zulip-acp/internal/zulipproto"
)

// --- parsing -------------------------------------------------------------

// TestIsBranchCommand: only the verb counts, and a bare `!branch` is
// still recognised — it is refused with an explanation, not answered
// with "unknown command".
func TestIsBranchCommand(t *testing.T) {
	for _, tc := range []struct {
		in   string
		arg  string
		want bool
	}{
		{"!branch spin this out", "spin this out", true},
		{"!BRANCH shouting", "shouting", true},
		{"  !branch  padded  ", " padded", true},
		{"!branch", "", true},
		{"/branch slash form", "slash form", true},
		{"!branching out", "", false},
		{"!archive", "", false},
		{"branch without a sigil", "", false},
	} {
		arg, ok := isBranchCommand(tc.in)
		if ok != tc.want || (ok && arg != tc.arg) {
			t.Fatalf("isBranchCommand(%q) = %q, %v; want %q, %v", tc.in, arg, ok, tc.arg, tc.want)
		}
	}
}

// TestParseBranch covers the whole argument grammar: with and without
// a channel prefix, and every way it can be unusable.
func TestParseBranch(t *testing.T) {
	for _, tc := range []struct {
		name    string
		in      string
		channel string
		text    string
		errSub  string
	}{
		{name: "no channel", in: "rework the splitter", text: "rework the splitter"},
		{name: "channel prefix", in: "#**design** rework the splitter", channel: "design", text: "rework the splitter"},
		{name: "channel with spaces", in: "#**ask fir**   go on", channel: "ask fir", text: "go on"},
		{name: "multi-line text", in: "first line\nsecond line", text: "first line\nsecond line"},
		{name: "hash mid-text is not a channel", in: "fix #42 please", text: "fix #42 please"},
		{name: "empty", in: "", errSub: "say what the new topic is about"},
		{name: "whitespace only", in: "   \n  ", errSub: "say what the new topic is about"},
		{name: "channel but no text", in: "#**design**   ", errSub: "say what the new topic is about"},
		{name: "unclosed mention", in: "#**design go on", errSub: "not closed"},
		{name: "empty mention", in: "#**** go on", errSub: "names no channel"},
		{name: "topic mention", in: "#**design>notes** go on", errSub: "names a topic"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, err := parseBranch(tc.in)
			if tc.errSub != "" {
				if err == nil || !strings.Contains(err.Error(), tc.errSub) {
					t.Fatalf("parseBranch(%q) err = %v, want one mentioning %q", tc.in, err, tc.errSub)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseBranch(%q): %v", tc.in, err)
			}
			if req.Channel != tc.channel || req.Text != tc.text {
				t.Fatalf("parseBranch(%q) = %+v, want channel %q text %q", tc.in, req, tc.channel, tc.text)
			}
		})
	}
}

// TestSuffixTopic: a collision suffix must never push the name past
// Zulip's MAX_TOPIC_LENGTH, which the server enforces by SILENT
// truncation — the suffix would be the half that got cut.
func TestSuffixTopic(t *testing.T) {
	if got := suffixTopic("notes", 2); got != "notes (2)" {
		t.Fatalf("suffixTopic = %q", got)
	}
	long := strings.Repeat("é", 60)
	got := suffixTopic(long, 12)
	if n := len([]rune(got)); n > 60 {
		t.Fatalf("suffixTopic = %q (%d runes), over MAX_TOPIC_LENGTH", got, n)
	}
	if !strings.HasSuffix(got, " (12)") {
		t.Fatalf("suffixTopic = %q", got)
	}
	// A base that ends in a space must not yield "name  (2)".
	if got := suffixTopic(strings.Repeat("a", 55)+" xxxx", 3); strings.Contains(got, "  (3)") {
		t.Fatalf("suffixTopic = %q", got)
	}
}

// --- harness -------------------------------------------------------------

// branchHarness serves two channels, so the cross-channel form has
// somewhere to go, and enables the loopback so the branch prompt
// carries its relay note.
func branchHarness(t *testing.T) *harness {
	t.Helper()
	agent := newAgent("done")
	broker := command.New(agent)
	var h *Handler
	tools, err := relaytool.New(relaytool.Config{
		Broker:    broker,
		ConvToken: func(k string) (string, bool) { return h.ConvToken(k) },
	})
	if err != nil {
		t.Fatalf("relaytool.New: %v", err)
	}
	hh := newHarness(t, agent, func(c *Config) {
		c.Commands = broker
		c.Loopback = tools
		c.Channels = channels.New(channels.Config{Explicit: map[int64]string{4: "fleet", 5: "design"}})
	})
	h = hh.h
	return hh
}

// branch delivers a `!branch` in a topic and waits for the resulting
// turn to finish.
//
// The triggering message is registered with the fake server too:
// ConvOrigin resolves the origin's CURRENT location by reading the
// branch-point message back, so a branch nobody can read is a branch
// with no origin.
func (hh *harness) branch(t *testing.T, topic, content string) {
	t.Helper()
	hh.z.mu.Lock()
	hh.z.messages[1] = zulipproto.Message{
		ID: 1, SenderID: humanID, SenderName: "Kfet", Content: mention(content),
		StreamID: 4, Topic: topic, Type: zulipproto.MessageTypeStream,
	}
	hh.z.mu.Unlock()
	hh.deliver(t, topic, mention(content))
}

// topicOf returns the topic a stored message was posted to.
func (hh *harness) topicOf(id int64) string {
	hh.z.mu.Lock()
	defer hh.z.mu.Unlock()
	return hh.z.topics[id]
}

// --- the happy path ------------------------------------------------------

// TestBranchOpensATopicAndRecordsItsOrigin is the whole gesture in one
// test: a topic is created, named from the text, seeded with a message
// mentioning the person who branched, pointed at from the origin, and
// the new conversation knows where it came from.
func TestBranchOpensATopicAndRecordsItsOrigin(t *testing.T) {
	hh := branchHarness(t)
	hh.branch(t, "planning", "!branch rework the rollover splitter\nit keeps sealing early")

	conv, ok := hh.j.Lookup(journal.Channel(4, "rework the rollover splitter"))
	if !ok {
		t.Fatalf("no conversation in the branched topic; journal = %+v", hh.j.Convs())
	}
	if conv.Parent == nil {
		t.Fatal("the branched conversation has no origin")
	}
	if conv.Parent.Key.Topic != "planning" || conv.Parent.Key.StreamID != 4 {
		t.Fatalf("origin = %+v", *conv.Parent)
	}
	if conv.Parent.MessageID == 0 {
		t.Fatal("the origin must carry the bare id of the !branch message")
	}

	var seed, pointer string
	for _, id := range hh.z.order {
		switch hh.topicOf(id) {
		case "rework the rollover splitter":
			if seed == "" {
				seed = hh.z.body(id)
			}
		case "planning":
			pointer = hh.z.body(id)
		}
	}
	if !strings.Contains(seed, "@**Kfet|8**") {
		t.Fatalf("the seed message does not mention the branching user, so a phone never surfaces it: %q", seed)
	}
	if !strings.Contains(pointer, "branched → #**fleet>rework the rollover splitter**") {
		t.Fatalf("origin pointer = %q", pointer)
	}
	// The origin agent must never see the command.
	for _, p := range hh.a.prompted() {
		if strings.Contains(p, "!branch") {
			t.Fatalf("the origin agent saw the command: %q", p)
		}
	}
}

// TestBranchPromptCarriesTheBackLink: the new session's first turn
// states where it came from, in clickable form, and tells the agent it
// may PULL that context rather than being handed a summary of it.
func TestBranchPromptCarriesTheBackLink(t *testing.T) {
	hh := branchHarness(t)
	hh.branch(t, "planning", "!branch rework the splitter")
	prompts := hh.a.prompted()
	if len(prompts) != 1 {
		t.Fatalf("prompts = %+v", prompts)
	}
	p := prompts[0]
	if !strings.Contains(p, "[Kfet] [branched from #**fleet>planning@") {
		t.Fatalf("prompt = %q", p)
	}
	if !strings.Contains(p, "rework the splitter") {
		t.Fatalf("the user's text is missing: %q", p)
	}
	if !strings.Contains(p, "origin=true") {
		t.Fatalf("the agent was not told how to read the origin: %q", p)
	}
	if strings.Contains(p, "!branch") {
		t.Fatalf("the command leaked into the prompt: %q", p)
	}
}

// TestBranchWithNoLoopbackSaysNothingAboutTools: without the relay MCP
// surface there is no `history` tool, and promising one would send the
// agent looking for something that does not exist.
func TestBranchWithNoLoopbackSaysNothingAboutTools(t *testing.T) {
	hh := cmdHarness(t, newAgent("done"), nil)
	hh.branch(t, "planning", "!branch rework the splitter")
	p := hh.a.prompted()
	if len(p) != 1 || strings.Contains(p[0], "origin=true") {
		t.Fatalf("prompt = %+v", p)
	}
	if !strings.Contains(p[0], "[branched from") {
		t.Fatalf("the back-link header is not conditional on the loopback: %q", p[0])
	}
}

// TestBranchIntoANamedChannel: the destination is the one the user
// named, not the one they typed in.
func TestBranchIntoANamedChannel(t *testing.T) {
	hh := branchHarness(t)
	hh.branch(t, "planning", "!branch #**design** rework the splitter")
	if _, ok := hh.j.Lookup(journal.Channel(5, "rework the splitter")); !ok {
		t.Fatalf("nothing in #design; journal = %+v", hh.j.Convs())
	}
	if _, ok := hh.j.Lookup(journal.Channel(4, "rework the splitter")); ok {
		t.Fatal("the branch also landed in the origin channel")
	}
}

// TestBranchChannelNameIsCaseInsensitive: Zulip channel names are
// unique case-insensitively, so a human should not have to match the
// realm's capitalisation.
func TestBranchChannelNameIsCaseInsensitive(t *testing.T) {
	hh := branchHarness(t)
	hh.branch(t, "planning", "!branch #**DESIGN** rework the splitter")
	if _, ok := hh.j.Lookup(journal.Channel(5, "rework the splitter")); !ok {
		t.Fatalf("case-insensitive channel lookup failed; journal = %+v", hh.j.Convs())
	}
}

// --- refusals ------------------------------------------------------------

// TestBranchRefusals: every way the command can be refused answers in
// the origin topic, creates nothing, and starts no turn.
func TestBranchRefusals(t *testing.T) {
	for _, tc := range []struct {
		name   string
		text   string
		tune   func(*harness)
		errSub string
	}{
		{name: "no text", text: "!branch", errSub: "say what the new topic is about"},
		{name: "unknown channel", text: "!branch #**nowhere** go", errSub: "do not serve a channel called"},
		{
			name:   "topics listing failed",
			text:   "!branch go on then",
			tune:   func(hh *harness) { hh.z.topicsErr = errors.New("zulip is down") },
			errSub: "could not check which topics",
		},
		{
			name:   "the opening message failed",
			text:   "!branch go on then",
			tune:   func(hh *harness) { hh.z.sendHook = func(c string) error { return errors.New("nope") } },
			errSub: "could not open a topic",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hh := branchHarness(t)
			if tc.tune != nil {
				tc.tune(hh)
			}
			hh.branch(t, "planning", tc.text)
			if n := len(hh.a.prompted()); n != 0 {
				t.Fatalf("a refused branch still burned %d turn(s)", n)
			}
			var said string
			for _, id := range hh.z.order {
				said += hh.z.body(id)
			}
			if tc.name == "the opening message failed" {
				// Every post is refused, so the answer is only in the log.
				if !hh.logged("nothing was created") {
					t.Fatalf("logs = %v", hh.logs)
				}
				return
			}
			if !strings.Contains(said, tc.errSub) {
				t.Fatalf("reply = %q, want one mentioning %q", said, tc.errSub)
			}
			for _, c := range hh.j.Convs() {
				if c.Key.Topic != "planning" {
					t.Fatalf("a refused branch created %+v", c)
				}
			}
		})
	}
}

// TestBranchFromADMNeedsAChannel: a DM is in no channel, and the relay
// must not GUESS where to publish the contents of a private
// conversation.
func TestBranchFromADMNeedsAChannel(t *testing.T) {
	hh := cmdHarness(t, newAgent("done"), func(c *Config) {
		c.DMs = true
		c.Channels = channels.New(channels.Config{Explicit: map[int64]string{4: "fleet", 5: "design"}})
	})
	hh.deliverDM(t, humanID, "!branch spin this out", humanID, botID)
	if !strings.Contains(hh.z.lastBody(), "Name one:") {
		t.Fatalf("reply = %q", hh.z.lastBody())
	}
	if len(hh.a.prompted()) != 0 {
		t.Fatal("a refused branch burned a turn")
	}

	// Named explicitly, it works — and the origin is the DM.
	hh.deliverDM(t, humanID, "!branch #**design** spin this out", humanID, botID)
	conv, ok := hh.j.Lookup(journal.Channel(5, "spin this out"))
	if !ok {
		t.Fatalf("journal = %+v", hh.j.Convs())
	}
	if conv.Parent == nil || !conv.Parent.Key.IsDM() {
		t.Fatalf("origin = %+v, want the DM it was branched from", conv.Parent)
	}
	if p := hh.a.prompted(); len(p) != 1 || !strings.Contains(p[0], "[branched from a direct message]") {
		t.Fatalf("prompt = %+v", p)
	}
}

// TestBranchRefusesAnUnservedOriginChannel drives the narrow window
// where a channel leaves the served set between the message arriving
// and the command being dispatched.
func TestBranchRefusesAnUnservedOriginChannel(t *testing.T) {
	hh := branchHarness(t)
	_, _, err := hh.h.branchDestination(journal.Channel(77, "gone"), "")
	if err == nil || !strings.Contains(err.Error(), "no longer serve this channel") {
		t.Fatalf("err = %v", err)
	}
}

// --- collisions ----------------------------------------------------------

// TestBranchAvoidsAnExistingTopic: appending into a live topic would
// drop two conversations into one agent session, or worse, into a
// human's thread.
func TestBranchAvoidsAnExistingTopic(t *testing.T) {
	hh := branchHarness(t)
	hh.z.addTopic(4, "rework the splitter")
	hh.branch(t, "planning", "!branch rework the splitter")
	if _, ok := hh.j.Lookup(journal.Channel(4, "rework the splitter (2)")); !ok {
		t.Fatalf("journal = %+v", hh.j.Convs())
	}
}

// TestBranchTopicCollisionIsCaseInsensitive: Zulip folds case when it
// compares topic names, so "Rework The Splitter" IS the same topic.
func TestBranchTopicCollisionIsCaseInsensitive(t *testing.T) {
	hh := branchHarness(t)
	hh.z.addTopic(4, "Rework The Splitter", "rework the splitter (2)")
	hh.branch(t, "planning", "!branch rework the splitter")
	if _, ok := hh.j.Lookup(journal.Channel(4, "rework the splitter (3)")); !ok {
		t.Fatalf("journal = %+v", hh.j.Convs())
	}
}

// TestBranchGivesUpOnAWallOfCollisions: better an error the user can
// act on than a topic called "notes (33)".
func TestBranchGivesUpOnAWallOfCollisions(t *testing.T) {
	hh := branchHarness(t)
	hh.z.addTopic(4, "notes")
	for n := 2; n <= branchMaxSuffix; n++ {
		hh.z.addTopic(4, fmt.Sprintf("notes (%d)", n))
	}
	hh.branch(t, "planning", "!branch notes")
	if !strings.Contains(hh.z.lastBody(), "topics called") {
		t.Fatalf("reply = %q", hh.z.lastBody())
	}
}

// --- degradation ---------------------------------------------------------

// TestBranchRefusesToLandOnItself: `!branch planning`, typed in the
// topic "planning", generates exactly that title. Posting the opening
// message into the conversation being spun out of is not a branch — it
// is the relay talking to itself.
func TestBranchRefusesToLandOnItself(t *testing.T) {
	hh := branchHarness(t)
	hh.branch(t, "planning", "!branch Planning")
	if !strings.Contains(hh.z.lastBody(), "branch this topic into itself") {
		t.Fatalf("reply = %q", hh.z.lastBody())
	}
	if n := len(hh.a.prompted()); n != 0 {
		t.Fatalf("a refused branch still burned %d turn(s)", n)
	}
}

// TestBranchSurvivesAFailedPointerMessage: the branch has already
// happened, so a pointer that cannot be posted is logged and the turn
// runs anyway. Losing the pointer must not lose the branch.
func TestBranchSurvivesAFailedPointerMessage(t *testing.T) {
	hh := branchHarness(t)
	hh.z.sendHook = func(content string) error {
		if strings.HasPrefix(content, "branched → ") {
			return errors.New("nope")
		}
		return nil
	}
	hh.branch(t, "planning", "!branch rework the splitter")
	if !hh.logged("could not say so in") {
		t.Fatalf("logs = %v", hh.logs)
	}
	if _, ok := hh.j.Lookup(journal.Channel(4, "rework the splitter")); !ok {
		t.Fatal("a failed pointer message cost the branch")
	}
}

// TestBranchReportsAFailedConversationAllocation: the topic exists, so
// the user is told to send a message in it rather than left guessing.
func TestBranchReportsAFailedConversationAllocation(t *testing.T) {
	hh := branchHarness(t)
	hh.z.sendHook = func(content string) error {
		if strings.Contains(content, "branched this out of") {
			hh.breakJournal(t)
		}
		return nil
	}
	hh.branch(t, "planning", "!branch rework the splitter")
	if !hh.logged("branching into") {
		t.Fatalf("logs = %v", hh.logs)
	}
	var said string
	for _, id := range hh.z.order {
		said += hh.z.body(id)
	}
	if !strings.Contains(said, "Send a message in it to try again") {
		t.Fatalf("reply = %q", said)
	}
	if n := len(hh.a.prompted()); n != 0 {
		t.Fatalf("a branch with no conversation still ran %d turn(s)", n)
	}
}

// --- rename anchor -------------------------------------------------------

// TestBranchAnchorsTheRenameInTheNewTopic: the `!branch` message is in
// the ORIGIN topic, so anchoring on it would make every rename the
// branched agent asks for fail as "no longer in it", and the topic
// would keep its auto-generated placeholder name for good.
func TestBranchAnchorsTheRenameInTheNewTopic(t *testing.T) {
	hh := branchHarness(t)
	// Capture the anchor while the turn is live: endTurn clears it.
	var anchorTopic string
	hh.a.during = func() {
		conv, ok := hh.j.Lookup(journal.Channel(4, "rework the splitter"))
		if !ok {
			return
		}
		hh.h.inflightMu.Lock()
		if e := hh.h.inflight[conv.ID]; e != nil && e.rename != nil {
			anchorTopic = hh.topicOf(e.rename.anchor)
		}
		hh.h.inflightMu.Unlock()
	}
	hh.branch(t, "planning", "!branch rework the splitter")
	if anchorTopic != "rework the splitter" {
		t.Fatalf("rename anchor is in topic %q, want the branched topic", anchorTopic)
	}
}

// --- the seed message ----------------------------------------------------

func TestBranchSeedNamesAnAnonymousSender(t *testing.T) {
	got := branchSeed(&zulipproto.Message{}, "#fleet > \"planning\"")
	if !strings.Contains(got, "someone") {
		t.Fatalf("seed = %q", got)
	}
}

// TestBranchPromptNamesAnUnknownOriginChannel: the origin channel can
// leave the served set between the branch and the prompt being built.
// A numeric id is worse than a name and better than a blank.
func TestBranchPromptNamesAnUnknownOriginChannel(t *testing.T) {
	hh := branchHarness(t)
	p := hh.h.branchPrompt(
		&zulipproto.Message{SenderName: "Ada"},
		journal.Parent{Key: journal.Channel(77, "gone"), MessageID: 3},
		"go on", "a topic",
	)
	if !strings.Contains(p, "#**77>gone@3**") {
		t.Fatalf("prompt = %q", p)
	}
}

// TestBranchTimeoutIsBounded pins that the branch does not inherit the
// event loop's context: it posts twice and starts a turn, and a wedged
// request must not hold up intake.
func TestBranchTimeoutIsBounded(t *testing.T) {
	if branchTimeout <= 0 || branchTimeout > 5*time.Minute {
		t.Fatalf("branchTimeout = %v", branchTimeout)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	hh := branchHarness(t)
	// A cancelled caller context must not stop the branch.
	hh.h.branchCommand(ctx, &zulipproto.Message{ID: 1, SenderID: humanID, SenderName: "Ada", StreamID: 4, Topic: "planning", Type: "stream"},
		journal.Channel(4, "planning"), "go on then")
	wctx, wcancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer wcancel()
	if err := hh.h.WaitIdle(wctx); err != nil {
		t.Fatalf("turn did not finish: %v", err)
	}
	if _, ok := hh.j.Lookup(journal.Channel(4, "go on then")); !ok {
		t.Fatalf("journal = %+v", hh.j.Convs())
	}
}

// --- end to end ----------------------------------------------------------

// branchMessages is the Zulip read side of the loopback: it records
// the narrow every `history` call asked for.
type branchMessages struct {
	narrow   []zulipproto.NarrowTerm
	beforeID int64
}

func (b *branchMessages) Messages(_ context.Context, narrow []zulipproto.NarrowTerm, _ int, beforeID int64) ([]zulipproto.Message, error) {
	b.narrow, b.beforeID = narrow, beforeID
	return []zulipproto.Message{{ID: 1, SenderName: "Kfet", Content: "how it started"}}, nil
}

// TestBranchedSessionReadsItsParentAndNothingElse is the whole
// permission model, end to end over the real tool set: `!branch`
// records the origin, the branched session's `history(origin: true)`
// narrows to exactly that topic, clamped at the branch point — and a
// conversation that was never branched can reach nothing at all.
func TestBranchedSessionReadsItsParentAndNothingElse(t *testing.T) {
	hh := branchHarness(t)
	hh.branch(t, "planning", "!branch rework the splitter")

	branched, ok := hh.j.Lookup(journal.Channel(4, "rework the splitter"))
	if !ok {
		t.Fatalf("journal = %+v", hh.j.Convs())
	}
	// An UNRELATED conversation, in the same channel, branched from
	// nothing.
	unrelated, err := hh.j.Ensure(journal.Channel(4, "someone else's topic"))
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	reader := &branchMessages{}
	tools, err := zulipmcp.NewTools(zulipmcp.Config{
		Client:  reader,
		ConvKey: func(k string) (journal.Key, bool) { return hh.h.ConvKey(k) },
		Origin:  func(k string) (journal.Parent, bool) { return hh.h.ConvOrigin(k) },
		Rename:  func(k journal.Key, title string) (string, error) { return hh.h.RenameTopic(k, title) },
		Logf:    func(string, ...any) {},
	})
	if err != nil {
		t.Fatalf("NewTools: %v", err)
	}
	var history func(string, json.RawMessage) (string, error)
	for _, x := range tools.Tools() {
		if x.Name == zulipmcp.ToolHistory {
			history = x.Handler
		}
	}
	if history == nil {
		t.Fatal("no history tool")
	}

	// The branched session reads its parent.
	if _, err := history(branched.ID, json.RawMessage(`{"origin":true}`)); err != nil {
		t.Fatalf("the branched session could not read its origin: %v", err)
	}
	if len(reader.narrow) != 2 || reader.narrow[1].Operand != "planning" {
		t.Fatalf("narrow = %+v, want the origin topic", reader.narrow)
	}
	if reader.beforeID != branched.Parent.MessageID+1 {
		t.Fatalf("beforeID = %d, want the branch point + 1", reader.beforeID)
	}

	// The unrelated session reaches nothing, and never touches Zulip.
	reader.narrow = nil
	if _, err := history(unrelated.ID, json.RawMessage(`{"origin":true}`)); err == nil {
		t.Fatal("a conversation that was never branched must have no origin to read")
	}
	if reader.narrow != nil {
		t.Fatal("the refused call still reached Zulip")
	}

	// And a stranger reaches nothing either.
	if _, err := history("cdeadbeef", json.RawMessage(`{"origin":true}`)); err == nil {
		t.Fatal("an unknown conversation must be refused")
	}
}

// TestConvOriginRefusesAStranger: the resolver behind
// `history(origin: true)` says no to a conv-id the journal never
// minted, and to one that was never branched.
func TestConvOriginRefusesAStranger(t *testing.T) {
	hh := branchHarness(t)
	if _, ok := hh.h.ConvOrigin("cdeadbeef"); ok {
		t.Fatal("an unknown conv-id resolved to an origin")
	}
	c, err := hh.j.Ensure(journal.Channel(4, "ordinary"))
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if _, ok := hh.h.ConvOrigin(c.ID); ok {
		t.Fatal("an unbranched conversation resolved to an origin")
	}
}

// TestConvOriginResolvesTheLocationFromTheMessage is the reason the
// journal stores a bare message id: a topic can be renamed, or moved
// to another channel by `!archive` or by any human. The stored key
// rots, and the failure is not a mere miss — a LATER topic of the same
// name would be read instead, which is the permission model silently
// pointing somewhere nobody granted.
func TestConvOriginResolvesTheLocationFromTheMessage(t *testing.T) {
	hh := branchHarness(t)
	hh.branch(t, "planning", "!branch rework the splitter")
	branched, ok := hh.j.Lookup(journal.Channel(4, "rework the splitter"))
	if !ok {
		t.Fatalf("journal = %+v", hh.j.Convs())
	}

	// The origin topic is renamed. The journal's cached key still says
	// "planning"; the message says otherwise, and the message wins.
	if err := hh.z.MoveMessage(context.Background(), branched.Parent.MessageID, "renamed since", propagateAll); err != nil {
		t.Fatalf("MoveMessage: %v", err)
	}
	got, ok := hh.h.ConvOrigin(branched.ID)
	if !ok {
		t.Fatal("the origin became unreadable after an ordinary rename")
	}
	if got.Key.Topic != "renamed since" {
		t.Fatalf("origin topic = %q, want the message's current one", got.Key.Topic)
	}
	if got.MessageID != branched.Parent.MessageID {
		t.Fatalf("the clamp moved: %+v", got)
	}
}

// TestConvOriginRefusesWhatItCannotVouchFor: an origin whose branch
// point cannot be read, or whose topic has left the served set, is
// refused rather than fallen back on. Falling back to the stored key
// would re-open the exact hole resolving closes.
func TestConvOriginRefusesWhatItCannotVouchFor(t *testing.T) {
	for _, tc := range []struct {
		name string
		tune func(*harness, int64)
		log  string
	}{
		{
			name: "the branch point is gone",
			tune: func(hh *harness, _ int64) { hh.z.getErr = errors.New("no such message") },
			log:  "is unreadable",
		},
		{
			name: "the origin left the served set",
			tune: func(hh *harness, id int64) {
				hh.z.mu.Lock()
				m := hh.z.messages[id]
				m.StreamID = 77
				hh.z.messages[id] = m
				hh.z.mu.Unlock()
			},
			log: "no longer served",
		},
		{
			name: "the branch point is now a DM",
			tune: func(hh *harness, id int64) {
				hh.z.mu.Lock()
				m := hh.z.messages[id]
				m.Type, m.StreamID = zulipproto.MessageTypePrivate, 0
				hh.z.messages[id] = m
				hh.z.mu.Unlock()
			},
			log: "no longer a channel message",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hh := branchHarness(t)
			hh.branch(t, "planning", "!branch rework the splitter")
			branched, _ := hh.j.Lookup(journal.Channel(4, "rework the splitter"))
			tc.tune(hh, branched.Parent.MessageID)
			if _, ok := hh.h.ConvOrigin(branched.ID); ok {
				t.Fatal("an origin nobody can vouch for was resolved anyway")
			}
			if !hh.logged(tc.log) {
				t.Fatalf("logs = %v", hh.logs)
			}
		})
	}
}

// TestConvOriginOfADMCostsNoLookup: a DM's key is the participant set,
// which is fixed forever — there is nothing to resolve and nothing
// that could have rotted, so it must not spend a round-trip.
func TestConvOriginOfADMCostsNoLookup(t *testing.T) {
	hh := branchHarness(t)
	c, err := hh.j.Branch(journal.Channel(4, "spun out"), journal.Parent{Key: journal.DM([]int64{4, 9}), MessageID: 7})
	if err != nil {
		t.Fatalf("Branch: %v", err)
	}
	before := len(hh.z.gets)
	got, ok := hh.h.ConvOrigin(c.ID)
	if !ok || !got.Key.IsDM() {
		t.Fatalf("origin = %+v, %v", got, ok)
	}
	if len(hh.z.gets) != before {
		t.Fatal("resolving a DM origin cost a message read")
	}
}

// TestBranchAvoidsATopicOnlyTheJournalKnows: Zulip's listing is the
// authority on what a human would see, but a live conversation whose
// messages have all been deleted has left that listing while still
// holding an agent session. Branching into it would hand two
// conversations to one session.
func TestBranchAvoidsATopicOnlyTheJournalKnows(t *testing.T) {
	hh := branchHarness(t)
	if _, err := hh.j.Ensure(journal.Channel(4, "rework the splitter")); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	hh.branch(t, "planning", "!branch rework the splitter")
	if _, ok := hh.j.Lookup(journal.Channel(4, "rework the splitter (2)")); !ok {
		t.Fatalf("journal = %+v", hh.j.Convs())
	}
}

// TestBranchSeedDegradesWithoutASenderID: there is no such message on
// the wire, but mentioning user 0 would be worse than a bare name.
func TestBranchSeedDegradesWithoutASenderID(t *testing.T) {
	if got := branchSeed(nil, "#fleet > \"planning\""); strings.Contains(got, "@**") {
		t.Fatalf("seed = %q", got)
	}
}
