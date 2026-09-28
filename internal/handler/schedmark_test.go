package handler

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kfet/acp-kit/schedule"
	"github.com/kfet/zulip-acp/internal/journal"
)

// markerMsg returns the id and body of the one stored message that
// carries the schedule marker, or 0 when none does.
func (z *fakeZulip) markerMsg() (int64, string) {
	z.mu.Lock()
	defer z.mu.Unlock()
	for _, id := range z.order {
		if strings.Contains(z.bodies[id], "⏰") {
			return id, z.bodies[id]
		}
	}
	return 0, ""
}

// armDuring makes the agent's NEXT turn run fn mid-turn, the way an MCP
// tool call would.
func (lh *loopHarness) armDuring(fn func()) {
	lh.a.mu.Lock()
	lh.a.during = func() {
		fn()
		// Prompt runs this with the lock released, so taking it is safe.
		lh.a.mu.Lock()
		lh.a.during = nil
		lh.a.mu.Unlock()
	}
	lh.a.mu.Unlock()
}

func mustSchedule(t *testing.T, h *Handler, tok, text string, at time.Time) schedule.Item {
	t.Helper()
	it, err := h.Schedule(tok, text, at, 0)
	if err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	return it
}

func TestFooterMarksAnArmedSchedule(t *testing.T) {
	at := fireEpoch.Add(time.Hour)
	cases := []struct {
		name   string
		before int // schedules already pending before the turn
		arm    []time.Time
		want   string
	}{
		{"one", 0, []time.Time{at}, "⏰ +1 → <time:2026-09-01T13:00:00Z>*"},
		{"others pending", 3, []time.Time{at}, "⏰ +1 → <time:2026-09-01T13:00:00Z> · 3 pending*"},
		{"several", 0, []time.Time{at.Add(time.Hour), at}, "⏰ +2 → <time:2026-09-01T13:00:00Z>*"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			lh := newLoopHarness(t, newAgent("done"), nil)
			tok := chanToken("loopback")
			lh.engage(t, "loopback")
			for i := 0; i < c.before; i++ {
				mustSchedule(t, lh.h, tok, fmt.Sprintf("old %d", i), at.Add(3*time.Hour))
			}
			lh.armDuring(func() {
				for _, a := range c.arm {
					mustSchedule(t, lh.h, tok, "wake", a)
				}
			})
			lh.deliverTurn(t, "loopback", mention("go"))
			id, body := lh.z.markerMsg()
			if !strings.HasSuffix(body, c.want) {
				t.Fatalf("footer = %q, want suffix %q", body, c.want)
			}
			added, _ := lh.z.reactions()
			if !slices.Contains(added, fmt.Sprintf("%d:%s", id, alarmEmoji)) {
				t.Fatalf("reactions = %v, want :%s: on %d", added, alarmEmoji, id)
			}
		})
	}
}

func TestFooterMarksACancelledSchedule(t *testing.T) {
	lh := newLoopHarness(t, newAgent("done"), nil)
	tok := chanToken("loopback")
	lh.engage(t, "loopback")
	var ids []string
	for i := 0; i < 3; i++ {
		ids = append(ids, mustSchedule(t, lh.h, tok, "x", fireEpoch.Add(time.Hour)).ID)
	}
	lh.armDuring(func() {
		if err := lh.h.Unschedule(tok, ids[0]); err != nil {
			t.Errorf("Unschedule: %v", err)
		}
	})
	lh.deliverTurn(t, "loopback", mention("drop one"))
	if _, body := lh.z.markerMsg(); !strings.HasSuffix(body, "⏰ −1 · 2 pending*") {
		t.Fatalf("footer = %q", body)
	}
	if added, _ := lh.z.reactions(); slices.ContainsFunc(added, func(s string) bool { return strings.HasSuffix(s, alarmEmoji) }) {
		t.Fatalf("a cancel added an alarm: %v", added)
	}
}

