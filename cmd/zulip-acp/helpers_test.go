package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	kit "github.com/kfet/acp-kit/sysprompt"
	"github.com/kfet/acp-kit/update"
	"github.com/kfet/zulip-acp/internal/config"
	"github.com/kfet/zulip-acp/internal/skills"
	"github.com/kfet/zulip-acp/internal/sysprompt"
	"github.com/kfet/zulip-acp/internal/zulipproto"
)

func swap[T any](p *T, v T) func() {
	old := *p
	*p = v
	return func() { *p = old }
}

func TestBuildSkillsCatalog(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TMPDIR", t.TempDir()) // contain the legacy-location sweep
	builtin, err := skills.LoadBuiltin(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	// No host skills dir -> builtin bundle only.
	cat := buildSkillsCatalog(builtin, filepath.Join(dir, "skills"))
	if !strings.Contains(cat, "<available_skills>") {
		t.Fatalf("missing block: %s", cat)
	}
	if !strings.Contains(cat, "notes") {
		t.Fatalf("builtin notes skill missing: %s", cat)
	}
	// The moved update skill only ships if its frontmatter kept
	// `builtin: true`.
	if !strings.Contains(cat, "update") {
		t.Fatalf("builtin update skill missing: %s", cat)
	}

	// A host skill is merged in.
	writeSkill(t, dir, "extra", "host one")
	cat = buildSkillsCatalog(builtin, filepath.Join(dir, "skills"))
	if !strings.Contains(cat, "host one") {
		t.Fatalf("host skill missing: %s", cat)
	}
}

// A host skill named after a builtin replaces it: that is the documented
// way to disable a builtin.
func TestBuildSkillsCatalog_HostOverridesBuiltin(t *testing.T) {
	dir := t.TempDir()
	builtin := []skills.Skill{{Name: "notes", Description: "builtin one", Path: "/b/SKILL.md"}}
	writeSkill(t, dir, "notes", "host wins")

	cat := buildSkillsCatalog(builtin, filepath.Join(dir, "skills"))
	if strings.Contains(cat, "builtin one") {
		t.Fatalf("builtin not overridden: %s", cat)
	}
	if !strings.Contains(cat, "host wins") {
		t.Fatalf("host override missing: %s", cat)
	}
}

func TestBuildSkillsCatalog_HostLoaderError(t *testing.T) {
	defer swap(&loadDirSkills, func(string) ([]skills.Skill, error) {
		return nil, errors.New("host-fail")
	})()
	// Host dir unreadable and no builtins -> empty catalog, no panic.
	if got := buildSkillsCatalog(nil, t.TempDir()); got != "" {
		t.Fatalf("expected empty catalog, got %q", got)
	}
}

func TestSystemPromptProvider_BuiltinLoaderError(t *testing.T) {
	defer swap(&loadBuiltinSkills, func(string) ([]skills.Skill, error) {
		return nil, errors.New("builtin-fail")
	})()
	defer swap(&loadDirSkills, func(string) ([]skills.Skill, error) { return nil, nil })()

	// Builtin extraction failing must not cost us the operator prompt.
	got := systemPromptProvider("", &config.Config{SystemPrompt: "operator-extra", StateDir: t.TempDir()})()
	if !strings.Contains(got, "operator-extra") {
		t.Fatalf("operator prompt dropped on builtin failure: %s", got)
	}
	if strings.Contains(got, "<available_skills>") {
		t.Fatalf("catalog emitted despite failed load: %s", got)
	}
}

// The embedded bundle cannot change at runtime, and LoadBuiltin writes
// under the state dir non-atomically, so it must run once per process —
// not once per session.
func TestSystemPromptProvider_LoadsBuiltinsOnce(t *testing.T) {
	calls := 0
	defer swap(&loadBuiltinSkills, func(string) ([]skills.Skill, error) {
		calls++
		return []skills.Skill{{Name: "b", Description: "builtin", Path: "/b"}}, nil
	})()

	provider := systemPromptProvider(filepath.Join(t.TempDir(), "config.json"), &config.Config{StateDir: t.TempDir()})
	provider()
	provider()
	if calls != 1 {
		t.Fatalf("LoadBuiltin called %d times, want 1", calls)
	}
}

// A skill dropped in after startup must appear without a relay restart:
// that is the whole reason the prompt is a provider and not a string.
func TestSystemPromptProviderSeesHostSkillsAddedAfterStartup(t *testing.T) {
	dir := t.TempDir()
	provider := systemPromptProvider(filepath.Join(dir, "config.json"),
		&config.Config{SystemPrompt: "operator-extra", StateDir: t.TempDir()})

	if got := provider(); strings.Contains(got, "host later") {
		t.Fatalf("host skill appeared before it existed: %s", got)
	}

	writeSkill(t, dir, "later", "host later")
	got := provider()
	if !strings.Contains(got, "host later") {
		t.Fatalf("host skill added after startup not picked up: %s", got)
	}
	if !strings.Contains(got, "operator-extra") {
		t.Fatalf("operator system_prompt dropped: %s", got)
	}
}

// disable_system_prompt must suppress everything and never read the
// skill dirs at all.
func TestSystemPromptProvider_Disabled(t *testing.T) {
	defer swap(&loadBuiltinSkills, func(string) ([]skills.Skill, error) {
		t.Fatal("LoadBuiltin called while disabled")
		return nil, nil
	})()
	defer swap(&loadDirSkills, func(string) ([]skills.Skill, error) {
		t.Fatal("LoadDir called while disabled")
		return nil, nil
	})()

	provider := systemPromptProvider("", &config.Config{
		SystemPrompt:        "operator-extra",
		DisableSystemPrompt: true,
	})
	if got := provider(); got != "" {
		t.Fatalf("disable_system_prompt should suppress everything, got %q", got)
	}
}

func writeSkill(t *testing.T, cfgDir, name, desc string) {
	t.Helper()
	d := filepath.Join(cfgDir, "skills", name)
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	body := []byte("---\nname: " + name + "\ndescription: " + desc + "\n---\n")
	if err := os.WriteFile(filepath.Join(d, "SKILL.md"), body, 0o644); err != nil {
		t.Fatal(err)
	}
}

// fakeMoveProbe answers the realm questions resolveArchive asks.
type fakeMoveProbe struct {
	allowed bool
	limit   time.Duration
	err     error
	asked   int64
}

func (p *fakeMoveProbe) ChannelMovePolicy(_ context.Context, user zulipproto.User) (zulipproto.MovePolicy, error) {
	p.asked = user.UserID
	return zulipproto.MovePolicy{Allowed: p.allowed, Limit: p.limit}, p.err
}

// fakeServed is the served-channel allowlist.
type fakeServed map[int64]string

func (s fakeServed) Name(id int64) (string, bool) { n, ok := s[id]; return n, ok }

// TestResolveArchive: the control is enabled ONLY when the destination
// exists, is outside the served set, and realm policy lets the bot move
// messages between channels. Every other answer is off — a destructive
// gesture must not be offered where it cannot work.
func TestResolveArchive(t *testing.T) {
	streams := []zulipproto.Stream{
		{StreamID: 4, Name: "fleet"},
		{StreamID: 12, Name: "archive"},
	}
	served := fakeServed{4: "fleet"}
	off, servedName := "", "fleet"
	cases := []struct {
		name      string
		cfg       *config.Config
		probe     *fakeMoveProbe
		served    fakeServed
		wantID    int64
		wantLimit time.Duration
	}{
		{name: "on", cfg: &config.Config{}, probe: &fakeMoveProbe{allowed: true}, served: served, wantID: 12},
		{name: "on, but time-limited", cfg: &config.Config{}, probe: &fakeMoveProbe{allowed: true, limit: 7 * 24 * time.Hour}, served: served, wantID: 12, wantLimit: 7 * 24 * time.Hour},
		{name: "disabled in config", cfg: &config.Config{ArchiveChannel: &off}, probe: &fakeMoveProbe{allowed: true}, served: served},
		{name: "no such channel", cfg: &config.Config{}, probe: &fakeMoveProbe{allowed: true}, served: served},
		{name: "the destination is served", cfg: &config.Config{ArchiveChannel: &servedName}, probe: &fakeMoveProbe{allowed: true}, served: served},
		{name: "realm policy says no", cfg: &config.Config{}, probe: &fakeMoveProbe{}, served: served},
		{name: "cannot tell", cfg: &config.Config{}, probe: &fakeMoveProbe{err: errors.New("too old")}, served: served},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			avail := streams
			if tc.name == "no such channel" {
				avail = streams[:1]
			}
			id, name, limit := resolveArchive(context.Background(), tc.cfg, tc.probe, avail, tc.served, zulipproto.User{UserID: 9})
			if limit != tc.wantLimit {
				t.Fatalf("limit = %s, want %s", limit, tc.wantLimit)
			}
			if id != tc.wantID {
				t.Fatalf("id = %d, want %d", id, tc.wantID)
			}
			if (name != "") != (tc.wantID != 0) {
				t.Fatalf("name = %q with id %d", name, id)
			}
			if tc.wantID != 0 && tc.probe.asked != 9 {
				t.Fatalf("the permission check asked about user %d", tc.probe.asked)
			}
		})
	}
}

