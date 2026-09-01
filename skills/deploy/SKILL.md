---
name: deploy
description: Deploy zulip-acp to a host as a systemd/launchd service that relays a Zulip realm's channels to spawned ACP agents. Covers the canonical file layout, the dial-out (no-funnel) model, secrets, and end-to-end verification.
---

# Deploy Skill

Deploy `zulip-acp` to a host where it relays one **Zulip realm** to ACP agents.
Unlike `poe-acp`, the relay **dials out**: it long-polls the Zulip
`GET /api/v1/events` queue over HTTPS and posts replies back with the bot's API
key. **There is no inbound HTTP listener, no port to open, and no Tailscale
Funnel** — the host needs only outbound reachability to the Zulip site. That is
the whole reason a tailnet-only Zulip works as a surface.

**Agent process model.** The relay spawns **one long-lived ACP agent process**
(`fir --mode acp`, ...) shared by every conversation. A Zulip **topic** maps to
an ACP **session** and to its own cwd — *not* its own process. The topic string
*is* the session id (no thread-id mapping table; topics can even be renamed).
Consequence: replacing the agent binary on disk changes nothing for running
conversations — you must restart the service to pick it up.

## Canonical file layout (do not deviate — this is the whole point of this skill)

```
~/.local/bin/zulip-acp                        # binary, on PATH, from `make deploy` or brew
~/.config/zulip-acp/config.json               # site, bot_email, channels, agent_cmd, state_dir
~/.config/zulip-acp/env                        # ZULIP_API_KEY=...   (mode 0600)
~/.config/zulip-acp/state/                     # per-conversation state + journal.json
~/.config/systemd/user/zulip-acp.service       # supervisor unit (Linux)
```

Logs go to journald (`journalctl --user -u zulip-acp`), **not** to hand-rotated
`relay.log*` files. Never run the relay as a bare `./zulip-acp` from a checkout
or a home-root folder: the binary belongs on PATH, config under `~/.config`,
state under the config dir, supervision to systemd.

Multi-bot on one host: give each its own `~/.config/zulip-acp/bot-<name>/`
(config.json + env + state/) and a `zulip-acp-<name>.service` unit, exactly like
poe-acp's multi-bot layout.

## Confirm with the user before acting

1. **Host** — ssh target (`user@host`), or `local`.
2. **Zulip site** — realm base URL, e.g. `https://zulip.example.ts.net`.
3. **Bot** — a Zulip **generic bot**: its `bot_email` and API key. Create under
   Zulip → Settings → Personal/Org → Bots. The key lands in `ZULIP_API_KEY`.
4. **Channels** — comma-separated channel **names** the bot serves. The bot must
   be *subscribed* to each. An empty list is a deliberate fatal error (a relay
   that answers in every channel of a realm is a footgun).
5. **ACP agent command** — default `fir --mode acp`.

## Steps

### 1. Ship the binary

From the repo, cross-build + arch-detect + scp to `~/.local/bin/zulip-acp`:

```bash
make deploy HOST=<host>
```

Or, once the repo is public and the tap carries it:

```bash
ssh <host> 'brew install kfet/ai/zulip-acp'
```

> **Private repo caveat:** while `kfet/zulip-acp` is private, `brew install`
> 404s on the release asset (the tap is public but the asset is not). Use
> `make deploy`, or `gh release download <tag> --repo kfet/zulip-acp`, until the
> repo is made public. Recorded in the repo BACKLOG.

### 2. Confirm the ACP agent is on the host PATH

```bash
ssh <host> 'command -v fir && fir --version'
```

### 3. Install config + secret

```bash
ssh <host> 'mkdir -p ~/.config/zulip-acp/state'
```

Write `~/.config/zulip-acp/config.json`:

```json
{
  "site": "https://zulip.example.ts.net",
  "bot_email": "fir-relay-bot@zulip.example.ts.net",
  "channels": ["fleet"],
  "agent_cmd": ["fir", "--mode", "acp"],
  "state_dir": "/home/<you>/.config/zulip-acp/state",
  "hide_thinking": false
}
```

