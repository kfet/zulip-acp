package journal

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func tmpJournal(t *testing.T) (*Journal, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state", "journal.json")
	j, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return j, path
}

func TestEnsureIsStableAndPersisted(t *testing.T) {
	j, path := tmpJournal(t)
	c1, err := j.Ensure(4, "session: fix the parser")
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if c1.ID == "" || c1.StreamID != 4 || c1.Topic != "session: fix the parser" {
		t.Fatalf("conv = %+v", c1)
	}
	c2, err := j.Ensure(4, "session: fix the parser")
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if c2.ID != c1.ID {
		t.Fatalf("conv-id not stable: %q vs %q", c1.ID, c2.ID)
	}
	// The same topic string in a DIFFERENT channel is a different
	// conversation — the key is (stream_id, topic), not topic alone.
	c3, err := j.Ensure(5, "session: fix the parser")
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if c3.ID == c1.ID {
		t.Fatal("same topic in two channels collapsed into one conversation")
	}

	// Reload from disk.
	j2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	got, ok := j2.Lookup(4, "session: fix the parser")
	if !ok || got.ID != c1.ID {
		t.Fatalf("after reload: %+v ok=%v", got, ok)
	}
	if _, ok := j2.Lookup(4, "unknown topic"); ok {
		t.Fatal("unknown topic must not resolve")
	}
	if n := len(j2.Convs()); n != 2 {
		t.Fatalf("Convs = %d", n)
	}
}

// TestConvIDIsSafePathComponent: the conv-id is used verbatim as an
// acp-kit state directory name, so it must never contain anything
// path-ish, whatever the topic looked like.
func TestConvIDIsSafePathComponent(t *testing.T) {
	j, _ := tmpJournal(t)
	for _, topic := range []string{"../../etc/passwd", "a/b/c", ".hidden", "", "emoji 🙂 topic", strings.Repeat("x", 500)} {
		c, err := j.Ensure(4, topic)
		if err != nil {
			t.Fatalf("Ensure(%q): %v", topic, err)
		}
		if strings.ContainsAny(c.ID, `/\.`) || c.ID == "" || len(c.ID) != 13 {
			t.Fatalf("unsafe conv-id %q from topic %q", c.ID, topic)
		}
	}
}

// TestRenameKeepsConvID is the whole reason the package exists.
func TestRenameKeepsConvID(t *testing.T) {
	j, path := tmpJournal(t)
	orig, err := j.Ensure(4, "untitled")
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if err := j.SetTail(orig.ID, 99); err != nil {
		t.Fatalf("SetTail: %v", err)
	}
	got, ok, err := j.Rename(4, "untitled", "session: real name")
	if err != nil || !ok {
		t.Fatalf("Rename = %+v ok=%v err=%v", got, ok, err)
	}
	if got.ID != orig.ID {
		t.Fatalf("conv-id changed on rename: %q → %q", orig.ID, got.ID)
	}
	if got.TailID != 99 {
		t.Fatalf("tail lost on rename: %d", got.TailID)
	}
	if _, ok := j.Lookup(4, "untitled"); ok {
		t.Fatal("old topic still resolves after rename")
	}
	after, ok := j.Lookup(4, "session: real name")
	if !ok || after.ID != orig.ID {
		t.Fatalf("new topic = %+v ok=%v", after, ok)
	}
	// Durable across a reload.
	j2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if c, ok := j2.Lookup(4, "session: real name"); !ok || c.ID != orig.ID {
		t.Fatalf("rename not persisted: %+v ok=%v", c, ok)
	}
}

func TestRenameEdgeCases(t *testing.T) {
	j, _ := tmpJournal(t)
	// Unknown source topic: nothing to migrate.
	if _, ok, err := j.Rename(4, "nope", "other"); ok || err != nil {
		t.Fatalf("unknown rename: ok=%v err=%v", ok, err)
	}
	a, err := j.Ensure(4, "alpha")
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	// A no-op rename changes nothing.
	if _, ok, err := j.Rename(4, "alpha", "alpha"); ok || err != nil {
		t.Fatalf("no-op rename: ok=%v err=%v", ok, err)
	}
	if c, ok := j.Lookup(4, "alpha"); !ok || c.ID != a.ID {
		t.Fatal("no-op rename disturbed the conversation")
	}
	// Renaming onto an occupied topic: the destination wins, and the
	// merged-away conv-id disappears rather than leaving two
	// conversations answering to one topic.
	b, err := j.Ensure(4, "beta")
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	got, ok, err := j.Rename(4, "alpha", "beta")
	if err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if ok {
		t.Fatal("a clashing rename must not report a migration")
	}
	if got.ID != b.ID {
		t.Fatalf("destination did not win: %q vs %q", got.ID, b.ID)
	}
	if _, ok := j.Lookup(4, "alpha"); ok {
		t.Fatal("source topic still resolves")
	}
	for _, c := range j.Convs() {
		if c.ID == a.ID {
			t.Fatal("merged-away conversation is still listed")
		}
	}
}

func TestTailTracking(t *testing.T) {
	j, path := tmpJournal(t)
	c, err := j.Ensure(4, "topic")
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if len(j.OpenTails()) != 0 {
		t.Fatal("no tails expected yet")
	}
	if err := j.SetTail(c.ID, 100); err != nil {
		t.Fatalf("SetTail: %v", err)
	}
	// Setting the same value again is a no-op (no rewrite).
	if err := j.SetTail(c.ID, 100); err != nil {
		t.Fatalf("SetTail: %v", err)
	}
	tails := j.OpenTails()
	if len(tails) != 1 || tails[0].TailID != 100 {
		t.Fatalf("OpenTails = %+v", tails)
	}
	// Survives a restart — that is how an interrupted turn is found.
	j2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if tails := j2.OpenTails(); len(tails) != 1 || tails[0].TailID != 100 {
		t.Fatalf("tails after reload = %+v", tails)
	}
	if err := j.SetTail(c.ID, 0); err != nil {
		t.Fatalf("SetTail clear: %v", err)
	}
	if len(j.OpenTails()) != 0 {
		t.Fatal("tail not cleared")
	}
	if err := j.SetTail("nosuchconv", 1); err == nil {
		t.Fatal("want error for unknown conversation")
	}
}

