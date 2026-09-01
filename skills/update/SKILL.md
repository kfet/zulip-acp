---
name: update
description: Update zulip-acp on a host, or restart a running relay. Covers systemd/launchd supervisor control for this simple long-poll relay (plain restart — no graceful worker-swap shim).
---

# Update Skill

Upgrade `zulip-acp` on **one** host and restart the supervisor, or just restart
a running relay. See the `deploy` skill for the canonical file layout; this skill
owns the upgrade/restart mechanics.

> **zulip-acp is a plain `Type=simple` relay.** There is **no** master/worker
> supervisor and **no** graceful SIGHUP worker swap (that is poe-acp-only). A
> restart is a hard restart: the in-flight turn's streamed edit stops, but the
> conversation state is on disk and Zulip re-delivers the event, so the next
> turn redrives from history. Nothing is permanently lost. Prefer quiet moments.

> **Restarting the relay is an ordinary, inline action.** Run it directly — no
> `setsid`, no `sleep`, no delayed one-shot unit. Under systemd the default
> `KillMode=control-group` tears down the whole cgroup, so a detached restart
> command sits inside its own blast radius anyway.

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

### 3. Upgrade path

**Direct deploy (hotfix / private repo — the usual path today):**
```bash
make deploy HOST=<host>                       # scp new binary to ~/.local/bin/zulip-acp
ssh <host> 'systemctl --user restart zulip-acp'
```

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

### 4. Verify

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

### 5. Report

One line: `<host>: <old> → <new>, service active`. On failure, surface the error
and stop — do not paper over.

## Pitfalls

- **Stale tap** — `brew upgrade` is a no-op until `brew update` refreshes the tap.
- **Missed recycle** — swapping the binary on disk does nothing to the running
  process; you must `systemctl --user restart` (there is no graceful reload here).
- **Upgrading the *agent* binary (`fir update`)** — the relay holds ONE long-lived
  agent process shared by all conversations. A new `fir` on disk is inert until
  you restart zulip-acp. Verify with `readlink /proc/<agent-pid>/exe`, not
  `fir --version` on disk.
- **Bare-process leftover** — if the host still runs `./zulip-acp` from a home
  folder instead of the systemd unit, `restart` won't touch it. Kill the stray
  process and migrate to the canonical deploy (see `deploy` skill).
- **Never detach or delay a restart** — no `setsid`, no `sleep N`, no one-shot
  timer. It is unnecessary and, under systemd's cgroup KillMode, doesn't even
  work reliably.

## Checklist

- [ ] Target version confirmed (latest pushed tag).
- [ ] Install method + supervisor identified.
- [ ] Binary upgraded via the matching path.
- [ ] Service restarted.
- [ ] `zulip-acp --version` matches target; service active; queue re-registered.
