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
	"github.com/kfet/acp-kit/command"
	"github.com/kfet/zulip-acp/internal/journal"
	"github.com/kfet/zulip-acp/internal/zulipproto"
)

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
	return zulipproto.Event{
		Type: zulipproto.EventMessage,
		Message: &zulipproto.Message{
			ID: 1, SenderID: sender, SenderName: "Kfet", Content: content,
			Type: zulipproto.MessageTypePrivate, DisplayRecipient: dmRecipients(recipients...),
		},
	}
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

func waitIdle(t *testing.T, hh *harness) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := hh.h.WaitIdle(ctx); err != nil {
		t.Fatalf("turn did not finish: %v", err)
	}
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

// only returns the single message the surface holds. A command reply is
// one message and never streams, so anything else is a bug to name.
func (hh *harness) only(t *testing.T) string {
	t.Helper()
	got := hh.z.stored()
	if len(got) != 1 {
		t.Fatalf("want exactly one message, got %d: %q", len(got), got)
	}
	return got[0]
}

func (z *fakeZulip) reset() {
	z.mu.Lock()
	defer z.mu.Unlock()
	z.order = nil
	z.bodies = map[int64]string{}
	z.topics = map[int64]string{}
	z.dms = map[int64][]int64{}
}

// cmdHarness is a harness with the shared broker wired in, which is how
// the relay actually runs.
func cmdHarness(t *testing.T, agent *fakeAgent, tune func(*Config)) *harness {
	t.Helper()
	return newHarness(t, agent, func(c *Config) {
		c.Commands = command.New(agent)
		if tune != nil {
			tune(c)
		}
	})
}

func dmCmdHarness(t *testing.T, agent *fakeAgent, tune func(*Config)) *harness {
	t.Helper()
	return cmdHarness(t, agent, func(c *Config) {
		c.DMs = true
		if tune != nil {
			tune(c)
		}
	})
}

// --- dispatch and gating -------------------------------------------------

// TestCommandNeverReachesTheAgent is the whole point: a command is
// relay business and must consume no agent turn.
func TestCommandNeverReachesTheAgent(t *testing.T) {
	hh := dmCmdHarness(t, newAgent("agent output"), nil)
	hh.deliverDM(t, humanID, "!help", humanID, botID)
	if got := hh.a.prompts; len(got) != 0 {
		t.Fatalf("command was forwarded to the agent: %q", got)
	}
	if !strings.Contains(hh.only(t), "!login") {
		t.Fatalf("reply = %q", hh.only(t))
	}
}

// TestCommandsWorkUnconditionallyInADM: a DM is addressed by
// construction, so a command needs no engagement and no mention.
func TestCommandsWorkUnconditionallyInADM(t *testing.T) {
	hh := dmCmdHarness(t, newAgent("x"), nil)
	hh.deliverDM(t, humanID, "!status", humanID, botID)
	got := hh.only(t)
	if !strings.Contains(got, "DM with U8, U9") {
		t.Fatalf("reply = %q", got)
	}
	if len(hh.j.Convs()) != 0 {
		t.Fatalf("a command must not allocate a conversation: %+v", hh.j.Convs())
	}
}

// TestChannelCommandNeedsMentionOrEngagement pins the documented
// channel gate: a command is honoured exactly when a prompt would be.
func TestChannelCommandNeedsMentionOrEngagement(t *testing.T) {
	hh := cmdHarness(t, newAgent("x"), nil)
	hh.deliver(t, "quiet", "!help")
	if hh.z.count() != 0 {
		t.Fatalf("relay answered an unaddressed command: %q", hh.z.stored())
	}

	hh.deliver(t, "quiet", mention("!help"))
	if !strings.Contains(hh.only(t), "!status") {
		t.Fatalf("reply = %q", hh.only(t))
	}
	if len(hh.j.Convs()) != 0 {
		t.Fatalf("`!help` allocated %+v", hh.j.Convs())
	}

	hh2 := cmdHarness(t, newAgent("x"), nil)
	hh2.deliver(t, "loud", mention("hello"))
	hh2.z.reset()
	hh2.deliver(t, "loud", "!status")
	if !strings.Contains(hh2.only(t), "**Status**") {
		t.Fatalf("reply = %q", hh2.only(t))
	}
}

