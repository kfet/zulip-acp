package handler

import (
	"context"
	"errors"
	"fmt"
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

// react feeds a reaction event and waits for any turn it started.
func (hh *harness) react(t *testing.T, ev zulipproto.Event) {
	t.Helper()
	hh.h.Handle(context.Background(), ev)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := hh.h.WaitIdle(ctx); err != nil {
		t.Fatalf("reaction turn did not finish: %v", err)
	}
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
			name: "removal",
			ev:   func(id int64) zulipproto.Event { return reactionEvent(humanID, id, "tada", zulipproto.ReactionRemove) },
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
			before := hh.promptCount()
			hh.react(t, c.ev(own))
			if got := hh.promptCount(); got != before {
				t.Fatalf("reaction reached the agent anyway: %q", hh.lastPrompt())
			}
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
	if !strings.HasPrefix(got, "[reaction] Ada Lovelace added :tada: to message 555 from Ada Lovelace (") {
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

	before := hh.promptCount()
	for _, id := range []int64{600, 600, 601, 601} {
		hh.react(t, reactionEvent(humanID, id, "tada", zulipproto.ReactionAdd))
	}
	if got := hh.promptCount(); got != before {
		t.Fatalf("a reaction outside the served set reached the agent: %q", hh.lastPrompt())
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
		hh.react(t, reactionEvent(humanID, int64(10_000+i), "tada", zulipproto.ReactionAdd))
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
	hh.react(t, reactionEvent(humanID, 20_001, "tada", zulipproto.ReactionAdd))
	hh.z.mu.Lock()
	gets = len(hh.z.gets)
	hh.z.mu.Unlock()
	if gets != reactionLookupsPerMinute+1 {
		t.Fatalf("%d lookups after the window rolled, want %d", gets, reactionLookupsPerMinute+1)
	}
}

// TestReactionNeverSupersedesATurn: a message may cancel a running
// turn, an emoji may not.
func TestReactionNeverSupersedesATurn(t *testing.T) {
	agent := newAgent("slow answer")
	agent.block = make(chan struct{})
	hh := newHarness(t, agent, func(c *Config) { c.Reactions = true })
	// Engage the topic first, with the agent free to answer.
	close(agent.block)
	hh.deliver(t, "t", mention("hi"))
	own := hh.z.lastID()

	agent.mu.Lock()
	agent.block = make(chan struct{})
	block := agent.block
	agent.mu.Unlock()
	hh.h.Handle(context.Background(), channelEvent(humanID, "t", "keep going"))
	<-agent.entered

	before := hh.promptCount()
	hh.h.Handle(context.Background(), reactionEvent(humanID, own, "tada", zulipproto.ReactionAdd))
	if got := hh.promptCount(); got != before {
		t.Fatalf("the reaction started a turn on top of a running one: %q", hh.lastPrompt())
	}
	if !hh.logged("a turn is already running") {
		t.Fatal("dropping a reaction mid-turn must be logged")
	}
	close(block)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := hh.h.WaitIdle(ctx); err != nil {
		t.Fatalf("turn did not finish: %v", err)
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
	before := hh.promptCount()
	hh.react(t, reactionEvent(humanID, own, "tada", zulipproto.ReactionAdd))
	if got := hh.promptCount(); got != before {
		t.Fatalf("a retired conversation answered a reaction: %q", hh.lastPrompt())
	}
}

// TestReactionOnUnservedChannelViaIndex: the served set moves
// underfoot, so a conversation in the index is not by itself
// permission to answer in it.
func TestReactionOnUnservedChannelViaIndex(t *testing.T) {
	hh, own := reactHarness(t, nil)
	hh.h.cfg.Channels = channels.New(channels.Config{Explicit: map[int64]string{7: "other"}})
	before := hh.promptCount()
	hh.react(t, reactionEvent(humanID, own, "tada", zulipproto.ReactionAdd))
	if got := hh.promptCount(); got != before {
		t.Fatalf("answered in a channel that left the served set: %q", hh.lastPrompt())
	}
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
		before := hh.promptCount()
		hh.react(t, reactionEvent(humanID, 900, "tada", zulipproto.ReactionAdd))
		if got := hh.promptCount(); got != before {
			t.Fatalf("a DM reaction landed with dms off: %q", hh.lastPrompt())
		}
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
		if got := hh.lastPrompt(); !strings.Contains(got, "added :tada: to message 900 from Ada Lovelace") {
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
		before := hh.promptCount()
		hh.react(t, reactionEvent(humanID, own, "tada", zulipproto.ReactionAdd))
		if got := hh.promptCount(); got != before {
			t.Fatalf("a DM reaction landed after DMs were switched off: %q", hh.lastPrompt())
		}
	})
	t.Run("no usable recipient list", func(t *testing.T) {
		hh := newHarness(t, newAgent("hello"), func(c *Config) { c.Reactions, c.DMs = true, true })
		broken := dm
		broken.DisplayRecipient = []byte(`"a channel name"`)
		hh.z.mu.Lock()
		hh.z.messages[900] = broken
		hh.z.mu.Unlock()
		hh.react(t, reactionEvent(humanID, 900, "tada", zulipproto.ReactionAdd))
		if got := hh.promptCount(); got != 0 {
			t.Fatalf("a DM with no participants was delivered: %q", hh.lastPrompt())
		}
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
	hh.react(t, reactionEvent(humanID, 4242, "tada", zulipproto.ReactionAdd))
	hh.react(t, reactionEvent(humanID, 4242, "tada", zulipproto.ReactionAdd))
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
	before := hh.promptCount()
	hh.react(t, reactionEvent(humanID, own, "wastebasket", zulipproto.ReactionAdd))
	if got := hh.promptCount(); got != before {
		t.Fatalf("a consumed reaction still reached the agent: %q", hh.lastPrompt())
	}
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
	before := hh.promptCount()
	hh.react(t, reactionEvent(humanID, 8888, "tada", zulipproto.ReactionAdd))
	if got := hh.promptCount(); got != before {
		t.Fatalf("answered in a channel that left the served set: %q", hh.lastPrompt())
	}
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

	before := hh.promptCount()
	hh.react(t, reactionEvent(humanID, 770, "tada", zulipproto.ReactionAdd))
	if got := hh.promptCount(); got != before {
		t.Fatalf("an unengaged topic answered: %q", hh.lastPrompt())
	}
	// Now the topic is engaged, so the same reaction resolves.
	hh.deliver(t, "later", mention("hello there"))
	hh.react(t, reactionEvent(humanID, 770, "tada", zulipproto.ReactionAdd))
	if got := hh.lastPrompt(); !strings.Contains(got, "to message 770 from Ada Lovelace") {
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
	before := hh.promptCount()
	hh.react(t, reactionEvent(55, own, "tada", zulipproto.ReactionAdd))
	if got := hh.promptCount(); got != before {
		t.Fatalf("a bot's reaction reached the agent: %q", hh.lastPrompt())
	}
	// Nothing was cached as a NAME for it, but the bot verdict is:
	// re-asking on every reaction a bot leaves is exactly the
	// per-event cost this path exists to avoid.
	hh.react(t, reactionEvent(55, own, "tada", zulipproto.ReactionAdd))
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
