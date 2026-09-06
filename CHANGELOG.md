# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.20.1] - 2026-09-06

### Fixed

- **Replacing an unresumable event queue is now lossless.** v0.19.1 refused to
  resume a queue whose registration changed — correctly — but then *deleted it
  and called `/register`*, which hands back the server's **current**
  `last_event_id`: everything posted between the predecessor's last poll and
  that instant was behind the new cursor and never delivered. The swap now
  overlaps instead. The replacement queue is registered **first**, while the old
  one is still alive and still buffering; the old queue is then drained (`GET
  /events` from the inherited cursor until it yields nothing, bounded per poll
  and overall) and its events dispatched in order; only then is it deleted and
  the replacement polled. Event ids are per-queue, so the window both queues saw
  is de-duplicated on event **identity** — message id, `(message id, edit
  timestamp)`, `(message id, user, emoji, op)` — through a bounded 512-entry
  FIFO, armed only for the swap and retired after the first poll of the
  replacement queue (an identity can legitimately recur — a reaction removed
  and re-added — so a permanent dedup set would swallow it). The drain polls
  with **`dont_block=true`** (`Client.DrainEvents`), so "the queue is empty" is
  the server's answer rather than a timeout guess that a slow server would make
  wrong. A reload signal landing mid-drain abandons the swap the *other* way —
  the replacement is deleted and the **old** queue handed to the successor with
  its own registration fingerprint, because it still holds what the drain never
  reached. Only five consecutive `/register` failures, or a drain that exceeds
  its budget, fall back to the old lossy behaviour, and both log the gap
  loudly. See `docs/graceful-reload.md`.

- **A long reload drain no longer loses the event queue to the server's
  garbage collector.** `-reload-drain-deadline` is 30 minutes and a Zulip queue
  is collected after ~10 minutes untouched, so a long turn silently cost the
  handoff its queue. The relay now registers with `queue_lifespan_secs` =
  drain deadline + 5m. Polling cannot substitute for this: Zulip refreshes a
  queue's clock only in `connect_handler`, which runs only when a poll actually
  blocks. A server that ignores the parameter says so in
  `ignored_parameters_unsupported`, and that is now warned about instead of
  assumed away.

## [0.20.0] - 2026-09-06

### Added

- **`rename_topic` — the agent names its own topic.** `autotopic_channels`
  moves a general-chat message into a topic of its own before answering it,
  but the name it picks has always been a pure heuristic over the raw
  markdown: the opening line, stripped and truncated. That is the *question*,
  verbatim — typos and all — and it never was a title, because at that instant
  nothing had read the message. The seam for an agent-generated name has been
  open in `internal/autotopic` since the feature shipped; this closes it.

  With `relay_mcp` on, the turn that opens an auto-named topic is told so, and
  the agent replaces the placeholder through a new Zulip-specific loopback
  tool. The rename is **deferred to the end of the turn**, like `new_session`
  and for a Zulip-shaped version of the same reason: a turn posts into the
  topic it started in — placeholder, streaming edits, rollovers, repost — so a
  topic that moved underneath it would split its own answer between two
  topics. On the wire it is `propagate_mode=change_all`, so the whole topic
  travels and the conversation's session follows it; the journal is migrated
  inline rather than waiting for the `update_message` echo, which closes the
  window in which a message in the new topic would allocate a second
  conversation. The arm belongs to the *turn*, not the conversation: a
  superseded turn unwinds while its replacement is already streaming into the
  old topic name, and it must not move the topic out from under it. Renaming
  onto a topic that already holds a conversation is refused rather than
  attempted — Zulip would merge them and the journal would orphan one session —
  and the anchor message is read back before the move, so a human who moved it
  elsewhere mid-turn cannot make the relay rename an unrelated topic. A rename
  refused by realm policy is logged and the topic keeps its placeholder name — a turn that answered correctly is never failed over a
  topic name. A scheduled turn cannot rename: a rename is a message edit, and
  a scheduled turn has no triggering message to anchor one on.