func TestCommandsObeyTheUserAllowlist(t *testing.T) {
	hh := dmCmdHarness(t, newAgent("x"), func(c *Config) {
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
	hh := dmCmdHarness(t, newAgent("x"), func(c *Config) {
		c.BotSenderIDs = map[int64]struct{}{7: {}}
	})
	for _, sender := range []int64{botID, 7} {
		hh.deliverDM(t, sender, "!new", sender, botID)
	}
	if hh.z.count() != 0 {
		t.Fatalf("relay obeyed a bot's command: %q", hh.z.stored())
	}
}

// TestNoBrokerMeansNoCommandSurface: with Commands unset every message
// is prose, so an operator can turn the whole thing off.
func TestNoBrokerMeansNoCommandSurface(t *testing.T) {
	hh := newHarness(t, newAgent("x"), func(c *Config) { c.DMs = true })
	hh.deliverDM(t, humanID, "!help", humanID, botID)
	if len(hh.a.prompts) != 1 || !strings.Contains(hh.a.prompts[0], "!help") {
		t.Fatalf("prompts = %q", hh.a.prompts)
	}
}

// --- Zulip's own slash messages ------------------------------------------

// TestZulipWidgetsAreNeverEaten is the Zulip-specific rule: /me, /poll
// and /todo are real messages and widgets, not client-side slash
// commands, and must reach the agent byte-for-byte. Swallowing a poll
// because "poll" is not a relay command would be indefensible.
func TestZulipWidgetsAreNeverEaten(t *testing.T) {
	for _, text := range []string{
		"/me waves at the agent",
		"/poll What shall we ship?",
		"/todo write the tests",
		"/ME shouting",
		"/me",
	} {
		t.Run(text, func(t *testing.T) {
			hh := dmCmdHarness(t, newAgent("ok"), nil)
			hh.deliverDM(t, humanID, text, humanID, botID)
			if len(hh.a.prompts) != 1 {
				t.Fatalf("widget was eaten: %q", hh.a.prompts)
			}
			if got := hh.a.prompts[0]; got != "[Kfet] "+text {
				t.Fatalf("prompt = %q, want %q", got, "[Kfet] "+text)
			}
		})
	}
}

// TestWidgetsWinOverAPendingLogin: a /poll must never be consumed as a
// failed redirect paste.
func TestWidgetsWinOverAPendingLogin(t *testing.T) {
	agent := newAgent("ok")
	agent.authMethods = []client.AuthMethod{{ID: "oauth-anthropic", Name: "Anthropic"}}
	agent.authResult = client.AuthResult{State: "needs_redirect", URL: "https://example/auth", ID: "a1"}
	hh := dmCmdHarness(t, agent, nil)

	hh.deliverDM(t, humanID, "!login anthropic", humanID, botID)
	if !strings.Contains(hh.only(t), "https://example/auth") {
		t.Fatalf("login did not start: %q", hh.only(t))
	}
	hh.z.reset()

	hh.deliverDM(t, humanID, "/poll lunch?", humanID, botID)
	if len(hh.a.prompts) != 1 || !strings.Contains(hh.a.prompts[0], "/poll lunch?") {
		t.Fatalf("the widget was eaten by the pending login: %q", hh.a.prompts)
	}
}

// TestSlashCommandsThatAreNotWidgetsStayProse: an unrecognised "/foo"
// belongs to Zulip's namespace, not the relay's, so it is forwarded
// rather than answered with an unknown-command error.
func TestSlashCommandsThatAreNotWidgetsStayProse(t *testing.T) {
	hh := dmCmdHarness(t, newAgent("ok"), nil)
	hh.deliverDM(t, humanID, "/somethingzulipy", humanID, botID)
	if len(hh.a.prompts) != 1 {
		t.Fatalf("prompts = %q", hh.a.prompts)
	}
}

// TestSlashSpelledRelayCommandsStillWork: "/" is accepted on input
// (poe-acp's grammar, kept) even though "!" is what we advertise.
func TestSlashSpelledRelayCommandsStillWork(t *testing.T) {
	hh := dmCmdHarness(t, newAgent("x"), nil)
	hh.deliverDM(t, humanID, "/status", humanID, botID)
	if !strings.Contains(hh.only(t), "**Status**") {
		t.Fatalf("reply = %q", hh.only(t))
	}
}

// --- prose, escapes and unknown commands ---------------------------------

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
		".hidden file",
	} {
		t.Run(text, func(t *testing.T) {
			hh := dmCmdHarness(t, newAgent("ok"), nil)
			hh.deliverDM(t, humanID, text, humanID, botID)
			if len(hh.a.prompts) != 1 {
				t.Fatalf("prose was eaten: %q", hh.a.prompts)
			}
			want := text
			if rest, ok := strings.CutPrefix(text, "!!"); ok {
				want = "!" + rest
			}
			if got := hh.a.prompts[0]; got != "[Kfet] "+want {
				t.Fatalf("prompt = %q, want %q", got, "[Kfet] "+want)
			}
		})
	}
}

