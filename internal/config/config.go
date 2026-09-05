// Package config loads the zulip-acp JSON config file.
package config

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/kfet/acp-kit/client"
	"github.com/kfet/acp-kit/statusline"
	"github.com/kfet/zulip-acp/internal/reload"
	"github.com/kfet/zulip-acp/internal/rollover"
	"github.com/kfet/zulip-acp/internal/zulipproto"
)

// Defaults for the tunables an operator rarely needs to touch.
const (
	DefaultIdleTimeout   = 30 * time.Minute
	DefaultPromptTimeout = 10 * time.Minute
	// DefaultEditInterval coalesces streaming edits. Zulip sustains
	// ~15 edits/sec without complaint, so this is a kindness to the
	// reader (every edit re-renders the whole message, on the server
	// and again on the phone) rather than a rate limit.
	DefaultEditInterval = 300 * time.Millisecond
	// DefaultSilentSentinel matches slack-acp and poe-acp so one fir
	// agent can serve every relay.
	DefaultSilentSentinel = "<<SILENT>>"
	// DefaultAckEmoji is the reaction the relay puts on a message the
	// moment it accepts it for handling, and removes when the turn
	// ends. Zulip has no typing indicator and a reaction is the only
	// acknowledgement that costs no message and can be retracted.
	DefaultAckEmoji = "eyes"
	// ChannelSentinel in the channels list means "also serve every
	// channel the bot is subscribed to, as that changes". It may stand
	// alone or sit alongside explicit names and ids.
	ChannelSentinel = "*"
)

