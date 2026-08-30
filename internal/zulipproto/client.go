// Package zulipproto is a minimal Zulip REST client: HTTP Basic auth,
// message send/edit/read, one-shot file uploads, channel resolution,
// and the event-queue primitives the long-poll runner is built on.
//
// Everything here is Zulip-specific by design and must never be
// promoted to acp-kit.
//
// # Facts this package encodes, all measured against Zulip 12.2
//
//   - Auth is HTTP Basic `email:api_key`. There is no signing secret,
//     no request-timestamp verification and no OAuth refresh.
//   - MAX_MESSAGE_LENGTH is 10000 code points and Zulip TRUNCATES
//     SILENTLY: a longer POST still returns {"result":"success"} and
//     stores 10000 characters with "\n[message truncated]" appended.
//     This package therefore refuses to send an oversized message
//     rather than let the surface eat the tail. Splitting is
//     internal/rollover's job.
//   - GET /api/v1/events does NOT accept a `timeout` parameter — the
//     server echoes it back in `ignored_parameters_unsupported`. The
//     long poll is bounded by the CLIENT's HTTP timeout only.
//   - Uploads are a single multipart round-trip returning a relative
//     URL to interpolate into message markdown. No extension allowlist,
//     no MIME sniffing.
//   - LENGTH IS NOT THE ONLY LIMIT. A body that is legal by length can
//     still be refused with HTTP 400 "Unable to render message" if the
//     server-side markdown/Pygments pass chokes on it. Measured: ~1000
//     consecutive emoji is enough, while 9000 CJK characters render
//     fine. This failure is LOUD, which makes it far kinder than the
//     silent truncation above — the relay surfaces it and falls back
//     to attaching the text as a file rather than losing it.
package zulipproto

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// MaxMessageLength is Zulip's MAX_MESSAGE_LENGTH, in Unicode code
// points. Exceeding it is not an error server-side — it is silent
// truncation — so the client checks it itself.
const MaxMessageLength = 10000

// DefaultLongPollTimeout bounds a single GET /events. The server holds
// the request open until an event arrives or roughly 90s elapse, so the
// client budget must be comfortably above that.
const DefaultLongPollTimeout = 110 * time.Second

// Config configures a Client.
type Config struct {
	// Site is the Zulip base URL, e.g. https://zulip.example.com. A
	// trailing slash and an explicit /api/v1 suffix are both tolerated.
	Site string
	// Email is the bot's Zulip email (HTTP Basic username).
	Email string
	// APIKey is the bot's API key (HTTP Basic password).
	APIKey string
	// HTTPClient overrides the default client. Its Timeout must be
	// generous enough for the /events long poll; use nil to get a
	// client configured for it.
	HTTPClient *http.Client
}

// Client talks to one Zulip realm as one bot.
type Client struct {
	base   string // ".../api/v1"
	email  string
	apiKey string
	hc     *http.Client
}

// APIError is a structured Zulip error response.
type APIError struct {
	// Status is the HTTP status code.
	Status int
	// Msg is Zulip's human-readable message.
	Msg string
	// Code is Zulip's machine-readable error code, e.g.
	// "BAD_EVENT_QUEUE_ID". Empty for errors Zulip did not classify.
	Code string
}

func (e *APIError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("zulip: %s (%s, HTTP %d)", e.Msg, e.Code, e.Status)
	}
	return fmt.Sprintf("zulip: %s (HTTP %d)", e.Msg, e.Status)
}

// IsBadEventQueue reports whether err is Zulip telling us our event
// queue is gone. This is ROUTINE — queues die whenever the server
// restarts or garbage-collects an idle queue — and the correct
// response is to register a fresh one, not to treat it as a failure.
func IsBadEventQueue(err error) bool {
	var ae *APIError
	if ok := asAPIError(err, &ae); !ok {
		return false
	}
	return ae.Code == "BAD_EVENT_QUEUE_ID"
}

