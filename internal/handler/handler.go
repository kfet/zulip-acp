// Package handler turns inbound Zulip events into ACP prompts and
// streams the agent's answer back into the originating topic.
//
// The unit of conversation is a Zulip TOPIC, scoped by its channel.
// The topic string itself is never used as a key — see internal/journal
// for why — so everything here works in terms of the journal's stable
// conv-id.
package handler

import (
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
	"time"

	acp "github.com/coder/acp-go-sdk"

	"github.com/kfet/acp-kit/client"
	"github.com/kfet/acp-kit/state"
	"github.com/kfet/zulip-acp/internal/journal"
	"github.com/kfet/zulip-acp/internal/rollover"
	"github.com/kfet/zulip-acp/internal/statusline"
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

// spinnerInterval animates the "Thinking…" placeholder. Zulip sustains
// ~15 edits/sec, so this is purely a readability choice.
const spinnerInterval = 900 * time.Millisecond

// Agent is the subset of *client.AgentProc the handler drives
// directly. Session lifecycle lives in Sessions.
type Agent interface {
	Prompt(ctx context.Context, sid acp.SessionId, prompt []acp.ContentBlock) (acp.StopReason, error)
	Models() (models []client.ModelInfo, currentID string)
	// SetModel selects the model for one session. It backs `!model
	// <id>`; the relay never calls it unless a user asked for a
	// specific model.
	SetModel(ctx context.Context, sid acp.SessionId, modelID string) error
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
}

// Poster is the Zulip surface the handler writes to.
type Poster interface {
	SendMessage(ctx context.Context, streamID int64, topic, content string) (int64, error)
	SendDirectMessage(ctx context.Context, userIDs []int64, content string) (int64, error)
	EditMessage(ctx context.Context, id int64, content string) error
	GetMessage(ctx context.Context, id int64) (zulipproto.Message, error)
	Upload(ctx context.Context, filename string, r io.Reader) (string, error)
	AddReaction(ctx context.Context, messageID int64, emoji string) error
	RemoveReaction(ctx context.Context, messageID int64, emoji string) error
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

	// PromptTimeout caps one agent turn.
	PromptTimeout time.Duration
	// EditInterval coalesces streaming edits.
	EditInterval time.Duration

	// Budget, SealMarker and ContinuationMarker configure the splitter.
	Budget             int
	SealMarker         string
	ContinuationMarker string

	// SilentSentinel lets the agent decline to answer an ambient turn.
	SilentSentinel string
	// HideThinking suppresses thought chunks on the streamed path.
	HideThinking bool

	// AckEmoji is the emoji reaction placed on the triggering message
	// for the duration of a turn. Empty disables the acknowledgement.
	AckEmoji string

	// Logf receives operational messages.
	Logf func(format string, args ...any)
}

// inflightEntry wraps a cancel func with its own identity, so clearing
// can tell its entry from one a follow-up has since installed.
type inflightEntry struct{ cancel context.CancelFunc }

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
	// state that no longer exists.
	modelMu      sync.Mutex
	modelChoices map[string]modelChoice
}

