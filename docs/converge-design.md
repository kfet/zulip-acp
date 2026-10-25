# converge.sh fix — advisor-approved plan

Root cause: two owners of "swap + reload". `zulip-acp update` discovers every install, swaps, and reloads; converge also swaps (gh fallback), probes, SIGHUPs, and polls. 2026-10-25 failure: update swapped the target + reloaded, then exited nonzero because of an unrelated stale root-owned /usr/local/bin copy (now removed); converge died after the real work. Fix ownership, not symptoms.

Ownership rule (comment at top of converge.sh): the Go binary owns the file swap; converge owns rendering, ONE reload per unit, and verification. Nothing reloads twice.

Keep bash. Target <=450 lines.

1. Go `zulip-acp update`: add `--binary PATH` (single install, no discovery) and `--no-reload` (swap only). In discovery mode, an install you cannot write is a `warn:` line, not a failure; exit code reflects only requested targets; never nonzero after a successful target swap. Unit tests incl. stray root-owned copy -> exit 0 + warning. Release it, then bump SELFUPDATE_MIN in converge to it.
2. converge cuts: delete gh-download fallback + "no way to install" die (below SELFUPDATE_MIN: one-line refusal with install.sh cmd). Delete probe_state, stale_decision, recycle_plan, SIGHUP path, 60s poll loop. install_binary = `$b update --binary $b --version v$want --no-reload -y`, then `$b --version` must equal want, else die.
3. Reload: exactly one `systemctl --user reload <unit>` per unit, only if binary/config/unit/fir changed. Verify /proc/$MainPID/exe --version == want, bounded 30s, else die with last 20 journal lines. Timeout = failure. Self-unit case (converge runs inside the target relay: MainPID is an ancestor, walk PPid chain or compare /proc/self/cgroup): skip the wait, print "reload pending: this relay reloads when the current turn ends", exit 0. Rendered unit must keep ExecReload=/bin/kill -HUP $MAINPID.
4. Group by host: binary + fir + fir-exts once per host; config + unit + reload per bot. Two bots on one host -> one update call, two reloads.
5. Simplify local-vs-ssh where safe (prefer one code path; keep a local transport only if self-ssh is unavailable — check; the update skill says hosts usually cannot ssh to themselves, so a simple, explicit local test is likely still needed — keep it minimal).
6. `--apply` prints the full plan before any write. Second `--apply` on an up-to-date fleet writes nothing, reloads nothing, exits 0 (acceptance test).
7. Test harness tests/converge_test.sh: CONVERGE_RSH fake runs locally in temp $HOME; stub zulip-acp, fir, systemctl, readlink on PATH recording args. Cases: fresh host; version bump; config-only; idempotent rerun; stray root install; host below SELFUPDATE_MIN; two bots one host (one update, two reloads); reload timeout fails; self-unit pending. Assert call sequence. Wire into make test + CI.
Must stay: --tot + dist.lock, enforce_require, config/unit render with diff + .bak-STAMP, credentials env_file refusal, fill_quick_models.
Update skills/update SKILL.md and deploy skill to match.

## Deviations from the plan text (advisor-approved)

1. **Recycle is one action per unit, not always `reload`.** A SIGHUP re-exec
   keeps the old argv and environment, so a changed unit file never takes
   effect through a reload, and a unit that is not running cannot be
   reloaded. `recycle_action` picks exactly one: not active → `enable --now`;
   unit changed → `restart` (loud warning: drops in-flight turns); already
   `reloading` → nothing; binary/config/fir moved or running image old →
   `reload`; else nothing.
2. **Self unit + unit change: refuse.** Converge writes the files (inert until
   a recycle), refuses the restart that would kill its own turn, and exits
   nonzero with the command to run from outside the relay.
3. **`reloading` is pending.** A SIGHUP is already draining; a second one is
   not sent.
4. **A binary that does not know `--binary`** (capability probe:
   `update -h | grep -- -binary`, not a version guess) gets
   `update --version vX </dev/null` — with no tty and no `-y` that is the
   same single-binary swap, no discovery, no reload. Only a binary below
   0.19.0 (or a missing one) gets the `install.sh` refusal. This lets the
   current fleet converge without a hand bootstrap.
5. **Flags.** `--target-root` and `--force-local` are gone (`CONVERGE_RSH`
   is the test transport). `--local` stays as the one explicit "this host is
   me" override. `converge.sh <bot>...` and `--all` group bots by host.
6. **Tests** are `test/converge_test.sh` (the repo uses `test/`).
7. **Size.** The kept parts (semver, `require`, validation, quick_models,
   `--tot`) are most of the file: about 700 code lines in total, against
   about 900 before. The swap/probe/recycle code shrank from about 560 lines
   to about 330.
