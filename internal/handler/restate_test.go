package handler

import (
	"context"
	"strings"
	"testing"

	acp "github.com/coder/acp-go-sdk"

	"github.com/kfet/zulip-acp/internal/journal"
	"github.com/kfet/zulip-acp/internal/rollover"
)

const restateA = `I researched each harness and asked the advisor. Here is the proposal for discussion. Nothing is built.

The core is a file format and one binary. The mem mcp command is the MCP server. The mem verb CLI is what adapters call. Both run the same code. The file format is the real contract: bookmarks JSONL in the fir format, briefings, and pending handoffs. Each adapter does five jobs: expose the tool as native, place the trigger, give the session id, inject the waiting briefing at start, and give a start fresh action.

Do you want changes to this design, or shall I run the four checks?`

const restateB = `I researched each harness and asked the advisor. This is the proposal for discussion. Nothing is built.

The core is a file format and one binary. The mem mcp command is the MCP server. The mem verb CLI is a CLI that adapters call. Both run the same code. The file format is the real contract: bookmarks JSONL in the fir format, briefings, and pending handoffs. Each adapter does 5 jobs: expose the tool as native, place the trigger, give the session id, inject the waiting briefing at start, and give a "start fresh" action.

Do you want changes to this design, or shall I run the four checks?`

func runRestate(t *testing.T, before, after string) (string, *fakeZulip) {
	t.Helper()
	z := newZulip()
	split, err := rollover.New(rollover.Config{Poster: &convPoster{client: z, key: journal.Channel(4, "t")}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	s := newStreamingSink(split, true)
	_ = s.OnUpdate(ctx, chunkNotification(before, nil))
	_ = split.Flush(ctx)
	_ = s.OnUpdate(ctx, toolCallNote())
	_ = s.OnUpdate(ctx, chunkNotification(after, nil))
	s.dropRestatement(ctx)
	if err := split.Close(ctx, ""); err != nil {
		t.Fatal(err)
	}
	return split.Transcript(), z
}

func TestSinkDropsRewordedRestatement(t *testing.T) {
	got, _ := runRestate(t, restateA, restateB)
	if strings.TrimSpace(got) != restateB {
		t.Fatalf("transcript = %q, want only the final wording", got)
	}
}

func TestSinkKeepsDistinctSegments(t *testing.T) {
	b := "The build passed on the build box. All 412 tests are green, and the coverage gate held at the same level as before. I tagged v1.2.3 and pushed it to the remote, so the release workflow now runs on CI and will publish the binaries."
	got, _ := runRestate(t, restateA, b)
	if !strings.Contains(got, "Nothing is built") || !strings.Contains(got, "release workflow") {
		t.Fatalf("distinct text lost: %q", got)
	}
}

func TestRestatesShortTextNever(t *testing.T) {
	if restates("Done.", "Done.") {
		t.Fatal("short text must not count")
	}
}

func TestSplitterRewriteShrinksChain(t *testing.T) {
	z := newZulip()
	split, err := rollover.New(rollover.Config{Budget: 300, Poster: &convPoster{client: z, key: journal.Channel(4, "t")}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	split.Append(strings.Repeat("line of text here\n", 60))
	_ = split.Flush(ctx)
	n := len(split.IDs())
	if err := split.Rewrite(ctx, "short\n"); err != nil {
		t.Fatal(err)
	}
	_ = split.Close(ctx, "")
	if n < 2 || len(split.IDs()) != 1 || split.Transcript() != "short\n" {
		t.Fatalf("before %d ids, after %v, raw %q", n, split.IDs(), split.Transcript())
	}
}

func TestSinkRecordsDrops(t *testing.T) {
	z := newZulip()
	split, _ := rollover.New(rollover.Config{Poster: &convPoster{client: z, key: journal.Channel(4, "t")}})
	ctx := context.Background()
	s := newStreamingSink(split, true)
	_ = s.OnUpdate(ctx, chunkNotification(restateA, nil))
	_ = s.OnUpdate(ctx, toolCallNote())
	_ = s.OnUpdate(ctx, chunkNotification(restateB, nil))
	s.dropRestatement(ctx)
	if d := s.Dropped(); len(d) != 1 || !strings.HasPrefix(d[0], "I researched") {
		t.Fatalf("dropped = %q", d)
	}
}

func TestDropNotes(t *testing.T) {
	if !strings.Contains(dropUserNote(1), "a passage") || !strings.Contains(dropUserNote(2), "2 passages") {
		t.Fatal("user note")
	}
	n := dropAgentNote([]string{strings.Repeat("x", 200), "line\nbreak"})
	if !strings.Contains(n, "[relay]") || !strings.Contains(n, "…") || !strings.Contains(n, "> line break") {
		t.Fatalf("agent note = %q", n)
	}
}

func TestHandlerReportsDroppedRestatement(t *testing.T) {
	agent := newAgent(restateB)
	agent.updates = []acp.SessionUpdate{toolCallNote().Update}
	agent.during = func() {
		agent.mu.Lock()
		sink := agent.sink
		agent.mu.Unlock()
		_ = sink.OnUpdate(context.Background(), chunkNotification(restateA, nil))
	}
	hh := newHarness(t, agent, nil)
	hh.deliver(t, "t", mention("design?"))
	all := strings.Join(hh.z.stored(), "\n")
	if !strings.Contains(all, "relay: removed a passage") || strings.Count(all, "I researched each harness") != 1 {
		t.Fatalf("stored = %q", all)
	}
	agent.mu.Lock()
	agent.during, agent.updates, agent.chunks = nil, nil, []string{"ok"}
	agent.mu.Unlock()
	hh.deliver(t, "t", mention("next"))
	if p := agent.prompts[1]; !strings.Contains(p, "[relay] In your last answer") {
		t.Fatalf("prompt = %q", p)
	}
}
