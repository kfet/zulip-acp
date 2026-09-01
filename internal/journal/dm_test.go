package journal

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestKeyShapesAreDisjoint: one Key type expresses both conversation
// shapes, so the thing that must be proven is that they can never
// collide — a channel conversation and a DM must not share an index
// however the numbers line up.
func TestKeyShapesAreDisjoint(t *testing.T) {
	if Channel(4, "t").IsDM() {
		t.Fatal("channel key claims to be a DM")
	}
	if !DM([]int64{4, 9}).IsDM() {
		t.Fatal("DM key does not claim to be a DM")
	}
	seen := map[string]string{}
	for _, k := range []Key{
		Channel(4, "t"),
		Channel(4, "u"),
		Channel(5, "t"),
		Channel(0, "4,9"),
		DM([]int64{4, 9}),
		DM([]int64{4}),
		DM([]int64{4, 9, 10}),
	} {
		if prev, clash := seen[k.index()]; clash {
			t.Fatalf("keys collide: %s and %s both index as %q", prev, k.Label(), k.index())
		}
		seen[k.index()] = k.Label()
	}
	// An empty participant set is not a conversation.
	if DM(nil).IsDM() {
		t.Fatal("empty DM key must not read as a DM")
	}
}

// TestDMKeyIsASet: order and duplicates must not matter, because
// Zulip's display_recipient order is not contractual.
func TestDMKeyIsASet(t *testing.T) {
	a := DM([]int64{9, 4, 22})
	b := DM([]int64{22, 9, 4, 9})
	if a.index() != b.index() {
		t.Fatalf("%q != %q", a.index(), b.index())
	}
	want := []int64{4, 9, 22}
	if len(a.UserIDs) != len(want) {
		t.Fatalf("UserIDs = %v", a.UserIDs)
	}
	for i, id := range want {
		if a.UserIDs[i] != id {
			t.Fatalf("UserIDs = %v, want %v", a.UserIDs, want)
		}
	}
	// The input slice must not be mutated under the caller.
	in := []int64{9, 4}
	_ = DM(in)
	if in[0] != 9 || in[1] != 4 {
		t.Fatalf("DM sorted the caller's slice: %v", in)
	}
}

func TestDMEnsureLookupAndPersist(t *testing.T) {
	j, path := tmpJournal(t)
	k := DM([]int64{9, 4})
	c, err := j.Ensure(k)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if !c.IsDM() || c.StreamID != 0 || c.Topic != "" {
		t.Fatalf("conv = %+v", c)
	}
	again, err := j.Ensure(DM([]int64{4, 9}))
	if err != nil || again.ID != c.ID {
		t.Fatalf("Ensure is not stable: %+v / %v", again, err)
	}
	if err := j.SetTail(c.ID, 77); err != nil {
		t.Fatalf("SetTail: %v", err)
	}
	j2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	got, ok := j2.Lookup(k)
	if !ok || got.ID != c.ID || got.TailID != 77 {
		t.Fatalf("round trip lost the DM: %+v ok=%v", got, ok)
	}
	if len(got.UserIDs) != 2 || got.UserIDs[0] != 4 || got.UserIDs[1] != 9 {
		t.Fatalf("UserIDs = %v", got.UserIDs)
	}
	// The on-disk shape carries user_ids and no channel identity.
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var f struct {
		Convs []map[string]any `json:"convs"`
	}
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(f.Convs) != 1 {
		t.Fatalf("convs = %v", f.Convs)
	}
	if _, ok := f.Convs[0]["user_ids"]; !ok {
		t.Fatalf("no user_ids on disk: %v", f.Convs[0])
	}
	if _, ok := f.Convs[0]["stream_id"]; ok {
		t.Fatalf("DM persisted a channel id: %v", f.Convs[0])
	}
}

// TestPreDMJournalLoadsUnchanged is the backward-compatibility gate: a
// journal written before DM support — {"id","stream_id","topic"}, no
// version bump — must load, index and round-trip as channel
// conversations.
func TestPreDMJournalLoadsUnchanged(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "journal.json")
	const old = `{
  "version": 1,
  "convs": [
    {"id": "c085fc8c1f77f", "stream_id": 4, "topic": "acp-e2e-roll"},
    {"id": "c0a12e93b11e1", "stream_id": 1, "topic": "multi-channel-check", "tail_id": 42}
  ]
}`
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	j, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	c, ok := j.Lookup(Channel(4, "acp-e2e-roll"))
	if !ok || c.ID != "c085fc8c1f77f" || c.IsDM() {
		t.Fatalf("old entry did not load as a channel conversation: %+v ok=%v", c, ok)
	}
	tails := j.OpenTails()
	if len(tails) != 1 || tails[0].TailID != 42 {
		t.Fatalf("tails = %+v", tails)
	}
	// Writing it back preserves both entries and adds a DM alongside.
	if _, err := j.Ensure(DM([]int64{4, 9})); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	j2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if n := len(j2.Convs()); n != 3 {
		t.Fatalf("convs = %d, want 3", n)
	}
	if c, ok := j2.Lookup(Channel(1, "multi-channel-check")); !ok || c.TailID != 42 {
		t.Fatalf("channel entry lost on rewrite: %+v ok=%v", c, ok)
	}
}

// TestHandEditedDMKeyIsNormalised: an operator (or an older bug) may
// leave user_ids unsorted or duplicated on disk; it must still index
// as the set Zulip delivers.
func TestHandEditedDMKeyIsNormalised(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.json")
	const raw = `{"version":1,"convs":[{"id":"cdeadbeef0001","user_ids":[9,4,9]}]}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	j, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	c, ok := j.Lookup(DM([]int64{4, 9}))
	if !ok || c.ID != "cdeadbeef0001" {
		t.Fatalf("unnormalised key did not index: %+v ok=%v", c, ok)
	}
}

// TestKeyLabelIsNotPromotedToConv: Conv embeds Key, so a String method
// on Key would be promoted and quietly hide the conv-id from every
// log line that formats a Conv. Label keeps that from happening.
func TestKeyLabelIsNotPromotedToConv(t *testing.T) {
	if _, isStringer := any(Conv{ID: "c1"}).(interface{ String() string }); isStringer {
		t.Fatal("Conv became a fmt.Stringer; formatting one no longer shows the conv-id")
	}
	if got := Channel(4, "t").Label(); got != `channel 4 > "t"` {
		t.Fatalf("Label = %q", got)
	}
	if got := DM([]int64{9, 4}).Label(); got != "DM 4,9" {
		t.Fatalf("Label = %q", got)
	}
}

// TestRenameIgnoresDMs: a DM has no topic, and its key lives in a
// disjoint namespace, so a rename can never migrate one.
func TestRenameIgnoresDMs(t *testing.T) {
	j, _ := tmpJournal(t)
	dm, err := j.Ensure(DM([]int64{4, 9}))
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if _, moved, err := j.Rename(0, "4,9", "renamed"); err != nil || moved {
		t.Fatalf("Rename touched a DM: moved=%v err=%v", moved, err)
	}
	if c, ok := j.Lookup(DM([]int64{4, 9})); !ok || c.ID != dm.ID {
		t.Fatalf("DM conversation disturbed: %+v ok=%v", c, ok)
	}
}
