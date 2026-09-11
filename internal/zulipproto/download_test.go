package zulipproto

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// rawServer serves attachment bytes, not JSON envelopes — which is the
// whole reason DownloadUpload cannot go through Client.send.
func rawServer(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

func rawClient(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()
	c, err := New(Config{Site: srv.URL, Email: "bot@example.com", APIKey: "key", HTTPClient: srv.Client()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func TestDownloadUpload(t *testing.T) {
	var gotPath, gotUser, gotPass string
	srv := rawServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath()
		gotUser, gotPass, _ = r.BasicAuth()
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write([]byte("JPEGBYTES"))
	})
	c := rawClient(t, srv)

	b, ct, err := c.DownloadUpload(context.Background(), "/user_uploads/2/ab/HASH/holiday%20snap.jpg", 1024)
	if err != nil {
		t.Fatalf("DownloadUpload: %v", err)
	}
	if string(b) != "JPEGBYTES" {
		t.Fatalf("body = %q", b)
	}
	if ct != "image/jpeg" {
		t.Fatalf("content type = %q", ct)
	}
	// The download is the REALM-ROOT path (the /api/v1 spelling is
	// the temporary-URL endpoint, not the file), and the filename's
	// original encoding is passed through untouched.
	if gotPath != "/user_uploads/2/ab/HASH/holiday%20snap.jpg" {
		t.Fatalf("path = %q", gotPath)
	}
	if gotUser != "bot@example.com" || gotPass != "key" {
		t.Fatalf("auth = %q:%q", gotUser, gotPass)
	}
}

// TestDownloadUploadUnbounded pins that a non-positive cap means "no
// cap" rather than "refuse everything".
func TestDownloadUploadUnbounded(t *testing.T) {
	srv := rawServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(make([]byte, 4096))
	})
	b, _, err := rawClient(t, srv).DownloadUpload(context.Background(), "/user_uploads/2/ab/HASH/big.bin", 0)
	if err != nil {
		t.Fatalf("DownloadUpload: %v", err)
	}
	if len(b) != 4096 {
		t.Fatalf("read %d bytes", len(b))
	}
}

func TestDownloadUploadTooLarge(t *testing.T) {
	srv := rawServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(make([]byte, 100))
	})
	b, _, err := rawClient(t, srv).DownloadUpload(context.Background(), "/user_uploads/2/ab/HASH/big.bin", 99)
	if !errors.Is(err, ErrUploadTooLarge) {
		t.Fatalf("err = %v, want ErrUploadTooLarge", err)
	}
	// No bytes come back: the cap has to bound memory, not merely
	// report on it after the fact.
	if b != nil {
		t.Fatalf("got %d bytes with an over-cap error", len(b))
	}
}

// TestDownloadUploadExactlyAtCap pins the boundary: a file the same
// size as the cap is fine. Off by one here would silently drop every
// attachment of exactly the configured size.
func TestDownloadUploadExactlyAtCap(t *testing.T) {
	srv := rawServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(make([]byte, 99))
	})
	b, _, err := rawClient(t, srv).DownloadUpload(context.Background(), "/user_uploads/2/ab/HASH/big.bin", 99)
	if err != nil || len(b) != 99 {
		t.Fatalf("DownloadUpload = %d bytes, %v", len(b), err)
	}
}

func TestDownloadUploadHTTPError(t *testing.T) {
	srv := rawServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	})
	_, _, err := rawClient(t, srv).DownloadUpload(context.Background(), "/user_uploads/2/ab/HASH/x.bin", 10)
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != http.StatusForbidden {
		t.Fatalf("err = %v, want a 403 APIError", err)
	}
}

func TestDownloadUploadTransportError(t *testing.T) {
	srv := rawServer(t, func(http.ResponseWriter, *http.Request) {})
	c := rawClient(t, srv)
	srv.Close()
	_, _, err := c.DownloadUpload(context.Background(), "/user_uploads/2/ab/HASH/x.bin", 10)
	if err == nil {
		t.Fatal("expected a transport error from a closed server")
	}
}

