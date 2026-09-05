package handler

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kfet/zulip-acp/internal/autotopic"
	"github.com/kfet/zulip-acp/internal/channels"
	"github.com/kfet/zulip-acp/internal/journal"
	"github.com/kfet/zulip-acp/internal/zulipproto"
)

// TestAutotopicNameFitsZulipsTopicLimit: the namer's budget IS the
// wire limit. If they ever drift, every generated topic starts failing
// the client's own length guard and silently falling back.
func TestAutotopicNameFitsZulipsTopicLimit(t *testing.T) {
	if autotopic.MaxLen != zulipproto.MaxTopicLength {
		t.Fatalf("autotopic.MaxLen = %d, zulipproto.MaxTopicLength = %d", autotopic.MaxLen, zulipproto.MaxTopicLength)
	}
}

// autotopicHarness builds a relay serving #fleet with general-chat
// naming switched on or off.
func autotopicHarness(t *testing.T, on bool) *harness {
	t.Helper()
	return newHarness(t, newAgent("ok"), func(c *Config) {
		cfg := channels.Config{Explicit: map[int64]string{4: "fleet"}}
		if on {
			cfg.Autotopic = map[int64]string{4: "fleet"}
		}
		c.Channels = channels.New(cfg)
	})
}

// topicOf returns the topic a posted message went to.
func (z *fakeZulip) topicOf(id int64) string {
	z.mu.Lock()
	defer z.mu.Unlock()
	return z.topics[id]
}

// TestAutotopicMovesGeneralChat: in a configured channel, a message in
// general chat (the empty topic) is moved to a topic named after it
// BEFORE the conversation is allocated, and the answer lands there.
func TestAutotopicMovesGeneralChat(t *testing.T) {
	hh := autotopicHarness(t, true)
	hh.deliver(t, "", mention("Deploy the relay tonight"))

	want := "Deploy the relay tonight"
	if got := hh.z.moved(); len(got) != 1 || got[0] != "1:"+want+":change_one" {
		t.Fatalf("moves = %v, want one change_one move to %q", got, want)
	}
	if got := hh.z.topicOf(hh.z.lastID()); got != want {
		t.Fatalf("answered in topic %q, want %q", got, want)
	}
	// The conversation must be allocated under the FINAL key — no
	// general-chat entry may be left behind to migrate later.
	if _, ok := hh.j.Lookup(journal.Channel(4, "")); ok {
		t.Fatal("a conversation was allocated in general chat")
	}
	if _, ok := hh.j.Lookup(journal.Channel(4, want)); !ok {
		t.Fatalf("no conversation allocated in topic %q", want)
	}
}

// TestAutotopicMovesGeneralChatDisplayName: the case v0.16.0 missed. A
// real Zulip 12.2 server (feature level 500) sends the empty topic as
// its translated DISPLAY NAME unless the client declares the
// empty_topic_name capability — which we deliberately do not. A
// `subject == ""` test therefore never fires in production.
func TestAutotopicMovesGeneralChatDisplayName(t *testing.T) {
	hh := autotopicHarness(t, true)
	hh.deliver(t, "general chat", mention("Deploy the relay tonight"))

	want := "Deploy the relay tonight"
	if got := hh.z.moved(); len(got) != 1 || got[0] != "1:"+want+":change_one" {
		t.Fatalf("moves = %v, want one change_one move to %q", got, want)
	}
	if got := hh.z.topicOf(hh.z.lastID()); got != want {
		t.Fatalf("answered in topic %q, want %q", got, want)
	}
	if _, ok := hh.j.Lookup(journal.Channel(4, "general chat")); ok {
		t.Fatal("a conversation was allocated under the general-chat display name")
	}
	if _, ok := hh.j.Lookup(journal.Channel(4, want)); !ok {
		t.Fatalf("no conversation allocated in topic %q", want)
	}
}

// TestAutotopicLeavesOtherChannelsAlone: general chat is an ordinary
// topic in a channel that is not configured for naming.
func TestAutotopicLeavesOtherChannelsAlone(t *testing.T) {
	hh := autotopicHarness(t, false)
	hh.deliver(t, "", mention("Deploy the relay tonight"))

	if got := hh.z.moved(); len(got) != 0 {
		t.Fatalf("moves = %v, want none in a non-autotopic channel", got)
	}
	if got := hh.z.topicOf(hh.z.lastID()); got != "" {
		t.Fatalf("answered in topic %q, want general chat", got)
	}
	if _, ok := hh.j.Lookup(journal.Channel(4, "")); !ok {
		t.Fatal("general chat conversation was not allocated")
	}
}

