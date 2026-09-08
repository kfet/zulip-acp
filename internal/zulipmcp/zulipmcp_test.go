package zulipmcp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestHostConfigIsSelfConsistent: the redirector runs as a second copy
// of this binary and must agree with the Host on the subcommand and the
// env var names, or every MCP session would fail at the preamble.
func TestHostConfigIsSelfConsistent(t *testing.T) {
	h, r := HostConfig("/var/lib/zulip-acp"), RedirConfig()
	if h.RedirSubcommand != r.Subcommand {
		t.Fatalf("subcommand mismatch: %q vs %q", h.RedirSubcommand, r.Subcommand)
	}
	if h.EnvSocket != r.EnvSocket || h.EnvToken != r.EnvToken {
		t.Fatalf("env mismatch: %+v vs %+v", h, r)
	}
	if h.ServerName != ServerName || h.ServerInfoName != ServerInfoName {
		t.Fatalf("identity drifted: %+v", h)
	}
	if h.SocketName != SocketName {
		t.Fatalf("socket naming drifted: %+v", h)
	}
	if h.ServerInfoVersion == "" {
		t.Fatal("serverInfo.version must be set")
	}
}

// TestHostConfigSocketIsStableUnderStateDir is the regression guard for
// the live bug: a socket under a fresh MkdirTemp moved on every process
// start, so a reload (which re-execs in place while the agent's
// redirector keeps running) left every live session dialling a path
// nothing would ever bind again. The path must be a pure function of
// the state dir, and must NOT be under $TMPDIR.
func TestHostConfigSocketIsStableUnderStateDir(t *testing.T) {
	a, b := HostConfig("/var/lib/zulip-acp"), HostConfig("/var/lib/zulip-acp")
	if a.Dir != b.Dir || a.Dir != filepath.Join("/var/lib/zulip-acp", DirName) {
		t.Fatalf("socket dir is not a stable function of the state dir: %q vs %q", a.Dir, b.Dir)
	}
	if a.DirPrefix != "" || a.BaseDir != "" {
		t.Fatalf("Dir must be the only thing choosing the path: %+v", a)
	}
	if strings.HasPrefix(a.Dir, os.TempDir()) {
		t.Fatalf("socket dir %q is under $TMPDIR; it must outlive a reload", a.Dir)
	}
}

// TestServerIsNamedRelayNotZulip pins the naming decision: the server
// exposes the RELAY's own interface, not Zulip's API.
func TestServerIsNamedRelayNotZulip(t *testing.T) {
	if ServerName != "relay" {
		t.Fatalf("ServerName = %q; calling it anything Zulip-shaped invites the wrong expectation", ServerName)
	}
}
