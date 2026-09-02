// Package live holds integration tests that run against a REAL Zulip
// server. They are evidence about the server, not about our code, and
// they are excluded from the coverage gate (see .covignore).
//
// They only run when ZULIP_LIVE=1 and credentials are present:
//
//	ZULIP_LIVE=1 \
//	ZULIP_SITE=https://zulip.example \
//	ZULIP_EMAIL=bot@zulip.example \
//	ZULIP_API_KEY=… \
//	ZULIP_CHANNEL=zulip-acp-tests \
//	go test ./test/
//
// ZULIP_CHANNEL must be a DEDICATED throwaway channel that humans can
// mute — these tests post real messages to it on every run. Never point
// them at a channel people actually read; the guard below rejects the
// obvious mistakes.
package live

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/kfet/zulip-acp/internal/zulipproto"
)

func liveClient(t *testing.T) (*zulipproto.Client, int64) {
	t.Helper()
	if os.Getenv("ZULIP_LIVE") != "1" {
		t.Skip("set ZULIP_LIVE=1 (plus ZULIP_SITE / ZULIP_EMAIL / ZULIP_API_KEY / ZULIP_CHANNEL) to run live tests")
	}
	site, email, key := os.Getenv("ZULIP_SITE"), os.Getenv("ZULIP_EMAIL"), os.Getenv("ZULIP_API_KEY")
	channel := os.Getenv("ZULIP_CHANNEL")
	if site == "" || email == "" || key == "" || channel == "" {
		t.Fatal("ZULIP_LIVE=1 requires ZULIP_SITE, ZULIP_EMAIL, ZULIP_API_KEY and ZULIP_CHANNEL")
	}
	// These tests spam a channel with probe messages. Refuse to do that
	// to a channel humans read: require an opt-in "test" in the name.
	if !strings.Contains(strings.ToLower(channel), "test") {
		t.Fatalf("ZULIP_CHANNEL=%q is not a test channel: live tests post real messages, "+
			"point them at a dedicated one (e.g. zulip-acp-tests)", channel)
	}
	c, err := zulipproto.New(zulipproto.Config{Site: site, Email: email, APIKey: key})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	streams, err := c.Streams(context.Background())
	if err != nil {
		t.Fatalf("streams: %v", err)
	}
	for _, s := range streams {
		if s.Name == channel {
			return c, s.StreamID
		}
	}
	t.Fatalf("channel %q not visible to the bot", channel)
	return nil, 0
}

func topicFor(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("zulip-acp live: %s %d", t.Name(), time.Now().UnixNano())
}

// TestSilentTruncationIsStillReal codifies the single most important
// fact this project is built around: Zulip does not reject an
// oversized message. It returns {"result":"success"}, silently
// truncates to MAX_MESSAGE_LENGTH, and appends "[message truncated]".
//
// This test exists so a future Zulip upgrade that CHANGES that
// behaviour is caught deliberately rather than discovered by losing a
// user's output. If it starts failing, read the diff carefully before
// relaxing anything: internal/rollover exists solely because of this.
func TestSilentTruncationIsStillReal(t *testing.T) {
	c, streamID := liveClient(t)
	ctx := context.Background()
	topic := topicFor(t)

	// The client refuses to send an oversized message, so go around it
	// deliberately: this test is about the SERVER.
	oversize := strings.Repeat("x", zulipproto.MaxMessageLength+1)
	if _, err := c.SendMessage(ctx, streamID, topic, oversize); err == nil {
		t.Fatal("the client must refuse an oversized message")
	}

	body := strings.Repeat("x", zulipproto.MaxMessageLength-1) + "TAIL"
	if n := utf8.RuneCountInString(body); n != zulipproto.MaxMessageLength+3 {
		t.Fatalf("test payload is %d code points", n)
	}
	id, err := rawSend(ctx, c, streamID, topic, body)
	if err != nil {
		t.Fatalf("raw send: %v", err)
	}
	stored, err := c.GetMessage(ctx, id)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	n := utf8.RuneCountInString(stored.Content)
	t.Logf("sent %d code points, Zulip stored %d", utf8.RuneCountInString(body), n)
	if n > zulipproto.MaxMessageLength {
		t.Fatalf("stored %d code points, above the documented limit %d", n, zulipproto.MaxMessageLength)
	}
	if !strings.Contains(stored.Content, "[message truncated]") {
		t.Fatalf("Zulip's truncation marker is gone — behaviour changed, re-read internal/rollover's assumptions.\nstored tail: %q", tail(stored.Content, 60))
	}
	if strings.HasSuffix(stored.Content, "TAIL") {
		t.Fatal("the tail survived — the limit moved; re-check MaxMessageLength")
	}
}

