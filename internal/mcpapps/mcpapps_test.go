package mcpapps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"
	"github.com/coder/websocket"
)

const appHTML = "<!doctype html><p>hi</p>"

type fakeBackend struct {
	mu       sync.Mutex
	reads    int
	readRaw  json.RawMessage
	readErr  error
	calls    []string
	callErr  error
	messages []string
	msgErr   error
}

func (b *fakeBackend) ReadResource(_ context.Context, conv, server, uri string) (json.RawMessage, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.reads++
	if b.readErr != nil {
		return nil, b.readErr
	}
	if b.readRaw != nil {
		return b.readRaw, nil
	}
	return json.RawMessage(fmt.Sprintf(`{"contents":[{"uri":%q,"mimeType":"text/html;profile=mcp-app","text":%q,
		"_meta":{"ui":{"csp":{"connectDomains":["https://api.example.com"]}}}}]}`, uri, appHTML)), nil
}

func (b *fakeBackend) CallTool(_ context.Context, conv, server, name string, args json.RawMessage) (json.RawMessage, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls = append(b.calls, conv+" "+server+" "+name+" "+string(args))
	if b.callErr != nil {
		return nil, b.callErr
	}
	return json.RawMessage(`{"content":[{"type":"text","text":"done"}]}`), nil
}

func (b *fakeBackend) Message(_ context.Context, conv, label, text string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.messages = append(b.messages, conv+" "+label+" "+text)
	return b.msgErr
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newApps(t *testing.T, tune func(*Config)) (*Apps, *fakeBackend, *clock) {
	t.Helper()
	b := &fakeBackend{}
	clk := &clock{t: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)}
	cfg := Config{PublicURL: "https://apps.example.com/", Backend: b, Now: clk.now, HostVersion: "1.0"}
	if tune != nil {
		tune(&cfg)
	}
	a, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return a, b, clk
}

func meta(m map[string]any) map[string]any { return map[string]any{MetaKey: m} }

func startUpdate(id string, m map[string]any) acp.SessionUpdate {
	return acp.SessionUpdate{ToolCall: &acp.SessionUpdateToolCall{
		ToolCallId: acp.ToolCallId(id), Title: "mcp__ui__show", Meta: m,
		RawInput: map[string]any{"n": 1},
	}}
}

func appMeta(vis ...string) map[string]any {
	ui := map[string]any{"resourceUri": "ui://demo/app.html"}
	if vis != nil {
		ui["visibility"] = vis
	}
	return meta(map[string]any{"server": "ui", "tool": "show", "toolMeta": map[string]any{"ui": ui}})
}

func doneUpdate(id string) acp.SessionUpdate {
	st := acp.ToolCallStatusCompleted
	title := "show"
	return acp.SessionUpdate{ToolCallUpdate: &acp.SessionToolCallUpdate{
		ToolCallId: acp.ToolCallId(id), Status: &st, Title: &title,
		Content: []acp.ToolCallContent{
			acp.ToolContent(acp.TextBlock("result text")),
			{Diff: &acp.ToolCallContentDiff{Path: "/x", NewText: "y"}},
		},
		Meta: meta(map[string]any{"server": "ui", "tool": "show",
			"structuredContent": map[string]any{"v": 2}, "resultMeta": map[string]any{"k": "m"}}),
	}}
}

