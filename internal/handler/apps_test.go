package handler

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"
	"github.com/kfet/zulip-acp/internal/journal"
	"github.com/kfet/zulip-acp/internal/mcpapps"
)

// ExtMethod records "<sid> <method> <params>" and answers {"ok":true}.
func (a *fakeAgent) ExtMethod(_ context.Context, sid acp.SessionId, method string, params map[string]any) (json.RawMessage, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	b, _ := json.Marshal(params)
	a.extCalls = append(a.extCalls, string(sid)+" "+method+" "+string(b))
	if a.extErr != nil {
		return nil, a.extErr
	}
	return json.RawMessage(`{"ok":true}`), nil
}

// lateBackend lets mcpapps.New run before the Handler exists.
type lateBackend struct{ h **Handler }

func (b lateBackend) ReadResource(ctx context.Context, c, s, u string) (json.RawMessage, error) {
	return (*b.h).ReadResource(ctx, c, s, u)
}
func (b lateBackend) CallTool(ctx context.Context, c, s, n string, a json.RawMessage) (json.RawMessage, error) {
	return (*b.h).CallTool(ctx, c, s, n, a)
}
func (b lateBackend) Message(ctx context.Context, c, l, t string) error {
	return (*b.h).Message(ctx, c, l, t)
}

func appToolCall(id string) acp.SessionUpdate {
	return acp.SessionUpdate{ToolCall: &acp.SessionUpdateToolCall{
		ToolCallId: acp.ToolCallId(id), Title: "mcp__ui__show",
		Meta: map[string]any{mcpapps.MetaKey: map[string]any{
			"server": "ui", "tool": "show",
			"toolMeta": map[string]any{"ui": map[string]any{"resourceUri": "ui://demo/app.html"}},
		}},
	}}
}

func appToolDone(id string) acp.SessionUpdate {
	st := acp.ToolCallStatusCompleted
	return acp.SessionUpdate{ToolCallUpdate: &acp.SessionToolCallUpdate{
		ToolCallId: acp.ToolCallId(id), Status: &st,
		Meta: map[string]any{mcpapps.MetaKey: map[string]any{"server": "ui", "tool": "show"}},
	}}
}

func newAppsHarness(t *testing.T, agent *fakeAgent, tune func(*Config)) (*harness, *mcpapps.Apps) {
	t.Helper()
	var h *Handler
	apps, err := mcpapps.New(mcpapps.Config{PublicURL: "https://apps.example.com", Backend: lateBackend{&h}})
	if err != nil {
		t.Fatal(err)
	}
	hh := newHarness(t, agent, func(c *Config) {
		c.Apps = apps
		if tune != nil {
			tune(c)
		}
	})
	h = hh.h
	return hh, apps
}

func TestAppToolCallPostsLink(t *testing.T) {
	agent := newAgent("here is your app")
	agent.updates = []acp.SessionUpdate{appToolCall("tc1"), appToolDone("tc1")}
	hh, apps := newAppsHarness(t, agent, nil)
	hh.deliver(t, "apps", mention("show me"))
	hh.h.appPosts.Wait()
	var links int
	for _, b := range hh.z.stored() {
		if strings.Contains(b, "🧩 [Open app: show](https://apps.example.com/a/") {
			links++
		}
	}
	if links != 1 || apps.Len() != 1 {
		t.Fatalf("links = %d, instances = %d; stored %q", links, apps.Len(), hh.z.stored())
	}
}

func TestAppLinkPostFailureIsLogged(t *testing.T) {
	agent := newAgent()
	hh, _ := newAppsHarness(t, agent, nil)
	hh.z.sendErr = errors.New("zulip down")
	hh.h.postAppLink(journal.Key{StreamID: 4, Topic: "apps"}.Token(), "x")
	hh.h.appPosts.Wait()
	if !hh.logged("post app link") {
		t.Fatalf("logs: %q", hh.logs)
	}
}

