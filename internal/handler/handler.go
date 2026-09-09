// Package handler turns inbound Zulip events into ACP prompts and
// streams the agent's answer back into the originating topic.
//
// The unit of conversation is a Zulip TOPIC, scoped by its channel.
// The topic string itself is never used as a key — see internal/journal
// for why — so everything here works in terms of the journal's stable
// conv-id.
package handler

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	acp "github.com/coder/acp-go-sdk"

	"github.com/kfet/acp-kit/client"
	"github.com/kfet/acp-kit/command"
	"github.com/kfet/acp-kit/relaytool"
	"github.com/kfet/acp-kit/schedule"
	"github.com/kfet/acp-kit/state"
	"github.com/kfet/zulip-acp/internal/autotopic"
	"github.com/kfet/zulip-acp/internal/journal"
	"github.com/kfet/zulip-acp/internal/rollover"
	"github.com/kfet/zulip-acp/internal/statusline"
	"github.com/kfet/zulip-acp/internal/zulipmcp"
	"github.com/kfet/zulip-acp/internal/zulipproto"
)

// InterruptedMarker is appended to a tail message the relay was still
// streaming into when it stopped. A restart kills the child agent, so
// the turn is dead regardless; the correct behaviour is to say so and
// let the next message start cleanly, never to try to continue a
// half-streamed message.
const InterruptedMarker = "\n\n*(relay restarted — turn interrupted)*"

// OutboxDir is the per-conversation directory an agent writes files
// into to have them uploaded to Zulip. Relative to the session cwd.
const OutboxDir = "outbox"

// sentDir holds files already uploaded, so a follow-up turn does not
// re-upload them.
const sentDir = ".sent"

// defaultSpinnerInterval animates the "Thinking…" placeholder. Zulip
// sustains ~15 edits/sec, so this is purely a readability choice.
const defaultSpinnerInterval = 900 * time.Millisecond

// Agent is the subset of *client.AgentProc the handler drives
// directly. Session lifecycle lives in Sessions.
type Agent interface {
	Prompt(ctx context.Context, sid acp.SessionId, prompt []acp.ContentBlock) (acp.StopReason, error)
	Models() (models []client.ModelInfo, currentID string)
	// SetModel selects the model for one session. It backs `!model
	// <id>`; the relay never calls it unless a user asked for a
	// specific model.
	SetModel(ctx context.Context, sid acp.SessionId, modelID string) error
	// AvailableCommands is the agent's advertised command catalog,
	// which gates the passthrough allowlist: the relay forwards
	// `!reload` as `/reload` only when the agent actually offers it.
	AvailableCommands() []client.CommandInfo
	// Caps is the agent's advertised capability set. Inbound
	// attachment ingestion consults promptCapabilities.image to decide
	// whether an image may be sent as a content block or must be named
	// on disk instead.
	Caps() client.Caps
}

// Sessions is the subset of *state.Manager the handler uses.
type Sessions interface {
	GetOrCreate(ctx context.Context, key string, sink client.SessionUpdateSink) (*state.Session, error)
	Touch(s *state.Session)
	Cancel(ctx context.Context, key string)
	TakePendingSystemPrompt(s *state.Session) string
	// StateDir is the root the per-conversation working directories
	// live under. `!status` reports the conversation's directory so a
	// human can go and look at it.
	StateDir() string
}

// ChannelSet is the relay's channel allowlist: it answers Name for a
// served channel and reports false for every other one.
//
// It is an interface rather than a map because the set may be static
// (an explicit `channels` list) or live (the "*" sentinel, following
// the bot's subscriptions); see internal/channels.
type ChannelSet interface {
	Name(streamID int64) (string, bool)
	// ID is the reverse of Name: it resolves a channel name typed as a
	// `#**mention**` to its id, and reports whether the relay serves
	// it. `!branch` is the only caller — it is handed a name and must
	// refuse a destination the relay could not post in.
	ID(name string) (int64, bool)
	// Ambient reports whether a channel engages without an @-mention.
	Ambient(streamID int64) bool
	// Autotopic reports whether a general-chat message in a channel
	// is moved to a generated topic before it is answered.
	Autotopic(streamID int64) bool
}

// Poster is the Zulip surface the handler writes to.
type Poster interface {
	SendMessage(ctx context.Context, streamID int64, topic, content string) (int64, error)
	SendDirectMessage(ctx context.Context, userIDs []int64, content string) (int64, error)
	// SendMessageWidget and SendDirectMessageWidget attach a
	// widget_content payload. Only `!opts` uses them; every other
	// message the relay posts is plain markdown.
	SendMessageWidget(ctx context.Context, streamID int64, topic, content, widget string) (int64, error)
	SendDirectMessageWidget(ctx context.Context, userIDs []int64, content, widget string) (int64, error)
	EditMessage(ctx context.Context, id int64, content string) error
	// MoveMessage retopics a message. Only the autotopic path uses
	// it, to lift a general-chat message into a topic of its own; it
	// may be refused by realm policy, so the caller degrades.
	MoveMessage(ctx context.Context, id int64, topic, propagateMode string) error
	// MoveMessageToChannel moves a message — with change_all, a whole
	// topic — to ANOTHER channel. Only the archive control uses it,
	// and only after startup has established that realm policy allows
	// it; see archive.go.
	MoveMessageToChannel(ctx context.Context, id, streamID int64, topic, propagateMode string) error
	// DeleteMessage removes a message the bot posted. Used to retire a
	// superseded `!opts` panel, which cannot be edited when it carries
	// a widget, and to retract the placeholder-seeded streaming chain
	// once the final answer has been re-posted (see
	// Handler.repostForNotify).
	DeleteMessage(ctx context.Context, id int64) error
	GetMessage(ctx context.Context, id int64) (zulipproto.Message, error)
	// Messages reads a narrow of the realm's history. The handler uses
	// it for exactly one thing: resolving the relay's OWN last message
	// in a conversation when neither memory nor the journal knows it —
	// see Handler.lastOwnMessage.
	Messages(ctx context.Context, narrow []zulipproto.NarrowTerm, limit int, beforeID int64) ([]zulipproto.Message, error)
	// OldestMessage returns the first message in a narrow. The archive
	// preflight is the only caller: with propagate_mode=change_all it
	// is the oldest message in the topic that Zulip's move time limit
	// is judged against.
	OldestMessage(ctx context.Context, narrow []zulipproto.NarrowTerm) (zulipproto.Message, bool, error)
	// Topics lists the topic names already in a channel. Only
	// `!branch` uses it, to avoid creating a topic that collides with
	// a live one — Zulip would silently merge the two.
	Topics(ctx context.Context, streamID int64) ([]string, error)
	// UserByID resolves a user id to a user record. Used only by the
	// reaction path, which is handed an id and nothing else.
	UserByID(ctx context.Context, id int64) (zulipproto.User, error)
	Upload(ctx context.Context, filename, contentType string, r io.Reader) (string, error)
	// DownloadUpload fetches an inbound attachment's bytes with the
	// bot's credentials, capped at max bytes. See inbox.go: a Zulip
	// message carries a link, never a file, so this is the only way
	// the agent ever sees what a human attached.
	DownloadUpload(ctx context.Context, uploadPath string, max int64) ([]byte, string, error)
	AddReaction(ctx context.Context, messageID int64, emoji string) error
	RemoveReaction(ctx context.Context, messageID int64, emoji string) error
	// SetTyping raises or lowers the typing indicator for one
	// conversation. It is quiet mode's liveness signal — see
	// typing.go — and is never used while streaming, where the
	// placeholder already shows the relay is alive.
	SetTyping(ctx context.Context, op string, streamID int64, topic string, userIDs []int64) error
}

