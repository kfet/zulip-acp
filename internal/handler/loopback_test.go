package handler

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kfet/acp-kit/command"
	"github.com/kfet/acp-kit/relaytool"
	"github.com/kfet/acp-kit/schedule"
	"github.com/kfet/zulip-acp/internal/journal"
)

var fireEpoch = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

// loopHarness is a cmdHarness with the schedule store and the loopback
// tool set wired in, which is how the relay actually runs with
// `relay_mcp` on.
type loopHarness struct {
	*harness
	store *schedule.Store
	tools *relaytool.Tools
	ticks chan time.Time
	now   func() time.Time
	done  chan struct{}
	stop  context.CancelFunc
}

func newLoopHarness(t *testing.T, agent *fakeAgent, tune func(*Config)) *loopHarness {
	t.Helper()
	lh := &loopHarness{ticks: make(chan time.Time), now: func() time.Time { return fireEpoch }}
	var h *Handler
	store, err := schedule.Open(schedule.Config{
		Path:  filepath.Join(t.TempDir(), "schedules.json"),
		Ticks: lh.ticks,
		Now:   func() time.Time { return lh.now() },
		Fire:  func(ctx context.Context, it schedule.Item) error { return h.FireSchedule(ctx, it) },
		Logf:  func(string, ...any) {},
	})
	if err != nil {
		t.Fatalf("schedule.Open: %v", err)
	}
	lh.store = store

	broker := command.New(agent)
	tools, err := relaytool.New(relaytool.Config{
		Broker:    broker,
		ConvToken: func(k string) (string, bool) { return h.ConvToken(k) },
	})
	if err != nil {
		t.Fatalf("relaytool.New: %v", err)
	}
	lh.tools = tools

	lh.harness = newHarness(t, agent, func(c *Config) {
		c.Commands = broker
		c.Schedules = store
		c.Loopback = tools
		c.DMs = true
		if tune != nil {
			tune(c)
		}
	})
	h = lh.harness.h
	return lh
}

// run starts the store's fire loop for the duration of the test.
func (lh *loopHarness) run(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	lh.stop = cancel
	lh.done = make(chan struct{})
	go func() {
		lh.store.Run(ctx)
		close(lh.done)
	}()
	t.Cleanup(func() {
		cancel()
		<-lh.done
	})
}

// token is the broker conversation token for a channel topic.
func chanToken(topic string) string { return journal.Channel(4, topic).Token() }

// engage drives one ordinary turn so the topic has a conversation.
func (lh *loopHarness) engage(t *testing.T, topic string) journal.Conv {
	t.Helper()
	lh.deliver(t, topic, mention("hello"))
	c, ok := lh.j.Lookup(journal.Channel(4, topic))
	if !ok {
		t.Fatalf("topic %q did not become a conversation", topic)
	}
	lh.z.reset()
	// Drain the agent's turn signal so a later wait cannot satisfy
	// itself with this turn's.
	select {
	case <-lh.a.entered:
	default:
	}
	return c
}

// --- identity ------------------------------------------------------------

// TestConvTokenIsDerivedNotSupplied pins the guarantee the whole design
// rests on: a tool call names no conversation, and an unknown session
// key is refused rather than guessed.
func TestConvTokenIsDerivedNotSupplied(t *testing.T) {
	lh := newLoopHarness(t, newAgent("ok"), nil)
	conv := lh.engage(t, "loopback")

	got, ok := lh.h.ConvToken(conv.ID)
	if !ok || got != chanToken("loopback") {
		t.Fatalf("ConvToken(%q) = %q, %v", conv.ID, got, ok)
	}
	if _, ok := lh.h.ConvToken("cdeadbeef"); ok {
		t.Fatal("an unknown conv-id must be refused, not guessed")
	}
	if !lh.logged("unknown conversation") {
		t.Fatal("the refusal was not logged")
	}
}

