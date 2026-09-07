package handler

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kfet/zulip-acp/internal/channels"
	"github.com/kfet/zulip-acp/internal/journal"
	"github.com/kfet/zulip-acp/internal/zulipproto"
)

// archiveStream is the destination channel in these tests. It is
// deliberately NOT in the harness's served set ({4: "fleet"}) — that is
// the property the whole feature rests on.
const archiveStream = int64(9)

// archiveHarness returns a harness with the archive control armed and
// one engaged conversation in #fleet > "t", plus the id of the relay's
// own last message in it.
//
// The reaction trigger is wired the way main does — to
// Handler.ArchiveReaction — after construction, because the closure
// needs the Handler that New returns.
func archiveHarness(t *testing.T, tune func(*Config)) (*harness, int64) {
	t.Helper()
	expired := make(chan string, 8)
	hh := cmdHarness(t, newAgent("hello"), func(c *Config) {
		c.Reactions = true
		c.ArchiveStreamID = archiveStream
		c.ArchiveChannel = "archive"
		c.OnArchiveExpired = func(id string) { expired <- id }
		if tune != nil {
			tune(c)
		}
	})
	hh.expired = expired
	// Wired the way main wires it: the closure needs the Handler that
	// New returns, and nothing can deliver an event until the first
	// Handle below.
	hh.h.cfg.ReactionTrigger = hh.h.ArchiveReaction
	hh.deliver(t, "t", mention("hi"))
	return hh, hh.z.lastID()
}

// archiveConv resolves the conversation currently answering in
// #fleet > "t".
func archiveConv(t *testing.T, hh *harness) journal.Conv {
	t.Helper()
	c, ok := hh.j.Lookup(journal.Channel(4, "t"))
	if !ok {
		t.Fatal("no conversation in #fleet > t")
	}
	return c
}

// tap feeds one :wastebasket: reaction and returns once Handle has
// decided — the archive path is fully synchronous on the event loop.
func (hh *harness) tap(user, msgID int64) {
	hh.h.Handle(context.Background(), reactionEvent(user, msgID, archiveEmoji, zulipproto.ReactionAdd))
}

// armed reports whether a conversation currently has an armed
// confirmation, and the message that would confirm it.
func (hh *harness) armed(convID string) (int64, bool) {
	hh.h.archiveMu.Lock()
	defer hh.h.archiveMu.Unlock()
	p, ok := hh.h.archivePending[convID]
	if !ok {
		return 0, false
	}
	return p.promptID, true
}

// --- the happy path, from both entry points ------------------------------

