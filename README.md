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
   email and API key. **Subscribe it to the channels it should serve** — with
   `"channels": ["*"]` that subscription is the entire configuration.

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

   Follow-ups in the same topic need no mention. With `"dms": true` you can
   also just DM the bot — no mention needed there at all.

## How it behaves

- **Immediate acknowledgement.** Zulip has no typing indicator, so the moment
  the relay accepts a message it reacts to it with `:eyes:`, and removes the
  reaction when the turn ends. It costs nothing in the topic and is retracted
  even when the turn ends in silence.
- **`@`-mention** in a topic the relay does not know → starts a conversation,
  and the answer **streams** into a single message that is edited as it arrives.
- **Any message** in a topic the relay has already engaged → answered too. If
  the agent decides the message was not for it, it emits the silent sentinel and
  the relay posts nothing at all. Otherwise a `Thinking…` placeholder goes up as
  soon as the streamed text can no longer *be* the sentinel — usually the first
  chunk — and the answer replaces it when the turn completes.
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
- **Direct messages** (opt-in, `"dms": true`). Every message in a DM with the
  bot — 1:1 or group — is addressed to it by construction, so **mention-gating
  is off**: the relay answers every message, and there is no ambient/abstain
  path. A DM conversation is keyed on the participant *set*, so the same people
  always land in the same session however Zulip happens to order them, and a
  group DM is its own conversation distinct from any 1:1 within it. Answers
  stream, roll over and carry `outbox/` attachments exactly as in a topic.
  DMs are **not** gated by `channels` — a DM is in no channel — so
  `allowed_user_ids` is the only allowlist that applies to one; set it unless
  every realm member should be able to open a session. Note that it gates the
  *sender*, not the audience: a reply in a **group** DM is delivered to every
  participant, so an allowlisted user can pull agent output in front of people
  who are not on the list. `"dms": true` with no
  `channels` at all is a valid DM-only relay.
- **`"channels": ["*"]` follows the bot's subscriptions.** Add the bot to a
  channel and it is served within seconds — no config edit, no restart; remove
  it and the relay stops answering there. Both moves are logged. The sentinel
  may stand alone or sit next to explicit entries
  (`["fleet", "*"]`), in which case the explicit channels stay in the allowlist
  even if the bot is later unsubscribed from them: the config wins. (The bot
  still has to be subscribed to *receive* events from a private channel.) An **empty**
  `channels` list stays a fatal error — "everything" must be asked for, never
  defaulted into.

## Commands

A message naming a relay command is handled by the **relay itself**. It never
reaches the agent and consumes no turn, so `!stop` and `!status` work even
while the agent is busy or wedged. The reply is posted as an ordinary message
in the same topic, or the same DM.

The broker is `acp-kit/command`, shared with `poe-acp`, so the two relays
offer the same surface.

| command | what it does |
| --- | --- |
| `!help` | list these commands |
| `!opts` | interactive options panel — buttons in the Zulip web app, a plain command list everywhere else |
| `!status` | where you are, the conversation and its state directory, model, session, relay version and uptime |
| `!model` | list the models the agent reports |
| `!model <filter>` | narrow that list |
| `!model <id>` | switch **this conversation** to that model, from the next message on |
| `!new` | retire this conversation and start a fresh one |
| `!stop` | interrupt the turn currently running here |
| `!schedules` | list the prompts the agent has armed here (needs `relay_mcp`) |
| `!unschedule <id>` | cancel one of them |
| `!login [provider]` | connect an LLM provider by OAuth; paste the redirect URL back as your next message |
| `!login cancel` | abort a login in progress |

Older spellings still work and are not going away: `!models`, `!relay`,
`!bot`, `!whoami`, `!reset`, `!cancel-login`.

