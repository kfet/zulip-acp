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

## Non-goals

Inbound attachment handling, a self-drive escape hatch, and setup wizards. See
[`BACKLOG.md`](../BACKLOG.md) for each, with reasons.

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

#### One key type, two conversation shapes

Zulip has exactly two conversation shapes, and `journal.Key` expresses both
rather than the journal carrying a second, parallel map:

| shape | key | index |
|---|---|---|
| channel topic | `(StreamID, Topic)` | `c\0<stream_id>\0<topic>` |
| direct message | `UserIDs` — the participant set, sorted, deduped, **including the bot** | `d\0<id>,<id>,…` |

`len(UserIDs) > 0` is the discriminant, and the leading tag byte keeps the two
namespaces disjoint for good, so no channel key can ever collide with a DM key
however the numbers line up. Sorting is what makes the DM key stable: Zulip's
`display_recipient` order is not contractual, and an order-sensitive key would
fork a fresh session on every message.

The key is flattened into the persisted `Conv` object, so the pre-DM on-disk
shape (`{"id","stream_id","topic"}`) still loads, still writes back byte-alike,
and needs **no version bump** — a DM is simply the entry that carries
`user_ids`. A journal from an older release loads as channel conversations
unchanged.

`Rename` is a channel-only operation and structurally cannot touch a DM: a DM
has no topic, and its key is in the other namespace. The handler additionally
drops any `update_message` event with no channel id before it gets that far.

### 3. Two turn shapes, because ambient turns may be declined

Both shapes open the same way: the triggering message gets an emoji reaction
(`ack_emoji`, a bare emoji name, default `eyes`) the moment it is accepted, removed on every
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

### The end-of-turn repost (`repost_on_close`)

Zulip generates a mobile push notification when a message is **created**, and
never when one is edited. Streaming is edits, so every push carried the eager
`Thinking...` placeholder and no phone ever saw an answer.

So when the turn finishes — after `split.Close`, which is what folds in the
outbox attachment links and any `(stopped: …)` note — `rollover.Splitter.Repost`
re-posts the chain as **new** messages and deletes the originals. The web
experience is untouched: the placeholder and its in-place edits are exactly as
before, right up to the swap.

Three decisions worth keeping:

- **The whole chain is recreated, not just the first message.** Deleting only
  the first would move it below its own continuations. The cost, stated
  plainly: an N-message turn fires N notifications instead of one. N is 1 for
  almost every turn.
- **Post before delete, always.** A failure anywhere leaves the fully-edited
  original chain in place, so output can never be lost; the worst case is a
  visible duplicate. `Repost` is a no-op unless `Start` actually seeded a
  placeholder — a chain whose first message was created carrying real text
  already notified correctly.
- **A refused delete trips a process-wide circuit breaker.** Delete and post
  ride the same permission surface: if the realm forbids the bot deleting its
  own messages (`delete_own_message_policy`) or the delete window has closed,
  every turn would post a copy and fail to retract the original — permanently
  doubled output in every topic, which is worse than the bug being fixed. The
  first `rollover.ErrRetract` sets `Handler.repostBroken`, logs once, and the
  relay degrades to the pre-repost behaviour for the life of the process. The
  breaker lives on the Handler, not the Splitter, because a Splitter is
  per-turn and would relearn the lesson forever.

`"repost_on_close": false` turns the whole thing off without a rollback.

Gating, in order:

1. Never act on our own message. First, before any allowlist, so a widened
   allowlist can never reorder it.
2. Never act on any other bot's message either. Zulip posts topic moves and
   welcome notices as cross-realm system bots (`sender_realm_str:
   "zulipinternal"`), which land in engaged topics.
