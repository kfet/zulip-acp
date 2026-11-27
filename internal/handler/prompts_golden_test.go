package handler

import (
	"flag"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/kfet/zulip-acp/internal/journal"
	"github.com/kfet/zulip-acp/internal/zulipproto"
)

var pngSizes = regexp.MustCompile(`\d+(\.\d+)? KB`)

var updateGolden = flag.Bool("update", false, "rewrite testdata/prompts.golden")

// TestPromptNotesGolden renders every note the relay adds to a prompt
// and compares the result with testdata/prompts.golden. The golden file
// was made before the text moved out of Go literals, so it proves the
// move changed no byte.
func TestPromptNotesGolden(t *testing.T) {
	var b strings.Builder
	add := func(name, s string) {
		b.WriteString("=== " + name + "\n" + s + "\n")
	}

	add("renameHint", renameHint("an auto topic"))
	add("literalSlash", literalSlash("/x is a path"))
	add("dropAgentNote", dropAgentNote([]string{"first\nrepeat", strings.Repeat("y", 200)}))

	plan := branchPlan{
		Actor:  &zulipproto.Message{SenderName: "Ada"},
		Parent: journal.Parent{Key: journal.Channel(4, "planning"), MessageID: 3},
		Text:   "go on",
	}
	other, own := plan, plan
	other.Author = "Bob"
	loop := branchHarness(t)
	own.Author = loop.h.cfg.BotFullName
	bare := cmdHarness(t, newAgent("done"), nil)
	for _, c := range []struct {
		name   string
		h      *harness
		p      branchPlan
		forked bool
	}{
		{"branch/loopback/fresh", loop, plan, false},
		{"branch/loopback/forked", loop, plan, true},
		{"branch/loopback/other", loop, other, false},
		{"branch/loopback/own", loop, own, false},
		{"branch/bare/fresh", bare, plan, false},
		{"branch/bare/forked", bare, plan, true},
	} {
		add(c.name, c.h.h.branchPrompt(c.p, "a topic", c.forked))
	}

	a := newAgent("ok")
	a.imageCap = true
	ih := inboxHarness(t, a, func(c *Config) { c.MaxInlineImagePixels = 392 })
	note, _ := ih.h.renderAttachments([]ingested{
		{ref: uploadRef{Path: "/user_uploads/1/a/x.bin"}, name: "x.bin", skip: "too big"},
		{ref: uploadRef{Path: "/user_uploads/1/b/y.txt"}, name: "y.txt", local: "/in/y.txt", mime: "text/plain", size: 12},
		{ref: uploadRef{Path: "/user_uploads/1/c/p.png"}, name: "p.png", local: "/in/p.png", mime: "image/png",
			size: len(realPNG(t, 600, 450)), data: realPNG(t, 600, 450)},
	})
	// The PNG encoder's output size changes between Go releases, so
	// the sizes of the image line are not part of the golden.
	add("attachments", pngSizes.ReplaceAllString(note, "N KB"))

	many := make([]string, MaxCatchupPerTopic+2)
	for i := range many {
		many[i] = fmt.Sprintf("[Ada] m%d", i)
	}
	add("catchup/few", catchupPrompt(&catchupTopic{prompts: many[:2], first: 1795800000}))
	add("catchup/many", catchupPrompt(&catchupTopic{prompts: many, first: 1795800000}))

	got := b.String()
	const path = "testdata/prompts.golden"
	if *updateGolden {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Errorf("prompt notes differ from %s:\n%s", path, got)
	}
}