func TestFooterHasNoMarkerWhenNothingChanged(t *testing.T) {
	cases := map[string]func(lh *loopHarness, tok string){
		"untouched": func(*loopHarness, string) {},
		"armed and cancelled": func(lh *loopHarness, tok string) {
			lh.armDuring(func() {
				it := mustSchedule(t, lh.h, tok, "x", fireEpoch.Add(time.Hour))
				if err := lh.h.Unschedule(tok, it.ID); err != nil {
					t.Errorf("Unschedule: %v", err)
				}
			})
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			lh := newLoopHarness(t, newAgent("done"), nil)
			tok := chanToken("loopback")
			lh.engage(t, "loopback")
			mustSchedule(t, lh.h, tok, "pending", fireEpoch.Add(time.Hour))
			setup(lh, tok)
			lh.deliverTurn(t, "loopback", mention("hi"))
			if id, body := lh.z.markerMsg(); id != 0 {
				t.Fatalf("unexpected marker: %q", body)
			}
			if !strings.Contains(strings.Join(lh.z.stored(), "\n"), "done") {
				t.Fatal("the turn did not answer")
			}
		})
	}
}

func TestFiredTurnIsMarkedAndDropsItsAlarm(t *testing.T) {
	lh := newLoopHarness(t, newAgent("woke"), nil)
	tok := chanToken("loopback")
	lh.engage(t, "loopback")
	lh.run(t)

	// A turn arms two schedules: the reaction goes on its reply.
	var first schedule.Item
	lh.armDuring(func() {
		first = mustSchedule(t, lh.h, tok, "soon", fireEpoch.Add(time.Minute))
		mustSchedule(t, lh.h, tok, "later", fireEpoch.Add(time.Hour))
	})
	lh.deliverTurn(t, "loopback", mention("arm"))
	armID, _ := lh.z.markerMsg()
	lh.z.reset()

	// The first fires: one still shares the reply, so the reaction stays.
	lh.now = func() time.Time { return fireEpoch.Add(2 * time.Minute) }
	lh.ticks <- fireEpoch
	lh.awaitTurnEnd(t)
	lh.stop()
	<-lh.done
	if _, body := lh.z.markerMsg(); !strings.HasSuffix(body, "⏰ fired · 1 pending*") {
		t.Fatalf("fired footer = %q", body)
	}
	if _, del := lh.z.reactions(); len(del) != 0 {
		t.Fatalf("removed %v while another schedule still shares the reply", del)
	}
	if _, _, ok := lh.j.TakeAlarm(first.ID); ok {
		t.Fatal("the fired schedule's alarm record survived")
	}

	// Cancelling the last one clears the reaction.
	rest := lh.h.Schedules(tok)
	if err := lh.h.Unschedule(tok, rest[0].ID); err != nil {
		t.Fatalf("Unschedule: %v", err)
	}
	if _, del := lh.z.reactions(); !slices.Equal(del, []string{fmt.Sprintf("%d:%s", armID, alarmEmoji)}) {
		t.Fatalf("removed = %v, want the alarm on %d", del, armID)
	}
}

func TestFiredTurnWithNothingLeftSaysFired(t *testing.T) {
	lh := newLoopHarness(t, newAgent("woke"), nil)
	tok := chanToken("loopback")
	lh.engage(t, "loopback")
	lh.run(t)
	mustSchedule(t, lh.h, tok, "soon", fireEpoch.Add(time.Minute))
	lh.now = func() time.Time { return fireEpoch.Add(2 * time.Minute) }
	lh.ticks <- fireEpoch
	lh.awaitTurnEnd(t)
	lh.stop()
	<-lh.done
	if _, body := lh.z.markerMsg(); !strings.HasSuffix(body, "⏰ fired*") {
		t.Fatalf("fired footer = %q", body)
	}
}