3. Route by conversation shape.
   - **Channel message**: channel allowlist (`internal/channels.Set`, behind
     the handler's `ChannelSet` interface).
   - **Direct message**: served only with `"dms": true`, and gated by the user
     allowlist alone. The channel allowlist has nothing to say about a DM —
     a DM is in no channel — so `allowed_user_ids` is the *only* thing between
     the realm and a session, which is why `dms` defaults to **off**.
   - Anything else is dropped and logged.
4. Optional user allowlist, identically for both shapes.
5. Then gating, which is where the two shapes genuinely differ:
   - In a channel an `@-mention` starts a conversation; anything else is
     answered only in an already-engaged topic. **The topic is the membership
     record**, which is why engagement survives a restart with no extra state.
   - In a DM every message is addressed to the bot by construction — there is
     nobody else in the conversation to be talking to — so mention-gating is
     **off** and every message is treated as addressed. Group DMs included.
     A DM therefore never takes the ambient/abstain path.

Posting back is the mirror image: `convPoster` binds the splitter's dumb
`Poster` interface to one conversation and makes exactly one decision — a DM key
posts via `POST /messages` with `type=private` and `to` as a JSON array of user
ids, a channel key with `type=stream`. Everything downstream is untouched: an
edit is a `PATCH` on a message id and cannot tell the shapes apart, so streaming
and 10k rollover work on a DM unchanged.

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

## The served channel set

`internal/channels.Set` holds two halves behind one `ChannelSet`:

- **Explicit** — the ids resolved from the `channels` config. Static, and
  authoritative: an explicitly listed channel is served whatever the bot's
  subscriptions do.
- **Followed** — requested with the `"*"` sentinel: the channels the bot is
  *subscribed* to. Seeded from `GET /users/me/subscriptions` at boot and then
  maintained from `subscription` (op `add`/`remove`) and `stream` (op
  `update`/`delete`) events, so the set moves while the relay runs.

Three properties this shape buys:

- An **empty** `channels` list stays fatal. "Serve the whole realm" is now
  expressible, but only as a deliberate `"*"`, never as a missing key.
- A followed queue is **never narrowed**: the set can grow at any moment, and a
  narrow decided at boot would silently under-deliver forever after.
- Events are lost while a queue is dead, so the set is **resynced on every
  registration** (`RunnerConfig.OnRegister`) rather than left to drift until the
  next restart.

The set is written by the event loop and read by the handler on its turn
goroutines, so it is `sync.RWMutex`-guarded, and `Sync` computes its join/leave
diff *under* the lock — the map it publishes becomes shared the instant it is
stored.

### Naming general chat (`autotopic_channels`)

Zulip 11 added "general chat": the empty topic (`""`). It is a real topic as
far as the API is concerned, so the relay would happily hold a conversation
there — and that is the problem. General chat is one shared feed per channel,
so every conversation the relay ever has in it lands in the same place,
interleaved, with no subject anyone can search for or mute.

A channel listed in `autotopic_channels` (a third static id set on
`channels.Set`, alongside `ambient`, keyed by id so a rename cannot drop it)
therefore **moves** an accepted general-chat message to a topic of its own —
`PATCH /messages/{id}` with `topic` and `propagate_mode=change_one` — and
answers there.

Three constraints shape it:

- **The move happens before the conversation is allocated.** `Handler.autotopic`
  runs after every gate and after command dispatch, but *before*
  `journal.Ensure`, so the conversation is created under its final key. Doing it
  later would need a key migration, and the rename path
  (`handleUpdate` → `journal.Rename`) exists for humans retitling a topic, not
  for the relay tidying up after itself.
- **`change_one`, never `change_all`.** The other messages in general chat
  belong to other people's conversations; moving them would be vandalism.
- **A failed move must never cost the turn.** Whether a bot may retopic a
  message is realm policy (`can_move_messages_between_topics_group`, plus the
  `move_messages_within_stream_limit_seconds` time limit), and older servers
  have no
  general chat at all. Any error is logged and the original key is used — the
  relay answers in general chat, exactly as it did before the feature existed.

The name itself comes from `internal/autotopic`, a pure `func(text, now)
string` over the raw markdown: first usable line, mentions and markdown
decoration stripped, whitespace collapsed, truncated on a word boundary to 60
code points, with a `chat <timestamp>` fallback so the result is never empty —
an empty topic *is* general chat, so a falsy name would silently mean "did not
move". Keeping it pure and Zulip-free is what makes it exhaustively
table-testable, and what makes swapping in an agent-generated title later a
one-call change.

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

## The relay command surface

A message beginning with `!` and naming a known command is handled by the
relay and **never forwarded to the agent**. It costs no turn, which is what
makes `!stop` and `!status` useful precisely when the agent is unresponsive.

### Why it exists at all

`!new`. A channel conversation can always be replaced by opening a new topic
— the topic *is* the identity. A **direct message cannot**: its key is the
participant set (see *Conv-id indirection*), which is fixed for as long as
those people exist. Without `!new` a DM is one session forever, with no way
to clear its context. Everything else on the surface is orientation that the
chat client cannot give you: which conv-id, which state directory, which
model, is something still running.

### Where the code lives, and the cross-repo question

The broker lives in **`acp-kit/command`**, shared with `poe-acp`. What stays
here is only what Zulip knows: the `Controller` implementation over the
journal and session manager, the `/me` / `/poll` / `/todo` pre-filter, the
`!!` escape, and the unknown-command reply.

This reverses the call made in v0.5.0, and the reasoning is worth keeping
rather than deleting, because it is the *same* rule reaching the opposite
conclusion on better evidence.

v0.5.0 shipped a small hand-written parse core in `internal/command` and
argued promotion was premature: `acp-kit/statusline` looks like the
precedent — generic core there, Zulip rendering here — but it is not, because
that contract is shared *by construction*, since relays and agent must agree
on bytes on the wire. Nothing was shared yet. The stated rule was
`internal/rollover`'s: keep it local behind import discipline, and **promote
when a second consumer exists to shape the API**.

A second consumer did exist, and the v0.5.0 analysis was simply wrong about
it. `poe-acp` had already carried a mature 655-line broker for months —
`!login` with the two-call `_meta.auth.interactive` bridge, `!help`,
`!status`, `!model`, `!new`, back-compat aliases, and a curated
agent-command passthrough allowlist — all of it tested to 100%. Dismissing it
as "fused with its OAuth broker and not extractable" was an assumption, not a
measurement. The measurement: that file imported exactly **two**
non-`acp-kit` things, `router.SessionStatus` and `router.RelayInfo`, both
plain data structs living in `router` only because that is where they were
first needed. Everything else already spoke `acp-kit/client`.

So the promotion was clean, and inventing a second surface instead was the
expensive mistake — not because the invented one was bad, but because
*inventing at all* discarded a working design and would have left two
brokers to drift. The port moved the package whole, **tests included**; the
977 lines of broker tests are the real reason no copy was left behind, since
acp-kit's 100% gate means they had to land there regardless.

Rendering deliberately stayed in `acp-kit/command` rather than being
duplicated per relay. Poe, Zulip and Slack all read CommonMark-ish markdown
and the strings are legal in all three verbatim — the same alignment
`internal/statusline` already documents. Copying identical prose into each
relay would recreate exactly the fork the move exists to remove. When a
surface genuinely needs different markup, that is the moment to add a
renderer interface, and not before.

Two extension points were added while lifting, both shaped by Zulip's
differences from Poe:

- **`command.TurnStopper`** — an optional capability the `Controller` may
  implement, which is what enables `!stop`. A relay that does not implement
  it leaves `!stop` unrecognised and the text forwards as ordinary prose, so
  `poe-acp` is byte-for-byte unchanged. Only a relay that *streams* a turn has
  anything to interrupt: poe answers one HTTP request per turn, this relay
  streams into an editable message.
- **Optional `SessionStatus` fields** (`ConvID`, `StateDir`, `Where`,
  `TurnRunning`) for relays that give a conversation its own identity,
  directory and place. The renderer prints only what is set, so a Poe-shaped
  controller produces the same output as before.

The lesson recorded for next time: "promote when a second consumer exists" is
right, but it obliges you to go and *look* at the sibling repos rather than
reason about them from memory. `internal/rollover` remains a promotion
candidate on the original terms — nothing has changed there, because no
second surface with a hard message ceiling has appeared.

### The conversation token

`acp-kit/command` identifies a conversation by one opaque `convID` string it
hands straight back to the relay. The obvious thing to pass is the conv-id,
and it is the **wrong** thing: `!new` replaces the conv-id, so a broker
holding one would be holding a stale identity the moment the command it is
running finishes.

The relay passes `journal.Key.Token()` instead — the key, not the id. A key
is what a conversation is *reached* by, and it is exactly what `!new` does
not change: the topic is still the topic, the participants still the
participants; only the conv-id behind it moves. The encoding is `Key.index()`,
already canonical and already the map key, so a token cannot disagree with a
lookup. `ParseToken` is deliberately strict — a best-effort guess would attach
a command to the wrong conversation, and `!new` on the wrong conversation
destroys context the user expected to keep.

### Dispatch

Ordering in `handleMessage` is load-bearing:

1. The **bot-own-message** and **system-bot-sender** guards, unchanged and
   still first. The relay must never obey a command it wrote itself.
2. Routing (channel key or DM key) and mention gating.
3. `allowed_user_ids`.
4. `Journal.Lookup` — a *read*.
5. The `!addressed && !engaged` drop.
6. **Command dispatch.**
7. `Journal.Ensure` and the agent turn.

Dispatch sits between the lookup and the allocation on purpose: a command
must never **allocate** a conversation. `!help` in a topic the relay has
never answered in leaves nothing on disk, and `!status` there honestly
reports "none yet".

### Gating in a channel

A command is honoured exactly when a prompt would be: the message mentions
the bot, or the topic is already engaged. The alternative — honouring
`!help`/`!status` on any channel message — was rejected: the relay answering
in a topic it was never summoned to is precisely the behaviour the mention
gate exists to prevent, and a command is no less visible than prose. `!new`
in a non-engaged topic is near-meaningless anyway (a new topic *is* a new
conversation), and reports as much rather than allocating one to retire.

In a DM every message reaches the handler by construction, so commands work
there unconditionally — which is the whole point.

### Sigils, and Zulip's own slash messages

The broker accepts `/`, `!` and `.` on input and advertises `!`
(`DisplaySigil`). On Poe that is because its client intercepts `/`-prefixed
messages. On Zulip the reason is different and sharper:

- Zulip's **real** slash commands (`/ping`, `/dark`, `/light`, …) are handled
  **client-side** in `zcommands.js` against the `/json/command` endpoint and
  send no message at all. A bot never sees them, and there is no bot
  slash-command registration API to collide with.
- But `/me`, `/poll` and `/todo` **are** messages. `/me` is flagged
  `is_me_message` on the message object by the markdown processor; `/poll` and
  `/todo` become widgets.

So `/` must never be advertised, and must never shadow those three. The relay
pre-filters them before the broker sees anything — `isWidget` in
`internal/handler/command.go`, using the exported `command.StripSigil` — and
forwards them byte-for-byte. The guard runs **ahead of the pending-login
check** as well, because a `/poll` arriving mid-login must not be eaten as a
failed redirect paste.

### The grammar, and not eating prose

Command names are matched case-insensitively on the **verb only**; the
argument keeps its case, because a model id is a literal the agent must
match. (A phone keyboard capitalises the first letter of a message, so a
case-sensitive `!new` would fail silently for exactly the people typing
one-handed.)

Only a **command-shaped** token counts for the unknown-command error: an
ASCII letter followed by letters, digits, `_` or `-`. `!important: fix the
parser`, `!5 minutes` and a lone `!` are therefore prose and reach the agent
byte-for-byte. A command-shaped token naming nothing known gets a one-line
error and is *also* not forwarded — `!hepl` becoming an agent turn is worse
than a typo notice. That error is scoped to `!` alone: an unrecognised
`/foo` is far more likely to be a Zulip feature than a mistyped relay
command, and a leading `.` is ordinary punctuation.

The escape for genuine prose is a doubled bang: `!!new` arrives as `!new`.
This is relay policy, applied before the broker, not a broker knob — and
deliberately ahead of the pending-login check too. Someone typing `!!foo`
while a login is in flight is plainly not pasting a redirect URL, so the
escape is honoured and the login stays pending for the paste that follows;
consuming it as a malformed redirect would abort the login instead.

### `!new`, retirement, and the tail

`Journal.Retire` marks the old `Conv` with `retired: true`, clears its tail,
drops it from the key index, and allocates a fresh conv-id under the same
key — one atomic write. The old `state/convs/<id>/` directory is never
touched; retiring is not deleting, and the reply names the directory.

Two details are not cosmetic:

- **The tail must be cleared**, or a restart would mark, and the next turn
  could stream into, a message belonging to a conversation nobody can reach.
- **The retired entry stays in the file**, addressable by id but not by key.
  It is the record of which state directories are dead, and — because `!new`
  cancels the retired conversation's in-flight turn — it is what lets that
  turn finish unwinding through `SetTail` without hitting "unknown
  conversation". A pre-`!new` journal simply has no `retired` field, so no
  version bump is needed.

### Agent-command passthrough

An allowlisted command the agent actually advertises (`reload`, `logout`,
`compact`, `session`, `changelog`, `mcp`, `skills`) is rewritten to its slash
form and forwarded through the **normal prompt path**, so the agent runs it
and streams a reply like any other turn. Both conditions are required: in the
allowlist *and* in `AgentCommands()`.

`resume`, `continue`, `name`, `share` and `export` are deliberately excluded,
and the reasoning carries over from poe-acp unchanged because it is
structural, not surface-specific: **the relay owns the conversation → session
mapping.** Letting the agent switch its own session underneath the relay would
desync that mapping, leaving the relay prompting into a session the agent has
moved on from. `!new` is the supported way to change session state. The rest
are side-effecting or account-scoped operations that make no sense driven from
a chat turn.

### `!model`

`acp-kit`'s client surface genuinely supports a switch (`AgentProc.SetModel`,
over `session/set_config_option` or the older `session/set_model`), so it is
not faked. The choice is **sticky per conversation** and held in memory only:
it is recorded by the command and pushed to the ACP session at the start of
the next turn, at most once per session id.

