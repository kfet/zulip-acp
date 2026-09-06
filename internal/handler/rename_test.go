package handler

import (
	"errors"
	"strings"
	"testing"

	"github.com/kfet/zulip-acp/internal/channels"
	"github.com/kfet/zulip-acp/internal/journal"
	"github.com/kfet/zulip-acp/internal/zulipmcp"
	"github.com/kfet/zulip-acp/internal/zulipproto"
)

// renameHarness is a loopback harness serving #fleet with general-chat
// naming on — the shape the feature exists for: the relay names the
// topic from the first line, the agent replaces that name with a title.
func renameHarness(t *testing.T, agent *fakeAgent) *loopHarness {
	t.Helper()
	return newLoopHarness(t, agent, func(c *Config) {
		c.Channels = channels.New(channels.Config{
			Explicit:  map[int64]string{4: "fleet"},
			Autotopic: map[int64]string{4: "fleet"},
		})
	})
}

// deliverLobby is deliverTurn for a general-chat message, with the
// triggering message seeded into the fake the way the server holds it:
// the rename path reads the anchor back before moving it.
func deliverLobby(t *testing.T, lh *loopHarness, content string) {
	t.Helper()
	lh.z.mu.Lock()
	lh.z.messages[1] = zulipproto.Message{ID: 1, SenderID: humanID, Topic: "", Content: content}
	lh.z.mu.Unlock()
	lh.deliverTurn(t, "", content)
}

// renameDuringTurn makes the agent ask for a rename mid-turn, the way
// an MCP tool call does, and records what the relay answered.
func renameDuringTurn(agent *fakeAgent, h **Handler, title string, out *string, rerr *error) {
	agent.during = func() {
		convs := (*h).cfg.Journal.Convs()
		if len(convs) != 1 {
			*rerr = errors.New("expected exactly one conversation")
			return
		}
		*out, *rerr = (*h).RenameTopic(convs[0].Key, title)
	}
}

// TestAgentRenamesAnAutoNamedTopic is the whole feature end to end: a
// general-chat message is moved to a heuristic topic, the agent renames
// it during the turn, and the rename lands — as a change_all move, so
// the conversation travels with it — only once the turn is over.
func TestAgentRenamesAnAutoNamedTopic(t *testing.T) {
	agent := newAgent("ok")
	lh := renameHarness(t, agent)
	var reply string
	var rerr error
	renameDuringTurn(agent, &lh.h, "Deferred topic rename design", &reply, &rerr)

	deliverLobby(t, lh, mention("auto move tooic never properly rensmes it!!!"))
	if rerr != nil {
		t.Fatalf("RenameTopic: %v", rerr)
	}
	if !strings.Contains(reply, "when this turn ends") {
		t.Fatalf("reply = %q; the agent must be told the rename is deferred", reply)
	}

	// Two moves: the autotopic lift out of general chat, then the
	// rename of the whole topic. The rename is anchored on the
	// triggering message and propagates to everything in the topic.
	moves := lh.z.moved()
	if len(moves) != 2 {
		t.Fatalf("moves = %v, want the autotopic move and the rename", moves)
	}
	if moves[1] != "1:Deferred topic rename design:change_all" {
		t.Fatalf("rename move = %q", moves[1])
	}
	// The journal followed inline, so the next message in the renamed
	// topic continues the same session rather than opening a new one.
	convs := lh.j.Convs()
	if len(convs) != 1 || convs[0].Topic != "Deferred topic rename design" {
		t.Fatalf("journal = %+v, want the one conversation under the new topic", convs)
	}
	if !lh.logged("agent renamed topic") {
		t.Fatalf("rename not logged: %v", lh.logs)
	}
}

// TestRenameLandsAfterTheAnswerIsPosted is the ordering that makes the
// rename deferred rather than merely late. A turn posts into the topic
// it started in — placeholder, streaming edits, rollovers, repost — so
// the topic must not move until the answer is complete. Observed from
// inside the PATCH itself, which is the one place that ordering is
// visible without racing the turn's unwinding.
func TestRenameLandsAfterTheAnswerIsPosted(t *testing.T) {
	agent := newAgent("the whole answer")
	lh := renameHarness(t, agent)
	var reply string
	var rerr error
	renameDuringTurn(agent, &lh.h, "A better title", &reply, &rerr)

	var bodyAtRename string
	// Runs with the fake's lock held, so it reads the fields directly.
	lh.z.moveHook = func() {
		if len(lh.z.moves) == 1 { // the autotopic move is already recorded: this is the rename
			bodyAtRename = lh.z.bodies[lh.z.order[len(lh.z.order)-1]]
		}
	}

	deliverLobby(t, lh, mention("first line becomes the topic"))
	if !strings.Contains(bodyAtRename, "the whole answer") {
		t.Fatalf("the topic moved while the answer was still %q", bodyAtRename)
	}
}

