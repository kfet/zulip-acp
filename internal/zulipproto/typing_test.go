package zulipproto

import (
	"context"
	"testing"
	"time"
)

// TestSetTypingChannel pins the channel form: the endpoint, the op and
// the (stream_id, topic) target. This is quiet mode's ONLY liveness
// signal, so the wire shape is load-bearing.
func TestSetTypingChannel(t *testing.T) {
	ts := newServer(t, func(recordedReq) (int, string) { return 200, okJSON("") })
	if err := newClient(t, ts).SetTyping(context.Background(), TypingStart, 4, "a topic", nil); err != nil {
		t.Fatalf("SetTyping: %v", err)
	}
	req := ts.requests()[0]
	if req.method != "POST" || req.path != "/api/v1/typing" {
		t.Fatalf("request = %+v", req)
	}
	for k, want := range map[string]string{
		"op": "start", "type": "channel", "stream_id": "4", "topic": "a topic",
	} {
		if got := req.form.Get(k); got != want {
			t.Fatalf("form[%s] = %q, want %q", k, got, want)
		}
	}
}

// TestSetTypingDirect: a DM takes the `direct` form with a JSON list of
// recipient ids, and must never send stream_id — Zulip rejects both at
// once.
func TestSetTypingDirect(t *testing.T) {
	ts := newServer(t, func(recordedReq) (int, string) { return 200, okJSON("") })
	if err := newClient(t, ts).SetTyping(context.Background(), TypingStop, 0, "", []int64{7, 9}); err != nil {
		t.Fatalf("SetTyping: %v", err)
	}
	req := ts.requests()[0]
	if req.form.Get("type") != "direct" || req.form.Get("to") != "[7,9]" || req.form.Get("op") != "stop" {
		t.Fatalf("form = %v", req.form)
	}
	if _, ok := req.form["stream_id"]; ok {
		t.Fatalf("a direct typing notification must not carry stream_id: %v", req.form)
	}
}

// TestSetTypingRefusals: both malformed calls are caught before the
// wire, so a bug here can never spam the server.
func TestSetTypingRefusals(t *testing.T) {
	ts := newServer(t, func(recordedReq) (int, string) { return 200, okJSON("") })
	c := newClient(t, ts)
	if err := c.SetTyping(context.Background(), "typing", 4, "t", nil); err == nil {
		t.Fatal("want an error on an unknown op")
	}
	if err := c.SetTyping(context.Background(), TypingStart, 0, "", nil); err == nil {
		t.Fatal("want an error on a typing notification with no target")
	}
	if n := len(ts.requests()); n != 0 {
		t.Fatalf("%d malformed requests reached the wire", n)
	}
}

// TestTypingStartedExpiry covers every answer the realm snapshot can
// give. The refresh cadence is derived from this, so "cannot tell" has
// to be distinguishable from "the server said 15 seconds".
func TestTypingStartedExpiry(t *testing.T) {
	cases := []struct {
		name string
		body string
		code int
		want time.Duration
		err  bool
	}{
		{name: "reported", code: 200, want: 8 * time.Second,
			body: okJSON(`"queue_id":"q1","` + ServerTypingExpirySetting + `":8000`)},
		{name: "absent", code: 200, want: DefaultTypingExpiry, body: okJSON(`"queue_id":"q1"`)},
		{name: "nonsensical", code: 200, want: DefaultTypingExpiry,
			body: okJSON(`"queue_id":"q1","` + ServerTypingExpirySetting + `":0`)},
		{name: "unreadable", code: 200, err: true,
			body: okJSON(`"queue_id":"q1","` + ServerTypingExpirySetting + `":"soon"`)},
		{name: "server error", code: 400, err: true, body: `{"result":"error","msg":"nope"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts := newServer(t, func(r recordedReq) (int, string) {
				if r.path == "/api/v1/register" {
					return tc.code, tc.body
				}
				return 200, okJSON("")
			})
			got, err := newClient(t, ts).TypingStartedExpiry(context.Background())
			if tc.err {
				if err == nil {
					t.Fatal("want an error")
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("expiry = %v, err = %v, want %v", got, err, tc.want)
			}
		})
	}
}
