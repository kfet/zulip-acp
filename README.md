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

or, on a Linux host (Raspberry Pi included):

```bash
curl -fsSL https://raw.githubusercontent.com/kfet/zulip-acp/main/install.sh \
  | BIN_DIR=$HOME/.local/bin sh
```

`install.sh` verifies the asset's sha256 against `checksums.txt` and installs
atomically. `BIN_DIR=` picks the destination — pass it: the default is
`/usr/local/bin` when that is writable, while the systemd unit and
`scripts/converge.sh` both expect `~/.local/bin/zulip-acp`. `VERSION=vX.Y.Z`
pins a release. With no token the latest release is resolved from the
`releases/latest` redirect, which spends **no** GitHub API quota — that matters
because the unauthenticated REST limit is 60 requests/hour *per IP*, so a NAT'd
fleet used to exhaust it partway through and leave every later host failing.
`GITHUB_TOKEN=` is needed only for a private repo; this one is public. It is
generated from
[distkit](https://github.com/kfet/distkit)'s canonical template — do not edit
it by hand; run `make install.sh`.

Or grab a binary from [releases](https://github.com/kfet/zulip-acp/releases),
or:

```bash
go install github.com/kfet/zulip-acp/cmd/zulip-acp@latest
```

Upgrades are never a hand-placed binary: `zulip-acp update` (below).

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
- **Files, both ways.** Anything the agent writes into `outbox/` in its working
  directory is uploaded and linked at the end of the turn. Anything a **human
  attaches** to a message is downloaded into `inbox/` and the agent is told the
  local path — with images also passed inline as ACP image blocks when the agent
  supports them — so "what's in this photo?" just works instead of the agent
  saying it cannot see the attachment. On by default
  (`"inbound_attachments": false` to disable); capped by
  `max_attachment_bytes` (20 MB) and `max_attachment_total_bytes` (60 MB), with
  anything over cap skipped and named rather than failing the turn. Inbound
  files persist alongside the conversation's other state; `!new` starts a fresh
  conversation with an empty `inbox/` and leaves the old files on disk.
- **Every answer is signed** with a one-line italic footer naming the model and
  the agent's own mood/plan labels — `*🏛️ opus-4.5 • steady • 2/5*` — from the
  `dev.acp-kit.status-line/v1` extension. It is a footer, not a header: mood and
  plan arrive mid-turn, so only the end of the answer can carry the final
  snapshot. The same line animates the live `Thinking…` placeholder. Turns that
  produce no output, and turns that fail, are not signed.
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
- **`autotopic_channels` names general chat.** Zulip 11's "general chat" is
  the channel's empty topic (which a server reports as the literal string
  `general chat` unless the client declares the `empty_topic_name` capability —
  the relay does not, so existing conversation keys stay put; both spellings
  are recognised). A relay answering there buries every
  conversation in one undifferentiated feed. In a channel listed here — names
  or ids, a subset of the served set — a general-chat message that the relay
  accepts is **moved** to a topic generated from its own text
  (`propagate_mode=change_one`, so nobody else's messages travel with it), and
  the conversation is opened there. The move happens *before* the conversation
  is allocated, so no journal migration is involved; if the server refuses it —
  realm `can_move_messages_between_topics_group` or the
  `move_messages_within_stream_limit_seconds` time limit, an older Zulip — the
  relay logs it and answers in
  general chat exactly as it would have. Everywhere else general chat is an
  ordinary topic.
  The generated name is a **placeholder**, not a title: it is the opening line
  stripped of markdown and truncated, because at that instant nothing has read
  the message. With `relay_mcp` on, the turn is told so and the agent replaces
  it through `rename_topic` (below) once it knows what the conversation is
  about.
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
| `!branch [#**channel**] <text>` | spin `<text>` out into a new topic that can read this one |
| `!new` | retire this conversation and start a fresh one |
| `!stop` | interrupt the turn currently running here |
| `!schedules` | list the prompts the agent has armed here (needs `relay_mcp`) |
| `!unschedule <id>` | cancel one of them |
| `!login [provider]` | connect an LLM provider by OAuth; paste the redirect URL back as your next message |
| `!login cancel` | abort a login in progress |

Older spellings still work and are not going away: `!models`, `!relay`,
`!bot`, `!whoami`, `!reset`, `!cancel-login`.

`!opts`, `!archive` and `!branch` are the commands that are **not** in the
shared broker: each needs something only Zulip has — a button widget, a
cross-channel topic move, a topic to create. See [Options panel](#options-panel),
[Archiving a topic](#archiving-a-topic-archive_channel) and
[Branching a topic](#branching-a-topic-branch).

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
| `site_aliases` | `[]` | other host names the same realm answers to, so an upload link copied from a browser on one of them is still recognised as ours. Bare hosts or full URLs |
| `bot_email` | — | bot's Zulip email. Env: `ZULIP_EMAIL` |
| `bot_api_key` | — | bot's API key. Env: `ZULIP_API_KEY` (preferred) |
| `channels` | — | channel names **or** ids to serve; also the allowlist. `"*"` = every channel the bot is subscribed to, tracked live |
| `ambient_channels` | `[]` | channels (names or ids) that engage with **no** @-mention, like a DM |
| `autotopic_channels` | `[]` | channels (names or ids) where a **general chat** message is moved to a topic named after it |
| `dms` | `false` | serve direct messages (1:1 and group). Not gated by `channels` |
| `allowed_user_ids` | everyone | restrict who the relay answers, in channels and DMs alike |
| `agent_cmd` | `["fir","--mode","acp"]` | agent argv |
| `state_dir` | `$XDG_STATE_HOME/zulip-acp` | per-conversation cwds + journal |
| `session_idle_timeout_seconds` | `1800` | idle session GC |
| `no_progress_timeout_seconds` | `120` | cut a turn with no agent output **and** no tool activity for this long. Tool calls reset it, so a long-running tool is never cut |
| `prompt_timeout_seconds` | off | **opt-in** absolute ceiling on one turn, regardless of progress. `0`/unset = no ceiling (it used to mean 600) |
| `system_prompt` | — | appended to the built-in Zulip formatting block |
| `disable_system_prompt` | `false` | skip injection entirely |
| `hide_thinking` | `false` | suppress the agent's thought lines |
| `silent_sentinel` | `<<SILENT>>` | agent output meaning "don't reply"; `""` disables abstain |
| `max_message_chars` | `9500` | per-message budget, in **code points** |
| `seal_marker` | `*(continued below)*` | closes a rolled-over message |
| `continuation_marker` | `*(continued from above)*` | opens a continuation |
| `edit_interval_ms` | `300` | streaming edit coalescing |
| `stream_edits` | `true` | publish the answer as it arrives. `false` = **quiet mode**: no intra-turn edits at all, the whole answer is published once when the turn closes. See below |
| `spinner_interval_ms` | `900` (`0` in quiet mode) | animation period of the `Thinking...` placeholder; `0` posts it once and never edits it |
| `ack_emoji` | `eyes` | bare emoji name (no colons) reacted onto a message while its turn runs; `""` disables |
| `reactions` | `true` | deliver emoji reactions (added **and** removed) into the owning conversation as one coalesced ambient turn. See below |
| `archive_channel` | `archive` | channel a topic is **moved** to by the `:wastebasket:` reaction or `!archive`; must be a channel the relay does **not** serve. `""` disables. See below |
| `repost_on_close` | `true` | at the end of a streamed turn, re-post the finished answer as new messages and delete the placeholder-seeded originals, so the mobile push carries the answer instead of `Thinking...`. See below |
| `relay_mcp` | `false` | **agent→relay loopback** — let the agent post out of band and schedule prompts back into its own conversation. See below |
| `inbound_attachments` | `true` | download the files a human attaches into the conversation's `inbox/` and put their local paths (and images, inline, where the agent takes them) in front of the agent. See below |
| `max_attachment_bytes` | `20971520` (20 MB) | cap on ONE inbound attachment; anything larger is skipped and named in the prompt |
| `max_attachment_total_bytes` | `62914560` (60 MB) | cap on one message's worth of inbound attachments; must be ≥ `max_attachment_bytes` |
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
  content-hashed dir under `<state-dir>/skills/` at startup — a stable path,
  because the system prompt promises the agent these paths stay valid for the
  session and a graceful reload replaces the process image mid-session.
  Extractions from older versions of the bundle are removed as they are
  superseded.
- **host** — `<config-dir>/skills/<name>/SKILL.md`, i.e. next to `config.json`.
  A host skill whose `name` matches a builtin **replaces** it; that is also how
  you disable a builtin (shadow it with a stub).

The host layer is rescanned on every session create/resume, so a skill dropped
into the host dir is picked up **without restarting the relay**. Builtins are
extracted once per process. A missing or malformed skill dir is logged and
skipped — it never blocks startup.

`disable_system_prompt` suppresses the catalog too.

### Quiet mode (`stream_edits`, `spinner_interval_ms`)

Every edit re-renders the whole message — on the server, in the web/desktop
client, and again on the phone. A long streamed turn therefore *flickers*. Two
knobs cut how often the relay edits its own message:

- `"stream_edits": false` — **quiet mode**. No intra-turn edits at all: the
  placeholder goes up, the answer accumulates in memory, and the whole thing is
  published once when the turn closes. You lose live streaming; you gain a
  still screen.
- `"spinner_interval_ms": 0` — post the `Thinking...` placeholder once and never
  animate it. No spinner goroutine is started at all. Any positive value sets
  the animation period instead (default `900`).

The two are resolved together, because the obvious combination is a trap: the
spinner only stops when the first streamed chunk *replaces* the placeholder, so
quiet mode with an animated placeholder would spin for the entire turn — the
worst of both. Leaving `spinner_interval_ms` unset in quiet mode therefore
turns the spinner **off**. An explicit positive value is still honoured, which
makes the spinner the only edit of the turn; that is a deliberate choice, so it
is not overridden.

Neither knob touches `repost_on_close`: the finished answer is still re-posted
as a new message so the push notification carries it. `edit_interval_ms` stops
mattering in quiet mode — there is no coalescing tick left to pace.

The cost of quiet mode is that a relay restart mid-turn leaves nothing but the
placeholder (marked `turn interrupted`): text that was never published cannot
survive the process. An agent error or a superseded turn still publishes
whatever was produced, because both close the message.

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

### Emoji reactions (`reactions`)

With `"reactions": true` (the default — opting out is the deliberate act) an
emoji reaction reaches the agent as one compact ambient turn:

```
[reaction] Ada Lovelace added :tada: to your own message 1234 ("the first few words…")
```

Taking a reaction back is delivered too — `removed :tada: from …` — because
un-reacting is real signal: an approval withdrawn, a trigger retracted.

**A burst is one turn.** Reactions are buffered per conversation for a few
seconds and delivered together:

```
[reactions] 3 in this conversation:
- Ada Lovelace added :tada: to your own message 1234 ("…")
- Bob Miller added :+1: to your own message 1234 ("…")
- Carol removed :eyes: from message 1200 by Dave ("…")
```

Ten people reacting to the same message costs one turn, not ten — which is what
makes the default affordable. A reaction that arrives while a turn is running is
folded into the same buffer rather than cancelling it or being dropped, and a
burst larger than 20 is still one turn (the rest are counted, not listed).
Buffering applies to reactions only: inbound human messages are never delayed.

The gating is deliberately narrow, because Zulip's `reaction` events are **not**
limited by the event queue's narrow — the relay sees reactions on everything the
bot can see. In order:

- only `add` and `remove` ops (anything else is a shape we do not understand);
- the relay's own reactions (the `ack_emoji`, the `!opts` tick) and any other
  bot's are dropped before anything else, so it can never loop on itself;
- `allowed_user_ids` applies exactly as it does to messages;
- the reacted-to message must resolve to a conversation the relay is **already
  engaged in**. A reaction never creates a conversation or a session: it cannot
  summon the bot;
- unresolved lookups are rate-limited and cached, so realm-wide reaction traffic
  cannot drive the relay's API usage.

It is delivered on the **ambient** path, so the agent can decline it with
`silent_sentinel`. Which reactions deserve a reply is the **agent's** judgement,
never a relay-side heuristic: the relay delivers every reaction that passes the
gates above and makes no attempt to guess which ones are interesting. The
built-in system prompt therefore sets the norm — a reaction is signal, not a
request; most deserve no reply at all; answering with the sentinel is the
expected outcome, not a failure; reply only when the reaction plainly changes
something or plainly asks for something; and never send a "thanks!"-style
acknowledgement. Set `"reactions": false` if you would rather a stray `:+1:`
never cost a turn.

### Branching a topic (`!branch`)

An idea surfaces in the middle of a conversation and deserves a topic of its
own. Type, in the topic it surfaced in:

```
!branch <text>
!branch #**other-channel** <text>
```

`<text>` is the **first message** of the new topic — both the opening prompt
and, through the same first-line heuristic `autotopic_channels` uses, the
topic's name. The origin agent never sees the command.

What happens:

- a new topic is created in this channel, or in the one you named. On a
  case-insensitive collision with a topic that already exists there the name
  gets a ` (2)` suffix — a branch never appends into a live conversation;
- the relay posts one opening message in it, @-mentioning you, so your phone
  actually surfaces the topic you are not looking at;
- one pointer line — `branched → #**channel>New topic**` — goes into the origin
  topic;
- the new session's first turn is `[branched from #**channel>Origin topic@<id>**]`
  followed by your text;
- the journal records the origin as (channel, topic, **bare message id**), in
  the same atomic write that allocates the conversation. The id is what the
  origin is actually located by at read time: `!archive` — or any human — can
  rename or move a topic, so the stored name rots, and a later topic of the same
  name would otherwise be read instead.

The context is **not** summarised into the new topic. It is pulled, lazily, by
the agent that needs it: with `relay_mcp` on, the branched session's `history`
tool gains `origin: true`, which reads the origin conversation clamped to
messages **at or before** the branch point. A lazy fetch cannot be wrong about
what matters; a summary written before anyone knows what the branch is about
can. It also means you can branch out of a topic the relay was never engaged in
— a human-only thread in an ambient channel — where there is no session to ask
for a summary in the first place.

The permission model is one sentence: **a session may read its declared parent
conversation, and nothing else.** One hop only — the parent's own parent is not
reachable, or "read my ancestors" would quietly become "read everything".

From a **direct message** you must name a channel. The relay will not guess
where to publish the contents of a private conversation. The destination must
be a channel the relay serves, and a branch that would land in the topic it was
typed in is refused — that is not a branch.

If the origin's branch-point message is later deleted, or its topic moves to a
channel the relay no longer serves, `origin: true` simply reports that the
origin is no longer reachable. It never falls back on the stored name.

### Linked messages are hydrated

When an incoming message contains a link to another Zulip message —
`#**channel>topic@949**`, or any URL with `/near/949` in it — the relay fetches
that message and puts it in front of the agent as a `[linked]` block, so "see
this" means something.

Bounded on purpose: at most three per message, each body truncated, deduped so
re-pasting the same link does not re-inject it, and — the check that matters —
**the same channel only**. `GET /messages/{id}` runs with the *bot's*
permissions, and the bot is subscribed to every channel it serves, so "served"
alone would let anyone in one served channel paste a `/near/` link into another
and have the relay read out a channel they cannot see. Restricting hydration to
the channel the link was posted in makes the check free and exact: whoever
posted there can read there. There is no hydration in a direct message, in
either direction.

### Attachments (`inbound_attachments`)

A Zulip message never carries a file. It carries markdown containing
`![photo.jpg](/user_uploads/2/20/HASH/photo.jpg)`, and the bytes sit behind an
authenticated endpoint — so an agent handed the raw text is handed a path it
cannot open, and answers "I can't see the attachments".

With `"inbound_attachments": true` (the default) the relay downloads them for it,
into `inbox/` in the conversation's working directory, and appends a block like:

```
[relay] This message has attachments. They have been downloaded into this
conversation's working directory — read them from the local paths below, do not
fetch the Zulip links.
- photo.jpg — local path: /…/convs/c1a2b3/inbox/photo.jpg (image/jpeg, 1.8 MB). Zulip link: /user_uploads/2/20/HASH/photo.jpg
```

The Zulip link is kept so no context is lost, and where the agent has advertised
ACP's `image` prompt capability the image *itself* also goes in as a content
block, so a vision model sees it without a tool call. Everything else is
path-only — as is any image over 5 MB, or past 10 MB of inlined images in one
message, which belong on disk rather than in a prompt.

Both `![alt](path)` and `[name](path)` are recognised, as is the absolute
`https://<your-realm>/user_uploads/…` form that "Copy link" produces. **A URL on
any other host is ignored**: the bot's credentials never leave its own realm.

If your realm answers to more than one name — say the relay dials a Tailscale
name while the humans browse a vanity domain — list the others in
`site_aliases`, or a link pasted from the other name is silently not an
attachment:

```json
"site": "https://zulip.internal.example",
"site_aliases": ["zulip.example.com"]
```

Composer-attached files are unaffected either way: those are relative paths.

Bounds, because anyone who can post in a served channel can attach a file: ten
references per message, `max_attachment_bytes` per file, and
`max_attachment_total_bytes` per message. Anything over cap — or any download
that simply fails — is **skipped and named**, so the agent is told what it did
not get and the turn still happens. Nothing here can fail a turn.

The files **persist**, next to the conversation's other state. `!new` does not
delete them: it starts a fresh conversation with a fresh working directory, so
the new session sees an empty `inbox/` while the old files stay on disk. Prune
`inbox/` yourself if a long-running conversation collects more than you want to
keep.

### Archiving a topic (`archive_channel`)

A finished conversation can be got out of the way with one gesture: react
`:wastebasket:` to the relay's **last** message in the topic — or type
`!archive` (`!arch`) — and the relay posts a warning. React `:wastebasket:` **to
that warning** and the topic is moved, whole, to `archive_channel`.

Nothing is deleted. The messages travel with the topic, and the conversation's
`state/convs/<id>/` working directory stays exactly where it is. What ends is the
conversation: the in-flight turn is cancelled, the ACP session stops, the journal
entry is retired, and only then is the move issued — in that order, because the
move arrives back as the same `update_message` event a rename does, and a session
that migrated with it would follow the topic into the archive instead of ending.

The destination must be a channel the relay does **not** serve. That is what
makes an archive final: an unserved channel is outside the allowlist by
construction, so an archived topic cannot re-engage the relay, and recovery is
one move back. What accumulates there is a retention-policy question, not the
relay's.

"The relay's last message" survives a restart: it is written through to the
journal and, failing that, resolved with one narrowed `GET /messages`, so the
gesture works on an old topic and not only on one the relay has answered in
since it last started. A conversation that has been retired (`!new`, or an
earlier archive) owns no message and arms nothing.

Only the confirmation counts, and only on the warning message: another emoji does
nothing, a `:wastebasket:` somewhere else does nothing, un-reacting does nothing.
An unconfirmed arming simply **expires** after two minutes and is logged; a later
tap is never a late confirmation — it starts a fresh cycle with a fresh warning.
The same `allowed_user_ids` gate that governs messages governs the gesture.

The relay settles at **startup** whether any of this can work — the channel
exists, it is not served, and realm policy
(`can_move_messages_between_channels_group`) lets the bot move messages between
channels — and silently disables the control with an explanatory log line if not.
Nobody should discover a missing permission by tapping the emoji. Zulip 12.0+ is
needed to answer that permission question about a *bot* user; on an older server
the control stays off.

A topic a human moves out of the served set by hand is treated the same way: the
conversation is retired rather than allowed to follow the topic somewhere the
relay does not answer.

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
- `rename_topic` — rename the topic **this** conversation lives in. It exists
  for the `autotopic_channels` placeholder above: the relay names a new topic
  from the opening line before anything has understood it, and the agent is
  the only thing that can do better. The rename is **deferred to the end of
  the turn** — a turn posts into the topic it started in, so moving it early
  would split the answer in two — and it moves the whole topic, so the
  conversation and its session follow it.
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
  or DM the call came from, and `rename_topic` can only rename that same
  topic.
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
nothing external is waiting, and `no_progress_timeout_seconds` is what bounds a turn as
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
~/.local/bin/zulip-acp                    # binary (first install: install.sh /
                                          # brew; upgrades: zulip-acp update)
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

### Upgrading

```bash
zulip-acp update                 # verify sha256, swap the binary atomically
systemctl --user reload zulip-acp   # drain in-flight turns, re-exec in place
```

`zulip-acp update` resolves the release over the GitHub API — asset bytes
included, which is what let it work while this repo was private and still
works unauthenticated now (`GITHUB_TOKEN`/`GH_TOKEN`/a logged-in `gh` is used
when present, and is worth having behind a shared IP for the rate limit) —
verifies the asset against `checksums.txt`, and renames it over the running
binary — the `ETXTBSY`-safe swap. `--check` reports without installing and
exits **3** when an update exists, `--version vX.Y.Z` pins, `--restart-cmd`
recycles afterwards. A Homebrew install is handed to `brew upgrade` rather
than swapped underneath the package manager; a package-manager prefix
(`/usr/bin`, `/sbin`, the brew trees) is refused even when you are root, and
an install directory you do not own is refused with the command to use
instead. **Never hand-place a
binary** — there is deliberately no `make deploy`.

The whole mechanism is [distkit](https://github.com/kfet/distkit), shared with
`fir`, `harb`, `mintick` and the sibling relays; `internal/updater` is just
the four strings that name this binary.

Hosts with a spec in `bots/` are converged instead, from `dist.lock`:

```bash
scripts/converge.sh --tot            # resolve latest-of-everything into dist.lock
scripts/converge.sh <bot>            # dry run
scripts/converge.sh <bot> --apply    # binary + fir + exts + config + unit + recycle
```

Full procedures: `skills/deploy/SKILL.md` and
`internal/skills/bundle/update/SKILL.md`.

## License

MIT — see [LICENSE](LICENSE).
