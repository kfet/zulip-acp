package handler

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/kfet/zulip-acp/internal/journal"
	"github.com/kfet/zulip-acp/internal/zulipproto"
)

// Emoji reactions as a conversational signal.
//
// # Why this needs gating at all
//
// A `reaction` event is NOT like a message event. Measured on Zulip
// 12.2 (see docs/zulip-protocol-reference.md):
//
//   - the /register narrow does not filter it — a queue narrowed to
//     one channel still receives reactions on messages elsewhere;
//   - it carries no stream id, no topic and no message body, only a
//     message_id, so the relay cannot tell where it happened without
//     spending an API call;
//   - it carries no user name either, only a user id.
//
// So the whole realm's :+1: traffic arrives here, and the naive
// implementation answers it with an unbounded stream of GET
// /messages/{id}. Everything below exists to make that impossible:
// cheap gates first (op, self, bots, allowlist), then an in-memory
// index of the messages the relay itself posted, then — for anything
// still unresolved — ONE rate-limited, negatively-cached lookup.
//
// # What a reaction may never do
//
// It may never create a conversation and it may never create a
// session: it resolves to a conversation the relay is already engaged
// in, or it is dropped. A reaction cannot summon the bot. It also
// never supersedes a running turn — cancelling someone's answer
// because a third party tapped an emoji would be absurd.
//
// # Room for a relay-side trigger
//
// A later feature wants a specific emoji on the relay's own last
// message to archive the topic — a RELAY action, not an agent turn.
// That hooks in at reactionTrigger below: it sees the resolved
// conversation and the resolved message before anything is handed to
// the agent, which is exactly where such a trigger belongs.

const (
	// reactionExcerptRunes bounds the quoted excerpt of the
	// reacted-to message. Enough to identify it, never enough to
	// matter for context.
	reactionExcerptRunes = 80

	// reactionIndexSize bounds each of the two message-id caches. Both
	// are pure accelerators: evicting an entry costs at most one extra
	// API call, never correctness.
	reactionIndexSize = 512

	// reactionLookupsPerMinute caps how often an unresolved reaction
	// may cost a GET /messages/{id}. This is the flood stop: reaction
	// events arrive for the whole realm, so without it a busy realm
	// would drive our API traffic.
	reactionLookupsPerMinute = 20

	// reactionLookupWindow is the period reactionLookupsPerMinute is
	// measured over.
	reactionLookupWindow = time.Minute

	// botMarker is the value cached in the name index for a user id
	// that turned out to be a bot. A name is never this, and a bot
	// never becomes a human, so one lookup settles it for good.
	botMarker = "\x00bot"

	// reactionDebounce is how long a conversation's reactions are
	// buffered before they are delivered as one turn.
	//
	// Four seconds, chosen from how the two sides actually behave. A
	// pile-on is people reacting to the SAME message after reading it,
	// which lands over a couple of seconds, and Zulip delivers the
	// events of one long poll together, so the whole burst is usually
	// inside one window. Longer would start to feel disconnected from
	// the tap that caused it, and would widen the gap in which the
	// agent answers something the user has already moved on from;
	// shorter would split an ordinary pile-on into several turns,
	// which is exactly the cost this exists to avoid.
	reactionDebounce = 4 * time.Second

	// reactionBatchMax caps how many reactions one turn lists. Beyond
	// it the burst is still ONE turn — the agent is told the count it
	// did not see. A prompt that grows with the size of a pile-on is
	// the other way to make this expensive.
	reactionBatchMax = 20
)

// reactionBatch is one conversation's buffered burst.
type reactionBatch struct {
	lines []string
	extra int
}

// msgIndex is a bounded id→string map with FIFO eviction.
//
// It backs three caches — messages the relay posted, message ids that
// did not resolve, and user names — all of which are hints. Nothing
// here is authoritative, so eviction is free.
type msgIndex struct {
	mu    sync.Mutex
	max   int
	m     map[int64]string
	order []int64
}

func newMsgIndex(max int) *msgIndex {
	return &msgIndex{max: max, m: make(map[int64]string, max)}
}

func (x *msgIndex) put(id int64, v string) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if _, ok := x.m[id]; ok {
		x.m[id] = v
		return
	}
	x.m[id] = v
	x.order = append(x.order, id)
	if len(x.order) > x.max {
		delete(x.m, x.order[0])
		x.order = x.order[1:]
	}
}

