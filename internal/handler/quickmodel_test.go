package handler

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/kfet/acp-kit/client"
	"github.com/kfet/zulip-acp/internal/journal"
	"github.com/kfet/zulip-acp/internal/zulipproto"
)

var testQuick = []QuickModel{
	{Emoji: "brain", Model: "anthropic-sub/opus", Label: "Anthropic sub · Opus"},
	{Emoji: "fish", Model: "sakana/ultra", Label: "Sakana · Ultra"},
	{Emoji: "star", Model: "astra/best"},
}

// quickHarness has the switcher on, ambient reactions as given, and one
// engaged conversation in #fleet > "t". It returns the relay's last
// message there.
func quickHarness(t *testing.T, reactions bool) (*harness, int64) {
	t.Helper()
	a := newAgent("hello")
	a.models = []client.ModelInfo{{ID: "anthropic-sub/opus"}, {ID: "sakana/ultra"}, {ID: "astra/best"}}
	a.model = "anthropic-sub/opus"
	hh := newHarness(t, a, func(c *Config) {
		c.Reactions = reactions
		c.QuickModels = testQuick
		c.QuickMenuEmoji = "gear"
		c.QuickSweepEmoji = "www"
	})
	hh.deliver(t, "t", mention("hi"))
	return hh, hh.z.lastID()
}

func (hh *harness) qtap(id int64, emoji string) {
	hh.h.Handle(context.Background(), reactionEvent(humanID, id, emoji, zulipproto.ReactionAdd))
}

func (hh *harness) hasReaction(id int64, emoji string) bool {
	added, _ := hh.z.reactions()
	want := strconv.FormatInt(id, 10) + ":" + emoji
	for _, r := range added {
		if r == want {
			return true
		}
	}
	return false
}

func (hh *harness) override(t *testing.T, topic string) string {
	t.Helper()
	conv, ok := hh.j.Lookup(journal.Channel(4, topic))
	if !ok {
		t.Fatalf("no conversation in %q", topic)
	}
	id, _ := hh.h.modelOverride(conv.ID)
	return id
}