Applying it lazily is deliberate. Doing it eagerly needs a live session, and
calling `GetOrCreate` outside a turn would *spawn* one — and re-register a
sink — as a side effect of what reads like a settings command. Keying the
"already applied" marker on the session id is what makes the choice survive
acp-kit's idle GC: a reaped session comes back with a new id, and the
mismatch is the signal to push again. `!new` carries the choice across to the
fresh conversation, because clearing context is not the same as reverting a
preference. A `SetModel` failure is logged and the turn proceeds on whatever
model the agent already had — refusing to answer at all over a preference
would be a worse trade.

### `!login`, and why it is kept

The login family and its two-call bridge came across with the port. It is
fair to ask whether it earns its keep on a **self-hosted** relay where the
operator can simply shell into the box and run `fir` interactively.

It is kept, for two reasons. Provider tokens **expire**, and when they do the
failure lands in a Zulip topic in front of whoever was talking to the bot —
being able to reconnect from that topic beats discovering you need SSH access
to a machine you may not have on you. And the cost of keeping it is now
almost nothing: it is shared code that `poe-acp` needs regardless, tested
there, so dropping it would mean *adding* a conditional to suppress it rather
than deleting anything.

The one command dropped from poe-acp's surface is **`!id`**. v0.5.0 had it;
the port does not, because it was a symptom of a worse `!status`. Poe's
`!status` renderer already prints the conversation id on its own line in
backticks, which is as copy-pasteable as a bare-id reply, so a whole command
existed to work around a formatting detail that turned out not to be a
problem. Deleting it keeps the two relays' surfaces identical, which is worth
more than one keystroke saved.

