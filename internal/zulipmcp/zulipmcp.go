// Package zulipmcp wires zulip-acp's self-hosted MCP server onto the
// generic acp-kit/mcphost Host. It owns everything Zulip-specific about
// the loopback: the `relay` server identity, the env var names, the
// redirector subcommand and the socket naming.
//
// It owns NO tools. The tool set is acp-kit/relaytool's, because every
// one of them is a relay-generic control that poe-acp and slack-acp
// need identically — status, model, post, schedule. If a tool ever
// needs to know something only Zulip knows (a topic, a stream id, a
// widget), THAT is the tool that belongs here, and the fact that this
// package exists at all is what keeps the option open.
//
// The conversation a tool call acts on is resolved server-side by
// mcphost from the connection token; nothing here or in relaytool ever
// takes a conversation as an argument.
package zulipmcp

import "github.com/kfet/acp-kit/mcphost"

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
	DirPrefix      = "zulip-acp-mcp-"
)

// HostConfig returns the mcphost.Config for the relay's MCP server.
func HostConfig() mcphost.Config {
	return mcphost.Config{
		DirPrefix:         DirPrefix,
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
