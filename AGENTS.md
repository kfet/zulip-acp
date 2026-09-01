## What this is

`zulip-acp` bridges a **self-hosted Zulip server** to an ACP-speaking coding
agent (`fir --mode acp`, Claude Code, …) over stdio. One binary, no MCP
surface. Each Zulip **topic** (scoped by its channel) maps 1:1 to an ACP
session inside a shared agent process.

It is the third relay in a family: `poe-acp` (Poe server bot, HTTP + SSE),
`slack-acp` (Slack, Socket Mode), `zulip-acp` (here). All ACP-side machinery
is shared in `github.com/kfet/acp-kit`.

See [docs/zulip-acp-design.md](docs/zulip-acp-design.md) for the design and
[docs/zulip-protocol-reference.md](docs/zulip-protocol-reference.md) for the
wire details.

## How to work here

Use idiomatic Go. Keep it simple.

Prefer `sync/atomic`, `sync.Once`, and channels over manual mutex management when appropriate.

Do not ignore any issues, address them promptly, even if preexisting. Do not postpone any work, even if it seems daunting — just break it down into smaller tasks. **Never dismiss a problem as "pre-existing" or "out of scope" — you own this entire codebase. If you see it, you fix it.**

Do not leave incomplete or stubbed code. Ensure all code is functional and tested.

## Repository layout

```
cmd/zulip-acp/          entry point: flags + wiring
docs/                   design doc + Zulip protocol reference
internal/config/        JSON config loader (DisallowUnknownFields)
internal/handler/       event → ACP prompt; streaming sink; topic poster; commands
internal/journal/       (stream_id, topic) → conv-id alias map + tail ids
internal/rollover/      pure 10k-code-point message splitter (NO Zulip imports)
internal/statusline/    Zulip-markdown status header renderer
internal/sysprompt/     built-in Zulip-formatting system prompt
internal/zulipproto/    HTTP Basic client + /events long-poll runner
test/                   live-server integration tests (ZULIP_LIVE=1)
```

## Think before you specialise

Before implementing a fix or feature inside a specific package, stop and ask:
**is this actually unique to this layer, or does it belong elsewhere?**

For every non-trivial change, first ask the cross-repo question: **does this
belong in `acp-kit`?** acp-kit is the home for primitives every relay needs —
ACP client wrapper, session state, debug log, skills, attachments, sysprompt
composition. If the change is about how a relay talks to an ACP agent
(handshake, capabilities, model probing, resume) it almost certainly belongs
in acp-kit so `slack-acp` and `poe-acp` get the fix once. If it is about the
Zulip wire protocol it stays here.

- Zulip protocol concerns (events, queues, message shapes, uploads) →
  `zulipproto`. **Never** push these to acp-kit.
- Agent-process concerns (spawn, stdio, ACP framing) → `acp-kit/client`.
- Session lifecycle (cwd, GC, resume) → `acp-kit/state`.
- **`internal/rollover` must never import anything Zulip-specific.** It is a
  promotion candidate for `acp-kit/chunker` (see `BACKLOG.md`); the import
  graph is what keeps that option open. HTTP code must never make a split
  decision.
- **The `!command` broker lives in `acp-kit/command`, shared with `poe-acp`.**
  Do not add a command, an alias or a rendering tweak here that belongs there —
  both relays must offer the same surface. What stays in
  `internal/handler/command.go` is only what Zulip knows: the `Controller`
  implementation, the `/me` / `/poll` / `/todo` pre-filter, the `!!` escape
  and the unknown-command reply.

## Git

More than one agent may hold a worktree of this repository at the same time.
Before any history rewrite, force-push, or ff-merge to main, run
`git worktree list` and check **every** worktree's index — a rewrite here does
not clean another worktree's staged files, and a stale index can silently
resurrect deleted files on the next merge.

Git commands that require an editor (`git rebase --continue`, `git commit`,
`git merge --continue`) will open vim non-interactively and hang. Always
prefix such commands with `GIT_EDITOR=true`:

```bash
GIT_EDITOR=true git rebase --continue
GIT_EDITOR=true git commit
```

When the user says "rebase to main", they mean local `main`, not `origin/main`.

