package handler

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kfet/acp-kit/relaytool"
	"github.com/kfet/acp-kit/schedule"
	"github.com/kfet/zulip-acp/internal/journal"
	"github.com/kfet/zulip-acp/internal/zulipproto"
)

// TestScheduleArmedInTheRenamingTurnSurvivesTheRename is the regression
// for "schedule: dropped … — its conversation is gone": a turn that
// arms a schedule and calls rename_topic moved the conversation to a
// new key, and the schedule stayed under the old one.
func TestScheduleArmedInTheRenamingTurnSurvivesTheRename(t *testing.T) {
	agent := newAgent("ok")
	lh := renameHarness(t, agent)
	var reply string
	var serr, rerr error
	agent.during = func() {
		convs := lh.h.cfg.Journal.Convs()
		if len(convs) != 1 {
			t.Errorf("convs = %+v", convs)
			return
		}
		_, serr = lh.h.Schedule(convs[0].Key.Token(), "check the deploy", fireEpoch.Add(time.Minute), 0)
		reply, rerr = lh.h.RenameTopic(convs[0].Key, "Deploy watch")
	}
	deliverLobby(t, lh, mention("watch the deploy for me"))
	agent.during = nil
	if serr != nil || rerr != nil {
		t.Fatalf("Schedule: %v; RenameTopic: %v (%q)", serr, rerr, reply)
	}
	newTok := journal.Channel(4, "Deploy watch").Token()
	if got := lh.h.Schedules(newTok); len(got) != 1 {
		t.Fatalf("schedules under the new topic = %+v, want the one armed before the rename", got)
	}
	if got := lh.store.List(""); len(got) != 1 || got[0].Conv != newTok {
		t.Fatalf("store = %+v, want one item under the new topic", got)
	}

	// And it fires into the renamed topic instead of being dropped.
	lh.run(t)
	before := len(agent.prompted())
	lh.now = func() time.Time { return fireEpoch.Add(2 * time.Minute) }
	lh.ticks <- fireEpoch
	lh.awaitTurnEnd(t)
	p := agent.prompted()
	if len(p) != before+1 || !strings.Contains(p[len(p)-1], "check the deploy") {
		t.Fatalf("prompts = %q, want the scheduled prompt", p)
	}
}

// TestNewSessionArmedInTheRenamingTurnIsApplied: `new_session` is
// deferred to the end of the turn, and so is `rename_topic`. The rename
// lands first and moves the token, so the deferred reset must move
// with it.
func TestNewSessionArmedInTheRenamingTurnIsApplied(t *testing.T) {
	agent := newAgent("ok")
	lh := renameHarness(t, agent)
	var orig journal.Conv
	var nerr, rerr error
	agent.during = func() {
		convs := lh.h.cfg.Journal.Convs()
		if len(convs) != 1 {
			t.Errorf("convs = %+v", convs)
			return
		}
		orig = convs[0]
		_, nerr = callTool(t, lh.tools, relaytool.ToolNewSession, orig.ID, `{}`)
		_, rerr = lh.h.RenameTopic(orig.Key, "Fresh start")
	}
	deliverLobby(t, lh, mention("start over in a new topic"))
	agent.during = nil
	if nerr != nil || rerr != nil {
		t.Fatalf("new_session: %v; RenameTopic: %v", nerr, rerr)
	}
	c, ok := lh.j.Lookup(journal.Channel(4, "Fresh start"))
	if !ok {
		t.Fatal("no conversation under the new topic")
	}
	if c.ID == orig.ID {
		t.Fatalf("the deferred new_session was lost in the rename; logs:\n%s", strings.Join(lh.logs, "\n"))
	}
}

// TestHumanTopicMoveCarriesSchedules: a rename by a human arrives as an
// update_message event, and the schedules follow it too.
func TestHumanTopicMoveCarriesSchedules(t *testing.T) {
	lh := newLoopHarness(t, newAgent("ok"), nil)
	lh.engage(t, "loopback")
	if _, err := lh.h.Schedule(chanToken("loopback"), "ping", fireEpoch.Add(time.Hour), 0); err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	lh.h.Handle(context.Background(), zulipproto.Event{
		Type: zulipproto.EventUpdateMessage, StreamID: 4, OrigTopic: "loopback", Topic: "renamed",
	})
	if got := lh.h.Schedules(chanToken("renamed")); len(got) != 1 {
		t.Fatalf("schedules under the new topic = %+v", got)
	}
	if got := lh.h.Schedules(chanToken("loopback")); len(got) != 0 {
		t.Fatalf("schedules left under the old topic = %+v", got)
	}
	if !lh.logged("schedule(s) follow the moved topic") {
		t.Fatalf("logs = %v", lh.logs)
	}
}