// instanceOf makes an app instance and returns it.
func instanceOf(t *testing.T, a *Apps) *instance {
	t.Helper()
	line := a.Observe("conv1", startUpdate("tc1", appMeta()))
	if line == "" {
		t.Fatal("no link")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.byCall["conv1\x00tc1"]
}

func TestNewValidates(t *testing.T) {
	b := &fakeBackend{}
	for _, c := range []Config{
		{PublicURL: "https://apps.example.com"},
		{PublicURL: "", Backend: b},
		{PublicURL: "ftp://apps.example.com", Backend: b},
		{PublicURL: "https://apps.example.com/x", Backend: b},
		{PublicURL: "https://apps.example.com?q=1", Backend: b},
		{PublicURL: "http://%zz", Backend: b},
		{PublicURL: "https://apps.example.com", ContentURL: "https://apps.example.com", Backend: b},
		{PublicURL: "https://apps.example.com", ContentURL: "nope", Backend: b},
	} {
		if _, err := New(c); err == nil {
			t.Errorf("New(%+v) = nil error", c)
		}
	}
	a, err := New(Config{PublicURL: "http://apps.example.com", ContentURL: "https://c.example.com", Backend: b})
	if err != nil {
		t.Fatal(err)
	}
	if a.cfg.TTL != DefaultTTL || a.cfg.MessageBurst != DefaultMessageBurst || a.cfg.MessageWindow != DefaultMessageWindow || a.wsOrigin() != "ws://apps.example.com" {
		t.Fatalf("defaults: %+v", a.cfg)
	}
	a.cfg.Logf("logf default is a no-op")
}

func TestParseCall(t *testing.T) {
	if _, ok := parseCall(acp.SessionUpdate{}); ok {
		t.Fatal("non tool call parsed")
	}
	if _, ok := parseCall(startUpdate("x", nil)); ok {
		t.Fatal("no meta parsed")
	}
	if _, ok := parseCall(startUpdate("x", map[string]any{MetaKey: "str"})); ok {
		t.Fatal("bad meta parsed")
	}
	c, ok := parseCall(startUpdate("x", appMeta("app")))
	if !ok || c.uri != "ui://demo/app.html" || c.server != "ui" || c.tool != "show" || c.vis[0] != "app" || string(c.input) != `{"n":1}` || c.result != nil {
		t.Fatalf("start: %+v", c)
	}
	c, ok = parseCall(doneUpdate("x"))
	want := `{"_meta":{"k":"m"},"content":[{"text":"result text","type":"text"}],"structuredContent":{"v":2}}`
	if !ok || c.status != "completed" || c.title != "show" || string(c.result) != want {
		t.Fatalf("done: %+v %s", c, c.result)
	}
	// An update with no status, title or result fields.
	c, ok = parseCall(acp.SessionUpdate{ToolCallUpdate: &acp.SessionToolCallUpdate{ToolCallId: "x", Meta: meta(map[string]any{})}})
	if !ok || c.result != nil || c.status != "" {
		t.Fatalf("bare update: %+v", c)
	}
	// A completed update with no structured content or result meta.
	st := acp.ToolCallStatusCompleted
	c, _ = parseCall(acp.SessionUpdate{ToolCallUpdate: &acp.SessionToolCallUpdate{ToolCallId: "x", Status: &st, Meta: meta(map[string]any{})}})
	if string(c.result) != `{"content":[]}` {
		t.Fatalf("plain result = %s", c.result)
	}
}

func TestObserve(t *testing.T) {
	a, _, _ := newApps(t, nil)
	if a.Observe("conv1", acp.SessionUpdate{}) != "" {
		t.Fatal("non tool call made a link")
	}
	// An MCP tool without a ui resource only records visibility.
	if a.Observe("conv1", startUpdate("tc0", meta(map[string]any{"server": "ui", "tool": "secret",
		"toolMeta": map[string]any{"ui": map[string]any{"visibility": []string{"model"}}}}))) != "" {
		t.Fatal("non-app tool made a link")
	}
	if a.Observe("conv1", doneUpdate("unknown")) != "" {
		t.Fatal("update of an unknown call made a link")
	}
	in := instanceOf(t, a)
	if got := a.Link(in.id); got != "https://apps.example.com/a/"+in.id+"?t="+in.token {
		t.Fatalf("link = %q", got)
	}
	if a.Link("nope") != "" {
		t.Fatal("link for unknown id")
	}
	if a.Observe("conv1", startUpdate("tc1", appMeta())) != "" {
		t.Fatal("second start made a second link")
	}
	ch, unsub := in.subscribe()
	defer unsub()
	if a.Observe("conv1", doneUpdate("tc1")) != "" {
		t.Fatal("update made a link")
	}
	<-ch
	if input, res := in.snapshot(); string(input) != `{"n":1}` || res == nil {
		t.Fatalf("snapshot: %s %s", input, res)
	}
	// A full subscriber channel does not block a second result.
	a.Observe("conv1", doneUpdate("tc1"))
	a.Observe("conv1", doneUpdate("tc1"))
	if !a.toolAllowed(in, "show") || a.toolAllowed(in, "secret") || !a.toolAllowed(in, "unseen") {
		t.Fatal("visibility")
	}
}

func TestAppVisible(t *testing.T) {
	if !appVisible(nil) || !appVisible([]string{"model", "app"}) || appVisible([]string{"model"}) || appVisible([]string{}) {
		t.Fatal("appVisible")
	}
}

func TestLabel(t *testing.T) {
	if got := label(call{tool: "a[b](c)`*_\\\nd"}); got != "a\\[b\\]\\(c\\)\\`\\*\\_\\\\ d" {
		t.Fatalf("label = %q", got)
	}
	if got := label(call{uri: "ui://x"}); got != "ui://x" {
		t.Fatalf("label = %q", got)
	}
}

func TestExpiryAndLookup(t *testing.T) {
	a, _, clk := newApps(t, func(c *Config) { c.TTL = time.Hour })
	in := instanceOf(t, a)
	if a.lookup(in.id, in.token, false) != in || a.lookup(in.id, in.contentToken, true) != in {
		t.Fatal("lookup")
	}
	if a.lookup(in.id, in.contentToken, false) != nil || a.lookup("x", in.token, false) != nil {
		t.Fatal("wrong token accepted")
	}
	clk.add(2 * time.Hour)
	if a.lookup(in.id, in.token, false) != nil {
		t.Fatal("expired accepted")
	}
	a.Observe("conv1", startUpdate("tc2", appMeta()))
	if a.Len() != 1 {
		t.Fatalf("expired not purged: %d", a.Len())
	}
}

func TestAllowMessage(t *testing.T) {
	in := &instance{}
	now := time.Unix(0, 0)
	for i := 0; i < 2; i++ {
		if !in.allowMessage(now, 2, time.Minute) {
			t.Fatal("refused under the limit")
		}
	}
	if in.allowMessage(now, 2, time.Minute) {
		t.Fatal("allowed over the limit")
	}
	if !in.allowMessage(now.Add(time.Minute), 2, time.Minute) {
		t.Fatal("window did not slide")
	}
}

func TestCSPAndResource(t *testing.T) {
	csp := buildCSP(cspMeta{
		ResourceDomains: []string{"https://cdn.example.com", "'unsafe-eval'", "https://a.example.com; script-src *"},
		ConnectDomains:  []string{"wss://*.example.com:8443"},
	})
	for _, want := range []string{"default-src 'none'", "script-src 'unsafe-inline' https://cdn.example.com;",
		"connect-src wss://*.example.com:8443", "frame-src 'none'", "sandbox allow-scripts allow-forms"} {
		if !strings.Contains(csp, want) {
			t.Errorf("csp %q lacks %q", csp, want)
		}
	}
	if strings.Contains(csp, "unsafe-eval") || strings.Contains(csp, "script-src *") {
		t.Fatalf("injected: %q", csp)
	}
	if !strings.Contains(buildCSP(cspMeta{}), "script-src 'unsafe-inline';") {
		t.Fatal("empty csp")
	}
	if _, ok := parseResource(json.RawMessage(`nope`), "u"); ok {
		t.Fatal("bad json")
	}
	if _, ok := parseResource(json.RawMessage(`{"contents":[{"uri":"u","mimeType":"text/plain","text":"x"}]}`), "u"); ok {
		t.Fatal("not html")
	}
	r, ok := parseResource(json.RawMessage(`{"contents":[{"uri":"v","mimeType":"text/html","text":"no"},{"uri":"u","mimeType":"text/html","text":"yes"}]}`), "u")
	if !ok || r.html != "yes" {
		t.Fatalf("resource %+v", r)
	}
}

func get(t *testing.T, srv *httptest.Server, path string) (*http.Response, string) {
	t.Helper()
	resp, err := http.Get(srv.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(b)
}

func TestHTTP(t *testing.T) {
	a, b, _ := newApps(t, nil)
	in := instanceOf(t, a)
	srv := httptest.NewServer(a.Handler())
	defer srv.Close()

	resp, body := get(t, srv, "/a/"+in.id+"?t="+in.token)
	if resp.StatusCode != 200 || !strings.Contains(body, "/s/shell.js") ||
		!strings.Contains(resp.Header.Get("Content-Security-Policy"), "frame-src 'self'") ||
		resp.Header.Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("shell: %d %v", resp.StatusCode, resp.Header)
	}
	if resp, _ := get(t, srv, "/a/"+in.id+"?t=bad"); resp.StatusCode != 404 {
		t.Fatalf("shell bad token: %d", resp.StatusCode)
	}
	if resp, body := get(t, srv, "/s/shell.js"); resp.StatusCode != 200 || !strings.Contains(body, "ui/initialize") {
		t.Fatalf("js: %d", resp.StatusCode)
	}
	if resp, _ := get(t, srv, "/c/"+in.id+"?t="+in.token); resp.StatusCode != 404 {
		t.Fatalf("content with shell token: %d", resp.StatusCode)
	}
	b.readErr = errors.New("boom")
	if resp, _ := get(t, srv, "/c/"+in.id+"?t="+in.contentToken); resp.StatusCode != 502 {
		t.Fatalf("content read error: %d", resp.StatusCode)
	}
	b.readErr, b.readRaw = nil, json.RawMessage(`{"contents":[]}`)
	if resp, _ := get(t, srv, "/c/"+in.id+"?t="+in.contentToken); resp.StatusCode != 502 {
		t.Fatalf("content no html: %d", resp.StatusCode)
	}
	b.readRaw = nil
	for i := 0; i < 2; i++ {
		resp, body := get(t, srv, "/c/"+in.id+"?t="+in.contentToken)
		csp := resp.Header.Get("Content-Security-Policy")
		if resp.StatusCode != 200 || body != appHTML || !strings.Contains(csp, "connect-src https://api.example.com") ||
			!strings.HasSuffix(csp, "frame-ancestors https://apps.example.com") {
			t.Fatalf("content: %d %q %q", resp.StatusCode, body, csp)
		}
	}
	if b.reads != 3 {
		t.Fatalf("reads = %d, want the HTML cached after the first success", b.reads)
	}
	if resp, _ := get(t, srv, "/ws/"+in.id+"?t=bad"); resp.StatusCode != 404 {
		t.Fatalf("ws bad token: %d", resp.StatusCode)
	}
	// The right origin, but not a WebSocket upgrade: Accept refuses it.
	req, _ := http.NewRequest("GET", srv.URL+"/ws/"+in.id+"?t="+in.token, nil)
	req.Header.Set("Origin", "https://apps.example.com")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode == 101 || resp.StatusCode == 403 {
		t.Fatalf("plain GET: %d", resp.StatusCode)
	}
}

func TestShellFramesContentOrigin(t *testing.T) {
	a, _, _ := newApps(t, func(c *Config) { c.ContentURL = "https://usercontent.example.com" })
	in := instanceOf(t, a)
	srv := httptest.NewServer(a.Handler())
	defer srv.Close()
	resp, _ := get(t, srv, "/a/"+in.id+"?t="+in.token)
	if !strings.Contains(resp.Header.Get("Content-Security-Policy"), "frame-src https://usercontent.example.com;") {
		t.Fatalf("csp %q", resp.Header.Get("Content-Security-Policy"))
	}
}

type wsClient struct {
	t    *testing.T
	conn *websocket.Conn
	next int64
}

func dial(t *testing.T, srv *httptest.Server, in *instance, origin string) (*wsClient, error) {
	t.Helper()
	h := http.Header{}
	h.Set("Origin", origin)
	conn, _, err := websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(srv.URL, "http")+"/ws/"+in.id+"?t="+in.token,
		&websocket.DialOptions{HTTPHeader: h})
	if err != nil {
		return nil, err
	}
	t.Cleanup(func() { _ = conn.CloseNow() })
	return &wsClient{t: t, conn: conn}, nil
}