// TestAutotopicNamedTopicUntouched: only general chat is moved. A
// message that already has a topic keeps it.
func TestAutotopicNamedTopicUntouched(t *testing.T) {
	hh := autotopicHarness(t, true)
	hh.deliver(t, "release", mention("cut v1"))
	if got := hh.z.moved(); len(got) != 0 {
		t.Fatalf("moves = %v, want none for an already-named topic", got)
	}
	if got := hh.z.topicOf(hh.z.lastID()); got != "release" {
		t.Fatalf("answered in topic %q, want %q", got, "release")
	}
}

// TestAutotopicFallsBackWhenMoveFails: a realm that refuses the move
// (policy, an older server, a transport fault) must still get its
// answer — in general chat, where the message actually is.
func TestAutotopicFallsBackWhenMoveFails(t *testing.T) {
	hh := autotopicHarness(t, true)
	hh.z.moveErr = errors.New("you don't have permission to edit this message")
	hh.deliver(t, "", mention("Deploy the relay tonight"))

	if hh.z.count() == 0 {
		t.Fatal("a failed move dropped the turn")
	}
	if got := hh.z.topicOf(hh.z.lastID()); got != "" {
		t.Fatalf("answered in topic %q, want general chat", got)
	}
	if !hh.logged("answering in general chat") {
		t.Fatal("the failed move was not logged")
	}
	if _, ok := hh.j.Lookup(journal.Channel(4, "")); !ok {
		t.Fatal("general chat conversation was not allocated after a failed move")
	}
}

// TestAutotopicDisplayNameFallsBackWhenMoveFails: when the move fails
// on the form production actually sees, the key stays exactly the
// topic string the server sent — the display name — and that string
// remains the journal key. Nothing is normalised behind the relay's
// back.
func TestAutotopicDisplayNameFallsBackWhenMoveFails(t *testing.T) {
	hh := autotopicHarness(t, true)
	hh.z.moveErr = errors.New("you don't have permission to edit this message")
	hh.deliver(t, "general chat", mention("Deploy the relay tonight"))

	if got := hh.z.topicOf(hh.z.lastID()); got != "general chat" {
		t.Fatalf("answered in topic %q, want %q", got, "general chat")
	}
	if _, ok := hh.j.Lookup(journal.Channel(4, "general chat")); !ok {
		t.Fatal("no conversation allocated under the topic the server sent")
	}
	if _, ok := hh.j.Lookup(journal.Channel(4, "")); ok {
		t.Fatal("the display name was normalised to the empty topic")
	}
}

// TestAutotopicMoveHappensBeforeAllocation: the ordering the design
// depends on. If the conversation were allocated first, the relay
// would need a key migration — and the rename path exists for humans
// retitling a topic, not for the relay tidying up after itself.
func TestAutotopicMoveHappensBeforeAllocation(t *testing.T) {
	hh := autotopicHarness(t, true)
	var allocated bool
	hh.z.moveHook = func() {
		_, general := hh.j.Lookup(journal.Channel(4, ""))
		_, named := hh.j.Lookup(journal.Channel(4, "Deploy the relay tonight"))
		allocated = general || named
	}
	hh.deliver(t, "", mention("Deploy the relay tonight"))
	if allocated {
		t.Fatal("a conversation was allocated before the move")
	}
}

// TestAutotopicDisambiguatesACollision: two people opening general
// chat with the same words must not be dropped into one conversation.
func TestAutotopicDisambiguatesACollision(t *testing.T) {
	hh := autotopicHarness(t, true)
	if _, err := hh.j.Ensure(journal.Channel(4, "hello there")); err != nil {
		t.Fatalf("seed conversation: %v", err)
	}
	hh.deliver(t, "", mention("hello there"))

	want := "hello there (#1)"
	if got := hh.z.moved(); len(got) != 1 || got[0] != "1:"+want+":change_one" {
		t.Fatalf("moves = %v, want a move to %q", got, want)
	}
	if _, ok := hh.j.Lookup(journal.Channel(4, want)); !ok {
		t.Fatalf("no conversation allocated in topic %q", want)
	}
}

