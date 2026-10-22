package handler

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kfet/zulip-acp/internal/journal"
	"github.com/kfet/zulip-acp/internal/zulipproto"
)

// fireTap feeds one :fire: reaction.
func (hh *harness) fireTap(user, msgID int64) {
	hh.h.Handle(context.Background(), reactionEvent(user, msgID, deleteEmoji, zulipproto.ReactionAdd))
}

// TestArchiveWarningIsPreReacted: the relay adds both answers to its
// own warning, so the user confirms with one tap — and the echo of
// those reactions, from the bot user, confirms nothing.
func TestArchiveWarningIsPreReacted(t *testing.T) {
	hh, own := archiveHarness(t, nil)
	conv := archiveConv(t, hh)
	hh.tap(humanID, own)
	prompt, ok := hh.armed(conv.ID)
	if !ok {
		t.Fatal("nothing armed")
	}
	hh.z.mu.Lock()
	added := slices.Clone(hh.z.reactAdd)
	hh.z.mu.Unlock()
	for _, e := range []string{archiveEmoji, deleteEmoji} {
		if !slices.Contains(added, fmt.Sprintf("%d:%s", prompt, e)) {
			t.Fatalf("no :%s: pre-reaction on %d: %v", e, prompt, added)
		}
	}
	if body := hh.z.body(prompt); !strings.Contains(body, ":"+deleteEmoji+":") || !strings.Contains(body, "delete the topic") {
		t.Fatalf("warning does not offer the delete: %q", body)
	}
	// The bot's own reactions echo back; neither may act.
	hh.tap(botID, prompt)
	hh.fireTap(botID, prompt)
	if _, still := hh.armed(conv.ID); !still || len(hh.z.moved()) != 0 || len(hh.z.deletedTopics()) != 0 {
		t.Fatalf("the relay's own pre-reaction acted: moves %v, deletes %v", hh.z.moved(), hh.z.deletedTopics())
	}
}

// TestArchivePreReactionFailureIsLogged: a failed pre-reaction costs
// convenience only; the arm stands.
func TestArchivePreReactionFailureIsLogged(t *testing.T) {
	hh, own := archiveHarness(t, nil)
	conv := archiveConv(t, hh)
	hh.z.mu.Lock()
	hh.z.reactErr = errors.New("boom")
	hh.z.mu.Unlock()
	hh.tap(humanID, own)
	if _, ok := hh.armed(conv.ID); !ok {
		t.Fatal("nothing armed")
	}
	if !hh.logged("could not pre-add") {
		t.Fatalf("logs = %v", hh.logs)
	}
}

// TestDeleteConfirm: :fire: on the warning ends the conversation and
// deletes the topic; nothing is moved.
func TestDeleteConfirm(t *testing.T) {
	hh, _ := archiveHarness(t, nil)
	conv := archiveConv(t, hh)
	hh.deliver(t, "t", "!archive")
	prompt, _ := hh.armed(conv.ID)
	hh.fireTap(humanID, prompt)
	if got := hh.z.deletedTopics(); len(got) != 1 || got[0] != "4:t" {
		t.Fatalf("deletes = %v", got)
	}
	if len(hh.z.moved()) != 0 {
		t.Fatalf("a delete also moved: %v", hh.z.moved())
	}
	if _, still := hh.armed(conv.ID); still {
		t.Fatal("the arm survived its confirmation")
	}
	if fresh := archiveConv(t, hh); fresh.ID == conv.ID {
		t.Fatal("the conversation was not retired")
	}
	if !hh.logged("deleted") {
		t.Fatalf("logs = %v", hh.logs)
	}
}

// TestDeleteUsesTheLiveTopic: a resolve since the arm renamed the topic;
// the delete must name the topic as it is now, read back from the warning.
func TestDeleteUsesTheLiveTopic(t *testing.T) {
	hh, _ := archiveHarness(t, nil)
	conv := archiveConv(t, hh)
	hh.deliver(t, "t", "!archive")
	prompt, _ := hh.armed(conv.ID)
	hh.z.mu.Lock()
	m := hh.z.messages[prompt]
	m.Topic = "✔ t"
	hh.z.messages[prompt] = m
	hh.z.mu.Unlock()
	hh.fireTap(humanID, prompt)
	if got := hh.z.deletedTopics(); len(got) != 1 || got[0] != "4:✔ t" {
		t.Fatalf("deletes = %v", got)
	}
}