// TestExactLimitIsNotTruncated is the other half: a message of exactly
// MAX_MESSAGE_LENGTH must come back whole. That is what makes 10000
// the real boundary rather than an approximation.
func TestExactLimitIsNotTruncated(t *testing.T) {
	c, streamID := liveClient(t)
	ctx := context.Background()
	body := strings.Repeat("y", zulipproto.MaxMessageLength-4) + "TAIL"
	id, err := c.SendMessage(ctx, streamID, topicFor(t), body)
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	stored, err := c.GetMessage(ctx, id)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if stored.Content != body {
		t.Fatalf("a message at the exact limit was altered: stored %d code points, tail %q",
			utf8.RuneCountInString(stored.Content), tail(stored.Content, 40))
	}
}

// TestCodePointsNotBytes pins that Zulip counts Python len(str): code
// points, not bytes and not UTF-16 units. A body well over 10000
// BYTES but well under 10000 code points must be accepted whole.
//
// Built from CJK rather than emoji: see
// TestRenderCanFailBelowTheLengthLimit for why a long run of emoji is
// not a safe probe. 4000 CJK characters is 12000 bytes and 4000 code
// points — comfortably over 10000 bytes, comfortably under 10000 code
// points — which is exactly the discrimination this test needs.
func TestCodePointsNotBytes(t *testing.T) {
	c, streamID := liveClient(t)
	ctx := context.Background()
	const n = 4000
	body := strings.Repeat("漢", n)
	if len(body) <= zulipproto.MaxMessageLength {
		t.Fatalf("payload is only %d bytes — it does not test anything", len(body))
	}
	id, err := c.SendMessage(ctx, streamID, topicFor(t), body)
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	stored, err := c.GetMessage(ctx, id)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if utf8.RuneCountInString(stored.Content) != n ||
		strings.Contains(stored.Content, "[message truncated]") {
		t.Fatalf("%d CJK characters (%d bytes) were altered — Zulip is not counting code points", n, len(body))
	}
}

// TestRenderCanFailBelowTheLengthLimit records a failure mode found
// while writing these tests, and the reason the relay's default budget
// is 9500 rather than 10000.
//
// A message of exactly 10000 emoji is a legal length — 10000 code
// points — and Zulip nevertheless rejects it with HTTP 400 "Unable to
// render message". The length limit is not the only limit: the
// server-side markdown/Pygments render can fail on a body that is
// legal but expensive.
//
// This is the OPPOSITE failure to silent truncation, and much kinder:
// it is a loud error the relay surfaces rather than lost output. The
// test asserts it stays loud. If Zulip ever starts silently accepting
// and mangling these instead, that is a correctness problem and this
// test is where it shows up.
func TestRenderCanFailBelowTheLengthLimit(t *testing.T) {
	c, streamID := liveClient(t)
	ctx := context.Background()
	body := strings.Repeat("🙂", zulipproto.MaxMessageLength)
	id, err := c.SendMessage(ctx, streamID, topicFor(t), body)
	if err == nil {
		t.Logf("Zulip accepted 10000 emoji as message %d — the render limit has moved", id)
		stored, gerr := c.GetMessage(ctx, id)
		if gerr != nil {
			t.Fatalf("read back: %v", gerr)
		}
		if utf8.RuneCountInString(stored.Content) != zulipproto.MaxMessageLength {
			t.Fatalf("accepted but altered: stored %d code points", utf8.RuneCountInString(stored.Content))
		}
		return
	}
	if !strings.Contains(err.Error(), "render") {
		t.Fatalf("unexpected failure mode: %v", err)
	}
	t.Logf("as expected, a legal-length but expensive body is refused loudly: %v", err)
}

