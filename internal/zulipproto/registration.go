package zulipproto

import (
	"encoding/json"
	"sort"
	"strings"
)

// A Zulip event queue is created ONCE, with a fixed server-side
// registration: the set of event types it carries and the narrow it is
// filtered by. Neither can be changed afterwards — there is no
// "re-register in place" call. A queue registered for
// ["message","update_message"] will never deliver a reaction event, no
// matter what the polling process believes it asked for.
//
// That matters because a graceful reload hands a LIVE queue from one
// process image to the next (see internal/reload). If the new image
// wants a different registration — a new event type, a changed narrow —
// resuming the inherited queue silently delivers the OLD registration
// forever, across every subsequent reload, until someone happens to do
// a hard restart. This is not hypothetical: it is exactly how emoji
// reaction delivery was dead in production on v0.18.1 while the feature
// was enabled and the code was correct.
//
// So the registration travels with the cursor, as a fingerprint, and
// the successor refuses to resume a queue whose fingerprint is not the
// one it wants.

// Registration is the server-side shape of an event queue: what
// /register was called with.
type Registration struct {
	EventTypes []string    `json:"event_types"`
	Narrow     [][2]string `json:"narrow"`
}

// RegistrationFingerprint renders a queue registration as canonical
// JSON: a stable, comparable string that also stays READABLE, so the
// mismatch can be described to the operator ("event types +reaction")
// rather than reported as two opaque hashes.
//
// Both lists are sorted, because neither ordering is meaningful to the
// server and a spurious mismatch would throw away a resumable queue —
// the cost of a false negative here is real, if small (see
// docs/graceful-reload.md).
func RegistrationFingerprint(eventTypes []string, narrow [][2]string) string {
	r := Registration{EventTypes: []string{}, Narrow: [][2]string{}}
	r.EventTypes = append(r.EventTypes, eventTypes...)
	sort.Strings(r.EventTypes)
	r.Narrow = append(r.Narrow, narrow...)
	sort.Slice(r.Narrow, func(i, j int) bool {
		if r.Narrow[i][0] != r.Narrow[j][0] {
			return r.Narrow[i][0] < r.Narrow[j][0]
		}
		return r.Narrow[i][1] < r.Narrow[j][1]
	})
	return mustJSON(r)
}

// DescribeRegistrationChange explains, in one operator-facing clause,
// why an inherited registration is not the one this image wants.
//
// old is whatever the predecessor image recorded: possibly "" (an
// upgrade FROM an image that never recorded one — the state every host
// is in before this fix lands) and possibly unparseable. Both are
// treated as "unknown, therefore different": resuming on a guess is how
// the defect this exists to prevent got a whole day of production.
func DescribeRegistrationChange(old, want string) string {
	if old == "" {
		return "the predecessor image recorded no registration, so it cannot be shown to match"
	}
	var o, w Registration
	if err := json.Unmarshal([]byte(old), &o); err != nil {
		return "the predecessor image recorded an unreadable registration"
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		// want is produced by RegistrationFingerprint in this same
		// process, so this is unreachable in practice; say something
		// truthful rather than nothing.
		return "the wanted registration is unreadable"
	}
	var parts []string
	if d := diffSets(o.EventTypes, w.EventTypes); d != "" {
		parts = append(parts, "event types "+d)
	}
	if RegistrationFingerprint(nil, o.Narrow) != RegistrationFingerprint(nil, w.Narrow) {
		parts = append(parts, "narrow "+narrowLabel(o.Narrow)+" → "+narrowLabel(w.Narrow))
	}
	if len(parts) == 0 {
		// Same content, different bytes — only reachable if the
		// predecessor wrote a differently formatted fingerprint.
		return "the recorded registration is formatted differently"
	}
	return strings.Join(parts, ", ")
}

// diffSets renders the change from old to want as "+a, +b -c", or ""
// when the sets are equal.
func diffSets(old, want []string) string {
	in := func(s []string, v string) bool {
		for _, x := range s {
			if x == v {
				return true
			}
		}
		return false
	}
	var parts []string
	for _, v := range want {
		if !in(old, v) {
			parts = append(parts, "+"+v)
		}
	}
	for _, v := range old {
		if !in(want, v) {
			parts = append(parts, "-"+v)
		}
	}
	return strings.Join(parts, " ")
}

func narrowLabel(n [][2]string) string {
	if len(n) == 0 {
		return "none"
	}
	parts := make([]string, 0, len(n))
	for _, p := range n {
		parts = append(parts, p[0]+":"+p[1])
	}
	return strings.Join(parts, "+")
}
