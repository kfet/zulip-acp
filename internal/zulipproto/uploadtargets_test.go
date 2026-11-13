package zulipproto

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

func TestHasUploadTargets(t *testing.T) {
	code := 0
	srv := rawServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/zlite/upload_targets" {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		if _, _, ok := r.BasicAuth(); !ok {
			t.Error("probe without auth")
		}
		w.WriteHeader(code)
	})
	c := rawClient(t, srv)
	for c2, want := range map[int]bool{404: false, 405: false, 400: true, 200: true} {
		code = c2
		got, err := c.HasUploadTargets(context.Background())
		if err != nil || got != want {
			t.Fatalf("%d: %v %v", c2, got, err)
		}
	}
	for _, c2 := range []int{401, 403, 502} {
		code = c2
		if got, err := c.HasUploadTargets(context.Background()); err == nil || got {
			t.Fatalf("%d: %v %v", c2, got, err)
		}
	}
	srv.Close()
	if _, err := c.HasUploadTargets(context.Background()); err == nil {
		t.Fatal("dead server")
	}
	var nilCtx context.Context
	if _, err := c.HasUploadTargets(nilCtx); err == nil {
		t.Fatal("nil context")
	}
}

func TestCreateUploadTarget(t *testing.T) {
	body := ""
	srv := rawServer(t, func(w http.ResponseWriter, r *http.Request) {
		// Decode the body the way zlite's mintTarget does
		// (internal/front/uptarget.go): a JSON object, size a number.
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("content type %q", ct)
		}
		var in struct {
			Name string `json:"name"`
			Size *int64 `json:"size"`
			Mime string `json:"mime"`
			TTL  int64  `json:"ttl"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Name != "a.bin" || in.Size == nil || *in.Size != 5 || in.Mime != "x/y" || in.TTL != 600 {
			t.Errorf("body %+v %v", in, err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	})
	c := rawClient(t, srv)
	ctx := context.Background()
	body = `{"result":"success","url":"/zlite/put/abc","token":"abc","expiresAt":"2026-11-11T10:00:00Z"}`
	tg, err := c.CreateUploadTarget(ctx, "a.bin", 5, "x/y", 10*time.Minute)
	if err != nil || tg.URL != srv.URL+"/zlite/put/abc" || tg.Token != "abc" || !tg.ExpiresAt.Equal(time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("%+v %v", tg, err)
	}
	body = `{"result":"success","url":"https://up.example/x","token":"t","expiresAt":1800000000}`
	tg, err = c.CreateUploadTarget(ctx, "a.bin", 5, "x/y", 10*time.Minute)
	if err != nil || tg.URL != "https://up.example/x" || tg.ExpiresAt.Unix() != 1800000000 {
		t.Fatalf("%+v %v", tg, err)
	}
	for _, b := range []string{`{"result":"error","msg":"no"}`, `{"result":"success","url":"","token":"t"}`} {
		body = b
		if _, err := c.CreateUploadTarget(ctx, "a.bin", 5, "x/y", 10*time.Minute); err == nil {
			t.Fatalf("%s: no error", b)
		}
	}
	var nilCtx context.Context
	if _, err := c.CreateUploadTarget(nilCtx, "a.bin", 5, "x/y", time.Minute); err == nil {
		t.Fatal("nil context")
	}
}

func TestGetUploadTarget(t *testing.T) {
	fail := false
	srv := rawServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/zlite/upload_targets/a%2Fb" && r.URL.RawPath != "/api/v1/zlite/upload_targets/a%2Fb" {
			t.Errorf("path %q", r.URL.RawPath)
		}
		w.Header().Set("Content-Type", "application/json")
		if fail {
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"result":"error","msg":"gone"}`))
			return
		}
		_, _ = w.Write([]byte(`{"result":"success","status":"done","path":"/user_uploads/2/x/a","size":5}`))
	})
	c := rawClient(t, srv)
	st, err := c.GetUploadTarget(context.Background(), "a/b")
	if err != nil || st != (UploadTargetStatus{Status: "done", Path: "/user_uploads/2/x/a", Size: 5}) {
		t.Fatalf("%+v %v", st, err)
	}
	fail = true
	if _, err := c.GetUploadTarget(context.Background(), "a/b"); err == nil {
		t.Fatal("no error")
	}
}

func TestParseTime(t *testing.T) {
	for _, raw := range []string{`"junk"`, `-1`, `null`, `{}`} {
		if !parseTime(json.RawMessage(raw)).IsZero() {
			t.Fatalf("%s", raw)
		}
	}
}
