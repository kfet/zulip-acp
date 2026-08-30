package zulipproto

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// recordedReq captures what the client actually put on the wire.
type recordedReq struct {
	method string
	path   string
	query  url.Values
	form   url.Values
	body   []byte
	ctype  string
	user   string
	pass   string
}

// testServer is a scriptable Zulip stand-in.
type testServer struct {
	*httptest.Server
	mu   chan struct{} // 1-buffered mutex
	reqs []recordedReq
	// handler returns (status, jsonBody) for a recorded request.
	handler func(r recordedReq) (int, string)
}

func newServer(t *testing.T, h func(r recordedReq) (int, string)) *testServer {
	t.Helper()
	ts := &testServer{mu: make(chan struct{}, 1), handler: h}
	ts.mu <- struct{}{}
	ts.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		rec := recordedReq{
			method: r.Method,
			path:   r.URL.Path,
			query:  r.URL.Query(),
			body:   body,
			ctype:  r.Header.Get("Content-Type"),
		}
		rec.user, rec.pass, _ = r.BasicAuth()
		if strings.HasPrefix(rec.ctype, "application/x-www-form-urlencoded") {
			rec.form, _ = url.ParseQuery(string(body))
		}
		<-ts.mu
		ts.reqs = append(ts.reqs, rec)
		ts.mu <- struct{}{}
		status, out := ts.handler(rec)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, out)
	}))
	t.Cleanup(ts.Close)
	return ts
}

func (ts *testServer) requests() []recordedReq {
	<-ts.mu
	defer func() { ts.mu <- struct{}{} }()
	out := make([]recordedReq, len(ts.reqs))
	copy(out, ts.reqs)
	return out
}

func okJSON(extra string) string {
	if extra == "" {
		return `{"result":"success","msg":""}`
	}
	return `{"result":"success","msg":"",` + extra + `}`
}

