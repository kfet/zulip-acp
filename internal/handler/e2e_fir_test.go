//go:build e2e

package handler

// End-to-end test of interject against a REAL fir binary ($FIR_BIN) in
// ACP mode, with a mock OpenAI-compatible LLM. Run with:
//
//	FIR_BIN=$(which fir) go test -tags e2e -run E2E ./internal/handler/
//
// The main turn runs a bash tool that blocks until the test releases
// it; a second message arrives meanwhile and goes to a fork. fir runs
// with a 1s ACP idle TTL, so its idle reaper passes over the running
// session before the side question: a fir that reaps a running turn
// aborts the tool and refuses the steer (fixed in fir 1.54.1).

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kfet/acp-kit/client"
	"github.com/kfet/acp-kit/interject"
	"github.com/kfet/acp-kit/state"
	"github.com/kfet/zulip-acp/internal/channels"
	"github.com/kfet/zulip-acp/internal/journal"
)

// e2eLLM answers the main turn with one blocking bash call, then text.
// A fork prompt (it carries interject.Instruction) gets side.
type e2eLLM struct {
	side    string
	release string // file whose existence ends the bash tool
	toolRun chan struct{}
	once    sync.Once

	mu   sync.Mutex
	main []string // bodies of non-fork requests, in order
}

func (m *e2eLLM) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	body := string(b)
	var req struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	_ = json.Unmarshal(b, &req)
	last := req.Messages[len(req.Messages)-1]
	if strings.Contains(body, "[side question") {
		sseText(w, m.side)
		return
	}
	if strings.Contains(body, "Name this session") {
		sseText(w, "e2e-session")
		return
	}
	m.mu.Lock()
	m.main = append(m.main, body)
	m.mu.Unlock()
	if last.Role == "user" && strings.Contains(string(last.Content), "do the long thing") {
		m.once.Do(func() { close(m.toolRun) })
		sseBash(w, "while [ ! -e "+m.release+" ]; do sleep 0.05; done; echo TOOL_OK")
		return
	}
	sseText(w, "MAIN_DONE")
}

func (m *e2eLLM) mainBodies() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.main...)
}

func sseChunk(w http.ResponseWriter, delta map[string]any, finish any) {
	b, _ := json.Marshal(map[string]any{"id": "x", "object": "chat.completion.chunk", "model": "mock",
		"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}})
	fmt.Fprintf(w, "data: %s\n\n", b)
	w.(http.Flusher).Flush()
}

func sseText(w http.ResponseWriter, s string) {
	w.Header().Set("Content-Type", "text/event-stream")
	sseChunk(w, map[string]any{"role": "assistant", "content": s}, nil)
	sseChunk(w, map[string]any{}, "stop")
	fmt.Fprint(w, "data: [DONE]\n\n")
}

func sseBash(w http.ResponseWriter, cmd string) {
	w.Header().Set("Content-Type", "text/event-stream")
	a, _ := json.Marshal(map[string]any{"command": cmd})
	sseChunk(w, map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"index": 0, "id": "call_1", "type": "function",
		"function": map[string]any{"name": "bash", "arguments": string(a)}}}}, nil)
	sseChunk(w, map[string]any{}, "tool_calls")
	fmt.Fprint(w, "data: [DONE]\n\n")
}

type e2eRig struct {
	hh       *harness
	llm      *e2eLLM
	agentDir string
}