// TestAutotopicEngagesWithoutMention: autotopic and ambient compose —
// the realistic deployment is a channel where a bare general-chat
// message opens a named topic.
func TestAutotopicEngagesWithoutMention(t *testing.T) {
	hh := newHarness(t, newAgent("ok"), func(c *Config) {
		c.Channels = channels.New(channels.Config{
			Explicit:  map[int64]string{4: "fleet"},
			Ambient:   map[int64]string{4: "fleet"},
			Autotopic: map[int64]string{4: "fleet"},
		})
	})
	hh.deliver(t, "", "ship the release")
	if got := hh.z.moved(); len(got) != 1 || got[0] != "1:ship the release:change_one" {
		t.Fatalf("moves = %v, want a move to %q", got, "ship the release")
	}
}

// TestAutotopicMoveEventIsNotARename: the relay's own move comes back
// as an update_message with an EMPTY orig_subject. That is not a
// human retitling a topic, and must not migrate anything.
func TestAutotopicMoveEventIsNotARename(t *testing.T) {
	hh := autotopicHarness(t, true)
	hh.deliver(t, "", mention("Deploy the relay tonight"))
	before, ok := hh.j.Lookup(journal.Channel(4, "Deploy the relay tonight"))
	if !ok {
		t.Fatal("conversation missing after the move")
	}

	hh.h.Handle(context.Background(), zulipproto.Event{
		Type: zulipproto.EventUpdateMessage, StreamID: 4,
		OrigTopic: "", Topic: "Deploy the relay tonight",
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := hh.h.WaitIdle(ctx); err != nil {
		t.Fatalf("WaitIdle: %v", err)
	}
	after, ok := hh.j.Lookup(journal.Channel(4, "Deploy the relay tonight"))
	if !ok || after.ID != before.ID {
		t.Fatalf("conversation changed: %+v → %+v (ok=%v)", before, after, ok)
	}
}

// TestAutotopicOnlyOnTheOpeningMessage: once the conversation exists,
// a follow-up in that topic is answered in place — nothing is moved
// twice.
func TestAutotopicOnlyOnTheOpeningMessage(t *testing.T) {
	hh := autotopicHarness(t, true)
	hh.deliver(t, "", mention("Deploy the relay tonight"))
	hh.deliver(t, "Deploy the relay tonight", "and roll back if it fails")
	if got := hh.z.moved(); len(got) != 1 {
		t.Fatalf("moves = %v, want exactly one", got)
	}
}

// TestAutotopicMovesDespiteEngagedLobby is the production bug from
// v0.16.2: in #ask-fir the general-chat key had been engaged long
// before the feature shipped, so every general-chat message continued
// that legacy conversation and was never moved — the feature was inert
// in that channel, permanently. General chat is a LOBBY: what the
// journal holds for it is not engagement.
//
// The legacy conversation itself must be left strictly alone: not
// deleted, not retired, not migrated. It simply stops receiving new
// messages.
func TestAutotopicMovesDespiteEngagedLobby(t *testing.T) {
	// Both spellings of the lobby key: the display name production
	// sees, and the empty string a journal written by a client with
	// the empty_topic_name capability would hold.
	for _, lobbyTopic := range []string{"general chat", ""} {
		hh := autotopicHarness(t, true)
		lobbyKey := journal.Channel(4, lobbyTopic)
		before, err := hh.j.Ensure(lobbyKey)
		if err != nil {
			t.Fatalf("seed lobby conversation: %v", err)
		}
		hh.deliver(t, lobbyTopic, mention("Deploy the relay tonight"))

		want := "Deploy the relay tonight"
		if got := hh.z.moved(); len(got) != 1 || got[0] != "1:"+want+":change_one" {
			t.Fatalf("lobby %q: moves = %v, want one change_one move to %q", lobbyTopic, got, want)
		}
		if got := hh.z.topicOf(hh.z.lastID()); got != want {
			t.Fatalf("lobby %q: answered in topic %q, want %q", lobbyTopic, got, want)
		}
		if _, ok := hh.j.Lookup(journal.Channel(4, want)); !ok {
			t.Fatalf("lobby %q: no conversation allocated in topic %q", lobbyTopic, want)
		}
		after, ok := hh.j.Lookup(lobbyKey)
		if !ok || after.ID != before.ID {
			t.Fatalf("lobby %q: conversation changed: %+v → %+v (ok=%v)", lobbyTopic, before, after, ok)
		}
	}
}

// TestAutotopicFallbackDoesNotLatchOff: a failed move answers in
// general chat, which allocates a conversation under the lobby key.
// That conversation must not disable the feature for the channel — the
// NEXT general-chat message is still named and moved. Before the lobby
// framing a single transient API error switched autotopic off until
// somebody edited the journal by hand.
func TestAutotopicFallbackDoesNotLatchOff(t *testing.T) {
	hh := autotopicHarness(t, true)
	hh.z.moveErr = errors.New("you don't have permission to edit this message")
	hh.deliver(t, "general chat", mention("first question"))
	if got := hh.z.moved(); len(got) != 0 {
		t.Fatalf("moves = %v, want none after a failed move", got)
	}
	if _, ok := hh.j.Lookup(journal.Channel(4, "general chat")); !ok {
		t.Fatal("the fallback answer allocated no conversation in general chat")
	}

	hh.z.moveErr = nil
	hh.deliver(t, "general chat", mention("second question"))
	if got := hh.z.moved(); len(got) != 1 || got[0] != "1:second question:change_one" {
		t.Fatalf("moves = %v, want the next message to be moved", got)
	}
	if _, ok := hh.j.Lookup(journal.Channel(4, "second question")); !ok {
		t.Fatal("no conversation allocated in the moved topic")
	}
}

// TestAutotopicUnaddressedIsNotMoved: the move now runs before the
// addressed/engaged decision, so that decision has to be re-derived
// correctly. In a channel that is not ambient a general-chat message
// still needs the @-mention to summon the relay — an unaddressed one
// moves nothing and allocates nothing.
func TestAutotopicUnaddressedIsNotMoved(t *testing.T) {
	hh := autotopicHarness(t, true)
	hh.deliver(t, "general chat", "just people talking amongst themselves")

	if got := hh.z.moved(); len(got) != 0 {
		t.Fatalf("moves = %v, want none for an unaddressed message", got)
	}
	if hh.z.count() != 0 {
		t.Fatal("relay answered an unaddressed general-chat message")
	}
	if _, ok := hh.j.Lookup(journal.Channel(4, "general chat")); ok {
		t.Fatal("an unaddressed message allocated a conversation")
	}
}

// TestAutotopicGatesRunBeforeTheMove: the gates that precede the move
// must keep preceding it. A sender outside AllowedUsers, and a
// `!command`, both consume the message with no topic move — a rejected
// sender must never be able to retopic anything, and `!help` in
// general chat must leave the channel exactly as it found it.
func TestAutotopicGatesRunBeforeTheMove(t *testing.T) {
	hh := cmdHarness(t, newAgent("ok"), func(c *Config) {
		c.Channels = channels.New(channels.Config{
			Explicit:  map[int64]string{4: "fleet"},
			Autotopic: map[int64]string{4: "fleet"},
		})
		c.AllowedUsers = map[int64]struct{}{42: {}}
	})
	hh.deliverAs(t, humanID, "general chat", mention("Deploy the relay tonight"))
	if got := hh.z.moved(); len(got) != 0 {
		t.Fatalf("moves = %v, want none from a sender outside the allowlist", got)
	}
	if hh.z.count() != 0 {
		t.Fatal("relay answered a user outside the allowlist")
	}

	hh.deliverAs(t, 42, "general chat", mention("!help"))
	if got := hh.z.moved(); len(got) != 0 {
		t.Fatalf("moves = %v, want none for a command", got)
	}
	if hh.z.count() == 0 {
		t.Fatal("!help was not answered")
	}
	if got := hh.z.topicOf(hh.z.lastID()); got != "general chat" {
		t.Fatalf("!help answered in topic %q, want general chat", got)
	}
	if _, ok := hh.j.Lookup(journal.Channel(4, "general chat")); ok {
		t.Fatal("a command allocated a conversation")
	}
}