// Config is the operator-facing JSON config.
type Config struct {
	// Site is the Zulip base URL, e.g. https://zulip.example.com.
	Site string `json:"site,omitempty"`
	// BotEmail and BotAPIKey are the bot's HTTP Basic credentials.
	BotEmail  string `json:"bot_email,omitempty"`
	BotAPIKey string `json:"bot_api_key,omitempty"`

	// Channels lists the Zulip channels the relay serves, by name or
	// by numeric id. Names are resolved to ids once at startup, and
	// the resolved set doubles as the channel allowlist: the relay
	// never answers anywhere else.
	Channels []string `json:"channels,omitempty"`

	// AmbientChannels lists channels the relay engages WITHOUT an
	// @-mention: in these, every message opens or continues the
	// topic's conversation, exactly as a DM does. Elsewhere a new
	// topic must @-mention the bot to summon it (the mention is the
	// membership record). Entries are names or ids, resolved like
	// Channels, and must also be served (via `channels` or "*").
	//
	// Same exposure as DMs: anyone who can post in an ambient channel
	// can summon an agent with a shell, no mention required.
	// allowed_user_ids still gates it.
	AmbientChannels []string `json:"ambient_channels,omitempty"`

	// AutotopicChannels lists channels where a "general chat" message
	// — Zulip 11's empty topic ("") — is MOVED to a freshly named
	// topic before the relay answers, so the conversation happens
	// there instead of in the channel's undifferentiated feed.
	//
	// Entries are names or ids, resolved like Channels, and must also
	// be served (via `channels` or "*"). Everywhere else general chat
	// behaves exactly as any other topic.
	AutotopicChannels []string `json:"autotopic_channels,omitempty"`

	// DMs enables direct-message conversations: 1:1 and group DMs with
	// the bot. Default FALSE — serving DMs is an explicit decision.
	//
	// The `channels` allowlist cannot gate a DM (a DM is in no
	// channel), so with DMs on, anyone in the realm who is not
	// excluded by allowed_user_ids can open a session with an agent
	// that has a shell. Same reasoning as requiring `channels` to be
	// set explicitly: "everything" must be asked for, never defaulted
	// into. allowed_user_ids applies to DMs unchanged.
	DMs bool `json:"dms,omitempty"`

	// AllowedUserIDs, if non-empty, restricts who the relay answers,
	// in channels and in DMs alike.
	AllowedUserIDs []int64 `json:"allowed_user_ids,omitempty"`

	// AgentCmd is the argv used to spawn the ACP agent.
	// Default: ["fir", "--mode", "acp"].
	AgentCmd []string `json:"agent_cmd,omitempty"`

	// StateDir roots per-conversation state. Each conversation gets a
	// stable cwd so agent state (e.g. .fir/) survives restarts and
	// idle GC. Default $XDG_STATE_HOME/zulip-acp.
	StateDir string `json:"state_dir,omitempty"`

	// SessionIdleTimeoutSeconds GCs idle sessions. 0 = 30 minutes.
	SessionIdleTimeoutSeconds int `json:"session_idle_timeout_seconds,omitempty"`
	// PromptTimeoutSeconds caps one agent turn. 0 = 10 minutes.
	PromptTimeoutSeconds int `json:"prompt_timeout_seconds,omitempty"`

	// SystemPrompt is appended to the built-in Zulip-formatting
	// instructions and injected into every ACP session.
	SystemPrompt string `json:"system_prompt,omitempty"`
	// DisableSystemPrompt skips system-prompt injection entirely,
	// including the built-in formatting block.
	DisableSystemPrompt bool `json:"disable_system_prompt,omitempty"`

	// HideThinking suppresses agent thought chunks from the posted
	// message.
	HideThinking bool `json:"hide_thinking,omitempty"`

	// SilentSentinel is the exact agent output that means "do not
	// reply". Only consulted for ambient (non-mention) turns.
	// Default "<<SILENT>>".
	SilentSentinel string `json:"silent_sentinel,omitempty"`

	// MaxMessageChars budgets one Zulip message, in CODE POINTS.
	// 0 uses rollover.DefaultBudget (9500), deliberately below Zulip's
	// hard 10000 so a server-side change in counting cannot cost
	// output.
	MaxMessageChars int `json:"max_message_chars,omitempty"`
	// SealMarker closes a message that has rolled over. This is a UX
	// choice, not a protocol one — change it freely.
	SealMarker string `json:"seal_marker,omitempty"`
	// ContinuationMarker opens every message after the first.
	ContinuationMarker string `json:"continuation_marker,omitempty"`

	// EditIntervalMs coalesces streaming edits. 0 = 300ms.
	EditIntervalMs int `json:"edit_interval_ms,omitempty"`

	// AckEmoji names the Zulip emoji reaction the relay adds to a
	// message it has accepted, and removes when the turn ends.
	// Unset = DefaultAckEmoji ("eyes"); an explicit "" disables the
	// acknowledgement entirely. It is a pointer precisely so those two
	// cases stay distinguishable.
	AckEmoji *string `json:"ack_emoji,omitempty"`

	// RepostOnClose re-posts the finished answer as NEW messages at the
	// end of a streamed turn, deleting the placeholder-seeded originals.
	//
	// Zulip generates a mobile push notification when a message is
	// CREATED and never when it is edited, so without this every push
	// reads "Thinking…". Unset = true; set it to false to keep the
	// streamed messages exactly where they are (older clients, or a
	// realm where the bot may not delete its own messages — though the
	// relay also disables reposting by itself the first time a delete
	// is refused).
	RepostOnClose *bool `json:"repost_on_close,omitempty"`

	// RelayMCP enables the agent→relay loopback: the relay hosts an
	// MCP server on a private unix socket and advertises it to the
	// agent, so the agent can read its own status, switch model, post
	// out of band, and schedule prompts back into this conversation.
	//
	// Default FALSE, and deliberately so. It hands the agent a way to
	// speak into the chat outside a turn and to arm work that runs
	// with no human watching, which is a real widening of what a
	// prompt-injected agent could do. Turn it on knowingly.
	RelayMCP bool `json:"relay_mcp,omitempty"`

	// MaxSchedulesPerConv, MaxSchedulesTotal and MaxScheduleDepth
	// bound scheduled prompts. 0 uses acp-kit/schedule's defaults
	// (10 / 100 / 3). MaxScheduleDepth is the important one: it caps
	// how long a schedule→turn→schedule chain can get, so recursion
	// always terminates.
	MaxSchedulesPerConv int `json:"max_schedules_per_conv,omitempty"`
	MaxSchedulesTotal   int `json:"max_schedules_total,omitempty"`
	MaxScheduleDepth    int `json:"max_schedule_depth,omitempty"`

	// MinScheduleIntervalSeconds floors a repeating schedule. 0 uses
	// acp-kit/schedule's default of 60s.
	MinScheduleIntervalSeconds int `json:"min_schedule_interval_seconds,omitempty"`
}

// Load reads and validates the config file. Unknown fields are an
// error: a typo in a config key must not be silently ignored.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var c Config
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	return &c, c.Validate()
}

