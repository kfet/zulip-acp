package selfupdate

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeRelease is a GitHub-shaped release server: one release tag, one
// binary asset for the current platform, and a checksums.txt over it.
type fakeRelease struct {
	tag      string
	body     []byte // asset bytes
	sums     string // checksums.txt contents; "" → generated over body
	omitBin  bool   // publish no binary asset
	omitSums bool   // publish no checksums.txt
	token    string // if set, requests must carry this bearer token
	srv      *httptest.Server
	requests int
}

func (f *fakeRelease) assetName() string {
	return fmt.Sprintf("%s-%s-%s", binaryName, runtime.GOOS, assetArch(runtime.GOARCH))
}

func (f *fakeRelease) start(t *testing.T) string {
	t.Helper()
	if f.tag == "" {
		f.tag = "v9.9.9"
	}
	if f.body == nil {
		f.body = []byte("#!/bin/sh\necho new\n")
	}
	if f.sums == "" {
		sum := sha256.Sum256(f.body)
		f.sums = fmt.Sprintf("%s  %s\n%s  LICENSE\n", hex.EncodeToString(sum[:]), f.assetName(), strings.Repeat("0", 64))
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		f.requests++
		if f.token != "" && r.Header.Get("Authorization") != "Bearer "+f.token {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/releases/latest"),
			strings.Contains(r.URL.Path, "/releases/tags/"):
			rel := map[string]any{"tag_name": f.tag, "assets": []map[string]string{}}
			var assets []map[string]string
			if !f.omitBin {
				assets = append(assets, map[string]string{
					"name": f.assetName(), "url": f.srv.URL + "/assets/bin",
				})
			}
			if !f.omitSums {
				assets = append(assets, map[string]string{
					"name": "checksums.txt", "url": f.srv.URL + "/assets/sums",
				})
			}
			rel["assets"] = assets
			_ = json.NewEncoder(w).Encode(rel)
		case r.URL.Path == "/assets/bin":
			_, _ = w.Write(f.body)
		case r.URL.Path == "/assets/sums":
			_, _ = w.Write([]byte(f.sums))
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f.srv.URL
}

// installed stages a fake installed binary and returns its path plus an
// ExecPath function reporting it.
func installed(t *testing.T, content string) (string, func() (string, error)) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, binaryName)
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	return path, func() (string, error) { return path, nil }
}

// noTokenEnv makes token discovery deterministic: no env vars, and an
// empty PATH so `gh` cannot be found.
func noTokenEnv(t *testing.T) {
	t.Helper()
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "")
	t.Setenv("PATH", "")
}

// shellEnv is noTokenEnv for tests that must still run `sh`: PATH keeps
// the system shell but is fronted by a stub `gh` that yields no token, so
// a developer's real credentials never reach the fake release server.
func shellEnv(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "")
	t.Setenv("PATH", dir+":/bin:/usr/bin")
}