// TestConvTokenSurvivesNew: `!new` replaces the conv-id but not the
// key, so a session created before the reset still resolves — which is
// exactly why the broker is addressed by key token.
func TestConvTokenSurvivesNew(t *testing.T) {
	lh := newLoopHarness(t, newAgent("ok"), nil)
	conv := lh.engage(t, "loopback")
	if err := lh.h.ResetSession(chanToken("loopback")); err != nil {
		t.Fatalf("ResetSession: %v", err)
	}
	got, ok := lh.h.ConvToken(conv.ID)
	if !ok || got != chanToken("loopback") {
		t.Fatalf("retired conv-id no longer resolves: %q, %v", got, ok)
	}
}

// TestConvKeyResolvesTheZulipConversation: the Zulip-specific loopback
// tools (`history`) need the journal.Key, not the broker token, and
// must refuse a session key the journal does not know.
func TestConvKeyResolvesTheZulipConversation(t *testing.T) {
	lh := newLoopHarness(t, newAgent("ok"), nil)
	conv := lh.engage(t, "loopback")

	got, ok := lh.h.ConvKey(conv.ID)
	if !ok || got.StreamID != 4 || got.Topic != "loopback" {
		t.Fatalf("ConvKey(%q) = %+v, %v", conv.ID, got, ok)
	}
	if _, ok := lh.h.ConvKey("cdeadbeef"); ok {
		t.Fatal("an unknown conv-id must be refused, not guessed")
	}
	if !lh.logged("unknown conversation") {
		t.Fatal("the refusal was not logged")
	}
}

// TestConvKeyResolvesADM: a DM key carries the participant set, which
// is what the `dm` narrow is built from.
func TestConvKeyResolvesADM(t *testing.T) {
	lh := newLoopHarness(t, newAgent("ok"), nil)
	conv, err := lh.j.Ensure(journal.DM([]int64{7, botID}))
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	got, ok := lh.h.ConvKey(conv.ID)
	if !ok || !got.IsDM() || len(got.UserIDs) != 2 {
		t.Fatalf("ConvKey(%q) = %+v, %v", conv.ID, got, ok)
	}
}

// --- post ----------------------------------------------------------------

func TestPostToChannelAndDM(t *testing.T) {
	lh := newLoopHarness(t, newAgent("ok"), nil)
	if err := lh.h.PostTo(chanToken("loopback"), "the deploy landed"); err != nil {
		t.Fatalf("PostTo: %v", err)
	}
	if got := lh.z.stored(); len(got) != 1 || !strings.Contains(got[0], "the deploy landed") {
		t.Fatalf("channel post = %q", got)
	}
	lh.z.reset()

	dm := journal.DM([]int64{humanID, botID}).Token()
	if err := lh.h.PostTo(dm, "and in a DM"); err != nil {
		t.Fatalf("PostTo(dm): %v", err)
	}
	if got := lh.z.stored(); len(got) != 1 || !strings.Contains(got[0], "and in a DM") {
		t.Fatalf("dm post = %q", got)
	}
}

// TestPostGoesThroughTheSplitter: Zulip truncates past
// MAX_MESSAGE_LENGTH silently, so an out-of-band post must roll over
// exactly like an answer does.
func TestPostGoesThroughTheSplitter(t *testing.T) {
	lh := newLoopHarness(t, newAgent("ok"), func(c *Config) { c.Budget = 200 })
	if err := lh.h.PostTo(chanToken("loopback"), strings.Repeat("x", 600)); err != nil {
		t.Fatalf("PostTo: %v", err)
	}
	got := lh.z.stored()
	if len(got) < 2 {
		t.Fatalf("a 600-char post fitted in %d message(s) with a 200 budget: %v", len(got), got)
	}
	for i, m := range got {
		if n := len([]rune(m)); n > 200 {
			t.Fatalf("message %d is %d code points, over budget", i, n)
		}
	}
}

func TestPostRejectsBadInput(t *testing.T) {
	lh := newLoopHarness(t, newAgent("ok"), nil)
	if err := lh.h.PostTo("not-a-token", "hi"); err == nil {
		t.Fatal("want a token parse error")
	}
	// A splitter that cannot be built (budget smaller than its own
	// markers) fails before anything is posted.
	lh2 := newLoopHarness(t, newAgent("ok"), func(c *Config) { c.Budget = 1; c.SealMarker = strings.Repeat("~", 50) })
	if err := lh2.h.PostTo(chanToken("loopback"), "hi"); err == nil {
		t.Fatal("want a splitter construction error")
	}
	lh.z.mu.Lock()
	lh.z.sendErr = errors.New("zulip down")
	lh.z.mu.Unlock()
	if err := lh.h.PostTo(chanToken("loopback"), "hi"); err == nil {
		t.Fatal("want the send error surfaced")
	}
}

