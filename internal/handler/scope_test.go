package handler

import (
	"context"
	"strings"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"
	"github.com/kfet/acp-kit/client"
	"github.com/kfet/acp-kit/convo"
	"github.com/kfet/acp-kit/origin"

	"github.com/kfet/zulip-acp/internal/journal"
	"github.com/kfet/zulip-acp/internal/rollover"
	"github.com/kfet/zulip-acp/internal/zulipproto"
)

const otherID = int64(77)

func (a *fakeAgent) seenOrigins() []origin.Origin {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]origin.Origin(nil), a.origins...)
}

func (s *fakeSessions) keys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for k := range s.created {
		out = append(out, k)
	}
	return out
}

func userEvent(id, sender int64, name, topic, content string) zulipproto.Event {
	return zulipproto.Event{Type: zulipproto.EventMessage, Message: &zulipproto.Message{
		ID: id, SenderID: sender, SenderName: name, Content: content,
		StreamID: 4, Topic: topic, Type: zulipproto.MessageTypeStream,
	}}
}

// TestOriginMetaRidesOnThePrompt: the sender reaches the agent as
// origin _meta, and the prompt text names nobody.
func TestOriginMetaRidesOnThePrompt(t *testing.T) {
	hh := newHarness(t, newAgent("ok"), func(c *Config) { c.Site = "https://chat.example.com" })
	hh.deliver(t, "plans", mention("hello"))
	got := hh.a.seenOrigins()
	want := origin.Origin{Platform: "zulip", TeamID: "https://chat.example.com", UserID: "8",
		ChannelID: "4", ThreadID: "plans", MessageID: "1", IsMention: true}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("origins = %+v, want %+v", got, want)
	}
	if p := hh.a.prompts[0]; p != "hello" || strings.Contains(p, "Kfet") {
		t.Fatalf("prompt = %q", p)
	}
}

func TestOriginMetaInADM(t *testing.T) {
	hh := dmCmdHarness(t, newAgent("ok"), nil)
	hh.deliverDM(t, humanID, "hi", humanID, botID)
	got := hh.a.seenOrigins()
	if len(got) != 1 || !got[0].IsDM || got[0].ChannelID != "" || got[0].ThreadID != journal.DM([]int64{humanID, botID}).Token() {
		t.Fatalf("origins = %+v", got)
	}
}

func TestWithOriginOfNeedsASender(t *testing.T) {
	hh := newHarness(t, newAgent(), nil)
	ctx := context.Background()
	if hh.h.withOriginOf(ctx, nil, journal.Key{}) != ctx {
		t.Fatal("nil message changed the context")
	}
	if hh.h.withOriginOf(ctx, &zulipproto.Message{}, journal.Key{}) != ctx {
		t.Fatal("sender-less message changed the context")
	}
	o, ok := origin.From(hh.h.withOriginOf(ctx, &zulipproto.Message{SenderID: 3}, journal.Channel(4, "t")))
	if !ok || o.MessageID != "" || o.UserID != "3" {
		t.Fatalf("origin = %+v", o)
	}
}

// TestLiteralSlashProseIsMarked: without the old name prefix, a user's
// own "/x" would start the prompt and the agent would run it.
func TestLiteralSlashProseIsMarked(t *testing.T) {
	if got := literalSlash("/reload"); got != literalSlashNote+"/reload" {
		t.Fatalf("got %q", got)
	}
	if got := literalSlash("hi /x"); got != "hi /x" {
		t.Fatalf("got %q", got)
	}
}