func TestRunReplacesBinaryAtomically(t *testing.T) {
	noTokenEnv(t)
	f := &fakeRelease{tag: "v1.2.3", body: []byte("NEW BINARY")}
	base := f.start(t)
	path, execPath := installed(t, "OLD BINARY")

	var out strings.Builder
	res, err := Run("1.0.0", Options{APIBase: base, ExecPath: execPath, Stdout: &out})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Updated || res.Current != "v1.0.0" || res.Target != "v1.2.3" {
		t.Fatalf("unexpected result: %+v", res)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "NEW BINARY" {
		t.Fatalf("binary not replaced: %q", got)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o755 {
		t.Fatalf("mode = %v, want 0755", fi.Mode().Perm())
	}
	if !strings.Contains(out.String(), "checksum verified") {
		t.Fatalf("stdout = %q", out.String())
	}
	// No staging litter left beside the binary.
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("leftover files: %v", entries)
	}
}

func TestRunPinnedVersionAndAlreadyCurrent(t *testing.T) {
	noTokenEnv(t)
	f := &fakeRelease{tag: "v2.0.0"}
	base := f.start(t)
	_, execPath := installed(t, "OLD")

	var out strings.Builder
	// "2.0.0" without the v, and the running version also without it:
	// both are normalised before comparison.
	res, err := Run("2.0.0", Options{APIBase: base, Version: "2.0.0", ExecPath: execPath, Stdout: &out})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Updated {
		t.Fatal("must not update when already current")
	}
	if !strings.Contains(out.String(), "already up to date") {
		t.Fatalf("stdout = %q", out.String())
	}
}

func TestRunCheckOnly(t *testing.T) {
	noTokenEnv(t)
	f := &fakeRelease{tag: "v3.0.0"}
	base := f.start(t)
	path, execPath := installed(t, "OLD")

	var out strings.Builder
	res, err := Run("v1.0.0", Options{APIBase: base, CheckOnly: true, ExecPath: execPath, Stdout: &out})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Updated {
		t.Fatal("check must not install")
	}
	if !strings.Contains(out.String(), "update available") {
		t.Fatalf("stdout = %q", out.String())
	}
	got, _ := os.ReadFile(path)
	if string(got) != "OLD" {
		t.Fatalf("check must leave the binary alone, got %q", got)
	}
}

func TestRunRefusesBrewInstall(t *testing.T) {
	noTokenEnv(t)
	for _, p := range []string{"/opt/homebrew/bin/zulip-acp", "/home/linuxbrew/.linuxbrew/bin/zulip-acp"} {
		_, err := Run("v1.0.0", Options{
			ExecPath: func() (string, error) { return p, nil },
			Stdout:   &strings.Builder{},
		})
		if err == nil || !strings.Contains(err.Error(), "brew upgrade zulip-acp") {
			t.Fatalf("%s: err = %v, want a brew refusal naming the fix", p, err)
		}
	}
}

// The swap stages a temp file next to the binary and renames it over, so
// an install directory owned by someone else is a managed install. It is
// refused up front — with the reason — not three round-trips later.
func TestRunRefusesDirectoryOwnedByAnotherUser(t *testing.T) {
	noTokenEnv(t)
	dir := t.TempDir()
	orig := geteuid
	geteuid = func() int { return orig() + 1 } // never the owner of dir
	t.Cleanup(func() { geteuid = orig })

	_, err := Run("v1.0.0", Options{
		ExecPath: func() (string, error) { return filepath.Join(dir, binaryName), nil },
		Stdout:   &strings.Builder{},
	})
	if err == nil || !strings.Contains(err.Error(), "not you") {
		t.Fatalf("err = %v, want an ownership refusal", err)
	}
}

func TestRunRefusesVanishedInstallDir(t *testing.T) {
	noTokenEnv(t)
	_, err := Run("v1.0.0", Options{
		ExecPath: func() (string, error) {
			return filepath.Join(t.TempDir(), "gone", binaryName), nil
		},
		Stdout: &strings.Builder{},
	})
	if err == nil || !strings.Contains(err.Error(), "cannot stat install dir") {
		t.Fatalf("err = %v", err)
	}
}

func TestRunRejectsBadRepo(t *testing.T) {
	_, err := Run("v1.0.0", Options{Repo: "kfet/zulip-acp/../../evil", Stdout: &strings.Builder{}})
	if err == nil || !strings.Contains(err.Error(), "want owner/name") {
		t.Fatalf("err = %v", err)
	}
}

func TestRunPrivateRepoNeedsToken(t *testing.T) {
	noTokenEnv(t)
	f := &fakeRelease{tag: "v4.0.0", token: "s3cret"}
	base := f.start(t)
	_, execPath := installed(t, "OLD")

	// Without the token the API answers 404, exactly as a private repo
	// does — and the error must point at the fix.
	_, err := Run("v1.0.0", Options{APIBase: base, ExecPath: execPath, Stdout: &strings.Builder{}})
	if err == nil || !strings.Contains(err.Error(), "GITHUB_TOKEN") {
		t.Fatalf("err = %v, want a token hint", err)
	}

	// A token that cannot see the release gets different advice: telling
	// someone to set a token they already set is a dead end.
	_, err = Run("v1.0.0", Options{APIBase: base, Token: "wrong", ExecPath: execPath, Stdout: &strings.Builder{}})
	if err == nil || !strings.Contains(err.Error(), "cannot read") {
		t.Fatalf("err = %v, want a token-scope hint", err)
	}

	// With the right one, the same private release installs.
	res, err := Run("v1.0.0", Options{APIBase: base, Token: "s3cret", ExecPath: execPath, Stdout: &strings.Builder{}})
	if err != nil || !res.Updated {
		t.Fatalf("Run with token: res=%+v err=%v", res, err)
	}
}

func TestRunTokenFromEnvironment(t *testing.T) {
	f := &fakeRelease{tag: "v4.1.0", token: "envtoken"}
	base := f.start(t)
	_, execPath := installed(t, "OLD")

	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "envtoken")
	res, err := Run("v1.0.0", Options{APIBase: base, ExecPath: execPath, Stdout: &strings.Builder{}})
	if err != nil || !res.Updated {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

func TestDiscoverToken(t *testing.T) {
	t.Run("env wins", func(t *testing.T) {
		t.Setenv("GITHUB_TOKEN", "from-env")
		if got := discoverToken(); got != "from-env" {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("gh cli", func(t *testing.T) {
		dir := t.TempDir()
		gh := filepath.Join(dir, "gh")
		if err := os.WriteFile(gh, []byte("#!/bin/sh\necho from-gh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("GITHUB_TOKEN", "")
		t.Setenv("GH_TOKEN", "")
		t.Setenv("PATH", dir)
		if got := discoverToken(); got != "from-gh" {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("nothing", func(t *testing.T) {
		noTokenEnv(t)
		if got := discoverToken(); got != "" {
			t.Fatalf("got %q", got)
		}
	})
}

func TestRunChecksumMismatch(t *testing.T) {
	noTokenEnv(t)
	f := &fakeRelease{tag: "v5.0.0", body: []byte("NEW")}
	f.sums = strings.Repeat("a", 64) + "  " + f.assetName() + "\n"
	base := f.start(t)
	path, execPath := installed(t, "OLD")

	_, err := Run("v1.0.0", Options{APIBase: base, ExecPath: execPath, Stdout: &strings.Builder{}})
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("err = %v", err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "OLD" {
		t.Fatal("a failed checksum must leave the installed binary untouched")
	}
}

func TestRunErrors(t *testing.T) {
	noTokenEnv(t)
	_, execPath := installed(t, "OLD")

	t.Run("exec path unknown", func(t *testing.T) {
		_, err := Run("v1", Options{ExecPath: func() (string, error) { return "", os.ErrNotExist }})
		if err == nil || !strings.Contains(err.Error(), "locate self") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("no binary asset", func(t *testing.T) {
		f := &fakeRelease{tag: "v6.0.0", omitBin: true}
		_, err := Run("v1", Options{APIBase: f.start(t), ExecPath: execPath, Stdout: &strings.Builder{}})
		if err == nil || !strings.Contains(err.Error(), "has no asset") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("no checksums asset", func(t *testing.T) {
		f := &fakeRelease{tag: "v6.1.0", omitSums: true}
		_, err := Run("v1", Options{APIBase: f.start(t), ExecPath: execPath, Stdout: &strings.Builder{}})
		if err == nil || !strings.Contains(err.Error(), "checksums.txt") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("no checksum line for our asset", func(t *testing.T) {
		f := &fakeRelease{tag: "v6.2.0", sums: strings.Repeat("0", 64) + "  LICENSE\n"}
		_, err := Run("v1", Options{APIBase: f.start(t), ExecPath: execPath, Stdout: &strings.Builder{}})
		if err == nil || !strings.Contains(err.Error(), "no checksum entry") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("api unreachable", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		url := srv.URL
		srv.Close()
		_, err := Run("v1", Options{APIBase: url, ExecPath: execPath, Stdout: &strings.Builder{}})
		if err == nil || !strings.Contains(err.Error(), "resolve release") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("bad api base", func(t *testing.T) {
		_, err := Run("v1", Options{APIBase: "http://\x7f/", ExecPath: execPath, Stdout: &strings.Builder{}})
		if err == nil || !strings.Contains(err.Error(), "resolve release") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("malformed release json", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("{{{"))
		}))
		t.Cleanup(srv.Close)
		_, err := Run("v1", Options{APIBase: srv.URL, ExecPath: execPath, Stdout: &strings.Builder{}})
		if err == nil || !strings.Contains(err.Error(), "resolve release") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("empty tag", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"assets":[]}`))
		}))
		t.Cleanup(srv.Close)
		_, err := Run("v1", Options{APIBase: srv.URL, ExecPath: execPath, Stdout: &strings.Builder{}})
		if err == nil || !strings.Contains(err.Error(), "empty tag_name") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("staging dir uncreatable", func(t *testing.T) {
		// The install directory exists but is read-only, so the sibling
		// temp dir the atomic rename needs cannot be made. Root ignores
		// mode bits, so there is nothing to assert there.
		if os.Geteuid() == 0 {
			t.Skip("root ignores directory permissions")
		}
		dir := t.TempDir()
		if err := os.Chmod(dir, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
		f := &fakeRelease{tag: "v6.3.0"}
		_, err := Run("v1", Options{
			APIBase:  f.start(t),
			ExecPath: func() (string, error) { return filepath.Join(dir, binaryName), nil },
			Stdout:   &strings.Builder{},
		})
		if err == nil || !strings.Contains(err.Error(), "tempdir") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("asset download fails", func(t *testing.T) {
		f := &fakeRelease{tag: "v6.4.0"}
		// A release whose asset URL 404s — what a private repo looks
		// like when the token can read metadata but not the asset.
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/releases/latest") {
				_, _ = fmt.Fprintf(w, `{"tag_name":"v6.4.0","assets":[{"name":%q,"url":"%s/nope"},{"name":"checksums.txt","url":"%s/nope"}]}`,
					f.assetName(), "http://"+r.Host, "http://"+r.Host)
				return
			}
			http.Error(w, "gone", http.StatusNotFound)
		}))
		t.Cleanup(srv.Close)
		_, err := Run("v1", Options{APIBase: srv.URL, ExecPath: execPath, Stdout: &strings.Builder{}})
		if err == nil || !strings.Contains(err.Error(), "download asset") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("checksums download fails", func(t *testing.T) {
		f := &fakeRelease{tag: "v6.5.0"}
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.HasSuffix(r.URL.Path, "/releases/latest"):
				_, _ = fmt.Fprintf(w, `{"tag_name":"v6.5.0","assets":[{"name":%q,"url":"%s/bin"},{"name":"checksums.txt","url":"%s/nope"}]}`,
					f.assetName(), "http://"+r.Host, "http://"+r.Host)
			case r.URL.Path == "/bin":
				_, _ = w.Write([]byte("NEW"))
			default:
				http.Error(w, "gone", http.StatusNotFound)
			}
		}))
		t.Cleanup(srv.Close)
		_, err := Run("v1", Options{APIBase: srv.URL, ExecPath: execPath, Stdout: &strings.Builder{}})
		if err == nil || !strings.Contains(err.Error(), "download checksums") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("rename fails", func(t *testing.T) {
		// A directory where the binary should be: the swap cannot happen,
		// and the error must be reported rather than silently ignored.
		dir := t.TempDir()
		target := filepath.Join(dir, binaryName)
		if err := os.Mkdir(target, 0o755); err != nil {
			t.Fatal(err)
		}
		f := &fakeRelease{tag: "v6.6.0"}
		_, err := Run("v1", Options{
			APIBase:  f.start(t),
			ExecPath: func() (string, error) { return target, nil },
			Stdout:   &strings.Builder{},
		})
		if err == nil || !strings.Contains(err.Error(), "replace") {
			t.Fatalf("err = %v", err)
		}
	})
}

// A symlinked install resolves to its target: the swap must replace the
// real file, and the refusal list must be checked against the resolved
// path (a ~/.local/bin symlink into /usr/local is a managed install).
func TestRunFollowsSymlinkAndToleratesAFreshPath(t *testing.T) {
	noTokenEnv(t)
	dir := t.TempDir()
	real := filepath.Join(dir, "zulip-acp-real")
	if err := os.WriteFile(real, []byte("OLD"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	f := &fakeRelease{tag: "v7.0.0", body: []byte("NEW")}
	res, err := Run("v1", Options{
		APIBase:  f.start(t),
		ExecPath: func() (string, error) { return link, nil },
		Stdout:   &strings.Builder{},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.ExecPath != real {
		t.Fatalf("ExecPath = %q, want the resolved target %q", res.ExecPath, real)
	}
	got, _ := os.ReadFile(real)
	if string(got) != "NEW" {
		t.Fatalf("symlink target not replaced: %q", got)
	}

	// An unresolvable path falls back to the reported one rather than
	// aborting: a fresh install is created, not resolved.
	fresh := filepath.Join(dir, "fresh")
	f2 := &fakeRelease{tag: "v7.1.0", body: []byte("FRESH")}
	res, err = Run("v1", Options{
		APIBase:  f2.start(t),
		ExecPath: func() (string, error) { return fresh, nil },
		Stdout:   &strings.Builder{},
	})
	if err != nil || !res.Updated || res.ExecPath != fresh {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

func TestDownloadWriteError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("x"))
	}))
	t.Cleanup(srv.Close)
	opts := Options{HTTPClient: srv.Client()}
	_, err := download(opts, srv.URL, filepath.Join(t.TempDir(), "no", "such", "file"), 0o644)
	if err == nil {
		t.Fatal("want an open error")
	}
	if _, err := download(opts, "http://\x7f/", filepath.Join(t.TempDir(), "x"), 0o644); err == nil {
		t.Fatal("want a request-construction error")
	}
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()
	if _, err := download(opts, deadURL, filepath.Join(t.TempDir(), "x"), 0o644); err == nil {
		t.Fatal("want a transport error")
	}
}

func TestDownloadNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "gone", http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	_, err := download(Options{HTTPClient: srv.Client()}, srv.URL, filepath.Join(t.TempDir(), "x"), 0o644)
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("err = %v", err)
	}
}

func TestDownloadTruncatedBody(t *testing.T) {
	// Hijack the connection and hang up mid-body: the client sees a
	// short read, which must fail the download rather than install a
	// truncated binary.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		conn, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		_, _ = buf.WriteString("HTTP/1.1 200 OK\r\nContent-Length: 100\r\n\r\nshort")
		_ = buf.Flush()
		_ = conn.Close()
	}))
	t.Cleanup(srv.Close)
	_, err := download(Options{HTTPClient: srv.Client()}, srv.URL, filepath.Join(t.TempDir(), "x"), 0o644)
	if err == nil {
		t.Fatal("a truncated body must not pass as a download")
	}
}

func TestLookupChecksumMissingFile(t *testing.T) {
	if _, err := lookupChecksum(filepath.Join(t.TempDir(), "nope"), "x"); err == nil {
		t.Fatal("want an error")
	}
}

func TestAssetArch(t *testing.T) {
	if got := assetArch("arm"); got != "armv6" {
		t.Fatalf("arm → %q", got)
	}
	if got := assetArch("amd64"); got != "amd64" {
		t.Fatalf("amd64 → %q", got)
	}
}

// Production calls Run with no writers and no ExecPath override. Drive
// that path on an "already current" release, which returns before it can
// touch the (real) running binary.
func TestRunDefaults(t *testing.T) {
	noTokenEnv(t)
	f := &fakeRelease{tag: "v9.5.0"}
	if _, err := Run("v9.5.0", Options{APIBase: f.start(t)}); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestRunDefaultsUseTheRealRepo(t *testing.T) {
	// Defaulting must not silently point at some other repo: assert the
	// constant rather than reaching the network.
	if DefaultRepo != "kfet/zulip-acp" {
		t.Fatalf("DefaultRepo = %q", DefaultRepo)
	}
}
