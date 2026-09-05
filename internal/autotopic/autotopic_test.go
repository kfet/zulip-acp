package autotopic

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestNameAt(t *testing.T) {
	now := time.Date(2026, 9, 5, 14, 30, 15, 0, time.UTC)
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain", "Deploy the relay to prod", "Deploy the relay to prod"},
		{"trailing punctuation", "can you fix the build?", "can you fix the build"},
		{"interior question mark", "what? really, fix it", "what? really, fix it"},
		{"mention stripped", "@**Relay Bot** ship the release", "ship the release"},
		{"mention with id", "@**Relay Bot|42** ship it", "ship it"},
		{"silent mention", "@_**Relay Bot**_ ship it", "ship it"},
		{"mention only", "@**Relay Bot**", "chat 2026-09-05 14:30:15"},
		{"bare at kept", "mail bot@example.com about it", "mail bot@example.com about it"},
		{"unclosed mention", "@**Relay ship it", "Relay ship it"},
		{"first line only", "Fix the parser\nit crashes on empty input", "Fix the parser"},
		{"leading blank lines", "\n\n  Restart the queue  ", "Restart the queue"},
		{"quote and bullet", "> - **Rotate** the API key", "Rotate the API key"},
		{"heading", "# Release checklist", "Release checklist"},
		{"code fence skipped", "```go\nfunc main() {}\n```\nexplain this", "func main"},
		{"horizontal rule skipped", "---\nafter the rule", "after the rule"},
		{"inline code", "run `make all` please", "run make all please"},
		{"link text kept", "see [the design doc](https://x.example/d) now", "see the design doc now"},
		{"unclosed link", "see [doc](broken", "see doc (broken"},
		{"underscore identifier kept", "why does make_test fail", "why does make_test fail"},
		{"whitespace collapsed", "too    many\tspaces", "too many spaces"},
		{"empty", "", "chat 2026-09-05 14:30:15"},
		// A message whose whole text is "general chat" would name the
		// topic after general chat itself — at best colliding with the
		// display name, and quite possibly a no-op move back to the
		// empty topic. Either way it means "did not move".
		{"names general chat", "general chat", "chat 2026-09-05 14:30:15"},
		{"general chat prefix kept", "general chat notes", "general chat notes"},
		{"punctuation only", "!!! ... ???", "chat 2026-09-05 14:30:15"},
		{"emoji only", "🙂🙂", "chat 2026-09-05 14:30:15"},
		{
			"truncated on a word boundary",
			"Please investigate the intermittent failure in the rollover splitter tests",
			"Please investigate the intermittent failure in the rollover",
		},
		{
			"long unbroken run cut hard",
			strings.Repeat("x", 80),
			strings.Repeat("x", 60),
		},
		{
			"late space keeps the hard cut",
			strings.Repeat("y", 58) + " tail words here",
			strings.Repeat("y", 58),
		},
		{
			"code points not bytes",
			strings.Repeat("é", 70),
			strings.Repeat("é", 60),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := NameAt(tc.in, now)
			if got != tc.want {
				t.Fatalf("NameAt(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if IsGeneralChat(got) {
				t.Fatal("a topic name must never be general chat — that means 'did not move'")
			}
			if n := utf8.RuneCountInString(got); n > MaxLen {
				t.Fatalf("topic is %d code points, want <= %d", n, MaxLen)
			}
		})
	}
}

// TestIsGeneralChat: the relay must recognise general chat in BOTH
// wire spellings — "" (only sent to clients declaring the
// empty_topic_name capability, which we deliberately do not) and the
// translated display name a real server actually sends. An ordinary
// topic that merely starts with the words is a human's topic.
func TestIsGeneralChat(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"", true},
		{"   ", true},
		{"general chat", true},
		{"General chat", true},
		{"GENERAL CHAT", true},
		{"  general chat  ", true},
		{"general", false},
		{"general chat notes", false},
		{"generalchat", false},
		{"chat", false},
		{"release", false},
	}
	for _, tc := range cases {
		if got := IsGeneralChat(tc.in); got != tc.want {
			t.Fatalf("IsGeneralChat(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// TestDisambiguate: the id makes a colliding name unique, and the
// result still fits the topic limit however long the name was.
func TestDisambiguate(t *testing.T) {
	if got := Disambiguate("hello there", 42); got != "hello there (#42)" {
		t.Fatalf("Disambiguate = %q", got)
	}
	long := Disambiguate(strings.Repeat("x", MaxLen), 9223372036854775807)
	if n := utf8.RuneCountInString(long); n > MaxLen {
		t.Fatalf("Disambiguate(%q…) is %d code points, want <= %d", long[:10], n, MaxLen)
	}
	if !strings.HasSuffix(long, "(#9223372036854775807)") {
		t.Fatalf("the id must survive truncation: %q", long)
	}
}

// TestFallbackFitsTheTopicLimit guards the one output that does not go
// through truncate: the clock fallback.
func TestFallbackFitsTheTopicLimit(t *testing.T) {
	got := NameAt("", time.Date(2026, 12, 31, 23, 59, 59, 0, time.UTC))
	if n := utf8.RuneCountInString(got); n > MaxLen {
		t.Fatalf("fallback %q is %d code points, want <= %d", got, n, MaxLen)
	}
}

// TestNameUsesTheWallClock covers the exported wrapper: only the
// fallback path can observe the clock, so drive that.
func TestNameUsesTheWallClock(t *testing.T) {
	got := Name("")
	if !strings.HasPrefix(got, "chat ") {
		t.Fatalf("Name(\"\") = %q, want a chat <timestamp> fallback", got)
	}
	if got == Name("hello there") {
		t.Fatal("a usable message must not fall back to the clock")
	}
}