func TestQuickMenuSwitchAndSweep(t *testing.T) {
	hh, own := quickHarness(t, true)
	hh.deliver(t, "u", mention("hi"))
	prompts := len(hh.a.prompted())

	hh.qtap(own, "gear")
	menu := hh.z.lastID()
	body := hh.z.body(menu)
	for _, want := range []string{
		"**Switch model** · now: :brain: Anthropic sub · Opus",
		"\n:fish: Sakana · Ultra",
		"\n:star: astra/best",
		"*Tap once: this topic. Tap :www: on the confirm: all sessions.*",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("menu lacks %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "not offered") {
		t.Fatalf("every model is offered: %s", body)
	}
	for _, q := range testQuick {
		if !hh.hasReaction(menu, q.Emoji) {
			t.Fatalf("menu lacks the pre-added :%s:", q.Emoji)
		}
	}
	// The relay's own pre-added reactions are ignored.
	hh.h.Handle(context.Background(), reactionEvent(botID, menu, "fish", zulipproto.ReactionAdd))
	if got := hh.override(t, "t"); got != "" {
		t.Fatalf("the relay's own reaction switched the model to %q", got)
	}

	hh.qtap(menu, "fish")
	if got := hh.override(t, "t"); got != "sakana/ultra" {
		t.Fatalf("override = %q", got)
	}
	confirm := hh.z.lastID()
	if got := hh.z.body(confirm); got != ":fish: → **Sakana · Ultra** for this topic, from the next turn.\n*Tap :www: to apply to all sessions.*" {
		t.Fatalf("confirm = %q", got)
	}
	if !hh.hasReaction(confirm, "www") {
		t.Fatal("confirm lacks the pre-added :www:")
	}
	if got := hh.override(t, "u"); got != "" {
		t.Fatalf("a topic switch leaked to another topic: %q", got)
	}

	// The menu shows the new current model.
	hh.qtap(confirm, "gear")
	if body := hh.z.body(hh.z.lastID()); !strings.Contains(body, "now: :fish: Sakana · Ultra") {
		t.Fatalf("menu after switch = %s", body)
	}

	hh.qtap(confirm, "www")
	if got := hh.z.body(hh.z.lastID()); !strings.Contains(got, ":fish: → **Sakana · Ultra** for all 2 session(s) and new topics") {
		t.Fatalf("sweep result = %q", got)
	}
	if got := hh.override(t, "u"); got != "sakana/ultra" {
		t.Fatalf("sweep did not reach topic u: %q", got)
	}
	if len(hh.a.prompted()) != prompts {
		t.Fatal("a switcher reaction reached the agent")
	}
	hh.h.reactMu.Lock()
	pending := len(hh.h.reactPending)
	hh.h.reactMu.Unlock()
	if pending != 0 {
		t.Fatal("a switcher reaction was queued for the agent")
	}

	// A new topic starts on the swept default, and the session gets it.
	hh.deliver(t, "v", mention("hi"))
	if got := hh.override(t, "v"); got != "sakana/ultra" {
		t.Fatalf("new topic override = %q", got)
	}
	found := false
	for _, s := range hh.a.setModel {
		if strings.HasSuffix(s, "=sakana/ultra") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no session was switched: %v", hh.a.setModel)
	}
}

func TestQuickRemovalAndForeignMessagesDoNothing(t *testing.T) {
	hh, own := quickHarness(t, false)
	n := hh.z.nextID()
	hh.h.Handle(context.Background(), reactionEvent(humanID, own, "gear", zulipproto.ReactionRemove))
	hh.h.Handle(context.Background(), reactionEvent(humanID, own, "fish", zulipproto.ReactionRemove))
	// Any other emoji is dropped at once with ambient reactions off.
	hh.qtap(own, "tada")
	// A human's message is never a switcher target.
	hh.z.mu.Lock()
	hh.z.messages[777] = zulipproto.Message{ID: 777, SenderID: humanID, StreamID: 4, Topic: "t", Type: zulipproto.MessageTypeStream}
	hh.z.mu.Unlock()
	hh.qtap(777, "gear")
	if hh.z.nextID() != n {
		t.Fatal("something was posted")
	}
	if got := hh.override(t, "t"); got != "" {
		t.Fatalf("override = %q", got)
	}
	// A removal on a confirmation is consumed too.
	hh.qtap(own, "fish")
	confirm := hh.z.lastID()
	n = hh.z.nextID()
	hh.h.Handle(context.Background(), reactionEvent(humanID, confirm, "www", zulipproto.ReactionRemove))
	if hh.z.nextID() != n {
		t.Fatal("removing :www: swept")
	}
}

func TestQuickUnknownModel(t *testing.T) {
	hh, own := quickHarness(t, false)
	hh.a.mu.Lock()
	hh.a.models = []client.ModelInfo{{ID: "anthropic-sub/opus"}}
	hh.a.model = "other/x"
	hh.a.mu.Unlock()
	hh.qtap(own, "gear")
	body := hh.z.body(hh.z.lastID())
	if !strings.Contains(body, "now: `other/x`") || !strings.Contains(body, ":fish: Sakana · Ultra *(not offered by the agent)*") {
		t.Fatalf("menu = %s", body)
	}
	if !hh.logged("WARN quick_models :fish:") {
		t.Fatal("no warning logged")
	}
	hh.qtap(own, "fish")
	if got := hh.z.body(hh.z.lastID()); !strings.Contains(got, ":fish: cannot switch to **Sakana · Ultra**: unknown model") {
		t.Fatalf("reply = %q", got)
	}
	if got := hh.override(t, "t"); got != "" {
		t.Fatalf("override = %q", got)
	}
}

func TestQuickSweepUnknownModel(t *testing.T) {
	hh, own := quickHarness(t, false)
	hh.qtap(own, "fish")
	confirm := hh.z.lastID()
	hh.a.mu.Lock()
	hh.a.models = []client.ModelInfo{{ID: "anthropic-sub/opus"}}
	hh.a.mu.Unlock()
	hh.qtap(confirm, "www")
	if got := hh.z.body(hh.z.lastID()); !strings.Contains(got, "Cannot apply to all sessions: unknown model") {
		t.Fatalf("reply = %q", got)
	}
}

func TestQuickMenuAgentDefault(t *testing.T) {
	hh, own := quickHarness(t, false)
	hh.a.mu.Lock()
	hh.a.models, hh.a.model = nil, ""
	hh.a.mu.Unlock()
	hh.qtap(own, "gear")
	if body := hh.z.body(hh.z.lastID()); !strings.Contains(body, "now: agent default") {
		t.Fatalf("menu = %s", body)
	}
}

func TestQuickSweepUnlistedModel(t *testing.T) {
	// A confirmation is bound to a model, not an entry; a sweep of a
	// model no entry names still reports it, under the sweep emoji.
	hh, own := quickHarness(t, false)
	conv, _ := hh.j.Lookup(journal.Channel(4, "t"))
	hh.h.quickConfirms.put(own, "astra/best")
	hh.h.cfg.QuickModels = testQuick[:2]
	hh.qtap(own, "www")
	if got := hh.z.body(hh.z.lastID()); !strings.Contains(got, ":www: → **astra/best** for all 1 session(s)") {
		t.Fatalf("reply = %q", got)
	}
	if id, _ := hh.h.modelOverride(conv.ID); id != "astra/best" {
		t.Fatalf("override = %q", id)
	}
}

func TestQuickPostFailures(t *testing.T) {
	hh, own := quickHarness(t, false)
	hh.z.mu.Lock()
	hh.z.reactErr = errors.New("no")
	hh.z.mu.Unlock()
	hh.qtap(own, "gear")
	if !hh.logged("adding :brain: to the model switcher") {
		t.Fatal("reaction failure not logged")
	}
	hh.z.mu.Lock()
	hh.z.sendErr = errors.New("down")
	hh.z.mu.Unlock()
	hh.qtap(own, "fish")
	if !hh.logged("posting the model switcher") {
		t.Fatal("post failure not logged")
	}
	if _, ok := hh.h.quickConfirms.get(0); ok {
		t.Fatal("a failed post was recorded as a confirmation")
	}
}

func TestQuickDefaultModelFile(t *testing.T) {
	hh, own := quickHarness(t, false)
	path := hh.h.defaultModelPath()
	// An unreadable default is logged and ignored.
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if hh.h.defaultModel() != "" || !hh.logged("reading the default model") {
		t.Fatal("unreadable default not reported")
	}
	// An unwritable state dir fails the default, not the sweep.
	hh.qtap(own, "fish")
	confirm := hh.z.lastID()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(path)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	hh.qtap(confirm, "www")
	if !hh.logged("persisting the default model") {
		t.Fatal("default write failure not logged")
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path+".tmp", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := hh.h.setDefaultModel("x"); err == nil {
		t.Fatal("want rename error")
	}
}

func TestQuickOffIgnoresEverything(t *testing.T) {
	hh := newHarness(t, newAgent("hello"), nil)
	if hh.h.isQuickEmoji("gear") {
		t.Fatal("gear is a switcher emoji with the switcher off")
	}
	if hh.h.quickReaction(context.Background(), journal.Conv{}, reactionEvent(humanID, 1, "gear", zulipproto.ReactionAdd), nil) {
		t.Fatal("consumed with the switcher off")
	}
	hh.h.applyDefaultModel("c1")
	if _, ok := hh.h.modelOverride("c1"); ok {
		t.Fatal("default applied with the switcher off")
	}
}

func TestQuickHelpLine(t *testing.T) {
	hh := cmdHarness(t, newAgent("x"), func(c *Config) {
		c.QuickModels, c.QuickMenuEmoji, c.QuickSweepEmoji = testQuick, "gear", "www"
	})
	hh.deliver(t, "t", mention("!help"))
	if got := hh.z.body(hh.z.lastID()); !strings.Contains(got, "react :gear: on a relay message — model switcher menu; :brain: :fish: :star: switches this topic, then :www: on the confirm") {
		t.Fatalf("help = %s", got)
	}
}

func TestQuickOtherEmojiStaysAmbientAndOverrideWins(t *testing.T) {
	hh, own := quickHarness(t, true)
	hh.react(t, reactionEvent(humanID, own, "tada", zulipproto.ReactionAdd))
	if p := hh.a.prompted(); !strings.Contains(p[len(p)-1], ":tada:") {
		t.Fatalf("an ordinary reaction did not reach the agent: %q", p)
	}
	// A topic's own choice wins over the swept default.
	hh.qtap(own, "fish")
	hh.qtap(hh.z.lastID(), "www")
	hh.qtap(own, "brain")
	hh.deliver(t, "t", mention("again"))
	if got := hh.override(t, "t"); got != "anthropic-sub/opus" {
		t.Fatalf("override = %q", got)
	}
}

func TestQuickSweepOnNoConfirmationOnlyReplies(t *testing.T) {
	hh, own := quickHarness(t, true)
	prompts := len(hh.a.prompted())
	hh.qtap(own, "www")
	if got := hh.z.body(hh.z.lastID()); !strings.Contains(got, "only on a switch confirmation") {
		t.Fatalf("reply = %q", got)
	}
	hh.h.reactMu.Lock()
	pending := len(hh.h.reactPending)
	hh.h.reactMu.Unlock()
	if pending != 0 || len(hh.a.prompted()) != prompts {
		t.Fatal(":www: reached the agent")
	}
}

func TestQuickDefaultNoLongerOffered(t *testing.T) {
	hh, own := quickHarness(t, false)
	hh.qtap(own, "fish")
	hh.qtap(hh.z.lastID(), "www")
	hh.a.mu.Lock()
	hh.a.models = []client.ModelInfo{{ID: "anthropic-sub/opus"}}
	hh.a.mu.Unlock()
	hh.deliver(t, "w", mention("hi"))
	if got := hh.override(t, "w"); got != "" {
		t.Fatalf("a dead default was applied: %q", got)
	}
	if !hh.logged("WARN the swept default model is not usable") {
		t.Fatal("no warning")
	}
}