// TestTopicMergeCarriesSchedulesToTheSurvivor: a topic moved onto one
// that has its own conversation retires the moved conversation. The
// topic is truth, so its schedules go to the conversation that now
// answers there.
func TestTopicMergeCarriesSchedulesToTheSurvivor(t *testing.T) {
	lh := newLoopHarness(t, newAgent("ok"), nil)
	lh.engage(t, "a")
	lh.engage(t, "b")
	if _, err := lh.h.Schedule(chanToken("a"), "ping", fireEpoch.Add(time.Hour), 0); err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	lh.h.Handle(context.Background(), zulipproto.Event{
		Type: zulipproto.EventUpdateMessage, StreamID: 4, OrigTopic: "a", Topic: "b",
	})
	if got := lh.h.Schedules(chanToken("b")); len(got) != 1 {
		t.Fatalf("schedules under the surviving topic = %+v", got)
	}
}

// TestMoveOfAnUnknownTopicTouchesNothing: no conversation, no state.
func TestMoveOfAnUnknownTopicTouchesNothing(t *testing.T) {
	lh := newLoopHarness(t, newAgent("ok"), nil)
	// Armed for a topic the journal does not know.
	if _, err := lh.h.Schedule(chanToken("ghost"), "ping", fireEpoch.Add(time.Hour), 0); err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	if _, moved, err := lh.h.renameConv(4, "ghost", "elsewhere"); moved || err != nil {
		t.Fatalf("renameConv = %v, %v", moved, err)
	}
	if got := lh.h.Schedules(chanToken("ghost")); len(got) != 1 {
		t.Fatalf("an unknown topic's schedules moved: %+v", got)
	}
}