func newE2ERig(t *testing.T, side string) *e2eRig {
	t.Helper()
	bin := os.Getenv("FIR_BIN")
	if bin == "" {
		t.Skip("FIR_BIN not set")
	}
	dir := t.TempDir()
	llm := &e2eLLM{side: side, release: filepath.Join(dir, "release"), toolRun: make(chan struct{})}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: llm}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	agentDir := filepath.Join(dir, "agent")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	models := fmt.Sprintf(`{"providers":{"mock":{"baseUrl":"http://%s/v1","apiKey":"k","api":"openai-completions","models":[{"id":"mock-model","name":"M","contextWindow":128000,"maxTokens":4096}]}}}`, ln.Addr())
	if err := os.WriteFile(filepath.Join(agentDir, "models.json"), []byte(models), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	cmd := []string{bin, "--mode", "acp", "--provider", "mock", "--model", "mock-model"}
	env := append(os.Environ(), "FIR_AGENT_DIR="+agentDir)
	cmd = append(cmd, "--acp-session-idle-ttl", "1s")
	if m := os.Getenv("E2E_REAL_MODEL"); m != "" {
		cmd, env = []string{bin, "--mode", "acp", "--model", m, "--acp-session-idle-ttl", "1s"}, nil
	}
	ap, err := client.Start(ctx, client.Config{Command: cmd, Cwd: dir, Env: env})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ap.Close() })
	sess, err := state.New(state.Config{Agent: ap, StateDir: filepath.Join(dir, "state")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	jdir := t.TempDir()
	j, err := journal.Open(filepath.Join(jdir, "journal.json"))
	if err != nil {
		t.Fatal(err)
	}
	hh := &harness{z: newZulip(), j: j, jdir: jdir}
	logf := func(format string, args ...any) {
		hh.logMu.Lock()
		hh.logs = append(hh.logs, fmt.Sprintf(format, args...))
		hh.logMu.Unlock()
		t.Logf(format, args...)
	}
	h, err := New(Config{
		Client:       hh.z,
		Agent:        ap,
		Sessions:     sess,
		Journal:      j,
		BotUserID:    botID,
		BotFullName:  botName,
		Channels:     channels.New(channels.Config{Explicit: map[int64]string{4: "fleet"}}),
		EditInterval: time.Millisecond,
		Interject:    interject.New(ap, interject.Config{Enabled: true, Logf: logf}),
		Logf:         logf,
	})
	if err != nil {
		t.Fatal(err)
	}
	hh.h = h
	return &e2eRig{hh: hh, llm: llm, agentDir: agentDir}
}

// run starts the long main turn, asks the side question while its
// tool runs, then lets the tool finish and waits for the relay to idle.
func (r *e2eRig) run(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	r.hh.h.Handle(ctx, busyMsg(mention("do the long thing")))
	<-r.llm.toolRun
	// Let fir's idle reaper (1s TTL, 1s tick) pass over the running
	// session. This is a wall-clock wait on purpose: the reaper is a
	// real timer inside the fir process, out of the test's reach.
	time.Sleep(3 * time.Second)
	r.hh.h.Handle(ctx, busyMsg("what are you doing?"))
	r.hh.h.ops.Wait() // the side question is done; main still runs
	if err := os.WriteFile(r.llm.release, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	wctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	if err := r.hh.h.WaitIdle(wctx); err != nil {
		t.Fatal(err)
	}
}

// sessionFilesContain reports whether any fir session file holds sub.
func (r *e2eRig) sessionFilesContain(t *testing.T, sub string) bool {
	t.Helper()
	found := false
	_ = filepath.Walk(r.agentDir, func(p string, fi os.FileInfo, err error) error {
		if err == nil && strings.HasSuffix(p, ".jsonl") {
			if b, _ := os.ReadFile(p); strings.Contains(string(b), sub) {
				found = true
			}
		}
		return nil
	})
	return found
}

// assertNoSupersede checks that the main turn was neither cancelled nor
// re-prompted, and that a later prompt carries no digest: the next
// prompt's history holds the side-Q&A header exactly headers times (1
// for the steer itself after inform, 0 after none).
func (r *e2eRig) assertNoSupersede(t *testing.T, headers int) {
	t.Helper()
	if r.hh.logged("superseding") || r.hh.logged("refused") {
		t.Errorf("logs: %q", r.hh.logs)
	}
	if r.sessionFilesContain(t, "Command aborted") {
		t.Error("the main turn's tool was aborted")
	}
	bodies := r.llm.mainBodies()
	if !strings.Contains(bodies[len(bodies)-1], "TOOL_OK") {
		t.Error("main turn did not see the tool finish")
	}
	r.hh.deliver(t, "busy", mention("next"))
	bodies = r.llm.mainBodies()
	if n := strings.Count(bodies[len(bodies)-1], "[while you were working, the user asked and a side branch answered]"); n != headers {
		t.Errorf("side-Q&A headers in the next prompt = %d, want %d", n, headers)
	}
}

func TestE2EInterjectInformSteers(t *testing.T) {
	r := newE2ERig(t, "Busy with the long thing.\nNOTE: user checked in\n2 9")
	r.run(t)
	bodies := r.llm.mainBodies()
	// Requests: the first (tool call), then the one after the tool,
	// which must carry the steer.
	if len(bodies) != 2 || !strings.Contains(bodies[1], "user checked in") {
		t.Fatalf("main requests = %d; steer in last = %v", len(bodies), strings.Contains(bodies[len(bodies)-1], "user checked in"))
	}
	r.assertNoSupersede(t, 1)
}

func TestE2EInterjectNoneRecords(t *testing.T) {
	r := newE2ERig(t, "Busy with the long thing.\n1 9")
	r.run(t)
	bodies := r.llm.mainBodies()
	if len(bodies) != 2 || strings.Contains(bodies[1], "Busy with the long thing") {
		t.Fatalf("main requests = %d, side Q&A leaked into main", len(bodies))
	}
	if !r.sessionFilesContain(t, `"side_query"`) {
		t.Error("no side_query entry in the session file")
	}
	r.assertNoSupersede(t, 0)
}

func TestE2EInterjectAbortCancels(t *testing.T) {
	r := newE2ERig(t, "Stopping.\n3 9")
	ctx := context.Background()
	r.hh.h.Handle(ctx, busyMsg(mention("do the long thing")))
	<-r.llm.toolRun
	r.hh.h.Handle(ctx, busyMsg("stop it"))
	wctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	// The tool never gets released: only the cancel can end the turn.
	if err := r.hh.h.WaitIdle(wctx); err != nil {
		t.Fatal(err)
	}
	if len(r.llm.mainBodies()) != 1 {
		t.Errorf("main went on after abort: %d requests", len(r.llm.mainBodies()))
	}
}

// TestE2EInterjectRealModel runs inform against a real model
// ($E2E_REAL_MODEL); it only logs what happened.
func TestE2EInterjectRealModel(t *testing.T) {
	if os.Getenv("E2E_REAL_MODEL") == "" {
		t.Skip("E2E_REAL_MODEL not set")
	}
	r := newE2ERig(t, "")
	ctx := context.Background()
	r.hh.h.Handle(ctx, busyMsg(mention("Run this exact bash command once: `for i in $(seq 1 40); do echo tick $i; sleep 1; done`. Then reply with one word: finished.")))
	// A real model gives no hook for "the tool runs now": wait.
	time.Sleep(12 * time.Second)
	r.hh.h.Handle(ctx, busyMsg("Please tell your main task: also print the word BANANA at the end. Reply with action 2 and confidence 9."))
	wctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	if err := r.hh.h.WaitIdle(wctx); err != nil {
		t.Fatal(err)
	}
	t.Logf("posts: %q", r.hh.z.stored())
	if r.hh.logged("superseding") || r.hh.logged("failed") {
		t.Errorf("superseded")
	}
}