## [0.19.1] - 2026-09-06

### Fixed

- **A graceful reload no longer resumes an event queue that cannot carry the
  events the new image wants.** A Zulip queue's `event_types` and `narrow` are
  frozen at `/register`, and the reload cursor was only
  `(queue_id, last_event_id)` — so every reload resumed the *predecessor's*
  registration, forever, until a hard restart. On v0.18.1 that made emoji
  reaction delivery silently dead in production: the queue predated the
  feature, so no `reaction` event could ever reach it. The cursor now carries a
  registration fingerprint (`zulipproto.RegistrationFingerprint`, canonical
  JSON of the sorted event types + narrow) through
  `ZULIP_ACP_QUEUE_REGISTRATION`, and a successor that wants a different
  registration deletes the stale queue and registers fresh, with a WARN naming
  the difference. An absent fingerprint counts as different — otherwise the
  very reload installing this fix would resume the broken queue. A fresh
  registration loses whatever was posted before it; that cost is stated in the
  log line and in `docs/graceful-reload.md`.

## [0.19.0] - 2026-09-06

### Added

- **`zulip-acp update` — self-update.** Resolves the latest (or a pinned)
  release through the GitHub **API**, verifies the asset against
  `checksums.txt`, and renames it over the running binary from a temp dir
  beside it: the `ETXTBSY`-safe atomic swap, in the binary, where it belongs.
  `--check` reports without installing, `--version` pins, `--repo` overrides
  the source, `--restart-cmd` recycles afterwards (prefer
  `systemctl --user reload zulip-acp`). It refuses to replace a
  package-manager-managed install and names the command to use instead. The
  API path matters here specifically: while `kfet/zulip-acp` is private the
  plain release-download URL and `brew install` both 404 on the asset, but the
  API serves it to a token (`GITHUB_TOKEN`, `GH_TOKEN`, or a logged-in `gh`).
  It also refuses an install directory owned by another user — the swap needs
  to write there, and a distro-packaged binary is not ours to move. New
  `internal/selfupdate`.
- **`scripts/converge.sh` + `dist.lock` + `bots/<name>.json` — fleet
  convergence**, ported from `poe-acp` and adapted to this relay's shape.
  `--tot` resolves latest-of-everything into the lock (and never converges);
  `<bot> --apply` makes one host match the lock: binary, `fir`, fir
  extensions, `config.json`, the systemd unit, then the recycle. **Converge is
  the only sanctioned way to touch a fleet host.** It reads the version of the
  image the RUNNING process executes (`/proc/<MainPID>/exe`), not the on-disk
  file, so a binary swapped but never recycled is reported as stale; it selects
  the graceful reload only when that live image can actually handle SIGHUP
  (>= 0.12.0, unit has `ExecReload`, unit unchanged, service up) and otherwise
  says why it must hard-restart. On the host it drives `zulip-acp update`
  rather than fetching the asset itself. poe-acp's master/worker supervisor
  model is deliberately NOT ported: a reload here holds the PID and re-execs in
  place, so the verification is "pid held, running image moved", and a reload
  still draining an in-flight turn is reported as pending, not as a failure.
  Offline tests in `test/converge_render.sh` (`make test-scripts`, part of
  `make all`) cover the renderers, the recycle matrix, and both mechanisms
  against a stubbed systemd and `/proc`. `--apply` refuses a host whose
  `EnvironmentFile` is missing before it writes anything, and says out loud
  that a hard restart drops in-flight turns and the messages behind them.

### Changed

- **The bundled `update` skill no longer tells anyone to hand-place a binary.**
  The documented order is now: converge for a host with a `bots/` spec →
  `zulip-acp update` everywhere else → `make deploy` / `brew` as fallbacks for
  an unreleased build or a brew-managed install. The stage-and-`mv -f` dance
  (and its matching pitfall bullet) is gone; `Text file busy` now points at the
  subcommand that solves it. `skills/deploy/SKILL.md`, `README.md` and
  `AGENTS.md` say the same thing.