// asAPIError is a tiny errors.As shim kept local so the exported
// surface does not depend on the errors package shape.
func asAPIError(err error, target **APIError) bool {
	for err != nil {
		if ae, ok := err.(*APIError); ok {
			*target = ae
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// New constructs a Client.
func New(cfg Config) (*Client, error) {
	site := strings.TrimSpace(cfg.Site)
	if site == "" {
		return nil, fmt.Errorf("zulip: empty site")
	}
	if cfg.Email == "" {
		return nil, fmt.Errorf("zulip: empty email")
	}
	if cfg.APIKey == "" {
		return nil, fmt.Errorf("zulip: empty api key")
	}
	u, err := url.Parse(site)
	if err != nil {
		return nil, fmt.Errorf("zulip: bad site %q: %w", site, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("zulip: site must be http(s), got %q", site)
	}
	base := strings.TrimSuffix(u.String(), "/")
	if !strings.HasSuffix(base, "/api/v1") {
		base += "/api/v1"
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: DefaultLongPollTimeout}
	}
	return &Client{base: base, email: cfg.Email, apiKey: cfg.APIKey, hc: hc}, nil
}

// User is the subset of a Zulip user record the relay needs.
type User struct {
	UserID   int64  `json:"user_id"`
	Email    string `json:"email"`
	FullName string `json:"full_name"`
	IsBot    bool   `json:"is_bot"`
}

// Message is the subset of a Zulip message the relay needs. Content is
// raw markdown when fetched with apply_markdown=false, which is the
// only way this package ever fetches.
type Message struct {
	ID          int64  `json:"id"`
	SenderID    int64  `json:"sender_id"`
	SenderEmail string `json:"sender_email"`
	SenderName  string `json:"sender_full_name"`
	Content     string `json:"content"`
	StreamID    int64  `json:"stream_id"`
	Topic       string `json:"subject"`
	Type        string `json:"type"`
	Timestamp   int64  `json:"timestamp"`
	// SenderRealm is the sender's realm string. Zulip's own system
	// bots — Notification Bot, Welcome Bot, the email gateway — are
	// cross-realm and always report SystemBotRealm. They do NOT appear
	// in GET /users, so this field is the only reliable way to
	// recognise them from a message event.
	SenderRealm string `json:"sender_realm_str"`
	// Client is the posting client's name ("website", "curl", and
	// "Internal" for server-generated messages).
	Client string `json:"client"`
}

// SystemBotRealm is the realm Zulip's cross-realm system bots live in.
// A topic move, for instance, posts a "This topic was moved here from
// …" notice as Notification Bot, which a relay must not treat as a
// human turn.
const SystemBotRealm = "zulipinternal"

// Stream is a Zulip channel.
type Stream struct {
	StreamID int64  `json:"stream_id"`
	Name     string `json:"name"`
}

// Me returns the authenticated bot's own user record. Used at startup
// both as a credentials check and to learn the sender id whose
// messages must never be fed back into the agent.
func (c *Client) Me(ctx context.Context) (User, error) {
	var resp struct {
		User
	}
	if err := c.do(ctx, http.MethodGet, "/users/me", nil, nil, &resp); err != nil {
		return User{}, err
	}
	return resp.User, nil
}

// Users lists the realm's users. Used at startup to learn which
// senders are bots, so the relay never answers one.
//
// NOTE: cross-realm system bots (Notification Bot and friends) are NOT
// in this list — match Message.SenderRealm against SystemBotRealm for
// those.
func (c *Client) Users(ctx context.Context) ([]User, error) {
	var resp struct {
		Members []User `json:"members"`
	}
	if err := c.do(ctx, http.MethodGet, "/users", nil, nil, &resp); err != nil {
		return nil, err
	}
	return resp.Members, nil
}

// Streams lists the channels the bot can see.
func (c *Client) Streams(ctx context.Context) ([]Stream, error) {
	var resp struct {
		Streams []Stream `json:"streams"`
	}
	if err := c.do(ctx, http.MethodGet, "/streams", nil, nil, &resp); err != nil {
		return nil, err
	}
	return resp.Streams, nil
}

// SendMessage posts content to (streamID, topic) and returns the new
// message id.
//
// It refuses content over MaxMessageLength rather than letting Zulip
// silently truncate it. A caller that hits this has a bug: splitting is
// internal/rollover's job and must happen before the wire.
func (c *Client) SendMessage(ctx context.Context, streamID int64, topic, content string) (int64, error) {
	if err := checkLength(content); err != nil {
		return 0, err
	}
	form := url.Values{
		"type":    {"stream"},
		"to":      {strconv.FormatInt(streamID, 10)},
		"topic":   {topic},
		"content": {content},
	}
	var resp struct {
		ID int64 `json:"id"`
	}
	if err := c.do(ctx, http.MethodPost, "/messages", nil, form, &resp); err != nil {
		return 0, err
	}
	return resp.ID, nil
}

// EditMessage replaces a message's content. Zulip re-renders the whole
// body server-side on every edit, which is why the relay coalesces.
//
// A fresh Zulip realm caps edits at message_content_edit_limit_seconds
// (default 600); a streaming relay must run against a realm where that
// is unlimited or long turns start failing with HTTP 400 mid-stream.
func (c *Client) EditMessage(ctx context.Context, id int64, content string) error {
	if err := checkLength(content); err != nil {
		return err
	}
	form := url.Values{"content": {content}}
	return c.do(ctx, http.MethodPatch, "/messages/"+strconv.FormatInt(id, 10), nil, form, nil)
}

// GetMessage fetches one message as raw markdown.
func (c *Client) GetMessage(ctx context.Context, id int64) (Message, error) {
	q := url.Values{"apply_markdown": {"false"}}
	var resp struct {
		Message Message `json:"message"`
	}
	if err := c.do(ctx, http.MethodGet, "/messages/"+strconv.FormatInt(id, 10), q, nil, &resp); err != nil {
		return Message{}, err
	}
	return resp.Message, nil
}

// TopicMessages returns up to limit of the most recent messages in
// (streamID, topic), oldest first, as raw markdown. Used to reconstruct
// a turn and to find the tail message the relay owned before a crash.
func (c *Client) TopicMessages(ctx context.Context, streamID int64, topic string, limit int) ([]Message, error) {
	narrow := mustJSON([]any{
		map[string]any{"operator": "channel", "operand": streamID},
		map[string]any{"operator": "topic", "operand": topic},
	})
	q := url.Values{
		"anchor":         {"newest"},
		"num_before":     {strconv.Itoa(limit)},
		"num_after":      {"0"},
		"narrow":         {narrow},
		"apply_markdown": {"false"},
	}
	var resp struct {
		Messages []Message `json:"messages"`
	}
	if err := c.do(ctx, http.MethodGet, "/messages", q, nil, &resp); err != nil {
		return nil, err
	}
	return resp.Messages, nil
}

// Upload uploads a file in a single multipart round-trip and returns
// the relative URL to interpolate into message markdown.
func (c *Client) Upload(ctx context.Context, filename string, r io.Reader) (string, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part := mustFormFile(mw, filename)
	if _, err := io.Copy(part, r); err != nil {
		return "", fmt.Errorf("zulip: read upload: %w", err)
	}
	mustCloseWriter(mw)
	req, err := c.newRequest(ctx, http.MethodPost, "/user_uploads", nil, &buf)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	var resp struct {
		URL string `json:"url"`
	}
	if err := c.send(req, &resp); err != nil {
		return "", err
	}
	return resp.URL, nil
}

// RegisterResult is the outcome of POST /register.
type RegisterResult struct {
	QueueID      string `json:"queue_id"`
	LastEventID  int64  `json:"last_event_id"`
	MaxMessageID int64  `json:"max_message_id"`
}

// Register creates an event queue. eventTypes and narrow are encoded as
// Zulip expects (JSON arrays in form fields). narrow entries are
// [operator, operand] pairs, e.g. {"channel", "4"}.
//
// The returned queue_id and last_event_id are IN-MEMORY state. Queues
// die on server restart, so persisting them is false comfort.
func (c *Client) Register(ctx context.Context, eventTypes []string, narrow [][2]string) (RegisterResult, error) {
	form := url.Values{
		// Raw markdown in, raw markdown out: the relay never wants
		// Zulip's rendered HTML.
		"apply_markdown": {"false"},
	}
	if len(eventTypes) > 0 {
		form.Set("event_types", mustJSON(eventTypes))
	}
	if len(narrow) > 0 {
		pairs := make([][]string, 0, len(narrow))
		for _, n := range narrow {
			pairs = append(pairs, []string{n[0], n[1]})
		}
		form.Set("narrow", mustJSON(pairs))
	}
	var resp RegisterResult
	if err := c.do(ctx, http.MethodPost, "/register", nil, form, &resp); err != nil {
		return RegisterResult{}, err
	}
	return resp, nil
}

// Event is one entry from GET /events.
//
// Zulip's event ids are per-queue and monotonic. update_message events
// carry a topic rename in Topic (new) plus OrigTopic (old) — that pair
// is what keeps a renamed topic from orphaning its session.
type Event struct {
	ID        int64    `json:"id"`
	Type      string   `json:"type"`
	Message   *Message `json:"message"`
	MessageID int64    `json:"message_id"`
	StreamID  int64    `json:"stream_id"`
	// Topic is the NEW topic on an update_message rename.
	Topic string `json:"subject"`
	// OrigTopic is the topic the message was in before the rename.
	OrigTopic string `json:"orig_subject"`
}

// Event type strings.
const (
	EventMessage       = "message"
	EventUpdateMessage = "update_message"
	EventHeartbeat     = "heartbeat"
)

// GetEvents long-polls the queue for events newer than lastEventID.
//
// There is no server-side timeout parameter: Zulip 12.2 reports
// `timeout` in ignored_parameters_unsupported. The poll is bounded by
// ctx and by the HTTP client's own timeout, and the server returns
// heartbeat events so an idle queue still produces traffic.
func (c *Client) GetEvents(ctx context.Context, queueID string, lastEventID int64) ([]Event, error) {
	q := url.Values{
		"queue_id":      {queueID},
		"last_event_id": {strconv.FormatInt(lastEventID, 10)},
	}
	var resp struct {
		Events []Event `json:"events"`
	}
	if err := c.do(ctx, http.MethodGet, "/events", q, nil, &resp); err != nil {
		return nil, err
	}
	return resp.Events, nil
}

// DeleteQueue tears down an event queue. Best-effort on shutdown: a
// leaked queue is garbage-collected by the server anyway.
func (c *Client) DeleteQueue(ctx context.Context, queueID string) error {
	return c.do(ctx, http.MethodDelete, "/events", nil, url.Values{"queue_id": {queueID}}, nil)
}

// checkLength enforces MAX_MESSAGE_LENGTH in CODE POINTS. Zulip counts
// Python len(str): not bytes, not UTF-16 units.
func checkLength(content string) error {
	if n := utf8.RuneCountInString(content); n > MaxMessageLength {
		return fmt.Errorf("zulip: message is %d code points, over MAX_MESSAGE_LENGTH %d — Zulip would truncate it silently", n, MaxMessageLength)
	}
	return nil
}

func (c *Client) newRequest(ctx context.Context, method, path string, query url.Values, body io.Reader) (*http.Request, error) {
	u := c.base + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, fmt.Errorf("zulip: build request: %w", err)
	}
	req.SetBasicAuth(c.email, c.apiKey)
	return req, nil
}

// do issues a request and decodes the JSON envelope into out (which
// may be nil when the caller only cares about success).
func (c *Client) do(ctx context.Context, method, path string, query url.Values, form url.Values, out any) error {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := c.newRequest(ctx, method, path, query, body)
	if err != nil {
		return err
	}
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	return c.send(req, out)
}

func (c *Client) send(req *http.Request, out any) error {
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("zulip: %s %s: %w", req.Method, req.URL.Path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("zulip: read %s %s: %w", req.Method, req.URL.Path, err)
	}
	// Zulip signals failure in the JSON envelope as well as the status
	// code, and the two do not always agree; trust the envelope.
	var env struct {
		Result string `json:"result"`
		Msg    string `json:"msg"`
		Code   string `json:"code"`
	}
	if err := json.Unmarshal(b, &env); err != nil {
		return fmt.Errorf("zulip: %s %s: bad JSON (HTTP %d): %w", req.Method, req.URL.Path, resp.StatusCode, err)
	}
	if env.Result != "success" {
		msg := env.Msg
		if msg == "" {
			msg = http.StatusText(resp.StatusCode)
		}
		return &APIError{Status: resp.StatusCode, Msg: msg, Code: env.Code}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("zulip: %s %s: decode: %w", req.Method, req.URL.Path, err)
	}
	return nil
}

// NarrowChannels builds the /register narrow for the given channels.
//
// Two traps live here, both measured on Zulip 12.2, and both of which
// fail by SILENTLY DELIVERING NOTHING — the queue registers fine and
// simply never produces an event.
//
//  1. The operand must be the channel NAME, not its id. `{"channel",
//     "4"}` is accepted and then matches a channel literally called
//     "4".
//  2. Narrow terms are a CONJUNCTION. Two channel terms mean "in
//     channel A *and* in channel B", which no message ever satisfies.
//     There is no way to express a channel union in a /register
//     narrow.
//
// So: exactly one channel gets a narrow; anything else gets none, and
// the caller filters by channel itself. Over-delivery is cheap and the
// relay already has a channel allowlist it must enforce regardless —
// under-delivery is silent and unrecoverable.
func NarrowChannels(names []string) [][2]string {
	if len(names) != 1 {
		return nil
	}
	return [][2]string{{"channel", names[0]}}
}
