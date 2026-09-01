# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.7.0] - 2026-09-01

### Added

- **The agent→relay loopback (`"relay_mcp": true`, off by default).** The
  relay hosts an MCP server on a private unix socket and advertises it to its
  own child agent, so the agent can drive the relay from inside a turn. ACP
  has no agent-initiated message and the streaming sink is bound per turn, but
  an MCP tool call runs agent→client — so this needs no protocol extension.
  Tools: `status`, `list_models`, `set_model`, `new_session`, `post`,
  `schedule`, `list_schedules`, `unschedule`.
- **`post`** — send a message into the current conversation out of band:
  progress on a long task, or a result that arrives after the turn that
  started it has ended. It posts through the rollover splitter like every
  agent answer, so a long post cannot be silently truncated by Zulip.
- **Scheduled prompts.** A schedule fires as a prompt back into the
  conversation it was armed in — same conv-id, same ACP session, so the agent
  has the topic's full history — and the answer streams into the topic
  through the existing path. Persisted in `<state-dir>/schedules.json`
  alongside the journal, so they survive a restart. Bounded by
  `max_schedule_depth` (3), `max_schedules_per_conv` (10),
  `max_schedules_total` (100) and `min_schedule_interval_seconds` (60).
- `!schedules` and `!unschedule <id>`, so a human can see and kill work the
  agent armed; `!status` now reports how many schedules are armed here.
- `internal/zulipmcp`: the MCP server's Zulip-side identity (socket naming,
  env vars, the `mcp-serve` redirector subcommand). It owns no tools — those
  are `acp-kit/relaytool`'s, because every one is relay-generic.
- `journal.LookupID`: resolves a conv-id back to its conversation, which is
  what turns an MCP session key into a broker conversation token.

### Security and safety

- **A tool call never names its conversation.** `mcphost` binds the session
  key server-side from the connection token, so the conversation a tool acts
  on is unspoofable. `post` deliberately has **no target parameter** — an
  agent that could post into arbitrary channels would be a realm-wide
  megaphone for anything that can prompt-inject it, so in v1 that is not a
  config toggle, it is inexpressible.
- **No `stop` tool, and `new_session` is deferred**, under one rule: a
  loopback tool must never destroy the turn that is calling it.
- **The own-sender guard is now load-bearing.** The agent posting into its own
  topic produces an event from the bot's own user id; `handleMessage` drops it
  before any allowlist. Covered by `TestLoopbackPostDoesNotFeedItselfBack`,
  which posts an @-mention into an engaged topic — every other gate would let
  it through.
- **A scheduled turn re-applies every gate at fire time**, not arm time: the
  channel must still be served, DMs must still be enabled, and the
  conversation must still exist. Failing one disarms the schedule instead of
  retrying it forever. A scheduled turn also never supersedes a human one — it
  waits for the conversation to go idle.

### Changed

- **AGENTS.md no longer says "no MCP surface".** That line described a
  deliberate constraint which this change deliberately lifts, for the one
  case the protocol makes correct: a private, single-consumer, token-scoped
  server offered only to the relay's own child agent. The relay still exposes
  no MCP surface to anything outside its own process.
- `!schedules` and `!unschedule` appear only when `relay_mcp` is on. The
  Handler implements `command.Scheduler` unconditionally, so the capability is
  gated on `CanSchedule()` rather than on the type assertion.
- Requires `acp-kit` v0.9.1.

## [0.6.0] - 2026-09-01

### Changed

- **The `!command` surface is now `acp-kit/command`, ported from `poe-acp`
  rather than invented here.** v0.5.0 shipped a small hand-written broker;
  `poe-acp` already had a mature, fully tested 655-line one. That package
  moved to acp-kit whole — tests included — and both relays now consume it, so
  the two surfaces cannot drift. `poe-acp` deletes its copy in the same
  change. See the design doc for why v0.5.0's "promotion is premature" call
  was wrong: the second consumer already existed and had not been looked at.
- **New commands inherited from `poe-acp`:** `!login [provider|cancel]` with
  the two-call `_meta.auth.interactive` bridge (paste the redirect URL back as
  your next message), `!model <filter>` for narrowing the model list, and
  **agent-command passthrough** — `!reload`, `!logout`, `!compact`,
  `!session`, `!changelog`, `!mcp`, `!skills` are forwarded to the agent as
  `/reload` etc. when it advertises them. `resume`, `continue`, `name`,
  `share` and `export` are deliberately excluded: the relay owns the
  conversation → session mapping, and letting the agent switch its own session
  underneath it would desync that mapping.