### `!opts`, and why it is the one command not in acp-kit

Every other `!command` lives in `acp-kit/command` so `poe-acp`, `slack-acp`
and this relay cannot drift. `!opts` is the deliberate exception: it adds no
capability at all, it *renders* the capabilities that already exist onto a
surface only Zulip has.

That surface is **`zform`**, Zulip's message-attached button widget: a bot
sends `widget_content` alongside an ordinary message and the web client draws
a heading plus buttons. Each button carries a `reply` string, and clicking it
sends that string **as an ordinary message from the clicking user**. That is
the entire mechanism, and it is what makes the feature safe to have: a button
is sugar over the existing `!` parser, so a click walks the same never-answer-
a-bot guard, the same `allowed_user_ids` check, the same engagement gate and
the same dispatch a typed command does. There is no second code path, and
every action still goes through the broker's exported actions in
`acp-kit/command/actions.go` — `!model` typed, a button clicked and the
loopback tool are one implementation.

Three constraints shaped the rest of it.

**The reader is on a phone.** `zform` renders in the Zulip **web app only**;
every other client shows the message's plain markdown. So the markdown body is
the product and the widget is decoration on top of it: the body is written
first, lists the same commands, and must be usable with a thumb. Widgets are
also a dev-docs *subsystem*
(`zulip.readthedocs.io/en/stable/subsystems/widgets.html`), not a versioned
API, which is why the coupling is one file — `internal/zulipproto/zform.go` —
and why a server that rejects `widget_content` outright still gets the panel,
posted without it.

