// This file is the react-to-archive control: a way to END a
// conversation and get its topic out of the way, without deleting
// anything and without the agent's involvement.
//
// # What archiving is
//
// The topic is MOVED to a channel the relay does not serve
// (`archive_channel`), with propagate_mode=change_all — one PATCH, the
// whole topic, messages intact. It is deliberately none of the
// alternatives:
//
//   - not DELETE /streams/{id}/delete_topic, which is org-admin only:
//     a relay bot must not hold realm admin for a convenience feature;
//   - not per-message deletion, which is O(n) API calls with a
//     partial-failure state in the middle;
//   - not "close the session and leave the topic", which leaves the
//     junk exactly where it was.
//
// An unserved destination is outside the channel allowlist BY
// CONSTRUCTION, so an archived topic cannot re-engage the relay, and
// recovery is one move back. What accumulates there is then a retention
// policy problem, not this relay's.
//
// Nothing on disk is touched: state/convs/<id>/ survives. Journal.Retire
// keeps the old conv-id and mints a NEW one for the key, so a later
// topic of the same name cannot resurrect the old memory — the state
// stays on disk without being reachable.
//
// # Two entry points, one code path
//
//   - REACTION: :wastebasket: on the relay's own LAST message, by an
//     allowlisted human. It arrives through Config.ReactionTrigger,
//     which runs before the agent is involved at all — a destructive
//     control must never depend on the model choosing to call a tool.
//     "The relay's own last message" is resolved across a process
//     boundary — memory, then the journal, then one narrowed API read
//     — because an in-memory-only answer made the gesture stop working
//     on every existing topic at every restart and reload. See
//     Handler.lastOwnMessage.
//   - COMMAND: `!archive` (or `!arch`).
//
// Either ARMS a confirmation and posts a warning. The ONLY confirmation
// is :wastebasket: on that warning message. An expired arm is never
// carried forward: a later tap starts a fresh cycle with a fresh
// warning, because the alternative is a destructive act authorised by
// something the user has long since forgotten.
//
// # Why the ordering below is load-bearing
//
// handleUpdate migrates a conversation when its topic moves, and a
// cross-channel move arrives as that SAME update_message event. Move
// first and the session FOLLOWS the topic into the archive instead of
// ending. So the conversation is retired BEFORE the PATCH is issued,
// and the echoed event then finds nothing to migrate.
package handler

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/kfet/acp-kit/command"
	"github.com/kfet/zulip-acp/internal/journal"
	"github.com/kfet/zulip-acp/internal/zulipproto"
)

const (
	// archiveEmoji is the ONE emoji that arms and the one that
	// confirms. A single symbol for both halves is what makes the
	// gesture learnable: tap it, read the warning, tap it again.
	archiveEmoji = "wastebasket"

	// archiveVerb and archiveAlias are the typed forms. `!purge` is
	// deliberately NOT among them: the action is an archive — nothing
	// is deleted — and a destructive-sounding name for a reversible
	// move would be a lie in the direction that matters.
	archiveVerb  = "archive"
	archiveAlias = "arch"
	archiveHelp  = "- `" + command.DisplaySigil + archiveVerb + "` — end this conversation and move the topic to the archive channel (confirm with :" + archiveEmoji + ":)\n"

	// archiveConfirmTTL is how long an armed confirmation lives.
	//
	// Long enough to read the warning and decide, short enough that a
	// tap tomorrow cannot land on yesterday's arming. When it lapses
	// the arm is simply forgotten and logged; the next tap starts a
	// new cycle and posts a new warning.
	archiveConfirmTTL = 2 * time.Minute

	// archiveTimeout bounds the API calls one archive costs — a post,
	// a move, and at most one failure notice. It runs on the event
	// loop, so it must not be able to wedge intake.
	archiveTimeout = 60 * time.Second
)

// pendingArchive is one conversation's armed confirmation.
//
// promptID is the message the confirming reaction MUST land on:
// identity of the confirmation, not just its timing. A :wastebasket: on
// any other message — including the relay's newer messages — is not a
// confirmation.
type pendingArchive struct {
	promptID int64
	expires  time.Time
	who      string
}

// archiveEnabled reports whether the control is wired up at all. The
// relay resolves the destination channel and the realm's move
// permission at STARTUP and leaves ArchiveStreamID zero when either
// answer is no, so everything below can treat this as settled.
func (h *Handler) archiveEnabled() bool { return h.cfg.ArchiveStreamID != 0 }

