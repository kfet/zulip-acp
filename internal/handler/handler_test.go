package handler

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"

	"github.com/kfet/acp-kit/client"
	"github.com/kfet/acp-kit/state"
	"github.com/kfet/zulip-acp/internal/channels"
	"github.com/kfet/zulip-acp/internal/journal"
	"github.com/kfet/zulip-acp/internal/rollover"
	"github.com/kfet/zulip-acp/internal/statusline"
	"github.com/kfet/zulip-acp/internal/zulipproto"
)

const (
	botID   = int64(9)
	botName = "fir-relay"
	humanID = int64(8)
)

// --- fakes ---------------------------------------------------------------

// fakeZulip models the surface: a message store plus an upload store.
type fakeZulip struct {
	mu     sync.Mutex
	next   int64
	bodies map[int64]string
	topics map[int64]string
	// dms records the recipient set of every message sent as a DM,
	// keyed by message id. A channel message never appears here.
	dms      map[int64][]int64
	order    []int64
	uploads  map[string][]byte
	sendErr  error
	editErr  error
	getErr   error
	uploadEr error
	// widgets records the widget_content each message was sent with,
	// and widgetErr makes the server refuse any message carrying one.
	widgets   map[int64]string
	widgetErr error
	// deleteErr models a realm that forbids a bot deleting its own
	// message — the fallback path when a panel is retired.
	deleteErr error
	// reactAdd/reactDel record every reaction call, in order, as
	// "<messageID>:<emoji>".
	reactAdd []string
	reactDel []string
	reactErr error
	// posted is signalled after every Post or Edit, so tests can
	// synchronise on surface state instead of polling a clock.
	posted chan struct{}
	// unreacted is signalled after every RemoveReaction. A superseded
	// turn retracts its ack AFTER its inflight entry is gone, so
	// WaitIdle cannot observe that cleanup — tests wait on this.
	unreacted chan struct{}
}

func newZulip() *fakeZulip {
	return &fakeZulip{
		bodies:  map[int64]string{},
		topics:  map[int64]string{},
		dms:     map[int64][]int64{},
		widgets: map[int64]string{},
		uploads: map[string][]byte{},
		posted:  make(chan struct{}, 256),

		unreacted: make(chan struct{}, 256),
	}
}

func (z *fakeZulip) signal() {
	select {
	case z.posted <- struct{}{}:
	default:
	}
}

func (z *fakeZulip) SendMessage(_ context.Context, _ int64, topic, content string) (int64, error) {
	z.mu.Lock()
	defer z.mu.Unlock()
	if z.sendErr != nil {
		return 0, z.sendErr
	}
	z.next++
	z.bodies[z.next] = content
	z.topics[z.next] = topic
	z.order = append(z.order, z.next)
	z.signal()
	return z.next, nil
}

// SendDirectMessage records the recipient set alongside the body, so
// a test can assert both that a DM went out and who it went to.
func (z *fakeZulip) SendDirectMessage(_ context.Context, userIDs []int64, content string) (int64, error) {
	z.mu.Lock()
	defer z.mu.Unlock()
	if z.sendErr != nil {
		return 0, z.sendErr
	}
	z.next++
	z.bodies[z.next] = content
	z.dms[z.next] = append([]int64(nil), userIDs...)
	z.order = append(z.order, z.next)
	z.signal()
	return z.next, nil
}

// SendMessageWidget records the widget payload alongside the body. A
// server that refuses widgets is modelled with widgetErr, which is how
// the graceful-degradation path is driven.
func (z *fakeZulip) SendMessageWidget(ctx context.Context, streamID int64, topic, content, widget string) (int64, error) {
	if err := z.widgetGate(widget); err != nil {
		return 0, err
	}
	id, err := z.SendMessage(ctx, streamID, topic, content)
	z.recordWidget(id, err, widget)
	return id, err
}

func (z *fakeZulip) SendDirectMessageWidget(ctx context.Context, userIDs []int64, content, widget string) (int64, error) {
	if err := z.widgetGate(widget); err != nil {
		return 0, err
	}
	id, err := z.SendDirectMessage(ctx, userIDs, content)
	z.recordWidget(id, err, widget)
	return id, err
}

func (z *fakeZulip) widgetGate(widget string) error {
	z.mu.Lock()
	defer z.mu.Unlock()
	if widget != "" && z.widgetErr != nil {
		return z.widgetErr
	}
	return nil
}

func (z *fakeZulip) recordWidget(id int64, err error, widget string) {
	if err != nil {
		return
	}
	z.mu.Lock()
	defer z.mu.Unlock()
	z.widgets[id] = widget
}

// widget returns the widget_content sent with a message, "" if none.
func (z *fakeZulip) widget(id int64) string {
	z.mu.Lock()
	defer z.mu.Unlock()
	return z.widgets[id]
}

// EditMessage mirrors the server on the one rule that shapes the
// `!opts` design: a message carrying a widget REFUSES every content
// edit ("Widgets cannot be edited.", measured on Zulip 12.2). Without
// this the fake would happily accept an edit the real server rejects,
// and the tests would prove nothing.
func (z *fakeZulip) EditMessage(_ context.Context, id int64, content string) error {
	z.mu.Lock()
	defer z.mu.Unlock()
	if z.editErr != nil {
		return z.editErr
	}
	if z.widgets[id] != "" {
		return &zulipproto.APIError{Status: 400, Msg: "Widgets cannot be edited.", Code: "BAD_REQUEST"}
	}
	if _, ok := z.bodies[id]; !ok {
		return fmt.Errorf("no such message %d", id)
	}
	z.bodies[id] = content
	z.signal()
	return nil
}

// DeleteMessage removes a message the way Zulip does when the realm
// allows it. deleteErr models a realm that does not (or a message past
// message_content_delete_limit_seconds).
func (z *fakeZulip) DeleteMessage(_ context.Context, id int64) error {
	z.mu.Lock()
	defer z.mu.Unlock()
	if z.deleteErr != nil {
		return z.deleteErr
	}
	if _, ok := z.bodies[id]; !ok {
		return &zulipproto.APIError{Status: 404, Msg: "Invalid message(s)", Code: "BAD_REQUEST"}
	}
	delete(z.bodies, id)
	delete(z.widgets, id)
	z.order = slices.DeleteFunc(z.order, func(x int64) bool { return x == id })
	return nil
}

func (z *fakeZulip) GetMessage(_ context.Context, id int64) (zulipproto.Message, error) {
	z.mu.Lock()
	defer z.mu.Unlock()
	if z.getErr != nil {
		return zulipproto.Message{}, z.getErr
	}
	body, ok := z.bodies[id]
	if !ok {
		return zulipproto.Message{}, fmt.Errorf("no such message %d", id)
	}
	return zulipproto.Message{ID: id, Content: body, SenderID: botID}, nil
}

func (z *fakeZulip) Upload(_ context.Context, filename string, r io.Reader) (string, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return "", err
	}
	z.mu.Lock()
	defer z.mu.Unlock()
	if z.uploadEr != nil {
		return "", z.uploadEr
	}
	z.uploads[filename] = b
	return "/user_uploads/2/ab/" + filename, nil
}

func (z *fakeZulip) AddReaction(_ context.Context, id int64, emoji string) error {
	z.mu.Lock()
	defer z.mu.Unlock()
	z.reactAdd = append(z.reactAdd, fmt.Sprintf("%d:%s", id, emoji))
	return z.reactErr
}

func (z *fakeZulip) RemoveReaction(_ context.Context, id int64, emoji string) error {
	z.mu.Lock()
	defer z.mu.Unlock()
	z.reactDel = append(z.reactDel, fmt.Sprintf("%d:%s", id, emoji))
	select {
	case z.unreacted <- struct{}{}:
	default:
	}
	return z.reactErr
}

func (z *fakeZulip) reactions() (added, removed []string) {
	z.mu.Lock()
	defer z.mu.Unlock()
	return append([]string(nil), z.reactAdd...), append([]string(nil), z.reactDel...)
}

func (z *fakeZulip) stored() []string {
	z.mu.Lock()
	defer z.mu.Unlock()
	out := make([]string, 0, len(z.order))
	for _, id := range z.order {
		out = append(out, z.bodies[id])
	}
	return out
}

func (z *fakeZulip) body(id int64) string {
	z.mu.Lock()
	defer z.mu.Unlock()
	return z.bodies[id]
}

func (z *fakeZulip) count() int {
	z.mu.Lock()
	defer z.mu.Unlock()
	return len(z.order)
}

// fakeAgent plays the ACP agent: it replays a scripted reply through
// whatever sink the session manager installed.
type fakeAgent struct {
	mu          sync.Mutex
	sink        client.SessionUpdateSink
	chunks      []string
	thoughts    []string
	meta        map[string]any
	stop        acp.StopReason
	err         error
	model       string
	models      []client.ModelInfo
	agentCmds   []client.CommandInfo
	authMethods []client.AuthMethod
	authResult  client.AuthResult
	authErr     error
	setModel    []string
	setErr      error
	prompts     []string
	// block, when non-nil, holds Prompt until closed — used to test
	// cancellation of an in-flight turn.
	block chan struct{}
	// hold, when non-nil, keeps Prompt running AFTER the chunks have
	// been emitted, so the coalescing watchdog gets a chance to
	// publish mid-turn.
	hold chan struct{}
	// entered is signalled when Prompt starts.
	entered chan struct{}
}

func newAgent(chunks ...string) *fakeAgent {
	return &fakeAgent{chunks: chunks, stop: acp.StopReasonEndTurn, entered: make(chan struct{}, 8)}
}

func (a *fakeAgent) Models() ([]client.ModelInfo, string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.models, a.model
}

