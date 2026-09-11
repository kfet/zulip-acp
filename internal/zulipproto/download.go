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
// Which URL serves the BYTES is the whole subtlety here, and the two
// spellings of `/user_uploads/…` are different endpoints, not two
// routes to one:
//
//   - `<realm root>/user_uploads/<rest>` is the download. Zulip's
//     rest_dispatch accepts `email:api_key` HTTP Basic there exactly
//     as it does on an /api/v1 call, and 12.2's serve_file_backend
//     (url_only=False) answers 200 with the file's own Content-Type
//     in ONE hop. This is the primary path.
//   - `<api base>/user_uploads/<rest>` is the documented *"get public
//     temporary URL"* endpoint (zulip.com/api/get-file-temporary-url,
//     new in Zulip 3.0 / feature level 1): serve_file_backend with
//     url_only=True, which returns
//     {"result":"success","url":"/user_uploads/temporary/<token>"}
//     and never the file. The token is a Django TimestampSigner
//     signature valid for SIGNED_ACCESS_TOKEN_VALIDITY_IN_SECONDS —
//     60s by default — and the docs require consumers to request it
//     immediately and never store it. It exists for clients whose
//     HTTP stack cannot follow redirects; here it is only a FALLBACK,
//     taken when the direct download did not yield a file.
//
// Writing a non-file into the inbox is the failure mode this guards:
// an unauthenticated request is answered with 302 →
// /accounts/login/?next=…, so a blindly-followed redirect chain saves
// an HTML login page under the attachment's name. The redirect policy
// below refuses that hop outright, and isNotFile rejects an HTML page
// or a JSON envelope even if some proxy serves one with status 200.
//
// Cross-host redirects are still allowed and still useful: an S3
// deployment in dev mode answers 302 to a signed URL, and Go's
// http.Client strips the Authorization header across hosts — correct,
// since the signature is the credential there and forwarding ours
// would leak the bot's API key to the storage provider. (In
// production the S3 hop is an internal nginx X-Accel-Redirect the
// client never sees.)
func (c *Client) DownloadUpload(ctx context.Context, uploadPath string, max int64) ([]byte, string, error) {
	rest, ok := UploadRest(uploadPath)
	if !ok {
		return nil, "", fmt.Errorf("zulip: %q is not a %s path", uploadPath, UploadPrefix)
	}
	b, ct, err := c.fetchUpload(ctx, c.realmURL(UploadPrefix+rest), true, uploadPath, max)
	switch {
	case errors.Is(err, ErrUploadTooLarge):
		// The cap is an answer about the file, not a failure to
		// reach it: a second endpoint would report the same size.
		return nil, "", err
	case err == nil && !isNotFile(b, ct):
		return b, ct, nil
	}
	// The direct download did not produce a file. Ask the documented
	// temporary-URL endpoint and follow it ONCE, immediately — the
	// token expires in about a minute and must never be stored.
	env, ect, ferr := c.fetchUpload(ctx, c.base+UploadPrefix+rest, true, uploadPath, max)
	if ferr != nil {
		return nil, "", ferr
	}
	tmp, ok := temporaryUploadURL(env, ect)
	if !ok {
		if err != nil {
			return nil, "", err
		}
		return nil, "", fmt.Errorf("zulip: %s served %s, not the file", uploadPath, ct)
	}
	return c.fetchUpload(ctx, c.realmURL(tmp), false, uploadPath, max)
}

// loginPrefixes are the paths Zulip redirects an unauthenticated (or
// unauthorised) upload request to. A redirect there is an auth
// failure wearing a 200's clothes: follow it and you save the login
// page as the attachment.
var loginPrefixes = []string{"/accounts/login", "/login"}

// fetchUpload GETs one URL and returns its bytes under the same cap
// rules as DownloadUpload. auth decides whether the bot's credentials
// go along: they must on both Zulip endpoints, and must NOT on a
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
	// A copy, so the redirect policy applies to this request only and
	// the shared client (and its connection pool) is untouched.
	hc := *c.hc
	hc.CheckRedirect = func(r *http.Request, via []*http.Request) error {
		for _, p := range loginPrefixes {
			if strings.HasPrefix(r.URL.Path, p) {
				return fmt.Errorf("redirected to %s: not authorized for this upload", r.URL.Path)
			}
		}
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		return nil
	}
	resp, err := hc.Do(req)
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

// isNotFile reports that a 200 response is a PAGE, not an attachment:
// an HTML body (Zulip's login or error page, or a proxy's) or the
// temporary-URL JSON envelope. Either one written into the inbox
// under the attachment's name is the bug this exists to prevent.
func isNotFile(body []byte, contentType string) bool {
	if mt, _, err := mime.ParseMediaType(contentType); err == nil && mt == "text/html" {
		return true
	}
	_, envelope := temporaryUploadURL(body, contentType)
	return envelope
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