// TestDownloadUploadTruncatedBody drives the read failure: the server
// promises a length and then hangs up, so io.ReadAll fails partway.
func TestDownloadUploadTruncatedBody(t *testing.T) {
	srv := rawServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "64")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("short"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		panic(http.ErrAbortHandler)
	})
	_, _, err := rawClient(t, srv).DownloadUpload(context.Background(), "/user_uploads/2/ab/HASH/x.bin", 1024)
	if err == nil {
		t.Fatal("expected a read error on a truncated body")
	}
}

// TestDownloadUploadBadRequest drives newRequest's failure: a DEL byte
// in the path is an invalid control character in a URL. It is
// reachable because the path is built from a message body anyone in
// the realm can post.
func TestDownloadUploadBadRequest(t *testing.T) {
	srv := rawServer(t, func(http.ResponseWriter, *http.Request) {})
	_, _, err := rawClient(t, srv).DownloadUpload(context.Background(), "/user_uploads/2/ab/HASH/x\x7f.bin", 10)
	if err == nil {
		t.Fatal("expected a request-construction error")
	}
}

func TestUploadRest(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
		ok   bool
	}{
		{"relative", "/user_uploads/2/ab/HASH/a.png", "2/ab/HASH/a.png", true},
		{"absolute", "https://zulip.example.com/user_uploads/2/ab/H/a.png", "2/ab/H/a.png", true},
		{"encoded name kept as written", "/user_uploads/2/ab/H/my%20file.png", "2/ab/H/my%20file.png", true},
		{"not an upload", "/user_avatars/2/x.png", "", false},
		{"nothing after the prefix", "/user_uploads/", "", false},
		{"empty segment", "/user_uploads/2//a.png", "", false},
		{"dot segment", "/user_uploads/2/./a.png", "", false},
		{"parent walk", "/user_uploads/2/../../etc/passwd", "", false},
		{"encoded parent walk", "/user_uploads/2/%2e%2e/x", "", false},
		{"smuggled separator", "/user_uploads/2/a%2Fb/x", "", false},
		{"bad escape", "/user_uploads/2/%zz/x", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := UploadRest(tc.in)
			if got != tc.want || ok != tc.ok {
				t.Fatalf("UploadRest(%q) = %q, %v; want %q, %v", tc.in, got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestDownloadUploadRejectsNonUploadPath(t *testing.T) {
	srv := rawServer(t, func(http.ResponseWriter, *http.Request) {
		t.Error("server must not be reached")
	})
	_, _, err := rawClient(t, srv).DownloadUpload(context.Background(), "/etc/passwd", 10)
	if err == nil || !strings.Contains(err.Error(), "is not a /user_uploads/ path") {
		t.Fatalf("err = %v", err)
	}
}

func TestUploadName(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"/user_uploads/2/ab/HASH/holiday%20snap.jpg", "holiday snap.jpg"},
		{"https://z.example.com/user_uploads/2/ab/HASH/report.pdf", "report.pdf"},
		{"/user_uploads/2/ab/HASH/%E6%97%A5.txt", "日.txt"},
		{"/nope/x.txt", ""},
	}
	for _, tc := range tests {
		if got := UploadName(tc.in); got != tc.want {
			t.Fatalf("UploadName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestDownloadUploadTemporaryURLFallback pins the fallback: when the
// direct download does not yield a file, the documented
// "get public temporary URL" endpoint under /api/v1 is asked, and the
// URL it returns is fetched immediately and WITHOUT credentials.
func TestDownloadUploadTemporaryURLFallback(t *testing.T) {
	var hops []string
	var tmpAuthed bool
	srv := rawServer(t, func(w http.ResponseWriter, r *http.Request) {
		hops = append(hops, r.URL.EscapedPath())
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/v1/user_uploads/"):
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			_, _ = w.Write([]byte(`{"result":"success","msg":"","url":"/user_uploads/temporary/tok/report.pdf"}`))
		case strings.HasPrefix(r.URL.Path, "/user_uploads/temporary/"):
			_, _, tmpAuthed = r.BasicAuth()
			w.Header().Set("Content-Type", "application/pdf")
			_, _ = w.Write([]byte("%PDF-1.7"))
		default:
			// A proxy that answers the direct download with a page.
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte("<html>log in</html>"))
		}
	})
	c := rawClient(t, srv)

	b, ct, err := c.DownloadUpload(context.Background(), "/user_uploads/2/ab/HASH/report.pdf", 1024)
	if err != nil {
		t.Fatalf("DownloadUpload: %v", err)
	}
	if string(b) != "%PDF-1.7" || ct != "application/pdf" {
		t.Fatalf("body = %q, ct = %q", b, ct)
	}
	want := []string{
		"/user_uploads/2/ab/HASH/report.pdf",
		"/api/v1/user_uploads/2/ab/HASH/report.pdf",
		"/user_uploads/temporary/tok/report.pdf",
	}
	if len(hops) != 3 || hops[0] != want[0] || hops[1] != want[1] || hops[2] != want[2] {
		t.Fatalf("hops = %q, want %q", hops, want)
	}
	// The token is the credential on the last hop; the bot's API key
	// must not travel with it.
	if tmpAuthed {
		t.Fatal("temporary URL fetched with the bot's credentials")
	}
}

// TestDownloadUploadRefusesLoginRedirect pins the failure mode that
// started all this: an unauthorised upload request is answered with a
// redirect to the login page, and following it would save an HTML
// page under the attachment's name.
func TestDownloadUploadRefusesLoginRedirect(t *testing.T) {
	srv := rawServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/accounts/login") {
			t.Error("login page must not be fetched")
			return
		}
		http.Redirect(w, r, "/accounts/login/?next="+r.URL.Path, http.StatusFound)
	})
	_, _, err := rawClient(t, srv).DownloadUpload(context.Background(), "/user_uploads/2/ab/HASH/x.pdf", 1024)
	if err == nil || !strings.Contains(err.Error(), "not authorized") {
		t.Fatalf("err = %v, want a refused login redirect", err)
	}
}

// TestDownloadUploadNeitherEndpointServesAFile pins the honest
// failure: the direct download returns a page and the fallback does
// not hand back a temporary URL either, so nothing is written.
func TestDownloadUploadNeitherEndpointServesAFile(t *testing.T) {
	srv := rawServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html>nope</html>"))
	})
	_, _, err := rawClient(t, srv).DownloadUpload(context.Background(), "/user_uploads/2/ab/HASH/x.pdf", 1024)
	if err == nil || !strings.Contains(err.Error(), "not the file") {
		t.Fatalf("err = %v", err)
	}
}