**A panel is state, not scrollback — and a widget message can never be
edited.** Measured on Zulip 12.2: `PATCH /messages/<id>` on a message carrying
`widget_content` returns 400 *"Widgets cannot be edited."* A zform is a
submessage and Zulip forbids content edits on any message that has one. This
kills the obvious design outright — a PATCHed self-updating panel would fail
on the first knob change on every server where the widget actually *worked*,
and would have shipped looking fine, because the degraded plain-markdown path
is the one that still edits.

So the panel updates by **re-post plus delete of the old one**. The net effect
is what was wanted — exactly one live panel, no growing pile of stale controls
— and it is better on a phone, since the panel lands where the reader is. This
is the only place the relay deletes a message it posted; deletion is a realm
policy (`delete_own_message_policy`) and time-limited
(`message_content_delete_limit_seconds`), so a refusal is *expected*: the
fallback rewrites the old panel to a pointer line (possible only for a panel
posted without its widget) and, failing that, leaves it alone — stale but never
wrong, since every button on it is still a valid command. A change is
acknowledged with an emoji reaction on the command message rather than a reply:
a settings change should leave nothing in the topic and nothing in the
transcript the model reads. The panel's message id is persisted in the journal
beside the streaming tail, and `!new` carries it to the fresh conversation: the
panel belongs to the *place*, not to the session.

