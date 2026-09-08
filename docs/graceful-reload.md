# Graceful reload: drain, then re-exec in place

**Status:** implemented (`internal/reload`, `internal/zulipproto`, `cmd/zulip-acp/main.go`).

`systemctl --user reload zulip-acp` upgrades the relay without dropping an
in-flight turn and without losing a single Zulip message.

## The problem it solves

A hard restart is destructive in two ways that compound.

**It silently eats messages.** The Zulip event-queue cursor is in-memory by
design — `internal/zulipproto/client.go` says persisting it is false comfort,
and that is right, because a persisted `queue_id` names a queue the server has
long since garbage-collected. On startup `Runner.register` calls `/register`,
which hands back the server's *current* `last_event_id`. Every message posted
before that instant is behind the new cursor and is **never delivered**. The
message that triggered the turn the restart just killed is one of them. The
user gets silence, and nothing anywhere logs a loss.

**It kills the agent that asked for it.** The relay hosts the ACP agent as a
child process, in the same cgroup. When *the agent itself* is asked to update
the relay — the common case, because the agent is the one being talked to —
`systemctl --user restart` tears down that cgroup and kills its own reply
mid-stream.

## Why not a master/worker supervisor

`poe-acp` solves the equivalent problem with a supervisor that forks workers
and hands each an inherited listen-socket fd. Copying that here would be
cargo-culting the *shape* of a solution without its *reason*.

poe-acp is an **HTTP server**. Its scarce resource is a bound listen socket,
and every instant that socket is closed is an instant clients get
`ECONNREFUSED`. The supervisor exists to hold the socket across worker
generations. Nothing else can.

zulip-acp is a **long-poll client**. It holds no socket, and while it is not
polling, nothing is lost: messages accumulate *server-side* in the Zulip event
queue and are delivered on the next `GetEvents` for that `queue_id`. The buffer
a supervisor would exist to provide already exists, remotely, for free. So a
supervisor here would hold nothing, and the control pipes, parent-death pipes,
ready signalling and drain ordering it needs would all be machinery in service
of no resource.

It is also ~150 lines instead of ~1400, and it never has two processes sharing
the state directory.

## The sequence

1. `SIGHUP` arrives (`ExecReload=/bin/kill -HUP $MAINPID`).
2. All **intake** stops. `zulipproto.Runner` stops polling and returns
   `ErrHandoff` — it does **not** delete the queue; `queue_id` and
   `last_event_id` are kept. The schedule store's context is cancelled too, so
   no timer can start a *new* turn and extend the drain indefinitely; an
   overdue item is simply claimed by the successor image on its next tick
   (`schedule.Store.due` takes everything whose time has come). Reactions
   waiting out their coalescing debounce are dropped for the same reason
   (`Handler.DropPendingReactions`): a buffered emoji is not a turn anybody is
   waiting on, and letting its timer expire mid-drain would start a turn the
   re-exec then kills. A reaction burst whose turn has already begun is an
   in-flight turn like any other and is drained at step 3.
3. `reload.Drain` blocks on `handler.WaitIdle` until every in-flight turn has
   finished posting, bounded by `-reload-drain-deadline` (30m).
4. The MCP loopback's session→token registry is read out
   (`mcphost.Host.ExportTokens`), then `cleanup()` closes the ACP agent, the
   session manager and the MCP host. The host is closed with `CloseForExec`,
   which stops serving **without unlinking the socket file** — the agent's
   redirector subprocesses are already redialling that exact path.
5. `reload.Exec` `syscall.Exec`s the on-disk binary, passing the cursor in
   `ZULIP_ACP_QUEUE_ID` / `ZULIP_ACP_LAST_EVENT_ID` /
   `ZULIP_ACP_QUEUE_REGISTRATION`, and the token registry in
   `ZULIP_ACP_MCP_TOKENS`. The registry is a bearer credential for every live
   session: it goes in the successor's environment and nowhere else — never a
   log line, never a file — and is scrubbed from the ACP agent's environment
   like the bot API key (`reload.AgentEnvNames`).
6. The new image reads that cursor and **resumes** `GetEvents` on the same
   queue instead of registering — *provided* the queue was registered with the
   event types and narrow this image wants. No gap and no double delivery. If
   the registration differs, the inherited queue is *swapped*: a replacement
   is registered while the old queue is still buffering, the old one is drained
   and dispatched, and only then deleted — still no gap, and the overlap both
   queues saw is de-duplicated. See *When the queue cannot be resumed* below.

