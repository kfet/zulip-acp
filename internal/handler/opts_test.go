package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kfet/acp-kit/client"
	"github.com/kfet/acp-kit/probe"
	"github.com/kfet/zulip-acp/internal/journal"
	"github.com/kfet/zulip-acp/internal/zulipproto"
)

// --- helpers -------------------------------------------------------------

// panelWidget decodes the widget_content attached to message id, so a
// test can assert what a WEB reader would be able to tap. The shape is
// Zulip's, not ours, so it is decoded structurally rather than by
// string match.
func panelWidget(t *testing.T, hh *harness, id int64) (heading string, replies []string) {
	t.Helper()
	raw := hh.z.widget(id)
	if raw == "" {
		t.Fatalf("message %d carries no widget", id)
	}
	var w struct {
		WidgetType string `json:"widget_type"`
		ExtraData  struct {
			Type    string `json:"type"`
			Heading string `json:"heading"`
			Choices []struct {
				Type      string `json:"type"`
				ShortName string `json:"short_name"`
				LongName  string `json:"long_name"`
				Reply     string `json:"reply"`
			} `json:"choices"`
		} `json:"extra_data"`
	}
	if err := json.Unmarshal([]byte(raw), &w); err != nil {
		t.Fatalf("widget is not JSON: %v (%s)", err, raw)
	}
	if w.WidgetType != "zform" || w.ExtraData.Type != "choices" {
		t.Fatalf("widget = %s, want a zform/choices", raw)
	}
	for _, c := range w.ExtraData.Choices {
		if c.Type != "multiple_choice" {
			t.Fatalf("choice type = %q", c.Type)
		}
		if c.ShortName == "" || c.LongName == "" {
			t.Fatalf("choice %+v has no label — unreadable as a button", c)
		}
		replies = append(replies, c.Reply)
	}
	return w.ExtraData.Heading, replies
}

// msgIDs returns the ids of the messages currently on the surface, in
// post order. optsHarness resets the store but not the id counter, so
// tests must never hard-code an id.
func msgIDs(hh *harness) []int64 {
	hh.z.mu.Lock()
	defer hh.z.mu.Unlock()
	return append([]int64(nil), hh.z.order...)
}

// lastMsg is the id of the most recently posted message.
func lastMsg(t *testing.T, hh *harness) int64 {
	t.Helper()
	ids := msgIDs(hh)
	if len(ids) == 0 {
		t.Fatal("nothing was posted")
	}
	return ids[len(ids)-1]
}

// isPoll reports whether message id is the model poll. A poll is
// identified by its CONTENT, because a bot cannot attach one as
// widget_content — the message body IS the `/poll` slash command and
// Zulip builds the widget from it (see zulipproto.PollContent).
// `!opts` posts a PAIR — the poll, then the panel — so every assertion
// about "the panel" has to say which of the two it means.
func isPoll(hh *harness, id int64) bool {
	return strings.HasPrefix(hh.z.body(id), "/poll ")
}

// panelMsg is the id of the live options panel: the last message that
// is not a poll.
func panelMsg(t *testing.T, hh *harness) int64 {
	t.Helper()
	ids := msgIDs(hh)
	for i := len(ids) - 1; i >= 0; i-- {
		if !isPoll(hh, ids[i]) {
			return ids[i]
		}
	}
	t.Fatal("no panel was posted")
	return 0
}

// panelBody is the live panel's markdown.
func panelBody(t *testing.T, hh *harness) string {
	t.Helper()
	return hh.z.body(panelMsg(t, hh))
}

// pollMsg is the id of the live model poll, or 0 when none was posted.
func pollMsg(hh *harness) int64 {
	ids := msgIDs(hh)
	for i := len(ids) - 1; i >= 0; i-- {
		if isPoll(hh, ids[i]) {
			return ids[i]
		}
	}
	return 0
}

// pollWidget reads back the poll Zulip would build from message id,
// so a test can assert what a PHONE reader would be able to vote on.
// It parses the `/poll` command exactly as the server does: the rest
// of the first line is the question, every further line is an option.
func pollWidget(t *testing.T, hh *harness, id int64) (question string, options []string) {
	t.Helper()
	body := hh.z.body(id)
	rest, ok := strings.CutPrefix(body, "/poll ")
	if !ok {
		t.Fatalf("message %d is not a poll: %q", id, body)
	}
	lines := strings.Split(rest, "\n")
	for _, o := range lines[1:] {
		if o == "" {
			t.Fatal("an unlabelled poll option is unreadable")
		}
		options = append(options, o)
	}
	if len(options) == 0 {
		t.Fatalf("poll %d has no options: %q", id, body)
	}
	return lines[0], options
}

// castVote feeds the submessage event a real client's vote produces.
// The key shape is the MEASURED one: "canned,<index>" for an option
// the poll shipped with.
func castVote(t *testing.T, hh *harness, voter, id int64, option, updown int) {
	t.Helper()
	hh.h.Handle(context.Background(), submessageEvent(1, id, voter,
		"widget", fmt.Sprintf(`{"type":"vote","key":"canned,%d","vote":%d}`, option, updown)))
}

// optsHarness is an engaged DM conversation with two models, which is
// the state most panel assertions want.
func optsHarness(t *testing.T) *harness {
	t.Helper()
	hh := dmCmdHarness(t, withModels(newAgent("x"), "a/one", "a/one", "b/two"), nil)
	hh.deliverDM(t, humanID, "hello", humanID, botID)
	hh.z.reset()
	return hh
}

// --- the panel -----------------------------------------------------------

// TestOptsPostsAPairThatReadsOnAPhone: neither widget renders
// everywhere, so between them the two markdown bodies must carry every
// control — a client that draws no widget at all still has to be able
// to type its way to all of it.
func TestOptsPostsAPairThatReadsOnAPhone(t *testing.T) {
	hh := optsHarness(t)
	hh.deliverDM(t, humanID, "!opts", humanID, botID)

	panel := panelBody(t, hh)
	for _, want := range []string{"⚙️", "one", "`!new`", "`!stop`", "`!status`", "`!help`"} {
		if !strings.Contains(panel, want) {
			t.Fatalf("panel %q is missing %s", panel, want)
		}
	}
	// The typable list is on the PANEL, not on the poll: a poll
	// message's body is the literal `/poll` command, which is what a
	// client that renders no widget would show the reader.
	for _, want := range []string{"`!model a/one`", "`!model b/two`"} {
		if !strings.Contains(panel, want) {
			t.Fatalf("panel %q is missing %s", panel, want)
		}
	}
	if !strings.HasPrefix(hh.z.body(pollMsg(hh)), "/poll ") {
		t.Fatalf("poll body = %q, want the slash command", hh.z.body(pollMsg(hh)))
	}
}

// TestModelPollIsVotableAndSelfLabelling is the whole reason the menu
// moved off chips: a poll renders and votes on iOS, and each option
// carries its own TEXT, so nothing depends on a glyph drawing or on a
// positional emoji↔model mapping.
func TestModelPollIsVotableAndSelfLabelling(t *testing.T) {
	hh := optsHarness(t)
	hh.deliverDM(t, humanID, "!opts", humanID, botID)

	id := pollMsg(hh)
	if id == 0 {
		t.Fatal("no model poll was posted")
	}
	question, options := pollWidget(t, hh, id)
	if !strings.Contains(question, "one") {
		t.Fatalf("question %q does not name the current model", question)
	}
	if strings.Join(options, "|") != "one|two" {
		t.Fatalf("options = %v, want the model labels current-first", options)
	}
	if got := hh.j.Convs()[0].PollID; got != id {
		t.Fatalf("journal points at poll %d, want %d", got, id)
	}
}

// TestAVoteChangesTheModel walks the feature end to end: a vote runs
// the same `!model <id>` a human could type, is acknowledged with the
// same reaction, and repaints the pair.
func TestAVoteChangesTheModel(t *testing.T) {
	hh := optsHarness(t)
	hh.deliverDM(t, humanID, "!opts", humanID, botID)
	poll := pollMsg(hh)

	castVote(t, hh, humanID, poll, 1, 1)

	if id, ok := hh.h.modelOverride(hh.j.Convs()[0].ID); !ok || id != "b/two" {
		t.Fatalf("model override = %q/%v, want b/two", id, ok)
	}
	added, _ := hh.z.reactions()
	if !slices.Contains(added, fmt.Sprintf("%d:%s", poll, optsAckEmoji)) {
		t.Fatalf("reactions = %v — the vote was not acknowledged on the poll", added)
	}
	if pollMsg(hh) == poll {
		t.Fatal("the pair was not repainted")
	}
	if got := panelBody(t, hh); !strings.Contains(got, "**⚙️ two**") {
		t.Fatalf("new panel does not show the new state: %q", got)
	}
}

// TestAnUnVoteMeansNothing pins the measured toggle: a poll vote
// arrives as +1, then -1 when it is taken back. The selection is the
// latest POSITIVE vote — reading a -1 as a choice would let un-ticking
// an option mean "no model", which is not a state the relay has.
func TestAnUnVoteMeansNothing(t *testing.T) {
	hh := optsHarness(t)
	hh.deliverDM(t, humanID, "!opts", humanID, botID)
	poll := pollMsg(hh)
	before := msgIDs(hh)

	castVote(t, hh, humanID, poll, 1, -1)

	if id, ok := hh.h.modelOverride(hh.j.Convs()[0].ID); ok {
		t.Fatalf("an un-vote set the model to %q", id)
	}
	if len(msgIDs(hh)) != len(before) {
		t.Fatal("the un-vote posted something")
	}
}

// TestAVoteRespectsTheAllowlist: a vote is a command, and a command
// surface may never be a way around the gates a typed message walks.
func TestAVoteRespectsTheAllowlist(t *testing.T) {
	const stranger = int64(4242)
	hh := dmCmdHarness(t, withModels(newAgent("x"), "a/one", "a/one", "b/two"), func(c *Config) {
		c.AllowedUsers = map[int64]struct{}{humanID: {}}
	})
	hh.deliverDM(t, humanID, "hello", humanID, botID)
	hh.deliverDM(t, humanID, "!opts", humanID, botID)
	poll := pollMsg(hh)

	castVote(t, hh, stranger, poll, 1, 1)

	if id, ok := hh.h.modelOverride(hh.j.Convs()[0].ID); ok {
		t.Fatalf("a user the allowlist does not name set the model to %q", id)
	}
}

