package handler

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kfet/zulip-acp/internal/config"
	"github.com/kfet/zulip-acp/internal/journal"
	"github.com/kfet/zulip-acp/internal/zulipproto"
)

const (
	testBoard   = int64(9)
	testWork    = int64(4) // "fleet" in the harness
	testViewKey = "view:board/board"
	testNoteKey = "notify:board/needs:needs_you"
	noticeTopic = "needs"
)

// testView is a view of fleet with the default groups.
var testView = config.ResolvedView{Key: testViewKey, StreamID: testBoard, Topic: "board", Sources: []int64{testWork}, Groups: config.DefaultViewGroups, StaleAfter: 72 * time.Hour}

var viewNow = time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)

// viewHarness is a harness with one board pair, a fixed clock and a
// debounce that only fires by hand.
type viewHarness struct {
	*harness
	pending []func()
	delays  []time.Duration
}

func newViewHarness(t *testing.T) *viewHarness {
	t.Helper()
	bh := &viewHarness{}
	bh.harness = newHarness(t, newAgent("ok"), func(c *Config) {
		c.Views = []config.ResolvedView{testView}
		c.Notifies = []config.ResolvedNotify{{Key: testNoteKey, StreamID: testBoard, Topic: noticeTopic, OnEnter: []string{journal.StateNeedsYou}}}
		c.Now = func() time.Time { return viewNow }
		c.ViewAfter = func(d time.Duration, f func()) {
			bh.delays = append(bh.delays, d)
			bh.pending = append(bh.pending, f)
		}
	})
	return bh
}

// conv makes a conversation in the work channel with state (none when
// "") set by setBy, idle since ago.
func (bh *viewHarness) conv(t *testing.T, topic, state, setBy, reason string, ago time.Duration) journal.Conv {
	t.Helper()
	c, err := bh.j.Ensure(journal.Channel(testWork, topic))
	if err != nil {
		t.Fatal(err)
	}
	if state != "" {
		at := viewNow.Add(-ago).UnixMilli()
		if _, _, err := bh.j.SetState(c.ID, journal.TopicState{State: state, SetBy: setBy, Reason: reason, SetAt: at}); err != nil {
			t.Fatal(err)
		}
	}
	c, _ = bh.j.LookupID(c.ID)
	return c
}

// viewBody is the current text of the board message.
func (bh *viewHarness) viewBody(t *testing.T) string {
	t.Helper()
	id := bh.j.View(testViewKey).MsgID
	bh.z.mu.Lock()
	defer bh.z.mu.Unlock()
	b, ok := bh.z.bodies[id]
	if !ok {
		t.Fatalf("no board message %d", id)
	}
	return b
}