// TestArchiveArmThenConfirm drives the whole gesture from each entry
// point: something arms a confirmation, the relay posts a warning, and
// only a :wastebasket: on THAT warning archives the topic.
func TestArchiveArmThenConfirm(t *testing.T) {
	arm := map[string]func(t *testing.T, hh *harness, own int64){
		"reaction on the relay's last message": func(_ *testing.T, hh *harness, own int64) {
			hh.tap(humanID, own)
		},
		"!archive": func(t *testing.T, hh *harness, _ int64) {
			hh.deliver(t, "t", "!archive")
		},
		"!arch": func(t *testing.T, hh *harness, _ int64) {
			hh.deliver(t, "t", "!arch")
		},
	}
	for name, doArm := range arm {
		t.Run(name, func(t *testing.T) {
			hh, own := archiveHarness(t, nil)
			conv := archiveConv(t, hh)
			stateDir := filepath.Join(hh.s.StateDir(), convsDir, conv.ID)
			if err := os.MkdirAll(stateDir, 0o755); err != nil {
				t.Fatalf("state dir: %v", err)
			}

			doArm(t, hh, own)

			prompt, ok := hh.armed(conv.ID)
			if !ok {
				t.Fatalf("nothing armed; posted %q", hh.z.stored())
			}
			if body := hh.z.body(prompt); !strings.Contains(body, "Archive this topic?") || !strings.Contains(body, "#archive") {
				t.Fatalf("warning message = %q", body)
			}
			if len(hh.z.moved()) != 0 {
				t.Fatalf("arming moved something: %v", hh.z.moved())
			}

			// The confirmation.
			hh.tap(humanID, prompt)

			if _, still := hh.armed(conv.ID); still {
				t.Fatal("the arm survived its confirmation")
			}
			moves := hh.z.moved()
			if len(moves) != 1 || !strings.Contains(moves[0], fmt.Sprintf("->%d", archiveStream)) ||
				!strings.HasSuffix(moves[0], propagateAll) {
				t.Fatalf("moves = %v", moves)
			}
			// The audit trail is posted before the move and therefore
			// travels with the topic.
			anchor := hh.z.lastID()
			if body := hh.z.body(anchor); !strings.Contains(body, "Archived by") {
				t.Fatalf("closing message = %q", body)
			}
			if !strings.HasPrefix(moves[0], fmt.Sprintf("%d:", anchor)) {
				t.Fatalf("the move is not anchored on the closing message: %v", moves)
			}
			// The conversation is retired: the key answers to a NEW,
			// empty conv-id, and the old state directory is untouched.
			fresh := archiveConv(t, hh)
			if fresh.ID == conv.ID {
				t.Fatal("the conversation was not retired")
			}
			if _, err := os.Stat(stateDir); err != nil {
				t.Fatalf("the retired conversation's state dir must survive: %v", err)
			}
			old, ok := hh.j.LookupID(conv.ID)
			if !ok || !old.Retired {
				t.Fatalf("old conversation = %+v (ok=%v)", old, ok)
			}
		})
	}
}

// TestArchiveRefusals is every way a :wastebasket: must NOT archive
// anything. Each case leaves the relay exactly as it found it: nothing
// armed, nothing moved.
func TestArchiveRefusals(t *testing.T) {
	cases := []struct {
		name string
		// prepare returns the event to feed.
		event func(hh *harness, own int64) zulipproto.Event
		tune  func(*Config)
		// delivered is true when the reaction should still reach the
		// agent as ordinary signal.
		delivered bool
	}{
		{
			name: "another emoji",
			event: func(_ *harness, own int64) zulipproto.Event {
				return reactionEvent(humanID, own, "tada", zulipproto.ReactionAdd)
			},
			delivered: true,
		},
		{
			name: "un-reacting is not an action",
			event: func(_ *harness, own int64) zulipproto.Event {
				return reactionEvent(humanID, own, archiveEmoji, zulipproto.ReactionRemove)
			},
			delivered: true,
		},
		{
			name: "not the relay's LAST message",
			event: func(hh *harness, own int64) zulipproto.Event {
				hh.z.mu.Lock()
				hh.z.messages[321] = zulipproto.Message{
					ID: 321, SenderID: botID, SenderName: botName, StreamID: 4, Topic: "t",
					Type: zulipproto.MessageTypeStream, Content: "an older answer of mine",
				}
				hh.z.mu.Unlock()
				_ = own
				return reactionEvent(humanID, 321, archiveEmoji, zulipproto.ReactionAdd)
			},
			delivered: true,
		},
		{
			name: "a user who is not on the allowlist",
			event: func(_ *harness, own int64) zulipproto.Event {
				return reactionEvent(4242, own, archiveEmoji, zulipproto.ReactionAdd)
			},
			tune: func(c *Config) { c.AllowedUsers = map[int64]struct{}{humanID: {}} },
		},
		{
			name: "a bot that appeared since startup",
			event: func(hh *harness, own int64) zulipproto.Event {
				hh.z.mu.Lock()
				hh.z.users[77] = zulipproto.User{UserID: 77, FullName: "Late Bot", IsBot: true}
				hh.z.mu.Unlock()
				return reactionEvent(77, own, archiveEmoji, zulipproto.ReactionAdd)
			},
		},
		{
			name: "the control is disabled",
			event: func(_ *harness, own int64) zulipproto.Event {
				return reactionEvent(humanID, own, archiveEmoji, zulipproto.ReactionAdd)
			},
			tune:      func(c *Config) { c.ArchiveStreamID = 0 },
			delivered: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hh, own := archiveHarness(t, tc.tune)
			conv := archiveConv(t, hh)
			ev := tc.event(hh, own)
			if tc.delivered {
				hh.react(t, ev)
				if got := hh.lastPrompt(); !strings.Contains(got, "[reaction]") {
					t.Fatalf("the reaction should have reached the agent: %q", got)
				}
			} else {
				hh.reactDropped(t, ev)
			}
			if _, ok := hh.armed(conv.ID); ok {
				t.Fatal("something was armed")
			}
			if len(hh.z.moved()) != 0 {
				t.Fatalf("something was moved: %v", hh.z.moved())
			}
			if fresh := archiveConv(t, hh); fresh.ID != conv.ID {
				t.Fatal("the conversation was retired")
			}
		})
	}
}