func TestUnknownCommandIsNamedAndNotForwarded(t *testing.T) {
	hh := dmCmdHarness(t, newAgent("x"), nil)
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

// --- passthrough ---------------------------------------------------------

// TestAgentCommandPassthrough: an allowlisted command the agent
// actually advertises is rewritten to its slash form and forwarded
// through the normal prompt path, so the agent runs it and streams.
func TestAgentCommandPassthrough(t *testing.T) {
	agent := newAgent("reloaded")
	agent.agentCmds = []client.CommandInfo{{Name: "reload", Description: "reload config"}}
	hh := dmCmdHarness(t, agent, nil)
	hh.deliverDM(t, humanID, "!reload", humanID, botID)
	if len(hh.a.prompts) != 1 || !strings.Contains(hh.a.prompts[0], "/reload") {
		t.Fatalf("prompts = %q, want the rewritten /reload", hh.a.prompts)
	}
}

// TestPassthroughNeedsTheAgentToAdvertiseIt: allowlisted but not
// offered means the text is not a command at all.
func TestPassthroughNeedsTheAgentToAdvertiseIt(t *testing.T) {
	hh := dmCmdHarness(t, newAgent("x"), nil)
	hh.deliverDM(t, humanID, "!reload", humanID, botID)
	got := hh.only(t)
	if !strings.Contains(got, "Unknown command `!reload`") {
		t.Fatalf("reply = %q", got)
	}
}

// TestPassthroughRefusesCommandsOutsideTheAllowlist pins the
// deliberate exclusions: the relay owns the conversation→session
// mapping, so letting the agent switch its own session underneath it
// would desync that mapping.
func TestPassthroughRefusesCommandsOutsideTheAllowlist(t *testing.T) {
	agent := newAgent("x")
	for _, name := range []string{"resume", "continue", "name", "share", "export"} {
		agent.agentCmds = append(agent.agentCmds, client.CommandInfo{Name: name})
	}
	hh := dmCmdHarness(t, agent, nil)
	for _, name := range []string{"resume", "continue", "name", "share", "export"} {
		hh.z.reset()
		hh.a.prompts = nil
		hh.deliverDM(t, humanID, "!"+name, humanID, botID)
		if len(hh.a.prompts) != 0 {
			t.Fatalf("!%s was forwarded to the agent: %q", name, hh.a.prompts)
		}
		if !strings.Contains(hh.only(t), "Unknown command") {
			t.Fatalf("!%s: %q", name, hh.only(t))
		}
	}
}

// --- !status -------------------------------------------------------------

func TestStatusInAnEngagedTopic(t *testing.T) {
	hh := cmdHarness(t, newAgent("x"), func(c *Config) {
		c.Version = "9.9.9"
		c.AgentCmd = "fir --mode acp"
		c.StartTime = time.Unix(1000, 0)
		c.Now = func() time.Time { return time.Unix(1061, 0) }
	})
	hh.a.model = "anthropic/opus"
	hh.deliver(t, "hacking", mention("hello"))
	conv := hh.j.Convs()[0]
	hh.z.reset()

	hh.deliver(t, "hacking", "!status")
	got := hh.only(t)
	for _, want := range []string{
		`#fleet > "hacking"`,
		"`" + conv.ID + "`",
		"`" + filepath.Join(hh.s.dir, "convs", conv.ID) + "`",
		"`anthropic/opus`",
		"9.9.9",
		"fir --mode acp",
		"1m1s",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("status %q is missing %s", got, want)
		}
	}
	if strings.Contains(got, "turn running") {
		t.Fatalf("no turn is running: %q", got)
	}
}

