// Package journal is the relay's durable, local cache of conversation
// identity.
//
// # Why this exists
//
// The Zulip topic is the session identity, but a topic string is
// arbitrary user text — spaces, slashes, dots, emoji — so it cannot be
// a state-directory path component, and it can be RENAMED at any time.
// Both problems are solved by one indirection: every conversation gets
// a stable, opaque conv-id at first contact, and acp-kit's state
// manager is keyed on that conv-id forever. A rename rewrites the
// alias, not the key, so the live ACP session and its working
// directory survive untouched.
//
// Doing it the other way — keying the session map on the topic and
// "migrating" on rename — looks simpler and is a trap: the state
// manager has no rekey operation, so the old entry lingers holding the
// SAME agent session id, and idle GC eventually reaps it and kills a
// live session out from under the new key.
//
// # Truth vs cache
//
// The topic is truth; this journal is a cache. On any conflict the
// topic wins: an unknown (channel, topic) is simply a new conversation.
// Losing the file costs continuity of naming, never correctness.
package journal

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Key is what a conversation is reached by. Zulip has exactly two
// conversation shapes and one type expresses both, so the journal
// never needs a parallel map:
//
//   - a channel conversation, identified by (StreamID, Topic);
//   - a direct message, identified by the SET of participating user
//     ids — sorted, deduped, and INCLUDING the bot, exactly as Zulip
//     reports it in display_recipient. A group DM is the same shape
//     with more members.
//
// UserIDs being non-empty is the discriminant: a channel conversation
// never carries one, and a DM has no channel or topic. That is why the
// on-disk shape needs no version bump — a pre-DM journal has no
// user_ids anywhere and loads as channel conversations unchanged.
type Key struct {
	// StreamID is the Zulip channel id (channel conversations only).
	StreamID int64 `json:"stream_id,omitempty"`
	// Topic is the current topic string, exactly as Zulip delivers it
	// — never normalised, never case-folded (channel conversations
	// only).
	Topic string `json:"topic,omitempty"`
	// UserIDs is the DM recipient set: sorted, deduped, bot included.
	UserIDs []int64 `json:"user_ids,omitempty"`
}

// Channel returns the key for a channel topic.
func Channel(streamID int64, topic string) Key {
	return Key{StreamID: streamID, Topic: topic}
}

// DM returns the key for a direct-message conversation with the given
// participants. The input is copied, sorted and deduped, so the same
// set in any order — and Zulip's display_recipient order is not
// stable — always yields the same key.
func DM(userIDs []int64) Key {
	ids := make([]int64, len(userIDs))
	copy(ids, userIDs)
	slices.Sort(ids)
	ids = slices.Compact(ids)
	if len(ids) == 0 {
		// An empty set is not a conversation; callers must not build
		// one, and returning the zero Key keeps IsDM honest.
		return Key{}
	}
	return Key{UserIDs: ids}
}

// IsDM reports whether k identifies a direct message.
func (k Key) IsDM() bool { return len(k.UserIDs) > 0 }

// index is the canonical map key. The leading tag keeps the two
// shapes in disjoint namespaces for good, so no channel key can ever
// collide with a DM key.
func (k Key) index() string {
	if k.IsDM() {
		return "d\x00" + k.userList()
	}
	return "c\x00" + strconv.FormatInt(k.StreamID, 10) + "\x00" + k.Topic
}

// userList renders the DM participant set as "4,9".
func (k Key) userList() string {
	parts := make([]string, len(k.UserIDs))
	for i, id := range k.UserIDs {
		parts[i] = strconv.FormatInt(id, 10)
	}
	return strings.Join(parts, ",")
}

// Label renders a key for the operator log.
//
// Deliberately NOT called String: Conv embeds Key, so a String method
// here would be promoted and silently turn every `%v` of a Conv — which
// is what carries the conv-id — into a rendering of its key alone.
func (k Key) Label() string {
	if k.IsDM() {
		return "DM " + k.userList()
	}
	return "channel " + strconv.FormatInt(k.StreamID, 10) + " > " + strconv.Quote(k.Topic)
}

// normalise canonicalises a key loaded from disk, so a hand-edited
// journal with an unsorted or duplicated user_ids list still indexes
// the same as the set Zulip delivers.
func (k Key) normalise() Key {
	if k.IsDM() {
		return DM(k.UserIDs)
	}
	return k
}

// Conv is one conversation: its key, its stable conv-id, and the id of
// the tail message the relay currently owns.
type Conv struct {
	// ID is the stable, opaque conversation id. It is a safe single
	// path component and is used as the acp-kit state manager key.
	ID string `json:"id"`
	// Key is flattened into the enclosing JSON object, which is what
	// keeps the pre-DM on-disk shape ({"id","stream_id","topic"})
	// readable and writable unchanged.
	Key
	// TailID is the message the relay is currently streaming into, or
	// 0 when no turn is in flight. A non-zero value surviving a
	// restart means that turn was interrupted.
	TailID int64 `json:"tail_id,omitempty"`
}

// file is the on-disk shape. Stored as a list so the file stays
// readable and diffable by an operator.
type file struct {
	Version int    `json:"version"`
	Convs   []Conv `json:"convs"`
}

const currentVersion = 1

// Journal is the persisted alias map. Safe for concurrent use.
type Journal struct {
	path string

	mu    sync.Mutex
	byID  map[string]*Conv
	byKey map[string]*Conv
}

