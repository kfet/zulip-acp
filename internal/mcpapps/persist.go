package mcpapps

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"time"
)

// saved is one instance on disk. The resource HTML is not kept: it is
// read from the agent again on first use.
type saved struct {
	ID           string          `json:"id"`
	Token        string          `json:"token"`
	ContentToken string          `json:"content_token"`
	ConvID       string          `json:"conv_id"`
	CallKey      string          `json:"call_key"`
	Server       string          `json:"server"`
	Tool         string          `json:"tool"`
	URI          string          `json:"uri"`
	Label        string          `json:"label"`
	Expires      time.Time       `json:"expires"`
	Input        json.RawMessage `json:"input,omitempty"`
	Result       json.RawMessage `json:"result,omitempty"`
}

// load reads StatePath. A missing file is an empty store; expired
// instances are dropped. A file that cannot be read or parsed is logged
// and set aside, and the store starts empty: losing old app links must
// never stop the relay.
func (a *Apps) load() {
	if a.cfg.StatePath == "" {
		return
	}
	b, err := os.ReadFile(a.cfg.StatePath)
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	var all []saved
	if err == nil {
		err = json.Unmarshal(b, &all)
	}
	if err != nil {
		aside := a.cfg.StatePath + ".bad"
		a.cfg.Logf("mcpapps: unusable state %s (%v); starting empty, old app links are gone (moved to %s: %v)",
			a.cfg.StatePath, err, aside, os.Rename(a.cfg.StatePath, aside))
		return
	}
	now := a.cfg.Now()
	for _, s := range all {
		if now.After(s.Expires) {
			continue
		}
		in := &instance{
			id: s.ID, token: s.Token, contentToken: s.ContentToken,
			convID: s.ConvID, callKey: s.CallKey, server: s.Server, tool: s.Tool,
			uri: s.URI, label: s.Label, expires: s.Expires,
			input: s.Input, result: s.Result,
			subs: map[chan struct{}]struct{}{},
		}
		a.byID[in.id] = in
		a.byCall[in.callKey] = in
	}
}

// save writes every instance to StatePath, atomically. A failure is
// logged: the instances still work until the next reload.
func (a *Apps) save() {
	if a.cfg.StatePath == "" {
		return
	}
	// One writer at a time: every conversation's turn can call save,
	// and two writers would share the temp file.
	a.saveMu.Lock()
	defer a.saveMu.Unlock()
	a.mu.Lock()
	all := make([]saved, 0, len(a.byID))
	for _, in := range a.byID {
		in.mu.Lock()
		all = append(all, saved{
			ID: in.id, Token: in.token, ContentToken: in.contentToken,
			ConvID: in.convID, CallKey: in.callKey, Server: in.server, Tool: in.tool,
			URI: in.uri, Label: in.label, Expires: in.expires,
			Input: in.input, Result: in.result,
		})
		in.mu.Unlock()
	}
	a.mu.Unlock()
	if err := writeAtomic(a.cfg.StatePath, mustJSON(json.Marshal(all))); err != nil {
		a.cfg.Logf("mcpapps: save state: %v", err)
	}
}

// writeAtomic replaces path with b. The file holds the link tokens, so
// it is private to the user.
func writeAtomic(path string, b []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
