package handler

import (
	"context"
	"fmt"
	"os"
	"path"
	"sort"
	"strconv"
	"sync"

	"github.com/kfet/acp-kit/origin"

	"github.com/kfet/zulip-acp/internal/journal"
	"github.com/kfet/zulip-acp/internal/zulipproto"
)

// AgentCwdRoot is the opaque cwd root of cwd_mode "agent". The relay
// sends AgentCwd(key) as the cwd of every session call and never
// creates it: it is not a directory on this host, only a stable name
// per session. The agent side maps it to a real directory. It is one
// name per session key, so session/list and session/resume still find
// each conversation's own sessions.
const AgentCwdRoot = "/acp-agent-cwd"

// AgentCwd is the placeholder cwd of one session key.
func AgentCwd(key string) string { return path.Join(AgentCwdRoot, key) }

// OriginPlatform is the origin _meta platform name of this relay.
const OriginPlatform = "zulip"

// originOf is the origin _meta of the message m, routed to key.
// acp-kit's client sends it on every session call made with a context
// that carries it (origin.With): session/new, resume, load, fork and
// prompt. It is the ONLY place the sender's identity reaches the
// agent; the prompt text carries none.
func (h *Handler) originOf(m *zulipproto.Message, key journal.Key, mention bool) origin.Origin {
	o := origin.Origin{
		Platform:  OriginPlatform,
		TeamID:    h.cfg.Site,
		UserID:    strconv.FormatInt(m.SenderID, 10),
		IsDM:      key.IsDM(),
		IsMention: mention,
	}
	if m.ID != 0 {
		o.MessageID = strconv.FormatInt(m.ID, 10)
	}
	if key.IsDM() {
		o.ThreadID = key.Token()
	} else {
		o.ChannelID = strconv.FormatInt(key.StreamID, 10)
		o.ThreadID = key.Topic
	}
	return o
}

// withOriginOf is ctx carrying the origin of m, when there is one.
func (h *Handler) withOriginOf(ctx context.Context, m *zulipproto.Message, key journal.Key) context.Context {
	if m == nil || m.SenderID == 0 {
		return ctx
	}
	return origin.With(ctx, h.originOf(m, key, false))
}

// addressee is the @-mention a per-user session's reply starts with,
// so each user in a shared topic sees which answer is theirs. It is
// empty for a shared session, and in a one-to-one DM, where there is
// nobody else the answer could be for.
func (h *Handler) addressee(conv journal.Conv) string {
	if conv.User == 0 || (conv.IsDM() && len(conv.UserIDs) <= 2) {
		return ""
	}
	return fmt.Sprintf("@**%s|%d**\n", conv.UserName, conv.User)
}

// localDir is the directory on THIS host where the relay keeps a
// session's files: inbound attachments and the outbox. In cwd_mode
// "local" it is the session's cwd. In cwd_mode "agent" the agent's cwd
// is a placeholder, so the files stay here, and reach the agent only as
// inline content.
func (h *Handler) localDir(conv journal.Conv, cwd string) string {
	if !h.cfg.AgentCwd {
		return cwd
	}
	return h.convDir(conv.SessionKey())
}

// sessionCwd is the cwd the agent is told for one session key. It
// matches what the state manager sends (see main's CwdFor).
func (h *Handler) sessionCwd(key string) string {
	if h.cfg.AgentCwd {
		return AgentCwd(key)
	}
	return h.convDir(key)
}

// makeSessionCwd is sessionCwd, created on this host when it is a
// real directory there.
func (h *Handler) makeSessionCwd(key string) (string, error) {
	cwd := h.sessionCwd(key)
	if h.cfg.AgentCwd {
		return cwd, nil
	}
	return cwd, os.MkdirAll(cwd, 0o755)
}

// noteUserKey records conv's per-user session key, so the controls
// that act on a whole conversation (`!stop`, `!new`, archive, the
// reload drain) reach every user's turn in it.
func (h *Handler) noteUserKey(conv journal.Conv) {
	if conv.User == 0 {
		return
	}
	m, _ := h.userKeys.LoadOrStore(conv.ID, &sync.Map{})
	m.(*sync.Map).Store(conv.SessionKey(), struct{}{})
}

// turnKeys is every turn and session key of the conversation convID:
// its shared key, then each per-user key noted for it, sorted.
func (h *Handler) turnKeys(convID string) []string {
	keys := []string{convID}
	m, ok := h.userKeys.Load(convID)
	if !ok {
		return keys
	}
	var users []string
	m.(*sync.Map).Range(func(k, _ any) bool {
		users = append(users, k.(string))
		return true
	})
	sort.Strings(users)
	return append(keys, users...)
}