// TestRenameHintIsOnlyGivenForAnAutoNamedTopic: the instruction is a
// per-turn note, and it is only true for the message the autotopic move
// lifted out of general chat.
func TestRenameHintIsOnlyGivenForAnAutoNamedTopic(t *testing.T) {
	agent := newAgent("ok")
	lh := renameHarness(t, agent)
	deliverLobby(t, lh, mention("opening line"))
	lh.deliverTurn(t, "a human named this", mention("hello"))

	if len(agent.prompts) != 2 {
		t.Fatalf("prompts = %v", agent.prompts)
	}
	if !strings.Contains(agent.prompts[0], zulipmcp.ToolRenameTopic) {
		t.Fatalf("auto-named turn was not told to rename: %q", agent.prompts[0])
	}
	if !strings.Contains(agent.prompts[0], "opening line") {
		t.Fatalf("the hint does not name the placeholder topic: %q", agent.prompts[0])
	}
	if strings.Contains(agent.prompts[1], zulipmcp.ToolRenameTopic) {
		t.Fatalf("a human-named topic was told to rename itself: %q", agent.prompts[1])
	}
}

// TestRenameHintIsWithheldWithoutTheLoopback: with `relay_mcp` off
// there is no tool to call, so telling the agent to call one would be
// an instruction it can only fail.
func TestRenameHintIsWithheldWithoutTheLoopback(t *testing.T) {
	agent := newAgent("ok")
	hh := newHarness(t, agent, func(c *Config) {
		c.Channels = channels.New(channels.Config{
			Explicit:  map[int64]string{4: "fleet"},
			Autotopic: map[int64]string{4: "fleet"},
		})
	})
	hh.deliver(t, "", mention("opening line"))
	if len(agent.prompts) != 1 || strings.Contains(agent.prompts[0], zulipmcp.ToolRenameTopic) {
		t.Fatalf("prompts = %v, want no rename hint with the loopback off", agent.prompts)
	}
}

// TestRenameFailedMoveKeepsTheTopic: moving is a realm POLICY. A refusal
// is logged and the conversation carries on under its placeholder name;
// the journal must not claim a rename that did not happen.
func TestRenameFailedMoveKeepsTheTopic(t *testing.T) {
	agent := newAgent("ok")
	lh := renameHarness(t, agent)
	var reply string
	var rerr error
	renameDuringTurn(agent, &lh.h, "A better title", &reply, &rerr)
	// Fail the SECOND move only: the autotopic lift must succeed, so
	// what is refused is the rename itself. The hook runs inside
	// MoveMessage with the lock held, before the error is consulted.
	lh.z.moveHook = func() {
		if len(lh.z.moves) == 1 {
			lh.z.moveErr = errors.New("realm forbids it")
		}
	}

	deliverLobby(t, lh, mention("opening line"))
	if !lh.logged("the topic keeps its name") {
		t.Fatalf("a refused rename was not logged: %v", lh.logs)
	}
	convs := lh.j.Convs()
	if len(convs) != 1 || convs[0].Topic != "opening line" {
		t.Fatalf("journal = %+v, want the conversation still under its placeholder name", convs)
	}
}

// TestRenameJournalFailureIsLogged: the topic moved but the journal
// could not record it. The event echo would normally fix this up; the
// log line is what says it did not happen inline.
func TestRenameJournalFailureIsLogged(t *testing.T) {
	agent := newAgent("ok")
	lh := renameHarness(t, agent)
	var reply string
	var rerr error
	renameDuringTurn(agent, &lh.h, "A better title", &reply, &rerr)
	agent.hold = make(chan struct{})
	go func() {
		<-agent.entered
		lh.breakJournal(t)
		close(agent.hold)
	}()

	deliverLobby(t, lh, mention("opening line"))
	if !lh.logged("the journal did not follow") {
		t.Fatalf("a failed journal migration was not logged: %v", lh.logs)
	}
}

// TestRenameToTheSameNameDoesNothing: an agent that "renames" the topic
// to what it is already called must not cost a PATCH.
func TestRenameToTheSameNameDoesNothing(t *testing.T) {
	agent := newAgent("ok")
	lh := renameHarness(t, agent)
	var reply string
	var rerr error
	renameDuringTurn(agent, &lh.h, "opening line", &reply, &rerr)

	deliverLobby(t, lh, mention("opening line"))
	if rerr != nil {
		t.Fatalf("RenameTopic: %v", rerr)
	}
	if moves := lh.z.moved(); len(moves) != 1 {
		t.Fatalf("moves = %v, want only the autotopic move", moves)
	}
}

// TestRenameNeedsALiveTurnWithAnAnchor: the refusals the agent can see.
// A conversation the relay does not know; a conversation whose turn is
// already over; and a turn with no triggering message to address the
// edit to, which is what a scheduled prompt is.
func TestRenameNeedsALiveTurnWithAnAnchor(t *testing.T) {
	lh := renameHarness(t, newAgent("ok"))
	if _, err := lh.h.RenameTopic(journal.Channel(4, "nowhere"), "a title"); !errors.Is(err, errUnknownConv) {
		t.Fatalf("err = %v, want the unknown-conversation refusal", err)
	}
	conv := lh.engage(t, "engaged")
	if _, err := lh.h.RenameTopic(conv.Key, "a title"); !errors.Is(err, errNoAnchor) {
		t.Fatalf("err = %v, want the no-anchor refusal once the turn is over", err)
	}
	// A scheduled turn holds the conversation but carries no anchor.
	lh.h.setInflight(conv.ID, &inflightEntry{cancel: func() {}, rename: &pendingRename{}})
	if _, err := lh.h.RenameTopic(conv.Key, "a title"); !errors.Is(err, errNoAnchor) {
		t.Fatalf("err = %v, want the no-anchor refusal for a scheduled turn", err)
	}
}

