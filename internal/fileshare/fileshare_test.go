package fileshare

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func newServer(t *testing.T, store Store) (*Server, *clock) {
	t.Helper()
	c := &clock{t: time.Unix(1000, 0)}
	s, err := New(Config{PublicURL: "https://relay.example/", Store: store, Now: c.now})
	if err != nil {
		t.Fatal(err)
	}
	return s, c
}

func okStore(path string) Store {
	return func(_ context.Context, name, mt string, data []byte) (string, error) {
		return path + name + ":" + mt + ":" + string(data), nil
	}
}

func put(s *Server, url, body string, cl bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPut, url, strings.NewReader(body))
	if !cl {
		req.ContentLength = -1
		req.Header.Del("Content-Length")
	} else {
		req.Header.Set("Content-Length", strconv.Itoa(len(body)))
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	return w
}

func TestNewValidates(t *testing.T) {
	if _, err := New(Config{PublicURL: "http://x", Store: okStore("")}); err == nil {
		t.Fatal("http accepted")
	}
	if _, err := New(Config{PublicURL: "https://x"}); err == nil {
		t.Fatal("nil store accepted")
	}
	s, err := New(Config{PublicURL: "https://x", Store: okStore(""), Logf: t.Logf})
	if err != nil || s.cfg.TTL != DefaultTTL || s.cfg.StoreTimeout != time.Minute || s.cfg.Now == nil {
		t.Fatalf("defaults: %v %+v", err, s.cfg)
	}
}

func TestRoundTrip(t *testing.T) {
	s, _ := newServer(t, okStore("/user_uploads/"))
	url, ref, exp, err := s.Mint("conv", Spec{Name: "a.txt", MimeType: "text/plain", Size: 5})
	if err != nil || !strings.HasPrefix(url, "https://relay.example/up/") || len(ref) != 32 || exp.IsZero() {
		t.Fatalf("mint: %q %q %v %v", url, ref, exp, err)
	}
	if _, _, err := s.Finish("conv", ref); err == nil {
		t.Fatal("finish before upload")
	}
	if w := put(s, url, "hello", true); w.Code != http.StatusCreated {
		t.Fatalf("put: %d %s", w.Code, w.Body)
	}
	if w := put(s, url, "hello", true); w.Code != http.StatusConflict {
		t.Fatalf("second put: %d", w.Code)
	}
	if _, _, err := s.Finish("other", ref); err == nil {
		t.Fatal("other owner finished")
	}
	p, n, err := s.Finish("conv", ref)
	if err != nil || p != "/user_uploads/a.txt:text/plain:hello" || n != 5 {
		t.Fatalf("finish: %q %d %v", p, n, err)
	}
	if _, _, err := s.Finish("conv", ref); err == nil {
		t.Fatal("finish twice")
	}
}

func TestPutWithoutContentLength(t *testing.T) {
	s, _ := newServer(t, okStore(""))
	url, _, _, _ := s.Mint("c", Spec{Size: 3})
	if w := put(s, url, "abc", false); w.Code != http.StatusCreated {
		t.Fatalf("%d", w.Code)
	}
}

func TestPutRefusals(t *testing.T) {
	s, c := newServer(t, okStore(""))
	for _, p := range []string{"/x", "/up/", "/up/a/b"} {
		if w := put(s, "https://relay.example"+p, "", true); w.Code != http.StatusNotFound {
			t.Fatalf("%s: %d", p, w.Code)
		}
	}
	url, _, _, _ := s.Mint("c", Spec{Size: 3})
	req := httptest.NewRequest(http.MethodGet, url, nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("get: %d", w.Code)
	}
	// Wrong Content-Length burns the token.
	if w := put(s, url, "abcd", true); w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("long cl: %d", w.Code)
	}
	if w := put(s, url, "abc", true); w.Code != http.StatusNotFound {
		t.Fatalf("burnt: %d", w.Code)
	}
	// Bad Content-Length header value.
	url, _, _, _ = s.Mint("c", Spec{Size: 3})
	req = httptest.NewRequest(http.MethodPut, url, strings.NewReader("abc"))
	req.Header.Set("Content-Length", "x")
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("bad cl: %d", w.Code)
	}
	// Short and long bodies without Content-Length.
	for _, body := range []string{"ab", "abcd"} {
		url, _, _, _ = s.Mint("c", Spec{Size: 3})
		if w := put(s, url, body, false); w.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("%q: %d", body, w.Code)
		}
	}
	// Expired.
	url, ref, _, _ := s.Mint("c", Spec{Size: 3})
	c.t = c.t.Add(DefaultTTL + time.Second)
	if w := put(s, url, "abc", true); w.Code != http.StatusNotFound {
		t.Fatalf("expired: %d", w.Code)
	}
	// Mint purges it.
	_, _, _, _ = s.Mint("c", Spec{Size: 1})
	if _, _, err := s.Finish("c", ref); err == nil {
		t.Fatal("expired finished")
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("boom") }

func TestPutReadError(t *testing.T) {
	s, _ := newServer(t, okStore(""))
	url, _, _, _ := s.Mint("c", Spec{Size: 3})
	req := httptest.NewRequest(http.MethodPut, url, errReader{})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("%d", w.Code)
	}
}

func TestStoreError(t *testing.T) {
	s, _ := newServer(t, func(context.Context, string, string, []byte) (string, error) {
		return "", errors.New("zulip down")
	})
	url, ref, _, _ := s.Mint("c", Spec{Size: 1})
	if w := put(s, url, "x", true); w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), "zulip down") {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if _, _, err := s.Finish("c", ref); err == nil {
		t.Fatal("finished after store error")
	}
}

func TestMintLimits(t *testing.T) {
	s, _ := newServer(t, okStore(""))
	if _, _, _, err := s.Mint("c", Spec{Size: 0}); err == nil {
		t.Fatal("zero size")
	}
	for i := 0; i < maxPerOwner; i++ {
		if _, _, _, err := s.Mint("c", Spec{Size: 1}); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, _, err := s.Mint("c", Spec{Size: 1}); err == nil {
		t.Fatal("over the owner cap")
	}
	for i := maxPerOwner; i < maxPending; i++ {
		if _, _, _, err := s.Mint(strconv.Itoa(i), Spec{Size: 1}); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, _, err := s.Mint("d", Spec{Size: 1}); err == nil {
		t.Fatal("over the total cap")
	}
}

func TestBusy(t *testing.T) {
	s, _ := newServer(t, okStore(""))
	for i := 0; i < maxReceiving; i++ {
		s.busy <- struct{}{}
	}
	url, _, _, _ := s.Mint("c", Spec{Size: 1})
	if w := put(s, url, "x", true); w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") == "" {
		t.Fatalf("%d", w.Code)
	}
	<-s.busy
	if w := put(s, url, "x", true); w.Code != http.StatusCreated {
		t.Fatalf("%d", w.Code)
	}
}

func TestStoredOutlivesTarget(t *testing.T) {
	s, c := newServer(t, okStore("p"))
	url, ref, _, _ := s.Mint("c", Spec{Size: 1})
	if w := put(s, url, "x", true); w.Code != http.StatusCreated {
		t.Fatalf("%d", w.Code)
	}
	c.t = c.t.Add(StoredTTL - time.Second)
	_, _, _, _ = s.Mint("c", Spec{Size: 1}) // purges what has expired
	if _, _, err := s.Finish("c", ref); err != nil {
		t.Fatalf("stored file purged at the target's expiry: %v", err)
	}
}
