package handler

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kfet/acp-kit/command"
	"github.com/kfet/acp-kit/mcphost"
	"github.com/kfet/zulip-acp/internal/fileshare"
	"github.com/kfet/zulip-acp/internal/journal"
	"github.com/kfet/zulip-acp/internal/zulipproto"
)

func TestPostFileToUploadsAndLinks(t *testing.T) {
	lh := newLoopHarness(t, newAgent("ok"), nil)
	ids, err := lh.h.PostFileTo(chanToken("loopback"), command.OutFile{Name: "a[1].txt", MimeType: "text/plain", Data: []byte("hi"), Caption: " see "})
	if err != nil || len(ids) != 1 {
		t.Fatalf("PostFileTo: %v %v", ids, err)
	}
	if got := string(lh.z.uploads["a[1].txt"]); got != "hi" {
		t.Fatalf("uploaded %q", got)
	}
	if b := lh.z.stored()[0]; b != "see\n\n[a(1).txt](/user_uploads/2/ab/a[1].txt)" {
		t.Fatalf("body %q", b)
	}
	// No caption: the link alone.
	if _, err := lh.h.PostFileTo(chanToken("loopback"), command.OutFile{Name: "b", Data: []byte("x")}); err != nil {
		t.Fatal(err)
	}
	if b := lh.z.stored()[1]; b != "[b](/user_uploads/2/ab/b)" {
		t.Fatalf("body %q", b)
	}
}

func TestPostFileToRefusals(t *testing.T) {
	lh := newLoopHarness(t, newAgent("ok"), nil)
	if _, err := lh.h.PostFileTo("bad", command.OutFile{Name: "a", Data: []byte("x")}); err == nil {
		t.Fatal("bad token")
	}
	lh.z.uploadEr = errors.New("full")
	if _, err := lh.h.PostFileTo(chanToken("t"), command.OutFile{Name: "a", Data: []byte("x")}); err == nil || !strings.Contains(err.Error(), "full") {
		t.Fatalf("err %v", err)
	}
}

func TestAttachmentsIn(t *testing.T) {
	lh := newLoopHarness(t, newAgent("ok"), nil)
	lh.z.history = []zulipproto.Message{
		{ID: 1, Content: "[old.pdf](/user_uploads/2/aa/old.pdf) and [dup](/user_uploads/2/cc/pic.png)"},
		{ID: 2, Content: "no files"},
		{ID: 3, Content: "![pic](/user_uploads/2/cc/pic.png) [x](/user_uploads/2/dd/data.zzunknown)"},
		{ID: 4, SenderID: botID, Content: "[mine](/user_uploads/9/zz/secret)"},
		{ID: 5, SenderID: 777, Content: "[other bot](/user_uploads/9/zz/secret2)"},
	}
	lh.h.cfg.BotSenderIDs = map[int64]struct{}{777: {}}
	as, err := lh.h.AttachmentsIn(chanToken("t"))
	if err != nil {
		t.Fatal(err)
	}
	want := []command.InboundAttachment{
		{ID: "2/cc/pic.png", Name: "pic.png", MimeType: "image/png", Size: -1},
		{ID: "2/dd/data.zzunknown", Name: "data.zzunknown", MimeType: zulipproto.DefaultContentType, Size: -1},
		{ID: "2/aa/old.pdf", Name: "old.pdf", MimeType: "application/pdf", Size: -1},
	}
	if len(as) != len(want) {
		t.Fatalf("got %+v", as)
	}
	for i := range want {
		if as[i] != want[i] {
			t.Fatalf("[%d] = %+v, want %+v", i, as[i], want[i])
		}
	}
	// A DM token reads the DM narrow.
	if _, err := lh.h.AttachmentsIn(journal.DM([]int64{humanID, botID}).Token()); err != nil {
		t.Fatal(err)
	}
	if n := lh.z.narrows[len(lh.z.narrows)-1]; len(n) == 0 || n[0].Operator != "dm" {
		t.Fatalf("narrow %+v", n)
	}
}

