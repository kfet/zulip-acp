// Package selfupdate implements `zulip-acp update`: resolve the latest
// (or a pinned) GitHub release for the current GOOS/GOARCH, verify its
// sha256 against the release's checksums.txt, and atomically replace the
// running binary.
//
// Design notes:
//
//   - Release assets are RAW binaries (goreleaser `formats: [binary]`),
//     named "zulip-acp-<os>-<arch>" — there is no archive to unpack, so
//     the downloaded asset IS the binary.
//   - Everything goes through the GitHub REST API, including the asset
//     bytes (`Accept: application/octet-stream` on the asset URL). The
//     plain releases/download/… URL 404s while kfet/zulip-acp is private
//     — which is also why `brew install` cannot serve this repo yet — so
//     the API path with a token is the only mechanism that works on a
//     fleet host today. It works unauthenticated on a public repo too,
//     so there is one code path rather than two.
//   - The download is staged in a temp dir NEXT TO the running binary
//     (same directory → same filesystem) so the final os.Rename is
//     atomic. This is the ETXTBSY-safe swap: a running executable cannot
//     be written or truncated in place, but renaming a sibling over it
//     replaces the directory entry while the live process keeps its old
//     inode mapped until it re-execs. It is the mechanism that makes the
//     hand-rolled "stage beside it and mv -f" dance unnecessary.
//   - Self-update is refused under a package-manager-owned prefix, and
//     refused when the install directory belongs to another user: both
//     are installs this process must not rewrite behind someone's back.
//     The user is told which command to use instead.
//
// The package is deliberately free of any Zulip- or relay-specific
// import: it is a promotion candidate for acp-kit, where slack-acp would
// pick it up (see BACKLOG.md). Only DefaultRepo, binaryName and the
// restart hint name this relay.
package selfupdate

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"syscall"
	"time"
)

// geteuid is a seam: the ownership refusal must be testable on a machine
// of any uid, including a root CI container.
var geteuid = os.Geteuid

// repoRe matches a github "owner/name": anything else would be pasted
// straight into the API path.
var repoRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

// DefaultRepo is the github "owner/name" releases are taken from.
const DefaultRepo = "kfet/zulip-acp"

// binaryName is both the release-asset stem and the installed file name.
const binaryName = "zulip-acp"

// defaultAPIBase is the GitHub REST API root.
const defaultAPIBase = "https://api.github.com"

// Options configures a single update run.
type Options struct {
	Repo       string // owner/repo (default kfet/zulip-acp)
	Version    string // explicit tag like "v0.18.0"; "" → latest
	CheckOnly  bool   // resolve + report, do not download or replace
	Token      string // GitHub token; "" → discovered from the environment
	APIBase    string // GitHub API root; "" → https://api.github.com
	HTTPClient *http.Client
	Stdout     io.Writer
	Stderr     io.Writer
	// ExecPath overrides os.Executable() — for tests.
	ExecPath func() (string, error)
}

// brewPaths are prefixes owned by Homebrew. A binary there is refused
// with the brew-specific hint; anything else not owned by us is refused
// by the ownership check below.
var brewPaths = []string{
	"/opt/homebrew/",
	"/usr/local/Cellar/",
	"/usr/local/Homebrew/",
	"/home/linuxbrew/",
}

// Result reports what an update run did, so the caller can decide
// follow-up actions such as recycling the service.
type Result struct {
	Current  string // current version (with leading v)
	Target   string // resolved target version (with leading v)
	Updated  bool   // true iff the binary was actually replaced
	ExecPath string // resolved path of the (replaced) binary
}

// release is the subset of the GitHub release JSON we use.
type release struct {
	TagName string `json:"tag_name"`
	Assets  []struct {
		Name string `json:"name"`
		URL  string `json:"url"`
	} `json:"assets"`
}

