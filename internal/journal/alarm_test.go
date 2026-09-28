package journal

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAlarmsPersistAndShare(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.json")
	j, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	for id, msg := range map[string]int64{"a": 7, "b": 7, "c": 9} {
		if err := j.SetAlarm(id, msg); err != nil {
			t.Fatalf("SetAlarm: %v", err)
		}
	}
	// Survives a reopen: a reload must not orphan a reaction.
	j, err = Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if m, shared, ok := j.TakeAlarm("a"); !ok || !shared || m != 7 {
		t.Fatalf("take a = %d %v %v, want 7 shared", m, shared, ok)
	}
	if m, shared, ok := j.TakeAlarm("b"); !ok || shared || m != 7 {
		t.Fatalf("take b = %d %v %v, want 7 alone", m, shared, ok)
	}
	if _, _, ok := j.TakeAlarm("b"); ok {
		t.Fatal("a taken alarm came back")
	}
	j, _ = Open(path)
	if _, _, ok := j.TakeAlarm("a"); ok {
		t.Fatal("a taken alarm was not persisted as gone")
	}
	if m, _, ok := j.TakeAlarm("c"); !ok || m != 9 {
		t.Fatalf("take c = %d %v", m, ok)
	}
}

func TestSetAlarmRollsBackOnAWriteFailure(t *testing.T) {
	dir := t.TempDir()
	j, err := Open(filepath.Join(dir, "journal.json"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := j.SetAlarm("a", 5); err != nil {
		t.Fatalf("SetAlarm: %v", err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if err := j.SetAlarm("a", 6); err == nil {
		t.Fatal("want write error")
	}
	if err := j.SetAlarm("new", 6); err == nil {
		t.Fatal("want write error")
	}
	if m, _, ok := j.TakeAlarm("a"); !ok || m != 5 {
		t.Fatalf("a = %d %v after a failed write, want 5", m, ok)
	}
	if _, _, ok := j.TakeAlarm("new"); ok {
		t.Fatal("a failed SetAlarm left a record")
	}
}