// TestABotCannotVote: the bot-sender snapshot is taken at startup, so
// a bot created since is recognisable only by the lookup the reaction
// path already makes. A vote runs a command, so it needs the same gate.
func TestABotCannotVote(t *testing.T) {
	hh := optsHarness(t)
	hh.deliverDM(t, humanID, "!opts", humanID, botID)
	poll := pollMsg(hh)
	const laterBot = int64(77)
	hh.z.users[laterBot] = zulipproto.User{UserID: laterBot, FullName: "Late Bot", IsBot: true}

	castVote(t, hh, laterBot, poll, 1, 1)
	castVote(t, hh, botID, poll, 1, 1)

	if id, ok := hh.h.modelOverride(hh.j.Convs()[0].ID); ok {
		t.Fatalf("a bot voted and set the model to %q", id)
	}
}

// TestAStartupSnapshotBotCannotVote: BotSenderIDs is the cheap gate,
// checked before the lookup that catches a bot created since.
func TestAStartupSnapshotBotCannotVote(t *testing.T) {
	const otherBot = int64(66)
	hh := dmCmdHarness(t, withModels(newAgent("x"), "a/one", "a/one", "b/two"), func(c *Config) {
		c.BotSenderIDs = map[int64]struct{}{otherBot: {}}
	})
	hh.deliverDM(t, humanID, "hello", humanID, botID)
	hh.deliverDM(t, humanID, "!opts", humanID, botID)

	castVote(t, hh, otherBot, pollMsg(hh), 1, 1)

	if id, ok := hh.h.modelOverride(hh.j.Convs()[0].ID); ok {
		t.Fatalf("a known bot voted and set the model to %q", id)
	}
}

// TestAWidgetEventOnARetiredConversationsMessageIsDropped: the relay's
// own message index outlives the conversation it belongs to — the
// entry maps a message id to a conv id, and `!new` retires that
// conversation without touching the index. A retired conversation
// answers to nothing.
func TestAWidgetEventOnARetiredConversationsMessageIsDropped(t *testing.T) {
	hh := optsHarness(t)
	hh.deliverDM(t, humanID, "hello again", humanID, botID)
	own := lastMsg(t, hh)
	hh.deliverDM(t, humanID, "!new", humanID, botID)
	hh.z.reset()

	hh.h.Handle(context.Background(), submessageEvent(1, own, humanID,
		"widget", `{"type":"vote","key":"canned,0","vote":1}`))

	if hh.logged("widget submessage") {
		t.Fatal("a widget event on a retired conversation's message was resolved")
	}
}

// TestAnOrphanedPollIsDeleted: the poll goes up first, so a panel that
// then fails to post would leave a second live poll in the topic —
// with the OLD pair still recorded, so only one of the two would
// resolve a vote.
func TestAnOrphanedPollIsDeleted(t *testing.T) {
	hh := optsHarness(t)
	hh.deliverDM(t, humanID, "!opts", humanID, botID)
	before := msgIDs(hh)
	// Fail exactly the panel: the poll above it has already gone up.
	hh.z.sendHook = func(content string) error {
		if strings.Contains(content, "**Session**") {
			return fmt.Errorf("zulip is down")
		}
		return nil
	}

	hh.deliverDM(t, humanID, "!opts", humanID, botID)

	if got := msgIDs(hh); len(got) != len(before) {
		t.Fatalf("%d messages, want the original pair and no orphan: %q", len(got), hh.z.stored())
	}
	if !hh.logged("posting options panel") {
		t.Fatal("the failure was not logged")
	}
	conv := hh.j.Convs()[0]
	if conv.OptsID != before[1] || conv.PollID != before[0] {
		t.Fatalf("journal moved to %d/%d, want the original %v", conv.OptsID, conv.PollID, before)
	}
}

// TestARetiredPollIsInert: a repaint deletes the old poll, but a realm
// that forbids deletion leaves it in the scrollback, still votable. A
// vote in a stale menu must not reconfigure a live conversation.
func TestARetiredPollIsInert(t *testing.T) {
	hh := optsHarness(t)
	hh.z.deleteErr = &zulipproto.APIError{Status: 400, Msg: "not permitted", Code: "BAD_REQUEST"}
	hh.deliverDM(t, humanID, "!opts", humanID, botID)
	old := pollMsg(hh)
	hh.deliverDM(t, humanID, "!opts", humanID, botID)
	if pollMsg(hh) == old {
		t.Fatal("the poll was not replaced")
	}

	castVote(t, hh, humanID, old, 1, 1)

	if id, ok := hh.h.modelOverride(hh.j.Convs()[0].ID); ok {
		t.Fatalf("a stale poll changed the model to %q", id)
	}
}

// TestAVoteMeansWhatItMeantWhenThePollWasPosted is the drift guard.
// The option→model mapping depends on the agent's model LIST, which
// can be re-probed — after `!login`, or when a provider is connected —
// without anything repainting the poll. A recomputed table would make
// option 2 select whatever now sorts second, on a poll whose own text
// says otherwise.
func TestAVoteMeansWhatItMeantWhenThePollWasPosted(t *testing.T) {
	a := withModels(newAgent("x"), "a/one", "a/one", "b/two")
	hh := dmCmdHarness(t, a, nil)
	hh.deliverDM(t, humanID, "hello", humanID, botID)
	hh.z.reset()
	hh.deliverDM(t, humanID, "!opts", humanID, botID)
	poll := pollMsg(hh)

	// The agent re-probes and now reports the models the other way up.
	a.models = []client.ModelInfo{{ID: "b/two"}, {ID: "a/one"}}

	castVote(t, hh, humanID, poll, 0, 1)

	if id, ok := hh.h.modelOverride(hh.j.Convs()[0].ID); !ok || id != "a/one" {
		t.Fatalf("model = %q/%v, want the a/one the poll still names first", id, ok)
	}
}

// TestAPollSurvivesAReload is why the option table is PERSISTED rather
// than held in memory. A graceful reload execs in place and loses every
// map on the Handler, including the per-conversation model override —
// so a relay that recomputed the table would order the options by a
// DIFFERENT effective model than the poll was posted with, and resolve
// a vote to a model other than the one written on the option.
//
// The journal is what survives, so the meaning travels with the id.
func TestAPollSurvivesAReload(t *testing.T) {
	hh := optsHarness(t)
	hh.deliverDM(t, humanID, "!model b/two", humanID, botID)
	hh.deliverDM(t, humanID, "!opts", humanID, botID)
	poll := pollMsg(hh)
	// The poll was posted with b/two current, so b/two is option 0 and
	// a/one is option 1.
	if got := hh.j.Convs()[0].PollReplies; !slices.Equal(got, []string{"!model b/two", "!model a/one"}) {
		t.Fatalf("poll replies = %v, want b/two's command first, verbatim", got)
	}

	// Everything in memory goes, exactly as an exec would take it.
	for _, c := range hh.j.Convs() {
		_ = hh.h.convo.Overrides().Set(c.ID, "")
	}

	castVote(t, hh, humanID, poll, 1, 1)

	if id, ok := hh.h.modelOverride(hh.j.Convs()[0].ID); !ok || id != "a/one" {
		t.Fatalf("model = %q/%v, want the a/one the poll's option 1 names", id, ok)
	}
}

// TestAVoteForAnOptionThePollNeverHadIsDropped: a vote naming an index
// the recorded table does not have — a hand-crafted submessage, or a
// journal write that failed. Switching to whatever happens to sit at
// that index is exactly the mis-selection the recorded table prevents.
func TestAVoteForAnOptionThePollNeverHadIsDropped(t *testing.T) {
	hh := optsHarness(t)
	hh.deliverDM(t, humanID, "!opts", humanID, botID)

	castVote(t, hh, humanID, pollMsg(hh), 9, 1)

	if id, ok := hh.h.modelOverride(hh.j.Convs()[0].ID); ok {
		t.Fatalf("a vote past the end of the list set the model to %q", id)
	}
	if !hh.logged("which offers 2") {
		t.Fatal("the dropped vote was not logged")
	}
}

// TestARetiredPollsOptionsAreReplaced: the recorded table describes
// the LIVE poll and nothing else. A repaint that left the previous
// poll's meaning in place would resolve a vote against the wrong list.
func TestARetiredPollsOptionsAreReplaced(t *testing.T) {
	hh := optsHarness(t)
	hh.deliverDM(t, humanID, "!opts", humanID, botID)
	hh.deliverDM(t, humanID, "!model b/two", humanID, botID)

	conv := hh.j.Convs()[0]
	if conv.PollID != pollMsg(hh) {
		t.Fatalf("journal poll %d is not the live one %d", conv.PollID, pollMsg(hh))
	}
	if !slices.Equal(conv.PollReplies, []string{"!model b/two", "!model a/one"}) {
		t.Fatalf("poll replies = %v, want the repainted poll's order", conv.PollReplies)
	}
}

// TestASubmessageThatIsNotAVoteIsDropped: a poll also emits
// "new_option" and "question" submessages, and a participant-added
// option is keyed by user id rather than by an index of ours.
func TestASubmessageThatIsNotAVoteIsDropped(t *testing.T) {
	hh := optsHarness(t)
	hh.deliverDM(t, humanID, "!opts", humanID, botID)
	poll := pollMsg(hh)

	for _, content := range []string{
		`{"type":"new_option","idx":1,"option":"mine"}`,
		`{"type":"vote","key":"9,1","vote":1}`,
		`not json at all`,
	} {
		hh.h.Handle(context.Background(), submessageEvent(1, poll, humanID, "widget", content))
	}

	if id, ok := hh.h.modelOverride(hh.j.Convs()[0].ID); ok {
		t.Fatalf("a non-vote submessage set the model to %q", id)
	}
}

// TestAVoteThatStoppedBeingACommandIsLoggedNotForwarded: the poll
// table and the parser are two files, and drift between them must be
// said out loud rather than sending "!model x" to the agent as prose.
func TestAVoteThatStoppedBeingACommandIsLoggedNotForwarded(t *testing.T) {
	hh := optsHarness(t)
	hh.deliverDM(t, humanID, "!opts", humanID, botID)
	poll := pollMsg(hh)
	// Removing the broker is the only way to make dispatch refuse
	// every command, which is precisely the drift being guarded
	// against.
	hh.h.cfg.Commands = nil

	castVote(t, hh, humanID, poll, 1, 1)

	if !hh.logged("which is no longer a command") {
		t.Fatal("the drift was not logged")
	}
}