The PID never moves, so systemd never sees the service stop: `Type=simple` is
correct and no readiness handshake is needed.

### Holding the queue open across a long drain

Step 3 can legitimately take half an hour (`-reload-drain-deadline`), and a
Zulip queue is **garbage-collected once nothing has touched it for the queue's
lifespan** — ten minutes by default. A long turn would therefore cost the very
queue the handoff exists to preserve, and the successor would register fresh
and skip everything posted during the reload.

Nothing the client does afterwards can prevent that. Zulip refreshes
`last_connection_time` only in `connect_handler`, which runs only when a poll
actually *blocks* — so a `dont_block` fetch does not count, and neither does a
blocking poll on a queue that already holds events, because it returns
immediately. The only lever is at registration: `/register` takes
**`queue_lifespan_secs`** (capped server-side at 7 days), so the relay
registers its queue with `-reload-drain-deadline + 5m`.

An older server takes the request and **ignores** the parameter, reporting it
in `ignored_parameters_unsupported` — Zulip does not fail such a request — so
the runner checks that list and warns once, rather than believing a guarantee
it did not get.

## Why the cursor handoff is exact

A Zulip event queue is server-side state keyed by `queue_id`. It is not bound
to a connection, a socket or a process — the server authenticates each
`GetEvents` request on its own. So a queue registered by one process image is
pollable by its successor, at the same `last_event_id`, with nothing skipped
and nothing replayed. An unpolled queue is garbage-collected after a few
minutes; an exec is sub-second.

This is not assumed. `test/reload_test.go`
(`TestEventQueueSurvivesReExec`, `ZULIP_LIVE=1`) proves it against a real
server by re-running itself across an actual `syscall.Exec`: it registers a
queue, posts a message while nobody is polling, execs, and asserts the resumed
queue still delivers that message — while a control queue registered *after*
the post, which is exactly what a hard restart does, never sees it.

## When the queue cannot be resumed

A Zulip event queue's `event_types` and `narrow` are **frozen at `/register`**.
There is no call that widens a live queue. So a queue registered for
`["message","update_message"]` will never deliver a `reaction` event, however
loudly the polling process believes it subscribed to one.

That collides with the handoff above. Because the cursor was originally *only*
`(queue_id, last_event_id)`, the successor resumed whatever it inherited, and
therefore kept polling the **predecessor's** registration — forever, across
every subsequent reload, until someone happened to do a hard restart. This is
not hypothetical: on v0.18.1 emoji reaction delivery was dead in production for
as long as the relay kept being reloaded. The feature was on, the code was
correct, and the queue could not carry the events.

So the registration travels with the cursor, as a fingerprint — the canonical
JSON of the sorted `event_types` and `narrow`
(`zulipproto.RegistrationFingerprint`) — and the successor **refuses to resume**
a queue whose fingerprint is not the one it wants. It **swaps** it, naming the
difference:

```
zulip: registration changed (event types +reaction); inherited queue 05db1c49…
cannot carry this image's events — registering a replacement, then draining it
zulip: drained 3 event(s) from inherited queue 05db1c49… — nothing posted
during the reload is lost
```

**A missing fingerprint counts as different.** An upgrade *from* an image that
never recorded one leaves it unset, and unset means *unknown*, not *matching* —
otherwise the very reload that installs this check would resume the broken
queue and preserve the defect it exists to end.

### The swap is lossless, and the order is the design

A queue that cannot be resumed still **holds messages** — everything posted
while the predecessor was draining and exec'ing. Deleting it and calling
`/register`, which is what v0.19.1 did, loses all of them: `/register` hands
back the server's *current* `last_event_id`, so anything posted before that
instant is behind the new cursor forever.

Carrying the old cursor across is not available either. Zulip event ids are
**per-queue** and monotonic only within a queue; the old queue's
`last_event_id` is meaningless to a new one.

What *is* available is **overlap**. `Runner.swapQueue`
(`internal/zulipproto/events.go`) does, in this order:

1. **Register the replacement first**, while the old queue is still alive and
   still buffering server-side. From that instant nothing new can be missed by
   both queues.
2. **Drain the old queue**: poll `GET /events` on it from the inherited cursor,
   repeatedly, until it yields nothing.
3. **Dispatch** what it held, in order, through the normal handler path —
   before the replacement is polled at all, so ordering is preserved.
4. **Delete** the old queue, then start polling the replacement.