// TestArchiveConfirmationIsMessageBound proves the confirmation is tied
// to the warning MESSAGE, not merely to the conversation: a
// :wastebasket: on the relay's newer message re-arms rather than
// confirming, and the wrong emoji on the warning confirms nothing.
func TestArchiveConfirmationIsMessageBound(t *testing.T) {
	hh, own := archiveHarness(t, nil)
	conv := archiveConv(t, hh)
	hh.tap(humanID, own)
	prompt, ok := hh.armed(conv.ID)
	if !ok {
		t.Fatal("nothing armed")
	}

	// Wrong emoji on the right message: ordinary signal.
	hh.react(t, reactionEvent(humanID, prompt, "tada", zulipproto.ReactionAdd))
	if len(hh.z.moved()) != 0 {
		t.Fatalf(":tada: archived something: %v", hh.z.moved())
	}
	if got, _ := hh.armed(conv.ID); got != prompt {
		t.Fatalf("the arm changed: %d", got)
	}

	// The right emoji on the WRONG message — an older message of ours,
	// not the warning — is not a confirmation. It is not the last one
	// either, so it does nothing at all.
	hh.z.mu.Lock()
	hh.z.messages[555] = zulipproto.Message{
		ID: 555, SenderID: botID, SenderName: botName, StreamID: 4, Topic: "t",
		Type: zulipproto.MessageTypeStream, Content: "older",
	}
	hh.z.mu.Unlock()
	hh.react(t, reactionEvent(humanID, 555, archiveEmoji, zulipproto.ReactionAdd))
	if len(hh.z.moved()) != 0 {
		t.Fatalf("a reaction on an old message archived something: %v", hh.z.moved())
	}
	if got, _ := hh.armed(conv.ID); got != prompt {
		t.Fatalf("the arm changed: %d", got)
	}
	if fresh := archiveConv(t, hh); fresh.ID != conv.ID {
		t.Fatal("the conversation was retired")
	}
}