func TestAppBackendExtMethods(t *testing.T) {
	agent := newAgent("ok")
	hh, _ := newAppsHarness(t, agent, nil)
	hh.deliver(t, "apps", mention("hi"))
	conv, _ := hh.j.Lookup(journal.Key{StreamID: 4, Topic: "apps"})
	ctx := context.Background()

	if _, err := hh.h.ReadResource(ctx, conv.ID, "ui", "ui://x"); err == nil || !strings.Contains(err.Error(), "does not support") {
		t.Fatalf("no ext cap: err = %v", err)
	}
	agent.mu.Lock()
	agent.exts = map[string]json.RawMessage{mcpapps.MetaKey: json.RawMessage(`{}`)}
	agent.mu.Unlock()
	if _, err := hh.h.CallTool(ctx, "nope", "ui", "t", nil); err == nil || !strings.Contains(err.Error(), "no live agent session") {
		t.Fatalf("no session: err = %v", err)
	}
	if _, err := hh.h.CallTool(ctx, "nope", "ui", "t", nil); err == nil {
		t.Fatal("want error")
	}
	if _, err := hh.h.ReadResource(ctx, "nope", "ui", "ui://x"); err == nil {
		t.Fatal("want error")
	}
	if raw, err := hh.h.ReadResource(ctx, conv.ID, "ui", "ui://x"); err != nil || string(raw) != `{"ok":true}` {
		t.Fatalf("read: %s %v", raw, err)
	}
	if _, err := hh.h.CallTool(ctx, conv.ID, "ui", "t", json.RawMessage(`{"n":1}`)); err != nil {
		t.Fatal(err)
	}
	sid := "sid-" + conv.ID
	want := []string{
		sid + ` _fir/mcp/resources/read {"server":"ui","uri":"ui://x"}`,
		sid + ` _fir/mcp/tools/call {"arguments":{"n":1},"name":"t","server":"ui"}`,
	}
	if strings.Join(agent.extCalls, "\n") != strings.Join(want, "\n") {
		t.Fatalf("calls:\n%s", strings.Join(agent.extCalls, "\n"))
	}
}

func TestAppMessageStartsTurn(t *testing.T) {
	agent := newAgent("answer")
	hh, _ := newAppsHarness(t, agent, nil)
	hh.deliver(t, "apps", mention("hi"))
	conv, _ := hh.j.Lookup(journal.Key{StreamID: 4, Topic: "apps"})
	if err := hh.h.Message(context.Background(), conv.ID, "show", "pick ``` B"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := hh.h.WaitIdle(ctx); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, b := range hh.z.stored() {
		if b == "🧩 **show** sent:\n````quote\npick ``` B\n````" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no echo in %q", hh.z.stored())
	}
	p := agent.prompted()
	if last := p[len(p)-1]; !strings.HasSuffix(last, "[app show] pick ``` B") {
		t.Fatalf("prompt = %q", last)
	}
}

func TestAppMessageGates(t *testing.T) {
	agent := newAgent("answer")
	hh, _ := newAppsHarness(t, agent, nil)
	ctx := context.Background()
	if err := hh.h.Message(ctx, "nope", "a", "b"); err == nil {
		t.Fatal("unknown conv: want error")
	}
	other, _ := hh.j.Ensure(journal.Key{StreamID: 99, Topic: "x"})
	if err := hh.h.Message(ctx, other.ID, "a", "b"); err == nil || !strings.Contains(err.Error(), "channel") {
		t.Fatalf("unserved channel: %v", err)
	}
	dm, _ := hh.j.Ensure(journal.Key{UserIDs: []int64{botID, humanID}})
	if err := hh.h.Message(ctx, dm.ID, "a", "b"); err == nil || !strings.Contains(err.Error(), "direct") {
		t.Fatalf("dm off: %v", err)
	}
	served, _ := hh.j.Ensure(journal.Key{StreamID: 4, Topic: "y"})
	hh.z.sendErr = errors.New("zulip down")
	if err := hh.h.Message(ctx, served.ID, "a", "b"); err == nil || !strings.Contains(err.Error(), "echo") {
		t.Fatalf("send fails: %v", err)
	}
}

func TestAppMessageDM(t *testing.T) {
	agent := newAgent("answer")
	hh, _ := newAppsHarness(t, agent, func(c *Config) { c.DMs = true })
	dm, _ := hh.j.Ensure(journal.Key{UserIDs: []int64{botID, humanID}})
	if err := hh.h.Message(context.Background(), dm.ID, "a", "b"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := hh.h.WaitIdle(ctx); err != nil {
		t.Fatal(err)
	}
}
