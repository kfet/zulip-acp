package handler

import (
	"os"
	"path/filepath"
	"strings"

	acp "github.com/coder/acp-go-sdk"
)

// EmptyTurnNote is the one follow-up prompt the relay sends when a turn
// ends with no reply. It states facts and gives no order: the topic has
// nothing from the agent, and its tool calls already ran.
const EmptyTurnNote = "[relay] Your turn ended with no reply text. The topic shows nothing from you for it. The tool calls of that turn already ran."

// emptyTurn reports whether a finished turn left the topic with nothing:
// it ended normally, streamed no text, posted nothing through a tool and
// left no file in the outbox. A cancelled or failed turn never gets here.
func (h *Handler) emptyTurn(token string, stop acp.StopReason, sink *streamingSink, cwd string) bool {
	if stop != "" && stop != acp.StopReasonEndTurn {
		return false
	}
	if strings.TrimSpace(sink.messageText()) != "" {
		return false
	}
	if _, ok := h.outOfBand.Load(token); ok {
		return false
	}
	return !outboxHasFiles(filepath.Join(cwd, OutboxDir))
}

func outboxHasFiles(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			return true
		}
	}
	return false
}
