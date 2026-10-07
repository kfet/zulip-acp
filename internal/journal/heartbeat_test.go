package journal

import (
	"os"
	"path/filepath"
	"testing"
)

func TestHeartbeatPersistsAndDrops(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.json")
	j, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := j.Heartbeat("s1"); ok {
		t.Fatal("want no record")
	}
	if err := j.SetHeartbeat("s1", Heartbeat{MsgID: 7, Count: 2}); err != nil {
		t.Fatal(err)
	}
	j2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if hb, ok := j2.Heartbeat("s1"); !ok || hb != (Heartbeat{MsgID: 7, Count: 2}) {
		t.Fatalf("reloaded = %+v %v", hb, ok)
	}
	j2.DropHeartbeat("s1")
	j2.DropHeartbeat("s1") // no record: no-op
	j3, _ := Open(path)
	if _, ok := j3.Heartbeat("s1"); ok {
		t.Fatal("dropped record survived a reload")
	}
}

func TestSetHeartbeatRollsBackOnAWriteFailure(t *testing.T) {
	dir := t.TempDir()
	j, err := Open(filepath.Join(dir, "journal.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := j.SetHeartbeat("old", Heartbeat{MsgID: 1, Count: 1}); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if err := j.SetHeartbeat("old", Heartbeat{MsgID: 2, Count: 2}); err == nil {
		t.Fatal("want write error")
	}
	if hb, _ := j.Heartbeat("old"); hb.MsgID != 1 {
		t.Fatalf("hb = %+v, want previous", hb)
	}
	if err := j.SetHeartbeat("new", Heartbeat{MsgID: 3}); err == nil {
		t.Fatal("want write error")
	}
	if _, ok := j.Heartbeat("new"); ok {
		t.Fatal("failed write left a record")
	}
}
