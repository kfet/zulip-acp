# Backlog

Things deliberately not done in v1, with the reason.

## acp-kit promotion candidates

- **`ValidatingSink.Release` — live streaming on the ambient path ("Tier 2").**
  `zulip-acp` now posts a `Thinking…` placeholder as soon as the streamed text
  diverges from the silent sentinel (`handler.sentinelWatch`), but the answer
  itself still lands in one lump at the end of the turn, because
  `ValidatingSink` buffers every `AgentMessageChunk` until `Commit`. The fix
  belongs in acp-kit, not here: add a `Release(ctx)` that flushes what is
  buffered and switches the sink to pass-through, so a caller that has proved
  abstention impossible can stream the rest live. `PromptAbstainable` would
  then treat a released sink as "answered". Every relay gets it at once.
  Deliberately out of scope of the change that added the placeholder.

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
  Recorded, not done — the v1 design is explicit that the third copy is
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

## Recorded from the v1 design, not built

- **Relay-initiated session naming.** The v1 design prescribes `<slug> · <4-char-id>`
  for topics the *relay* creates. v1 has no code path that starts a topic — the
  human always names it — so the rule has nothing to apply to. It is recorded
  here so that whenever a relay-initiated path appears (a scheduled digest, an
  agent proactively opening a session) it uses that convention rather than
  inventing another.

## Known limits, accepted

- **Messages posted while the event queue is dead are never seen.** Between a
  queue expiring (`BAD_EVENT_QUEUE_ID`, server restart, idle GC) and the
  re-register, anything posted is missed: the new queue starts at its own
  `last_event_id` and the relay never learns those messages existed. This is
  exactly the cursor discipline the v1 design prescribes, and persisting the old
  cursor would not help — the queue backing it is gone. A future backfill has an
  anchor if it wants one: `/register` returns `max_message_id`, so a relay could
  diff it against the last id it processed and replay the gap through
  `/messages`. Not done: it needs a dedup rule and a bound on how much history
  to replay, and neither is obvious.

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

- **`brew install kfet/ai/zulip-acp` will 404 while this repo is private.** The
  tap push now works (v0.1.1's formula is live on `kfet/homebrew-ai`, which is
  public, with sha256s matching the published assets), but the `url` lines it
  contains point at release assets in `kfet/zulip-acp`, and **release-asset
  downloads from a private repo require authentication**. Verified: an
  unauthenticated fetch of
  `…/zulip-acp/releases/download/v0.1.1/zulip-acp-darwin-arm64` returns **HTTP
  404**, where the equivalent `poe-acp` URL (public repo) returns 200.

  So the formula is correct and the plumbing is proven, but it is not yet
  installable. Two ways out, both the operator's call:
  - **Make `kfet/zulip-acp` public**, like `poe-acp` and `slack-acp` already
    are. Nothing further changes; the formula starts working immediately.
  - **Keep it private** and have installers export a `HOMEBREW_GITHUB_API_TOKEN`
    with read access. That works but is not "brew install and go", and the
    formula sitting in a public tap advertises a download nobody else can
    fetch.

  Not decided here: repository visibility is not a change to make on someone's
  behalf.

- **`poe-acp` and `slack-acp` should migrate to this repo's deploy-key scheme
  for their Homebrew tap push.** Both still authenticate to `kfet/homebrew-ai`
  with a `HOMEBREW_TAP_TOKEN` personal access token. That is worse on three
  counts, and the third one is a scheduled outage:
  - **Unreadable.** A GitHub secret is write-only, so nobody can inspect what
    those PATs actually are without rotating them.
  - **Unknown scope.** A classic PAT with `repo` is account-wide: it can write
    every repository the account can, not just the tap. A deploy key cannot
    leave the one repo it is attached to.
  - **They expire.** Fine-grained PATs are capped at a one-year lifetime, and
    a classic PAT may have been created with any expiry at all. When it lapses
    the release job fails at the formula step — or, worse, silently stops
    updating the tap — long after anyone remembers why.

  `zulip-acp` uses a write **deploy key** on `kfet/homebrew-ai` instead
  (`.goreleaser.yaml` `brews[].repository.git`, secret `HOMEBREW_TAP_SSH_KEY`):
  repo-scoped, non-expiring, and revocable on its own without touching anything
  else. The migration is a three-line diff per repo plus one secret, and each
  needs its **own** key — sharing one across repos would re-create the
  blast-radius problem the change exists to remove. Not done here: this task is
  scoped to `zulip-acp`, and those repos are not to be touched from it.

- **Mobile push notifications are OFF.** Self-hosted Zulip cannot push to iOS
  without registering with Zulip's Mobile Push Notification Service, which
  sees notification metadata (sender/channel/topic/volume). That is an
  undecided privacy trade-off for the operator; the relay takes no position
  and the server is not registered.