// SetModel records every model selection as "<sessionID>=<modelID>".
func (a *fakeAgent) SetModel(_ context.Context, sid acp.SessionId, modelID string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.setErr != nil {
		return a.setErr
	}
	a.setModel = append(a.setModel, string(sid)+"="+modelID)
	return nil
}

// AuthMethods and Authenticate satisfy the broker's Authenticator.
func (a *fakeAgent) AuthMethods() []client.AuthMethod {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.authMethods
}

func (a *fakeAgent) Authenticate(_ context.Context, _, _, _ string, _ bool) (client.AuthResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.authResult, a.authErr
}

// AvailableCommands is the agent's advertised catalog, which gates the
// passthrough allowlist.
func (a *fakeAgent) AvailableCommands() []client.CommandInfo {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.agentCmds
}

func (a *fakeAgent) selections() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.setModel)
}

func (a *fakeAgent) Prompt(ctx context.Context, _ acp.SessionId, blocks []acp.ContentBlock) (acp.StopReason, error) {
	a.mu.Lock()
	a.prompts = append(a.prompts, blocks[0].Text.Text)
	sink, chunks, thoughts, meta := a.sink, a.chunks, a.thoughts, a.meta
	block, hold, stop, err := a.block, a.hold, a.stop, a.err
	a.mu.Unlock()
	select {
	case a.entered <- struct{}{}:
	default:
	}
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	for _, th := range thoughts {
		_ = sink.OnUpdate(ctx, thoughtNotification(th, meta))
	}
	for _, c := range chunks {
		if err := sink.OnUpdate(ctx, chunkNotification(c, meta)); err != nil {
			return "", err
		}
	}
	if hold != nil {
		select {
		case <-hold:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	return stop, err
}

func chunkNotification(text string, meta map[string]any) acp.SessionNotification {
	return acp.SessionNotification{
		Meta: meta,
		Update: acp.SessionUpdate{
			AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{Content: acp.TextBlock(text)},
		},
	}
}

func thoughtNotification(text string, meta map[string]any) acp.SessionNotification {
	return acp.SessionNotification{
		Meta: meta,
		Update: acp.SessionUpdate{
			AgentThoughtChunk: &acp.SessionUpdateAgentThoughtChunk{Content: acp.TextBlock(text)},
		},
	}
}

// fakeSessions plays acp-kit's state.Manager.
type fakeSessions struct {
	mu       sync.Mutex
	agent    *fakeAgent
	dir      string
	sessions map[string]*state.Session
	created  map[string]int
	err      error
	pending  string
	cancels  []string
}

func newSessions(t *testing.T, a *fakeAgent) *fakeSessions {
	t.Helper()
	return &fakeSessions{agent: a, dir: t.TempDir(), sessions: map[string]*state.Session{}, created: map[string]int{}}
}

func (s *fakeSessions) GetOrCreate(_ context.Context, key string, sink client.SessionUpdateSink) (*state.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return nil, s.err
	}
	s.agent.mu.Lock()
	s.agent.sink = sink
	s.agent.mu.Unlock()
	if sess, ok := s.sessions[key]; ok {
		return sess, nil
	}
	cwd := filepath.Join(s.dir, key)
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		return nil, err
	}
	// A re-created session gets a NEW session id, exactly as acp-kit
	// does after idle GC reaps one. The first creation keeps the plain
	// form so tests can name it.
	sid := "sid-" + key
	if n := s.created[key]; n > 0 {
		sid = fmt.Sprintf("%s#%d", sid, n)
	}
	s.created[key]++
	sess := &state.Session{Key: key, SessionID: acp.SessionId(sid), Cwd: cwd}
	s.sessions[key] = sess
	return sess, nil
}

func (s *fakeSessions) Touch(*state.Session) {}

func (s *fakeSessions) StateDir() string { return s.dir }

func (s *fakeSessions) Cancel(_ context.Context, key string) {
	s.mu.Lock()
	s.cancels = append(s.cancels, key)
	s.mu.Unlock()
}

func (s *fakeSessions) TakePendingSystemPrompt(*state.Session) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.pending
	s.pending = ""
	return p
}

// --- harness -------------------------------------------------------------

type harness struct {
	h     *Handler
	z     *fakeZulip
	a     *fakeAgent
	s     *fakeSessions
	j     *journal.Journal
	jdir  string
	logs  []string
	logMu sync.Mutex
}

// breakJournal makes every subsequent journal write fail, by removing
// write permission from the directory holding it. Used to drive the
// persistence-failure branches without reaching into the package.
func (hh *harness) breakJournal(t *testing.T) {
	t.Helper()
	if err := os.Chmod(hh.jdir, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(hh.jdir, 0o700) })
}

func newHarness(t *testing.T, agent *fakeAgent, tune func(*Config)) *harness {
	t.Helper()
	z := newZulip()
	sess := newSessions(t, agent)
	jdir := t.TempDir()
	j, err := journal.Open(filepath.Join(jdir, "journal.json"))
	if err != nil {
		t.Fatalf("journal: %v", err)
	}
	hh := &harness{z: z, a: agent, s: sess, j: j, jdir: jdir}
	cfg := Config{
		Client:         z,
		Agent:          agent,
		Sessions:       sess,
		Journal:        j,
		BotUserID:      botID,
		BotFullName:    botName,
		Channels:       channels.New(channels.Config{Explicit: map[int64]string{4: "fleet"}}),
		EditInterval:   time.Millisecond,
		SilentSentinel: "<<SILENT>>",
		Logf: func(format string, args ...any) {
			hh.logMu.Lock()
			hh.logs = append(hh.logs, fmt.Sprintf(format, args...))
			hh.logMu.Unlock()
		},
	}
	if tune != nil {
		tune(&cfg)
	}
	h, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	hh.h = h
	return hh
}

// deliver feeds a channel message and waits for the turn to finish.
func (hh *harness) deliver(t *testing.T, topic, content string) {
	t.Helper()
	hh.deliverAs(t, humanID, topic, content)
}

func (hh *harness) deliverAs(t *testing.T, sender int64, topic, content string) {
	t.Helper()
	hh.h.Handle(context.Background(), channelEvent(sender, topic, content))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := hh.h.WaitIdle(ctx); err != nil {
		t.Fatalf("turn did not finish: %v", err)
	}
}

func (hh *harness) logged(sub string) bool {
	hh.logMu.Lock()
	defer hh.logMu.Unlock()
	for _, l := range hh.logs {
		if strings.Contains(l, sub) {
			return true
		}
	}
	return false
}

func mention(rest string) string { return "@**" + botName + "** " + rest }

// --- construction --------------------------------------------------------

