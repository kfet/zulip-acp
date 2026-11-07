package journal

// # Topic state
//
// A conversation can carry a STATE: what the user must do with the
// topic. The agent records it with the `topic_state` loopback tool,
// the user with `!state`, and `zulip-acp import-states` seeds it once.
// `!inbox` reads it to show the user which of many topics to open.
//
// The state belongs to the PLACE, not to the session: `!new` carries
// it to the fresh conversation, as it carries the parent pointer. It
// changes only when somebody records a change. Nothing resets it per
// turn; that is the point of it.
//
// Two activity facts go with it. LastActivity is when a human or the
// relay last wrote in the topic. ActivitySince counts the turns that
// started since the state was last recorded, so a reader can tell that
// a state may be stale.

import (
	"fmt"
	"slices"
	"strings"
)

// The topic states, in the order the `topic_state` tool lists them.
const (
	StateNeedsYou = "needs_you"
	StateFYI      = "fyi"
	StateWaiting  = "waiting"
	StateBlocked  = "blocked"
	StateDone     = "done"
)

// States is every topic state, in the order the tool lists them.
var States = []string{StateNeedsYou, StateFYI, StateWaiting, StateBlocked, StateDone}

// ValidState reports whether s is a topic state.
func ValidState(s string) bool { return slices.Contains(States, s) }

// Who recorded a state.
const (
	SetByAgent  = "agent"
	SetByUser   = "user"
	SetByImport = "import"
)

// MaxReasonRunes bounds TopicState.Reason. It is one line for the
// inbox, and the journal is rewritten whole on every commit.
const MaxReasonRunes = 300

// TopicState is the state of a conversation's topic.
type TopicState struct {
	// State is one of States.
	State string `json:"state"`
	// Reason is one line for the inbox: what the user must do, or the
	// outcome.
	Reason string `json:"reason,omitempty"`
	// SetAt is when it was recorded, in Unix milliseconds.
	SetAt int64 `json:"set_at"`
	// SetBy is SetByAgent, SetByUser or SetByImport.
	SetBy string `json:"set_by"`
}

// clampReason cuts a reason to one line of at most MaxReasonRunes and
// silences every Zulip mention in it ("@**x**" → "@_**x**", and the same
// for a group "@*x*"). A reason is posted in places its author did not
// write in — `!inbox`, and the parent topic of a branch — so it must
// not notify anybody.
func clampReason(s string) string {
	s = strings.ReplaceAll(s, "@*", "@_*")
	for i, r := range s {
		if r == '\n' || r == '\r' {
			s = s[:i]
			break
		}
	}
	if r := []rune(s); len(r) > MaxReasonRunes {
		s = string(r[:MaxReasonRunes-1]) + "…"
	}
	return s
}

// SetState records s as the state of conversation convID and resets
// its ActivitySince. SetAt is filled in when 0, and Reason is cut to
// one line.
//
// It returns the state the conversation had before (zero when it had
// none) and whether anything changed. Recording the same state with
// the same reason again is a no-op: nothing is written.
func (j *Journal) SetState(convID string, s TopicState) (prev TopicState, changed bool, err error) {
	if !ValidState(s.State) {
		return TopicState{}, false, fmt.Errorf("journal: %q is not a topic state", s.State)
	}
	s.Reason = clampReason(s.Reason)
	if s.SetAt == 0 {
		s.SetAt = nowMillis()
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	c, ok := j.byID[convID]
	if !ok {
		return TopicState{}, false, fmt.Errorf("journal: unknown conversation %q", convID)
	}
	if c.State != nil {
		prev = *c.State
	}
	if prev.State == s.State && prev.Reason == s.Reason {
		return prev, false, nil
	}
	old, oldSince := c.State, c.ActivitySince
	c.State, c.ActivitySince = &s, 0
	return prev, true, j.commit(func() { c.State, c.ActivitySince = old, oldSince })
}

// NoteActivity records that somebody wrote in conversation convID now.
// turn is true when a turn starts: it also counts toward ActivitySince.
func (j *Journal) NoteActivity(convID string, turn bool) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	c, ok := j.byID[convID]
	if !ok {
		return fmt.Errorf("journal: unknown conversation %q", convID)
	}
	oldAt, oldSince := c.LastActivity, c.ActivitySince
	c.LastActivity = nowMillis()
	if turn {
		c.ActivitySince++
	}
	return j.commit(func() { c.LastActivity, c.ActivitySince = oldAt, oldSince })
}

// StateImport is one entry of a state import: the state a conversation
// gets if it has none yet.
type StateImport struct {
	// ConvID names the conversation. When it is unknown, or retired,
	// Key finds the live conversation of the topic instead.
	ConvID string
	Key    Key
	State  TopicState
}

// ImportStates records each entry's state, with SetBy SetByImport, on
// a conversation that has no state yet. A conversation that already
// has one keeps it: an import never overrides the agent or the user.
// It returns how many conversations got a state. All of them are
// written at once.
func (j *Journal) ImportStates(entries []StateImport) (int, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	var set []*Conv
	since := map[*Conv]int{}
	for _, e := range entries {
		if !ValidState(e.State.State) {
			continue
		}
		c, ok := j.byID[e.ConvID]
		if !ok || c.Retired {
			if c, ok = j.byKey[e.Key.normalise().index()]; !ok {
				continue
			}
		}
		if c.State != nil {
			continue
		}
		s := e.State
		s.SetBy, s.Reason = SetByImport, clampReason(s.Reason)
		if s.SetAt == 0 {
			s.SetAt = nowMillis()
		}
		since[c] = c.ActivitySince
		c.State, c.ActivitySince = &s, 0
		set = append(set, c)
	}
	if len(set) == 0 {
		return 0, nil
	}
	return len(set), j.commit(func() {
		for _, c := range set {
			c.State, c.ActivitySince = nil, since[c]
		}
	})
}
