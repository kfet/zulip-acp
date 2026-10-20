package handler

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/kfet/zulip-acp/internal/journal"
	"github.com/kfet/zulip-acp/internal/zulipproto"
)

// lastReaction returns the newest reaction the relay added, or "".
func (hh *harness) lastReaction() string {
	added, _ := hh.z.reactions()
	if len(added) == 0 {
		return ""
	}
	return added[len(added)-1]
}

// TestReactToAddsToAMessageInThisTopic: a message in the caller's own
// topic gets the emoji, even when the topic name differs only in case.
func TestReactToAddsToAMessageInThisTopic(t *testing.T) {
	hh, origin := toolHarness(t)
	id := hh.plant("Planning", "pick one", botID, botName)
	if err := hh.h.ReactTo(origin.ID, id, "one"); err != nil {
		t.Fatalf("ReactTo: %v", err)
	}
	if got, want := hh.lastReaction(), fmt.Sprintf("%d:one", id); got != want {
		t.Fatalf("reaction = %q, want %q", got, want)
	}
}

// TestReactToRefusesAnythingOutsideThisConversation: another topic, an
// unreadable id, an unserved channel and a stale conversation are all
// refused, and none of them reaches Zulip.
func TestReactToRefusesAnythingOutsideThisConversation(t *testing.T) {
	hh, origin := toolHarness(t)
	before := hh.lastReaction()
	foreign := hh.plant("elsewhere", "secret", humanID, "Kfet")
	if err := hh.h.ReactTo(origin.ID, foreign, "one"); err == nil || !strings.Contains(err.Error(), "not in this conversation") {
		t.Fatalf("foreign: %v", err)
	}
	hh.z.mu.Lock()
	hh.z.next++
	unserved := hh.z.next
	hh.z.messages[unserved] = zulipproto.Message{ID: unserved, StreamID: 999, Topic: "planning", Type: zulipproto.MessageTypeStream}
	hh.z.mu.Unlock()
	if err := hh.h.ReactTo(origin.ID, unserved, "one"); err == nil {
		t.Fatal("unserved channel: want refusal")
	}
	if err := hh.h.ReactTo(origin.ID, 424242, "one"); err == nil {
		t.Fatal("unknown id: want refusal")
	}
	if err := hh.h.ReactTo("cdeadbeef", foreign, "one"); err == nil {
		t.Fatal("unknown conversation: want refusal")
	}
	if _, _, _, err := hh.j.Retire(journal.Channel(4, "planning")); err != nil {
		t.Fatalf("Retire: %v", err)
	}
	mine := hh.plant("planning", "x", botID, botName)
	if err := hh.h.ReactTo(origin.ID, mine, "one"); err == nil {
		t.Fatal("retired conversation: want refusal")
	}
	if got := hh.lastReaction(); got != before {
		t.Fatalf("a refused call reacted: %q", got)
	}
}

// TestReactToSurfacesTheServersError: a failed reaction is the tool's
// error, so the agent knows the choice was not offered.
func TestReactToSurfacesTheServersError(t *testing.T) {
	hh, origin := toolHarness(t)
	id := hh.plant("planning", "pick", botID, botName)
	hh.z.mu.Lock()
	hh.z.reactErr = errors.New("BAD_EMOJI")
	hh.z.mu.Unlock()
	if err := hh.h.ReactTo(origin.ID, id, "nope"); err == nil || !strings.Contains(err.Error(), "BAD_EMOJI") {
		t.Fatalf("err = %v", err)
	}
}

func TestSameConv(t *testing.T) {
	dm := journal.DM([]int64{humanID, botID})
	for _, c := range []struct {
		a, b journal.Key
		want bool
	}{
		{journal.Channel(4, "Plan"), journal.Channel(4, "✔ plan"), true},
		{journal.Channel(4, "plan"), journal.Channel(5, "plan"), false},
		{dm, journal.DM([]int64{botID, humanID}), true},
		{dm, journal.DM([]int64{humanID}), false},
		{dm, journal.Channel(4, "plan"), false},
		{journal.Channel(4, "plan"), dm, false},
	} {
		if got := sameConv(c.a, c.b); got != c.want {
			t.Errorf("sameConv(%s, %s) = %v", c.a.Label(), c.b.Label(), got)
		}
	}
}
