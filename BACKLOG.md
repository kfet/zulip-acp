# Backlog

Things deliberately not done in v1, with the reason.

## acp-kit promotion candidates

- **`internal/rollover` → `acp-kit/chunker`.** The splitter is written with
  zero Zulip imports precisely so it can move. It is not promoted in v1: a
  new, unproven design earns its API in one consumer first. Promote once a
  second surface with a hard message ceiling needs it (Matrix, Discord,
  SMS-style gateways) — at which point the `Poster` interface and the
  seal/continuation markers become the negotiated API, not an accident of
  Zulip's 10k limit.

- **The `sink` + streaming layers are being written for the third time.**
  `poe-acp`, `slack-acp` and now `zulip-acp` each carry a near-identical
  `streamingSink` (render an `acp.SessionNotification` into surface text,
  suppress thoughts on `hide_thinking`, cache `statusline` `_meta`, prepend a
  header once on the first user-visible chunk). Three copies is the signal:
  this belongs in acp-kit as a renderer with a per-surface markup strategy.
  Recorded, not done — the brief for v1 is explicit that the third copy is
  the *evidence*, and the extraction is its own change with its own tests.

## Deferred features

- **`~/.zuliprc` support.** Zulip's own tooling reads an ini file with
  `email` / `key` / `site`. We accept config JSON + env only. Cheap to add.
- **Direct messages.** v1 answers in channel topics only. DMs need a
  different conv-key shape (a user-id set, not `(stream_id, topic)`) and a
  separate gating decision.
- **Startup model probe.** `slack-acp` probes the agent at boot purely so the
  provider emoji can render on turn one. Cosmetic; we resolve the emoji
  lazily from `agent.Models()` instead and skip ~150 lines of
  retry/backoff code that would need 100% coverage.
- **Attachment *inbound* handling.** The relay uploads agent-produced files
  and interpolates the `/user_uploads/...` URL. Fetching user-attached files
  back down and handing them to the agent as `acp-kit/attachments` blocks is
  a natural next step.
- **`installsvc` / `init` wizard subcommands.** `slack-acp` has both. They
  are accretions, not core; a systemd unit is documented in the README.
- **Self-drive escape hatch.** `slack-acp`'s sentinel-gated hatch that lets
  the bot act on its own messages. Deliberately absent: it reopens the
  bot-message boundary, and there is no demand for it here yet.
- **Mid-line split fidelity for inline code spans.** The splitter tracks
  fenced-block state but not single-backtick spans. A mid-line split (only
  possible when one line exceeds the whole budget) can break a span. Cosmetic
  and rare; documented rather than fixed.

## Known limits, accepted

- **Bot senders are snapshotted at startup.** `GET /users` is read once to
  learn which realm users are bots, so a bot created while the relay is running
  is not recognised until it restarts. The bots that actually post unprompted —
  Zulip's cross-realm system bots — are caught by their sender realm instead,
  so the practical gap is narrow.
- **Zulip can refuse a legal-length message.** ~1000 consecutive emoji trips
  the server-side renderer with HTTP 400 "Unable to render message" even though
  the body is far under 10000 code points (9000 CJK characters render fine).
  The relay does not try to be clever about it: it uploads the whole answer as
  `answer.md` and posts a link, so no output is lost. Pinned by
  `test/live_test.go:TestRenderCanFailBelowTheLengthLimit`.

## Operational

- **Mobile push notifications are OFF.** Self-hosted Zulip cannot push to iOS
  without registering with Zulip's Mobile Push Notification Service, which
  sees notification metadata (sender/channel/topic/volume). That is an
  undecided privacy trade-off for the operator; the relay takes no position
  and the server is not registered.