// Validate checks the fields that can be wrong on their own.
// Credentials may arrive from the environment instead, so they are
// checked separately by ValidateCredentials.
func (c *Config) Validate() error {
	if c.SessionIdleTimeoutSeconds < 0 {
		return fmt.Errorf("session_idle_timeout_seconds must be >= 0")
	}
	if c.PromptTimeoutSeconds < 0 {
		return fmt.Errorf("prompt_timeout_seconds must be >= 0")
	}
	if c.EditIntervalMs < 0 {
		return fmt.Errorf("edit_interval_ms must be >= 0")
	}
	if c.MaxMessageChars < 0 {
		return fmt.Errorf("max_message_chars must be >= 0")
	}
	if c.MaxSchedulesPerConv < 0 || c.MaxSchedulesTotal < 0 || c.MaxScheduleDepth < 0 {
		return fmt.Errorf("schedule limits must be >= 0")
	}
	if c.MinScheduleIntervalSeconds < 0 {
		return fmt.Errorf("min_schedule_interval_seconds must be >= 0")
	}
	if c.MaxMessageChars > zulipproto.MaxMessageLength {
		return fmt.Errorf("max_message_chars %d exceeds Zulip's MAX_MESSAGE_LENGTH of %d — Zulip would silently truncate every message at the limit and the relay would lose output",
			c.MaxMessageChars, zulipproto.MaxMessageLength)
	}
	// Zulip's UI writes reactions as `:eyes:`, but the API field is a
	// bare emoji_name. A colonised or spaced value would fail on every
	// single turn, non-fatally, with nothing but a log line to say so.
	if e := c.GetAckEmoji(); strings.ContainsAny(e, ": \t") {
		return fmt.Errorf("ack_emoji %q must be a bare Zulip emoji name such as %q — no colons, no spaces", e, DefaultAckEmoji)
	}
	for _, ch := range c.Channels {
		if strings.TrimSpace(ch) == "" {
			return fmt.Errorf("channels must not contain empty entries")
		}
	}
	for _, ch := range c.AmbientChannels {
		if strings.TrimSpace(ch) == "" {
			return fmt.Errorf("ambient_channels must not contain empty entries")
		}
	}
	for _, ch := range c.AutotopicChannels {
		if strings.TrimSpace(ch) == "" {
			return fmt.Errorf("autotopic_channels must not contain empty entries")
		}
	}
	// A splitter built from these markers must still be constructible;
	// catching it here beats failing at the first long answer.
	if _, err := rollover.New(rollover.Config{
		Poster:     noopPoster{},
		Budget:     c.Budget(),
		SealMarker: c.SealMarker,
		ContMarker: c.ContinuationMarker,
	}); err != nil {
		return fmt.Errorf("message budget/markers: %w", err)
	}
	return nil
}

// noopPoster satisfies rollover.Poster for the constructor check in
// Validate. It is never driven.
type noopPoster struct{}

func (noopPoster) Post(context.Context, string) (int64, error) { return 0, nil }
func (noopPoster) Edit(context.Context, int64, string) error   { return nil }

// ValidateCredentials returns an operator-friendly error when the
// Zulip connection details are missing.
func ValidateCredentials(site, email, apiKey string) error {
	var missing []string
	if site == "" {
		missing = append(missing, "site (ZULIP_SITE)")
	}
	if email == "" {
		missing = append(missing, "bot_email (ZULIP_EMAIL)")
	}
	if apiKey == "" {
		missing = append(missing, "bot_api_key (ZULIP_API_KEY)")
	}
	if len(missing) == 0 {
		return nil
	}
	return fmt.Errorf("missing Zulip credentials: %s.\n"+
		"  Create a bot at <site>/#organization/bots (type: Generic), then copy its email and API key.\n"+
		"  Set them in the config file or in the environment.", strings.Join(missing, ", "))
}

// Budget returns the configured per-message code-point budget.
func (c *Config) Budget() int {
	if c.MaxMessageChars <= 0 {
		return rollover.DefaultBudget
	}
	return c.MaxMessageChars
}

// IdleTimeout returns the session GC timeout.
func (c *Config) IdleTimeout() time.Duration {
	if c.SessionIdleTimeoutSeconds <= 0 {
		return DefaultIdleTimeout
	}
	return time.Duration(c.SessionIdleTimeoutSeconds) * time.Second
}

