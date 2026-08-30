package statusline

import (
	"strings"
	"testing"
)

func TestHeader(t *testing.T) {
	if got := Header(Status{}); got != "" {
		t.Fatalf("empty status must render nothing, got %q", got)
	}
	got := Header(Status{ProviderEmoji: "🤖", Mood: "steady", Plan: "3/7"})
	if !strings.HasPrefix(got, "> *") || !strings.HasSuffix(got, "*") {
		t.Fatalf("header is not a Zulip italic blockquote: %q", got)
	}
	for _, want := range []string{"🤖", "steady", "3/7", " • "} {
		if !strings.Contains(got, want) {
			t.Fatalf("header %q missing %q", got, want)
		}
	}
}

func TestThinkingAndSpinner(t *testing.T) {
	// Always a visible frame, even with nothing known yet.
	if got := Thinking(Status{}); !strings.Contains(got, "Thinking…") {
		t.Fatalf("Thinking = %q", got)
	}
	if got := Spinner(Status{}, ""); !strings.Contains(got, "Thinking…") {
		t.Fatalf("Spinner with empty dots = %q", got)
	}
	got := Spinner(Status{Mood: "curious"}, "..")
	if !strings.Contains(got, "curious") || !strings.Contains(got, "Thinking..") {
		t.Fatalf("Spinner = %q", got)
	}
}

func TestWireContractReExports(t *testing.T) {
	if ExtensionID != "dev.acp-kit.status-line/v1" {
		t.Fatalf("extension id drifted: %q", ExtensionID)
	}
	mood, plan, ok := ParseMeta(map[string]any{
		ExtensionID: map[string]any{"mood": "engaged", "plan": "1/2"},
	})
	if !ok || mood != "engaged" || plan != "1/2" {
		t.Fatalf("ParseMeta = %q %q %v", mood, plan, ok)
	}
	if _, _, ok := ParseMeta(nil); ok {
		t.Fatal("nil meta must not report the extension present")
	}
	if ProviderEmojiForModel("nonesuch/model-1") != "" {
		t.Fatal("unknown provider must render no emoji")
	}
	if ProviderEmojiForModel("anthropic/claude-sonnet-4") == "" {
		t.Fatal("known provider must render an emoji")
	}
}