func TestGoneScheduleDropsItsAlarm(t *testing.T) {
	lh := newLoopHarness(t, newAgent("ok"), nil)
	if err := lh.j.SetAlarm("s1", 77); err != nil {
		t.Fatal(err)
	}
	// A repeating item in a conversation the journal never had.
	err := lh.h.FireSchedule(context.Background(), schedule.Item{ID: "s1", Conv: chanToken("nowhere"), Every: time.Hour})
	if err == nil {
		t.Fatal("fired into a conversation that does not exist")
	}
	if _, del := lh.z.reactions(); !slices.Equal(del, []string{"77:" + alarmEmoji}) {
		t.Fatalf("removed = %v", del)
	}
}

func TestRepeatingFireKeepsItsAlarm(t *testing.T) {
	lh := newLoopHarness(t, newAgent("tick"), nil)
	tok := chanToken("loopback")
	lh.engage(t, "loopback")
	if err := lh.j.SetAlarm("r1", 55); err != nil {
		t.Fatal(err)
	}
	if err := lh.h.FireSchedule(context.Background(), schedule.Item{ID: "r1", Conv: tok, Text: "tick", Every: time.Hour}); err != nil {
		t.Fatalf("FireSchedule: %v", err)
	}
	lh.awaitTurnEnd(t)
	if _, del := lh.z.reactions(); len(del) != 0 {
		t.Fatalf("a repeating schedule lost its alarm: %v", del)
	}
}

