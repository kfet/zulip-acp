package journal

import (
	"strings"
	"testing"
)

func TestTokenRoundTrips(t *testing.T) {
	keys := []Key{
		Channel(4, "hacking"),
		Channel(4, ""),
		Channel(0, "weird"),
		Channel(4, "topic with spaces, commas and \"quotes\""),
		Channel(4, "emoji 🔥 topic"),
		DM([]int64{9, 4}),
		DM([]int64{22, 9, 4}),
		DM([]int64{7}),
	}
	for _, k := range keys {
		got, err := ParseToken(k.Token())
		if err != nil {
			t.Fatalf("ParseToken(%q): %v", k.Token(), err)
		}
		if got.Token() != k.Token() {
			t.Fatalf("round trip: %q → %q", k.Token(), got.Token())
		}
		if got.IsDM() != k.IsDM() {
			t.Fatalf("%q changed shape", k.Token())
		}
	}
}

// TestTokenNamespacesStayDisjoint: a channel token must never parse to
// a DM key or vice versa, or a command could act on the wrong
// conversation entirely.
func TestTokenNamespacesStayDisjoint(t *testing.T) {
	ch := Channel(49, "x").Token()
	dm := DM([]int64{4, 9}).Token()
	if ch == dm {
		t.Fatal("a channel and a DM produced the same token")
	}
	c, err := ParseToken(ch)
	if err != nil || c.IsDM() {
		t.Fatalf("channel token parsed as %+v, %v", c, err)
	}
	d, err := ParseToken(dm)
	if err != nil || !d.IsDM() {
		t.Fatalf("DM token parsed as %+v, %v", d, err)
	}
}

// TestParseTokenRejectsGarbage: parsing is strict on purpose. A
// best-effort guess would attach a command to the wrong conversation,
// and `!new` on the wrong conversation destroys context.
func TestParseTokenRejectsGarbage(t *testing.T) {
	bad := []string{
		"",
		"nonsense",
		"c",
		"x\x004\x00topic",
		"c\x00notanumber\x00topic",
		"c\x004",            // channel with no topic separator
		"d\x00",             // no participants
		"d\x004,notanumber", // bad id in the set
		"d\x00,",            // empty ids
	}
	for _, s := range bad {
		if k, err := ParseToken(s); err == nil {
			t.Errorf("ParseToken(%q) = %+v, want an error", s, k)
		} else if !strings.Contains(err.Error(), "journal:") {
			t.Errorf("ParseToken(%q) error is unlabelled: %v", s, err)
		}
	}
}

// TestTokenMatchesTheMapKey pins the property the whole design rests
// on: a token cannot disagree with a lookup, because it IS the map key.
func TestTokenMatchesTheMapKey(t *testing.T) {
	j, _ := tmpJournal(t)
	k := Channel(4, "hacking")
	want, err := j.Ensure(k)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	parsed, err := ParseToken(k.Token())
	if err != nil {
		t.Fatalf("ParseToken: %v", err)
	}
	got, ok := j.Lookup(parsed)
	if !ok || got.ID != want.ID {
		t.Fatalf("Lookup via token = %+v, %v; want %q", got, ok, want.ID)
	}
}

// TestTokenSurvivesRetire is the reason the token is the KEY and not
// the conv-id: `!new` replaces the conv-id, and a broker holding one
// would be holding a stale identity.
func TestTokenSurvivesRetire(t *testing.T) {
	j, _ := tmpJournal(t)
	k := Channel(4, "hacking")
	old, err := j.Ensure(k)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	token := k.Token()
	if _, fresh, _, err := j.Retire(k); err != nil {
		t.Fatalf("Retire: %v", err)
	} else if fresh.ID == old.ID {
		t.Fatal("Retire reused the conv-id")
	}
	// The same token still reaches the conversation — now the fresh one.
	parsed, err := ParseToken(token)
	if err != nil {
		t.Fatalf("ParseToken: %v", err)
	}
	got, ok := j.Lookup(parsed)
	if !ok || got.ID == old.ID {
		t.Fatalf("token went stale after Retire: %+v, %v", got, ok)
	}
}