// TestLoopbackPostDoesNotFeedItselfBack is the loop hazard, made
// explicit. The agent posts; Zulip delivers that message back as an
// event; the own-sender guard must drop it before anything else. That
// guard was always there — the loopback is what makes it load-bearing.
func TestLoopbackPostDoesNotFeedItselfBack(t *testing.T) {
	lh := newLoopHarness(t, newAgent("ok"), nil)
	lh.engage(t, "loopback")
	before := len(lh.a.prompts)

	if err := lh.h.PostTo(chanToken("loopback"), mention("I am talking to myself")); err != nil {
		t.Fatalf("PostTo: %v", err)
	}
	// Zulip echoes the bot's own message into the event stream — with
	// an @-mention of the bot, in a topic the relay is engaged in, so
	// EVERY other gate would let it through.
	lh.h.Handle(context.Background(), channelEvent(botID, "loopback", mention("I am talking to myself")))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := lh.h.WaitIdle(ctx); err != nil {
		t.Fatalf("WaitIdle: %v", err)
	}
	if got := len(lh.a.prompts); got != before {
		t.Fatalf("the relay answered its own post: %d new prompt(s): %q", got-before, lh.a.prompts)
	}
}

// --- scheduling ----------------------------------------------------------

func TestScheduleRequiresTheStore(t *testing.T) {
	hh := cmdHarness(t, newAgent("ok"), nil) // no Schedules wired
	if _, err := hh.h.Schedule(chanToken("t"), "x", fireEpoch, 0); err == nil {
		t.Fatal("want refusal without a store")
	}
	if got := hh.h.Schedules(chanToken("t")); got != nil {
		t.Fatalf("want no schedules, got %v", got)
	}
	if err := hh.h.Unschedule(chanToken("t"), "s1"); err == nil {
		t.Fatal("want refusal without a store")
	}
}

func TestScheduleRejectsAMalformedToken(t *testing.T) {
	lh := newLoopHarness(t, newAgent("ok"), nil)
	if _, err := lh.h.Schedule("not-a-token", "x", fireEpoch.Add(time.Hour), 0); err == nil {
		t.Fatal("want a token parse error")
	}
}

func TestScheduleAddListRemove(t *testing.T) {
	lh := newLoopHarness(t, newAgent("ok"), nil)
	tok := chanToken("loopback")
	it, err := lh.h.Schedule(tok, "check the deploy", fireEpoch.Add(time.Hour), 0)
	if err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	if got := lh.h.Schedules(tok); len(got) != 1 || got[0].ID != it.ID {
		t.Fatalf("Schedules = %v", got)
	}
	if got := lh.h.Schedules(chanToken("elsewhere")); len(got) != 0 {
		t.Fatal("schedules leaked into another conversation")
	}
	if err := lh.h.Unschedule(tok, it.ID); err != nil {
		t.Fatalf("Unschedule: %v", err)
	}
}