`"channels": ["*"]` instead of a list serves every channel the bot is
subscribed to, and follows subscription changes at runtime — adding the bot to
a channel starts serving it with no config edit and no restart. An empty list
is still a fatal error.

Write `~/.config/zulip-acp/env` (mode **0600**):

```
ZULIP_API_KEY=<bot-api-key>
```

```bash
ssh <host> 'chmod 600 ~/.config/zulip-acp/env'
```

Sanity-check what the binary will actually resolve:

```bash
ssh <host> '~/.local/bin/zulip-acp --config ~/.config/zulip-acp/config.json --print-paths'
```

### 4. Install the service (Linux: systemd user unit)

`zulip-acp` is a plain long-poll client — **no sd_notify, no master/worker
supervisor, no graceful worker swap** (that shim is poe-acp-only). It is a
`Type=simple` service; restart is a hard restart. An in-flight turn is dropped
on restart but the conversation state is on disk and Zulip re-delivers, so the
next event redrives it. Keep restarts for quiet moments where you can.

Write `~/.config/systemd/user/zulip-acp.service`:

```ini
[Unit]
Description=zulip-acp (fir fleet relay)
After=network-online.target
Wants=network-online.target

[Service]
# systemd user units do NOT inherit your login shell PATH. The ACP agent
# (fir) lives in ~/.local/bin, so it MUST be set explicitly, or the relay
# starts, authenticates, resolves channels - and then dies with
#   agent start: start agent: exec: "fir": executable file not found in $PATH
# looping on Restart=on-failure. Hit for real on the first deployment.
Environment=PATH=%h/.local/bin:/usr/local/bin:/usr/bin:/bin
EnvironmentFile=%h/.config/zulip-acp/env
ExecStart=%h/.local/bin/zulip-acp --config %h/.config/zulip-acp/config.json
Restart=on-failure
RestartSec=2s

[Install]
WantedBy=default.target
```

Enable + start + survive logout:

```bash
ssh <host> 'systemctl --user daemon-reload && \
  systemctl --user enable --now zulip-acp && \
  loginctl enable-linger $USER'
```

macOS: a launchd user agent, same shape as poe-acp's `deploy` skill — wrap in
`sh -c 'set -a; . ~/.config/zulip-acp/env; set +a; exec /opt/homebrew/bin/zulip-acp --config ~/.config/zulip-acp/config.json'`,
set `PATH` to include the agent binary dir, `RunAtLoad`+`KeepAlive` true.

### 5. Verify end-to-end (do NOT trust the process being up)

```bash
ssh <host> 'systemctl --user is-active zulip-acp'          # active
ssh <host> 'journalctl --user -u zulip-acp -n 30 --no-pager'  # event queue registered, channels resolved
```

### The gating rule you MUST know before testing

`handler.handleMessage` gates every message:

```go
mentioned := h.mentioned(text)
existing, engaged := h.cfg.Journal.Lookup(m.StreamID, m.Topic)
if !mentioned && !engaged { return }   // silent: no log line, no reply
```

- In a **new** topic the bot must be **@-mentioned** (`@**<bot-full-name>**`)
  to be summoned.
- In a topic it is **already engaged in** (present in `state/journal.json`) it
  answers plain messages with no mention.
- A non-mention in a fresh topic is dropped **silently** - no journal line at
  all. Do not read that as a broken relay: it is the designed behaviour, and it
  is the single easiest way to waste an hour "debugging" a healthy deploy.

Two probes give full coverage - one proves the relay works, the other proves
migrated state was actually read:

