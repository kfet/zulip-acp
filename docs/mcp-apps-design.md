# MCP Apps in zulip-acp — Option B (link out)

Status: implemented (`internal/mcpapps`). Date: 2026-10-25.

## Problem

An MCP App is a tool whose definition `_meta.ui.resourceUri` names a
`ui://` HTML resource. A host must render that HTML in a sandboxed iframe
and bridge the app's `postMessage` calls to the MCP server. Zulip cannot
render an iframe or HTML in a message, and it has no client plugin API. So
the relay hosts the app itself, outside Zulip, and posts a link to it.

## Goals

- Show any MCP App that an agent tool opens, on web, desktop and mobile.
- One tap from the Zulip message to the live app.
- The app can call tools of its own server and send messages back to the
  same agent session.
- No fork of Zulip.

## Non-goals

- Inline rendering inside the Zulip message.
- A general-purpose web UI for zulip-acp.
- PNG previews and zform shortcuts (possible later phases, not built).

## Split of work

fir does no UI work. Since v1.42.0 it passes MCP data through to the ACP
client, without interpreting it, as the ACP extension `dev.fir.mcp/v1`
(fir `docs/acp-mcp-passthrough.md`):

- `agentCapabilities._meta["dev.fir.mcp/v1"]` advertises the extension.
- `tool_call` and `tool_call_update` carry `_meta["dev.fir.mcp/v1"]` with
  `server`, `tool` (the original MCP name), `toolMeta` (includes
  `ui.resourceUri` and `ui.visibility`), and on the update `resultMeta` and
  `structuredContent`.
- `resource` and `resource_link` result blocks are forwarded as ACP content
  blocks.
- Two extension methods use the session's own MCP connection:
  `_fir/mcp/resources/read` `{sessionId, server, uri}` and
  `_fir/mcp/tools/call` `{sessionId, server, name, arguments}`. Both return
  the raw MCP result. fir refuses a call unless the matching agent tool is
  in the session's active tool set, so the client never gets more than the
  model has.

zulip-acp does all UI work. acp-kit (v0.37.0) adds `AgentProc.ExtMethod`,
which sends an extension request with the session's wire id, so a call still
reaches the right session after an agent respawn.

## Flow

1. The agent calls an MCP tool whose `toolMeta.ui.resourceUri` is set.
2. `appRouter` (`internal/handler/apps.go`), in the turn's sink chain, gives
   every tool-call update to `mcpapps.Apps.Observe`. On the first update of
   an app tool call, Observe makes an **instance**: a random id, a shell
   token, a content token, the conversation id, the server, the tool and the
   resource URI. Later updates of the same call add the tool input
   (`rawInput`) and the result.