## [0.18.1] - 2026-09-06

### Fixed

- **A latent test race turned into a CI failure.**
  `TestAmbientPlaceholderGoesUpBeforeTheTurnEnds` watched the Zulip surface for
  the ambient placeholder and then asserted the journal tail, but posting and
  recording the tail are two steps and only the first is observable there. The
  window widened in 0.18.0 (`trackTail` now also indexes the message id for the
  reaction path), and CI caught it. The handler gained an `OnEarlyPlaceholder`
  test hook that fires once BOTH steps are done, so the assertion no longer
  races the goroutine it is about.

## [0.18.0] - 2026-09-06

### Added

- **Emoji reaction REMOVALS are delivered too.** Un-reacting is real signal — an
  approval withdrawn, a trigger retracted — and reads as `removed :x: from …`
  against the add's `added :x: to …`. Same gates, same silence-by-default norm.
- **Reaction bursts are coalesced into one turn.** Reactions are buffered per
  conversation for `reactionDebounce` (4s) and delivered together as
  `[reactions] N in this conversation:` with one line each, capped at 20 lines
  plus a count of the rest. Ten people reacting to the same message now costs
  one agent turn instead of ten, which is what makes `"reactions"` affordable as
  a default-on feature. A reaction arriving while a turn is running — or while
  the flush waits for it — is folded into the same delivery rather than
  cancelling the turn or being dropped; the wait and the claim are one critical
  section (`claimConvIdle`, shared with scheduled prompts). Coalescing applies
  to reactions ONLY: inbound human messages are never delayed, and a message
  burst still supersedes turn-by-turn.

### Changed

- `"reactions"` is documented as default-on with opting **out** as the
  deliberate act, now that a pile-on costs one turn.

### Fixed

- **A queued turn was charged for its own wait.** A scheduled prompt — and now a
  coalesced reaction burst — waits for the conversation to go idle, but its
  `PromptTimeout` context was created *before* that wait. Behind a human turn
  longer than `prompt_timeout_seconds` (exactly when reactions pile up) the turn
  started already expired and posted `*error: context deadline exceeded*` into
  the topic. The clock now starts when the conversation is claimed.
- Buffered reactions are dropped when polling stops (`SIGHUP`/`SIGTERM`), so a
  debounce cannot expire mid-drain and start a turn the re-exec kills; a flush
  that wakes to an emptied buffer releases the conversation instead of prompting
  the agent with nothing. Documented in `docs/graceful-reload.md`.

## [0.17.1] - 2026-09-06

### Changed

- **The injected system prompt now sets the reaction norm explicitly.** Which
  reactions deserve a reply is the agent's judgement and never a relay-side
  heuristic — the relay delivers every gate-passing reaction faithfully — so the
  built-in block spells out the posture instead of hinting at it: a reaction is
  ambient signal and not a request, most deserve no reply at all, emitting the
  silent sentinel is the normal and expected outcome rather than a failure, a
  reply is warranted only when the reaction plainly changes something or plainly
  asks for something (a rejection on a proposal just made, an agreed trigger
  emoji), and a "thanks!"-style acknowledgement is never sent. With no sentinel
  configured the agent cannot decline, so the instruction asks for the shortest
  possible reply instead of demanding something impossible.

## [0.17.0] - 2026-09-06

### Added

- **Emoji reactions reach the agent** (`"reactions"`, default **on**). A
  reaction on a message in a conversation the relay is already engaged in is
  delivered as one compact ambient turn — `[reaction] Ada Lovelace added
  :tada: to your own message 1234 ("…")` — which the agent may decline with the
  silent sentinel, and the built-in system prompt says silence is the normal
  answer. Gating, in order and all mandatory: `op` must be `add`; the relay's
  own reactions (`ack_emoji`, the `!opts` tick) and other bots' are dropped
  before any allowlist; `allowed_user_ids` applies; the message must resolve to
  an **already-engaged** conversation, so a reaction can never create one or
  summon the bot. Resolution is an in-memory index of the relay's own messages
  first (no API call), then the journal's recorded ids, then one
  rate-limited, negatively-cached `GET /messages/{id}` — because Zulip's
  `/register` narrow does **not** filter `reaction` events (measured: a queue
  narrowed to one channel still receives reactions from every channel the bot
  can see). A reaction never supersedes a running turn, and a bot that
  appeared after startup is caught by the same user lookup that names the
  reactor.
