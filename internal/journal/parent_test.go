package journal

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// openTemp is a journal in a fresh directory.
func openTemp(t *testing.T) (*Journal, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "journal.json")
	j, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return j, path
}

// TestBranchRecordsTheBranchPoint: the pointer survives a reload,
// which is the whole point of putting it in the journal rather than in
// memory — a relay restart must not cost a branch its origin.
func TestBranchRecordsTheBranchPoint(t *testing.T) {
	j, path := openTemp(t)
	c, err := j.Branch(Channel(4, "spun out"), Parent{Key: Channel(4, "where it started"), MessageID: 949})
	if err != nil {
		t.Fatalf("Branch: %v", err)
	}
	if c.Parent == nil || c.Parent.MessageID != 949 || c.Parent.Key.Topic != "where it started" {
		t.Fatalf("conv = %+v", c)
	}
	// And it is reachable by key, like any other conversation.
	if got, ok := j.Lookup(Channel(4, "spun out")); !ok || got.ID != c.ID {
		t.Fatalf("Lookup = %+v, %v", got, ok)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	got, ok := reopened.LookupID(c.ID)
	if !ok || got.Parent == nil || got.Parent.MessageID != 949 {
		t.Fatalf("the origin did not survive a reload: %+v", got)
	}
}

// TestBranchNormalisesADMOrigin: a DM key is a SET, so a parent
// written with an unsorted participant list must index the same as the
// one Zulip delivers, or the origin narrow would name a different
// conversation.
func TestBranchNormalisesADMOrigin(t *testing.T) {
	j, path := openTemp(t)
	c, err := j.Branch(Channel(4, "spun out"), Parent{Key: Key{UserIDs: []int64{9, 4, 9}}, MessageID: 7})
	if err != nil {
		t.Fatalf("Branch: %v", err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	got, _ := reopened.LookupID(c.ID)
	if len(got.Parent.Key.UserIDs) != 2 || got.Parent.Key.UserIDs[0] != 4 {
		t.Fatalf("parent user ids = %v, want the sorted, deduped set", got.Parent.Key.UserIDs)
	}
}

// TestBranchRefusals: every way a branch cannot be recorded.
func TestBranchRefusals(t *testing.T) {
	j, _ := openTemp(t)
	key := Channel(4, "spun out")

	if _, err := j.Branch(key, Parent{Key: Channel(4, "origin")}); err == nil {
		t.Fatal("a branch with no branch point has nothing to clamp a read to and must be refused")
	}
	if _, err := j.Branch(key, Parent{Key: key, MessageID: 1}); err == nil {
		t.Fatal("a conversation must not be able to declare itself its own origin")
	}
	if _, err := j.Branch(key, Parent{Key: Channel(4, "origin"), MessageID: 1}); err != nil {
		t.Fatalf("Branch: %v", err)
	}
	if _, err := j.Branch(key, Parent{Key: Channel(4, "origin"), MessageID: 2}); err == nil {
		t.Fatal("branching onto a live conversation would hand its session an origin nobody declared")
	}
}

// TestBranchRollsBackAFailedWrite: the in-memory state must never
// disagree with what the caller was told, or a restart silently undoes
// the change. Same contract every other mutation here holds.
func TestBranchRollsBackAFailedWrite(t *testing.T) {
	j, path := openTemp(t)
	dir := filepath.Dir(path)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if _, err := j.Branch(Channel(4, "spun out"), Parent{Key: Channel(4, "origin"), MessageID: 1}); err == nil {
		t.Fatal("a failed write must be reported")
	}
	if _, ok := j.Lookup(Channel(4, "spun out")); ok {
		t.Fatal("the in-memory state kept a conversation the file does not have")
	}
}

// TestRetireCarriesTheOrigin: the parent belongs to the PLACE. `!new`
// clears the session's context, and the topic is still one that was
// branched out of somewhere.
func TestRetireCarriesTheOrigin(t *testing.T) {
	j, _ := openTemp(t)
	key := Channel(4, "spun out")
	if _, err := j.Branch(key, Parent{Key: Channel(4, "origin"), MessageID: 949}); err != nil {
		t.Fatalf("Branch: %v", err)
	}
	prev, fresh, existed, err := j.Retire(key)
	if err != nil || !existed {
		t.Fatalf("Retire: %v existed=%v", err, existed)
	}
	if prev.Parent == nil || fresh.Parent == nil {
		t.Fatalf("prev = %+v fresh = %+v", prev, fresh)
	}
	if fresh.Parent.MessageID != 949 {
		t.Fatalf("fresh origin = %+v", *fresh.Parent)
	}
}

// TestParentSurvivesAMove: a branched topic that is renamed or moved
// keeps its origin, because the conv record itself follows the move.
func TestParentSurvivesAMove(t *testing.T) {
	j, _ := openTemp(t)
	if _, err := j.Branch(Channel(4, "spun out"), Parent{Key: Channel(4, "origin"), MessageID: 949}); err != nil {
		t.Fatalf("Branch: %v", err)
	}
	moved, ok, err := j.Rename(4, "spun out", "a better name")
	if err != nil || !ok {
		t.Fatalf("Rename: %v ok=%v", err, ok)
	}
	if moved.Parent == nil || moved.Parent.MessageID != 949 {
		t.Fatalf("origin lost across a rename: %+v", moved)
	}
}

// TestPreBranchJournalStillLoads: the field is additive, so a
// pre-branch file and a post-branch file are both valid — and a
// hand-written parent with no branch point is dropped, because
// nothing downstream should have to ask what one would mean.
func TestPreBranchJournalStillLoads(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "journal.json")
	body := `{"version":1,"convs":[
		{"id":"cabc","stream_id":4,"topic":"old"},
		{"id":"cdef","stream_id":4,"topic":"half","parent":{"stream_id":4,"topic":"origin"}}
	]}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	j, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	for _, id := range []string{"cabc", "cdef"} {
		c, ok := j.LookupID(id)
		if !ok || c.Parent != nil {
			t.Fatalf("conv %s = %+v", id, c)
		}
	}
	// And a written journal names the field the way the doc says.
	c2, err := j.Branch(Channel(4, "spun out"), Parent{Key: Channel(4, "old"), MessageID: 5})
	if err != nil {
		t.Fatalf("Branch: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(b), `"parent"`) || !strings.Contains(string(b), `"message_id": 5`) {
		t.Fatalf("on-disk shape = %s", b)
	}
	if _, ok := j.LookupID(c2.ID); !ok {
		t.Fatal("the branched conversation is not addressable by id")
	}
}
