package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/kfet/acp-kit/schedule"

	"github.com/kfet/zulip-acp/internal/journal"
	"github.com/kfet/zulip-acp/internal/rollover"
	"github.com/kfet/zulip-acp/internal/zulipproto"
)

// heartbeatTextRunes caps the schedule text the heartbeat line names.
const heartbeatTextRunes = 60

// heartbeat records that a scheduled turn ran and had nothing to say.
//
// A repeating check that abstains must show it is alive, but it must
// not post a new message each time: seven "nothing new" messages in a
// row bury the topic. So each schedule keeps ONE small heartbeat
// message and edits it in place while it is still the newest message
// in the conversation. When somebody (the relay included) has posted
// after it, or the edit fails (for example the realm's edit time
// limit), a new heartbeat is posted and remembered instead. The record
// is in the journal, so a restart edits the same message.
//
// Real content never goes here: an edit does not notify, so an answer
// is always a new message.
//
// sched is nil for a human turn, which gets no heartbeat.
func (h *Handler) heartbeat(ctx context.Context, conv journal.Conv, sched *schedule.Item) {
	if sched == nil {
		return
	}
	if hb, ok := h.cfg.Journal.Heartbeat(sched.ID); ok && hb.Hash == "" && h.isNewest(ctx, conv.Key, hb.MsgID) {
		// A record with a hash is an answer (collapseRepeat): it is
		// never edited into a heartbeat line.
		n := hb.Count + 1
		err := h.cfg.Client.EditMessage(ctx, hb.MsgID, h.heartbeatText(n, sched.Text))
		if err == nil {
			h.saveHeartbeat(sched.ID, journal.Heartbeat{MsgID: hb.MsgID, Count: n})
			return
		}
		h.cfg.Logf("handler: editing heartbeat %d: %v; posting a new one", hb.MsgID, err)
	}
	post := &convPoster{client: h.cfg.Client, key: conv.Key}
	id, err := post.Post(ctx, h.heartbeatText(1, sched.Text))
	if err != nil {
		h.cfg.Logf("handler: posting heartbeat in %s: %v", conv.ID, err)
		return
	}
	h.rememberOwn(conv.ID, id)
	h.saveHeartbeat(sched.ID, journal.Heartbeat{MsgID: id, Count: 1})
}

func (h *Handler) saveHeartbeat(schedID string, hb journal.Heartbeat) {
	if err := h.cfg.Journal.SetHeartbeat(schedID, hb); err != nil {
		h.cfg.Logf("handler: recording heartbeat of %s: %v", schedID, err)
	}
}

// heartbeatText is the one short line: when, how many times, and what.
func (h *Handler) heartbeatText(n int, text string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(text), "\n")
	if r := []rune(line); len(r) > heartbeatTextRunes {
		line = string(r[:heartbeatTextRunes]) + "…"
	}
	return fmt.Sprintf("*⏰ checked %s ×%d · %s*", h.now().Format("15:04"), n, line)
}

// isNewest reports whether msgID is the newest message in the
// conversation, by anybody, when the messages in skip are not counted.
// A failed read reports false: a new message is the safe direction.
func (h *Handler) isNewest(ctx context.Context, key journal.Key, msgID int64, skip ...int64) bool {
	narrow := zulipproto.DMNarrow(key.UserIDs)
	if !key.IsDM() {
		narrow = zulipproto.TopicNarrow(key.StreamID, key.Topic)
	}
	msgs, err := h.cfg.Client.Messages(ctx, narrow, 1+len(skip), 0)
	if err != nil {
		h.cfg.Logf("handler: reading the newest message: %v", err)
		return false
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		if !slices.Contains(skip, msgs[i].ID) {
			return msgs[i].ID == msgID
		}
	}
	return false
}

