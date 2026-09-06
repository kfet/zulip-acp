package zulipproto

import (
	"fmt"
	"testing"
)

// TestSeenSetIsBoundedAndEvicts: the dedup window exists to cover the
// overlap of a queue swap, not the process lifetime. It must forget
// the oldest identity once full, so memory can never grow with uptime.
func TestSeenSetIsBoundedAndEvicts(t *testing.T) {
	s := newSeenSet(3)
	for i := range 3 {
		if !s.add(fmt.Sprintf("k%d", i)) {
			t.Fatalf("k%d was reported as already seen", i)
		}
	}
	if s.len() != 3 {
		t.Fatalf("len = %d, want 3", s.len())
	}
	if s.add("k1") {
		t.Fatal("k1 was reported as new on a second sighting")
	}
	// One more entry evicts the oldest (k0) and nothing else.
	if !s.add("k3") {
		t.Fatal("k3 was reported as already seen")
	}
	if s.len() != 3 {
		t.Fatalf("len = %d after eviction, want 3 — the window is not bounded", s.len())
	}
	if !s.add("k0") {
		t.Fatal("k0 was still remembered after being evicted")
	}
	if s.add("k3") {
		t.Fatal("k3 was forgotten too early")
	}
}

// TestNewSeenSetDefaultsItsWindow: a zero DedupWindow must not produce
// a zero-length ring, which would divide by zero on the first add.
func TestNewSeenSetDefaultsItsWindow(t *testing.T) {
	s := newSeenSet(0)
	if len(s.ring) != DefaultDedupWindow {
		t.Fatalf("ring = %d, want DefaultDedupWindow %d", len(s.ring), DefaultDedupWindow)
	}
	if !s.add("k") || s.add("k") {
		t.Fatal("a defaulted set does not dedup")
	}
}

// TestEventKey pins the identities the swap overlap is matched on.
// Zulip event ids are per-queue, so none of these may use ev.ID.
func TestEventKey(t *testing.T) {
	tests := []struct {
		name string
		ev   Event
		want string
		ok   bool
	}{{
		name: "message keys on the global message id",
		ev:   Event{ID: 7, Type: EventMessage, Message: &Message{ID: 500}},
		want: "message:500",
		ok:   true,
	}, {
		name: "a message event without a message has no identity",
		ev:   Event{ID: 7, Type: EventMessage},
	}, {
		name: "a message event with a zero message id has no identity",
		ev:   Event{ID: 7, Type: EventMessage, Message: &Message{}},
	}, {
		name: "an edit keys on the message id and the edit time",
		ev:   Event{ID: 8, Type: EventUpdateMessage, MessageID: 500, EditTimestamp: 1700},
		want: "update:500:1700",
		ok:   true,
	}, {
		name: "an edit without a message id has no identity",
		ev:   Event{ID: 8, Type: EventUpdateMessage},
	}, {
		name: "a reaction keys on who reacted with what, to what, and how",
		ev: Event{ID: 9, Type: EventReaction, MessageID: 500, UserID: 12,
			EmojiName: "wastebasket", Op: ReactionAdd},
		want: "reaction:500:12:wastebasket:add",
		ok:   true,
	}, {
		name: "a reaction without a message id has no identity",
		ev:   Event{ID: 9, Type: EventReaction, UserID: 12},
	}, {
		name: "a subscription event is dispatched unconditionally",
		ev:   Event{ID: 10, Type: EventSubscription, Op: "add"},
	}}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := eventKey(tc.ev)
			if got != tc.want || ok != tc.ok {
				t.Fatalf("eventKey() = %q, %v; want %q, %v", got, ok, tc.want, tc.ok)
			}
		})
	}
}

// TestEventKeyDistinguishesTwoEditsOfOneMessage: an edited message
// keeps its id across every edit, so keying on the message id alone
// would silently swallow the second edit.
func TestEventKeyDistinguishesTwoEditsOfOneMessage(t *testing.T) {
	first, _ := eventKey(Event{Type: EventUpdateMessage, MessageID: 500, EditTimestamp: 1700})
	second, _ := eventKey(Event{Type: EventUpdateMessage, MessageID: 500, EditTimestamp: 1760})
	if first == second {
		t.Fatalf("two edits of message 500 share the identity %q", first)
	}
	posted, _ := eventKey(Event{Type: EventMessage, Message: &Message{ID: 500}})
	if posted == first {
		t.Fatalf("a message and an edit of it share the identity %q", posted)
	}
}
