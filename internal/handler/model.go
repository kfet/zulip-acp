package handler

import (
	"context"

	acp "github.com/coder/acp-go-sdk"
)

// modelChoice is a conversation's sticky model and the session it was
// last pushed to. Recording the session id is what makes the choice
// survive idle GC: a conversation whose session was reaped comes back
// with a new session id, and the mismatch is the signal to re-apply.
type modelChoice struct {
	id      string
	applied acp.SessionId
}

func (h *Handler) modelOverride(convID string) (string, bool) {
	h.modelMu.Lock()
	defer h.modelMu.Unlock()
	c, ok := h.modelChoices[convID]
	if !ok {
		return "", false
	}
	return c.id, true
}

func (h *Handler) setModelOverride(convID, modelID string) {
	h.modelMu.Lock()
	defer h.modelMu.Unlock()
	h.modelChoices[convID] = modelChoice{id: modelID}
}

// carryModelOverride moves a sticky choice from a retired conversation
// to the fresh one that replaced it, dropping the applied marker so
// the new session gets its own push.
func (h *Handler) carryModelOverride(from, to string) {
	h.modelMu.Lock()
	defer h.modelMu.Unlock()
	if c, ok := h.modelChoices[from]; ok {
		delete(h.modelChoices, from)
		h.modelChoices[to] = modelChoice{id: c.id}
	}
}

// applyModel pushes a conversation's sticky model to its ACP session,
// at most once per session.
//
// Applying it lazily rather than at `!model` time is deliberate: doing
// it eagerly needs a live session, and calling GetOrCreate outside a
// turn would spawn one — and re-register a sink — as a side effect of
// what reads like a settings command.
//
// A failure is logged and the turn continues: the agent answers on
// whatever model it already had, which is a far better outcome than
// refusing to answer at all over a preference.
func (h *Handler) applyModel(ctx context.Context, convID string, sid acp.SessionId) {
	h.modelMu.Lock()
	c, ok := h.modelChoices[convID]
	if !ok || c.applied == sid {
		h.modelMu.Unlock()
		return
	}
	h.modelMu.Unlock()

	if err := h.cfg.Agent.SetModel(ctx, sid, c.id); err != nil {
		h.cfg.Logf("handler: selecting model %s for %s: %v", c.id, convID, err)
		return
	}
	h.modelMu.Lock()
	if cur, still := h.modelChoices[convID]; still && cur.id == c.id {
		h.modelChoices[convID] = modelChoice{id: c.id, applied: sid}
	}
	h.modelMu.Unlock()
}
