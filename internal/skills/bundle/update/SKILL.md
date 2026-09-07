---
builtin: true
name: update
description: Update zulip-acp on a host, or recycle a running relay. The upgrade verb is `zulip-acp update` (self-update with an atomic swap), or `scripts/converge.sh` for a fleet host — never a hand-placed binary. Covers the graceful SIGHUP reload (drain in-flight turns, then re-exec in place — no lost messages), when a hard restart is still required, and how to reload the relay you are yourself running inside.
---

# Update Skill

Upgrade `zulip-acp` on **one** host and recycle it, or just recycle a running
relay. See the `deploy` skill for the canonical file layout; this skill owns the
upgrade/recycle mechanics.

> **Fleet hosts: converge is the only sanctioned way to touch a host.** For a
> bot with a spec in `bots/<name>.json`, version moves go through the lock:
> `scripts/converge.sh --tot` (rewrites `dist.lock`; review + commit), then
> `scripts/converge.sh <bot> --apply` per host. Everywhere else the verb is
> `zulip-acp update`. **Never place a binary by hand** — no `cp`, no staged
> `.new` file, no `mv` into `~/.local/bin`.

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

## The canonical order — pick the FIRST one that applies

1. **Fleet host with a spec in `bots/<name>.json` → converge, always.**
   `scripts/converge.sh --tot` rewrites `dist.lock` (review + commit), then
   `scripts/converge.sh <bot> --apply` converges that host. **Converge is the
   only sanctioned way to touch a fleet host** — it moves the binary, `fir`,
   the fir extensions, `config.json` and the unit to the locked state, then
   picks and verifies the recycle for you. Never hand-upgrade a fleet host to
   an unlocked version.
2. **Any other host → `zulip-acp update`.** The binary updates itself: it
   resolves the release over the GitHub API, verifies the sha256 against
   `checksums.txt`, and swaps the file atomically underneath the running
   process. This is what converge itself runs on the host.
3. **Nothing installed yet → `install.sh`.** It places the first binary,
   checksum-verified, with the same asset naming as the self-update; after
   that the host updates itself. Pass `BIN_DIR` — the unit, converge and the
   version probe all expect `~/.local/bin/zulip-acp`, while the script
   defaults to `/usr/local/bin` when that is writable:

   ```bash
   ssh <host> 'curl -fsSL https://raw.githubusercontent.com/kfet/zulip-acp/main/install.sh \
                 | BIN_DIR=$HOME/.local/bin sh'
   ```

There is no fourth option. A build that is not released yet is not a thing to
ship to a host: **cut the release** and then `zulip-acp update`. A
brew-managed install needs nothing special either — `zulip-acp update` detects
the keg and runs `brew upgrade` for you.

**Never hand-place a binary.** No `cp`/`mv` into `~/.local/bin`, no staged
`.new` file, no `scp` of a locally built binary onto a fleet host. There is
deliberately no `make deploy` target for this reason. The atomic,
`ETXTBSY`-safe swap lives *inside* the binary (`github.com/kfet/distkit`,
wired up in `internal/updater`) precisely so nobody has to reproduce it by
hand — and a hand-placed binary is invisible to `dist.lock`, so the next
converge silently disagrees with the host.

## Inputs

Confirm with the user before acting:

1. **Host** — `local` or `user@host`. Default local. If it has a spec in
   `bots/`, use converge (rule 1) and stop reading the per-host steps.
2. **Target version** — default: latest `vX.Y.Z` release. Override only if
   asked. If `VERSION` is ahead of every pushed tag, an unpublished release
   exists — run the `release` flow first.

## Steps

### 1. Determine target version

```bash
gh release view --repo kfet/zulip-acp --json tagName -q .tagName
```

`git tag --sort=-v:refname | head -1` after a `git fetch --tags` answers the
same question from a checkout.

### 2. Probe the host