// TestDownloadUploadReportsDirectError pins that when the direct
// download failed outright and the fallback is not an indirection
// either, the caller hears about the FIRST, more meaningful failure.
func TestDownloadUploadReportsDirectError(t *testing.T) {
	srv := rawServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/v1/") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"result":"success","msg":"no url here"}`))
			return
		}
		w.WriteHeader(http.StatusForbidden)
	})
	_, _, err := rawClient(t, srv).DownloadUpload(context.Background(), "/user_uploads/2/ab/HASH/x.pdf", 1024)
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != http.StatusForbidden {
		t.Fatalf("err = %v, want the 403 from the direct download", err)
	}
}

// TestDownloadUploadTooLargeSkipsFallback pins that the size cap is an
// answer about the file, not a reason to try the other endpoint.
func TestDownloadUploadTooLargeSkipsFallback(t *testing.T) {
	var hops int
	srv := rawServer(t, func(w http.ResponseWriter, _ *http.Request) {
		hops++
		_, _ = w.Write([]byte("0123456789"))
	})
	_, _, err := rawClient(t, srv).DownloadUpload(context.Background(), "/user_uploads/2/ab/HASH/x.bin", 3)
	if !errors.Is(err, ErrUploadTooLarge) {
		t.Fatalf("err = %v", err)
	}
	if hops != 1 {
		t.Fatalf("hops = %d, want 1", hops)
	}
}