func (c *wsClient) read() map[string]any {
	c.t.Helper()
	_, b, err := c.conn.Read(context.Background())
	if err != nil {
		c.t.Fatal(err)
	}
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return m
}

func (c *wsClient) call(method string, params any) map[string]any {
	c.t.Helper()
	c.next++
	b, _ := json.Marshal(map[string]any{"id": c.next, "method": method, "params": params})
	if err := c.conn.Write(context.Background(), websocket.MessageText, b); err != nil {
		c.t.Fatal(err)
	}
	return c.read()
}

func errOf(m map[string]any) string {
	e, _ := m["error"].(map[string]any)
	s, _ := e["message"].(string)
	return s
}

func TestBridge(t *testing.T) {
	a, b, clk := newApps(t, func(c *Config) { c.MessageBurst = 1 })
	in := instanceOf(t, a)
	a.Observe("conv1", startUpdate("tc0", meta(map[string]any{"server": "ui", "tool": "secret",
		"toolMeta": map[string]any{"ui": map[string]any{"visibility": []string{"model"}}}})))
	srv := httptest.NewServer(a.Handler())
	defer srv.Close()

	if _, err := dial(t, srv, in, "https://evil.example.com"); err == nil {
		t.Fatal("foreign origin accepted")
	}
	c, err := dial(t, srv, in, "https://apps.example.com")
	if err != nil {
		t.Fatal(err)
	}
	init := c.call("init", nil)["result"].(map[string]any)
	if init["label"] != "show" || init["hostVersion"] != "1.0" || init["result"] != nil ||
		init["contentUrl"] != "https://apps.example.com/c/"+in.id+"?t="+in.contentToken {
		t.Fatalf("init = %v", init)
	}
	// The result arrives after init: it is pushed.
	a.Observe("conv1", doneUpdate("tc1"))
	if push := c.read(); push["method"] != "tool-result" {
		t.Fatalf("push = %v", push)
	}
	if r := c.call("tools/call", map[string]any{"name": "show", "arguments": map[string]any{"a": 1}}); r["result"] == nil {
		t.Fatalf("tools/call = %v", r)
	}
	c.call("tools/call", map[string]any{"name": "show", "arguments": nil})
	if e := errOf(c.call("tools/call", map[string]any{"name": "secret"})); !strings.Contains(e, "not available") {
		t.Fatalf("hidden tool: %q", e)
	}
	if e := errOf(c.call("tools/call", "bad")); !strings.Contains(e, "tool name") {
		t.Fatalf("bad params: %q", e)
	}
	b.callErr = errors.New("tool failed")
	if e := errOf(c.call("tools/call", map[string]any{"name": "show"})); e != "tool failed" {
		t.Fatalf("tool error: %q", e)
	}
	if got := strings.Join(b.calls, "|"); got != `conv1 ui show {"a":1}|conv1 ui show {}|conv1 ui show {}` {
		t.Fatalf("calls = %s", got)
	}
	text := func(s string) map[string]any {
		return map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": s}, map[string]any{"type": "image"}}}
	}
	if e := errOf(c.call("ui/message", text(" "))); !strings.Contains(e, "no text") {
		t.Fatalf("empty: %q", e)
	}
	if e := errOf(c.call("ui/message", text(strings.Repeat("x", MaxMessageRunes+1)))); !strings.Contains(e, "too long") {
		t.Fatalf("long: %q", e)
	}
	b.msgErr = errors.New("gone")
	if e := errOf(c.call("ui/message", text("pick B"))); e != "gone" {
		t.Fatalf("backend error: %q", e)
	}
	if e := errOf(c.call("ui/message", text("again"))); !strings.Contains(e, "too many") {
		t.Fatalf("rate: %q", e)
	}
	if b.messages[0] != "conv1 show pick B" {
		t.Fatalf("messages = %q", b.messages)
	}
	if e := errOf(c.call("nope", nil)); !strings.Contains(e, "unknown method") {
		t.Fatalf("unknown: %q", e)
	}
	clk.add(DefaultTTL + time.Second)
	if e := errOf(c.call("init", nil)); !strings.Contains(e, "expired") {
		t.Fatalf("expired: %q", e)
	}
	// A request with no id ends the connection.
	_ = c.conn.Write(context.Background(), websocket.MessageText, []byte(`{"method":"init"}`))
	if _, _, err := c.conn.Read(context.Background()); websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
		t.Fatalf("close = %v", err)
	}
}

