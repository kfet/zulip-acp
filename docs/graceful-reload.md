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
   (`schedule.Store.due` takes everything whose time has come).
3. `reload.Drain` blocks on `handler.WaitIdle` until every in-flight turn has
   finished posting, bounded by `-reload-drain-deadline` (30m).
4. `cleanup()` closes the ACP agent, the session manager and the MCP host.
5. `reload.Exec` `syscall.Exec`s the on-disk binary, passing the cursor in
   `ZULIP_ACP_QUEUE_ID` / `ZULIP_ACP_LAST_EVENT_ID`.
6. The new image reads that cursor and **resumes** `GetEvents` on the same
   queue instead of registering. No gap and no double delivery.

The PID never moves, so systemd never sees the service stop: `Type=simple` is
correct and no readiness handshake is needed.

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
| `syscall.Exec` fails (binary removed mid-reload) | `log.Fatalf`, exit non-zero, `Restart=on-failure` brings the relay back cold. The orphaned queue is named in the log line. |
| Binary replaced by an atomic `mv` before the reload | `os.Executable` reads `/proc/self/exe`, which names the now-unlinked inode as `"<path> (deleted)"`. `reload.SelfPath` strips that marker, re-stats, and falls back to `os.Args[0]` through `PATH`. Getting this wrong would fail the exec *after* the agent had already been shut down. |

## Operator surface

- `systemctl --user reload zulip-acp` — binary or config change.
- `systemctl --user restart zulip-acp` — unit-file change, a stopped service,
  or the **first cutover** onto a build that has reload support (the older
  binary has no `SIGHUP` handler and would simply die on the signal).
- `-reload-drain-deadline` (30m) — leak backstop for a reload drain. Nothing
  external is waiting; `prompt_timeout` is what bounds a turn as work.
- `-drain-deadline` (30s) — stop drain. Keep it under `TimeoutStopSec`.
