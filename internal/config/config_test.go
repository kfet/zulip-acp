package config

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kfet/acp-kit/statusline"
	"github.com/kfet/zulip-acp/internal/rollover"
	"github.com/kfet/zulip-acp/internal/zulipproto"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return p
}

func TestLoad(t *testing.T) {
	p := writeConfig(t, `{
	  "site": "https://zulip.example",
	  "bot_email": "bot@zulip.example",
	  "bot_api_key": "secret",
	  "channels": ["fleet", "7"],
	  "allowed_user_ids": [8, 9],
	  "hide_thinking": true,
	  "max_message_chars": 8000,
	  "edit_interval_ms": 250,
	  "prompt_timeout_seconds": 60,
	  "session_idle_timeout_seconds": 120
	}`)
	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Site != "https://zulip.example" || !c.HideThinking || c.Budget() != 8000 {
		t.Fatalf("config = %+v", c)
	}
	if c.EditInterval() != 250*time.Millisecond || c.PromptTimeout() != time.Minute ||
		c.IdleTimeout() != 2*time.Minute {
		t.Fatalf("durations = %s %s %s", c.EditInterval(), c.PromptTimeout(), c.IdleTimeout())
	}
	users := c.AllowedUsers()
	if len(users) != 2 {
		t.Fatalf("allowed users = %v", users)
	}
	if _, ok := users[9]; !ok {
		t.Fatal("user 9 missing from allowlist")
	}
}

func TestLoadErrors(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "absent.json")); err == nil {
		t.Fatal("want read error")
	}
	if _, err := Load(writeConfig(t, `{ not json`)); err == nil {
		t.Fatal("want parse error")
	}
	// A typo in a key must be an error, never silently ignored.
	if _, err := Load(writeConfig(t, `{"hide_thinkng": true}`)); err == nil {
		t.Fatal("want unknown-field error")
	}
	// Validation runs as part of Load.
	if _, err := Load(writeConfig(t, `{"edit_interval_ms": -1}`)); err == nil {
		t.Fatal("want validation error")
	}
}

func TestValidate(t *testing.T) {
	bad := []struct {
		name string
		cfg  Config
	}{
		{"idle", Config{SessionIdleTimeoutSeconds: -1}},
		{"prompt", Config{PromptTimeoutSeconds: -1}},
		{"edit", Config{EditIntervalMs: -1}},
		{"budget negative", Config{MaxMessageChars: -1}},
		{"budget over Zulip's limit", Config{MaxMessageChars: zulipproto.MaxMessageLength + 1}},
		{"empty channel", Config{Channels: []string{"fleet", "  "}}},
		{"markers exceed budget", Config{MaxMessageChars: 100, SealMarker: strings.Repeat("s", 90)}},
	}
	for _, c := range bad {
		if err := c.cfg.Validate(); err == nil {
			t.Fatalf("%s: want error", c.name)
		}
	}
	if err := (&Config{}).Validate(); err != nil {
		t.Fatalf("zero config must validate: %v", err)
	}
	// The budget ceiling error must name the silent-truncation risk.
	err := (&Config{MaxMessageChars: zulipproto.MaxMessageLength + 1}).Validate()
	if !strings.Contains(err.Error(), "truncate") {
		t.Fatalf("error text = %q", err)
	}
}

func TestDefaults(t *testing.T) {
	c := &Config{}
	if c.Budget() != rollover.DefaultBudget {
		t.Fatalf("budget = %d", c.Budget())
	}
	if c.IdleTimeout() != DefaultIdleTimeout || c.PromptTimeout() != DefaultPromptTimeout ||
		c.EditInterval() != DefaultEditInterval {
		t.Fatal("duration defaults not applied")
	}
	if c.GetSilentSentinel() != DefaultSilentSentinel {
		t.Fatalf("sentinel = %q", c.GetSilentSentinel())
	}
	if got := c.GetAgentCmd(); len(got) != 3 || got[0] != "fir" {
		t.Fatalf("agent cmd = %v", got)
	}
	if c.AllowedUsers() != nil {
		t.Fatal("empty allowlist must be nil (meaning anyone)")
	}
	c2 := &Config{SilentSentinel: "<<QUIET>>", AgentCmd: []string{"claude"}}
	if c2.GetSilentSentinel() != "<<QUIET>>" || c2.GetAgentCmd()[0] != "claude" {
		t.Fatal("explicit values not honoured")
	}
}

func TestValidateCredentials(t *testing.T) {
	if err := ValidateCredentials("https://z", "a@b", "k"); err != nil {
		t.Fatalf("complete credentials: %v", err)
	}
	err := ValidateCredentials("", "", "")
	if err == nil {
		t.Fatal("want error")
	}
	for _, want := range []string{"ZULIP_SITE", "ZULIP_EMAIL", "ZULIP_API_KEY"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error must mention %s: %q", want, err)
		}
	}
	if err := ValidateCredentials("https://z", "", "k"); err == nil || !strings.Contains(err.Error(), "bot_email") {
		t.Fatalf("error = %v", err)
	}
}

