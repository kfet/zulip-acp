package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kfet/acp-kit/client"
	"github.com/kfet/zulip-acp/internal/journal"
	"github.com/kfet/zulip-acp/internal/zulipproto"
)

// only returns the single message the surface holds, failing if there
// is not exactly one. A command reply is one message and never streams,
// so anything else is a bug the test should name.
func (hh *harness) only(t *testing.T) string {
	t.Helper()
	got := hh.z.stored()
	if len(got) != 1 {
		t.Fatalf("want exactly one message, got %d: %q", len(got), got)
	}
	return got[0]
}

// engage runs one ordinary turn so the topic/DM has a conversation.
func (hh *harness) engage(t *testing.T, topic string) journal.Conv {
	t.Helper()
	hh.deliver(t, topic, mention("hello"))
	convs := hh.j.Convs()
	if len(convs) != 1 {
		t.Fatalf("want one conversation, got %+v", convs)
	}
	hh.z.reset()
	return convs[0]
}

func (z *fakeZulip) reset() {
	z.mu.Lock()
	defer z.mu.Unlock()
	z.order = nil
	z.bodies = map[int64]string{}
	z.topics = map[int64]string{}
	z.dms = map[int64][]int64{}
}

// --- dispatch and gating -------------------------------------------------

// TestCommandNeverReachesTheAgent is the whole point: a command is
// relay business and must consume no agent turn.
func TestCommandNeverReachesTheAgent(t *testing.T) {
	hh := dmHarness(t, newAgent("agent output"), nil)
	hh.deliverDM(t, humanID, "!help", humanID, botID)
	if got := hh.a.prompts; len(got) != 0 {
		t.Fatalf("command was forwarded to the agent: %q", got)
	}
	if !strings.Contains(hh.only(t), "Relay commands") {
		t.Fatalf("reply = %q", hh.only(t))
	}
}

// TestCommandsWorkUnconditionallyInADM: a DM is addressed by
// construction, so a command needs no engagement and no mention.
func TestCommandsWorkUnconditionallyInADM(t *testing.T) {
	hh := dmHarness(t, newAgent("x"), nil)
	hh.deliverDM(t, humanID, "!status", humanID, botID)
	got := hh.only(t)
	if !strings.Contains(got, "DM with U8, U9") {
		t.Fatalf("reply = %q", got)
	}
	if !strings.Contains(got, "none yet") {
		t.Fatalf("a command must not allocate a conversation: %q", got)
	}
	if len(hh.j.Convs()) != 0 {
		t.Fatalf("command allocated %+v", hh.j.Convs())
	}
}

// TestChannelCommandNeedsMentionOrEngagement pins the documented
// channel gate: a command is honoured exactly when a prompt would be.
func TestChannelCommandNeedsMentionOrEngagement(t *testing.T) {
	// Virgin topic, no mention: none of the relay's business.
	hh := newHarness(t, newAgent("x"), nil)
	hh.deliver(t, "quiet", "!help")
	if hh.z.count() != 0 {
		t.Fatalf("relay answered an unaddressed command: %q", hh.z.stored())
	}

	// Virgin topic, mentioned: honoured, and still allocates nothing.
	hh.deliver(t, "quiet", mention("!help"))
	if !strings.Contains(hh.only(t), "Relay commands") {
		t.Fatalf("reply = %q", hh.only(t))
	}
	if len(hh.j.Convs()) != 0 {
		t.Fatalf("`!help` allocated %+v", hh.j.Convs())
	}

	// Engaged topic, no mention: honoured ambiently, like any
	// follow-up.
	hh2 := newHarness(t, newAgent("x"), nil)
	hh2.engage(t, "loud")
	hh2.deliver(t, "loud", "!id")
	if !strings.HasPrefix(hh2.only(t), "`c") {
		t.Fatalf("reply = %q", hh2.only(t))
	}
}

