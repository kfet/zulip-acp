package handler

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kfet/zulip-acp/internal/channels"
	"github.com/kfet/zulip-acp/internal/journal"
	"github.com/kfet/zulip-acp/internal/zulipproto"
)

// reactionEvent builds a Zulip reaction event, in the exact shape the
// server sends (measured on Zulip 12.2 — see
// docs/zulip-protocol-reference.md): no stream id, no topic, no name.
func reactionEvent(user, msgID int64, emoji, op string) zulipproto.Event {
	return zulipproto.Event{
		Type: zulipproto.EventReaction, Op: op, UserID: user, MessageID: msgID,
		EmojiName: emoji, EmojiCode: "1f389", ReactionType: "unicode_emoji",
	}
}

// reactHarness returns a harness with reactions on and one engaged
// conversation in #fleet > "t", plus the id of the relay's own last
// message in it.
func reactHarness(t *testing.T, tune func(*Config)) (*harness, int64) {
	t.Helper()
	hh := newHarness(t, newAgent("hello"), func(c *Config) {
		c.Reactions = true
		if tune != nil {
			tune(c)
		}
	})
	hh.deliver(t, "t", mention("hi"))
	return hh, hh.z.lastID()
}

// react feeds one or more reaction events, drives the debounce, and
// returns once the coalesced turn has finished.
func (hh *harness) react(t *testing.T, evs ...zulipproto.Event) {
	t.Helper()
	for _, ev := range evs {
		hh.h.Handle(context.Background(), ev)
	}
	hh.flushReactions(t)
}

// flushReactions fires the debounce timer and waits for the coalesced
// turn to start and then finish. The two guard timeouts are failure
// detection, not synchronisation: every step is driven by hand.
func (hh *harness) flushReactions(t *testing.T) {
	t.Helper()
	select {
	case hh.timer <- time.Now():
	case <-time.After(10 * time.Second):
		t.Fatal("no reaction flush was armed")
	}
	select {
	case <-hh.batches:
	case <-time.After(10 * time.Second):
		t.Fatal("the coalesced reaction turn never started")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := hh.h.WaitIdle(ctx); err != nil {
		t.Fatalf("reaction turn did not finish: %v", err)
	}
}

// reactDropped feeds a reaction that must not reach the agent: nothing
// is buffered, so nothing will ever be delivered. Fully synchronous —
// Handle returns having decided.
func (hh *harness) reactDropped(t *testing.T, ev zulipproto.Event) {
	t.Helper()
	before := hh.promptCount()
	hh.h.Handle(context.Background(), ev)
	if n := hh.pendingReactions(); n != 0 {
		t.Fatalf("reaction buffered %d line(s) instead of being dropped", n)
	}
	if got := hh.promptCount(); got != before {
		t.Fatalf("reaction reached the agent: %q", hh.lastPrompt())
	}
}

// pendingReactions counts everything currently buffered for delivery.
func (hh *harness) pendingReactions() int {
	hh.h.reactMu.Lock()
	defer hh.h.reactMu.Unlock()
	n := 0
	for _, b := range hh.h.reactPending {
		n += len(b.lines) + b.extra
	}
	return n
}

// lastPrompt is the most recent prompt the agent received, "" if none.
func (hh *harness) lastPrompt() string {
	hh.a.mu.Lock()
	defer hh.a.mu.Unlock()
	if len(hh.a.prompts) == 0 {
		return ""
	}
	return hh.a.prompts[len(hh.a.prompts)-1]
}

func (hh *harness) promptCount() int {
	hh.a.mu.Lock()
	defer hh.a.mu.Unlock()
	return len(hh.a.prompts)
}

// --- gating --------------------------------------------------------------

// TestReactionGates drives every reason a reaction is refused. Each
// case reacts on the relay's OWN last message — the one thing that
// always resolves — so nothing but the gate under test can be the
// reason the agent stayed unprompted.
func TestReactionGates(t *testing.T) {
	cases := []struct {
		name string
		tune func(*Config)
		ev   func(msgID int64) zulipproto.Event
	}{
		{
			name: "feature disabled",
			tune: func(c *Config) { c.Reactions = false },
			ev:   func(id int64) zulipproto.Event { return reactionEvent(humanID, id, "tada", zulipproto.ReactionAdd) },
		},
		{
			// Not a reaction op at all. Zulip sends only add/remove,
			// and anything else is a shape we do not understand.
			name: "unknown op",
			ev:   func(id int64) zulipproto.Event { return reactionEvent(humanID, id, "tada", "update") },
		},
		{
			// The relay puts :eyes: on every message it accepts and
			// :check: on an `!opts` change. Re-ingesting either would
			// be a turn that produces another reaction, forever.
			name: "the relay's own reaction",
			ev:   func(id int64) zulipproto.Event { return reactionEvent(botID, id, "eyes", zulipproto.ReactionAdd) },
		},
		{
			name: "another bot's reaction",
			tune: func(c *Config) { c.BotSenderIDs = map[int64]struct{}{77: {}} },
			ev:   func(id int64) zulipproto.Event { return reactionEvent(77, id, "tada", zulipproto.ReactionAdd) },
		},
		{
			name: "user not on the allowlist",
			tune: func(c *Config) { c.AllowedUsers = map[int64]struct{}{humanID: {}} },
			ev:   func(id int64) zulipproto.Event { return reactionEvent(4242, id, "tada", zulipproto.ReactionAdd) },
		},
		{
			// Nothing knows this message: no index entry, no journal
			// id, and GetMessage does not find it either.
			name: "unresolvable message",
			ev:   func(int64) zulipproto.Event { return reactionEvent(humanID, 999999, "tada", zulipproto.ReactionAdd) },
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			hh, own := reactHarness(t, c.tune)
			hh.reactDropped(t, c.ev(own))
		})
	}
}

