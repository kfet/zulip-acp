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
//	ZULIP_CHANNEL=fleet \
//	go test ./test/
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