func (x *msgIndex) get(id int64) (string, bool) {
	x.mu.Lock()
	defer x.mu.Unlock()
	v, ok := x.m[id]
	return v, ok
}

// dropValue forgets every entry stored under v. It is how the negative
// cache is invalidated for exactly one conversation — engaging a topic
// changes the answer for the messages IN it and for nothing else, and
// an autotopic channel mints a conversation on every general-chat
// message, so a blanket reset would keep the cache permanently empty
// there.
func (x *msgIndex) dropValue(v string) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if v == "" {
		return
	}
	kept := x.order[:0]
	for _, id := range x.order {
		if x.m[id] == v {
			delete(x.m, id)
			continue
		}
		kept = append(kept, id)
	}
	x.order = kept
}

// handleReaction turns an emoji reaction into an ambient turn in the
// conversation that owns the reacted-to message.
//
// The gate order is deliberate and must not be reordered: everything
// free happens before anything that costs an API call.
func (h *Handler) handleReaction(ctx context.Context, ev zulipproto.Event) {
	if !h.cfg.Reactions {
		return
	}
	// Both ops are delivered. Un-reacting is real signal — an approval
	// withdrawn, a trigger taken back — and it costs no more than an
	// add, because a burst of either is coalesced into one turn (see
	// enqueueReaction). What keeps the relay's own :eyes: retraction
	// out is the user-id guard below, not the op.
	if ev.Op != zulipproto.ReactionAdd && ev.Op != zulipproto.ReactionRemove {
		return
	}
	// The relay's own reactions: the in-flight ack (Handler.ack) and
	// the `!opts` acknowledgement. Feeding those back would be an
	// immediate self-sustaining loop, so this guard comes first,
	// before any allowlist, exactly as it does for messages.
	if ev.UserID == h.cfg.BotUserID {
		return
	}
	if _, isBot := h.cfg.BotSenderIDs[ev.UserID]; isBot {
		return
	}
	if h.cfg.AllowedUsers != nil {
		if _, ok := h.cfg.AllowedUsers[ev.UserID]; !ok {
			return
		}
	}
	conv, msg, ok := h.convForReaction(ctx, ev.MessageID)
	if !ok {
		// Silence is the whole point: a reaction on a message the
		// relay has nothing to do with is not an error.
		return
	}
	if h.reactionTrigger(ctx, conv, ev, msg) {
		return
	}
	// The last gate, and the only one that costs a call: BotSenderIDs
	// is a startup snapshot, so a bot that appeared since — or a
	// cross-realm system bot, which is in no user list — is recognised
	// only here, while resolving the name the prompt needs anyway.
	who, isBot := h.reactor(ctx, ev.UserID)
	if isBot {
		return
	}
	h.enqueueReaction(ctx, conv, reactionLine(who, ev, msg, h.cfg.BotUserID))
}

// enqueueReaction buffers one rendered reaction for a conversation and,
// if this is the first of a burst, arms the flush that will deliver the
// whole burst as ONE turn.
//
// This is what makes reactions affordable enough to be on by default.
// Ten people tapping the same message is one event each and one
// conversational fact — "ten people reacted" — so it must cost one
// agent turn, not ten. Without it the feature would be a token pump
// that any popular message could start.
func (h *Handler) enqueueReaction(ctx context.Context, conv journal.Conv, line string) {
	h.reactMu.Lock()
	b, armed := h.reactPending[conv.ID]
	if !armed {
		b = &reactionBatch{}
		h.reactPending[conv.ID] = b
	}
	switch {
	case len(b.lines) < reactionBatchMax:
		b.lines = append(b.lines, line)
	default:
		// A burst larger than the cap is still one turn: the agent is
		// told how many it did not see rather than being handed a
		// prompt that grows without limit.
		b.extra++
	}
	h.reactMu.Unlock()
	if !armed {
		go h.flushReactions(ctx, conv)
	}
}

