package journal

// # State import
//
// `zulip-acp import-states <file>` seeds topic states once, from a
// file a triage agent wrote:
//
//	{"<conv_id>": {"topic": "…", "stream_id": 4, "state": "fyi",
//	               "reason": "…", "dup_of": "…", "set_at": …, "set_by": "…"}}
//
// The running relay holds the journal in memory and rewrites it whole
// on every change, so a second process must not write journal.json.
// The command therefore only checks the file and STAGES a copy in the
// state dir. The relay applies the staged copy when it next opens the
// journal (a start, or a graceful reload) and then renames it, so it
// is applied once.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// StagedImportName is the staged import file in the state dir.
const StagedImportName = "state-import.json"

// importEntry is one value of the triage file.
type importEntry struct {
	Topic    string          `json:"topic"`
	StreamID int64           `json:"stream_id"`
	State    string          `json:"state"`
	Reason   string          `json:"reason"`
	DupOf    string          `json:"dup_of"`
	SetAt    json.RawMessage `json:"set_at"`
}

// ParseStateImport reads a triage file. An entry with dup_of becomes
// done, with the reason "dup of <dup_of>". An entry with no valid state
// is skipped and named in skipped. The entries come out ordered by
// conv-id.
func ParseStateImport(b []byte) (entries []StateImport, skipped []string, err error) {
	var raw map[string]importEntry
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, nil, fmt.Errorf("parse state import: %w", err)
	}
	ids := make([]string, 0, len(raw))
	for id := range raw {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		e := raw[id]
		s := TopicState{State: strings.TrimSpace(e.State), Reason: strings.TrimSpace(e.Reason), SetAt: importTime(e.SetAt), SetBy: SetByImport}
		if d := strings.TrimSpace(e.DupOf); d != "" {
			s.State, s.Reason = StateDone, "dup of "+d
		}
		if !ValidState(s.State) {
			skipped = append(skipped, fmt.Sprintf("%s: state %q", id, e.State))
			continue
		}
		entries = append(entries, StateImport{ConvID: id, Key: Channel(e.StreamID, e.Topic), State: s})
	}
	return entries, skipped, nil
}

// importTime reads set_at: Unix seconds or milliseconds, or an RFC 3339
// string. Anything else is 0, which means "now" on import.
func importTime(raw json.RawMessage) int64 {
	var n float64
	if json.Unmarshal(raw, &n) == nil && n > 0 {
		if n < 1e12 {
			return int64(n * 1000)
		}
		return int64(n)
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			return t.UnixMilli()
		}
	}
	return 0
}

// StageStateImport checks the triage file b and stages it in
// stateDir. It returns how many entries it will apply and the entries
// it skipped.
func StageStateImport(stateDir string, b []byte) (int, []string, error) {
	entries, skipped, err := ParseStateImport(b)
	if err != nil {
		return 0, nil, err
	}
	if len(entries) == 0 {
		return 0, skipped, errors.New("state import: no entry has a valid state")
	}
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return 0, nil, fmt.Errorf("state import: %w", err)
	}
	path := filepath.Join(stateDir, StagedImportName)
	if err := os.WriteFile(path+".tmp", b, 0o600); err != nil {
		return 0, nil, fmt.Errorf("state import: %w", err)
	}
	if err := os.Rename(path+".tmp", path); err != nil {
		return 0, nil, fmt.Errorf("state import: %w", err)
	}
	return len(entries), skipped, nil
}

// ApplyStagedImport applies the staged import in the journal's state
// dir, if there is one, and renames it to <name>.applied so it is
// applied once. It returns how many conversations got a state.
func (j *Journal) ApplyStagedImport() (int, error) {
	path := filepath.Join(j.stateDir(), StagedImportName)
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("state import: %w", err)
	}
	entries, _, err := ParseStateImport(b)
	if err != nil {
		return 0, err
	}
	n, err := j.ImportStates(entries)
	if err != nil {
		return 0, err
	}
	if err := os.Rename(path, path+".applied"); err != nil {
		return n, fmt.Errorf("state import: applied, but %w", err)
	}
	return n, nil
}