func TestAttachmentsInCapsAndErrors(t *testing.T) {
	lh := newLoopHarness(t, newAgent("ok"), nil)
	if _, err := lh.h.AttachmentsIn("bad"); err == nil {
		t.Fatal("bad token")
	}
	var b strings.Builder
	for i := 0; i < 10; i++ {
		b.WriteString("[f](/user_uploads/2/x/f" + string(rune('a'+i)) + ") ")
	}
	for i := 0; i < 6; i++ {
		lh.z.history = append(lh.z.history, zulipproto.Message{ID: int64(i), Content: strings.ReplaceAll(b.String(), "/x/", "/"+string(rune('a'+i))+"/")})
	}
	as, err := lh.h.AttachmentsIn(chanToken("t"))
	if err != nil || len(as) != maxListed {
		t.Fatalf("%d %v", len(as), err)
	}
	lh.z.historyErr = errors.New("down")
	if _, err := lh.h.AttachmentsIn(chanToken("t")); err == nil {
		t.Fatal("history error")
	}
}

func TestDownloadLinkFor(t *testing.T) {
	lh := newLoopHarness(t, newAgent("ok"), nil)
	lh.z.history = []zulipproto.Message{{ID: 1, Content: "[r](/user_uploads/2/ab/r.pdf) [s](/user_uploads/2/ab/s.bin)"}}
	lh.z.addUpload("/user_uploads/2/ab/r.pdf", "application/pdf; charset=binary", []byte("12345"))
	lh.z.addUpload("/user_uploads/2/ab/s.bin", "", []byte("1"))
	before := time.Now()
	tgt, err := lh.h.DownloadLinkFor(chanToken("t"), "2/ab/r.pdf")
	if err != nil {
		t.Fatal(err)
	}
	if tgt.URL != "https://zulip.example/user_uploads/temporary/tok/2/ab/r.pdf" || tgt.Size != 5 || tgt.Name != "r.pdf" || tgt.MimeType != "application/pdf" || tgt.ExpiresAt.Before(before) {
		t.Fatalf("%+v", tgt)
	}
	// An unparseable served type keeps the guess.
	tgt, err = lh.h.DownloadLinkFor(chanToken("t"), "2/ab/s.bin")
	if err != nil || tgt.MimeType != "application/octet-stream" {
		t.Fatalf("%+v %v", tgt, err)
	}
	if _, err := lh.h.DownloadLinkFor(chanToken("t"), "2/zz/other"); err == nil {
		t.Fatal("foreign id")
	}
	if _, err := lh.h.DownloadLinkFor("bad", "x"); err == nil {
		t.Fatal("bad token")
	}
	lh.z.tempBase = "http://zulip.example"
	if _, err := lh.h.DownloadLinkFor(chanToken("t"), "2/ab/r.pdf"); err == nil {
		t.Fatal("http link handed out")
	}
	lh.z.downloadErr = errors.New("gone")
	if _, err := lh.h.DownloadLinkFor(chanToken("t"), "2/ab/r.pdf"); err == nil {
		t.Fatal("temp url error")
	}
}

