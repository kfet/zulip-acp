package zulipproto

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strings"
)

// UploadPrefix is the path every Zulip attachment link starts with.
// The composer writes a relative `/user_uploads/…` link; "Copy link"
// produces the same path under the realm's origin.
const UploadPrefix = "/user_uploads/"

// ErrUploadTooLarge is returned by DownloadUpload when the file is
// bigger than the caller's cap. It is a value, not a fault: the caller
// is expected to skip the file and say so, never to fail the turn.
var ErrUploadTooLarge = errors.New("zulip: upload exceeds the size cap")

// DownloadUpload fetches one `/user_uploads/…` attachment with the
// bot's credentials and returns its bytes and the Content-Type the
// server served it with.
//
// max caps the response in bytes; a file over it returns
// ErrUploadTooLarge and NO bytes, having read at most max+1 of them —
// the cap must bound memory, not merely report on it afterwards. A
// non-positive max means unbounded, which no caller in this relay
// uses.
//
// Three facts this encodes, all measured against Zulip 12.2:
//
//   - The authenticated endpoint is `/api/v1/user_uploads/<rest>`, so
//     the path is appended to the API base like any other call. The
//     realm-root spelling of the same path is a browser/session
//     surface and is NOT part of the API.
//   - The response is usually RAW BYTES, not a JSON envelope, so this
//     cannot go through send: there is normally no {"result":
//     "success"} to inspect and the status code is the only signal.
//     But NOT always: on some deployments the same endpoint answers
//     with Zulip's temporary-URL envelope
//     ({"result":"success","url":"/user_uploads/temporary/…"})
//     instead of the file, and the bytes are only at that second URL.
//     Writing the envelope to disk as if it were the file is the bug
//     this indirection exists to avoid, so it is followed once,
//     unauthenticated (the token in the path IS the credential).
//   - With S3 storage the endpoint answers 302 to a signed URL on
//     another host. Go's http.Client follows it and strips the
//     Authorization header across hosts, which is exactly right — the
//     signature is the credential there, and forwarding ours would
//     leak the bot's API key to the storage provider.
func (c *Client) DownloadUpload(ctx context.Context, uploadPath string, max int64) ([]byte, string, error) {
	rest, ok := UploadRest(uploadPath)
	if !ok {
		return nil, "", fmt.Errorf("zulip: %q is not a %s path", uploadPath, UploadPrefix)
	}
	b, ct, err := c.fetchUpload(ctx, c.base+UploadPrefix+rest, true, uploadPath, max)
	if err != nil {
		return nil, "", err
	}
	// Follow the temporary-URL indirection at most once: the second
	// hop serves the file itself and never another envelope.
	if tmp, ok := temporaryUploadURL(b, ct); ok {
		return c.fetchUpload(ctx, c.realmURL(tmp), false, uploadPath, max)
	}
	return b, ct, nil
}

// fetchUpload GETs one URL and returns its bytes under the same cap
// rules as DownloadUpload. auth decides whether the bot's credentials
// go along: they must for the API endpoint, and must NOT for a
// temporary URL, whose one-shot token is itself the credential.
// what is the original upload path, used only in error text.
func (c *Client) fetchUpload(ctx context.Context, rawURL string, auth bool, what string, max int64) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, "", fmt.Errorf("zulip: build request: %w", err)
	}
	if auth {
		req.SetBasicAuth(c.email, c.apiKey)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("zulip: GET %s: %w", what, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, "", &APIError{Status: resp.StatusCode, Msg: http.StatusText(resp.StatusCode)}
	}
	r := io.Reader(resp.Body)
	if max > 0 {
		r = io.LimitReader(resp.Body, max+1)
	}
	b, err := io.ReadAll(r)
	if err != nil {
		return nil, "", fmt.Errorf("zulip: read %s: %w", what, err)
	}
	if max > 0 && int64(len(b)) > max {
		return nil, "", fmt.Errorf("%w: %s is over %d bytes", ErrUploadTooLarge, what, max)
	}
	return b, resp.Header.Get("Content-Type"), nil
}

// realmURL turns a temporary URL into something fetchable: Zulip
// writes it realm-relative, so it is hung off the realm root — the API
// base with its /api/v1 suffix removed. An absolute URL (what an S3
// deployment hands back) is already fetchable and is left alone.
func (c *Client) realmURL(ref string) string {
	if strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://") {
		return ref
	}
	return strings.TrimSuffix(c.base, "/api/v1") + ref
}

// temporaryUploadURL reports whether a download response is Zulip's
// temporary-URL envelope rather than the file, returning the URL the
// bytes actually live at.
//
// The test is deliberately narrow — JSON content type, a successful
// envelope, and a url that is itself an upload path — because a user
// may legitimately upload a .json file, and that file must never be
// mistaken for an indirection.
func temporaryUploadURL(body []byte, contentType string) (string, bool) {
	mt, _, err := mime.ParseMediaType(contentType)
	if err != nil || mt != "application/json" {
		return "", false
	}
	var env struct {
		Result string `json:"result"`
		URL    string `json:"url"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return "", false
	}
	if env.Result != "success" || !strings.Contains(env.URL, UploadPrefix) {
		return "", false
	}
	return env.URL, true
}

// UploadRest splits the part of an upload path that follows
// UploadPrefix, reporting whether the input was an upload path at all.
//
// It is deliberately strict about what it will hand to the server: the
// path is percent-decoded per segment to check it, and any segment
// that is empty, "." or ".." is refused. A `/user_uploads/../..` walk
// would otherwise leave the upload namespace entirely, and the string
// it is built from came from a message anyone in the realm can post.
//
// The rest is returned in its ORIGINAL, still-encoded spelling: a
// filename may legitimately contain a '?', a '#' or a space, and
// re-encoding it ourselves would be a second, subtly different escaper
// in front of the same server.
func UploadRest(p string) (string, bool) {
	i := strings.Index(p, UploadPrefix)
	if i < 0 {
		return "", false
	}
	rest := p[i+len(UploadPrefix):]
	if rest == "" {
		return "", false
	}
	for _, seg := range strings.Split(rest, "/") {
		dec, err := url.PathUnescape(seg)
		if err != nil {
			return "", false
		}
		if dec == "" || dec == "." || dec == ".." || strings.Contains(dec, "/") {
			return "", false
		}
	}
	return rest, true
}

// UploadName is the human filename an upload path ends in,
// percent-decoded, or "" when the path yields nothing usable.
//
// Zulip's own layout is `/user_uploads/<realm>/<ab>/<hash>/<name>`, so
// the last segment is the name a human chose — which is the whole
// reason inbound files are not stored under their hash.
func UploadName(p string) string {
	rest, ok := UploadRest(p)
	if !ok {
		return ""
	}
	// The error is dropped because UploadRest has already decoded
	// every segment of rest successfully; a second failure here would
	// be a contradiction, not a case.
	name, _ := url.PathUnescape(path.Base(rest))
	return name
}
