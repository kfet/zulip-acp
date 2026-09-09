// Typing notifications are the ONLY liveness signal Zulip offers that
// costs nothing: they generate no message, no unread and — crucially —
// no push notification. A "Thinking…" placeholder message costs one
// push per turn, because Zulip pushes on message CREATION and never on
// an edit, so in quiet mode (`"stream_edits": false`) the placeholder
// is replaced by this.
//
// The signal is EPHEMERAL. The server drops it after
// server_typing_started_expiry_period_milliseconds (15s on a stock
// realm), so a long turn must re-send `start` well inside that window
// and send `stop` on every exit path.
package zulipproto

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// The two typing ops POST /typing accepts.
const (
	TypingStart = "start"
	TypingStop  = "stop"
)

// ServerTypingExpirySetting is the register-response key naming how
// long the server keeps a `start` alive.
const ServerTypingExpirySetting = "server_typing_started_expiry_period_milliseconds"

// DefaultTypingExpiry is Zulip's stock value for that setting, used
// when a server does not report one.
const DefaultTypingExpiry = 15 * time.Second

// SetTyping publishes a typing notification for one conversation.
//
// The target is the conversation the relay is answering in: a channel
// topic (type=channel, stream_id + topic) or a direct message
// (type=direct, `to` = the recipient user ids). Passing user ids
// selects the DM form; they are mutually exclusive server-side.
//
// op is TypingStart or TypingStop. A `start` EXPIRES — see
// TypingStartedExpiry — so a caller holding the indicator up for a
// whole turn must refresh it.
//
// Typing notifications are decoration: every caller here logs and
// continues, and a turn is never failed because one did not land.
func (c *Client) SetTyping(ctx context.Context, op string, streamID int64, topic string, userIDs []int64) error {
	if op != TypingStart && op != TypingStop {
		return fmt.Errorf("zulip: typing op %q is neither %q nor %q", op, TypingStart, TypingStop)
	}
	form := url.Values{"op": {op}}
	switch {
	case len(userIDs) > 0:
		form.Set("type", "direct")
		form.Set("to", mustJSON(userIDs))
	case streamID > 0:
		form.Set("type", "channel")
		form.Set("stream_id", strconv.FormatInt(streamID, 10))
		form.Set("topic", topic)
	default:
		return fmt.Errorf("zulip: typing notification with no target")
	}
	return c.do(ctx, http.MethodPost, "/typing", nil, form, nil)
}

// TypingStartedExpiry reads how long this server keeps a `start`
// alive, so the refresh cadence follows the realm instead of a
// hardcoded 15 seconds.
//
// A server that does not report the setting — it arrived with the
// typing rework, and older builds simply omit the key — yields
// DefaultTypingExpiry and no error: the stock value is the right guess
// and refusing to show liveness over it would be absurd.
func (c *Client) TypingStartedExpiry(ctx context.Context) (time.Duration, error) {
	snap, err := c.realmSnapshot(ctx)
	if err != nil {
		return 0, err
	}
	raw, ok := snap[ServerTypingExpirySetting]
	if !ok {
		return DefaultTypingExpiry, nil
	}
	var ms int64
	if err := json.Unmarshal(raw, &ms); err != nil {
		return 0, fmt.Errorf("zulip: %s is not a number: %w", ServerTypingExpirySetting, err)
	}
	if ms <= 0 {
		return DefaultTypingExpiry, nil
	}
	return time.Duration(ms) * time.Millisecond, nil
}