- **Back-compat aliases** now work: `!models`, `!relay`, `!bot`, `!whoami`,
  `!reset`, `!cancel-login`.
- **`!status` gained relay version, uptime and agent command**, and now
  reports the conversation and its state directory as optional fields on the
  shared renderer.
- **Sigils `/` and `.` are accepted** alongside `!`, which stays the
  advertised one. Zulip's `/me`, `/poll` and `/todo` are pre-filtered and
  always reach the agent untouched — they are real messages and widgets, not
  client-side slash commands — including mid-login, where a `/poll` must not
  be mistaken for a pasted redirect URL.
- Command names are matched case-insensitively on the verb; the argument keeps
  its case, since a model id is a literal the agent must match.

### Fixed

- `!status` no longer counts **retired** conversations in "active
  conversations". Retired entries stay in the journal as the record of which
  state directories are dead, so the number only ever grew with every `!new`.

### Removed

- **`!id`**, added in v0.5.0. `!status` already prints the conversation id on
  its own line in backticks, which is just as copy-pasteable, so the command
  existed to work around a formatting detail that was not actually a problem.
  Dropping it keeps this relay's surface identical to `poe-acp`'s.
- **`!cancel` as an alias for `!stop`.** `!login cancel` and `!cancel-login`
  own that word, and a `!cancel` that sometimes aborts a login and sometimes
  kills a turn is the worst possible ambiguity in the one command a user
  reaches for when something has gone wrong. `!stop` is the only spelling.

### Added

- `journal.Key.Token` / `journal.ParseToken`: an opaque, round-trippable
  conversation token. The broker identifies a conversation by one string it
  hands back, and it must be the **key**, not the conv-id — `!new` replaces
  the conv-id, so a broker holding one would be holding a stale identity.

## [0.5.0] - 2026-09-01

### Added

- **Relay `!command` surface, in channel topics and DMs alike.** `!help`,
  `!status`, `!id`, `!model [id]`, `!new` (`!reset`) and `!stop` (`!cancel`)
  are handled by the relay itself: they never reach the agent and consume no
  turn, so `!stop` and `!status` still work when the agent is wedged. Replies
  are ordinary messages posted where the command arrived — no placeholder, no
  streaming, no `:eyes:` lifecycle.
- **`!new` (`!reset`) retires a conversation and allocates a fresh conv-id.**
  This is the only way to start over in a **direct message**, whose key is the
  participant set and therefore fixed forever. The retired conversation keeps
  its `state/convs/<id>/` directory — the reply names it — and its recorded
  tail message is cleared so the next turn cannot stream into it. Any turn
  running in the retired conversation is cancelled first. A `!model` choice
  carries over; the history does not.
- **`!model [id]` shows the agent's models or switches this conversation to
  one**, via acp-kit's `AgentProc.SetModel`. The choice is sticky per
  conversation, in memory only, and pushed to the ACP session at the start of
  the next turn — re-applied automatically if idle GC swaps the session out.
- `journal.Journal.Retire` and the `retired` field on a journal entry. A
  retired conversation stays addressable by id but never re-claims its key, so
  a turn still unwinding in it resolves cleanly and the file records which
  state directories are dead. Absent in a pre-`!new` journal, so no version
  bump.
- `zulipproto.Message.RecipientNames`, for rendering DM participants in human
  terms in `!status`.

### Fixed

- **A journal write that fails no longer half-applies.** `Ensure`, `SetTail`,
  `Rename` and the new `Retire` all mutated the in-memory maps before
  attempting to persist, so a failed write returned an error while the relay
  went on behaving as though the change had succeeded — until a restart
  reloaded the untouched file and silently undid it. Every mutation now rolls
  back on a failed write. Most visible on `!new`, which tells the user it
  failed and must therefore actually have failed.

### Changed

- The built-in system prompt tells the agent that `!`-commands are handled by
  the relay and never reach it, so it points users at `!help` instead of
  inventing commands.
- A message that merely *starts* with `!` is still ordinary prose and reaches
  the agent byte-for-byte: only a command-shaped first token (a letter, then
  letters/digits/`_`/`-`) is parsed as a command. `!important: fix this` and
  `!5 minutes` are forwarded unchanged. A command-shaped token naming nothing
  known gets a short error and is not forwarded either. To send prose that
  really does begin with a command name, double the bang — `!!new` arrives as
  `!new`.

## [0.4.0] - 2026-09-01

### Added

