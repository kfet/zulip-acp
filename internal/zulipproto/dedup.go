package zulipproto

import "strconv"

// DefaultDedupWindow is how many already-dispatched event identities a
// Runner remembers. It only has to cover the OVERLAP of a queue swap —
// what a busy realm can produce between registering the replacement
// queue and deleting the old one, which is a drain, not a workday — so
// a few hundred is generous and the memory is trivial.
const DefaultDedupWindow = 512

// seenSet is a bounded FIFO set of event identities: it answers "have
// I dispatched this already?" and forgets the oldest entry once it is
// full, so it can never grow with uptime.
//
// It is touched only from the runner's own goroutine, so it needs no
// lock.
type seenSet struct {
	set  map[string]struct{}
	ring []string
	next int
}

func newSeenSet(capacity int) *seenSet {
	if capacity <= 0 {
		capacity = DefaultDedupWindow
	}
	return &seenSet{set: make(map[string]struct{}, capacity), ring: make([]string, capacity)}
}

// add records key and reports whether it is NEW — false means this
// event has been dispatched already and must be dropped.
func (s *seenSet) add(key string) bool {
	if _, dup := s.set[key]; dup {
		return false
	}
	if evicted := s.ring[s.next]; evicted != "" {
		delete(s.set, evicted)
	}
	s.ring[s.next] = key
	s.next = (s.next + 1) % len(s.ring)
	s.set[key] = struct{}{}
	return true
}

// len reports how many identities are currently remembered. For tests.
func (s *seenSet) len() int { return len(s.set) }

// eventKey derives a small comparable identity for an event, or false
// when the event carries nothing stable to compare.
//
// Zulip event ids are PER-QUEUE, so they say nothing about whether two
// queues delivered the same thing. MESSAGE ids are realm-global and
// stable, which is what makes the overlap of a queue swap
// de-duplicable at all.
//
// Types with no key — subscription, stream — are always dispatched.
// They carry op=add/remove/update against a set the handler applies
// idempotently, so a second sighting costs nothing, and they have no
// identity of their own to key on.
func eventKey(ev Event) (string, bool) {
	switch ev.Type {
	case EventMessage:
		if ev.Message == nil || ev.Message.ID == 0 {
			return "", false
		}
		return "message:" + strconv.FormatInt(ev.Message.ID, 10), true
	case EventUpdateMessage:
		if ev.MessageID == 0 {
			return "", false
		}
		// A message can be edited more than once, and every edit is a
		// separate event with the SAME message_id, so the edit's
		// timestamp is part of its identity. It has one-second
		// resolution: two edits of one message inside the same second
		// AND inside the swap overlap would collapse into one. That is
		// a far smaller error than dropping the message entirely,
		// which is what the alternative did.
		return "update:" + strconv.FormatInt(ev.MessageID, 10) + ":" + strconv.FormatInt(ev.EditTimestamp, 10), true
	case EventReaction:
		if ev.MessageID == 0 {
			return "", false
		}
		// A reaction event has no id of its own; who reacted, with
		// what, to which message, and whether they added or removed it
		// is the whole event.
		return "reaction:" + strconv.FormatInt(ev.MessageID, 10) + ":" +
			strconv.FormatInt(ev.UserID, 10) + ":" + ev.EmojiName + ":" + ev.Op, true
	}
	return "", false
}