// TestRekeyScheduleFailureIsLogged: the journal move has happened, so a
// schedule store that cannot persist is reported, not returned.
func TestRekeyScheduleFailureIsLogged(t *testing.T) {
	lh := newLoopHarness(t, newAgent("ok"), nil)
	path := filepath.Join(t.TempDir(), "s.json")
	store, err := schedule.Open(schedule.Config{
		Path: path,
		Fire: func(context.Context, schedule.Item) error { return nil },
		Now:  func() time.Time { return fireEpoch },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Add("from", "x", fireEpoch.Add(time.Hour), 0); err != nil {
		t.Fatal(err)
	}
	// The destination path is a NON-EMPTY directory, so the save fails.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(path, "occupied"), 0o755); err != nil {
		t.Fatal(err)
	}
	lh.h.cfg.Schedules = store
	lh.h.rekey("from", "to")
	if !lh.logged("its schedules did not follow") {
		t.Fatalf("logs = %v", lh.logs)
	}
}

// TestRekeyWithoutTheLoopbackIsANoOp: a relay with no schedules, no
// loopback and no commands has no token state to move.
func TestRekeyWithoutTheLoopbackIsANoOp(t *testing.T) {
	hh := newHarness(t, newAgent("ok"), func(c *Config) {
		c.Commands, c.Schedules, c.Loopback = nil, nil, nil
	})
	hh.h.rekey("from", "to")
	hh.h.rekey("same", "same")
}

// TestRenamedTopicIsSettledAfterTheTurn: endTurn settles the status
// mark under the topic's NEW name. With the old token it found no
// conversation and the renamed topic kept no mark.
func TestRenamedTopicIsSettledAfterTheTurn(t *testing.T) {
	lh := statusHarness(t)
	lh.seedTrigger("work")
	var rerr error
	lh.a.during = func() {
		c, ok := lh.h.cfg.Journal.Lookup(journal.Channel(4, "work"))
		if !ok {
			rerr = errUnknownConv
			return
		}
		_, rerr = lh.h.RenameTopic(c.Key, "Better title")
	}
	lh.deliverTurn(t, "work", mention("go"))
	lh.a.during = nil
	if rerr != nil {
		t.Fatalf("RenameTopic: %v", rerr)
	}
	convs := lh.j.Convs()
	if len(convs) != 1 || convs[0].Topic != "✔ Better title" {
		t.Fatalf("journal = %+v, want the renamed topic resolved", convs)
	}
}

// parkedFire starts a human turn in topic "busy" that blocks, parks a
// scheduled fire behind it, runs change while both wait, then lets the
// human turn finish and returns what the fire returned.
func parkedFire(t *testing.T, change func(lh *loopHarness, conv journal.Conv)) (*loopHarness, error) {
	t.Helper()
	agent := newAgent("answer")
	waiting := make(chan string, 4)
	lh := newLoopHarness(t, agent, func(c *Config) {
		c.OnWaitForConv = func(id string) {
			select {
			case waiting <- id:
			default:
			}
		}
	})
	lh.engage(t, "other")
	agent.block = make(chan struct{})
	lh.h.Handle(context.Background(), channelEvent(humanID, "busy", mention("hello")))
	<-agent.entered
	conv, ok := lh.j.Lookup(journal.Channel(4, "busy"))
	if !ok {
		t.Fatal("conversation was not allocated")
	}
	fired := make(chan error, 1)
	go func() {
		fired <- lh.h.FireSchedule(context.Background(),
			schedule.Item{ID: "s1", Conv: chanToken("busy"), Text: "later", Depth: 1})
	}()
	<-waiting
	change(lh, conv)
	close(agent.block)
	err := <-fired
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if werr := lh.h.WaitIdle(ctx); werr != nil {
		t.Fatalf("WaitIdle: %v", werr)
	}
	return lh, err
}

// TestFiredScheduleFollowsARenameItWaitedOut: the human turn the fire
// waited for renamed the topic. The scheduled answer goes to the new
// name, not to a second topic under the old one.
func TestFiredScheduleFollowsARenameItWaitedOut(t *testing.T) {
	lh, err := parkedFire(t, func(lh *loopHarness, _ journal.Conv) {
		if _, moved, err := lh.h.renameConv(4, "busy", "renamed"); !moved || err != nil {
			t.Errorf("renameConv = %v, %v", moved, err)
		}
	})
	if err != nil {
		t.Fatalf("FireSchedule: %v", err)
	}
	lh.z.mu.Lock()
	last := lh.z.order[len(lh.z.order)-1]
	lh.z.mu.Unlock()
	if got := lh.topicOf(last); got != "renamed" {
		t.Fatalf("the scheduled answer went to topic %q, want the renamed one", got)
	}
}

// TestFiredScheduleFollowsANewSessionItWaitedOut: the human turn the
// fire waited for retired the conversation. The scheduled turn runs in
// the conversation that now answers in the topic.
func TestFiredScheduleFollowsANewSessionItWaitedOut(t *testing.T) {
	var fresh journal.Conv
	lh, err := parkedFire(t, func(lh *loopHarness, _ journal.Conv) {
		var rerr error
		if _, fresh, _, rerr = lh.j.Retire(journal.Channel(4, "busy")); rerr != nil {
			t.Errorf("Retire: %v", rerr)
		}
	})
	if err != nil {
		t.Fatalf("FireSchedule: %v", err)
	}
	if got := lh.sessionCount(fresh.SessionKey()); got != 1 {
		t.Fatalf("the fresh conversation has %d sessions, want the scheduled turn's", got)
	}
}

// TestFiredScheduleIsGoneWhenItsTopicMergedAway: the topic was moved
// onto one with its own conversation while the fire waited, and the
// moved conversation was retired with no successor at its key.
func TestFiredScheduleIsGoneWhenItsTopicMergedAway(t *testing.T) {
	_, err := parkedFire(t, func(lh *loopHarness, _ journal.Conv) {
		if _, _, err := lh.h.renameConv(4, "busy", "other"); err != nil {
			t.Errorf("renameConv: %v", err)
		}
	})
	if !errors.Is(err, schedule.ErrGone) {
		t.Fatalf("FireSchedule = %v, want ErrGone", err)
	}
}

// TestRekeyMovesTheStatusLock: the lock held under the old name is the
// lock taken under the new one, so the two cannot run at once.
func TestRekeyMovesTheStatusLock(t *testing.T) {
	hh := newHarness(t, newAgent("ok"), nil)
	unlock := hh.h.statusLock(journal.Channel(4, "old"))
	defer unlock()
	hh.h.rekey(chanToken("old"), chanToken("new"))
	v, ok := hh.h.statusMu.Load(chanToken("new"))
	if !ok || v.(*sync.Mutex).TryLock() {
		t.Fatal("the status lock did not follow the topic")
	}
	if _, ok := hh.h.statusMu.Load(chanToken("old")); ok {
		t.Fatal("the old name kept a status lock")
	}
}
