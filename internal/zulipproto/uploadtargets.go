package zulipproto

// The zlite upload-target extension: a server-minted, credential-free
// PUT URL for one file. Stock Zulip has no such endpoint (its uploads,
// web and API, need a session or the API key), so the relay probes for
// it at startup.
//
// Contract (zlite, NOT yet verified against a running zlite):
//
//	POST /api/v1/zlite/upload_targets  name, size, mime, ttl (seconds)
//	  → {url, expiresAt, token}        url takes one PUT with no auth
//	GET  /api/v1/zlite/upload_targets/<token>
//	  → {status, path, size}           path is the /user_uploads/… path

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const uploadTargetsPath = "/zlite/upload_targets"

// UploadTarget is a server-minted upload URL.
type UploadTarget struct {
	URL       string
	Token     string
	ExpiresAt time.Time
}

// UploadTargetStatus is what the server holds for a target.
type UploadTargetStatus struct {
	Status string
	Path   string
	Size   int64
}

// HasUploadTargets reports whether the server offers upload targets.
// It sends an empty POST: a server without the endpoint answers 404 (or
// 405), one with it refuses the empty request with another status.
func (c *Client) HasUploadTargets(ctx context.Context) (bool, error) {
	req, err := c.newRequest(ctx, http.MethodPost, uploadTargetsPath, nil, strings.NewReader(""))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.hc.Do(req)
	if err != nil {
		return false, fmt.Errorf("zulip: probe upload targets: %w", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNotFound, http.StatusMethodNotAllowed:
		return false, nil
	case http.StatusUnauthorized, http.StatusForbidden:
		return false, fmt.Errorf("zulip: probe upload targets: HTTP %d", resp.StatusCode)
	}
	if resp.StatusCode >= 500 {
		return false, fmt.Errorf("zulip: probe upload targets: HTTP %d", resp.StatusCode)
	}
	return true, nil
}

// CreateUploadTarget asks the server for a single-use PUT URL for one
// file of size bytes, valid for ttl.
func (c *Client) CreateUploadTarget(ctx context.Context, name string, size int64, mimeType string, ttl time.Duration) (UploadTarget, error) {
	form := url.Values{
		"name": {name},
		"size": {strconv.FormatInt(size, 10)},
		"mime": {mimeType},
		"ttl":  {strconv.Itoa(int(ttl / time.Second))},
	}
	var resp struct {
		URL       string          `json:"url"`
		Token     string          `json:"token"`
		ExpiresAt json.RawMessage `json:"expiresAt"`
	}
	if err := c.do(ctx, http.MethodPost, uploadTargetsPath, nil, form, &resp); err != nil {
		return UploadTarget{}, err
	}
	if resp.URL == "" || resp.Token == "" {
		return UploadTarget{}, fmt.Errorf("zulip: upload target without url or token")
	}
	return UploadTarget{URL: c.realmURL(resp.URL), Token: resp.Token, ExpiresAt: parseTime(resp.ExpiresAt)}, nil
}

// GetUploadTarget reports what the server stored for a target.
func (c *Client) GetUploadTarget(ctx context.Context, token string) (UploadTargetStatus, error) {
	var resp struct {
		Status string `json:"status"`
		Path   string `json:"path"`
		Size   int64  `json:"size"`
	}
	if err := c.do(ctx, http.MethodGet, uploadTargetsPath+"/"+url.PathEscape(token), nil, nil, &resp); err != nil {
		return UploadTargetStatus{}, err
	}
	return UploadTargetStatus{Status: resp.Status, Path: resp.Path, Size: resp.Size}, nil
}

// parseTime reads a time given as RFC 3339 text or as Unix seconds. An
// unreadable value is the zero time, which means "no expiry given".
func parseTime(raw json.RawMessage) time.Time {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		t, _ := time.Parse(time.RFC3339, s)
		return t
	}
	var n float64
	if json.Unmarshal(raw, &n) == nil && n > 0 {
		return time.Unix(int64(n), 0)
	}
	return time.Time{}
}