func TestBridgeMessageOK(t *testing.T) {
	a, b, _ := newApps(t, nil)
	in := instanceOf(t, a)
	srv := httptest.NewServer(a.Handler())
	defer srv.Close()
	c, err := dial(t, srv, in, "https://apps.example.com")
	if err != nil {
		t.Fatal(err)
	}
	r := c.call("ui/message", map[string]any{"content": []any{map[string]any{"type": "text", "text": "a"}, map[string]any{"type": "text", "text": "b"}}})
	if r["error"] != nil || b.messages[0] != "conv1 show a\n\nb" {
		t.Fatalf("r = %v, messages = %q", r, b.messages)
	}
	// The client goes away: the server ends the bridge.
	_ = c.conn.Close(websocket.StatusNormalClosure, "")
}

func TestPersistence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "apps.json")
	a, _, clk := newApps(t, func(c *Config) { c.StatePath = path; c.TTL = time.Hour })
	in := instanceOf(t, a)
	a.Observe("conv1", doneUpdate("tc1"))

	b, _, _ := newApps(t, func(c *Config) { c.StatePath = path; c.Now = clk.now })
	got := b.lookup(in.id, in.token, false)
	if got == nil || got.convID != "conv1" || got.label != "show" {
		t.Fatalf("not restored: %+v", got)
	}
	if _, res := got.snapshot(); res == nil {
		t.Fatal("result not restored")
	}
	// The call key is restored too: a later update finds the instance.
	if b.Observe("conv1", startUpdate("tc1", appMeta())) != "" {
		t.Fatal("restored call made a second link")
	}
	clk.add(2 * time.Hour)
	c, _, _ := newApps(t, func(c *Config) { c.StatePath = path; c.Now = clk.now })
	if c.Len() != 0 {
		t.Fatal("expired instance restored")
	}
}