func newClient(t *testing.T, ts *testServer) *Client {
	t.Helper()
	c, err := New(Config{Site: ts.URL, Email: "bot@example.com", APIKey: "key", HTTPClient: ts.Client()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func TestNewValidation(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
	}{
		{"empty site", Config{Email: "a", APIKey: "b"}},
		{"empty email", Config{Site: "https://z", APIKey: "b"}},
		{"empty key", Config{Site: "https://z", Email: "a"}},
		{"bad url", Config{Site: "://nope", Email: "a", APIKey: "b"}},
		{"bad scheme", Config{Site: "ftp://z", Email: "a", APIKey: "b"}},
	}
	for _, c := range cases {
		if _, err := New(c.cfg); err == nil {
			t.Fatalf("%s: want error", c.name)
		}
	}
	// Trailing slash and an explicit /api/v1 are both tolerated, and
	// the default HTTP client is long-poll friendly.
	for _, site := range []string{"https://z.example/", "https://z.example", "https://z.example/api/v1"} {
		c, err := New(Config{Site: site, Email: "a", APIKey: "b"})
		if err != nil {
			t.Fatalf("New(%q): %v", site, err)
		}
		if c.base != "https://z.example/api/v1" {
			t.Fatalf("New(%q).base = %q", site, c.base)
		}
		if c.hc.Timeout != DefaultLongPollTimeout {
			t.Fatalf("default client timeout = %s", c.hc.Timeout)
		}
	}
}

func TestMeAndAuth(t *testing.T) {
	ts := newServer(t, func(recordedReq) (int, string) {
		return 200, okJSON(`"user_id":9,"email":"bot@example.com","full_name":"fir-relay","is_bot":true`)
	})
	u, err := newClient(t, ts).Me(context.Background())
	if err != nil {
		t.Fatalf("Me: %v", err)
	}
	if u.UserID != 9 || !u.IsBot || u.FullName != "fir-relay" {
		t.Fatalf("Me = %+v", u)
	}
	req := ts.requests()[0]
	if req.path != "/api/v1/users/me" || req.user != "bot@example.com" || req.pass != "key" {
		t.Fatalf("request = %+v", req)
	}
}

func TestStreams(t *testing.T) {
	ts := newServer(t, func(recordedReq) (int, string) {
		return 200, okJSON(`"streams":[{"stream_id":4,"name":"fleet"},{"stream_id":5,"name":"general"}]`)
	})
	got, err := newClient(t, ts).Streams(context.Background())
	if err != nil {
		t.Fatalf("Streams: %v", err)
	}
	if len(got) != 2 || got[0].StreamID != 4 || got[0].Name != "fleet" {
		t.Fatalf("Streams = %+v", got)
	}
}

func TestSendMessage(t *testing.T) {
	ts := newServer(t, func(recordedReq) (int, string) { return 200, okJSON(`"id":42`) })
	id, err := newClient(t, ts).SendMessage(context.Background(), 4, "a topic", "hello")
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if id != 42 {
		t.Fatalf("id = %d", id)
	}
	req := ts.requests()[0]
	if req.method != http.MethodPost || req.path != "/api/v1/messages" {
		t.Fatalf("request = %+v", req)
	}
	for k, want := range map[string]string{"type": "stream", "to": "4", "topic": "a topic", "content": "hello"} {
		if got := req.form.Get(k); got != want {
			t.Fatalf("form[%s] = %q, want %q", k, got, want)
		}
	}
}

// TestOversizedMessageRefused is the guard against Zulip's silent
// truncation: the API would return success and eat the tail.
func TestOversizedMessageRefused(t *testing.T) {
	ts := newServer(t, func(recordedReq) (int, string) { return 200, okJSON(`"id":1`) })
	c := newClient(t, ts)
	big := strings.Repeat("x", MaxMessageLength+1)
	if _, err := c.SendMessage(context.Background(), 4, "t", big); err == nil {
		t.Fatal("want error on oversized send")
	}
	if err := c.EditMessage(context.Background(), 1, big); err == nil {
		t.Fatal("want error on oversized edit")
	}
	// Code points, not bytes: 10000 emoji is 40000 bytes and legal.
	if _, err := c.SendMessage(context.Background(), 4, "t", strings.Repeat("🙂", MaxMessageLength)); err != nil {
		t.Fatalf("emoji at the exact limit must be accepted: %v", err)
	}
	if n := len(ts.requests()); n != 1 {
		t.Fatalf("oversized payloads must not reach the wire; %d requests", n)
	}
}

func TestEditMessage(t *testing.T) {
	ts := newServer(t, func(recordedReq) (int, string) { return 200, okJSON("") })
	if err := newClient(t, ts).EditMessage(context.Background(), 7, "new body"); err != nil {
		t.Fatalf("EditMessage: %v", err)
	}
	req := ts.requests()[0]
	if req.method != http.MethodPatch || req.path != "/api/v1/messages/7" || req.form.Get("content") != "new body" {
		t.Fatalf("request = %+v", req)
	}
}

func TestGetMessage(t *testing.T) {
	ts := newServer(t, func(recordedReq) (int, string) {
		return 200, okJSON(`"message":{"id":7,"sender_id":9,"content":"raw **md**","stream_id":4,"subject":"topic"}`)
	})
	m, err := newClient(t, ts).GetMessage(context.Background(), 7)
	if err != nil {
		t.Fatalf("GetMessage: %v", err)
	}
	if m.ID != 7 || m.Content != "raw **md**" || m.Topic != "topic" {
		t.Fatalf("message = %+v", m)
	}
	if ts.requests()[0].query.Get("apply_markdown") != "false" {
		t.Fatal("must fetch raw markdown")
	}
}

func TestTopicMessages(t *testing.T) {
	ts := newServer(t, func(recordedReq) (int, string) {
		return 200, okJSON(`"messages":[{"id":1,"content":"a"},{"id":2,"content":"b"}]`)
	})
	got, err := newClient(t, ts).TopicMessages(context.Background(), 4, "sess", 10)
	if err != nil {
		t.Fatalf("TopicMessages: %v", err)
	}
	if len(got) != 2 || got[1].Content != "b" {
		t.Fatalf("messages = %+v", got)
	}
	q := ts.requests()[0].query
	if q.Get("anchor") != "newest" || q.Get("num_before") != "10" || q.Get("num_after") != "0" || q.Get("apply_markdown") != "false" {
		t.Fatalf("query = %v", q)
	}
	var narrow []map[string]any
	if err := json.Unmarshal([]byte(q.Get("narrow")), &narrow); err != nil {
		t.Fatalf("narrow: %v", err)
	}
	if len(narrow) != 2 || narrow[0]["operator"] != "channel" || narrow[1]["operand"] != "sess" {
		t.Fatalf("narrow = %+v", narrow)
	}
}

func TestUpload(t *testing.T) {
	var gotBody string
	ts := newServer(t, func(r recordedReq) (int, string) {
		gotBody = string(r.body)
		return 200, okJSON(`"url":"/user_uploads/2/ab/file.log"`)
	})
	u, err := newClient(t, ts).Upload(context.Background(), "file.log", strings.NewReader("payload"))
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if u != "/user_uploads/2/ab/file.log" {
		t.Fatalf("url = %q", u)
	}
	if !strings.Contains(gotBody, "payload") || !strings.Contains(gotBody, `filename="file.log"`) {
		t.Fatalf("multipart body = %q", gotBody)
	}
	if !strings.HasPrefix(ts.requests()[0].ctype, "multipart/form-data") {
		t.Fatalf("content type = %q", ts.requests()[0].ctype)
	}
}

// errReader fails on Read so Upload's io.Copy error branch is driven
// explicitly rather than by luck.
type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, fmt.Errorf("read boom") }

func TestUploadReadError(t *testing.T) {
	ts := newServer(t, func(recordedReq) (int, string) { return 200, okJSON(`"url":"/x"`) })
	if _, err := newClient(t, ts).Upload(context.Background(), "f", errReader{}); err == nil {
		t.Fatal("want read error")
	}
}

func TestUploadRequestError(t *testing.T) {
	c, err := New(Config{Site: "https://z.example", Email: "a", APIKey: "b"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// A cancelled context makes newRequest's caller fail at send time;
	// an invalid method makes it fail at build time.
	c.base = "https://z.example/api/v1\n"
	if _, err := c.Upload(context.Background(), "f", strings.NewReader("x")); err == nil {
		t.Fatal("want request build error")
	}
}

func TestRegisterAndEvents(t *testing.T) {
	ts := newServer(t, func(r recordedReq) (int, string) {
		if r.path == "/api/v1/register" {
			return 200, okJSON(`"queue_id":"q1","last_event_id":-1,"max_message_id":30`)
		}
		return 200, okJSON(`"events":[{"id":0,"type":"heartbeat"},{"id":1,"type":"message","message":{"id":33,"sender_id":5,"content":"hi","stream_id":4,"subject":"t"}}]`)
	})
	c := newClient(t, ts)
	res, err := c.Register(context.Background(), []string{"message", "update_message"}, [][2]string{{"channel", "4"}})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if res.QueueID != "q1" || res.LastEventID != -1 || res.MaxMessageID != 30 {
		t.Fatalf("register = %+v", res)
	}
	form := ts.requests()[0].form
	if form.Get("apply_markdown") != "false" {
		t.Fatal("register must ask for raw markdown")
	}
	var types []string
	if err := json.Unmarshal([]byte(form.Get("event_types")), &types); err != nil || len(types) != 2 {
		t.Fatalf("event_types = %q (%v)", form.Get("event_types"), err)
	}
	var narrow [][]string
	if err := json.Unmarshal([]byte(form.Get("narrow")), &narrow); err != nil || narrow[0][0] != "channel" || narrow[0][1] != "4" {
		t.Fatalf("narrow = %q (%v)", form.Get("narrow"), err)
	}

	evs, err := c.GetEvents(context.Background(), "q1", -1)
	if err != nil {
		t.Fatalf("GetEvents: %v", err)
	}
	if len(evs) != 2 || evs[0].Type != EventHeartbeat || evs[1].Message.Content != "hi" {
		t.Fatalf("events = %+v", evs)
	}
	q := ts.requests()[1].query
	if q.Get("queue_id") != "q1" || q.Get("last_event_id") != "-1" {
		t.Fatalf("query = %v", q)
	}
	// The `timeout` parameter is deliberately NOT sent: Zulip 12.2
	// reports it in ignored_parameters_unsupported.
	if q.Has("timeout") {
		t.Fatal("timeout parameter must not be sent")
	}
}

func TestRegisterWithoutNarrowOrTypes(t *testing.T) {
	ts := newServer(t, func(recordedReq) (int, string) {
		return 200, okJSON(`"queue_id":"q","last_event_id":0`)
	})
	if _, err := newClient(t, ts).Register(context.Background(), nil, nil); err != nil {
		t.Fatalf("Register: %v", err)
	}
	form := ts.requests()[0].form
	if form.Has("event_types") || form.Has("narrow") {
		t.Fatalf("form = %v", form)
	}
}

func TestDeleteQueue(t *testing.T) {
	ts := newServer(t, func(recordedReq) (int, string) { return 200, okJSON("") })
	if err := newClient(t, ts).DeleteQueue(context.Background(), "q1"); err != nil {
		t.Fatalf("DeleteQueue: %v", err)
	}
	req := ts.requests()[0]
	if req.method != http.MethodDelete || req.form.Get("queue_id") != "q1" {
		t.Fatalf("request = %+v", req)
	}
}

func TestAPIErrors(t *testing.T) {
	ts := newServer(t, func(r recordedReq) (int, string) {
		switch r.query.Get("queue_id") {
		case "dead":
			return 400, `{"result":"error","msg":"Bad event queue ID: dead","code":"BAD_EVENT_QUEUE_ID"}`
		case "nocode":
			return 500, `{"result":"error","msg":""}`
		case "garbage":
			return 200, `not json at all`
		case "baddecode":
			return 200, `{"result":"success","msg":"","events":"not-an-array"}`
		}
		return 400, `{"result":"error","msg":"nope","code":"BAD_REQUEST"}`
	})
	c := newClient(t, ts)
	ctx := context.Background()

	_, err := c.GetEvents(ctx, "dead", 5)
	if !IsBadEventQueue(err) {
		t.Fatalf("want BAD_EVENT_QUEUE_ID, got %v", err)
	}
	if !strings.Contains(err.Error(), "BAD_EVENT_QUEUE_ID") {
		t.Fatalf("error text = %q", err.Error())
	}

	_, err = c.GetEvents(ctx, "other", 5)
	if err == nil || IsBadEventQueue(err) {
		t.Fatalf("want plain API error, got %v", err)
	}

	// An error with no msg falls back to the HTTP status text and
	// renders without a code.
	_, err = c.GetEvents(ctx, "nocode", 5)
	if err == nil || !strings.Contains(err.Error(), "Internal Server Error") {
		t.Fatalf("error = %v", err)
	}

	if _, err := c.GetEvents(ctx, "garbage", 5); err == nil || !strings.Contains(err.Error(), "bad JSON") {
		t.Fatalf("error = %v", err)
	}
	if _, err := c.GetEvents(ctx, "baddecode", 5); err == nil || !strings.Contains(err.Error(), "decode") {
		t.Fatalf("error = %v", err)
	}
}

func TestIsBadEventQueueOnNonAPIError(t *testing.T) {
	if IsBadEventQueue(nil) {
		t.Fatal("nil is not a bad-queue error")
	}
	if IsBadEventQueue(fmt.Errorf("plain")) {
		t.Fatal("plain error is not a bad-queue error")
	}
	// Wrapped API errors are still recognised.
	wrapped := fmt.Errorf("outer: %w", &APIError{Status: 400, Code: "BAD_EVENT_QUEUE_ID", Msg: "gone"})
	if !IsBadEventQueue(wrapped) {
		t.Fatal("wrapped API error not recognised")
	}
	// A wrapper chain that bottoms out on a non-API error.
	if IsBadEventQueue(fmt.Errorf("outer: %w", fmt.Errorf("inner"))) {
		t.Fatal("non-API chain misclassified")
	}
}

func TestTransportError(t *testing.T) {
	c, err := New(Config{Site: "http://127.0.0.1:1", Email: "a", APIKey: "b",
		HTTPClient: &http.Client{Timeout: 2 * time.Second}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := c.Me(context.Background()); err == nil {
		t.Fatal("want transport error")
	}
}

func TestBuildRequestError(t *testing.T) {
	c, err := New(Config{Site: "https://z.example", Email: "a", APIKey: "b"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c.base = "https://z.example/api/v1\x7f"
	if err := c.EditMessage(context.Background(), 1, "x"); err == nil {
		t.Fatal("want build error")
	}
}

// bodyErrServer returns a Content-Length that outruns the body, so the
// read-body error branch is driven deterministically.
func TestReadBodyError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1000")
		_, _ = io.WriteString(w, "{")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		if hj, ok := w.(http.Hijacker); ok {
			conn, _, err := hj.Hijack()
			if err == nil {
				_ = conn.Close()
			}
		}
	}))
	defer srv.Close()
	c, err := New(Config{Site: srv.URL, Email: "a", APIKey: "b", HTTPClient: srv.Client()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := c.Me(context.Background()); err == nil {
		t.Fatal("want body read error")
	}
}

// TestServerErrorsPropagate drives the error return of every wrapper
// so a failure at the transport can never be mistaken for success.
func TestServerErrorsPropagate(t *testing.T) {
	ts := newServer(t, func(recordedReq) (int, string) {
		return 500, `{"result":"error","msg":"realm on fire"}`
	})
	c := newClient(t, ts)
	ctx := context.Background()
	if _, err := c.Me(ctx); err == nil {
		t.Fatal("Me: want error")
	}
	if _, err := c.Streams(ctx); err == nil {
		t.Fatal("Streams: want error")
	}
	if _, err := c.SendMessage(ctx, 4, "t", "x"); err == nil {
		t.Fatal("SendMessage: want error")
	}
	if err := c.EditMessage(ctx, 1, "x"); err == nil {
		t.Fatal("EditMessage: want error")
	}
	if _, err := c.GetMessage(ctx, 1); err == nil {
		t.Fatal("GetMessage: want error")
	}
	if _, err := c.TopicMessages(ctx, 4, "t", 5); err == nil {
		t.Fatal("TopicMessages: want error")
	}
	if _, err := c.Upload(ctx, "f", strings.NewReader("x")); err == nil {
		t.Fatal("Upload: want error")
	}
	if _, err := c.Register(ctx, nil, nil); err == nil {
		t.Fatal("Register: want error")
	}
	if err := c.DeleteQueue(ctx, "q"); err == nil {
		t.Fatal("DeleteQueue: want error")
	}
	if _, err := c.GetEvents(ctx, "q", 0); err == nil {
		t.Fatal("GetEvents: want error")
	}
}

// TestNarrowChannel pins the operand shape. Getting this wrong is
// silent: the queue registers fine and then delivers nothing.
func TestNarrowChannel(t *testing.T) {
	got := NarrowChannel("fleet")
	if got[0] != "channel" || got[1] != "fleet" {
		t.Fatalf("NarrowChannel = %v", got)
	}
}

func TestUsers(t *testing.T) {
	ts := newServer(t, func(recordedReq) (int, string) {
		return 200, okJSON(`"members":[{"user_id":8,"email":"a@b","is_bot":false},{"user_id":9,"email":"bot@b","is_bot":true}]`)
	})
	got, err := newClient(t, ts).Users(context.Background())
	if err != nil {
		t.Fatalf("Users: %v", err)
	}
	if len(got) != 2 || got[1].UserID != 9 || !got[1].IsBot {
		t.Fatalf("users = %+v", got)
	}
	if ts.requests()[0].path != "/api/v1/users" {
		t.Fatalf("path = %q", ts.requests()[0].path)
	}
}

func TestUsersError(t *testing.T) {
	ts := newServer(t, func(recordedReq) (int, string) {
		return 500, `{"result":"error","msg":"down"}`
	})
	if _, err := newClient(t, ts).Users(context.Background()); err == nil {
		t.Fatal("want error")
	}
}
