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

// mustOverrides panics if convo.NewOverrides failed. With a nil Store it
// cannot fail, so an error here is a wiring bug.
func mustOverrides(o *convo.Overrides, err error) *convo.Overrides {
	if err != nil {
		panic("handler: overrides: " + err.Error())
	}
	return o
}
