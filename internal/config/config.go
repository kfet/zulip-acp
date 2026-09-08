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
	DefaultIdleTimeout = 30 * time.Minute
	// DefaultNoProgressTimeout bounds a WEDGED turn: two minutes with
	// no agent output and no tool activity at all. It is not a working
	// bound — see Config.NoProgressTimeout.
	DefaultNoProgressTimeout = 2 * time.Minute
	// DefaultZulipCallTimeout bounds a single relay-initiated Zulip API
	// call made outside a turn (the `post` loopback tool, the relay-MCP
	// history/rename tools). These used to borrow prompt_timeout, which
	// no longer has a default to borrow; they are HTTP requests, not
	// turns, and one wedged request must not hang the agent forever.
	DefaultZulipCallTimeout = 2 * time.Minute
	// DefaultEditInterval coalesces streaming edits. Zulip sustains
	// ~15 edits/sec without complaint, so this is a kindness to the
	// reader (every edit re-renders the whole message, on the server
	// and again on the phone) rather than a rate limit.
	DefaultEditInterval = 300 * time.Millisecond
	// DefaultSpinnerInterval animates the "Thinking…" placeholder
	// while the agent has produced nothing yet. Same reasoning as
	// DefaultEditInterval: readability, not a rate limit.
	DefaultSpinnerInterval = 900 * time.Millisecond
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
	// DefaultArchiveChannel is where `!archive` (and the :wastebasket:
	// reaction) moves a topic when archive_channel is unset. The
	// feature stays off unless a channel of this name actually exists
	// and the bot may move messages into it — see
	// Config.GetArchiveChannel.
	DefaultArchiveChannel = "archive"
)