**One place repaints.** `Handler.SetModelOverride` is the single choke point
every model change passes through — typed, tapped, or made by the agent
through the `select_model` loopback tool — so the repaint lives there rather
than in the command path. A panel that could be left claiming a model the
conversation is not on would be worse than no panel, because it is also the
status line.

**A button must never offer what the agent cannot do.** Model buttons come
from the boot-time probe (`Agent.Models()`), capped so the panel fits one
screen, current model first, with `!model <filter>` reaching the rest. Thinking
level is deliberately *absent*: acp-kit exposes the model config option but no
snapshot of an agent's other `configOptions`, so the relay cannot know a
session has one. Inventing the knob would mean a button that silently fails.
When acp-kit surfaces those options, the knob belongs here — and the acp-kit
change comes first.

Finally, an unknown `!command` now answers with the panel. It used to answer
with a one-line error, which was correct and useless: the moment a user
mistypes a command is the moment they most need the menu. It is still never
forwarded to the agent — a typo must not burn a turn.

### What a command reply is not

It is an ordinary message posted where the command arrived. No `:eyes:`
lifecycle, no `Thinking…` placeholder, no streaming, no tail tracking: all of
that exists to cover latency the relay does not have here, since the reply is
complete before it is composed. A reply that cannot be posted is logged and
dropped — unlike agent output, it costs nothing to ask for again, so the
`rescue` path does not apply.

## The agent→relay loopback

Off by default (`"relay_mcp": true`). It gives the agent a way to drive the
relay from inside a turn: read its own status, switch model, **post out of
band**, **schedule a prompt back into this conversation**, and **read that
conversation's earlier messages**.

### Why MCP, and not an ACP extension

ACP has no agent-initiated message. The agent speaks only inside a turn it was
prompted for, and this relay's streaming sink is bound per turn
(`handler.run` → `state.Manager.GetOrCreate`), so an out-of-turn
`session/update` has nowhere to land. But an **MCP tool call runs
agent→client**, which ACP fully supports. So the loopback needs no protocol
extension: the relay hosts an MCP server, advertises it on `session/new`, and
the agent calls into it like any other tool.

The transport is `acp-kit/mcphost`: a private 0700 directory, a 0600 unix
socket, and a dumb redirector subprocess (`zulip-acp mcp-serve`) that the
agent spawns and that does nothing but pipe stdio to the socket after writing
a one-line token preamble.

### Identity is the foundation

`mcphost` resolves the token to a **session key server-side**. A tool call
therefore already knows, unspoofably, which conversation it came from.
Nothing in the tool surface accepts a conversation as an argument — the
handler maps the session key (a conv-id) to the broker's conversation token
via `Journal.LookupID`, and every tool acts on that conversation and only that
one. `TestConversationIsNeverAnArgument` in acp-kit enforces it against the
schemas.

Take that guarantee away and `post` becomes a realm-wide megaphone for
anything that can prompt-inject the agent.

### One implementation, two front ends

The command surface and the MCP surface are **one implementation**. Both go
through the exported actions on `acp-kit/command.Broker`; the `!command`
handlers are renderers over those actions, and `acp-kit/relaytool` hands the
same results to the agent. Forking them is the mistake v0.5.0 made with the
command surface itself and v0.6.0 corrected — it must not be re-made one layer
up.

The relay-side seam is a single type: the `Controller` the `Handler` already
implemented, plus two OPTIONAL capabilities in the shape of `TurnStopper`:

| capability | enables | zulip-acp | poe-acp |
|---|---|---|---|
| `Controller` | `!status`, `!model`, `!new`, and their tools | yes | yes |
| `TurnStopper` | `!stop` | yes | no — one HTTP request per turn |
| `Poster` | `post` tool | yes | no — nothing to speak on after the response |
| `Scheduler` | `schedule` tools, `!schedules`, `!unschedule` | yes | no — same reason |

`history` sits outside that table on purpose: it has no `!command` twin and no
Broker action, because it is not a relay-generic control at all. See below.

### What is deliberately not exposed

