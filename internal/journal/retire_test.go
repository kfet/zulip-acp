package journal

import (
	"os"
	"strings"
	"testing"
)

func TestRetireAllocatesAFreshConvAndKeepsTheOldOne(t *testing.T) {
	j, path := tmpJournal(t)
	k := Channel(4, "hacking")
	old, err := j.Ensure(k)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if err := j.SetTail(old.ID, 77); err != nil {
		t.Fatalf("SetTail: %v", err)
	}

	prev, fresh, existed, err := j.Retire(k)
	if err != nil {
		t.Fatalf("Retire: %v", err)
	}
	if !existed {
		t.Fatal("Retire reported no existing conversation")
	}
	if prev.ID != old.ID {
		t.Fatalf("retired %q, want %q", prev.ID, old.ID)
	}
	if fresh.ID == old.ID {
		t.Fatal("Retire reused the conv-id")
	}
	if fresh.StreamID != 4 || fresh.Topic != "hacking" {
		t.Fatalf("fresh conv has the wrong key: %+v", fresh)
	}
	// The tail must be cleared, or the next turn would stream into the
	// retired conversation's message.
	if fresh.TailID != 0 {
		t.Fatalf("fresh conv carries tail %d", fresh.TailID)
	}
	if got := j.OpenTails(); len(got) != 0 {
		t.Fatalf("OpenTails = %+v, want none", got)
	}

	// The key now resolves to the fresh conv.
	got, ok := j.Lookup(k)
	if !ok || got.ID != fresh.ID {
		t.Fatalf("Lookup = %+v, %v; want %q", got, ok, fresh.ID)
	}

	// The retired conv is still addressable by id, so an in-flight
	// turn unwinding into SetTail does not hit "unknown conversation".
	if err := j.SetTail(old.ID, 0); err != nil {
		t.Fatalf("SetTail on retired conv: %v", err)
	}

	// …and it survives a reload without ever re-claiming the key.
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(b), `"retired": true`) {
		t.Fatalf("retired flag not persisted: %s", b)
	}
	j2, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	got, ok = j2.Lookup(k)
	if !ok || got.ID != fresh.ID {
		t.Fatalf("after reload Lookup = %+v, %v; want %q", got, ok, fresh.ID)
	}
	if len(j2.Convs()) != 2 {
		t.Fatalf("Convs = %+v, want both", j2.Convs())
	}
}

func TestRetireUnknownKeyIsNotAnError(t *testing.T) {
	j, _ := tmpJournal(t)
	prev, fresh, existed, err := j.Retire(Channel(4, "never seen"))
	if err != nil {
		t.Fatalf("Retire: %v", err)
	}
	if existed {
		t.Fatal("Retire invented an existing conversation")
	}
	if prev.ID != "" {
		t.Fatalf("prev = %+v, want zero", prev)
	}
	if fresh.ID != "" {
		t.Fatalf("Retire allocated for an unknown key: %+v", fresh)
	}
	if _, ok := j.Lookup(Channel(4, "never seen")); ok {
		t.Fatal("Retire allocated a conversation for an unknown key")
	}
}

func TestRetireWorksOnADM(t *testing.T) {
	j, _ := tmpJournal(t)
	k := DM([]int64{9, 4})
	old, err := j.Ensure(k)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	_, fresh, existed, err := j.Retire(k)
	if err != nil || !existed {
		t.Fatalf("Retire: %v, %v", existed, err)
	}
	if fresh.ID == old.ID || !fresh.IsDM() {
		t.Fatalf("fresh = %+v", fresh)
	}
	got, _ := j.Lookup(DM([]int64{4, 9}))
	if got.ID != fresh.ID {
		t.Fatalf("Lookup = %+v, want %q", got, fresh.ID)
	}
}

func TestRetireSaveError(t *testing.T) {
	j, path := tmpJournal(t)
	k := Channel(4, "hacking")
	if _, err := j.Ensure(k); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	dir := strings.TrimSuffix(path, "/journal.json")
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if _, _, _, err := j.Retire(k); err == nil {
		t.Fatal("Retire did not report the persistence failure")
	}
}

