package statusline

import (
	"strings"
	"testing"
)

func TestFooter(t *testing.T) {
	if got := Footer(Status{}); got != "" {
		t.Fatalf("empty status must render nothing, got %q", got)
	}
	// The whole line, exactly: blank line, italics, emoji and model as
	// ONE segment, then mood and plan.
	got := Footer(Status{ProviderEmoji: "🏛️", Model: "opus-4.5", Mood: "steady", Plan: "2/5"})
	if want := "\n\n*🏛️ opus-4.5 • steady • 2/5*"; got != want {
		t.Fatalf("Footer = %q, want %q", got, want)
	}
	// It is a footer, not the blockquoted spinner.
	if strings.Contains(got, ">") {
		t.Fatalf("footer must not be a blockquote: %q", got)
	}
	// Half an identity still renders, without a stray separator.
	if got := Footer(Status{Model: "gpt-5-codex"}); got != "\n\n*gpt-5-codex*" {
		t.Fatalf("model-only footer = %q", got)
	}
	if got := Footer(Status{ProviderEmoji: "🌐"}); got != "\n\n*🌐*" {
		t.Fatalf("emoji-only footer = %q", got)
	}
	// Mood alone is enough to be worth signing.
	if got := Footer(Status{Mood: "curious"}); got != "\n\n*curious*" {
		t.Fatalf("mood-only footer = %q", got)
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
	// The live line names the model too, in the same one-segment form
	// the footer uses.
	got = Spinner(Status{ProviderEmoji: "🏛️", Model: "opus-4.5", Mood: "steady", Plan: "2/5"}, "...")
	if want := "> *🏛️ opus-4.5 • steady • 2/5 • Thinking...*"; got != want {
		t.Fatalf("Spinner = %q, want %q", got, want)
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
	if got := ShortModelName("anthropic/claude-opus-4-5-20251001"); got != "opus-4.5" {
		t.Fatalf("ShortModelName = %q", got)
	}
}