func TestNewValidation(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("want error on missing dependencies")
	}
	z := newZulip()
	a := newAgent()
	s := newSessions(t, a)
	j, err := journal.Open(filepath.Join(t.TempDir(), "j.json"))
	if err != nil {
		t.Fatalf("journal: %v", err)
	}
	if _, err := New(Config{Client: z, Agent: a, Sessions: s, Journal: j}); err == nil {
		t.Fatal("want error on missing channel allowlist")
	}
	set := channels.New(channels.Config{Explicit: map[int64]string{4: "fleet"}})
	h, err := New(Config{Client: z, Agent: a, Sessions: s, Journal: j, Channels: set})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if h.cfg.PromptTimeout != 10*time.Minute || h.cfg.EditInterval != 300*time.Millisecond {
		t.Fatalf("defaults not applied: %+v", h.cfg)
	}
	h.cfg.Logf("smoke %d", 1) // default no-op logger must be callable
	if h.sealMarker() != rollover.DefaultSealMarker {
		t.Fatalf("seal marker = %q", h.sealMarker())
	}
	h2, err := New(Config{Client: z, Agent: a, Sessions: s, Journal: j, Channels: set, SealMarker: "~fin~"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if h2.sealMarker() != "~fin~" {
		t.Fatalf("seal marker = %q", h2.sealMarker())
	}
}

// --- gating --------------------------------------------------------------

func TestSelfAuthoredMessageIsRefused(t *testing.T) {
	hh := newHarness(t, newAgent("should never run"), nil)
	// The relay's own message, WITH a mention of itself and in an
	// allowed channel: the self-guard runs first and unconditionally.
	hh.deliverAs(t, botID, "loop", mention("do it again"))
	if hh.z.count() != 0 {
		t.Fatal("relay answered its own message — self-loop")
	}
	if len(hh.j.Convs()) != 0 {
		t.Fatal("self-authored message created a conversation")
	}
}

func TestGatingDrops(t *testing.T) {
	cases := []struct {
		name string
		ev   zulipproto.Message
	}{
		{"direct message", zulipproto.Message{SenderID: humanID, Type: "private", Content: mention("hi"), StreamID: 4, Topic: "t"}},
		{"other channel", zulipproto.Message{SenderID: humanID, Type: "stream", Content: mention("hi"), StreamID: 99, Topic: "t"}},
		{"empty text", zulipproto.Message{SenderID: humanID, Type: "stream", Content: "   ", StreamID: 4, Topic: "t"}},
		{"no mention, unknown topic", zulipproto.Message{SenderID: humanID, Type: "stream", Content: "just chatting", StreamID: 4, Topic: "t"}},
	}
	for _, c := range cases {
		hh := newHarness(t, newAgent("nope"), nil)
		m := c.ev
		hh.h.Handle(context.Background(), zulipproto.Event{Type: zulipproto.EventMessage, Message: &m})
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := hh.h.WaitIdle(ctx); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		cancel()
		if hh.z.count() != 0 {
			t.Fatalf("%s: relay answered anyway", c.name)
		}
	}
	// A nil message payload and an unknown event type are both no-ops.
	hh := newHarness(t, newAgent("nope"), nil)
	hh.h.Handle(context.Background(), zulipproto.Event{Type: zulipproto.EventMessage})
	hh.h.Handle(context.Background(), zulipproto.Event{Type: "reaction"})
	if hh.z.count() != 0 {
		t.Fatal("relay answered a malformed event")
	}
}

func TestUserAllowlist(t *testing.T) {
	hh := newHarness(t, newAgent("hello"), func(c *Config) {
		c.AllowedUsers = map[int64]struct{}{42: {}}
	})
	hh.deliverAs(t, humanID, "t", mention("hi"))
	if hh.z.count() != 0 {
		t.Fatal("relay answered a user outside the allowlist")
	}
	if !hh.logged("not allowed") {
		t.Fatal("expected an allowlist log")
	}
	hh.deliverAs(t, 42, "t", mention("hi"))
	if hh.z.count() != 1 {
		t.Fatal("allowlisted user was not answered")
	}
}

func TestMentionVariants(t *testing.T) {
	for _, form := range []string{
		"@**fir-relay**",
		"@**fir-relay|9**",
		"@_**fir-relay**_",
		"@_**fir-relay|9**_",
	} {
		hh := newHarness(t, newAgent("ok"), nil)
		hh.deliver(t, "t-"+form, form+" ping")
		if hh.z.count() != 1 {
			t.Fatalf("mention form %q was not recognised", form)
		}
		// The addressing syntax is stripped; the sender is named.
		if got := hh.a.prompts[0]; got != "[Kfet] ping" {
			t.Fatalf("prompt = %q", got)
		}
	}
	// A message that is ONLY a mention still reaches the agent rather
	// than becoming an empty prompt.
	hh := newHarness(t, newAgent("ok"), nil)
	hh.deliver(t, "bare", "@**fir-relay**")
	if got := hh.a.prompts[0]; got != "[Kfet] @**fir-relay**" {
		t.Fatalf("bare mention prompt = %q", got)
	}
	// With no bot name configured nothing can be a mention.
	hh2 := newHarness(t, newAgent("ok"), func(c *Config) { c.BotFullName = "" })
	hh2.deliver(t, "t", "@**fir-relay** hi")
	if hh2.z.count() != 0 {
		t.Fatal("mention matched with no bot name configured")
	}
}

// --- the happy path ------------------------------------------------------

func TestAddressedTurnStreamsAndAnswers(t *testing.T) {
	agent := newAgent("Hello, ", "world.")
	agent.model = "anthropic/claude-sonnet-4"
	hh := newHarness(t, agent, nil)
	hh.deliver(t, "session: greet", mention("say hello"))

	if hh.z.count() != 1 {
		t.Fatalf("expected exactly one message, got %d", hh.z.count())
	}
	body := hh.z.body(1)
	if !strings.HasSuffix(body, "Hello, world.") {
		t.Fatalf("body = %q", body)
	}
	// The provider emoji resolved from the agent's current model shows
	// up in the status header.
	if !strings.HasPrefix(body, "> *") {
		t.Fatalf("status header missing: %q", body)
	}
	// A conversation was recorded, and its tail was cleared when the
	// turn completed.
	convs := hh.j.Convs()
	if len(convs) != 1 || convs[0].Topic != "session: greet" || convs[0].StreamID != 4 {
		t.Fatalf("journal = %+v", convs)
	}
	if len(hh.j.OpenTails()) != 0 {
		t.Fatal("tail not cleared after a completed turn")
	}
}

func TestFollowUpReusesTheSameSession(t *testing.T) {
	agent := newAgent("first")
	hh := newHarness(t, agent, nil)
	hh.deliver(t, "session: memory", mention("remember ZEBRA"))
	convID := hh.j.Convs()[0].ID

	// A follow-up with no mention: the topic is engaged, so it is
	// answered, and it must land on the SAME session key.
	agent.mu.Lock()
	agent.chunks = []string{"ZEBRA"}
	agent.mu.Unlock()
	hh.deliver(t, "session: memory", "what was the codeword?")

	if got := len(hh.s.sessions); got != 1 {
		t.Fatalf("%d sessions created, want 1", got)
	}
	if _, ok := hh.s.sessions[convID]; !ok {
		t.Fatalf("session key is not the conv-id: %v", hh.s.sessions)
	}
	if len(hh.j.Convs()) != 1 {
		t.Fatalf("journal grew: %+v", hh.j.Convs())
	}
}

func TestNewTopicIsANewSession(t *testing.T) {
	hh := newHarness(t, newAgent("ok"), nil)
	hh.deliver(t, "session: one", mention("hi"))
	hh.deliver(t, "session: two", mention("hi"))
	if len(hh.s.sessions) != 2 {
		t.Fatalf("%d sessions, want 2 independent ones", len(hh.s.sessions))
	}
	convs := hh.j.Convs()
	if len(convs) != 2 || convs[0].ID == convs[1].ID {
		t.Fatalf("conversations = %+v", convs)
	}
}

// TestSameTopicInTwoChannelsAreTwoSessions pins that the conv-key is
// (stream_id, topic), never topic alone.
func TestSameTopicInTwoChannelsAreTwoSessions(t *testing.T) {
	hh := newHarness(t, newAgent("ok"), func(c *Config) {
		c.Channels = channels.New(channels.Config{Explicit: map[int64]string{4: "fleet", 5: "ops"}})
	})
	for _, stream := range []int64{4, 5} {
		hh.h.Handle(context.Background(), zulipproto.Event{
			Type: zulipproto.EventMessage,
			Message: &zulipproto.Message{
				SenderID: humanID, SenderName: "Kfet", Content: mention("hi"),
				StreamID: stream, Topic: "standup", Type: "stream",
			},
		})
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := hh.h.WaitIdle(ctx); err != nil {
			t.Fatalf("WaitIdle: %v", err)
		}
		cancel()
	}
	if len(hh.s.sessions) != 2 {
		t.Fatalf("%d sessions, want 2", len(hh.s.sessions))
	}
}

// TestFollowedChannelSetGatesAtRuntime is the end of the "*" story
// inside the handler: the allowlist is consulted per event, so a
// channel the bot is subscribed to mid-run starts being served, and an
// unsubscribe stops it — with no restart and no config edit.
func TestFollowedChannelSetGatesAtRuntime(t *testing.T) {
	set := channels.New(channels.Config{Follow: true})
	hh := newHarness(t, newAgent("ok"), func(c *Config) { c.Channels = set })
	const sandbox = int64(2)

	send := func() {
		t.Helper()
		hh.h.Handle(context.Background(), zulipproto.Event{
			Type: zulipproto.EventMessage,
			Message: &zulipproto.Message{
				SenderID: humanID, SenderName: "Kfet", Content: mention("hi"),
				StreamID: sandbox, Topic: "runtime", Type: "stream",
			},
		})
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := hh.h.WaitIdle(ctx); err != nil {
			t.Fatalf("WaitIdle: %v", err)
		}
	}

	// Not subscribed yet: silence.
	send()
	if n := hh.z.count(); n != 0 {
		t.Fatalf("answered in an unserved channel (%d messages)", n)
	}

	sub := zulipproto.Event{
		Type: zulipproto.EventSubscription, Op: "add",
		Subscriptions: []zulipproto.Stream{{StreamID: sandbox, Name: "sandbox"}},
	}
	set.Apply(sub)
	send()
	if hh.z.count() == 0 {
		t.Fatal("no answer after the bot was subscribed at runtime")
	}
	if !hh.logged("in #sandbox") {
		t.Fatal("channel name missing from the new-conversation log")
	}

	// Unsubscribe: the topic is engaged, but the channel is gone, so
	// even a follow-up must be dropped.
	sub.Op = "remove"
	set.Apply(sub)
	before := hh.z.count()
	send()
	if hh.z.count() != before {
		t.Fatal("answered after the bot was unsubscribed")
	}
}

func TestSystemPromptIsInlinedOnce(t *testing.T) {
	hh := newHarness(t, newAgent("ok"), nil)
	hh.s.pending = "SYSTEM RULES"
	hh.deliver(t, "t", mention("hi"))
	if got := hh.a.prompts[0]; !strings.HasPrefix(got, "SYSTEM RULES\n\n") {
		t.Fatalf("prompt = %q", got)
	}
	hh.deliver(t, "t", "follow up")
	if got := hh.a.prompts[1]; strings.Contains(got, "SYSTEM RULES") {
		t.Fatalf("system prompt re-inlined: %q", got)
	}
}

// --- ambient / abstain ---------------------------------------------------

func TestAmbientAbstainPostsNothing(t *testing.T) {
	agent := newAgent("hello")
	hh := newHarness(t, agent, nil)
	hh.deliver(t, "t", mention("hi"))
	posted := hh.z.count()

	agent.mu.Lock()
	agent.chunks = []string{"<<SILENT>>"}
	agent.mu.Unlock()
	hh.deliver(t, "t", "some chatter not aimed at the bot")

	if hh.z.count() != posted {
		t.Fatalf("abstained turn posted a message: %v", hh.z.stored())
	}
	if !hh.logged("abstained") {
		t.Fatal("expected an abstain log")
	}
	// The prompt still reached the agent, so it stays caught up.
	if len(agent.prompts) != 2 {
		t.Fatalf("prompts = %v", agent.prompts)
	}
}

func TestAmbientAnswerIsPostedInOneMessage(t *testing.T) {
	agent := newAgent("first")
	hh := newHarness(t, agent, nil)
	hh.deliver(t, "t", mention("hi"))

	agent.mu.Lock()
	agent.chunks = []string{"a real ", "answer"}
	agent.thoughts = []string{"pondering"}
	agent.mu.Unlock()
	hh.deliver(t, "t", "and what about X?")

	if hh.z.count() != 2 {
		t.Fatalf("expected a second message, got %d", hh.z.count())
	}
	body := hh.z.body(2)
	if !strings.Contains(body, "a real answer") {
		t.Fatalf("body = %q", body)
	}
	// Thoughts are force-hidden on the abstain path: one that reached
	// the surface before the verdict could not be retracted.
	if strings.Contains(body, "pondering") {
		t.Fatalf("thought leaked past the abstain verdict: %q", body)
	}
	// The placeholder that went up mid-turn is the SAME message the
	// answer landed in — it is edited, never appended to.
	if strings.Contains(body, "Thinking") {
		t.Fatalf("placeholder survived into the answer: %q", body)
	}
}

// TestAmbientWithoutSentinelStreams: with no sentinel configured there
// is no abstain verdict to wait for, so ambient turns stream like
// addressed ones.
func TestAmbientWithoutSentinelStreams(t *testing.T) {
	agent := newAgent("streamed")
	hh := newHarness(t, agent, func(c *Config) { c.SilentSentinel = "" })
	hh.deliver(t, "t", mention("hi"))
	hh.deliver(t, "t", "follow up")
	if hh.z.count() != 2 {
		t.Fatalf("messages = %d", hh.z.count())
	}
}

// --- rollover through the handler ---------------------------------------

func TestLongAnswerRollsOverWithNoTextLost(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < 400; i++ {
		fmt.Fprintf(&sb, "line %03d of a very long answer\n", i)
	}
	full := sb.String()
	agent := newAgent(full)
	hh := newHarness(t, agent, func(c *Config) { c.Budget = 500 })
	hh.deliver(t, "long", mention("write a lot"))

	if hh.z.count() < 3 {
		t.Fatalf("expected 3+ messages, got %d", hh.z.count())
	}
	// Reconstruct: strip the decorations and assert nothing was lost.
	var got strings.Builder
	bodies := hh.z.stored()
	for i, b := range bodies {
		b = strings.TrimSuffix(b, rollover.DefaultSealMarker)
		if i > 0 {
			b = strings.TrimPrefix(b, rollover.DefaultContMarker)
		}
		got.WriteString(b)
	}
	if !strings.Contains(got.String(), full) {
		t.Fatalf("text was lost in rollover: reconstructed %d chars, agent wrote %d",
			len(got.String()), len(full))
	}
	// Every message respects the budget, in code points.
	for i, b := range bodies {
		if n := len([]rune(b)); n > 500 {
			t.Fatalf("message %d is %d code points", i, n)
		}
	}
}

// --- errors --------------------------------------------------------------

func TestSplitterConfigError(t *testing.T) {
	hh := newHarness(t, newAgent("ok"), func(c *Config) {
		c.Budget = 50 // too small for the markers
	})
	hh.deliver(t, "t", mention("hi"))
	if hh.z.count() != 0 {
		t.Fatal("a broken splitter must not post")
	}
	if !hh.logged("splitter") {
		t.Fatal("expected a splitter error log")
	}
}

func TestSessionCreationErrorIsReported(t *testing.T) {
	hh := newHarness(t, newAgent("ok"), nil)
	hh.s.err = errors.New("agent is dead")
	hh.deliver(t, "t", mention("hi"))
	// The placeholder is resolved into an error rather than left
	// hanging forever.
	if got := hh.z.body(1); !strings.Contains(got, "agent is dead") {
		t.Fatalf("body = %q", got)
	}
	if len(hh.j.OpenTails()) != 0 {
		t.Fatal("tail not cleared after a failed turn")
	}
}

func TestPromptErrorIsReported(t *testing.T) {
	agent := newAgent("partial")
	agent.err = errors.New("model exploded")
	hh := newHarness(t, agent, nil)
	hh.deliver(t, "t", mention("hi"))
	if got := hh.z.body(1); !strings.Contains(got, "model exploded") {
		t.Fatalf("body = %q", got)
	}
	if !hh.logged("failed") {
		t.Fatal("expected a turn-failure log")
	}
}

func TestAbstainPromptErrorIsReported(t *testing.T) {
	agent := newAgent("first")
	hh := newHarness(t, agent, nil)
	hh.deliver(t, "t", mention("hi"))
	agent.mu.Lock()
	agent.err = errors.New("ambient boom")
	agent.mu.Unlock()
	hh.deliver(t, "t", "follow up")
	if !strings.Contains(strings.Join(hh.z.stored(), "\n"), "ambient boom") {
		t.Fatalf("stored = %v", hh.z.stored())
	}
}

func TestPlaceholderPostFailureIsNonFatal(t *testing.T) {
	hh := newHarness(t, newAgent("answer"), nil)
	hh.z.mu.Lock()
	hh.z.sendErr = errors.New("zulip down")
	hh.z.mu.Unlock()
	hh.deliver(t, "t", mention("hi"))
	if !hh.logged("placeholder post failed") {
		t.Fatal("expected a placeholder log")
	}
}

func TestStopReasonIsSurfaced(t *testing.T) {
	agent := newAgent("partial answer")
	agent.stop = acp.StopReasonMaxTokens
	hh := newHarness(t, agent, nil)
	hh.deliver(t, "t", mention("hi"))
	if got := hh.z.body(1); !strings.Contains(got, "stopped: max_tokens") {
		t.Fatalf("body = %q", got)
	}
}

func TestTailTrackingErrorsAreLogged(t *testing.T) {
	hh := newHarness(t, newAgent("ok"), nil)
	hh.deliver(t, "t", mention("hi"))
	// A journal that no longer knows the conversation makes both tail
	// writes fail; neither may escalate.
	hh.h.trackTail("nosuchconv", mustSplitter(t, hh.z))
	hh.h.clearTail("nosuchconv")
	if !hh.logged("recording tail") || !hh.logged("clearing tail") {
		t.Fatalf("expected tail logs, got %v", hh.logs)
	}
}

func mustSplitter(t *testing.T, z *fakeZulip) *rollover.Splitter {
	t.Helper()
	s, err := rollover.New(rollover.Config{Poster: &convPoster{client: z, key: journal.Channel(4, "t")}})
	if err != nil {
		t.Fatalf("rollover.New: %v", err)
	}
	if err := s.Start(context.Background(), "placeholder"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	return s
}

// --- cancellation --------------------------------------------------------

// TestFollowUpCancelsTheRunningTurn: a new message in the same topic
// supersedes whatever is still generating there.
func TestFollowUpCancelsTheRunningTurn(t *testing.T) {
	agent := newAgent("late answer")
	agent.block = make(chan struct{})
	hh := newHarness(t, agent, nil)

	msg := func(text string) zulipproto.Event {
		return zulipproto.Event{Type: zulipproto.EventMessage, Message: &zulipproto.Message{
			SenderID: humanID, SenderName: "Kfet", Content: text,
			StreamID: 4, Topic: "busy", Type: "stream",
		}}
	}
	hh.h.Handle(context.Background(), msg(mention("do something slow")))
	<-agent.entered // the first turn is genuinely in flight

	// The second message cancels it. Unblock so the cancelled Prompt
	// can observe its context and return.
	agent.mu.Lock()
	agent.block = nil
	agent.mu.Unlock()
	hh.h.Handle(context.Background(), msg("actually, do this instead"))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := hh.h.WaitIdle(ctx); err != nil {
		t.Fatalf("WaitIdle: %v", err)
	}
	hh.s.mu.Lock()
	cancels := len(hh.s.cancels)
	hh.s.mu.Unlock()
	if cancels != 1 {
		t.Fatalf("session cancel issued %d times, want 1", cancels)
	}
}

func TestWaitIdleRespectsContext(t *testing.T) {
	agent := newAgent("slow")
	agent.block = make(chan struct{})
	hh := newHarness(t, agent, nil)
	hh.h.Handle(context.Background(), zulipproto.Event{
		Type: zulipproto.EventMessage,
		Message: &zulipproto.Message{
			SenderID: humanID, SenderName: "Kfet", Content: mention("wait"),
			StreamID: 4, Topic: "t", Type: "stream",
		},
	})
	<-agent.entered
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := hh.h.WaitIdle(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("WaitIdle = %v", err)
	}
	close(agent.block)
	done, dcancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer dcancel()
	_ = hh.h.WaitIdle(done)
}

// --- topic rename --------------------------------------------------------

func TestTopicRenameKeepsTheSession(t *testing.T) {
	agent := newAgent("ok")
	hh := newHarness(t, agent, nil)
	hh.deliver(t, "untitled", mention("start"))
	convID := hh.j.Convs()[0].ID

	hh.h.Handle(context.Background(), zulipproto.Event{
		Type: zulipproto.EventUpdateMessage, StreamID: 4,
		OrigTopic: "untitled", Topic: "session: the real task",
	})
	if !hh.logged("session " + convID + " follows it") {
		t.Fatalf("rename not logged: %v", hh.logs)
	}
	// The next message in the RENAMED topic continues the same session.
	hh.deliver(t, "session: the real task", "carry on")
	if len(hh.s.sessions) != 1 {
		t.Fatalf("rename orphaned the session: %v", hh.s.sessions)
	}
	if _, ok := hh.s.sessions[convID]; !ok {
		t.Fatal("conv-id changed across a rename")
	}
}

func TestRenameEventsThatDoNothing(t *testing.T) {
	hh := newHarness(t, newAgent("ok"), nil)
	hh.deliver(t, "topic", mention("hi"))
	events := []zulipproto.Event{
		{Type: zulipproto.EventUpdateMessage, StreamID: 4, OrigTopic: "", Topic: "x"},
		{Type: zulipproto.EventUpdateMessage, StreamID: 4, OrigTopic: "x", Topic: ""},
		{Type: zulipproto.EventUpdateMessage, StreamID: 4, OrigTopic: "x", Topic: "x"},
		{Type: zulipproto.EventUpdateMessage, StreamID: 99, OrigTopic: "topic", Topic: "y"},
		{Type: zulipproto.EventUpdateMessage, StreamID: 4, OrigTopic: "unknown", Topic: "y"},
	}
	for _, ev := range events {
		hh.h.Handle(context.Background(), ev)
	}
	if c, ok := hh.j.Lookup(journal.Channel(4, "topic")); !ok || c.Topic != "topic" {
		t.Fatalf("a no-op rename disturbed the journal: %+v", hh.j.Convs())
	}
}

func TestRenameJournalErrorIsLogged(t *testing.T) {
	hh := newHarness(t, newAgent("ok"), nil)
	hh.deliver(t, "topic", mention("hi"))
	// Break the journal's backing store so the rename cannot persist.
	hh.breakJournal(t)
	hh.h.Handle(context.Background(), zulipproto.Event{
		Type: zulipproto.EventUpdateMessage, StreamID: 4,
		OrigTopic: "topic", Topic: "renamed",
	})
	if !hh.logged("topic rename") {
		t.Fatalf("expected a rename-failure log, got %v", hh.logs)
	}
}

func TestConversationAllocationErrorIsLogged(t *testing.T) {
	hh := newHarness(t, newAgent("ok"), nil)
	hh.breakJournal(t)
	hh.deliver(t, "brand new", mention("hi"))
	if hh.z.count() != 0 {
		t.Fatal("posted despite failing to allocate a conversation")
	}
	if !hh.logged("allocate conversation") {
		t.Fatalf("logs = %v", hh.logs)
	}
}

// --- restart -------------------------------------------------------------

func TestMarkInterrupted(t *testing.T) {
	hh := newHarness(t, newAgent("ok"), nil)
	ctx := context.Background()

	// Three conversations: one with a live tail, one whose tail was
	// already sealed, one whose message has vanished.
	live, err := hh.j.Ensure(journal.Channel(4, "live"))
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	sealed, err := hh.j.Ensure(journal.Channel(4, "sealed"))
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	gone, err := hh.j.Ensure(journal.Channel(4, "gone"))
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	liveID, _ := hh.z.SendMessage(ctx, 4, "live", "half an answer")
	sealedID, _ := hh.z.SendMessage(ctx, 4, "sealed", "a full answer"+rollover.DefaultSealMarker)
	for _, p := range []struct {
		id  string
		msg int64
	}{{live.ID, liveID}, {sealed.ID, sealedID}, {gone.ID, 9999}} {
		if err := hh.j.SetTail(p.id, p.msg); err != nil {
			t.Fatalf("SetTail: %v", err)
		}
	}

	hh.h.MarkInterrupted(ctx)

	if got := hh.z.body(liveID); !strings.HasSuffix(got, InterruptedMarker) {
		t.Fatalf("live tail not marked: %q", got)
	}
	if got := hh.z.body(sealedID); strings.Contains(got, InterruptedMarker) {
		t.Fatalf("a sealed message was edited: %q", got)
	}
	if len(hh.j.OpenTails()) != 0 {
		t.Fatalf("tails not cleared: %+v", hh.j.OpenTails())
	}
	if !hh.logged("reading interrupted message") {
		t.Fatal("expected a log for the vanished message")
	}

	// Running it twice must not stack markers.
	if err := hh.j.SetTail(live.ID, liveID); err != nil {
		t.Fatalf("SetTail: %v", err)
	}
	hh.h.MarkInterrupted(ctx)
	if n := strings.Count(hh.z.body(liveID), strings.TrimSpace(InterruptedMarker)); n != 1 {
		t.Fatalf("marker applied %d times", n)
	}
}

func TestMarkInterruptedEditFailureIsLogged(t *testing.T) {
	hh := newHarness(t, newAgent("ok"), nil)
	ctx := context.Background()
	c, err := hh.j.Ensure(journal.Channel(4, "live"))
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	id, _ := hh.z.SendMessage(ctx, 4, "live", "half an answer")
	if err := hh.j.SetTail(c.ID, id); err != nil {
		t.Fatalf("SetTail: %v", err)
	}
	hh.z.mu.Lock()
	hh.z.editErr = errors.New("edit window closed")
	hh.z.mu.Unlock()
	hh.h.MarkInterrupted(ctx)
	if !hh.logged("marking message") {
		t.Fatalf("logs = %v", hh.logs)
	}
}

// --- outbox --------------------------------------------------------------

func TestOutboxUploads(t *testing.T) {
	agent := newAgent("here you go")
	hh := newHarness(t, agent, nil)
	// Populate the outbox from inside the turn, which is when a real
	// agent would write it.
	agent.mu.Lock()
	orig := agent.chunks
	agent.mu.Unlock()
	_ = orig

	// First turn creates the session (and therefore the cwd).
	hh.deliver(t, "files", mention("prepare a file"))
	cwd := hh.s.sessions[hh.j.Convs()[0].ID].Cwd
	outbox := filepath.Join(cwd, OutboxDir)
	if err := os.MkdirAll(outbox, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	payload := []byte("log line one\nlog line two\n")
	for _, name := range []string{"report.log", "patch.diff"} {
		if err := os.WriteFile(filepath.Join(outbox, name), payload, 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	// A dotfile and a subdirectory are both skipped.
	if err := os.WriteFile(filepath.Join(outbox, ".hidden"), payload, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(outbox, "sub"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	hh.deliver(t, "files", "and send it over")

	body := hh.z.body(2)
	if !strings.Contains(body, "**Attachments:**") ||
		!strings.Contains(body, "[report.log](/user_uploads/2/ab/report.log)") ||
		!strings.Contains(body, "[patch.diff](/user_uploads/2/ab/patch.diff)") {
		t.Fatalf("attachment links missing: %q", body)
	}
	hh.z.mu.Lock()
	if string(hh.z.uploads["report.log"]) != string(payload) {
		t.Fatal("uploaded bytes differ from the file")
	}
	if _, leaked := hh.z.uploads[".hidden"]; leaked {
		t.Fatal("dotfile was uploaded")
	}
	hh.z.mu.Unlock()

	// Uploaded files move aside so a follow-up does not re-upload them.
	if _, err := os.Stat(filepath.Join(outbox, sentDir, "report.log")); err != nil {
		t.Fatalf("file not moved to .sent: %v", err)
	}
	hh.deliver(t, "files", "anything else?")
	if strings.Contains(hh.z.body(3), "Attachments") {
		t.Fatalf("re-uploaded on a later turn: %q", hh.z.body(3))
	}
}

func TestOutboxErrors(t *testing.T) {
	hh := newHarness(t, newAgent("ok"), nil)
	ctx := context.Background()

	// A missing outbox is the normal case and is silent.
	if got := hh.h.uploadOutbox(ctx, t.TempDir()); got != "" {
		t.Fatalf("missing outbox produced %q", got)
	}
	if hh.logged("reading outbox") {
		t.Fatal("a missing outbox must not be logged")
	}

	// An unreadable outbox is logged, not fatal.
	cwd := t.TempDir()
	blocker := filepath.Join(cwd, OutboxDir)
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := hh.h.uploadOutbox(ctx, cwd); got != "" {
		t.Fatalf("broken outbox produced %q", got)
	}
	if !hh.logged("reading outbox") {
		t.Fatal("expected an outbox log")
	}

	// A failing upload skips that file and keeps going.
	cwd2 := t.TempDir()
	ob := filepath.Join(cwd2, OutboxDir)
	if err := os.MkdirAll(ob, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ob, "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	hh.z.mu.Lock()
	hh.z.uploadEr = errors.New("upload rejected")
	hh.z.mu.Unlock()
	if got := hh.h.uploadOutbox(ctx, cwd2); got != "" {
		t.Fatalf("failed upload produced %q", got)
	}
	if !hh.logged("uploading a.txt") {
		t.Fatalf("logs = %v", hh.logs)
	}

	// An unreadable file is skipped too.
	hh.z.mu.Lock()
	hh.z.uploadEr = nil
	hh.z.mu.Unlock()
	if err := os.Chmod(filepath.Join(ob, "a.txt"), 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(ob, "a.txt"), 0o600) })
	if got := hh.h.uploadOutbox(ctx, cwd2); got != "" {
		t.Fatalf("unreadable file produced %q", got)
	}
}

func TestOutboxRenameFailure(t *testing.T) {
	hh := newHarness(t, newAgent("ok"), nil)
	cwd := t.TempDir()
	ob := filepath.Join(cwd, OutboxDir)
	if err := os.MkdirAll(filepath.Join(ob, sentDir), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ob, "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	// A directory at the destination makes the rename fail.
	if err := os.MkdirAll(filepath.Join(ob, sentDir, "a.txt", "child"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if got := hh.h.uploadOutbox(context.Background(), cwd); got != "" {
		t.Fatalf("rename failure produced %q", got)
	}
	if !hh.logged("uploading a.txt") {
		t.Fatalf("logs = %v", hh.logs)
	}
}

func TestOutboxMkdirFailure(t *testing.T) {
	hh := newHarness(t, newAgent("ok"), nil)
	cwd := t.TempDir()
	ob := filepath.Join(cwd, OutboxDir)
	if err := os.MkdirAll(ob, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ob, "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	// A regular file where .sent should be makes MkdirAll fail.
	if err := os.WriteFile(filepath.Join(ob, sentDir), []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := hh.h.uploadOutbox(context.Background(), cwd); got != "" {
		t.Fatalf("mkdir failure produced %q", got)
	}
}

// --- background loops ----------------------------------------------------

func TestWatchdogLoop(t *testing.T) {
	z := newZulip()
	split, err := rollover.New(rollover.Config{Poster: &convPoster{client: z, key: journal.Channel(4, "t")}})
	if err != nil {
		t.Fatalf("rollover.New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tick := make(chan time.Time)
	afterCalled := make(chan struct{}, 4)
	done := make(chan struct{})
	go func() {
		defer close(done)
		watchdogLoop(ctx, split, tick, func() { afterCalled <- struct{}{} })
	}()

	// A tick with nothing pending must issue no call at all.
	tick <- time.Time{}
	if z.count() != 0 {
		t.Fatal("an idle tick published something")
	}
	// A tick with pending text publishes it and runs the callback.
	split.Append("streamed text")
	tick <- time.Time{}
	<-afterCalled
	if z.body(1) != "streamed text" {
		t.Fatalf("body = %q", z.body(1))
	}
	cancel()
	// The loop is parked in select; unblock it so it can see ctx.
	select {
	case tick <- time.Time{}:
	case <-done:
	}
	<-done
}

func TestWatchdogStopsOnFlushError(t *testing.T) {
	z := newZulip()
	z.sendErr = errors.New("zulip down")
	split, err := rollover.New(rollover.Config{Poster: &convPoster{client: z, key: journal.Channel(4, "t")}})
	if err != nil {
		t.Fatalf("rollover.New: %v", err)
	}
	split.Append("text")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tick := make(chan time.Time)
	done := make(chan struct{})
	go func() { defer close(done); watchdogLoop(ctx, split, tick, func() {}) }()
	tick <- time.Time{}
	<-done // the loop exits by itself rather than hammering a dead server
}

// TestWatchdogTickerWrapper covers the thin ticker wrapper around the
// testable loop.
func TestWatchdogTickerWrapper(t *testing.T) {
	z := newZulip()
	split, err := rollover.New(rollover.Config{Poster: &convPoster{client: z, key: journal.Channel(4, "t")}})
	if err != nil {
		t.Fatalf("rollover.New: %v", err)
	}
	split.Append("via the real ticker")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	published := make(chan struct{})
	go watchdog(ctx, split, time.Millisecond, func() { close(published) })
	<-published
}

func TestSpinnerAnimatesThenDisarms(t *testing.T) {
	z := newZulip()
	split, err := rollover.New(rollover.Config{Poster: &convPoster{client: z, key: journal.Channel(4, "t")}})
	if err != nil {
		t.Fatalf("rollover.New: %v", err)
	}
	if err := split.Start(context.Background(), statusline.Thinking(statusline.Status{})); err != nil {
		t.Fatalf("Start: %v", err)
	}
	<-z.posted
	sink := newStreamingSink(split, false)
	sink.SetProviderEmoji("🤖")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tick := make(chan time.Time)
	done := make(chan struct{})
	go func() { defer close(done); spinnerLoop(ctx, split, sink, tick) }()
	tick <- time.Time{}
	<-z.posted // an animated frame landed
	if got := z.body(1); !strings.Contains(got, "Thinking.") {
		t.Fatalf("frame = %q", got)
	}
	// The first real text closes the placeholder window, and the
	// spinner disarms itself without being cancelled.
	split.Append("the answer")
	tick <- time.Time{}
	<-done
}

// TestSpinnerTickerWrapper covers the thin ticker wrapper.
func TestSpinnerTickerWrapper(t *testing.T) {
	z := newZulip()
	split, err := rollover.New(rollover.Config{Poster: &convPoster{client: z, key: journal.Channel(4, "t")}})
	if err != nil {
		t.Fatalf("rollover.New: %v", err)
	}
	if err := split.Start(context.Background(), "placeholder"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	<-z.posted
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go spinner(ctx, split, newStreamingSink(split, false), time.Millisecond)
	<-z.posted
}

func TestSpinnerStopsOnContextCancel(t *testing.T) {
	z := newZulip()
	split, err := rollover.New(rollover.Config{Poster: &convPoster{client: z, key: journal.Channel(4, "t")}})
	if err != nil {
		t.Fatalf("rollover.New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		spinnerLoop(ctx, split, newStreamingSink(split, false), make(chan time.Time))
	}()
	cancel()
	<-done
}

// --- sink ----------------------------------------------------------------

func TestSinkRendering(t *testing.T) {
	z := newZulip()
	split, err := rollover.New(rollover.Config{Poster: &convPoster{client: z, key: journal.Channel(4, "t")}})
	if err != nil {
		t.Fatalf("rollover.New: %v", err)
	}
	sink := newStreamingSink(split, false)
	ctx := context.Background()

	// Status _meta arrives first and lands in the header.
	if err := sink.OnUpdate(ctx, acp.SessionNotification{
		Meta: map[string]any{statusline.ExtensionID: map[string]any{"mood": "steady", "plan": "1/3"}},
	}); err != nil {
		t.Fatalf("OnUpdate: %v", err)
	}
	if got := sink.Status(); got.Mood != "steady" || got.Plan != "1/3" {
		t.Fatalf("status = %+v", got)
	}
	if err := sink.OnUpdate(ctx, thoughtNotification("thinking\nabout\nit", nil)); err != nil {
		t.Fatalf("OnUpdate: %v", err)
	}
	if err := sink.OnUpdate(ctx, chunkNotification("body text", nil)); err != nil {
		t.Fatalf("OnUpdate: %v", err)
	}
	got := split.Transcript()
	if !strings.HasPrefix(got, "> *steady • 1/3*\n") {
		t.Fatalf("header not prepended once: %q", got)
	}
	// A multi-line thought becomes one italic line.
	if !strings.Contains(got, "*thinking about it*\n") {
		t.Fatalf("thought = %q", got)
	}
	if !strings.HasSuffix(got, "body text") {
		t.Fatalf("body = %q", got)
	}
	// Updates that produce nothing visible append nothing.
	before := split.Transcript()
	if err := sink.OnUpdate(ctx, acp.SessionNotification{
		Update: acp.SessionUpdate{AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{
			Content: acp.ContentBlock{},
		}},
	}); err != nil {
		t.Fatalf("OnUpdate: %v", err)
	}
	if err := sink.OnUpdate(ctx, acp.SessionNotification{}); err != nil {
		t.Fatalf("OnUpdate: %v", err)
	}
	if split.Transcript() != before {
		t.Fatal("an empty update appended text")
	}
}

func TestSinkHidesThinking(t *testing.T) {
	z := newZulip()
	split, err := rollover.New(rollover.Config{Poster: &convPoster{client: z, key: journal.Channel(4, "t")}})
	if err != nil {
		t.Fatalf("rollover.New: %v", err)
	}
	sink := newStreamingSink(split, true)
	if err := sink.OnUpdate(context.Background(), thoughtNotification("secret", nil)); err != nil {
		t.Fatalf("OnUpdate: %v", err)
	}
	if split.Transcript() != "" {
		t.Fatalf("thought leaked: %q", split.Transcript())
	}
	// An empty thought renders nothing even when thoughts are shown.
	sink2 := newStreamingSink(split, false)
	if err := sink2.OnUpdate(context.Background(), thoughtNotification("", nil)); err != nil {
		t.Fatalf("OnUpdate: %v", err)
	}
	if split.Transcript() != "" {
		t.Fatalf("empty thought produced %q", split.Transcript())
	}
}

func TestSinkHeaderOmittedWhenEmpty(t *testing.T) {
	z := newZulip()
	split, err := rollover.New(rollover.Config{Poster: &convPoster{client: z, key: journal.Channel(4, "t")}})
	if err != nil {
		t.Fatalf("rollover.New: %v", err)
	}
	sink := newStreamingSink(split, false)
	if err := sink.OnUpdate(context.Background(), chunkNotification("plain", nil)); err != nil {
		t.Fatalf("OnUpdate: %v", err)
	}
	if split.Transcript() != "plain" {
		t.Fatalf("an empty status must add no header: %q", split.Transcript())
	}
}

func TestOneLineCapsRunes(t *testing.T) {
	long := strings.Repeat("漢", 300)
	got := oneLine(long)
	if r := []rune(got); len(r) != 201 || r[200] != '…' {
		t.Fatalf("oneLine produced %d runes", len(r))
	}
	if !strings.HasPrefix(got, "漢") {
		t.Fatal("multibyte prefix mangled")
	}
}

// --- poster --------------------------------------------------------------

func TestTopicPoster(t *testing.T) {
	z := newZulip()
	p := &convPoster{client: z, key: journal.Channel(4, "a topic")}
	ctx := context.Background()
	id, err := p.Post(ctx, "hello")
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	if z.topics[id] != "a topic" {
		t.Fatalf("topic = %q", z.topics[id])
	}
	if err := p.Edit(ctx, id, "goodbye"); err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if z.body(id) != "goodbye" {
		t.Fatalf("body = %q", z.body(id))
	}
}

// TestFailTurnCannotReportIsLogged: when the agent fails AND Zulip is
// also down, the relay logs and gives up rather than spinning.
func TestFailTurnCannotReportIsLogged(t *testing.T) {
	agent := newAgent("partial")
	agent.err = errors.New("model exploded")
	hh := newHarness(t, agent, nil)
	hh.z.mu.Lock()
	hh.z.sendErr = errors.New("zulip down")
	hh.z.mu.Unlock()
	hh.deliver(t, "t", mention("hi"))
	if !hh.logged("reporting error into") {
		t.Fatalf("logs = %v", hh.logs)
	}
}

// TestWatchdogPublishesMidTurn pins the streaming property end to end:
// the answer reaches Zulip while the agent is still working, and the
// journal learns which message the relay owns before the turn ends.
// That tail record is what makes crash recovery possible at all.
func TestWatchdogPublishesMidTurn(t *testing.T) {
	agent := newAgent("streamed mid-turn")
	agent.hold = make(chan struct{})
	hh := newHarness(t, agent, nil)

	hh.h.Handle(context.Background(), zulipproto.Event{
		Type: zulipproto.EventMessage,
		Message: &zulipproto.Message{
			SenderID: humanID, SenderName: "Kfet", Content: mention("stream to me"),
			StreamID: 4, Topic: "streaming", Type: "stream",
		},
	})
	// Wait until the streamed text is actually stored on the surface,
	// while the agent's turn is still open.
	deadline := time.After(10 * time.Second)
	for {
		if strings.Contains(hh.z.body(1), "streamed mid-turn") {
			break
		}
		select {
		case <-hh.z.posted:
		case <-deadline:
			t.Fatalf("nothing streamed mid-turn; body = %q", hh.z.body(1))
		}
	}
	tails := hh.j.OpenTails()
	if len(tails) != 1 || tails[0].TailID != 1 {
		t.Fatalf("tail not recorded mid-turn: %+v", tails)
	}
	close(agent.hold)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := hh.h.WaitIdle(ctx); err != nil {
		t.Fatalf("WaitIdle: %v", err)
	}
	if len(hh.j.OpenTails()) != 0 {
		t.Fatal("tail not cleared after the turn")
	}
}

// TestSystemBotAndOtherBotsAreRefused pins the second half of the
// self-loop guard. Zulip posts topic moves and welcome messages as
// cross-realm system bots; those land in an engaged topic and would
// otherwise burn a full agent turn on "This topic was moved here
// from …". Cross-realm bots do not appear in GET /users, so the realm
// string is the only signal that catches them.
func TestSystemBotAndOtherBotsAreRefused(t *testing.T) {
	cases := []struct {
		name string
		msg  zulipproto.Message
	}{
		{"system bot", zulipproto.Message{
			SenderID: 6, SenderName: "Notification Bot", Type: "stream",
			SenderRealm: zulipproto.SystemBotRealm, Client: "Internal",
			Content:  "This topic was moved here from #**fleet>old**.",
			StreamID: 4, Topic: "engaged",
		}},
		{"another realm bot", zulipproto.Message{
			SenderID: 77, SenderName: "Other Relay", Type: "stream",
			Content: mention("hello from a peer relay"), StreamID: 4, Topic: "engaged",
		}},
	}
	for _, c := range cases {
		hh := newHarness(t, newAgent("ok"), func(cfg *Config) {
			cfg.BotSenderIDs = map[int64]struct{}{77: {}}
		})
		// Engage the topic first, so only the sender check can stop it.
		hh.deliver(t, "engaged", mention("hi"))
		posted := hh.z.count()

		m := c.msg
		hh.h.Handle(context.Background(), zulipproto.Event{Type: zulipproto.EventMessage, Message: &m})
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := hh.h.WaitIdle(ctx); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		cancel()
		if hh.z.count() != posted {
			t.Fatalf("%s: relay reacted to a bot message", c.name)
		}
		if len(hh.a.prompts) != 1 {
			t.Fatalf("%s: a bot message reached the agent: %v", c.name, hh.a.prompts)
		}
	}
}

// TestRescueUploadsOutputWhenPostingFails is the "never drop output"
// backstop. Zulip can refuse a body that is legal by length but too
// expensive to render (HTTP 400 "Unable to render message"), and the
// realm can close its edit window. Neither may cost the agent's work.
func TestRescueUploadsOutputWhenPostingFails(t *testing.T) {
	agent := newAgent("an answer worth keeping")
	hh := newHarness(t, agent, nil)
	// The placeholder posts fine; every later write fails.
	hh.z.mu.Lock()
	hh.z.editErr = errors.New("Unable to render message")
	hh.z.mu.Unlock()
	hh.deliver(t, "doomed", mention("hi"))

	hh.z.mu.Lock()
	rescued, ok := hh.z.uploads["answer.md"]
	hh.z.mu.Unlock()
	if !ok {
		t.Fatalf("output was dropped; uploads = %v", hh.z.uploads)
	}
	if !strings.Contains(string(rescued), "an answer worth keeping") {
		t.Fatalf("rescued the wrong thing: %q", rescued)
	}
	if !hh.logged("rescued") {
		t.Fatalf("logs = %v", hh.logs)
	}
	// And the topic learns where the answer went.
	if !strings.Contains(strings.Join(hh.z.stored(), "\n"), "[answer.md](/user_uploads/2/ab/answer.md)") {
		t.Fatalf("no notice posted: %v", hh.z.stored())
	}
}

func TestRescueFailuresAreLogged(t *testing.T) {
	// Nothing to rescue: silent.
	hh := newHarness(t, newAgent("x"), nil)
	post := &convPoster{client: hh.z, key: journal.Channel(4, "t")}
	hh.h.rescue(context.Background(), post, "   ", errors.New("cause"))
	hh.z.mu.Lock()
	n := len(hh.z.uploads)
	hh.z.mu.Unlock()
	if n != 0 {
		t.Fatal("an empty transcript was uploaded")
	}

	// The upload itself fails: logged, never escalated.
	hh.z.mu.Lock()
	hh.z.uploadEr = errors.New("upload refused")
	hh.z.mu.Unlock()
	hh.h.rescue(context.Background(), post, "real output", errors.New("cause"))
	if !hh.logged("could not rescue") {
		t.Fatalf("logs = %v", hh.logs)
	}

	// The upload works but the announcement does not.
	hh2 := newHarness(t, newAgent("x"), nil)
	hh2.z.mu.Lock()
	hh2.z.sendErr = errors.New("zulip down")
	hh2.z.mu.Unlock()
	hh2.h.rescue(context.Background(), &convPoster{client: hh2.z, key: journal.Channel(4, "t")},
		"real output", errors.New("cause"))
	if !hh2.logged("could not announce it") {
		t.Fatalf("logs = %v", hh2.logs)
	}
}

// TestSupersededTurnReadsAsSuperseded: cancelling a running turn is
// something the relay did on purpose, so it must not surface as
// "error: context canceled" — noise pointing at nothing the user can
// act on.
func TestSupersededTurnReadsAsSuperseded(t *testing.T) {
	agent := newAgent("half an answer")
	agent.block = make(chan struct{})
	hh := newHarness(t, agent, nil)
	msg := func(text string) zulipproto.Event {
		return zulipproto.Event{Type: zulipproto.EventMessage, Message: &zulipproto.Message{
			SenderID: humanID, SenderName: "Kfet", Content: text,
			StreamID: 4, Topic: "busy", Type: "stream",
		}}
	}
	hh.h.Handle(context.Background(), msg(mention("something slow")))
	<-agent.entered
	agent.mu.Lock()
	agent.block = nil
	agent.mu.Unlock()
	hh.h.Handle(context.Background(), msg("actually, this instead"))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := hh.h.WaitIdle(ctx); err != nil {
		t.Fatalf("WaitIdle: %v", err)
	}
	all := strings.Join(hh.z.stored(), "\n")
	if !strings.Contains(all, "superseded by your next message") {
		t.Fatalf("stored = %q", all)
	}
	if strings.Contains(all, "context canceled") {
		t.Fatalf("raw cancellation leaked to the surface: %q", all)
	}
}

// --- sentinel watch ------------------------------------------------------

// chunkSplits returns the same text as: one chunk, one rune per chunk,
// and split in the middle. A prefix scanner that is right for one
// splitting and wrong for another is not a scanner.
func chunkSplits(s string) [][]string {
	one := []string{s}
	if s == "" {
		one = nil
	}
	var runes []string
	for _, r := range s {
		runes = append(runes, string(r))
	}
	half := []string{s}
	if n := len([]rune(s)); n > 1 {
		r := []rune(s)
		half = []string{string(r[:n/2]), string(r[n/2:])}
	}
	return [][]string{one, runes, half}
}

func TestSentinelWatch(t *testing.T) {
	const sentinel = "<<SILENT>>"
	cases := []struct {
		name string
		text string
		want bool
	}{
		{"exact sentinel never fires", sentinel, false},
		{"sentinel with surrounding whitespace never fires", "\n\n  " + sentinel + "\n", false},
		{"empty stream never fires", "", false},
		{"whitespace only never fires", "   \n\t", false},
		{"a normal answer fires", "here is the answer", true},
		{"sentinel prefix that diverges fires", "<<SILENTLY ignoring that>>", true},
		{"sentinel plus trailing text fires", sentinel + " actually, wait", true},
		{"leading whitespace then an answer fires", "\n\n  hello", true},
	}
	for _, c := range cases {
		for i, chunks := range chunkSplits(c.text) {
			t.Run(fmt.Sprintf("%s/%d", c.name, i), func(t *testing.T) {
				var fired int
				down := &capturingSink{}
				w := &sentinelWatch{next: down, sentinel: sentinel, onCommit: func() { fired++ }}
				for _, ch := range chunks {
					if err := w.OnUpdate(context.Background(), chunkNotification(ch, nil)); err != nil {
						t.Fatalf("OnUpdate: %v", err)
					}
				}
				// Non-message updates are delegated and never observed.
				if err := w.OnUpdate(context.Background(), thoughtNotification("thinking", nil)); err != nil {
					t.Fatalf("OnUpdate: %v", err)
				}
				want := 0
				if c.want {
					want = 1
				}
				if fired != want {
					t.Fatalf("onCommit fired %d times, want %d", fired, want)
				}
				if got := len(down.got); got != len(chunks)+1 {
					t.Fatalf("delegated %d updates, want %d", got, len(chunks)+1)
				}
			})
		}
	}
}

// TestSentinelWatchWithoutCallback covers the nil-onCommit guard: the
// watch must be usable as a pure pass-through.
func TestSentinelWatchWithoutCallback(t *testing.T) {
	down := &capturingSink{}
	w := &sentinelWatch{next: down, sentinel: "<<SILENT>>"}
	if err := w.OnUpdate(context.Background(), chunkNotification("an answer", nil)); err != nil {
		t.Fatalf("OnUpdate: %v", err)
	}
	// A chunk with no text block at all is delegated but not observed.
	if err := w.OnUpdate(context.Background(), acp.SessionNotification{
		Update: acp.SessionUpdate{AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{}},
	}); err != nil {
		t.Fatalf("OnUpdate: %v", err)
	}
	if len(down.got) != 2 {
		t.Fatalf("delegated %d updates, want 2", len(down.got))
	}
}

// capturingSink records what a sink wrapper delegated downstream.
type capturingSink struct {
	mu  sync.Mutex
	got []acp.SessionNotification
}

func (c *capturingSink) OnUpdate(_ context.Context, n acp.SessionNotification) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.got = append(c.got, n)
	return nil
}

// TestAmbientPlaceholderGoesUpBeforeTheTurnEnds is the whole point of
// sentinelWatch: the user must see something while the agent is still
// working, not only when it finishes.
func TestAmbientPlaceholderGoesUpBeforeTheTurnEnds(t *testing.T) {
	agent := newAgent("first")
	hh := newHarness(t, agent, nil)
	hh.deliver(t, "t", mention("hi"))

	agent.mu.Lock()
	agent.chunks = []string{"working on it"}
	agent.hold = make(chan struct{})
	agent.mu.Unlock()

	hh.h.Handle(context.Background(), zulipproto.Event{
		Type: zulipproto.EventMessage,
		Message: &zulipproto.Message{
			ID: 2, SenderID: humanID, SenderName: "Kfet", Content: "and what about X?",
			StreamID: 4, Topic: "t", Type: "stream",
		},
	})
	deadline := time.After(10 * time.Second)
	for hh.z.count() < 2 {
		select {
		case <-hh.z.posted:
		case <-deadline:
			t.Fatal("no placeholder posted while the ambient turn was still running")
		}
	}
	if body := hh.z.body(2); !strings.Contains(body, "Thinking") {
		t.Fatalf("placeholder body = %q", body)
	}
	if tails := hh.j.OpenTails(); len(tails) != 1 || tails[0].TailID != 2 {
		t.Fatalf("tail not recorded for the early placeholder: %+v", tails)
	}

	close(agent.hold)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := hh.h.WaitIdle(ctx); err != nil {
		t.Fatalf("WaitIdle: %v", err)
	}
	if body := hh.z.body(2); !strings.Contains(body, "working on it") {
		t.Fatalf("answer not delivered into the placeholder: %q", body)
	}
	if hh.z.count() != 2 {
		t.Fatalf("answer landed in a new message instead of the placeholder: %v", hh.z.stored())
	}
}

// TestAmbientPlaceholderFailureIsNonFatal drives the early-placeholder
// error branch: the post fails, the turn still answers.
func TestAmbientPlaceholderFailureIsNonFatal(t *testing.T) {
	agent := newAgent("first")
	hh := newHarness(t, agent, nil)
	hh.deliver(t, "t", mention("hi"))

	hh.z.mu.Lock()
	hh.z.sendErr = errors.New("zulip down")
	hh.z.mu.Unlock()
	agent.mu.Lock()
	agent.chunks = []string{"an answer"}
	agent.mu.Unlock()
	hh.deliver(t, "t", "follow up")

	if !hh.logged("placeholder post failed") {
		t.Fatalf("expected the placeholder failure to be logged: %v", hh.logs)
	}
}

// --- acknowledgement reaction -------------------------------------------

func TestAckReaction(t *testing.T) {
	cases := []struct {
		name  string
		tune  func(*Config)
		setup func(*harness, *fakeAgent)
		// second is the ambient follow-up text; the first turn is
		// always an @-mention.
		second   string
		wantAdd  []string
		wantDel  []string
		wantLogs []string
	}{
		{
			name:    "addressed turn",
			tune:    func(c *Config) { c.AckEmoji = "eyes" },
			second:  "",
			wantAdd: []string{"1:eyes"},
			wantDel: []string{"1:eyes"},
		},
		{
			name: "ambient abstain still retracts",
			tune: func(c *Config) { c.AckEmoji = "eyes" },
			setup: func(_ *harness, a *fakeAgent) {
				a.mu.Lock()
				a.chunks = []string{"<<SILENT>>"}
				a.mu.Unlock()
			},
			second:  "chatter",
			wantAdd: []string{"1:eyes", "1:eyes"},
			wantDel: []string{"1:eyes", "1:eyes"},
		},
		{
			name: "agent error still retracts",
			tune: func(c *Config) { c.AckEmoji = "eyes" },
			setup: func(_ *harness, a *fakeAgent) {
				a.mu.Lock()
				a.err = errors.New("agent exploded")
				a.mu.Unlock()
			},
			second:  "follow up",
			wantAdd: []string{"1:eyes", "1:eyes"},
			wantDel: []string{"1:eyes", "1:eyes"},
		},
		{
			name: "reaction failures are non-fatal",
			tune: func(c *Config) { c.AckEmoji = "eyes" },
			setup: func(hh *harness, _ *fakeAgent) {
				hh.z.mu.Lock()
				hh.z.reactErr = errors.New("no such emoji")
				hh.z.mu.Unlock()
			},
			wantAdd:  []string{"1:eyes"},
			wantDel:  []string{"1:eyes"},
			wantLogs: []string{"adding :eyes:", "removing :eyes:"},
		},
		{
			name: "empty emoji disables the feature",
			tune: func(c *Config) { c.AckEmoji = "" },
		},
		{
			name:    "custom emoji is honoured",
			tune:    func(c *Config) { c.AckEmoji = "hourglass" },
			wantAdd: []string{"1:hourglass"},
			wantDel: []string{"1:hourglass"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			agent := newAgent("an answer")
			hh := newHarness(t, agent, c.tune)
			if c.setup != nil {
				c.setup(hh, agent)
			}
			hh.deliver(t, "t", mention("hi"))
			if c.second != "" {
				hh.deliver(t, "t", c.second)
			}
			add, del := hh.z.reactions()
			if !slices.Equal(add, c.wantAdd) {
				t.Fatalf("added = %v, want %v", add, c.wantAdd)
			}
			if !slices.Equal(del, c.wantDel) {
				t.Fatalf("removed = %v, want %v", del, c.wantDel)
			}
			for _, want := range c.wantLogs {
				if !hh.logged(want) {
					t.Fatalf("missing log %q in %v", want, hh.logs)
				}
			}
		})
	}
}

// TestAckReactionSurvivesCancellation pins the WithoutCancel: a turn
// superseded by a follow-up must still take its reaction back off.
func TestAckReactionSurvivesCancellation(t *testing.T) {
	agent := newAgent("slow answer")
	agent.block = make(chan struct{})
	hh := newHarness(t, agent, func(c *Config) { c.AckEmoji = "eyes" })

	hh.h.Handle(context.Background(), zulipproto.Event{
		Type: zulipproto.EventMessage,
		Message: &zulipproto.Message{
			ID: 7, SenderID: humanID, SenderName: "Kfet", Content: mention("slow please"),
			StreamID: 4, Topic: "t", Type: "stream",
		},
	})
	<-agent.entered

	agent.mu.Lock()
	agent.block = nil
	agent.mu.Unlock()
	hh.deliver(t, "t", mention("never mind"))

	// The superseded turn is deleted from h.inflight the instant it is
	// cancelled, so WaitIdle above says nothing about its deferred
	// retraction. Wait for both removals to actually reach the server.
	for i := 0; i < 2; i++ {
		select {
		case <-hh.z.unreacted:
		case <-time.After(10 * time.Second):
			add, del := hh.z.reactions()
			t.Fatalf("reactions: added %v, removed %v — a cancelled turn left its ack behind", add, del)
		}
	}

	add, del := hh.z.reactions()
	if len(add) != 2 || len(del) != 2 {
		t.Fatalf("reactions: added %v, removed %v — a cancelled turn left its ack behind", add, del)
	}
}

// TestReactionEventsAreIgnored pins the loop guard: the relay's own
// ack reaction produces a `reaction` event, and re-ingesting it would
// start a turn per turn, forever.
func TestReactionEventsAreIgnored(t *testing.T) {
	agent := newAgent("must not run")
	hh := newHarness(t, agent, func(c *Config) { c.AckEmoji = "eyes" })
	// Engage the topic first, so ambient gating cannot be what saves us.
	hh.deliver(t, "t", mention("hi"))
	before := hh.z.count()

	for _, ev := range []zulipproto.Event{
		{Type: "reaction", MessageID: 1, StreamID: 4, Topic: "t"},
		{Type: "reaction", MessageID: 1, StreamID: 4, Topic: "t", Message: &zulipproto.Message{
			ID: 1, SenderID: humanID, SenderName: "Kfet", Content: "hi", StreamID: 4, Topic: "t", Type: "stream",
		}},
	} {
		hh.h.Handle(context.Background(), ev)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := hh.h.WaitIdle(ctx); err != nil {
		t.Fatalf("WaitIdle: %v", err)
	}
	if hh.z.count() != before {
		t.Fatalf("a reaction event started a turn: %v", hh.z.stored())
	}
}