When merging a feature branch back to main, always use `git merge --ff-only`
to keep a linear history. Rebase the branch first if needed.

## Stuck loops

If you have run the same command (`go test`, `go build`) more than 5 times
since your last file edit, you are in a stuck loop. Stop. Do not re-read any
file you have already read this session. Rewrite the problematic file
completely from scratch.

## Build and test

Run `make test` to verify your changes. Always finish every task with
`make all` to confirm the full build and test suite passes (vet,
test-race-cover with a 100% coverage gate, 5 cross-builds, native build,
check-licenses).

When fixing a regression, **write the test first** so it fails before your
fix, then make it pass.

Live-server tests live in `test/` and only run with `ZULIP_LIVE=1` plus
`ZULIP_SITE` / `ZULIP_EMAIL` / `ZULIP_API_KEY` set. They are excluded from
the coverage gate: they are evidence about the *server*, not about our code.

## Testing — avoid wall-clock timeouts

- **Channels over polling** — use `chan struct{}` signals, `sync.WaitGroup`, or
  callbacks instead of `require.Eventually` with arbitrary timeouts.
- **No `time.Sleep` in tests.** Sleep-based tests are flaky under CI load and
  the race detector. Inject clocks and tick channels instead.
- **Callbacks in Config, not after init** — if a struct spawns goroutines on
  creation, callbacks must be set via the config struct *before* construction.
- **Cover every branch deterministically — a flaky line fails the 100% gate at
  random.** Never rely on timing to cover an error branch: drive it explicitly,
  or if it is genuinely unreachable route it through a `*_must.go` panic-helper
  so it is excluded from the count.

## Zulip traps that bite

- **`MAX_MESSAGE_LENGTH` is 10000 *code points*, and Zulip truncates
  SILENTLY** — a 10001-char POST returns `{"result":"success"}` and stores
  10000 chars with `\n[message truncated]` appended. Count code points
  (`utf8.RuneCountInString`), never bytes. This is the #1 correctness risk in
  the project; `internal/rollover` exists solely because of it.
- **`message_content_edit_limit_seconds` defaults to 600** on a fresh install,
  which makes streaming PATCHes fail with 400 on any turn longer than ten
  minutes. The deployment must set it to unlimited. Documented in the README.
- **Event queues die on server restart.** `queue_id` and `last_event_id` are
  in-memory only; persisting them is false comfort. `BAD_EVENT_QUEUE_ID` is
  routine — re-register and log at info, not error.
- **Never echo the bot's own messages back into the agent.** Filter on sender
  id, before any allowlist.
- **The topic is truth; the journal is a cache.** On conflict, the topic wins.

## Changelog

When making non-trivial changes, add an entry under `## [Unreleased]` in
`CHANGELOG.md` using the appropriate subsection (`### Added`, `### Fixed`,
`### Changed`, `### Removed`). One line per change, newest first within its
subsection. Do not bump `VERSION`; that happens during release.

## Release

`make publish` pushes `main + vVERSION` to `origin`; `release.yml` runs
`make all` + `make notices` and then GoReleaser, which publishes the GitHub
release and updates `Formula/zulip-acp.rb` in the shared `kfet/homebrew-ai`
tap. Users install with `brew install kfet/ai/zulip-acp`.

## Caveman Mode

Ultra-compressed communication. Slash token usage ~75% by speaking like
caveman while keeping full technical accuracy.

### Grammar
- Drop articles (a, an, the)
- Drop filler (just, really, basically, actually, simply)
- Drop pleasantries (sure, certainly, of course, happy to)
- Short synonyms (big not extensive, fix not "implement a solution for")
- No hedging (skip "it might be worth considering")
- Fragments fine. No need full sentence
- Technical terms stay exact
- Code blocks unchanged. Caveman speak around code, not in code
- Error messages quoted exact

### Pattern
`[thing] [action] [reason]. [next step].`

Not: "Sure! I'd be happy to help. The issue is likely caused by..."
Yes: "Bug in auth middleware. Token expiry check use `<` not `<=`. Fix:"

### Boundaries
- Code: write normal. Caveman English only
- Git commits: normal
- PR descriptions: normal
- User say "stop caveman" or "normal mode": revert immediately