// TestFireSchedulePromptsTheConversation is the loopback's payoff: the
// scheduled prompt re-enters the SAME conversation, so the agent has
// the topic's history and the answer streams into that topic.
func TestFireSchedulePromptsTheConversation(t *testing.T) {
	lh := newLoopHarness(t, newAgent("the deploy landed"), nil)
	conv := lh.engage(t, "loopback")
	lh.run(t)

	if _, err := lh.h.Schedule(chanToken("loopback"), "did it land?", fireEpoch.Add(time.Minute), 0); err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	lh.now = func() time.Time { return fireEpoch.Add(2 * time.Minute) }
	lh.ticks <- fireEpoch

	// The scheduled turn has started once the agent is prompted again.
	<-lh.a.entered
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := lh.h.WaitIdle(ctx); err != nil {
		t.Fatalf("WaitIdle: %v", err)
	}
	// Let the fire goroutine finish unwinding before reading the fakes.
	lh.stop()
	<-lh.done

	last := lh.a.prompts[len(lh.a.prompts)-1]
	if !strings.HasPrefix(last, "["+scheduledSender+"] ") || !strings.Contains(last, "did it land?") {
		t.Fatalf("scheduled prompt = %q", last)
	}
	if !strings.Contains(strings.Join(lh.z.stored(), "\n"), "the deploy landed") {
		t.Fatalf("the answer did not land in the topic: %q", lh.z.stored())
	}
	// Same session, so the agent keeps the topic's context.
	if got := lh.sessionCount(conv.ID); got != 1 {
		t.Fatalf("a scheduled turn created %d sessions, want 1 (reused)", got)
	}
	// No :eyes: acknowledgement: a scheduled turn has no triggering
	// message to react to.
	added, _ := lh.z.reactions()
	for _, a := range added {
		if strings.HasPrefix(a, "0:") {
			t.Fatalf("reacted to message 0: %v", added)
		}
	}
}

// TestFireScheduleReAppliesTheGates: a scheduled turn has no human in
// the loop, so every gate an interactive turn passes is re-checked at
// fire time, and a failure disarms the item instead of retrying it.
func TestFireScheduleReAppliesTheGates(t *testing.T) {
	cases := []struct {
		name string
		conv string
		tune func(*Config)
	}{
		{name: "malformed token", conv: "not-a-token"},
		{name: "channel no longer served", conv: journal.Channel(99, "gone").Token()},
		{name: "dms no longer enabled", conv: journal.DM([]int64{humanID, botID}).Token(),
			tune: func(c *Config) { c.DMs = false }},
		{name: "no conversation", conv: chanToken("never-engaged")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lh := newLoopHarness(t, newAgent("ok"), tc.tune)
			err := lh.h.FireSchedule(context.Background(), schedule.Item{ID: "s1", Conv: tc.conv, Text: "x", Depth: 1})
			if !errors.Is(err, schedule.ErrGone) {
				t.Fatalf("err = %v, want ErrGone", err)
			}
			if len(lh.a.prompts) != 0 {
				t.Fatalf("a gated schedule still prompted: %q", lh.a.prompts)
			}
		})
	}
}