- **Direct-message support, opt-in with `"dms": true`.** 1:1 and group DMs with
  the bot each map to their own ACP session. Mention-gating is off in a DM —
  every message there is addressed to the bot by construction — so there is no
  ambient/abstain path; streaming, 10k rollover, `outbox/` attachments and
  interrupted-turn marking all work as they do in a topic. `channels` does not
  gate DMs (a DM is in no channel); `allowed_user_ids` does. `"dms": true` with
  no `channels` is a valid DM-only relay.

### Changed

- The journal is keyed on a typed `journal.Key` expressing both conversation
  shapes — `(stream_id, topic)` for a channel, the sorted participant user-id
  set for a DM — rather than a stringly `(stream_id, topic)` pair. The on-disk
  shape is unchanged for channel conversations and needs no version bump: a DM
  is the entry that carries `user_ids`.
- The event queue is no longer narrowed to a single channel when DMs are
  served: a `/register` channel narrow is a conjunction and would silently
  exclude every DM.

## [0.3.0] - 2026-09-01

### Added

- **`"*"` in `channels` serves every channel the bot is subscribed to, and
  tracks it at runtime.** Adding the bot to a channel starts serving it within
  seconds — no config edit and no restart — and unsubscribing stops it; both
  are logged. The sentinel may stand alone or be mixed with explicit names and
  ids, which stay served regardless of subscription state. An empty `channels`
  list remains a fatal error. The relay registers the `subscription` and
  `stream` event types in that mode, and resyncs the set from
  `GET /users/me/subscriptions` on every queue registration, so the set cannot
  drift silently across the event gap a dead queue leaves behind.

### Changed

- The handler's channel allowlist is now a `ChannelSet` interface consulted per
  event (`internal/channels`), instead of a map snapshotted at boot. Configs
  that list channels explicitly behave exactly as before, including the
  single-channel event-queue narrow.

## [0.2.0] - 2026-09-01

### Added

- **Immediate acknowledgement by emoji reaction.** The instant a message is
  accepted for handling — mentioned *or* ambient — the relay reacts to it with
  `:eyes:` and removes the reaction when the turn ends. Zulip has no typing
  indicator, and a reaction is the only feedback that is instant, costs no
  message in the topic, and can be retracted even when the turn ends in
  silence. Configurable via `ack_emoji`; set it to `""` to disable. Reaction
  failures are logged and never fail a turn, and removal runs on a detached
  context so a cancelled or superseded turn still cleans up. `ack_emoji` is
  validated at load: Zulip's UI writes reactions as `:eyes:` but the API takes
  a bare `emoji_name`, and a colonised value would otherwise fail silently on
  every turn.
- **Early `Thinking…` placeholder on the ambient path.** An ambient turn used
  to post nothing at all until the agent's abstain verdict was in, which on a
  long turn meant minutes of silence. The verdict is knowable much sooner: the
  moment the streamed message text is non-empty and is no longer a prefix of
  the silent sentinel, a reply is certain. A new `sentinelWatch` sink observes
  the stream above acp-kit's `ValidatingSink` and puts the placeholder (and its
  spinner) up at that point. The answer still lands via the normal end-of-turn
  commit, so an abstaining turn still posts nothing.
- `deploy` and `update` skills (`skills/`), modelled on poe-acp's, defining
  the canonical deployment layout: binary in `~/.local/bin`, config/env/state
  under `~/.config/zulip-acp/`, supervision by a systemd user unit, logs to
  journald. The unit **must** set `Environment=PATH=%h/.local/bin:...`: systemd
  user units do not inherit the login shell PATH, so the relay would otherwise
  start, authenticate, resolve channels and then die with
  `exec: "fir": executable file not found in $PATH`. The deploy skill also
  documents the message gating rule (a new topic requires an @-mention;
  already-engaged topics do not) — a non-mention in a fresh topic is dropped
  silently with no log line, which is easily mistaken for a broken relay.
- `packaging/systemd/zulip-acp.service` — ready-to-install unit template.
- `docs/config.example.json`.
- README "Deployment" section.

### Changed

- Removed `BRIEF.md` and `REPORT.md` from the repository and from its whole
  history. They were agent build-session scaffolding — a task brief and a
  status report — not project documentation. Everything durable they recorded
  already lives in `docs/zulip-protocol-reference.md`, `docs/zulip-acp-design.md`
  and `BACKLOG.md`.
- `.gitignore` brought to parity with the sibling relays (`.env`, `.envrc`,
  `*.local.json`, `.DS_Store`, `/.fir/`, `*.test`).