func TestPersistenceErrors(t *testing.T) {
	dir := t.TempDir()
	var logs []string
	logf := func(f string, args ...any) { logs = append(logs, fmt.Sprintf(f, args...)) }
	// A state path that is a directory cannot be read: start empty.
	a, _, _ := newApps(t, func(c *Config) { c.StatePath = t.TempDir(); c.Logf = logf })
	if a.Len() != 0 {
		t.Fatal("not empty")
	}
	bad := filepath.Join(dir, "bad.json")
	_ = os.WriteFile(bad, []byte("nope"), 0o600)
	newApps(t, func(c *Config) { c.StatePath = bad; c.Logf = logf })
	if _, err := os.Stat(bad + ".bad"); err != nil {
		t.Fatalf("bad state not set aside: %v", err)
	}
	// The temp file cannot be written: the directory does not exist.
	a, _, _ = newApps(t, func(c *Config) { c.StatePath = filepath.Join(dir, "missing", "a.json"); c.Logf = logf })
	instanceOf(t, a)
	// The rename fails: the target is a non-empty directory.
	target := filepath.Join(dir, "target")
	_ = os.MkdirAll(filepath.Join(target, "x"), 0o700)
	if err := writeAtomic(target, []byte("{}")); err == nil {
		t.Fatal("rename onto a directory: want error")
	}
	joined := strings.Join(logs, "\n")
	if !strings.Contains(joined, "save state") || !strings.Contains(joined, "unusable state") {
		t.Fatalf("logs = %q", logs)
	}
}