// TestStatusFallsBackToIDsWithoutNames covers a DM whose
// display_recipient carries no full names.
func TestStatusFallsBackToIDsWithoutNames(t *testing.T) {
	hh := dmCmdHarness(t, newAgent("x"), nil)
	hh.h.Handle(context.Background(), namelessDM(humanID, "!status", humanID, botID))
	if !strings.Contains(hh.only(t), "DM 8,9") {
		t.Fatalf("status = %q", hh.only(t))
	}
}

func TestStatusReportsARunningTurn(t *testing.T) {
	agent := newAgent("x")
	agent.block = make(chan struct{})
	hh := cmdHarness(t, agent, nil)
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

// --- !new ----------------------------------------------------------------

// TestNewRetiresTheConversationInADM is the motivating gap: a DM is
// keyed on the participant set, so `!new` is the only way to start over.
func TestNewRetiresTheConversationInADM(t *testing.T) {
	hh := dmCmdHarness(t, newAgent("x"), nil)
	hh.deliverDM(t, humanID, "first", humanID, botID)
	old := hh.j.Convs()[0]
	hh.z.reset()

	hh.deliverDM(t, humanID, "!RESET", humanID, botID)
	if !strings.Contains(hh.only(t), "Fresh session") {
		t.Fatalf("reply = %q", hh.only(t))
	}
	fresh, ok := hh.j.Lookup(journal.DM([]int64{humanID, botID}))
	if !ok || fresh.ID == old.ID {
		t.Fatalf("conversation not retired: %+v", fresh)
	}
	hh.z.reset()
	hh.deliverDM(t, humanID, "second", humanID, botID)
	if _, ran := hh.s.sessions[fresh.ID]; !ran {
		t.Fatalf("next turn did not use the fresh conversation: %v", hh.s.sessions)
	}
}

func TestNewOnAConversationThatDoesNotExistYet(t *testing.T) {
	hh := dmCmdHarness(t, newAgent("x"), nil)
	hh.deliverDM(t, humanID, "!new", humanID, botID)
	if !strings.Contains(hh.only(t), "no conversation here yet") {
		t.Fatalf("reply = %q", hh.only(t))
	}
	if len(hh.j.Convs()) != 0 {
		t.Fatalf("`!new` allocated %+v", hh.j.Convs())
	}
}

// TestNewClearsTheTailSoTheNextTurnStartsClean pins the requirement
// that a retired conversation's tail message is forgotten.
func TestNewClearsTheTailSoTheNextTurnStartsClean(t *testing.T) {
	hh := dmCmdHarness(t, newAgent("x"), nil)
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
	hh := dmCmdHarness(t, agent, func(c *Config) { c.AckEmoji = "eyes" })
	hh.h.Handle(context.Background(), dmEvent(humanID, "go", humanID, botID))
	<-agent.entered

	hh.h.Handle(context.Background(), dmEvent(humanID, "!new", humanID, botID))
	hh.awaitTurnCleanup(t)
	if len(hh.s.cancels) == 0 {
		t.Fatal("`!new` left the running turn alone")
	}
}

func TestNewReportsAJournalFailure(t *testing.T) {
	hh := dmCmdHarness(t, newAgent("x"), nil)
	hh.deliverDM(t, humanID, "hello", humanID, botID)
	old := hh.j.Convs()[0]
	hh.z.reset()
	hh.breakJournal(t)
	hh.deliverDM(t, humanID, "!new", humanID, botID)
	if !strings.Contains(hh.only(t), "Couldn't reset") {
		t.Fatalf("reply = %q", hh.only(t))
	}
	if !hh.logged("retiring conversation") {
		t.Fatalf("logs = %q", hh.logs)
	}
	// The reply said it failed, so it must actually have failed.
	if got, ok := hh.j.Lookup(journal.DM([]int64{humanID, botID})); !ok || got.ID != old.ID {
		t.Fatalf("conversation = %+v, %v; want the unchanged %q", got, ok, old.ID)
	}
}

// --- !stop ---------------------------------------------------------------

func TestStopInterruptsTheRunningTurn(t *testing.T) {
	agent := newAgent("x")
	agent.block = make(chan struct{})
	hh := cmdHarness(t, agent, func(c *Config) { c.AckEmoji = "eyes" })
	hh.h.Handle(context.Background(), channelEvent(humanID, "hacking", mention("go")))
	<-agent.entered
	hh.z.reset()

	hh.h.Handle(context.Background(), channelEvent(humanID, "hacking", "!stop"))
	hh.awaitTurnCleanup(t)
	if !strings.Contains(hh.z.stored()[0], "Interrupted") {
		t.Fatalf("reply = %q", hh.z.stored())
	}
	if len(hh.s.cancels) == 0 {
		t.Fatal("`!stop` did not cancel the session")
	}
}

func TestStopWithNothingRunning(t *testing.T) {
	hh := dmCmdHarness(t, newAgent("x"), nil)
	hh.deliverDM(t, humanID, "!stop", humanID, botID)
	if !strings.Contains(hh.only(t), "Nothing is running") {
		t.Fatalf("reply = %q", hh.only(t))
	}
	hh.deliverDM(t, humanID, "hello", humanID, botID)
	hh.z.reset()
	hh.deliverDM(t, humanID, "!stop", humanID, botID)
	if !strings.Contains(hh.only(t), "Nothing is running") {
		t.Fatalf("reply = %q", hh.only(t))
	}
}

// TestCancelIsNotAnAliasForStop: `!login cancel` and `!cancel-login`
// own that word. A `!cancel` that sometimes aborts a login and
// sometimes kills a turn is the worst ambiguity in the one command a
// user reaches for when something has gone wrong.
func TestCancelIsNotAnAliasForStop(t *testing.T) {
	agent := newAgent("x")
	agent.block = make(chan struct{})
	hh := cmdHarness(t, agent, func(c *Config) { c.AckEmoji = "eyes" })
	hh.h.Handle(context.Background(), channelEvent(humanID, "hacking", mention("go")))
	<-agent.entered
	hh.z.reset()

	hh.h.Handle(context.Background(), channelEvent(humanID, "hacking", "!cancel-login"))
	if got := hh.only(t); strings.Contains(got, "Interrupted") {
		t.Fatalf("!cancel-login stopped the turn: %q", got)
	}
	if len(hh.s.cancels) != 0 {
		t.Fatalf("!cancel-login cancelled a turn: %v", hh.s.cancels)
	}
	close(agent.block)
	waitIdle(t, hh)
}

// --- !model --------------------------------------------------------------

func withModels(a *fakeAgent, current string, ids ...string) *fakeAgent {
	a.model = current
	for _, id := range ids {
		a.models = append(a.models, client.ModelInfo{ID: id, Name: id})
	}
	return a
}

func TestModelLists(t *testing.T) {
	hh := dmCmdHarness(t, withModels(newAgent("x"), "a/one", "a/one", "b/two"), nil)
	hh.deliverDM(t, humanID, "!model", humanID, botID)
	got := hh.only(t)
	for _, want := range []string{"2 models available", "`a/one`", "`b/two`"} {
		if !strings.Contains(got, want) {
			t.Fatalf("reply %q is missing %s", got, want)
		}
	}
}

func TestModelSwitchNeedsAConversation(t *testing.T) {
	hh := dmCmdHarness(t, withModels(newAgent("x"), "a/one", "a/one", "b/two"), nil)
	hh.deliverDM(t, humanID, "!model b/two", humanID, botID)
	if !strings.Contains(hh.only(t), "no conversation here yet") {
		t.Fatalf("reply = %q", hh.only(t))
	}
}

// TestModelSwitchAppliesOnTheNextTurn: the choice is recorded now and
// pushed to the ACP session when the next turn opens it.
func TestModelSwitchAppliesOnTheNextTurn(t *testing.T) {
	hh := dmCmdHarness(t, withModels(newAgent("x"), "a/one", "a/one", "b/two"), nil)
	hh.deliverDM(t, humanID, "hello", humanID, botID)
	conv := hh.j.Convs()[0]
	hh.z.reset()

	hh.deliverDM(t, humanID, "!model b/two", humanID, botID)
	if !strings.Contains(hh.only(t), "b/two") {
		t.Fatalf("reply = %q", hh.only(t))
	}
	if got := hh.a.selections(); len(got) != 0 {
		t.Fatalf("the switch was pushed outside a turn: %v", got)
	}
	hh.z.reset()

	hh.deliverDM(t, humanID, "!status", humanID, botID)
	if !strings.Contains(hh.only(t), "b/two") {
		t.Fatalf("status = %q", hh.only(t))
	}
	hh.z.reset()

	hh.deliverDM(t, humanID, "again", humanID, botID)
	hh.deliverDM(t, humanID, "and again", humanID, botID)
	want := "sid-" + conv.ID + "=b/two"
	if got := hh.a.selections(); len(got) != 1 || got[0] != want {
		t.Fatalf("selections = %v, want [%s]", got, want)
	}
}

func TestModelRejectsAnUnknownID(t *testing.T) {
	hh := dmCmdHarness(t, withModels(newAgent("x"), "a/one", "a/one", "b/two"), nil)
	hh.deliverDM(t, humanID, "hello", humanID, botID)
	hh.z.reset()
	// Not an exact id, so the broker treats it as a list filter.
	hh.deliverDM(t, humanID, "!model nope", humanID, botID)
	if got := hh.only(t); !strings.Contains(got, "none match") && !strings.Contains(got, "0 model") {
		t.Fatalf("reply = %q", got)
	}
	if _, ok := hh.h.modelOverride(hh.j.Convs()[0].ID); ok {
		t.Fatal("a non-matching filter set an override")
	}
}

func TestModelSwitchFailureIsLoggedAndTheTurnContinues(t *testing.T) {
	hh := dmCmdHarness(t, withModels(newAgent("answer"), "a/one", "a/one", "b/two"), nil)
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
	hh := dmCmdHarness(t, withModels(newAgent("x"), "a/one", "a/one", "b/two"), nil)
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

// TestModelChoiceSurvivesASessionSwap covers idle GC: a reaped session
// comes back with a new id, so the choice must be pushed again.
func TestModelChoiceSurvivesASessionSwap(t *testing.T) {
	hh := dmCmdHarness(t, withModels(newAgent("x"), "a/one", "a/one", "b/two"), nil)
	hh.deliverDM(t, humanID, "hello", humanID, botID)
	conv := hh.j.Convs()[0]
	hh.deliverDM(t, humanID, "!model b/two", humanID, botID)
	hh.deliverDM(t, humanID, "again", humanID, botID)

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
	hh := dmCmdHarness(t, withModels(newAgent("x"), "a/one", "a/one", "b/two"), nil)
	hh.h.setModelOverride("c1", "b/two")
	hh.h.applyModel(context.Background(), "c1", "sid-1")
	hh.h.setModelOverride("c1", "a/one")
	hh.h.applyModel(context.Background(), "c1", "sid-1")
	if got := hh.a.selections(); len(got) != 2 {
		t.Fatalf("selections = %v, want both pushes", got)
	}
	hh.h.applyModel(context.Background(), "c1", "sid-1")
	if got := hh.a.selections(); len(got) != 2 {
		t.Fatalf("selections = %v, want no extra push", got)
	}
}

func TestApplyModelWithNoChoice(t *testing.T) {
	hh := dmCmdHarness(t, newAgent("x"), nil)
	hh.h.applyModel(context.Background(), "c-nothing", "sid-1")
	if got := hh.a.selections(); len(got) != 0 {
		t.Fatalf("selections = %v", got)
	}
}

// --- !login --------------------------------------------------------------

// TestLoginFlow drives the two-call bridge end to end: the first turn
// yields a URL, the next turn from the same conversation submits the
// pasted redirect.
func TestLoginFlow(t *testing.T) {
	agent := newAgent("x")
	agent.authMethods = []client.AuthMethod{{ID: "oauth-anthropic", Name: "Anthropic"}}
	agent.authResult = client.AuthResult{State: "needs_redirect", URL: "https://example/auth", ID: "a1"}
	hh := dmCmdHarness(t, agent, nil)

	hh.deliverDM(t, humanID, "!login anthropic", humanID, botID)
	if !strings.Contains(hh.only(t), "https://example/auth") {
		t.Fatalf("login start: %q", hh.only(t))
	}
	hh.z.reset()

	// The pasted URL is not sigil-prefixed, so only the pending-login
	// state can recognise it — and it must not reach the agent as prose.
	agent.authResult = client.AuthResult{State: "ok"}
	hh.deliverDM(t, humanID, "https://example/callback?code=xyz", humanID, botID)
	if !strings.Contains(hh.only(t), "Authenticated") {
		t.Fatalf("login finish: %q", hh.only(t))
	}
	if len(hh.a.prompts) != 0 {
		t.Fatalf("the pasted redirect reached the agent: %q", hh.a.prompts)
	}
}

// TestLoginIsPerConversation: a pending login in one DM must not
// swallow an unrelated message in a different topic.
func TestLoginIsPerConversation(t *testing.T) {
	agent := newAgent("answer")
	agent.authMethods = []client.AuthMethod{{ID: "oauth-anthropic", Name: "Anthropic"}}
	agent.authResult = client.AuthResult{State: "needs_redirect", URL: "https://example/auth", ID: "a1"}
	hh := dmCmdHarness(t, agent, nil)

	hh.deliverDM(t, humanID, "!login anthropic", humanID, botID)
	hh.z.reset()

	// A different conversation entirely — an ordinary channel turn.
	hh.deliver(t, "hacking", mention("hello there"))
	if len(hh.a.prompts) != 1 {
		t.Fatalf("the channel turn was eaten by the DM's pending login: %q", hh.a.prompts)
	}
}

func TestLoginErrorIsReported(t *testing.T) {
	agent := newAgent("x")
	agent.authMethods = []client.AuthMethod{{ID: "oauth-anthropic", Name: "Anthropic"}}
	agent.authErr = errors.New("agent exploded")
	hh := dmCmdHarness(t, agent, nil)
	hh.deliverDM(t, humanID, "!login anthropic", humanID, botID)
	if !strings.Contains(hh.only(t), "Command failed") {
		t.Fatalf("reply = %q", hh.only(t))
	}
	if !hh.logged("agent exploded") {
		t.Fatalf("logs = %q", hh.logs)
	}
}

func TestCommandReplyFailureIsLogged(t *testing.T) {
	hh := dmCmdHarness(t, newAgent("x"), nil)
	hh.z.sendErr = errors.New("boom")
	hh.deliverDM(t, humanID, "!help", humanID, botID)
	if !hh.logged("posting command reply") {
		t.Fatalf("logs = %q", hh.logs)
	}
}

// --- Controller edge cases ------------------------------------------------

// TestControllerRejectsAMalformedToken: the token is relay-generated,
// so a bad one is a programming error — logged, never surfaced, and
// never silently attached to the wrong conversation.
func TestControllerRejectsAMalformedToken(t *testing.T) {
	hh := dmCmdHarness(t, newAgent("x"), nil)
	if got := hh.h.StatusFor("garbage"); got.ConvID != "" {
		t.Fatalf("StatusFor(garbage) = %+v", got)
	}
	if got := hh.h.RelayInfo("garbage"); got.SessionID != "" {
		t.Fatalf("RelayInfo(garbage) = %+v", got)
	}
	if err := hh.h.SetModelOverride("garbage", "m"); err == nil {
		t.Fatal("SetModelOverride accepted a malformed token")
	}
	if err := hh.h.ResetSession("garbage"); err == nil {
		t.Fatal("ResetSession accepted a malformed token")
	}
	if hh.h.StopTurn("garbage") {
		t.Fatal("StopTurn accepted a malformed token")
	}
	if !hh.logged("malformed conversation token") {
		t.Fatalf("logs = %q", hh.logs)
	}
}

// TestSetModelOverrideWithNoModelList: an agent that has not reported
// its catalog yet must not block a switch, or the very first `!model`
// after a cold start would always fail.
func TestSetModelOverrideWithNoModelList(t *testing.T) {
	hh := dmCmdHarness(t, newAgent("x"), nil)
	hh.deliverDM(t, humanID, "hello", humanID, botID)
	key := journal.DM([]int64{humanID, botID})
	if err := hh.h.SetModelOverride(key.Token(), "anything/goes"); err != nil {
		t.Fatalf("SetModelOverride: %v", err)
	}
	if id, _ := hh.h.modelOverride(hh.j.Convs()[0].ID); id != "anything/goes" {
		t.Fatalf("override = %q", id)
	}
}

func TestWhereForAZeroKey(t *testing.T) {
	hh := dmCmdHarness(t, newAgent("x"), nil)
	if got := hh.h.whereFor(journal.Key{}); got != "" {
		t.Fatalf("whereFor(zero) = %q, want empty", got)
	}
}

// TestReplyIgnoresEmptyContent: a command that produced nothing to say
// must not post an empty message into the topic.
func TestReplyIgnoresEmptyContent(t *testing.T) {
	hh := dmCmdHarness(t, newAgent("x"), nil)
	hh.h.reply(context.Background(), journal.DM([]int64{humanID, botID}), "   \n ")
	if hh.z.count() != 0 {
		t.Fatalf("empty reply was posted: %q", hh.z.stored())
	}
}

// TestSetModelOverrideRejectsAnUnknownID: the broker only calls this
// on an exact id match, so drive the guard directly — it is the last
// thing standing between a typo and a session pinned to a model the
// agent does not have.
func TestSetModelOverrideRejectsAnUnknownID(t *testing.T) {
	hh := dmCmdHarness(t, withModels(newAgent("x"), "a/one", "a/one"), nil)
	hh.deliverDM(t, humanID, "hello", humanID, botID)
	key := journal.DM([]int64{humanID, botID})
	err := hh.h.SetModelOverride(key.Token(), "b/nope")
	if err == nil || !strings.Contains(err.Error(), "unknown model") {
		t.Fatalf("SetModelOverride = %v, want an unknown-model error", err)
	}
	if _, ok := hh.h.modelOverride(hh.j.Convs()[0].ID); ok {
		t.Fatal("a rejected id was recorded anyway")
	}
}

// TestEscapeWinsOverAPendingLogin pins a deliberate ordering choice.
// Someone typing "!!foo" mid-login is plainly not pasting a redirect
// URL, so the escape is honoured — the text reaches the agent as
// "!foo" and the login stays pending for the paste that follows.
// Consuming it as a malformed redirect would abort the login instead.
func TestEscapeWinsOverAPendingLogin(t *testing.T) {
	agent := newAgent("ok")
	agent.authMethods = []client.AuthMethod{{ID: "oauth-anthropic", Name: "Anthropic"}}
	agent.authResult = client.AuthResult{State: "needs_redirect", URL: "https://example/auth", ID: "a1"}
	hh := dmCmdHarness(t, agent, nil)

	hh.deliverDM(t, humanID, "!login anthropic", humanID, botID)
	hh.z.reset()

	hh.deliverDM(t, humanID, "!!not a url", humanID, botID)
	if len(hh.a.prompts) != 1 || !strings.Contains(hh.a.prompts[0], "!not a url") {
		t.Fatalf("escape was eaten by the pending login: %q", hh.a.prompts)
	}
	// The login is still pending, so the real paste still completes it.
	agent.authResult = client.AuthResult{State: "ok"}
	hh.z.reset()
	hh.deliverDM(t, humanID, "https://example/callback?code=xyz", humanID, botID)
	if !strings.Contains(hh.only(t), "Authenticated") {
		t.Fatalf("login did not survive the escape: %q", hh.only(t))
	}
}

// TestActiveConversationCountExcludesRetired: retired entries stay in
// the journal as the record of which state directories are dead, so
// counting them would make `!status` report a number that only ever
// grows with every `!new`.
func TestActiveConversationCountExcludesRetired(t *testing.T) {
	hh := dmCmdHarness(t, newAgent("x"), nil)
	hh.deliverDM(t, humanID, "hello", humanID, botID)
	hh.deliverDM(t, humanID, "!new", humanID, botID)
	hh.deliverDM(t, humanID, "!new", humanID, botID)
	if got := len(hh.j.Convs()); got != 3 {
		t.Fatalf("journal holds %d convs, want 3 (one live, two retired)", got)
	}
	hh.z.reset()
	hh.deliverDM(t, humanID, "!status", humanID, botID)
	if got := hh.only(t); !strings.Contains(got, "active conversations: 1") {
		t.Fatalf("status = %q, want one active conversation", got)
	}
}

// TestUnknownCommandAcceptsDigitsAndPunctuation: a command NAME may
// contain digits, "_" and "-" after its first letter, so those must be
// reported as unknown commands rather than forwarded as prose. The
// shape check is the only thing standing between "!important: fix
// this" and a swallowed message, so both sides of it are pinned.
func TestUnknownCommandAcceptsDigitsAndPunctuation(t *testing.T) {
	for _, text := range []string{"!fix-2", "!step_3", "!v2"} {
		name, ok := unknownCommand(text)
		if !ok {
			t.Fatalf("%q should be command-shaped", text)
		}
		if name != strings.TrimPrefix(text, "!") {
			t.Fatalf("unknownCommand(%q) = %q", text, name)
		}
	}
	for _, text := range []string{"!important: fix this", "!2fast", "!", "hello"} {
		if _, ok := unknownCommand(text); ok {
			t.Fatalf("%q must NOT be treated as a command", text)
		}
	}
}
