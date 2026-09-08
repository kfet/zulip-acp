package handler

import (
	"context"
	"errors"
	"testing"

	"github.com/kfet/zulip-acp/internal/journal"
	"github.com/kfet/zulip-acp/internal/zulipproto"
)

// This file is about ONE production bug: the :wastebasket: gesture used
// to work only on a conversation the CURRENT process had posted in.
// Handler.lastOwn is in-memory, so every restart — and every
// `systemctl --user reload`, which this relay does often — silently
// took the gesture away from every existing topic, while `!archive`
// went on working. The fix is a three-step resolution (memory, then
// the journal's persisted record, then one bounded API read), and each
// step is asserted here, including what must NOT be resolvable.

// restart drops everything this process remembered about which
// messages are its own, exactly as a restart or a reload does. The
// journal on disk is deliberately left untouched: that is the whole
// point of the record.
//
// own is made resolvable server-side, because a reaction on a message
// the current process never posted reaches its conversation through
// GET /messages/{id}.
func (hh *harness) restart(own, sender int64) {
	hh.h.lastOwnMu.Lock()
	hh.h.lastOwn = map[string]int64{}
	hh.h.lastOwnMu.Unlock()
	hh.h.ownMsgs = newMsgIndex(reactionIndexSize)
	hh.z.mu.Lock()
	defer hh.z.mu.Unlock()
	hh.z.messages[own] = zulipproto.Message{ID: own, StreamID: 4, Topic: "t", SenderID: sender}
}

// TestArchiveGestureSurvivesARestartViaTheJournal is the regression:
// tapping :wastebasket: on the relay's last message must arm even
// though this process has posted nothing in the conversation. The
// journal knows, so it must cost no API read.
func TestArchiveGestureSurvivesARestartViaTheJournal(t *testing.T) {
	hh, own := archiveHarness(t, nil)
	conv := archiveConv(t, hh)
	if rec, _ := hh.j.LookupID(conv.ID); rec.LastOwnID != own {
		t.Fatalf("journal did not record the relay's own message: %+v", rec)
	}
	hh.restart(own, botID)
	gets := len(hh.z.gets)

	hh.tap(humanID, own)

	if _, ok := hh.armed(conv.ID); !ok {
		t.Fatalf("the gesture did not arm after a restart; posted %q, logs %v", hh.z.stored(), hh.logs)
	}
	if n := hh.z.narrowCalls(); n != 0 {
		t.Fatalf("the journal answer cost %d /messages read(s)", n)
	}
	// And not through GET /messages/{id} either: that lookup is rate
	// limited, so a reaction flood would otherwise take the gesture
	// away exactly when the journal already knows the answer.
	if len(hh.z.gets) != gets {
		t.Fatalf("resolving the reacted-to message cost %d GET /messages/{id}", len(hh.z.gets)-gets)
	}
}

// TestArchiveGestureFallsBackToTheServer: neither memory nor the
// journal knows — a conversation last answered before the record
// existed — so the relay asks the server for its own newest message in
// the topic, once, and caches the answer.
func TestArchiveGestureFallsBackToTheServer(t *testing.T) {
	hh, own := archiveHarness(t, nil)
	conv := archiveConv(t, hh)
	hh.restart(own, botID)
	if err := hh.j.SetLastOwn(conv.ID, 0); err != nil {
		t.Fatalf("SetLastOwn: %v", err)
	}
	hh.z.setHistory(zulipproto.Message{ID: own, SenderID: botID})

	// The cache is asserted BEFORE any tap: arming posts a warning,
	// which repopulates memory and journal on its own and would make
	// a broken cache look fine.
	if got := hh.h.lastOwnMessage(context.Background(), conv); got != own {
		t.Fatalf("lastOwnMessage = %d, want %d", got, own)
	}
	if got := hh.h.lastOwnMessage(context.Background(), conv); got != own {
		t.Fatalf("second lastOwnMessage = %d, want %d", got, own)
	}
	if n := hh.z.narrowCalls(); n != 1 {
		t.Fatalf("/messages reads = %d, want exactly 1", n)
	}
	if got := hh.h.cachedOwn(conv.ID); got != own {
		t.Fatalf("in-memory cache = %d, want %d", got, own)
	}
	if rec, _ := hh.j.LookupID(conv.ID); rec.LastOwnID != own {
		t.Fatalf("the fallback answer was not persisted: %+v", rec)
	}
	// The narrow is the conversation AND the bot: a lookup that could
	// return somebody else's message would arm a destructive gesture
	// on it.
	got := hh.z.narrow(0)
	if len(got) != 3 || got[0].Operator != "channel" || got[1].Operator != "topic" ||
		got[1].Operand != "t" || got[2].Operator != "sender" || got[2].Operand != botID {
		t.Fatalf("narrow = %+v", got)
	}

	hh.tap(humanID, own)

	if _, ok := hh.armed(conv.ID); !ok {
		t.Fatalf("the gesture did not arm through the server fallback; logs %v", hh.logs)
	}
	if n := hh.z.narrowCalls(); n != 1 {
		t.Fatalf("the fallback answer was not cached: %d reads", n)
	}
}