// TestNoPollWithoutAConversation: a vote is resolved against
// conv.PollID, and `!opts` in a place with no journal entry allocates
// nothing. A poll nobody could act on is worse than none, because
// unlike a zform button it looks live on every client.
func TestNoPollWithoutAConversation(t *testing.T) {
	hh := dmCmdHarness(t, withModels(newAgent("x"), "a/one", "a/one"), nil)
	hh.deliverDM(t, humanID, "!opts", humanID, botID)

	if id := pollMsg(hh); id != 0 {
		t.Fatalf("poll %d posted where no vote could ever be honoured", id)
	}
	if got := hh.j.Convs(); len(got) != 0 {
		t.Fatalf("the panel allocated %v", got)
	}
}

// TestPollPostFailureLeavesThePanel: the poll is the better surface,
// not the only one — every model it offers is still typeable.
func TestPollPostFailureLeavesThePanel(t *testing.T) {
	hh := optsHarness(t)
	// Fail exactly the poll: the panel goes up first and must stand.
	hh.z.sendHook = func(content string) error {
		if strings.HasPrefix(content, "/poll ") {
			return fmt.Errorf("zulip is down")
		}
		return nil
	}
	hh.deliverDM(t, humanID, "!opts", humanID, botID)

	if id := pollMsg(hh); id != 0 {
		t.Fatalf("poll %d was posted after all", id)
	}
	if !strings.Contains(panelBody(t, hh), "`!model a/one`") {
		t.Fatalf("the panel did not carry the models itself: %q", panelBody(t, hh))
	}
	if !hh.logged("posting model poll") {
		t.Fatal("the failure was not logged")
	}
	conv := hh.j.Convs()[0]
	if conv.PollID != 0 || conv.PollReplies != nil {
		t.Fatalf("journal records poll %d meaning %v, though none was posted", conv.PollID, conv.PollReplies)
	}
}

// TestOptsButtonsAreOnlyEverTypeableCommands: a click sends the reply
// string as an ordinary message, so every button must be a command the
// relay already parses — nothing new may hide behind one.
func TestOptsButtonsAreOnlyEverTypeableCommands(t *testing.T) {
	hh := optsHarness(t)
	hh.deliverDM(t, humanID, "!opts", humanID, botID)

	heading, replies := panelWidget(t, hh, panelMsg(t, hh))
	if heading == "" {
		t.Fatal("widget has no heading")
	}
	want := []string{"!new", "!stop", "!status"}
	if strings.Join(replies, "|") != strings.Join(want, "|") {
		t.Fatalf("replies = %v, want %v", replies, want)
	}
	body := panelBody(t, hh)
	for _, r := range replies {
		if !strings.Contains(body, "`"+r+"`") {
			t.Fatalf("button %q has no markdown equivalent in %q", r, body)
		}
	}
}

// TestOptsNeverOffersAModelTheAgentLacks pins the rule that makes the
// buttons safe: the list comes from the agent's own probe, capped, with
// the current model first and the remainder reachable by filter.
func TestOptsNeverOffersAModelTheAgentLacks(t *testing.T) {
	ids := make([]string, 0, optsModelCap+3)
	for i := 0; i < optsModelCap+3; i++ {
		ids = append(ids, fmt.Sprintf("p/m%d", i))
	}
	hh := dmCmdHarness(t, withModels(newAgent("x"), "p/m4", ids...), nil)
	hh.deliverDM(t, humanID, "hello", humanID, botID)
	hh.z.reset()
	hh.deliverDM(t, humanID, "!opts", humanID, botID)

	poll := pollMsg(hh)
	_, options := pollWidget(t, hh, poll)
	if len(options) != optsModelCap {
		t.Fatalf("%d poll options, want the %d cap", len(options), optsModelCap)
	}
	if options[0] != "m4" {
		t.Fatalf("current model is not first: %v", options)
	}
	for _, reply := range hh.j.Convs()[0].PollReplies {
		if !strings.HasPrefix(reply, "!model p/m") {
			t.Fatalf("poll offers unknown model %q", reply)
		}
	}
	if body := panelBody(t, hh); !strings.Contains(body, "and 3 more") {
		t.Fatalf("panel %q does not say how to reach the rest", body)
	}
}

// TestOptsWithNoModelsPointsAtLogin: an agent with no provider
// connected has nothing to offer, and the panel must say what to do
// rather than render an empty Model section.
func TestOptsWithNoModelsPointsAtLogin(t *testing.T) {
	hh := dmCmdHarness(t, newAgent("x"), nil)
	hh.deliverDM(t, humanID, "hello", humanID, botID)
	hh.z.reset()
	hh.deliverDM(t, humanID, "!opts", humanID, botID)

	body := hh.only(t)
	if !strings.Contains(body, "No models available") || !strings.Contains(body, "!login") {
		t.Fatalf("panel = %q", body)
	}
	// No models means no poll: a menu with nothing in it is not a
	// degraded menu, it is a broken one.
	if id := pollMsg(hh); id != 0 {
		t.Fatalf("an empty model poll %d was posted", id)
	}
	// Session buttons still make sense with no provider.
	if _, replies := panelWidget(t, hh, lastMsg(t, hh)); len(replies) != 3 {
		t.Fatalf("replies = %v, want only the session buttons", replies)
	}
}

// TestOptsShowsTheConversationsOwnModel: the header is a state
// readout, so a conversation with a sticky override must show the
// override and not the agent's default.
func TestOptsShowsTheConversationsOwnModel(t *testing.T) {
	hh := optsHarness(t)
	hh.deliverDM(t, humanID, "!model b/two", humanID, botID)
	hh.deliverDM(t, humanID, "!opts", humanID, botID)

	if body := panelBody(t, hh); !strings.Contains(body, "**⚙️ two**") {
		t.Fatalf("panel header does not show the override: %q", body)
	}
	if body := panelBody(t, hh); !strings.Contains(body, "`!model b/two` ←") {
		t.Fatalf("panel does not mark the current model: %q", body)
	}
}

// --- one live panel ------------------------------------------------------

// TestAWidgetPanelCanNeverBeEdited pins the server rule the whole
// lifecycle is built around, so nobody "simplifies" the re-post away.
// Measured on Zulip 12.2: PATCH on a message carrying widget_content
// returns 400 "Widgets cannot be edited."
func TestAWidgetPanelCanNeverBeEdited(t *testing.T) {
	hh := optsHarness(t)
	hh.deliverDM(t, humanID, "!opts", humanID, botID)
	id := lastMsg(t, hh)
	if hh.z.widget(id) == "" {
		t.Fatal("the panel carries no widget, so this proves nothing")
	}
	err := hh.z.EditMessage(context.Background(), id, "anything")
	if !zulipproto.RejectedByServer(err) {
		t.Fatalf("editing a widget message = %v, want a 4xx refusal", err)
	}
}

// TestKnobChangeReplacesThePanel: changing a setting must not grow the
// topic. The ack is a reaction, and the panel is re-posted with the old
// one deleted — a widget message cannot be edited in place.
func TestKnobChangeReplacesThePanel(t *testing.T) {
	hh := optsHarness(t)
	hh.deliverDM(t, humanID, "!opts", humanID, botID)
	first := panelMsg(t, hh)
	hh.deliverDM(t, humanID, "!model b/two", humanID, botID)

	if got := hh.z.count(); got != 2 {
		t.Fatalf("%d messages live, want exactly one panel and one poll: %q", got, hh.z.stored())
	}
	second := panelMsg(t, hh)
	if second == first {
		t.Fatal("the panel was not replaced")
	}
	if got := hh.z.body(second); !strings.Contains(got, "**⚙️ two**") {
		t.Fatalf("new panel does not show the new state: %q", got)
	}
	if added, _ := hh.z.reactions(); len(added) == 0 || !strings.HasSuffix(added[len(added)-1], ":"+optsAckEmoji) {
		t.Fatalf("no reaction ack: %v", added)
	}
}

// TestTheAgentsOwnModelChangeUpdatesThePanel: the loopback tool goes
// through the same broker action as a typed command, so the panel must
// not be left claiming a model the conversation no longer uses.
func TestTheAgentsOwnModelChangeUpdatesThePanel(t *testing.T) {
	hh := optsHarness(t)
	hh.deliverDM(t, humanID, "!opts", humanID, botID)
	if err := hh.h.SetModelOverride(journal.DM([]int64{humanID, botID}).Token(), "b/two"); err != nil {
		t.Fatalf("SetModelOverride: %v", err)
	}
	if got := panelBody(t, hh); !strings.Contains(got, "**⚙️ two**") {
		t.Fatalf("panel = %q", got)
	}
	if got := hh.z.count(); got != 2 {
		t.Fatalf("%d messages live, want one panel and one poll", got)
	}
}

// TestKnobChangeWithNoPanelStaysQuiet: a state change is not a reason
// to start posting a panel at a conversation that never asked for one.
func TestKnobChangeWithNoPanelStaysQuiet(t *testing.T) {
	hh := optsHarness(t)
	hh.deliverDM(t, humanID, "!model b/two", humanID, botID)
	if got := hh.z.count(); got != 0 {
		t.Fatalf("knob change posted %q", hh.z.stored())
	}
}

// TestAskingAgainMovesThePanelToTheBottom: a panel scrolled a hundred
// messages up is not a menu, so an explicit request re-posts it — and
// the previous one is deleted, so exactly one is ever live.
func TestAskingAgainMovesThePanelToTheBottom(t *testing.T) {
	hh := optsHarness(t)
	hh.deliverDM(t, humanID, "!opts", humanID, botID)
	first := panelMsg(t, hh)
	hh.deliverDM(t, humanID, "!opts", humanID, botID)

	ids := msgIDs(hh)
	if len(ids) != 2 {
		t.Fatalf("%d messages live, want one panel and one poll: %q", len(ids), hh.z.stored())
	}
	panel := panelMsg(t, hh)
	if panel == first {
		t.Fatal("the panel did not move")
	}
	if !strings.Contains(hh.z.body(panel), "**⚙️") {
		t.Fatalf("new panel = %q", hh.z.body(panel))
	}
	conv := hh.j.Convs()[0]
	if conv.OptsID != panel || conv.PollID != pollMsg(hh) {
		t.Fatalf("journal points at %d/%d, want %d/%d", conv.OptsID, conv.PollID, panel, pollMsg(hh))
	}
}

