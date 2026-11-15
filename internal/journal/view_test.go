package journal

import (
	"os"
	"path/filepath"
	"testing"
)

func TestViewRecordPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.json")
	j, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if r := j.View("view:a/b"); r.MsgID != 0 || r.Notices != nil {
		t.Fatalf("want empty, got %+v", r)
	}
	notes := map[string]int64{"c1": 5}
	if err := j.SetView("view:a/b", ViewRecord{MsgID: 7, Notices: notes}); err != nil {
		t.Fatal(err)
	}
	notes["c2"] = 6 // the record holds a copy
	j2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	r := j2.View("view:a/b")
	if r.MsgID != 7 || len(r.Notices) != 1 || r.Notices["c1"] != 5 {
		t.Fatalf("reloaded = %+v", r)
	}
	r.Notices["c3"] = 1 // the caller gets a copy
	if len(j2.View("view:a/b").Notices) != 1 {
		t.Fatal("caller changed the record")
	}
}

func TestSetViewRollsBackOnAWriteFailure(t *testing.T) {
	dir := t.TempDir()
	j, err := Open(filepath.Join(dir, "journal.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := j.SetView("old", ViewRecord{MsgID: 1}); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if err := j.SetView("old", ViewRecord{MsgID: 2}); err == nil {
		t.Fatal("want write error")
	}
	if j.View("old").MsgID != 1 {
		t.Fatal("not rolled back")
	}
	if err := j.SetView("new", ViewRecord{MsgID: 3}); err == nil {
		t.Fatal("want write error")
	}
	if j.View("new").MsgID != 0 {
		t.Fatal("new record kept")
	}
}