// TestArchiveExpiryStartsFreshCycle is the rule that an expired arming
// is never carried forward: after it lapses, the next tap produces a
// NEW warning to answer rather than archiving on the strength of a
// decision the user made minutes ago.
func TestArchiveExpiryStartsFreshCycle(t *testing.T) {
	hh, own := archiveHarness(t, nil)
	conv := archiveConv(t, hh)
	hh.tap(humanID, own)
	first, ok := hh.armed(conv.ID)
	if !ok {
		t.Fatal("nothing armed")
	}
	// Fire the injected expiry timer and wait for the arm to be
	// forgotten — no sleeping, no polling.
	hh.atimer <- time.Now()
	select {
	case id := <-hh.expired:
		if id != conv.ID {
			t.Fatalf("expired %q, want %q", id, conv.ID)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the arm never expired")
	}
	if _, still := hh.armed(conv.ID); still {
		t.Fatal("an expired arm is still armed")
	}
	if !hh.logged("expired unconfirmed") {
		t.Fatalf("the expiry must be logged: %v", hh.logs)
	}

	// The late tap lands on the old warning, which is still the last
	// message the relay posted. It must arm afresh, not archive.
	hh.tap(humanID, first)
	if len(hh.z.moved()) != 0 {
		t.Fatalf("a late tap archived: %v", hh.z.moved())
	}
	second, ok := hh.armed(conv.ID)
	if !ok || second == first {
		t.Fatalf("no fresh cycle: armed=%v id=%d first=%d", ok, second, first)
	}
	if body := hh.z.body(second); !strings.Contains(body, "Archive this topic?") {
		t.Fatalf("no fresh warning was posted: %q", body)
	}
}

// TestArchiveLapsedArmIsRefusedBeforeItsTimer covers the same rule in
// the window where the deadline has passed but the timer goroutine has
// not run yet: takeArchiveConfirm must refuse on the clock alone.
func TestArchiveLapsedArmIsRefusedBeforeItsTimer(t *testing.T) {
	now := time.Now()
	hh, own := archiveHarness(t, func(c *Config) {
		c.Now = func() time.Time { return now }
	})
	conv := archiveConv(t, hh)
	hh.tap(humanID, own)
	prompt, ok := hh.armed(conv.ID)
	if !ok {
		t.Fatal("nothing armed")
	}
	now = now.Add(archiveConfirmTTL + time.Second)
	hh.tap(humanID, prompt)
	if len(hh.z.moved()) != 0 {
		t.Fatalf("a lapsed arm was honoured: %v", hh.z.moved())
	}
	if !hh.logged("late archive confirmation") {
		t.Fatalf("the refusal must be logged: %v", hh.logs)
	}
	if _, rearmed := hh.armed(conv.ID); !rearmed {
		t.Fatal("the tap should have started a fresh cycle")
	}
}

// TestArchiveOrdering is the load-bearing part: the conversation must
// be retired BEFORE the topic moves, or handleUpdate would migrate the
// session into the archive channel instead of ending it.
func TestArchiveOrdering(t *testing.T) {
	hh, own := archiveHarness(t, nil)
	conv := archiveConv(t, hh)
	var atMove struct {
		retired  bool
		inflight bool
		posted   int
	}
	hh.z.mu.Lock()
	hh.z.moveHook = func() {
		c, ok := hh.j.LookupID(conv.ID)
		atMove.retired = ok && c.Retired
		atMove.inflight = hh.h.isInflight(conv.ID)
		atMove.posted = len(hh.z.order)
	}
	hh.z.mu.Unlock()

	hh.tap(humanID, own)
	prompt, _ := hh.armed(conv.ID)
	hh.tap(humanID, prompt)

	if !atMove.retired {
		t.Fatal("the topic moved before the conversation was retired")
	}
	if atMove.inflight {
		t.Fatal("a turn was still in flight when the topic moved")
	}
	if got := hh.z.body(hh.z.lastID()); !strings.Contains(got, "Archived by") {
		t.Fatalf("the closing message must be posted before the move: %q", got)
	}
	// The relay's own last-message index must not keep pointing a new
	// gesture at a conversation that is over.
	if _, ok := hh.armed(conv.ID); ok {
		t.Fatal("an arm survived the archive")
	}
}

// TestArchiveFailures covers what the relay says when Zulip or the
// journal refuses. In every case it must state plainly what did and did
// not happen — an archive that half-worked and said nothing would be
// the worst outcome of all.
func TestArchiveFailures(t *testing.T) {
	t.Run("the warning cannot be posted", func(t *testing.T) {
		hh, own := archiveHarness(t, nil)
		conv := archiveConv(t, hh)
		hh.z.mu.Lock()
		hh.z.sendErr = errors.New("boom")
		hh.z.mu.Unlock()
		hh.tap(humanID, own)
		if _, ok := hh.armed(conv.ID); ok {
			t.Fatal("armed a confirmation nobody can see")
		}
		if !hh.logged("could not post the archive confirmation") {
			t.Fatalf("logs = %v", hh.logs)
		}
	})

	t.Run("the closing message cannot be posted", func(t *testing.T) {
		hh, own := archiveHarness(t, nil)
		conv := archiveConv(t, hh)
		hh.tap(humanID, own)
		prompt, _ := hh.armed(conv.ID)
		hh.z.mu.Lock()
		hh.z.sendErr = errors.New("boom")
		hh.z.mu.Unlock()
		hh.tap(humanID, prompt)
		if len(hh.z.moved()) != 0 {
			t.Fatalf("moved without an audit trail: %v", hh.z.moved())
		}
		if fresh := archiveConv(t, hh); fresh.ID != conv.ID {
			t.Fatal("the conversation was retired anyway")
		}
		if !hh.logged("could not post the closing message") {
			t.Fatalf("logs = %v", hh.logs)
		}
	})

	t.Run("the journal cannot record the retirement", func(t *testing.T) {
		hh, own := archiveHarness(t, nil)
		hh.tap(humanID, own)
		conv := archiveConv(t, hh)
		prompt, _ := hh.armed(conv.ID)
		hh.breakJournal(t)
		hh.tap(humanID, prompt)
		if len(hh.z.moved()) != 0 {
			t.Fatalf("moved after a failed retirement: %v", hh.z.moved())
		}
		if !hh.logged("retiring it failed") {
			t.Fatalf("logs = %v", hh.logs)
		}
		if got := hh.z.lastBody(); !strings.Contains(got, "did not move the topic") {
			t.Fatalf("the topic must be told: %q", got)
		}
	})

	t.Run("the move is refused", func(t *testing.T) {
		hh, own := archiveHarness(t, nil)
		conv := archiveConv(t, hh)
		hh.tap(humanID, own)
		prompt, _ := hh.armed(conv.ID)
		hh.z.mu.Lock()
		hh.z.moveErr = errors.New("not allowed")
		hh.z.mu.Unlock()
		hh.tap(humanID, prompt)
		if fresh := archiveConv(t, hh); fresh.ID == conv.ID {
			t.Fatal("the conversation should still have ended")
		}
		if got := hh.z.lastBody(); !strings.Contains(got, "moving the topic to #archive failed") {
			t.Fatalf("the topic must be told: %q", got)
		}
		if !hh.logged("the topic stayed put") {
			t.Fatalf("logs = %v", hh.logs)
		}
	})

	t.Run("the conversation vanished under the confirmation", func(t *testing.T) {
		hh, own := archiveHarness(t, nil)
		conv := archiveConv(t, hh)
		hh.tap(humanID, own)
		prompt, _ := hh.armed(conv.ID)
		_ = prompt
		// The confirmation names a conversation whose topic is no
		// longer engaged — `!new`, a rename, or a race with another
		// archive. Retire is the single source of truth about that.
		hh.h.archiveConversation(context.Background(), journal.Conv{ID: conv.ID, Key: journal.Channel(4, "gone")}, "Ada")
		if len(hh.z.moved()) != 0 {
			t.Fatalf("moved a topic that is no longer the conversation: %v", hh.z.moved())
		}
		if !hh.logged("no longer the conversation") {
			t.Fatalf("logs = %v", hh.logs)
		}
	})

	t.Run("the archive channel became one we serve", func(t *testing.T) {
		hh, own := archiveHarness(t, func(c *Config) {
			c.Channels = channels.New(channels.Config{Explicit: map[int64]string{4: "fleet", archiveStream: "archive"}})
		})
		conv := archiveConv(t, hh)
		hh.tap(humanID, own)
		prompt, _ := hh.armed(conv.ID)
		hh.tap(humanID, prompt)
		if len(hh.z.moved()) != 0 {
			t.Fatalf("moved into a served channel: %v", hh.z.moved())
		}
		if !hh.logged("is now in the served set") {
			t.Fatalf("logs = %v", hh.logs)
		}
		if got := hh.z.lastBody(); !strings.Contains(got, "Nothing was changed") {
			t.Fatalf("the topic must be told: %q", got)
		}
	})
}

// --- the typed entry point -----------------------------------------------

// TestArchiveCommandSurface pins what the typed forms do, including the
// ones that must NOT exist: `!purge` names a destruction that does not
// happen, so it is not a command at all.
func TestArchiveCommandSurface(t *testing.T) {
	cases := []struct {
		text string
		arms bool
		// reply, when set, must appear in what the relay posts.
		reply string
	}{
		{text: "!archive", arms: true},
		{text: "!arch", arms: true},
		{text: "!ARCHIVE", arms: true},
		{text: "!purge", reply: "Unknown command"},
		{text: "!archive the design notes", reply: "Unknown command"},
	}
	for _, tc := range cases {
		t.Run(tc.text, func(t *testing.T) {
			hh, _ := archiveHarness(t, nil)
			conv := archiveConv(t, hh)
			hh.deliver(t, "t", tc.text)
			_, armed := hh.armed(conv.ID)
			if armed != tc.arms {
				t.Fatalf("armed=%v, want %v (posted %q)", armed, tc.arms, hh.z.stored())
			}
			if tc.reply != "" && !strings.Contains(hh.z.lastBody(), tc.reply) {
				t.Fatalf("reply = %q, want %q in it", hh.z.lastBody(), tc.reply)
			}
		})
	}
}

// TestArchiveCommandRefusals covers the typed form where there is
// nothing to archive. Each answers in the topic rather than silently
// doing nothing.
func TestArchiveCommandRefusals(t *testing.T) {
	t.Run("archiving is not configured", func(t *testing.T) {
		hh, _ := archiveHarness(t, func(c *Config) { c.ArchiveStreamID = 0 })
		hh.deliver(t, "t", "!archive")
		if got := hh.z.lastBody(); !strings.Contains(got, "not configured") {
			t.Fatalf("reply = %q", got)
		}
	})
	t.Run("a direct message has no topic", func(t *testing.T) {
		hh, _ := archiveHarness(t, func(c *Config) { c.DMs = true })
		hh.deliverDM(t, humanID, "hello", humanID, botID)
		hh.deliverDM(t, humanID, "!archive", humanID, botID)
		if got := hh.z.lastBody(); !strings.Contains(got, "no topic to move") {
			t.Fatalf("reply = %q", got)
		}
		if len(hh.z.moved()) != 0 {
			t.Fatalf("a DM was moved: %v", hh.z.moved())
		}
	})
	t.Run("a wastebasket in a DM is ordinary signal", func(t *testing.T) {
		hh, _ := archiveHarness(t, func(c *Config) { c.DMs = true })
		hh.deliverDM(t, humanID, "hello", humanID, botID)
		hh.react(t, reactionEvent(humanID, hh.z.lastID(), archiveEmoji, zulipproto.ReactionAdd))
		if got := hh.lastPrompt(); !strings.Contains(got, ":wastebasket:") {
			t.Fatalf("prompt = %q", got)
		}
		if len(hh.z.moved()) != 0 {
			t.Fatalf("a DM was moved: %v", hh.z.moved())
		}
	})
	t.Run("there is no conversation here yet", func(t *testing.T) {
		hh, _ := archiveHarness(t, nil)
		hh.deliver(t, "empty", mention("!archive"))
		if got := hh.z.lastBody(); !strings.Contains(got, "no conversation here to archive") {
			t.Fatalf("reply = %q", got)
		}
	})
}

// TestArchiveConfirmRaceGuard covers takeArchiveConfirm's own guard:
// armedFor and the consume are two steps, so a second tap arriving in
// between must find nothing rather than archive twice.
func TestArchiveConfirmRaceGuard(t *testing.T) {
	hh, _ := archiveHarness(t, nil)
	conv := archiveConv(t, hh)
	if hh.h.takeArchiveConfirm(conv.ID, 1234) {
		t.Fatal("confirmed with nothing armed")
	}
	hh.deliver(t, "t", "!archive")
	prompt, _ := hh.armed(conv.ID)
	if hh.h.takeArchiveConfirm(conv.ID, prompt+1) {
		t.Fatal("confirmed on the wrong message")
	}
	if _, still := hh.armed(conv.ID); !still {
		t.Fatal("a mismatched confirmation consumed the arm")
	}
}

// TestArchiveHelpLine: the command is advertised in `!help` only where
// it can actually run.
func TestArchiveHelpLine(t *testing.T) {
	on, _ := archiveHarness(t, nil)
	on.deliver(t, "t", "!help")
	if !strings.Contains(on.z.lastBody(), "`!archive`") {
		t.Fatalf("help = %q", on.z.lastBody())
	}
	off, _ := archiveHarness(t, func(c *Config) { c.ArchiveStreamID = 0 })
	off.deliver(t, "t", "!help")
	if strings.Contains(off.z.lastBody(), "`!archive`") {
		t.Fatalf("a disabled command must not be advertised: %q", off.z.lastBody())
	}
}

// TestArchiveAttribution: the typed form names the sender without an
// API call, and falls back to something vague rather than to a user id.
func TestArchiveAttribution(t *testing.T) {
	if got := senderName(nil); got != "someone" {
		t.Fatalf("senderName(nil) = %q", got)
	}
	if got := senderName(&zulipproto.Message{SenderName: "  "}); got != "someone" {
		t.Fatalf("senderName(blank) = %q", got)
	}
	hh, _ := archiveHarness(t, nil)
	conv := archiveConv(t, hh)
	hh.deliver(t, "t", "!archive")
	prompt, _ := hh.armed(conv.ID)
	if got := hh.z.body(prompt); !strings.Contains(got, "Kfet") {
		t.Fatalf("warning = %q", got)
	}
}

// --- handleUpdate: a topic that leaves the served set --------------------

// TestTopicMovedOutOfServedSet is the latent bug this feature made
// impossible to ignore: a cross-channel move must END the conversation,
// not let its session follow the topic out of the allowlist.
func TestTopicMovedOutOfServedSet(t *testing.T) {
	cases := []struct {
		name    string
		ev      zulipproto.Event
		retired bool
		topic   string
	}{
		{
			name:    "moved to an unserved channel",
			ev:      zulipproto.Event{Type: zulipproto.EventUpdateMessage, StreamID: 4, NewStreamID: archiveStream, OrigTopic: "t", Topic: "t"},
			retired: true,
			topic:   "t",
		},
		{
			name:    "moved to an unserved channel with no topic in the event",
			ev:      zulipproto.Event{Type: zulipproto.EventUpdateMessage, StreamID: 4, NewStreamID: archiveStream, OrigTopic: "t"},
			retired: true,
			topic:   "t",
		},
		{
			name:    "moved to an unserved channel AND renamed",
			ev:      zulipproto.Event{Type: zulipproto.EventUpdateMessage, StreamID: 4, NewStreamID: archiveStream, OrigTopic: "t", Topic: "elsewhere"},
			retired: true,
			topic:   "t",
		},
		{
			name:  "moved to another SERVED channel: the session follows",
			ev:    zulipproto.Event{Type: zulipproto.EventUpdateMessage, StreamID: 4, NewStreamID: 5, OrigTopic: "t", Topic: "t"},
			topic: "t",
		},
		{
			name:  "renamed within the channel: the session follows",
			ev:    zulipproto.Event{Type: zulipproto.EventUpdateMessage, StreamID: 4, OrigTopic: "t", Topic: "renamed"},
			topic: "renamed",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hh, _ := archiveHarness(t, func(c *Config) {
				c.Channels = channels.New(channels.Config{Explicit: map[int64]string{4: "fleet", 5: "other"}})
			})
			conv := archiveConv(t, hh)
			hh.h.Handle(context.Background(), tc.ev)
			old, _ := hh.j.LookupID(conv.ID)
			if old.Retired != tc.retired {
				t.Fatalf("retired = %v, want %v", old.Retired, tc.retired)
			}
			dest := tc.ev.StreamID
			if !tc.retired && tc.ev.NewStreamID != 0 {
				dest = tc.ev.NewStreamID
			}
			c, ok := hh.j.Lookup(journal.Channel(dest, tc.topic))
			if !ok {
				t.Fatalf("nothing answers in channel %d > %q: %+v", dest, tc.topic, hh.j.Convs())
			}
			if tc.retired == (c.ID == conv.ID) {
				t.Fatalf("conv %s answers where it should not (retired=%v)", c.ID, tc.retired)
			}
		})
	}
}