// New constructs a Handler.
func New(cfg Config) (*Handler, error) {
	if cfg.Client == nil || cfg.Agent == nil || cfg.Sessions == nil || cfg.Journal == nil {
		return nil, fmt.Errorf("handler: Client, Agent, Sessions and Journal are all required")
	}
	if cfg.Channels == nil {
		return nil, fmt.Errorf("handler: Channels is required — a relay with no channel allowlist would answer the whole realm")
	}
	if cfg.PromptTimeout <= 0 {
		cfg.PromptTimeout = 10 * time.Minute
	}
	if cfg.EditInterval <= 0 {
		cfg.EditInterval = 300 * time.Millisecond
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	h := &Handler{cfg: cfg, inflight: map[string]*inflightEntry{}, modelChoices: map[string]modelChoice{}}
	h.inflightCond = sync.NewCond(&h.inflightMu)
	return h, nil
}

// Handle is the zulipproto.EventHandler entry point.
func (h *Handler) Handle(ctx context.Context, ev zulipproto.Event) {
	switch ev.Type {
	case zulipproto.EventMessage:
		h.handleMessage(ctx, ev.Message)
	case zulipproto.EventUpdateMessage:
		h.handleUpdate(ev)
	}
}

// handleUpdate migrates a conversation when its topic is renamed.
// Missing this costs a spurious duplicate session: the same agent
// session would keep running under the old name while a fresh one was
// created under the new one.
//
// There is no DM analogue and this path must never touch one: a direct
// message has no topic to rename, and its conv key lives in a disjoint
// namespace, so Journal.Rename could not match one even if it were
// called. The StreamID guard below makes that explicit rather than
// incidental — Zulip sends no stream id on a DM update event.
func (h *Handler) handleUpdate(ev zulipproto.Event) {
	if ev.OrigTopic == "" || ev.Topic == "" || ev.OrigTopic == ev.Topic {
		return
	}
	if ev.StreamID == 0 {
		return
	}
	// A rename in a channel that has since left the served set is
	// dropped. The topic is truth and the journal is a cache, so the
	// worst case is a stale entry the next message in the new topic
	// supersedes.
	if _, ok := h.cfg.Channels.Name(ev.StreamID); !ok {
		return
	}
	c, moved, err := h.cfg.Journal.Rename(ev.StreamID, ev.OrigTopic, ev.Topic)
	if err != nil {
		h.cfg.Logf("handler: topic rename %q → %q: %v", ev.OrigTopic, ev.Topic, err)
		return
	}
	if moved {
		h.cfg.Logf("handler: topic renamed %q → %q, session %s follows it", ev.OrigTopic, ev.Topic, c.ID)
	}
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
	} else {
		// The channel allowlist gates channel messages only. A DM is
		// in no channel, so there is nothing here to measure it
		// against; AllowedUsers below is what gates it.
		if _, ok := h.cfg.Channels.Name(m.StreamID); !ok {
			return
		}
		key, addressed = journal.Channel(m.StreamID, m.Topic), h.mentioned(text)
	}

	if h.cfg.AllowedUsers != nil {
		if _, ok := h.cfg.AllowedUsers[m.SenderID]; !ok {
			h.cfg.Logf("handler: dropping message %d from user %d (not allowed)", m.ID, m.SenderID)
			return
		}
	}

	existing, engaged := h.cfg.Journal.Lookup(key)
	if !addressed && !engaged {
		return
	}

	// Commands are parsed AFTER every guard above and BEFORE any
	// conversation is allocated, so `!help` in a topic the relay has
	// never answered in leaves no state behind. A command consumes the
	// message; nothing here reaches the agent.
	prompt, handled := h.dispatch(ctx, m, key, existing, engaged, h.promptText(text))
	if handled {
		return
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
	}

	prompt = "[" + m.SenderName + "] " + prompt

	// A follow-up supersedes whatever is still running in this topic.
	h.cancelInflight(ctx, conv.ID)
	pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), h.cfg.PromptTimeout)
	entry := &inflightEntry{cancel: cancel}
	h.setInflight(conv.ID, entry)
	go func() {
		defer h.clearInflight(conv.ID, entry)
		defer cancel()
		if err := h.run(pctx, conv, prompt, addressed, m.ID); err != nil {
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
//     immediately — Zulip has no typing indicator, so it is the first
//     thing the user sees while a cold agent starts — and the answer
//     is edited in as it arrives.
//   - Ambient (a follow-up in an engaged topic, with a sentinel
//     configured): buffer, because the agent may decline and a message
//     that appears and then vanishes is worse on a phone than one that
//     arrives a beat later. The placeholder is NOT withheld until the
//     end of the turn, though: sentinelWatch posts it the moment the
//     streamed text can no longer become the sentinel, which is
//     usually the first chunk. The answer itself still lands via the
//     normal end-of-turn commit.
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
		if err := split.Start(ctx, statusline.Thinking(sink.Status())); err != nil {
			// Non-fatal: the first real chunk will post instead.
			h.cfg.Logf("handler: placeholder post failed: %v", err)
		}
		h.trackTail(conv.ID, split)
		go spinner(wctx, split, sink, spinnerInterval)
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
			if err := split.Start(ctx, statusline.Thinking(sink.Status())); err != nil {
				h.cfg.Logf("handler: placeholder post failed: %v", err)
			}
			h.trackTail(conv.ID, split)
			go spinner(wctx, split, sink, spinnerInterval)
		}}
	}
	sess, err = h.cfg.Sessions.GetOrCreate(ctx, conv.ID, sinkFor)
	if err != nil {
		wcancel()
		_ = split.Close(context.WithoutCancel(ctx), fmt.Sprintf("\n*error: %v*", err))
		h.trackTail(conv.ID, split)
		h.clearTail(conv.ID)
		return err
	}
	if _, currentID := h.cfg.Agent.Models(); currentID != "" {
		sink.SetProviderEmoji(statusline.ProviderEmojiForModel(currentID))
	}

	sess.Mu.Lock()
	defer sess.Mu.Unlock()
	h.cfg.Sessions.Touch(sess)
	h.applyModel(ctx, conv.ID, sess.SessionID)

	text := prompt
	if prefix := h.cfg.Sessions.TakePendingSystemPrompt(sess); prefix != "" {
		text = prefix + "\n\n" + text
	}
	blocks := []acp.ContentBlock{acp.TextBlock(text)}

	go watchdog(wctx, split, h.cfg.EditInterval, func() { h.trackTail(conv.ID, split) })

	var stop acp.StopReason
	if abstaining {
		res, perr := client.PromptAbstainable(ctx, h.cfg.Agent, sess.SessionID, blocks, vs, h.cfg.SilentSentinel)
		wcancel()
		if perr != nil {
			return h.failTurn(ctx, conv, split, perr)
		}
		if res.Abstained {
			h.cfg.Logf("handler: agent abstained in %s", conv.ID)
			h.clearTail(conv.ID)
			return nil
		}
		stop = res.Stop
	} else {
		stop, err = h.cfg.Agent.Prompt(ctx, sess.SessionID, blocks)
		wcancel()
		if err != nil {
			return h.failTurn(ctx, conv, split, err)
		}
	}

	fctx := context.WithoutCancel(ctx)
	suffix := h.uploadOutbox(fctx, sess.Cwd)
	if stop != "" && stop != acp.StopReasonEndTurn {
		suffix += fmt.Sprintf("\n\n*(stopped: %s)*", stop)
	}
	cerr := split.Close(fctx, suffix)
	if cerr != nil {
		h.rescue(fctx, post, split.Transcript(), cerr)
	}
	h.clearTail(conv.ID)
	return cerr
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
	url, err := h.cfg.Client.Upload(ctx, "answer.md", strings.NewReader(transcript))
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
func (h *Handler) failTurn(ctx context.Context, conv journal.Conv, split *rollover.Splitter, cause error) error {
	suffix := fmt.Sprintf("\n\n*error: %v*", cause)
	if errors.Is(cause, context.Canceled) {
		suffix = "\n\n*(superseded by your next message)*"
	}
	fctx := context.WithoutCancel(ctx)
	if err := split.Close(fctx, suffix); err != nil {
		h.cfg.Logf("handler: reporting error into %s: %v", conv.ID, err)
	}
	h.clearTail(conv.ID)
	return cause
}

// trackTail records the message the relay is currently streaming into,
// so a crash mid-turn can be reported on the next start.
func (h *Handler) trackTail(convID string, split *rollover.Splitter) {
	if id := split.TailID(); id != 0 {
		if err := h.cfg.Journal.SetTail(convID, id); err != nil {
			h.cfg.Logf("handler: recording tail for %s: %v", convID, err)
		}
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
	url, err := h.cfg.Client.Upload(ctx, name, f)
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
