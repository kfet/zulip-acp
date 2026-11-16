// This file holds the LIFECYCLE rules (config `lifecycle`): a topic in
// one of a rule's states, idle at least the rule's time, is archived by
// the relay itself, through the same archiveConversation that
// `!archive` uses.
//
// A rule never acts on a topic that runs, has a schedule, or needs the
// user (needs_you, blocked): config allows only LifecycleStates, and
// eligible checks the rest at the moment of the act. Idle is measured
// from the last message in the topic, so a new message resets it.
//
// A topic older than the realm's move time limit cannot be moved by
// the bot. It is skipped quietly, and logged once, instead of telling
// the topic every hour.
package handler

import (
	"context"
	"slices"
	"time"
	"unicode/utf8"

	"github.com/kfet/zulip-acp/internal/config"
	"github.com/kfet/zulip-acp/internal/journal"
	"github.com/kfet/zulip-acp/internal/zulipproto"
)

// lifecycleWho names the actor in the closing message.
const lifecycleWho = "the lifecycle rule"

// runLifecycle archives every conversation a rule matches.
func (h *Handler) runLifecycle(ctx context.Context) {
	if len(h.cfg.Lifecycle) == 0 || !h.archiveEnabled() {
		return
	}
	now := h.now()
	for _, c := range h.cfg.Journal.Convs() {
		r, ok := h.lifecycleRule(c, now)
		if !ok {
			continue
		}
		if _, skip := h.tooOld.Load(c.ID); skip || h.recentInTopic(ctx, c.Key, now.Add(-r.Idle)) {
			continue
		}
		if ok, _ := h.movable(ctx, c.Key); !ok {
			h.tooOld.Store(c.ID, true)
			h.cfg.Logf("handler: lifecycle skips %s: I cannot move it (see above); it stays", h.describe(c.Key))
			continue
		}
		topic := ""
		if r.PrefixSource {
			topic = prefixTopic(h.channelName(c.StreamID), c.Topic)
		}
		h.cfg.Logf("handler: lifecycle archives %s (%s): idle since %s", h.describe(c.Key), c.ID, time.UnixMilli(lastSeen(c)).UTC().Format(time.RFC3339))
		h.archiveConversation(ctx, c, lifecycleWho, topic)
	}
}

// lifecycleRule returns the first rule that matches c now.
func (h *Handler) lifecycleRule(c journal.Conv, now time.Time) (config.ResolvedLifecycle, bool) {
	if !viewable(c, nil) || h.isInflight(c.ID) || h.hasSchedule(c.Key) {
		return config.ResolvedLifecycle{}, false
	}
	state := viewOpen
	if c.State != nil {
		state = c.State.State
	}
	for _, r := range h.cfg.Lifecycle {
		if inSources(r.Sources, c.StreamID) && slices.Contains(r.States, state) && now.Sub(time.UnixMilli(lastSeen(c))) >= r.Idle {
			return r, true
		}
	}
	return config.ResolvedLifecycle{}, false
}

// recentInTopic reports whether the topic's newest message is after
// cutoff. The topic is truth: the journal misses what was written
// while the relay was down. A failed read counts as recent: archive
// only on evidence.
func (h *Handler) recentInTopic(ctx context.Context, key journal.Key, cutoff time.Time) bool {
	msgs, err := h.cfg.Client.Messages(ctx, zulipproto.TopicNarrow(key.StreamID, key.Topic), 1, 0)
	if err != nil {
		h.cfg.Logf("handler: lifecycle skips %s: reading its newest message failed (%v)", h.describe(key), err)
		return true
	}
	return len(msgs) > 0 && time.Unix(msgs[len(msgs)-1].Timestamp, 0).After(cutoff)
}

// hasSchedule reports whether a schedule is armed for key.
func (h *Handler) hasSchedule(key journal.Key) bool {
	return h.cfg.Schedules != nil && len(h.cfg.Schedules.List(key.Token())) > 0
}

// prefixTopic is "channel: topic", cut to Zulip's topic length.
func prefixTopic(channel, topic string) string {
	t := channel + ": " + topic
	if utf8.RuneCountInString(t) <= zulipproto.MaxTopicLength {
		return t
	}
	return string([]rune(t)[:zulipproto.MaxTopicLength-1]) + "…"
}
