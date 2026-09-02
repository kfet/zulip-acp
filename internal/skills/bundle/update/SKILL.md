---
builtin: true
name: update
description: Update zulip-acp on a host, or restart a running relay. Covers systemd/launchd supervisor control for this simple long-poll relay (plain restart — no graceful worker-swap shim), and how to restart the relay you are yourself running inside.
---

# Update Skill

Upgrade `zulip-acp` on **one** host and restart the supervisor, or just restart
a running relay. See the `deploy` skill for the canonical file layout; this skill
owns the upgrade/restart mechanics.

> **zulip-acp is a plain `Type=simple` relay.** There is **no** master/worker
> supervisor and **no** graceful SIGHUP worker swap (that is poe-acp-only). A
> restart is a hard restart.

> **A hard restart loses the in-flight turn — it does not redrive it.** The
> `queue_id` / `last_event_id` cursor is in-memory by design
> (`internal/zulipproto/client.go`: *"persisting them is false comfort"*), so on
> startup `Runner.register` (`internal/zulipproto/events.go`) takes a **fresh
> queue at the server's current `last_event_id`**. The message that triggered
> the dying turn is already behind that cursor and is never re-delivered.
> Conversation state on disk survives, so nothing is corrupted — but the user
> gets silence and must re-send. Prefer quiet moments, and see *Restarting the
> relay you are running inside*.

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
ssh <host> 'brew list --versions zulip-acp 2>/dev/null'      # brew install?
ssh <host> 'systemctl --user is-active zulip-acp 2>/dev/null' # Linux supervisor
ssh <host> 'launchctl list 2>/dev/null | grep -i zulip-acp'   # macOS supervisor
```

If installed already equals target, say so and stop unless a forced restart is
wanted.

A version string like `0.8.0-dev+<sha>.dirty` is **not** a release build — it
came from a working tree, not the tag. Treat it as stale regardless of the
number.

### 3. Upgrade path

**Direct deploy (hotfix / private repo — the usual path today):**
```bash
make deploy HOST=<host>                       # scp new binary to ~/.local/bin/zulip-acp
ssh <host> 'systemctl --user restart zulip-acp'
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
~/.local/bin/zulip-acp --version                            # confirm before restarting
```

Build from a **clean tree at the tag**. A stale `bin/` from before the release
commit yields a `-dev+…dirty` binary that installs happily and reports the wrong
version.

**Brew-managed (once the repo is public):**
```bash
ssh <host> 'brew update && brew upgrade zulip-acp'
ssh <host> 'systemctl --user restart zulip-acp'      # Linux
# macOS: launchctl kickstart -k gui/$UID/<label>
```

`daemon-reload` is only needed when the **unit file itself** changed:
```bash
ssh <host> 'systemctl --user daemon-reload && systemctl --user restart zulip-acp'
```

Config- or channel-only change (edited `config.json`): a `restart` is still the
mechanism — there is no reload verb for this relay.

### 4. Restarting the relay you are running inside

Normally a restart is an ordinary inline action: run it directly, no `setsid`,
no `sleep`, no timer unit.

**The exception is when you are the agent this relay hosts.** Your process is a
child of the relay, inside its cgroup. `systemctl --user restart` tears that
cgroup down, so the restart kills you mid-turn: your reply is cut off
mid-stream — whatever was already flushed stays in the message, the rest is
lost — and per the redrive note above it is never retried.

`setsid` does not save you — it escapes the process *group*, not the cgroup.
What does work is a **transient unit**, which systemd places in a cgroup of its
own, outside the blast radius:

```bash
systemd-run --user --collect --on-active=45 \
  systemctl --user restart zulip-acp
```

`--collect` garbage-collects the unit afterwards; leave the name to systemd
(`run-r<hex>`). Passing a fixed `--unit=` name breaks the second invocation with
*"Unit … already exists"* if a previous timer has not fired or a failed unit
lingers.

Finish the turn, post the reply, and let the timer fire after you are done. Say
out loud that the restart is scheduled and when. Use this **only** for the
restart-from-inside case; for every other host, restart inline.

> **macOS: untested from inside.** launchd has no `systemd-run` equivalent and
> the cgroup reasoning does not transfer. Restart from a shell outside the
> relay.

### 5. Verify

```bash
ssh <host> '~/.local/bin/zulip-acp --version'          # == target
ssh <host> 'systemctl --user is-active zulip-acp'       # active
ssh <host> 'journalctl --user -u zulip-acp -n 20 --no-pager'  # re-registered event queue, channels resolved
```

Confirm the running image, not just the on-disk binary:

```bash
ssh <host> 'pid=$(systemctl --user show -p MainPID --value zulip-acp); readlink /proc/$pid/exe; /proc/$pid/exe --version'
```

For real confidence, post a message into a served channel and confirm a reply
(see the `deploy` skill's smoke test).

### 6. Report

One line: `<host>: <old> → <new>, service active`. On failure, surface the error
and stop — do not paper over.

## Pitfalls

- **Stale tap** — `brew upgrade` is a no-op until `brew update` refreshes the tap.
- **Missed recycle** — swapping the binary on disk does nothing to the running
  process; you must `systemctl --user restart` (there is no graceful reload here).
- **`Text file busy`** — never `cp` over the running binary; stage beside it and
  `mv -f`. Leave no `.prev` backup litter behind.
- **Upgrading the *agent* binary (`fir update`)** — the relay holds ONE long-lived
  agent process shared by all conversations. A new `fir` on disk is inert until
  you restart zulip-acp. Verify with `readlink /proc/<agent-pid>/exe`, not
  `fir --version` on disk.
- **Bare-process leftover** — if the host still runs `./zulip-acp` from a home
  folder instead of the systemd unit, `restart` won't touch it. Kill the stray
  process and migrate to the canonical deploy (see `deploy` skill).
- **Detaching a restart is wrong everywhere except from inside the relay** —
  `setsid` and `sleep N &` do not survive the cgroup teardown anyway. The one
  sanctioned form is the `systemd-run --user --collect --on-active=N` transient
  unit in §4.

## Checklist

- [ ] Target version confirmed (latest pushed tag).
- [ ] Install method + supervisor identified.
- [ ] Binary upgraded via the matching path, from a clean tree.
- [ ] Service restarted (inline, or scheduled if restarting from inside).
- [ ] `zulip-acp --version` matches target; service active; queue re-registered.
