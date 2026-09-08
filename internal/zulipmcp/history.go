// This file is zulipmcp's tool half: the loopback tools that only
// Zulip can serve.
//
// # Why here and not in relaytool
//
// acp-kit/relaytool owns the relay-GENERIC loopback surface — status,
// model, post, schedule — because poe-acp and slack-acp need every one
// of them identically, through the same command.Broker action. Reading
// back a conversation is not that. It is a query against a Zulip topic
// or DM narrow, expressed in Zulip's message shapes, and the answer to
// "which conversation may I read" is a journal.Key. That is exactly the
// case the package doc always reserved this package for.
//
// # Identity
//
// The rule is unchanged and absolute: no tool takes a conversation as
// an argument. mcphost binds the session key server-side from the
// connection token; ConvKey turns it into the journal.Key that says
// which narrow may be read. An agent cannot name a topic it is not in,
// because there is nowhere to name one.
//
// # Bounded output
//
// `history` is the first tool whose RESULT can be large — a topic can
// hold thousands of messages of arbitrary length, and an unbounded read
// would blow the agent's context window in a single call. So the reply
// is bounded twice: per message and in total, newest kept first, with
// the paging anchor stated in the reply itself.
package zulipmcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kfet/acp-kit/mcphost"
	"github.com/kfet/zulip-acp/internal/journal"
	"github.com/kfet/zulip-acp/internal/zulipproto"
)

// ToolHistory is the tool name exposed to the agent.
const ToolHistory = "history"

// Output bounds. A relay tool answers into the agent's context window,
// so the reply is capped whatever the conversation holds.
const (
	// DefaultLimit is what an agent gets when it asks for no number:
	// enough to re-establish what a topic was about, small enough to
	// spend on speculatively.
	DefaultLimit = 20
	// MaxLimit caps `limit`. Asking for more is clamped rather than
	// rejected — the reply says so.
	MaxLimit = 200
	// MaxMessageRunes bounds ONE message body. Zulip allows 10000 code
	// points per message, so a handful of maximal ones would already
	// swamp a reply.
	MaxMessageRunes = 2000
	// MaxTotalRunes bounds the whole reply. When it binds, the OLDEST
	// messages are dropped: the newest are the ones worth having, and
	// before_id pages back to the rest.
	MaxTotalRunes = 20000
)

// Client is the slice of the Zulip client `history` needs.
type Client interface {
	Messages(ctx context.Context, narrow []zulipproto.NarrowTerm, limit int, beforeID int64) ([]zulipproto.Message, error)
}

// Config configures a Tools.
type Config struct {
	// Client fetches messages. Required.
	Client Client
	// ConvKey maps the mcphost session key — resolved server-side from
	// the connection token — to the Zulip conversation it names.
	// Required: without it there is no identity, and without identity
	// there is no safe read. Returning ok=false rejects the call.
	ConvKey func(sessionKey string) (journal.Key, bool)
	// Origin maps the same session key to the conversation this one
	// was BRANCHED from, if any. Required, and it is the entire
	// permission model of `history(origin: true)`: a session may read
	// its declared parent, clamped to messages at or before the branch
	// point, and nothing else.
	//
	// ONE HOP. It resolves the parent of the CALLER, never the parent
	// of the parent, so a chain of branches does not accumulate into a
	// licence to read the whole realm.
	Origin func(sessionKey string) (journal.Parent, bool)
	// Rename arms a rename of the conversation's topic, to be applied
	// when the turn ends. Required; it is the Handler's, because a
	// rename must not land while the turn is still posting into the
	// old topic. The returned string is what the agent is told.
	Rename func(key journal.Key, title string) (string, error)
	// Timeout bounds one Zulip round-trip. The agent's turn is blocked
	// until the tool call returns, so an unbounded fetch would let one
	// wedged request hang the turn forever.
	Timeout time.Duration
	// Logf receives operational messages.
	Logf func(format string, args ...any)
}

// DefaultTimeout bounds a history fetch when Config.Timeout is unset.
const DefaultTimeout = 30 * time.Second

// Tool is one registered tool, kept as data so the set can be built and
// exercised without a socket, a subprocess or an MCP handshake — the
// same shape relaytool uses, for the same reason.
type Tool struct {
	Name        string
	Description string
	Schema      map[string]any
	Handler     mcphost.Handler
}

// Tools is the Zulip-specific loopback tool set.
type Tools struct {
	cfg Config
}