func TestUnknownCommandIsNamedAndNotForwarded(t *testing.T) {
	hh := dmHarness(t, newAgent("x"), nil)
	hh.deliverDM(t, humanID, "!frobnicate the thing", humanID, botID)
	got := hh.only(t)
	for _, want := range []string{"`!frobnicate`", "`!help`", "`!!frobnicate`"} {
		if !strings.Contains(got, want) {
			t.Fatalf("reply %q is missing %s", got, want)
		}
	}
	if len(hh.a.prompts) != 0 {
		t.Fatalf("an unknown command was forwarded: %q", hh.a.prompts)
	}
}

// TestProseStartingWithBangReachesTheAgentUnchanged is the rule that
// matters most: never eat a message that merely happens to start with
// a bang.
func TestProseStartingWithBangReachesTheAgentUnchanged(t *testing.T) {
	for _, text := range []string{
		"!important: fix the parser",
		"!5 minutes left",
		"!",
		"! new",
		"!!new",
		"!!important: fix",
	} {
		t.Run(text, func(t *testing.T) {
			hh := dmHarness(t, newAgent("ok"), nil)
			hh.deliverDM(t, humanID, text, humanID, botID)
			if len(hh.a.prompts) != 1 {
				t.Fatalf("prose was eaten: %q", hh.a.prompts)
			}
			want := strings.TrimPrefix(text, "!!")
			if text != want {
				want = "!" + want
			}
			if got := hh.a.prompts[0]; got != "[Kfet] "+want {
				t.Fatalf("prompt = %q, want %q", got, "[Kfet] "+want)
			}
		})
	}
}

func TestCommandsObeyTheUserAllowlist(t *testing.T) {
	hh := dmHarness(t, newAgent("x"), func(c *Config) {
		c.AllowedUsers = map[int64]struct{}{999: {}}
	})
	hh.deliverDM(t, humanID, "!help", humanID, botID)
	if hh.z.count() != 0 {
		t.Fatalf("a disallowed user got a command reply: %q", hh.z.stored())
	}
}

// TestBotGuardsRunAheadOfCommandParsing: the relay must never obey a
// command it wrote itself, nor one a system bot posted.
func TestBotGuardsRunAheadOfCommandParsing(t *testing.T) {
	hh := dmHarness(t, newAgent("x"), func(c *Config) {
		c.BotSenderIDs = map[int64]struct{}{7: {}}
	})
	for _, sender := range []int64{botID, 7} {
		hh.deliverDM(t, sender, "!new", sender, botID)
	}
	if hh.z.count() != 0 {
		t.Fatalf("relay obeyed a bot's command: %q", hh.z.stored())
	}
}

func TestCommandReplyFailureIsLogged(t *testing.T) {
	hh := dmHarness(t, newAgent("x"), nil)
	hh.z.sendErr = errors.New("boom")
	hh.deliverDM(t, humanID, "!help", humanID, botID)
	if !hh.logged("posting command reply") {
		t.Fatalf("logs = %q", hh.logs)
	}
}

// --- !help ---------------------------------------------------------------

func TestHelpListsEveryCommandAndTheEscape(t *testing.T) {
	hh := dmHarness(t, newAgent("x"), nil)
	hh.deliverDM(t, humanID, "!HELP", humanID, botID)
	got := hh.only(t)
	for _, want := range []string{
		"`!help`", "`!status`", "`!id`", "`!model [id]`", "`!new`", "`!stop`",
		"(also `!reset`)", "(also `!cancel`)", "`!!important`",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("help %q is missing %s", got, want)
		}
	}
}

// --- !status and !id -----------------------------------------------------

func TestStatusInAnEngagedTopic(t *testing.T) {
	hh := newHarness(t, newAgent("x"), func(c *Config) { c.AckEmoji = "" })
	hh.a.model = "anthropic/opus"
	conv := hh.engage(t, "hacking")
	hh.deliver(t, "hacking", "!status")
	got := hh.only(t)
	for _, want := range []string{
		`#fleet > "hacking"`,
		"`" + conv.ID + "`",
		"`" + filepath.Join(hh.s.dir, "convs", conv.ID) + "`",
		"`anthropic/opus`",
		"turn running: no",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("status %q is missing %s", got, want)
		}
	}
}