// TestReactionResolvesRelayOwnMessage is the common case: a reaction on
// the message the relay just posted, resolved from memory with NO API
// call, and delivered as an ambient turn.
func TestReactionResolvesRelayOwnMessage(t *testing.T) {
	hh, own := reactHarness(t, nil)
	hh.z.mu.Lock()
	hh.z.gets = nil
	hh.z.mu.Unlock()

	hh.react(t, reactionEvent(humanID, own, "wastebasket", zulipproto.ReactionAdd))

	got := hh.lastPrompt()
	want := fmt.Sprintf("[reaction] Ada Lovelace added :wastebasket: to your own message %d", own)
	if !strings.HasPrefix(got, want) {
		t.Fatalf("prompt = %q, want prefix %q", got, want)
	}
	hh.z.mu.Lock()
	gets := len(hh.z.gets)
	hh.z.mu.Unlock()
	if gets != 0 {
		t.Fatalf("a reaction on our own last message cost %d GET /messages", gets)
	}
	// A second reaction from the same user must not re-resolve the name.
	hh.react(t, reactionEvent(humanID, own, "tada", zulipproto.ReactionAdd))
	hh.z.mu.Lock()
	names := len(hh.z.userGets)
	hh.z.mu.Unlock()
	if names != 1 {
		t.Fatalf("user name resolved %d times, want 1 (cached)", names)
	}
}

// TestReactionOnHumanMessageFallsBackToLookup covers the other resolution
// path: a message the relay did not post, resolved with one
// GET /messages/{id} onto an engaged topic.
func TestReactionOnHumanMessageFallsBackToLookup(t *testing.T) {
	hh, _ := reactHarness(t, nil)
	hh.z.mu.Lock()
	hh.z.messages[555] = zulipproto.Message{
		ID: 555, SenderID: humanID, SenderName: "Ada Lovelace", StreamID: 4, Topic: "t",
		Type: zulipproto.MessageTypeStream,
		Content: "the quick brown fox jumps over the lazy dog and then keeps going well past " +
			"eighty characters so the excerpt has to bite",
	}
	hh.z.mu.Unlock()

	hh.react(t, reactionEvent(humanID, 555, "tada", zulipproto.ReactionAdd))

	got := hh.lastPrompt()
	if !strings.HasPrefix(got, "[reaction] Ada Lovelace added :tada: to message 555 by Ada Lovelace (") {
		t.Fatalf("prompt = %q", got)
	}
	if strings.Contains(got, "has to bite") {
		t.Fatalf("excerpt was not bounded: %q", got)
	}
	if !strings.Contains(got, "…") {
		t.Fatalf("truncated excerpt must be marked: %q", got)
	}
}

// TestReactionOutsideServedSetIsDroppedOnce proves both halves of the
// flood defence for an unrelated message: it is dropped, and the
// negative cache means a second reaction on it costs no second lookup.
func TestReactionOutsideServedSetIsDroppedOnce(t *testing.T) {
	hh, _ := reactHarness(t, nil)
	hh.z.mu.Lock()
	hh.z.messages[600] = zulipproto.Message{
		ID: 600, SenderID: humanID, StreamID: 99, Topic: "elsewhere", Type: zulipproto.MessageTypeStream,
		Content: "not our channel",
	}
	// An engaged channel but an unknown topic: resolvable, not engaged.
	hh.z.messages[601] = zulipproto.Message{
		ID: 601, SenderID: humanID, StreamID: 4, Topic: "never seen", Type: zulipproto.MessageTypeStream,
		Content: "not our topic",
	}
	hh.z.gets = nil
	hh.z.mu.Unlock()

	for _, id := range []int64{600, 600, 601, 601} {
		hh.reactDropped(t, reactionEvent(humanID, id, "tada", zulipproto.ReactionAdd))
	}
	hh.z.mu.Lock()
	gets := len(hh.z.gets)
	hh.z.mu.Unlock()
	if gets != 2 {
		t.Fatalf("%d lookups for 4 reactions on 2 messages, want 2 (negative cache)", gets)
	}
}

// TestReactionLookupRateLimited is the flood stop: reaction events are
// NOT filtered by the queue's narrow, so an unbounded realm must not
// be able to drive our API traffic.
func TestReactionLookupRateLimited(t *testing.T) {
	now := time.Now()
	hh, _ := reactHarness(t, func(c *Config) { c.Now = func() time.Time { return now } })
	hh.z.mu.Lock()
	hh.z.gets = nil
	hh.z.mu.Unlock()

	for i := range reactionLookupsPerMinute + 5 {
		hh.reactDropped(t, reactionEvent(humanID, int64(10_000+i), "tada", zulipproto.ReactionAdd))
	}
	hh.z.mu.Lock()
	gets := len(hh.z.gets)
	hh.z.mu.Unlock()
	if gets != reactionLookupsPerMinute {
		t.Fatalf("%d lookups, want the cap of %d", gets, reactionLookupsPerMinute)
	}
	if !hh.logged("over 20 lookups") {
		t.Fatal("the rate limit must say so in the log")
	}
	if n := hh.loggedCount("over 20 lookups"); n != 1 {
		t.Fatalf("the rate limit logged %d times, want 1 per window — a log line per dropped reaction is the same flood", n)
	}
	// The window rolls: past it, lookups are allowed again.
	now = now.Add(2 * reactionLookupWindow)
	hh.reactDropped(t, reactionEvent(humanID, 20_001, "tada", zulipproto.ReactionAdd))
	hh.z.mu.Lock()
	gets = len(hh.z.gets)
	hh.z.mu.Unlock()
	if gets != reactionLookupsPerMinute+1 {
		t.Fatalf("%d lookups after the window rolled, want %d", gets, reactionLookupsPerMinute+1)
	}
}

