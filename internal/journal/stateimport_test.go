package journal

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const triage = `{
  "cB": {"topic": "b", "stream_id": 1, "state": "fyi", "reason": " read it ", "set_at": 1700000000},
  "cA": {"topic": "a", "stream_id": 1, "state": "needs_you", "set_at": "2026-01-02T03:04:05Z", "set_by": "triage"},
  "cC": {"topic": "c", "stream_id": 1, "state": "", "dup_of": "a"},
  "cD": {"topic": "d", "stream_id": 1, "state": "huh"},
  "cE": {"topic": "e", "stream_id": 1, "state": "done", "set_at": 1700000000123},
  "cF": {"topic": "f", "stream_id": 1, "state": "done", "set_at": "yesterday"}
}`

func TestParseStateImport(t *testing.T) {
	entries, skipped, err := ParseStateImport([]byte(triage))
	if err != nil {
		t.Fatal(err)
	}
	if len(skipped) != 1 || !strings.HasPrefix(skipped[0], "cD:") {
		t.Fatalf("skipped = %v", skipped)
	}
	got := map[string]TopicState{}
	for _, e := range entries {
		got[e.ConvID] = e.State
	}
	for id, want := range map[string]TopicState{
		"cA": {State: StateNeedsYou, SetAt: 1767323045000, SetBy: SetByImport},
		"cB": {State: StateFYI, Reason: "read it", SetAt: 1700000000000, SetBy: SetByImport},
		"cC": {State: StateDone, Reason: "dup of a", SetBy: SetByImport},
		"cE": {State: StateDone, SetAt: 1700000000123, SetBy: SetByImport},
		"cF": {State: StateDone, SetBy: SetByImport},
	} {
		if got[id] != want {
			t.Errorf("%s = %+v, want %+v", id, got[id], want)
		}
	}
	if entries[0].ConvID != "cA" || !entries[0].Key.Same(Channel(1, "a")) {
		t.Fatalf("first = %+v", entries[0])
	}
	if _, _, err := ParseStateImport([]byte("[")); err == nil {
		t.Fatal("want parse error")
	}
}

// TestStagedImportAppliesOnce: staging writes no journal; the next
// Open-time apply sets states and retires the staged file.
func TestStagedImportAppliesOnce(t *testing.T) {
	j, _ := tmpJournal(t)
	a, _ := j.Ensure(Channel(1, "a"))
	dir := filepath.Dir(j.path)

	if n, err := j.ApplyStagedImport(); n != 0 || err != nil {
		t.Fatalf("nothing staged: %d %v", n, err)
	}
	n, skipped, err := StageStateImport(dir, []byte(strings.Replace(triage, `"cA"`, `"`+a.ID+`"`, 1)))
	if err != nil || n != 5 || len(skipped) != 1 {
		t.Fatalf("stage: %d %v %v", n, skipped, err)
	}
	if c, _ := j.LookupID(a.ID); c.State != nil {
		t.Fatal("staging must not write the journal")
	}
	if n, err := j.ApplyStagedImport(); n != 1 || err != nil {
		t.Fatalf("apply: %d %v", n, err)
	}
	if c, _ := j.LookupID(a.ID); c.State == nil || c.State.State != StateNeedsYou {
		t.Fatalf("state = %+v", c.State)
	}
	if _, err := os.Stat(filepath.Join(dir, StagedImportName+".applied")); err != nil {
		t.Fatal(err)
	}
	if n, err := j.ApplyStagedImport(); n != 0 || err != nil {
		t.Fatalf("second apply: %d %v", n, err)
	}
}

func TestStageStateImportRefusals(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := StageStateImport(dir, []byte("{")); err == nil {
		t.Fatal("want parse error")
	}
	if _, _, err := StageStateImport(dir, []byte(`{"x":{"state":"nope"}}`)); err == nil {
		t.Fatal("want no-valid-entry error")
	}
	file := filepath.Join(dir, "file")
	_ = os.WriteFile(file, nil, 0o600)
	if _, _, err := StageStateImport(filepath.Join(file, "sub"), []byte(triage)); err == nil {
		t.Fatal("want mkdir error")
	}
	ro := filepath.Join(dir, "ro")
	_ = os.Mkdir(ro, 0o500)
	t.Cleanup(func() { _ = os.Chmod(ro, 0o700) })
	if _, _, err := StageStateImport(ro, []byte(triage)); err == nil {
		t.Fatal("want write error")
	}
	// The rename fails when the target is a directory.
	busy := filepath.Join(dir, "busy")
	_ = os.MkdirAll(filepath.Join(busy, StagedImportName, "x"), 0o755)
	if _, _, err := StageStateImport(busy, []byte(triage)); err == nil {
		t.Fatal("want rename error")
	}
}

func TestApplyStagedImportFailures(t *testing.T) {
	j, _ := tmpJournal(t)
	a, _ := j.Ensure(Channel(1, "a"))
	dir := filepath.Dir(j.path)
	staged := filepath.Join(dir, StagedImportName)

	// Unreadable: a directory.
	_ = os.Mkdir(staged, 0o755)
	if _, err := j.ApplyStagedImport(); err == nil {
		t.Fatal("want read error")
	}
	_ = os.Remove(staged)
	_ = os.WriteFile(staged, []byte("{"), 0o600)
	if _, err := j.ApplyStagedImport(); err == nil {
		t.Fatal("want parse error")
	}
	body := `{"` + a.ID + `":{"state":"done"}}`
	_ = os.WriteFile(staged, []byte(body), 0o600)
	// The journal cannot be written.
	readOnlyDir(t, j)
	if _, err := j.ApplyStagedImport(); err == nil {
		t.Fatal("want journal write error")
	}
	_ = os.Chmod(dir, 0o700)
	// The rename fails: the target is a non-empty directory.
	_ = os.MkdirAll(filepath.Join(staged+".applied", "x"), 0o755)
	if n, err := j.ApplyStagedImport(); n != 1 || err == nil {
		t.Fatalf("want applied with a rename error: %d %v", n, err)
	}
}