func TestStatusReportsAnUnknownModel(t *testing.T) {
	hh := dmHarness(t, newAgent("x"), nil)
	hh.deliverDM(t, humanID, "!status", humanID, botID)
	if !strings.Contains(hh.only(t), "not reported yet") {
		t.Fatalf("status = %q", hh.only(t))
	}
}

// TestStatusFallsBackToIDsWithoutNames covers a DM whose
// display_recipient carries no full names.
func TestStatusFallsBackToIDsWithoutNames(t *testing.T) {
	hh := dmHarness(t, newAgent("x"), nil)
	hh.h.Handle(context.Background(), namelessDM(humanID, "!status", humanID, botID))
	if !strings.Contains(hh.only(t), "DM 8,9") {
		t.Fatalf("status = %q", hh.only(t))
	}
}

func TestStatusReportsARunningTurn(t *testing.T) {
	agent := newAgent("x")
	agent.block = make(chan struct{})
	hh := newHarness(t, agent, nil)
	hh.h.Handle(context.Background(), channelEvent(humanID, "hacking", mention("go")))
	<-agent.entered
	hh.z.reset()

	hh.h.Handle(context.Background(), channelEvent(humanID, "hacking", "!status"))
	if !strings.Contains(hh.only(t), "turn running: yes") {
		t.Fatalf("status = %q", hh.only(t))
	}
	close(agent.block)
	waitIdle(t, hh)
}

func TestIDBeforeAndAfterEngagement(t *testing.T) {
	hh := dmHarness(t, newAgent("x"), nil)
	hh.deliverDM(t, humanID, "!id", humanID, botID)
	if !strings.Contains(hh.only(t), "No conversation here yet") {
		t.Fatalf("reply = %q", hh.only(t))
	}
	hh.z.reset()

	hh.deliverDM(t, humanID, "hello", humanID, botID)
	hh.z.reset()
	hh.deliverDM(t, humanID, "!id", humanID, botID)
	want := "`" + hh.j.Convs()[0].ID + "`"
	if got := hh.only(t); got != want {
		t.Fatalf("reply = %q, want %q", got, want)
	}
}

// --- !new ----------------------------------------------------------------

// TestNewRetiresTheConversationInADM is the motivating gap: a DM is
// keyed on the participant set, so `!new` is the only way to start over
// there.
func TestNewRetiresTheConversationInADM(t *testing.T) {
	hh := dmHarness(t, newAgent("x"), nil)
	hh.deliverDM(t, humanID, "first", humanID, botID)
	old := hh.j.Convs()[0]
	hh.z.reset()

	hh.deliverDM(t, humanID, "!RESET", humanID, botID)
	reply := hh.only(t)
	fresh, ok := hh.j.Lookup(journal.DM([]int64{humanID, botID}))
	if !ok || fresh.ID == old.ID {
		t.Fatalf("conversation not retired: %+v", fresh)
	}
	for _, want := range []string{"`" + fresh.ID + "`", "`" + old.ID + "`", filepath.Join(hh.s.dir, "convs", old.ID)} {
		if !strings.Contains(reply, want) {
			t.Fatalf("reply %q is missing %s", reply, want)
		}
	}

	// The next message starts the fresh conversation, not the old one.
	hh.z.reset()
	hh.deliverDM(t, humanID, "second", humanID, botID)
	if _, ran := hh.s.sessions[fresh.ID]; !ran {
		t.Fatalf("next turn did not use the fresh conversation: %v", hh.s.sessions)
	}
}

func TestNewOnAConversationThatDoesNotExistYet(t *testing.T) {
	hh := dmHarness(t, newAgent("x"), nil)
	hh.deliverDM(t, humanID, "!new", humanID, botID)
	if !strings.Contains(hh.only(t), "Nothing to retire") {
		t.Fatalf("reply = %q", hh.only(t))
	}
	if len(hh.j.Convs()) != 0 {
		t.Fatalf("`!new` allocated %+v", hh.j.Convs())
	}
}