// Config configures a Handler.
type Config struct {
	Client   Poster
	Agent    Agent
	Sessions Sessions
	Journal  *journal.Journal

	// BotUserID is the relay's own Zulip user id. Messages from it are
	// refused unconditionally: the relay must never feed its own
	// output back into the agent.
	BotUserID int64
	// BotFullName is the bot's display name, used to recognise
	// @-mentions in raw markdown.
	BotFullName string
	// BotSenderIDs are user ids whose messages are never treated as a
	// human turn: every bot in the realm, snapshotted at startup. A
	// bot that appears later is not in the set, but the cross-realm
	// system bots — the ones that actually post unprompted — are
	// caught by the SenderRealm check instead.
	BotSenderIDs map[int64]struct{}

	// Channels is the served-channel allowlist. The relay answers
	// nowhere else. It is consulted per event rather than snapshotted,
	// because a set that follows the bot's subscriptions changes
	// underfoot while the relay runs.
	Channels ChannelSet
	// AllowedUsers, if non-nil, restricts who the relay answers. It
	// applies to direct messages exactly as it does in a channel.
	AllowedUsers map[int64]struct{}

	// DMs enables direct-message conversations. Off by default: a
	// relay hands the whole realm an agent with a shell, and the
	// channel allowlist — which cannot gate a DM, because a DM is in
	// no channel — is the only thing standing in the way. Serving DMs
	// must therefore be something an operator asks for.
	DMs bool

	// NoProgressTimeout cuts a turn that has stopped making progress:
	// no agent output and no tool-call activity for this long. It is a
	// WEDGE guard, not a working bound — a legitimately long tool call
	// keeps resetting it, so a turn that is working is never cut. 0
	// defaults to 2 minutes.
	NoProgressTimeout time.Duration
	// TurnCeiling is an OPT-IN absolute cap on one turn, enforced
	// regardless of progress. 0 (the default) means no ceiling.
	TurnCeiling time.Duration
	// ZulipCallTimeout bounds one relay-initiated Zulip API call made
	// outside a turn (the `post` loopback tool). 0 defaults to 2
	// minutes.
	ZulipCallTimeout time.Duration
	// EditInterval coalesces streaming edits.
	EditInterval time.Duration
	// BatchEdits suppresses intra-turn streaming edits entirely: the
	// answer is published once, when the turn closes. The zero value
	// is the streaming behaviour, so it is stated as the negative —
	// config.Config's operator-facing key is `stream_edits`.
	//
	// It also suppresses the eager "Thinking…" placeholder, which is
	// the whole point: with no placeholder, the single post at Close
	// is the first and only message CREATED, so the one push
	// notification the user gets carries the real answer. Liveness
	// comes from the typing indicator instead — see typing.go.
	BatchEdits bool
	// SpinnerInterval animates the "Thinking…" placeholder. nil is the
	// default: 900ms while streaming, off in batch mode. A non-nil 0
	// disables the animation — the placeholder is posted once and no
	// spinner goroutine is started at all. It is unused in batch mode,
	// where there is no placeholder to animate.
	SpinnerInterval *time.Duration
	// TypingInterval is how often quiet mode refreshes the typing
	// indicator. It must be comfortably inside the realm's
	// server_typing_started_expiry_period_milliseconds; resolve it
	// with TypingIntervalFor. 0 in batch mode defaults to 10s; a
	// negative value turns the indicator off. Unused while streaming.
	TypingInterval time.Duration

	// Budget, SealMarker and ContinuationMarker configure the splitter.
	Budget             int
	SealMarker         string
	ContinuationMarker string

	// SilentSentinel lets the agent decline to answer an ambient turn.
	SilentSentinel string
	// HideThinking suppresses thought chunks on the streamed path.
	HideThinking bool

	// RepostOnClose re-posts a finished streamed answer as NEW
	// messages and deletes the placeholder-seeded originals, so the
	// mobile push notification carries the answer instead of
	// "Thinking…" — Zulip notifies on message CREATION only.
	RepostOnClose bool

	// AckEmoji is the emoji reaction placed on the triggering message
	// for the duration of a turn. Empty disables the acknowledgement.
	AckEmoji string

	// Site is the relay's own realm URL. Inbound attachment ingestion
	// needs it to recognise an ABSOLUTE `https://<realm>/user_uploads/…`
	// link as ours; an empty value restricts ingestion to relative
	// paths, which is the safe direction to fail.
	Site string

	// SiteAliases are additional host names this realm answers to, so
	// an absolute upload link copied from a browser is recognised as
	// ours even when the human reached the realm by a different name
	// than the relay does. Entries may be bare hosts or full URLs.
	SiteAliases []string

	// InboundAttachments downloads the files a human attached to a
	// message into <cwd>/inbox and puts their local paths (and, when
	// the agent takes image blocks, the images themselves) in front of
	// the agent. See inbox.go.
	InboundAttachments bool
	// MaxAttachmentBytes caps ONE inbound attachment and
	// MaxAttachmentTotalBytes caps a whole message's worth. A file
	// over either is skipped with a note in the prompt, never a failed
	// turn. Both must be positive when InboundAttachments is set; New
	// fills a zero with the default.
	MaxAttachmentBytes      int64
	MaxAttachmentTotalBytes int64

	// Reactions delivers emoji reactions into the owning conversation
	// as an ambient synthetic turn. See reaction.go for why every gate
	// there is mandatory — reaction events are NOT narrowed by the
	// event queue, so the whole realm's traffic reaches us.
	Reactions bool

	// ReactionTrigger, if set, gets first refusal on every reaction
	// that resolves to an engaged conversation, and reports whether it
	// consumed it — i.e. whether the relay itself acted on the
	// reaction instead of handing it to the agent.
	//
	// In production this is Handler.ArchiveReaction (see archive.go):
	// archiving a topic is a RELAY action, not an agent turn, and a
	// destructive control must never depend on the model choosing to
	// call a tool.
	ReactionTrigger func(ctx context.Context, conv journal.Conv, ev zulipproto.Event, m *zulipproto.Message) bool

	// ArchiveStreamID and ArchiveChannel are the destination of the
	// archive control: the channel a topic is MOVED to when someone
	// reacts :wastebasket: to the relay's last message or types
	// `!archive`. Zero disables the whole control.
	//
	// The destination must be a channel this relay does NOT serve —
	// that is what makes an archived topic unable to re-engage it —
	// and the bot must be permitted by realm policy to move messages
	// between channels. Both are established at startup (see
	// cmd/zulip-acp), so nothing here has to discover them mid-action.
	ArchiveStreamID int64
	ArchiveChannel  string

	// ArchiveMoveLimit is the realm's move_messages_between_streams
	// time limit as it applies to THIS bot: how old the oldest message
	// in a topic may be and still be movable. Zero means unlimited —
	// either the realm sets no limit, or the bot is exempt.
	//
	// It is not decoration. A move refused on age grounds fails at the
	// LAST step of an archive, after the conversation has been ended
	// and retired, leaving the topic where it was with no conversation
	// attached to it. Knowing the limit lets the archive settle the
	// question before it touches anything; see Handler.movable.
	ArchiveMoveLimit time.Duration

	// After is the timer the reaction debounce waits on. Defaults to
	// time.After.
	//
	// It is injected for the same reason the clock is: a debounce
	// proved by sleeping is a flaky test, and this is the one place in
	// the relay whose behaviour IS a duration.
	After func(d time.Duration) <-chan time.Time

	// OnReactionBatch, if set, is called with a conversation's
	// coalesced reaction burst at the instant the conversation has
	// been claimed for it and the buffer emptied — i.e. the one moment
	// from which WaitIdle can see the turn.
	//
	// It exists so the test suite can prove coalescing without racing
	// a timer; nil in production. Like OnWaitForConv it must only
	// signal.
	OnReactionBatch func(convID string, n int)

	// Commands is the acp-kit chat-command broker. Nil disables the
	// whole `!command` surface, which is what the relay does when it
	// has no agent to ask. New wires the Handler in as the broker's
	// Controller, so the caller must not call SetController itself.
	Commands *command.Broker

	// Schedules is the durable store behind scheduled prompts. Nil
	// disables scheduling: the Handler still satisfies
	// command.Scheduler, but every call reports that the relay cannot
	// do it.
	Schedules *schedule.Store
	// Loopback is the self-hosted MCP tool set. Nil disables the
	// agent→relay loopback. The Handler needs it only to drain
	// deferred actions at the end of a turn.
	Loopback *relaytool.Tools

	// Version, AgentCmd and StartTime are reported by `!status`. All
	// optional: an unset field is simply omitted from the reply.
	Version   string
	AgentCmd  string
	StartTime time.Time
	// Now is the clock `!status` measures uptime against. Injected so
	// the test suite never has to sleep. Defaults to time.Now.
	Now func() time.Time

	// OnWaitForConv, if set, is called just before a scheduled turn
	// parks waiting for a conversation to go idle.
	//
	// It exists so the test suite can prove the queueing behaviour
	// deterministically: "the scheduled turn waited rather than
	// superseding the human one" is only observable at the moment the
	// wait is entered, and asserting it with a timer would be exactly
	// the flaky, timing-dependent test AGENTS.md forbids. Nil in
	// production.
	//
	// It runs while the handler's inflight lock is HELD, so it must do
	// nothing but signal. Anything that touches the Handler deadlocks.
	OnWaitForConv func(convID string)

	// OnEarlyPlaceholder, if set, is called once the ambient path has
	// posted its placeholder AND recorded the tail for it.
	//
	// It exists because those are two steps, and the surface only
	// witnesses the first: a test that watched for the posted message
	// and then asserted on the journal was racing the goroutine that
	// records it — which is exactly the kind of nearly-always-passing
	// test that fails once in CI and teaches nobody anything. Nil in
	// production; it must only signal.
	OnEarlyPlaceholder func(convID string)

	// OnTurnEnd, if set, is called once a completed turn's deferred
	// loopback actions have been applied — i.e. at the very last
	// instant the turn's goroutine touches anything.
	//
	// It exists for the same reason as OnWaitForConv: WaitIdle cannot
	// see this work, because endTurn runs deliberately AFTER the turn
	// has left the inflight map (see handleMessage). A test that
	// asserted on it without this hook would be racing the harness's
	// own TempDir cleanup. Nil in production.
	OnTurnEnd func(convID string)

	// OnArchiveExpired, if set, is called after an armed archive
	// confirmation has lapsed and been forgotten.
	//
	// Like the other On* hooks it exists so the test suite can prove
	// the behaviour without racing a timer: the expiry is observable
	// only as an absence, and asserting on an absence with a sleep is
	// exactly the flaky test AGENTS.md forbids. Nil in production; it
	// must only signal.
	OnArchiveExpired func(convID string)

	// Logf receives operational messages.
	Logf func(format string, args ...any)
}

