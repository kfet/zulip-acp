package handler

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kfet/zulip-acp/internal/zulipproto"
)

// These tests cover relay lines that land BELOW the agent's reply — a
// "⤷ branch result" line, a "branched →" notice. They become the
// relay's newest message, and a :wastebasket: on the reply above them
// used to fall through to the agent and do nothing.

// relayLine records a relay message posted after the reply, the way
// the poster does, and returns its id.
func relayLine(hh *harness, convID string, id int64) int64 {
	hh.h.rememberOwn(convID, id)
	return id
}

func TestArchiveTapOnReplyAboveRelayLines(t *testing.T) {
	for _, line := range []string{"⤷ branch result", "branched →"} {
		t.Run(line, func(t *testing.T) {
			hh, reply := archiveHarness(t, nil)
			conv := archiveConv(t, hh)
			l := relayLine(hh, conv.ID, reply+100)
			hh.z.setHistory(
				zulipproto.Message{ID: reply - 1, SenderID: humanID},
				zulipproto.Message{ID: reply, SenderID: botID},
				zulipproto.Message{ID: l, SenderID: botID, Content: line},
			)
			hh.tap(humanID, reply)
			if _, ok := hh.armed(conv.ID); !ok {
				t.Fatalf("tap on the reply did not arm; posted %q", hh.z.stored())
			}
		})
	}
}

func TestArchiveTapOnRelayLineArms(t *testing.T) {
	hh, reply := archiveHarness(t, nil)
	conv := archiveConv(t, hh)
	l := relayLine(hh, conv.ID, reply+100)
	hh.tap(humanID, l)
	if _, ok := hh.armed(conv.ID); !ok {
		t.Fatal("tap on the newest relay line did not arm")
	}
}

// A resolve notice and another bot are not humans: they do not end the
// tail.
func TestArchiveTailIgnoresBots(t *testing.T) {
	hh, reply := archiveHarness(t, nil)
	conv := archiveConv(t, hh)
	hh.z.users[77] = zulipproto.User{UserID: 77, FullName: "Nagios", IsBot: true}
	l := relayLine(hh, conv.ID, reply+100)
	hh.z.setHistory(
		zulipproto.Message{ID: reply, SenderID: botID},
		zulipproto.Message{ID: reply + 1, SenderID: 5, SenderRealm: zulipproto.SystemBotRealm},
		zulipproto.Message{ID: reply + 2, SenderID: 77},
		zulipproto.Message{ID: l, SenderID: botID},
	)
	hh.tap(humanID, reply)
	if _, ok := hh.armed(conv.ID); !ok {
		t.Fatalf("bots ended the tail; logs %v", hh.logs)
	}
}

func TestArchiveOldMessageHintsOnce(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	hh, reply := archiveHarness(t, func(c *Config) { c.Now = func() time.Time { return now } })
	conv := archiveConv(t, hh)
	l := relayLine(hh, conv.ID, reply+100)
	hh.z.setHistory(
		zulipproto.Message{ID: reply, SenderID: botID},
		zulipproto.Message{ID: reply + 1, SenderID: humanID},
		zulipproto.Message{ID: l, SenderID: botID},
	)
	hints := func() int {
		n := 0
		for _, b := range hh.z.stored() {
			if b == archiveHintText {
				n++
			}
		}
		return n
	}
	hh.tap(humanID, reply)
	hh.tap(humanID, reply)
	if _, ok := hh.armed(conv.ID); ok {
		t.Fatal("tap on an old message armed")
	}
	if n := hints(); n != 1 {
		t.Fatalf("got %d hints, want 1: %q", n, hh.z.stored())
	}
	now = now.Add(archiveHintEvery)
	hh.tap(humanID, reply)
	if n := hints(); n != 2 {
		t.Fatalf("got %d hints after the limit, want 2", n)
	}
}

// After a restart nothing is cached; the one bounded read decides.
func TestArchiveTailAfterRestart(t *testing.T) {
	hh, reply := archiveHarness(t, nil)
	conv := archiveConv(t, hh)
	l := reply + 100
	if err := hh.j.SetLastOwn(conv.ID, l); err != nil {
		t.Fatal(err)
	}
	hh.restart(reply, botID)
	hh.z.setHistory(
		zulipproto.Message{ID: reply, SenderID: botID},
		zulipproto.Message{ID: l, SenderID: botID},
	)
	hh.tap(humanID, reply)
	if _, ok := hh.armed(conv.ID); !ok {
		t.Fatalf("tap after restart did not arm; logs %v", hh.logs)
	}
}

// A tap older than the scan window, or a failed read, is not the tail.
func TestArchiveTailMissAndReadFailure(t *testing.T) {
	hh, reply := archiveHarness(t, nil)
	conv := archiveConv(t, hh)
	relayLine(hh, conv.ID, reply+100)
	hh.z.setHistory(zulipproto.Message{ID: reply + 100, SenderID: botID})
	hh.tap(humanID, reply)
	if _, ok := hh.armed(conv.ID); ok {
		t.Fatal("a message outside the window armed")
	}
	hh.z.failHistory(errors.New("boom"))
	hh.h.archiveHinted = map[string]time.Time{}
	hh.tap(humanID, reply)
	if _, ok := hh.armed(conv.ID); ok {
		t.Fatal("a failed read armed")
	}
	if !slices.ContainsFunc(hh.logs, func(s string) bool { return strings.Contains(s, "reading the end of") }) {
		t.Fatalf("failure not logged: %v", hh.logs)
	}
}

// The narrow uses the live topic name of the reacted-to message, which
// can differ from the journal name (a resolve adds "✔ ").
func TestArchiveTailUsesLiveTopic(t *testing.T) {
	hh, reply := archiveHarness(t, nil)
	conv := archiveConv(t, hh)
	l := reply + 100
	if err := hh.j.SetLastOwn(conv.ID, l); err != nil {
		t.Fatal(err)
	}
	hh.restart(reply, botID)
	hh.z.mu.Lock()
	hh.z.messages[reply] = zulipproto.Message{ID: reply, StreamID: 4, Topic: "✔ t", SenderID: botID}
	hh.z.mu.Unlock()
	hh.z.setHistory(zulipproto.Message{ID: reply, SenderID: botID}, zulipproto.Message{ID: l, SenderID: botID})
	hh.tap(humanID, reply)
	if _, ok := hh.armed(conv.ID); !ok {
		t.Fatal("did not arm")
	}
	nw := hh.z.narrow(hh.z.narrowCalls() - 1)
	if nw[1].Operand != "✔ t" {
		t.Fatalf("narrow used %v, want the live topic", nw[1].Operand)
	}
}

func TestArchiveHintForgottenWithArm(t *testing.T) {
	hh, _ := archiveHarness(t, nil)
	conv := archiveConv(t, hh)
	hh.h.archiveHinted[conv.ID] = time.Now()
	hh.h.dropArchiveArm(conv.ID)
	if _, ok := hh.h.archiveHinted[conv.ID]; ok {
		t.Fatal("hint record outlived the conversation")
	}
}
