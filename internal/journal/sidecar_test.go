package journal

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/kfet/acp-kit/state"
)

// readSidecar returns the sidecar of id under the journal at path.
func readSidecar(t *testing.T, path, id string) (sidecar, bool) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(filepath.Dir(path), "convs", id, state.MetaFile))
	if os.IsNotExist(err) {
		return sidecar{}, false
	}
	if err != nil {
		t.Fatal(err)
	}
	var s sidecar
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	return s, true
}

func writeSidecar(t *testing.T, path string, s sidecar) {
	t.Helper()
	b, _ := json.Marshal(s)
	if err := state.WriteMeta(filepath.Dir(path), s.ID, b); err != nil {
		t.Fatal(err)
	}
}

func fixClock(t *testing.T, ms ...int64) {
	t.Helper()
	old := nowMillis
	i := 0
	nowMillis = func() int64 { v := ms[i%len(ms)]; i++; return v }
	t.Cleanup(func() { nowMillis = old })
}

func TestSidecarFollowsEveryIdentityChange(t *testing.T) {
	fixClock(t, 100, 200, 300)
	j, path := tmpJournal(t)

	a, err := j.Ensure(Channel(4, "alpha"))
	if err != nil {
		t.Fatal(err)
	}
	s, ok := readSidecar(t, path, a.ID)
	if !ok || s.ID != a.ID || s.StreamID != 4 || s.Topic != "alpha" || s.Created != 100 || s.Retired || s.Version != currentVersion {
		t.Fatalf("ensure: %+v %v", s, ok)
	}

	// Branch records the parent.
	b, err := j.Branch(Channel(4, "beta"), Parent{Key: Channel(4, "alpha"), MessageID: 9})
	if err != nil {
		t.Fatal(err)
	}
	s, _ = readSidecar(t, path, b.ID)
	if s.Parent == nil || s.Parent.Topic != "alpha" || s.Parent.MessageID != 9 || s.Created != 200 {
		t.Fatalf("branch: %+v", s)
	}

	// Rename moves the key, and the child's parent pointer follows.
	if _, ok, err := j.Rename(4, "alpha", "gamma"); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if s, _ = readSidecar(t, path, a.ID); s.Topic != "gamma" {
		t.Fatalf("rename: %+v", s)
	}
	if s, _ = readSidecar(t, path, b.ID); s.Parent.Topic != "gamma" {
		t.Fatalf("child: %+v", s.Parent)
	}

	// A resolve changes only the topic string.
	if _, ok, err := j.Rename(4, "gamma", ResolvedPrefix+"gamma"); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if s, _ = readSidecar(t, path, a.ID); s.Topic != ResolvedPrefix+"gamma" {
		t.Fatalf("resolve: %+v", s)
	}

	// Retire marks the old one and writes the fresh one.
	prev, fresh, _, err := j.Retire(Channel(4, "beta"))
	if err != nil {
		t.Fatal(err)
	}
	if s, _ = readSidecar(t, path, prev.ID); !s.Retired {
		t.Fatalf("retired: %+v", s)
	}
	if s, _ = readSidecar(t, path, fresh.ID); s.Retired || s.Parent == nil || s.Created != 300 {
		t.Fatalf("fresh: %+v", s)
	}

	// A clashing rename retires the source.
	if _, ok, err := j.Rename(4, ResolvedPrefix+"gamma", "beta"); err != nil || ok {
		t.Fatal(ok, err)
	}
	if s, _ = readSidecar(t, path, a.ID); !s.Retired {
		t.Fatalf("clash: %+v", s)
	}
}

// TestRebuildFromSidecars is the restore case end to end: the journal
// is lost, and the next start answers every topic with its old id.
func TestRebuildFromSidecars(t *testing.T) {
	j, path := tmpJournal(t)
	a, _ := j.Ensure(Channel(4, "alpha"))
	d, _ := j.Ensure(DM([]int64{9, 2}))
	b, _ := j.Branch(Channel(4, "beta"), Parent{Key: Channel(4, "alpha"), MessageID: 7})
	old, fresh, _, _ := j.Retire(Channel(4, "alpha"))
	if old.ID != a.ID {
		t.Fatal(old.ID)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	j2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]Key{fresh.ID: Channel(4, "alpha"), d.ID: DM([]int64{2, 9}), b.ID: Channel(4, "beta")} {
		c, ok := j2.Lookup(want)
		if !ok || c.ID != k {
			t.Fatalf("%s: got %+v %v, want %s", want.Label(), c, ok, k)
		}
	}
	if c, _ := j2.Lookup(Channel(4, "beta")); c.Parent == nil || c.Parent.MessageID != 7 {
		t.Fatalf("parent lost: %+v", c)
	}
	if c, ok := j2.LookupID(a.ID); !ok || !c.Retired {
		t.Fatalf("retired lost: %+v", c)
	}
	// The rebuilt journal is on disk: a third start adopts nothing.
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	j3, _ := Open(path)
	if len(j3.Convs()) != 4 {
		t.Fatal(j3.Convs())
	}
}

