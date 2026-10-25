// Package mcpapps hosts MCP Apps for zulip-acp, outside Zulip.
//
// Zulip cannot render an iframe or HTML in a message. So when an agent
// tool declares a `ui://` resource (MCP Apps), the relay keeps an app
// INSTANCE for that tool call, posts a link to it in the topic, and
// serves a host shell at that link. The shell loads the app HTML into a
// sandboxed iframe and implements the host side of the MCP Apps
// postMessage protocol. It talks to the relay over a WebSocket, and the
// relay forwards the app's requests to the agent through fir's
// dev.fir.mcp/v1 ACP extension.
//
// The package knows nothing about Zulip. Everything it needs from the
// relay comes through Backend, and the conversation an instance belongs
// to is fixed when the instance is made, never taken from a request.
//
// See docs/mcp-apps-design.md.
package mcpapps

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	acp "github.com/coder/acp-go-sdk"
)

// Backend is what the relay does for an app. convID is the instance's
// conversation, never a value the app sent.
type Backend interface {
	// ReadResource reads a resource of an MCP server in the
	// conversation's live session (fir `_fir/mcp/resources/read`).
	ReadResource(ctx context.Context, convID, server, uri string) (json.RawMessage, error)
	// CallTool calls a tool of an MCP server in the conversation's live
	// session (fir `_fir/mcp/tools/call`) and returns the raw result.
	CallTool(ctx context.Context, convID, server, name string, args json.RawMessage) (json.RawMessage, error)
	// Message starts a new user turn in the conversation with text,
	// and echoes it into the topic. label names the app.
	Message(ctx context.Context, convID, label, text string) error
}

// Defaults for Config.
const (
	DefaultTTL           = 24 * time.Hour
	DefaultMessageBurst  = 5
	DefaultMessageWindow = time.Minute
	// MaxMessageRunes bounds the text of one ui/message.
	MaxMessageRunes = 8000
)

// Config configures Apps.
type Config struct {
	// PublicURL is the origin the links point at, for example
	// "https://apps.example.com". Required: it is what a reverse proxy
	// serves in front of the local listener. Origin only, no path.
	PublicURL string
	// ContentURL, if set, is a second origin the app HTML is served
	// from (for example "https://usercontent.example.com"). Without it
	// the app comes from PublicURL; the iframe sandbox and the CSP
	// `sandbox` directive still give it an opaque origin.
	ContentURL string
	// TTL is how long an instance and its link live. 0 is DefaultTTL.
	TTL time.Duration
	// MessageBurst ui/messages are allowed per MessageWindow per
	// instance. 0 is the default.
	MessageBurst  int
	MessageWindow time.Duration
	Backend       Backend
	// HostVersion is reported to the app in ui/initialize.
	HostVersion string
	// StatePath, if set, is a JSON file the instances are kept in, so
	// a link stays valid across a graceful reload or a restart.
	StatePath string
	Now       func() time.Time
	Logf      func(format string, args ...any)
}

// Apps is the instance store and the HTTP handler.
type Apps struct {
	cfg     Config
	public  *url.URL
	content *url.URL // == public when ContentURL is unset

	saveMu sync.Mutex
	mu     sync.Mutex
	byID   map[string]*instance
	byCall map[string]*instance // convID \x00 toolCallId
	vis    map[string][]string  // convID \x00 server \x00 tool
}

// New validates cfg and returns an empty store.
func New(cfg Config) (*Apps, error) {
	if cfg.Backend == nil {
		return nil, errors.New("mcpapps: Backend is required")
	}
	pub, err := parseOrigin(cfg.PublicURL)
	if err != nil {
		return nil, fmt.Errorf("mcpapps: public URL: %w", err)
	}
	content := pub
	if cfg.ContentURL != "" {
		if content, err = parseOrigin(cfg.ContentURL); err != nil {
			return nil, fmt.Errorf("mcpapps: content URL: %w", err)
		}
		if content.Host == pub.Host && content.Scheme == pub.Scheme {
			return nil, errors.New("mcpapps: content URL must be a different origin from the public URL")
		}
	}
	if cfg.TTL <= 0 {
		cfg.TTL = DefaultTTL
	}
	if cfg.MessageBurst <= 0 {
		cfg.MessageBurst = DefaultMessageBurst
	}
	if cfg.MessageWindow <= 0 {
		cfg.MessageWindow = DefaultMessageWindow
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	a := &Apps{
		cfg: cfg, public: pub, content: content,
		byID:   map[string]*instance{},
		byCall: map[string]*instance{},
		vis:    map[string][]string{},
	}
	a.load()
	return a, nil
}

// parseOrigin accepts an http(s) URL with a host and no path, query or
// fragment.
func parseOrigin(s string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSuffix(s, "/"))
	if err != nil {
		return nil, err
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return nil, fmt.Errorf("%q: scheme must be https or http", s)
	}
	if u.Host == "" || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return nil, fmt.Errorf("%q: must be an origin only, like https://apps.example.com", s)
	}
	return u, nil
}

// instance is one app shown for one tool call.
type instance struct {
	id, token, contentToken string
	convID, callKey         string
	server, tool            string
	uri, label              string
	expires                 time.Time

	mu       sync.Mutex
	input    json.RawMessage
	result   json.RawMessage
	res      *resource
	subs     map[chan struct{}]struct{}
	messages []time.Time
}