// ArchiveReaction is what Config.ReactionTrigger is wired to in
// production. It reports whether it consumed the reaction.
//
// Everything it relies on has already been checked by handleReaction:
// the reaction is not the relay's own, not another bot's, the user is
// on the allowlist, and conv is a conversation the relay is engaged in
// and still serves. What is left is this file's own policy — the right
// emoji, an add rather than a remove, and the right message.
func (h *Handler) ArchiveReaction(ctx context.Context, conv journal.Conv, ev zulipproto.Event, _ *zulipproto.Message) bool {
	if !h.archiveEnabled() || ev.EmojiName != archiveEmoji {
		return false
	}
	// Un-reacting is not an action. Retracting a :wastebasket: must
	// mean "never mind", never "do it now".
	if ev.Op != zulipproto.ReactionAdd {
		return false
	}
	// A direct message has no topic and lives in no channel, so there
	// is nothing to move. The reaction stays ordinary signal and
	// reaches the agent.
	if conv.Key.IsDM() {
		return false
	}
	confirm := h.armedFor(conv.ID, ev.MessageID)
	last := ev.MessageID == h.lastOwnMessage(ctx, conv)
	if !confirm && !last {
		// Not the arming target and not the confirmation: an ordinary
		// reaction that happens to be a wastebasket.
		return false
	}
	// Only now is a name worth an API call — and it is also the last
	// bot check: BotSenderIDs is a startup snapshot, and a bot that
	// appeared since must not be able to archive a topic.
	who, isBot := h.reactor(ctx, ev.UserID)
	if isBot {
		return false
	}
	if confirm && h.takeArchiveConfirm(conv.ID, ev.MessageID) {
		h.archiveConversation(ctx, conv, who)
		return true
	}
	if !last {
		// A tap on a LAPSED warning that the conversation has since
		// moved past. The arm is gone and nothing is armed in its
		// place: a fresh cycle must be started from the message the
		// gesture is defined on, not from a warning buried in the
		// scrollback.
		return true
	}
	h.armArchive(ctx, conv, who)
	return true
}

// isArchiveCommand reports whether text is the bare `!archive` (or
// `!arch`) command.
//
// Strict like isOpts, and for a much sharper reason: an argument means
// the user meant something else, and "!archive the old design notes"
// must never archive THIS topic because a sentence began with the word.
// It falls through to the unknown-command panel, exactly as
// "!opts why is this slow" does.
func isArchiveCommand(text string) bool {
	body, ok := command.StripSigil(strings.TrimSpace(text))
	if !ok {
		return false
	}
	body = strings.TrimSpace(body)
	return strings.EqualFold(body, archiveVerb) || strings.EqualFold(body, archiveAlias)
}

// archiveCommand is the typed entry point. It arms exactly what the
// reaction arms — a confirmation that only a :wastebasket: on the
// posted warning can complete — so there is one destructive path, not
// two.
func (h *Handler) archiveCommand(ctx context.Context, key journal.Key, who string) {
	if !h.archiveEnabled() {
		h.reply(ctx, key, "Archiving is not configured on this relay: there is no archive channel it may move a topic to.")
		return
	}
	if key.IsDM() {
		h.reply(ctx, key, "A direct message has no topic to move, so it cannot be archived. `"+command.DisplaySigil+"new` starts a fresh conversation here instead.")
		return
	}
	conv, ok := h.cfg.Journal.Lookup(key)
	if !ok {
		h.reply(ctx, key, "There is no conversation here to archive yet.")
		return
	}
	h.armArchive(ctx, conv, who)
}

// armArchive posts the warning and records what would confirm it.
//
// The warning is posted BEFORE the arm is recorded: if the post fails
// there is nothing to confirm, and an arm nobody can see would be a
// trap — a later :wastebasket: on some unrelated message of ours would
// find it.
func (h *Handler) armArchive(ctx context.Context, conv journal.Conv, who string) {
	// Asking twice is not two archives. A live arm is left exactly as
	// it is: re-posting the warning would bury the message the
	// confirmation is bound to, and each re-arm would park another
	// expiry timer.
	if id, live := h.liveArm(conv.ID); live {
		h.reply(ctx, conv.Key, fmt.Sprintf("This topic is already waiting to be archived — react :%s: to the warning above (message %d) to confirm, or ignore it.", archiveEmoji, id))
		return
	}
	// Preflight BEFORE the warning. A warning that invites a
	// confirmation for a move the realm will refuse is the same trap
	// this file exists to avoid, one step earlier: the user taps twice
	// and is told no at the end.
	if ok, why := h.movable(ctx, conv.Key); !ok {
		h.reply(ctx, conv.Key, why)
		return
	}
	post := &convPoster{client: h.cfg.Client, key: conv.Key}
	id, err := post.Post(ctx, archiveWarning(who, h.cfg.ArchiveChannel))
	if err != nil {
		h.cfg.Logf("handler: could not post the archive confirmation in %s: %v", h.describe(conv.Key), err)
		return
	}
	h.rememberOwn(conv.ID, id)
	h.archiveMu.Lock()
	h.archivePending[conv.ID] = &pendingArchive{promptID: id, expires: h.now().Add(archiveConfirmTTL), who: who}
	h.archiveMu.Unlock()
	h.cfg.Logf("handler: %s armed an archive of %s (%s); confirm with :%s: on message %d within %s",
		who, h.describe(conv.Key), conv.ID, archiveEmoji, id, archiveConfirmTTL)
	go h.expireArchive(conv.ID, id)
}