// inflightEntry wraps a cancel func with its own identity, so clearing
// can tell its entry from one a follow-up has since installed.
//
// rename is the topic rename this turn has armed through the
// `rename_topic` loopback tool, applied as the turn ends. It hangs off
// the TURN rather than the conversation on purpose — see rename.go —
// and is read and written under inflightMu, like the map itself.
type inflightEntry struct {
	cancel context.CancelFunc
	rename *pendingRename
}

// Handler implements the event side of the relay.
type Handler struct {
	cfg Config

	inflightMu   sync.Mutex
	inflightCond *sync.Cond
	inflight     map[string]*inflightEntry

	// modelChoices holds the sticky per-conversation model set with
	// `!model <id>`. In memory only: a model choice is a session-shaped
	// preference, and a relay restart drops the ACP sessions it applied
	// to anyway, so persisting it would only preserve a claim about
	// state that no longer exists. It holds at most one entry per
	// conversation a human has run `!model` in — the same order as the
	// session map itself — so it needs no GC of its own.
	modelMu      sync.Mutex
	modelChoices map[string]modelChoice

	// dmNames remembers the display names of a DM's participants,
	// learned from the messages arriving in it. The key holds user
	// IDS, and an id is not a human term — `!status` should say "DM
	// with Kfet, fir-relay". Bounded by the number of DM
	// conversations, and purely cosmetic: an unknown set falls back to
	// the id list.
	dmMu    sync.Mutex
	dmNames map[string][]string

	// optsMu serialises `!opts` panel replacement. See showPanel: the
	// panel's message id is a read-modify-write reachable from both
	// the event loop and a turn goroutine.
	optsMu sync.Mutex

	// repostBroken is the end-of-turn repost circuit breaker. It is on
	// the Handler, not the Splitter, precisely because it must outlive
	// a turn: see repostForNotify.
	repostBroken atomic.Bool

	// ownMsgs maps a message the relay posted to the conversation that
	// owns it, so a reaction on the relay's own last message resolves
	// with no API call. badMsgs is the negative cache for message ids
	// that did not resolve, and userNames caches reactor names. All
	// three are bounded hints — see reaction.go.
	ownMsgs   *msgIndex
	badMsgs   *msgIndex
	userNames *msgIndex

	// linkMsgs remembers which messages have already been hydrated
	// into which conversation, so re-pasting a link in a follow-up
	// does not re-inject the same block every turn. A bounded hint
	// like the three above — see link.go.
	linkMsgs *msgIndex

	// lastOwn is the NEWEST message the relay has posted in each
	// conversation. The archive control needs more than "this message
	// is ours" (ownMsgs): reacting to an old answer from last week
	// must not arm anything, so the gesture is defined on the last
	// message and nothing else. One int per conversation, written
	// wherever ownMsgs is — see rememberOwn.
	//
	// It is a CACHE, not the record: the record is the journal's
	// Conv.LastOwnID, which survives the restarts and reloads this map
	// does not. See lastOwnMessage for the three-step resolution.
	lastOwnMu sync.Mutex
	lastOwn   map[string]int64

	// archivePending holds each conversation's armed archive
	// confirmation. An entry exists only between the warning being
	// posted and the confirmation, the expiry, or the end of the
	// conversation — see archive.go.
	archiveMu      sync.Mutex
	archivePending map[string]*pendingArchive

	// lookupMu, lookupStart, lookupCount and lookupWarned are the token
	// bucket in front of the reaction path's GET /messages/{id}.
	lookupMu     sync.Mutex
	lookupStart  time.Time
	lookupCount  int
	lookupWarned bool

	// reactPending buffers each conversation's in-flight burst of
	// reactions, so a pile-on costs one turn. An entry exists only
	// while a flush is armed for it — see enqueueReaction.
	reactMu      sync.Mutex
	reactPending map[string]*reactionBatch
}

