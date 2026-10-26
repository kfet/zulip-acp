---
builtin: true
name: promise
description: Load this whenever you are about to say you will do something later — check back, follow up, deploy when it lands, report once it ships, remind, watch for, verify after — or whenever you hand work to another agent and intend to report its outcome. Turns a stated future intention into a real wake mechanism, because your turn ends and nothing wakes you by default.
---

# promise — never state a future intention without a hook

Your turn ends when you finish replying. **Nothing wakes you.** No timer fires,
no background thread resumes, no completion of a spawned agent reaches you.

So a sentence like:

> "I'll deploy once it lands."
> "I'll check back in a bit."
> "I'll let you know when the release is out."

is, by default, **a lie with no mechanism behind it**. The user believes a
follow-up is coming. Nothing is coming. They find out an hour later by asking.

This skill exists because that has happened. Do not let it happen again.

## The rule

> **Before the reply that contains the promise leaves your hands, the wake
> mechanism must already exist and be confirmed.** Not after. Not "I'll set it
> up next turn" — there is no next turn.

If you cannot create a hook, do not promise. Say plainly: "I have no way to
notice that; ping me when it lands." That is honest and costs nothing.

## Mechanisms, best first

**1. The doer notifies you.** If a spawned agent, worktree session, or fleet
member is doing the work, make *it* report:

```
send_session id_prefix=<session> deliver_as=followUp
  "When you are completely done — after ship-it merges and releases, or if you
   stop early for any reason — notify the session that spawned you:
   send_session cwd=<your cwd> deliver_as=prompt, with a short message stating
   the outcome, whether it actually worked or you fell back, and anything the
   operator must do by hand.
   That session's turn ends when it replies, so it cannot observe you
   finishing unless you tell it."
```

Best because it fires on the **actual event**, not on a guess about timing.

**2. A scheduled self-prompt.** `mcp__relay__schedule` with `in: "20m"`. The
prompt text must be a **complete instruction to a stranger** — it arrives with
conversation history, but write it as if it does not. Name the paths, the
commands, the success check, and what to do when the answer is "not yet".
For a repeating check, use one `every:` schedule; do not re-schedule from a
scheduled turn (acp-kit refuses past depth 3). Put the exit condition and the
schedule's own id in the text; when the condition holds, the turn reports and
calls `unschedule` on itself (allowed at any depth).

**3. A durable reminder.** If available, `watcher_add` (kind `time`, from the
fir-exts watchers package — not every host has it) fires on the next turn
at/after its due time. Weaker: it needs a turn to happen. Use it when there is a human in
the loop anyway, or with `repeat` for a poll-until-true check.

**4. `wait`** — only for something settling within the current turn, seconds to
minutes. It blocks. Never use it for another agent's whole task.

## Use two when it matters

The doer-notifies hook fires on the event but dies with the doer — an agent
that crashes, is killed, or hits a rate limit reports nothing. Pair it with a
scheduled backstop. Overlapping hooks are cheap; a dropped promise is not.

## Verify the hook exists

Creating a hook can fail **silently**:

- During a relay restart the loopback MCP socket is gone, so
  `mcp__relay__schedule` returns *tool not found*. The promise survives; the
  schedule does not.
- `send_session` into a session that has already finished is accepted and
  discarded. A spawned agent that shipped and tore down its worktree is gone —
  its followUp goes nowhere.

So: **confirm, don't assume.** `mcp__relay__list_schedules` after scheduling.
`observe_session` to check the target is still live before sending into it. If
the tool errored, retry or fall back to another mechanism — do not shrug and
keep the promise anyway.

## When the hook fires

Do the thing before reporting. If the work is not done yet, say briefly where
it is and **re-arm** — a check that fires once and finds "not ready" has
consumed the promise without keeping it.

## Checklist

- [ ] Did this reply state or imply a future action?
- [ ] Does a mechanism now exist for it — doer-notifies, schedule, `watcher_add` reminder?
- [ ] Was creating it **confirmed**, not assumed?
- [ ] Is there a backstop if the primary hook dies with its process?
- [ ] Does the fired prompt stand alone, and does it re-arm on "not yet"?