```bash
ssh <host> '~/.local/bin/zulip-acp --version 2>/dev/null || echo not-installed'
ssh <host> 'brew list --versions zulip-acp 2>/dev/null'       # brew install?
ssh <host> 'systemctl --user is-active zulip-acp 2>/dev/null' # Linux supervisor
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

### 3. Upgrade

**Fleet host (has a `bots/<name>.json` spec):**
```bash
scripts/converge.sh --tot                    # rewrite dist.lock; review + commit
git diff dist.lock
scripts/converge.sh <bot>                    # dry run: what would change, and
                                             # which recycle it would use
scripts/converge.sh <bot> --apply            # converge + recycle + verify
```

Converge refuses to guess: it reads the version of the image the *running*
process is executing (via `/proc/<MainPID>/exe`), not the on-disk file, and
reports `graceful reload` or `hard restart` with the reason before it acts.

**Any other host — the binary updates itself:**
```bash
ssh <host> 'zulip-acp update'                        # latest
ssh <host> 'zulip-acp update --check'                # report only, install nothing
ssh <host> 'zulip-acp update --version v0.19.0'      # pin
ssh <host> 'zulip-acp update --restart-cmd "systemctl --user reload zulip-acp"'
ssh <host> 'systemctl --user reload zulip-acp'       # if no --restart-cmd
```

> **Everything goes through the GitHub API**, asset bytes included. That is
> what made this work while `kfet/zulip-acp` was private, and it is unchanged
> now that the repo is public: a token (`GITHUB_TOKEN`, `GH_TOKEN`, then a
> logged-in `gh`, in that order) is used when present and is not required.
> Keep `gh` logged in on a host behind a shared IP anyway — the
> **unauthenticated API rate limit** is per-address, and a 403 from a sweep of
> hosts looks exactly like a permissions failure.

`zulip-acp update` handles a **Homebrew** install by upgrading it through brew
(`brew update && brew upgrade <formula>`) rather than swapping the keg
underneath the package manager — a self-updated keg is silently reverted by
the next `brew upgrade`, leaving a host that reports one version and runs
another. It **refuses** an install directory not owned by you (a distro
package, `/usr/bin`, a shared `/opt` tree): it would have to write there to
swap the file. The refusal is deliberate and names the command to use
instead — a root-owned `/usr/local/bin` says `sudo zulip-acp update`. Do not
work around it by hand.

`--check` exits **3** when a newer release exists, 0 when up to date, so a
sweep across hosts can act on the exit code without parsing stdout.

**Unreleased build (hotfix from a working tree):** there is no supported path
for this, and no `make deploy` — cut the release (see the release flow), then
`zulip-acp update`. A binary built from a working tree reports
`X.Y.Z-dev+<sha>.dirty`, is invisible to `dist.lock`, and the next converge
silently disagrees with the host.

**Brew-managed host:** the verb is still `zulip-acp update`. It detects the keg
and runs `brew update && brew upgrade kfet/ai/zulip-acp` itself — the
fully-qualified formula out of the keg's install receipt, so a same-named
formula from another tap cannot bind. Recycle afterwards as usual.

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
- **`Text file busy`** — a running executable cannot be written in place, which
  is exactly why `zulip-acp update` exists: it stages the download beside the
  binary and `rename(2)`s it over, replacing the directory entry while the live
  process keeps its old inode. Do not reproduce that by hand — use the
  subcommand. (The reload copes with the unlinked inode: `reload.SelfPath`
  strips the `"(deleted)"` marker `/proc/self/exe` reports.)
- **A hand-placed binary is invisible to the lock** — `dist.lock` is what
  converge believes a fleet host runs. Anything installed around converge makes
  the next `--apply` disagree with the host, or silently "downgrade" it back to
  the locked version.
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

- [ ] Target version confirmed (latest release).
- [ ] Fleet host? → converge (`--tot`, commit the lock, `<bot> --apply`) and nothing else.
- [ ] Otherwise: `zulip-acp update` — never a hand-placed binary.
- [ ] Install method + supervisor identified; `ExecReload` present in the unit.
- [ ] Relay recycled with `reload` (or `restart`, if unit-file/first-cutover/dead).
- [ ] `/proc/<MainPID>/exe --version` matches target; service active; journal
      shows `resuming inherited event queue`.
