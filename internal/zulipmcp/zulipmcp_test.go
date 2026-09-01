package zulipmcp

import "testing"

// TestHostConfigIsSelfConsistent: the redirector runs as a second copy
// of this binary and must agree with the Host on the subcommand and the
// env var names, or every MCP session would fail at the preamble.
func TestHostConfigIsSelfConsistent(t *testing.T) {
	h, r := HostConfig(), RedirConfig()
	if h.RedirSubcommand != r.Subcommand {
		t.Fatalf("subcommand mismatch: %q vs %q", h.RedirSubcommand, r.Subcommand)
	}
	if h.EnvSocket != r.EnvSocket || h.EnvToken != r.EnvToken {
		t.Fatalf("env mismatch: %+v vs %+v", h, r)
	}
	if h.ServerName != ServerName || h.ServerInfoName != ServerInfoName {
		t.Fatalf("identity drifted: %+v", h)
	}
	if h.DirPrefix != DirPrefix || h.SocketName != SocketName {
		t.Fatalf("socket naming drifted: %+v", h)
	}
	if h.ServerInfoVersion == "" {
		t.Fatal("serverInfo.version must be set")
	}
}

// TestServerIsNamedRelayNotZulip pins the naming decision: the server
// exposes the RELAY's own interface, not Zulip's API.
func TestServerIsNamedRelayNotZulip(t *testing.T) {
	if ServerName != "relay" {
		t.Fatalf("ServerName = %q; calling it anything Zulip-shaped invites the wrong expectation", ServerName)
	}
}
