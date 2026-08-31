# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- **Immediate acknowledgement by emoji reaction.** The instant a message is
  accepted for handling — mentioned *or* ambient — the relay reacts to it with
  `:eyes:` and removes the reaction when the turn ends. Zulip has no typing
  indicator, and a reaction is the only feedback that is instant, costs no
  message in the topic, and can be retracted even when the turn ends in
  silence. Configurable via `ack_emoji`; set it to `""` to disable. Reaction
  failures are logged and never fail a turn, and removal runs on a detached
  context so a cancelled or superseded turn still cleans up.
- **Early `Thinking…` placeholder on the ambient path.** An ambient turn used
  to post nothing at all until the agent's abstain verdict was in, which on a
  long turn meant minutes of silence. The verdict is knowable much sooner: the
  moment the streamed message text is non-empty and is no longer a prefix of
  the silent sentinel, a reply is certain. A new `sentinelWatch` sink observes
  the stream above acp-kit's `ValidatingSink` and puts the placeholder (and its
  spinner) up at that point. The answer still lands via the normal end-of-turn
  commit, so an abstaining turn still posts nothing.
- `deploy` and `update` skills (`.fir/skills/`), modelled on poe-acp's, defining
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

[Unreleased]: https://github.com/kfet/zulip-acp/compare/v0.1.1...HEAD
[0.1.1]: https://github.com/kfet/zulip-acp/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/kfet/zulip-acp/releases/tag/v0.1.0