// TestPerUserSessionsSplitATopic: in thread_user scope each sender gets
// a session of their own, each reply @-mentions its user, and one
// user's message does not supersede another user's turn.
func TestPerUserSessionsSplitATopic(t *testing.T) {
	agent := newAgent("answer")
	agent.block = make(chan struct{})
	hh := newHarness(t, agent, func(c *Config) { c.PerUserSessions = true })
	hh.h.Handle(context.Background(), userEvent(1, humanID, "Kfet", "shared", mention("mine")))
	<-agent.entered
	hh.h.Handle(context.Background(), userEvent(2, otherID, "Ada", "shared", "theirs"))
	select {
	case <-agent.entered:
	case <-time.After(2 * time.Second):
		t.Fatalf("logs: %q", hh.logs)
	}
	if busy := hh.h.Busy(); len(busy) != 2 {
		t.Fatalf("busy = %q, want both turns running", busy)
	}
	close(agent.block)
	hh.idle(t)
	all := strings.Join(hh.z.stored(), "\n")
	if strings.Contains(all, "superseded") {
		t.Fatalf("one user's message superseded the other's turn: %q", all)
	}
	if !strings.Contains(all, "@**Kfet|8**\n") || !strings.Contains(all, "@**Ada|77**\n") {
		t.Fatalf("replies do not address their users: %q", all)
	}
	conv, _ := hh.j.Lookup(journal.Channel(4, "shared"))
	keys := hh.s.keys()
	if len(keys) != 2 {
		t.Fatalf("sessions = %q", keys)
	}
	for _, k := range keys {
		if k != conv.ID+"-u8" && k != conv.ID+"-u77" {
			t.Fatalf("session key %q", k)
		}
	}
	if got := hh.h.turnKeys(conv.ID); len(got) != 3 || got[0] != conv.ID {
		t.Fatalf("turnKeys = %q", got)
	}
}

// TestPerUserStopReachesEveryUser: `!stop` is a control of the whole
// conversation, so it stops every user's turn in it.
func TestPerUserStopReachesEveryUser(t *testing.T) {
	agent := newAgent("answer")
	agent.block = make(chan struct{})
	hh := cmdHarness(t, agent, func(c *Config) { c.PerUserSessions = true })
	hh.h.Handle(context.Background(), userEvent(1, humanID, "Kfet", "shared", mention("mine")))
	<-agent.entered
	hh.h.Handle(context.Background(), userEvent(2, otherID, "Ada", "shared", "theirs"))
	<-agent.entered
	conv, _ := hh.j.Lookup(journal.Channel(4, "shared"))
	if !hh.h.cancelInflight(context.Background(), conv.ID) {
		t.Fatal("nothing was stopped")
	}
	hh.idle(t)
	if hh.h.isInflight(conv.ID) {
		t.Fatal("a turn survived the stop")
	}
	if hh.h.cancelInflight(context.Background(), conv.ID) {
		t.Fatal("a second stop found a turn")
	}
	hh.h.endSession(context.Background(), conv.ID)
	if got := hh.h.turnKeys(conv.ID); len(got) != 1 {
		t.Fatalf("turnKeys after endSession = %q", got)
	}
}

func TestAddressee(t *testing.T) {
	hh := newHarness(t, newAgent(), nil)
	ch := journal.Conv{ID: "c", Key: journal.Channel(4, "t"), User: 8, UserName: "Kfet"}
	if got := hh.h.addressee(ch); got != "@**Kfet|8**\n" {
		t.Fatalf("channel: %q", got)
	}
	if got := hh.h.addressee(journal.Conv{ID: "c", Key: journal.Channel(4, "t")}); got != "" {
		t.Fatalf("shared: %q", got)
	}
	one := journal.Conv{ID: "c", Key: journal.DM([]int64{8, 9}), User: 8, UserName: "Kfet"}
	if got := hh.h.addressee(one); got != "" {
		t.Fatalf("1:1 DM: %q", got)
	}
	group := journal.Conv{ID: "c", Key: journal.DM([]int64{8, 9, 77}), User: 8, UserName: "Kfet"}
	if got := hh.h.addressee(group); got == "" {
		t.Fatal("group DM: no addressee")
	}
}

