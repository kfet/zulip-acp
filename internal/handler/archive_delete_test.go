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
	hh.h.Handle(context.Background(), reactionEvent(user, msgID, defaultDeleteEmoji, zulipproto.ReactionAdd))
}

// TestArchiveWarningIsPreReacted: the relay adds both answers to its
// own warning, so the user confirms with one tap — and the echo of
// those reactions, from the bot user, confirms nothing.
func TestArchiveWarningIsPreReacted(t *testing.T) {
	hh, own := archiveHarness(t, func(c *Config) { c.DeleteReady = true })
	conv := archiveConv(t, hh)
	hh.tap(humanID, own)
	prompt, ok := hh.armed(conv.ID)
	if !ok {
		t.Fatal("nothing armed")
	}
	hh.h.echoWG.Wait()
	hh.z.mu.Lock()
	added := slices.Clone(hh.z.reactAdd)
	hh.z.mu.Unlock()
	for _, e := range []string{archiveEmoji, defaultDeleteEmoji} {
		if !slices.Contains(added, fmt.Sprintf("%d:%s", prompt, e)) {
			t.Fatalf("no :%s: pre-reaction on %d: %v", e, prompt, added)
		}
	}
	if body := hh.z.body(prompt); !strings.Contains(body, ":"+defaultDeleteEmoji+":") || !strings.Contains(body, "delete the topic") || strings.Contains(body, "config block") {
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
	hh.h.echoWG.Wait()
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

// TestDeleteConfirmCustomEmoji: a configured delete emoji replaces
// :fire: in the pre-reaction, the warning and the confirm matcher.
func TestDeleteConfirmCustomEmoji(t *testing.T) {
	hh, _ := archiveHarness(t, func(c *Config) { c.DeleteReady = true; c.DeleteEmoji = "bomb" })
	conv := archiveConv(t, hh)
	hh.deliver(t, "t", "!archive")
	prompt, _ := hh.armed(conv.ID)
	hh.h.echoWG.Wait()
	hh.z.mu.Lock()
	added := slices.Clone(hh.z.reactAdd)
	hh.z.mu.Unlock()
	if !slices.Contains(added, fmt.Sprintf("%d:bomb", prompt)) || slices.Contains(added, fmt.Sprintf("%d:fire", prompt)) {
		t.Fatalf("pre-reactions = %v", added)
	}
	if body := hh.z.body(prompt); !strings.Contains(body, ":bomb:") || strings.Contains(body, ":fire:") {
		t.Fatalf("warning = %q", body)
	}
	if !strings.Contains(archiveHelp(hh.h.deleteEmoji()), ":bomb:") {
		t.Fatal("help does not name :bomb:")
	}
	hh.fireTap(humanID, prompt)
	if len(hh.z.deletedTopics()) != 0 {
		t.Fatal(":fire: deleted with a custom delete emoji")
	}
	hh.h.Handle(context.Background(), reactionEvent(humanID, prompt, "bomb", zulipproto.ReactionAdd))
	if got := hh.z.deletedTopics(); len(got) != 1 {
		t.Fatalf("deletes = %v", got)
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
		{"refused: no delete block", &zulipproto.APIError{Status: 400, Msg: "Must be an organization administrator"}, "`delete` config block"},
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
			if !hh.logged("failed (") {
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
		hh.h.deleteConversation(context.Background(), gone, "nobody-here", "someone", humanID, 1)
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

// fakeDeleter is the second client of the `delete` config block.
type fakeDeleter struct {
	got []string
	err error
}

func (d *fakeDeleter) DeleteTopic(_ context.Context, streamID int64, topic string) error {
	if d.err != nil {
		return d.err
	}
	d.got = append(d.got, fmt.Sprintf("%d:%s", streamID, topic))
	return nil
}

// TestFireNotOfferedWhenDeleteCannotWork: without a delete client or
// an admin relay bot, the warning carries :wastebasket: only.
func TestFireNotOfferedWhenDeleteCannotWork(t *testing.T) {
	hh, own := archiveHarness(t, nil)
	conv := archiveConv(t, hh)
	hh.tap(humanID, own)
	prompt, _ := hh.armed(conv.ID)
	hh.h.echoWG.Wait()
	hh.z.mu.Lock()
	added := slices.Clone(hh.z.reactAdd)
	hh.z.mu.Unlock()
	if slices.Contains(added, fmt.Sprintf("%d:%s", prompt, defaultDeleteEmoji)) {
		t.Fatalf(":fire: pre-added with no way to delete: %v", added)
	}
	if !slices.Contains(added, fmt.Sprintf("%d:%s", prompt, archiveEmoji)) {
		t.Fatalf("no :wastebasket: pre-added: %v", added)
	}
	if body := hh.z.body(prompt); !strings.Contains(body, "`delete` config block") {
		t.Fatalf("warning does not say what the delete needs: %q", body)
	}
}

// TestDeleteUsesTheDeleteClient: with the `delete` block, the second
// client deletes and the relay bot does not.
func TestDeleteUsesTheDeleteClient(t *testing.T) {
	d := &fakeDeleter{}
	hh, own := archiveHarness(t, func(c *Config) { c.Deleter = d; c.DeleteReady = true })
	conv := archiveConv(t, hh)
	hh.tap(humanID, own)
	prompt, _ := hh.armed(conv.ID)
	hh.fireTap(humanID, prompt)
	if len(d.got) != 1 || d.got[0] != "4:t" {
		t.Fatalf("delete client calls = %v", d.got)
	}
	if len(hh.z.deletedTopics()) != 0 {
		t.Fatalf("the relay bot deleted: %v", hh.z.deletedTopics())
	}
	if !hh.logged(fmt.Sprintf("warning %d", prompt)) {
		t.Fatalf("delete not logged with its warning id: %v", hh.logs)
	}
}

// TestDeleteClientRefused: a refusal of the configured delete bot
// names the realm setting it is missing.
func TestDeleteClientRefused(t *testing.T) {
	d := &fakeDeleter{err: &zulipproto.APIError{Status: 400, Msg: "Insufficient permission"}}
	hh, own := archiveHarness(t, func(c *Config) { c.Deleter = d; c.DeleteReady = true })
	conv := archiveConv(t, hh)
	hh.tap(humanID, own)
	prompt, _ := hh.armed(conv.ID)
	hh.fireTap(humanID, prompt)
	if got := hh.z.lastBody(); !strings.Contains(got, "can_delete_any_message_group") {
		t.Fatalf("reply = %q", got)
	}
}

// TestDeleteOwnersOnly: a non-owner's :fire: deletes nothing and
// leaves the arm for an owner; an owner's deletes.
func TestDeleteOwnersOnly(t *testing.T) {
	d := &fakeDeleter{}
	const owner = 777
	hh, own := archiveHarness(t, func(c *Config) {
		c.Deleter, c.DeleteReady, c.DeleteOwnersOnly = d, true, true
		c.OpsOwners = []int64{owner}
	})
	conv := archiveConv(t, hh)
	hh.tap(humanID, own)
	prompt, _ := hh.armed(conv.ID)
	hh.fireTap(humanID, prompt)
	if len(d.got) != 0 {
		t.Fatalf("a non-owner deleted: %v", d.got)
	}
	if got := hh.z.lastBody(); !strings.Contains(got, "Only an owner") {
		t.Fatalf("reply = %q", got)
	}
	if _, still := hh.armed(conv.ID); !still {
		t.Fatal("a refused :fire: consumed the arm")
	}
	hh.fireTap(owner, prompt)
	if len(d.got) != 1 {
		t.Fatalf("the owner could not delete: %v", d.got)
	}
}