- `zulipproto`: the `reaction` event type and its fields (`op`, `user_id`,
  `message_id`, `emoji_name`, `emoji_code`, `reaction_type`), plus
  `Client.UserByID` — a reaction event carries only ids, so naming the reactor
  needs a lookup. Both documented with the live-measured payload in
  `docs/zulip-protocol-reference.md`.

### Fixed

- A `Runner` with `Handoff` armed now waits for its watcher goroutine before
  returning. Whether that goroutine ever reached its `pollCtx.Done()` branch
  was previously a scheduling race, which made the 100% coverage gate fail at
  random; it is now deterministic, and the goroutine can no longer outlive
  `Run`.

## [0.16.3] - 2026-09-06

### Fixed

- **Autotopic never fired in a channel whose general chat was already
  engaged.** The move ran inside `if !engaged`, so a conversation the journal
  already held for the `(stream, "general chat")` key — a legacy one from
  before the feature shipped, as in `#ask-fir`, or one a failed move had just
  left behind — routed every later general-chat message straight past the move,
  disabling the feature in that channel permanently. A single transient API
  error was enough. General chat in an autotopic channel is now a **lobby**,
  never a conversation: the journal entry under the lobby key is not
  engagement, the move runs on every general-chat message, and the lookup is
  done against the FINAL key. The pre-existing lobby conversation is left
  untouched — it simply stops receiving new messages. The gates that must
  precede a move (channel allowlist, `AllowedUsers`, command dispatch) still
  do, and an unaddressed general-chat message in a non-ambient channel is still
  not moved.

## [0.16.2] - 2026-09-06

### Fixed

