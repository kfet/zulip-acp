package handler

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