// TestNewClearsTheTailSoTheNextTurnStartsClean pins the requirement
// that a retired conversation's tail message is forgotten.
func TestNewClearsTheTailSoTheNextTurnStartsClean(t *testing.T) {
	hh := dmHarness(t, newAgent("x"), nil)
	hh.deliverDM(t, humanID, "hello", humanID, botID)
	old := hh.j.Convs()[0]
	if err := hh.j.SetTail(old.ID, 42); err != nil {
		t.Fatalf("SetTail: %v", err)
	}
	hh.deliverDM(t, humanID, "!new", humanID, botID)
	if tails := hh.j.OpenTails(); len(tails) != 0 {
		t.Fatalf("OpenTails = %+v, want none", tails)
	}
}

// TestNewCancelsTheRunningTurn: retiring a conversation must not leave
// its turn streaming into a conversation the user just declared over.
func TestNewCancelsTheRunningTurn(t *testing.T) {
	agent := newAgent("x")
	agent.block = make(chan struct{})
	hh := dmHarness(t, agent, func(c *Config) { c.AckEmoji = "eyes" })
	hh.h.Handle(context.Background(), dmEvent(humanID, "go", humanID, botID))
	<-agent.entered

	hh.h.Handle(context.Background(), dmEvent(humanID, "!new", humanID, botID))
	hh.awaitTurnCleanup(t)
	if len(hh.s.cancels) == 0 {
		t.Fatal("`!new` left the running turn alone")
	}
}

func TestNewReportsAJournalFailure(t *testing.T) {
	hh := dmHarness(t, newAgent("x"), nil)
	hh.deliverDM(t, humanID, "hello", humanID, botID)
	old := hh.j.Convs()[0]
	hh.z.reset()
	hh.breakJournal(t)
	hh.deliverDM(t, humanID, "!new", humanID, botID)
	if !strings.Contains(hh.only(t), "Couldn't start a new conversation") {
		t.Fatalf("reply = %q", hh.only(t))
	}
	if !hh.logged("retiring conversation") {
		t.Fatalf("logs = %q", hh.logs)
	}
	// The reply said it failed, so it must actually have failed: the
	// key still resolves to the original conversation. Anything else
	// and the relay would behave as though `!new` worked, until a
	// restart reloaded the untouched file and silently undid it.
	if got, ok := hh.j.Lookup(journal.DM([]int64{humanID, botID})); !ok || got.ID != old.ID {
		t.Fatalf("conversation = %+v, %v; want the unchanged %q", got, ok, old.ID)
	}
}

// TestNewLosesTheRaceWithAnotherRetire covers the window between
// Lookup and Retire: something else already moved the key on.
func TestNewLosesTheRaceWithAnotherRetire(t *testing.T) {
	hh := dmHarness(t, newAgent("x"), nil)
	hh.deliverDM(t, humanID, "hello", humanID, botID)
	conv := hh.j.Convs()[0]
	// Rename the key out from under the conversation, the way a topic
	// move would, so Retire finds nothing at it.
	if _, _, _, err := hh.j.Retire(journal.DM([]int64{humanID, botID})); err != nil {
		t.Fatalf("Retire: %v", err)
	}
	hh.z.reset()
	if got := hh.h.cmdNew(context.Background(), journal.DM([]int64{humanID, 77}), conv, true); !strings.Contains(got, "Nothing to retire") {
		t.Fatalf("reply = %q", got)
	}
}

// --- !stop ---------------------------------------------------------------

func TestStopInterruptsTheRunningTurn(t *testing.T) {
	agent := newAgent("x")
	agent.block = make(chan struct{})
	hh := newHarness(t, agent, func(c *Config) { c.AckEmoji = "eyes" })
	hh.h.Handle(context.Background(), channelEvent(humanID, "hacking", mention("go")))
	<-agent.entered
	hh.z.reset()

	hh.h.Handle(context.Background(), channelEvent(humanID, "hacking", "!cancel"))
	hh.awaitTurnCleanup(t)
	if !strings.Contains(hh.z.stored()[0], "Interrupted") {
		t.Fatalf("reply = %q", hh.z.stored())
	}
	if len(hh.s.cancels) == 0 {
		t.Fatal("`!stop` did not cancel the session")
	}
}

