package handler

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"

	"github.com/kfet/acp-kit/schedule"
	"github.com/kfet/zulip-acp/internal/journal"
	"github.com/kfet/zulip-acp/internal/rollover"
	"github.com/kfet/zulip-acp/internal/zulipproto"
)

// fire runs one scheduled turn for item id in topic and waits for it.
func (lh *loopHarness) fire(t *testing.T, topic, id, text string) {
	t.Helper()
	err := lh.h.FireSchedule(context.Background(),
		schedule.Item{ID: id, Conv: chanToken(topic), Text: text, Depth: 1, Every: time.Minute})
	if err != nil {
		t.Fatalf("FireSchedule: %v", err)
	}
}

func silentLoop(t *testing.T) *loopHarness {
	lh := newLoopHarness(t, newAgent("hi"), func(c *Config) {
		c.Now = func() time.Time { return time.Date(2026, 10, 7, 9, 5, 0, 0, time.Local) }
	})
	lh.engage(t, "hb")
	lh.a.mu.Lock()
	lh.a.chunks = []string{"<<SILENT>>"}
	lh.a.mu.Unlock()
	return lh
}

func noSentinel(t *testing.T, z *fakeZulip) {
	t.Helper()
	for _, b := range z.stored() {
		if strings.Contains(b, "<<SILENT>>") {
			t.Fatalf("sentinel posted: %q", b)
		}
	}
}

// TestScheduledTurnAbstainsWithHeartbeat is the regression: a scheduled
// turn that answers with the sentinel posted it verbatim, one message
// per fire. Now it posts one heartbeat and edits it while it is newest.
func TestScheduledTurnAbstainsWithHeartbeat(t *testing.T) {
	lh := silentLoop(t)
	lh.fire(t, "hb", "s1", "check the build\nmore detail")
	noSentinel(t, lh.z)
	got := lh.z.stored()
	if len(got) != 1 || got[0] != "*⏰ checked 09:05 ×1 · check the build*" {
		t.Fatalf("stored = %q", got)
	}
	id := lh.z.order[0]
	if hb, _ := lh.j.Heartbeat("s1"); hb != (journal.Heartbeat{MsgID: id, Count: 1}) {
		t.Fatalf("journal = %+v", hb)
	}

	// Still the newest message: edit in place.
	lh.z.setHistory(zulipproto.Message{ID: id})
	lh.fire(t, "hb", "s1", "check the build")
	if got := lh.z.stored(); len(got) != 1 || !strings.Contains(got[0], "×2") {
		t.Fatalf("stored = %q", got)
	}

	// Somebody posted after it: a new heartbeat.
	lh.z.setHistory(zulipproto.Message{ID: id + 100})
	lh.fire(t, "hb", "s1", "check the build")
	if got := lh.z.stored(); len(got) != 2 || !strings.Contains(got[1], "×1") {
		t.Fatalf("stored = %q", got)
	}
	id2 := lh.z.order[1]

	// The edit fails (edit time limit): a new heartbeat.
	lh.z.setHistory(zulipproto.Message{ID: id2})
	lh.z.mu.Lock()
	lh.z.editErr = errors.New("edit limit")
	lh.z.mu.Unlock()
	lh.fire(t, "hb", "s1", "check the build")
	if got := lh.z.stored(); len(got) != 3 {
		t.Fatalf("stored = %q", got)
	}
	if !lh.logged("editing heartbeat") {
		t.Fatal("edit failure not logged")
	}

	// The read fails: a new heartbeat.
	lh.z.failHistory(errors.New("down"))
	lh.fire(t, "hb", "s1", "check the build")
	if got := lh.z.stored(); len(got) != 4 {
		t.Fatalf("stored = %q", got)
	}

	// Cancelling the schedule forgets its heartbeat.
	lh.h.unmarkAlarm(context.Background(), "s1")
	if _, ok := lh.j.Heartbeat("s1"); ok {
		t.Fatal("heartbeat survived the schedule")
	}
}

func TestHeartbeatPostAndRecordFailures(t *testing.T) {
	lh := silentLoop(t)
	lh.z.mu.Lock()
	lh.z.sendErr = errors.New("no post")
	lh.z.mu.Unlock()
	lh.fire(t, "hb", "s1", "x")
	if !lh.logged("posting heartbeat") {
		t.Fatal("post failure not logged")
	}
	lh.z.mu.Lock()
	lh.z.sendErr = nil
	lh.z.mu.Unlock()
	lh.breakJournal(t)
	lh.fire(t, "hb", "s1", "x")
	if !lh.logged("recording heartbeat") {
		t.Fatal("record failure not logged")
	}
}

func TestHeartbeatTextCaps(t *testing.T) {
	lh := silentLoop(t)
	got := lh.h.heartbeatText(3, strings.Repeat("é", 70))
	if !strings.Contains(got, "×3 · "+strings.Repeat("é", 60)+"…*") {
		t.Fatalf("text = %q", got)
	}
}