// Open loads the journal at path, creating an empty one if the file
// does not exist. A corrupt file is an error, not a silent reset: the
// operator should see it.
func Open(path string) (*Journal, error) {
	j := &Journal{path: path, byID: map[string]*Conv{}, byKey: map[string]*Conv{}}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return j, nil
	}
	if err != nil {
		return nil, fmt.Errorf("journal: read %s: %w", path, err)
	}
	var f file
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("journal: parse %s: %w", path, err)
	}
	for i := range f.Convs {
		c := f.Convs[i]
		c.Key = c.Key.normalise()
		j.index(&c)
	}
	return j, nil
}

// index installs c in both maps. Caller holds mu (or is Open, which
// has no concurrent users yet).
func (j *Journal) index(c *Conv) {
	j.byID[c.ID] = c
	j.byKey[c.Key.index()] = c
}

// Lookup returns the conversation for k, if known.
func (j *Journal) Lookup(k Key) (Conv, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	c, ok := j.byKey[k.index()]
	if !ok {
		return Conv{}, false
	}
	return *c, true
}

// Ensure returns the conversation for k, allocating a fresh conv-id
// and persisting it if this is the first time the relay has seen the
// key.
func (j *Journal) Ensure(k Key) (Conv, error) {
	j.mu.Lock()
	if c, ok := j.byKey[k.index()]; ok {
		out := *c
		j.mu.Unlock()
		return out, nil
	}
	c := &Conv{ID: j.newID(), Key: k}
	j.index(c)
	out := *c
	err := j.save()
	j.mu.Unlock()
	if err != nil {
		return Conv{}, err
	}
	return out, nil
}

// Rename migrates the conversation at (streamID, oldTopic) to
// newTopic, keeping its conv-id — and therefore its live ACP session
// and working directory — intact.
//
// Channel conversations only: a DM has no topic, so there is nothing
// here for one to match. Its key is in a disjoint namespace, which is
// what makes that structural rather than a convention.
//
// Returns ok=false when the old topic is unknown (nothing to migrate)
// or when the new topic already has its own conversation, in which
// case the existing one wins and the stale alias is dropped. The topic
// is truth: two conv-ids must never share a key.
func (j *Journal) Rename(streamID int64, oldTopic, newTopic string) (Conv, bool, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	oldKey, newKey := Channel(streamID, oldTopic).index(), Channel(streamID, newTopic).index()
	if oldKey == newKey {
		return Conv{}, false, nil
	}
	c, ok := j.byKey[oldKey]
	if !ok {
		return Conv{}, false, nil
	}
	delete(j.byKey, oldKey)
	if existing, clash := j.byKey[newKey]; clash {
		// The destination topic already has a conversation. Keep it,
		// and let the migrated one become unreachable rather than
		// leaving two conv-ids answering to the same topic.
		delete(j.byID, c.ID)
		out := *existing
		return out, false, j.save()
	}
	c.Topic = newTopic
	j.byKey[newKey] = c
	out := *c
	return out, true, j.save()
}

// SetTail records the message id the relay is streaming into for a
// conversation. Pass 0 to clear it when the turn completes.
func (j *Journal) SetTail(convID string, msgID int64) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	c, ok := j.byID[convID]
	if !ok {
		return fmt.Errorf("journal: unknown conversation %q", convID)
	}
	if c.TailID == msgID {
		return nil
	}
	c.TailID = msgID
	return j.save()
}

// OpenTails returns every conversation with a tail message still
// recorded — i.e. a turn that was in flight when the relay stopped.
// On startup these are marked as interrupted and cleared.
func (j *Journal) OpenTails() []Conv {
	j.mu.Lock()
	defer j.mu.Unlock()
	var out []Conv
	for _, c := range j.byID {
		if c.TailID != 0 {
			out = append(out, *c)
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].ID < out[b].ID })
	return out
}

// Convs returns every known conversation, ordered by conv-id.
func (j *Journal) Convs() []Conv {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := make([]Conv, 0, len(j.byID))
	for _, c := range j.byID {
		out = append(out, *c)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].ID < out[b].ID })
	return out
}

// newID mints an unused conv-id. Caller holds mu.
func (j *Journal) newID() string {
	for {
		var b [6]byte
		mustRandom(b[:])
		id := "c" + hex.EncodeToString(b[:])
		if _, taken := j.byID[id]; !taken {
			return id
		}
	}
}

// save writes the whole journal atomically. Caller holds mu.
func (j *Journal) save() error {
	convs := make([]Conv, 0, len(j.byID))
	for _, c := range j.byID {
		convs = append(convs, *c)
	}
	sort.Slice(convs, func(a, b int) bool { return convs[a].ID < convs[b].ID })
	b := append(mustMarshal(file{Version: currentVersion, Convs: convs}), '\n')
	if err := os.MkdirAll(filepath.Dir(j.path), 0o755); err != nil {
		return fmt.Errorf("journal: mkdir: %w", err)
	}
	tmp := j.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("journal: write: %w", err)
	}
	if err := os.Rename(tmp, j.path); err != nil {
		return fmt.Errorf("journal: commit: %w", err)
	}
	return nil
}

// randomRead is crypto/rand.Read, indirected so the must-helper stays
// trivially auditable.
var randomRead = rand.Read