// expireArchive forgets an arm that was never confirmed.
//
// The expiry is a real timer rather than a lazy check on the next tap,
// because "nothing happened" is exactly the case worth a log line: the
// operator should be able to see that someone started an archive and
// walked away. The timer is the injected one, so tests drive it rather
// than sleep.
func (h *Handler) expireArchive(convID string, promptID int64) {
	<-h.after(archiveConfirmTTL)
	h.archiveMu.Lock()
	p, ok := h.archivePending[convID]
	expired := ok && p.promptID == promptID
	if expired {
		delete(h.archivePending, convID)
	}
	h.archiveMu.Unlock()
	if expired {
		h.cfg.Logf("handler: archive of %s expired unconfirmed after %s — nothing was moved; a further :%s: starts a new cycle",
			convID, archiveConfirmTTL, archiveEmoji)
		if h.cfg.OnArchiveExpired != nil {
			h.cfg.OnArchiveExpired(convID)
		}
	}
}

// liveArm reports the message an UNLAPSED confirmation for convID is
// waiting on. A lapsed arm is not live: it is about to be forgotten by
// its own timer, and re-arming over it is exactly right.
func (h *Handler) liveArm(convID string) (int64, bool) {
	h.archiveMu.Lock()
	defer h.archiveMu.Unlock()
	p, ok := h.archivePending[convID]
	if !ok || !h.now().Before(p.expires) {
		return 0, false
	}
	return p.promptID, true
}

// armedFor reports whether msgID is the message an armed confirmation
// for convID is waiting on. It does not consume the arm.
func (h *Handler) armedFor(convID string, msgID int64) bool {
	h.archiveMu.Lock()
	defer h.archiveMu.Unlock()
	p, ok := h.archivePending[convID]
	return ok && p.promptID == msgID
}

// takeArchiveConfirm consumes the arm for convID and reports whether it
// authorises an archive NOW.
//
// A lapsed arm is consumed and refused, never honoured: the timer may
// not have fired yet, and an expired confirmation must never be carried
// forward into a destructive act. The caller then arms a fresh cycle,
// so the user's tap is not silently ignored — it produces a new warning
// to answer.
func (h *Handler) takeArchiveConfirm(convID string, msgID int64) bool {
	h.archiveMu.Lock()
	p, ok := h.archivePending[convID]
	if ok && p.promptID == msgID {
		delete(h.archivePending, convID)
	}
	h.archiveMu.Unlock()
	if !ok || p.promptID != msgID {
		return false
	}
	if !h.now().Before(p.expires) {
		h.cfg.Logf("handler: ignoring a late archive confirmation for %s: the arming from %s had expired — nothing was archived", convID, p.who)
		return false
	}
	return true
}

// dropArchiveArm forgets any arm for a conversation. Called when the
// conversation ends for some OTHER reason — it must not be possible for
// a confirmation to outlive the thing it was about.
func (h *Handler) dropArchiveArm(convID string) {
	h.archiveMu.Lock()
	delete(h.archivePending, convID)
	h.archiveMu.Unlock()
}