// TestConcurrentSaves: every conversation's turn can save at once, and
// the file must always parse.
func TestConcurrentSaves(t *testing.T) {
	path := filepath.Join(t.TempDir(), "apps.json")
	a, _, _ := newApps(t, func(c *Config) { c.StatePath = path })
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a.Observe(fmt.Sprintf("conv%d", i), startUpdate("tc", appMeta()))
		}()
	}
	wg.Wait()
	var all []saved
	b, _ := os.ReadFile(path)
	if err := json.Unmarshal(b, &all); err != nil || len(all) != 20 {
		t.Fatalf("state: %d entries, %v", len(all), err)
	}
}

func TestBridgeOriginScheme(t *testing.T) {
	a, _, _ := newApps(t, nil)
	in := instanceOf(t, a)
	srv := httptest.NewServer(a.Handler())
	defer srv.Close()
	for _, o := range []string{"http://apps.example.com", "null", ""} {
		if _, err := dial(t, srv, in, o); err == nil {
			t.Errorf("origin %q accepted", o)
		}
	}
}

// blockingBackend holds every tool call until release is closed.
type blockingBackend struct {
	fakeBackend
	entered chan struct{}
	release chan struct{}
}

func (b *blockingBackend) CallTool(ctx context.Context, conv, server, name string, args json.RawMessage) (json.RawMessage, error) {
	b.entered <- struct{}{}
	<-b.release
	return json.RawMessage(`{}`), nil
}

// TestBridgeBoundsInflight: past maxInflight requests the bridge refuses
// at once, so one client cannot start unbounded work.
func TestBridgeBoundsInflight(t *testing.T) {
	bb := &blockingBackend{entered: make(chan struct{}, maxInflight+1), release: make(chan struct{})}
	a, err := New(Config{PublicURL: "https://apps.example.com", Backend: bb})
	if err != nil {
		t.Fatal(err)
	}
	in := instanceOf(t, a)
	srv := httptest.NewServer(a.Handler())
	defer srv.Close()
	c, err := dial(t, srv, in, "https://apps.example.com")
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= maxInflight+1; i++ {
		b, _ := json.Marshal(map[string]any{"id": i, "method": "tools/call", "params": map[string]any{"name": "show"}})
		if err := c.conn.Write(context.Background(), websocket.MessageText, b); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < maxInflight; i++ {
		<-bb.entered
	}
	if m := c.read(); m["id"] != float64(maxInflight+1) || !strings.Contains(errOf(m), "in flight") {
		t.Fatalf("over the bound: %v", m)
	}
	close(bb.release)
	for i := 0; i < maxInflight; i++ {
		if m := c.read(); m["error"] != nil {
			t.Fatalf("reply %v", m)
		}
	}
}