// TestDeleteNeverArms: :fire: on the relay's last message is ordinary
// signal, not an arming.
func TestDeleteNeverArms(t *testing.T) {
	hh, own := archiveHarness(t, nil)
	conv := archiveConv(t, hh)
	hh.fireTap(humanID, own)
	if _, ok := hh.armed(conv.ID); ok {
		t.Fatal(":fire: armed a confirmation")
	}
	if len(hh.z.deletedTopics()) != 0 {
		t.Fatalf("deleted: %v", hh.z.deletedTopics())
	}
}

// TestDeleteLapsedArm: a :fire: after the TTL deletes nothing and does
// not start a fresh cycle.
func TestDeleteLapsedArm(t *testing.T) {
	now := time.Now()
	hh, own := archiveHarness(t, func(c *Config) { c.Now = func() time.Time { return now } })
	conv := archiveConv(t, hh)
	hh.tap(humanID, own)
	prompt, _ := hh.armed(conv.ID)
	now = now.Add(archiveConfirmTTL + time.Second)
	hh.fireTap(humanID, prompt)
	if len(hh.z.deletedTopics()) != 0 {
		t.Fatalf("a lapsed arm was honoured: %v", hh.z.deletedTopics())
	}
	if _, ok := hh.armed(conv.ID); ok {
		t.Fatal("a lapsed :fire: re-armed")
	}
}

// TestDeleteFailures: every way the delete can fail tells the topic.
func TestDeleteFailures(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"refused: no admin role", &zulipproto.APIError{Status: 400, Msg: "Must be an organization administrator"}, "organisation administrator"},
		{"transport failure", errors.New("boom"), "deleting the topic failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hh, own := archiveHarness(t, nil)
			conv := archiveConv(t, hh)
			hh.tap(humanID, own)
			prompt, _ := hh.armed(conv.ID)
			hh.z.mu.Lock()
			hh.z.topicDeleteErr = tc.err
			hh.z.mu.Unlock()
			hh.fireTap(humanID, prompt)
			if got := hh.z.lastBody(); !strings.Contains(got, tc.want) {
				t.Fatalf("reply = %q", got)
			}
			if !hh.logged("delete_topic failed") {
				t.Fatalf("logs = %v", hh.logs)
			}
		})
	}

	t.Run("the journal cannot record the retirement", func(t *testing.T) {
		hh, own := archiveHarness(t, nil)
		conv := archiveConv(t, hh)
		hh.tap(humanID, own)
		prompt, _ := hh.armed(conv.ID)
		hh.breakJournal(t)
		hh.fireTap(humanID, prompt)
		if len(hh.z.deletedTopics()) != 0 {
			t.Fatalf("deleted after a failed retirement: %v", hh.z.deletedTopics())
		}
		if got := hh.z.lastBody(); !strings.Contains(got, "did not delete the topic") {
			t.Fatalf("reply = %q", got)
		}
	})

	t.Run("the conversation is already gone", func(t *testing.T) {
		hh, _ := archiveHarness(t, nil)
		conv := archiveConv(t, hh)
		if _, _, _, err := hh.j.Retire(conv.Key); err != nil {
			t.Fatal(err)
		}
		// A key nobody holds: Retire reports it did not exist.
		gone := journal.Conv{ID: conv.ID, Key: journal.Channel(4, "nobody-here")}
		hh.h.deleteConversation(context.Background(), gone, "nobody-here", "someone")
		if len(hh.z.deletedTopics()) != 0 {
			t.Fatalf("deleted a topic it no longer held: %v", hh.z.deletedTopics())
		}
		if !hh.logged("no longer the conversation") {
			t.Fatalf("logs = %v", hh.logs)
		}
	})
}

// TestTopicOf: the reacted message's own topic wins over the journal's.
func TestTopicOf(t *testing.T) {
	k := journal.Channel(4, "t")
	if got := topicOf(k, nil); got != "t" {
		t.Fatalf("nil message: %q", got)
	}
	if got := topicOf(k, &zulipproto.Message{}); got != "t" {
		t.Fatalf("empty topic: %q", got)
	}
	if got := topicOf(k, &zulipproto.Message{Topic: "✔ t"}); got != "✔ t" {
		t.Fatalf("message topic: %q", got)
	}
}