// TestFireScheduleWaitsForAHumanTurn: a human turn must never be
// superseded by a scheduled one.
func TestFireScheduleWaitsForAHumanTurn(t *testing.T) {
	agent := newAgent("answer")
	agent.block = make(chan struct{})
	waiting := make(chan string, 4)
	lh := newLoopHarness(t, agent, func(c *Config) {
		c.OnWaitForConv = func(id string) {
			select {
			case waiting <- id:
			default:
			}
		}
	})

	// Engage the topic with a turn that is still running.
	lh.h.Handle(context.Background(), channelEvent(humanID, "busy", mention("hello")))
	<-agent.entered

	conv, ok := lh.j.Lookup(journal.Channel(4, "busy"))
	if !ok {
		t.Fatal("conversation was not allocated")
	}
	// The human turn is registered as inflight before Handle returns.
	if !lh.h.isInflight(conv.ID) {
		t.Fatal("the human turn is not inflight")
	}
	fired := make(chan error, 1)
	go func() {
		fired <- lh.h.FireSchedule(context.Background(),
			schedule.Item{ID: "s1", Conv: chanToken("busy"), Text: "later", Depth: 1})
	}()

	// The scheduled turn has parked behind the human one. Only now is
	// it safe to let the human turn finish.
	if got := <-waiting; got != conv.ID {
		t.Fatalf("waiting on %q, want %q", got, conv.ID)
	}
	close(agent.block)
	if err := <-fired; err != nil {
		t.Fatalf("FireSchedule: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := lh.h.WaitIdle(ctx); err != nil {
		t.Fatalf("WaitIdle: %v", err)
	}
	if len(lh.a.prompts) != 2 {
		t.Fatalf("prompts = %q, want the human's then the scheduled one", lh.a.prompts)
	}
	// The decisive property: the scheduled turn WAITED rather than
	// superseding, so nothing was ever cancelled.
	if got := lh.cancelled(); len(got) != 0 {
		t.Fatalf("a scheduled turn superseded a human one: %v", got)
	}
}

func TestClaimConvIdleHonoursContext(t *testing.T) {
	lh := newLoopHarness(t, newAgent("ok"), nil)
	lh.h.setInflight("cbusy", &inflightEntry{cancel: func() {}})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := lh.h.claimConvIdle(ctx, "cbusy", &inflightEntry{cancel: func() {}}); err == nil {
		t.Fatal("want the context error")
	}
}

// --- deferred new_session ------------------------------------------------

// TestDeferredNewSessionAppliesAfterTheTurn: the agent asking for a
// fresh session must not cancel the turn that asked.
func TestDeferredNewSessionAppliesAfterTheTurn(t *testing.T) {
	lh := newLoopHarness(t, newAgent("done"), nil)
	conv := lh.engage(t, "loopback")

	// Arm the deferred reset the way the tool does, then run a turn.
	if _, err := callTool(t, lh.tools, relaytool.ToolNewSession, conv.ID, `{}`); err != nil {
		t.Fatalf("new_session: %v", err)
	}
	if _, ok := lh.j.Lookup(journal.Channel(4, "loopback")); !ok {
		t.Fatal("the conversation vanished before the turn ended")
	}
	lh.deliver(t, "loopback", "carry on")

	fresh, ok := lh.j.Lookup(journal.Channel(4, "loopback"))
	if !ok {
		t.Fatal("no conversation after the deferred reset")
	}
	if fresh.ID == conv.ID {
		t.Fatal("the deferred new_session was never applied")
	}
}

func TestEndTurnWithoutLoopbackIsANoOp(t *testing.T) {
	hh := cmdHarness(t, newAgent("ok"), nil)
	hh.h.endTurn(journal.Conv{ID: "c1", Key: journal.Channel(4, "t")})
}

// callTool invokes a relaytool tool by name, the way mcphost would.
func callTool(t *testing.T, tools *relaytool.Tools, name, sessionKey, args string) (string, error) {
	t.Helper()
	for _, x := range tools.Tools() {
		if x.Name == name {
			return x.Handler(sessionKey, []byte(args))
		}
	}
	t.Fatalf("tool %q is not registered", name)
	return "", nil
}

// --- human oversight -----------------------------------------------------

// TestSchedulesAreVisibleAndKillableFromChat: armed work an agent
// created must be inspectable and cancellable by a human, or the
// runaway bounds are the only thing standing between a loop and a bill.
func TestSchedulesAreVisibleAndKillableFromChat(t *testing.T) {
	lh := newLoopHarness(t, newAgent("ok"), nil)
	lh.engage(t, "loopback")
	it, err := lh.h.Schedule(chanToken("loopback"), "check the deploy", fireEpoch.Add(time.Hour), 0)
	if err != nil {
		t.Fatalf("Schedule: %v", err)
	}

	lh.deliver(t, "loopback", "!schedules")
	listing := lh.only(t)
	if !strings.Contains(listing, it.ID) || !strings.Contains(listing, "check the deploy") {
		t.Fatalf("!schedules = %q", listing)
	}
	lh.z.reset()

	lh.deliver(t, "loopback", "!unschedule "+it.ID)
	if got := lh.only(t); !strings.Contains(got, "Cancelled") {
		t.Fatalf("!unschedule = %q", got)
	}
	if got := lh.h.Schedules(chanToken("loopback")); len(got) != 0 {
		t.Fatalf("the schedule survived: %v", got)
	}
}

// TestStatusReportsArmedSchedules: `!status` says how many schedules
// are armed, so a human notices work they did not start.
func TestStatusReportsArmedSchedules(t *testing.T) {
	lh := newLoopHarness(t, newAgent("ok"), nil)
	lh.engage(t, "loopback")
	if _, err := lh.h.Schedule(chanToken("loopback"), "later", fireEpoch.Add(time.Hour), 0); err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	lh.deliver(t, "loopback", "!status")
	if got := lh.only(t); !strings.Contains(got, "schedules armed here: 1") {
		t.Fatalf("!status = %q", got)
	}
}

// sessionCount reads how many sessions were created for a key, under
// the fake's own lock.
func (lh *loopHarness) sessionCount(key string) int {
	lh.s.mu.Lock()
	defer lh.s.mu.Unlock()
	return lh.s.created[key]
}

// cancelled reports the conversation keys the session manager was asked
// to cancel — i.e. the turns something superseded.
func (lh *loopHarness) cancelled() []string {
	lh.s.mu.Lock()
	defer lh.s.mu.Unlock()
	return append([]string(nil), lh.s.cancels...)
}

// TestFireScheduleGivesUpWhenCancelled: a shutdown while a scheduled
// turn is queued behind a human one must abandon the wait, not hang the
// store's goroutine forever.
func TestFireScheduleGivesUpWhenCancelled(t *testing.T) {
	waiting := make(chan string, 4)
	lh := newLoopHarness(t, newAgent("ok"), func(c *Config) {
		c.OnWaitForConv = func(id string) {
			select {
			case waiting <- id:
			default:
			}
		}
	})
	conv := lh.engage(t, "busy")
	// Occupy the conversation without running a turn, so the wait is
	// the only thing under test.
	lh.h.setInflight(conv.ID, &inflightEntry{cancel: func() {}})

	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		errc <- lh.h.FireSchedule(ctx, schedule.Item{ID: "s1", Conv: chanToken("busy"), Text: "later", Depth: 1})
	}()
	// Cancel only once the wait has actually parked, so the wake-up
	// path is exercised rather than the fast "already cancelled" exit.
	<-waiting
	cancel()
	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if len(lh.a.prompts) != 1 {
		t.Fatalf("a cancelled wait still prompted: %q", lh.a.prompts)
	}
}