`!opts` is the one command that is **not** in the shared broker: it renders
controls that already exist onto a surface only Zulip has. See
[Options panel](#options-panel).

Commands the **agent** advertises are forwarded to it when they are on a small
curated allowlist — `!reload`, `!logout`, `!compact`, `!session`,
`!changelog`, `!mcp`, `!skills`. These do reach the agent and stream a reply
like any other turn. `!help` lists whichever ones your agent currently offers.

`!new` is why this surface matters most. A channel conversation can always be
replaced by opening a new topic, but a **direct message is keyed on the
participant set**, so without `!new` a DM is one conversation forever with no
way to clear its context. Retiring is not deleting: the old conversation keeps
its `state/convs/<id>/` directory, it just stops answering. Your `!model`
choice carries over; the history does not.

### Grammar

- **Sigils.** `!`, `/` and `.` are all accepted; `!` is what gets advertised.
- **Case-insensitive** on the command name — `!NEW` works. The *argument*
  keeps its case, since a model id is a literal.
- **Zulip's own `/me`, `/poll` and `/todo` always reach the agent untouched.**
  They are real messages and widgets, not client-side slash commands, so the
  relay never intercepts them — including mid-login, where a `/poll` must not
  be mistaken for a pasted redirect URL. (Zulip's actual slash commands, like
  `/ping`, are handled by your client and never reach a bot at all.)
- **Prose that merely starts with a bang still reaches the agent.**
  `!important: fix the parser`, `!5 minutes left` and a lone `!` are forwarded
  unchanged. A command-shaped `!` token that names nothing answers with the
  **options panel** and is **not** forwarded either — `!hepl` should not become
  an agent turn, and the moment you mistype a command is the moment you most
  need the menu.
- **To say something that really does start with a command name, double the
  bang.** `!!new` arrives at the agent as `!new`.

Gating is exactly a prompt's. In a **DM** commands always work, because every
message there is addressed to the bot. In a **channel** a command is honoured
when the message `@`-mentions the bot *or* the topic is already engaged — the
relay stays out of topics it was never summoned to, `!help` included.
`allowed_user_ids` gates commands exactly as it gates prompts, and the
never-answer-a-bot guard runs ahead of all command parsing. No command ever
allocates a conversation: `!help` in a fresh topic leaves nothing on disk.

### Options panel

`!opts` posts one **live** control message per conversation, kept current:

- Its header is the current state — the model this conversation will actually
  use — so the panel is the menu and the status line at once.
- In the Zulip **web app** it renders as buttons, using Zulip's `zform` widget.
  A button carries a `reply` string that the web client sends as an *ordinary
  message from you*, so every button is just a command you could have typed;
  nothing new hides behind one, and clicks pass through the same gates and the
  same allowlist as text.
- **Everywhere else — the phone app included — the buttons do not render**, so
  the panel's markdown body lists the same commands and is written to be usable
  with a thumb. If the server refuses the widget outright (widgets disabled, or
  an older Zulip), the panel still posts without it.
- Model buttons come from the agent's own probe, so one can never offer a model
  the agent does not have. The list is capped; `!model <filter>` reaches the
  rest.
- Changing a setting is acknowledged with an **emoji reaction** on your
  message — no reply, and nothing about your configuration enters the
  transcript the model reads. That includes a change the *agent* makes through
  its loopback tool: every model change goes through one place, so the panel
  never claims a model you are not on.
- **A widget message cannot be edited.** Zulip refuses a content edit on any
  message carrying a widget (`"Widgets cannot be edited."`), so the panel
  updates by being **re-posted and the old one deleted** — the one and only
  thing this relay ever deletes. The topic still holds exactly one live panel,
  and it lands where you are reading rather than somewhere above the fold. If
  the realm forbids the bot deleting its own message, the old panel is rewritten
  to a one-line pointer where that is possible, and otherwise simply left: stale,
  never wrong, since every button on it is still a valid command.
- Before a place has a conversation, the panel says so and how to start one:
  commands never allocate one, and in an unengaged channel topic a button's
  reply would not even be answered.

## Configuration

Every key is optional except the credentials and `channels` (which may be
omitted only when `"dms": true` makes it a DM-only relay).

| key | default | meaning |
|---|---|---|
| `site` | — | Zulip base URL. Env: `ZULIP_SITE` |
| `bot_email` | — | bot's Zulip email. Env: `ZULIP_EMAIL` |
| `bot_api_key` | — | bot's API key. Env: `ZULIP_API_KEY` (preferred) |
| `channels` | — | channel names **or** ids to serve; also the allowlist. `"*"` = every channel the bot is subscribed to, tracked live |
| `dms` | `false` | serve direct messages (1:1 and group). Not gated by `channels` |
| `allowed_user_ids` | everyone | restrict who the relay answers, in channels and DMs alike |
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
| `ack_emoji` | `eyes` | bare emoji name (no colons) reacted onto a message while its turn runs; `""` disables |
| `repost_on_close` | `true` | at the end of a streamed turn, re-post the finished answer as new messages and delete the placeholder-seeded originals, so the mobile push carries the answer instead of `Thinking...`. See below |
| `relay_mcp` | `false` | **agent→relay loopback** — let the agent post out of band and schedule prompts back into its own conversation. See below |
| `max_schedule_depth` | `3` | how long a schedule→turn→schedule chain may get |
| `max_schedules_per_conv` | `10` | schedules armed at once in one conversation |
| `max_schedules_total` | `100` | schedules armed at once across the relay |
| `min_schedule_interval_seconds` | `60` | floor on a repeating schedule |

The API key is declared as a secret and is **scrubbed from the agent's
environment** before the child process starts.

### Skills

The relay injects a fir-style `<available_skills>` catalog into every session's
system prompt, alongside `system_prompt`. Two layers are merged:

- **builtin** — SKILL.md files embedded in the binary (`internal/skills/bundle/`)
  whose frontmatter sets `builtin: true`. They are extracted to a
  content-hashed dir under `$TMPDIR` at startup.
- **host** — `<config-dir>/skills/<name>/SKILL.md`, i.e. next to `config.json`.
  A host skill whose `name` matches a builtin **replaces** it; that is also how
  you disable a builtin (shadow it with a stub).

The host layer is rescanned on every session create/resume, so a skill dropped
into the host dir is picked up **without restarting the relay**. Builtins are
extracted once per process. A missing or malformed skill dir is logged and
skipped — it never blocks startup.

`disable_system_prompt` suppresses the catalog too.

### Notifications and `repost_on_close`

Zulip generates a mobile push notification when a message is **created**, never
when it is edited. The relay streams by posting an eager `Thinking...`
placeholder and editing the answer into it, so every push on your phone used to
read `Thinking...` and never showed the reply.

With `"repost_on_close": true` (the default) the streaming experience on
web/desktop is unchanged, but when the turn finishes the relay re-posts the
finished chain as **new** messages and deletes the originals, so a fresh push
carries the real answer. Notes:

- The **whole** chain is recreated, not just the first message — deleting only
  the first would move it below its own continuations. The cost is that a turn
  split across N messages fires N notifications; N is 1 for almost every turn.
- New messages are posted before any old one is deleted, so a failure can never
  lose output; the worst case is a duplicate.
- If the bot may not delete its own messages (`delete_own_message_policy`, or a
  closed delete window), the **first** refused delete disables reposting for the
  rest of the process and logs loudly, instead of doubling every topic forever.
  Set `"repost_on_close": false` to turn the feature off outright.

### The agent→relay loopback (`relay_mcp`)

With `"relay_mcp": true` the relay hosts a small MCP server on a private unix
socket and advertises it to its own child agent. The agent can then:

- read its `status`, `list_models` and `set_model` — the same controls as the
  `!commands`, through the same code;
- `post` a message into **this** conversation out of band, so a long task can
  report progress, or a result can arrive after the turn that started it has
  ended;
- `schedule` / `list_schedules` / `unschedule` a prompt to itself. On fire it
  re-enters the same conversation with its full history and the answer streams
  into the topic normally.
- read `history` — the conversation's **own earlier messages**, oldest first,
  as raw markdown, including the bot's own past replies. That is how an agent
  whose session was cleared (or that started after a restart) recovers what a
  topic was about, without shelling out to the Zulip API with the bot's
  credentials.

That is what makes *"go do X and tell me when it lands"* expressible.

It is **off by default** because it is a real widening of what a
prompt-injected agent could do. Three things bound it:

- **It can only speak where it already is.** The conversation is resolved from
  the MCP connection token, server-side. No tool takes a channel, topic or
  user as an argument, so there is no way to address anywhere else — and that
  applies to reading as much as to posting: `history` can only read the topic
  or DM the call came from.
- **Output is bounded.** `history` caps each message body and the reply as a
  whole, keeps the newest end, and states the `before_id` to page further
  back — one call cannot flood the agent's context window.
- **Scheduling is bounded in depth, breadth and rate** (the four keys above),
  so a schedule that schedules a schedule always terminates.
- **You can see and kill what is armed**: `!schedules` lists it, `!unschedule`
  cancels it, and `!status` reports the count.

`!new` does **not** cancel schedules — it clears context, not commitments — so
use `!unschedule` for that. A firing is at-most-once: a crash in the moment
between claiming a due schedule and running it loses that one firing, which is
the safe direction for work nobody is watching.

The socket lives in a 0700 directory, the socket file is 0600, and each
session gets its own random token. Nothing outside this host's user can reach
it, and nothing outside the relay's own child agent is told it exists.

Useful flags: `--print-paths`, `--version`, `--channels`, `--agent-cmd`,
`--state-dir`. Set `ZULIP_ACP_DEBUG=1` for protocol-level logging.

## Running as a service

```bash
cp packaging/systemd/zulip-acp.service ~/.config/systemd/user/
chmod 600 ~/.config/zulip-acp/env      # holds ZULIP_API_KEY=…
systemctl --user daemon-reload
systemctl --user enable --now zulip-acp
loginctl enable-linger $USER           # survive logout
journalctl --user -u zulip-acp -f
```

### Graceful reload

```bash
systemctl --user reload zulip-acp      # binary or config change
```

SIGHUP makes the relay stop polling the Zulip event queue **without deleting
it**, wait for every in-flight agent turn to finish posting, and then re-exec
the on-disk binary **in place, same PID**. The queue buffers server-side across
the window and the new image resumes it at the same `last_event_id`, so nothing
posted during the reload is lost and nothing is delivered twice.

That matters because a hard restart is silently lossy: the event-queue cursor is
in-memory by design, so a cold start registers a fresh queue at the server's
*current* `last_event_id` and never sees anything posted before that instant —
including the message that triggered the turn the restart just killed.

It also means an agent hosted by this relay can run
`systemctl --user reload zulip-acp` **inline, mid-turn**, and still finish its
reply: `ExecReload` is a bare `kill -HUP` that returns immediately, and the
relay waits for that very turn to drain before it execs.

A hard `restart` is still required for three things: a change to the unit file,
a service that is stopped or dead, and the **first cutover** onto a build that
has reload support (the older binary has no SIGHUP handler and would just die).

Two knobs bound the drains: `-reload-drain-deadline` (30m, a leak backstop —
nothing external is waiting, and `prompt_timeout` is what bounds a turn as
work) and `-drain-deadline` (30s, a service stop — keep it under
`TimeoutStopSec`). Details in
[docs/graceful-reload.md](docs/graceful-reload.md).

## Documentation

- [docs/graceful-reload.md](docs/graceful-reload.md) — how
  `systemctl --user reload` drains and re-execs in place, why the Zulip event
  queue makes a master/worker supervisor unnecessary here (unlike `poe-acp`),
  and the failure modes.
- [docs/zulip-acp-design.md](docs/zulip-acp-design.md) — architecture, the three
  decisions that matter, and what was deliberately *not* ported from `slack-acp`.
- [docs/zulip-protocol-reference.md](docs/zulip-protocol-reference.md) — the
  Zulip wire protocol as **measured**, including the three traps that cost real
  debugging time: silent truncation, the `/register` narrow operand, and system
  bots posting into your topics.
- [BACKLOG.md](BACKLOG.md) — what is deliberately not done yet, and why.
- [internal/skills/bundle/update/SKILL.md](internal/skills/bundle/update/SKILL.md)
  — the update/reload procedure. It is **shipped in the binary**: every session
  the relay spawns gets it in its skills catalog, so a relay can be asked to
  update itself. Ordinary markdown, so read it like any other doc.
- [skills/deploy/SKILL.md](skills/deploy/SKILL.md) — first-install layout. Not
  embedded: you need it *before* there is a relay to ask. `fir` finds it with
  one line of **global** config — relative skill paths resolve against the
  working directory, so this discovers `./skills/` in every project that has
  one:

  ```json
  // ~/.config/fir/settings.json
  { "skills": ["skills"] }
  ```

  Add `internal/skills/bundle` to that list when working in this checkout, to
  pick up the embedded skills from source rather than from the installed binary.
  A per-checkout `.fir/skills -> ../skills` symlink works too (fir follows and
  de-duplicates symlinked roots). `.fir/` itself is deliberately untracked:
  agent configuration is per-deployment, the repository only ships the content.

## Development

```bash
make          # vet, race+shuffle tests with a 100% coverage gate, 5 cross-builds, licenses
make test     # quick
```

Live tests run against a real server and are excluded from the coverage gate:

```bash
ZULIP_LIVE=1 ZULIP_SITE=… ZULIP_EMAIL=… ZULIP_API_KEY=… ZULIP_CHANNEL=zulip-acp-tests \
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

Full procedures: `skills/deploy/SKILL.md` and
`internal/skills/bundle/update/SKILL.md`.

## License

MIT — see [LICENSE](LICENSE).