// NewTools constructs a Tools.
func NewTools(cfg Config) (*Tools, error) {
	if cfg.Client == nil {
		return nil, errors.New("zulipmcp: Client is required")
	}
	if cfg.ConvKey == nil {
		return nil, errors.New("zulipmcp: ConvKey is required")
	}
	if cfg.Origin == nil {
		return nil, errors.New("zulipmcp: Origin is required")
	}
	if cfg.Rename == nil {
		return nil, errors.New("zulipmcp: Rename is required")
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultTimeout
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	return &Tools{cfg: cfg}, nil
}

// Register installs the tool set on h, in Tools order.
func (t *Tools) Register(h *mcphost.Host) {
	for _, x := range t.Tools() {
		h.Tool(x.Name, x.Description, x.Schema, x.Handler)
	}
}

// caller is the resolved identity of one tool call: the conversation
// it came from, and — on demand — the conversation that one was
// branched out of.
//
// The key is resolved by wrap, from the mcphost session key alone,
// before any tool body runs. That is what keeps the rule structural
// rather than conventional: a tool body is handed the conversations it
// may act on and has no way to name another.
//
// The ORIGIN is deliberately a thunk rather than a resolved value.
// Resolving it costs the relay a Zulip round-trip — the parent's
// location is read back from the branch-point message, because a
// stored topic name rots when the topic is renamed or archived — and
// an ordinary `history` call must not pay for a parent it is not
// asking about. It is still resolved from the session key and nothing
// else; there is simply nothing for a tool body to pass it.
type caller struct {
	// key is the conversation the call came from.
	key journal.Key
	// origin resolves the conversation this one was branched from.
	// There is deliberately no grandparent: see Config.Origin.
	origin func() (journal.Parent, bool)
}

// Tools builds the tool set as data.
func (t *Tools) Tools() []Tool {
	return []Tool{{
		Name: ToolHistory,
		Description: "Read earlier messages of THIS conversation, oldest first, as raw markdown — " +
			"including your own past replies. Use it to recover what was said before your current " +
			"session started, or before the context was cleared. Long replies are truncated: page " +
			"further back with before_id. With origin=true it instead reads the conversation THIS " +
			"topic was branched out of, up to the moment of the branch — available only when the relay " +
			"opened this topic with `!branch`. Those two are the only conversations it can ever read; " +
			"there is no way to address any other topic or DM, and the origin's own origin is not " +
			"reachable.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"limit": map[string]any{
					"type": "integer",
					"description": fmt.Sprintf(
						"How many messages to fetch, newest-most first. Default %d, maximum %d.",
						DefaultLimit, MaxLimit),
				},
				"before_id": map[string]any{
					"type": "integer",
					"description": "Return only messages OLDER than this message id, exclusive. " +
						"Pass the oldest id from a previous call to page further back. " +
						"Omit to start from the newest message.",
				},
				"origin": map[string]any{
					"type": "boolean",
					"description": "Read the conversation this topic was branched out of, instead of " +
						"this one. Only messages at or before the branch point are returned — the " +
						"origin's later messages are not yours to read. Fails when this topic was not " +
						"branched from anything.",
				},
			},
		},
		Handler: t.wrap(func(c caller, args json.RawMessage) (string, error) {
			var a struct {
				Limit    int   `json:"limit"`
				BeforeID int64 `json:"before_id"`
				Origin   bool  `json:"origin"`
			}
			if err := decode(args, &a); err != nil {
				return "", err
			}
			return t.history(c, a.Limit, a.BeforeID, a.Origin)
		}),
	}, t.renameTool()}
}

// history fetches and renders one page, of this conversation or of its
// origin.
func (t *Tools) history(c caller, limit int, beforeID int64, origin bool) (string, error) {
	if limit < 0 || beforeID < 0 {
		return "", errors.New("limit and before_id must not be negative")
	}
	clamped := false
	switch {
	case limit == 0:
		limit = DefaultLimit
	case limit > MaxLimit:
		limit, clamped = MaxLimit, true
	}
	key := c.key
	if origin {
		parent, ok := c.origin()
		if !ok {
			return "", errors.New("there is no origin conversation to read: either this topic was not branched out of another one, " +
				"or the conversation it came from is no longer reachable. Call history without origin to read this conversation instead")
		}
		key = parent.Key
		// The clamp, and the whole reason a branched session can be
		// handed another conversation at all: it may read what led to
		// the branch, never what the origin said afterwards.
		//
		// Zulip's anchor is any integer and need not be a real message
		// id, so "at or before N" is exactly "before N+1, exclusive".
		limitID := parent.MessageID + 1
		if beforeID == 0 || beforeID > limitID {
			beforeID = limitID
		}
	}
	var narrow []zulipproto.NarrowTerm
	if key.IsDM() {
		// A DM is in no channel, so a channel narrow would match
		// nothing there — silently.
		narrow = zulipproto.DMNarrow(key.UserIDs)
	} else {
		narrow = zulipproto.TopicNarrow(key.StreamID, key.Topic)
	}
	ctx, cancel := context.WithTimeout(context.Background(), t.cfg.Timeout)
	defer cancel()
	msgs, err := t.cfg.Client.Messages(ctx, narrow, limit, beforeID)
	if err != nil {
		return "", err
	}
	t.cfg.Logf("zulipmcp: history read %d message(s) in %s (%s)", len(msgs), key.Label(), whichConv(origin))
	return render(msgs, clamped, origin), nil
}

