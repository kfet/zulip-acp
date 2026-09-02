---
builtin: true
name: update
description: Update zulip-acp on a host, or recycle a running relay. Covers the graceful SIGHUP reload (drain in-flight turns, then re-exec in place — no lost messages), when a hard restart is still required, and how to reload the relay you are yourself running inside.
---

# Update Skill

Upgrade `zulip-acp` on **one** host and recycle it, or just recycle a running
relay. See the `deploy` skill for the canonical file layout; this skill owns the
upgrade/recycle mechanics.

## reload vs restart — pick the right verb

> **`systemctl --user reload zulip-acp` is the default verb.** SIGHUP makes the
> relay stop polling the Zulip event queue *without deleting it*, wait for every
> in-flight agent turn to finish posting, and then re-exec the on-disk binary
> **in place, same PID**. The queue keeps buffering server-side across the
> window and the new image resumes it at the exact same `last_event_id`, so
> **nothing posted during the reload is lost and nothing is delivered twice**.
> See `docs/graceful-reload.md`.

Use **reload** for:

- a new binary staged on disk;
- an edited `config.json`;
- picking up a newly-installed `fir` (the relay holds one long-lived agent
  process; the exec starts a fresh one).

Use **restart** — a hard, destructive restart — only for:

- a change to the **unit file** itself (after `daemon-reload`);
- a service that is **stopped or dead** (there is no process to signal);
- the **first cutover** onto a build that has reload support at all. The older
  binary has no SIGHUP handler; sending it one just kills it. Cut over once
  with `restart`, and every recycle after that is a `reload`.

> **A hard restart loses messages, silently.** The `queue_id` /
> `last_event_id` cursor is in-memory by design
> (`internal/zulipproto/client.go`: *"persisting them is false comfort"*), so on
> a cold start `Runner.register` (`internal/zulipproto/events.go`) takes a
> **fresh queue at the server's current `last_event_id`**. Every message posted
> before that instant — including the one that triggered the turn the restart
> just killed — is behind the cursor and is never delivered. Conversation state
> on disk survives, so nothing is corrupted, but the user gets silence and must
> re-send. This is exactly what `reload` exists to avoid.

## Inputs

Confirm with the user before acting:

1. **Host** — `local` or `user@host`. Default local.
2. **Target version** — default: latest `vX.Y.Z` tag on `origin`. Override only
   if asked. If `VERSION` is ahead of every pushed tag, an unpublished release
   exists — run the `release` flow first.

## Steps

### 1. Determine target version

```bash
git fetch --tags origin && git tag --sort=-v:refname | head -1
```

### 2. Probe the host

```bash
ssh <host> '~/.local/bin/zulip-acp --version 2>/dev/null || echo not-installed'
ssh <host> 'brew list --versions zulip-acp 2>/dev/null'       # brew install?
ssh <host> 'systemctl --user is-active zulip-acp 2>/dev/null' # Linux supervisor
ssh <host> 'launchctl list 2>/dev/null | grep -i zulip-acp'   # macOS supervisor
```

If installed already equals target, say so and stop unless a forced recycle is
wanted.

A version string like `0.8.0-dev+<sha>.dirty` is **not** a release build — it
came from a working tree, not the tag. Treat it as stale regardless of the
number.

Check whether the running image can reload at all:

```bash
ssh <host> 'systemctl --user cat zulip-acp | grep -c ExecReload'
```

Zero means the unit predates reload support: install the new unit, `restart`
once, and reload from then on.

### 3. Upgrade path

**Direct deploy (hotfix / private repo — the usual path today):**
```bash
make deploy HOST=<host>                      # scp new binary to ~/.local/bin/zulip-acp
ssh <host> 'systemctl --user reload zulip-acp'
```

`make deploy` needs a `HOST`. Updating **local** is the same idea by hand, with
one catch: `cp` onto the running binary fails with `Text file busy`. Replace it
atomically instead — `mv` swaps the directory entry and leaves the running
image untouched:

```bash
make build-all
cp bin/zulip-acp-linux-amd64 ~/.local/bin/.zulip-acp.new
chmod +x ~/.local/bin/.zulip-acp.new
mv -f ~/.local/bin/.zulip-acp.new ~/.local/bin/zulip-acp   # atomic
~/.local/bin/zulip-acp --version                            # confirm before reloading
```

Build from a **clean tree at the tag**. A stale `bin/` from before the release
commit yields a `-dev+…dirty` binary that installs happily and reports the wrong
version.

**Brew-managed (once the repo is public):**
```bash
ssh <host> 'brew update && brew upgrade zulip-acp'
ssh <host> 'systemctl --user reload zulip-acp'       # Linux
# macOS: launchctl kill SIGHUP gui/$UID/<label>
```

