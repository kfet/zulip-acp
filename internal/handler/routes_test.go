package handler

import (
	"errors"
	"strings"
	"testing"

	"github.com/kfet/zulip-acp/internal/channels"
	"github.com/kfet/zulip-acp/internal/config"
	"github.com/kfet/zulip-acp/internal/journal"
	"github.com/kfet/zulip-acp/internal/zulipproto"
)

// routeHarness serves #fleet (4) and #work (5), with a route 4 → 5.
func routeHarness(t *testing.T, linkBack bool, tune ...func(*Config)) *harness {
	t.Helper()
	return newHarness(t, newAgent("ok"), func(c *Config) {
		c.Channels = channels.New(channels.Config{Explicit: map[int64]string{4: "fleet", 5: "work"}})
		c.Routes = map[int64]Route{4: {To: 5, LinkBack: linkBack}}
		for _, f := range tune {
			f(c)
		}
	})
}

func TestRouteMovesNewTopic(t *testing.T) {
	hh := routeHarness(t, true)
	hh.deliver(t, "deploy", "ship it tonight") // no mention
	if got := hh.z.moved(); len(got) != 1 || got[0] != "1:->5:deploy:change_one" {
		t.Fatalf("moves = %v", got)
	}
	if _, ok := hh.j.Lookup(journal.Channel(5, "deploy")); !ok {
		t.Fatal("no conversation in the destination")
	}
	if _, ok := hh.j.Lookup(journal.Channel(4, "deploy")); ok {
		t.Fatal("a conversation in the source")
	}
	var link bool
	for _, b := range hh.z.stored() {
		if b == "↪ moved to #**work>deploy**" {
			link = true
		}
	}
	if !link {
		t.Fatalf("no link back: %q", hh.z.stored())
	}
}

func TestRouteGeneralChatIsNamed(t *testing.T) {
	hh := routeHarness(t, false)
	hh.deliver(t, "", "Deploy the relay tonight")
	if got := hh.z.moved(); len(got) != 1 || got[0] != "1:->5:Deploy the relay tonight:change_one" {
		t.Fatalf("moves = %v", got)
	}
	for _, b := range hh.z.stored() {
		if strings.HasPrefix(b, "↪") {
			t.Fatalf("link back without link_back: %q", b)
		}
	}
}

func TestRouteDisambiguates(t *testing.T) {
	hh := routeHarness(t, false)
	hh.z.addTopic(5, "Deploy")
	hh.deliver(t, "deploy", "go")
	if got := hh.z.moved(); len(got) != 1 || got[0] != "1:->5:deploy (#1):change_one" {
		t.Fatalf("moves = %v", got)
	}

	hh = routeHarness(t, false)
	if _, err := hh.j.Ensure(journal.Channel(5, "deploy")); err != nil {
		t.Fatal(err)
	}
	hh.deliver(t, "deploy", "go")
	if got := hh.z.moved(); len(got) != 1 || got[0] != "1:->5:deploy (#1):change_one" {
		t.Fatalf("moves = %v", got)
	}

	// A failed topic list counts as not taken.
	hh = routeHarness(t, false)
	hh.z.topicsErr = errors.New("down")
	hh.deliver(t, "deploy", "go")
	if got := hh.z.moved(); len(got) != 1 || got[0] != "1:->5:deploy:change_one" || !hh.logged("listing the topics") {
		t.Fatalf("moves = %v", got)
	}
}

func TestRouteMoveFailsAnswersInPlace(t *testing.T) {
	hh := routeHarness(t, true)
	hh.z.moveErr = errors.New("not allowed")
	hh.deliver(t, "deploy", "go")
	if _, ok := hh.j.Lookup(journal.Channel(4, "deploy")); !ok || !hh.logged("answering in place") {
		t.Fatal("no answer in place")
	}
}

func TestRouteLinkBackFailureLogged(t *testing.T) {
	hh := routeHarness(t, true)
	hh.z.sendHook = func(c string) error {
		if strings.HasPrefix(c, "↪") {
			return errors.New("nope")
		}
		return nil
	}
	hh.deliver(t, "deploy", "go")
	if !hh.logged("posting the link back") {
		t.Fatal("not logged")
	}
}

func TestRouteSkipsEngagedTopicAndDM(t *testing.T) {
	hh := routeHarness(t, false)
	if _, err := hh.j.Ensure(journal.Channel(4, "old")); err != nil {
		t.Fatal(err)
	}
	hh.deliver(t, "old", mention("more"))
	if got := hh.z.moved(); len(got) != 0 {
		t.Fatalf("moved an engaged topic: %v", got)
	}
	if _, ok := hh.h.routeFor(&zulipproto.Message{Type: zulipproto.MessageTypePrivate}, journal.Key{}); ok {
		t.Fatal("route on a DM")
	}
}

// TestRouteWinsOverAutotopic: a channel with both moves across.
func TestRouteWinsOverAutotopic(t *testing.T) {
	hh := routeHarness(t, false, func(c *Config) {
		c.Channels = channels.New(channels.Config{Explicit: map[int64]string{4: "fleet", 5: "work"}, Autotopic: map[int64]string{4: "fleet"}})
	})
	hh.deliver(t, "", "hello there")
	if got := hh.z.moved(); len(got) != 1 || got[0] != "1:->5:hello there:change_one" {
		t.Fatalf("moves = %v", got)
	}
}

// TestRouteGeneralChatIsALobby: a conversation the journal holds in
// general chat must not stop the route, as for autotopic.
func TestRouteGeneralChatIsALobby(t *testing.T) {
	hh := routeHarness(t, false)
	if _, err := hh.j.Ensure(journal.Channel(4, "")); err != nil {
		t.Fatal(err)
	}
	hh.deliver(t, "", "hello there")
	if got := hh.z.moved(); len(got) != 1 || got[0] != "1:->5:hello there:change_one" {
		t.Fatalf("moves = %v", got)
	}
}

// TestRouteSkipsViewTopics: a reply in a view or notify topic stays.
func TestRouteSkipsViewTopics(t *testing.T) {
	hh := routeHarness(t, false, func(c *Config) {
		c.Views = []config.ResolvedView{{Key: "v", StreamID: 4, Topic: "Board"}}
		c.Notifies = []config.ResolvedNotify{{Key: "n", StreamID: 4, Topic: "alerts"}}
	})
	for _, topic := range []string{"board", "alerts"} {
		if _, ok := hh.h.routeFor(&zulipproto.Message{StreamID: 4, Topic: topic}, journal.Channel(4, topic)); ok {
			t.Fatalf("route in view topic %q", topic)
		}
	}
	if _, ok := hh.h.routeFor(&zulipproto.Message{StreamID: 4, Topic: "x"}, journal.Channel(4, "x")); !ok {
		t.Fatal("no route in an ordinary topic")
	}
}
