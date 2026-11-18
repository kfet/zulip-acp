package handler

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/kfet/acp-kit/schedule"
	"github.com/kfet/zulip-acp/internal/config"
	"github.com/kfet/zulip-acp/internal/journal"
	"github.com/kfet/zulip-acp/internal/zulipproto"
)

// lifecycleHarness is a view harness with one archive rule over the
// work channel: done or fyi, idle a week, source prefix on.
func lifecycleHarness(t *testing.T) *viewHarness {
	t.Helper()
	bh := newViewHarness(t)
	bh.h.cfg.ArchiveStreamID, bh.h.cfg.ArchiveChannel = 77, "archive"
	bh.h.cfg.Lifecycle = []config.ResolvedLifecycle{{States: []string{journal.StateDone, journal.StateFYI}, Idle: 168 * time.Hour, PrefixSource: true, Sources: []int64{testWork}}}
	return bh
}

// archived returns the topics moved to the archive channel.
func (bh *viewHarness) archived() string {
	return strings.Join(bh.z.moved(), "|")
}

func TestLifecycleArchives(t *testing.T) {
	bh := lifecycleHarness(t)
	ctx := context.Background()
	week := 200 * time.Hour
	old := bh.conv(t, "old", journal.StateDone, journal.SetByUser, "", week)
	bh.conv(t, "fresh", journal.StateDone, journal.SetByUser, "", time.Hour)
	bh.conv(t, "nu", journal.StateNeedsYou, journal.SetByUser, "", week)
	busy := bh.conv(t, "busy", journal.StateFYI, journal.SetByUser, "", week)
	sched := bh.conv(t, "sched", journal.StateDone, journal.SetByUser, "", week)
	bh.h.setInflight(busy.ID, &inflightEntry{cancel: func() {}})
	store, err := schedule.Open(schedule.Config{Path: filepath.Join(t.TempDir(), "s.json"), Ticks: make(chan time.Time), Now: func() time.Time { return viewNow },
		Fire: func(context.Context, schedule.Item) (schedule.Result, error) { return schedule.Result{}, nil }, Logf: func(string, ...any) {}})
	if err != nil {
		t.Fatal(err)
	}
	bh.h.cfg.Schedules = store
	if _, err := bh.h.Schedule(sched.Key.Token(), "x", viewNow.Add(time.Hour), 0, schedule.Budget{}); err != nil {
		t.Fatal(err)
	}

	// A message in the topic within the idle time: the topic is truth.
	bh.z.setHistory(zulipproto.Message{ID: 1, Timestamp: viewNow.Add(-time.Hour).Unix()})
	bh.h.runLifecycle(ctx)
	if got := bh.archived(); got != "" {
		t.Fatalf("archived a topic with a recent message: %s", got)
	}
	// The read fails: nothing moves.
	bh.z.historyErr = errors.New("down")
	bh.h.runLifecycle(ctx)
	if bh.archived() != "" || !bh.logged("reading its newest message failed") {
		t.Fatal("archived without evidence")
	}
	bh.z.historyErr = nil

	bh.z.setHistory(zulipproto.Message{ID: 1, Timestamp: viewNow.Add(-week).Unix()})
	bh.h.runLifecycle(ctx)
	got := bh.archived()
	if !strings.Contains(got, ":->77:fleet: old:") || strings.Count(got, "->77") != 1 {
		t.Fatalf("moves = %s", got)
	}
	if _, ok := bh.j.Lookup(old.Key); ok {
		t.Fatal("the archived conversation still answers")
	}
	if !bh.logged("lifecycle archives") || !bh.posted("Archived by the lifecycle rule") {
		t.Fatal("no log or closing message")
	}
}

func TestLifecycleTooOldAndOff(t *testing.T) {
	bh := lifecycleHarness(t)
	ctx := context.Background()
	bh.conv(t, "old", journal.StateDone, journal.SetByUser, "", 200*time.Hour)
	bh.z.setHistory(zulipproto.Message{ID: 1, Timestamp: viewNow.Add(-200 * time.Hour).Unix()})
	bh.h.cfg.ArchiveMoveLimit = time.Hour
	bh.z.oldest = zulipproto.Message{ID: 1, Timestamp: viewNow.Add(-200 * time.Hour).Unix()}
	bh.h.runLifecycle(ctx)
	bh.h.runLifecycle(ctx)
	if bh.archived() != "" || bh.logCount("lifecycle skips") != 1 {
		t.Fatalf("moves %s, skip logs %d", bh.archived(), bh.logCount("lifecycle skips"))
	}

	// Archiving off: rules do nothing.
	bh.h.cfg.ArchiveMoveLimit = 0
	bh.h.tooOld.Clear()
	bh.h.cfg.ArchiveStreamID = 0
	bh.h.runLifecycle(ctx)
	if bh.archived() != "" {
		t.Fatal("archived with archiving off")
	}
}

func TestLifecycleRunsInViewLoop(t *testing.T) {
	bh := newViewHarness(t)
	bh.h.cfg.Views, bh.h.cfg.Notifies = nil, nil
	if bh.h.hasViews() {
		t.Fatal("no views, no rules")
	}
	bh.h.cfg.Lifecycle = []config.ResolvedLifecycle{{States: []string{journal.StateDone}, Idle: time.Hour}}
	if !bh.h.hasViews() {
		t.Fatal("a rule alone must start the loop")
	}
}

func TestPrefixTopic(t *testing.T) {
	if got := prefixTopic("fleet", "x"); got != "fleet: x" {
		t.Fatal(got)
	}
	long := prefixTopic("fleet", strings.Repeat("y", 80))
	if utf8.RuneCountInString(long) != zulipproto.MaxTopicLength || !strings.HasSuffix(long, "…") {
		t.Fatal(long)
	}
}

func TestViewCollapse(t *testing.T) {
	bh := newViewHarness(t)
	v := testView
	v.Collapse, v.CollapseShow = []string{journal.StateDone, journal.StateFYI}, 2
	bh.conv(t, "nu", journal.StateNeedsYou, journal.SetByUser, "", 0)
	for i, tp := range []string{"d1", "d2", "d3"} {
		bh.conv(t, tp, journal.StateDone, journal.SetByUser, "", time.Duration(i)*time.Hour)
	}
	out := bh.h.renderView(v, bh.j.Convs())
	spoiler := strings.Index(out, "```spoiler Done (3)\n")
	if spoiler < 0 || strings.Index(out, "**Needs you**") > spoiler || !strings.HasSuffix(out, "- …and 1 more\n```\n") {
		t.Fatalf("view:\n%s", out)
	}
	if strings.Contains(out, "fleet>d3") || !strings.Contains(out, "fleet>d1") {
		t.Fatalf("not the newest two:\n%s", out)
	}
	// A full view closes the spoiler fence.
	v.CollapseShow = 100
	for i := range 120 {
		bh.conv(t, strings.Repeat("z", 40)+string(rune('a'+i%26))+strings.Repeat("q", i/26), journal.StateDone, journal.SetByUser, strings.Repeat("r", 30), 0)
	}
	out = bh.h.renderView(v, bh.j.Convs())
	if !strings.HasSuffix(out, "```\n\n…the view is full.") {
		t.Fatalf("full view tail: %q", out[len(out)-60:])
	}
}

// posted reports whether any stored message contains sub.
func (bh *viewHarness) posted(sub string) bool {
	bh.z.mu.Lock()
	defer bh.z.mu.Unlock()
	for _, b := range bh.z.bodies {
		if strings.Contains(b, sub) {
			return true
		}
	}
	return false
}
