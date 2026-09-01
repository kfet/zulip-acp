package handler

import "github.com/kfet/acp-kit/command"

// mustOutcome asserts the broker's contract: when the relay has
// already established that a message is a command — either because a
// login is pending for this conversation, or because Broker.IsCommand
// said so — Broker.Handle returns an Outcome to render.
//
// It cannot be reached from a test, because it cannot be reached at
// all: acp-kit's Handle returns (nil, nil) only for a sigil-prefixed
// body that matches none of its cases, and every body IsCommand
// accepts is one that Handle matches. The two functions are defined
// against the same list.
//
// It panics rather than returning a nil the caller would have to guard,
// because the alternative — silently consuming a user's message and
// posting nothing — is the one failure mode this relay must never
// have. If acp-kit ever drifts so that the two lists disagree, a crash
// on the first affected command is how we find out immediately instead
// of through a user reporting that the bot went quiet.
func mustOutcome(out *command.Outcome) *command.Outcome {
	if out == nil {
		panic("handler: acp-kit/command returned no outcome for a recognised command — IsCommand and Handle disagree")
	}
	return out
}
