// This file is the Handler half of the `branch` loopback tool (see
// internal/zulipmcp/branch.go): the agent-callable twin of `!branch`
// and the :fork_and_knife: reaction.
//
// It adds no way to branch. Every task becomes a branchPlan and goes
// through branchOnce, the half the two human gestures share, so all
// three entry points create topics, write the journal and start turns
// in exactly one way. What is new here is only what an AGENT needs
// that a human does not:
//
//   - a branch point named by message id (`from_msg`), refused unless
//     it is in the caller's own topic — the child's
//     history(origin=true) is clamped there, and an id from another
//     topic would widen that read to a conversation nobody granted;
//   - many branches in one call, capped by zulipmcp.MaxBranchTasks;
//   - errors returned to the agent per task, not posted into the topic;
//   - ONE notification for the whole call in the caller's topic, and,
//     when the caller is itself a branch, one line in the ROOT of the
//     branch tree, so a human watching the root sees the fan-out at
//     every depth.
//
// Nested branching is allowed: a child has the same tools as anybody
// else. The one-hop read rule still holds, because each child's parent
// pointer names only its direct caller.
package handler

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/kfet/zulip-acp/internal/journal"
	"github.com/kfet/zulip-acp/internal/zulipmcp"
	"github.com/kfet/zulip-acp/internal/zulipproto"
)

const (
	// branchToolGesture names the tool in the branch log line.
	branchToolGesture = "the branch tool"
	// branchLatestWindow is how far back the default branch point is
	// looked for. The newest message in the caller's topic is usually
	// the relay's own in-flight answer; the default is the newest one
	// that is NOT the relay's, and it is almost always within a few.
	branchLatestWindow = 50
	// branchGoalRunes bounds the one-line goal beside each link in the
	// notification.
	branchGoalRunes = 120
	// branchRootDepth bounds the walk to the root of a branch tree. It
	// is a guard against a cycle that renames could in theory build,
	// not a limit on how deep branching may go: past it, the deepest
	// ancestor found is used.
	branchRootDepth = 32
)

// BranchTasks is zulipmcp.Config.Branch: it runs the tasks of one
// `branch` call out of the conversation sessionKey names.
//
// A whole-call error means nothing was created. After that, each task
// succeeds or fails on its own, and a failure is reported in its
// result — one bad from_msg must not cost the other nine branches.
func (h *Handler) BranchTasks(sessionKey string, tasks []zulipmcp.BranchTask) ([]zulipmcp.BranchResult, error) {
	// A retired conversation still resolves by id, so a turn unwinding
	// after `!new` is refused here: it must not start new work.
	caller, ok := h.cfg.Journal.LookupID(sessionKey)
	if !ok || caller.Retired {
		return nil, errors.New("this conversation is no longer active")
	}
	dest, channel, err := h.branchDestination(caller.Key, "")
	if err != nil {
		return nil, err
	}
	// Each Zulip read and post outside branchOnce gets its own bound:
	// the caller's turn waits on this call.
	ctx := context.Background()
	// The relay itself is the Actor: no human asked for these branches,
	// so the seed mentions nobody, and the prompt says the relay did it.
	actor := &zulipproto.Message{SenderName: h.cfg.BotFullName}

	results := make([]zulipmcp.BranchResult, len(tasks))
	var made []string
	var links []string
	for i, task := range tasks {
		p, err := h.planBranchTask(ctx, caller.Key, dest, channel, actor, task)
		if err != nil {
			results[i].Error = err.Error()
			continue
		}
		conv, link, err := h.branchOnce(ctx, p)
		if err != nil {
			results[i].Error = err.Error()
			continue
		}
		results[i] = zulipmcp.BranchResult{Topic: conv.Key.Topic, Link: link, ConvID: conv.ID}
		made = append(made, "- "+link+" — "+branchGoal(p.Text))
		links = append(links, link)
	}
	if len(made) > 0 {
		h.notifyBranches(ctx, caller, channel, made, links)
	}
	return results, nil
}

// planBranchTask turns one task into a branchPlan, or says why it
// cannot.
func (h *Handler) planBranchTask(ctx context.Context, key journal.Key, dest int64, channel string, actor *zulipproto.Message, task zulipmcp.BranchTask) (branchPlan, error) {
	m, err := h.branchPoint(ctx, key, task.FromMsg)
	if err != nil {
		return branchPlan{}, err
	}
	text, author := strings.TrimSpace(task.Seed), ""
	if text == "" {
		// The :fork_and_knife: rule: the branch-point message IS the
		// seed, and its author is named so the agent is not told the
		// relay wrote somebody else's words.
		text, author = strings.TrimSpace(m.Content), h.authorOf(m)
		if text == "" {
			return branchPlan{}, fmt.Errorf("message %d has no text to use as the seed; give a seed", m.ID)
		}
	}
	return branchPlan{
		Origin:  key,
		Dest:    dest,
		Channel: channel,
		Actor:   actor,
		Author:  author,
		Parent:  journal.Parent{Key: key, MessageID: m.ID},
		Text:    text,
		Title:   task.Title,
		Gesture: branchToolGesture,
		Quiet:   true,
	}, nil
}