func TestViewRenderSections(t *testing.T) {
	bh := newViewHarness(t)
	bh.conv(t, "ask", journal.StateNeedsYou, journal.SetByAgent, "pick a name", time.Hour)
	bh.conv(t, "stuck", journal.StateBlocked, journal.SetByUser, "", 2*time.Hour)
	run := bh.conv(t, "busy", journal.StateWaiting, journal.SetByAgent, "", time.Minute)
	bh.conv(t, "slow", journal.StateWaiting, journal.SetByAgent, "", 4*24*time.Hour)
	bh.conv(t, "old", journal.StateDone, journal.SetByImport, "shipped", 5*time.Hour)
	bh.conv(t, "note", journal.StateFYI, journal.SetByAgent, "", 3*time.Hour)
	bh.conv(t, "fresh", "", "", "", 0)
	// A retired conv (its fresh successor is open), a DM and another channel are not in the view.
	gone := bh.conv(t, "gone", "", "", "", 0)
	if _, _, _, err := bh.j.Retire(gone.Key); err != nil {
		t.Fatal(err)
	}
	if _, err := bh.j.Ensure(journal.DM([]int64{1, 2})); err != nil {
		t.Fatal(err)
	}
	if _, err := bh.j.Ensure(journal.Channel(77, "elsewhere")); err != nil {
		t.Fatal(err)
	}
	e := &inflightEntry{cancel: func() {}}
	bh.h.setInflight(run.ID, e)
	text := bh.h.renderView(testView, bh.j.Convs())
	// Without a "running" group a running conv stays in its state's group.
	noRun := testView
	noRun.Groups = []config.ViewGroup{{Title: "W", States: []string{journal.StateWaiting}}}
	noRun.StaleAfter = 0
	text2 := bh.h.renderView(noRun, bh.j.Convs())
	bh.h.clearInflight(run.ID, e)

	for _, want := range []string{
		"📋 **board** — #**fleet**\n",
		"**Needs you** (1)\n- #**fleet>ask** · 1h · needs_you — pick a name\n",
		"**Blocked** (1)\n- #**fleet>stuck** · 2h · blocked\n",
		"**Running** (1)\n- #**fleet>busy** · 1m · running · waiting\n",
		"**Waiting** (1)\n- #**fleet>slow** · 4d · stalled\n",
		"**Open** (2)\n",
		"- #**fleet>old** · 5h · done (guessed) — shipped\n",
		"- #**fleet>note** · 3h · fyi\n",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("view lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "elsewhere") {
		t.Errorf("view has another channel:\n%s", text)
	}
	if strings.Index(text, "Needs you") > strings.Index(text, "Blocked") ||
		strings.Index(text, "Running") > strings.Index(text, "**Done**") {
		t.Errorf("order:\n%s", text)
	}
	if !strings.Contains(text2, "**W** (2)\n- #**fleet>busy** · 1m · running · waiting\n- #**fleet>slow** · 4d · waiting\n") ||
		strings.Contains(text2, "ask") {
		t.Errorf("no running group, or a conv in no group shown:\n%s", text2)
	}
}

func TestViewRenderRollupEmptyCappedFull(t *testing.T) {
	bh := newViewHarness(t)
	roll := testView
	roll.Sources = []int64{testWork, 5}
	if text := bh.h.renderView(roll, bh.j.Convs()); text != "📋 **board** — #**fleet** #**5**\n\nNo topics." {
		t.Fatalf("empty = %q", text)
	}
	for i := range viewGroupMax + 3 {
		bh.conv(t, "d"+string(rune('a'+i)), journal.StateDone, journal.SetByAgent, "", time.Duration(i)*time.Minute)
	}
	if text := bh.h.renderView(testView, bh.j.Convs()); !strings.Contains(text, "- …and 3 more\n") {
		t.Fatalf("cap:\n%s", text)
	}
	long := strings.Repeat("x", journal.MaxReasonRunes)
	for _, st := range []string{journal.StateNeedsYou, journal.StateBlocked, journal.StateWaiting, journal.StateFYI} {
		for i := range viewGroupMax {
			bh.conv(t, st+strings.Repeat("y", i), st, journal.SetByAgent, long, time.Minute)
		}
	}
	text := bh.h.renderView(testView, bh.j.Convs())
	if !strings.HasSuffix(text, "…the view is full.") || len([]rune(text)) > viewMaxRunes+50 {
		t.Fatalf("full: %d runes, tail %q", len([]rune(text)), text[len(text)-40:])
	}
}

func TestViewDebounce(t *testing.T) {
	bh := newViewHarness(t)
	c := bh.conv(t, "t", "", "", "", 0)
	if _, err := bh.h.recordState(c, journal.TopicState{State: journal.StateBlocked, SetBy: journal.SetByUser}); err != nil {
		t.Fatal(err)
	}
	bh.h.viewsDirty()
	bh.h.viewsDirty()
	if len(bh.pending) != 1 || bh.delays[0] != DefaultViewDebounce {
		t.Fatalf("pending %d delays %v", len(bh.pending), bh.delays)
	}
	if got := bh.z.stored(); len(got) != 0 {
		t.Fatalf("posted before the debounce: %q", got)
	}
	bh.pending[0]()
	if !strings.Contains(bh.viewBody(t), "#**fleet>t** · 0m · blocked") {
		t.Fatalf("board = %q", bh.viewBody(t))
	}
	// Fired: the next change arms a new refresh.
	bh.h.viewsDirty()
	if len(bh.pending) != 2 {
		t.Fatalf("pending %d", len(bh.pending))
	}
}

func TestViewDirtyWithoutViews(t *testing.T) {
	var fired bool
	hh := newHarness(t, newAgent("ok"), func(c *Config) {
		c.ViewAfter = func(time.Duration, func()) { fired = true }
	})
	hh.h.viewsDirty()
	hh.h.RunViews(context.Background())
	if fired {
		t.Fatal("armed a refresh with no views")
	}
}

func TestViewDefaultAfter(t *testing.T) {
	done := make(chan struct{})
	hh := newHarness(t, newAgent("ok"), func(c *Config) {
		c.Views = []config.ResolvedView{testView}
		c.ViewDebounce = time.Nanosecond
		c.Logf = func(f string, _ ...any) {}
	})
	hh.z.sendHook = func(string) error { close(done); return nil }
	hh.h.viewsDirty()
	<-done
	// The send runs under viewMu: wait for the refresh to finish its
	// journal write before the temp dir is removed.
	hh.h.viewMu.Lock()
	hh.h.viewMu.Unlock() //nolint:staticcheck // a barrier, not a critical section
}

func TestViewPublishEditRepost(t *testing.T) {
	bh := newViewHarness(t)
	ctx := context.Background()
	bh.h.RefreshViews(ctx)
	first := bh.j.View(testViewKey).MsgID
	if first == 0 || bh.z.topics[first] != "board" {
		t.Fatalf("first board %d in %q", first, bh.z.topics[first])
	}

	// Newest and unchanged: no edit.
	bh.z.setHistory(zulipproto.Message{ID: first})
	edits := bh.z.editN
	bh.h.RefreshViews(ctx)
	if bh.z.editN != edits {
		t.Fatal("edited an unchanged board")
	}

	// Newest and changed: edited in place.
	bh.conv(t, "a", journal.StateBlocked, journal.SetByUser, "", 0)
	bh.h.RefreshViews(ctx)
	if bh.j.View(testViewKey).MsgID != first || !strings.Contains(bh.viewBody(t), "fleet>a") {
		t.Fatal("not edited in place")
	}

	// Edit refused: a new board, the old one deleted.
	bh.conv(t, "b", journal.StateBlocked, journal.SetByUser, "", 0)
	bh.z.editErrFor = first
	bh.h.RefreshViews(ctx)
	second := bh.j.View(testViewKey).MsgID
	if second == first || !strings.Contains(bh.viewBody(t), "fleet>b") {
		t.Fatal("not reposted")
	}
	if _, ok := bh.z.bodies[first]; ok {
		t.Fatal("old board kept")
	}

	// Not newest: reposted, and a failed delete is only logged.
	bh.z.setHistory(zulipproto.Message{ID: second + 50})
	bh.z.deleteErr = errors.New("nope")
	bh.h.RefreshViews(ctx)
	if bh.j.View(testViewKey).MsgID == second {
		t.Fatal("not reposted")
	}
	if !bh.logged("deleting old view:") {
		t.Fatal("delete failure not logged")
	}

	// A failed post keeps the old record.
	bh.z.deleteErr = nil
	bh.z.sendErr = errors.New("down")
	keep := bh.j.View(testViewKey).MsgID
	bh.h.RefreshViews(ctx)
	if bh.j.View(testViewKey).MsgID != keep || !bh.logged("posting view:") {
		t.Fatal("failed post")
	}
}

func TestViewNeedsYouNotices(t *testing.T) {
	bh := newViewHarness(t)
	ctx := context.Background()
	c := bh.conv(t, "q", journal.StateNeedsYou, journal.SetByAgent, "say yes", 0)
	bh.conv(t, "r", journal.StateNeedsYou, journal.SetByAgent, "", 0)
	bh.h.RefreshViews(ctx)
	notices := bh.j.View(testNoteKey).Notices
	id, ok := notices[c.ID]
	if len(notices) != 2 || !ok || bh.z.topics[id] != noticeTopic || bh.z.bodies[id] != "🔔 #**fleet>q** is needs_you — say yes" {
		t.Fatalf("notices %v body %q", notices, bh.z.bodies[id])
	}

	// Still needs you: no second notice.
	n := len(bh.z.stored())
	bh.h.RefreshViews(ctx)
	if len(bh.z.stored()) != n {
		t.Fatal("notice posted twice")
	}

	// Leaves needs_you: the notice is deleted.
	if _, err := bh.h.recordState(c, journal.TopicState{State: journal.StateDone, SetBy: journal.SetByUser}); err != nil {
		t.Fatal(err)
	}
	bh.h.RefreshViews(ctx)
	if _, ok := bh.z.bodies[id]; ok {
		t.Fatal("notice kept")
	}
	if len(bh.j.View(testNoteKey).Notices) != 1 {
		t.Fatalf("notices %v", bh.j.View(testNoteKey).Notices)
	}
}

func TestViewNoticeFailures(t *testing.T) {
	bh := newViewHarness(t)
	ctx := context.Background()
	c := bh.conv(t, "q", journal.StateNeedsYou, journal.SetByAgent, "", 0)
	bh.z.sendHook = func(content string) error {
		if strings.HasPrefix(content, "🔔") {
			return errors.New("refused")
		}
		return nil
	}
	bh.h.RefreshViews(ctx)
	if len(bh.j.View(testNoteKey).Notices) != 0 || !bh.logged("posting notify:") {
		t.Fatal("send failure")
	}
	bh.z.sendHook = nil
	bh.h.RefreshViews(ctx)
	if len(bh.j.View(testNoteKey).Notices) != 1 {
		t.Fatal("no notice")
	}
	if _, err := bh.h.recordState(c, journal.TopicState{State: journal.StateFYI, SetBy: journal.SetByUser}); err != nil {
		t.Fatal(err)
	}
	bh.z.deleteErr = errors.New("nope")
	bh.h.RefreshViews(ctx)
	if !bh.logged("deleting notify:") {
		t.Fatal("delete failure not logged")
	}
}

func TestViewJournalFailure(t *testing.T) {
	bh := newViewHarness(t)
	bh.breakJournal(t)
	bh.h.RefreshViews(context.Background())
	if !bh.logged("recording view:") {
		t.Fatal("journal failure not logged")
	}
}

func TestViewLoop(t *testing.T) {
	bh := newViewHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	tick := make(chan time.Time)
	done := make(chan struct{})
	go func() { bh.h.viewLoop(ctx, tick); close(done) }()
	tick <- viewNow // the start refresh has run once this is taken
	tick <- viewNow
	cancel()
	<-done
	if bh.j.View(testViewKey).MsgID == 0 {
		t.Fatal("no board")
	}
	// RunViews with a cancelled context refreshes once and returns.
	bh.h.RunViews(ctx)
}
