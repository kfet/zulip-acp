package zulipproto

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

func TestTemporaryUpload(t *testing.T) {
	var headAuthed bool
	srv := rawServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/v1/user_uploads/"):
			if _, _, ok := r.BasicAuth(); !ok {
				t.Error("envelope fetched without credentials")
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"result":"success","url":"/user_uploads/temporary/tok/a.pdf"}`))
		case r.URL.Path == "/user_uploads/temporary/tok/a.pdf" && r.Method == http.MethodHead:
			_, _, headAuthed = r.BasicAuth()
			w.Header().Set("Content-Type", "application/pdf")
			w.Header().Set("Content-Length", "1234")
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	})
	c := rawClient(t, srv)
	u, n, ct, err := c.TemporaryUpload(context.Background(), "/user_uploads/2/ab/H/a.pdf")
	if err != nil || u != srv.URL+"/user_uploads/temporary/tok/a.pdf" || n != 1234 || ct != "application/pdf" {
		t.Fatalf("%q %d %q %v", u, n, ct, err)
	}
	if headAuthed {
		t.Fatal("HEAD sent the bot's credentials")
	}
}

func TestTemporaryUploadErrors(t *testing.T) {
	ctx := context.Background()
	mode := ""
	srv := rawServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/v1/") {
			switch mode {
			case "envfail":
				w.WriteHeader(http.StatusForbidden)
				return
			case "noenv":
				w.Header().Set("Content-Type", "text/plain")
				_, _ = w.Write([]byte("x"))
				return
			case "badurl":
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"result":"success","url":"http://[::1/user_uploads/x"}`))
				return
			case "deadurl":
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"result":"success","url":"http://127.0.0.1:1/user_uploads/x"}`))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"result":"success","url":"/user_uploads/temporary/t/x"}`))
			return
		}
		switch mode {
		case "head404":
			w.WriteHeader(http.StatusNotFound)
		case "nolen", "badrange":
			if r.Method == http.MethodGet && mode == "badrange" {
				w.Header().Set("Content-Range", "bytes junk")
				w.WriteHeader(http.StatusPartialContent)
				return
			}
			w.(http.Flusher).Flush()
		case "getdrop":
			if r.Method == http.MethodGet {
				conn, _, _ := w.(http.Hijacker).Hijack()
				_ = conn.Close()
				return
			}
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
	c := rawClient(t, srv)
	if _, _, _, err := c.TemporaryUpload(ctx, "/nope"); err == nil {
		t.Fatal("non-upload path accepted")
	}
	for _, m := range []string{"envfail", "noenv", "badurl", "deadurl", "head404", "nolen", "badrange", "getdrop"} {
		mode = m
		if _, _, _, err := c.TemporaryUpload(ctx, "/user_uploads/1/x"); err == nil {
			t.Fatalf("%s: no error", m)
		}
	}
}

func TestTemporaryUploadSizeFromGet(t *testing.T) {
	mode := ""
	srv := rawServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/v1/") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"result":"success","url":"/user_uploads/temporary/t/x"}`))
			return
		}
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if r.Header.Get("Range") != "bytes=0-0" {
			t.Errorf("range %q", r.Header.Get("Range"))
		}
		w.Header().Set("Content-Type", "text/plain")
		if mode == "206" {
			w.Header().Set("Content-Range", "bytes 0-0/777")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write([]byte("x"))
			return
		}
		_, _ = w.Write([]byte("abcd"))
	})
	c := rawClient(t, srv)
	for m, want := range map[string]int64{"206": 777, "200": 4} {
		mode = m
		_, n, ct, err := c.TemporaryUpload(context.Background(), "/user_uploads/1/x")
		if err != nil || n != want || ct != "text/plain" {
			t.Fatalf("%s: %d %q %v", m, n, ct, err)
		}
	}
}