// flushReactions waits out the debounce window, then waits for the
// conversation to be free, and delivers everything buffered as one
// ambient turn.
//
// The batch is taken AFTER the conversation is claimed, so a reaction
// that lands while a human turn is still running — or during the wait
// for it — is folded into the same delivery instead of racing it or
// being dropped. That ordering is the whole reason this is not a
// simple timer.
func (h *Handler) flushReactions(ctx context.Context, conv journal.Conv) {
	select {
	case <-h.after(reactionDebounce):
	case <-ctx.Done():
		// Shutting down. A buffered reaction is not a turn anyone is
		// waiting on, so it is dropped rather than kept alive past the
		// relay it belongs to.
		h.takeReactions(conv.ID)
		return
	}
	// The turn's context is created cancellable-only and the CLOCK IS
	// STARTED AFTER THE CLAIM. Deriving the timeout before the wait
	// would charge the reaction turn for however long the human turn
	// it queued behind took — and a conversation busy for longer than
	// PromptTimeout (the exact case reactions pile up in) would start
	// this turn already expired and post "*error: context deadline
	// exceeded*" into the topic, caused by nothing but an emoji.
	turnCtx, cancelTurn := context.WithCancel(context.WithoutCancel(ctx))
	entry := &inflightEntry{cancel: cancelTurn}
	if err := h.claimConvIdle(ctx, conv.ID, entry); err != nil {
		cancelTurn()
		h.takeReactions(conv.ID)
		return
	}
	pctx, cancelTimeout := context.WithTimeout(turnCtx, h.cfg.PromptTimeout)
	cancel := func() { cancelTimeout(); cancelTurn() }
	lines, extra := h.takeReactions(conv.ID)
	// Re-read the conversation: seconds have passed, and unlike a
	// message turn — where this window is microseconds — the topic may
	// have been renamed (posting under the stale name would recreate
	// it) or retired by `!new` (the reaction belongs to the
	// conversation the user just replaced). The id is stable; the key
	// is not.
	fresh, ok := h.cfg.Journal.LookupID(conv.ID)
	if !ok || fresh.Retired {
		h.cfg.Logf("handler: dropping %d buffered reaction(s): %s is gone", len(lines)+extra, conv.ID)
		h.clearInflight(conv.ID, entry)
		cancel()
		return
	}
	conv = fresh
	if len(lines) == 0 && extra == 0 {
		// The buffer was emptied under us — DropPendingReactions at
		// shutdown. Release the claim rather than running a turn with
		// nothing in it.
		h.clearInflight(conv.ID, entry)
		cancel()
		return
	}
	if h.cfg.OnReactionBatch != nil {
		h.cfg.OnReactionBatch(conv.ID, len(lines))
	}
	h.cfg.Logf("handler: delivering %d reaction(s) to %s", len(lines)+extra, conv.ID)
	// Ambient, never addressed: a reaction is by nature something the
	// agent should usually pass over, so it goes down the same path a
	// non-mention channel message takes and may be answered with the
	// silent sentinel. No ack reaction either (msgID 0) — reacting to
	// a reaction is noise, and the message reacted to is often an old
	// one nobody is looking at any more.
	h.runTurn(pctx, cancel, conv, entry, reactionBatchPrompt(lines, extra), false, 0)
}

// DropPendingReactions discards every buffered reaction burst and
// reports how many reactions were dropped.
//
// It is the shutdown/reload counterpart to the debounce: once polling
// has stopped, a reaction still waiting to be coalesced is not a turn
// anyone is waiting on, and letting its timer expire mid-drain would
// start a turn the re-exec then kills. A flush already claimed and
// running is a real turn and is drained normally by WaitIdle.
func (h *Handler) DropPendingReactions() int {
	h.reactMu.Lock()
	defer h.reactMu.Unlock()
	n := 0
	for id, b := range h.reactPending {
		n += len(b.lines) + b.extra
		delete(h.reactPending, id)
	}
	return n
}

// takeReactions removes and returns a conversation's buffered
// reactions. An empty result is possible only on a shutdown race and
// renders as nothing.
func (h *Handler) takeReactions(convID string) (lines []string, extra int) {
	h.reactMu.Lock()
	defer h.reactMu.Unlock()
	b, ok := h.reactPending[convID]
	if !ok {
		return nil, 0
	}
	delete(h.reactPending, convID)
	return b.lines, b.extra
}

// after is the injected timer. Production uses time.After; the test
// suite supplies a channel it fires by hand, because a debounce proved
// by sleeping is a flaky test.
func (h *Handler) after(d time.Duration) <-chan time.Time {
	if h.cfg.After != nil {
		return h.cfg.After(d)
	}
	return time.After(d)
}