// movable reports whether the WHOLE topic can still be moved to the
// archive channel — and, when it cannot, the sentence to say instead.
//
// This is the preflight the archive would otherwise do by failing:
// Zulip evaluates move_messages_between_streams_limit_seconds against
// every message in the set being moved, so with change_all the OLDEST
// message in the topic decides for all of them. Discovering that at
// step 4 means a conversation already ended and retired for a move
// that never happened; discovering it here costs one narrowed read and
// changes nothing.
//
// "Cannot tell" is a refusal, exactly as it is at startup. A probe
// that errors is not evidence that the move would work, and the
// failure it would let through is the destructive one.
func (h *Handler) movable(ctx context.Context, key journal.Key) (bool, string) {
	policy := zulipproto.MovePolicy{Allowed: true, Limit: h.cfg.ArchiveMoveLimit}
	if policy.Limit <= 0 {
		// No limit applies to this bot: every topic is movable however
		// far back it reaches, and the read below would tell us
		// nothing worth an API call.
		return true, ""
	}
	oldest, ok, err := h.cfg.Client.OldestMessage(ctx, zulipproto.TopicNarrow(key.StreamID, key.Topic))
	if err != nil {
		h.cfg.Logf("handler: not archiving %s: could not read the oldest message in the topic to check the move time limit (%v)", h.describe(key), err)
		return false, "I could not check whether this topic is still young enough for me to move it, so I have not touched it. Try again, or move the topic by hand."
	}
	if !ok {
		h.cfg.Logf("handler: not archiving %s: the topic reads as empty, so there is nothing to anchor a move to", h.describe(key))
		return false, "I could not find any message in this topic to move, so I have not touched it."
	}
	if !policy.TooOld(time.Unix(oldest.Timestamp, 0), h.now()) {
		return true, ""
	}
	h.cfg.Logf("handler: not archiving %s: its oldest message (%d) is older than the realm's %s move limit — refusing before anything is changed",
		h.describe(key), oldest.ID, policy.Limit)
	return false, archiveTooOld(policy.Limit)
}

// archiveTooOld explains a refusal the user can actually act on: the
// two fixes are realm settings, not anything this relay can do, so the
// message names them rather than just apologising.
func archiveTooOld(limit time.Duration) string {
	return fmt.Sprintf("I cannot archive this topic: it reaches back further than %s, and this realm only lets me move messages sent within that window. **Nothing was changed** — the conversation is still live.\n\n"+
		"Two ways to lift it, both in organisation settings: set *moving messages to another channel* to **any time**, or give this bot the **moderator** role — moderators are exempt from the limit. Failing that, move the topic by hand.",
		humanLimit(limit))
}

// humanLimit renders a move limit the way the Zulip setting offers it
// — days, hours, minutes — rather than as Go's "168h0m0s".
func humanLimit(d time.Duration) string {
	plural := func(n int64, unit string) string {
		if n == 1 {
			return fmt.Sprintf("1 %s", unit)
		}
		return fmt.Sprintf("%d %ss", n, unit)
	}
	switch {
	case d >= 24*time.Hour && d%(24*time.Hour) == 0:
		return plural(int64(d/(24*time.Hour)), "day")
	case d >= time.Hour && d%time.Hour == 0:
		return plural(int64(d/time.Hour), "hour")
	case d >= time.Minute:
		return plural(int64(d/time.Minute), "minute")
	default:
		return d.String()
	}
}