// branchPoint resolves a task's from_msg, refusing an id that is not in
// the caller's own topic. Zero is the newest message in the topic that
// the relay did not send.
func (h *Handler) branchPoint(ctx context.Context, key journal.Key, id int64) (*zulipproto.Message, error) {
	ctx, cancel := context.WithTimeout(ctx, originTimeout)
	defer cancel()
	if id == 0 {
		msgs, err := h.cfg.Client.Messages(ctx, zulipproto.TopicNarrow(key.StreamID, key.Topic), branchLatestWindow, 0)
		if err != nil {
			return nil, fmt.Errorf("I could not read this topic to find the latest message (%v); give from_msg", err)
		}
		for i := len(msgs) - 1; i >= 0; i-- {
			if msgs[i].SenderID != h.cfg.BotUserID {
				return &msgs[i], nil
			}
		}
		return nil, fmt.Errorf("there is no recent message from anybody but the relay in this topic; give from_msg")
	}
	// One refusal for "unreadable" and "elsewhere": two would let an
	// agent probe which message ids exist in other channels.
	// Zulip compares topic names case-insensitively, so this does too.
	m, err := h.cfg.Client.GetMessage(ctx, id)
	if err != nil || m.IsDM() || m.StreamID != key.StreamID || !strings.EqualFold(m.Topic, key.Topic) {
		if err != nil {
			h.cfg.Logf("handler: branch point %d unreadable: %v", id, err)
		}
		return nil, fmt.Errorf("message %d is not in this topic; from_msg must name a message in the conversation you are branching out of", id)
	}
	return &m, nil
}

// notifyBranches posts the call's one notification in the caller's
// topic and, when the caller is itself a branch, one line in the root
// of its branch tree. Both are best-effort: the branches exist either
// way, and the tool's result carries the links too.
func (h *Handler) notifyBranches(ctx context.Context, caller journal.Conv, channel string, made, links []string) {
	ctx, cancel := context.WithTimeout(ctx, branchTimeout)
	defer cancel()
	body := "🌿 Branched out of this topic:\n" + strings.Join(made, "\n")
	if _, err := (&convPoster{client: h.cfg.Client, key: caller.Key}).Post(ctx, body); err != nil {
		h.cfg.Logf("handler: branched out of %s but could not say so there: %v", h.describe(caller.Key), err)
	}
	root, ok := h.branchRoot(caller)
	if !ok || root.Label() == caller.Key.Label() {
		return
	}
	line := "🌿 " + topicMention(channel, caller.Key.Topic) + " branched → " + strings.Join(links, ", ")
	if _, err := (&convPoster{client: h.cfg.Client, key: root}).Post(ctx, line); err != nil {
		h.cfg.Logf("handler: branched out of %s but could not say so in the root %s: %v", h.describe(caller.Key), h.describe(root), err)
	}
}

// branchRoot walks a conversation's parent pointers to the top of its
// branch tree, and reports whether it has a parent at all.
//
// Each hop is resolved from the branch-point message, as ConvOrigin
// does, so a renamed or moved ancestor is found where it is now. The
// walk stops at the first ancestor the relay holds no conversation for
// — a human-only topic that was branched out of is a root too.
//
// This reads WHERE the ancestors are, never WHAT they say. It grants
// nothing: the one-hop history rule is unchanged.
func (h *Handler) branchRoot(c journal.Conv) (journal.Key, bool) {
	var root journal.Key
	found := false
	for range branchRootDepth {
		p, ok := h.resolveOrigin(c)
		if !ok {
			break
		}
		root, found = p.Key, true
		next, ok := h.cfg.Journal.Lookup(p.Key)
		if !ok {
			break
		}
		c = next
	}
	return root, found
}

// branchGoal is the one-line goal beside a link: the seed's first
// non-blank line, bounded.
func branchGoal(seed string) string {
	line := strings.TrimSpace(strings.SplitN(strings.TrimSpace(seed), "\n", 2)[0])
	return truncateRunes(line, branchGoalRunes)
}

// truncateRunes cuts s to at most n code points — the unit Zulip counts
// in — marking a cut with an ellipsis.
func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "…"
}
