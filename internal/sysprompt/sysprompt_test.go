package sysprompt

import (
	"strings"
	"testing"
)

func TestResolve(t *testing.T) {
	got := Resolve("", false, "", "")
	if !strings.Contains(got, "Zulip") {
		t.Fatalf("built-in block missing: %q", got)
	}
	// The block must NOT ask the agent to self-limit its length —
	// rollover handles that, and a self-censoring agent answers worse.
	if strings.Contains(got, "10000") {
		t.Fatal("system prompt must not leak the message-length limit to the agent")
	}
	if !strings.Contains(got, "```go") {
		t.Fatal("system prompt should teach language-tagged fences")
	}
	// The agent cannot use the outbox convention it is never told about.
	if !strings.Contains(got, "./outbox/") {
		t.Fatal("system prompt must document the outbox convention")
	}

	withExtra := Resolve("You are the ops bot.", false, "", "")
	if !strings.Contains(withExtra, "You are the ops bot.") || !strings.Contains(withExtra, "Zulip") {
		t.Fatalf("extra text not composed: %q", withExtra)
	}
	if Resolve("anything", true, "", "") != "" {
		t.Fatal("disabled injection must produce nothing")
	}
	withCatalog := Resolve("", false, "<available_skills>x</available_skills>", "")
	if !strings.Contains(withCatalog, "available_skills") {
		t.Fatalf("catalog not composed: %q", withCatalog)
	}
}

func TestSentinelInstruction(t *testing.T) {
	if got := Resolve("", false, "", ""); strings.Contains(got, "ambiently") {
		t.Fatalf("no sentinel must produce no abstain instruction: %q", got)
	}
	got := Resolve("", false, "", "<<SILENT>>")
	if !strings.Contains(got, "<<SILENT>>") || !strings.Contains(got, "ambiently") {
		t.Fatalf("instruction = %q", got)
	}
}