// New constructs a Handler.
func New(cfg Config) (*Handler, error) {
	if cfg.Client == nil || cfg.Agent == nil || cfg.Sessions == nil || cfg.Journal == nil {
		return nil, fmt.Errorf("handler: Client, Agent, Sessions and Journal are all required")
	}
	if cfg.Channels == nil {
		return nil, fmt.Errorf("handler: Channels is required — a relay with no channel allowlist would answer the whole realm")
	}
	if cfg.NoProgressTimeout <= 0 {
		cfg.NoProgressTimeout = 2 * time.Minute
	}
	if cfg.ZulipCallTimeout <= 0 {
		cfg.ZulipCallTimeout = 2 * time.Minute
	}
	if cfg.EditInterval <= 0 {
		cfg.EditInterval = 300 * time.Millisecond
	}
	if cfg.SpinnerInterval == nil {
		d := defaultSpinnerInterval
		if cfg.BatchEdits {
			// Quiet mode posts no placeholder at all, so there is
			// nothing to animate.
			d = 0
		}
		cfg.SpinnerInterval = &d
	}
	if cfg.BatchEdits && cfg.TypingInterval == 0 {
		cfg.TypingInterval = defaultTypingInterval
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	if cfg.MaxAttachmentBytes <= 0 {
		cfg.MaxAttachmentBytes = DefaultMaxAttachmentBytes
	}
	if cfg.MaxAttachmentTotalBytes <= 0 {
		cfg.MaxAttachmentTotalBytes = DefaultMaxAttachmentTotalBytes
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	h := &Handler{
		cfg:            cfg,
		inflight:       map[string]*inflightEntry{},
		modelChoices:   map[string]modelChoice{},
		dmNames:        map[string][]string{},
		ownMsgs:        newMsgIndex(reactionIndexSize),
		badMsgs:        newMsgIndex(reactionIndexSize),
		userNames:      newMsgIndex(reactionIndexSize),
		linkMsgs:       newMsgIndex(linkIndexSize),
		lastOwn:        map[string]int64{},
		archivePending: map[string]*pendingArchive{},
		reactPending:   map[string]*reactionBatch{},
		lookupStart:    cfg.Now(),
	}
	h.inflightCond = sync.NewCond(&h.inflightMu)
	// Wiring the Controller here rather than in the caller keeps the
	// broker↔handler construction cycle out of main, and guarantees it
	// happens before any event can arrive.
	if cfg.Commands != nil {
		cfg.Commands.SetController(h)
	}
	return h, nil
}

// now is the injected clock.
func (h *Handler) now() time.Time { return h.cfg.Now() }

// Handle is the zulipproto.EventHandler entry point.
func (h *Handler) Handle(ctx context.Context, ev zulipproto.Event) {
	switch ev.Type {
	case zulipproto.EventMessage:
		h.handleMessage(ctx, ev.Message)
	case zulipproto.EventUpdateMessage:
		h.handleUpdate(ev)
	case zulipproto.EventReaction:
		h.handleReaction(ctx, ev)
	}
}

// handleUpdate migrates a conversation when its topic moves, and ENDS
// one when the topic leaves the served set.
//
// Missing the first costs a spurious duplicate session: the same agent
// session would keep running under the old name while a fresh one was
// created under the new one.
//
// The second is the cross-channel case, which arrives as the same
// event carrying a new_stream_id. A conversation must never follow its
// topic into a channel the relay does not serve: the session would go
// on living, addressable by a key the allowlist refuses, and every
// message in it would be dropped — a ghost. So it is retired, exactly
// as `!new` retires one, and the state directory is left on disk.
//
// There is no DM analogue and this path must never touch one: a direct
// message has no topic to rename, and its conv key lives in a disjoint
// namespace, so Journal.Move could not match one even if it were
// called. The StreamID guard below makes that explicit rather than
// incidental — Zulip sends no stream id on a DM update event.
func (h *Handler) handleUpdate(ev zulipproto.Event) {
	if ev.StreamID == 0 {
		return
	}
	moved := ev.NewStreamID != 0 && ev.NewStreamID != ev.StreamID
	// A pure content edit carries no topic pair and moves nothing.
	if !moved && (ev.OrigTopic == "" || ev.Topic == "" || ev.OrigTopic == ev.Topic) {
		return
	}
	// A channel move need not rename anything, in which case Zulip
	// sends the topic on one side only. The conversation is identified
	// by where it WAS, so a missing half is filled from the other.
	oldTopic, newTopic := ev.OrigTopic, ev.Topic
	if oldTopic == "" {
		oldTopic = newTopic
	}
	if newTopic == "" {
		newTopic = oldTopic
	}
	if oldTopic == "" {
		return
	}
	// A move in a channel that has since left the served set is
	// dropped. The topic is truth and the journal is a cache, so the
	// worst case is a stale entry the next message in the new topic
	// supersedes.
	if _, ok := h.cfg.Channels.Name(ev.StreamID); !ok {
		return
	}
	dest := ev.StreamID
	if moved {
		name, served := h.cfg.Channels.Name(ev.NewStreamID)
		if !served {
			h.retireMoved(journal.Channel(ev.StreamID, oldTopic), ev.NewStreamID)
			return
		}
		h.cfg.Logf("handler: topic %q moved to #%s, which is also served", oldTopic, name)
		dest = ev.NewStreamID
	}
	c, migrated, err := h.cfg.Journal.Move(ev.StreamID, oldTopic, dest, newTopic)
	if err != nil {
		h.cfg.Logf("handler: topic move %q → %q: %v", oldTopic, newTopic, err)
		return
	}
	if migrated {
		h.cfg.Logf("handler: topic moved %q → %q, session %s follows it", oldTopic, newTopic, c.ID)
	}
}

// retireMoved ends the conversation whose topic has just left the
// served set — which is what the archive control does deliberately, and
// what a human moving a topic to some other channel does incidentally.
//
// Nothing on disk is deleted: Retire keeps the old conv-id and its
// state/convs/<id>/ directory and mints a fresh id for the key, so a
// later topic of that name cannot reach the old session's memory.
//
// The archive control retires the conversation BEFORE it issues the
// move, so by the time its own echoed event lands here the key belongs
// to the empty conversation Retire minted. Retiring that one costs a
// journal write and nothing else; it is the same fail-safe answer, not
// a special case worth a flag that could go stale.
func (h *Handler) retireMoved(key journal.Key, dest int64) {
	// Retire is the single source of truth about whether there WAS a
	// conversation here: a topic nobody engaged in moving away is
	// ordinary traffic, not an event. The session is stopped after the
	// journal write rather than before it, because the topic has
	// ALREADY gone — unlike the archive path, there is no move left to
	// race with, and retiring first means nothing can be handed the
	// conversation in between.
	prev, fresh, existed, err := h.cfg.Journal.Retire(key)
	if err != nil {
		h.cfg.Logf("handler: topic %q moved to channel %d, which I do not serve, but retiring the conversation failed: %v", key.Topic, dest, err)
		return
	}
	if !existed {
		return
	}
	// context.Background: an event has no turn context to inherit, and
	// ending a conversation must not be abandoned half-way because the
	// poll loop moved on.
	h.endSession(context.Background(), prev.ID)
	h.cfg.Logf("handler: topic %q moved to channel %d, which I do not serve — %s is retired (its files are untouched); %s would answer if the topic came back",
		key.Topic, dest, prev.ID, fresh.ID)
}

// handleMessage decides whether a message is ours to answer and, if
// so, starts a turn.
func (h *Handler) handleMessage(ctx context.Context, m *zulipproto.Message) {
	if m == nil {
		return
	}
	// The relay must never act on its own message. This is the first
	// guard, before any allowlist, so a widened allowlist can never
	// reorder it.
	if m.SenderID == h.cfg.BotUserID {
		return
	}
	// Nor on any other bot's. Zulip posts topic moves, stream
	// creations and welcome messages as cross-realm system bots, which
	// land in a topic the relay is engaged in and would otherwise burn
	// a full agent turn on "This topic was moved here from …".
	if m.SenderRealm == zulipproto.SystemBotRealm {
		return
	}
	if _, isBot := h.cfg.BotSenderIDs[m.SenderID]; isBot {
		return
	}
	if m.Type != zulipproto.MessageTypeStream && !m.IsDM() {
		h.cfg.Logf("handler: ignoring message %d of unknown type %q", m.ID, m.Type)
		return
	}
	text := strings.TrimSpace(m.Content)
	if text == "" {
		return
	}

	// Routing and gating in one step, because the two conversation
	// shapes differ in both.
	//
	//   - In a channel an @-mention summons the relay and starts a
	//     conversation; after that it answers ambiently, because the
	//     topic itself is the membership record — which is exactly why
	//     engagement survives a restart with no extra state.
	//   - A direct message is addressed to the bot by construction:
	//     there is nobody else in the conversation to be talking to.
	//     Mention gating is therefore OFF and every DM is treated as
	//     addressed, group DMs included.
	var (
		key       journal.Key
		addressed bool
	)
	if m.IsDM() {
		if !h.cfg.DMs {
			h.cfg.Logf("handler: ignoring direct message %d (dms not enabled)", m.ID)
			return
		}
		ids := m.Recipients()
		if len(ids) == 0 {
			// display_recipient is polymorphic, and this is the only
			// way it can come back useless. Without the participant
			// set there is no conv key and nobody to reply to.
			h.cfg.Logf("handler: direct message %d has no usable recipient list", m.ID)
			return
		}
		key, addressed = journal.DM(ids), true
		h.rememberDMNames(key, m.RecipientNames())
	} else {
		// The channel allowlist gates channel messages only. A DM is
		// in no channel, so there is nothing here to measure it
		// against; AllowedUsers below is what gates it.
		if _, ok := h.cfg.Channels.Name(m.StreamID); !ok {
			return
		}
		// An ambient channel engages like a DM: every message is
		// addressed, so the opening message of a fresh topic summons
		// the relay with no @-mention. Elsewhere the mention is the
		// membership record.
		key = journal.Channel(m.StreamID, m.Topic)
		addressed = h.cfg.Channels.Ambient(m.StreamID) || h.mentioned(text)
	}

	if h.cfg.AllowedUsers != nil {
		if _, ok := h.cfg.AllowedUsers[m.SenderID]; !ok {
			h.cfg.Logf("handler: dropping message %d from user %d (not allowed)", m.ID, m.SenderID)
			return
		}
	}

	// In an autotopic channel general chat is a LOBBY, not a
	// conversation: every message there is named and moved out. What
	// the journal already holds under the lobby key — a conv from
	// before the feature shipped, or one a failed move left behind —
	// must therefore NOT count as engagement, or that single entry
	// would route every future general-chat message straight past the
	// move and disable the feature in that channel forever.
	lobby := h.isLobby(m)

	// named is the topic an autotopic move generated for this message,
	// and it is set only when the move actually happened. It is the one
	// case where the relay knows the topic's name is a placeholder — so
	// it is the one case where the agent is told to replace it.
	var named string

	var (
		existing journal.Conv
		engaged  bool
	)
	if !lobby {
		existing, engaged = h.cfg.Journal.Lookup(key)
	}
	if !addressed && !engaged {
		// A lobby message is gated on being addressed alone: in a
		// non-ambient channel the mention still summons the relay, and
		// an unaddressed one is not moved.
		return
	}

	// Commands are parsed AFTER every guard above and BEFORE any
	// conversation is allocated — and before any topic move — so
	// `!help` in a topic the relay has never answered in leaves no
	// state behind and retopics nothing. A command consumes the
	// message; nothing here reaches the agent.
	prompt, handled := h.dispatch(ctx, m, key, h.promptText(text))
	if handled {
		return
	}

	if lobby {
		// The move happens BEFORE the lookup below, so the
		// conversation is looked up and allocated under the FINAL key
		// and no journal migration is ever needed. A failed move
		// yields the lobby key back and the relay answers in general
		// chat — for this message only; the next one is attempted
		// afresh.
		before := key.Topic
		key = h.autotopic(ctx, m, key, text)
		if key.Topic != before {
			named = key.Topic
		}
		existing, engaged = h.cfg.Journal.Lookup(key)
	}

	conv := existing
	if !engaged {
		var err error
		conv, err = h.cfg.Journal.Ensure(key)
		if err != nil {
			h.cfg.Logf("handler: allocate conversation for %s: %v", key.Label(), err)
			return
		}
		h.cfg.Logf("handler: new conversation %s in %s", conv.ID, h.describe(key))
		// A reaction on a message in this conversation may already
		// have been refused as unresolvable — the topic was not
		// engaged when it arrived. Engagement is the only moment that
		// answer can change, and it changes it for this key alone.
		h.badMsgs.dropValue(key.Label())
	}

	prompt = "[" + m.SenderName + "] " + prompt
	// Hydration runs after the conversation exists — it dedupes per
	// conversation — and before the rename hint, so the relay's own
	// instruction stays the last thing in the prompt.
	prompt += h.hydrateLinks(ctx, conv.ID, m)
	if named != "" && h.cfg.Loopback != nil {
		prompt += renameHint(named)
	}

	h.startTurn(ctx, conv, prompt, addressed, m.ID)
}

// renameHint is the one-off instruction appended to the turn that
// opened an auto-named topic.
//
// It is a per-TURN note rather than a line in the durable system prompt
// on purpose. The instruction is only true for the single message the
// autotopic move lifted out of general chat; every other turn is in a
// topic a human named, and telling the agent on every one of those that
// its topic is a placeholder is how a relay ends up renaming the
// channel out from under people.
func renameHint(topic string) string {
	return "\n\n[relay] This message opened a new topic. The relay auto-named it \"" + topic +
		"\" from the message's first line, which is a placeholder, not a title. Once you know what this " +
		"conversation is about, call the relay `" + zulipmcp.ToolRenameTopic + "` tool with a better one. " +
		"Do not mention the rename in your reply."
}

// startTurn supersedes whatever is running in the conversation and
// runs one turn in the background.
//
// ackMsgID is the message the in-flight acknowledgement reaction goes
// on; 0 means no acknowledgement.
func (h *Handler) startTurn(ctx context.Context, conv journal.Conv, prompt string, addressed bool, ackMsgID int64) {
	h.startTurnAnchored(ctx, conv, prompt, addressed, ackMsgID, ackMsgID)
}

// startTurnAnchored is startTurn with the rename ANCHOR stated
// separately from the acknowledged message.
//
// They are the same message for every ordinary turn — the one a human
// sent, which is in the topic the answer goes to — and `!branch` is the
// one case where they differ: the triggering `!branch` message is in
// the ORIGIN topic, while the turn runs in the new one. A rename is an
// edit of a message IN the topic being renamed (see rename.go), so the
// anchor must be the seed message the relay posted in the new topic,
// or the agent's rename would be refused as "no longer in it" and the
// branched topic would keep its auto-generated placeholder name for
// good.
func (h *Handler) startTurnAnchored(ctx context.Context, conv journal.Conv, prompt string, addressed bool, ackMsgID, anchorID int64) {
	// A follow-up supersedes whatever is still running in this topic.
	h.cancelInflight(ctx, conv.ID)
	// Cancellable only. The turn's real bound is the progress-resetting
	// liveness clock, armed inside run once the agent is about to be
	// prompted.
	pctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	entry := &inflightEntry{cancel: cancel, rename: &pendingRename{anchor: anchorID}}
	h.setInflight(conv.ID, entry)
	h.runTurn(pctx, cancel, conv, entry, prompt, addressed, ackMsgID)
}

// runTurn owns the turn goroutine and its unwinding. Both entry points
// above end here, so there is exactly one place that decides what
// happens as a turn finishes.
func (h *Handler) runTurn(pctx context.Context, cancel context.CancelFunc, conv journal.Conv, entry *inflightEntry, prompt string, addressed bool, ackMsgID int64) {
	go func() {
		// LIFO: deferred actions the agent asked for are applied only
		// after the turn is fully unwound and no longer inflight, so
		// `new_session` cannot cancel the very turn that requested it.
		defer h.endTurn(conv, entry)
		defer h.clearInflight(conv.ID, entry)
		defer cancel()
		if err := h.run(pctx, conv, prompt, addressed, ackMsgID); err != nil {
			h.cfg.Logf("handler: turn for %s failed: %v", conv.ID, err)
		}
	}()
}

// run executes one agent turn end to end.
//
// Every turn starts by reacting to the triggering message, which is
// the only acknowledgement Zulip can give instantly without posting
// anything: it costs no topic noise and it is retractable, so it is
// safe even on a turn that ends in silence. The reaction is removed on
// every exit path.
//
// Two shapes after that:
//
//   - Addressed (an @-mention): stream. An eager placeholder goes up
//     immediately — it is the first thing the user sees while a cold
//     agent starts — and the answer is edited in as it arrives.
//   - Ambient (a follow-up in an engaged topic, with a sentinel
//     configured): buffer, because the agent may decline and a message
//     that appears and then vanishes is worse on a phone than one that
//     arrives a beat later. The placeholder is NOT withheld until the
//     end of the turn, though: sentinelWatch posts it the moment the
//     streamed text can no longer become the sentinel, which is
//     usually the first chunk. The answer itself still lands via the
//     normal end-of-turn commit.
//
// In QUIET mode (BatchEdits) there is no placeholder on either shape.
// A placeholder is a created message and Zulip pushes on creation, so
// it costs a push notification reading "Thinking" before the one
// carrying the answer. Liveness is the typing indicator instead — see
// typing.go, and note that the comment which used to justify the
// placeholder here ("Zulip has no typing indicator") is stale: it does,
// for channels as well as DMs, verified on Zulip 12.2.
func (h *Handler) run(ctx context.Context, conv journal.Conv, prompt string, addressed bool, msgID int64) error {
	defer h.ack(ctx, msgID)()

	post := &convPoster{client: h.cfg.Client, key: conv.Key}
	split, err := rollover.New(rollover.Config{
		Poster:     post,
		Budget:     h.cfg.Budget,
		SealMarker: h.cfg.SealMarker,
		ContMarker: h.cfg.ContinuationMarker,
	})
	if err != nil {
		return fmt.Errorf("splitter: %w", err)
	}
	abstaining := !addressed && h.cfg.SilentSentinel != ""
	// On the abstain path thoughts are ALWAYS hidden: a thought that
	// reached the splitter before the verdict would post a message the
	// verdict cannot retract.
	sink := newStreamingSink(split, h.cfg.HideThinking || abstaining)

	wctx, wcancel := context.WithCancel(ctx)
	defer wcancel()

	if !abstaining {
		h.showWorking(ctx, wctx, conv, split, sink)
	}

	var sess *state.Session
	sinkFor := client.SessionUpdateSink(sink)
	var vs *client.ValidatingSink
	if abstaining {
		vs = client.NewValidatingSink(sink)
		// The abstain verdict is only final at the end of the turn,
		// but the moment the streamed text stops being a prefix of the
		// sentinel it is already known that a reply IS coming — so the
		// placeholder can go up then instead of minutes later.
		sinkFor = &sentinelWatch{next: vs, sentinel: h.cfg.SilentSentinel, onCommit: func() {
			h.showWorking(ctx, wctx, conv, split, sink)
			if h.cfg.OnEarlyPlaceholder != nil {
				h.cfg.OnEarlyPlaceholder(conv.ID)
			}
		}}
	}
	// The turn's bound. Armed HERE, not at intake: a turn that waited
	// behind another one must not be charged for the wait. The sink is
	// wrapped OUTERMOST so buffered paths (the abstain ValidatingSink)
	// cannot make a streaming agent look silent — liveness sees every
	// update as it lands, before anything downstream holds it back.
	live, lctx, stopLive := client.StartTurnLiveness(ctx, client.TurnLivenessConfig{
		NoProgressTimeout: h.cfg.NoProgressTimeout,
		MaxTurnDuration:   h.cfg.TurnCeiling,
	})
	defer stopLive()
	sinkFor = live.Wrap(sinkFor)

	sess, err = h.cfg.Sessions.GetOrCreate(lctx, conv.ID, sinkFor)
	if err != nil {
		wcancel()
		_ = split.Close(context.WithoutCancel(ctx), fmt.Sprintf("\n*error: %v*", err))
		h.trackTail(conv.ID, split)
		h.clearTail(conv.ID)
		return err
	}
	// Pre-seed the model identity so the spinner's first frame already
	// names the model. It is resolved again below, once applyModel has
	// settled a sticky `!model` choice, so the footer tracks the model
	// that actually served the turn.
	h.resolveModelInfo(sink)

	sess.Mu.Lock()
	defer sess.Mu.Unlock()
	h.cfg.Sessions.Touch(sess)
	h.applyModel(ctx, conv.ID, sess.SessionID)
	h.resolveModelInfo(sink)

	text := prompt
	if prefix := h.cfg.Sessions.TakePendingSystemPrompt(sess); prefix != "" {
		text = prefix + "\n\n" + text
	}
	// Inbound attachments are ingested here, and not at intake, for one
	// reason: the files go in the conversation's working directory, and
	// the session is what knows where that is. See inbox.go.
	note, extra := h.ingestAttachments(ctx, sess.Cwd, text)
	blocks := append([]acp.ContentBlock{acp.TextBlock(text + note)}, extra...)

	if !h.cfg.BatchEdits {
		go watchdog(wctx, split, h.cfg.EditInterval, func() { h.trackTail(conv.ID, split) })
	}

	var stop acp.StopReason
	if abstaining {
		res, perr := client.PromptAbstainable(lctx, h.cfg.Agent, sess.SessionID, blocks, vs, h.cfg.SilentSentinel)
		wcancel()
		if perr != nil {
			return h.failTurn(lctx, conv, split, perr)
		}
		if res.Abstained {
			h.cfg.Logf("handler: agent abstained in %s", conv.ID)
			h.clearTail(conv.ID)
			return nil
		}
		stop = res.Stop
		stopLive()
	} else {
		stop, err = h.cfg.Agent.Prompt(lctx, sess.SessionID, blocks)
		wcancel()
		if err != nil {
			return h.failTurn(lctx, conv, split, err)
		}
		// The bound covers the prompt and nothing after it. Disarming
		// here rather than only on the deferred path keeps the clock
		// and the thing it measures the same length; the flush,
		// upload and repost below run detached and must never be cut.
		// It must come AFTER failTurn, which reads the cause.
		stopLive()
	}

	fctx := context.WithoutCancel(ctx)
	suffix := h.uploadOutbox(fctx, sess.Cwd)
	if stop != "" && stop != acp.StopReasonEndTurn {
		suffix += fmt.Sprintf("\n\n*(stopped: %s)*", stop)
	}
	// The status footer is the very last thing in the answer — after
	// the attachment links and any "(stopped: …)" note — and it goes on
	// BEFORE Close flushes, so the body the end-of-turn repost copies
	// already carries it. Close then has nothing of its own to append.
	split.Append(suffix)
	sink.maybeAppendFooter()
	cerr := split.Close(fctx, "")
	if cerr != nil {
		h.rescue(fctx, post, split.Transcript(), cerr)
	} else {
		// Record the chain BEFORE the repost, which may replace the
		// ids and does its own tracking. In quiet mode this is the
		// only trackTail of the whole turn — nothing was posted
		// earlier — and it is what makes a reaction on the answer, and
		// the archive gesture, resolve without an API lookup.
		h.trackTail(conv.ID, split)
		h.repostForNotify(fctx, conv, split)
	}
	h.clearTail(conv.ID)
	return cerr
}

// resolveModelInfo pushes the relay-resolved model identity — provider
// emoji plus short display name — into the sink's status snapshot.
//
// The model id is relay-owned: the agent never supplies it. It is
// resolved twice per turn, before and after applyModel, because a
// sticky `!model` choice is pushed to the session mid-run and the
// status line should name the model that actually served the turn. An
// agent that reports no current model leaves the previous snapshot
// alone rather than blanking a known one.
func (h *Handler) resolveModelInfo(sink *streamingSink) {
	if _, currentID := h.cfg.Agent.Models(); currentID != "" {
		sink.SetModelInfo(
			statusline.ProviderEmojiForModel(currentID),
			statusline.ShortModelName(currentID),
		)
	}
}

// ack adds the in-flight reaction to the triggering message and
// returns the func that removes it.
//
// Reactions are decoration: every failure is logged and swallowed, and
// a turn is never failed because one did not stick. The removal runs
// on a context detached from the turn's, so a cancelled or superseded
// turn still cleans up after itself.
func (h *Handler) ack(ctx context.Context, msgID int64) func() {
	if h.cfg.AckEmoji == "" || msgID == 0 {
		return func() {}
	}
	if err := h.cfg.Client.AddReaction(ctx, msgID, h.cfg.AckEmoji); err != nil {
		h.cfg.Logf("handler: adding :%s: to message %d: %v", h.cfg.AckEmoji, msgID, err)
	}
	return func() {
		if err := h.cfg.Client.RemoveReaction(context.WithoutCancel(ctx), msgID, h.cfg.AckEmoji); err != nil {
			h.cfg.Logf("handler: removing :%s: from message %d: %v", h.cfg.AckEmoji, msgID, err)
		}
	}
}

// rescue is the last line of defence for the rule that matters most:
// NEVER drop output.
//
// Posting can fail for reasons the splitter cannot foresee — the realm
// closed its edit window, the server is briefly down, or (measured on
// Zulip 12.2) the markdown renderer refuses a body that is legal by
// length but expensive to render, e.g. a long run of emoji, which
// comes back as HTTP 400 "Unable to render message". Rather than log
// the failure and lose the agent's work, upload the whole transcript
// as a file and post a short message linking it. Uploads are raw bytes
// and never rendered, so this path cannot fail the same way.
func (h *Handler) rescue(ctx context.Context, post *convPoster, transcript string, cause error) {
	if strings.TrimSpace(transcript) == "" {
		return
	}
	url, err := h.cfg.Client.Upload(ctx, "answer.md", zulipproto.ContentType("answer.md", nil), strings.NewReader(transcript))
	if err != nil {
		h.cfg.Logf("handler: could not rescue %d chars of output: %v (original failure: %v)", len(transcript), err, cause)
		return
	}
	notice := fmt.Sprintf("*(the answer could not be posted inline: %v — the full text is attached)*\n\n[answer.md](%s)", cause, url)
	if _, err := post.Post(ctx, notice); err != nil {
		h.cfg.Logf("handler: rescued output to %s but could not announce it: %v", url, err)
		return
	}
	h.cfg.Logf("handler: posting failed (%v); rescued %d chars of output to %s", cause, len(transcript), url)
}

// failTurn reports an agent error into the topic instead of leaving a
// placeholder hanging forever.
//
// A superseded turn is not a fault and does not read as one: when a
// follow-up arrives the relay cancels the running turn on purpose, so
// "error: context canceled" would be noise pointing at nothing the
// user can act on.
//
// Whatever the suffix, the partial answer streamed so far is preserved:
// split.Close appends to it rather than replacing it, so a turn cut
// mid-flight keeps everything the agent had already said.
//
// The reason is read from the TURN CONTEXT's cause, not from cause
// itself. An agent that is cancelled mid-prompt reports it back as an
// ordinary JSON-RPC error — "context deadline exceeded", carrying no Go
// sentinel — so classifying on the returned error alone is exactly how
// a wedged turn came to surface as a bare, meaningless "*error: context
// deadline exceeded*".
func (h *Handler) failTurn(ctx context.Context, conv journal.Conv, split *rollover.Splitter, cause error) error {
	suffix := fmt.Sprintf("\n\n*error: %v*", cause)
	switch c := context.Cause(ctx); {
	case errors.Is(c, client.ErrNoProgress):
		suffix = fmt.Sprintf("\n\n*(stopped: no output and no tool activity from the agent for %s — it looks wedged)*",
			h.cfg.NoProgressTimeout)
	case errors.Is(c, client.ErrTurnCeiling):
		suffix = fmt.Sprintf("\n\n*(stopped: this turn hit the %s ceiling set by `prompt_timeout_seconds`)*",
			h.cfg.TurnCeiling)
	case errors.Is(c, context.Canceled):
		suffix = "\n\n*(superseded by your next message)*"
	}
	fctx := context.WithoutCancel(ctx)
	if err := split.Close(fctx, suffix); err != nil {
		h.cfg.Logf("handler: reporting error into %s: %v", conv.ID, err)
	}
	h.clearTail(conv.ID)
	return cause
}

// repostForNotify recreates the finished message chain as new messages
// so Zulip fires a push notification carrying the real answer.
//
// Zulip generates a mobile notification when a message is CREATED and
// never when one is edited, so the eager "Thinking…" placeholder that
// makes the web experience good is exactly what every phone shows.
// Re-posting after the answer is complete fixes the phone without
// changing the desktop stream at all.
//
// It runs AFTER split.Close, so what gets re-posted is the final
// content — outbox attachment links and any "(stopped: …)" note
// included — and never a half-streamed body.
//
// The cost, stated out loud so nobody reads it as a bug: an N-message
// chain now fires N notifications instead of one, because the whole
// chain is recreated to keep it in order. N is 1 for almost every turn.
//
// The circuit breaker is the important part. Delete and post ride the
// same permission surface: if the realm forbids the bot deleting its
// own messages (delete_own_message_policy) or the delete window has
// closed, every turn would post a fresh copy and fail to retract the
// old one — permanently doubled output in every topic. So the FIRST
// refused delete disables reposting for the lifetime of the process,
// loudly and exactly once, degrading to the pre-repost behaviour. The
// turn that trips it is the only one the user sees twice.
//
// The error path deliberately does NOT repost. The commonest failTurn
// is a superseded turn, and moving "(superseded by your next message)"
// to the bottom of the topic to ping a phone with it is pure noise.
func (h *Handler) repostForNotify(ctx context.Context, conv journal.Conv, split *rollover.Splitter) {
	if !h.cfg.RepostOnClose || h.repostBroken.Load() {
		return
	}
	err := split.Repost(ctx)
	// The ids moved even when a delete failed: the new messages are
	// live and they are what any later edit must address.
	h.trackTail(conv.ID, split)
	if err == nil {
		return
	}
	if errors.Is(err, rollover.ErrRetract) && !h.repostBroken.Swap(true) {
		h.cfg.Logf("handler: DISABLING end-of-turn repost for the rest of this process: the bot could not delete its own message (%v). "+
			`Push notifications will read "Thinking..." again; grant the bot permission to delete its own messages, `+
			`or set "repost_on_close": false to silence this.`, err)
		return
	}
	h.cfg.Logf("handler: re-posting the answer in %s: %v", conv.ID, err)
}

// trackTail records the message the relay is currently streaming into,
// so a crash mid-turn can be reported on the next start.
//
// It also indexes that message id against the conversation, which is
// what lets a reaction on the relay's LAST message resolve without an
// API call — the tail is the only id indexed, so a reaction on an
// earlier message of a multi-message answer still costs a lookup. The
// journal's tail is cleared when the turn ends — it means
// "interrupted", not "mine" — so what remembers a FINISHED turn's
// message is the in-memory index and, across a restart,
// Conv.LastOwnID (see rememberOwn).
func (h *Handler) trackTail(convID string, split *rollover.Splitter) {
	if id := split.TailID(); id != 0 {
		h.rememberOwn(convID, id)
		if err := h.cfg.Journal.SetTail(convID, id); err != nil {
			h.cfg.Logf("handler: recording tail for %s: %v", convID, err)
		}
	}
}

// rememberOwn records a message the relay posted: which conversation
// owns it, and whether it is that conversation's newest.
//
// The "newest" half is compared rather than assigned, because the two
// writers are not ordered with respect to each other — a turn's tail
// and an out-of-band post (an `!opts` panel, an archive warning) can
// interleave. Zulip message ids are monotonic, so max is the answer.
//
// It is also written THROUGH to the journal, which is what makes the
// archive gesture survive a restart or a reload. The journal applies
// the same max rule, and a write of an id it already holds is a no-op,
// so the streaming path does not touch the disk on every flush.
func (h *Handler) rememberOwn(convID string, msgID int64) {
	h.ownMsgs.put(msgID, convID)
	h.lastOwnMu.Lock()
	if msgID > h.lastOwn[convID] {
		h.lastOwn[convID] = msgID
	}
	h.lastOwnMu.Unlock()
	if err := h.cfg.Journal.SetLastOwn(convID, msgID); err != nil {
		h.cfg.Logf("handler: recording last own message for %s: %v", convID, err)
	}
}

// cachedOwn returns the in-memory answer, or 0 when this process has
// posted nothing in the conversation since it started.
func (h *Handler) cachedOwn(convID string) int64 {
	h.lastOwnMu.Lock()
	defer h.lastOwnMu.Unlock()
	return h.lastOwn[convID]
}

// lastOwnMessage returns the newest message the relay has posted in a
// conversation, or 0 if there is none.
//
// Three steps, stopping at the first answer:
//
//  1. the in-memory cache — free, and correct for anything this
//     process posted;
//  2. the journal's persisted record — free, and what makes the
//     gesture work on a conversation the relay last posted in before
//     the current process existed. lastOwn used to be the whole
//     answer, which meant the react-to-archive gesture silently
//     stopped working on every existing topic at every restart AND
//     every reload;
//  3. ONE bounded API read — newest message in this topic sent by the
//     bot, limit 1 — cached back into both. It runs only for a
//     :wastebasket: that matched nothing else, so its cost is bounded
//     by how often somebody taps a trash can.
//
// A RETIRED conversation resolves to nothing, and that check comes
// FIRST — before even the in-memory cache. `!new` retires without
// going through endSession, so a warm cache entry can outlive the
// conversation it describes; a conversation that is over must own no
// message by whichever path it is asked about.
//
// Channel conversations only for step 3 — the only caller excludes
// direct messages, which have no topic to archive.
func (h *Handler) lastOwnMessage(ctx context.Context, conv journal.Conv) int64 {
	rec, ok := h.cfg.Journal.LookupID(conv.ID)
	if !ok || rec.Retired {
		return 0
	}
	if id := h.cachedOwn(conv.ID); id != 0 {
		return id
	}
	if rec.LastOwnID != 0 {
		h.rememberOwn(conv.ID, rec.LastOwnID)
		return rec.LastOwnID
	}
	return h.fetchLastOwn(ctx, conv)
}

// fetchLastOwn is step 3: ask the server which message in this topic
// the bot itself posted last.
//
// A failure is not an error the user hears about — it degrades to
// "nothing is armed", exactly as an unknown message always has. The
// sender narrow is belt and braces: the answer is checked against the
// bot's own user id before it is trusted, because arming a
// destructive gesture on somebody else's message would be the one
// unrecoverable way to get this wrong.
//
// Deliberately NOT behind allowReactionLookup, the token bucket in
// front of GET /messages/{id}. That bucket exists because ANY reaction
// on ANY message in the realm can reach that lookup, so a flood needs
// a ceiling. This read is reached only by a :wastebasket: on a message
// that already resolved to a served, engaged conversation, and a
// success is cached in memory and on disk — and a refused read here
// would mean a destructive control that silently stops working under
// load, which is worse than the read.
func (h *Handler) fetchLastOwn(ctx context.Context, conv journal.Conv) int64 {
	narrow := append(zulipproto.TopicNarrow(conv.StreamID, conv.Topic), zulipproto.SenderNarrow(h.cfg.BotUserID))
	msgs, err := h.cfg.Client.Messages(ctx, narrow, 1, 0)
	if err != nil {
		h.cfg.Logf("handler: looking up my last message in %s: %v", h.describe(conv.Key), err)
		return 0
	}
	if len(msgs) == 0 || msgs[len(msgs)-1].SenderID != h.cfg.BotUserID {
		return 0
	}
	id := msgs[len(msgs)-1].ID
	h.rememberOwn(conv.ID, id)
	return id
}

// forgetOwn drops a conversation's last-message record — from memory
// AND from the journal. Called from endSession and from `!new`, i.e.
// wherever a conversation ENDS: the entry would otherwise outlive
// everything it refers to, and a gesture on a message of a
// conversation that is over must find nothing rather than something
// stale. Clearing only the map would leave the persisted id to hand
// the same stale answer back on the next lookup.
func (h *Handler) forgetOwn(convID string) {
	h.lastOwnMu.Lock()
	delete(h.lastOwn, convID)
	h.lastOwnMu.Unlock()
	if err := h.cfg.Journal.SetLastOwn(convID, 0); err != nil {
		h.cfg.Logf("handler: clearing last own message for %s: %v", convID, err)
	}
}

// clearTail forgets the tail message for a conversation, so a later
// restart does not mark a finished turn as interrupted.
//
// Benign race, deliberately not locked: a turn that has just been
// superseded can clear the tail its replacement already recorded. The
// cost is one missed "interrupted" marker if the relay dies inside
// that window; the replacement's own watchdog re-records the tail on
// its next flush. Serialising it would mean holding a lock across the
// whole turn for no correctness gain.
func (h *Handler) clearTail(convID string) {
	if err := h.cfg.Journal.SetTail(convID, 0); err != nil {
		h.cfg.Logf("handler: clearing tail for %s: %v", convID, err)
	}
}

// MarkInterrupted annotates every message the relay was streaming into
// when it last stopped. A restart kills the child agent, so the turn is
// dead; saying so beats leaving a truncated answer that looks complete.
//
// It never attempts to continue a half-streamed message.
func (h *Handler) MarkInterrupted(ctx context.Context) {
	for _, c := range h.cfg.Journal.OpenTails() {
		m, err := h.cfg.Client.GetMessage(ctx, c.TailID)
		if err != nil {
			h.cfg.Logf("handler: reading interrupted message %d: %v", c.TailID, err)
			h.clearTail(c.ID)
			continue
		}
		// A sealed message is finished by definition and must never be
		// edited again.
		if !strings.Contains(m.Content, strings.TrimSpace(h.sealMarker())) &&
			!strings.Contains(m.Content, strings.TrimSpace(InterruptedMarker)) {
			if err := h.cfg.Client.EditMessage(ctx, c.TailID, m.Content+InterruptedMarker); err != nil {
				h.cfg.Logf("handler: marking message %d interrupted: %v", c.TailID, err)
			} else {
				h.cfg.Logf("handler: marked interrupted turn in %s (message %d)", c.ID, c.TailID)
			}
		}
		h.clearTail(c.ID)
	}
}

func (h *Handler) sealMarker() string {
	if h.cfg.SealMarker == "" {
		return rollover.DefaultSealMarker
	}
	return h.cfg.SealMarker
}

// uploadOutbox uploads every regular file the agent left in
// <cwd>/outbox/ and returns the markdown to append to the answer.
//
// End of turn only: uploading opportunistically would race the agent
// still writing the file. Uploaded files move to outbox/.sent/ so a
// follow-up turn does not re-upload them.
func (h *Handler) uploadOutbox(ctx context.Context, cwd string) string {
	dir := filepath.Join(cwd, OutboxDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			h.cfg.Logf("handler: reading outbox %s: %v", dir, err)
		}
		return ""
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	var links []string
	for _, name := range names {
		url, err := h.uploadOne(ctx, dir, name)
		if err != nil {
			h.cfg.Logf("handler: uploading %s: %v", name, err)
			continue
		}
		links = append(links, "- ["+name+"]("+url+")")
	}
	if len(links) == 0 {
		return ""
	}
	return "\n\n**Attachments:**\n" + strings.Join(links, "\n")
}

func (h *Handler) uploadOne(ctx context.Context, dir, name string) (string, error) {
	path := filepath.Join(dir, name)
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	// Peek the head for sniffing and then upload the *bufio.Reader, so
	// the peeked bytes are not lost. Peek's error is dropped on purpose:
	// a short or empty file yields fewer bytes and io.EOF, which sniffs
	// fine, and a genuine I/O error resurfaces on the next fill during
	// the upload copy, where it is reported as the upload failing.
	br := bufio.NewReaderSize(f, 512)
	head, _ := br.Peek(512)
	url, err := h.cfg.Client.Upload(ctx, name, zulipproto.ContentType(name, head), br)
	// Close on a file opened for reading has nothing to flush, so its
	// error carries no information the upload result does not.
	_ = f.Close()
	if err != nil {
		return "", err
	}
	sent := filepath.Join(dir, sentDir)
	if err := os.MkdirAll(sent, 0o755); err != nil {
		return "", err
	}
	if err := os.Rename(path, filepath.Join(sent, name)); err != nil {
		return "", err
	}
	return url, nil
}

// propagateOne is Zulip's propagate_mode for "move exactly this
// message" — the only mode autotopic may use: the messages already in
// general chat belong to other people's conversations.
const propagateOne = "change_one"

// isLobby reports whether m arrived in an autotopic channel's general
// chat — which the relay treats as a LOBBY, never a conversation.
//
// General chat reaches us as "" or, without the empty_topic_name
// capability, as the display name; see autotopic.IsGeneralChat.
func (h *Handler) isLobby(m *zulipproto.Message) bool {
	return !m.IsDM() && autotopic.IsGeneralChat(m.Topic) && h.cfg.Channels.Autotopic(m.StreamID)
}

// autotopic moves a general-chat message into a topic named after it,
// returning the key the conversation should be allocated under. It is
// called only for a lobby message (see isLobby).
//
// ANY failure (realm policy, an older server, a transport error) is
// logged and yields the original key: the relay then answers in
// general chat exactly as it did before, and the turn is never dropped
// for a cosmetic reason. That fallback is per-message — it must never
// latch the feature off for the channel.
func (h *Handler) autotopic(ctx context.Context, m *zulipproto.Message, key journal.Key, text string) journal.Key {
	topic := autotopic.NameAt(h.promptText(text), h.now())
	// A generated name is a heuristic, so two people opening general
	// chat with "hi" would land in ONE conversation — sharing a
	// session, a cwd and each other's context. The message id breaks
	// the tie.
	if _, taken := h.cfg.Journal.Lookup(journal.Channel(m.StreamID, topic)); taken {
		topic = autotopic.Disambiguate(topic, m.ID)
	}
	if err := h.cfg.Client.MoveMessage(ctx, m.ID, topic, propagateOne); err != nil {
		h.cfg.Logf("handler: autotopic: moving message %d to topic %q failed (%v) — answering in general chat", m.ID, topic, err)
		return key
	}
	h.cfg.Logf("handler: autotopic: moved message %d from general chat to topic %q", m.ID, topic)
	return journal.Channel(m.StreamID, topic)
}

// mentioned reports whether raw markdown addresses the bot. Zulip
// renders a mention as @**Full Name**, optionally disambiguated with
// the user id, and a silent mention as @_**Full Name**_.
func (h *Handler) mentioned(text string) bool {
	for _, tok := range h.mentionTokens() {
		if strings.Contains(text, tok) {
			return true
		}
	}
	return false
}

// stripMention removes the mention token so the agent sees the message
// the human meant, not the addressing syntax.
func (h *Handler) stripMention(text string) string {
	for _, tok := range h.mentionTokens() {
		text = strings.ReplaceAll(text, tok, "")
	}
	return strings.TrimSpace(text)
}

// promptText is the message with the addressing syntax removed. A
// message that is NOTHING but a mention falls back to the raw text, so
// the agent is never handed an empty prompt.
func (h *Handler) promptText(text string) string {
	if s := h.stripMention(text); s != "" {
		return s
	}
	return text
}

func (h *Handler) mentionTokens() []string {
	name := h.cfg.BotFullName
	if name == "" {
		return nil
	}
	id := strconv.FormatInt(h.cfg.BotUserID, 10)
	return []string{
		"@**" + name + "|" + id + "**",
		"@_**" + name + "|" + id + "**_",
		"@**" + name + "**",
		"@_**" + name + "**_",
	}
}

// describe renders a conversation key for the operator log, resolving
// the channel name the allowlist knows.
func (h *Handler) describe(k journal.Key) string {
	if k.IsDM() {
		return k.Label()
	}
	name, ok := h.cfg.Channels.Name(k.StreamID)
	if !ok {
		return k.Label()
	}
	return fmt.Sprintf("#%s > %q", name, k.Topic)
}

// --- inflight bookkeeping ------------------------------------------------

// cancelInflight stops the turn running for convID, if any, and
// reports whether there was one. `!stop` uses that answer to tell the
// difference between interrupting something and doing nothing.
func (h *Handler) cancelInflight(ctx context.Context, convID string) bool {
	h.inflightMu.Lock()
	e, ok := h.inflight[convID]
	if ok {
		delete(h.inflight, convID)
		h.inflightCond.Broadcast()
	}
	h.inflightMu.Unlock()
	if ok {
		e.cancel()
		h.cfg.Sessions.Cancel(ctx, convID)
	}
	return ok
}

// isInflight reports whether a turn is running for convID.
func (h *Handler) isInflight(convID string) bool {
	h.inflightMu.Lock()
	defer h.inflightMu.Unlock()
	_, ok := h.inflight[convID]
	return ok
}

func (h *Handler) setInflight(convID string, e *inflightEntry) {
	h.inflightMu.Lock()
	h.inflight[convID] = e
	h.inflightMu.Unlock()
}

func (h *Handler) clearInflight(convID string, e *inflightEntry) {
	h.inflightMu.Lock()
	if cur, ok := h.inflight[convID]; ok && cur == e {
		delete(h.inflight, convID)
		h.inflightCond.Broadcast()
	}
	h.inflightMu.Unlock()
}

// WaitIdle blocks until no turn is in flight or ctx is done. Used for
// graceful shutdown and to synchronise tests without polling.
func (h *Handler) WaitIdle(ctx context.Context) error {
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
		case <-stop:
			return
		}
		h.inflightMu.Lock()
		h.inflightCond.Broadcast()
		h.inflightMu.Unlock()
	}()
	h.inflightMu.Lock()
	defer h.inflightMu.Unlock()
	for len(h.inflight) > 0 && ctx.Err() == nil {
		h.inflightCond.Wait()
	}
	return ctx.Err()
}

