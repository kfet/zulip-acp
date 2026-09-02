package zulipproto

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// TestZFormShape pins the wire shape against zulip's own widget docs
// (and the trivia_quiz bot in zulip/python-zulip-api). It is a dev-docs
// subsystem, not a versioned API, so the shape is asserted structurally
// rather than trusted.
func TestZFormShape(t *testing.T) {
	got := ZForm("Options", []ZFormChoice{
		Choice("opus", "Claude Opus 4.5", "!model anthropic/claude-opus-4-5"),
	})
	var w struct {
		WidgetType string `json:"widget_type"`
		ExtraData  struct {
			Type    string        `json:"type"`
			Heading string        `json:"heading"`
			Choices []ZFormChoice `json:"choices"`
		} `json:"extra_data"`
	}
	if err := json.Unmarshal([]byte(got), &w); err != nil {
		t.Fatalf("not JSON: %v (%s)", err, got)
	}
	if w.WidgetType != WidgetTypeZForm || w.ExtraData.Type != "choices" || w.ExtraData.Heading != "Options" {
		t.Fatalf("widget = %s", got)
	}
	if len(w.ExtraData.Choices) != 1 {
		t.Fatalf("choices = %+v", w.ExtraData.Choices)
	}
	c := w.ExtraData.Choices[0]
	if c.Type != "multiple_choice" || c.ShortName != "opus" ||
		c.LongName != "Claude Opus 4.5" || c.Reply != "!model anthropic/claude-opus-4-5" {
		t.Fatalf("choice = %+v", c)
	}
}

// TestZFormWithNoChoicesIsNoWidget: a button form with no buttons is
// not a degraded widget, it is a broken one, and sending it would only
// risk the server rejecting a message whose markdown was fine.
func TestZFormWithNoChoicesIsNoWidget(t *testing.T) {
	if got := ZForm("Options", nil); got != "" {
		t.Fatalf("ZForm with no choices = %q", got)
	}
}

// TestSendWithWidget: widget_content rides alongside the ordinary
// content, on both the channel and the direct-message endpoint.
func TestSendWithWidget(t *testing.T) {
	widget := ZForm("Options", []ZFormChoice{Choice("new", "Fresh context", "!new")})

	ts := newServer(t, func(recordedReq) (int, string) { return 200, okJSON(`"id":7`) })
	c := newClient(t, ts)
	if _, err := c.SendMessageWidget(context.Background(), 4, "t", "body", widget); err != nil {
		t.Fatalf("SendMessageWidget: %v", err)
	}
	if _, err := c.SendDirectMessageWidget(context.Background(), []int64{8, 9}, "body", widget); err != nil {
		t.Fatalf("SendDirectMessageWidget: %v", err)
	}
	for i, req := range ts.requests() {
		if got := req.form.Get("widget_content"); got != widget {
			t.Fatalf("request %d widget_content = %q", i, got)
		}
		if got := req.form.Get("content"); got != "body" {
			t.Fatalf("request %d content = %q", i, got)
		}
	}
}

// TestSendWithoutWidgetOmitsTheParameter: the plain send path must not
// grow an empty widget_content, which older servers would report as an
// unsupported parameter for every message the relay posts.
func TestSendWithoutWidgetOmitsTheParameter(t *testing.T) {
	ts := newServer(t, func(recordedReq) (int, string) { return 200, okJSON(`"id":7`) })
	c := newClient(t, ts)
	if _, err := c.SendMessage(context.Background(), 4, "t", "body"); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if _, err := c.SendDirectMessage(context.Background(), []int64{8}, "body"); err != nil {
		t.Fatalf("SendDirectMessage: %v", err)
	}
	for i, req := range ts.requests() {
		if _, ok := req.form["widget_content"]; ok {
			t.Fatalf("request %d carries widget_content", i)
		}
	}
}