func TestSchedCommandListsPending(t *testing.T) {
	lh := newLoopHarness(t, newAgent("ok"), nil)
	tok := chanToken("loopback")
	lh.engage(t, "loopback")

	lh.deliver(t, "loopback", "!sched")
	if got := lh.only(t); got != "⏰ Nothing is scheduled here." {
		t.Fatalf("empty !sched = %q", got)
	}
	lh.z.reset()

	a := mustSchedule(t, lh.h, tok, "check the deploy\nthen report", fireEpoch.Add(time.Hour))
	b, err := lh.h.Schedule(tok, strings.Repeat("y", schedPreviewRunes+5), fireEpoch.Add(2*time.Hour), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	lh.deliver(t, "loopback", "!SCHED")
	want := "⏰ 2 pending:\n\n" +
		"- `" + a.ID + "` <time:2026-09-01T13:00:00Z> — check the deploy\n" +
		"- `" + b.ID + "` <time:2026-09-01T14:00:00Z> (every 1h0m0s) — " + strings.Repeat("y", schedPreviewRunes) + "…\n"
	if got := lh.only(t); got != want {
		t.Fatalf("!sched =\n%q\nwant\n%q", got, want)
	}
}

func TestSchedCommandWithoutTheStore(t *testing.T) {
	hh := cmdHarness(t, newAgent("ok"), nil)
	hh.deliver(t, "plain", mention("hello"))
	hh.z.reset()
	hh.deliver(t, "plain", "!help")
	if got := hh.only(t); strings.Contains(got, "!sched") {
		t.Fatalf("help advertised !sched with no store: %s", got)
	}
	hh.z.reset()
	hh.deliver(t, "plain", "!sched")
	if got := strings.Join(hh.z.stored(), "\n"); !strings.Contains(got, "Unknown command") {
		t.Fatalf("!sched = %q", got)
	}
	// The broker refuses the listing itself: say why.
	hh.z.reset()
	hh.h.schedCommand(context.Background(), journal.Channel(4, "plain"))
	if got := hh.only(t); !strings.HasPrefix(got, "❌ ") {
		t.Fatalf("schedCommand = %q", got)
	}
}

func TestSchedIsAdvertised(t *testing.T) {
	lh := newLoopHarness(t, newAgent("ok"), nil)
	lh.engage(t, "loopback")
	lh.deliver(t, "loopback", "!help")
	if got := lh.only(t); !strings.Contains(got, "`!sched`") {
		t.Fatalf("!help = %q", got)
	}
	lh.z.reset()
	lh.deliver(t, "loopback", "!opts")
	id := lh.z.lastID()
	if body := lh.z.body(id); !strings.Contains(body, "`!sched` — pending schedules") {
		t.Fatalf("!opts = %q", body)
	}
	if _, replies := panelWidget(t, lh.harness, id); !slices.Contains(replies, "!sched") {
		t.Fatalf("!opts buttons = %v", replies)
	}
}

func TestSchedMarkerNoTurn(t *testing.T) {
	// Changes made while no turn runs — a human `!unschedule` — mark
	// nothing, and must not panic.
	h := &Handler{}
	h.noteArmed(schedule.Item{ID: "a", Conv: "t"})
	h.noteCancelled("t", "a")
	st := h.beginSchedTurn("t", false)
	h.endSchedTurn("t", &schedTurn{}) // a stale turn must not unhook the live one
	if h.schedTurns["t"] != st {
		t.Fatal("a stale end removed the live turn")
	}
	h.endSchedTurn("t", st)
	if len(h.schedTurns) != 0 {
		t.Fatal("end did not remove the turn")
	}
}

func TestMarkArmedEdges(t *testing.T) {
	lh := newLoopHarness(t, newAgent("ok"), nil)
	st := &schedTurn{armed: []schedule.Item{{ID: "a"}}}
	lh.h.markArmed(context.Background(), st, nil, 0)                        // no message
	lh.h.markArmed(context.Background(), st, nil, 9)                        // already gone
	lh.h.markArmed(context.Background(), st, []schedule.Item{{ID: "a"}}, 9) // marks
	if added, _ := lh.z.reactions(); !slices.Equal(added, []string{"9:" + alarmEmoji}) {
		t.Fatalf("added = %v", added)
	}
	if m, _, ok := lh.j.TakeAlarm("a"); !ok || m != 9 {
		t.Fatalf("alarm record = %d %v", m, ok)
	}
}

func TestSchedMarkerCombined(t *testing.T) {
	at := fireEpoch.Add(time.Hour)
	st := &schedTurn{fired: true, armed: []schedule.Item{{ID: "n", At: at}}, cancelled: 1}
	got := (&Handler{}).schedMarker(st, []schedule.Item{{ID: "n"}, {ID: "o"}})
	if want := "⏰ fired · +1 → <time:2026-09-01T13:00:00Z> · −1 · 1 pending"; got != want {
		t.Fatalf("marker = %q, want %q", got, want)
	}
}

func TestUnscheduleUnknownID(t *testing.T) {
	lh := newLoopHarness(t, newAgent("ok"), nil)
	if err := lh.h.Unschedule(chanToken("loopback"), "nope"); err == nil {
		t.Fatal("cancelled a schedule that does not exist")
	}
	if _, del := lh.z.reactions(); len(del) != 0 {
		t.Fatalf("removed %v for a failed cancel", del)
	}
}

func TestAlarmFailuresAreSwallowed(t *testing.T) {
	lh := newLoopHarness(t, newAgent("ok"), nil)
	lh.z.reactErr = errors.New("no such emoji")
	pending := []schedule.Item{{ID: "a"}}
	st := &schedTurn{armed: pending}
	lh.h.markArmed(context.Background(), st, pending, 9)
	if m, _, ok := lh.j.TakeAlarm("a"); !ok || m != 9 {
		t.Fatal("a failed reaction lost the record")
	}
	if err := lh.j.SetAlarm("b", 9); err != nil {
		t.Fatal(err)
	}
	lh.h.unmarkAlarm(context.Background(), "b") // logged, not fatal

	// A journal that cannot be written: no record, so no reaction.
	lh.z.reactErr = nil
	before, _ := lh.z.reactions()
	dir := lh.jdir
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	lh.h.markArmed(context.Background(), st, pending, 9)
	if added, _ := lh.z.reactions(); len(added) != len(before) {
		t.Fatalf("reacted without a record: %v", added)
	}
}