// TestTopicMoveEdgeCases: events that name no conversation change
// nothing, and a failure to record the retirement is reported.
func TestTopicMoveEdgeCases(t *testing.T) {
	hh, _ := archiveHarness(t, nil)
	conv := archiveConv(t, hh)
	for _, ev := range []zulipproto.Event{
		// A content edit: no topic pair, no move.
		{Type: zulipproto.EventUpdateMessage, StreamID: 4, MessageID: 1},
		// A DM edit carries no stream id.
		{Type: zulipproto.EventUpdateMessage, OrigTopic: "t", Topic: "x"},
		// A move within a channel the relay does not serve.
		{Type: zulipproto.EventUpdateMessage, StreamID: 77, OrigTopic: "t", Topic: "x"},
		// A move out of a topic nothing is engaged in.
		{Type: zulipproto.EventUpdateMessage, StreamID: 4, NewStreamID: archiveStream, OrigTopic: "nobody home"},
		// A "move" with no topic at all on either side.
		{Type: zulipproto.EventUpdateMessage, StreamID: 4, NewStreamID: archiveStream},
	} {
		hh.h.Handle(context.Background(), ev)
	}
	if c := archiveConv(t, hh); c.ID != conv.ID {
		t.Fatalf("the conversation changed: %s → %s", conv.ID, c.ID)
	}

	broken, _ := archiveHarness(t, nil)
	bconv := archiveConv(t, broken)
	broken.breakJournal(t)
	broken.h.Handle(context.Background(), zulipproto.Event{
		Type: zulipproto.EventUpdateMessage, StreamID: 4, NewStreamID: archiveStream, OrigTopic: "t", Topic: "t",
	})
	_ = bconv
	if !broken.logged("retiring the conversation failed") {
		t.Fatalf("logs = %v", broken.logs)
	}
}

