package mcpapps

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	acp "github.com/coder/acp-go-sdk"
	"github.com/kfet/acp-kit/client"
)

// fakeAgentEnv makes this test binary run as a fake ACP agent that
// speaks fir's dev.fir.mcp/v1 extension. See runFakeAgent.
const fakeAgentEnv = "ZULIP_ACP_MCPAPPS_FAKE_AGENT"

func TestMain(m *testing.M) {
	if os.Getenv(fakeAgentEnv) == "1" {
		runFakeAgent()
		return
	}
	os.Exit(m.Run())
}

// runFakeAgent answers a prompt with one MCP App tool call, shaped as
// fir 1.42.0 sends it: a tool_call with the tool _meta, then a completed
// tool_call_update with the result _meta, structuredContent, and a
// rawOutput in the session/load replay shape. It serves
// _fir/mcp/resources/read and _fir/mcp/tools/call for that session.
func runFakeAgent() {
	const sid = "fake-sess"
	var conn *acp.Connection
	handler := func(ctx context.Context, method string, params json.RawMessage) (any, *acp.RequestError) {
		var p map[string]any
		_ = json.Unmarshal(params, &p)
		switch method {
		case acp.AgentMethodInitialize:
			return map[string]any{
				"protocolVersion": acp.ProtocolVersionNumber,
				"agentCapabilities": map[string]any{"_meta": map[string]any{MetaKey: map[string]any{
					"toolCallMeta": true, "resourceBlocks": true,
					"methods": []string{MethodResourcesRead, MethodToolsCall},
				}}},
			}, nil
		case acp.AgentMethodSessionNew:
			return map[string]any{"sessionId": sid}, nil
		case acp.AgentMethodSessionPrompt:
			send := func(u map[string]any) {
				_ = conn.SendNotification(ctx, acp.ClientMethodSessionUpdate, map[string]any{"sessionId": sid, "update": u})
			}
			send(map[string]any{
				"sessionUpdate": "tool_call", "toolCallId": "tc1", "title": "mcp__ui__show_app",
				"status": "pending", "rawInput": map[string]any{"n": 1},
				"_meta": map[string]any{MetaKey: map[string]any{"server": "ui", "tool": "show_app",
					"toolMeta": map[string]any{"ui": map[string]any{"resourceUri": "ui://demo/app.html"}}}},
			})
			send(map[string]any{
				"sessionUpdate": "tool_call_update", "toolCallId": "tc1", "status": "completed",
				"content": []any{map[string]any{"type": "content", "content": map[string]any{"type": "text", "text": "shown"}}},
				"rawOutput": map[string]any{"content": []any{map[string]any{"type": "text", "text": "shown"}},
					"details": map[string]any{"mcp": map[string]any{"structuredContent": map[string]any{"v": 7}}}},
				"_meta": map[string]any{MetaKey: map[string]any{"server": "ui", "tool": "show_app",
					"structuredContent": map[string]any{"v": 7}, "resultMeta": map[string]any{"r": 1}}},
			})
			return map[string]any{"stopReason": "end_turn"}, nil
		case MethodResourcesRead:
			if p["sessionId"] != sid || p["server"] != "ui" {
				return nil, acp.NewInvalidParams(p)
			}
			return map[string]any{"contents": []any{map[string]any{
				"uri": p["uri"], "mimeType": "text/html;profile=mcp-app", "text": "<p>fake app</p>",
			}}}, nil
		case MethodToolsCall:
			if p["sessionId"] != sid || p["name"] != "show_app" {
				return nil, acp.NewInvalidParams(p)
			}
			return map[string]any{"content": []any{}, "structuredContent": p["arguments"]}, nil
		}
		return nil, acp.NewMethodNotFound(method)
	}
	conn = acp.NewConnection(handler, os.Stdout, os.Stdin)
	<-conn.Done()
}

// agentBackend is a Backend over a real acp-kit client.
type agentBackend struct {
	agent *client.AgentProc
	sid   acp.SessionId
}

func (b agentBackend) ReadResource(ctx context.Context, _, server, uri string) (json.RawMessage, error) {
	return b.agent.ExtMethod(ctx, b.sid, MethodResourcesRead, map[string]any{"server": server, "uri": uri})
}
func (b agentBackend) CallTool(ctx context.Context, _, server, name string, args json.RawMessage) (json.RawMessage, error) {
	return b.agent.ExtMethod(ctx, b.sid, MethodToolsCall, map[string]any{"server": server, "name": name, "arguments": args})
}
func (agentBackend) Message(context.Context, string, string, string) error {
	return errors.New("not in this test")
}

type observeSink struct {
	apps  **Apps
	links chan string
}

func (s observeSink) OnUpdate(_ context.Context, n acp.SessionNotification) error {
	if l := (*s.apps).Observe("conv1", n.Update); l != "" {
		s.links <- l
	}
	return nil
}

// TestFakeACPAgentEndToEnd drives the whole path over real ACP stdio:
// a ui:// tool call becomes a link, the shell's content URL serves the
// HTML read through _fir/mcp/resources/read, and a bridge tools/call
// reaches _fir/mcp/tools/call.
func TestFakeACPAgentEndToEnd(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	agent, err := client.Start(ctx, client.Config{
		Command: []string{exe, "-test.run", "^$"},
		Env:     append(os.Environ(), fakeAgentEnv+"=1"),
		Stderr:  io.Discard,
		Policy:  client.PermissionFunc(client.AllowAllPermissions),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = agent.Close() })
	if _, ok := agent.Caps().Extensions[MetaKey]; !ok {
		t.Fatalf("extension not advertised: %+v", agent.Caps().Extensions)
	}

	var apps *Apps
	sink := observeSink{apps: &apps, links: make(chan string, 4)}
	sid, err := agent.NewSession(ctx, t.TempDir(), sink, nil)
	if err != nil {
		t.Fatal(err)
	}
	apps, err = New(Config{PublicURL: "https://apps.example.com", Backend: agentBackend{agent: agent, sid: sid}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := agent.Prompt(ctx, sid, []acp.ContentBlock{acp.TextBlock("show")}); err != nil {
		t.Fatal(err)
	}
	link := <-sink.links
	if !strings.HasPrefix(link, "🧩 [Open app: show\\_app](https://apps.example.com/a/") {
		t.Fatalf("link = %q", link)
	}
	in := apps.byCall["conv1\x00tc1"]
	input, result := in.snapshot()
	if string(input) != `{"n":1}` || string(result) != `{"_meta":{"r":1},"content":[{"text":"shown","type":"text"}],"structuredContent":{"v":7}}` {
		t.Fatalf("input %s result %s", input, result)
	}

	srv := httptest.NewServer(apps.Handler())
	defer srv.Close()
	if resp, body := get(t, srv, "/c/"+in.id+"?t="+in.contentToken); resp.StatusCode != 200 || body != "<p>fake app</p>" {
		t.Fatalf("content: %d %q", resp.StatusCode, body)
	}
	c, err := dial(t, srv, in, "https://apps.example.com")
	if err != nil {
		t.Fatal(err)
	}
	r := c.call("tools/call", map[string]any{"name": "show_app", "arguments": map[string]any{"pick": "B"}})
	if res, _ := r["result"].(map[string]any); res == nil || res["structuredContent"].(map[string]any)["pick"] != "B" {
		t.Fatalf("tools/call = %v", r)
	}
	if e := errOf(c.call("tools/call", map[string]any{"name": "other"})); e == "" {
		t.Fatal("fir refusal not passed back")
	}
}
