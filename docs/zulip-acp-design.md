# zulip-acp — design

`zulip-acp` bridges a **self-hosted Zulip server** to an ACP-speaking coding
agent (`fir --mode acp`, Claude Code, …) over stdio. One binary, no MCP surface,
no inbound HTTP.

It is the third relay in a family:

| relay | surface | inbound transport |
|---|---|---|
| `poe-acp` | Poe server bot | HTTP + SSE (inbound) |
| `slack-acp` | Slack | Socket Mode (websocket) |
| **`zulip-acp`** | self-hosted Zulip | **`/events` long poll (outbound only)** |

All ACP-side machinery is shared in [`acp-kit`](https://github.com/kfet/acp-kit):
the agent process wrapper (`client`), session lifecycle and stable per-conversation
cwds (`state`), system-prompt composition (`sysprompt`), and the status-line wire
contract (`statusline`). **A new surface relay is a protocol adapter only.**

## Goals

- A Zulip **topic** is a conversation. Ask a question in a topic, get an answer;
  follow up in the same topic and the agent remembers.
- **Never drop output.** Zulip's message ceiling is low and its truncation is
  silent; losing the tail of an answer is the failure mode this design is
  organised around.
- **Dial out only.** The relay runs on a tailnet with no public exposure and no
  tunnel.
- Restart-safe. The topic is the state; local files are a cache.

## Non-goals (v1)

Direct messages, inbound attachment handling, a self-drive escape hatch, and
setup wizards. See [`BACKLOG.md`](../BACKLOG.md) for each, with reasons.

## Shape

```
        Zulip                        zulip-acp                       agent
  ┌──────────────┐          ┌─────────────────────────┐        ┌────────────┐
  │  /register   │◀────────▶│ zulipproto.Runner       │        │            │
  │  /events     │  long    │  · last_event_id cursor │        │  fir       │
  │              │  poll    │  · queue re-register    │        │  --mode    │
  └──────────────┘          └───────────┬─────────────┘        │  acp       │
                                        │ Event                │            │
                            ┌───────────▼─────────────┐        │            │
                            │ handler                 │        │            │
                            │  · gating               │        │            │
                            │  · journal: conv-id     │        │            │
                            │  · state.Manager ───────┼───────▶│  session   │
                            │  · streamingSink        │◀───────┤  updates   │
                            └───────────┬─────────────┘        └────────────┘
                                        │ Append (pure)
                            ┌───────────▼─────────────┐
                            │ rollover.Splitter       │
                            │  · budget in code points│
                            │  · fence-aware seals    │
                            └───────────┬─────────────┘
  ┌──────────────┐                      │ Post / Edit (300ms tick)
  │  /messages   │◀─────────────────────┘
  └──────────────┘
```

## The three decisions that matter

### 1. Rollover, because Zulip truncates silently

`MAX_MESSAGE_LENGTH` is **10000 Unicode code points**, and Zulip does not reject
an oversized message. It returns `{"result":"success"}`, stores 10000
characters, and appends `\n[message truncated]`. No error, at any layer. A relay
that streams a long turn into one edited message loses the tail and never finds
out.

So `internal/rollover` owns the whole problem, and **HTTP code never makes a
split decision**. It is a pure component consuming a dumb poster interface
(`Post` / `Edit`), with zero Zulip imports — which is both a design discipline
and what keeps it promotable to `acp-kit/chunker` later.

Its invariant, asserted by its tests:

> `concat(RawSlices()) == Transcript()`, and no message payload — decorations
> included — exceeds the budget **in code points**.

Two layers keep that true. **Layer 1 is raw**: the transcript, the byte offsets
where messages are cut, and the fenced-code-block parity, all computed over raw
text. **Layer 2 is decoration**: the continuation marker, the reopened fence,
the closing fence and the seal marker, frozen per sealed message. Synthetic
fences never feed back into the parity — that is the specific bug that makes
message 3 and beyond invert if you track state over the decorated payloads.

Consequences worth stating:

- **Headroom is reserved before the cut is chosen**, not after. Reserving it
  after is the classic off-by-one where a slice fits pre-marker but not
  post-marker.
- **Sealed messages are immutable.** The edit that seals a message is the last
  write it ever receives. Only the tail is edited.
- **Splits prefer line boundaries.** A mid-line split happens only when a single
  line exceeds the whole budget, and then only on a rune boundary.
- **Sealing is greedy and monotone**, so split points are a function of the
  transcript prefix alone. `TestChunkingDoesNotChangeSplits` pins that the same
  text chunked six different ways produces byte-identical messages.
- The live tail's open fence is **left open**. Zulip renders an unterminated
  fence as code-to-end-of-message, which is the right look mid-stream, and
  auto-closing it would break the prefix invariant.

Default budget is **9500**, not 10000, so a server-side change in how the limit
is counted cannot cost output.

### 2. Conv-id indirection, because topics get renamed

The topic is the session identity — but a topic string is arbitrary user text
(spaces, slashes, emoji), so it cannot be a state-directory path component, and
Zulip lets you rename a topic with `propagate_mode=change_all`.

`internal/journal` gives every `(stream_id, topic)` pair an opaque conv-id at
first contact, and `state.Manager` is keyed on that conv-id **forever**. A
rename rewrites the alias; the ACP session and its working directory never move.

The obvious alternative — key on the topic, migrate the map entry on rename — is
a trap. `state.Manager` has **no rekey operation**, so the stale entry keeps the
*same* agent session id, and idle GC eventually reaps it and calls
`DropSession`, killing a live session out from under the new key.

Note also that the key is `(stream_id, topic)`, never topic alone: the same
topic string in two channels is two conversations.

**The topic is truth; the journal is a cache.** On conflict the topic wins, and
an unknown topic is simply a new conversation. Losing the file costs continuity
of naming, never correctness.

### 3. Two turn shapes, because ambient turns may be declined

Both shapes open the same way: the triggering message gets an emoji reaction
(`ack_emoji`, default `:eyes:`) the moment it is accepted, removed on every
exit path — success, error, abstain, cancellation. Zulip has no typing
indicator, and a reaction is the only acknowledgement that is instant, adds
nothing to the topic, and can be **retracted**, which is what makes it safe on
a turn that may end in silence. Removal runs on a `context.WithoutCancel`
context so a superseded turn still cleans up after itself, and every reaction
call is non-fatal: a turn is never failed over decoration.

- **Addressed** (an `@-mention`): stream. An eager placeholder goes up
  immediately — a cold agent takes seconds — a spinner animates it, and a 300ms
  watchdog publishes the splitter's pending state.
- **Ambient** (any message in a topic the relay has already engaged, with a
  sentinel configured): buffer. The turn runs through acp-kit's
  `PromptAbstainable`, and the *answer* is not posted until the agent's verdict
  is in. Post-then-delete would ping the phone and then gaslight the reader.

  The **placeholder**, however, does not wait for the end of the turn. The
  negative verdict is knowable far earlier: once the accumulated message text
  is non-empty and is no longer a prefix of the sentinel, no continuation of
  the stream can ever equal it, so a reply is certain. `handler.sentinelWatch`
  sits above the `ValidatingSink`, observes `AgentMessageChunk` text, and fires
  exactly once on divergence to post the placeholder and start the spinner. It
  deliberately does **not** call `ValidatingSink.Commit`: `Commit` resets the
  accumulated text, which would make `PromptAbstainable` see `""` and declare a
  false abstain. The prefix test is on trimmed text, so the leading newlines
  some agents emit before the sentinel do not read as a reply.

  Streaming the answer itself live on the ambient path needs an acp-kit change
  (a `ValidatingSink.Release` that flushes and switches to pass-through); see
  BACKLOG.md.

Thought chunks are **force-hidden** on the ambient path regardless of
`hide_thinking`: a thought that reached the surface before the verdict could not
be retracted.

Gating, in order:

1. Never act on our own message. First, before any allowlist, so a widened
   allowlist can never reorder it.
2. Never act on any other bot's message either. Zulip posts topic moves and
   welcome notices as cross-realm system bots (`sender_realm_str:
   "zulipinternal"`), which land in engaged topics.
3. Channel allowlist (the resolved `channels` config), then optional user
   allowlist.
4. Then: an `@-mention` starts a conversation; anything else is answered only in
   an already-engaged topic. **The topic is the membership record**, which is
   why engagement survives a restart with no extra state.

## Streaming and back-pressure

The sink performs **no I/O**. It renders an `acp.SessionNotification` into text
and appends it to the pure splitter; publishing happens on the coalescing tick.
That separation is what stops a slow Zulip edit from back-pressuring the ACP
stream.

Zulip sustains ~15 edits/sec unthrottled (measured, and pinned by
`test/live_test.go`), so the 300ms interval is a kindness to the reader — every
edit re-renders the whole message server-side and again on the phone — not a
rate limit. **Slack's `minInterval = time.Second` and its front-trimming are
deliberately not ported**: they serve limits Zulip does not have, and trimming
discards output when rollover is cheap.

`rollover.Flush` releases its plan mutex around each network call, so a separate
`ioMu` serialises every method that talks to the poster. Without it the
coalescing watchdog and `Close` can both observe the same unposted message and
both `Post` it. Regression test: `TestConcurrentFlushDoesNotDoublePost`.

## Restart semantics

A relay restart kills the child agent, so any in-flight turn is dead regardless.
The correct behaviour is therefore *not* to resume it:

- On startup, every tail message recorded in the journal is edited to append
  `*(relay restarted — turn interrupted)*`. Sealed messages are never touched.
- The next inbound message in the topic goes through `state.Manager`, which
  resumes or recreates the ACP session against the conversation's stable cwd.

A queue narrow cannot express a union of channels (narrow terms are a
conjunction), so a relay serving more than one channel registers an
**unnarrowed** queue and filters with the channel allowlist it must enforce
anyway. Over-delivery is cheap; under-delivery is silent.

Event queues get the same treatment. `queue_id` and `last_event_id` are held in
memory only — queues die on server restart, so persisting them is false comfort.
`BAD_EVENT_QUEUE_ID` is **routine**: re-register, reset the cursor, log at info.

## Never drop output

Beyond rollover, two backstops:

- The relay refuses to put an oversized body on the wire at all
  (`zulipproto.SendMessage` / `EditMessage`), rather than letting Zulip truncate
  it silently. A caller that hits this has a bug.
- If posting fails anyway — the realm closed its edit window, the server is
  down, or Zulip's renderer refuses a body that is legal by length but expensive
  (HTTP 400 "Unable to render message"; ~1000 consecutive emoji is enough) — the
  handler uploads the whole transcript as `answer.md` and posts a link. Uploads
  are raw bytes and are never rendered, so that path cannot fail the same way.

## Attachments

Each conversation has a stable working directory. Anything the agent writes into
`<cwd>/outbox/` is uploaded at the **end of the turn** and linked from the
answer, then moved to `outbox/.sent/` so a follow-up does not re-upload it.
End-of-turn only: uploading opportunistically races the agent still writing the
file. The convention is documented to the agent in the built-in system prompt —
an agent cannot use a convention it is never told about.

## What was deliberately not ported from slack-acp

| slack-acp | why not |
|---|---|
| `PostStreamer` 1/sec throttle, `maxChars` front-trimming | serves Slack limits Zulip does not have; trimming *discards output* |
| `thread_ts` plumbing | Zulip's reply target is `(channel, topic)`; there is no parent message, and porting it re-introduces state that can desync |
| Socket Mode ack semantics | meaningless here; the `last_event_id` cursor is the real contract |
| `internal/verify`, `internal/probe`, `slackproto/manifest` | Slack app manifests and request-signature verification. Basic auth has no signing secret, so there is nothing to verify — inventing an equivalent would be theatre |
| the self-drive hatch, `initcmd`, `installsvc` | accretions, not core; each is 100%-coverage surface with no demand here |

## Layout

```
cmd/zulip-acp/          flags + wiring (excluded from the coverage gate)
internal/config/        JSON config, DisallowUnknownFields
internal/handler/       gating, turn execution, streaming sink, outbox, poster
internal/journal/       (stream_id, topic) → conv-id alias map + tail ids
internal/rollover/      pure code-point splitter — NO Zulip imports
internal/statusline/    Zulip-markdown mood/plan header
internal/sysprompt/     built-in Zulip formatting block
internal/zulipproto/    HTTP Basic client + /events long-poll runner
test/                   live-server tests (ZULIP_LIVE=1), coverage-exempt
```