// fakeTypingProbe answers the one realm question
// resolveTypingInterval asks.
type fakeTypingProbe struct {
	expiry time.Duration
	err    error
	asked  int
}

func (p *fakeTypingProbe) TypingStartedExpiry(context.Context) (time.Duration, error) {
	p.asked++
	return p.expiry, p.err
}

// TestResolveTypingInterval: streaming mode never probes (it has a
// placeholder and sends no typing at all), quiet mode follows the
// realm, and a probe that fails degrades to the stock cadence rather
// than losing liveness.
func TestResolveTypingInterval(t *testing.T) {
	cases := []struct {
		name  string
		quiet bool
		probe *fakeTypingProbe
		want  time.Duration
		asked int
	}{
		{name: "streaming", quiet: false, probe: &fakeTypingProbe{expiry: 15 * time.Second}, want: 0, asked: 0},
		{name: "quiet", quiet: true, probe: &fakeTypingProbe{expiry: 30 * time.Second}, want: 20 * time.Second, asked: 1},
		{name: "probe failed", quiet: true, probe: &fakeTypingProbe{err: errors.New("nope")}, want: 10 * time.Second, asked: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveTypingInterval(context.Background(), tc.probe, tc.quiet)
			if got != tc.want {
				t.Fatalf("interval = %s, want %s", got, tc.want)
			}
			if tc.probe.asked != tc.asked {
				t.Fatalf("probed %d times, want %d", tc.probe.asked, tc.asked)
			}
		})
	}
}

