# zulip-acp

[![ci](https://github.com/kfet/zulip-acp/actions/workflows/ci.yml/badge.svg)](https://github.com/kfet/zulip-acp/actions/workflows/ci.yml)
[![release](https://github.com/kfet/zulip-acp/actions/workflows/release.yml/badge.svg)](https://github.com/kfet/zulip-acp/actions/workflows/release.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/kfet/zulip-acp.svg)](https://pkg.go.dev/github.com/kfet/zulip-acp)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

Talk to a coding agent from your phone, through your own Zulip server.

`zulip-acp` bridges a **self-hosted Zulip** instance to an ACP-speaking agent
(`fir --mode acp`, Claude Code, …) over stdio. Each Zulip **topic** is a
conversation: ask a question, get a streamed answer, follow up in the same topic
and the agent remembers. Rename the topic ten turns in and the session follows
it.

It is the third relay in a family with [`poe-acp`](https://github.com/kfet/poe-acp)
and [`slack-acp`](https://github.com/kfet/slack-acp); all ACP-side machinery is
shared in [`acp-kit`](https://github.com/kfet/acp-kit).

## Why Zulip

- **No inbound HTTP.** The relay dials out only, over `/events` long polling. No
  tunnel, no webhook, no public exposure — it runs happily on a tailnet.
- **Topics are sessions.** No thread-id mapping table to lose, and topics can be
  renamed in place.
- **Streaming actually streams.** Zulip sustains ~15 message edits/sec; Slack's
  `chat.update` is ~1/sec/channel.
- **It is yours.** Self-hosted, HTTP Basic auth, one bot, no app manifest.

## Install

```bash
brew install kfet/ai/zulip-acp
```

or grab a binary from [releases](https://github.com/kfet/zulip-acp/releases), or:

```bash
go install github.com/kfet/zulip-acp/cmd/zulip-acp@latest
```

## Quick start

1. **Create a bot.** In Zulip: *Settings → Personal → Bots* (or
   `<site>/#organization/bots`) → **Add a bot** → type **Generic**. Copy its
   email and API key. **Subscribe it to the channels it should serve.**

2. **Set message editing to unlimited.** *Organization settings → Message
   editing → Message edit limit → **Unlimited***.

   > ⚠️ This is not optional. A fresh realm defaults to **10 minutes**
   > (`message_content_edit_limit_seconds = 600`). The relay edits one message
   > for the duration of a turn, so any turn longer than that starts failing
   > with HTTP 400 mid-answer.

3. **Write `~/.config/zulip-acp/config.json`:**

   ```json
   {
     "site": "https://zulip.example.com",
     "bot_email": "fir-relay-bot@zulip.example.com",
     "channels": ["fleet"],
     "agent_cmd": ["fir", "--mode", "acp"]
   }
   ```

4. **Run it**, keeping the key out of the config file:

   ```bash
   ZULIP_API_KEY=… zulip-acp --config ~/.config/zulip-acp/config.json
   ```

5. **Say hello.** In the `fleet` channel, start a topic and `@`-mention the bot:

   > `@**fir-relay** what's in this repo?`

   Follow-ups in the same topic need no mention.

## How it behaves

- **`@`-mention** in a topic the relay does not know → starts a conversation,
  and the answer **streams** into a single message that is edited as it arrives.
- **Any message** in a topic the relay has already engaged → answered too. If
  the agent decides the message was not for it, it emits the silent sentinel and
  the relay posts nothing at all.
- **Answers over ~9500 characters roll over** into further messages, marked
  `*(continued below)*` / `*(continued from above)*`. Fenced code blocks are
  closed and reopened, with their language tag, across the seam. **No text is
  ever dropped** — Zulip's 10000-character limit truncates *silently*, so the
  relay counts for itself.
- **Files.** Anything the agent writes into `outbox/` in its working directory
  is uploaded and linked at the end of the turn.
- **Restarts** are safe. An interrupted turn is marked
  `*(relay restarted — turn interrupted)*`, and the next message in the topic
  picks the session back up.
- **The relay never answers a bot**, including its own messages and Zulip's
  system notices.

## Configuration

Every key is optional except the credentials and `channels`.

| key | default | meaning |
|---|---|---|
| `site` | — | Zulip base URL. Env: `ZULIP_SITE` |
| `bot_email` | — | bot's Zulip email. Env: `ZULIP_EMAIL` |
| `bot_api_key` | — | bot's API key. Env: `ZULIP_API_KEY` (preferred) |
| `channels` | — | channel names **or** ids to serve; also the allowlist |
| `allowed_user_ids` | everyone | restrict who the relay answers |
| `agent_cmd` | `["fir","--mode","acp"]` | agent argv |
| `state_dir` | `$XDG_STATE_HOME/zulip-acp` | per-conversation cwds + journal |
| `session_idle_timeout_seconds` | `1800` | idle session GC |
| `prompt_timeout_seconds` | `600` | wall-clock cap on one turn |
| `system_prompt` | — | appended to the built-in Zulip formatting block |
| `disable_system_prompt` | `false` | skip injection entirely |
| `hide_thinking` | `false` | suppress the agent's thought lines |
| `silent_sentinel` | `<<SILENT>>` | agent output meaning "don't reply"; `""` disables abstain |
| `max_message_chars` | `9500` | per-message budget, in **code points** |
| `seal_marker` | `*(continued below)*` | closes a rolled-over message |
| `continuation_marker` | `*(continued from above)*` | opens a continuation |
| `edit_interval_ms` | `300` | streaming edit coalescing |

The API key is declared as a secret and is **scrubbed from the agent's
environment** before the child process starts.

Useful flags: `--print-paths`, `--version`, `--channels`, `--agent-cmd`,
`--state-dir`. Set `ZULIP_ACP_DEBUG=1` for protocol-level logging.

## Running as a service

```ini
# ~/.config/systemd/user/zulip-acp.service
[Unit]
Description=zulip-acp
After=network-online.target

[Service]
EnvironmentFile=%h/.config/zulip-acp/env
ExecStart=%h/.local/bin/zulip-acp --config %h/.config/zulip-acp/config.json
Restart=always
RestartSec=5

[Install]
WantedBy=default.target
```

```bash
chmod 600 ~/.config/zulip-acp/env      # holds ZULIP_API_KEY=…
systemctl --user enable --now zulip-acp
journalctl --user -u zulip-acp -f
```

## Documentation

- [docs/zulip-acp-design.md](docs/zulip-acp-design.md) — architecture, the three
  decisions that matter, and what was deliberately *not* ported from `slack-acp`.
- [docs/zulip-protocol-reference.md](docs/zulip-protocol-reference.md) — the
  Zulip wire protocol as **measured**, including the three traps that cost real
  debugging time: silent truncation, the `/register` narrow operand, and system
  bots posting into your topics.
- [BACKLOG.md](BACKLOG.md) — what is deliberately not done yet, and why.
- [.fir/skills/deploy/SKILL.md](.fir/skills/deploy/SKILL.md) and
  [.fir/skills/update/SKILL.md](.fir/skills/update/SKILL.md) — the deployment and
  update procedures. They are written as **agent skills** because the operator of
  a relay fleet is usually an agent; they are ordinary markdown, so read them
  like any other doc.

## Development

```bash
make          # vet, race+shuffle tests with a 100% coverage gate, 5 cross-builds, licenses
make test     # quick
```

Live tests run against a real server and are excluded from the coverage gate:

```bash
ZULIP_LIVE=1 ZULIP_SITE=… ZULIP_EMAIL=… ZULIP_API_KEY=… ZULIP_CHANNEL=fleet \
  go test -v ./test/
```

They exist to pin **server** behaviour — most importantly that oversized
messages are still silently truncated — so a future Zulip upgrade that changes
it is caught deliberately rather than discovered by losing someone's output.

## A note on push notifications

Self-hosted Zulip cannot send iOS push notifications without registering with
Zulip's Mobile Push Notification Service, which sees notification metadata
(sender, channel, topic, volume). `zulip-acp` takes no position on that and does
nothing about it — it is a privacy trade-off for you to make deliberately.
Without it, the mobile app only updates while it is open.

## Deployment

Canonical layout (mirrors `poe-acp`):

```
~/.local/bin/zulip-acp                    # binary (make deploy / brew)
~/.config/zulip-acp/config.json           # see docs/config.example.json
~/.config/zulip-acp/env                   # ZULIP_API_KEY=...  (mode 0600)
~/.config/zulip-acp/state/                # per-conversation state + journal.json
~/.config/systemd/user/zulip-acp.service  # packaging/systemd/zulip-acp.service
```

Logs go to journald (`journalctl --user -u zulip-acp`). Never run the relay as a
bare `./zulip-acp` out of a checkout or a home-root folder.

The relay **dials out** (long-polls `GET /api/v1/events`) — no inbound listener,
no port to open, no Tailscale Funnel. It therefore works against a tailnet-only
Zulip.

Full procedures: `.fir/skills/deploy/SKILL.md` and `.fir/skills/update/SKILL.md`.

## License

MIT — see [LICENSE](LICENSE).
