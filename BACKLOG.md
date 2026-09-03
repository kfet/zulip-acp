# Backlog

Things deliberately not done in v1, with the reason.

## acp-kit promotion candidates

> **Done, v0.6.0:** the `!command` broker was promoted to `acp-kit/command`
> — ported from `poe-acp`, which deleted its copy in the same change. The
> lesson is recorded in the design doc: "promote when a second consumer
> exists" obliges you to go and *look* at the sibling repos rather than
> reason about them from memory. The entry below was written on the
> assumption that no second consumer existed; one had for months.

- **`internal/rollover` → `acp-kit/chunker`.** The splitter is written with
  zero Zulip imports precisely so it can move. It is not promoted in v1: a
  new, unproven design earns its API in one consumer first. Promote once a
  second surface with a hard message ceiling needs it (Matrix, Discord,
  SMS-style gateways) — at which point the `Poster` interface and the
  seal/continuation markers become the negotiated API, not an accident of
  Zulip's 10k limit.

- **`internal/reload` is transport-generic — but promoting it would be
  premature, and possibly wrong.** Nothing in it imports Zulip: it is a
  cursor struct, an env round-trip, a bounded `WaitIdle`, and a
  `syscall.Exec` that resolves its own binary through the `"(deleted)"`
  trap. `slack-acp` could use it almost verbatim. What would have to change
  first is the shape of the thing being carried: `Cursor` is
  `(queue_id, last_event_id)` because that is what Zulip's `/register`
  hands back, and Slack's Socket Mode has no cursor at all — it has a
  WebSocket that simply reconnects, so a promoted package would carry an
  opaque `map[string]string` (or a `Resumer` interface) instead, and each
  relay would own the encode/decode. Worth doing at the second consumer,
  not before. **`poe-acp` is explicitly NOT that consumer**: it is an HTTP
  server whose scarce resource is a bound listen socket, so it needs a
  master/worker supervisor and drain-then-exec cannot replace it. See
  `docs/graceful-reload.md` for why the two relays diverge here.

- **The reverse promotion — `poe-acp/internal/supervisor` → `acp-kit`, to give
  this relay a zero-latency upgrade — is blocked on conversation concurrency,
  not on the supervisor.** `reload` drains before it execs, so a reload issued
  while some *other* topic is mid-turn stops polling until that turn ends
  (bounded by `-reload-drain-deadline`, default 30m). A master/worker swap
  would remove that wait entirely: W2 takes the cursor and polls immediately
  while W1 finishes its tails. Two things stand in the way, and the second is
  the real one.

  First, the supervisor is less generic than its package doc claims. It says it
  is "transport-generic and knows only about a `net.Listener`", but
  `Config.Addr` is required, `supervisorListen` calls `net.Listen("tcp", addr)`
  unconditionally, and the inherited listener fd **is** the worker discriminant
  (`POE_ACP_WORKER_FD` present ⇒ I am a worker). A relay with no listener at all
  needs that contract re-cut — in a package currently running poe-acp in
  production.

  Second, and decisively: **two workers cannot share a conversation.** This
  relay guarantees that a follow-up supersedes whatever is still running in the
  same topic (`cancelInflight`, `internal/handler/handler.go`). Under a swap W1
  must stop polling — one queue, one poller — so every new message *including a
  follow-up in a topic W1 is still draining* lands on W2, which can neither see
  nor cancel W1's in-flight turn. Two agent processes would then own one conv
  directory and stream edits into one Zulip topic. poe-acp is immune because its
  unit of work is an HTTP stream that its client redrives; a chat topic is
  stateful, shared, and visibly wrong when two workers write it. Closing that
  gap needs cross-process cancellation plus per-conversation locking, which is
  the expensive part — the fork/signal choreography is the part that already
  exists.

  Do not promote on symmetry with poe-acp. The trigger is a *measured* drain
  tail that hurts; until then the cheap mitigation is a shorter
  `-reload-drain-deadline`, and the common case — the agent reloading the relay
  that hosts it — drains in seconds because the only in-flight turn is the one
  that just finished replying.

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
  to replay, and neither is obvious. **A planned upgrade no longer hits this**:
  `systemctl --user reload` hands the LIVE queue to the successor image rather
  than letting it die (`internal/reload`, `docs/graceful-reload.md`). What is
  left here is the unplanned case — a server restart or an idle GC — which no
  amount of cursor handoff can cover, because the queue itself is gone.

- **A service STOP still cuts an in-flight turn; only a reload preserves one.**
  The ACP agent is spawned with `exec.CommandContext(ctx, …)` (acp-kit
  `client.Start`), so a cancelled signal context kills it immediately and
  `-drain-deadline` can only cover flushing output the agent had already
  produced. The reload path sidesteps this by never cancelling `ctx` — it stops
  *intake* through a separate channel — which is exactly why a reply survives a
  reload and not a restart (`docs/graceful-reload.md`). Making a stop drain for
  real would mean giving the agent a context that outlives the signal and
  closing it explicitly in teardown; that trades a guaranteed kill for a
  best-effort one, and every `log.Fatalf` path would then be able to orphan a
  live agent process. Not done deliberately: the answer to "I want my turn to
  survive" is `reload`, which is now the documented default verb.

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

- > **Resolved.** `kfet/zulip-acp` is now **public**, so the first option below
  > was taken and `brew install kfet/ai/zulip-acp` works. Re-verified on
  > 2026-09-03: an unauthenticated fetch of
  > `…/download/v0.12.1/zulip-acp-darwin-arm64` returns **HTTP 200**, and the
  > tap formula on `kfet/homebrew-ai` is at `0.12.1` with sha256s for all five
  > targets. The entry is kept for the reasoning, not as an open question.

  **~~`brew install kfet/ai/zulip-acp` will 404 while this repo is private.~~** The
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

- > **Done.** Both migrated on 2026-08-31. Verified against the *remotes* on
  > 2026-09-03: `poe-acp` and `slack-acp` both carry
  > `brews[].repository.git.private_key` reading `HOMEBREW_TAP_SSH_KEY`, no
  > repo still holds a `HOMEBREW_TAP_TOKEN` secret, and `kfet/homebrew-ai` has
  > five separate write deploy keys — `zulip-acp`, `poe-acp`, `slack-acp`,
  > `fir`, `airan` — so the own-key-per-repo rule held.
  >
  > **Check the remote, not a local clone.** This entry was first re-read from
  > a stale `~/src/slack-acp` (HEAD at v0.4.1, repo shipping v0.4.3) and
  > wrongly reported as still open.

  **~~`poe-acp` and `slack-acp` should migrate to this repo's deploy-key scheme
  for their Homebrew tap push.~~** Both authenticated to `kfet/homebrew-ai`
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