Step 2 needs a stopping rule. `GET /events` takes no server-side *timeout*
parameter (Zulip 12.2 reports `timeout` in `ignored_parameters_unsupported`),
but it does take **`dont_block=true`**, which returns whatever the queue holds
immediately — including an empty list. That is the drain primitive
(`Client.DrainEvents`), and it matters: with a blocking poll, "empty" could
only be *guessed* from a timeout, and a server slow to answer a **non-empty**
batch would be misread as drained, deleting a queue that still held messages.
So an empty result is the server's own statement and ends the drain; a timeout
is a fault and is reported as one. A dead queue (`BAD_EVENT_QUEUE_ID`) is a
completed drain — it has nothing left to give — and a poll whose events do not
advance the cursor ends it too, rather than spinning.

### The overlap, and the dedup window

Everything posted between step 1 and step 4 lands in **both** queues, so it
would otherwise be delivered twice. Per-queue event ids cannot match the two
sightings, but **message ids are realm-global and stable**, so identity is
derived from the event's content (`internal/zulipproto/dedup.go`):

| event | identity |
| --- | --- |
| `message` | `message:<message id>` |
| `update_message` | `update:<message id>:<edit timestamp>` |
| `reaction` | `reaction:<message id>:<user id>:<emoji>:<op>` |

`subscription` and `stream` events have no identity of their own and are
dispatched unconditionally: they are `add`/`remove`/`update` ops applied to a
set idempotently, so a second sighting changes nothing.

The runner keeps a **bounded FIFO** of the last `DefaultDedupWindow` (512)
dispatched identities and drops a second sighting. It evicts oldest-first, so
it cannot grow with uptime.

It is **armed only for the swap**, and retired after the first completed poll
of the replacement queue. That is deliberate, and the window is exactly right
in both directions:

- One poll is enough, because every event that reached both queues was posted
  *before* the old queue was deleted — so it was already buffered in the new
  queue when that poll was issued, and `GET /events` returns everything a queue
  is holding.
- One poll is also the most that is safe. An event identity can legitimately
  **recur**: a user who adds a reaction, removes it and adds it again produces
  the same `(message, user, emoji, op)` twice, and a permanently armed dedup
  set would silently swallow the second one. A duplicate *delivery* is only
  possible during a swap, so that is the only window the check runs in.

One known limit inside the window: `edit_timestamp` has one-second resolution,
so two edits of the *same* message within the *same second* and inside the
overlap would collapse into one. That is a far smaller error than dropping the
message entirely, which is what it replaces.

### Where a gap is still possible

Two bounded cases remain, and both log a `WARN` naming the queue rather than
passing in silence:

- **`/register` fails five times during the swap.** There is nothing to swap
  *to*; the unusable queue is dropped and the loop retries a fresh
  registration. A single 5xx does not cost the messages the old queue holds —
  the swap retries under the ordinary backoff first — but a server that will
  not register at all leaves no alternative.
- **The drain exceeds `DrainBudget` (60s)**, or fails on a fault that is not a
  dead queue. The replacement is already registered and is used regardless — a
  swap must never hang startup — so whatever the old queue still held is lost,
  and said so.

A **reload signal landing mid-drain** is not one of those cases. The old queue
still holds everything the drain never reached, while the replacement holds
only the overlap, so the swap is abandoned in the *other* direction: the
replacement is deleted and the **old** queue is handed to the successor, at the
cursor the drain reached, together with **its own** registration fingerprint.
The successor simply redoes the swap. Handing on this image's fingerprint
instead would tell the successor that an unresumable queue is resumable — the
exact defect this machinery exists to end.

A corollary for operators: **a relay already running a pre-fix image needs one
hard `systemctl --user restart`** to escape a queue registered under the old
shape. A reload cannot do it, because the image doing the resuming is the one
without the check.

## Why a reload drains and a stop does not, really

The ACP agent is spawned with `exec.CommandContext(ctx, …)` (acp-kit
`client.Start`), so it dies the moment the process's signal context is
cancelled. That is why the two paths differ in kind, not just in budget:

- **Reload** never cancels `ctx`. Only *intake* stops, via a separate
  `Handoff` channel and `intakeCtx`. The agent keeps running, the turn keeps
  producing, and `-reload-drain-deadline` is a real window in which a real
  reply gets finished. This is the whole mechanism.
- **Stop** cancels `ctx` by construction — that is what a stop *is* — so the
  agent is torn down with it and `-drain-deadline` can only cover flushing
  what the agent had already produced. A turn cut this way is annotated by
  the next start (`handler.MarkInterrupted`).