// --- background loops ----------------------------------------------------

// watchdog publishes pending splitter state on a fixed tick. This is
// the ONLY place a streaming edit is issued; the sink itself never
// performs I/O, so a slow Zulip edit cannot back-pressure the ACP
// stream.
func watchdog(ctx context.Context, split *rollover.Splitter, period time.Duration, after func()) {
	t := time.NewTicker(period)
	defer t.Stop()
	watchdogLoop(ctx, split, t.C, after)
}

// watchdogLoop is the testable core: it takes the tick channel, so a
// test can drive every branch by hand instead of racing a real ticker
// — a branch covered only when the timing happens to suit is a 100%
// gate that fails at random.
func watchdogLoop(ctx context.Context, split *rollover.Splitter, tick <-chan time.Time, after func()) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick:
			if !split.Pending() {
				continue
			}
			if err := split.Flush(context.WithoutCancel(ctx)); err != nil {
				return
			}
			after()
		}
	}
}

// showWorking puts up the relay's "I am on it" signal for a turn, in
// whichever form the mode allows.
//
// Streaming: an eager placeholder message, optionally animated. It is
// a real message that the answer is then edited into, so the user sees
// text appear as it arrives.
//
// Quiet (BatchEdits): NOTHING is posted. A placeholder would be a
// second created message, and Zulip pushes on creation only — the
// whole point of quiet mode is that the single message created at
// Close is the one the push carries. The typing indicator says the
// relay is working instead, and the ack reaction remains the durable
// "seen it" marker.
//
// turnCtx bounds the posted placeholder; workCtx bounds the background
// signal, and is cancelled the moment the agent's prompt returns.
func (h *Handler) showWorking(turnCtx, workCtx context.Context, conv journal.Conv, split *rollover.Splitter, sink *streamingSink) {
	if h.cfg.BatchEdits {
		h.startTyping(workCtx, conv.Key)
		return
	}
	if err := split.Start(turnCtx, statusline.Thinking(sink.Status())); err != nil {
		// Non-fatal: the first real chunk will post instead.
		h.cfg.Logf("handler: placeholder post failed: %v", err)
	}
	h.trackTail(conv.ID, split)
	h.startSpinner(workCtx, split, sink)
}