// TestArchiveAskingTwiceIsOneArm: a second `!archive` must not bury the
// warning the confirmation is bound to, nor park a second expiry timer.
func TestArchiveAskingTwiceIsOneArm(t *testing.T) {
	hh, _ := archiveHarness(t, nil)
	conv := archiveConv(t, hh)
	hh.deliver(t, "t", "!archive")
	first, _ := hh.armed(conv.ID)
	hh.deliver(t, "t", "!archive")
	second, _ := hh.armed(conv.ID)
	if second != first {
		t.Fatalf("the arm moved: %d → %d", first, second)
	}
	if got := hh.z.lastBody(); !strings.Contains(got, "already waiting to be archived") {
		t.Fatalf("reply = %q", got)
	}
	// The original warning still confirms.
	hh.tap(humanID, first)
	if len(hh.z.moved()) != 1 {
		t.Fatalf("moves = %v", hh.z.moved())
	}
}

// TestArchiveLapsedWarningIsNotAFreshCycle: once the conversation has
// moved past a lapsed warning, tapping it archives nothing AND arms
// nothing — a fresh cycle starts from the message the gesture is
// defined on, not from a warning buried in the scrollback.
func TestArchiveLapsedWarningIsNotAFreshCycle(t *testing.T) {
	now := time.Now()
	hh, own := archiveHarness(t, func(c *Config) {
		c.Now = func() time.Time { return now }
	})
	conv := archiveConv(t, hh)
	hh.tap(humanID, own)
	prompt, _ := hh.armed(conv.ID)
	now = now.Add(archiveConfirmTTL + time.Second)
	// Something else posts after the warning, so it is no longer last.
	hh.deliver(t, "t", mention("still here?"))
	hh.tap(humanID, prompt)
	if len(hh.z.moved()) != 0 {
		t.Fatalf("a lapsed warning archived: %v", hh.z.moved())
	}
	if _, armed := hh.armed(conv.ID); armed {
		t.Fatal("a buried warning armed a fresh cycle")
	}
}