func TestResolveChannels(t *testing.T) {
	available := []zulipproto.Stream{
		{StreamID: 4, Name: "fleet"},
		{StreamID: 7, Name: "general"},
		{StreamID: 9, Name: "Fleet"}, // Zulip channel names are case-sensitive
	}
	c := &Config{Channels: []string{"fleet", " 7 ", "Fleet"}}
	got, err := c.ResolveChannels(available)
	if err != nil {
		t.Fatalf("ResolveChannels: %v", err)
	}
	if len(got) != 3 || got[4] != "fleet" || got[7] != "general" || got[9] != "Fleet" {
		t.Fatalf("resolved = %v", got)
	}

	// An empty list is an error: answering in every channel of a realm
	// by default is a footgun, not a convenience.
	if _, err := (&Config{}).ResolveChannels(available); err == nil {
		t.Fatal("want error for no channels")
	}
	if _, err := (&Config{Channels: []string{"nope"}}).ResolveChannels(available); err == nil {
		t.Fatal("want error for unknown name")
	}
	if _, err := (&Config{Channels: []string{"404"}}).ResolveChannels(available); err == nil {
		t.Fatal("want error for unknown id")
	}
}

func TestAgentClientConfig(t *testing.T) {
	c := &Config{BotAPIKey: "super-secret", AgentCmd: []string{"fir", "--mode", "acp"}}
	var stderr bytes.Buffer
	got := c.AgentClientConfig(&stderr)
	if len(got.Command) != 3 || got.Stderr != &stderr {
		t.Fatalf("client config = %+v", got)
	}
	// The relay's own credential must be scrubbed from the child: the
	// agent is driven by text from people who are not the operator.
	if len(got.Secrets) != 1 || got.Secrets[0] != "super-secret" {
		t.Fatalf("secrets = %v", got.Secrets)
	}
	var sawKey bool
	for _, n := range got.SecretEnvNames {
		if n == "ZULIP_API_KEY" {
			sawKey = true
		}
	}
	if !sawKey {
		t.Fatalf("secret env names = %v", got.SecretEnvNames)
	}
	if _, ok := got.ClientMeta[statusline.ExtensionID]; !ok {
		t.Fatalf("status-line extension not advertised: %v", got.ClientMeta)
	}
}

func TestNoopPosterIsInert(t *testing.T) {
	// Validate constructs a splitter to check the markers fit; the
	// poster it hands over must never do anything.
	var p noopPoster
	if id, err := p.Post(t.Context(), "x"); id != 0 || err != nil {
		t.Fatalf("Post = %d, %v", id, err)
	}
	if err := p.Edit(t.Context(), 1, "x"); err != nil {
		t.Fatalf("Edit = %v", err)
	}
}

func TestPathDefaults(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/xdg/config")
	t.Setenv("XDG_STATE_HOME", "/xdg/state")
	if got := DefaultConfigDir(); got != filepath.Join("/xdg/config", "zulip-acp") {
		t.Fatalf("config dir = %q", got)
	}
	if got := DefaultConfigPath(); got != filepath.Join("/xdg/config", "zulip-acp", "config.json") {
		t.Fatalf("config path = %q", got)
	}
	if got := DefaultStateDir(); got != filepath.Join("/xdg/state", "zulip-acp") {
		t.Fatalf("state dir = %q", got)
	}

	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("HOME", "/home/tester")
	if got := DefaultConfigDir(); got != filepath.Join("/home/tester", ".config", "zulip-acp") {
		t.Fatalf("config dir = %q", got)
	}
	if got := DefaultStateDir(); got != filepath.Join("/home/tester", ".local", "state", "zulip-acp") {
		t.Fatalf("state dir = %q", got)
	}

	// No HOME either: fall back to the temp dir.
	t.Setenv("HOME", "")
	if got := DefaultConfigDir(); got != filepath.Join(os.TempDir(), "zulip-acp") {
		t.Fatalf("config dir = %q", got)
	}
	if got := DefaultStateDir(); got != filepath.Join(os.TempDir(), "zulip-acp") {
		t.Fatalf("state dir = %q", got)
	}
}

// TestAckEmoji pins the three-state field: unset means the default,
// an explicit value is honoured, and an explicit "" disables the
// acknowledgement entirely.
func TestAckEmoji(t *testing.T) {
	cases := []struct {
		name string
		json string
		want string
	}{
		{"unset defaults", `{}`, DefaultAckEmoji},
		{"explicit value", `{"ack_emoji":"hourglass"}`, "hourglass"},
		{"explicit empty disables", `{"ack_emoji":""}`, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, []byte(c.json), 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}
			cfg, err := Load(path)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if got := cfg.GetAckEmoji(); got != c.want {
				t.Fatalf("GetAckEmoji() = %q, want %q", got, c.want)
			}
		})
	}
}

// TestAckEmojiIsValidated rejects the shape an operator will actually
// type: Zulip's UI shows `:eyes:`, but the API field is a bare
// emoji_name. Colons would fail on EVERY turn with nothing but a log
// line, so the config refuses to start instead.
func TestAckEmojiIsValidated(t *testing.T) {
	for _, bad := range []string{`":eyes:"`, `"eyes:"`, `"white check mark"`} {
		path := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(path, []byte(`{"ack_emoji":`+bad+`}`), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		if _, err := Load(path); err == nil {
			t.Fatalf("ack_emoji %s was accepted", bad)
		}
	}
}
