// This file is the handler's side of MCP Apps (internal/mcpapps).
//
// mcpapps knows nothing about Zulip, so the three things it needs from
// the relay come from here: the live ACP session of a conversation (to
// read the app HTML and to call tools through fir's dev.fir.mcp/v1
// extension), and a way to put an app's ui/message into the topic as a
// new user turn. appRouter, in the turn's sink chain, is what notices
// an app tool call and posts the link.
package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	acp "github.com/coder/acp-go-sdk"
	"github.com/kfet/acp-kit/client"
	"github.com/kfet/zulip-acp/internal/mcpapps"
)

// AppObserver sees every session update of a turn. It returns a line to
// post in the topic when the update starts an MCP App, else "".
// *mcpapps.Apps implements it.
type AppObserver interface {
	Observe(convID string, u acp.SessionUpdate) string
}

// appSender is the attribution of a turn an app started. Turns are
// prefixed with their sender; an app must not pass as the human who
// opened it.
const appSender = "app"

// appRouter forwards every update and gives tool calls to the app
// observer. The link goes up out of band, not into the answer stream:
// an abstaining turn must still show the app it opened.
type appRouter struct {
	next   client.SessionUpdateSink
	apps   AppObserver
	convID string
	post   func(line string)
}

// OnUpdate implements client.SessionUpdateSink.
func (r *appRouter) OnUpdate(ctx context.Context, n acp.SessionNotification) error {
	if n.Update.ToolCall != nil || n.Update.ToolCallUpdate != nil {
		if line := r.apps.Observe(r.convID, n.Update); line != "" {
			r.post(line)
		}
	}
	return r.next.OnUpdate(ctx, n)
}

// postAppLink posts an app link line in the conversation's topic. It
// runs in its own goroutine: the sink is called from the agent's update
// stream, and a slow Zulip must not stall it. PostTo bounds the call.
func (h *Handler) postAppLink(token, line string) {
	h.appPosts.Add(1)
	go func() {
		defer h.appPosts.Done()
		if err := h.PostTo(token, line); err != nil {
			h.cfg.Logf("handler: post app link: %v", err)
		}
	}()
}

// appSession is the conversation's live ACP session. An app never
// creates one: a session is made by a turn, and a reaped one comes back
// with the next message in the topic.
func (h *Handler) appSession(convID string) (acp.SessionId, error) {
	if _, ok := h.cfg.Agent.Caps().Extensions[mcpapps.MetaKey]; !ok {
		return "", fmt.Errorf("the agent does not support %s", mcpapps.MetaKey)
	}
	sid, _, ok := h.cfg.Sessions.Live(convID)
	if !ok {
		return "", errors.New("the conversation has no live agent session; send a message in the topic, then reload the app")
	}
	return sid, nil
}

// ReadResource implements mcpapps.Backend.
func (h *Handler) ReadResource(ctx context.Context, convID, server, uri string) (json.RawMessage, error) {
	sid, err := h.appSession(convID)
	if err != nil {
		return nil, err
	}
	return h.cfg.Agent.ExtMethod(ctx, sid, mcpapps.MethodResourcesRead, map[string]any{"server": server, "uri": uri})
}

// CallTool implements mcpapps.Backend.
func (h *Handler) CallTool(ctx context.Context, convID, server, name string, args json.RawMessage) (json.RawMessage, error) {
	sid, err := h.appSession(convID)
	if err != nil {
		return nil, err
	}
	return h.cfg.Agent.ExtMethod(ctx, sid, mcpapps.MethodToolsCall, map[string]any{"server": server, "name": name, "arguments": args})
}

// Message implements mcpapps.Backend: an app's ui/message becomes a new
// user turn in its conversation, exactly as if a human had typed it —
// so, like a typed message, it supersedes a turn in flight.
//
// The text is echoed into the topic FIRST, so the topic history shows
// what the agent was told and by whom, and the echo is the message the
// turn acknowledges. The relay's gates are re-applied here, as for a
// scheduled prompt: the channel must still be served, DMs still on, and
// the conversation still in the journal.
func (h *Handler) Message(ctx context.Context, convID, label, text string) error {
	conv, ok := h.cfg.Journal.LookupID(convID)
	if !ok {
		return errors.New("the conversation of this app no longer exists")
	}
	key := conv.Key
	if key.IsDM() {
		if !h.cfg.DMs {
			return errors.New("direct messages are no longer served")
		}
	} else if _, ok := h.cfg.Channels.Name(key.StreamID); !ok {
		return errors.New("the channel of this app is no longer served")
	}
	ids, err := h.PostToIDs(key.Token(), appEcho(label, text))
	if err != nil {
		return fmt.Errorf("echo into the topic: %w", err)
	}
	var ack int64
	if len(ids) > 0 {
		ack, _ = strconv.ParseInt(ids[len(ids)-1], 10, 64)
		h.liveTurns.Store(conv.ID, ack)
	}
	h.cfg.Logf("handler: app %q starts a turn in %s", label, h.describe(key))
	h.startTurn(context.WithoutCancel(ctx), conv, "["+appSender+" "+label+"] "+text, true, ack)
	return nil
}

// appEcho is the topic copy of an app's message: a quote block, with a
// fence longer than any backtick run in the text so the text cannot
// close it.
func appEcho(label, text string) string {
	fence, run := 3, 0
	for _, r := range text {
		if r == '`' {
			run++
			if run >= fence {
				fence = run + 1
			}
		} else {
			run = 0
		}
	}
	f := strings.Repeat("`", fence)
	return fmt.Sprintf("🧩 **%s** sent:\n%squote\n%s\n%s", label, f, text, f)
}
