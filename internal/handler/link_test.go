package handler

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/kfet/zulip-acp/internal/channels"
	"github.com/kfet/zulip-acp/internal/zulipproto"
)

// --- parsing -------------------------------------------------------------

// TestMessageLinks covers the whole grammar. It is deliberately
// strict: a false positive costs an API call and injects a message
// nobody pointed at.
func TestMessageLinks(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want []int64
	}{
		{"nothing", "just a message", nil},
		{"mention link", "see #**ask-fir>Some topic@949**", []int64{949}},
		{"near link", "see https://z.example/#narrow/channel/9-x/topic/y/near/949", []int64{949}},
		{"near link in markdown", "see [this](https://z/#narrow/.../near/12) please", []int64{12}},
		{"plain channel mention is not a link", "over in #**ask-fir**", nil},
		{"topic mention without an id is not a link", "in #**ask-fir>Some topic**", nil},
		{"at in the topic name", "#**c>a@b topic@949**", []int64{949}},
		{"non-numeric id", "#**c>t@abc**", nil},
		{"zero id", "#**c>t@0**", nil},
		{"empty near", "/near/ nothing", nil},
		{"deduped", "#**c>t@7** and again #**c>t@7** and /near/7", []int64{7}},
		{"order preserved", "#**c>t@3** #**c>t@1**", []int64{3, 1}},
		{"capped", "#**c>t@1** #**c>t@2** #**c>t@3** #**c>t@4**", []int64{1, 2, 3}},
		{"capped across both forms", "#**c>t@1** #**c>t@2** /near/3 /near/4", []int64{1, 2, 3}},
		{"unterminated mention", "#**c>t@9", nil},
		{"no mention at all after the marker", "#** **", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := messageLinks(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("messageLinks(%q) = %v, want %v", tc.in, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("messageLinks(%q) = %v, want %v", tc.in, got, tc.want)
				}
			}
		})
	}
}

// --- hydration -----------------------------------------------------------

// linkHarness serves one channel and knows one linked message.
func linkHarness(t *testing.T) *harness {
	t.Helper()
	hh := cmdHarness(t, newAgent("done"), func(c *Config) {
		c.Channels = channels.New(channels.Config{Explicit: map[int64]string{4: "fleet"}})
	})
	hh.z.messages[949] = zulipproto.Message{
		ID: 949, SenderName: "Grace Hopper", Content: "the answer is in the compiler",
		StreamID: 4, Topic: "Some topic", Type: zulipproto.MessageTypeStream, Timestamp: 1756800000,
	}
	return hh
}

// TestLinkedMessageIsHydratedIntoThePrompt: "see this" means something.
func TestLinkedMessageIsHydratedIntoThePrompt(t *testing.T) {
	hh := linkHarness(t)
	hh.deliver(t, "planning", mention("what did we decide? #**fleet>Some topic@949**"))
	p := hh.a.prompted()
	if len(p) != 1 {
		t.Fatalf("prompts = %+v", p)
	}
	if !strings.Contains(p[0], "[linked] #**fleet>Some topic@949** — Grace Hopper, 2025-09-02") {
		t.Fatalf("prompt = %q", p[0])
	}
	if !strings.Contains(p[0], "the answer is in the compiler") {
		t.Fatalf("prompt = %q", p[0])
	}
}

// TestLinkedMessageIsHydratedOnce: re-linking the same message in a
// follow-up must not re-inject it turn after turn.
func TestLinkedMessageIsHydratedOnce(t *testing.T) {
	hh := linkHarness(t)
	hh.deliver(t, "planning", mention("see #**fleet>Some topic@949**"))
	hh.deliver(t, "planning", mention("as I said, #**fleet>Some topic@949**"))
	p := hh.a.prompted()
	if len(p) != 2 {
		t.Fatalf("prompts = %+v", p)
	}
	if !strings.Contains(p[0], "[linked]") {
		t.Fatalf("first prompt = %q", p[0])
	}
	if strings.Contains(p[1], "[linked]") {
		t.Fatalf("the same link was hydrated twice: %q", p[1])
	}
}

// TestLinkHydrationRefusesWhatTheAllowlistRefuses: the channel
// allowlist bounds what an agent can be shown, and a pasted link is
// not an exception to it.
func TestLinkHydrationRefusesWhatTheAllowlistRefuses(t *testing.T) {
	for _, tc := range []struct {
		name string
		msg  zulipproto.Message
	}{
		{
			name: "another channel, served or not",
			msg:  zulipproto.Message{ID: 950, StreamID: 77, Topic: "secret", Type: zulipproto.MessageTypeStream, Content: "x"},
		},
		{
			name: "a direct message",
			msg:  zulipproto.Message{ID: 950, Type: zulipproto.MessageTypePrivate, Content: "x"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hh := linkHarness(t)
			hh.z.messages[950] = tc.msg
			hh.deliver(t, "planning", mention("see /near/950"))
			p := hh.a.prompted()
			if len(p) != 1 || strings.Contains(p[0], "[linked]") {
				t.Fatalf("prompt = %+v", p)
			}
			if !hh.logged("not hydrating linked message 950: it is not in #fleet") {
				t.Fatalf("logs = %v", hh.logs)
			}
		})
	}
}

