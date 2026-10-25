package mcpapps

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/coder/websocket"
)

//go:embed shell/shell.html shell/shell.js
var shellFS embed.FS

// fetchTimeout bounds one agent request made for an app.
const fetchTimeout = 60 * time.Second

// maxRequestBytes bounds one WebSocket message from the shell.
const maxRequestBytes = 1 << 20

// maxInflight bounds the requests one bridge runs at once. A request
// past it is refused at once, so the read loop never blocks and a
// closed connection is seen without delay.
const maxInflight = 8

// Handler serves the shell, the app HTML and the WebSocket bridge. Bind
// it to a loopback address; a reverse proxy makes it public.
func (a *Apps) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /a/{id}", a.serveShell)
	mux.HandleFunc("GET /s/shell.js", serveJS)
	mux.HandleFunc("GET /c/{id}", a.serveContent)
	mux.HandleFunc("GET /ws/{id}", a.serveWS)
	return mux
}

func noStore(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Content-Type-Options", "nosniff")
}

// wsOrigin is the ws(s) form of the public origin.
func (a *Apps) wsOrigin() string {
	return strings.Replace(a.public.String(), "http", "ws", 1)
}

func (a *Apps) serveShell(w http.ResponseWriter, r *http.Request) {
	if a.lookup(r.PathValue("id"), r.URL.Query().Get("t"), false) == nil {
		http.Error(w, "this app link is not valid or has expired", http.StatusNotFound)
		return
	}
	frame := "'self'"
	if a.content != a.public {
		frame = a.content.String()
	}
	noStore(w)
	w.Header().Set("Content-Security-Policy", strings.Join([]string{
		"default-src 'none'",
		"script-src 'self'",
		"style-src 'unsafe-inline'",
		"connect-src " + a.wsOrigin(),
		"frame-src " + frame,
		"base-uri 'none'",
		"form-action 'none'",
		"frame-ancestors 'none'",
	}, "; "))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	b, _ := shellFS.ReadFile("shell/shell.html")
	_, _ = w.Write(b)
}

func serveJS(w http.ResponseWriter, _ *http.Request) {
	noStore(w)
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	b, _ := shellFS.ReadFile("shell/shell.js")
	_, _ = w.Write(b)
}

func (a *Apps) serveContent(w http.ResponseWriter, r *http.Request) {
	in := a.lookup(r.PathValue("id"), r.URL.Query().Get("t"), true)
	if in == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	res, err := a.resource(r.Context(), in)
	if err != nil {
		a.cfg.Logf("mcpapps: app %s: read %s: %v", in.id, in.uri, err)
		http.Error(w, "the app could not be loaded: "+err.Error(), http.StatusBadGateway)
		return
	}
	noStore(w)
	w.Header().Set("Content-Security-Policy", res.csp+"; frame-ancestors "+a.public.String())
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(res.html))
}

// resource returns the app HTML of in, read from the agent once and then
// kept for the life of the instance.
func (a *Apps) resource(ctx context.Context, in *instance) (resource, error) {
	in.mu.Lock()
	res := in.res
	in.mu.Unlock()
	if res != nil {
		return *res, nil
	}
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	raw, err := a.cfg.Backend.ReadResource(ctx, in.convID, in.server, in.uri)
	if err != nil {
		return resource{}, err
	}
	got, ok := parseResource(raw, in.uri)
	if !ok {
		return resource{}, errors.New("the server returned no HTML for " + in.uri)
	}
	in.mu.Lock()
	in.res = &got
	in.mu.Unlock()
	return got, nil
}

