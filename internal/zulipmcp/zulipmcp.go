// Package zulipmcp wires zulip-acp's self-hosted MCP server onto the
// generic acp-kit/mcphost Host. It owns everything Zulip-specific about
// the loopback: the `relay` server identity, the env var names, the
// redirector subcommand and the socket naming.
//
// It owns exactly ONE tool, and only because that tool cannot live
// anywhere else: `history`, which reads back the conversation's own
// earlier messages. Everything relay-generic — status, model, post,
// schedule — is acp-kit/relaytool's, because poe-acp and slack-acp
// need those identically. The dividing line the package doc always
// stated still holds: if a tool needs to know something only Zulip
// knows (a topic, a stream id, a DM narrow, a widget), THAT is the
// tool that belongs here. See history.go.
//
// The conversation a tool call acts on is resolved server-side by
// mcphost from the connection token; nothing here or in relaytool ever
// takes a conversation as an argument.
package zulipmcp

import (
	"path/filepath"

	"github.com/kfet/acp-kit/mcphost"
)

// Env var names the main process sets on the spawned redirector (via
// the ACP McpServerStdio.Env), so no secret lands on a command line.
const (
	EnvToken  = "ZULIPACP_MCP_TOKEN"
	EnvSocket = "ZULIPACP_MCP_SOCKET"
)

// Redirector subcommand and the server identity advertised to the
// agent. The server is named `relay` rather than `zulip`: what it
// exposes is the RELAY's own interface, not Zulip's API, and calling it
// `zulip` would invite exactly the wrong expectation.
const (
	Subcommand     = "mcp-serve"
	ServerName     = "relay"
	ServerInfoName = "zulip-acp"
	SocketName     = "mcp.sock"
	// DirName is the StateDir subdirectory holding the socket.
	DirName = "mcp"
)

// HostConfig returns the mcphost.Config for the relay's MCP server,
// with the socket under stateDir.
//
// The path must be STABLE across a graceful reload. A reload re-execs
// this process in place while the agent's redirector subprocess keeps
// running and keeps holding the socket path it was told about at
// session/new; a fresh MkdirTemp per process start therefore left every
// live session pointing at a socket nothing would ever bind again, and
// the agent silently lost every mcp__relay__* tool for the rest of the
// session. StateDir is the one directory whose lifetime already matches
// the relay's, so the socket lives there. mcphost never removes a
// caller-supplied Dir — see mcphost.Config.Dir.
func HostConfig(stateDir string) mcphost.Config {
	return mcphost.Config{
		Dir:               filepath.Join(stateDir, DirName),
		SocketName:        SocketName,
		RedirSubcommand:   Subcommand,
		ServerName:        ServerName,
		ServerInfoName:    ServerInfoName,
		ServerInfoVersion: "1",
		EnvSocket:         EnvSocket,
		EnvToken:          EnvToken,
	}
}

// RedirConfig returns the mcphost.RedirConfig for the redirector
// subcommand interception in main.
func RedirConfig() mcphost.RedirConfig {
	return mcphost.RedirConfig{
		Subcommand: Subcommand,
		EnvSocket:  EnvSocket,
		EnvToken:   EnvToken,
	}
}
