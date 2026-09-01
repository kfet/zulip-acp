package journal

import (
	"fmt"
	"strconv"
	"strings"
)

// Token renders a Key as an opaque, round-trippable string.
//
// # Why this exists
//
// acp-kit's command broker identifies a conversation by a single
// opaque `convID` string that it hands straight back to the relay. The
// obvious thing to pass is the conv-id — and it is the WRONG thing,
// because `!new` replaces the conv-id. A broker holding one would be
// holding a stale identity the moment the command it is running
// finishes.
//
// So the relay passes the KEY instead. A key is what a conversation is
// reached by, and it is exactly the thing `!new` does not change: the
// topic is still the topic, the DM participants are still the
// participants. Only the conv-id behind it moves.
//
// The encoding is Key.index(), which is already canonical, already
// keeps the two conversation shapes in disjoint namespaces, and is
// already the map key — so a token cannot disagree with a lookup.
func (k Key) Token() string { return k.index() }

// ParseToken reverses Token.
//
// It is strict: a malformed token is an error, never a best-effort
// guess. Guessing would silently attach a command to the wrong
// conversation, and `!new` on the wrong conversation destroys context
// the user expected to keep.
func ParseToken(s string) (Key, error) {
	tag, rest, ok := strings.Cut(s, "\x00")
	if !ok {
		return Key{}, fmt.Errorf("journal: malformed conversation token %q", s)
	}
	switch tag {
	case "c":
		sid, topic, ok := strings.Cut(rest, "\x00")
		if !ok {
			return Key{}, fmt.Errorf("journal: malformed channel token %q", s)
		}
		streamID, err := strconv.ParseInt(sid, 10, 64)
		if err != nil {
			return Key{}, fmt.Errorf("journal: malformed channel id in token %q: %w", s, err)
		}
		return Channel(streamID, topic), nil
	case "d":
		parts := strings.Split(rest, ",")
		ids := make([]int64, 0, len(parts))
		for _, p := range parts {
			id, err := strconv.ParseInt(p, 10, 64)
			if err != nil {
				return Key{}, fmt.Errorf("journal: malformed user id in token %q: %w", s, err)
			}
			ids = append(ids, id)
		}
		// Split never yields an empty slice and every element must
		// parse, so ids is non-empty here and DM cannot return the
		// zero Key.
		return DM(ids), nil
	default:
		return Key{}, fmt.Errorf("journal: unknown conversation token kind %q", tag)
	}
}