// TestReactionDuringTurnFoldsIntoTheBatch: an emoji must never cancel
// a running answer, and it must not be thrown away either — it waits
// for the conversation and is delivered with whatever else arrived.
func TestReactionDuringTurnFoldsIntoTheBatch(t *testing.T) {
	agent := newAgent("slow answer")
	hh := newHarness(t, agent, func(c *Config) { c.Reactions = true })
	hh.deliver(t, "t", mention("hi"))
	own := hh.z.lastID()

	// A second human turn, held open by the agent. Drain the entry
	// signal from the first turn so the wait below observes THIS one.
	for len(agent.entered) > 0 {
		<-agent.entered
	}
	agent.mu.Lock()
	agent.block = make(chan struct{})
	block := agent.block
	agent.mu.Unlock()
	hh.h.Handle(context.Background(), channelEvent(humanID, "t", "keep going"))
	<-agent.entered

	before := hh.promptCount()
	hh.h.Handle(context.Background(), reactionEvent(humanID, own, "tada", zulipproto.ReactionAdd))
	hh.h.Handle(context.Background(), reactionEvent(4242, own, "+1", zulipproto.ReactionAdd))
	if got := hh.promptCount(); got != before {
		t.Fatalf("the reaction started a turn on top of a running one: %q", hh.lastPrompt())
	}
	if n := hh.pendingReactions(); n != 2 {
		t.Fatalf("buffered %d reactions during the turn, want 2", n)
	}
	// The debounce expires while the human turn is still running: the
	// flush must wait for the conversation rather than race it.
	select {
	case hh.timer <- time.Now():
	case <-time.After(10 * time.Second):
		t.Fatal("no reaction flush was armed")
	}
	if got := hh.promptCount(); got != before {
		t.Fatalf("the reaction turn jumped the running one: %q", hh.lastPrompt())
	}
	close(block)
	select {
	case n := <-hh.batches:
		if n != 2 {
			t.Fatalf("delivered %d reactions, want both", n)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the buffered reactions were never delivered")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := hh.h.WaitIdle(ctx); err != nil {
		t.Fatalf("turn did not finish: %v", err)
	}
	got := hh.lastPrompt()
	if !strings.HasPrefix(got, "[reactions] 2 in this conversation:") ||
		!strings.Contains(got, ":tada:") || !strings.Contains(got, ":+1:") {
		t.Fatalf("prompt = %q", got)
	}
}

// TestReactionOnRetiredConversationDropped: `!new` retires a
// conversation, and a reaction on the message it left behind must not
// resurrect it.
func TestReactionOnRetiredConversationDropped(t *testing.T) {
	hh, own := reactHarness(t, nil)
	if _, _, existed, err := hh.j.Retire(journal.Channel(4, "t")); err != nil || !existed {
		t.Fatalf("Retire: existed=%v err=%v", existed, err)
	}
	hh.reactDropped(t, reactionEvent(humanID, own, "tada", zulipproto.ReactionAdd))
}

// TestReactionOnUnservedChannelViaIndex: the served set moves
// underfoot, so a conversation in the index is not by itself
// permission to answer in it.
func TestReactionOnUnservedChannelViaIndex(t *testing.T) {
	hh, own := reactHarness(t, nil)
	hh.h.cfg.Channels = channels.New(channels.Config{Explicit: map[int64]string{7: "other"}})
	hh.reactDropped(t, reactionEvent(humanID, own, "tada", zulipproto.ReactionAdd))
}

// TestReactionResolvesFromJournal covers the tier the in-memory index
// cannot: ids the JOURNAL holds — an interrupted turn's tail, an
// `!opts` panel — which are what survives a restart.
func TestReactionResolvesFromJournal(t *testing.T) {
	for _, c := range []struct {
		name string
		set  func(hh *harness, convID string, id int64) error
	}{
		{"tail", func(hh *harness, convID string, id int64) error { return hh.j.SetTail(convID, id) }},
		{"opts panel", func(hh *harness, convID string, id int64) error { return hh.j.SetOpts(convID, id) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			hh, _ := reactHarness(t, nil)
			conv, ok := hh.j.Lookup(journal.Channel(4, "t"))
			if !ok {
				t.Fatal("no conversation")
			}
			const id = int64(7777)
			if err := c.set(hh, conv.ID, id); err != nil {
				t.Fatalf("record %s: %v", c.name, err)
			}
			// Drop the in-memory index so only the journal can answer.
			hh.h.ownMsgs = newMsgIndex(reactionIndexSize)
			hh.react(t, reactionEvent(humanID, id, "tada", zulipproto.ReactionAdd))
			if got := hh.lastPrompt(); !strings.Contains(got, "to your own message 7777") {
				t.Fatalf("prompt = %q", got)
			}
		})
	}
}

// TestReactionInDM: a DM reaction resolves only when DMs are served.
func TestReactionInDM(t *testing.T) {
	dm := zulipproto.Message{
		ID: 900, SenderID: humanID, SenderName: "Ada Lovelace", Type: zulipproto.MessageTypePrivate,
		Content:          "a direct question",
		DisplayRecipient: []byte(fmt.Sprintf(`[{"id":%d},{"id":%d}]`, humanID, botID)),
	}
	t.Run("dms disabled", func(t *testing.T) {
		hh, _ := reactHarness(t, nil)
		hh.z.mu.Lock()
		hh.z.messages[900] = dm
		hh.z.mu.Unlock()
		hh.reactDropped(t, reactionEvent(humanID, 900, "tada", zulipproto.ReactionAdd))
	})
	t.Run("dms enabled", func(t *testing.T) {
		hh := newHarness(t, newAgent("hello"), func(c *Config) { c.Reactions, c.DMs = true, true })
		hh.h.Handle(context.Background(), dmEvent(humanID, "hi there", humanID, botID))
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := hh.h.WaitIdle(ctx); err != nil {
			t.Fatalf("DM turn did not finish: %v", err)
		}
		own := hh.z.lastID()
		hh.z.mu.Lock()
		hh.z.messages[900] = dm
		hh.z.mu.Unlock()
		hh.react(t, reactionEvent(humanID, 900, "tada", zulipproto.ReactionAdd))
		if got := hh.lastPrompt(); !strings.Contains(got, "added :tada: to message 900 by Ada Lovelace") {
			t.Fatalf("prompt = %q", got)
		}
		// And on the relay's own DM message, which resolves from the
		// in-memory index — the tier that checks "do we still serve
		// this conversation" without a message to look at.
		hh.react(t, reactionEvent(humanID, own, "tada", zulipproto.ReactionAdd))
		if got := hh.lastPrompt(); !strings.Contains(got, fmt.Sprintf("to your own message %d", own)) {
			t.Fatalf("prompt = %q", got)
		}
		// With DMs switched off underneath it, the same reaction is
		// dropped: a journal entry is not by itself permission.
		hh.h.cfg.DMs = false
		hh.reactDropped(t, reactionEvent(humanID, own, "tada", zulipproto.ReactionAdd))
	})
	t.Run("no usable recipient list", func(t *testing.T) {
		hh := newHarness(t, newAgent("hello"), func(c *Config) { c.Reactions, c.DMs = true, true })
		broken := dm
		broken.DisplayRecipient = []byte(`"a channel name"`)
		hh.z.mu.Lock()
		hh.z.messages[900] = broken
		hh.z.mu.Unlock()
		hh.reactDropped(t, reactionEvent(humanID, 900, "tada", zulipproto.ReactionAdd))
	})
}

// TestReactionIsAmbient: a reaction routes through the abstain path, so
// the agent may answer with the silent sentinel and nothing is posted.
func TestReactionIsAmbient(t *testing.T) {
	hh, own := reactHarness(t, nil)
	hh.a.mu.Lock()
	hh.a.chunks = []string{"<<SILENT>>"}
	hh.a.mu.Unlock()
	before := hh.z.count()
	hh.react(t, reactionEvent(humanID, own, "tada", zulipproto.ReactionAdd))
	if got := hh.z.count(); got != before {
		t.Fatalf("the agent abstained but %d message(s) appeared", got-before)
	}
	if !strings.HasPrefix(hh.lastPrompt(), "[reaction] ") {
		t.Fatalf("prompt = %q", hh.lastPrompt())
	}
}

// TestReactionLookupFailureIsLogged: GetMessage failing is a drop, not
// a crash, and it is cached like any other non-resolution.
func TestReactionLookupFailureIsLogged(t *testing.T) {
	hh, _ := reactHarness(t, nil)
	hh.z.mu.Lock()
	hh.z.getErr = errors.New("boom")
	hh.z.gets = nil
	hh.z.mu.Unlock()
	hh.reactDropped(t, reactionEvent(humanID, 4242, "tada", zulipproto.ReactionAdd))
	hh.reactDropped(t, reactionEvent(humanID, 4242, "tada", zulipproto.ReactionAdd))
	if !hh.logged("reading reacted-to message 4242") {
		t.Fatal("a failed lookup must be logged")
	}
	hh.z.mu.Lock()
	gets := len(hh.z.gets)
	hh.z.mu.Unlock()
	if gets != 1 {
		t.Fatalf("%d lookups, want 1 (the failure is cached)", gets)
	}
}

// TestReactionUnknownUserFallsBackToID: the name is decoration, and a
// user Zulip will not describe must not cost the turn.
func TestReactionUnknownUserFallsBackToID(t *testing.T) {
	hh, own := reactHarness(t, nil)
	hh.z.mu.Lock()
	delete(hh.z.users, humanID)
	hh.z.mu.Unlock()
	hh.react(t, reactionEvent(humanID, own, "tada", zulipproto.ReactionAdd))
	if got := hh.lastPrompt(); !strings.HasPrefix(got, fmt.Sprintf("[reaction] user %d added", humanID)) {
		t.Fatalf("prompt = %q", got)
	}
	if !hh.logged("resolving user 8") {
		t.Fatal("a failed name lookup must be logged")
	}
	// Nothing was cached, so a later success still names the human.
	hh.z.mu.Lock()
	hh.z.users[humanID] = zulipproto.User{UserID: humanID, FullName: "Ada Lovelace"}
	hh.z.mu.Unlock()
	hh.react(t, reactionEvent(humanID, own, "tada", zulipproto.ReactionAdd))
	if got := hh.lastPrompt(); !strings.HasPrefix(got, "[reaction] Ada Lovelace added") {
		t.Fatalf("prompt = %q", got)
	}
	// A user with no full name at all still renders as the id.
	hh.z.mu.Lock()
	hh.z.users[4242] = zulipproto.User{UserID: 4242}
	hh.z.mu.Unlock()
	hh.react(t, reactionEvent(4242, own, "tada", zulipproto.ReactionAdd))
	if got := hh.lastPrompt(); !strings.HasPrefix(got, "[reaction] user 4242 added") {
		t.Fatalf("prompt = %q", got)
	}
}

// TestReactionTriggerHook pins the shape the later archive-on-emoji
// feature hooks into: it sees a resolved conversation and can consume
// the reaction before the agent does. Nothing is wired to it in
// production, so a resolved reaction always reaches the agent.
func TestReactionTriggerHook(t *testing.T) {
	var seen []string
	hh, own := reactHarness(t, func(c *Config) {
		c.ReactionTrigger = func(_ context.Context, conv journal.Conv, ev zulipproto.Event, m *zulipproto.Message) bool {
			seen = append(seen, fmt.Sprintf("%s:%s:%d:%v", conv.ID, ev.EmojiName, ev.MessageID, m == nil))
			return ev.EmojiName == "wastebasket"
		}
	})
	hh.reactDropped(t, reactionEvent(humanID, own, "wastebasket", zulipproto.ReactionAdd))
	// One it does not claim is delivered as usual.
	hh.react(t, reactionEvent(humanID, own, "tada", zulipproto.ReactionAdd))
	if got := hh.lastPrompt(); !strings.Contains(got, ":tada:") {
		t.Fatalf("prompt = %q", got)
	}
	conv, ok := hh.j.Lookup(journal.Channel(4, "t"))
	if !ok {
		t.Fatal("no conversation")
	}
	want := []string{
		fmt.Sprintf("%s:wastebasket:%d:true", conv.ID, own),
		fmt.Sprintf("%s:tada:%d:true", conv.ID, own),
	}
	if len(seen) != 2 || seen[0] != want[0] || seen[1] != want[1] {
		t.Fatalf("trigger saw %q, want %q", seen, want)
	}
	// The default relay wires no trigger at all.
	plain, pown := reactHarness(t, nil)
	plain.react(t, reactionEvent(humanID, pown, "wastebasket", zulipproto.ReactionAdd))
	if got := plain.lastPrompt(); !strings.Contains(got, ":wastebasket:") {
		t.Fatalf("v1 must deliver every resolved reaction: %q", got)
	}
}

// TestReactionOnRelayMessageOutsideIndex covers the resolution tier
// where the message is FETCHED and turns out to be the relay's own —
// an older message of ours that the bounded index has since evicted.
func TestReactionOnRelayMessageOutsideIndex(t *testing.T) {
	hh, _ := reactHarness(t, nil)
	hh.z.mu.Lock()
	hh.z.messages[321] = zulipproto.Message{
		ID: 321, SenderID: botID, SenderName: botName, StreamID: 4, Topic: "t",
		Type: zulipproto.MessageTypeStream, Content: "an older answer of mine",
	}
	hh.z.mu.Unlock()
	hh.react(t, reactionEvent(humanID, 321, "tada", zulipproto.ReactionAdd))
	want := `[reaction] Ada Lovelace added :tada: to your own message 321 ("an older answer of mine")`
	if got := hh.lastPrompt(); got != want {
		t.Fatalf("prompt = %q, want %q", got, want)
	}
}

// TestReactionFromJournalOutsideServedSet: the journal tier is subject
// to the same moving served set as the index tier.
func TestReactionFromJournalOutsideServedSet(t *testing.T) {
	hh, _ := reactHarness(t, nil)
	conv, ok := hh.j.Lookup(journal.Channel(4, "t"))
	if !ok {
		t.Fatal("no conversation")
	}
	if err := hh.j.SetTail(conv.ID, 8888); err != nil {
		t.Fatalf("SetTail: %v", err)
	}
	hh.h.ownMsgs = newMsgIndex(reactionIndexSize)
	hh.h.cfg.Channels = channels.New(channels.Config{Explicit: map[int64]string{7: "other"}})
	hh.reactDropped(t, reactionEvent(humanID, 8888, "tada", zulipproto.ReactionAdd))
}

// --- units ---------------------------------------------------------------

func TestMsgIndexBounds(t *testing.T) {
	x := newMsgIndex(2)
	x.put(1, "a")
	x.put(2, "b")
	x.put(1, "a2") // overwrite must not consume a slot
	x.put(3, "c")
	if _, ok := x.get(1); ok {
		t.Fatal("oldest entry was not evicted")
	}
	if v, ok := x.get(3); !ok || v != "c" {
		t.Fatalf("get(3) = %q,%v", v, ok)
	}
	if v, ok := x.get(2); !ok || v != "b" {
		t.Fatalf("get(2) = %q,%v", v, ok)
	}
}

func TestExcerpt(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"   \n\t ", ""},
		{"one\ntwo   three", "one two three"},
		{strings.Repeat("x", 90), strings.Repeat("x", 80) + "…"},
	}
	for _, c := range cases {
		if got := excerpt(c.in, reactionExcerptRunes); got != c.want {
			t.Fatalf("excerpt(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestReactionNegativeCacheClearedOnEngagement: "not ours" is only true
// until the relay is engaged in that topic. A reaction refused before
// the conversation existed must not stay refused forever.
func TestReactionNegativeCacheClearedOnEngagement(t *testing.T) {
	hh, _ := reactHarness(t, nil)
	hh.z.mu.Lock()
	hh.z.messages[770] = zulipproto.Message{
		ID: 770, SenderID: humanID, SenderName: "Ada Lovelace", StreamID: 4, Topic: "later",
		Type: zulipproto.MessageTypeStream, Content: "an earlier remark",
	}
	hh.z.mu.Unlock()

	hh.reactDropped(t, reactionEvent(humanID, 770, "tada", zulipproto.ReactionAdd))
	// Now the topic is engaged, so the same reaction resolves.
	hh.deliver(t, "later", mention("hello there"))
	hh.react(t, reactionEvent(humanID, 770, "tada", zulipproto.ReactionAdd))
	if got := hh.lastPrompt(); !strings.Contains(got, "to message 770 by Ada Lovelace") {
		t.Fatalf("prompt = %q", got)
	}
}

// TestReactionFromLateBotDropped: BotSenderIDs is a startup snapshot,
// so a bot that appeared since — or a cross-realm system bot, which is
// in no user list at all — is recognisable only from the user record
// the reaction path fetches anyway.
func TestReactionFromLateBotDropped(t *testing.T) {
	hh, own := reactHarness(t, nil)
	hh.z.mu.Lock()
	hh.z.users[55] = zulipproto.User{UserID: 55, FullName: "Notification Bot", IsBot: true}
	hh.z.mu.Unlock()
	hh.reactDropped(t, reactionEvent(55, own, "tada", zulipproto.ReactionAdd))
	// Nothing was cached as a NAME for it, but the bot verdict is:
	// re-asking on every reaction a bot leaves is exactly the
	// per-event cost this path exists to avoid.
	hh.reactDropped(t, reactionEvent(55, own, "tada", zulipproto.ReactionAdd))
	hh.z.mu.Lock()
	n := len(hh.z.userGets)
	hh.z.mu.Unlock()
	if n != 1 {
		t.Fatalf("bot lookups = %d, want 1 (the verdict is cached)", n)
	}
}

// TestMsgIndexDropValue: the negative cache is invalidated one
// conversation at a time, never wholesale.
func TestMsgIndexDropValue(t *testing.T) {
	x := newMsgIndex(4)
	x.put(1, "channel 4 > \"a\"")
	x.put(2, "channel 4 > \"b\"")
	x.put(3, "")
	x.dropValue("") // a hard failure is not a conversation; drop nothing
	if _, ok := x.get(3); !ok {
		t.Fatal("dropValue(\"\") must not touch the index")
	}
	x.dropValue("channel 4 > \"a\"")
	if _, ok := x.get(1); ok {
		t.Fatal("the matching entry survived")
	}
	for _, id := range []int64{2, 3} {
		if _, ok := x.get(id); !ok {
			t.Fatalf("entry %d was dropped with the wrong key", id)
		}
	}
	// Eviction order is still intact after a drop.
	x.put(5, "e")
	x.put(6, "f")
	x.put(7, "g")
	if _, ok := x.get(2); ok {
		t.Fatal("FIFO order broke after dropValue")
	}
}

// TestReactionsCoalesceIntoOneTurn is what makes "reactions on by
// default" affordable: ten people tapping the same message is one
// conversational fact and must cost ONE turn.
func TestReactionsCoalesceIntoOneTurn(t *testing.T) {
	hh, own := reactHarness(t, nil)
	before := hh.promptCount()

	evs := make([]zulipproto.Event, 0, 10)
	for i := range 10 {
		evs = append(evs, reactionEvent(int64(100+i), own, "tada", zulipproto.ReactionAdd))
	}
	hh.react(t, evs...)

	if got := hh.promptCount(); got != before+1 {
		t.Fatalf("10 reactions cost %d turns, want 1", got-before)
	}
	got := hh.lastPrompt()
	if !strings.HasPrefix(got, "[reactions] 10 in this conversation:") {
		t.Fatalf("prompt = %q", got)
	}
	if n := strings.Count(got, "\n- "); n != 10 {
		t.Fatalf("prompt lists %d reactions, want 10: %q", n, got)
	}
	// The buffer is empty again, so the next burst arms a fresh flush.
	if n := hh.pendingReactions(); n != 0 {
		t.Fatalf("%d reactions left buffered after delivery", n)
	}
	hh.react(t, reactionEvent(humanID, own, "eyes", zulipproto.ReactionAdd))
	if got := hh.lastPrompt(); !strings.HasPrefix(got, "[reaction] ") {
		t.Fatalf("a lone reaction after a burst = %q", got)
	}
}

// TestReactionBurstIsCapped: a pile-on is one turn AND a bounded
// prompt. Beyond the cap the agent is told the count, not the content.
func TestReactionBurstIsCapped(t *testing.T) {
	hh, own := reactHarness(t, nil)
	evs := make([]zulipproto.Event, 0, reactionBatchMax+3)
	for i := range reactionBatchMax + 3 {
		evs = append(evs, reactionEvent(int64(200+i), own, "tada", zulipproto.ReactionAdd))
	}
	hh.react(t, evs...)

	got := hh.lastPrompt()
	if !strings.HasPrefix(got, fmt.Sprintf("[reactions] %d in this conversation:", reactionBatchMax+3)) {
		t.Fatalf("prompt = %q", got)
	}
	if n := strings.Count(got, "\n- "); n != reactionBatchMax+1 {
		t.Fatalf("prompt lists %d lines, want %d plus the overflow note: %q", n, reactionBatchMax, got)
	}
	if !strings.Contains(got, "…and 3 more") {
		t.Fatalf("the overflow must be counted: %q", got)
	}
}

// TestReactionRemovalIsDelivered: un-reacting is real signal — an
// approval withdrawn, a trigger taken back — and reads differently
// from an add.
func TestReactionRemovalIsDelivered(t *testing.T) {
	hh, own := reactHarness(t, nil)
	hh.react(t, reactionEvent(humanID, own, "+1", zulipproto.ReactionRemove))
	want := fmt.Sprintf("[reaction] Ada Lovelace removed :+1: from your own message %d", own)
	if got := hh.lastPrompt(); got != want {
		t.Fatalf("prompt = %q, want %q", got, want)
	}
	// A message we did not post reads the same way, with attribution.
	hh.z.mu.Lock()
	hh.z.messages[810] = zulipproto.Message{
		ID: 810, SenderID: humanID, SenderName: "Ada Lovelace", StreamID: 4, Topic: "t",
		Type: zulipproto.MessageTypeStream, Content: "ship it?",
	}
	hh.z.mu.Unlock()
	hh.react(t, reactionEvent(humanID, 810, "rocket", zulipproto.ReactionRemove))
	if got := hh.lastPrompt(); got != `[reaction] Ada Lovelace removed :rocket: from message 810 by Ada Lovelace ("ship it?")` {
		t.Fatalf("prompt = %q", got)
	}
	// Add and remove in one burst keep their order and their verbs.
	hh.react(t,
		reactionEvent(humanID, own, "eyes", zulipproto.ReactionAdd),
		reactionEvent(humanID, own, "eyes", zulipproto.ReactionRemove),
	)
	got := hh.lastPrompt()
	add := strings.Index(got, "added :eyes:")
	rem := strings.Index(got, "removed :eyes:")
	if add < 0 || rem < 0 || add > rem {
		t.Fatalf("burst lost the add/remove order: %q", got)
	}
}

// TestReactionBatchDroppedOnShutdown: a buffered reaction is not a turn
// anybody is waiting on, so it dies with the relay rather than
// outliving it.
func TestReactionBatchDroppedOnShutdown(t *testing.T) {
	hh, own := reactHarness(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	before := hh.promptCount()
	hh.h.Handle(ctx, reactionEvent(humanID, own, "tada", zulipproto.ReactionAdd))
	if n := hh.pendingReactions(); n != 1 {
		t.Fatalf("buffered %d reactions, want 1", n)
	}
	cancel()
	// The flush goroutine wakes on the cancelled context, empties the
	// buffer and starts nothing. Draining is observable through
	// WaitIdle plus the buffer going empty.
	deadline := time.Now().Add(10 * time.Second)
	for hh.pendingReactions() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("the buffer was not dropped on shutdown")
		}
		runtime.Gosched()
	}
	if got := hh.promptCount(); got != before {
		t.Fatalf("a shutdown-dropped reaction still reached the agent: %q", hh.lastPrompt())
	}
}

// TestReactionBatchPromptEmpty: the shutdown race can hand the renderer
// an empty batch; it must render nothing rather than an empty header.
func TestReactionBatchPromptEmpty(t *testing.T) {
	if got := reactionBatchPrompt(nil, 0); got != "" {
		t.Fatalf("reactionBatchPrompt(nil) = %q", got)
	}
}

// TestReactionFlushGivesUpOnShutdown: the flush can also be waiting for
// a busy conversation when the relay stops. It must let go of the
// buffer instead of holding a goroutine open on a dead relay.
func TestReactionFlushGivesUpOnShutdown(t *testing.T) {
	agent := newAgent("slow answer")
	hh := newHarness(t, agent, func(c *Config) { c.Reactions = true })
	hh.deliver(t, "t", mention("hi"))
	own := hh.z.lastID()

	for len(agent.entered) > 0 {
		<-agent.entered
	}
	agent.mu.Lock()
	agent.block = make(chan struct{})
	block := agent.block
	agent.mu.Unlock()
	hh.h.Handle(context.Background(), channelEvent(humanID, "t", "keep going"))
	<-agent.entered

	ctx, cancel := context.WithCancel(context.Background())
	before := hh.promptCount()
	hh.h.Handle(ctx, reactionEvent(humanID, own, "tada", zulipproto.ReactionAdd))
	// The debounce expires, so the flush is now parked on the busy
	// conversation — and that is where the shutdown catches it.
	select {
	case hh.timer <- time.Now():
	case <-time.After(10 * time.Second):
		t.Fatal("no reaction flush was armed")
	}
	cancel()
	deadline := time.Now().Add(10 * time.Second)
	for hh.pendingReactions() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("the buffer was not dropped when the wait was abandoned")
		}
		runtime.Gosched()
	}
	close(block)
	wctx, wcancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer wcancel()
	if err := hh.h.WaitIdle(wctx); err != nil {
		t.Fatalf("turn did not finish: %v", err)
	}
	if got := hh.promptCount(); got != before {
		t.Fatalf("the abandoned reaction still reached the agent: %q", hh.lastPrompt())
	}
}

