package journal

import "testing"

// TestBaseTopicStripsStatusPrefixes: the resolved prefix and every
// configured glyph are stripped, in any order and any number, so the
// status never changes a conversation's identity.
func TestBaseTopicStripsStatusPrefixes(t *testing.T) {
	SetStatusPrefixes("⧗", "", "⁇")
	t.Cleanup(func() { SetStatusPrefixes() })
	for in, want := range map[string]string{
		"foo":         "foo",
		"✔ foo":       "foo",
		"⧗ foo":       "foo",
		"⁇ foo":       "foo",
		"⧗ ✔ ⁇ foo":   "foo",
		"✔ ✔ foo":     "foo",
		"⧗foo":        "⧗foo",
		"foo ⧗ bar":   "foo ⧗ bar",
		"⧗ ":          "",
		"":            "",
		"? foo":       "? foo",
		"⧗ ⧗ foo bar": "foo bar",
	} {
		if got := BaseTopic(in); got != want {
			t.Errorf("BaseTopic(%q) = %q, want %q", in, got, want)
		}
	}
	if Channel(4, "⧗ foo").Token() != Channel(4, "foo").Token() {
		t.Error("a status glyph changed the token")
	}
}

// TestBaseTopicWithoutStatusPrefixes: unset, only the resolved prefix
// is stripped.
func TestBaseTopicWithoutStatusPrefixes(t *testing.T) {
	SetStatusPrefixes()
	if got := BaseTopic("⧗ foo"); got != "⧗ foo" {
		t.Fatalf("BaseTopic = %q", got)
	}
	if got := BaseTopic("✔ foo"); got != "foo" {
		t.Fatalf("BaseTopic = %q", got)
	}
}

func TestStatusPrefix(t *testing.T) {
	if StatusPrefix("") != "" || StatusPrefix("⧗") != "⧗ " {
		t.Fatal("StatusPrefix")
	}
}

// TestLookupFindsAStatusPrefixedTopic: a topic renamed to carry a
// glyph is still the same conversation.
func TestLookupFindsAStatusPrefixedTopic(t *testing.T) {
	SetStatusPrefixes("⧗")
	t.Cleanup(func() { SetStatusPrefixes() })
	j, _ := tmpJournal(t)
	c, err := j.Ensure(Channel(4, "foo"))
	if err != nil {
		t.Fatal(err)
	}
	got, ok := j.Lookup(Channel(4, "⧗ foo"))
	if !ok || got.ID != c.ID {
		t.Fatalf("Lookup = %+v, %v", got, ok)
	}
}

// TestModeSwitchKeepsTheConversation: a topic left as "⁇ foo" must still
// be "foo" once the relay runs with another prefix set, as long as the
// old glyph is still in it — main registers the defaults always.
func TestModeSwitchKeepsTheConversation(t *testing.T) {
	SetStatusPrefixes("⧗", "⁇")
	t.Cleanup(func() { SetStatusPrefixes() })
	j, path := tmpJournal(t)
	c, err := j.Ensure(Channel(4, "⁇ foo"))
	if err != nil {
		t.Fatal(err)
	}
	SetStatusPrefixes("!", "?", "⧗", "⁇")
	j2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := j2.Lookup(Channel(4, "foo")); !ok || got.ID != c.ID {
		t.Fatalf("Lookup = %+v, %v", got, ok)
	}
}