// TestDownloadUploadJSONFile pins that an uploaded .json file is
// served through untouched — only a success envelope carrying an
// upload URL is an indirection.
func TestDownloadUploadJSONFile(t *testing.T) {
	const body = `{"result":"success","msg":"not an indirection"}`
	srv := rawServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	})
	b, _, err := rawClient(t, srv).DownloadUpload(context.Background(), "/user_uploads/2/ab/HASH/data.json", 1024)
	if err != nil {
		t.Fatalf("DownloadUpload: %v", err)
	}
	if string(b) != body {
		t.Fatalf("body = %q", b)
	}
}

// TestTemporaryUploadURL pins what is and is not an indirection: only
// a JSON success envelope whose url is itself an upload path.
func TestTemporaryUploadURL(t *testing.T) {
	const env = `{"result":"success","url":"/user_uploads/temporary/tok/a.pdf"}`
	cases := []struct {
		name, body, ctype, want string
		ok                      bool
	}{
		{"envelope", env, "application/json; charset=utf-8", "/user_uploads/temporary/tok/a.pdf", true},
		{"absolute url", `{"result":"success","url":"https://s3/user_uploads/t/a.pdf"}`, "application/json", "https://s3/user_uploads/t/a.pdf", true},
		{"not json", env, "application/pdf", "", false},
		{"unparseable content type", env, "application/json; charset", "", false},
		{"not json at all", "not json", "application/json", "", false},
		{"error envelope", `{"result":"error","url":"/user_uploads/x/a.pdf"}`, "application/json", "", false},
		{"no url", `{"result":"success","msg":"hi"}`, "application/json", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := temporaryUploadURL([]byte(tc.body), tc.ctype)
			if ok != tc.ok || got != tc.want {
				t.Fatalf("got %q,%v want %q,%v", got, ok, tc.want, tc.ok)
			}
		})
	}
}

// TestRealmURL pins that a relative temporary URL hangs off the realm
// root (the API base without /api/v1) and an absolute one is untouched.
func TestRealmURL(t *testing.T) {
	c, err := New(Config{Site: "https://zulip.example.com", Email: "b@e.com", APIKey: "k"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := c.realmURL("/user_uploads/temporary/tok/a.pdf"); got != "https://zulip.example.com/user_uploads/temporary/tok/a.pdf" {
		t.Fatalf("relative = %q", got)
	}
	if got := c.realmURL("https://s3.example.com/x?sig=1"); got != "https://s3.example.com/x?sig=1" {
		t.Fatalf("absolute = %q", got)
	}
}

// TestDownloadUploadFollowsBenignRedirect pins that the redirect
// policy only blocks the login page: an S3 deployment in dev mode
// answers 302 to a signed URL, and that hop must be followed.
func TestDownloadUploadFollowsBenignRedirect(t *testing.T) {
	srv := rawServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/signed/") {
			w.Header().Set("Content-Type", "application/pdf")
			_, _ = w.Write([]byte("%PDF-1.4"))
			return
		}
		http.Redirect(w, r, "/signed/blob?sig=abc", http.StatusFound)
	})
	b, ct, err := rawClient(t, srv).DownloadUpload(context.Background(), "/user_uploads/2/ab/HASH/x.pdf", 1024)
	if err != nil {
		t.Fatalf("DownloadUpload: %v", err)
	}
	if string(b) != "%PDF-1.4" || ct != "application/pdf" {
		t.Fatalf("body = %q, ct = %q", b, ct)
	}
}

// TestDownloadUploadRedirectLoop pins the hop ceiling: a server that
// redirects forever must end the attempt, not hang it.
func TestDownloadUploadRedirectLoop(t *testing.T) {
	n := 0
	srv := rawServer(t, func(w http.ResponseWriter, r *http.Request) {
		n++
		http.Redirect(w, r, fmt.Sprintf("/hop/%d", n), http.StatusFound)
	})
	_, _, err := rawClient(t, srv).DownloadUpload(context.Background(), "/user_uploads/2/ab/HASH/x.pdf", 1024)
	if err == nil || !strings.Contains(err.Error(), "stopped after 10 redirects") {
		t.Fatalf("err = %v", err)
	}
}
