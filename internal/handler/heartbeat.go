package handler

import (
	"context"
	"fmt"
	"strings"

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
	if hb, ok := h.cfg.Journal.Heartbeat(sched.ID); ok && h.isNewest(ctx, conv.Key, hb.MsgID) {
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
// conversation, by anybody. A failed read reports false: a new message
// is the safe direction.
func (h *Handler) isNewest(ctx context.Context, key journal.Key, msgID int64) bool {
	narrow := zulipproto.DMNarrow(key.UserIDs)
	if !key.IsDM() {
		narrow = zulipproto.TopicNarrow(key.StreamID, key.Topic)
	}
	msgs, err := h.cfg.Client.Messages(ctx, narrow, 1, 0)
	if err != nil {
		h.cfg.Logf("handler: reading the newest message: %v", err)
		return false
	}
	return len(msgs) == 1 && msgs[0].ID == msgID
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
