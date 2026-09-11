package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
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
	return branchHarnessTuned(t, nil)
}

// branchHarnessTuned is branchHarness with a hook into the Config, for
// the reaction tests below — which need reactions on and the trigger
// seam wired.
func branchHarnessTuned(t *testing.T, tune func(*Config)) *harness {
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
		if tune != nil {
			tune(c)
		}
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
	p := hh.h.branchPrompt(branchPlan{
		Actor:  &zulipproto.Message{SenderName: "Ada"},
		Parent: journal.Parent{Key: journal.Channel(77, "gone"), MessageID: 3},
		Text:   "go on",
	}, "a topic")
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

// --- the :fork_and_knife: reaction ---------------------------------------

// branchReactHarness is branchHarness with reactions on and the
// reaction seam wired exactly the way main wires it — archive first,
// then branch — plus one engaged conversation in #fleet > "planning".
//
// The archive control is left unconfigured, so ArchiveReaction is
// inert and every tap below reaches BranchReaction. That is the
// production ordering, not a test convenience: the point of running
// both is that the two consumers do not shadow each other.
func branchReactHarness(t *testing.T) *harness {
	t.Helper()
	hh := branchHarnessTuned(t, func(c *Config) {
		c.Reactions = true
		c.AckEmoji = "eyes"
	})
	hh.h.cfg.ReactionTrigger = func(ctx context.Context, conv journal.Conv, ev zulipproto.Event, m *zulipproto.Message) bool {
		return hh.h.ArchiveReaction(ctx, conv, ev, m) || hh.h.BranchReaction(ctx, conv, ev, m)
	}
	hh.deliver(t, "planning", mention("hi"))
	return hh
}

// plant registers a message with the fake server without posting it,
// so a reaction on it resolves the way one on a human's message does:
// not in the relay's own index, so convForReaction reads it back and
// hands BranchReaction the body it needs.
func (hh *harness) plant(topic, content string, sender int64, name string) int64 {
	hh.z.mu.Lock()
	defer hh.z.mu.Unlock()
	hh.z.next++
	id := hh.z.next
	hh.z.messages[id] = zulipproto.Message{
		ID: id, SenderID: sender, SenderName: name, Content: content,
		StreamID: 4, Topic: topic, Type: zulipproto.MessageTypeStream,
	}
	return id
}

// fork feeds one :fork_and_knife: and waits for whatever turn it
// started. BranchReaction itself is synchronous on the event loop; only
// the branched turn is not.
func (hh *harness) fork(t *testing.T, user, msgID int64) {
	t.Helper()
	hh.h.Handle(context.Background(), reactionEvent(user, msgID, branchEmoji, zulipproto.ReactionAdd))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := hh.h.WaitIdle(ctx); err != nil {
		t.Fatalf("branched turn did not finish: %v", err)
	}
}

// inflightOf reads the turn currently claimed for a conversation. Used
// to prove, by pointer identity, that a branch did not supersede it.
func (hh *harness) inflightOf(convID string) *inflightEntry {
	hh.h.inflightMu.Lock()
	defer hh.h.inflightMu.Unlock()
	return hh.h.inflight[convID]
}

// TestBranchReactionSpinsTheMessageOut is the whole gesture: tapping
// :fork_and_knife: on a message opens a topic named after THAT
// message, seeded and prompted with its body, pointing back at the
// message it was spun out of.
func TestBranchReactionSpinsTheMessageOut(t *testing.T) {
	hh := branchReactHarness(t)
	origin, ok := hh.j.Lookup(journal.Channel(4, "planning"))
	if !ok {
		t.Fatal("no origin conversation")
	}
	// Sent by somebody else entirely: the branching user is the
	// REACTOR, and this is what tells the two apart.
	id := hh.plant("planning", "rework the rollover splitter\nit keeps sealing early", 99, "Grace Hopper")

	hh.fork(t, humanID, id)

	conv, ok := hh.j.Lookup(journal.Channel(4, "rework the rollover splitter"))
	if !ok {
		t.Fatalf("no branched topic; journal = %+v", hh.j.Convs())
	}
	if conv.Parent == nil {
		t.Fatal("the branched conversation has no origin")
	}
	// Anchored AND clamped at the reacted-to message, not at "now".
	if p := *conv.Parent; p.Key.StreamID != 4 || p.Key.Topic != "planning" || p.MessageID != id {
		t.Fatalf("origin = %+v, want the reacted-to message %d in #fleet > planning", p, id)
	}
	if conv.ID == origin.ID {
		t.Fatal("the branch reused the origin conversation")
	}

	// The seed mentions the reactor, never the message's author.
	var seed string
	for _, b := range hh.z.stored() {
		if strings.Contains(b, "branched this out of") {
			seed = b
		}
	}
	if !strings.Contains(seed, "@**Ada Lovelace|8**") {
		t.Fatalf("seed = %q, want an @-mention of the reactor", seed)
	}
	if strings.Contains(seed, "Grace Hopper") {
		t.Fatalf("seed = %q, want the REACTOR named, not the message's sender", seed)
	}

	// The pointer line goes into the origin topic, and it is the only
	// thing the origin conversation is told.
	var pointer bool
	for _, b := range hh.z.stored() {
		if strings.Contains(b, "branched → #**fleet>rework the rollover splitter**") {
			pointer = true
		}
	}
	if !pointer {
		t.Fatalf("no pointer line in the origin; posted %q", hh.z.stored())
	}

	// The message's body is the new session's first prompt, attributed
	// to the reactor and carrying the clamped back-link.
	prompt := hh.lastPrompt()
	if !strings.Contains(prompt, "[Ada Lovelace] [branched from #**fleet>planning@"+fmt.Sprint(id)+"**]") {
		t.Fatalf("prompt = %q", prompt)
	}
	if !strings.Contains(prompt, "it keeps sealing early") {
		t.Fatalf("prompt does not carry the reacted-to message: %q", prompt)
	}

	// The ack reaction lands on the message the user tapped — that is
	// where they are looking.
	added, _ := hh.z.reactions()
	if !slices.Contains(added, fmt.Sprintf("%d:eyes", id)) {
		t.Fatalf("ack reactions = %v, want one on message %d", added, id)
	}
	if !hh.logged(`branched #fleet > "planning" (:fork_and_knife: on message`) {
		t.Fatalf("logs = %v", hh.logs)
	}
}

// TestBranchReactionOnTheRelaysOwnMessage covers the other resolution
// tier: a reaction on a message the relay itself posted resolves out of
// its own index, so convForReaction hands over no message and the body
// has to be read back with one GET.
func TestBranchReactionOnTheRelaysOwnMessage(t *testing.T) {
	hh := branchReactHarness(t)
	own := hh.z.lastID()

	hh.fork(t, humanID, own)

	// The relay's answer was "done", so that is what the topic is
	// called and what the branched session is prompted with.
	conv, ok := hh.j.Lookup(journal.Channel(4, "done"))
	if !ok {
		t.Fatalf("no branched topic; journal = %+v", hh.j.Convs())
	}
	if conv.Parent == nil || conv.Parent.MessageID != own {
		t.Fatalf("origin = %+v, want message %d", conv.Parent, own)
	}
}

// TestBranchReactionGatesFallThrough: every case the gesture must NOT
// claim stays ordinary reaction signal and reaches the agent. That is
// the difference between "not a branch trigger" and "swallowed".
func TestBranchReactionGatesFallThrough(t *testing.T) {
	for name, ev := range map[string]func(own int64) zulipproto.Event{
		"another emoji": func(own int64) zulipproto.Event {
			return reactionEvent(humanID, own, "+1", zulipproto.ReactionAdd)
		},
		"un-reacting never un-branches": func(own int64) zulipproto.Event {
			return reactionEvent(humanID, own, branchEmoji, zulipproto.ReactionRemove)
		},
	} {
		t.Run(name, func(t *testing.T) {
			hh := branchReactHarness(t)
			before := len(hh.j.Convs())
			hh.react(t, ev(hh.z.lastID()))
			if got := len(hh.j.Convs()); got != before {
				t.Fatalf("conversations %d → %d: something was branched", before, got)
			}
			if !strings.HasPrefix(hh.lastPrompt(), "[reaction]") {
				t.Fatalf("the reaction did not reach the agent: %q", hh.lastPrompt())
			}
		})
	}
}

// TestBranchReactionInADMFallsThrough: a reaction cannot name a
// destination channel and a DM is in none, so the gesture is not a
// branch trigger there at all — it is passed to the agent untouched,
// and `!branch #**channel** <text>` is how you branch out of a DM.
func TestBranchReactionInADMFallsThrough(t *testing.T) {
	hh := dmHarness(t, newAgent("done"), func(c *Config) {
		c.Reactions = true
	})
	hh.h.cfg.ReactionTrigger = hh.h.BranchReaction
	hh.deliverDM(t, humanID, "hello", humanID, botID)
	own := hh.z.lastID()

	hh.react(t, reactionEvent(humanID, own, branchEmoji, zulipproto.ReactionAdd))

	for _, c := range hh.j.Convs() {
		if !c.IsDM() {
			t.Fatalf("a DM reaction created %+v", c)
		}
	}
	if !strings.Contains(hh.lastPrompt(), branchEmoji) {
		t.Fatalf("the reaction did not reach the agent: %q", hh.lastPrompt())
	}
}

// TestBranchReactionFromABotIsIgnored: BotSenderIDs is a startup
// snapshot, so a bot that appeared since is caught only by the name
// lookup — and it must not be able to create topics.
func TestBranchReactionFromABotIsIgnored(t *testing.T) {
	hh := branchReactHarness(t)
	hh.z.mu.Lock()
	hh.z.users[77] = zulipproto.User{UserID: 77, FullName: "Nagios", IsBot: true}
	hh.z.mu.Unlock()
	before := len(hh.j.Convs())

	hh.reactDropped(t, reactionEvent(77, hh.z.lastID(), branchEmoji, zulipproto.ReactionAdd))

	if got := len(hh.j.Convs()); got != before {
		t.Fatalf("a bot branched a topic: %d → %d", before, got)
	}
}

// TestBranchReactionBranchesAMessageOnce: two people reading the same
// message will tap the same emoji on it. The second tap must link the
// topic that exists, not open "… (2)" beside it.
func TestBranchReactionBranchesAMessageOnce(t *testing.T) {
	hh := branchReactHarness(t)
	id := hh.plant("planning", "rework the splitter", 99, "Grace Hopper")
	hh.fork(t, humanID, id)
	after := len(hh.j.Convs())

	hh.fork(t, humanID, id)

	if got := len(hh.j.Convs()); got != after {
		t.Fatalf("a second tap opened another conversation: %d → %d", after, got)
	}
	if !strings.Contains(hh.z.lastBody(), "already been branched → #**fleet>rework the splitter**") {
		t.Fatalf("second tap said %q", hh.z.lastBody())
	}
}

// TestBranchReactionRememberedPerConversation: the memory is keyed on
// (conversation, message), so `!new` — which retires the conversation
// and mints a fresh one for the same topic — does not leave the old
// one's branch refusing the new one's.
func TestBranchReactionRememberedPerConversation(t *testing.T) {
	hh := branchReactHarness(t)
	id := hh.plant("planning", "rework the splitter", 99, "Grace Hopper")
	hh.fork(t, humanID, id)
	hh.deliver(t, "planning", "!new")

	hh.fork(t, humanID, id)

	if _, ok := hh.j.Lookup(journal.Channel(4, "rework the splitter (2)")); !ok {
		t.Fatalf("the fresh conversation could not branch the same message; journal = %+v", hh.j.Convs())
	}
}

// TestBranchReactionLeavesTheOriginTurnRunning is the property the
// whole design rests on: a branch happens entirely in the NEW
// conversation. The origin's turn is not superseded, its claim is not
// taken, and the pointer line is the only thing written to it.
func TestBranchReactionLeavesTheOriginTurnRunning(t *testing.T) {
	hh := branchReactHarness(t)
	origin, ok := hh.j.Lookup(journal.Channel(4, "planning"))
	if !ok {
		t.Fatal("no origin conversation")
	}
	id := hh.plant("planning", "rework the splitter", 99, "Grace Hopper")

	// The tap is delivered from INSIDE the origin's next turn, which
	// is the only way to be sure the turn really is in flight when it
	// lands. sync.Once keeps the branched turn's own Prompt from
	// re-entering this.
	var once sync.Once
	var before, after *inflightEntry
	hh.a.mu.Lock()
	hh.a.during = func() {
		once.Do(func() {
			before = hh.inflightOf(origin.ID)
			hh.h.Handle(context.Background(), reactionEvent(humanID, id, branchEmoji, zulipproto.ReactionAdd))
			after = hh.inflightOf(origin.ID)
		})
	}
	hh.a.mu.Unlock()

	hh.deliver(t, "planning", mention("carry on"))

	if before == nil {
		t.Fatal("the origin turn held no claim to begin with")
	}
	if after != before {
		t.Fatalf("the branch disturbed the origin's turn: claim %p → %p", before, after)
	}
	if _, ok := hh.j.Lookup(journal.Channel(4, "rework the splitter")); !ok {
		t.Fatalf("nothing was branched; journal = %+v", hh.j.Convs())
	}
	// And the undisturbed turn delivered its answer, in the ORIGIN
	// topic — the branched turn answers "done" too, in its own.
	var answered bool
	hh.z.mu.Lock()
	for id, b := range hh.z.bodies {
		if strings.Contains(b, "done") && hh.z.topics[id] == "planning" {
			answered = true
		}
	}
	hh.z.mu.Unlock()
	if !answered {
		t.Fatalf("the origin turn lost its answer; posted %q", hh.z.stored())
	}
}

// TestBranchReactionRefusals covers every way a tap is consumed but
// creates nothing. Each says why in the origin topic, because a
// gesture that silently does nothing is indistinguishable from a bug.
func TestBranchReactionRefusals(t *testing.T) {
	for _, tc := range []struct {
		name   string
		setup  func(t *testing.T, hh *harness) int64
		errSub string
	}{
		{
			name: "a message with no text to branch",
			setup: func(_ *testing.T, hh *harness) int64 {
				return hh.plant("planning", "   \n ", 99, "Grace Hopper")
			},
			errSub: "no text in that message to branch",
		},
		{
			name: "the message cannot be read back",
			setup: func(_ *testing.T, hh *harness) int64 {
				own := hh.z.lastID()
				hh.z.mu.Lock()
				hh.z.getErr = errors.New("boom")
				hh.z.mu.Unlock()
				return own
			},
			errSub: "could not read that message back",
		},
		{
			// A refusal from the SHARED half: the tap is still
			// consumed, and nothing is remembered, so tapping again
			// once the channel answers is a real retry.
			name: "the destination's topics cannot be listed",
			setup: func(_ *testing.T, hh *harness) int64 {
				id := hh.plant("planning", "rework the splitter", 99, "Grace Hopper")
				hh.z.mu.Lock()
				hh.z.topicsErr = errors.New("boom")
				hh.z.mu.Unlock()
				return id
			},
			errSub: "could not check which topics are already in that channel",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hh := branchReactHarness(t)
			before := len(hh.j.Convs())
			id := tc.setup(t, hh)

			hh.fork(t, humanID, id)

			if got := len(hh.j.Convs()); got != before {
				t.Fatalf("conversations %d → %d: something was created", before, got)
			}
			if !strings.Contains(hh.z.lastBody(), tc.errSub) {
				t.Fatalf("reply = %q, want one mentioning %q", hh.z.lastBody(), tc.errSub)
			}
			// Consumed: a refusal plus an ambient "someone reacted"
			// would be noise on top of an answer.
			if strings.HasPrefix(hh.lastPrompt(), "[reaction]") {
				t.Fatalf("the refused reaction also reached the agent: %q", hh.lastPrompt())
			}
		})
	}
}