// Config is the operator-facing JSON config.
type Config struct {
	// Site is the Zulip base URL, e.g. https://zulip.example.com.
	Site string `json:"site,omitempty"`

	// SiteAliases are OTHER host names the same realm answers to.
	//
	// A realm is routinely reachable under more than one name — a
	// Tailscale or LAN name the relay dials, and a public vanity
	// domain the humans browse. Attachment ingestion accepts an
	// ABSOLUTE `https://<host>/user_uploads/…` link only when <host> is
	// one of ours, so a link someone pasted from "Copy link" in a
	// browser on the OTHER name is otherwise silently not an
	// attachment. Composer-attached files use relative paths and are
	// unaffected.
	//
	// Entries may be bare hosts ("zulip.example.com", with an optional
	// :port) or full URLs; only the host part is used.
	SiteAliases []string `json:"site_aliases,omitempty"`
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
	// NoProgressTimeoutSeconds cuts a turn that has gone silent: no
	// agent output and no tool activity for this long. Tool calls count
	// as progress, so a legitimately long tool is never cut. 0 = 2
	// minutes.
	NoProgressTimeoutSeconds int `json:"no_progress_timeout_seconds,omitempty"`
	// PromptTimeoutSeconds is an OPT-IN absolute ceiling on one agent
	// turn, enforced regardless of progress.
	//
	// 0 = NO ceiling. This changed in v0.27.0: it used to mean "10
	// minutes", a plain wall-clock cap that punished exactly the turns
	// working hardest — a turn was killed at 10m00s mid-tool-call while
	// the tool went on running. The guard that actually fires now is
	// NoProgressTimeoutSeconds. A config that sets this key gets a
	// startup warning naming both bounds in effect.
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

	// StreamEdits publishes the answer AS IT ARRIVES, by editing the
	// relay's own message every EditInterval. Unset = true, which is
	// the behaviour everyone has today.
	//
	// Set it to false for "quiet" (batch) mode: nothing is edited
	// during the turn and the whole answer is published once, when the
	// turn closes. The web and desktop clients re-render a message on
	// every edit, so a long turn visibly flickers; quiet mode trades
	// live streaming for a still screen.
	//
	// EditIntervalMs is then unused: there is no coalescing tick left
	// to set the period of.
	//
	// It is a pointer so "unset" and an explicit false stay
	// distinguishable, exactly like RepostOnClose.
	//
	// Quiet mode also turns the placeholder spinner OFF by default —
	// see SpinnerIntervalMs. Animating a placeholder for a whole turn
	// with no streamed text under it is the WORST case, not a
	// compromise, so the two settings are resolved together.
	StreamEdits *bool `json:"stream_edits,omitempty"`

	// SpinnerIntervalMs animates the "Thinking…" placeholder until the
	// first chunk of the answer replaces it.
	//
	// Unset = 900ms when StreamEdits is on, and 0 when it is off. An
	// explicit 0 means DO NOT ANIMATE: the placeholder is posted once
	// and never edited again — no spinner goroutine is started at all.
	// An explicit positive value is honoured in quiet mode too, which
	// is then the only edit the relay makes during a turn; that is a
	// deliberate choice, so it is not overridden.
	//
	// It is a pointer so "unset" and an explicit 0 stay
	// distinguishable, exactly like AckEmoji.
	SpinnerIntervalMs *int `json:"spinner_interval_ms,omitempty"`

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

	// Reactions delivers Zulip emoji reactions to the agent as a
	// synthetic user turn ("[reaction] Ada added :tada: to …"),
	// additions and removals alike.
	//
	// Unset = TRUE, and opting OUT is the deliberate act. It only ever
	// fires in a conversation the relay is ALREADY engaged in, for a
	// user the allowlist already permits — a deliberate signal from
	// someone mid-conversation, not a new way in — it never creates a
	// conversation or a session, and a burst of reactions is coalesced
	// into ONE turn, so a pile-on costs what a single reaction costs.
	// Set it to false if you do not want a stray :+1: to cost a turn
	// at all.
	Reactions *bool `json:"reactions,omitempty"`

	// ArchiveChannel names the channel a topic is MOVED to when
	// somebody archives it — by reacting :wastebasket: to the relay's
	// last message, or by typing `!archive`.
	//
	// Unset = DefaultArchiveChannel ("archive"); an explicit "" turns
	// the feature off. It is a pointer so those two cases stay
	// distinguishable, exactly like AckEmoji.
	//
	// The destination MUST be a channel the relay does not serve. That
	// is what makes an archive final: an unserved channel is outside
	// the allowlist by construction, so the topic cannot re-engage the
	// relay, and recovery is one move back. The relay checks this at
	// startup — along with whether the channel exists and whether realm
	// policy lets the bot move messages between channels — and disables
	// the feature with a log line if any of it does not hold, rather
	// than failing at the moment someone taps the emoji.
	//
	// Nothing is deleted: the topic and its messages travel intact, and
	// the conversation's state/convs/<id>/ directory stays on disk.
	ArchiveChannel *string `json:"archive_channel,omitempty"`

	// InboundAttachments downloads the files a human attaches to a
	// message into the conversation's working directory (`inbox/`) and
	// tells the agent where they are — with images additionally sent
	// as ACP image content blocks when the agent advertises support.
	//
	// Unset = TRUE. A Zulip message carries a LINK, never a file, and
	// the bytes sit behind an authenticated endpoint, so without this
	// an agent asked about an attached photo can only say it cannot
	// see it. Set it to false to keep the relay from fetching anything
	// a message points at.
	//
	// The files persist: they live in the conversation's state
	// directory, like everything else the conversation owns, and
	// nothing deletes them. `!new` mints a fresh conversation with a
	// fresh directory, so the new session starts with an empty inbox
	// while the old files stay on disk.
	InboundAttachments *bool `json:"inbound_attachments,omitempty"`

	// MaxAttachmentBytes caps ONE inbound attachment and
	// MaxAttachmentTotalBytes caps a whole message's worth. 0 uses the
	// handler defaults (20 MB and 60 MB).
	//
	// Anything over cap is SKIPPED and named in the prompt — the turn
	// still happens, and the agent is told what it did not get, so it
	// can ask for a smaller file instead of hallucinating about one it
	// never saw.
	MaxAttachmentBytes      int64 `json:"max_attachment_bytes,omitempty"`
	MaxAttachmentTotalBytes int64 `json:"max_attachment_total_bytes,omitempty"`

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
	if c.NoProgressTimeoutSeconds < 0 {
		return fmt.Errorf("no_progress_timeout_seconds must be >= 0")
	}
	if c.EditIntervalMs < 0 {
		return fmt.Errorf("edit_interval_ms must be >= 0")
	}
	if c.SpinnerIntervalMs != nil && *c.SpinnerIntervalMs < 0 {
		return fmt.Errorf("spinner_interval_ms must be >= 0")
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
	if c.MaxAttachmentBytes < 0 || c.MaxAttachmentTotalBytes < 0 {
		return fmt.Errorf("attachment size caps must be >= 0")
	}
	// A per-message total below the per-file cap is not an error but
	// it is certainly a mistake: the first attachment would be capped
	// at the total and every later one refused, which reads to the
	// operator as "large attachments randomly fail".
	if c.MaxAttachmentBytes > 0 && c.MaxAttachmentTotalBytes > 0 && c.MaxAttachmentTotalBytes < c.MaxAttachmentBytes {
		return fmt.Errorf("max_attachment_total_bytes (%d) must be >= max_attachment_bytes (%d) — a message budget smaller than one file's cap would refuse files the per-file cap allows",
			c.MaxAttachmentTotalBytes, c.MaxAttachmentBytes)
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

// NoProgressTimeout returns the per-turn no-progress window.
func (c *Config) NoProgressTimeout() time.Duration {
	if c.NoProgressTimeoutSeconds <= 0 {
		return DefaultNoProgressTimeout
	}
	return time.Duration(c.NoProgressTimeoutSeconds) * time.Second
}

// TurnCeiling returns the OPT-IN absolute per-turn cap. 0 means none.
//
// Named for what it is rather than after its JSON key so that every
// call site of the old PromptTimeout() had to be re-read when the
// meaning changed underneath it.
func (c *Config) TurnCeiling() time.Duration {
	if c.PromptTimeoutSeconds <= 0 {
		return 0
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

// GetStreamEdits reports whether the answer is published as it
// arrives (streaming edits) rather than once at the end of the turn.
// Unset means true.
func (c *Config) GetStreamEdits() bool {
	return c.StreamEdits == nil || *c.StreamEdits
}

// SpinnerInterval returns the placeholder animation period, or 0 for
// "do not animate".
//
// Unset follows the streaming mode: 900ms while streaming, off in
// quiet mode — a spinner is the only thing left editing there, and a
// placeholder that animates for a whole turn is exactly the flicker
// quiet mode exists to remove. An explicit value always wins.
func (c *Config) SpinnerInterval() time.Duration {
	if c.SpinnerIntervalMs == nil {
		if !c.GetStreamEdits() {
			return 0
		}
		return DefaultSpinnerInterval
	}
	return time.Duration(*c.SpinnerIntervalMs) * time.Millisecond
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

// GetReactions reports whether emoji reactions are delivered to the
// agent. Unset means true.
func (c *Config) GetReactions() bool {
	return c.Reactions == nil || *c.Reactions
}

// GetArchiveChannel returns the archive destination channel name: the
// default when unset, or the configured value — including "" for
// "archiving is disabled".
func (c *Config) GetArchiveChannel() string {
	if c.ArchiveChannel == nil {
		return DefaultArchiveChannel
	}
	return strings.TrimSpace(*c.ArchiveChannel)
}

// GetInboundAttachments reports whether the relay downloads the files
// a human attaches to a message. Unset means true.
func (c *Config) GetInboundAttachments() bool {
	return c.InboundAttachments == nil || *c.InboundAttachments
}

// ResolveArchiveChannel maps the configured archive channel onto a
// Zulip channel, using the same name-or-id rules as ResolveChannels.
//
// It returns ok=false when archiving is disabled or the channel is not
// visible to the bot. That is NOT an error: a missing archive channel
// disables one convenience control, and refusing to start the relay
// over it would be wildly out of proportion. The caller logs why.
func (c *Config) ResolveArchiveChannel(available []zulipproto.Stream) (zulipproto.Stream, bool) {
	want := c.GetArchiveChannel()
	if want == "" {
		return zulipproto.Stream{}, false
	}
	if id, err := strconv.ParseInt(want, 10, 64); err == nil {
		for _, s := range available {
			if s.StreamID == id {
				return s, true
			}
		}
		return zulipproto.Stream{}, false
	}
	for _, s := range available {
		if s.Name == want {
			return s, true
		}
	}
	return zulipproto.Stream{}, false
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
// The graceful-reload cursor and the loopback MCP token registry
// (reload.AgentEnvNames) are scrubbed for the same reason. Both are
// present only in a process that was re-exec'd. A live queue id is a
// relay capability: whoever holds it and a credential can poll the
// relay's own event queue and take delivery of its messages. The token
// registry is worse — it is every live session's bearer token, so an
// agent holding it could speak for any conversation the relay serves.
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