// TestTakeReactionsUnknownConv: taking a buffer that is not there is
// the shutdown race — both exits empty it, and the second must be a
// no-op rather than a panic.
func TestTakeReactionsUnknownConv(t *testing.T) {
	hh, _ := reactHarness(t, nil)
	lines, extra := hh.h.takeReactions("no-such-conv")
	if lines != nil || extra != 0 {
		t.Fatalf("takeReactions(unknown) = %v,%d", lines, extra)
	}
}

// TestAfterDefaultsToTimeAfter: production wires no timer, so the
// debounce must fall back to the real clock.
func TestAfterDefaultsToTimeAfter(t *testing.T) {
	hh, _ := reactHarness(t, func(c *Config) { c.After = nil })
	if ch := hh.h.after(time.Hour); ch == nil {
		t.Fatal("after() with no injected timer returned nil")
	}
}

// TestBufferedReactionsFollowTheConversation: seconds pass between
// buffering a reaction and delivering it, so the conversation is
// re-read at the last moment. A rename must be followed, and a
// conversation retired by `!new` in the meantime must be dropped —
// the reaction belonged to the one the user just replaced.
func TestBufferedReactionsFollowTheConversation(t *testing.T) {
	t.Run("rename is followed", func(t *testing.T) {
		hh, own := reactHarness(t, nil)
		hh.h.Handle(context.Background(), reactionEvent(humanID, own, "tada", zulipproto.ReactionAdd))
		if _, moved, err := hh.j.Rename(4, "t", "t (renamed)"); err != nil || !moved {
			t.Fatalf("Rename: moved=%v err=%v", moved, err)
		}
		hh.flushReactions(t)
		if got := hh.z.topics[hh.z.lastID()]; got != "t (renamed)" {
			t.Fatalf("reaction turn posted into topic %q, want the renamed one", got)
		}
	})
	t.Run("retirement drops the burst", func(t *testing.T) {
		hh, own := reactHarness(t, nil)
		before := hh.promptCount()
		hh.h.Handle(context.Background(), reactionEvent(humanID, own, "tada", zulipproto.ReactionAdd))
		if _, _, existed, err := hh.j.Retire(journal.Channel(4, "t")); err != nil || !existed {
			t.Fatalf("Retire: existed=%v err=%v", existed, err)
		}
		select {
		case hh.timer <- time.Now():
		case <-time.After(10 * time.Second):
			t.Fatal("no reaction flush was armed")
		}
		deadline := time.Now().Add(10 * time.Second)
		for hh.pendingReactions() != 0 {
			if time.Now().After(deadline) {
				t.Fatal("the buffer outlived the conversation")
			}
			runtime.Gosched()
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := hh.h.WaitIdle(ctx); err != nil {
			t.Fatalf("handler did not go idle: %v", err)
		}
		if got := hh.promptCount(); got != before {
			t.Fatalf("a retired conversation answered a buffered reaction: %q", hh.lastPrompt())
		}
		if !hh.logged("is gone") {
			t.Fatal("dropping a burst whose conversation vanished must be logged")
		}
	})
}

// TestBufferedReactionTurnGetsAFullTimeout: the debounce, and the wait
// for a busy conversation, must not be charged to the turn they queue.
//
// Getting this wrong is not a slow turn but a WRONG MESSAGE: a
// conversation busy for longer than the turn bound — exactly when
// reactions pile up — would start the reaction turn already expired and
// post an error into the topic, caused by nothing but an emoji.
//
// It is asserted on the OPT-IN ceiling, because that is the bound with
// a visible deadline; the no-progress window is armed in the very same
// place, so proving one proves both.
func TestBufferedReactionTurnGetsAFullTimeout(t *testing.T) {
	agent := newAgent("answer")
	waited := make(chan time.Time, 4)
	hh := newHarness(t, agent, func(c *Config) {
		c.Reactions = true
		c.TurnCeiling = 5 * time.Second
		c.OnWaitForConv = func(string) {
			select {
			case waited <- time.Now():
			default:
			}
		}
	})
	hh.deliver(t, "t", mention("hi"))
	own := hh.z.lastID()

	for len(agent.entered) > 0 {
		<-agent.entered
	}
	agent.mu.Lock()
	agent.block = make(chan struct{})
	block := agent.block
	agent.mu.Unlock()
	hh.h.Handle(context.Background(), channelEvent(humanID, "t", "keep going"))
	<-agent.entered

	hh.h.Handle(context.Background(), reactionEvent(humanID, own, "tada", zulipproto.ReactionAdd))
	select {
	case hh.timer <- time.Now():
	case <-time.After(10 * time.Second):
		t.Fatal("no reaction flush was armed")
	}
	// The instant the flush parked on the busy conversation. Anything
	// timed BEFORE the claim — the old shape — necessarily has a
	// deadline older than this.
	parked := <-waited
	close(block)
	select {
	case <-hh.batches:
	case <-time.After(10 * time.Second):
		t.Fatal("the buffered reaction was never delivered")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := hh.h.WaitIdle(ctx); err != nil {
		t.Fatalf("turn did not finish: %v", err)
	}
	// The clock starts when the turn actually runs, so the deadline is
	// strictly later than one measured from the moment the reaction was
	// buffered.
	if got := agent.lastDeadline(t); !got.After(parked.Add(5 * time.Second)) {
		t.Fatalf("reaction turn deadline %v was charged for the wait (parked at %v)", got, parked)
	}
	if body := hh.z.lastBody(); strings.Contains(body, "stopped:") {
		t.Fatalf("an emoji produced an error message: %q", body)
	}
}

// TestDropPendingReactionsOnShutdown: when polling stops, a reaction
// still waiting out its debounce is abandoned — and the flush that
// wakes to an empty buffer releases the conversation instead of
// prompting the agent with nothing.
func TestDropPendingReactionsOnShutdown(t *testing.T) {
	hh, own := reactHarness(t, nil)
	before := hh.promptCount()
	hh.h.Handle(context.Background(), reactionEvent(humanID, own, "tada", zulipproto.ReactionAdd))
	hh.h.Handle(context.Background(), reactionEvent(4242, own, "+1", zulipproto.ReactionAdd))

	if n := hh.h.DropPendingReactions(); n != 2 {
		t.Fatalf("DropPendingReactions() = %d, want 2", n)
	}
	if n := hh.h.DropPendingReactions(); n != 0 {
		t.Fatalf("a second drop found %d reactions", n)
	}
	// The armed flush still fires; it must find nothing and do nothing.
	select {
	case hh.timer <- time.Now():
	case <-time.After(10 * time.Second):
		t.Fatal("no reaction flush was armed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := hh.h.WaitIdle(ctx); err != nil {
		t.Fatalf("handler did not go idle: %v", err)
	}
	if got := hh.promptCount(); got != before {
		t.Fatalf("an empty batch prompted the agent: %q", hh.lastPrompt())
	}
	// And the conversation is free for the next real turn.
	hh.deliver(t, "t", mention("still there?"))
	if got := hh.promptCount(); got != before+1 {
		t.Fatalf("the released conversation did not accept a new turn (%d prompts)", got)
	}
}