func TestStopWithNothingRunning(t *testing.T) {
	hh := dmHarness(t, newAgent("x"), nil)
	// No conversation at all…
	hh.deliverDM(t, humanID, "!stop", humanID, botID)
	if !strings.Contains(hh.only(t), "Nothing is running") {
		t.Fatalf("reply = %q", hh.only(t))
	}
	// …and a finished one.
	hh.deliverDM(t, humanID, "hello", humanID, botID)
	hh.z.reset()
	hh.deliverDM(t, humanID, "!stop", humanID, botID)
	if !strings.Contains(hh.only(t), "Nothing is running") {
		t.Fatalf("reply = %q", hh.only(t))
	}
}

// --- !model --------------------------------------------------------------

func withModels(a *fakeAgent, current string, ids ...string) *fakeAgent {
	a.model = current
	for _, id := range ids {
		a.models = append(a.models, client.ModelInfo{ID: id, Name: id})
	}
	return a
}

func TestModelWithoutAnyReportedModels(t *testing.T) {
	hh := dmHarness(t, newAgent("x"), nil)
	hh.deliverDM(t, humanID, "!model", humanID, botID)
	if !strings.Contains(hh.only(t), "has not reported its models") {
		t.Fatalf("reply = %q", hh.only(t))
	}
}

func TestModelLists(t *testing.T) {
	hh := dmHarness(t, withModels(newAgent("x"), "a/one", "a/one", "b/two"), nil)
	hh.deliverDM(t, humanID, "!model", humanID, botID)
	got := hh.only(t)
	for _, want := range []string{"**2 models**", "`a/one` ← current", "`b/two`"} {
		if !strings.Contains(got, want) {
			t.Fatalf("reply %q is missing %s", got, want)
		}
	}
}

func TestModelFilters(t *testing.T) {
	hh := dmHarness(t, withModels(newAgent("x"), "a/one", "a/one", "b/two"), nil)
	hh.deliverDM(t, humanID, "!model TWO", humanID, botID)
	got := hh.only(t)
	if !strings.Contains(got, "**1 of 2 models** match \"TWO\"") || !strings.Contains(got, "`b/two`") {
		t.Fatalf("reply = %q", got)
	}
	hh.z.reset()

	hh.deliverDM(t, humanID, "!model nope", humanID, botID)
	if !strings.Contains(hh.only(t), "No model id matches \"nope\"") {
		t.Fatalf("reply = %q", hh.only(t))
	}
}

func TestModelListIsCapped(t *testing.T) {
	a := newAgent("x")
	ids := make([]string, 0, modelsListCap+3)
	for i := range modelsListCap + 3 {
		ids = append(ids, "p/m"+string(rune('a'+i%26))+string(rune('a'+i/26)))
	}
	hh := dmHarness(t, withModels(a, "", ids...), nil)
	hh.deliverDM(t, humanID, "!model", humanID, botID)
	if !strings.Contains(hh.only(t), "…and 3 more") {
		t.Fatalf("reply = %q", hh.only(t))
	}
}

func TestModelSwitchNeedsAConversation(t *testing.T) {
	hh := dmHarness(t, withModels(newAgent("x"), "a/one", "a/one", "b/two"), nil)
	hh.deliverDM(t, humanID, "!model b/two", humanID, botID)
	if !strings.Contains(hh.only(t), "no conversation here yet") {
		t.Fatalf("reply = %q", hh.only(t))
	}
}