// PromptTimeout returns the per-turn wall-clock cap.
func (c *Config) PromptTimeout() time.Duration {
	if c.PromptTimeoutSeconds <= 0 {
		return DefaultPromptTimeout
	}
	return time.Duration(c.PromptTimeoutSeconds) * time.Second
}

// EditInterval returns the streaming coalescing period.
func (c *Config) EditInterval() time.Duration {
	if c.EditIntervalMs <= 0 {
		return DefaultEditInterval
	}
	return time.Duration(c.EditIntervalMs) * time.Millisecond
}

// MinScheduleInterval returns the configured repeat floor, or 0 to let
// acp-kit/schedule apply its own default.
func (c *Config) MinScheduleInterval() time.Duration {
	return time.Duration(c.MinScheduleIntervalSeconds) * time.Second
}

// GetSilentSentinel returns the configured sentinel or the default.
func (c *Config) GetSilentSentinel() string {
	if c.SilentSentinel == "" {
		return DefaultSilentSentinel
	}
	return c.SilentSentinel
}

// GetAckEmoji returns the acknowledgement reaction emoji: the default
// when unset, or the configured value — including "" for "disabled".
func (c *Config) GetAckEmoji() string {
	if c.AckEmoji == nil {
		return DefaultAckEmoji
	}
	return *c.AckEmoji
}

// GetRepostOnClose reports whether a finished streamed turn is
// re-posted as new messages so the push notification carries the real
// answer. Unset means true.
func (c *Config) GetRepostOnClose() bool {
	return c.RepostOnClose == nil || *c.RepostOnClose
}

// GetAgentCmd returns the configured agent argv or the default.
func (c *Config) GetAgentCmd() []string {
	if len(c.AgentCmd) == 0 {
		return []string{"fir", "--mode", "acp"}
	}
	return c.AgentCmd
}

// ResolveChannels maps the configured channel names/ids onto Zulip
// channel ids using the realm's channel list. Numeric entries are
// taken as ids directly; everything else is matched against channel
// names, case-sensitively, because Zulip channel names are.
//
// The ChannelSentinel entry is not a channel and is skipped here; see
// FollowsSubscriptions.
//
// An empty Channels list is an error rather than "everything": a relay
// that answers in every channel of a realm by default is a footgun.
// The sentinel exists so that "everything" is something an operator
// asks for explicitly, never something a missing key produces. The one
// exception is a DM-only relay ("dms": true with no channels), which
// serves no channel at all.
func (c *Config) ResolveChannels(available []zulipproto.Stream) (map[int64]string, error) {
	if len(c.Channels) == 0 {
		if c.DMs {
			// A DM-only relay is a legitimate deployment: DMs are not
			// gated by the channel allowlist, so an empty one is not
			// the footgun it would otherwise be.
			return map[int64]string{}, nil
		}
		return nil, fmt.Errorf("no channels configured — set \"channels\" to the channel names or ids the relay should serve, or to [%q] to serve every channel the bot is subscribed to, or set \"dms\": true to serve direct messages only", ChannelSentinel)
	}
	byName := make(map[string]zulipproto.Stream, len(available))
	byID := make(map[int64]zulipproto.Stream, len(available))
	for _, s := range available {
		byName[s.Name] = s
		byID[s.StreamID] = s
	}
	out := make(map[int64]string, len(c.Channels))
	for _, want := range c.Channels {
		want = strings.TrimSpace(want)
		if want == ChannelSentinel {
			continue
		}
		if id, err := strconv.ParseInt(want, 10, 64); err == nil {
			s, ok := byID[id]
			if !ok {
				return nil, fmt.Errorf("channel id %d not visible to the bot — subscribe it to the channel", id)
			}
			out[s.StreamID] = s.Name
			continue
		}
		s, ok := byName[want]
		if !ok {
			return nil, fmt.Errorf("channel %q not visible to the bot — check the name (Zulip channel names are case-sensitive) and that the bot is subscribed", want)
		}
		out[s.StreamID] = s.Name
	}
	return out, nil
}

// ResolveAmbient maps the configured ambient_channels names/ids onto
// Zulip channel ids, using the same rules as ResolveChannels. Unlike
// ResolveChannels an empty list is fine — it simply yields an empty
// set, meaning "no ambient channels, mention-gate everywhere".
func (c *Config) ResolveAmbient(available []zulipproto.Stream) (map[int64]string, error) {
	return resolveNamed("ambient_channels", c.AmbientChannels, available)
}

