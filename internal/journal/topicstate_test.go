package journal

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func stateClock(t *testing.T, ms int64) *int64 {
	t.Helper()
	now := ms
	prev := nowMillis
	nowMillis = func() int64 { return now }
	t.Cleanup(func() { nowMillis = prev })
	return &now
}

func TestSetStatePersistsAndIsChangeOnly(t *testing.T) {
	now := stateClock(t, 1000)
	j, path := tmpJournal(t)
	c, _ := j.Ensure(Channel(1, "t"))

	if _, _, err := j.SetState(c.ID, TopicState{State: "bogus"}); err == nil {
		t.Fatal("want invalid-state error")
	}
	if _, _, err := j.SetState("nope", TopicState{State: StateDone}); err == nil {
		t.Fatal("want unknown-conversation error")
	}
	if err := j.NoteActivity(c.ID, true); err != nil {
		t.Fatal(err)
	}
	prev, changed, err := j.SetState(c.ID, TopicState{State: StateNeedsYou, Reason: "pick A or B\nsecond line", SetBy: SetByAgent})
	if err != nil || !changed || prev.State != "" {
		t.Fatalf("first set: prev=%+v changed=%v err=%v", prev, changed, err)
	}
	*now = 2000
	prev, changed, err = j.SetState(c.ID, TopicState{State: StateNeedsYou, Reason: "pick A or B", SetBy: SetByAgent})
	if err != nil || changed || prev.State != StateNeedsYou {
		t.Fatalf("same state: prev=%+v changed=%v err=%v", prev, changed, err)
	}
	for range 3 {
		if err := j.NoteActivity(c.ID, true); err != nil {
			t.Fatal(err)
		}
	}
	if err := j.NoteActivity(c.ID, false); err != nil {
		t.Fatal(err)
	}
	if err := j.NoteActivity("nope", false); err == nil {
		t.Fatal("want unknown-conversation error")
	}

	j2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := j2.LookupID(c.ID)
	want := TopicState{State: StateNeedsYou, Reason: "pick A or B", SetAt: 1000, SetBy: SetByAgent}
	if got.State == nil || *got.State != want {
		t.Fatalf("state after reopen = %+v, want %+v", got.State, want)
	}
	if got.ActivitySince != 3 || got.LastActivity != 2000 {
		t.Fatalf("since=%d last=%d, want 3 and 2000", got.ActivitySince, got.LastActivity)
	}

	// A new state resets the count.
	if _, changed, _ := j2.SetState(c.ID, TopicState{State: StateDone, SetBy: SetByUser}); !changed {
		t.Fatal("want changed")
	}
	if got, _ := j2.LookupID(c.ID); got.ActivitySince != 0 || got.State.SetAt != 2000 {
		t.Fatalf("after change: %+v", got)
	}
}

func TestSetStateClampsReason(t *testing.T) {
	j, _ := tmpJournal(t)
	c, _ := j.Ensure(Channel(1, "t"))
	long := strings.Repeat("é", MaxReasonRunes+10)
	if _, _, err := j.SetState(c.ID, TopicState{State: StateFYI, Reason: long}); err != nil {
		t.Fatal(err)
	}
	got, _ := j.LookupID(c.ID)
	if r := []rune(got.State.Reason); len(r) != MaxReasonRunes || r[len(r)-1] != '…' {
		t.Fatalf("reason has %d runes", len(r))
	}
}

func TestStateSurvivesNew(t *testing.T) {
	stateClock(t, 5)
	j, _ := tmpJournal(t)
	k := Channel(1, "t")
	c, _ := j.Ensure(k)
	_, _, _ = j.SetState(c.ID, TopicState{State: StateWaiting, Reason: "build"})
	_ = j.NoteActivity(c.ID, true)
	_, fresh, _, err := j.Retire(k)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.State == nil || fresh.State.State != StateWaiting || fresh.ActivitySince != 1 || fresh.LastActivity != 5 {
		t.Fatalf("fresh = %+v", fresh)
	}
}