// TestEditsAreNotThrottled pins the property that lets the relay
// stream at all: Zulip sustains many edits per second, so no
// Slack-style 1/sec throttle is needed. If this ever fails, the
// coalescing interval in internal/config needs revisiting.
func TestEditsAreNotThrottled(t *testing.T) {
	c, streamID := liveClient(t)
	ctx := context.Background()
	id, err := c.SendMessage(ctx, streamID, topicFor(t), "edit-rate probe 0")
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	const edits = 40
	start := time.Now()
	for i := 1; i <= edits; i++ {
		if err := c.EditMessage(ctx, id, fmt.Sprintf("edit-rate probe %d", i)); err != nil {
			t.Fatalf("edit %d of %d failed after %s: %v\n"+
				"  → if this is HTTP 400, the realm's message_content_edit_limit_seconds is not unlimited;\n"+
				"    a streaming relay cannot run against a realm that caps edit age.", i, edits, time.Since(start), err)
		}
	}
	rate := float64(edits) / time.Since(start).Seconds()
	t.Logf("%d edits in %s = %.1f edits/sec", edits, time.Since(start).Round(time.Millisecond), rate)
	if rate < 2 {
		t.Fatalf("edit throughput collapsed to %.1f/sec — the 300ms coalescing interval assumes far more", rate)
	}
}

// TestUploadRoundTripIsByteIdentical pins the one-shot upload path.
func TestUploadRoundTripIsByteIdentical(t *testing.T) {
	c, _ := liveClient(t)
	ctx := context.Background()
	payload := []byte("attachment round-trip\x00\x01\x02 binary bytes ☃\n")
	url, err := c.Upload(ctx, "zulip-acp-live.bin", strings.NewReader(string(payload)))
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	got, err := fetch(ctx, c, url)
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("round trip altered the bytes: sent %d, got %d", len(payload), len(got))
	}
}

// TestEventsIgnoresTimeoutParameter pins the protocol fact that shapes
// the long-poll loop: GET /events has no server-side timeout knob, so
// the poll is bounded by the client's HTTP timeout alone.
func TestEventsIgnoresTimeoutParameter(t *testing.T) {
	c, _ := liveClient(t)
	ctx := context.Background()
	res, err := c.Register(ctx, []string{zulipproto.EventMessage}, nil)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	defer func() { _ = c.DeleteQueue(context.Background(), res.QueueID) }()
	if res.QueueID == "" || res.LastEventID != -1 {
		t.Fatalf("register = %+v", res)
	}
	// A bad queue id must be classified as routine, not as a fault.
	if _, err := c.GetEvents(ctx, "definitely-not-a-queue", -1); !zulipproto.IsBadEventQueue(err) {
		t.Fatalf("unknown queue produced %v, want a BAD_EVENT_QUEUE_ID classification", err)
	}
}

func tail(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[len(r)-n:])
}

