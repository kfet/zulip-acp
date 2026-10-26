package handler

import "github.com/kfet/zulip-acp/internal/journal"

// The sticky per-conversation `!model` choice lives in acp-kit's shared
// convo.Overrides (h.convo.Overrides()), and is pushed to the session
// lazily, at most once per session, by convo.Manager.ApplyModel at the
// start of each turn. Applying it lazily rather than at `!model` time is
// deliberate: doing it eagerly needs a live session, and calling
// GetOrCreate outside a turn would spawn one — and re-register a sink —
// as a side effect of what reads like a settings command.

// modelOverride returns convID's sticky model choice, if any.
func (h *Handler) modelOverride(convID string) (string, bool) {
	return h.convo.Overrides().Get(convID)
}

// inheritModel copies the parent's choice before the child's first turn.
// A pending override wins over the parent's current session model. The
// copy is independent of the parent and also applies when a fork fails.
func (h *Handler) inheritModel(origin journal.Key, childID string) {
	parent, ok := h.cfg.Journal.Lookup(origin)
	if !ok {
		return
	}
	id, chosen := h.modelOverride(parent.ID)
	if !chosen {
		if sid, _, live := h.cfg.Sessions.Live(parent.ID); live {
			id, _ = h.cfg.Agent.CurrentModel(sid)
		}
	}
	if id == "" {
		return
	}
	if err := h.convo.Overrides().Set(childID, id); err != nil {
		h.cfg.Logf("handler: persisting branch model for %s: %v", childID, err)
	}
}