// TestPanelIsRewrittenWhenItCannotBeDeleted: deleting one's own message
// is a realm policy and time-limited, so the fallback matters. A
// widget-less panel can still be edited into a pointer line.
func TestPanelIsRewrittenWhenItCannotBeDeleted(t *testing.T) {
	hh := optsHarness(t)
	hh.z.widgetErr = &zulipproto.APIError{Status: 400, Msg: "widgets are disabled", Code: "BAD_REQUEST"}
	hh.deliverDM(t, humanID, "!opts", humanID, botID)
	first := lastMsg(t, hh)
	hh.z.deleteErr = &zulipproto.APIError{Status: 400, Msg: "not permitted", Code: "BAD_REQUEST"}
	hh.deliverDM(t, humanID, "!opts", humanID, botID)

	if got := hh.z.body(first); got != supersededPanel {
		t.Fatalf("old panel = %q, want the pointer line", got)
	}
	if got := hh.z.count(); got != 4 {
		t.Fatalf("%d messages, want both pointers plus the new pair: %q", got, hh.z.stored())
	}
}

// TestPanelIsLeftAloneWhenNeitherDeleteNorEditWorks: a widget panel on
// a realm that forbids deletion cannot be retired at all. It is stale,
// never wrong — every button on it is still a valid command — so the
// new panel goes up anyway.
func TestPanelIsLeftAloneWhenNeitherDeleteNorEditWorks(t *testing.T) {
	hh := optsHarness(t)
	hh.deliverDM(t, humanID, "!opts", humanID, botID)
	hh.z.deleteErr = &zulipproto.APIError{Status: 400, Msg: "not permitted", Code: "BAD_REQUEST"}
	hh.deliverDM(t, humanID, "!opts", humanID, botID)

	if got := hh.z.count(); got != 4 {
		t.Fatalf("%d messages, want the stale pair plus the new one", got)
	}
	if !hh.logged("retiring options panel") {
		t.Fatal("the failure was not logged")
	}
}

// TestRetiringAnAlreadyDeletedPanelIsSilent: a human can delete the
// panel, or a topic move can take it out of reach. That is the state
// retirement wanted, so it must not be reported as a fault or chased
// with an edit that cannot work either.
func TestRetiringAnAlreadyDeletedPanelIsSilent(t *testing.T) {
	hh := optsHarness(t)
	hh.deliverDM(t, humanID, "!opts", humanID, botID)
	for _, id := range msgIDs(hh) {
		if err := hh.z.DeleteMessage(context.Background(), id); err != nil {
			t.Fatalf("pre-delete: %v", err)
		}
	}
	hh.deliverDM(t, humanID, "!opts", humanID, botID)

	if got := hh.z.count(); got != 2 {
		t.Fatalf("%d messages, want just the new pair", got)
	}
	if hh.logged("options panel") {
		t.Fatal("a panel that was already gone was reported as a failure")
	}
}

// TestPanelRetirementSurvivesAnUnreachableServer: a transport failure
// is not a refusal, and must not be mistaken for one.
func TestPanelRetirementSurvivesAnUnreachableServer(t *testing.T) {
	hh := optsHarness(t)
	hh.deliverDM(t, humanID, "!opts", humanID, botID)
	hh.z.deleteErr = fmt.Errorf("connection reset")
	hh.deliverDM(t, humanID, "!opts", humanID, botID)

	if !hh.logged("deleting options panel") {
		t.Fatal("the failure was not logged")
	}
	if !hh.logged("deleting model poll") {
		t.Fatal("the poll's retirement failure was not logged")
	}
	if got := hh.z.count(); got != 4 {
		t.Fatalf("%d messages, want the undeleted pair plus the new one", got)
	}
}

// TestPanelSurvivesNew: `!new` retires the conversation, but the panel
// is a property of the PLACE and stays the one being updated.
func TestPanelSurvivesNew(t *testing.T) {
	hh := optsHarness(t)
	hh.deliverDM(t, humanID, "!opts", humanID, botID)
	panel, poll := panelMsg(t, hh), pollMsg(hh)
	hh.deliverDM(t, humanID, "!new", humanID, botID)

	for _, c := range hh.j.Convs() {
		if c.Retired {
			if c.OptsID != 0 || c.PollID != 0 {
				t.Fatalf("retired conversation kept the control pair %d/%d", c.OptsID, c.PollID)
			}
			continue
		}
		if c.OptsID != panel || c.PollID != poll {
			t.Fatalf("fresh conversation controls = %d/%d, want %d/%d", c.OptsID, c.PollID, panel, poll)
		}
	}
}

// --- degradation ---------------------------------------------------------

// TestPanelPostsWithoutItsWidget: a server with widgets disabled must
// still get the panel. The markdown is the product.
func TestPanelPostsWithoutItsWidget(t *testing.T) {
	hh := optsHarness(t)
	hh.z.widgetErr = &zulipproto.APIError{Status: 400, Msg: "widgets are disabled", Code: "BAD_REQUEST"}
	hh.deliverDM(t, humanID, "!opts", humanID, botID)

	// The POLL is untouched by this: its content is the `/poll` slash
	// command, so it needs no widget_content at all and a server with
	// widgets disabled still gets a votable menu. Only the panel's
	// zform is lost, and the panel still carries every command as
	// markdown.
	body := panelBody(t, hh)
	if !strings.Contains(body, "`!model a/one`") {
		t.Fatalf("panel = %q", body)
	}
	if got := hh.z.widget(panelMsg(t, hh)); got != "" {
		t.Fatalf("widget was sent after all: %q", got)
	}
	if id := pollMsg(hh); id == 0 {
		t.Fatal("the poll was lost with the zform, though it needs no widget_content")
	}
	if !hh.logged("widget refused") {
		t.Fatal("the degradation was not logged")
	}
}

// TestPanelPostFailureIsLoggedNotFatal: the panel is decoration over
// controls that all still work by typing.
func TestPanelPostFailureIsLoggedNotFatal(t *testing.T) {
	hh := optsHarness(t)
	hh.z.sendErr = fmt.Errorf("zulip is down")
	hh.deliverDM(t, humanID, "!opts", humanID, botID)

	if got := hh.z.count(); got != 0 {
		t.Fatalf("something was posted: %q", hh.z.stored())
	}
	if !hh.logged("posting options panel") {
		t.Fatal("the failure was not logged")
	}
}

// TestWidgetRefusalIsToldFromAnUnreachableServer: only a REFUSAL is
// worth retrying without the widget. A transport failure would fail the
// same way twice.
func TestWidgetRefusalIsToldFromAnUnreachableServer(t *testing.T) {
	hh := optsHarness(t)
	hh.z.widgetErr = fmt.Errorf("connection reset")
	hh.deliverDM(t, humanID, "!opts", humanID, botID)

	if got := hh.z.count(); got != 0 {
		t.Fatalf("a transport failure was retried: %q", hh.z.stored())
	}
	if hh.logged("widget refused") {
		t.Fatal("a transport failure was reported as a refusal")
	}
	if !hh.logged("posting options panel") {
		t.Fatal("the failure was not logged")
	}
}

// TestPanelIDPersistenceFailureIsLogged: the journal is a cache. A
// write failure costs one extra panel later, never correctness.
func TestPanelIDPersistenceFailureIsLogged(t *testing.T) {
	hh := optsHarness(t)
	hh.breakJournal(t)
	hh.deliverDM(t, humanID, "!opts", humanID, botID)

	if got := hh.z.count(); got != 2 {
		t.Fatalf("the pair was not posted: %q", hh.z.stored())
	}
	if !hh.logged("recording options panel") {
		t.Fatal("the failure was not logged")
	}
}

// TestOptsAllocatesNoConversation: commands run off Journal.Lookup and
// never Ensure, so asking for the panel in a conversation the relay has
// never answered in must leave nothing on disk.
func TestOptsAllocatesNoConversation(t *testing.T) {
	hh := dmCmdHarness(t, withModels(newAgent("x"), "a/one", "a/one"), nil)
	hh.deliverDM(t, humanID, "!opts", humanID, botID)

	if got := hh.z.count(); got != 1 {
		t.Fatalf("panel was not posted: %q", hh.z.stored())
	}
	if got := hh.j.Convs(); len(got) != 0 {
		t.Fatalf("the panel allocated %v", got)
	}
	if got := hh.a.prompts; len(got) != 0 {
		t.Fatalf("!opts reached the agent: %q", got)
	}
}

// TestPanelSaysWhenThereIsNoConversationYet: in a channel a button's
// reply would not even be answered in an unengaged topic, so the panel
// must say what to do instead of offering controls that do nothing.
func TestPanelSaysWhenThereIsNoConversationYet(t *testing.T) {
	for _, tc := range []struct {
		name    string
		deliver func(hh *harness)
		want    string
	}{
		{"dm", func(hh *harness) { hh.deliverDM(t, humanID, "!opts", humanID, botID) }, "send a message"},
		{"channel", func(hh *harness) { hh.deliver(t, "fresh", mention("!opts")) }, "@-mention me"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hh := dmCmdHarness(t, withModels(newAgent("x"), "a/one", "a/one"), nil)
			tc.deliver(hh)
			body := hh.only(t)
			if !strings.Contains(body, "No conversation here yet") || !strings.Contains(body, tc.want) {
				t.Fatalf("panel = %q", body)
			}
		})
	}
}

// TestEngagedPanelDropsTheHint: once the conversation exists the hint
// would be a lie, and a panel that is also a status line must not lie.
func TestEngagedPanelDropsTheHint(t *testing.T) {
	hh := optsHarness(t)
	hh.deliverDM(t, humanID, "!opts", humanID, botID)
	if body := panelBody(t, hh); strings.Contains(body, "No conversation here yet") {
		t.Fatalf("panel = %q", body)
	}
}

// --- discoverability -----------------------------------------------------

// TestUnknownCommandTeaches is the whole point of the failure mode: an
// unknown `!foo` answers with the menu instead of being forwarded to
// the agent as prose.
func TestUnknownCommandTeaches(t *testing.T) {
	hh := optsHarness(t)
	hh.deliverDM(t, humanID, "!frobnicate", humanID, botID)

	body := panelBody(t, hh)
	for _, want := range []string{"Unknown command `!frobnicate`", "!!frobnicate", "**⚙️", "`!model a/one`"} {
		if !strings.Contains(body, want) {
			t.Fatalf("reply %q is missing %s", body, want)
		}
	}
	if id := pollMsg(hh); id == 0 {
		t.Fatal("the menu came without its model poll")
	}
	if got := hh.a.prompts; len(got) != 1 {
		t.Fatalf("the typo burned an agent turn: %q", got)
	}
	if got := hh.j.Convs()[0].OptsID; got != panelMsg(t, hh) {
		t.Fatalf("the menu was not adopted as the panel (OptsID %d)", got)
	}
}