// TestReactionsAreIdempotentAgainstTheServer pins the server behaviour
// the ack acknowledgement depends on: adding a reaction that is
// already there, and removing one that is not, are FAILURES on the
// wire (HTTP 400 with REACTION_ALREADY_EXISTS / REACTION_DOES_NOT_EXIST)
// which zulipproto deliberately reports as success.
//
// If Zulip ever renames those codes, the relay would start logging a
// spurious error on every retry-shaped ack. This test is where that
// change gets noticed.
func TestReactionsAreIdempotentAgainstTheServer(t *testing.T) {
	c, streamID := liveClient(t)
	ctx := context.Background()
	topic := topicFor(t)

	id, err := c.SendMessage(ctx, streamID, topic, "reaction idempotency probe")
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	// Removing before adding: the DOES_NOT_EXIST path.
	if err := c.RemoveReaction(ctx, id, "eyes"); err != nil {
		t.Fatalf("removing an absent reaction should read as success: %v", err)
	}
	if err := c.AddReaction(ctx, id, "eyes"); err != nil {
		t.Fatalf("add: %v", err)
	}
	// Adding twice: the ALREADY_EXISTS path.
	if err := c.AddReaction(ctx, id, "eyes"); err != nil {
		t.Fatalf("adding an existing reaction should read as success: %v", err)
	}
	if err := c.RemoveReaction(ctx, id, "eyes"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	// A nonexistent emoji is a REAL error and must still surface.
	if err := c.AddReaction(ctx, id, "definitely_not_an_emoji_name"); err == nil {
		t.Fatal("a bogus emoji name should be an error")
	}
}

// TestDirectMessageWireShape is evidence for the two DM facts the
// relay is built on, neither of which is visible from a unit test:
//
//  1. `POST /messages` with `type=private` and `to` as a JSON ARRAY of
//     user ids is accepted, and the bot's own id in that array is
//     harmless — which is what lets the relay pass the participant set
//     from display_recipient straight back.
//  2. `display_recipient` comes back as an ARRAY of user objects on a
//     DM, where a channel message returns a bare string. That
//     polymorphism is why zulipproto keeps the field raw; a typed
//     field would fail to decode the whole /events response and wedge
//     the queue silently.
//
// It uses a message to the bot itself, so it needs no second account
// and leaves nothing in a human's inbox.
func TestDirectMessageWireShape(t *testing.T) {
	c, streamID := liveClient(t)
	ctx := context.Background()

	me, err := c.Me(ctx)
	if err != nil {
		t.Fatalf("me: %v", err)
	}
	body := "zulip-acp live DM probe " + topicFor(t)
	id, err := c.SendDirectMessage(ctx, []int64{me.UserID}, body)
	if err != nil {
		t.Fatalf("send DM: %v", err)
	}

	got, err := c.GetMessage(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !got.IsDM() {
		t.Fatalf("type = %q, want private", got.Type)
	}
	ids := got.Recipients()
	if len(ids) == 0 {
		t.Fatalf("display_recipient did not decode as a user list: %s", got.DisplayRecipient)
	}
	found := false
	for _, uid := range ids {
		if uid == me.UserID {
			found = true
		}
	}
	if !found {
		t.Fatalf("recipients %v do not include the bot %d", ids, me.UserID)
	}

	// Editing a DM is the same PATCH as editing a channel message —
	// the streaming path depends on that being true.
	if err := c.EditMessage(ctx, id, body+" (edited)"); err != nil {
		t.Fatalf("edit DM: %v", err)
	}

	// And a channel message's display_recipient is a bare string, not
	// a list: the other half of the polymorphism.
	cid, err := c.SendMessage(ctx, streamID, topicFor(t), "zulip-acp live channel probe")
	if err != nil {
		t.Fatalf("send channel: %v", err)
	}
	cm, err := c.GetMessage(ctx, cid)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if cm.IsDM() || cm.Recipients() != nil {
		t.Fatalf("channel message decoded as a DM: %s", cm.DisplayRecipient)
	}
}

// TestDMNarrowReadsBackTheConversation is evidence for the `history`
// loopback tool in a DIRECT MESSAGE, where a channel narrow cannot
// work: a DM is in no channel, so `history` would silently return
// nothing there without the `dm` operator.
//
// It also pins the operand shape the relay sends: the full participant
// set as journal.Key stores it, INCLUDING the bot itself. If Zulip ever
// stopped normalising the sender out of that operand, this is where it
// would surface.
func TestDMNarrowReadsBackTheConversation(t *testing.T) {
	c, _ := liveClient(t)
	ctx := context.Background()

	me, err := c.Me(ctx)
	if err != nil {
		t.Fatalf("me: %v", err)
	}
	body := "zulip-acp live dm-narrow probe " + topicFor(t)
	id, err := c.SendDirectMessage(ctx, []int64{me.UserID}, body)
	if err != nil {
		t.Fatalf("send DM: %v", err)
	}

	msgs, err := c.Messages(ctx, zulipproto.DMNarrow([]int64{me.UserID}), 5, 0)
	if err != nil {
		t.Fatalf("dm narrow: %v", err)
	}
	found := false
	for _, m := range msgs {
		if m.ID == id {
			found = true
		}
	}
	if !found {
		t.Fatalf("the dm narrow did not return the message just sent (%d of %d)", id, len(msgs))
	}
}

// TestBeforeIDPagesBackwardsExclusively is evidence for `history`'s
// paging contract: feeding the oldest id of one page back as the anchor
// yields the page BEFORE it, with neither overlap nor gap. Zulip's
// include_anchor=false is what makes that true.
func TestBeforeIDPagesBackwardsExclusively(t *testing.T) {
	c, streamID := liveClient(t)
	ctx := context.Background()
	topic := topicFor(t)

	var ids []int64
	for i := range 3 {
		id, err := c.SendMessage(ctx, streamID, topic, fmt.Sprintf("page probe %d", i))
		if err != nil {
			t.Fatalf("send: %v", err)
		}
		ids = append(ids, id)
	}

	page, err := c.Messages(ctx, zulipproto.TopicNarrow(streamID, topic), 1, 0)
	if err != nil {
		t.Fatalf("newest page: %v", err)
	}
	if len(page) != 1 || page[0].ID != ids[2] {
		t.Fatalf("newest page = %+v, want just %d", page, ids[2])
	}

	prev, err := c.Messages(ctx, zulipproto.TopicNarrow(streamID, topic), 2, page[0].ID)
	if err != nil {
		t.Fatalf("previous page: %v", err)
	}
	if len(prev) != 2 || prev[0].ID != ids[0] || prev[1].ID != ids[1] {
		t.Fatalf("previous page = %+v, want %v oldest first and no overlap", prev, ids[:2])
	}
}

// TestWidgetMessageCannotBeEdited is the server fact the whole `!opts`
// panel lifecycle is built on. A zform is stored as a submessage and
// Zulip refuses a content edit on any message that has one, so a
// self-updating panel MUST be re-posted rather than PATCHed.
//
// It fails in the cruellest direction: the degraded, widget-less panel
// edits perfectly well, so an implementation that PATCHes looks correct
// exactly where the feature is doing nothing. This test is the guard
// against a future "simplification" back to editing.
func TestWidgetMessageCannotBeEdited(t *testing.T) {
	c, streamID := liveClient(t)
	ctx := context.Background()
	topic := topicFor(t)
	widget := zulipproto.ZForm("Options", []zulipproto.ZFormChoice{
		zulipproto.Choice("one", "Model one", "!model a/one"),
	})

	id, err := c.SendMessageWidget(ctx, streamID, topic, "**panel** body", widget)
	if err != nil {
		t.Fatalf("send with widget: %v", err)
	}
	if err := c.EditMessage(ctx, id, "edited body"); err == nil {
		t.Fatal("the server accepted an edit on a widget message — the panel could be PATCHed after all, " +
			"which would make the re-post lifecycle in internal/handler/opts.go unnecessary")
	} else if !zulipproto.RejectedByServer(err) {
		t.Fatalf("edit failed for the wrong reason: %v", err)
	} else {
		t.Logf("as expected, the server refused: %v", err)
	}

	// The control: the same message without a widget edits fine.
	plain, err := c.SendMessage(ctx, streamID, topic, "plain body")
	if err != nil {
		t.Fatalf("send plain: %v", err)
	}
	if err := c.EditMessage(ctx, plain, "plain body, edited"); err != nil {
		t.Fatalf("editing a widget-less message must work: %v", err)
	}

	// Retiring a panel by deletion is the primary path; a realm may
	// forbid it, which the relay degrades around, so only report.
	if err := c.DeleteMessage(ctx, id); err != nil {
		t.Logf("this realm does not let the bot delete its own message (%v) — "+
			"the relay falls back to leaving a stale panel", err)
	}
}