```bash
Z=<site>; A="<your-email>:<your-api-key>"; C=fleet; BOT="fir-relay"
T="deploy smoke $(date +%s)"

# A) NEW topic -> MUST @-mention, else it is silently ignored
curl -sS -u "$A" "$Z/api/v1/messages" -d type=stream -d "to=$C" \
  --data-urlencode "topic=$T" \
  --data-urlencode "content=@**$BOT** run this and reply with ONLY the raw output: echo OK-\$(hostname)" >/dev/null

# B) EXISTING engaged topic -> no mention needed; proves journal/state was read
curl -sS -u "$A" "$Z/api/v1/messages" -d type=stream -d "to=$C" \
  --data-urlencode "topic=<a topic already in state/journal.json>" \
  --data-urlencode "content=ping" >/dev/null

sleep 30
# NOTE: -G is required, else curl POSTs the params and the read returns no messages
curl -sS -u "$A" -G "$Z/api/v1/messages" --data-urlencode anchor=newest \
  -d num_before=5 -d num_after=0 -d apply_markdown=false \
  --data-urlencode "narrow=[{\"operator\":\"channel\",\"operand\":\"$C\"},{\"operator\":\"topic\",\"operand\":\"$T\"}]" \
  | python3 -c 'import sys,json;[print(m["sender_full_name"],"|",m["content"][:80]) for m in json.load(sys.stdin)["messages"]]'
```

Expect a real reply to (A) containing the executed output, and a reply to (B)
with no mention. `journalctl` should show
`handler: new conversation <id> in #<channel> > "<topic>"` for (A). Note Zulip
anonymises sender emails (`user8@...`) when email visibility is restricted -
match on `sender_full_name`, not the address.

### 6. Retire any old deployment

If a previous bare-process deploy exists (e.g. `~/zulip-acp/` with a committed
binary and `relay.log*`), migrate its `state/` into `~/.config/zulip-acp/state`
**before** first start (preserves conversation continuity), then remove the old
dir once the systemd service is verified live.

## Realm one-time settings (bite once, documented forever)

- **Message length**: Zulip truncates content past **10,000 chars silently**
  (returns success, appends `[message truncated]`). The relay rolls over across
  multiple messages; nothing to configure, but know the ceiling.
- **Edit window**: a fresh realm defaults `Settings → Organization → Message
  editing` to a finite window (e.g. 600s). The relay streams by *editing* one
  message, so a turn longer than the window starts failing edits with 400. Set
  **"Allow message editing" with no time limit** (or a very large one) for the
  bot's realm.
- **Push**: self-hosted Zulip cannot push to iOS directly; it registers with the
  Zulip bouncer, which sees metadata. This is a policy decision — the relay takes
  no position. Leave push off unless the user opts in.

## Pitfalls

- **Bare-process deploy** — the classic mistake: `./zulip-acp --config config.json`
  from `~/zulip-acp/`. No supervision (dies on reboot), binary committed in home,
  hand-rotated logs, state buried in a home-root folder. Use the canonical layout.
- **Channel not subscribed** — the bot must be *subscribed* to every served
  channel or its events never arrive. Subscribe the bot, don't just name it.
- **Stale binary shadowing** — verify the version the service execs by absolute
  path (`~/.local/bin/zulip-acp --version`) or the running image
  (`readlink /proc/<pid>/exe`), not a bare `zulip-acp --version` that PATH may
  resolve to a checkout copy.
- **state_dir in config vs `--state-dir`** — if both are set the flag wins. Keep
  state under `~/.config/zulip-acp/state`; never leave it defaulting into a cwd.

## Handoff checklist

- [ ] `~/.local/bin/zulip-acp --version` is the intended release.
- [ ] `~/.config/zulip-acp/{config.json,env,state}` present; `env` is mode 0600.
- [ ] `--print-paths` shows the expected site, channels, state dir, agent cmd.
- [ ] systemd user unit enabled + `loginctl enable-linger`.
- [ ] Real message into a served channel round-trips to an agent reply.
- [ ] Realm edit window is unlimited; 10k truncation understood.
- [ ] Any old bare-process deploy retired, state migrated first.
