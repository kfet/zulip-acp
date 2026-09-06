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
)

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
	// Removals are not delivered. "Someone took a reaction back" is
	// not a prompt, and a reaction the relay itself retracts at the
	// end of every turn would otherwise be one.
	if ev.Op != zulipproto.ReactionAdd {
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
	prompt := h.reactionPrompt(who, ev, msg)
	// Ambient, never addressed: a reaction is by nature something the
	// agent should usually pass over, so it goes down the same path a
	// non-mention channel message takes and may be answered with the
	// silent sentinel. No ack reaction either (msgID 0) — reacting to
	// a reaction is noise, and the message reacted to is often an old
	// one nobody is looking at any more.
	//
	// And it must never destroy a turn: a message supersedes the
	// running one on purpose — the human changed their mind — but
	// cancelling an answer because someone tapped an emoji would be
	// pure loss.
	if !h.startTurnIfIdle(ctx, conv, prompt, false, 0) {
		h.cfg.Logf("handler: dropping :%s: on message %d — a turn is already running in %s", ev.EmojiName, ev.MessageID, conv.ID)
		return
	}
	h.cfg.Logf("handler: reaction :%s: from user %d delivered to %s", ev.EmojiName, ev.UserID, conv.ID)
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

// reactionPrompt renders the synthetic user turn.
//
// One compact line, and unambiguous about whose message was reacted
// to: "your own message" is the case a later trigger and the agent
// itself both care about most.
func (h *Handler) reactionPrompt(who string, ev zulipproto.Event, m *zulipproto.Message) string {
	target := "message " + strconv.FormatInt(ev.MessageID, 10)
	if m == nil {
		// Resolved from the relay's own index or the journal's
		// recorded ids: it is ours by construction.
		target = "your own " + target
	} else if m.SenderID == h.cfg.BotUserID {
		target = "your own " + target
	} else if m.SenderName != "" {
		target += " from " + m.SenderName
	}
	line := fmt.Sprintf("[reaction] %s added :%s: to %s", who, ev.EmojiName, target)
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