// archiveConversation performs the archive. The ordering here is the
// whole feature; see the file comment.
func (h *Handler) archiveConversation(ctx context.Context, conv journal.Conv, who string) {
	// The served set moves underfoot when it follows the bot's
	// subscriptions, so "the destination is not served" is re-checked
	// at the moment of the act and not only at startup. Archiving INTO
	// a served channel would hand the relay its own junk back.
	if name, served := h.cfg.Channels.Name(h.cfg.ArchiveStreamID); served {
		h.cfg.Logf("handler: refusing to archive %s: the archive channel #%s is now in the served set", conv.ID, name)
		h.reply(ctx, conv.Key, "I cannot archive this topic: the archive channel is one I now serve, so moving the topic there would not end anything. Nothing was changed.")
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), archiveTimeout)
	defer cancel()

	// Re-run the preflight the arming already passed. Two minutes of
	// confirmation TTL is enough for the oldest message to cross the
	// limit, and this is the last moment at which a refusal is still
	// free: everything below either posts, ends or retires something.
	if ok, why := h.movable(ctx, conv.Key); !ok {
		h.reply(ctx, conv.Key, why)
		return
	}

	// The audit trail is posted FIRST so it travels with the topic:
	// after the move this message is in the archive channel, next to
	// the conversation it describes. It is also the anchor the move is
	// addressed to — Zulip has no move-topic endpoint, so a topic move
	// is an edit of some message IN the topic, and the one we just
	// posted is the one message certain to be there.
	post := &convPoster{client: h.cfg.Client, key: conv.Key}
	anchor, err := post.Post(ctx, archiveNotice(who, h.cfg.ArchiveChannel))
	if err != nil {
		h.cfg.Logf("handler: not archiving %s: could not post the closing message (%v) — nothing was changed", conv.ID, err)
		return
	}
	h.rememberOwn(conv.ID, anchor)

	// 1 & 2: the turn, then the ACP session. A turn still streaming
	// would keep posting into a topic that is about to move, and the
	// session must stop working on a conversation that is over.
	h.endSession(ctx, conv.ID)

	// 3: retire BEFORE the move. The move's own update_message event
	// would otherwise migrate this conversation into the archive
	// channel instead of ending it.
	prev, _, existed, err := h.cfg.Journal.Retire(conv.Key)
	if err != nil {
		h.cfg.Logf("handler: not archiving %s: retiring it failed (%v) — the topic stays where it is", conv.ID, err)
		h.reply(ctx, conv.Key, fmt.Sprintf("I could not record the end of this conversation (%v), so I did not move the topic. Nothing was changed.", err))
		return
	}
	if !existed {
		// Racing `!new` or a rename. The conversation this archive was
		// about is already gone, and moving the topic now would move
		// whatever took its place.
		h.cfg.Logf("handler: not archiving %s: it is no longer the conversation in %s", conv.ID, h.describe(conv.Key))
		return
	}

	// 4: and only now the move.
	if err := h.cfg.Client.MoveMessageToChannel(ctx, anchor, h.cfg.ArchiveStreamID, "", propagateAll); err != nil {
		h.cfg.Logf("handler: archiving %s: the move to #%s failed (%v) — the conversation is ended but the topic stayed put",
			prev.ID, h.cfg.ArchiveChannel, err)
		h.reply(ctx, conv.Key, fmt.Sprintf("I ended this conversation, but moving the topic to #%s failed (%v). The messages are all still here; move the topic by hand, or just leave it — the next message starts a fresh conversation.",
			h.cfg.ArchiveChannel, err))
		return
	}
	h.cfg.Logf("handler: %s archived %s (%s): the topic moved to #%s and the session ended; %s is untouched on disk",
		who, h.describe(conv.Key), prev.ID, h.cfg.ArchiveChannel, prev.ID)
}

// endSession stops everything running for a conversation: the relay's
// own turn, the agent's work on the ACP session, any buffered reactions
// and any archive arm.
//
// Sessions.Cancel is called unconditionally, not only when a turn was
// in flight: a scheduled prompt or a loopback call can have the agent
// busy on a session the relay is not currently holding a turn for. The
// session itself is not deleted — the state manager has no such call,
// and it does not need one: the conv-id is retired immediately after,
// so nothing can ever address the session again and the idle GC
// collects it. The working directory stays on disk either way.
func (h *Handler) endSession(ctx context.Context, convID string) {
	h.cancelInflight(ctx, convID)
	h.cfg.Sessions.Cancel(ctx, convID)
	h.takeReactions(convID)
	h.dropArchiveArm(convID)
	h.forgetOwn(convID)
}

// senderName is the human name to attribute a typed `!archive` to.
// A message event carries it, so unlike the reaction path this costs
// no API call. The fallback is deliberately vague rather than a user
// id: the name is prose in a warning message, not an identity.
func senderName(m *zulipproto.Message) string {
	if m == nil || strings.TrimSpace(m.SenderName) == "" {
		return "someone"
	}
	return m.SenderName
}

// archiveWarning is the message a :wastebasket: must land on to
// confirm. It says what will happen, what will NOT happen (nothing is
// deleted), and how to walk away — a destructive prompt that does not
// explain how to decline is a trap.
func archiveWarning(who, channel string) string {
	return fmt.Sprintf("⚠️ **Archive this topic?** — %s asked to end this conversation and move the whole topic to **#%s**, a channel I do not serve.\n\n"+
		"React :%s: **to this message** within %s to confirm. Nothing is deleted: every message travels with the topic, and the session's files stay on disk. Ignore this message to cancel.",
		who, channel, archiveEmoji, archiveConfirmTTL)
}

// archiveNotice is the last thing said in the topic before it moves,
// and therefore the first thing anyone reads when they find it in the
// archive.
func archiveNotice(who, channel string) string {
	return fmt.Sprintf("🗑️ **Archived by %s.** This conversation has ended and the topic is moving to **#%s**; I do not answer there.\n\n"+
		"Nothing was deleted. Move the topic back to bring the messages home — a fresh conversation starts on the next message either way.",
		who, channel)
}