`daemon-reload` is only needed when the **unit file itself** changed, and that
is one of the cases that needs a hard restart:
```bash
ssh <host> 'systemctl --user daemon-reload && systemctl --user restart zulip-acp'
```

Config- or channel-only change (edited `config.json`): `reload`.

### 4. Reloading the relay you are running inside

**Run it inline. Your reply survives.**

```bash
systemctl --user reload zulip-acp
```

This is the case the reload was built for. You are the agent this relay hosts,
so your process is a child of the relay inside its cgroup — but `reload` never
tears that cgroup down. The `ExecReload` is a bare `kill -HUP`, so the command
returns immediately, and the relay then **waits for your turn to finish
posting** before it re-execs. Finish your reply normally; the exec happens
after you are done, and the user's next message is waiting in the queue the new
image resumes.

Do **not** detach it, schedule it, or wrap it. No `setsid`, no `sleep N &`, no
`systemd-run --on-active=N` transient unit. Those were workarounds for
`restart` killing the caller, and they are obsolete — `setsid` never worked
anyway (it escapes the process *group*, not the cgroup, so `KillMode=control-group`
still kills it; it only appeared to work because it survived the agent
harness's per-call process-group cleanup).

If you genuinely need a **hard restart** from inside — a unit-file change —
then you cannot survive it: say so, post your reply first, and let the user run
it, or accept that the turn ends there.

> **macOS:** `launchctl kill SIGHUP gui/$UID/<label>` is the equivalent, and is
> untested from inside. Prefer running it from a shell outside the relay.

### 5. Verify

```bash
ssh <host> '~/.local/bin/zulip-acp --version'                 # == target
ssh <host> 'systemctl --user is-active zulip-acp'              # active
ssh <host> 'journalctl --user -u zulip-acp -n 30 --no-pager'
```

A successful reload leaves a distinctive trail in the journal:

```
zulip-acp: SIGHUP — graceful reload: no longer polling, draining in-flight turns …
zulip-acp: re-exec with queue <uuid> at event <n>
zulip-acp: resuming inherited event queue <uuid> (last_event_id=<n>) …
```

Seeing `event queue … registered` instead of `resuming inherited event queue`
means the cursor did not survive — the reload degraded into a cold start and
messages in the window were dropped. Read the preceding WARN.

Confirm the running image, not just the on-disk binary. The PID is **unchanged**
across a reload, so check the exe link rather than the PID:

```bash
ssh <host> 'pid=$(systemctl --user show -p MainPID --value zulip-acp); readlink /proc/$pid/exe; /proc/$pid/exe --version'
```

For real confidence, post a message into a served channel and confirm a reply
(see the `deploy` skill's smoke test).

### 6. Report

One line: `<host>: <old> → <new>, reloaded, service active`. On failure, surface
the error and stop — do not paper over.

## Pitfalls

- **Stale tap** — `brew upgrade` is a no-op until `brew update` refreshes the tap.
- **Missed recycle** — swapping the binary on disk does nothing to the running
  process; you must `systemctl --user reload zulip-acp`.
- **`Text file busy`** — never `cp` over the running binary; stage beside it and
  `mv -f`. Leave no `.prev` backup litter behind. (The reload copes with the
  unlinked inode this leaves behind: `reload.SelfPath` strips the
  `"(deleted)"` marker `/proc/self/exe` reports.)
- **Unchanged PID is expected** — a reload re-execs in place, so `MainPID` does
  not move and neither does uptime in `systemctl status`. Use the journal lines
  above, or `/proc/<pid>/exe --version`, to confirm the new image is running.
- **Upgrading the *agent* binary (`fir update`)** — the relay holds ONE
  long-lived agent process shared by all conversations. A new `fir` on disk is
  inert until the relay re-execs. A `reload` picks it up. Verify with
  `readlink /proc/<agent-pid>/exe`, not `fir --version` on disk.
- **Bare-process leftover** — if the host still runs `./zulip-acp` from a home
  folder instead of the systemd unit, neither verb touches it. `kill -HUP` on
  its PID still performs a graceful reload; then migrate to the canonical
  deploy (see `deploy` skill).
- **A reload waits for the drain** — if some other conversation is mid-turn, the
  exec does not happen until that turn finishes (up to `-reload-drain-deadline`,
  30m). That is correct, not a hang. The queue is buffering throughout.

## Checklist

- [ ] Target version confirmed (latest pushed tag).
- [ ] Install method + supervisor identified; `ExecReload` present in the unit.
- [ ] Binary upgraded via the matching path, from a clean tree.
- [ ] Relay recycled with `reload` (or `restart`, if unit-file/first-cutover/dead).
- [ ] `/proc/<MainPID>/exe --version` matches target; service active; journal
      shows `resuming inherited event queue`.