// startSpinner animates the placeholder for this turn, unless the
// animation is disabled — in which case NO goroutine is started and
// the placeholder is left exactly as posted. That is the whole point:
// a disabled spinner must cost zero edits, not slower ones.
func (h *Handler) startSpinner(ctx context.Context, split *rollover.Splitter, sink *streamingSink) {
	period := *h.cfg.SpinnerInterval
	if period <= 0 {
		return
	}
	go spinner(ctx, split, sink, period)
}

// spinner animates the placeholder until the first real chunk lands,
// self-disarming on the splitter's alive=false signal.
func spinner(ctx context.Context, split *rollover.Splitter, sink *streamingSink, period time.Duration) {
	t := time.NewTicker(period)
	defer t.Stop()
	spinnerLoop(ctx, split, sink, t.C)
}

// spinnerLoop is the testable core; see watchdogLoop.
func spinnerLoop(ctx context.Context, split *rollover.Splitter, sink *streamingSink, tick <-chan time.Time) {
	frames := []string{".", "..", "..."}
	for i := 0; ; i++ {
		select {
		case <-ctx.Done():
			return
		case <-tick:
			frame := statusline.Spinner(sink.Status(), frames[i%len(frames)])
			alive, _ := split.UpdatePlaceholder(context.WithoutCancel(ctx), frame)
			if !alive {
				return
			}
		}
	}
}