// The watchdog note must reach the agent, rendered from the configured
// window — not from a copy of the default living in this repo.
func TestSystemPromptProvider_CarriesLivenessNote(t *testing.T) {
	defer swap(&loadBuiltinSkills, func(string) ([]skills.Skill, error) { return nil, nil })()
	defer swap(&loadDirSkills, func(string) ([]skills.Skill, error) { return nil, nil })()

	got := systemPromptProvider("", &config.Config{
		StateDir:                 t.TempDir(),
		NoProgressTimeoutSeconds: 300,
	})()
	want := kit.LivenessNote(5 * time.Minute)
	if !strings.HasSuffix(got, "\n\n"+want) {
		t.Fatalf("liveness note missing or misjoined:\n%s", got)
	}
	if strings.Contains(sysprompt.Base, "Turn watchdog") {
		t.Fatal("the Zulip block must not carry the watchdog text itself")
	}
}

func TestChildPID(t *testing.T) {
	root := t.TempDir()
	mk := func(p string) { os.MkdirAll(filepath.Join(root, p), 0o755) }
	mk("10/task/10")
	mk("10/task/11")
	os.WriteFile(filepath.Join(root, "10/task/10/children"), []byte("20 21"), 0o644)
	os.WriteFile(filepath.Join(root, "10/task/11/children"), []byte("22\n"), 0o644)
	for pid, exe := range map[string]string{"20": "/bin/other", "22": "/opt/fir (deleted)"} {
		mk(pid)
		os.Symlink(exe, filepath.Join(root, pid, "exe"))
	}
	if got := childPID(root, 10, "/opt/fir"); got != 22 {
		t.Fatal(got)
	}
	if childPID(root, 10, "/nope") != 0 || childPID(root, 10, "") != 0 {
		t.Fatal("want 0")
	}
}

func TestResolveBin(t *testing.T) {
	if resolveBin("definitely-not-a-binary-xyz") != "" {
		t.Fatal("want empty")
	}
	if p := resolveBin("sh"); !filepath.IsAbs(p) {
		t.Fatal(p)
	}
}

func TestConvergeWrap(t *testing.T) {
	env := func(v string) func(string) string { return func(string) string { return v } }
	found := func(string) string { return "/usr/bin/systemd-run" }
	missing := func(string) string { return "" }
	if w := convergeWrap(env(""), found); w != nil {
		t.Fatal("not under systemd:", w)
	}
	if w := convergeWrap(env("abc"), missing); w != nil {
		t.Fatal("no systemd-run:", w)
	}
	if w := convergeWrap(env("abc"), found); strings.Join(w, " ") != "/usr/bin/systemd-run --user --scope --quiet --collect" {
		t.Fatal(w)
	}
}

func TestNewUpdater(t *testing.T) {
	cfg := &config.Config{StateDir: t.TempDir()}
	if newUpdater(cfg, "1", "", nil, nil, nil) != nil {
		t.Fatal("no owners must disable")
	}
	cfg.UpdateOwnerIDs = []int64{7}
	idle := func(context.Context) error { return errors.New("busy") }
	fir := filepath.Join(t.TempDir(), "fir")
	os.WriteFile(fir, []byte("#!/bin/sh\necho fir 1.0\n"), 0o755)
	u := newUpdater(cfg, "1", fir, func() string { return "" }, func() []string { return nil }, idle)
	if u == nil || newUpdater(cfg, "1", "", nil, nil, nil) == nil {
		t.Fatal("want updater")
	}
	// --force exercises CancelAll/WaitIdle; --check exercises AgentPID.
	cfg.UpdateOwnerIDs = []int64{7}
	if r := u.Handle(context.Background(), update.Request{Requester: "7", Text: "!update --check"}); !strings.Contains(r.Text, "zulip-acp") {
		t.Fatal(r.Text)
	}
	if r := u.Handle(context.Background(), update.Request{Requester: "7", Text: "!update fir --force"}); !strings.Contains(r.Text, "NOT reloading") {
		t.Fatal("empty")
	}
}

func TestAgentBinFor(t *testing.T) {
	if agentBinFor(nil) != "" || agentBinFor([]string{"ssh", "host", "fir"}) != "" {
		t.Fatal("non-fir must be empty")
	}
	agentBinFor([]string{"fir", "--mode", "acp"}) // resolves if installed; must not panic
}
