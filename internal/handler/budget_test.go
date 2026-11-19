package handler

import (
	"context"
	"testing"
	"time"

	"github.com/kfet/acp-kit/schedule"
)

// TestFireScheduleReportsSilence: the store backs off and stops on
// silent fires, so FireSchedule must say which turns had no news.
func TestFireScheduleReportsSilence(t *testing.T) {
	lh := silentLoop(t)
	res, err := lh.h.FireSchedule(context.Background(), schedule.Item{ID: "s1", Conv: chanToken("hb"), Text: "check", Every: time.Minute})
	if err != nil || !res.Silent {
		t.Fatalf("abstained turn: %+v %v", res, err)
	}
	lh.a.mu.Lock()
	lh.a.chunks = []string{"news!"}
	lh.a.mu.Unlock()
	res, err = lh.h.FireSchedule(context.Background(), schedule.Item{ID: "s1", Conv: chanToken("hb"), Text: "check", Every: time.Minute})
	if err != nil || res.Silent {
		t.Fatalf("turn with news: %+v %v", res, err)
	}
}

// TestCollapsedRepeatIsSilent: the same answer again is no news.
func TestCollapsedRepeatIsSilent(t *testing.T) {
	lh := repeatLoop(t, "build red", nil)
	it := schedule.Item{ID: "s1", Conv: chanToken("hb"), Text: "check", Every: time.Minute}
	if res, _ := lh.h.FireSchedule(context.Background(), it); res.Silent {
		t.Fatal("first answer is news")
	}
	if res, _ := lh.h.FireSchedule(context.Background(), it); !res.Silent {
		t.Fatal("a collapsed repeat must count as silent")
	}
}

func TestScheduleStoppedNeverPosts(t *testing.T) {
	lh := newLoopHarness(t, newAgent("hi"), nil)
	lh.engage(t, "loopback")
	n := len(lh.z.stored())
	for _, why := range []schedule.Stop{schedule.StopFires, schedule.StopSilent} {
		lh.h.ScheduleStopped(schedule.Item{ID: "s1", Conv: chanToken("loopback")}, why)
	}
	if len(lh.z.stored()) != n {
		t.Fatal("the agent is told in a turn; the relay posts nothing")
	}
}

func TestRetime(t *testing.T) {
	lh := newLoopHarness(t, newAgent("hi"), nil)
	lh.engage(t, "loopback")
	it, err := lh.h.Schedule(chanToken("loopback"), "ping", fireEpoch.Add(time.Hour), 0, schedule.Budget{})
	if err != nil {
		t.Fatal(err)
	}
	got, err := lh.h.Retime(chanToken("loopback"), it.ID, fireEpoch.Add(2*time.Hour), time.Hour, schedule.Budget{MaxFires: 3})
	if err != nil || got.Every != time.Hour || got.MaxFires != 3 {
		t.Fatalf("Retime = %+v, %v", got, err)
	}
	if _, err := lh.h.Retime(chanToken("loopback"), "nope", fireEpoch.Add(time.Hour), 0, schedule.Budget{}); err == nil {
		t.Fatal("want error for an unknown id")
	}
	var off Handler
	if _, err := off.Retime("x", "y", fireEpoch, 0, schedule.Budget{}); err == nil {
		t.Fatal("want error without a store")
	}
}
