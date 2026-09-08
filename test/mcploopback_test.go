package live

// End-to-end proof that the agent keeps its loopback MCP tools across a
// graceful reload. Unlike its neighbours in this package it needs NO
// Zulip server and no credentials: everything it asserts is about our
// own processes and our own unix socket, so it runs on every `go test`.
//
// # The bug it pins
//
// A reload re-execs the relay in place (internal/reload). The ACP agent
// is killed and restarted, but the MCP REDIRECTOR is a child of the
// agent's own process tree in production and, either way, the token it
// was handed at session/new outlives the relay's process image. Before
// the fix the successor image
//
//   - bound a DIFFERENT socket path (mcphost's MkdirTemp),
//   - had had that path deleted from under it by Close, and
//   - minted a fresh token registry that rejected the token the
//     redirector still held,
//
// so every mcp__relay__* call returned "tool not found" for the rest of
// the session — silently, after the agent had already promised to use
// one.
//
// # Shape
//
// Three real processes and one real execve:
//
//	parent    the `go test` process: drives a real redirector over
//	          pipes, exactly as the agent drives it over stdio, and
//	          asserts the tool calls.
//	relay g1  a child holding a real mcphost.Host on the state-dir
//	          socket, with the REAL zulipmcp tool set on it. On the
//	          signal it exports its token registry, closes WITHOUT
//	          unlinking, and syscall.Exec's itself.
//	relay g2  the exec'd image: seeds the registry from the env and
//	          binds the same path.
//
// The parent's second `history` call must succeed, must be served by
// generation 2, and must resolve to the SAME session key — which is
// only possible if the seeded registry accepted the redirector's
// pre-exec token.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/kfet/acp-kit/mcphost"
	"github.com/kfet/zulip-acp/internal/journal"
	"github.com/kfet/zulip-acp/internal/reload"
	"github.com/kfet/zulip-acp/internal/zulipmcp"
	"github.com/kfet/zulip-acp/internal/zulipproto"
)

// Child-process selectors and the fixed identities the three processes
// have to agree on.
const (
	envLoopbackRole = "ZULIP_ACP_TEST_LOOPBACK_ROLE"
	envStateDir     = "ZULIP_ACP_TEST_STATE_DIR"

	// convID is the session key the host binds the token to — in
	// production the conv-id, the basename of the session cwd.
	convID = "conv-e2e"
	// streamID / topic are what ConvKey resolves that session key to.
	streamID = 4242
	topic    = "loopback-survives-exec"

	// Control-pipe fds handed to the relay child. They survive execve,
	// which is what lets generation 2 report ready on the same pipe
	// generation 1 was listening on.
	fdControlIn  = 3 // parent → relay: "exec"
	fdControlOut = 4 // relay → parent: "ready <socket> <token>" / "ready2"
)