// ResolveAutotopic maps the configured autotopic_channels names/ids
// onto Zulip channel ids. An empty list yields an empty set, meaning
// "general chat is an ordinary topic everywhere".
func (c *Config) ResolveAutotopic(available []zulipproto.Stream) (map[int64]string, error) {
	return resolveNamed("autotopic_channels", c.AutotopicChannels, available)
}

// resolveNamed resolves a secondary channel list — one that modifies
// behaviour in channels the relay already serves — onto stream ids.
// key names the config field, so the operator is told which list is
// wrong.
func resolveNamed(key string, wants []string, available []zulipproto.Stream) (map[int64]string, error) {
	if len(wants) == 0 {
		return map[int64]string{}, nil
	}
	byName := make(map[string]zulipproto.Stream, len(available))
	byID := make(map[int64]zulipproto.Stream, len(available))
	for _, s := range available {
		byName[s.Name] = s
		byID[s.StreamID] = s
	}
	out := make(map[int64]string, len(wants))
	for _, want := range wants {
		want = strings.TrimSpace(want)
		if id, err := strconv.ParseInt(want, 10, 64); err == nil {
			s, ok := byID[id]
			if !ok {
				return nil, fmt.Errorf("%s id %d not visible to the bot — subscribe it to the channel", key, id)
			}
			out[s.StreamID] = s.Name
			continue
		}
		s, ok := byName[want]
		if !ok {
			return nil, fmt.Errorf("%s %q not visible to the bot — check the name (Zulip channel names are case-sensitive) and that the bot is subscribed", key, want)
		}
		out[s.StreamID] = s.Name
	}
	return out, nil
}

// FollowsSubscriptions reports whether the channels list carries the
// ChannelSentinel, i.e. whether the served set tracks the bot's own
// subscriptions at runtime.
func (c *Config) FollowsSubscriptions() bool {
	for _, ch := range c.Channels {
		if strings.TrimSpace(ch) == ChannelSentinel {
			return true
		}
	}
	return false
}

// AllowedUsers returns the allowlist as a set, or nil when empty
// (meaning "anyone").
func (c *Config) AllowedUsers() map[int64]struct{} {
	if len(c.AllowedUserIDs) == 0 {
		return nil
	}
	m := make(map[int64]struct{}, len(c.AllowedUserIDs))
	for _, id := range c.AllowedUserIDs {
		m[id] = struct{}{}
	}
	return m
}

// AgentClientConfig assembles the acp-kit client config for the child
// agent.
//
// The bot API key is declared as a secret so client.Start scrubs it
// from the child's environment: the agent is driven by text from
// people who are not the operator, and anything it can read it can
// use to impersonate the relay.
//
// The graceful-reload cursor (reload.AgentEnvNames) is scrubbed for the
// same reason. It is present only in a process that was re-exec'd, and
// a live queue id is a relay capability: whoever holds it and a
// credential can poll the relay's own event queue and take delivery of
// its messages.
func (c *Config) AgentClientConfig(stderr io.Writer) client.Config {
	return client.Config{
		Command:        c.GetAgentCmd(),
		Stderr:         stderr,
		SecretEnvNames: append([]string{"ZULIP_API_KEY", "ZULIP_EMAIL"}, reload.AgentEnvNames()...),
		Secrets:        []string{c.BotAPIKey},
		ClientMeta: map[string]any{
			statusline.ExtensionID: map[string]any{},
		},
	}
}

// DefaultConfigDir is the operator's config root.
func DefaultConfigDir() string {
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "zulip-acp")
	}
	if h, err := os.UserHomeDir(); err == nil && h != "" {
		return filepath.Join(h, ".config", "zulip-acp")
	}
	return filepath.Join(os.TempDir(), "zulip-acp")
}

// DefaultConfigPath is the conventional config.json location.
func DefaultConfigPath() string { return filepath.Join(DefaultConfigDir(), "config.json") }

// DefaultStateDir is the conventional per-conversation state root.
func DefaultStateDir() string {
	if d := os.Getenv("XDG_STATE_HOME"); d != "" {
		return filepath.Join(d, "zulip-acp")
	}
	if h, err := os.UserHomeDir(); err == nil && h != "" {
		return filepath.Join(h, ".local", "state", "zulip-acp")
	}
	return filepath.Join(os.TempDir(), "zulip-acp")
}