// TestModelSwitchAppliesOnTheNextTurn: the choice is recorded now and
// pushed to the ACP session when the next turn opens it.
func TestModelSwitchAppliesOnTheNextTurn(t *testing.T) {
	hh := dmHarness(t, withModels(newAgent("x"), "a/one", "a/one", "b/two"), nil)
	hh.deliverDM(t, humanID, "hello", humanID, botID)
	conv := hh.j.Convs()[0]
	hh.z.reset()

	hh.deliverDM(t, humanID, "!model b/two", humanID, botID)
	if !strings.Contains(hh.only(t), "Model set to `b/two`") {
		t.Fatalf("reply = %q", hh.only(t))
	}
	if got := hh.a.selections(); len(got) != 0 {
		t.Fatalf("the switch was pushed outside a turn: %v", got)
	}
	hh.z.reset()

	// `!status` and `!model` both report the sticky choice.
	hh.deliverDM(t, humanID, "!status", humanID, botID)
	if !strings.Contains(hh.only(t), "`b/two` (set with `!model`)") {
		t.Fatalf("status = %q", hh.only(t))
	}
	hh.z.reset()
	hh.deliverDM(t, humanID, "!model", humanID, botID)
	if !strings.Contains(hh.only(t), "`b/two` ← current") {
		t.Fatalf("model list = %q", hh.only(t))
	}
	hh.z.reset()

	// The next turn pushes it, exactly once.
	hh.deliverDM(t, humanID, "again", humanID, botID)
	hh.deliverDM(t, humanID, "and again", humanID, botID)
	want := []string{"sid-" + conv.ID + "=b/two"}
	if got := hh.a.selections(); len(got) != 1 || got[0] != want[0] {
		t.Fatalf("selections = %v, want %v", got, want)
	}
}

func TestModelSwitchFailureIsLoggedAndTheTurnContinues(t *testing.T) {
	hh := dmHarness(t, withModels(newAgent("answer"), "a/one", "a/one", "b/two"), nil)
	hh.deliverDM(t, humanID, "hello", humanID, botID)
	hh.deliverDM(t, humanID, "!model b/two", humanID, botID)
	hh.a.setErr = errors.New("nope")
	hh.z.reset()

	hh.deliverDM(t, humanID, "again", humanID, botID)
	if !hh.logged("selecting model b/two") {
		t.Fatalf("logs = %q", hh.logs)
	}
	if !strings.Contains(strings.Join(hh.z.stored(), " "), "answer") {
		t.Fatalf("the turn did not complete: %q", hh.z.stored())
	}
}

// TestNewCarriesTheModelChoiceOver: `!new` clears context, not the
// user's stated preference.
func TestNewCarriesTheModelChoiceOver(t *testing.T) {
	hh := dmHarness(t, withModels(newAgent("x"), "a/one", "a/one", "b/two"), nil)
	hh.deliverDM(t, humanID, "hello", humanID, botID)
	hh.deliverDM(t, humanID, "!model b/two", humanID, botID)
	hh.deliverDM(t, humanID, "!new", humanID, botID)
	fresh, _ := hh.j.Lookup(journal.DM([]int64{humanID, botID}))
	hh.z.reset()

	hh.deliverDM(t, humanID, "next", humanID, botID)
	want := "sid-" + fresh.ID + "=b/two"
	if got := hh.a.selections(); len(got) != 1 || got[0] != want {
		t.Fatalf("selections = %v, want [%s]", got, want)
	}
}

// TestModelChoiceSurvivesASessionSwap covers idle GC: a conversation
// whose session was reaped comes back with a new session id, and the
// choice must be pushed again.
func TestModelChoiceSurvivesASessionSwap(t *testing.T) {
	hh := dmHarness(t, withModels(newAgent("x"), "a/one", "a/one", "b/two"), nil)
	hh.deliverDM(t, humanID, "hello", humanID, botID)
	conv := hh.j.Convs()[0]
	hh.deliverDM(t, humanID, "!model b/two", humanID, botID)
	hh.deliverDM(t, humanID, "again", humanID, botID)

	// Reap the session, as acp-kit's idle GC would.
	hh.s.mu.Lock()
	delete(hh.s.sessions, conv.ID)
	hh.s.mu.Unlock()

	hh.deliverDM(t, humanID, "after the gap", humanID, botID)
	if got := hh.a.selections(); len(got) != 2 {
		t.Fatalf("selections = %v, want the choice re-applied", got)
	}
}