// retract deletes what a turn has posted so far, without a flush: the
// placeholder, and any streamed text. It is the safety net for an
// answer that turned out to be only the silence sentinel. The
// conversation's last-own record pointed at what was deleted, so it is
// forgotten; the next lookup reads it from the server.
func (h *Handler) retract(ctx context.Context, convID string, split *rollover.Splitter) {
	ids := split.IDs()
	for _, id := range ids {
		if err := h.cfg.Client.DeleteMessage(ctx, id); err != nil {
			h.cfg.Logf("handler: deleting %d: %v", id, err)
		}
	}
	if len(ids) > 0 {
		h.forgetOwn(convID)
	}
}

// Normalisation for repeatHash: the parts of an answer that change on
// every fire although the answer is the same.
var (
	repeatUUID  = regexp.MustCompile(`(?i)\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`)
	repeatReqID = regexp.MustCompile(`(?i)request[-_ ]id:\s*\S+`)
	repeatClock = regexp.MustCompile(`\b\d{1,2}:\d{2}(:\d{2})?\b`)
)

// repeatHash is the hash of a scheduled answer, with the parts that
// change on every fire removed: UUIDs, "request-id: …" and clock
// times. The caller gives the agent text and the suffix, without the
// status footer and the schedule marker.
func repeatHash(text string) string {
	t := repeatReqID.ReplaceAllString(text, "")
	t = repeatUUID.ReplaceAllString(t, "")
	t = repeatClock.ReplaceAllString(t, "")
	sum := sha256.Sum256([]byte(strings.Join(strings.Fields(t), " ")))
	return hex.EncodeToString(sum[:])
}

// collapseRepeat stops a repeating schedule that gives the same answer
// each time (for example a provider error) from posting a new message
// on each fire.
//
// When the schedule's last answer had the same hash and is still the
// newest message in the conversation, the new answer is retracted and
// the old message is edited to the new text and a "×N" note. It
// returns the id of the message that stays, or 0 when the caller must
// keep the new answer. An edit that fails keeps the new answer: a
// new post is the safe direction.
//
// The first answer always posts as a new message, so it notifies.
func (h *Handler) collapseRepeat(ctx context.Context, conv journal.Conv, split *rollover.Splitter, prefix string, sched *schedule.Item, hash string) int64 {
	if sched == nil {
		return 0
	}
	prev, ok := h.cfg.Journal.Heartbeat(sched.ID)
	if !ok || prev.Hash != hash || len(split.IDs()) != 1 || !h.isNewest(ctx, conv.Key, prev.MsgID, split.IDs()...) {
		return 0
	}
	n := prev.Count + 1
	body := prefix + strings.TrimRight(split.Transcript(), "\n") + fmt.Sprintf("\n\n*×%d · last %s*", n, h.now().Format("15:04"))
	budget := h.cfg.Budget
	if budget == 0 {
		budget = rollover.DefaultBudget
	}
	if utf8.RuneCountInString(body) > budget {
		h.cfg.Logf("handler: repeated answer in %s is over the message limit with its note; keeping the new one", conv.ID)
		return 0
	}
	if err := h.cfg.Client.EditMessage(ctx, prev.MsgID, body); err != nil {
		h.cfg.Logf("handler: editing repeated answer %d: %v; keeping the new one", prev.MsgID, err)
		return 0
	}
	h.retract(ctx, conv.ID, split)
	h.rememberOwn(conv.ID, prev.MsgID)
	h.saveHeartbeat(sched.ID, journal.Heartbeat{MsgID: prev.MsgID, Count: n, Hash: hash})
	return prev.MsgID
}

// recordAnswer remembers the message of a scheduled answer, so that
// the next fire with the same answer can collapse into it.
func (h *Handler) recordAnswer(sched *schedule.Item, msgID int64, hash string) {
	if sched == nil || msgID == 0 {
		return
	}
	h.saveHeartbeat(sched.ID, journal.Heartbeat{MsgID: msgID, Count: 1, Hash: hash})
}