// TestPostToIsBounded: the tool call is answered only when PostTo
// returns, so a wedged Zulip request must not hang the agent's turn
// forever.
func TestPostToIsBounded(t *testing.T) {
	lh := newLoopHarness(t, newAgent("ok"), func(c *Config) { c.PromptTimeout = time.Millisecond })
	lh.z.mu.Lock()
	lh.z.sendErr = context.DeadlineExceeded
	lh.z.mu.Unlock()
	if err := lh.h.PostTo(chanToken("loopback"), "hi"); err == nil {
		t.Fatal("want the send failure surfaced rather than a hang")
	}
}

// TestFireScheduleClaimsAtomically: the wait for an idle conversation
// and the claim of it happen under one lock, so a message arriving in
// the gap cannot have its turn silently displaced.
func TestFireScheduleClaimsAtomically(t *testing.T) {
	lh := newLoopHarness(t, newAgent("ok"), nil)
	conv := lh.engage(t, "loopback")
	done := make(chan error, 1)
	go func() {
		done <- lh.h.FireSchedule(context.Background(),
			schedule.Item{ID: "s1", Conv: chanToken("loopback"), Text: "later", Depth: 1})
	}()
	if err := <-done; err != nil {
		t.Fatalf("FireSchedule: %v", err)
	}
	// The claim is released again, so the next turn is not blocked.
	if lh.h.isInflight(conv.ID) {
		t.Fatal("the scheduled turn left its claim behind")
	}
}

// TestSchedulingSurfaceHiddenWithoutTheStore: with `relay_mcp` off the
// relay must not advertise commands nothing can serve.
func TestSchedulingSurfaceHiddenWithoutTheStore(t *testing.T) {
	hh := cmdHarness(t, newAgent("ok"), nil) // no Schedules wired
	if hh.h.CanSchedule() {
		t.Fatal("CanSchedule must be false without a store")
	}
	hh.deliver(t, "plain", mention("hello"))
	hh.z.reset()
	hh.deliver(t, "plain", "!help")
	if got := hh.only(t); strings.Contains(got, "!schedules") {
		t.Fatalf("help advertised scheduling with no store: %s", got)
	}
	// And `!schedules` is not a command at all — it reaches the agent as
	// prose via the unknown-command reply rather than pretending.
	hh.z.reset()
	hh.deliver(t, "plain", "!schedules")
	if got := hh.only(t); !strings.Contains(got, "Unknown command") {
		t.Fatalf("!schedules with no store = %q", got)
	}
}