func TestUploads(t *testing.T) {
	lh := newLoopHarness(t, newAgent("ok"), nil)
	var stored []byte
	fs, err := fileshare.New(fileshare.Config{PublicURL: "https://relay.example", Store: func(_ context.Context, name, _ string, data []byte) (string, error) {
		stored = data
		return "/user_uploads/2/ff/" + name, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	u := NewUploads(func() *Handler { return lh.h }, fs)
	tok := chanToken("t")
	spec := mcphost.UploadSpec{Name: "../big.bin", MimeType: "application/octet-stream", Size: 3}
	if _, err := u.UploadTargetFor("bad", spec); err == nil {
		t.Fatal("bad token")
	}
	if _, err := u.UploadTargetFor(tok, mcphost.UploadSpec{}); err == nil {
		t.Fatal("zero size")
	}
	tgt, err := u.UploadTargetFor(tok, spec)
	if err != nil || tgt.Method != "PUT" || !strings.HasPrefix(tgt.URL, "https://relay.example/up/") || tgt.Ref == "" || tgt.ExpiresAt.IsZero() {
		t.Fatalf("%+v %v", tgt, err)
	}
	if _, err := u.FinishUploadFor(tok, spec, tgt.Ref); err == nil {
		t.Fatal("finished before upload")
	}
	putBody(t, fs, tgt.URL, "abc")
	f, err := u.FinishUploadFor(tok, spec, tgt.Ref)
	if err != nil || f.Ref != "/user_uploads/2/ff/big.bin" || f.Size != 3 || string(stored) != "abc" {
		t.Fatalf("%+v %v", f, err)
	}
	f.Name = "big.bin"
	if _, err := u.PostUploadedTo(tok, f, ""); err != nil {
		t.Fatal(err)
	}
	if b := lh.z.stored()[0]; b != "[big.bin](/user_uploads/2/ff/big.bin)" {
		t.Fatalf("%q", b)
	}
}

func putBody(t *testing.T, fs *fileshare.Server, url, body string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, url, strings.NewReader(body))
	w := httptest.NewRecorder()
	fs.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("PUT: %d %s", w.Code, w.Body)
	}
}

type fakeTargets struct {
	createErr error
	st        zulipproto.UploadTargetStatus
	stErr     error
	created   string
}

func (f *fakeTargets) CreateUploadTarget(_ context.Context, name string, size int64, mt string, ttl time.Duration) (zulipproto.UploadTarget, error) {
	f.created = fmt.Sprintf("%s:%d:%s:%v", name, size, mt, ttl)
	if f.createErr != nil {
		return zulipproto.UploadTarget{}, f.createErr
	}
	return zulipproto.UploadTarget{URL: "https://z.example/put/t1", Token: "t1", ExpiresAt: time.Unix(5, 0)}, nil
}

func (f *fakeTargets) GetUploadTarget(_ context.Context, token string) (zulipproto.UploadTargetStatus, error) {
	if token != "t1" {
		return zulipproto.UploadTargetStatus{}, errors.New("unknown")
	}
	return f.st, f.stErr
}

func TestZliteUploads(t *testing.T) {
	lh := newLoopHarness(t, newAgent("ok"), nil)
	api := &fakeTargets{}
	z := NewZliteUploads(func() *Handler { return lh.h }, api, time.Second)
	tok := chanToken("t")
	spec := mcphost.UploadSpec{Name: "dir/big.bin", MimeType: "x/y", Size: 3}
	if _, err := z.UploadTargetFor("bad", spec); err == nil {
		t.Fatal("bad token")
	}
	tgt, err := z.UploadTargetFor(tok, spec)
	if err != nil || tgt.URL != "https://z.example/put/t1" || tgt.Method != "PUT" || !tgt.ExpiresAt.Equal(time.Unix(5, 0)) || tgt.Ref != "t1" || api.created != "big.bin:3:x/y:10m0s" {
		t.Fatalf("%+v %v %s", tgt, err, api.created)
	}
	api.createErr = errors.New("no")
	if _, err := z.UploadTargetFor(tok, spec); err == nil {
		t.Fatal("create error")
	}
	if _, err := z.FinishUploadFor(tok, spec, "t1"); err == nil {
		t.Fatal("nothing stored")
	}
	api.st = zulipproto.UploadTargetStatus{Status: "done", Path: "https://evil/user_uploads/x"}
	if _, err := z.FinishUploadFor(tok, spec, "t1"); err == nil {
		t.Fatal("foreign path accepted")
	}
	api.stErr = errors.New("down")
	if _, err := z.FinishUploadFor(tok, spec, "t1"); err == nil {
		t.Fatal("status error")
	}
	api.stErr = nil
	api.st = zulipproto.UploadTargetStatus{Status: "done", Path: "/user_uploads/2/ab/big.bin", Size: 3}
	f, err := z.FinishUploadFor(tok, spec, "t1")
	if err != nil || f.Ref != "/user_uploads/2/ab/big.bin" || f.Size != 3 {
		t.Fatalf("%+v %v", f, err)
	}
	f.Name = "big.bin"
	if _, err := z.PostUploadedTo(tok, f, ""); err != nil {
		t.Fatal(err)
	}
	if b := lh.z.stored()[0]; b != "[big.bin](/user_uploads/2/ab/big.bin)" {
		t.Fatalf("%q", b)
	}
}