// TestBranchReactionUnservedChannel drives the one refusal
// handleReaction's own gates make unreachable: BranchReaction is called
// directly with a conversation in a channel the relay does not serve,
// which is what a subscription lost between the two checks would look
// like.
func TestBranchReactionUnservedChannel(t *testing.T) {
	hh := branchReactHarness(t)
	id := hh.plant("planning", "rework the splitter", 99, "Grace Hopper")
	conv := journal.Conv{ID: "c-gone", Key: journal.Channel(404, "elsewhere")}

	if !hh.h.BranchReaction(context.Background(), conv, reactionEvent(humanID, id, branchEmoji, zulipproto.ReactionAdd), nil) {
		t.Fatal("the reaction was not consumed")
	}
	if !strings.Contains(hh.z.lastBody(), "no longer serve this channel") {
		t.Fatalf("reply = %q", hh.z.lastBody())
	}
}

// TestBranchReactionAttributesTheTextToItsAuthor: the prompt's `[name]`
// prefix is the person branching, but the text is somebody else's. The
// agent must be told, because `allowed_user_ids` decides whose words
// reach it — and one allowlisted tap can spin in a message from
// somebody it would never have delivered.
func TestBranchReactionAttributesTheTextToItsAuthor(t *testing.T) {
	t.Run("somebody else's message", func(t *testing.T) {
		hh := branchReactHarness(t)
		id := hh.plant("planning", "rework the splitter", 99, "Grace Hopper")
		hh.fork(t, humanID, id)
		if !strings.Contains(hh.lastPrompt(), "written by Grace Hopper, not by Ada Lovelace") {
			t.Fatalf("prompt = %q", hh.lastPrompt())
		}
	})
	t.Run("the agent's own message", func(t *testing.T) {
		hh := branchReactHarness(t)
		hh.fork(t, humanID, hh.z.lastID())
		if !strings.Contains(hh.lastPrompt(), "an earlier message of your own") {
			t.Fatalf("prompt = %q", hh.lastPrompt())
		}
	})
	t.Run("the reactor's own message carries no clause", func(t *testing.T) {
		hh := branchReactHarness(t)
		id := hh.plant("planning", "rework the splitter", humanID, "Ada Lovelace")
		hh.fork(t, humanID, id)
		if strings.Contains(hh.lastPrompt(), "written by") {
			t.Fatalf("prompt = %q, want no attribution clause", hh.lastPrompt())
		}
	})
}

