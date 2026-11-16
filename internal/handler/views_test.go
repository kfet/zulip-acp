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

	// Not newest: still edited in place — one long-lived message.
	bh.z.setHistory(zulipproto.Message{ID: second + 50})
	bh.conv(t, "c", journal.StateBlocked, journal.SetByUser, "", 0)
	bh.h.RefreshViews(ctx)
	if bh.j.View(testViewKey).MsgID != second || !strings.Contains(bh.viewBody(t), "fleet>c") {
		t.Fatal("not edited in place when not newest")
	}

	// Edit refused and delete refused (Zulip's time limit): a new
	// board is posted, and the old one, which cannot be edited either,
	// is logged once and forgotten.
	bh.conv(t, "d", journal.StateBlocked, journal.SetByUser, "", 0)
	bh.z.editErrFor = second
	bh.z.deleteErr = errors.New("The time limit for deleting this message has passed")
	bh.h.RefreshViews(ctx)
	if bh.j.View(testViewKey).MsgID == second {
		t.Fatal("not reposted")
	}
	bh.z.editErrFor = 0
	if !bh.logged("dropping old view:") {
		t.Fatal("the double failure was not logged")
	}

	// A failed post keeps the old record.
	bh.z.deleteErr = nil
	bh.z.sendErr = errors.New("down")
	keep := bh.j.View(testViewKey).MsgID
	bh.z.editErrFor = keep
	bh.conv(t, "e", journal.StateBlocked, journal.SetByUser, "", 0)
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
	id := bh.j.View(testNoteKey).Notices[c.ID]
	bh.z.deleteErr = errors.New("The time limit for deleting this message has passed")
	bh.h.RefreshViews(ctx)
	if b := bh.z.bodies[id]; b != "✔ ~~notice~~ — handled" {
		t.Fatalf("notice not struck: %q", b)
	}
	if len(bh.j.View(testNoteKey).Notices) != 0 {
		t.Fatal("a dropped notice is still tracked")
	}

	// Delete and edit both refused: logged once, then forgotten.
	if _, err := bh.h.recordState(c, journal.TopicState{State: journal.StateNeedsYou, SetBy: journal.SetByUser}); err != nil {
		t.Fatal(err)
	}
	bh.h.RefreshViews(ctx)
	id = bh.j.View(testNoteKey).Notices[c.ID]
	if _, err := bh.h.recordState(c, journal.TopicState{State: journal.StateFYI, SetBy: journal.SetByUser}); err != nil {
		t.Fatal(err)
	}
	bh.z.editErrFor = id
	bh.h.RefreshViews(ctx)
	bh.h.RefreshViews(ctx)
	if n := bh.logCount("dropping notify:"); n != 1 {
		t.Fatalf("logged %d times, want once", n)
	}
	if len(bh.j.View(testNoteKey).Notices) != 0 {
		t.Fatal("a failed notice is still tracked")
	}
}