**A loopback tool must never destroy the turn that is calling it.** Two
commands fall foul of that one rule:

- **No `stop` tool.** An agent cancelling its own in-flight turn either does
  nothing or kills the very turn whose tool call asked for it, leaving the
  result undeliverable and the user reading *(superseded)*. Deferring it to
  end-of-turn would make it a no-op by definition. There is no coherent
  reading, so there is no tool. `!stop` remains, for the human, where it makes
  sense.
- **`new_session` is deferred.** Resetting a session cancels the turn in
  flight — the same foot-gun. So the tool records the intent and the relay
  applies it in `Handler.endTurn`, after the turn has left the inflight map.
  Same `Controller`, same implementation, honest timing. The `defer` ordering
  in `handleMessage` and `FireSchedule` is load-bearing: `endTurn` is
  registered first so it runs last.

### `post` and its blast radius

`post` sends a message into the current conversation, out of band. It is what
makes *"go do X and tell me when it lands"* expressible.

There is **no target parameter**, in v1 and by decision. An agent that can
post into arbitrary channels is a new and serious capability, and the
conservative form of that decision is not an `allowed_user_ids` check or a
config allowlist — both of which are one flag away from being wrong — but
making it *inexpressible*. Widening it later means adding a parameter, an
allowlist and a threat model, in that order.

It posts through the **rollover splitter**, like every agent answer. Zulip
truncates past `MAX_MESSAGE_LENGTH` silently; a `post` that called
`SendMessage` directly would be a brand-new way to lose output.

### `history`: the one Zulip-specific tool

Every other loopback tool is relay-generic and lives in `acp-kit/relaytool`,
over a `command.Broker` action, so `poe-acp` and `slack-acp` get it too.
`history` is not: it is a query against a Zulip **narrow**, it answers in
Zulip message shapes, and the question "which conversation may I read" is
answered by a `journal.Key`. So it lives in `internal/zulipmcp`, which existed
from the start precisely to keep that option open, and it resolves identity
through `Handler.ConvKey` — the same server-side binding as `ConvToken`, one
layer earlier, because a narrow needs the stream id and topic (or the DM
participant set), not an opaque broker token.

It exists because the alternative was observed in the wild: an agent whose
session had been cleared shelled out to the Zulip REST API **with the bot's
own credentials** to read back its topic. That works, and it is exactly the
capability the relay should own rather than leak.

- **Channel and DM both.** `zulipproto.TopicNarrow` for a topic,
  `zulipproto.DMNarrow` for a direct message — the `dm` operator with the full
  participant set. A DM is in no channel, so a channel narrow would silently
  return nothing there.
- **Oldest first, raw markdown, bot messages included.** Recovering its own
  prior answers is half the point.
- **Bounded twice.** `MaxMessageRunes` per body (a single Zulip message can be
  10000 code points) and `MaxTotalRunes` for the whole reply. The budget is
  spent newest-first and the result reversed, so what survives a binding cap is
  the recent end; the reply says how many were dropped, whether bodies were
  truncated, and the `before_id` to page further back. An unbounded read would
  blow the agent's context window in one call.
- **`before_id` is exclusive**, so feeding back the oldest id of a page yields
  the page before it with neither overlap nor gap.

### The loop hazard

The agent posts → Zulip delivers that message back on the event queue → the
relay must not treat its own words as a new turn. `handleMessage`'s **first**
guard, before any allowlist, is `m.SenderID == h.cfg.BotUserID`. That guard
was always correct; the loopback is what makes it load-bearing, so it has a
test of its own — `TestLoopbackPostDoesNotFeedItselfBack` posts an
@-mention of the bot into an engaged topic, which every *other* gate would
wave through, and asserts no turn results.

### Scheduling

`schedule` / `list_schedules` / `unschedule`. On fire the relay injects the
stored text as a **prompt** into the conversation it was scheduled from — same
conv-id, same ACP session, so the agent has the topic's full history — and the
answer streams into that topic through the existing path: rollover,
statusline, attachments. Schedules live in `<state-dir>/schedules.json`,
alongside the journal, so they survive a restart.

This is the layer `mintick` (host-level cron) cannot serve: it can run a
command at a time, but it cannot deliver into a conversation *with context*.
The converse also holds — host chores stay in host cron. Nothing here is a
general-purpose scheduler.

**Runaway control** is first-class, because a scheduled prompt whose turn
schedules another is unbounded recursion that costs real money.
`acp-kit/schedule` applies four bounds, all on by default:

| bound | default | config |
|---|---|---|
| chain depth (`schedule→turn→schedule`) | 3 | `max_schedule_depth` |
| armed per conversation | 10 | `max_schedules_per_conv` |
| armed in total | 100 | `max_schedules_total` |
| repeat floor | 60s | `min_schedule_interval_seconds` |

Depth is *derived*, never supplied: an item armed inside a firing scheduled
turn takes its parent's depth plus one. That works only because `Fire` blocks
for the whole turn, which is why `Handler.FireSchedule` is synchronous. A
missed window is skipped rather than replayed, so a relay that was down for a
day does not fire a minutely schedule 1440 times on startup.

None of that removes the need for a human to be able to **look and kill**:
`!schedules` lists what is armed here, `!unschedule <id>` cancels one, and
`!status` reports the count so armed work nobody started is noticed.

Two semantics worth stating out loud, because both are choices:

- **Delivery is at-most-once.** A due item is claimed — advanced, or removed if
  it is a one-shot — in the same critical section that reads it, and only then
  fired. A crash in the window between loses that firing. The alternative,
  claiming after a successful fire, double-fires on every restart, and a
  duplicate unattended turn costs real money where a missed one costs a
  reminder.
- **`!new` does not cancel schedules.** They are keyed on the conversation
  KEY, which `!new` does not change, so a schedule armed before a reset still
  fires — into the fresh conversation. That is deliberate: `!new` clears
  context, it does not cancel commitments. A human who asked to be told when
  something lands did not ask to stop being told by starting a new session.
  `!unschedule` is how you cancel one, and `!status` keeps the count visible.

### A scheduled turn has no human in the loop

Every gate an interactive turn passes is re-applied at **fire** time, not arm
time, and failing one returns `schedule.ErrGone`, which disarms the item
rather than retrying it forever:

- the channel must still be served (unsubscribe and it stops firing);
- direct messages must still be enabled;
- the conversation must still exist in the journal.

The one gate with no analogue is `allowed_user_ids`: a scheduled prompt has no
sender. It needs none — it can only exist because an allowed user drove a turn
that armed it, and it re-enters that same conversation, so it can never reach
anywhere its author could not.

A scheduled turn also **never supersedes a human one**. Where a follow-up
message cancels the turn in flight, `FireSchedule` waits for the conversation
to go idle (`awaitConvIdle`, on the existing inflight condition variable) and
then runs as an ordinary addressed turn.

## Upgrading without dropping a turn

`systemctl --user reload zulip-acp` (SIGHUP) stops intake, drains the in-flight
turns, and `syscall.Exec`s the on-disk binary in place, handing the live Zulip
event queue forward in the environment. The PID never moves and no message is
lost, because the queue buffers server-side while we are not polling.

**This is deliberately NOT poe-acp's master/worker supervisor.** That exists to
hold a bound listen socket across worker generations; a long-poll client has no
socket, so the supervisor would hold nothing. The full argument, the wire
contract and the live evidence are in
[graceful-reload.md](graceful-reload.md).

## Layout

```
cmd/zulip-acp/          flags + wiring (excluded from the coverage gate)
internal/autotopic/     general-chat topic namer — pure func, NO Zulip imports
internal/command/       `!command` parse core — NO Zulip imports
internal/config/        JSON config, DisallowUnknownFields
internal/handler/       gating, commands, turn execution, streaming sink, outbox, poster
internal/handler/loopback.go
                        agent→relay loopback: conv-id → broker token / journal key,
                        out-of-band post, scheduled-prompt firing and its gates
internal/handler/opts.go
                        `!opts`: the one self-updating options panel per conversation
internal/journal/       conv key (channel topic | DM user set) → conv-id, tail and
                        options-panel message ids
internal/reload/        graceful reload: drain in-flight turns, then re-exec in
                        place with the event-queue cursor in the environment
                        (see docs/graceful-reload.md)
internal/rollover/      pure code-point splitter — NO Zulip imports
internal/statusline/    Zulip-markdown model/mood/plan line (spinner + footer)
internal/sysprompt/     built-in Zulip formatting block
internal/zulipmcp/      MCP server identity (socket naming, env vars, subcommand)
                        and the one Zulip-specific loopback tool, `history`
internal/zulipproto/    HTTP Basic client + /events long-poll runner
internal/zulipproto/zform.go
                        the ONLY coupling to Zulip's widget subsystem
test/                   live-server tests (ZULIP_LIVE=1), coverage-exempt
```