- Skills moved from `.fir/skills/` to **`skills/`**, and `.fir/` is now ignored
  wholesale. Agent configuration is per-deployment, so the repository ships the
  content and each deployment wires it up locally. One line of global fir
  config does it — `{"skills": ["skills"]}` in `~/.config/fir/settings.json`,
  where relative paths resolve against the working directory, so `./skills/`
  is discovered in every project that has one. A `.fir/skills -> ../skills`
  symlink also works. Nothing agent-specific is tracked.

## [0.1.1] - 2026-08-31

### Changed

- The Homebrew tap push now authenticates with an ssh **write deploy key** on
  `kfet/homebrew-ai` (`HOMEBREW_TAP_SSH_KEY`) instead of a personal access
  token. A deploy key is scoped to one repository and does not expire, where a
  fine-grained PAT is account-wide, opaque once created, and capped at a
  one-year lifetime — it breaks silently, later. `skip_upload` keys off the
  new secret, so an unconfigured clone still skips the tap rather than failing
  the release.

### Fixed

- The Homebrew formula's `desc` said "HTTP relay between Poe server bots and
  ACP-speaking agents" — inherited verbatim when the release scaffolding was
  mirrored from `poe-acp`, and about to be published to a user-facing tap for
  the first time.

## [0.1.0] - 2026-08-30

### Added

- Initial release: a relay bridging a self-hosted Zulip server to an
  ACP-speaking coding agent (`fir --mode acp`) over stdio.
- `zulipproto.NarrowChannels`, which encodes two Zulip `/register` traps that
  both fail by silently delivering nothing: the channel operand must be a NAME,
  and narrow terms are a conjunction (so more than one channel cannot be
  narrowed at all — the queue is left unnarrowed and the channel allowlist
  filters).
- `internal/zulipproto`: HTTP Basic API client (send/edit/get messages,
  one-shot multipart uploads, stream resolution) plus the
  `POST /register` + `GET /events` long-poll runner with `last_event_id`
  cursor discipline, `BAD_EVENT_QUEUE_ID` re-registration, heartbeat
  liveness and jittered backoff.
- `internal/rollover`: a pure, surface-agnostic message splitter that
  keeps every posted message under Zulip's 10000 **code point**
  `MAX_MESSAGE_LENGTH` — Zulip truncates silently, so the relay counts
  for itself. Fence-aware (closes and reopens fenced code blocks with
  their language tag across a seal), line-boundary preferring, and
  never re-edits a sealed message.
- `internal/journal`: durable `(stream_id, topic)` → conversation-id
  alias map, so a topic rename migrates the session instead of
  orphaning it, plus the owned tail-message id used for crash backfill.
- `internal/handler`: inbound-event gating, ACP prompt dispatch, and a
  streaming sink that coalesces edits on a ~300ms tick.
- `internal/statusline`: Zulip-markdown renderer for the
  `dev.acp-kit.status-line/v1` mood/plan header.
- Outbox attachments: files the agent leaves in `<cwd>/outbox/` are uploaded
  at the end of the turn and linked from the answer.
- Output rescue: if a message cannot be posted inline — a closed edit window, a
  server hiccup, or Zulip refusing a legal-length body it cannot render — the
  whole transcript is uploaded as `answer.md` and linked, so no output is lost.
- A turn superseded by a follow-up reads as `*(superseded by your next
  message)*` rather than leaking `error: context canceled`.
- On startup any unsealed tail message the relay authored is marked
  `*(relay restarted — turn interrupted)*`.
- `test/live_test.go`: integration tests against a real server (`ZULIP_LIVE=1`)
  pinning silent truncation, code-point counting, edit throughput, upload
  round-trips and event-queue semantics. Excluded from the coverage gate.
- `docs/zulip-acp-design.md` and `docs/zulip-protocol-reference.md`.

[Unreleased]: https://github.com/kfet/zulip-acp/compare/v0.6.0...HEAD
[0.6.0]: https://github.com/kfet/zulip-acp/compare/v0.5.0...v0.6.0
[0.5.0]: https://github.com/kfet/zulip-acp/compare/v0.4.0...v0.5.0
[0.4.0]: https://github.com/kfet/zulip-acp/compare/v0.3.0...v0.4.0
[0.3.0]: https://github.com/kfet/zulip-acp/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/kfet/zulip-acp/compare/v0.1.1...v0.2.0
[0.1.1]: https://github.com/kfet/zulip-acp/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/kfet/zulip-acp/releases/tag/v0.1.0