// Observe looks at one session update of conversation convID. When the
// update starts an app tool call, it makes an instance and returns the
// line to post in the topic. Later updates of the same call add the tool
// input and result to the instance. It returns "" otherwise.
func (a *Apps) Observe(convID string, u acp.SessionUpdate) string {
	c, ok := parseCall(u)
	if !ok {
		return ""
	}
	now := a.cfg.Now()
	a.mu.Lock()
	if c.vis != nil && c.server != "" && c.tool != "" {
		a.vis[convID+"\x00"+c.server+"\x00"+c.tool] = c.vis
	}
	key := convID + "\x00" + c.id
	inst, made := a.byCall[key], false
	if inst == nil && c.uri != "" && c.server != "" {
		a.purgeLocked(now)
		inst = &instance{
			id: rand.Text(), token: rand.Text(), contentToken: rand.Text(),
			convID: convID, callKey: key, server: c.server, tool: c.tool, uri: c.uri,
			label:   label(c),
			expires: now.Add(a.cfg.TTL),
			subs:    map[chan struct{}]struct{}{},
		}
		a.byID[inst.id] = inst
		a.byCall[key] = inst
		made = true
	}
	a.mu.Unlock()
	if inst == nil {
		return ""
	}
	if inst.update(c) || made {
		a.save()
	}
	if !made {
		return ""
	}
	a.cfg.Logf("mcpapps: app %s for %s/%s (%s)", inst.id, inst.server, inst.tool, inst.uri)
	return fmt.Sprintf("🧩 [Open app: %s](%s)", inst.label, a.Link(inst.id))
}

// label is the app's display name: the MCP tool name, else the resource.
// Markdown link syntax is escaped so it cannot break the link.
func label(c call) string {
	s := c.tool
	if s == "" {
		s = c.uri
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\n' || r == '\r':
			b.WriteRune(' ')
		case strings.ContainsRune("[]()`*_\\", r):
			b.WriteRune('\\')
			b.WriteRune(r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// Link is the shell URL of instance id, token included. The token is the
// capability: whoever holds the link can use the app.
func (a *Apps) Link(id string) string {
	a.mu.Lock()
	inst := a.byID[id]
	a.mu.Unlock()
	if inst == nil {
		return ""
	}
	return a.public.String() + "/a/" + inst.id + "?t=" + inst.token
}

// update stores the input and result c carries and wakes the shells
// waiting for a result. It reports whether anything changed.
func (in *instance) update(c call) bool {
	in.mu.Lock()
	defer in.mu.Unlock()
	if c.input != nil {
		in.input = c.input
	}
	if c.result != nil {
		in.result = c.result
		for ch := range in.subs {
			select {
			case ch <- struct{}{}:
			default:
			}
		}
	}
	return c.input != nil || c.result != nil
}

// snapshot returns the current input and result.
func (in *instance) snapshot() (input, result json.RawMessage) {
	in.mu.Lock()
	defer in.mu.Unlock()
	return in.input, in.result
}

// subscribe returns a channel that gets a value each time a result
// arrives, and a function that ends the subscription.
func (in *instance) subscribe() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	in.mu.Lock()
	in.subs[ch] = struct{}{}
	in.mu.Unlock()
	return ch, func() {
		in.mu.Lock()
		delete(in.subs, ch)
		in.mu.Unlock()
	}
}

// allowMessage applies the per-instance ui/message rate limit.
func (in *instance) allowMessage(now time.Time, burst int, window time.Duration) bool {
	in.mu.Lock()
	defer in.mu.Unlock()
	kept := in.messages[:0]
	for _, t := range in.messages {
		if now.Sub(t) < window {
			kept = append(kept, t)
		}
	}
	in.messages = kept
	if len(kept) >= burst {
		return false
	}
	in.messages = append(in.messages, now)
	return true
}

// purgeLocked drops expired instances. a.mu held.
func (a *Apps) purgeLocked(now time.Time) {
	for id, in := range a.byID {
		if now.After(in.expires) {
			delete(a.byID, id)
		}
	}
	for k, in := range a.byCall {
		if now.After(in.expires) {
			delete(a.byCall, k)
		}
	}
}

// Len is the number of instances held, expired ones included until the
// next purge.
func (a *Apps) Len() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.byID)
}

// lookup returns the live instance id if tok is its shell token
// (content=false) or its content token (content=true).
func (a *Apps) lookup(id, tok string, content bool) *instance {
	a.mu.Lock()
	in := a.byID[id]
	a.mu.Unlock()
	if in == nil || a.cfg.Now().After(in.expires) {
		return nil
	}
	want := in.token
	if content {
		want = in.contentToken
	}
	if subtle.ConstantTimeCompare([]byte(tok), []byte(want)) != 1 {
		return nil
	}
	return in
}

// toolAllowed reports whether the app of in may call tool name. The tool
// must be on the app's own server, and it must not be one the relay has
// seen declared without "app" visibility. A tool the relay has not seen
// has the MCP Apps default visibility, which includes "app"; fir still
// refuses any tool that is not in the session's active tool set.
func (a *Apps) toolAllowed(in *instance, name string) bool {
	a.mu.Lock()
	vis, seen := a.vis[in.convID+"\x00"+in.server+"\x00"+name]
	a.mu.Unlock()
	return !seen || appVisible(vis)
}