// TestModelChoiceReplacedMidFlight covers the guard that stops a
// completed SetModel from stamping "applied" onto a choice the user
// has since changed.
func TestModelChoiceReplacedMidFlight(t *testing.T) {
	hh := dmHarness(t, withModels(newAgent("x"), "a/one", "a/one", "b/two"), nil)
	hh.h.setModelOverride("c1", "b/two")
	hh.a.setModel = nil
	// Swap the choice while SetModel is notionally in flight by
	// re-setting it before the applied marker lands.
	hh.h.applyModel(context.Background(), "c1", "sid-1")
	hh.h.setModelOverride("c1", "a/one")
	hh.h.applyModel(context.Background(), "c1", "sid-1")
	if got := hh.a.selections(); len(got) != 2 {
		t.Fatalf("selections = %v, want both pushes", got)
	}
	// A third call with the same choice and session is a no-op.
	hh.h.applyModel(context.Background(), "c1", "sid-1")
	if got := hh.a.selections(); len(got) != 2 {
		t.Fatalf("selections = %v, want no extra push", got)
	}
}

func TestApplyModelWithNoChoice(t *testing.T) {
	hh := dmHarness(t, newAgent("x"), nil)
	hh.h.applyModel(context.Background(), "c-nothing", "sid-1")
	if got := hh.a.selections(); len(got) != 0 {
		t.Fatalf("selections = %v", got)
	}
}

// --- event helpers -------------------------------------------------------

// channelEvent builds a channel message event without waiting for the
// turn, so a test can inspect the relay mid-flight.
func channelEvent(sender int64, topic, content string) zulipproto.Event {
	return zulipproto.Event{
		Type: zulipproto.EventMessage,
		Message: &zulipproto.Message{
			ID: 1, SenderID: sender, SenderName: "Kfet", Content: content,
			StreamID: 4, Topic: topic, Type: zulipproto.MessageTypeStream,
		},
	}
}

// dmEvent is channelEvent's direct-message counterpart.
func dmEvent(sender int64, content string, recipients ...int64) zulipproto.Event {
	ev := zulipproto.Event{
		Type: zulipproto.EventMessage,
		Message: &zulipproto.Message{
			ID: 1, SenderID: sender, SenderName: "Kfet", Content: content,
			Type: zulipproto.MessageTypePrivate, DisplayRecipient: dmRecipients(recipients...),
		},
	}
	return ev
}

// namelessDM is a direct message whose display_recipient carries ids
// but no full names — the shape `!status` must degrade gracefully on.
func namelessDM(sender int64, content string, recipients ...int64) zulipproto.Event {
	ev := dmEvent(sender, content, recipients...)
	parts := make([]string, 0, len(recipients))
	for _, id := range recipients {
		parts = append(parts, fmt.Sprintf(`{"id":%d}`, id))
	}
	ev.Message.DisplayRecipient = json.RawMessage("[" + strings.Join(parts, ",") + "]")
	return ev
}

// awaitTurnCleanup waits for a CANCELLED turn to finish unwinding.
// WaitIdle cannot see one: cancellation drops the inflight entry
// immediately, while the turn is still posting its epitaph and
// clearing its journal tail. Retracting the ack reaction is the last
// thing run() does, so it is the honest signal that the turn has
// stopped touching the filesystem.
func (hh *harness) awaitTurnCleanup(t *testing.T) {
	t.Helper()
	select {
	case <-hh.z.unreacted:
	case <-time.After(10 * time.Second):
		t.Fatal("cancelled turn never finished unwinding")
	}
}

func waitIdle(t *testing.T, hh *harness) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := hh.h.WaitIdle(ctx); err != nil {
		t.Fatalf("turn did not finish: %v", err)
	}
}
