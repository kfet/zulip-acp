package handler

import (
	"context"
	"encoding/json"
	"fmt"
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
			want: "Available commands:\n\n- `!help` — show this\n" + optsHelpLine + "- `!status` — state\n\nAgent commands:\n\n- `!reload`\n",
		},
		{
			name: "no !help bullet to anchor to",
			out:  "Commands:\n\n- `!status`\n",
			want: "Commands:\n\n- `!status`\n" + optsHelpLine,
		},
		{
			name: "unterminated !help bullet",
			out:  "- `!help` — show this",
			want: "- `!help` — show this\n" + optsHelpLine,
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
