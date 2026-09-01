package zulipproto

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestSendDirectMessage(t *testing.T) {
	ts := newServer(t, func(recordedReq) (int, string) { return 200, okJSON(`"id":77`) })
	id, err := newClient(t, ts).SendDirectMessage(context.Background(), []int64{4, 9}, "hello")
	if err != nil {
		t.Fatalf("SendDirectMessage: %v", err)
	}
	if id != 77 {
		t.Fatalf("id = %d", id)
	}
	req := ts.requests()[0]
	if req.method != http.MethodPost || req.path != "/api/v1/messages" {
		t.Fatalf("request = %+v", req)
	}
	if got := req.form.Get("type"); got != "private" {
		t.Fatalf("type = %q, want private", got)
	}
	// `to` must be a JSON ARRAY of user ids, not the deprecated
	// comma-separated or email forms.
	if got := req.form.Get("to"); got != "[4,9]" {
		t.Fatalf("to = %q, want a JSON array", got)
	}
	if got := req.form.Get("content"); got != "hello" {
		t.Fatalf("content = %q", got)
	}
	if req.form.Has("topic") {
		t.Fatalf("a DM must carry no topic: %v", req.form)
	}
}

// TestSendDirectMessageRefusals: MAX_MESSAGE_LENGTH applies to DMs as
// silently as it does to channel messages, and a DM with no recipients
// has nowhere to go.
func TestSendDirectMessageRefusals(t *testing.T) {
	ts := newServer(t, func(recordedReq) (int, string) { return 200, okJSON(`"id":1`) })
	c := newClient(t, ts)
	if _, err := c.SendDirectMessage(context.Background(), []int64{4}, strings.Repeat("x", MaxMessageLength+1)); err == nil {
		t.Fatal("want error on oversized DM")
	}
	if _, err := c.SendDirectMessage(context.Background(), nil, "hi"); err == nil {
		t.Fatal("want error on a DM with no recipients")
	}
	if n := len(ts.requests()); n != 0 {
		t.Fatalf("refused payloads must not reach the wire; %d requests", n)
	}
}

func TestSendDirectMessageServerError(t *testing.T) {
	ts := newServer(t, func(recordedReq) (int, string) {
		return 400, `{"result":"error","msg":"nope","code":"BAD_REQUEST"}`
	})
	if _, err := newClient(t, ts).SendDirectMessage(context.Background(), []int64{4}, "hi"); err == nil {
		t.Fatal("want error")
	}
}

// TestRecipients pins the polymorphic display_recipient trap: the same
// field is a channel NAME on a channel message and a user ARRAY on a
// DM, so it is decoded lazily and never as a typed field.
func TestRecipients(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		typ  string
		want []int64
		isDM bool
	}{
		{"dm", `[{"id":9,"email":"bot@x"},{"id":4,"email":"a@x"}]`, MessageTypePrivate, []int64{9, 4}, true},
		{"group dm", `[{"id":9},{"id":4},{"id":22}]`, MessageTypePrivate, []int64{9, 4, 22}, true},
		{"channel", `"fleet"`, MessageTypeStream, nil, false},
		{"absent", ``, MessageTypeStream, nil, false},
		{"empty list", `[]`, MessageTypePrivate, nil, true},
		{"garbage", `{"not":"a list"}`, MessageTypePrivate, nil, true},
	}
	for _, c := range cases {
		m := Message{Type: c.typ}
		if c.raw != "" {
			m.DisplayRecipient = json.RawMessage(c.raw)
		}
		got := m.Recipients()
		if len(got) != len(c.want) {
			t.Fatalf("%s: Recipients = %v, want %v", c.name, got, c.want)
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Fatalf("%s: Recipients = %v, want %v", c.name, got, c.want)
			}
		}
		if m.IsDM() != c.isDM {
			t.Fatalf("%s: IsDM = %v", c.name, m.IsDM())
		}
	}
}

// TestMessageDecodesBothRecipientShapes: a single /events response
// mixing a channel message and a DM must decode whole. A typed
// display_recipient would fail here and wedge the queue silently.
func TestMessageDecodesBothRecipientShapes(t *testing.T) {
	const raw = `[
		{"id":1,"type":"stream","display_recipient":"fleet","subject":"t"},
		{"id":2,"type":"private","display_recipient":[{"id":4},{"id":9}]}
	]`
	var msgs []Message
	if err := json.Unmarshal([]byte(raw), &msgs); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(msgs) != 2 || msgs[0].Recipients() != nil || len(msgs[1].Recipients()) != 2 {
		t.Fatalf("msgs = %+v", msgs)
	}
}

// TestNarrowChannelsDropsForDMs: a channel narrow is a conjunction, so
// it excludes every DM. Serving DMs means no narrow at all.
func TestNarrowChannelsDropsForDMs(t *testing.T) {
	if got := NarrowChannels([]string{"fleet"}, true); got != nil {
		t.Fatalf("a DM-serving relay must not narrow to a channel, got %v", got)
	}
	if got := NarrowChannels(nil, true); got != nil {
		t.Fatalf("got %v", got)
	}
}