// wsRequest is one request from the shell.
type wsRequest struct {
	ID     int64           `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

// wsError is the error member of a reply.
type wsError struct {
	Message string `json:"message"`
}

// wsMessage is a reply (ID set) or a push (Method set) to the shell.
type wsMessage struct {
	ID     int64    `json:"id,omitempty"`
	Method string   `json:"method,omitempty"`
	Result any      `json:"result,omitempty"`
	Params any      `json:"params,omitempty"`
	Error  *wsError `json:"error,omitempty"`
}

// bridge is one shell connection.
type bridge struct {
	a    *Apps
	in   *instance
	conn *websocket.Conn
	wmu  sync.Mutex
}

func (b *bridge) send(ctx context.Context, m wsMessage) {
	b.wmu.Lock()
	defer b.wmu.Unlock()
	_ = b.conn.Write(ctx, websocket.MessageText, mustJSON(json.Marshal(m)))
}

func (a *Apps) serveWS(w http.ResponseWriter, r *http.Request) {
	in := a.lookup(r.PathValue("id"), r.URL.Query().Get("t"), false)
	if in == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	// The shell is the only page that may open the bridge. The app
	// itself runs in an opaque origin and sends "Origin: null". The
	// whole origin is compared, scheme included, so the library's
	// host-only check is turned off.
	if r.Header.Get("Origin") != a.public.String() {
		http.Error(w, "forbidden origin", http.StatusForbidden)
		return
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	conn.SetReadLimit(maxRequestBytes)
	ctx, cancel := context.WithCancel(r.Context())
	var wg sync.WaitGroup
	defer func() {
		cancel()
		wg.Wait()
	}()
	b := &bridge{a: a, in: in, conn: conn}
	results, unsub := in.subscribe()
	defer unsub()
	wg.Add(1)
	go func() {
		defer wg.Done()
		b.push(ctx, results)
	}()
	sem := make(chan struct{}, maxInflight)
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			_ = conn.CloseNow()
			return
		}
		var req wsRequest
		if json.Unmarshal(data, &req) != nil || req.ID == 0 {
			_ = conn.Close(websocket.StatusPolicyViolation, "bad request")
			return
		}
		select {
		case sem <- struct{}{}:
		default:
			b.send(ctx, wsMessage{ID: req.ID, Error: &wsError{Message: "too many requests in flight"}})
			continue
		}
		wg.Add(1)
		go func() {
			defer func() {
				<-sem
				wg.Done()
			}()
			res, err := b.handle(ctx, req)
			m := wsMessage{ID: req.ID, Result: res}
			if err != nil {
				m = wsMessage{ID: req.ID, Error: &wsError{Message: err.Error()}}
			}
			b.send(ctx, m)
		}()
	}
}

// push sends the tool result to the shell when it arrives after init.
func (b *bridge) push(ctx context.Context, results <-chan struct{}) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-results:
			_, res := b.in.snapshot()
			b.send(ctx, wsMessage{Method: "tool-result", Params: res})
		}
	}
}

// initResult is the reply to "init".
type initResult struct {
	Label       string          `json:"label"`
	Server      string          `json:"server"`
	Tool        string          `json:"tool"`
	ContentURL  string          `json:"contentUrl"`
	HostVersion string          `json:"hostVersion"`
	Input       json.RawMessage `json:"input,omitempty"`
	Result      json.RawMessage `json:"result,omitempty"`
}

type toolCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// messageParams is MCP Apps ui/message: a user message of content blocks.
type messageParams struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
}

func (b *bridge) handle(ctx context.Context, req wsRequest) (any, error) {
	a, in := b.a, b.in
	if a.cfg.Now().After(in.expires) {
		return nil, errors.New("this app link has expired")
	}
	switch req.Method {
	case "init":
		input, result := in.snapshot()
		return initResult{
			Label: in.label, Server: in.server, Tool: in.tool,
			ContentURL:  a.content.String() + "/c/" + in.id + "?t=" + in.contentToken,
			HostVersion: a.cfg.HostVersion,
			Input:       input, Result: result,
		}, nil
	case "tools/call":
		var p toolCallParams
		if json.Unmarshal(req.Params, &p) != nil || p.Name == "" {
			return nil, errors.New("tools/call needs a tool name")
		}
		if !a.toolAllowed(in, p.Name) {
			return nil, errors.New("tool " + p.Name + " is not available to apps")
		}
		if len(p.Arguments) == 0 || string(p.Arguments) == "null" {
			p.Arguments = json.RawMessage(`{}`)
		}
		ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
		defer cancel()
		return a.cfg.Backend.CallTool(ctx, in.convID, in.server, p.Name, p.Arguments)
	case "ui/message":
		var p messageParams
		_ = json.Unmarshal(req.Params, &p)
		var parts []string
		for _, c := range p.Content {
			if c.Type == "text" && strings.TrimSpace(c.Text) != "" {
				parts = append(parts, c.Text)
			}
		}
		text := strings.Join(parts, "\n\n")
		if text == "" {
			return nil, errors.New("ui/message has no text")
		}
		if utf8.RuneCountInString(text) > MaxMessageRunes {
			return nil, errors.New("ui/message is too long")
		}
		if !in.allowMessage(a.cfg.Now(), a.cfg.MessageBurst, a.cfg.MessageWindow) {
			return nil, errors.New("too many messages from this app; wait a minute")
		}
		ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
		defer cancel()
		if err := a.cfg.Backend.Message(ctx, in.convID, in.label, text); err != nil {
			return nil, err
		}
		return struct{}{}, nil
	}
	return nil, errors.New("unknown method " + req.Method)
}