// reactionBatchPrompt renders a burst as one synthetic user turn. A
// single reaction keeps the compact one-line form; several are listed,
// so the agent can see at a glance that it is looking at a pile-on
// rather than a conversation.
func reactionBatchPrompt(lines []string, extra int) string {
	if len(lines) == 0 {
		return ""
	}
	if len(lines) == 1 && extra == 0 {
		return "[reaction] " + lines[0]
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "[reactions] %d in this conversation:", len(lines)+extra)
	for _, l := range lines {
		sb.WriteString("\n- " + l)
	}
	if extra > 0 {
		fmt.Fprintf(&sb, "\n- …and %d more", extra)
	}
	return sb.String()
}

// reactionTrigger is the hook for RELAY-side reaction actions — the
// planned "react with :wastebasket: on my last message to archive the
// topic". It runs after the conversation and the message are resolved
// and before the agent is involved at all, and reports whether it
// consumed the reaction.
//
// Nothing is wired to it in production (Config.ReactionTrigger is nil):
// v1 delivers every resolved reaction to the agent. It exists so that
// adding a trigger later is a change in one place rather than a
// re-plumbing of the gate order.
func (h *Handler) reactionTrigger(ctx context.Context, conv journal.Conv, ev zulipproto.Event, m *zulipproto.Message) bool {
	if h.cfg.ReactionTrigger == nil {
		return false
	}
	return h.cfg.ReactionTrigger(ctx, conv, ev, m)
}

// convForReaction resolves the reacted-to message to a conversation
// the relay is engaged in, plus the message itself when it had to be
// fetched anyway.
//
// Three tiers, cheapest first:
//
//  1. the in-memory index of messages the relay streamed into, which
//     covers the common case — a reaction on the relay's own last
//     message — with no API call at all;
//  2. the journal's own recorded ids (an interrupted turn's tail, an
//     `!opts` panel), which survive a restart the index does not;
//  3. one rate-limited GET /messages/{id}, negatively cached so the
//     same unrelated message never costs a second one.
//
// It NEVER allocates a conversation: an unknown key is a drop.
func (h *Handler) convForReaction(ctx context.Context, msgID int64) (journal.Conv, *zulipproto.Message, bool) {
	if convID, ok := h.ownMsgs.get(msgID); ok {
		c, ok := h.cfg.Journal.LookupID(convID)
		if ok && !c.Retired && h.serves(c.Key) {
			return c, nil, true
		}
		return journal.Conv{}, nil, false
	}
	if _, seen := h.badMsgs.get(msgID); seen {
		return journal.Conv{}, nil, false
	}
	// The journal scan is O(conversations) under the journal's lock,
	// so it sits AFTER the negative cache: a message already known not
	// to be ours never pays for it twice.
	if c, ok := h.cfg.Journal.LookupMessage(msgID); ok {
		if !h.serves(c.Key) {
			return journal.Conv{}, nil, false
		}
		return c, nil, true
	}
	if !h.allowReactionLookup() {
		return journal.Conv{}, nil, false
	}
	m, err := h.cfg.Client.GetMessage(ctx, msgID)
	if err != nil {
		h.badMsgs.put(msgID, "")
		h.cfg.Logf("handler: reading reacted-to message %d: %v", msgID, err)
		return journal.Conv{}, nil, false
	}
	key, ok := h.keyForMessage(m)
	if !ok {
		h.badMsgs.put(msgID, "")
		return journal.Conv{}, nil, false
	}
	c, ok := h.cfg.Journal.Lookup(key)
	if !ok {
		// Resolvable, but its conversation does not exist YET. The
		// value is the key, so engaging that conversation later drops
		// exactly these entries and nothing else.
		h.badMsgs.put(msgID, key.Label())
		return journal.Conv{}, nil, false
	}
	return c, &m, true
}

// keyForMessage derives the conversation key a message belongs to,
// applying the same surface gates a message event gets: a channel must
// be served, and a DM only counts when DMs are enabled.
func (h *Handler) keyForMessage(m zulipproto.Message) (journal.Key, bool) {
	if m.IsDM() {
		if !h.cfg.DMs {
			return journal.Key{}, false
		}
		ids := m.Recipients()
		if len(ids) == 0 {
			return journal.Key{}, false
		}
		return journal.DM(ids), true
	}
	if _, ok := h.cfg.Channels.Name(m.StreamID); !ok {
		return journal.Key{}, false
	}
	return journal.Channel(m.StreamID, m.Topic), true
}