// TestBranchReactionDedupFollowsARename: the branched agent is told to
// rename its topic as soon as it knows what the conversation is about,
// so a link captured at branch time is stale almost immediately. The
// memory stores the conv-id and resolves the location from it.
func TestBranchReactionDedupFollowsARename(t *testing.T) {
	hh := branchReactHarness(t)
	id := hh.plant("planning", "rework the splitter", 99, "Grace Hopper")
	hh.fork(t, humanID, id)
	// A rename as Zulip delivers one: the topic moves, and
	// handleUpdate migrates the conversation with it.
	hh.h.Handle(context.Background(), zulipproto.Event{
		Type: zulipproto.EventUpdateMessage, StreamID: 4,
		OrigTopic: "rework the splitter", Topic: "splitter rework",
	})

	hh.fork(t, humanID, id)

	if !strings.Contains(hh.z.lastBody(), "already been branched → #**fleet>splitter rework**") {
		t.Fatalf("second tap said %q, want the topic's CURRENT name", hh.z.lastBody())
	}
}

// TestBranchReactionDedupForgetsARetiredBranch: `!new` in the branched
// topic retires the conversation the memory points at. There is then
// no live conversation to point anyone at, so a fresh tap is a fresh
// branch rather than a link to nothing.
func TestBranchReactionDedupForgetsARetiredBranch(t *testing.T) {
	hh := branchReactHarness(t)
	id := hh.plant("planning", "rework the splitter", 99, "Grace Hopper")
	hh.fork(t, humanID, id)
	if _, _, _, err := hh.j.Retire(journal.Channel(4, "rework the splitter")); err != nil {
		t.Fatalf("retire: %v", err)
	}

	hh.fork(t, humanID, id)

	// Retire mints a fresh conversation for the same key, so the
	// original topic name is taken and the re-branch lands beside it.
	if _, ok := hh.j.Lookup(journal.Channel(4, "rework the splitter (2)")); !ok {
		t.Fatalf("nothing was re-branched; journal = %+v", hh.j.Convs())
	}
	if strings.Contains(hh.z.lastBody(), "already been branched") {
		t.Fatalf("the tap was refused by a retired branch: %q", hh.z.lastBody())
	}
}
