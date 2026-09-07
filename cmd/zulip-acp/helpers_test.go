package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kfet/zulip-acp/internal/config"
	"github.com/kfet/zulip-acp/internal/skills"
	"github.com/kfet/zulip-acp/internal/zulipproto"
)

func swap[T any](p *T, v T) func() {
	old := *p
	*p = v
	return func() { *p = old }
}

func TestBuildSkillsCatalog(t *testing.T) {
	dir := t.TempDir()
	builtin, err := skills.LoadBuiltin()
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
	defer swap(&loadBuiltinSkills, func() ([]skills.Skill, error) {
		return nil, errors.New("builtin-fail")
	})()
	defer swap(&loadDirSkills, func(string) ([]skills.Skill, error) { return nil, nil })()

	// Builtin extraction failing must not cost us the operator prompt.
	got := systemPromptProvider("", &config.Config{SystemPrompt: "operator-extra"})()
	if !strings.Contains(got, "operator-extra") {
		t.Fatalf("operator prompt dropped on builtin failure: %s", got)
	}
	if strings.Contains(got, "<available_skills>") {
		t.Fatalf("catalog emitted despite failed load: %s", got)
	}
}

// The embedded bundle cannot change at runtime, and LoadBuiltin writes to
// $TMPDIR non-atomically, so it must run once per process — not once per
// session.
func TestSystemPromptProvider_LoadsBuiltinsOnce(t *testing.T) {
	calls := 0
	defer swap(&loadBuiltinSkills, func() ([]skills.Skill, error) {
		calls++
		return []skills.Skill{{Name: "b", Description: "builtin", Path: "/b"}}, nil
	})()

	provider := systemPromptProvider(filepath.Join(t.TempDir(), "config.json"), &config.Config{})
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
		&config.Config{SystemPrompt: "operator-extra"})

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
	defer swap(&loadBuiltinSkills, func() ([]skills.Skill, error) {
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

// fakeMoveProbe answers the one realm question resolveArchive asks.
type fakeMoveProbe struct {
	allowed bool
	err     error
	asked   int64
}

func (p *fakeMoveProbe) CanMoveMessagesBetweenChannels(_ context.Context, userID int64) (bool, error) {
	p.asked = userID
	return p.allowed, p.err
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
		name   string
		cfg    *config.Config
		probe  *fakeMoveProbe
		served fakeServed
		wantID int64
	}{
		{name: "on", cfg: &config.Config{}, probe: &fakeMoveProbe{allowed: true}, served: served, wantID: 12},
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
			id, name := resolveArchive(context.Background(), tc.cfg, tc.probe, avail, tc.served, 9)
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
