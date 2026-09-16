package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/kfet/acp-kit/client"
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

// TestOptsPostsAPanelThatReadsOnAPhone: the markdown body is the
// product, because zform renders only in the web app. Everything a
// button offers must also be typeable from the text.
func TestOptsPostsAPanelThatReadsOnAPhone(t *testing.T) {
	hh := optsHarness(t)
	hh.deliverDM(t, humanID, "!opts", humanID, botID)

	body := hh.only(t)
	for _, want := range []string{"⚙️", "one", "`!model a/one`", "`!model b/two`", "`!new`", "`!stop`", "`!status`", "`!help`"} {
		if !strings.Contains(body, want) {
			t.Fatalf("panel %q is missing %s", body, want)
		}
	}
}

// TestOptsButtonsAreOnlyEverTypeableCommands: a click sends the reply
// string as an ordinary message, so every button must be a command the
// relay already parses — nothing new may hide behind one.
func TestOptsButtonsAreOnlyEverTypeableCommands(t *testing.T) {
	hh := optsHarness(t)
	hh.deliverDM(t, humanID, "!opts", humanID, botID)

	heading, replies := panelWidget(t, hh, lastMsg(t, hh))
	if heading == "" {
		t.Fatal("widget has no heading")
	}
	want := []string{"!model a/one", "!model b/two", "!new", "!stop", "!status"}
	if strings.Join(replies, "|") != strings.Join(want, "|") {
		t.Fatalf("replies = %v, want %v", replies, want)
	}
	body := hh.z.body(lastMsg(t, hh))
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

	_, replies := panelWidget(t, hh, lastMsg(t, hh))
	models := replies[:len(replies)-3] // the three session buttons
	if len(models) != optsModelCap {
		t.Fatalf("%d model buttons, want the %d cap", len(models), optsModelCap)
	}
	if models[0] != "!model p/m4" {
		t.Fatalf("current model is not first: %v", models)
	}
	for _, r := range models {
		id := strings.TrimPrefix(r, "!model ")
		if !strings.HasPrefix(id, "p/m") {
			t.Fatalf("button offers unknown model %q", id)
		}
	}
	if body := hh.z.body(lastMsg(t, hh)); !strings.Contains(body, "and 3 more") {
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

	body := hh.only(t)
	if !strings.Contains(body, "**⚙️ two**") {
		t.Fatalf("panel header does not show the override: %q", body)
	}
	if !strings.Contains(body, "`!model b/two` ←") {
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
	first := lastMsg(t, hh)
	hh.deliverDM(t, humanID, "!model b/two", humanID, botID)

	if got := hh.z.count(); got != 1 {
		t.Fatalf("%d panels live, want exactly one: %q", got, hh.z.stored())
	}
	second := lastMsg(t, hh)
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
	if got := hh.z.body(lastMsg(t, hh)); !strings.Contains(got, "**⚙️ two**") {
		t.Fatalf("panel = %q", got)
	}
	if got := hh.z.count(); got != 1 {
		t.Fatalf("%d panels live, want one", got)
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
	first := lastMsg(t, hh)
	hh.deliverDM(t, humanID, "!opts", humanID, botID)

	ids := msgIDs(hh)
	if len(ids) != 1 {
		t.Fatalf("%d panels live, want one: %q", len(ids), hh.z.stored())
	}
	if ids[0] == first {
		t.Fatal("the panel did not move")
	}
	if !strings.Contains(hh.z.body(ids[0]), "**⚙️") {
		t.Fatalf("new panel = %q", hh.z.body(ids[0]))
	}
	if got := hh.j.Convs()[0].OptsID; got != ids[0] {
		t.Fatalf("journal points at panel %d, want %d", got, ids[0])
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
	if got := hh.z.count(); got != 2 {
		t.Fatalf("%d messages, want the pointer plus the new panel", got)
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

	if got := hh.z.count(); got != 2 {
		t.Fatalf("%d messages, want the stale panel plus the new one", got)
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
	if err := hh.z.DeleteMessage(context.Background(), lastMsg(t, hh)); err != nil {
		t.Fatalf("pre-delete: %v", err)
	}
	hh.deliverDM(t, humanID, "!opts", humanID, botID)

	if got := hh.z.count(); got != 1 {
		t.Fatalf("%d messages, want just the new panel", got)
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
	if got := hh.z.count(); got != 2 {
		t.Fatalf("%d messages, want the undeleted panel plus the new one", got)
	}
}

// TestPanelSurvivesNew: `!new` retires the conversation, but the panel
// is a property of the PLACE and stays the one being updated.
func TestPanelSurvivesNew(t *testing.T) {
	hh := optsHarness(t)
	hh.deliverDM(t, humanID, "!opts", humanID, botID)
	hh.deliverDM(t, humanID, "!new", humanID, botID)

	for _, c := range hh.j.Convs() {
		if c.Retired {
			if c.OptsID != 0 {
				t.Fatalf("retired conversation kept panel %d", c.OptsID)
			}
			continue
		}
		if want := msgIDs(hh)[0]; c.OptsID != want {
			t.Fatalf("fresh conversation panel = %d, want %d", c.OptsID, want)
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

	body := hh.only(t)
	if !strings.Contains(body, "`!model a/one`") {
		t.Fatalf("panel = %q", body)
	}
	if got := hh.z.widget(lastMsg(t, hh)); got != "" {
		t.Fatalf("widget was sent after all: %q", got)
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

	if got := hh.z.count(); got != 1 {
		t.Fatalf("the panel was not posted: %q", hh.z.stored())
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
	if strings.Contains(hh.only(t), "No conversation here yet") {
		t.Fatalf("panel = %q", hh.only(t))
	}
}

// --- discoverability -----------------------------------------------------

// TestUnknownCommandTeaches is the whole point of the failure mode: an
// unknown `!foo` answers with the menu instead of being forwarded to
// the agent as prose.
func TestUnknownCommandTeaches(t *testing.T) {
	hh := optsHarness(t)
	hh.deliverDM(t, humanID, "!frobnicate", humanID, botID)

	body := hh.only(t)
	for _, want := range []string{"Unknown command `!frobnicate`", "!!frobnicate", "**⚙️", "`!model a/one`"} {
		if !strings.Contains(body, want) {
			t.Fatalf("reply %q is missing %s", body, want)
		}
	}
	if got := hh.a.prompts; len(got) != 1 {
		t.Fatalf("the typo burned an agent turn: %q", got)
	}
	if got := hh.j.Convs()[0].OptsID; got != lastMsg(t, hh) {
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
	if !strings.Contains(hh.only(t), "**⚙️") {
		t.Fatalf("mid-login !opts = %q", hh.only(t))
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

// TestModelButtonFallsBackToTheID: an agent that reports a model with
// no human name still gets a labelled button — an unlabelled one is
// untappable.
func TestModelButtonFallsBackToTheID(t *testing.T) {
	a := newAgent("x")
	a.model = "a/one"
	a.models = []client.ModelInfo{{ID: "a/one"}}
	hh := dmCmdHarness(t, a, nil)
	hh.deliverDM(t, humanID, "hello", humanID, botID)
	hh.z.reset()
	hh.deliverDM(t, humanID, "!opts", humanID, botID)

	// panelWidget fails the test if any button lacks a label.
	if _, replies := panelWidget(t, hh, lastMsg(t, hh)); replies[0] != "!model a/one" {
		t.Fatalf("replies = %v", replies)
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

// TestPanelSeedsItsChipsInOrder is the load-bearing assertion of the
// whole feature: Zulip renders a message's reactions in first-added
// order, so `one`..`six` must be seeded in exactly modelChoices order
// or the digits name the wrong models.
func TestPanelSeedsItsChipsInOrder(t *testing.T) {
	hh := chipHarness(t)
	hh.deliverDM(t, humanID, "!opts", humanID, botID)

	panel := lastMsg(t, hh)
	want := []string{"one", "two", optsNewEmoji, optsStopEmoji, optsStatusEmoji}
	if got := chipsOn(hh, panel); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("chips = %v, want %v", got, want)
	}
	body := hh.z.body(panel)
	// Two models, so the legend must say "1-2" — a footer naming six
	// chips over a row of two is the same lie as a dead button.
	if !strings.Contains(body, optsReactionFooter(2)) {
		t.Fatalf("panel %q does not explain its chips", body)
	}
}

// TestChipsMatchTheButtons: a chip and the zform button beside it must
// carry the SAME command, or the two surfaces mean different things.
func TestChipsMatchTheButtons(t *testing.T) {
	hh := chipHarness(t)
	hh.deliverDM(t, humanID, "!opts", humanID, botID)

	_, replies := panelWidget(t, hh, lastMsg(t, hh))
	_, _, _, choices := hh.h.panelState(hh.j.Convs()[0].Key)
	chips := hh.h.optsChips(choices)
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

	if got := chipsOn(hh, lastMsg(t, hh)); len(got) != 0 {
		t.Fatalf("chips = %v, want none", got)
	}
	if body := hh.z.body(lastMsg(t, hh)); strings.Contains(body, "Tap a chip") {
		t.Fatalf("panel %q promises chips it did not seed", body)
	}
}

// TestTappingAModelChipChangesTheModel walks the whole point of the
// feature end to end: a tap runs the same command the button would
// have sent, acknowledges with the same reaction, and repaints.
func TestTappingAModelChipChangesTheModel(t *testing.T) {
	hh := chipHarness(t)
	hh.deliverDM(t, humanID, "!opts", humanID, botID)
	panel := lastMsg(t, hh)

	tap(t, hh, panel, "two")

	convID := hh.j.Convs()[0].ID
	if id, ok := hh.h.modelOverride(convID); !ok || id != "b/two" {
		t.Fatalf("model override = %q/%v, want b/two", id, ok)
	}
	added, _ := hh.z.reactions()
	if !slices.Contains(added, fmt.Sprintf("%d:%s", panel, optsAckEmoji)) {
		t.Fatalf("reactions = %v — the tap was not acknowledged on the panel", added)
	}
	if lastMsg(t, hh) == panel {
		t.Fatal("the panel was not repainted")
	}
	if n := hh.pendingReactions(); n != 0 {
		t.Fatalf("the tap was ALSO narrated to the agent (%d buffered)", n)
	}
}

// TestTappingASessionChipRunsTheCommand: the three constant chips go
// through the broker exactly as the typed commands do.
func TestTappingASessionChipRunsTheCommand(t *testing.T) {
	hh := chipHarness(t)
	hh.deliverDM(t, humanID, "!opts", humanID, botID)
	panel := lastMsg(t, hh)

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
	panel := lastMsg(t, hh)
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
	panel := lastMsg(t, hh)
	after := msgIDs(hh)

	hh.h.Handle(context.Background(), reactionEvent(humanID, panel, "two", zulipproto.ReactionRemove))

	if id, ok := hh.h.modelOverride(hh.j.Convs()[0].ID); ok {
		t.Fatalf("removing :two: changed the model to %q", id)
	}
	// Swallowed entirely, not forwarded: "kfet removed :two: from your
	// own message" is noise the agent would try to interpret, about a
	// control the user has already used.
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
	old := lastMsg(t, hh)
	hh.deliverDM(t, humanID, "!opts", humanID, botID)
	if lastMsg(t, hh) == old {
		t.Fatal("the panel was not replaced")
	}

	tap(t, hh, old, "two")

	if id, ok := hh.h.modelOverride(hh.j.Convs()[0].ID); ok {
		t.Fatalf("a stale chip changed the model to %q", id)
	}
}

// TestANonChipOnThePanelStillReachesTheAgent: the panel is a message,
// and a human reacting to it with something that is not a chip is
// saying something. Only the chip emoji are consumed.
func TestANonChipOnThePanelStillReachesTheAgent(t *testing.T) {
	hh := chipHarness(t)
	hh.deliverDM(t, humanID, "!opts", humanID, botID)

	tap(t, hh, lastMsg(t, hh), "tada")

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
	panel := lastMsg(t, hh)

	for _, e := range chipsOn(hh, panel) {
		hh.h.Handle(context.Background(), reactionEvent(botID, panel, e, zulipproto.ReactionAdd))
	}
	if id, ok := hh.h.modelOverride(hh.j.Convs()[0].ID); ok {
		t.Fatalf("the relay's own seeding tapped its own chip (%q)", id)
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

	if got := chipsOn(hh, lastMsg(t, hh)); len(got) != 5 {
		t.Fatalf("attempted %v — seeding stopped at the first refusal", got)
	}
	if !hh.logged("seeding :one:") {
		t.Fatal("the refusal was not logged")
	}
}

// TestChipsNeverOutnumberTheDigits: optsModelCap and the digit table
// are one fact spelled twice, and a panel may never offer a model it
// cannot name.
func TestChipsNeverOutnumberTheDigits(t *testing.T) {
	hh := chipHarness(t)
	many := make([]zulipproto.ZFormChoice, 0, len(optsDigitEmoji)+3)
	for i := range cap(many) {
		many = append(many, zulipproto.Choice("m", "m", fmt.Sprintf("!model m/%d", i)))
	}
	chips := hh.h.optsChips(many)
	if got, want := len(chips), len(optsDigitEmoji)+3; got != want {
		t.Fatalf("%d chips from %d choices, want %d", got, len(many), want)
	}
	for i, c := range chips[:len(optsDigitEmoji)] {
		if c.emoji != optsDigitEmoji[i] {
			t.Fatalf("chip %d is :%s:, want :%s:", i, c.emoji, optsDigitEmoji[i])
		}
	}
}

// TestAChipThatStoppedBeingACommandIsLoggedNotForwarded: the chip
// table and the parser are two files, and the failure mode if they
// ever part company is a bare "!stop" reaching the agent as prose.
func TestAChipThatStoppedBeingACommandIsLoggedNotForwarded(t *testing.T) {
	hh := chipHarness(t)
	hh.deliverDM(t, humanID, "!opts", humanID, botID)
	panel := lastMsg(t, hh)
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

// TestTheChipLegendCountsTheChipsItHas: a footer naming six digit
// chips over a row of two is the same lie as a button that does
// nothing. All three phrasings are reachable — an agent with no models
// still gets the three session chips, so the panel still has a legend.
func TestTheChipLegendCountsTheChipsItHas(t *testing.T) {
	for _, tc := range []struct {
		models []string
		want   string
	}{
		{nil, "no models"},
		{[]string{"a/one"}, "1 model,"},
		{[]string{"a/one", "b/two"}, "1-2 models"},
	} {
		a := newAgent("x")
		if len(tc.models) > 0 {
			a = withModels(a, tc.models[0], tc.models...)
		}
		hh := dmCmdHarness(t, a, func(c *Config) { c.Reactions = true })
		hh.deliverDM(t, humanID, "hello", humanID, botID)
		hh.z.reset()
		hh.deliverDM(t, humanID, "!opts", humanID, botID)

		if body := hh.z.body(lastMsg(t, hh)); !strings.Contains(body, tc.want) {
			t.Fatalf("%d model(s): panel %q does not say %q", len(tc.models), body, tc.want)
		}
	}
}

// TestAChipMeansWhatItMeantWhenItWasSeeded is the drift guard. The
// mapping also depends on the agent's model LIST, which can be
// re-probed — after `!login`, or when a provider is connected —
// without anything repainting the panel. A recomputed table would make
// :one: point at whatever now sorts first, on a panel that still says
// otherwise in its own text.
func TestAChipMeansWhatItMeantWhenItWasSeeded(t *testing.T) {
	a := withModels(newAgent("x"), "a/one", "a/one", "b/two")
	hh := dmCmdHarness(t, a, func(c *Config) { c.Reactions = true })
	hh.deliverDM(t, humanID, "hello", humanID, botID)
	hh.z.reset()
	hh.deliverDM(t, humanID, "!opts", humanID, botID)
	panel := lastMsg(t, hh)
	body := hh.z.body(panel)

	// The agent re-probes and now reports the models the other way up.
	// Nothing repainted the panel, so its text still reads as before.
	a.models = []client.ModelInfo{{ID: "b/two"}, {ID: "a/one"}}
	if got := hh.z.body(panel); got != body {
		t.Fatal("the panel repainted itself — this test no longer proves anything")
	}

	tap(t, hh, panel, "one")

	if id, ok := hh.h.modelOverride(hh.j.Convs()[0].ID); !ok || id != "a/one" {
		t.Fatalf("model = %q/%v, want the a/one the panel still names beside :one:", id, ok)
	}
}

// TestAPanelSurvivesLosingItsChipTable: the table is in memory, so a
// restart loses it while the journal still remembers the panel's id.
// Recomputing is the fallback — a panel that went dead across a reload
// would be worse, since the chips are still sitting on the message and
// a thumb has no way to know.
func TestAPanelSurvivesLosingItsChipTable(t *testing.T) {
	hh := chipHarness(t)
	hh.deliverDM(t, humanID, "!opts", humanID, botID)
	panel := lastMsg(t, hh)
	hh.h.forgetChips(panel)

	tap(t, hh, panel, "two")

	if id, ok := hh.h.modelOverride(hh.j.Convs()[0].ID); !ok || id != "b/two" {
		t.Fatalf("model = %q/%v, want b/two from the recomputed table", id, ok)
	}
}

// TestARetiredPanelForgetsItsChips: the table must not outlive the
// panel it describes, or the relay holds memory for a message it will
// never honour a tap on.
func TestARetiredPanelForgetsItsChips(t *testing.T) {
	hh := chipHarness(t)
	hh.deliverDM(t, humanID, "!opts", humanID, botID)
	old := lastMsg(t, hh)
	hh.deliverDM(t, humanID, "!opts", humanID, botID)

	hh.h.chipMu.Lock()
	defer hh.h.chipMu.Unlock()
	if _, ok := hh.h.panelChips[old]; ok {
		t.Fatal("the retired panel's chip table is still held")
	}
	if n := len(hh.h.panelChips); n != 1 {
		t.Fatalf("%d chip tables held, want exactly the live panel's", n)
	}
}

// TestABotCannotTapAChip: a chip is a command, and `!new` or `!stop`
// run by a bot that appeared since startup — so it is in no
// BotSenderIDs snapshot — would be a gate bypass in the one place the
// consequences are destructive rather than noisy.
func TestABotCannotTapAChip(t *testing.T) {
	hh := chipHarness(t)
	hh.deliverDM(t, humanID, "!opts", humanID, botID)
	panel := lastMsg(t, hh)
	const laterBot = int64(77)
	hh.z.users[laterBot] = zulipproto.User{UserID: laterBot, FullName: "Late Bot", IsBot: true}

	hh.h.Handle(context.Background(), reactionEvent(laterBot, panel, "two", zulipproto.ReactionAdd))

	if id, ok := hh.h.modelOverride(hh.j.Convs()[0].ID); ok {
		t.Fatalf("a bot tapped a chip and set the model to %q", id)
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
	hh.h.chipMu.Lock()
	defer hh.h.chipMu.Unlock()
	if n := len(hh.h.panelChips); n != 0 {
		t.Fatalf("%d chip table(s) held for a panel nothing will ever retire", n)
	}
}
