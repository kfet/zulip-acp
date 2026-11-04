package journal

import (
	"bytes"
	"encoding/json"
	"log"
	"path/filepath"
	"sort"

	"github.com/kfet/acp-kit/state"
)

// # Sidecars: the journal can be rebuilt from disk
//
// Each conversation also writes its identity — id, key, parent,
// created, retired — to state/convs/<id>/.conv.json (acp-kit's
// state.MetaFile). If journal.json is lost, the next start adopts every
// conversation it finds there, so every topic answers to its old
// conv-id, and therefore to its old working directory and agent
// session.
//
// There is no separate restore tool. Open runs the same two steps on
// every start: adopt the sidecars the journal does not know, then
// write the sidecar of every conversation whose sidecar is missing or
// stale. With a full journal the first step does nothing; on a host
// that ran an older version the second step is the backfill.
//
// A sidecar holds only identity. Message ids (tail, opts, last own)
// and turns are not in it: losing them costs one extra control
// message, never a session.

// sidecar is the on-disk shape of state.MetaFile.
type sidecar struct {
	Version int    `json:"version"`
	ID      string `json:"id"`
	Key
	Parent  *Parent `json:"parent,omitempty"`
	Created int64   `json:"created,omitempty"`
	Retired bool    `json:"retired,omitempty"`
}

// sidecarOf is what c writes to its sidecar.
func sidecarOf(c *Conv) []byte {
	return append(mustMarshalSidecar(sidecar{Version: currentVersion, ID: c.ID, Key: c.Key, Parent: c.Parent, Created: c.Created, Retired: c.Retired}), '\n')
}

// syncSidecars writes the sidecar of each of cs whose file on disk
// (have, by id; nil when unknown) differs. Caller holds mu.
//
// A failed write is logged and not returned. The sidecar is the
// backup of the journal, not the journal: a disk problem with it must
// not fail the user's command, and the next start retries the write.
func (j *Journal) syncSidecars(have map[string][]byte, cs ...*Conv) {
	for _, c := range cs {
		b := sidecarOf(c)
		if old, ok := have[c.ID]; ok && bytes.Equal(old, b) {
			continue
		}
		if err := state.WriteMeta(j.stateDir(), c.ID, b); err != nil {
			log.Printf("journal: sidecar %s: %v", c.ID, err)
		}
	}
}

// stateDir is the acp-kit state dir: journal.json lives in it.
func (j *Journal) stateDir() string { return filepath.Dir(j.path) }

// adoptSidecars is the start-up step: add every conversation that has
// a sidecar but no journal entry, then bring every sidecar up to date.
// Caller is Open: no concurrent users yet.
//
// The rules, in order:
//   - an entry already in the journal is never changed;
//   - a sidecar whose key a live journal entry holds is adopted
//     retired: the journal wins the key;
//   - among sidecars that claim one free key, the newest created wins
//     (ties: the higher id), and the rest are adopted retired;
//   - a retired sidecar stays retired.
//
// It returns how many conversations it adopted.
func (j *Journal) adoptSidecars() int {
	have, err := state.ReadMetas(j.stateDir())
	if err != nil {
		// Do not stop the relay: it can still serve every conversation
		// the journal knows. Backfill is skipped too, because have is
		// unknown and a blind rewrite could hide the same fault.
		log.Printf("journal: read sidecars: %v", err)
		return 0
	}
	var found []*Conv
	for id, b := range have {
		if _, known := j.byID[id]; known {
			continue
		}
		var s sidecar
		if err := json.Unmarshal(b, &s); err != nil || s.ID != id {
			log.Printf("journal: sidecar %s: not a conversation, ignored", id)
			continue
		}
		c := &Conv{ID: s.ID, Key: s.Key.normalise(), Created: s.Created, Retired: s.Retired}
		if s.Parent != nil && s.Parent.MessageID != 0 {
			p := *s.Parent
			p.Key = p.Key.normalise()
			c.Parent = &p
		}
		found = append(found, c)
	}
	// Newest first, so the first live claim of a key wins it.
	sort.Slice(found, func(a, b int) bool {
		if found[a].Created != found[b].Created {
			return found[a].Created > found[b].Created
		}
		return found[a].ID > found[b].ID
	})
	for _, c := range found {
		if _, taken := j.byKey[c.Key.index()]; taken {
			c.Retired = true
		}
		j.index(c)
	}
	if len(found) > 0 {
		if err := j.save(); err != nil {
			// The adopted entries stay in memory and the relay runs on
			// them; the next commit writes them, or the next start
			// adopts them again.
			log.Printf("journal: save adopted conversations: %v", err)
		}
	}
	all := make([]*Conv, 0, len(j.byID))
	for _, c := range j.byID {
		all = append(all, c)
	}
	j.syncSidecars(have, all...)
	return len(found)
}
