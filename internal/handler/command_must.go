package handler

import (
	"github.com/kfet/acp-kit/convo"
	"github.com/kfet/zulip-acp/internal/journal"
)

// mustKey parses a broker token the convo Manager has already resolved.
// Its ModelChanged hook fires only after Resolve (convFor) parsed the
// same token successfully, so a parse error here cannot happen.
func mustKey(token string) journal.Key {
	key, err := journal.ParseToken(token)
	if err != nil {
		panic("handler: resolved token no longer parses: " + err.Error())
	}
	return key
}

// mustConvo panics if convo.New failed. The handler passes no override
// Store and always an Agent — the only two ways it can fail — so an
// error here is a wiring bug, not a runtime condition.
func mustConvo(m *convo.Manager, err error) *convo.Manager {
	if err != nil {
		panic("handler: convo manager: " + err.Error())
	}
	return m
}