// serves reports whether a conversation's key is still one the relay
// answers in. The served set moves underfoot (it can follow the bot's
// subscriptions), so a journal entry is not by itself permission.
func (h *Handler) serves(k journal.Key) bool {
	if k.IsDM() {
		return h.cfg.DMs
	}
	_, ok := h.cfg.Channels.Name(k.StreamID)
	return ok
}

// allowReactionLookup is the token bucket in front of GET
// /messages/{id}. It measures against the injected clock, so tests
// drive it rather than wait for one.
//
// Exceeding the cap is logged ONCE per window, not once per event: the
// thing being defended against is a flood, and a log line per dropped
// reaction would just move the flood into the journal.
func (h *Handler) allowReactionLookup() bool {
	h.lookupMu.Lock()
	defer h.lookupMu.Unlock()
	now := h.now()
	if now.Sub(h.lookupStart) >= reactionLookupWindow {
		h.lookupStart, h.lookupCount, h.lookupWarned = now, 0, false
	}
	if h.lookupCount >= reactionLookupsPerMinute {
		if !h.lookupWarned {
			h.lookupWarned = true
			h.cfg.Logf("handler: not resolving further reactions this minute — over %d lookups per %s", reactionLookupsPerMinute, reactionLookupWindow)
		}
		return false
	}
	h.lookupCount++
	return true
}

// reactionLine renders ONE reaction as a fragment of the synthetic
// turn. It is a plain function, not a method, because it decides
// nothing about the relay — only how a fact reads.
//
// Two things it must always make unambiguous: whether the reaction was
// added or REMOVED (un-reacting is real signal: an approval withdrawn,
// a trigger taken back), and whether the message reacted to was the
// relay's own.
func reactionLine(who string, ev zulipproto.Event, m *zulipproto.Message, botUserID int64) string {
	verb, prep := "added", "to"
	if ev.Op == zulipproto.ReactionRemove {
		verb, prep = "removed", "from"
	}
	target := "message " + strconv.FormatInt(ev.MessageID, 10)
	switch {
	case m == nil:
		// Resolved from the relay's own index or the journal's
		// recorded ids: it is ours by construction.
		target = "your own " + target
	case m.SenderID == botUserID:
		target = "your own " + target
	case m.SenderName != "":
		target += " by " + m.SenderName
	}
	line := fmt.Sprintf("%s %s :%s: %s %s", who, verb, ev.EmojiName, prep, target)
	if m != nil {
		if ex := excerpt(m.Content, reactionExcerptRunes); ex != "" {
			line += " (" + strconv.Quote(ex) + ")"
		}
	}
	return line
}

// excerpt flattens a message body to one short line.
func excerpt(content string, limit int) string {
	s := strings.Join(strings.Fields(content), " ")
	if s == "" {
		return ""
	}
	if utf8.RuneCountInString(s) <= limit {
		return s
	}
	return string([]rune(s)[:limit]) + "…"
}

// reactor resolves the reacting user to a display name, and reports
// whether they are a bot.
//
// A reaction event carries no name at all, and "user 8 reacted" is not
// something to hand an agent that talks to humans. The lookup is one
// HTTP call the FIRST time a given user reacts and never again.
//
// The bot answer is the second reason this call exists: BotSenderIDs is
// a startup snapshot and a reaction event carries no realm string, so a
// bot created since startup — or a cross-realm system bot, which never
// appears in GET /users at all — is only recognisable here. A failed
// lookup degrades to the id and to "not a bot": the name is decoration
// and refusing a human because Zulip would not describe them would be
// worse than answering a bot.
func (h *Handler) reactor(ctx context.Context, id int64) (name string, isBot bool) {
	if cached, ok := h.userNames.get(id); ok {
		return cached, cached == botMarker
	}
	name = "user " + strconv.FormatInt(id, 10)
	u, err := h.cfg.Client.UserByID(ctx, id)
	if err != nil {
		h.cfg.Logf("handler: resolving user %d: %v", id, err)
		// Not cached: a transient failure must not stick a numeric
		// name to a human for the life of the process.
		return name, false
	}
	if u.IsBot {
		// Cached like any other answer — a bot does not become a
		// human, and re-asking on every one of its reactions is the
		// unbounded per-event cost this file exists to avoid.
		h.userNames.put(id, botMarker)
		return name, true
	}
	if u.FullName != "" {
		name = u.FullName
	}
	h.userNames.put(id, name)
	return name, false
}