- **Every attachment was served as `application/octet-stream`.**
  `Client.Upload` built its multipart part with `CreateFormFile`, which
  hardcodes that type; Zulip stores the declared type verbatim and serves the
  file back with it, so nothing the relay uploaded ever previewed inline — a
  `.log` or `.png` was a forced download, which is worst on mobile. `Upload`
  now takes an explicit `contentType`, and new `zulipproto.ContentType`
  resolves it: an explicit text-extension table first (determinism — otherwise
  the answer comes from the host's `/etc/mime.types`), then
  `mime.TypeByExtension`, then `http.DetectContentType` over the first 512
  bytes, with every `text/*` flattened to `text/plain; charset=utf-8` because
  that is what Zulip's inline allowlist renders. Declared types are normalised
  through `mime.ParseMediaType` + `FormatMediaType` before reaching the
  header, so a caller cannot inject one.

### Changed

- The built-in system prompt now tells the agent that an outbox file's
  extension decides how Zulip serves it. It is the only lever the agent has
  over the attachment's content type.

## [0.16.1] - 2026-09-06

### Fixed

- **`autotopic_channels` was dead code against a real server.** The general-chat
  test was `topic == ""`, but Zulip only sends the empty string for the empty
  topic to clients that declare the `empty_topic_name` client capability in
  `POST /register`; to everyone else it substitutes the translated display
  name. Measured on Zulip 12.2 (feature level 500), `GET /messages` returns
  `"subject": "general chat"`, so the check never fired. New
  `autotopic.IsGeneralChat` recognises both spellings (trimmed,
  case-insensitive) and is used ONLY in `Handler.autotopic` — the capability is
  deliberately not declared, because it would flip every empty-topic
  conversation's journal key at once and orphan live sessions. Every other
  topic comparison, and the journal key itself, is unchanged. `autotopic.NameAt`
  also refuses to generate the name `general chat` for the same reason it never
  generated `""`: it would mean "did not move". Note the display name is
  translated and only the English one is matched, so a non-English realm still
  needs the capability route.

## [0.16.0] - 2026-09-05

### Added

- **`autotopic_channels`: general chat gets a topic of its own.** Zulip 11's
  "general chat" is literally the empty topic, so a conversation held there
  buries itself in the channel's one shared feed. In a channel listed in the
  new `autotopic_channels` key, an accepted general-chat message is MOVED to a
  topic generated from its text (`PATCH /messages/{id}` with
  `propagate_mode=change_one`) and the conversation is opened there. The move
  happens before the conversation is allocated, so no journal migration is
  involved, and any failure — realm
  `can_move_messages_between_topics_group`, an older server — is logged and
  answered in place, never dropped. A generated name that would collide with
  an existing conversation gets the message id appended, so two people opening
  general chat with the same words never share one session (`config`:
  `AutotopicChannels` + `ResolveAutotopic`; `channels.Set`: static autotopic id
  set + `Autotopic(id)`; `zulipproto`: `MoveMessage` + `MaxTopicLength`, which
  Zulip enforces by silent truncation exactly as it does `MAX_MESSAGE_LENGTH`;
  new `internal/autotopic`: pure heuristic namer, first usable line, mentions
  and markdown stripped, word-boundary truncation to 60 code points,
  `chat <timestamp>` fallback).

## [0.15.0] - 2026-09-04

First release carrying **both** lines of work that were briefly cut as two
different `v0.14.0`s. The published `v0.14.0` (italic status footer + model
short name, acp-kit v0.10.0) is unchanged and is included below it in this
file; `ambient_channels`, which had only ever existed in an unpushed local
`v0.14.0`, is released here for the first time.

### Added

- **`ambient_channels`: engage a channel without an @-mention.** In a channel
  the relay only wakes when @-mentioned or when the topic is already engaged,
  so the opening bare message of a fresh topic was dropped before any session
  existed and a reply-to-everything `system_prompt` never applied. Channels
  listed in the new `ambient_channels` config key behave like DMs: every
  message is addressed, so the first message of a new topic summons the relay
  with no mention. Names or ids, resolved with the same rules as `channels`;
  an empty list is fine and means "mention-gate everywhere" (`config`:
  `AmbientChannels` + `ResolveAmbient`; `channels.Set`: static ambient id set
  + `Ambient(id)` predicate, keyed by id so a rename cannot drop it;
  `handler`: `addressed = Channels.Ambient(stream) || mentioned(text)`).

### Changed

- The status line is an italic footer naming the model — see [0.14.0] below.
  It reached this host's line of history only in this release.

## [0.14.0] - 2026-09-04

### Changed

- **The status line moved from a header to an italic footer, and now names the
  model.** It is appended once at the END of the answer
  (`\n\n*🏛️ opus-4.5 • steady • 2/5*`) instead of being prepended to the first
  user-visible chunk. `mood` and `plan` are agent-supplied and normally arrive
  mid-turn, so a header rendered on the first chunk showed a status the agent
  had not published yet — usually the emoji alone; the footer is rendered from
  the final snapshot. It is suppressed on turns that produced no user-visible
  content and on error turns, it goes on before `split.Close` flushes so the
  end-of-turn repost carries it exactly once, and the live `Thinking…`
  placeholder gains the model too.
- `streamingSink.SetProviderEmoji(emoji)` widened to
  `SetModelInfo(emoji, model)`; the model identity is resolved a second time
  after `applyModel`, so the line names the model that actually served the turn.
- acp-kit bumped to v0.10.0 for `statusline.ShortModelName` /
  `statusline.Status.Model`.

## [0.13.0] - 2026-09-03

### Added

- **Push notifications carry the answer, not `Thinking...`.** Zulip generates a
  mobile notification when a message is CREATED and never when one is edited,
  so every push showed the eager streaming placeholder. A finished streamed
  turn now re-posts its whole message chain as new messages and deletes the
  originals (`rollover.Splitter.Repost`). New copies go up *before* any old one
  comes down, so output can never be lost; a refused delete trips a
  process-wide circuit breaker that disables reposting (logged once) rather
  than doubling every future turn. New `repost_on_close` config key, default
  `true`.

## [0.12.1] - 2026-09-03

### Changed

- `BACKLOG.md`: recorded why `poe-acp`'s master/worker supervisor is **not**
  being promoted to `acp-kit` to give this relay a zero-latency upgrade. The
  blocker is not the supervisor (though `Config.Addr` and the inherited
  listener fd, which doubles as the worker discriminant, are less generic than
  its package doc claims) — it is that two workers cannot share a conversation
  without breaking the follow-up-supersedes guarantee in `cancelInflight`.
  Promotion needs a measured drain tail, not symmetry with poe-acp.

## [0.12.0] - 2026-09-02

### Added

- Graceful reload: `systemctl --user reload zulip-acp` (SIGHUP) now stops
  polling **without deleting the Zulip event queue**, drains the in-flight
  agent turns, and re-execs the on-disk binary in place — same PID, cursor
  carried forward in `ZULIP_ACP_QUEUE_ID` / `ZULIP_ACP_LAST_EVENT_ID`. Nothing
  posted during the window is lost and nothing is delivered twice, and an agent
  hosted by the relay can reload **inline, mid-turn**, and still finish its
  reply. New `internal/reload`; see `docs/graceful-reload.md`.
- `zulipproto.RunnerConfig.Handoff` / `ResumeQueueID` / `ResumeLastEventID` and
  `Runner.Cursor`: stop the loop leaving the queue alive, and resume an
  inherited queue instead of registering a fresh one. `OnRegister` still fires
  once on resume, so a followed-channel set is resynced.
- `-reload-drain-deadline` (30m) and `-drain-deadline` (30s) bound the reload
  and stop drains.
- Live test `TestEventQueueSurvivesReExec`: proves against a real server, across
  an actual `syscall.Exec`, that a resumed queue still delivers a message posted
  during the window while a freshly registered one never sees it.
- `packaging/systemd/zulip-acp.service` gained `ExecReload` and
  `TimeoutStopSec`.

### Security

- The reload cursor is scrubbed from the ACP agent's environment
  (`reload.AgentEnvNames`, wired through `Config.AgentClientConfig` beside the
  bot API key). A live `queue_id` is a relay capability, not configuration:
  combined with any credential it lets its holder poll the relay's own event
  queue and take delivery of messages meant for the relay — and the agent is
  driven by text from people who are not the operator.

### Fixed

- A `SIGTERM` arriving during a reload drain now wins: the re-exec is abandoned,
  the remaining turns get the short stop budget and the event queue is torn
  down. Previously `systemctl stop` issued during a reload was ignored for up to
  `-reload-drain-deadline` (30m) until systemd SIGKILLed the cgroup.
- A second `SIGHUP` arriving during the (invisible, minutes-long) drain no
  longer risks killing the new image: `signal.Ignore` is armed before the exec,
  and `SIG_IGN` is inherited across `execve`, so a HUP delivered mid-exec is
  dropped instead of hitting the default action before the new image has
  installed its handler.
- A `SIGTERM` arriving mid-reload while nothing was in flight would still
  re-exec, so `systemctl stop` on an idle relay restarted it in place and was
  then SIGKILLed at `TimeoutStopSec`. Pre-emption is now decided by the signal
  context, not by whether the drain came out clean.
- A reload that finds no live queue to hand on (it had just expired) now logs a
  WARN saying the new image will register fresh and miss messages, instead of
  degrading silently.

### Changed

- The bundled `update` skill now teaches `reload` as the default verb. The
  `systemd-run --user --collect --on-active=N` transient-unit workaround and the
  surrounding `setsid` discussion are **deleted**: they existed only because a
  hard restart killed the agent that requested it, and `setsid` never escaped
  the cgroup anyway. Hard restart is kept for a unit-file change, a stopped
  service, and the first cutover onto a reload-capable build.

## [0.11.0] - 2026-09-02

### Added

- `!opts`: an interactive options panel. One live control message per
  conversation — Zulip `zform` buttons in the web app, an equivalent plain
  markdown command list everywhere else (the phone included), with graceful
  degradation when a server refuses `widget_content`. Model buttons are drawn
  from the agent's own probe, so a button can never offer a model the agent
  does not have. Because Zulip refuses every content edit on a message carrying
  a widget ("Widgets cannot be edited.", measured on 12.2), the panel updates by
  being re-posted with the old one deleted — the only message this relay ever
  deletes — falling back to a pointer line, and then to leaving it, when the
  realm forbids that.
- `!model <exact-id>` is now acknowledged with an emoji reaction instead of a
  reply message: configuration chatter stays out of the topic, and out of the
  transcript the model reads. Every model change — typed, tapped, or made by the
  agent through its loopback tool — refreshes the panel from one choke point.
- `zulipproto.DeleteMessage`, `zulipproto.RejectedByServer` (tells a 4xx
  refusal from a transport failure, so only a refusal is retried differently)
  and `zulipproto.IsMissing`.
- Live test `TestWidgetMessageCannotBeEdited`, and a
  `docs/zulip-protocol-reference.md` section on widgets — the edit refusal fails
  in the cruel direction (the degraded, widget-less path edits fine), so it is
  pinned as evidence rather than left as a comment.
- The journal records each conversation's panel message id (`opts_id`), and
  carries it across `!new` — the panel belongs to the place, not the session.

### Changed

- An unknown `!command` now answers with the options panel rather than a
  one-line error. The failure mode teaches; it is still never forwarded to the
  agent.
- `!help` lists `!opts`, appended by the relay since the shared `acp-kit`
  broker cannot know about a Zulip-only surface.
- The built-in system prompt names `!opts` among the relay commands.

### Fixed

- A turn's deferred loopback actions (`new_session`) run after the turn leaves
  the inflight map, so `WaitIdle` could return while the turn was still writing
  the journal — which raced the test harness's own temp-dir cleanup. `Handler`
  now offers an `OnTurnEnd` hook (nil in production) that makes the window
  observable, and the loopback tests wait on it.

## [0.10.0] - 2026-09-02

### Added

- Loopback `history` tool: with `relay_mcp` on, the agent can read its own
  conversation's earlier messages — oldest first, raw markdown, its own past
  replies included — instead of shelling out to the Zulip API with the bot's
  credentials after a session reset. Channel topics and DMs both, identity
  resolved server-side from the connection token, output bounded per message
  and in total with a `before_id` anchor to page further back.
- `zulipproto.Messages` with typed narrow terms (`TopicNarrow`, `DMNarrow`)
  and exclusive `before_id` paging.

### Removed

- `zulipproto.TopicMessages`, superseded by `Messages` + `TopicNarrow`. It had
  no production caller.

## [0.9.0] - 2026-09-02

### Added

- Builtin `update` skill: the update/restart procedure now ships **inside the
  binary** and is injected into every session's catalog, so a relay can be asked
  to update itself. Moved from `skills/update/` and corrected — see below.

### Changed

- The built-in system prompt now teaches Zulip's **actual** markdown dialect
  rather than "CommonMark-flavoured". Verified against
  `/api/v1/messages/render`: underscore emphasis (`_x_`, `__x__`) does not
  render, a single newline is a hard line break, nested lists require exactly
  two spaces (four emits a literal `- `), and headings, spoilers, `$$KaTeX$$`,
  mentions, channel/topic links and `<time:…>` are all available.
- The `update` skill's claim that a hard restart is redriven by Zulip was
  **wrong** and is removed. `queue_id`/`last_event_id` are in-memory by design,
  so a restarting relay registers a fresh queue at the server's current
  `last_event_id`: the message that triggered an in-flight turn is behind that
  cursor and is never re-delivered. The skill now documents the one sanctioned
  way for an agent to restart the relay hosting it — a `systemd-run --user
  --on-active=N` transient unit, which lands in its own cgroup and so survives
  the teardown that kills `setsid`.

## [0.8.0] - 2026-09-02

### Added

- Skills catalog injection, ported from poe-acp. The relay now merges an
  embedded builtin bundle (`internal/skills/bundle/`) with a host directory at
  `<config-dir>/skills/` and injects a fir-style `<available_skills>` block into
  every session's system prompt. Host skills override same-named builtins, which
  doubles as the disable mechanism.
- Builtin `notes` skill: points the agent at `~/.local/state/zulip-acp/notes/`
  as persistent cross-conversation scratch, and documents `notes/fleet/` as the
  Syncthing-shared fleet-wide subdirectory.

### Changed

- The durable system prompt is now supplied via `state.SystemPromptProvider`
  rather than a fixed string, so a skill added to the host dir after startup is
  visible to the next session without a relay restart. Builtins are extracted
  once per process; `disable_system_prompt` short-circuits before any skill dir
  is read. `system_prompt` and `disable_system_prompt` behave exactly as before.

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

[Unreleased]: https://github.com/kfet/zulip-acp/compare/v0.20.0...HEAD
[0.20.0]: https://github.com/kfet/zulip-acp/compare/v0.19.1...v0.20.0
[0.19.1]: https://github.com/kfet/zulip-acp/compare/v0.19.0...v0.19.1
[0.19.0]: https://github.com/kfet/zulip-acp/compare/v0.18.1...v0.19.0
[0.18.1]: https://github.com/kfet/zulip-acp/compare/v0.18.0...v0.18.1
[0.18.0]: https://github.com/kfet/zulip-acp/compare/v0.17.1...v0.18.0
[0.17.1]: https://github.com/kfet/zulip-acp/compare/v0.17.0...v0.17.1
[0.17.0]: https://github.com/kfet/zulip-acp/compare/v0.16.3...v0.17.0
[0.16.3]: https://github.com/kfet/zulip-acp/compare/v0.16.2...v0.16.3
[0.16.2]: https://github.com/kfet/zulip-acp/compare/v0.16.1...v0.16.2
[0.16.1]: https://github.com/kfet/zulip-acp/compare/v0.16.0...v0.16.1
[0.16.0]: https://github.com/kfet/zulip-acp/compare/v0.15.0...v0.16.0
[0.15.0]: https://github.com/kfet/zulip-acp/compare/v0.14.0...v0.15.0
[0.14.0]: https://github.com/kfet/zulip-acp/compare/v0.13.0...v0.14.0
[0.13.0]: https://github.com/kfet/zulip-acp/compare/v0.12.1...v0.13.0
[0.12.1]: https://github.com/kfet/zulip-acp/compare/v0.12.0...v0.12.1
[0.12.0]: https://github.com/kfet/zulip-acp/compare/v0.11.0...v0.12.0
[0.11.0]: https://github.com/kfet/zulip-acp/compare/v0.10.0...v0.11.0
[0.10.0]: https://github.com/kfet/zulip-acp/compare/v0.9.0...v0.10.0
[0.9.0]: https://github.com/kfet/zulip-acp/compare/v0.8.0...v0.9.0
[0.8.0]: https://github.com/kfet/zulip-acp/compare/v0.7.0...v0.8.0
[0.7.0]: https://github.com/kfet/zulip-acp/compare/v0.6.0...v0.7.0
[0.6.0]: https://github.com/kfet/zulip-acp/compare/v0.5.0...v0.6.0
[0.5.0]: https://github.com/kfet/zulip-acp/compare/v0.4.0...v0.5.0
[0.4.0]: https://github.com/kfet/zulip-acp/compare/v0.3.0...v0.4.0
[0.3.0]: https://github.com/kfet/zulip-acp/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/kfet/zulip-acp/compare/v0.1.1...v0.2.0
[0.1.1]: https://github.com/kfet/zulip-acp/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/kfet/zulip-acp/releases/tag/v0.1.0