// TestSentinelSafetyNetRetractFailure: the delete of the placeholder
// fails; the relay logs and still posts nothing new.
func TestSentinelSafetyNetRetractFailure(t *testing.T) {
	hh := dmHarness(t, newAgent("<<SILENT>>"), nil)
	hh.z.mu.Lock()
	hh.z.deleteErr = errors.New("forbidden")
	hh.z.mu.Unlock()
	hh.deliverDM(t, humanID, "you there?", humanID, botID)
	if !hh.logged("only with the sentinel") || !hh.logged("deleting") {
		t.Fatal("safety net did not run")
	}
}

func toolCallNote() acp.SessionNotification {
	return acp.SessionNotification{Update: acp.SessionUpdate{ToolCall: &acp.SessionUpdateToolCall{ToolCallId: "t1"}}}
}

func TestSinkDropsRepeatAfterToolCall(t *testing.T) {
	cases := []struct {
		name   string
		before string
		after  []string
		want   string
	}{
		{"identical", "Para one.\n\nPara two.", []string{"\n\nPara ", "one.\n\nPara two."}, "Para one.\n\nPara two."},
		{"one paragraph", "Para one.\n\nPara two.", []string{"Para two."}, "Para one.\n\nPara two."},
		{"repeat then new", "Para one.", []string{"Para one.", "\n\nNew bit."}, "Para one.\n\nNew bit."},
		{"new text", "Para one.", []string{"Para", " zwei."}, "Para one.Para zwei."},
		{"prefix only kept", "Para one.", []string{"Para"}, "Para one.Para"},
		{"nothing before", "", []string{"Hello"}, "Hello"},
		{"earlier paragraph kept", "Done.\n\nPara two.", []string{"Done."}, "Done.\n\nPara two.Done."},
		{"blank first", "Para one.", []string{"\n", "Para one."}, "Para one."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			z := newZulip()
			split, err := rollover.New(rollover.Config{Poster: &convPoster{client: z, key: journal.Channel(4, "t")}})
			if err != nil {
				t.Fatal(err)
			}
			s := newStreamingSink(split, true)
			ctx := context.Background()
			if tc.before != "" {
				_ = s.OnUpdate(ctx, chunkNotification(tc.before, nil))
			}
			_ = s.OnUpdate(ctx, toolCallNote())
			_ = s.OnUpdate(ctx, acp.SessionNotification{Update: acp.SessionUpdate{Plan: &acp.SessionUpdatePlan{}}})
			for _, c := range tc.after {
				_ = s.OnUpdate(ctx, chunkNotification(c, nil))
			}
			s.maybeAppendFooter()
			if got := split.Transcript(); got != tc.want {
				t.Fatalf("transcript = %q, want %q", got, tc.want)
			}
			if !strings.HasPrefix(s.messageText(), tc.before) {
				t.Fatalf("said = %q", s.messageText())
			}
		})
	}
}

// TestSentinelSafetyNetForgetsDeletedOwn: the deleted placeholder must
// not stay the conversation's last own message.
func TestSentinelSafetyNetForgetsDeletedOwn(t *testing.T) {
	hh := dmHarness(t, newAgent("<<SILENT>>"), nil)
	hh.deliverDM(t, humanID, "you there?", humanID, botID)
	c := hh.j.Convs()[0]
	if got := hh.h.cachedOwn(c.ID); got != 0 {
		t.Fatalf("cached own = %d, want forgotten", got)
	}
	if rec, _ := hh.j.LookupID(c.ID); rec.LastOwnID != 0 {
		t.Fatalf("journal own = %d, want forgotten", rec.LastOwnID)
	}
}

// TestSentinelSafetyNetQuietMode: nothing was posted, so nothing is
// deleted and nothing is posted.
func TestSentinelSafetyNetQuietMode(t *testing.T) {
	hh := dmHarness(t, newAgent("<<SILENT>>"), func(c *Config) { c.BatchEdits = true })
	hh.deliverDM(t, humanID, "you there?", humanID, botID)
	if hh.z.nextID() != 0 || !hh.logged("only with the sentinel") {
		t.Fatalf("posted %d messages", hh.z.nextID())
	}
}

// TestSentinelWithAttachmentStillPosts: an attachment is an answer.
func TestSentinelWithAttachmentStillPosts(t *testing.T) {
	agent := newAgent("first")
	hh := dmHarness(t, agent, nil)
	hh.deliverDM(t, humanID, "make it", humanID, botID)
	cwd := hh.s.sessions[hh.j.Convs()[0].ID].Cwd
	outbox := filepath.Join(cwd, OutboxDir)
	if err := os.MkdirAll(outbox, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outbox, "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	agent.mu.Lock()
	agent.chunks = []string{"<<SILENT>>"}
	agent.mu.Unlock()
	hh.deliverDM(t, humanID, "again", humanID, botID)
	found := false
	for _, b := range hh.z.stored() {
		found = found || strings.Contains(b, "a.txt")
	}
	if !found {
		t.Fatalf("attachment lost: %q", hh.z.stored())
	}
}