// TestViewDropsArchivedTopic: an archived topic, and one a human moved
// to a channel the relay does not serve, leave the board and lose their
// notice. Before the fix the key got a fresh conversation that kept the
// topic state, so the board still listed the topic under needs_you.
func TestViewDropsArchivedTopic(t *testing.T) {
	bh := newViewHarness(t)
	ctx := context.Background()
	for _, gone := range []func(c journal.Conv){
		func(c journal.Conv) { bh.h.cfg.ArchiveStreamID = 77; bh.h.archiveConversation(ctx, c, "kfet", "") },
		func(c journal.Conv) { bh.h.retireMoved(c.Key, 77) },
	} {
		c := bh.conv(t, "q", journal.StateNeedsYou, journal.SetByAgent, "", 0)
		bh.h.cfg.ArchiveStreamID = 0
		bh.h.RefreshViews(ctx)
		if !strings.Contains(bh.viewBody(t), "fleet>q") || len(bh.j.View(testNoteKey).Notices) != 1 {
			t.Fatal("topic not listed")
		}
		armed := len(bh.pending)
		gone(c)
		if len(bh.pending) <= armed && !bh.h.viewArmed.Load() {
			t.Fatal("no refresh armed")
		}
		bh.h.RefreshViews(ctx)
		if strings.Contains(bh.viewBody(t), "fleet>q") {
			t.Fatalf("archived topic still listed:\n%s", bh.viewBody(t))
		}
		if len(bh.j.View(testNoteKey).Notices) != 0 {
			t.Fatal("notice kept")
		}
		if _, ok := bh.j.Lookup(c.Key); ok {
			t.Fatal("a replacement conversation was minted")
		}
		bh.h.viewArmed.Store(false)
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

// TestViewDropStaleBoards: at start, every old board the relay left in
// the board topic is struck; the current one and other messages stay.
func TestViewDropStaleBoards(t *testing.T) {
	bh := newViewHarness(t)
	ctx := context.Background()
	bh.h.RefreshViews(ctx)
	cur := bh.j.View(testViewKey).MsgID
	old, _ := bh.z.SendMessage(ctx, testBoard, "board", "📋 **board** — old")
	other, _ := bh.z.SendMessage(ctx, testBoard, "board", "hello")
	bh.z.deleteErr = errors.New("The time limit for deleting this message has passed")
	bh.z.setHistory(zulipproto.Message{ID: old, Content: "📋 **board** — old"}, zulipproto.Message{ID: other, Content: "hello"}, zulipproto.Message{ID: cur, Content: "📋 **board** — now"})
	bh.h.dropStaleViews(ctx)
	if b := bh.z.bodies[old]; b != "~~board~~ — moved to a newer message below" {
		t.Fatalf("old board %q", b)
	}
	if bh.z.bodies[other] != "hello" || !strings.HasPrefix(bh.z.bodies[cur], "📋") {
		t.Fatal("touched a message it should not")
	}
	bh.z.historyErr = errors.New("down")
	bh.h.dropStaleViews(ctx)
	if !bh.logged("reading old view:") || !bh.logged("reading old notify:") {
		t.Fatal("read failure not logged")
	}
}

// TestViewDropStaleNotices: at start, a notice no notify entry tracks
// (an older release forgot it when the delete was refused) is struck;
// a tracked notice stays.
func TestViewDropStaleNotices(t *testing.T) {
	bh := newViewHarness(t)
	ctx := context.Background()
	c := bh.conv(t, "q", journal.StateNeedsYou, journal.SetByAgent, "", 0)
	bh.h.RefreshViews(ctx)
	live := bh.j.View(testNoteKey).Notices[c.ID]
	lost, _ := bh.z.SendMessage(ctx, testBoard, noticeTopic, "🔔 #**fleet>gone** is needs_you")
	bh.z.deleteErr = errors.New("The time limit for deleting this message has passed")
	bh.z.setHistory(zulipproto.Message{ID: lost, Content: "🔔 #**fleet>gone** is needs_you"}, zulipproto.Message{ID: live, Content: bh.z.bodies[live]})
	bh.h.dropStaleViews(ctx)
	if b := bh.z.bodies[lost]; b != noticeGone {
		t.Fatalf("lost notice %q", b)
	}
	if !strings.HasPrefix(bh.z.bodies[live], noticeMark) {
		t.Fatal("a tracked notice was struck")
	}
}

// TestViewRepairGone: a live successor that an older release minted
// for an archived topic is retired at start when its topic is empty,
// and kept while the topic still holds messages or cannot be read.
func TestViewRepairGone(t *testing.T) {
	bh := newViewHarness(t)
	ctx := context.Background()
	c := bh.conv(t, "q", journal.StateNeedsYou, journal.SetByAgent, "", 0)
	bh.conv(t, "lone", journal.StateNeedsYou, journal.SetByAgent, "", 0)
	if _, _, _, err := bh.j.Retire(c.Key); err != nil { // the old release's mint
		t.Fatal(err)
	}
	succ, _ := bh.j.Lookup(c.Key)
	bh.h.RefreshViews(ctx)
	if _, ok := bh.j.View(testNoteKey).Notices[succ.ID]; !ok {
		t.Fatal("no notice for the successor")
	}

	bh.z.oldestErr = errors.New("down")
	bh.h.repairGone(ctx)
	bh.z.oldestErr = nil
	bh.z.oldest = zulipproto.Message{ID: 1}
	bh.h.repairGone(ctx)
	if _, ok := bh.j.Lookup(c.Key); !ok {
		t.Fatal("retired on no evidence")
	}

	bh.z.oldest = zulipproto.Message{}
	calls := bh.z.oldestCalls()
	bh.h.repairGone(ctx)
	if _, ok := bh.j.Lookup(c.Key); ok {
		t.Fatal("successor not retired")
	}
	if got, _ := bh.j.LookupID(succ.ID); !got.Retired {
		t.Fatal("successor not marked retired")
	}
	if bh.z.oldestCalls()-calls != 2 || !bh.logged("repaired "+succ.ID) {
		t.Fatal("a topic without a retired twin was checked, or no log")
	}
	if _, ok := bh.j.Lookup(journal.Channel(testWork, "lone")); !ok {
		t.Fatal("a normal conversation was retired")
	}
	bh.h.RefreshViews(ctx)
	if strings.Contains(bh.viewBody(t), "fleet>q") {
		t.Fatal("the board still lists the archived topic")
	}
	if _, ok := bh.j.View(testNoteKey).Notices[succ.ID]; ok {
		t.Fatal("the repaired topic kept its notice")
	}

	// A failed journal write is logged.
	c = bh.conv(t, "r", journal.StateNeedsYou, journal.SetByAgent, "", 0)
	if _, _, _, err := bh.j.Retire(c.Key); err != nil {
		t.Fatal(err)
	}
	bh.breakJournal(t)
	bh.h.repairGone(ctx)
	if !bh.logged("repairing ") {
		t.Fatal("retire failure not logged")
	}
}

// logCount is how many log lines contain sub.
func (hh *harness) logCount(sub string) int {
	hh.logMu.Lock()
	defer hh.logMu.Unlock()
	n := 0
	for _, l := range hh.logs {
		if strings.Contains(l, sub) {
			n++
		}
	}
	return n
}