So "use `reload`, not `restart`" is not a style preference. A restart cannot
preserve a turn no matter how generous its timeout, because the thing
producing the turn is inside the context being cancelled. See `BACKLOG.md`
for what it would take to change that.

## What is deliberately not here

**No two-process overlap, so no shared-state hazard.** There is only ever one
zulip-acp process. The journal, the per-conversation state dirs and
`schedules.json` have exactly one writer at all times, so no lockfile is needed
and none exists.

**No `sd_notify` / `Type=notify`.** It would buy accurate startup ordering and
nothing else; the PID is stable, so the readiness handshake that a
worker-swapping supervisor needs has no analogue here.

**No blocking reload.** `ExecReload` is a bare `kill`, so
`systemctl --user reload` returns as soon as the signal is delivered. That is
the point: an agent hosted by this relay runs the reload **inline**, mid-turn,
and the relay then waits in step 3 for that very turn to finish before going
anywhere. A blocking reload would deadlock exactly that case.

## Verified end to end

Against the live server, with a throwaway bot and channel (2026-09-02, relay
pid stable at 833097/854272 throughout):

- **A reply survives the reload.** A turn running a 50s shell loop was sent
  SIGHUP mid-flight. The journal shows `SIGHUP — graceful reload` at T+0,
  `re-exec with queue … at event 58` at T+40s once the turn finished, and the
  topic holds exactly **one** complete reply — not truncated, not duplicated.
- **Nothing posted in the window is lost.** A message posted to a second topic
  *while the relay was not polling* was dispatched 30s later by the new image,
  immediately after `resuming inherited event queue …`.
- **The agent can reload itself, inline.** A turn that ran `kill -HUP <relay>`
  as its first tool call, then slept 15s, then answered, posted its answer
  intact. This is the case the whole change exists for.
- **Scheduled prompts are unaffected.** A prompt scheduled 90s out during a
  turn that also reloaded the relay fired normally in the *new* image.
- **The stop path is unchanged.** SIGTERM drains and deletes the queue.

## Failure modes

| Failure | Behaviour |
|---|---|
| Drain deadline expires with turns still running | Exec happens anyway; the successor's `handler.MarkInterrupted` annotates the half-streamed messages. Same as a hard restart — the worst case here, not the normal one. |
| The inherited queue died (server restarted during the exec) | `BAD_EVENT_QUEUE_ID` is routine: the runner registers fresh and carries on. Messages in the window are lost, as with a hard restart. |
| `ZULIP_ACP_QUEUE_ID` / `ZULIP_ACP_LAST_EVENT_ID` half-set or malformed | Neither is honoured. Register fresh, log a WARN. Half a cursor would silently skip or replay events, which is worse than a logged gap. |
| The new image wants different `event_types` or a different `narrow` | The inherited queue **cannot** carry them (both are frozen at `/register`). It is swapped: a replacement is registered while it is still buffering, it is drained and dispatched, then deleted — nothing lost, overlap de-duplicated. See *When the queue cannot be resumed*. |
| `ZULIP_ACP_QUEUE_REGISTRATION` absent (upgrade from an image predating it) | Treated as *unknown, therefore different*: re-register rather than resume. Resuming on a guess is the defect this check exists to end. |
| `ZULIP_ACP_MCP_TOKENS` malformed | Logged as a WARN; the relay comes up with an empty registry. Sessions predating the reload lose their `mcp__relay__*` tools until they are re-created; everything else works. Refusing to start would take every conversation down to protect the loopback of a few. |
| `syscall.Exec` fails (binary removed mid-reload) | `log.Fatalf`, exit non-zero, `Restart=on-failure` brings the relay back cold. The orphaned queue is named in the log line. |
| Binary replaced by an atomic `mv` before the reload | `os.Executable` reads `/proc/self/exe`, which names the now-unlinked inode as `"<path> (deleted)"`. `reload.SelfPath` strips that marker, re-stats, and falls back to `os.Args[0]` through `PATH`. Getting this wrong would fail the exec *after* the agent had already been shut down. |

## Operator surface

- `systemctl --user reload zulip-acp` — binary or config change.
- `systemctl --user restart zulip-acp` — unit-file change, a stopped service,
  or the **first cutover** onto a build that has reload support (the older
  binary has no `SIGHUP` handler and would simply die on the signal).
- `-reload-drain-deadline` (30m) — leak backstop for a reload drain. Nothing
  external is waiting; `no_progress_timeout_seconds` is what bounds a turn as work.
- `-drain-deadline` (30s) — stop drain. Keep it under `TimeoutStopSec`.