// TestLinkHydrationSurvivesAFailedFetch: a link to a deleted message
// must not fail the turn, grow an apology, or be remembered as
// hydrated — a transient failure has to be retryable.
func TestLinkHydrationSurvivesAFailedFetch(t *testing.T) {
	hh := linkHarness(t)
	hh.z.getErr = errors.New("no such message")
	hh.deliver(t, "planning", mention("see /near/949"))
	if p := hh.a.prompted(); len(p) != 1 || strings.Contains(p[0], "[linked]") {
		t.Fatalf("prompt = %+v", p)
	}
	if !hh.logged("not hydrating linked message 949") {
		t.Fatalf("logs = %v", hh.logs)
	}
	if _, seen := hh.h.linkMsgs.get(949); seen {
		t.Fatal("a failed fetch was remembered as hydrated, so the link can never be retried")
	}
}

// TestLinkHydrationTruncatesALongBody: hydration is unasked-for, so it
// must cost less than something the agent chose to fetch.
func TestLinkHydrationTruncatesALongBody(t *testing.T) {
	hh := linkHarness(t)
	m := hh.z.messages[949]
	m.Content = strings.Repeat("é", maxLinkRunes+500)
	hh.z.messages[949] = m
	hh.deliver(t, "planning", mention("see /near/949"))
	p := hh.a.prompted()
	if len(p) != 1 || !strings.Contains(p[0], "[truncated]") {
		t.Fatalf("prompt = %q", p)
	}
	if n := len([]rune(p[0])); n > maxLinkRunes+500 {
		t.Fatalf("the hydrated body was not bounded: %d runes", n)
	}
}

// TestLinkHydrationIsCappedPerMessage: a message pasting forty links
// must not spend forty API calls.
func TestLinkHydrationIsCappedPerMessage(t *testing.T) {
	hh := linkHarness(t)
	var refs []string
	for i := int64(1); i <= 40; i++ {
		hh.z.messages[900+i] = zulipproto.Message{
			ID: 900 + i, SenderName: "Grace", Content: fmt.Sprintf("body %d", i),
			StreamID: 4, Topic: "t", Type: zulipproto.MessageTypeStream,
		}
		refs = append(refs, fmt.Sprintf("/near/%d", 900+i))
	}
	hh.deliver(t, "planning", mention("look: "+strings.Join(refs, " ")))
	p := hh.a.prompted()
	if len(p) != 1 {
		t.Fatalf("prompts = %+v", p)
	}
	if n := strings.Count(p[0], "[linked]"); n != maxLinks {
		t.Fatalf("hydrated %d links, want %d", n, maxLinks)
	}
}

// TestLinkBlockFallsBackToTheSenderEmail: Zulip always sends a full
// name, but a message with none must still be attributable.
func TestLinkBlockFallsBackToTheSenderEmail(t *testing.T) {
	got := linkBlock(zulipproto.Message{ID: 1, SenderEmail: "bot@example.com", Content: "x"}, "fleet")
	if !strings.Contains(got, "bot@example.com") {
		t.Fatalf("linkBlock = %q", got)
	}
}

// TestHydrateLinksIsInertWithoutLinks costs nothing when there is
// nothing to fetch — no context, no API call, no allocation.
func TestHydrateLinksIsInertWithoutLinks(t *testing.T) {
	hh := linkHarness(t)
	if got := hh.h.hydrateLinks(context.Background(), "c1", &zulipproto.Message{Content: "plain prose"}); got != "" {
		t.Fatalf("hydrateLinks = %q", got)
	}
	if len(hh.z.gets) != 0 {
		t.Fatalf("a message with no links still cost %d fetch(es)", len(hh.z.gets))
	}
}

// TestLinkHydrationRefusesADMConversation: a direct message has no
// channel to measure a link against, and the reader-can-see-it check
// is entirely a channel comparison. Rather than invent a weaker rule
// for DMs, there is no hydration in one.
func TestLinkHydrationRefusesADMConversation(t *testing.T) {
	hh := cmdHarness(t, newAgent("done"), func(c *Config) {
		c.DMs = true
		c.Channels = channels.New(channels.Config{Explicit: map[int64]string{4: "fleet"}})
	})
	hh.z.messages[949] = zulipproto.Message{
		ID: 949, SenderName: "Grace", Content: "secret", StreamID: 4, Topic: "t",
		Type: zulipproto.MessageTypeStream,
	}
	hh.deliverDM(t, humanID, "see /near/949", humanID, botID)
	p := hh.a.prompted()
	if len(p) != 1 || strings.Contains(p[0], "[linked]") {
		t.Fatalf("prompt = %+v", p)
	}
	if len(hh.z.gets) != 0 {
		t.Fatal("a DM still spent a message read on a link")
	}
}

// TestLinkHydrationNeedsAServedChannel: the channel a link was posted
// in is what makes "whoever posted there can read there" true, so a
// channel the relay does not serve resolves nothing.
func TestLinkHydrationNeedsAServedChannel(t *testing.T) {
	hh := linkHarness(t)
	got := hh.h.hydrateLinks(context.Background(), "c1", &zulipproto.Message{
		StreamID: 77, Topic: "elsewhere", Type: zulipproto.MessageTypeStream, Content: "see /near/949",
	})
	if got != "" {
		t.Fatalf("hydrateLinks = %q", got)
	}
	if len(hh.z.gets) != 0 {
		t.Fatal("an unserved channel still spent a message read")
	}
}