func TestOpenTailsOrderedAndMultiple(t *testing.T) {
	j, _ := tmpJournal(t)
	for i, topic := range []string{"a", "b", "c"} {
		c, err := j.Ensure(4, topic)
		if err != nil {
			t.Fatalf("Ensure: %v", err)
		}
		if i < 2 {
			if err := j.SetTail(c.ID, int64(10+i)); err != nil {
				t.Fatalf("SetTail: %v", err)
			}
		}
	}
	tails := j.OpenTails()
	if len(tails) != 2 {
		t.Fatalf("OpenTails = %+v", tails)
	}
	if tails[0].ID > tails[1].ID {
		t.Fatalf("OpenTails not ordered: %+v", tails)
	}
	convs := j.Convs()
	for i := 1; i < len(convs); i++ {
		if convs[i-1].ID > convs[i].ID {
			t.Fatalf("Convs not ordered: %+v", convs)
		}
	}
}

func TestOpenErrors(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := Open(bad); err == nil {
		t.Fatal("want parse error")
	}
	// A directory where the file should be: unreadable, and not
	// silently treated as absent.
	asDir := filepath.Join(dir, "adir")
	if err := os.Mkdir(asDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if _, err := Open(asDir); err == nil {
		t.Fatal("want read error")
	}
}

func TestSaveErrors(t *testing.T) {
	dir := t.TempDir()
	// A regular file where the journal's parent directory should be
	// makes MkdirAll fail.
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	j, err := Open(filepath.Join(dir, "ok.json"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	j.path = filepath.Join(blocker, "sub", "journal.json")
	if _, err := j.Ensure(4, "t"); err == nil {
		t.Fatal("want mkdir error")
	}

	// A read-only directory makes the temp write fail.
	ro := filepath.Join(dir, "ro")
	if err := os.Mkdir(ro, 0o500); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(ro, 0o700) })
	j2, err := Open(filepath.Join(ro, "journal.json"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := j2.Ensure(4, "t"); err == nil {
		t.Fatal("want write error")
	}

	// A directory occupying the destination path makes the rename fail
	// after a successful temp write.
	dest := filepath.Join(dir, "dest.json")
	if err := os.Mkdir(dest, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dest, "keep"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	j3, err := Open(filepath.Join(dir, "dest.json.src"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	j3.path = dest
	if _, err := j3.Ensure(4, "t"); err == nil {
		t.Fatal("want rename error")
	}
}

// TestSetTailAndRenameSaveErrors drives the persistence-failure return
// of the two other mutating methods.
func TestSetTailAndRenameSaveErrors(t *testing.T) {
	dir := t.TempDir()
	j, err := Open(filepath.Join(dir, "journal.json"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	c, err := j.Ensure(4, "alpha")
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if _, err := j.Ensure(4, "beta"); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	// Point the journal at an undwritable path for the mutations below.
	blocked := filepath.Join(dir, "file")
	if err := os.WriteFile(blocked, []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	j.path = filepath.Join(blocked, "sub", "journal.json")
	if err := j.SetTail(c.ID, 5); err == nil {
		t.Fatal("SetTail: want save error")
	}
	if _, _, err := j.Rename(4, "alpha", "gamma"); err == nil {
		t.Fatal("Rename: want save error")
	}
	if _, _, err := j.Rename(4, "gamma", "beta"); err == nil {
		t.Fatal("clashing Rename: want save error")
	}
}

// TestNewIDRetriesOnCollision drives the collision branch of newID by
// feeding it a random source that repeats itself once.
func TestNewIDRetriesOnCollision(t *testing.T) {
	orig := randomRead
	t.Cleanup(func() { randomRead = orig })
	var n int
	randomRead = func(b []byte) (int, error) {
		n++
		for i := range b {
			b[i] = byte(n / 3) // three identical draws, then a new one
		}
		return len(b), nil
	}
	j, _ := tmpJournal(t)
	a, err := j.Ensure(4, "one")
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	b, err := j.Ensure(4, "two")
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if a.ID == b.ID {
		t.Fatal("collision was not retried")
	}
	if n < 3 {
		t.Fatalf("random source called %d times, expected a retry", n)
	}
}

func TestMustRandomPanics(t *testing.T) {
	orig := randomRead
	t.Cleanup(func() { randomRead = orig })
	randomRead = func([]byte) (int, error) { return 0, errors.New("no entropy") }
	defer func() {
		if recover() == nil {
			t.Fatal("want panic")
		}
	}()
	mustRandom(make([]byte, 4))
}

// TestConcurrentEnsure exercises the lock discipline under the race
// detector.
func TestConcurrentEnsure(t *testing.T) {
	j, _ := tmpJournal(t)
	var wg sync.WaitGroup
	ids := make([]string, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c, err := j.Ensure(4, "shared topic")
			if err != nil {
				t.Errorf("Ensure: %v", err)
				return
			}
			ids[i] = c.ID
		}(i)
	}
	wg.Wait()
	for i := 1; i < len(ids); i++ {
		if ids[i] != ids[0] {
			t.Fatalf("conv-id raced: %q vs %q", ids[0], ids[i])
		}
	}
}