// TestWidgetSendsCheckLength: the widget is not counted against
// MAX_MESSAGE_LENGTH, but the content still is — Zulip truncates it
// silently either way.
func TestWidgetSendsCheckLength(t *testing.T) {
	ts := newServer(t, func(recordedReq) (int, string) { return 200, okJSON(`"id":1`) })
	c := newClient(t, ts)
	big := strings.Repeat("x", MaxMessageLength+1)
	if _, err := c.SendMessageWidget(context.Background(), 4, "t", big, "{}"); err == nil {
		t.Fatal("want error on oversized widget send")
	}
	if _, err := c.SendDirectMessageWidget(context.Background(), []int64{8}, big, "{}"); err == nil {
		t.Fatal("want error on oversized widget DM")
	}
	if _, err := c.SendDirectMessageWidget(context.Background(), nil, "body", "{}"); err == nil {
		t.Fatal("want error with no recipients")
	}
	if n := len(ts.requests()); n != 0 {
		t.Fatalf("refused payloads reached the wire: %d requests", n)
	}
}

// TestWidgetSendFailuresSurface: a server that rejects the message
// outright must report it, so the caller can retry without the widget.
func TestWidgetSendFailuresSurface(t *testing.T) {
	ts := newServer(t, func(recordedReq) (int, string) {
		return 400, `{"result":"error","msg":"widgets are disabled","code":"BAD_REQUEST"}`
	})
	c := newClient(t, ts)
	if _, err := c.SendMessageWidget(context.Background(), 4, "t", "body", "{}"); err == nil {
		t.Fatal("want error")
	}
	if _, err := c.SendDirectMessageWidget(context.Background(), []int64{8}, "body", "{}"); err == nil {
		t.Fatal("want error")
	}
}

// TestDeleteMessage: retiring a superseded `!opts` panel is the ONLY
// thing this relay deletes, and it must report a refusal honestly —
// deleting one's own message is a realm policy, not a given.
func TestDeleteMessage(t *testing.T) {
	ts := newServer(t, func(recordedReq) (int, string) { return 200, okJSON(`"msg":""`) })
	c := newClient(t, ts)
	if err := c.DeleteMessage(context.Background(), 42); err != nil {
		t.Fatalf("DeleteMessage: %v", err)
	}
	req := ts.requests()[0]
	if req.method != "DELETE" || req.path != "/api/v1/messages/42" {
		t.Fatalf("request = %+v", req)
	}

	bad := newServer(t, func(recordedReq) (int, string) {
		return 400, `{"result":"error","msg":"You don't have permission to delete this message","code":"BAD_REQUEST"}`
	})
	if err := newClient(t, bad).DeleteMessage(context.Background(), 42); err == nil {
		t.Fatal("want error")
	}
}

// TestRejectedByServer separates "this server will not accept what I
// sent" from "I could not reach the server" — the difference between a
// retry that can work and one that cannot.
func TestRejectedByServer(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"transport", errors.New("connection reset"), false},
		{"refusal", &APIError{Status: 400, Msg: "Widgets cannot be edited."}, true},
		{"not found", &APIError{Status: 404, Msg: "Invalid message(s)"}, true},
		{"server fault", &APIError{Status: 500, Msg: "Internal server error"}, false},
		{"wrapped refusal", fmt.Errorf("posting: %w", &APIError{Status: 400}), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := RejectedByServer(tc.err); got != tc.want {
				t.Fatalf("RejectedByServer(%v) = %v", tc.err, got)
			}
		})
	}
}

// TestIsMissing: the one 4xx that means "already in the state you
// wanted" — a caller retiring a message has nothing left to do.
func TestIsMissing(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"transport", errors.New("connection reset"), false},
		{"gone", &APIError{Status: 404, Msg: "Invalid message(s)"}, true},
		{"refused", &APIError{Status: 400, Msg: "not permitted"}, false},
		{"wrapped gone", fmt.Errorf("deleting: %w", &APIError{Status: 404}), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsMissing(tc.err); got != tc.want {
				t.Fatalf("IsMissing(%v) = %v", tc.err, got)
			}
		})
	}
}