// TestMain dispatches the child roles before any test output exists:
// the redirector proxies raw JSON-RPC on stdout, so nothing else may
// ever write there.
func TestMain(m *testing.M) {
	switch os.Getenv(envLoopbackRole) {
	case "redir":
		if err := mcphost.RunRedir(zulipmcp.RedirConfig()); err != nil {
			fmt.Fprintln(os.Stderr, "redir:", err)
			os.Exit(1)
		}
		os.Exit(0)
	case "relay":
		runRelayChild()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// --- the test ------------------------------------------------------------

func TestLoopbackToolsSurviveAReload(t *testing.T) {
	stateDir := t.TempDir()

	relay, control := startRelay(t, stateDir)
	socket, token := relay.awaitReady(t, "ready")

	// The agent side: a real redirector process, spoken to over pipes
	// the way the agent speaks to it over stdio.
	rd := startRedir(t, socket, token)

	rd.request(t, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`)
	rd.send(t, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	if got := rd.history(t, 2); !strings.Contains(got, "gen1 "+convID) {
		t.Fatalf("pre-reload history = %q", got)
	}

	// --- the reload -------------------------------------------------
	if _, err := control.Write([]byte("exec\n")); err != nil {
		t.Fatalf("signal the reload: %v", err)
	}
	relay.awaitReady(t, "ready2")

	// --- the next turn ----------------------------------------------
	// Same redirector process, same token, a process image that has
	// been replaced in between. This is exactly what broke live.
	got := rd.history(t, 3)
	if !strings.Contains(got, "gen2 "+convID) {
		t.Fatalf("post-reload history = %q; the successor did not serve this call for the pre-exec session key", got)
	}

	// The whole surface came back, not just the one method.
	var m map[string]any
	if err := json.Unmarshal([]byte(rd.request(t, `{"jsonrpc":"2.0","id":4,"method":"tools/list"}`)), &m); err != nil {
		t.Fatalf("decode tools/list: %v", err)
	}
	res, ok := m["result"].(map[string]any)
	if !ok {
		t.Fatalf("tools/list after the reload: %v", m)
	}
	names := map[string]bool{}
	for _, x := range res["tools"].([]any) {
		names[x.(map[string]any)["name"].(string)] = true
	}
	if !names[zulipmcp.ToolHistory] {
		t.Fatalf("tools/list after the reload = %v, want %s among them", names, zulipmcp.ToolHistory)
	}
}

// TestSocketPathIsInsideTheStateDir pins the other half of the fix at
// the level a human debugs at: the socket the agent was told about is
// under the state dir, not under $TMPDIR, so it is still there — and
// still the same path — after an upgrade.
func TestSocketPathIsInsideTheStateDir(t *testing.T) {
	stateDir := t.TempDir()
	relay, control := startRelay(t, stateDir)
	socket, _ := relay.awaitReady(t, "ready")
	want := filepath.Join(stateDir, zulipmcp.DirName, zulipmcp.SocketName)
	if socket != want {
		t.Fatalf("socket = %q, want %q", socket, want)
	}
	if _, err := control.Write([]byte("exec\n")); err != nil {
		t.Fatalf("signal the reload: %v", err)
	}
	relay.awaitReady(t, "ready2")
	// The successor bound the same path, and the socket file survived
	// the predecessor's close.
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("socket gone after the reload: %v", err)
	}
}

// --- the relay child -----------------------------------------------------

// relayProc is the parent's handle on the relay child.
type relayProc struct {
	cmd    *exec.Cmd
	events *bufio.Reader
}

// startRelay spawns generation 1 and returns it plus the write end of
// the control pipe. Both pipes are plain fds, so they survive the
// child's execve and generation 2 answers on the same one.
func startRelay(t *testing.T, stateDir string) (*relayProc, *os.File) {
	t.Helper()
	ctlR, ctlW, err := os.Pipe()
	if err != nil {
		t.Fatalf("control pipe: %v", err)
	}
	evR, evW, err := os.Pipe()
	if err != nil {
		t.Fatalf("event pipe: %v", err)
	}

	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), envLoopbackRole+"=relay", envStateDir+"="+stateDir)
	cmd.ExtraFiles = []*os.File{ctlR, evW} // fd 3, fd 4
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start relay child: %v", err)
	}
	// The child owns its ends now; holding them here would keep the
	// event pipe from ever reporting EOF.
	_ = ctlR.Close()
	_ = evW.Close()

	p := &relayProc{cmd: cmd, events: bufio.NewReader(evR)}
	t.Cleanup(func() {
		_ = ctlW.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		_ = evR.Close()
	})
	return p, ctlW
}

// awaitReady blocks for the child's next control line and asserts its
// first field. Returns the remaining fields.
func (p *relayProc) awaitReady(t *testing.T, want string) (string, string) {
	t.Helper()
	type result struct {
		line string
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		l, err := p.events.ReadString('\n')
		ch <- result{l, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatalf("relay child died waiting for %q: %v", want, r.err)
		}
		f := strings.Split(strings.TrimSpace(r.line), "\t")
		if len(f) == 0 || f[0] != want {
			t.Fatalf("relay child said %q, want %q", strings.TrimSpace(r.line), want)
		}
		f = append(f, "", "")
		return f[1], f[2]
	case <-time.After(30 * time.Second):
		t.Fatalf("timed out waiting for the relay child to say %q", want)
		return "", ""
	}
}

// runRelayChild is both generations of the relay: the code path is
// identical, which is the point — a successor image is just this
// program starting again, with a token registry in its environment.
func runRelayChild() {
	stateDir := os.Getenv(envStateDir)
	gen := "gen1"
	seed := os.Getenv(reload.EnvMCPTokens)
	if seed != "" {
		gen = "gen2"
	}

	host, err := mcphost.New(zulipmcp.HostConfig(stateDir))
	if err != nil {
		fatalChild("mcphost.New: %v", err)
	}
	tools, err := zulipmcp.NewTools(zulipmcp.Config{
		Client:  &stubMessages{gen: gen},
		ConvKey: func(k string) (journal.Key, bool) { return journal.Channel(streamID, topic), k == convID },
		Origin:  func(string) (journal.Parent, bool) { return journal.Parent{}, false },
		Rename:  func(journal.Key, string) (string, error) { return "", nil },
		Timeout: 10 * time.Second,
		Logf:    func(string, ...any) {},
	})
	if err != nil {
		fatalChild("zulipmcp.NewTools: %v", err)
	}
	tools.Register(host)
	// Ordering is the contract: New → Tool×N → SeedTokens → Listen.
	if err := host.SeedTokens(seed); err != nil {
		fatalChild("SeedTokens: %v", err)
	}
	if err := host.Listen(); err != nil {
		fatalChild("Listen: %v", err)
	}

	control := os.NewFile(fdControlIn, "control")
	events := os.NewFile(fdControlOut, "events")

	if gen == "gen2" {
		// The successor announces itself and then simply serves until
		// the parent is done with it.
		mustWrite(events, "ready2\n")
		_, _ = io.Copy(io.Discard, control) // blocks until the parent closes the pipe
		_ = host.Close()
		return
	}

	// Generation 1: hand the agent a session token, the way
	// client.MCPServersForSession does at session/new.
	cfg := host.ServerConfigForSession(convID)
	token := ""
	for _, e := range cfg[0].Stdio.Env {
		if e.Name == zulipmcp.EnvToken {
			token = e.Value
		}
	}
	mustWrite(events, fmt.Sprintf("ready\t%s\t%s\n", host.SocketPath(), token))

	// Wait for the reload signal, then do exactly what main does.
	if _, err := bufio.NewReader(control).ReadString('\n'); err != nil {
		fatalChild("control read: %v", err)
	}
	blob := host.ExportTokens()
	if err := host.CloseForExec(); err != nil {
		fatalChild("CloseForExec: %v", err)
	}
	env := reload.Environ(os.Environ(), reload.Cursor{}, blob)
	if err := syscall.Exec(os.Args[0], os.Args, env); err != nil {
		fatalChild("exec: %v", err)
	}
}

func fatalChild(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "relay child: "+format+"\n", args...)
	os.Exit(1)
}

func mustWrite(f *os.File, s string) {
	if _, err := f.WriteString(s); err != nil {
		fatalChild("write control: %v", err)
	}
}

// stubMessages stands in for the Zulip client. Its reply names the
// generation that served it and the session key the host resolved, so
// the parent can tell WHICH process answered and for WHOM.
type stubMessages struct{ gen string }

func (s *stubMessages) Messages(_ context.Context, narrow []zulipproto.NarrowTerm, _ int, _ int64) ([]zulipproto.Message, error) {
	if len(narrow) == 0 {
		return nil, fmt.Errorf("empty narrow")
	}
	return []zulipproto.Message{{
		ID:         7,
		SenderName: "probe",
		Content:    s.gen + " " + convID,
		Timestamp:  1700000000,
	}}, nil
}

// --- the redirector harness ----------------------------------------------

// redirProc drives a real redirector subprocess over pipes, standing in
// for the agent's stdio.
type redirProc struct {
	in  io.WriteCloser
	out *bufio.Reader
}

func startRedir(t *testing.T, socket, token string) *redirProc {
	t.Helper()
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(),
		envLoopbackRole+"=redir",
		zulipmcp.EnvSocket+"="+socket,
		zulipmcp.EnvToken+"="+token)
	cmd.Stderr = os.Stderr
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("redir stdin: %v", err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("redir stdout: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start redir: %v", err)
	}
	t.Cleanup(func() {
		_ = in.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return &redirProc{in: in, out: bufio.NewReader(out)}
}

func (r *redirProc) send(t *testing.T, line string) {
	t.Helper()
	if _, err := r.in.Write([]byte(line + "\n")); err != nil {
		t.Fatalf("write to redir: %v", err)
	}
}

func (r *redirProc) request(t *testing.T, line string) string {
	t.Helper()
	r.send(t, line)
	type result struct {
		line string
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		l, err := r.out.ReadString('\n')
		ch <- result{l, err}
	}()
	select {
	case res := <-ch:
		if res.err != nil {
			t.Fatalf("read from redir: %v", res.err)
		}
		return strings.TrimSpace(res.line)
	case <-time.After(60 * time.Second):
		t.Fatal("timed out waiting for a response through the redirector")
		return ""
	}
}

// history calls the real `history` tool and returns its text, failing
// the test on a JSON-RPC error or an is-error result — which is what
// "the agent lost its tools" looks like on the wire.
func (r *redirProc) history(t *testing.T, id int) string {
	t.Helper()
	req := `{"jsonrpc":"2.0","id":` + strconv.Itoa(id) +
		`,"method":"tools/call","params":{"name":"` + zulipmcp.ToolHistory + `","arguments":{"limit":5}}}`
	var m map[string]any
	if err := json.Unmarshal([]byte(r.request(t, req)), &m); err != nil {
		t.Fatalf("decode tools/call: %v", err)
	}
	if e, ok := m["error"]; ok {
		t.Fatalf("tools/call returned an error: %v", e)
	}
	res, ok := m["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result in %v", m)
	}
	if isErr, _ := res["isError"].(bool); isErr {
		t.Fatalf("tool reported an error: %v", res)
	}
	var sb strings.Builder
	for _, c := range res["content"].([]any) {
		sb.WriteString(c.(map[string]any)["text"].(string))
	}
	return sb.String()
}