// whichConv names the conversation a reply is about. The empty page in
// particular has to say which one was empty, or an agent reading its
// origin cannot tell "nothing there" from "wrong place".
func whichConv(origin bool) string {
	if origin {
		return "the origin conversation"
	}
	return "this conversation"
}

// render turns a page into the agent-facing reply, bounded per message
// and in total.
//
// Messages arrive oldest first. The budget is spent NEWEST first and
// the result reversed, so what survives a bound is the recent end of
// the conversation, and before_id names the oldest that did survive.
func render(msgs []zulipproto.Message, clamped, origin bool) string {
	where := whichConv(origin)
	if len(msgs) == 0 {
		return "No earlier messages in " + where + "."
	}
	var (
		blocks    []string
		total     int
		truncated bool
		oldestID  int64
	)
	for i := len(msgs) - 1; i >= 0; i-- {
		b, cut := block(msgs[i])
		n := utf8.RuneCountInString(b)
		if len(blocks) > 0 && total+n > MaxTotalRunes {
			break
		}
		truncated = truncated || cut
		total += n
		oldestID = msgs[i].ID
		blocks = append(blocks, b)
	}
	dropped := len(msgs) - len(blocks)
	for l, r := 0, len(blocks)-1; l < r; l, r = l+1, r-1 {
		blocks[l], blocks[r] = blocks[r], blocks[l]
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "%d earlier message(s) in %s, oldest first:\n\n", len(blocks), where)
	sb.WriteString(strings.Join(blocks, "\n"))
	sb.WriteString("\n")
	if clamped {
		fmt.Fprintf(&sb, "\nlimit was clamped to the maximum of %d.", MaxLimit)
	}
	if dropped > 0 {
		fmt.Fprintf(&sb, "\n%d older message(s) were dropped to keep this reply under %d characters.",
			dropped, MaxTotalRunes)
	}
	if truncated {
		fmt.Fprintf(&sb, "\nMessage bodies longer than %d characters were truncated.", MaxMessageRunes)
	}
	again := ""
	if origin {
		again = "origin=true and "
	}
	fmt.Fprintf(&sb, "\nTo read further back, call %s again with %sbefore_id=%d.", ToolHistory, again, oldestID)
	return sb.String()
}

// block renders one message, truncating an over-long body, and reports
// whether it had to.
func block(m zulipproto.Message) (string, bool) {
	who := m.SenderName
	if who == "" {
		who = m.SenderEmail
	}
	when := time.Unix(m.Timestamp, 0).UTC().Format(time.RFC3339)
	body, cut := truncate(m.Content, MaxMessageRunes)
	return fmt.Sprintf("[#%d %s] %s:\n%s\n", m.ID, when, who, body), cut
}

// truncate cuts s to at most n code points — code points, never bytes,
// because that is the unit Zulip counts in and the unit a body of
// non-ASCII text is long in.
func truncate(s string, n int) (string, bool) {
	if utf8.RuneCountInString(s) <= n {
		return s, false
	}
	r := []rune(s)
	return string(r[:n]) + "… [truncated]", true
}

// wrap resolves the mcphost session key to the conversations the call
// may act on before the handler runs, so no tool body ever sees a raw
// session key and no tool body can be written that takes a
// conversation as an argument.
func (t *Tools) wrap(fn func(c caller, args json.RawMessage) (string, error)) mcphost.Handler {
	return func(sessionKey string, args json.RawMessage) (string, error) {
		key, ok := t.cfg.ConvKey(sessionKey)
		if !ok {
			return "", errors.New("this conversation is no longer active")
		}
		return fn(caller{
			key:    key,
			origin: func() (journal.Parent, bool) { return t.cfg.Origin(sessionKey) },
		}, args)
	}
}

// decode unmarshals a tool's arguments, turning a decode failure into
// the message the agent sees.
func decode(args json.RawMessage, v any) error {
	if len(args) == 0 {
		return nil // a no-argument call may send nothing at all
	}
	if err := json.Unmarshal(args, v); err != nil {
		return errors.New("invalid params: " + err.Error())
	}
	return nil
}
