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

// MaxTopicLength is Zulip's MAX_TOPIC_LENGTH, in Unicode code points.
// Like a message body, an over-long topic is truncated rather than
// refused, so the client checks it itself.
const MaxTopicLength = 60

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
	// DisplayRecipient is Zulip's POLYMORPHIC recipient field, left
	// raw on purpose: for a channel message it is the channel NAME (a
	// JSON string), and for a direct message it is a JSON ARRAY of
	// user objects. A typed field would fail to decode one of the two
	// shapes and take the whole /events response down with it,
	// silently wedging the queue. Use Recipients.
	DisplayRecipient json.RawMessage `json:"display_recipient"`
	// Client is the posting client's name ("website", "curl", and
	// "Internal" for server-generated messages).
	Client string `json:"client"`
}

// Message type strings. Zulip still calls a direct message "private"
// on the wire, in both message objects and the send form.
const (
	MessageTypeStream  = "stream"
	MessageTypePrivate = "private"
)

// IsDM reports whether m is a direct message (1:1 or group).
func (m Message) IsDM() bool { return m.Type == MessageTypePrivate }

// Recipients returns the user ids of a direct message's participants —
// every recipient plus the sender, which for a DM to the relay always
// includes the bot itself. The order Zulip uses is not contractual, so
// callers must treat it as a set.
//
// It returns nil for a channel message, where display_recipient is the
// channel name rather than a list.
func (m Message) Recipients() []int64 {
	if len(m.DisplayRecipient) == 0 {
		return nil
	}
	var users []struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(m.DisplayRecipient, &users); err != nil {
		return nil
	}
	out := make([]int64, 0, len(users))
	for _, u := range users {
		out = append(out, u.ID)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// RecipientNames returns the full names of a direct message's
// participants, in the order Zulip listed them. It is for human-facing
// prose only — a name is not an identity, and nothing may key on it.
// Returns nil for a channel message, and skips any entry with no name.
func (m Message) RecipientNames() []string {
	if len(m.DisplayRecipient) == 0 {
		return nil
	}
	var users []struct {
		FullName string `json:"full_name"`
	}
	if err := json.Unmarshal(m.DisplayRecipient, &users); err != nil {
		return nil
	}
	out := make([]string, 0, len(users))
	for _, u := range users {
		if u.FullName != "" {
			out = append(out, u.FullName)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
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

// Subscriptions lists the channels the bot is SUBSCRIBED to, which is
// a subset of what Streams reports. It is the boot-time snapshot for
// the "*" (follow-subscriptions) channel set; runtime changes arrive
// as subscription events instead.
func (c *Client) Subscriptions(ctx context.Context) ([]Stream, error) {
	var resp struct {
		Subscriptions []Stream `json:"subscriptions"`
	}
	if err := c.do(ctx, http.MethodGet, "/users/me/subscriptions", nil, nil, &resp); err != nil {
		return nil, err
	}
	return resp.Subscriptions, nil
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
	return c.SendMessageWidget(ctx, streamID, topic, content, "")
}

// SendMessageWidget is SendMessage with an optional widget_content
// payload attached (see zform.go). An empty widget sends nothing extra,
// which is why SendMessage is one line.
//
// The widget is NOT counted against MaxMessageLength: Zulip measures
// that against the message content alone, and widget_content is a
// separate submessage.
func (c *Client) SendMessageWidget(ctx context.Context, streamID int64, topic, content, widget string) (int64, error) {
	if err := checkLength(content); err != nil {
		return 0, err
	}
	form := url.Values{
		"type":    {MessageTypeStream},
		"to":      {strconv.FormatInt(streamID, 10)},
		"topic":   {topic},
		"content": {content},
	}
	setWidget(form, widget)
	var resp struct {
		ID int64 `json:"id"`
	}
	if err := c.do(ctx, http.MethodPost, "/messages", nil, form, &resp); err != nil {
		return 0, err
	}
	return resp.ID, nil
}

// setWidget adds widget_content to a send form when there is one.
//
// A server with widgets disabled, or one older than the parameter,
// reports it in ignored_parameters_unsupported and posts the message
// anyway — so the markdown body must always stand on its own. The
// caller is responsible for retrying without the widget if a server
// ever REJECTS it outright; that is a content decision and does not
// belong in the HTTP layer.
func setWidget(form url.Values, widget string) {
	if widget != "" {
		form.Set("widget_content", widget)
	}
}

// SendDirectMessage posts content to a direct conversation with the
// given user ids and returns the new message id. A group DM is the
// same call with more ids.
//
// `to` is a JSON ARRAY of user ids: the comma-separated form Zulip
// also accepts is deprecated, and the email form is worse still. The
// bot's own id may be included — Zulip ignores it — so the recipient
// set can be passed through from display_recipient unchanged.
//
// Length is checked exactly as for a channel message: MAX_MESSAGE_LENGTH
// applies to DMs too, and truncation there is just as silent.
func (c *Client) SendDirectMessage(ctx context.Context, userIDs []int64, content string) (int64, error) {
	return c.SendDirectMessageWidget(ctx, userIDs, content, "")
}

// SendDirectMessageWidget is SendDirectMessage with an optional
// widget_content payload. See SendMessageWidget.
func (c *Client) SendDirectMessageWidget(ctx context.Context, userIDs []int64, content, widget string) (int64, error) {
	if err := checkLength(content); err != nil {
		return 0, err
	}
	if len(userIDs) == 0 {
		return 0, fmt.Errorf("zulip: direct message with no recipients")
	}
	form := url.Values{
		"type":    {MessageTypePrivate},
		"to":      {mustJSON(userIDs)},
		"content": {content},
	}
	setWidget(form, widget)
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

// MoveMessage moves a message to another topic in the same channel.
//
// Zulip models a topic move as a message edit: PATCH /messages/{id}
// with `topic` and no `content`. propagateMode selects how much of the
// old topic travels with it — "change_one" (this message only),
// "change_later", or "change_all".
//
// Whether the bot may move a message is a REALM POLICY
// (can_move_messages_between_topics_group, itself bounded by
// move_messages_within_stream_limit_seconds) and older servers reject
// some propagate modes outright, so callers must degrade rather than
// depend on it.
func (c *Client) MoveMessage(ctx context.Context, id int64, topic, propagateMode string) error {
	if n := utf8.RuneCountInString(topic); n > MaxTopicLength {
		return fmt.Errorf("zulip: topic is %d code points, over MAX_TOPIC_LENGTH %d — Zulip would truncate it silently", n, MaxTopicLength)
	}
	form := url.Values{"topic": {topic}, "propagate_mode": {propagateMode}}
	return c.do(ctx, http.MethodPatch, "/messages/"+strconv.FormatInt(id, 10), nil, form, nil)
}

// DeleteMessage removes a message the bot posted.
//
// It exists for exactly one caller: retiring a superseded `!opts`
// panel. A panel carrying a widget CANNOT be edited (see zform.go), so
// deleting it is the only way to keep one live panel per conversation.
// Nothing else in the relay deletes anything — agent output and human
// messages are never touched.
//
// Whether the bot may delete its own message is a REALM POLICY
// (delete_own_message_policy) and is additionally time-limited by
// message_content_delete_limit_seconds, so this call is expected to
// fail on some servers. Callers must degrade rather than depend on it.
func (c *Client) DeleteMessage(ctx context.Context, id int64) error {
	return c.do(ctx, http.MethodDelete, "/messages/"+strconv.FormatInt(id, 10), nil, nil, nil)
}

// RejectedByServer reports whether err is Zulip REFUSING a request —
// an API error with a 4xx status — as opposed to a transport failure, a
// cancelled context or a server fault.
//
// It is the difference between "this server will not accept what I
// sent" (try a simpler request) and "I could not reach the server"
// (retrying the same way is pointless).
func RejectedByServer(err error) bool {
	var ae *APIError
	if !asAPIError(err, &ae) {
		return false
	}
	return ae.Status >= 400 && ae.Status < 500
}

// IsMissing reports whether err is Zulip saying the message is not
// there — deleted by a human, or moved out of reach. It is the one 4xx
// that means "already in the state you wanted", so a caller retiring
// something has nothing left to do.
func IsMissing(err error) bool {
	var ae *APIError
	if !asAPIError(err, &ae) {
		return false
	}
	return ae.Status == http.StatusNotFound
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

// Emoji reaction error codes. Zulip reports both of these as failures
// with HTTP 400, but for a relay that uses a reaction as a transient
// ack they mean "already in the state you asked for" — the only sane
// reading is success.
const (
	reactionAlreadyExists = "REACTION_ALREADY_EXISTS"
	reactionDoesNotExist  = "REACTION_DOES_NOT_EXIST"
)

// AddReaction adds an emoji reaction to a message.
//
// The relay uses this as its only immediate acknowledgement: Zulip has
// no typing indicator, and a reaction is retractable, so it can say "I
// have your message" without leaving anything behind in the topic.
//
// Adding a reaction that is already there is treated as success.
func (c *Client) AddReaction(ctx context.Context, messageID int64, emoji string) error {
	return c.reaction(ctx, http.MethodPost, messageID, emoji, reactionAlreadyExists)
}

// RemoveReaction removes an emoji reaction previously added by this
// bot. Removing one that is not there is treated as success.
func (c *Client) RemoveReaction(ctx context.Context, messageID int64, emoji string) error {
	return c.reaction(ctx, http.MethodDelete, messageID, emoji, reactionDoesNotExist)
}

func (c *Client) reaction(ctx context.Context, method string, messageID int64, emoji, benign string) error {
	form := url.Values{"emoji_name": {emoji}}
	path := "/messages/" + strconv.FormatInt(messageID, 10) + "/reactions"
	err := c.do(ctx, method, path, nil, form, nil)
	var ae *APIError
	if asAPIError(err, &ae) && ae.Code == benign {
		return nil
	}
	return err
}

// NarrowTerm is one term of a GET /messages narrow.
//
// This is NOT the same shape as the /register narrow, which takes
// [operator, operand] STRING pairs and rejects a numeric channel id
// (see docs/zulip-protocol-reference.md). Here the operand is typed:
// a channel operand is the numeric id, a dm operand is a list of user
// ids. Keeping the two shapes in separate types is what stops one
// being passed where the other is meant.
type NarrowTerm struct {
	Operator string `json:"operator"`
	Operand  any    `json:"operand"`
}

// TopicNarrow narrows to one topic in one channel.
func TopicNarrow(streamID int64, topic string) []NarrowTerm {
	return []NarrowTerm{
		{Operator: "channel", Operand: streamID},
		{Operator: "topic", Operand: topic},
	}
}

// DMNarrow narrows to the direct-message conversation between exactly
// the given users.
//
// The operand is the full participant set INCLUDING the bot itself,
// which is how journal.Key stores it; Zulip normalises the sender out
// of the operand server-side, so passing it is correct and passing a
// set that omits a participant would select a DIFFERENT conversation.
func DMNarrow(userIDs []int64) []NarrowTerm {
	ids := make([]int64, len(userIDs))
	copy(ids, userIDs)
	return []NarrowTerm{{Operator: "dm", Operand: ids}}
}

// Messages returns up to limit messages matching narrow, oldest first,
// as raw markdown.
//
// beforeID pages backwards: 0 anchors at the newest message, and a
// non-zero value anchors at that message id EXCLUSIVE, so feeding back
// the oldest id of one page yields the page before it with no overlap
// and no gap.
//
// include_anchor is sent ONLY when paging. With anchor=newest the
// anchor is a synthetic id above every real message, and asking the
// server to exclude it is at best a no-op and at worst drops the newest
// message — which is the one message a history read must never lose.
func (c *Client) Messages(ctx context.Context, narrow []NarrowTerm, limit int, beforeID int64) ([]Message, error) {
	q := url.Values{
		"anchor":         {"newest"},
		"num_before":     {strconv.Itoa(limit)},
		"num_after":      {"0"},
		"narrow":         {mustJSON(narrow)},
		"apply_markdown": {"false"},
	}
	if beforeID > 0 {
		q.Set("anchor", strconv.FormatInt(beforeID, 10))
		q.Set("include_anchor", "false")
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

	// Op discriminates subscription and stream events:
	// "add"/"remove"/"peer_add"/… for subscription,
	// "create"/"delete"/"update" for stream.
	Op string `json:"op"`
	// Subscriptions carries the channels added to, or removed from,
	// the BOT's own subscriptions (subscription op add/remove).
	Subscriptions []Stream `json:"subscriptions"`
	// Streams carries the channels of a stream create/delete event.
	Streams []Stream `json:"streams"`
	// Property and Value describe a stream op=update. Value is left
	// raw on purpose: Zulip sends a string, a bool or a number
	// depending on the property, and a typed field would fail to
	// decode the WHOLE /events response — silently wedging the queue —
	// the first time a property the relay does not care about changed.
	Property string          `json:"property"`
	Value    json.RawMessage `json:"value"`
}

// RenamedTo returns the new channel name of a stream op=update
// rename event, and false for any other event.
func (e Event) RenamedTo() (string, bool) {
	if e.Type != EventStream || e.Op != "update" || e.Property != "name" {
		return "", false
	}
	var name string
	if err := json.Unmarshal(e.Value, &name); err != nil || name == "" {
		return "", false
	}
	return name, true
}

// Event type strings.
const (
	EventMessage       = "message"
	EventUpdateMessage = "update_message"
	EventSubscription  = "subscription"
	EventStream        = "stream"
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
// Three traps live here, all measured on Zulip 12.2, and all of which
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
//  3. For the same reason a channel narrow EXCLUDES DIRECT MESSAGES
//     outright: a DM is in no channel, so it can never satisfy the
//     term. A relay that serves DMs must therefore drop the narrow —
//     hence serveDMs.
//
// So: exactly one channel and no DMs gets a narrow; anything else gets
// none, and the caller filters itself. Over-delivery is cheap and the
// relay already has an allowlist it must enforce regardless —
// under-delivery is silent and unrecoverable.
func NarrowChannels(names []string, serveDMs bool) [][2]string {
	if serveDMs || len(names) != 1 {
		return nil
	}
	return [][2]string{{"channel", names[0]}}
}
