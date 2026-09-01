package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kfet/zulip-acp/internal/journal"
	"github.com/kfet/zulip-acp/internal/zulipproto"
)

// dmRecipients builds the polymorphic display_recipient payload Zulip
// sends for a direct message: a JSON array of user objects.
func dmRecipients(ids ...int64) json.RawMessage {
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, fmt.Sprintf(`{"id":%d,"email":"u%d@x","full_name":"U%d"}`, id, id, id))
	}
	return json.RawMessage("[" + strings.Join(parts, ",") + "]")
}

// dmHarness is a harness with DMs enabled.
func dmHarness(t *testing.T, agent *fakeAgent, tune func(*Config)) *harness {
	t.Helper()
	return newHarness(t, agent, func(c *Config) {
		c.DMs = true
		if tune != nil {
			tune(c)
		}
	})
}

// deliverDM feeds a direct message and waits for the turn to finish.
func (hh *harness) deliverDM(t *testing.T, sender int64, content string, recipients ...int64) {
	t.Helper()
	hh.h.Handle(context.Background(), zulipproto.Event{
		Type: zulipproto.EventMessage,
		Message: &zulipproto.Message{
			ID: 1, SenderID: sender, SenderName: "Kfet", Content: content,
			Type: zulipproto.MessageTypePrivate, DisplayRecipient: dmRecipients(recipients...),
		},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := hh.h.WaitIdle(ctx); err != nil {
		t.Fatalf("turn did not finish: %v", err)
	}
}

// TestDMNeedsNoMention is the whole gating difference: in a DM there is
// nobody else to be talking to, so the relay answers a bare message
// that would be ignored in a channel.
func TestDMNeedsNoMention(t *testing.T) {
	hh := dmHarness(t, newAgent("hello from the agent"), nil)
	hh.deliverDM(t, humanID, "no mention here", humanID, botID)
	if hh.z.count() == 0 {
		t.Fatal("relay ignored a direct message")
	}
	last := hh.z.order[len(hh.z.order)-1]
	if !strings.Contains(hh.z.body(last), "hello from the agent") {
		t.Fatalf("answer = %q", hh.z.body(last))
	}
	// It went out as a DM to the whole participant set, not into a
	// channel topic.
	got := hh.z.dms[last]
	if len(got) != 2 || got[0] != humanID || got[1] != botID {
		t.Fatalf("DM recipients = %v, want the sorted set [%d %d]", got, humanID, botID)
	}
	if hh.z.topics[last] != "" {
		t.Fatalf("a DM must not be posted into a topic, got %q", hh.z.topics[last])
	}
	convs := hh.j.Convs()
	if len(convs) != 1 || !convs[0].IsDM() {
		t.Fatalf("convs = %+v, want one DM conversation", convs)
	}
	if convs[0].StreamID != 0 || convs[0].Topic != "" {
		t.Fatalf("DM conversation carries channel identity: %+v", convs[0])
	}
}

// TestDMKeyIgnoresRecipientOrder: Zulip's display_recipient order is
// not contractual, so the same people must always resolve to the same
// conversation — otherwise every message would fork a fresh session.
func TestDMKeyIgnoresRecipientOrder(t *testing.T) {
	hh := dmHarness(t, newAgent("ok"), nil)
	hh.deliverDM(t, humanID, "first", humanID, botID, 77)
	hh.deliverDM(t, humanID, "second", 77, botID, humanID)
	if n := len(hh.j.Convs()); n != 1 {
		t.Fatalf("group DM forked into %d conversations", n)
	}
	if n := len(hh.a.prompts); n != 2 {
		t.Fatalf("prompts = %d, want both turns to reach the agent", n)
	}
}

// TestDMDisabledByDefault pins the opt-in: an unconfigured relay does
// not serve DMs at all.
func TestDMDisabledByDefault(t *testing.T) {
	hh := newHarness(t, newAgent("should not run"), nil)
	hh.deliverDM(t, humanID, "hello?", humanID, botID)
	if hh.z.count() != 0 {
		t.Fatal("relay answered a DM with dms disabled")
	}
	if !hh.logged("dms not enabled") {
		t.Fatalf("expected an opt-in log, got %v", hh.logs)
	}
}

// TestDMGuardsRunFirst: the bot's own messages and the realm's system
// bots are refused before any DM logic, so DM support can never open a
// self-loop.
func TestDMGuardsRunFirst(t *testing.T) {
	for _, c := range []struct {
		name string
		m    zulipproto.Message
	}{
		{"own message", zulipproto.Message{ID: 1, SenderID: botID, Type: zulipproto.MessageTypePrivate, Content: "loop", DisplayRecipient: dmRecipients(botID, humanID)}},
		{"system bot", zulipproto.Message{ID: 2, SenderID: 123, SenderRealm: zulipproto.SystemBotRealm, Type: zulipproto.MessageTypePrivate, Content: "welcome", DisplayRecipient: dmRecipients(botID, 123)}},
		{"realm bot", zulipproto.Message{ID: 3, SenderID: 55, Type: zulipproto.MessageTypePrivate, Content: "beep", DisplayRecipient: dmRecipients(botID, 55)}},
	} {
		hh := dmHarness(t, newAgent("nope"), func(cfg *Config) {
			cfg.BotSenderIDs = map[int64]struct{}{55: {}}
		})
		m := c.m
		hh.h.Handle(context.Background(), zulipproto.Event{Type: zulipproto.EventMessage, Message: &m})
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := hh.h.WaitIdle(ctx); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		cancel()
		if hh.z.count() != 0 || len(hh.j.Convs()) != 0 {
			t.Fatalf("%s: relay engaged with a message it must refuse", c.name)
		}
	}
}

// TestDMAllowlist: allowed_user_ids gates a DM exactly as it gates a
// channel message. It is the ONLY allowlist that can — a DM is in no
// channel.
func TestDMAllowlist(t *testing.T) {
	hh := dmHarness(t, newAgent("hi"), func(c *Config) {
		c.AllowedUsers = map[int64]struct{}{42: {}}
	})
	hh.deliverDM(t, humanID, "let me in", humanID, botID)
	if hh.z.count() != 0 {
		t.Fatal("relay answered a DM from a user outside the allowlist")
	}
	if !hh.logged("not allowed") {
		t.Fatal("expected an allowlist log")
	}
	hh.deliverDM(t, 42, "and me?", 42, botID)
	if hh.z.count() == 0 {
		t.Fatal("allowlisted user was not answered in a DM")
	}
}

// TestDMIgnoresChannelAllowlist: a DM-only relay serves no channel, and
// that must not stop it answering a DM.
func TestDMIgnoresChannelAllowlist(t *testing.T) {
	hh := dmHarness(t, newAgent("still here"), func(c *Config) {
		c.Channels = emptyChannels{}
	})
	hh.deliverDM(t, humanID, "anyone home", humanID, botID)
	if hh.z.count() == 0 {
		t.Fatal("empty channel allowlist suppressed a DM")
	}
}

// emptyChannels serves no channel at all.
type emptyChannels struct{}

func (emptyChannels) Name(int64) (string, bool) { return "", false }

// TestDMWithoutRecipients: display_recipient is polymorphic, so a
// message that does not carry a usable participant list has no conv key
// and nobody to answer. Dropped, loudly.
func TestDMWithoutRecipients(t *testing.T) {
	for _, raw := range []json.RawMessage{nil, json.RawMessage(`"fleet"`), json.RawMessage(`[]`)} {
		hh := dmHarness(t, newAgent("nope"), nil)
		m := zulipproto.Message{
			ID: 7, SenderID: humanID, Type: zulipproto.MessageTypePrivate,
			Content: "hi", DisplayRecipient: raw,
		}
		hh.h.Handle(context.Background(), zulipproto.Event{Type: zulipproto.EventMessage, Message: &m})
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := hh.h.WaitIdle(ctx); err != nil {
			t.Fatalf("%v", err)
		}
		cancel()
		if hh.z.count() != 0 {
			t.Fatalf("display_recipient %s: relay answered anyway", raw)
		}
		if !hh.logged("no usable recipient list") {
			t.Fatalf("display_recipient %s: expected a log, got %v", raw, hh.logs)
		}
	}
}

// TestUnknownMessageTypeIsDropped: neither a channel message nor a DM.
func TestUnknownMessageTypeIsDropped(t *testing.T) {
	hh := dmHarness(t, newAgent("nope"), nil)
	m := zulipproto.Message{ID: 8, SenderID: humanID, Type: "carrier-pigeon", Content: "hi"}
	hh.h.Handle(context.Background(), zulipproto.Event{Type: zulipproto.EventMessage, Message: &m})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	if err := hh.h.WaitIdle(ctx); err != nil {
		t.Fatalf("%v", err)
	}
	cancel()
	if hh.z.count() != 0 {
		t.Fatal("relay answered a message of unknown type")
	}
	if !hh.logged("unknown type") {
		t.Fatalf("expected a log, got %v", hh.logs)
	}
}

// TestDMJournalFailureIsReported drives the allocate-conversation error
// branch on the DM path.
func TestDMJournalFailureIsReported(t *testing.T) {
	hh := dmHarness(t, newAgent("nope"), nil)
	hh.breakJournal(t)
	hh.deliverDM(t, humanID, "hi", humanID, botID)
	if hh.z.count() != 0 {
		t.Fatal("relay answered despite failing to allocate a conversation")
	}
	if !hh.logged("allocate conversation for DM") {
		t.Fatalf("expected a DM-shaped allocation log, got %v", hh.logs)
	}
}

// TestTopicRenameNeverTouchesDMs: an update_message event with no
// channel id — the only shape a DM could ever produce — must not reach
// the rename path.
func TestTopicRenameNeverTouchesDMs(t *testing.T) {
	hh := dmHarness(t, newAgent("ok"), nil)
	hh.deliverDM(t, humanID, "hello", humanID, botID)
	before := hh.j.Convs()
	hh.h.Handle(context.Background(), zulipproto.Event{
		Type: zulipproto.EventUpdateMessage, StreamID: 0,
		OrigTopic: "old", Topic: "new",
	})
	after := hh.j.Convs()
	if len(after) != len(before) || after[0].ID != before[0].ID || after[0].IsDM() != true {
		t.Fatalf("rename disturbed the DM conversation: %+v → %+v", before, after)
	}
	if hh.logged("topic renamed") {
		t.Fatal("rename path ran for an event with no channel")
	}
}

// TestDMRollover: the 10k splitter path posts follow-up messages as
// DMs too, not into a channel.
func TestDMRollover(t *testing.T) {
	long := strings.Repeat("x", 300)
	hh := dmHarness(t, newAgent(long, long, long), func(c *Config) {
		c.Budget = 200
	})
	hh.deliverDM(t, humanID, "write a lot", humanID, botID)
	if hh.z.count() < 2 {
		t.Fatalf("expected a rollover, got %d messages", hh.z.count())
	}
	for _, id := range hh.z.order {
		if len(hh.z.dms[id]) != 2 {
			t.Fatalf("message %d was not sent as a DM (recipients %v)", id, hh.z.dms[id])
		}
	}
}

// TestConvPosterRoutes pins the one decision the poster makes.
func TestConvPosterRoutes(t *testing.T) {
	z := newZulip()
	dm := &convPoster{client: z, key: journal.DM([]int64{botID, humanID})}
	id, err := dm.Post(context.Background(), "hello")
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	if len(z.dms[id]) != 2 {
		t.Fatalf("DM key did not post as a DM: %v", z.dms)
	}
	// An edit is a PATCH on a message id and cannot tell the two
	// shapes apart — which is exactly why streaming works unchanged.
	if err := dm.Edit(context.Background(), id, "hello again"); err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if z.body(id) != "hello again" {
		t.Fatalf("body = %q", z.body(id))
	}
	ch := &convPoster{client: z, key: journal.Channel(4, "t")}
	cid, err := ch.Post(context.Background(), "in channel")
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	if z.topics[cid] != "t" || z.dms[cid] != nil {
		t.Fatalf("channel key posted as a DM: topic=%q dms=%v", z.topics[cid], z.dms[cid])
	}
}

// TestDescribe covers the log rendering of both key shapes, including
// a channel that has left the served set.
func TestDescribe(t *testing.T) {
	hh := dmHarness(t, newAgent("ok"), nil)
	if got := hh.h.describe(journal.Channel(4, "t")); got != `#fleet > "t"` {
		t.Fatalf("channel = %q", got)
	}
	if got := hh.h.describe(journal.Channel(99, "t")); !strings.Contains(got, "channel 99") {
		t.Fatalf("unserved channel = %q", got)
	}
	if got := hh.h.describe(journal.DM([]int64{humanID, botID})); got != "DM 8,9" {
		t.Fatalf("dm = %q", got)
	}
}

// TestDMIsAlwaysAddressed: a DM never takes the abstain path, even with
// a sentinel configured — it is addressed to the bot by construction,
// so the relay streams and always answers.
func TestDMIsAlwaysAddressed(t *testing.T) {
	hh := dmHarness(t, newAgent("<<SILENT>>"), nil)
	hh.deliverDM(t, humanID, "you there?", humanID, botID)
	if hh.z.count() == 0 {
		t.Fatal("a DM was abstained from; DMs are always addressed")
	}
	last := hh.z.order[len(hh.z.order)-1]
	if !strings.Contains(hh.z.body(last), "<<SILENT>>") {
		t.Fatalf("body = %q", hh.z.body(last))
	}
}