3. The relay posts one line in the topic, out of band and in its own
   goroutine (so an abstaining turn still shows it, and a slow Zulip does not
   stall the agent's update stream): `🧩 [Open app: <tool>](https://apps.example.com/a/<id>?t=<token>)`.
4. The user taps the link. The page is the **host shell**
   (`internal/mcpapps/shell`, embedded in the binary). It moves the token
   out of the address bar into `sessionStorage`, opens a WebSocket to the
   relay and sends `init`.
5. The shell loads the app into an iframe with `sandbox="allow-scripts
   allow-forms"` (no `allow-same-origin`) from `/c/<id>?t=<content token>`.
   The relay reads the HTML on first use through `_fir/mcp/resources/read`
   in the conversation's live session, and keeps it for the life of the
   instance.
6. The shell plays the host side of the MCP Apps postMessage protocol
   (JSON-RPC 2.0):
   - `ui/initialize` → host info, capabilities and host context (theme,
     platform, locale, time zone, viewport);
   - `ui/notifications/initialized` → the shell sends
     `ui/notifications/tool-input` and `ui/notifications/tool-result`; a
     result that arrives later is pushed by the relay and forwarded;
   - `tools/call` → relay → `_fir/mcp/tools/call` on the app's own server;
   - `ui/message` → relay → a new user turn in the topic (below);
   - `ui/open-link` → `window.open` with `noopener,noreferrer`, http(s) only;
   - `ui/notifications/size-changed` → iframe height;
   - `ping` → `{}`; anything else → `-32601`.
7. A `ui/message` is echoed into the topic first, as a quote block from the
   bot (`🧩 **<tool>** sent: …`). Then it starts a turn exactly like a human
   message, prefixed `[app <tool>]`, with the echo as the acknowledged
   message. The relay never answers its own messages, so the echo starts no
   second turn.

## Components

| Component | Where | Notes |
|---|---|---|
| `internal/mcpapps` | relay | instance store, tokens, persistence, HTTP handler, WebSocket bridge. No Zulip imports |
| `internal/mcpapps/shell` | relay | host shell: `shell.html`, `shell.js` (embedded) |
| `internal/handler/apps.go` | relay | `appRouter`, the `mcpapps.Backend` implementation, `ui/message` → turn |
| HTTP listener | relay | loopback only, default `127.0.0.1:8737` |
| Reverse proxy | deployment | serves `public_url` in front of the listener. NOT part of this repo |

### HTTP routes

| Route | Auth | Serves |
|---|---|---|
| `GET /a/{id}?t=` | shell token | the shell page |
| `GET /s/shell.js` | none | the shell script (static) |
| `GET /c/{id}?t=` | content token | the app HTML, with the resource CSP |
| `GET /ws/{id}?t=` | shell token + `Origin` = public URL | the WebSocket bridge |

The bridge speaks `{id, method, params}` → `{id, result}` or
`{id, error:{message}}`, plus a push `{method:"tool-result", params}`.
Methods: `init`, `tools/call`, `ui/message`.

## Configuration

```json
"mcp_apps": {
  "public_url": "https://apps.example.com",
  "content_url": "https://usercontent.example.com",
  "listen": "127.0.0.1:8737",
  "ttl_hours": 24
}
```

- `public_url` is REQUIRED. Without it the feature is off: a link must point
  somewhere a phone can reach. Origin only, no path.
- `content_url` is optional. When set, the app HTML is linked from this
  second origin. Without it the app comes from `public_url`, and the iframe
  sandbox plus the CSP `sandbox` directive still give it an opaque origin.
- `listen` must be a loopback address; the config loader refuses anything
  else.
- `ttl_hours` is the link lifetime (default 24).

Instances are saved in `<state_dir>/mcp-apps.json` (mode 0600; it holds the
tokens), so a link survives a graceful reload or a restart. The app HTML is
not saved; it is read again on first use. A state file that cannot be read
or parsed is moved to `mcp-apps.json.bad` and the relay starts with no
instances: old links stop working, but the relay always starts.

## Security

- **Tokens.** Each instance has two random tokens (`crypto/rand`, about
  130 bits each), bound to its conversation, compared in constant time, and
  valid for `ttl_hours`. The shell token opens the shell and the bridge. The
  content token only reads the app HTML, so the app (which can read its own
  URL) never holds the shell token.
- **Identity.** The conversation of an instance is fixed when the instance
  is made. No request names a conversation.
- **Who gets the link.** Whoever can read the topic. The token is the
  capability. The relay cannot know who holds the browser, so
  `allowed_user_ids` does NOT apply to an app: anyone with the link can call
  the app's tools and start turns in the topic. This is the trade-off of
  link-out hosting. Enable `mcp_apps` only where everyone who can read the
  served topics may drive the agent, and keep the proxy's access log private
  (the link carries the token until the shell removes it from the address
  bar).
- **Origin isolation.** The iframe has no `allow-same-origin`, and the app
  response carries the CSP `sandbox allow-scripts allow-forms` directive, so
  the app runs in an opaque origin even if it is opened directly. It cannot
  read the shell's storage. Its WebSocket attempts send `Origin: null`. The
  bridge accepts only an `Origin` equal to `public_url`, scheme included.
- **CSP.** The app response's policy is default-deny. It allows inline
  script and style (an app is one HTML file), `data:`/`blob:` images and
  media, and only the domains in the resource's `_meta.ui.csp`
  (`connectDomains`, `resourceDomains`, `frameDomains`, `baseUriDomains`).
  A declared domain must look like `https://host[:port]` or
  `wss://host[:port]` (one leading `*.` allowed); anything else is dropped,
  so a resource cannot inject directives. `frame-ancestors` is the public
  origin. The shell's own policy allows only its script, its WebSocket and
  the content origin as a frame.
- **Tool calls.** An app may call only tools on its own server. A tool that
  the relay has seen declared with a `ui.visibility` that lacks `"app"` is
  refused. A tool the relay has not seen has the MCP Apps default visibility
  (`["model","app"]`); fir still refuses any tool that is not in the
  session's active tool set.
- **Load.** One bridge runs at most 8 requests at once; a request past that
  is refused at once. Each agent request has a 60 s limit.
- **ui/message.** At most 5 per minute per instance, at most 8000 code
  points each, text blocks only. Each is echoed into the topic, so nothing
  reaches the agent unseen. The relay's gates are applied again: the channel
  must still be served, DMs still on, and the conversation still in the
  journal. Like a typed message, it supersedes a turn in flight.
- **No new MCP surface.** The listener speaks HTTP to browsers. It is not an
  MCP server, and it gives nothing that the conversation's own agent session
  does not already have.

## Lifetime

- An instance lives until its TTL ends. Expired instances are dropped when a
  new instance is made and when the state is loaded.
- An app never creates an agent session. Its requests go to the
  conversation's LIVE session. After `!new`, or after idle GC reaps the
  session, the app works again once the next turn has made a new session;
  until then `tools/call` and the HTML fetch fail with a message that says
  so. `ui/message` always works, because it starts a turn.
- The listener stops accepting at shutdown and before a re-exec. Open
  bridges end when the process exits or execs; the shell then shows
  "disconnected" and reconnects when the page is reloaded.

## fir 1.42.0 `rawOutput`

fir 1.42.0 changed the live `tool_call_update.rawOutput` to the
`session/load` replay shape. zulip-acp and acp-kit do not read `rawOutput`
anywhere (checked with `grep -rn rawOutput` over both trees), and
`mcpapps` builds the app's `CallToolResult` from the ACP `content` blocks
plus `structuredContent` and `resultMeta` from `_meta["dev.fir.mcp/v1"]`.
`TestFakeACPAgentEndToEnd` sends a `rawOutput` in the replay shape and
proves that the result does not depend on it.

## Deployment note (not built here)

Public exposure is a deployment step, outside this repository. Put a reverse
proxy with TLS in front of the loopback listener for `public_url` (and for
`content_url`, if used, pointing at the same listener). Follow the fleet's
public-exposure rules for which host and proxy may do this. The proxy must
pass WebSocket upgrades on `/ws/` and keep the `Host` header, so the
bridge's `Origin` check matches. The fleet's `dist.lock` must pin fir 1.42.0
or later: an older fir sends no `dev.fir.mcp/v1` data, and then no link is
ever posted.

## Tests

- `internal/mcpapps/mcpapps_test.go`: config checks, `_meta` parsing,
  instance store, expiry, tokens, visibility, rate limit, CSP filtering,
  HTTP routes and headers, the WebSocket bridge (origin check, every method,
  errors, the result push), persistence.
- `internal/mcpapps/e2e_test.go`: a fake ACP agent (this test binary,
  re-executed) emits a `ui://` tool call over real stdio and serves
  `_fir/mcp/resources/read` and `_fir/mcp/tools/call`; the test drives it
  through acp-kit's client, the store, the HTTP handler and the bridge.
- `internal/mcpapps/shell/shell_test.js`: the shell under Node.js with fake
  browser APIs (run by `TestShellJSBehaviour` when Node.js is installed).
- `internal/handler/apps_test.go`: the link post in a turn, the backend's
  extension calls, and `ui/message` → echo + turn, with its gates.

## Open questions

1. PNG preview: worth a headless Chromium dependency on the host?
2. zform shortcuts (Option C) for simple app actions.
