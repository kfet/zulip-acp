package live

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/kfet/zulip-acp/internal/zulipproto"
)

// rawSend posts a message straight over HTTP, bypassing the client's
// own MAX_MESSAGE_LENGTH guard. Used only by the truncation test,
// which is about what the SERVER does with an oversized body — the
// guard is exactly what stops production code from finding out.
func rawSend(ctx context.Context, _ *zulipproto.Client, streamID int64, topic, content string) (int64, error) {
	form := url.Values{
		"type":    {"stream"},
		"to":      {strconv.FormatInt(streamID, 10)},
		"topic":   {topic},
		"content": {content},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimSuffix(os.Getenv("ZULIP_SITE"), "/")+"/api/v1/messages",
		strings.NewReader(form.Encode()))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(os.Getenv("ZULIP_EMAIL"), os.Getenv("ZULIP_API_KEY"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	var env struct {
		Result string `json:"result"`
		Msg    string `json:"msg"`
		ID     int64  `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return 0, err
	}
	if env.Result != "success" {
		return 0, fmt.Errorf("zulip: %s", env.Msg)
	}
	return env.ID, nil
}

// fetch downloads a /user_uploads URL with the bot's credentials.
func fetch(ctx context.Context, _ *zulipproto.Client, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimSuffix(os.Getenv("ZULIP_SITE"), "/")+path, nil)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(os.Getenv("ZULIP_EMAIL"), os.Getenv("ZULIP_API_KEY"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download %s: HTTP %d", path, resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

// asUser performs a form POST against the live server as an arbitrary
// account, returning the decoded envelope. It is how a test reaches an
// endpoint the relay's own client has no method for — the relay never
// CASTS a vote, it only receives them, so zulipproto has no submessage
// writer and must not grow one.
func asUser(ctx context.Context, email, key, method, path string, form url.Values) (map[string]any, error) {
	var body io.Reader
	target := strings.TrimSuffix(os.Getenv("ZULIP_SITE"), "/") + path
	if method == http.MethodGet {
		target += "?" + form.Encode()
	} else {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(email, key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	var env map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return nil, err
	}
	if env["result"] != "success" {
		return env, fmt.Errorf("zulip: %v (HTTP %d)", env["msg"], resp.StatusCode)
	}
	return env, nil
}

// pollWidgetOf reads back the poll widget Zulip built for a message,
// returning its question and its option labels. Fails if the message
// carries no poll submessage.
func pollWidgetOf(ctx context.Context, c *zulipproto.Client, id int64) (question string, options []string, extraKeys []string, err error) {
	m, err := c.GetMessage(ctx, id)
	if err != nil {
		return "", nil, nil, err
	}
	for _, sm := range m.Submessages {
		if sm.MsgType != zulipproto.WidgetMsgType {
			continue
		}
		var w struct {
			WidgetType string         `json:"widget_type"`
			Extra      map[string]any `json:"extra_data"`
		}
		if err := json.Unmarshal([]byte(sm.Content), &w); err != nil {
			return "", nil, nil, err
		}
		if w.WidgetType != zulipproto.WidgetTypePoll {
			continue
		}
		for k := range w.Extra {
			extraKeys = append(extraKeys, k)
		}
		sort.Strings(extraKeys)
		question, _ = w.Extra["question"].(string)
		raw, _ := w.Extra["options"].([]any)
		for _, o := range raw {
			s, _ := o.(string)
			options = append(options, s)
		}
		return question, options, extraKeys, nil
	}
	return "", nil, nil, fmt.Errorf("message %d carries no poll submessage", id)
}