func readOnlyDir(t *testing.T, j *Journal) {
	t.Helper()
	dir := filepath.Dir(j.path)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
}

func TestStateWritesUndoOnFailure(t *testing.T) {
	j, _ := tmpJournal(t)
	c, _ := j.Ensure(Channel(1, "t"))
	_ = j.NoteActivity(c.ID, true)
	readOnlyDir(t, j)
	if _, _, err := j.SetState(c.ID, TopicState{State: StateDone}); err == nil {
		t.Fatal("want write error")
	}
	if err := j.NoteActivity(c.ID, true); err == nil {
		t.Fatal("want write error")
	}
	if _, err := j.ImportStates([]StateImport{{ConvID: c.ID, State: TopicState{State: StateDone}}}); err == nil {
		t.Fatal("want write error")
	}
	got, _ := j.LookupID(c.ID)
	if got.State != nil || got.ActivitySince != 1 {
		t.Fatalf("after failed writes: %+v", got)
	}
}

func TestImportStates(t *testing.T) {
	stateClock(t, 7)
	j, path := tmpJournal(t)
	a, _ := j.Ensure(Channel(1, "a"))
	b, _ := j.Ensure(Channel(1, "b"))
	_, _, _ = j.SetState(b.ID, TopicState{State: StateNeedsYou, SetBy: SetByAgent})
	d, _ := j.Ensure(Channel(1, "d"))
	_ = j.NoteActivity(d.ID, true)
	// "e" was replaced by `!new`: the import names the retired id.
	e, _ := j.Ensure(Channel(1, "e"))
	_, eNew, _, _ := j.Retire(Channel(1, "e"))

	n, err := j.ImportStates([]StateImport{
		{ConvID: a.ID, State: TopicState{State: StateFYI, Reason: "x", SetAt: 3, SetBy: SetByAgent}},
		{ConvID: b.ID, State: TopicState{State: StateDone}},                         // has a state: kept
		{ConvID: "gone", Key: Channel(1, "d"), State: TopicState{State: StateDone}}, // found by key
		{ConvID: e.ID, Key: Channel(1, "e"), State: TopicState{State: StateBlocked}},
		{ConvID: a.ID, State: TopicState{State: StateDone}},                         // second entry for a: kept first
		{ConvID: "gone", Key: Channel(9, "z"), State: TopicState{State: StateDone}}, // unknown
		{ConvID: a.ID, State: TopicState{State: "bogus"}},
	})
	if err != nil || n != 3 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	j2, _ := Open(path)
	for id, want := range map[string]TopicState{
		a.ID:    {State: StateFYI, Reason: "x", SetAt: 3, SetBy: SetByImport},
		b.ID:    {State: StateNeedsYou, SetAt: 7, SetBy: SetByAgent},
		d.ID:    {State: StateDone, SetAt: 7, SetBy: SetByImport},
		eNew.ID: {State: StateBlocked, SetAt: 7, SetBy: SetByImport},
	} {
		got, _ := j2.LookupID(id)
		if got.State == nil || *got.State != want {
			t.Errorf("%s: %+v, want %+v", id, got.State, want)
		}
	}
	if got, _ := j2.LookupID(d.ID); got.ActivitySince != 0 {
		t.Errorf("import must reset the count, got %d", got.ActivitySince)
	}
	if n, err := j.ImportStates(nil); n != 0 || err != nil {
		t.Fatalf("empty import: %d %v", n, err)
	}
	if !ValidState(StateDone) || ValidState("x") {
		t.Fatal("ValidState")
	}
}

func TestReasonMentionsAreSilenced(t *testing.T) {
	if got := clampReason("ping @**all** and @*admins* and @_**Kfet**"); got != "ping @_**all** and @_*admins* and @_**Kfet**" {
		t.Fatalf("= %q", got)
	}
}
