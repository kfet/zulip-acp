# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.1.0] - 2026-08-30

### Added

- Initial release: a relay bridging a self-hosted Zulip server to an
  ACP-speaking coding agent (`fir --mode acp`) over stdio.
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
- On startup any unsealed tail message the relay authored is marked
  `*(relay restarted — turn interrupted)*`.
- `test/live_test.go`: integration tests against a real server (`ZULIP_LIVE=1`)
  pinning silent truncation, code-point counting, edit throughput, upload
  round-trips and event-queue semantics. Excluded from the coverage gate.
- `docs/zulip-acp-design.md` and `docs/zulip-protocol-reference.md`.

[Unreleased]: https://github.com/kfet/zulip-acp/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/kfet/zulip-acp/releases/tag/v0.1.0