// TestLastOwnFallbackRefusesWhatIsNotOurs: the fallback is the one
// place a message id arrives from outside, so it is checked rather
// than trusted — and a server that will not answer arms nothing at
// all.
func TestLastOwnFallbackRefusesWhatIsNotOurs(t *testing.T) {
	cases := map[string]func(z *fakeZulip){
		"nothing in the topic":  func(z *fakeZulip) {},
		"someone else's newest": func(z *fakeZulip) { z.setHistory(zulipproto.Message{ID: 4242, SenderID: humanID}) },
		"the read failed":       func(z *fakeZulip) { z.failHistory(errors.New("boom")) },
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			hh, own := archiveHarness(t, nil)
			conv := archiveConv(t, hh)
			hh.restart(own, botID)
			if err := hh.j.SetLastOwn(conv.ID, 0); err != nil {
				t.Fatalf("SetLastOwn: %v", err)
			}
			setup(hh.z)

			if got := hh.h.lastOwnMessage(context.Background(), conv); got != 0 {
				t.Fatalf("lastOwnMessage = %d, want 0", got)
			}
			hh.tap(humanID, own)
			if _, ok := hh.armed(conv.ID); ok {
				t.Fatal("an unresolvable last message armed an archive")
			}
		})
	}
}

// TestRetiredConversationResolvesNoOwnMessage: a conversation that is
// over owns nothing. Neither the journal nor the server may hand one
// back — a retired conversation must not be archivable through a stale
// record.
func TestRetiredConversationResolvesNoOwnMessage(t *testing.T) {
	hh, own := archiveHarness(t, nil)
	conv := archiveConv(t, hh)
	hh.restart(own, botID)
	hh.z.setHistory(zulipproto.Message{ID: own, SenderID: botID})

	prev, _, existed, err := hh.j.Retire(conv.Key)
	if err != nil || !existed {
		t.Fatalf("Retire: %v existed=%v", err, existed)
	}
	if prev.LastOwnID != 0 {
		t.Fatalf("a retired conversation kept its own-message record: %+v", prev)
	}

	if got := hh.h.lastOwnMessage(context.Background(), prev); got != 0 {
		t.Fatalf("retired conversation resolved message %d", got)
	}
	if n := hh.z.narrowCalls(); n != 0 {
		t.Fatalf("a retired conversation cost %d /messages read(s)", n)
	}
	// Nor does the replacement inherit it: the fresh conversation has
	// posted nothing, and the journal must not say otherwise.
	fresh := archiveConv(t, hh)
	if rec, _ := hh.j.LookupID(fresh.ID); rec.LastOwnID != 0 {
		t.Fatalf("the fresh conversation inherited %d", rec.LastOwnID)
	}
	// A conversation the journal has never heard of resolves to
	// nothing too, and spends nothing finding out.
	if got := hh.h.lastOwnMessage(context.Background(), journal.Conv{ID: "nosuchconv"}); got != 0 {
		t.Fatalf("an unknown conversation resolved message %d", got)
	}
	if n := hh.z.narrowCalls(); n != 0 {
		t.Fatalf("an unknown conversation cost %d /messages read(s)", n)
	}
}

// TestRetiredConversationIgnoresAWarmCache: `!new` retires without
// going through endSession, so the in-memory record can outlive the
// conversation. The retired check therefore comes BEFORE the cache —
// a conversation that is over owns no message by any path.
func TestRetiredConversationIgnoresAWarmCache(t *testing.T) {
	hh, own := archiveHarness(t, nil)
	conv := archiveConv(t, hh)
	if got := hh.h.cachedOwn(conv.ID); got != own {
		t.Fatalf("cache = %d, want the relay's own %d", got, own)
	}

	hh.deliver(t, "t", "!new")

	if got := hh.h.cachedOwn(conv.ID); got != 0 {
		t.Fatalf("`!new` left %d in memory for the retired conversation", got)
	}
	// Even with the cache put back by hand — the map is a hint, and
	// the conversation being over is the fact.
	hh.h.rememberOwn(conv.ID, own)
	retired, ok := hh.j.LookupID(conv.ID)
	if !ok || !retired.Retired {
		t.Fatalf("conversation was not retired: %+v", retired)
	}
	if got := hh.h.lastOwnMessage(context.Background(), retired); got != 0 {
		t.Fatalf("retired conversation resolved message %d from a warm cache", got)
	}
}

// TestArchiveClearsThePersistedOwnMessage: ending a conversation must
// clear the record as well as the map. Leaving it on disk would let
// the next lookup hand back the id of a message belonging to a
// conversation that is over.
func TestArchiveClearsThePersistedOwnMessage(t *testing.T) {
	hh, own := archiveHarness(t, nil)
	conv := archiveConv(t, hh)

	hh.tap(humanID, own)
	prompt, ok := hh.armed(conv.ID)
	if !ok {
		t.Fatalf("nothing armed; posted %q", hh.z.stored())
	}
	hh.tap(humanID, prompt)

	rec, ok := hh.j.LookupID(conv.ID)
	if !ok {
		t.Fatal("the archived conversation vanished from the journal")
	}
	if !rec.Retired || rec.LastOwnID != 0 {
		t.Fatalf("archived conversation = %+v", rec)
	}
	if got := hh.h.cachedOwn(conv.ID); got != 0 {
		t.Fatalf("in-memory record survived the archive: %d", got)
	}
}

// TestOwnMessageRecordDegradesWhenTheJournalCannotBeWritten: the
// record is a cache of a fact the topic already holds, so a journal
// that refuses the write is logged and survived, never fatal to the
// turn that was posting.
func TestOwnMessageRecordDegradesWhenTheJournalCannotBeWritten(t *testing.T) {
	hh, _ := archiveHarness(t, nil)

	hh.h.rememberOwn("nosuchconv", 5)
	hh.h.forgetOwn("nosuchconv")

	if !hh.logged("recording last own message for nosuchconv") ||
		!hh.logged("clearing last own message for nosuchconv") {
		t.Fatalf("logs = %v", hh.logs)
	}
}