// --- poster --------------------------------------------------------------

// convPoster binds the splitter's dumb Poster interface to one Zulip
// conversation, channel topic or DM. The ONLY decision it makes is
// which send endpoint the key implies; it never decides anything about
// content, which is the whole point of keeping split logic out of the
// HTTP layer. Rollover and the streaming edit path therefore work on a
// DM unchanged — an edit is a PATCH on a message id and does not care
// how the message was addressed.
type convPoster struct {
	client Poster
	key    journal.Key
}

func (p *convPoster) Post(ctx context.Context, content string) (int64, error) {
	if p.key.IsDM() {
		return p.client.SendDirectMessage(ctx, p.key.UserIDs, content)
	}
	return p.client.SendMessage(ctx, p.key.StreamID, p.key.Topic, content)
}

func (p *convPoster) Edit(ctx context.Context, id int64, content string) error {
	return p.client.EditMessage(ctx, id, content)
}

// Delete implements rollover.Deleter, which the splitter uses to
// retract the placeholder-seeded chain at the end of a turn once the
// final content has been re-posted as new messages.
func (p *convPoster) Delete(ctx context.Context, id int64) error {
	return p.client.DeleteMessage(ctx, id)
}

// PostWidget is Post with a zform widget attached. Only the `!opts`
// panel uses it; the splitter never does, because a widget on a
// streamed answer would be re-rendered on every edit.
func (p *convPoster) PostWidget(ctx context.Context, content, widget string) (int64, error) {
	if p.key.IsDM() {
		return p.client.SendDirectMessageWidget(ctx, p.key.UserIDs, content, widget)
	}
	return p.client.SendMessageWidget(ctx, p.key.StreamID, p.key.Topic, content, widget)
}