// A retired conversation must never be re-indexed by key on load, even
// if a hand-edited journal leaves its key fields in place.
func TestRetiredEntriesNeverClaimTheirKey(t *testing.T) {
	j, path := tmpJournal(t)
	k := Channel(4, "hacking")
	if _, err := j.Ensure(k); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if _, _, _, err := j.Retire(k); err != nil {
		t.Fatalf("Retire: %v", err)
	}
	j2, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	// Exactly one conv answers to the key, and it is the live one.
	c, ok := j2.Lookup(k)
	if !ok || c.Retired {
		t.Fatalf("Lookup = %+v, %v", c, ok)
	}
}

// TestFailedWritesLeaveTheJournalUnchanged is the rule every mutation
// here obeys: a mutation that cannot be persisted must not be visible
// in memory either. Otherwise the relay tells the user the change
// failed and then behaves as though it succeeded — until a restart
// reloads the file and it silently un-happens.
//
// The subtests share one journal and run in source order, each relying
// on the previous one having rolled back cleanly. That is the point:
// a leaked mutation shows up as a later subtest failing.
func TestFailedWritesLeaveTheJournalUnchanged(t *testing.T) {
	j, path := tmpJournal(t)
	kept, err := j.Ensure(Channel(4, "kept"))
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if err := j.SetTail(kept.ID, 11); err != nil {
		t.Fatalf("SetTail: %v", err)
	}
	dir := strings.TrimSuffix(path, "/journal.json")
	readOnly := func() {
		if err := os.Chmod(dir, 0o500); err != nil {
			t.Fatalf("chmod: %v", err)
		}
	}
	writable := func() {
		if err := os.Chmod(dir, 0o700); err != nil {
			t.Fatalf("chmod: %v", err)
		}
	}
	t.Cleanup(writable)

	t.Run("Ensure", func(t *testing.T) {
		readOnly()
		defer writable()
		if _, err := j.Ensure(Channel(4, "brand new")); err == nil {
			t.Fatal("Ensure did not report the failure")
		}
		if c, ok := j.Lookup(Channel(4, "brand new")); ok {
			t.Fatalf("a conversation that could not be persisted is live: %+v", c)
		}
		if n := len(j.Convs()); n != 1 {
			t.Fatalf("Convs = %d, want 1", n)
		}
	})

	t.Run("SetTail", func(t *testing.T) {
		readOnly()
		defer writable()
		if err := j.SetTail(kept.ID, 99); err == nil {
			t.Fatal("SetTail did not report the failure")
		}
		if got := j.Convs()[0].TailID; got != 11 {
			t.Fatalf("TailID = %d, want the unchanged 11", got)
		}
	})

	t.Run("Rename", func(t *testing.T) {
		readOnly()
		defer writable()
		if _, _, err := j.Rename(4, "kept", "moved"); err == nil {
			t.Fatal("Rename did not report the failure")
		}
		if _, ok := j.Lookup(Channel(4, "moved")); ok {
			t.Fatal("a rename that could not be persisted is live")
		}
		c, ok := j.Lookup(Channel(4, "kept"))
		if !ok || c.ID != kept.ID {
			t.Fatalf("the original key was lost: %+v, %v", c, ok)
		}
	})

	t.Run("Rename onto a clash", func(t *testing.T) {
		clash, err := j.Ensure(Channel(4, "clash"))
		if err != nil {
			t.Fatalf("Ensure: %v", err)
		}
		readOnly()
		defer writable()
		if _, _, err := j.Rename(4, "kept", "clash"); err == nil {
			t.Fatal("Rename did not report the failure")
		}
		c, ok := j.Lookup(Channel(4, "kept"))
		if !ok || c.ID != kept.ID {
			t.Fatalf("the losing conversation was dropped anyway: %+v, %v", c, ok)
		}
		if c, ok := j.Lookup(Channel(4, "clash")); !ok || c.ID != clash.ID {
			t.Fatalf("the winning conversation changed: %+v, %v", c, ok)
		}
	})

	t.Run("Retire", func(t *testing.T) {
		readOnly()
		defer writable()
		if _, _, _, err := j.Retire(Channel(4, "kept")); err == nil {
			t.Fatal("Retire did not report the failure")
		}
		c, ok := j.Lookup(Channel(4, "kept"))
		if !ok || c.ID != kept.ID {
			t.Fatalf("the conversation was retired anyway: %+v, %v", c, ok)
		}
		if c.Retired || c.TailID != 11 {
			t.Fatalf("conv mutated by a failed Retire: %+v", c)
		}
	})
}