func TestAdoptPartialAndConflicts(t *testing.T) {
	j, path := tmpJournal(t)
	kept, _ := j.Ensure(Channel(4, "kept"))
	// Sidecars the journal does not know:
	writeSidecar(t, path, sidecar{ID: "cmissing", Key: Channel(4, "missing"), Created: 5})
	// claims a key the journal holds: the journal wins.
	writeSidecar(t, path, sidecar{ID: "cclash", Key: Channel(4, "kept"), Created: 99})
	// two claim one free key: newest created wins, then higher id.
	writeSidecar(t, path, sidecar{ID: "cold", Key: Channel(4, "dup"), Created: 1})
	writeSidecar(t, path, sidecar{ID: "cnew", Key: Channel(4, "dup"), Created: 2})
	writeSidecar(t, path, sidecar{ID: "ctie1", Key: DM([]int64{1, 2}), Created: 3})
	writeSidecar(t, path, sidecar{ID: "ctie2", Key: Key{UserIDs: []int64{2, 1}}, Created: 3})
	// retired stays retired, even on a free key.
	writeSidecar(t, path, sidecar{ID: "cdead", Key: Channel(4, "dead"), Retired: true})
	// a parent with no branch point is dropped, as Open drops one.
	writeSidecar(t, path, sidecar{ID: "cnop", Key: Channel(4, "nop"), Parent: &Parent{Key: Channel(4, "kept")}})
	writeSidecar(t, path, sidecar{ID: "cpar", Key: Channel(4, "par"), Parent: &Parent{Key: Key{UserIDs: []int64{2, 1}}, MessageID: 3}})
	// junk is ignored.
	if err := state.WriteMeta(filepath.Dir(path), "cjunk", []byte("{")); err != nil {
		t.Fatal(err)
	}
	writeSidecar(t, path, sidecar{ID: "cother", Key: Channel(4, "x")})
	if err := os.Rename(filepath.Join(filepath.Dir(path), "convs", "cother"), filepath.Join(filepath.Dir(path), "convs", "cwrongdir")); err != nil {
		t.Fatal(err)
	}
	// Edit the kept entry in the file: adoption must not override it.
	_ = kept

	j2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	look := func(k Key) string { c, _ := j2.Lookup(k); return c.ID }
	if look(Channel(4, "missing")) != "cmissing" || look(Channel(4, "kept")) != kept.ID ||
		look(Channel(4, "dup")) != "cnew" || look(DM([]int64{1, 2})) != "ctie2" || look(Channel(4, "dead")) != "" {
		t.Fatalf("convs: %+v", j2.Convs())
	}
	for _, id := range []string{"cclash", "cold", "ctie1", "cdead"} {
		if c, ok := j2.LookupID(id); !ok || !c.Retired {
			t.Fatalf("%s: %+v %v", id, c, ok)
		}
	}
	if c, _ := j2.LookupID("cnop"); c.Parent != nil {
		t.Fatalf("parent kept: %+v", c.Parent)
	}
	if c, _ := j2.LookupID("cpar"); c.Parent == nil || c.Parent.UserIDs[0] != 1 {
		t.Fatalf("parent: %+v", c.Parent)
	}
	for _, id := range []string{"cjunk", "cwrongdir", "cother"} {
		if _, ok := j2.LookupID(id); ok {
			t.Fatalf("%s adopted", id)
		}
	}
	// The losers' sidecars now say retired too.
	if s, _ := readSidecar(t, path, "cold"); !s.Retired {
		t.Fatalf("sidecar not synced: %+v", s)
	}
}

func TestBackfillWritesMissingAndStaleSidecars(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	path := filepath.Join(dir, "journal.json")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A journal from before sidecars, with one entry that Open's
	// dedupe will retire.
	body := `{"version":1,"convs":[{"id":"ca","stream_id":4,"topic":"a","last_own_id":5},{"id":"cb","stream_id":4,"topic":"✔ a","last_own_id":3}]}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	// cb has a stale sidecar that still says live.
	writeSidecar(t, path, sidecar{Version: 1, ID: "cb", Key: Channel(4, "✔ a")})
	if _, err := Open(path); err != nil {
		t.Fatal(err)
	}
	if s, ok := readSidecar(t, path, "ca"); !ok || s.Topic != "a" || s.Retired {
		t.Fatalf("ca: %+v %v", s, ok)
	}
	if s, _ := readSidecar(t, path, "cb"); !s.Retired {
		t.Fatalf("cb: %+v", s)
	}
}

func TestSidecarFailuresDoNotFailTheJournal(t *testing.T) {
	j, path := tmpJournal(t)
	convs := filepath.Join(filepath.Dir(path), "convs")
	// convs is a file: every sidecar write and the scan fail.
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(convs, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := j.Ensure(Channel(4, "a"))
	if err != nil {
		t.Fatal(err)
	}
	j2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := j2.Lookup(Channel(4, "a")); got.ID != c.ID {
		t.Fatal(got)
	}
}

func TestAdoptSaveFailureKeepsAdopted(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	path := filepath.Join(dir, "journal.json")
	writeSidecar(t, path, sidecar{ID: "ca", Key: Channel(4, "a")})
	// journal.json.tmp is a dir: the save of the adopted set fails.
	if err := os.MkdirAll(path+".tmp", 0o755); err != nil {
		t.Fatal(err)
	}
	j, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if c, ok := j.Lookup(Channel(4, "a")); !ok || c.ID != "ca" {
		t.Fatal(c, ok)
	}
}
