package live

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
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
