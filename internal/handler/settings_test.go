package handler

import (
	"errors"
	"strings"
	"testing"

	acp "github.com/coder/acp-go-sdk"
	"github.com/kfet/acp-kit/client"
)

// thinkHarness is a DM conversation with one finished turn, whose live
// session reports thinking level "medium".
func thinkHarness(t *testing.T) (*harness, string, acp.SessionId) {
	t.Helper()
	hh := optsHarness(t)
	waitIdle(t, hh)
	var conv string
	var sid acp.SessionId
	hh.s.mu.Lock()
	for k, s := range hh.s.sessions {
		conv, sid = k, s.SessionID
	}
	hh.s.mu.Unlock()
	hh.a.mu.Lock()
	hh.a.stats = map[acp.SessionId]client.SessionStats{sid: {Thinking: "medium"}}
	hh.a.mu.Unlock()
	return hh, conv, sid
}

func TestThinkShowsTheReportedLevel(t *testing.T) {
	hh, _, _ := thinkHarness(t)
	hh.deliverDM(t, humanID, "!think", humanID, botID)
	if got := hh.only(t); !strings.Contains(got, "Thinking: `medium`") || !strings.Contains(got, "`high`") {
		t.Fatalf("reply = %q", got)
	}
}

func TestThinkWithNoConversationShowsTheDefault(t *testing.T) {
	hh := dmCmdHarness(t, newAgent("x"), nil)
	hh.deliverDM(t, humanID, "!think", humanID, botID)
	if got := hh.only(t); !strings.Contains(got, "agent default") {
		t.Fatalf("reply = %q", got)
	}
	hh.z.reset()
	hh.deliverDM(t, humanID, "!think high", humanID, botID)
	if got := hh.only(t); !strings.Contains(got, "no conversation here yet") {
		t.Fatalf("reply = %q", got)
	}
}

func TestThinkRejectsAnUnknownLevel(t *testing.T) {
	hh, _, _ := thinkHarness(t)
	hh.deliverDM(t, humanID, "!think huge", humanID, botID)
	if got := hh.only(t); !strings.Contains(got, "Unknown thinking level `huge`") {
		t.Fatalf("reply = %q", got)
	}
}

func TestThinkRefusesWhenTheAgentReportsNoLevel(t *testing.T) {
	hh, _, _ := thinkHarness(t)
	hh.a.mu.Lock()
	hh.a.stats = nil
	hh.a.mu.Unlock()
	hh.deliverDM(t, humanID, "!think high", humanID, botID)
	if got := hh.only(t); !strings.Contains(got, "does not report a thinking level") {
		t.Fatalf("reply = %q", got)
	}
}

// TestThinkIsStickyAndAppliedOncePerSession: the choice is pushed at
// the start of the next turn, once, and again after `!new`.
func TestThinkIsStickyAndAppliedOncePerSession(t *testing.T) {
	hh, conv, sid := thinkHarness(t)
	hh.deliverDM(t, humanID, "!THINK High", humanID, botID)
	if n := hh.z.count(); n != 0 {
		t.Fatalf("a change posted %d messages; want only a reaction", n)
	}
	if added, _ := hh.z.reactions(); len(added) == 0 || !strings.HasSuffix(added[len(added)-1], "check") {
		t.Fatalf("reactions = %q", added)
	}
	hh.deliverDM(t, humanID, "!think", humanID, botID)
	if got := hh.only(t); !strings.Contains(got, "`high`") {
		t.Fatalf("reply = %q", got)
	}
	hh.deliverDM(t, humanID, "go", humanID, botID)
	waitIdle(t, hh)
	hh.deliverDM(t, humanID, "again", humanID, botID)
	waitIdle(t, hh)
	want := string(sid) + ":thinking_level=high"
	if got := hh.a.configs(); len(got) != 1 || got[0] != want {
		t.Fatalf("config calls = %q, want [%q]", got, want)
	}
	_ = conv
	// `!new` carries the choice to the fresh conversation.
	hh.deliverDM(t, humanID, "!new", humanID, botID)
	hh.deliverDM(t, humanID, "fresh", humanID, botID)
	waitIdle(t, hh)
	if got := hh.a.configs(); len(got) != 2 || !strings.HasSuffix(got[1], ":thinking_level=high") {
		t.Fatalf("config calls after !new = %q", got)
	}
}

func TestThinkApplyFailureIsLoggedAndRetried(t *testing.T) {
	hh, _, _ := thinkHarness(t)
	hh.a.mu.Lock()
	hh.a.configErr = errors.New("boom")
	hh.a.mu.Unlock()
	hh.deliverDM(t, humanID, "!think low", humanID, botID)
	hh.deliverDM(t, humanID, "go", humanID, botID)
	waitIdle(t, hh)
	if !hh.logged("thinking_level=low") {
		t.Fatal("failure was not logged")
	}
	hh.a.mu.Lock()
	hh.a.configErr = nil
	hh.a.mu.Unlock()
	hh.deliverDM(t, humanID, "go", humanID, botID)
	waitIdle(t, hh)
	if got := hh.a.configs(); len(got) != 1 {
		t.Fatalf("config calls = %q", got)
	}
}

// TestPanelOffersThinkingOnlyWhenReported: no knob the agent has not
// reported, and every button is a typable command.
func TestPanelOffersThinkingOnlyWhenReported(t *testing.T) {
	hh := optsHarness(t)
	hh.deliverDM(t, humanID, "!opts", humanID, botID)
	if strings.Contains(panelBody(t, hh), "**Thinking**") {
		t.Fatal("panel offers thinking the agent never reported")
	}
	hh, _, _ = thinkHarness(t)
	hh.deliverDM(t, humanID, "!opts", humanID, botID)
	panel := panelBody(t, hh)
	if !strings.Contains(panel, "**Thinking**") || !strings.Contains(panel, "`!think medium` ←") {
		t.Fatalf("panel = %q", panel)
	}
	if w := hh.z.widget(panelMsg(t, hh)); !strings.Contains(w, `"reply":"!think high"`) {
		t.Fatalf("widget = %q", w)
	}
}

func TestParseSettingNeedsASigil(t *testing.T) {
	if _, _, ok := parseSetting("think high"); ok {
		t.Fatal("prose parsed as a command")
	}
}

func TestSettingValueWithNoLiveSession(t *testing.T) {
	hh := optsHarness(t)
	if v := hh.h.settingValue(0, "no-such-conv", true); v != "" {
		t.Fatalf("value = %q", v)
	}
}