// Run performs the update according to opts. It returns a Result
// describing the outcome, and a non-nil error if the update failed. On
// "already up to date" or CheckOnly it returns Updated=false, err=nil.
func Run(currentVersion string, opts Options) (Result, error) {
	if opts.Repo == "" {
		opts.Repo = DefaultRepo
	}
	if !repoRe.MatchString(opts.Repo) {
		return Result{}, fmt.Errorf("bad repo %q: want owner/name", opts.Repo)
	}
	if opts.APIBase == "" {
		opts.APIBase = defaultAPIBase
	}
	if opts.HTTPClient == nil {
		// No total-request deadline: a release binary is ~10 MB and a Pi
		// on a slow link legitimately takes minutes. Bound the part that
		// can actually hang instead — a server that accepts the
		// connection and never answers.
		opts.HTTPClient = &http.Client{Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			ResponseHeaderTimeout: 30 * time.Second,
		}}
	}
	if opts.Stdout == nil {
		opts.Stdout = os.Stdout
	}
	if opts.ExecPath == nil {
		opts.ExecPath = os.Executable
	}
	var res Result

	exe, err := opts.ExecPath()
	if err != nil {
		return res, fmt.Errorf("locate self: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(exe)
	if err != nil {
		resolved = exe
	}
	res.ExecPath = resolved
	for _, p := range brewPaths {
		if strings.HasPrefix(resolved, p) {
			return res, fmt.Errorf("refusing to self-update: %s is a Homebrew install; run `brew upgrade %s` instead", resolved, binaryName)
		}
	}
	// The swap needs to create a temp file in the install directory and
	// rename over the binary, so a directory owned by someone else is a
	// managed install (a distro package, /usr/bin, a shared /opt tree) —
	// refuse it here, with the reason, rather than failing three network
	// round-trips later with "permission denied".
	if err := ownedByUs(filepath.Dir(resolved)); err != nil {
		return res, fmt.Errorf("refusing to self-update: %s: %w; use the package manager that installed it, or install %s under your own home", resolved, err, binaryName)
	}

	if opts.Token == "" {
		opts.Token = discoverToken()
	}

	// 1. Resolve the release (tag + asset URLs) in one API call.
	rel, err := fetchRelease(opts, opts.Version)
	if err != nil {
		return res, err
	}
	target := ensureV(rel.TagName)
	cur := ensureV(currentVersion)
	res.Current, res.Target = cur, target

	// 2. Compare with what is running. "target", not "latest": with
	// --version pinned this may be older than what is installed, and
	// calling a downgrade "latest" would be a lie.
	fmt.Fprintf(opts.Stdout, "current: %s\ntarget:  %s\n", cur, target)
	if target == cur {
		fmt.Fprintln(opts.Stdout, "already up to date.")
		return res, nil
	}
	if opts.CheckOnly {
		fmt.Fprintf(opts.Stdout, "update available: %s → %s. run `%s update` to install.\n", cur, target, binaryName)
		return res, nil
	}

	// 3. Download + verify.
	asset := fmt.Sprintf("%s-%s-%s", binaryName, runtime.GOOS, assetArch(runtime.GOARCH))
	binURL, err := assetURL(rel, asset)
	if err != nil {
		return res, err
	}
	sumsURL, err := assetURL(rel, "checksums.txt")
	if err != nil {
		return res, err
	}
	fmt.Fprintf(opts.Stdout, "downloading %s %s\n", asset, target)

	tmpDir, err := os.MkdirTemp(filepath.Dir(resolved), "."+binaryName+"-update-*")
	if err != nil {
		return res, fmt.Errorf("tempdir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	// The asset IS the binary; stage it executable so no separate chmod
	// is needed (rename preserves the mode).
	newBin := filepath.Join(tmpDir, asset)
	actual, err := download(opts, binURL, newBin, 0o755)
	if err != nil {
		return res, fmt.Errorf("download asset: %w", err)
	}
	sumsPath := filepath.Join(tmpDir, "checksums.txt")
	if _, err := download(opts, sumsURL, sumsPath, 0o644); err != nil {
		return res, fmt.Errorf("download checksums: %w", err)
	}
	expected, err := lookupChecksum(sumsPath, asset)
	if err != nil {
		return res, err
	}
	if actual != expected {
		return res, fmt.Errorf("checksum mismatch: expected %s, got %s", expected, actual)
	}
	fmt.Fprintln(opts.Stdout, "✓ checksum verified")

	// 4. Atomic rename. tmpDir is alongside resolved → same filesystem.
	if err := os.Rename(newBin, resolved); err != nil {
		return res, fmt.Errorf("replace: %w", err)
	}
	res.Updated = true
	fmt.Fprintf(opts.Stdout, "✓ %s updated: %s → %s at %s\n", binaryName, cur, target, resolved)
	return res, nil
}

// assetURL returns the API URL of the named asset in rel.
func assetURL(rel *release, name string) (string, error) {
	for _, a := range rel.Assets {
		if a.Name == name {
			return a.URL, nil
		}
	}
	return "", fmt.Errorf("release %s has no asset %q", rel.TagName, name)
}

// fetchRelease resolves a release through the GitHub API: the latest one
// when tag is empty, otherwise that exact tag.
func fetchRelease(opts Options, tag string) (*release, error) {
	u := opts.APIBase + "/repos/" + opts.Repo + "/releases/latest"
	if tag != "" {
		u = opts.APIBase + "/repos/" + opts.Repo + "/releases/tags/" + ensureV(tag)
	}
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("resolve release: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	authorize(req, opts.Token)
	resp, err := opts.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("resolve release: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// A private repo answers 404 to an unauthenticated caller, so the
		// advice differs entirely depending on whether we sent a token.
		hint := "a private repo needs GITHUB_TOKEN, GH_TOKEN or a logged-in `gh`"
		if opts.Token != "" {
			hint = "no such release, or the token cannot read " + opts.Repo
		}
		return nil, fmt.Errorf("resolve release: github api: %s (%s)", resp.Status, hint)
	}
	var rel release
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, fmt.Errorf("resolve release: %w", err)
	}
	if rel.TagName == "" {
		return nil, errors.New("resolve release: github api: empty tag_name")
	}
	return &rel, nil
}

// download GETs src as raw bytes, writes them to dst with the given file
// mode, and returns the hex sha256 of what was written (hashed inline,
// so the file we just wrote is never read back).
func download(opts Options, src, dst string, mode os.FileMode) (string, error) {
	req, err := http.NewRequest(http.MethodGet, src, nil)
	if err != nil {
		return "", err
	}
	// Asset URLs serve the release JSON unless octet-stream is asked for.
	req.Header.Set("Accept", "application/octet-stream")
	authorize(req, opts.Token)
	resp, err := opts.HTTPClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET %s: %s", src, resp.Status)
	}
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(f, h), resp.Body); err != nil {
		return "", err
	}
	// Best-effort durability hint. Integrity is already guaranteed by the
	// returned sha256 the caller checks against checksums.txt, and the
	// update is freely re-runnable, so we do not gate on it.
	_ = f.Sync()
	return hex.EncodeToString(h.Sum(nil)), nil
}

// authorize attaches a bearer token when one is available. Without it a
// private repo answers 404 to every request, asset downloads included.
func authorize(req *http.Request, token string) {
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
}

// discoverToken finds a GitHub token the way the surrounding tooling
// does: the standard environment variables first, then a logged-in `gh`.
// A fleet host has gh configured but rarely exports a token.
func discoverToken() string {
	for _, k := range []string{"GITHUB_TOKEN", "GH_TOKEN"} {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	out, err := exec.Command("gh", "auth", "token").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// ownedByUs reports whether dir belongs to the effective user, which is
// the precondition for staging a temp file there and renaming it over the
// binary.
func ownedByUs(dir string) error {
	fi, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("cannot stat install dir %s: %w", dir, err)
	}
	// Every platform in the cross-build matrix is unix, so the assertion
	// holds; a new GOOS that broke it should panic loudly here rather
	// than silently skip the ownership refusal.
	st := fi.Sys().(*syscall.Stat_t)
	if int(st.Uid) != geteuid() {
		return fmt.Errorf("install dir %s is owned by uid %d, not you (uid %d)", dir, st.Uid, geteuid())
	}
	return nil
}

func lookupChecksum(sumsPath, asset string) (string, error) {
	data, err := os.ReadFile(sumsPath)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[1] == asset {
			return fields[0], nil
		}
	}
	return "", fmt.Errorf("no checksum entry for %s", asset)
}

// ensureV prepends "v" to a version string that lacks it.
func ensureV(s string) string {
	if strings.HasPrefix(s, "v") {
		return s
	}
	return "v" + s
}

// assetArch maps runtime.GOARCH to the arch suffix in release asset
// names. GOARCH=arm (GOARM=6 in our matrix) publishes as "armv6".
func assetArch(goarch string) string {
	if goarch == "arm" {
		return "armv6"
	}
	return goarch
}