// TestHelpAdvertisesOpts: `!help` is composed in acp-kit and cannot
// know about a Zulip-only command, so the relay appends it.
func TestHelpAdvertisesOpts(t *testing.T) {
	hh := optsHarness(t)
	hh.deliverDM(t, humanID, "!help", humanID, botID)
	if got := hh.only(t); !strings.Contains(got, "`!opts`") {
		t.Fatalf("help = %q", got)
	}
}

// TestDecorateLeavesOtherRepliesAlone: only `!help` grows a line.
func TestDecorateLeavesOtherRepliesAlone(t *testing.T) {
	hh := optsHarness(t)
	for _, tc := range []struct{ name, in, out string }{
		{"not a command", "hello", "reply"},
		{"another command", "!status", "reply"},
		{"empty outcome", "!help", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := hh.h.decorate(tc.in, tc.out); got != tc.out {
				t.Fatalf("decorate(%q, %q) = %q", tc.in, tc.out, got)
			}
		})
	}
}

// TestDecoratePlacesTheLineWithTheRelayCommands: the broker's help ends
// with an optional "Agent commands:" section, and `!opts` filed under
// that heading would be a lie about who runs it. It goes next to
// `!help`, or — if the shared text ever stops listing `!help` at all —
// at the end, which is degraded but never wrong.
func TestDecoratePlacesTheLineWithTheRelayCommands(t *testing.T) {
	hh := optsHarness(t)
	for _, tc := range []struct{ name, out, want string }{
		{
			name: "next to !help",
			out:  "Available commands:\n\n- `!help` — show this\n- `!status` — state\n\nAgent commands:\n\n- `!reload`\n",
			want: "Available commands:\n\n- `!help` — show this\n" + optsHelpLine + branchHelp + "- `!status` — state\n\nAgent commands:\n\n- `!reload`\n",
		},
		{
			name: "no !help bullet to anchor to",
			out:  "Commands:\n\n- `!status`\n",
			want: "Commands:\n\n- `!status`\n" + optsHelpLine + branchHelp,
		},
		{
			name: "unterminated !help bullet",
			out:  "- `!help` — show this",
			want: "- `!help` — show this\n" + optsHelpLine + branchHelp,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := hh.h.decorate("!help", tc.out); got != tc.want {
				t.Fatalf("decorate = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestPanelAndKnobsWinOverAPendingLogin: both run ahead of the
// pending-login path, and must. A pasted redirect URL never carries a
// sigil, so `!opts` mid-login is plainly not one — eating it as a
// malformed paste would abort the login AND lose the command.
func TestPanelAndKnobsWinOverAPendingLogin(t *testing.T) {
	agent := withModels(newAgent("x"), "a/one", "a/one", "b/two")
	agent.authMethods = []client.AuthMethod{{ID: "oauth-anthropic", Name: "Anthropic"}}
	agent.authResult = client.AuthResult{State: "needs_redirect", URL: "https://example/auth", ID: "a1"}
	hh := dmCmdHarness(t, agent, nil)
	hh.deliverDM(t, humanID, "hello", humanID, botID)
	hh.deliverDM(t, humanID, "!login anthropic", humanID, botID)
	token := journal.DM([]int64{humanID, botID}).Token()
	if !hh.h.cfg.Commands.HasPending(token) {
		t.Fatal("the login did not start")
	}
	hh.z.reset()

	hh.deliverDM(t, humanID, "!opts", humanID, botID)
	if body := panelBody(t, hh); !strings.Contains(body, "**⚙️") {
		t.Fatalf("mid-login !opts = %q", body)
	}
	hh.deliverDM(t, humanID, "!model b/two", humanID, botID)
	if _, ok := hh.h.modelOverride(hh.j.Convs()[0].ID); !ok {
		t.Fatal("a mid-login knob change was swallowed by the login")
	}
	if !hh.h.cfg.Commands.HasPending(token) {
		t.Fatal("the login was aborted by a command that is not a redirect paste")
	}
}

// --- parsing -------------------------------------------------------------

func TestIsOpts(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want bool
	}{
		{"plain", "!opts", true},
		{"padded", "  !opts  ", true},
		{"phone capitalisation", "!Opts", true},
		{"dot sigil", ".opts", true},
		{"no sigil", "opts", false},
		{"prose that starts with it", "!opts why is this slow", false},
		{"other command", "!status", false},
		{"empty", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isOpts(tc.in); got != tc.want {
				t.Fatalf("isOpts(%q) = %v", tc.in, got)
			}
		})
	}
}

// TestModelKnobOnlyMatchesAnExactID: a filter is a listing, not a
// change. Switching models off an approximate match would be the worst
// kind of surprise.
func TestModelKnobOnlyMatchesAnExactID(t *testing.T) {
	hh := dmCmdHarness(t, withModels(newAgent("x"), "a/one", "a/one", "b/two"), nil)
	for _, tc := range []struct {
		name, in, want string
	}{
		{"exact id", "!model b/two", "b/two"},
		{"padded", "!model   b/two  ", "b/two"},
		{"capitalised verb", "!Model b/two", "b/two"},
		{"filter", "!model two", ""},
		{"bare", "!model", ""},
		{"other command", "!status now", ""},
		{"no sigil", "model b/two", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := hh.h.modelKnob(tc.in)
			if (tc.want == "") == ok {
				t.Fatalf("modelKnob(%q) = %q, %v", tc.in, got, ok)
			}
			if got != tc.want {
				t.Fatalf("modelKnob(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestKnobFailureFallsThroughToTheBroker: a change that cannot be
// applied must be SPOKEN, not silently reacted to — the reaction means
// "done", and it would be a lie here.
func TestKnobFailureFallsThroughToTheBroker(t *testing.T) {
	hh := dmCmdHarness(t, withModels(newAgent("x"), "a/one", "a/one", "b/two"), nil)
	hh.deliverDM(t, humanID, "!model b/two", humanID, botID)

	if !strings.Contains(hh.only(t), "no conversation here yet") {
		t.Fatalf("reply = %q", hh.only(t))
	}
	if added, _ := hh.z.reactions(); len(added) != 0 {
		t.Fatalf("a failed change was acked: %v", added)
	}
}

// TestKnobAckSurvivesAReactionFailure: reactions are decoration.
func TestKnobAckSurvivesAReactionFailure(t *testing.T) {
	hh := optsHarness(t)
	hh.z.reactErr = fmt.Errorf("no such emoji")
	hh.deliverDM(t, humanID, "!model b/two", humanID, botID)

	if !hh.logged("adding :" + optsAckEmoji + ":") {
		t.Fatal("the failure was not logged")
	}
	if _, ok := hh.h.modelOverride(hh.j.Convs()[0].ID); !ok {
		t.Fatal("the change itself did not stick")
	}
}

// TestModelOptionFallsBackToTheID: an agent that reports a model with
// no human name still gets a labelled poll option — an unlabelled one
// is unvotable.
func TestModelOptionFallsBackToTheID(t *testing.T) {
	a := newAgent("x")
	a.model = "a/one"
	a.models = []client.ModelInfo{{ID: "a/one"}}
	hh := dmCmdHarness(t, a, nil)
	hh.deliverDM(t, humanID, "hello", humanID, botID)
	hh.z.reset()
	hh.deliverDM(t, humanID, "!opts", humanID, botID)

	// pollWidget fails the test if any option lacks a label.
	if _, options := pollWidget(t, hh, pollMsg(hh)); options[0] != "one" {
		t.Fatalf("options = %v", options)
	}
	if got := hh.j.Convs()[0].PollReplies; got[0] != "!model a/one" {
		t.Fatalf("option 0 means %q", got[0])
	}
}

func TestModelLabel(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"anthropic/claude-opus-4-5", "claude-opus-4-5"},
		{"gpt-5", "gpt-5"},
		{"provider/", "provider/"},
		{"", "no model"},
	} {
		if got := modelLabel(tc.in); got != tc.want {
			t.Fatalf("modelLabel(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// --- the reaction chips --------------------------------------------------

// chipHarness is optsHarness with reactions on, which is the only
// configuration in which the panel wears chips at all.
func chipHarness(t *testing.T) *harness {
	t.Helper()
	hh := dmCmdHarness(t, withModels(newAgent("x"), "a/one", "a/one", "b/two"), func(c *Config) {
		c.Reactions = true
	})
	hh.deliverDM(t, humanID, "hello", humanID, botID)
	hh.z.reset()
	return hh
}

// chipsOn returns the emoji seeded on message id, in the order they
// were added — which is the order Zulip renders them in, and therefore
// the order the digits must line up with.
func chipsOn(hh *harness, id int64) []string {
	added, _ := hh.z.reactions()
	prefix := fmt.Sprintf("%d:", id)
	out := []string{}
	for _, a := range added {
		if rest, ok := strings.CutPrefix(a, prefix); ok {
			out = append(out, rest)
		}
	}
	return out
}

// tap feeds the reaction event a thumb produces on the panel.
func tap(t *testing.T, hh *harness, id int64, emoji string) {
	t.Helper()
	hh.h.Handle(context.Background(), reactionEvent(humanID, id, emoji, zulipproto.ReactionAdd))
}

// TestPanelSeedsItsChipsInOrder: Zulip renders a message's reactions
// in first-added order, so a concurrent seed would draw the row out of
// step with the legend that names it.
func TestPanelSeedsItsChipsInOrder(t *testing.T) {
	hh := chipHarness(t)
	hh.deliverDM(t, humanID, "!opts", humanID, botID)

	panel := panelMsg(t, hh)
	want := []string{optsNewEmoji, optsStopEmoji, optsStatusEmoji}
	if got := chipsOn(hh, panel); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("chips = %v, want %v", got, want)
	}
	if !strings.Contains(hh.z.body(panel), optsReactionFooter()) {
		t.Fatalf("panel %q does not explain its chips", hh.z.body(panel))
	}
}

// TestNoModelChipsAreSeeded: the digit chips are GONE. MEASURED, the
// iOS client did not draw `:one:` and `:two:` even with every
// reaction present on the server, so the current model and the one
// below it were unreachable by thumb — on the client the chips existed
// for. The poll replaces them, and a chip row that silently drops its
// first two entries must not come back.
func TestNoModelChipsAreSeeded(t *testing.T) {
	hh := chipHarness(t)
	hh.deliverDM(t, humanID, "!opts", humanID, botID)

	for _, e := range chipsOn(hh, panelMsg(t, hh)) {
		if e != optsNewEmoji && e != optsStopEmoji && e != optsStatusEmoji {
			t.Fatalf("chip :%s: is not a session control", e)
		}
	}
	for _, c := range hh.h.optsChips() {
		if strings.Contains(c.reply, "model") {
			t.Fatalf("chip :%s: still means %q", c.emoji, c.reply)
		}
	}
}

// TestChipsMatchTheButtons: a chip and the zform button beside it must
// carry the SAME command, or the two surfaces mean different things.
func TestChipsMatchTheButtons(t *testing.T) {
	hh := chipHarness(t)
	hh.deliverDM(t, humanID, "!opts", humanID, botID)

	_, replies := panelWidget(t, hh, panelMsg(t, hh))
	chips := hh.h.optsChips()
	if len(chips) != len(replies) {
		t.Fatalf("%d chips against %d buttons", len(chips), len(replies))
	}
	for i, c := range chips {
		if c.reply != replies[i] {
			t.Fatalf("chip %d is %q but button %d is %q", i, c.reply, i, replies[i])
		}
	}
}

// TestNoChipsWhenReactionsAreOff: with "reactions": false the relay
// does not subscribe to reaction events at all, so a seeded chip would
// be a button that provably cannot work. None is seeded, and the
// footer that would explain them is not promised either.
func TestNoChipsWhenReactionsAreOff(t *testing.T) {
	hh := optsHarness(t)
	hh.deliverDM(t, humanID, "!opts", humanID, botID)

	if got := chipsOn(hh, panelMsg(t, hh)); len(got) != 0 {
		t.Fatalf("chips = %v, want none", got)
	}
	if body := panelBody(t, hh); strings.Contains(body, "Tap a chip") {
		t.Fatalf("panel %q promises chips it did not seed", body)
	}
}

// TestTappingASessionChipRunsTheCommand: the three constant chips go
// through the broker exactly as the typed commands do.
func TestTappingASessionChipRunsTheCommand(t *testing.T) {
	hh := chipHarness(t)
	hh.deliverDM(t, humanID, "!opts", humanID, botID)
	panel := panelMsg(t, hh)

	tap(t, hh, panel, optsStatusEmoji)

	if !hh.logged("running \"!status\"") {
		t.Fatal("the status chip did not dispatch")
	}
	if got := hh.z.body(lastMsg(t, hh)); !strings.Contains(got, "**Status**") {
		t.Fatalf("no status reply was posted: %q", got)
	}
}

// TestTappingNewChipResetsTheConversation covers the one chip that
// destroys the conversation it is tapped in — and with it the panel
// id, which is what makes the leftover chips inert.
func TestTappingNewChipResetsTheConversation(t *testing.T) {
	hh := chipHarness(t)
	hh.deliverDM(t, humanID, "!opts", humanID, botID)
	panel := panelMsg(t, hh)
	before := hh.j.Convs()[0].ID

	tap(t, hh, panel, optsNewEmoji)

	// Retire keeps the old entry and mints a new id, so the test asks
	// the question `!new` actually answers: is this place's LIVE
	// conversation a different one now?
	key := hh.j.Convs()[0].Key
	fresh, ok := hh.j.Lookup(key)
	if !ok || fresh.ID == before {
		t.Fatalf("live conversation is %q/%v, want a fresh id (was %s)", fresh.ID, ok, before)
	}
}

// TestUnTappingIsANoOp pins the measured limit that shapes the design:
// a bot cannot remove another user's reaction, so an un-tap can never
// be undone and must therefore never mean anything.
func TestUnTappingIsANoOp(t *testing.T) {
	hh := chipHarness(t)
	hh.deliverDM(t, humanID, "!opts", humanID, botID)
	panel := panelMsg(t, hh)
	after := msgIDs(hh)

	hh.h.Handle(context.Background(), reactionEvent(humanID, panel, optsStatusEmoji, zulipproto.ReactionRemove))

	// Swallowed entirely, not forwarded: "kfet removed :bar_chart:
	// from your own message" is noise the agent would try to
	// interpret, about a control the user has already used.
	if n := hh.pendingReactions(); n != 0 {
		t.Fatalf("buffered %d, want the un-tap swallowed", n)
	}
	if len(msgIDs(hh)) != len(after) {
		t.Fatal("the un-tap posted something")
	}
}

// TestARetiredPanelIsInert: a repaint deletes the old panel, but a
// realm that forbids deletion leaves it in the scrollback with its
// chips on it. Tapping one must not reconfigure anything.
func TestARetiredPanelIsInert(t *testing.T) {
	hh := chipHarness(t)
	hh.z.deleteErr = &zulipproto.APIError{Status: 400, Msg: "not permitted", Code: "BAD_REQUEST"}
	hh.deliverDM(t, humanID, "!opts", humanID, botID)
	old := panelMsg(t, hh)
	hh.deliverDM(t, humanID, "!opts", humanID, botID)
	if panelMsg(t, hh) == old {
		t.Fatal("the panel was not replaced")
	}
	before := hh.z.count()

	tap(t, hh, old, optsStatusEmoji)

	if got := hh.z.count(); got != before {
		t.Fatalf("a stale chip ran its command (%d messages, was %d)", got, before)
	}
}

// TestANonChipOnThePanelStillReachesTheAgent: the panel is a message,
// and a human reacting to it with something that is not a chip is
// saying something. Only the chip emoji are consumed.
func TestANonChipOnThePanelStillReachesTheAgent(t *testing.T) {
	hh := chipHarness(t)
	hh.deliverDM(t, humanID, "!opts", humanID, botID)

	tap(t, hh, panelMsg(t, hh), "tada")

	if n := hh.pendingReactions(); n != 1 {
		t.Fatalf("buffered %d, want the non-chip reaction forwarded", n)
	}
	hh.h.DropPendingReactions()
}

// TestTheRelaysOwnSeedingDoesNotTriggerItself is the loop guard: the
// bot adds the chips itself, and every one of those is a reaction
// event on the panel with a chip emoji on it.
func TestTheRelaysOwnSeedingDoesNotTriggerItself(t *testing.T) {
	hh := chipHarness(t)
	hh.deliverDM(t, humanID, "!opts", humanID, botID)
	panel := panelMsg(t, hh)

	before := hh.z.count()
	for _, e := range chipsOn(hh, panel) {
		hh.h.Handle(context.Background(), reactionEvent(botID, panel, e, zulipproto.ReactionAdd))
	}
	if got := hh.z.count(); got != before {
		t.Fatalf("the relay's own seeding tapped its own chip (%d messages, was %d)", got, before)
	}
	if n := hh.pendingReactions(); n != 0 {
		t.Fatalf("the relay narrated its own chips to the agent (%d)", n)
	}
}

// TestAFailedChipSeedLosesOneChipNotTheMenu: an emoji a realm lacks
// must cost exactly that chip.
func TestAFailedChipSeedLosesOneChipNotTheMenu(t *testing.T) {
	hh := chipHarness(t)
	hh.z.reactErr = fmt.Errorf("no such emoji")
	hh.deliverDM(t, humanID, "!opts", humanID, botID)

	if got := chipsOn(hh, panelMsg(t, hh)); len(got) != 3 {
		t.Fatalf("attempted %v — seeding stopped at the first refusal", got)
	}
	if !hh.logged("seeding :" + optsNewEmoji + ":") {
		t.Fatal("the refusal was not logged")
	}
}

// TestAChipThatStoppedBeingACommandIsLoggedNotForwarded: the chip
// table and the parser are two files, and the failure mode if they
// ever part company is a bare "!stop" reaching the agent as prose.
func TestAChipThatStoppedBeingACommandIsLoggedNotForwarded(t *testing.T) {
	hh := chipHarness(t)
	hh.deliverDM(t, humanID, "!opts", humanID, botID)
	panel := panelMsg(t, hh)
	// Removing the broker is the only way to make dispatch refuse
	// every command, which is precisely the drift being guarded
	// against.
	hh.h.cfg.Commands = nil

	tap(t, hh, panel, optsStopEmoji)

	if !hh.logged("which is no longer a command") {
		t.Fatal("the drift was not logged")
	}
	if n := hh.pendingReactions(); n != 0 {
		t.Fatalf("the chip was forwarded to the agent anyway (%d buffered)", n)
	}
}

// TestABotCannotTapAChip: a chip is a command, and `!new` or `!stop`
// run by a bot that appeared since startup — so it is in no
// BotSenderIDs snapshot — would be a gate bypass in the one place the
// consequences are destructive rather than noisy.
func TestABotCannotTapAChip(t *testing.T) {
	hh := chipHarness(t)
	hh.deliverDM(t, humanID, "!opts", humanID, botID)
	panel := panelMsg(t, hh)
	const laterBot = int64(77)
	hh.z.users[laterBot] = zulipproto.User{UserID: laterBot, FullName: "Late Bot", IsBot: true}
	before := hh.z.count()

	hh.h.Handle(context.Background(), reactionEvent(laterBot, panel, optsStatusEmoji, zulipproto.ReactionAdd))

	if got := hh.z.count(); got != before {
		t.Fatalf("a bot tapped a chip and ran its command (%d messages, was %d)", got, before)
	}
}

// TestAnUnengagedPanelSeedsNoChips: `!opts` before the conversation
// exists posts a panel whose every control answers "there is none".
// Buttons are rendered anyway — they cost nothing — but a chip costs
// an HTTP call to place, would sit there looking live, and could never
// be honoured: nothing records the message as THE panel, so no tap can
// match. It must also leave no chip table behind, since no retirement
// is ever coming to drop one.
func TestAnUnengagedPanelSeedsNoChips(t *testing.T) {
	hh := dmCmdHarness(t, withModels(newAgent("x"), "a/one", "a/one"), func(c *Config) {
		c.Reactions = true
	})
	hh.deliverDM(t, humanID, "!opts", humanID, botID)

	if got := chipsOn(hh, lastMsg(t, hh)); len(got) != 0 {
		t.Fatalf("chips = %v on a panel nothing can tap", got)
	}
	if body := hh.z.body(lastMsg(t, hh)); strings.Contains(body, "Tap a chip") {
		t.Fatalf("panel %q promises chips it did not seed", body)
	}
}

// --- `!model <filter>`: the pair, narrowed ------------------------------

// filterHarness is an engaged DM with a catalogue big enough that the
// cap bites and a filter is worth typing.
func filterHarness(t *testing.T) *harness {
	t.Helper()
	ids := []string{"a/opus-4-5", "a/sonnet-4-5", "a/haiku-4-5", "b/GPT-5-opus", "b/gpt-5-mini"}
	hh := dmCmdHarness(t, withModels(newAgent("x"), "a/sonnet-4-5", ids...), nil)
	hh.deliverDM(t, humanID, "hello", humanID, botID)
	hh.z.reset()
	return hh
}

// TestModelFilterPostsTheNarrowedPair is the feature: `!model <filter>`
// answers with the SAME control pair `!opts` posts, with both halves
// narrowed to acp-kit's matches — not with prose the user has to retype
// by thumb.
func TestModelFilterPostsTheNarrowedPair(t *testing.T) {
	hh := filterHarness(t)
	hh.deliverDM(t, humanID, "!model opus", humanID, botID)

	// The POLL is narrowed, and case-insensitively, exactly as
	// command.MatchModels matches.
	question, options := pollWidget(t, hh, pollMsg(hh))
	if !slices.Equal(options, []string{"opus-4-5", "GPT-5-opus"}) {
		t.Fatalf("poll options = %v, want both opus models in agent order", options)
	}
	// The question says which question it is, and still says the state.
	for _, want := range []string{"opus", "now: sonnet-4-5"} {
		if !strings.Contains(question, want) {
			t.Fatalf("poll question %q lacks %q", question, want)
		}
	}
	// The PANEL is narrowed too — it is the non-widget reader's only
	// copy of the same list, so an unfiltered panel under a filtered
	// poll would contradict it.
	panel := panelBody(t, hh)
	for _, want := range []string{"matching `opus`", "`!model a/opus-4-5`", "`!model b/GPT-5-opus`", "`!model` for the full list"} {
		if !strings.Contains(panel, want) {
			t.Fatalf("panel %q lacks %q", panel, want)
		}
	}
	// The header still reads the current model — it is the state
	// readout — but the LIST must be only the matches.
	list := panel[strings.Index(panel, "**Model**"):strings.Index(panel, "**Session**")]
	if strings.Contains(list, "sonnet") || strings.Contains(list, "haiku") || strings.Contains(list, "mini") {
		t.Fatalf("model list %q includes a model the filter excluded", list)
	}
	// One live pair, in the one OptsID/PollID slot — not a second poll.
	conv := hh.j.Convs()[0]
	if conv.PollID != pollMsg(hh) || conv.OptsID != panelMsg(t, hh) {
		t.Fatalf("journal pair %d/%d is not the live one %d/%d", conv.OptsID, conv.PollID, panelMsg(t, hh), pollMsg(hh))
	}
	if !slices.Equal(conv.PollReplies, []string{"!model a/opus-4-5", "!model b/GPT-5-opus"}) {
		t.Fatalf("poll replies = %v, want the narrowed commands verbatim", conv.PollReplies)
	}
}

// TestAVoteInAFilteredPollSwitchesModel: the filtered pair is not a
// display — a vote in it resolves through the persisted table down the
// ordinary dispatch path, and lands on a model that was never option 0
// and is not the current one.
func TestAVoteInAFilteredPollSwitchesModel(t *testing.T) {
	hh := filterHarness(t)
	hh.deliverDM(t, humanID, "!model opus", humanID, botID)
	poll := pollMsg(hh)

	castVote(t, hh, humanID, poll, 1, 1)

	if id, ok := hh.h.modelOverride(hh.j.Convs()[0].ID); !ok || id != "b/GPT-5-opus" {
		t.Fatalf("model = %q,%v want the second FILTERED option", id, ok)
	}
	// And the vote repainted, which is the primitive's contract: the
	// filtered poll is gone and the fresh pair shows the new state.
	if pollMsg(hh) == poll {
		t.Fatal("the vote did not repaint the pair")
	}
}

// TestAFilteredPollDoesNotForceTheCurrentModelIn: a filter is a
// question about the catalogue. Answering "haiku" with a sonnet the
// user did not ask about would make the list something other than the
// matches — safe to omit precisely because the table is persisted.
func TestAFilteredPollDoesNotForceTheCurrentModelIn(t *testing.T) {
	hh := filterHarness(t)
	hh.deliverDM(t, humanID, "!model haiku", humanID, botID)

	_, options := pollWidget(t, hh, pollMsg(hh))
	if !slices.Equal(options, []string{"haiku-4-5"}) {
		t.Fatalf("poll options = %v, want only the match", options)
	}
	// Absent from the options, present in the question.
	if q, _ := pollWidget(t, hh, pollMsg(hh)); !strings.Contains(q, "now: sonnet-4-5") {
		t.Fatalf("question %q does not say the state", q)
	}
	castVote(t, hh, humanID, pollMsg(hh), 0, 1)
	if id, ok := hh.h.modelOverride(hh.j.Convs()[0].ID); !ok || id != "a/haiku-4-5" {
		t.Fatalf("model = %q,%v want the only match", id, ok)
	}
}

// TestAFilterMatchingNothingChangesNothing: destroying a working
// control to answer a typo is the wrong trade, and an empty poll cannot
// be posted anyway. It falls through to the broker's prose.
func TestAFilterMatchingNothingChangesNothing(t *testing.T) {
	hh := filterHarness(t)
	hh.deliverDM(t, humanID, "!opts", humanID, botID)
	panel, poll := panelMsg(t, hh), pollMsg(hh)

	hh.deliverDM(t, humanID, "!model gemini", humanID, botID)

	if got := hh.z.lastBody(); !strings.Contains(got, "none match") {
		t.Fatalf("reply = %q, want the broker's prose", got)
	}
	conv := hh.j.Convs()[0]
	if conv.OptsID != panel || conv.PollID != poll {
		t.Fatalf("the live pair moved to %d/%d from %d/%d", conv.OptsID, conv.PollID, panel, poll)
	}
	// Retiring DELETES, which the fake models by dropping the body.
	if hh.z.body(panel) == "" || hh.z.body(poll) == "" {
		t.Fatal("the live pair was retired to answer an empty filter")
	}
}

// TestBareModelKeepsTheCatalogue: bare `!model` is the CATALOGUE — the
// broker's prose names every id, which is how a reader learns what
// exists past the poll's cap. `!opts` already posts the unfiltered
// pair, so posting it here would add no surface and remove the only one
// that spells the rest out.
func TestBareModelKeepsTheCatalogue(t *testing.T) {
	hh := filterHarness(t)
	hh.deliverDM(t, humanID, "!model", humanID, botID)

	if got := hh.z.lastBody(); !strings.Contains(got, "models available") || !strings.Contains(got, "a/haiku-4-5") {
		t.Fatalf("reply = %q, want the broker's full listing", got)
	}
	if pollMsg(hh) != 0 {
		t.Fatal("bare !model posted a poll")
	}
}

// TestAnExactIDIsNeverAFilter: `!model <exact-id>` is the knob
// fast-path, and a knob change that FAILS must be spoken. Falling into
// the filter branch would answer a failed change with a menu and
// swallow the reason.
func TestAnExactIDIsNeverAFilter(t *testing.T) {
	// Not engaged, so the change fails.
	hh := dmCmdHarness(t, withModels(newAgent("x"), "a/one", "a/one", "b/two"), nil)
	hh.deliverDM(t, humanID, "!model b/two", humanID, botID)
	if !strings.Contains(hh.only(t), "no conversation here yet") {
		t.Fatalf("reply = %q, want the broker's reason", hh.only(t))
	}
}

// TestModelFilterParsing covers the shapes the filter branch claims and
// the ones it must leave alone.
func TestModelFilterParsing(t *testing.T) {
	for _, tc := range []struct {
		in, want string
	}{
		{"!model opus", "opus"},
		{"!model   opus  ", "opus"},
		{".model opus", "opus"},
		{"!model a/b c/d", "a/b c/d"},
		// Flattened at the parse boundary: the filter is echoed into a
		// poll question (where a newline splits into an extra option)
		// and a markdown list item (where it breaks the list).
		{"!model op\nus", "op us"},
		{"!model op\r\nus", "op  us"},
		{"!model", ""},
		{"!model   ", ""},
		{"!opts", ""},
		{"!models", ""},
		{"model opus", ""},
		{"", ""},
	} {
		got, ok := modelFilter(tc.in)
		if tc.want == "" {
			if ok {
				t.Fatalf("modelFilter(%q) = %q, want no filter", tc.in, got)
			}
			continue
		}
		if !ok || got != tc.want {
			t.Fatalf("modelFilter(%q) = %q,%v want %q,true", tc.in, got, ok, tc.want)
		}
	}
}

// TestAFilteredPairIsCappedToo: the cap is about readability, so it
// applies whether or not a filter narrowed the list first.
func TestAFilteredPairIsCappedToo(t *testing.T) {
	ids := make([]string, 0, optsModelCap+3)
	for i := 0; i < optsModelCap+3; i++ {
		ids = append(ids, fmt.Sprintf("p/keep%d", i))
	}
	hh := dmCmdHarness(t, withModels(newAgent("x"), "p/keep0", ids...), nil)
	hh.deliverDM(t, humanID, "hello", humanID, botID)
	hh.z.reset()
	hh.deliverDM(t, humanID, "!model keep", humanID, botID)

	if _, options := pollWidget(t, hh, pollMsg(hh)); len(options) != optsModelCap {
		t.Fatalf("%d filtered options, want the %d cap", len(options), optsModelCap)
	}
	if body := panelBody(t, hh); !strings.Contains(body, "and 3 more") {
		t.Fatalf("panel %q does not say how many the cap hid", body)
	}
}

// TestARepaintGoesBackToTheFullList: nothing persists the filter, and a
// repaint is triggered by a STATE change that has no filter in it.
// Repainting wide is the honest reading — and it is what makes the
// filtered pair self-healing rather than stuck narrow.
func TestARepaintGoesBackToTheFullList(t *testing.T) {
	hh := filterHarness(t)
	hh.deliverDM(t, humanID, "!model haiku", humanID, botID)
	if _, options := pollWidget(t, hh, pollMsg(hh)); len(options) != 1 {
		t.Fatalf("options = %v, want the filtered one", options)
	}

	// A model change from anywhere repaints.
	hh.deliverDM(t, humanID, "!model a/opus-4-5", humanID, botID)

	_, options := pollWidget(t, hh, pollMsg(hh))
	if len(options) != 5 {
		t.Fatalf("repainted options = %v, want the whole list back", options)
	}
	if options[0] != "opus-4-5" {
		t.Fatalf("repainted options = %v, want the new current model pinned first", options)
	}
	if body := panelBody(t, hh); strings.Contains(body, "matching") {
		t.Fatalf("repainted panel %q still claims a filter", body)
	}
}

// TestAnEmptyFilterIsDecidedUnderTheLock: the emptiness test lives
// inside placePanel, against the very model list the pair would be
// built from — not in a caller, before the lock. Testing it outside
// would be a read-modify-write across two Agent.Models() reads, and
// Models() is re-probed after `!login`: a list that lost its last match
// in between would pass the outer test and then retire the live pair to
// post an empty panel. This pins the no-post outcome directly.
func TestAnEmptyFilterIsDecidedUnderTheLock(t *testing.T) {
	hh := filterHarness(t)
	hh.deliverDM(t, humanID, "!opts", humanID, botID)
	panel, poll := panelMsg(t, hh), pollMsg(hh)
	conv := hh.j.Convs()[0]

	// placePanel is the locked half; call it as showFilteredPair does.
	id, chips, ok := hh.h.placePanel(context.Background(), conv.Key, "", "gemini")
	if ok || id != 0 || chips != nil {
		t.Fatalf("placePanel posted %d/%v for a filter that matches nothing", id, chips)
	}
	// Nothing posted, nothing retired, nothing rewritten.
	if hh.z.body(panel) == "" || hh.z.body(poll) == "" {
		t.Fatal("the live pair was retired for an empty filter")
	}
	if got := hh.j.Convs()[0]; got.OptsID != conv.OptsID || got.PollID != conv.PollID {
		t.Fatalf("the journal pair moved to %d/%d from %d/%d", got.OptsID, got.PollID, conv.OptsID, conv.PollID)
	}
	// And `!opts` with no models at all still posts — its body is what
	// tells the user to connect a provider, so an empty CATALOGUE must
	// not be silent the way an empty FILTER is.
	bare := dmCmdHarness(t, newAgent("x"), nil)
	bare.deliverDM(t, humanID, "hello", humanID, botID)
	bare.z.reset()
	bare.deliverDM(t, humanID, "!opts", humanID, botID)
	if body := panelBody(t, bare); !strings.Contains(body, "connect a provider") {
		t.Fatalf("panel %q does not tell an empty agent's user what to do", body)
	}
	if pollMsg(bare) != 0 {
		t.Fatal("a poll went up with no models to offer")
	}
}

// --- the empty model list, worded honestly -------------------------------
//
// "No models available — connect a provider with `!login`" used to
// cover every way the list could be empty and was true of exactly one
// of them. These tests are the other ways, on both surfaces that render
// it. The bug they guard is not hypothetical: on the fleet host a
// reload re-exec'd at 04:42:46, an `!model haiku` was the first thing
// to touch the fresh process, and a fully authenticated relay told its
// user to log in.

// TestOptsWhileProbePendingSaysSo: before the startup probe has
// finished there is no model list and NOTHING is wrong. Telling the
// user to `!login` sends them to fix a working provider.
func TestOptsWhileProbePendingSaysSo(t *testing.T) {
	hh := dmCmdHarness(t, newAgent("x"), func(c *Config) {
		c.ProbeStatus = func() probe.Status { return probe.StatusPending }
	})
	hh.deliverDM(t, humanID, "hello", humanID, botID)
	hh.z.reset()
	hh.deliverDM(t, humanID, "!opts", humanID, botID)

	body := hh.only(t)
	if !strings.Contains(body, "still starting") {
		t.Fatalf("panel = %q, want a not-ready-yet note", body)
	}
	if strings.Contains(body, "!login") {
		t.Fatalf("panel blames the provider for a pending probe: %q", body)
	}
}

// TestOptsAfterProbeFailureSaysSo: an exhausted probe budget is a relay
// problem, and the panel must not pin it on the user's credentials.
func TestOptsAfterProbeFailureSaysSo(t *testing.T) {
	hh := dmCmdHarness(t, newAgent("x"), func(c *Config) {
		c.ProbeStatus = func() probe.Status { return probe.StatusFailed }
	})
	hh.deliverDM(t, humanID, "hello", humanID, botID)
	hh.z.reset()
	hh.deliverDM(t, humanID, "!opts", humanID, botID)

	body := hh.only(t)
	if !strings.Contains(body, "could not be probed") {
		t.Fatalf("panel = %q, want a probe-failed note", body)
	}
	if strings.Contains(body, "!login") {
		t.Fatalf("panel blames the provider for a failed probe: %q", body)
	}
}

// TestModelCommandWhileProbePendingSaysSo: the SHAPE the bug was
// actually reported in. `!model haiku` against an empty catalogue never
// reaches the panel — a filter with no matches renders no panel — so it
// used to fall through to the broker's "connect a provider with
// `!login`". It must answer for the relay's real state instead.
func TestModelCommandWhileProbePendingSaysSo(t *testing.T) {
	for _, text := range []string{"!model", "!model haiku", "!model a/one"} {
		t.Run(text, func(t *testing.T) {
			hh := dmCmdHarness(t, newAgent("x"), func(c *Config) {
				c.ProbeStatus = func() probe.Status { return probe.StatusPending }
			})
			hh.deliverDM(t, humanID, "hello", humanID, botID)
			hh.z.reset()
			hh.deliverDM(t, humanID, text, humanID, botID)

			body := hh.only(t)
			if !strings.Contains(body, "still starting") {
				t.Fatalf("reply = %q, want a not-ready-yet note", body)
			}
			if strings.Contains(body, "!login") {
				t.Fatalf("reply blames the provider for a pending probe: %q", body)
			}
		})
	}
}

// TestModelCommandWithModelsIsUntouched: the interception must not
// shadow a healthy catalogue's listing.
func TestModelCommandWithModelsIsUntouched(t *testing.T) {
	hh := dmCmdHarness(t, withModels(newAgent("x"), "a/one", "a/one", "b/two"), nil)
	hh.deliverDM(t, humanID, "hello", humanID, botID)
	hh.z.reset()
	hh.deliverDM(t, humanID, "!model nosuch", humanID, botID)

	body := hh.only(t)
	if !strings.Contains(body, "none match") {
		t.Fatalf("reply = %q, want the broker's filter-miss prose", body)
	}
}

// TestProbeStatusDefaultsToProbed: a relay that reports no probe state
// is taken at its word — an empty list is the agent's real answer, which
// is what this said before the probe existed.
func TestProbeStatusDefaultsToProbed(t *testing.T) {
	h := &Handler{}
	if got := h.probeStatus(); got != probe.StatusProbed {
		t.Fatalf("probeStatus() = %v, want probed", got)
	}
	if note := h.emptyModelNote(); !strings.Contains(note, "!login") {
		t.Fatalf("note = %q, want the login advice", note)
	}
}

// TestModelCommandAfterProbeFailureSaysSo: the `!model` surface must
// report a failed probe too, not just a pending one.
func TestModelCommandAfterProbeFailureSaysSo(t *testing.T) {
	hh := dmCmdHarness(t, newAgent("x"), func(c *Config) {
		c.ProbeStatus = func() probe.Status { return probe.StatusFailed }
	})
	hh.deliverDM(t, humanID, "hello", humanID, botID)
	hh.z.reset()
	hh.deliverDM(t, humanID, "!models haiku", humanID, botID)

	body := hh.only(t)
	if !strings.Contains(body, "could not be probed") {
		t.Fatalf("reply = %q, want a probe-failed note (via the !models alias)", body)
	}
}

// TestAgentModelsIsReadInOnePlace guards the invariant the wording
// depends on. sawModels can only mean "the agent has reported models"
// if EVERY read records it, and a read added straight onto cfg.Agent
// would silently stop it meaning that — the status line's read did
// exactly that until this was caught in review. Enforced against the
// source because there is no type-level way to say it.
func TestAgentModelsIsReadInOnePlace(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	total := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		if n := strings.Count(string(b), "cfg.Agent.Models()"); n > 0 {
			total += n
			if f != "opts.go" {
				t.Errorf("%s reads cfg.Agent.Models() directly %d time(s); go through h.models()", f, n)
			}
		}
	}
	if total != 1 {
		t.Fatalf("%d direct cfg.Agent.Models() reads in the package, want exactly the one accessor", total)
	}
}

// TestEmptyModelNoteBlamesTheProviderOnceModelsHaveBeenSeen: a list the
// agent HAS reported and now has not is a provider that went away, and
// no amount of probe history changes that. Without this, a relay whose
// startup probe failed would keep blaming itself for a state the user
// can fix with `!login`.
func TestEmptyModelNoteBlamesTheProviderOnceModelsHaveBeenSeen(t *testing.T) {
	agent := withModels(newAgent("x"), "a/one", "a/one")
	h := &Handler{cfg: Config{
		Agent:       agent,
		ProbeStatus: func() probe.Status { return probe.StatusFailed },
	}}

	// Not seen yet: the probe's failure is the best explanation there is.
	if note := h.emptyModelNote(); !strings.Contains(note, "could not be probed") {
		t.Fatalf("note = %q, want the probe-failed note", note)
	}
	// One read of a non-empty catalogue is all it takes, and ANY read
	// counts — the status line's is enough, which is why every read in
	// this package goes through h.models().
	if models, _ := h.models(); len(models) != 1 {
		t.Fatalf("models() = %v, want the one model", models)
	}
	agent.models = nil
	if note := h.emptyModelNote(); !strings.Contains(note, "!login") {
		t.Fatalf("note = %q, want the login advice once a list has been seen", note)
	}
}

// TestModelCommandDoesNotAbortAPendingLogin: the interception runs ahead
// of the pending-login path on purpose. The broker treats ANY message in
// a conversation with a pending login as the pasted redirect URL
// (Broker.Handle peeks before it parses), so `!model` used to be
// consumed as a malformed paste and end the login. A sigil-prefixed
// question about models is plainly not a paste.
func TestModelCommandDoesNotAbortAPendingLogin(t *testing.T) {
	agent := newAgent("x")
	agent.authMethods = []client.AuthMethod{{ID: "oauth-anthropic", Name: "Anthropic"}}
	agent.authResult = client.AuthResult{State: "needs_redirect", URL: "https://example/auth", ID: "a1"}
	hh := dmCmdHarness(t, agent, nil)

	hh.deliverDM(t, humanID, "!login anthropic", humanID, botID)
	if !strings.Contains(hh.only(t), "https://example/auth") {
		t.Fatalf("login start: %q", hh.only(t))
	}
	hh.z.reset()

	hh.deliverDM(t, humanID, "!model", humanID, botID)
	if body := hh.only(t); !strings.Contains(body, "No models available") {
		t.Fatalf("reply = %q, want the empty-catalogue note", body)
	}
	hh.z.reset()

	// The login is still pending: the paste that follows completes it.
	agent.authResult = client.AuthResult{State: "ok"}
	hh.deliverDM(t, humanID, "https://example/callback?code=xyz", humanID, botID)
	if !strings.Contains(hh.only(t), "Authenticated") {
		t.Fatalf("`!model` ended the pending login: %q", hh.only(t))
	}
}