// TestAgentCwdIsAPlaceholder: in cwd_mode "agent" no host path is
// told to the agent, and the relay's own files stay on this host.
func TestAgentCwdIsAPlaceholder(t *testing.T) {
	hh := newHarness(t, newAgent(), func(c *Config) { c.AgentCwd = true })
	conv := journal.Conv{ID: "c1", User: 8}
	if got := hh.h.sessionCwd(conv.SessionKey()); got != "/acp-agent-cwd/c1-u8" {
		t.Fatalf("sessionCwd = %q", got)
	}
	if got, err := hh.h.makeSessionCwd("c1"); err != nil || got != AgentCwd("c1") {
		t.Fatalf("makeSessionCwd = %q, %v", got, err)
	}
	if got := hh.h.localDir(conv, "/ignored"); got != hh.h.convDir("c1-u8") {
		t.Fatalf("localDir = %q", got)
	}
	local := newHarness(t, newAgent(), nil)
	if got := local.h.localDir(conv, "/cwd"); got != "/cwd" {
		t.Fatalf("local localDir = %q", got)
	}
	if got := local.h.sessionCwd("c1"); got != local.h.convDir("c1") {
		t.Fatalf("local sessionCwd = %q", got)
	}
}

// TestAgentCwdLoginIsPerSender: in cwd_mode "agent" a login is the
// sender's own, so another user's message is not eaten as the paste.
func TestAgentCwdLoginIsPerSender(t *testing.T) {
	agent := newAgent("answer")
	agent.authMethods = []client.AuthMethod{{ID: "oauth-anthropic", Name: "Anthropic"}}
	agent.authResult = client.AuthResult{State: "needs_redirect", URL: "https://example.com/auth", ID: "a1"}
	hh := dmCmdHarness(t, agent, func(c *Config) { c.AgentCwd = true })
	hh.deliverDM(t, humanID, "!login anthropic", humanID, botID, otherID)
	key := journal.DM([]int64{humanID, botID, otherID})
	if !hh.h.cfg.Commands.HasPending(loginToken(key.Token(), humanID)) || hh.h.cfg.Commands.HasPending(key.Token()) {
		t.Fatal("the login is not the sender's")
	}
	hh.deliverDM(t, otherID, "https://example.com/not-a-paste", humanID, botID, otherID)
	if len(hh.a.prompts) != 1 {
		t.Fatalf("another user's message was eaten: %q", hh.a.prompts)
	}
	agent.authResult = client.AuthResult{State: "ok"}
	hh.z.reset()
	hh.deliverDM(t, humanID, "https://example.com/callback?code=x", humanID, botID, otherID)
	if !strings.Contains(strings.Join(hh.z.stored(), "\n"), "Authenticated") {
		t.Fatalf("login finish: %q", hh.z.stored())
	}
}

func TestLoginScopePassesWithoutASender(t *testing.T) {
	hh := dmCmdHarness(t, newAgent(), func(c *Config) { c.AgentCwd = true })
	if v := hh.h.loginScope(context.Background(), &convo.In{Text: "!login", Meta: dispatchMeta{}}); v != convo.Pass {
		t.Fatalf("verdict = %v", v)
	}
}

// TestPerUserBranchForksTheBranchersSession: in thread_user scope with
// cwd_mode "agent", a branch forks the brancher's own origin session
// into their own child session, under placeholder cwds.
func TestPerUserBranchForksTheBranchersSession(t *testing.T) {
	hh := branchHarnessTuned(t, func(c *Config) { c.PerUserSessions = true; c.AgentCwd = true })
	origin := engagedOrigin(t, hh)
	hh.branch(t, "planning", "!branch rework the splitter")
	child := branchedConv(t, hh, "rework the splitter")
	forks := hh.a.forked()
	want := forkCall{Cwd: AgentCwd(child.ID + "-u8"), Parent: acp.SessionId("sid-" + origin.ID + "-u8")}
	if len(forks) != 1 || forks[0] != want {
		t.Fatalf("forks = %+v, want %+v", forks, want)
	}
	o := hh.a.seenOrigins()
	if last := o[len(o)-1]; last.UserID != "8" || last.ThreadID != "rework the splitter" {
		t.Fatalf("branch origin = %+v", last)
	}
}

func TestPrefixedBudget(t *testing.T) {
	if got := prefixedBudget(0, ""); got != 0 {
		t.Fatalf("got %d", got)
	}
	if got := prefixedBudget(0, "ab"); got != rollover.DefaultBudget-2 {
		t.Fatalf("got %d", got)
	}
	if got := prefixedBudget(500, "é\n"); got != 498 {
		t.Fatalf("got %d", got)
	}
}
