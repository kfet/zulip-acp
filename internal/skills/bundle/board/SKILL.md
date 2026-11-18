---
builtin: true
name: board
description: Explain or set up the relay's board — routes, views, notify, lifecycle auto-archive and topic_state. Use when the user asks about the board, where it is, why a topic is (or is not) listed, or wants to add or change a view, notify entry, route or lifecycle rule.
---

# Board

A board is a channel where humans drop new tasks and see the state of every
task topic. It is built from five config keys plus the `topic_state` tool.
Nothing here is fixed per relay: every channel, topic and duration comes from
the config. Never assume a channel name — read it (§1).

## 1. Find this relay's board

The relay reads one JSON file: the `-config` flag of its service (default
`~/.config/zulip-acp/config.json`). Never print `bot_api_key`; use `jq`:

```bash
F=~/.config/zulip-acp/config.json   # or the -config path of the service
jq '{routes, views, notify, lifecycle, archive_channel}' "$F"
```

- `views[].at` and `notify[].at` are `"<channel>/<topic>"`: the board message
  and the notices are there. The text up to the first `/` is the channel.
- `views[].sources` are the channels whose topics the board lists.
- `routes[].from` is usually the board channel too: humans post there.
- An empty or missing key means that part of the board is off.

`zulip-acp config-schema views` (or `routes`, `notify`, `lifecycle`)
prints every key with its doc. Read it before you change one; see the
`config` skill for the safe edit flow.

## 2. The parts

- **`routes`** `{from, to, link_back}`: a human post that starts a new topic
  in `from` moves to `to`, and the turn runs there. With `link_back` the relay
  posts `↪ moved to …` in the source topic. A post in a view or notify topic
  is never moved. `to` must be a served channel, and realm policy must let the
  bot move messages between channels; else the start log says routes are OFF.
- **`views`** `{at, sources, groups, stale_after, collapse}`: ONE message the
  relay edits. It lists the topics of `sources`, grouped by state (`groups`:
  title + states, in order; states are the topic states plus `running` and
  `open`). A `waiting` topic idle longer than `stale_after` (e.g. `"72h"`)
  shows as `stalled`, and a `needs_you` topic as `stale`; both go below the
  fresh topics of their group. `collapse: {states, show}` puts the groups of those
  states last, in a spoiler block with the count and the `show` newest. The
  view refreshes about 5s after a change, every hour, and at start.
- **`notify`** `{at, on_enter, sources}`: an edit does not notify, so the
  relay posts one short `🔔` message when a topic ENTERS an `on_enter` state,
  and deletes it (or strikes it, past Zulip's delete time limit) when the
  topic leaves it.
- **`lifecycle`** `[{when: {states, idle}, action: "archive", prefix_source,
  sources}]`: archive a topic by itself, through the `!archive` path, so
  `archive_channel` must be on. Allowed states: `done`, `fyi`, `waiting`,
  `open`. A topic that runs, has a schedule, or is `needs_you`/`blocked` is
  never archived. Idle is read from the topic's newest message, so a new
  message resets it. Rules run at start and every hour. A topic older than
  the realm's move time limit is skipped (the start log says the limit).
- **`topic_state`** (agent tool) and `!state` (humans) set what the user must
  do with a topic: `needs_you`, `fyi`, `waiting`, `blocked`, `done`, with a
  short reason. This is what the board groups by. Set it only when it
  changes, and put the question itself in the topic: the reason shows only on
  the board and in `!inbox`. A human message (not a `!` command) in a
  `needs_you` topic clears the state to `open` before the turn starts.

An archived, deleted, or moved-away topic leaves the board and loses its
notice.

## 3. Gotcha: a new key needs a restart, not a reload

`systemctl --user reload zulip-acp` (SIGHUP) re-reads the config with the
RUNNING binary. If the config has a key that binary does not know (you added
`lifecycle` or `collapse` before the update landed), the reload is refused:

```
SIGHUP reload refused: config … is invalid: parse config: json: unknown field "…"
```

The relay keeps its old config. `check-config` run with the OLD binary fails
the same way. Fix: first update the binary (see the
`update` skill), then do a HARD restart so the new binary reads the file:

```bash
zulip-acp check-config -config "$F"   # with the NEW binary: must print OK
systemctl --user restart zulip-acp    # the unit name may differ; launchd: see the update skill
```

A restart can cut a running turn short; tell the user first. Then verify in the log:

```bash
journalctl --user -u zulip-acp -n 40 --no-pager | grep -E 'view:|notify:|lifecycle|route|refused|ERROR'
```
