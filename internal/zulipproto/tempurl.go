package zulipproto

import (
	"context"
	"errors"
	"fmt"
	"net/http"
)

// TemporaryUpload asks Zulip for a temporary signed URL to one
// `/user_uploads/…` attachment and measures it.
//
// The URL comes from the documented "get public temporary URL"
// endpoint (GET <api base>/user_uploads/<rest>, which answers
// {"result":"success","url":"/user_uploads/temporary/<token>"}). Its
// token is the credential, valid for about 60 seconds, so the URL can
// go to a third party without the bot's credentials. The size comes from
// a HEAD on that URL, sent WITHOUT credentials, because the reader of
// the URL has none either.
//
// The URL is absolute: a realm-relative one is hung off the realm root.
func (c *Client) TemporaryUpload(ctx context.Context, uploadPath string) (url string, size int64, contentType string, err error) {
	rest, ok := UploadRest(uploadPath)
	if !ok {
		return "", 0, "", fmt.Errorf("zulip: %q is not a %s path", uploadPath, UploadPrefix)
	}
	env, ct, err := c.fetchUpload(ctx, c.base+UploadPrefix+rest, true, uploadPath, 64<<10)
	if err != nil {
		return "", 0, "", err
	}
	tmp, ok := temporaryUploadURL(env, ct)
	if !ok {
		return "", 0, "", fmt.Errorf("zulip: %s gave no temporary URL", uploadPath)
	}
	url = c.realmURL(tmp)
	size, contentType, err = c.measure(ctx, url, uploadPath)
	if err != nil {
		return "", 0, "", err
	}
	return url, size, contentType, nil
}

// measure finds the size of a file at an unauthenticated URL: a HEAD,
// and when that gives no size, a GET of the first byte, whose
// Content-Range holds the total. The body is never read.
func (c *Client) measure(ctx context.Context, url, what string) (int64, string, error) {
	resp, err := c.probe(ctx, http.MethodHead, url, what)
	if err != nil {
		return 0, "", err
	}
	if resp.StatusCode == http.StatusOK && resp.ContentLength >= 0 {
		return resp.ContentLength, resp.Header.Get("Content-Type"), nil
	}
	resp, err = c.probe(ctx, http.MethodGet, url, what)
	if err != nil {
		return 0, "", err
	}
	ct := resp.Header.Get("Content-Type")
	switch resp.StatusCode {
	case http.StatusPartialContent:
		var a, b, total int64
		if _, err := fmt.Sscanf(resp.Header.Get("Content-Range"), "bytes %d-%d/%d", &a, &b, &total); err == nil && total >= 0 {
			return total, ct, nil
		}
	case http.StatusOK:
		if resp.ContentLength >= 0 {
			return resp.ContentLength, ct, nil
		}
	default:
		return 0, "", &APIError{Status: resp.StatusCode, Msg: http.StatusText(resp.StatusCode)}
	}
	return 0, "", errors.New("zulip: the temporary URL gave no size")
}

// probe sends one request without credentials, a GET asking for the
// first byte only, and closes the body unread.
func (c *Client) probe(ctx context.Context, method, url, what string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, nil)
	if err != nil {
		return nil, fmt.Errorf("zulip: build request: %w", err)
	}
	if method == http.MethodGet {
		req.Header.Set("Range", "bytes=0-0")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("zulip: %s %s: %w", method, what, err)
	}
	_ = resp.Body.Close()
	return resp, nil
}