// TestRenameOntoALiveTopicIsRefused: renaming onto a topic that already
// holds a conversation would merge the two on Zulip and orphan this
// session in the journal — the agent would lose the session it is
// running in to make a cosmetic change.
func TestRenameOntoALiveTopicIsRefused(t *testing.T) {
	lh := renameHarness(t, newAgent("ok"))
	occupied := lh.engage(t, "already busy")
	conv := lh.engage(t, "engaged")
	lh.h.setInflight(conv.ID, &inflightEntry{cancel: func() {}, rename: &pendingRename{anchor: 1}})
	if _, err := lh.h.RenameTopic(conv.Key, occupied.Topic); !errors.Is(err, errTopicTaken) {
		t.Fatalf("err = %v, want the taken-topic refusal", err)
	}
}

// TestSupersededTurnDoesNotRename is the race the arm is keyed on the
// TURN to prevent: a follow-up cancels the running turn and starts its
// own, so the dying turn unwinds while the new one is streaming into the
// old topic name. It must not move the topic under it.
func TestSupersededTurnDoesNotRename(t *testing.T) {
	lh := renameHarness(t, newAgent("ok"))
	conv := lh.engage(t, "engaged")
	dying := &inflightEntry{cancel: func() {}, rename: &pendingRename{anchor: 1, title: "a better title"}}
	// The successor holds the conversation; the dying turn's entry is
	// already out of the map, exactly as clearInflight leaves it.
	lh.h.setInflight(conv.ID, &inflightEntry{cancel: func() {}, rename: &pendingRename{anchor: 2}})
	before := len(lh.z.moved())
	lh.h.applyRename(conv, dying)
	if got := lh.z.moved(); len(got) != before {
		t.Fatalf("moves = %v, want none from a superseded turn", got)
	}
	if !lh.logged("a newer turn holds") {
		t.Fatalf("the dropped rename was not logged: %v", lh.logs)
	}
}

// TestRenameSkippedWhenTheAnchorLeftTheTopic: the anchor is only an
// anchor while it is still in the topic. A human moving the triggering
// message mid-turn must not make the relay rename wherever it landed.
func TestRenameSkippedWhenTheAnchorLeftTheTopic(t *testing.T) {
	lh := renameHarness(t, newAgent("ok"))
	conv := lh.engage(t, "engaged")
	armed := &inflightEntry{cancel: func() {}, rename: &pendingRename{anchor: 1, title: "a better title"}}

	lh.z.messages[1] = zulipproto.Message{ID: 1, Topic: "somewhere else"}
	lh.h.applyRename(conv, armed)
	lh.z.getErr = errors.New("gone")
	lh.h.applyRename(conv, armed)
	if got := lh.z.moved(); len(got) != 0 {
		t.Fatalf("moves = %v, want none when the anchor is not in the topic", got)
	}
	if lh.loggedCount("is no longer in it") != 2 {
		t.Fatalf("both skips must be logged: %v", lh.logs)
	}
}

// TestRenameWhoseJournalAlreadyMovedIsLogged: the topic moved but the
// journal was no longer under the old name — a human rename that landed
// first. Nothing is broken, but a silent no-op here would be indistinguishable
// from a rename that worked.
func TestRenameWhoseJournalAlreadyMovedIsLogged(t *testing.T) {
	lh := renameHarness(t, newAgent("ok"))
	conv := lh.engage(t, "engaged")
	lh.z.messages[1] = zulipproto.Message{ID: 1, Topic: "vanished"}
	stale := journal.Conv{ID: conv.ID, Key: journal.Channel(4, "vanished")}
	lh.h.applyRename(stale, &inflightEntry{cancel: func() {}, rename: &pendingRename{anchor: 1, title: "a better title"}})
	if !lh.logged("the journal had already moved on") {
		t.Fatalf("expected a moved-on log: %v", lh.logs)
	}
}

// TestEndTurnWithoutAnArmIsANoOp: a turn that armed nothing, and the
// nil entry a direct endTurn call passes.
func TestEndTurnWithoutAnArmIsANoOp(t *testing.T) {
	lh := renameHarness(t, newAgent("ok"))
	conv := lh.engage(t, "engaged")
	lh.h.applyRename(conv, nil)
	lh.h.applyRename(conv, &inflightEntry{cancel: func() {}})
	if got := lh.z.moved(); len(got) != 0 {
		t.Fatalf("moves = %v, want none", got)
	}
}
