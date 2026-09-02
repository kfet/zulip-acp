# Zulip protocol reference

Everything here was **measured against a live Zulip 12.2 server** (feature level
500), not read off a docs page. Where the observed behaviour differs from what
you would guess, that is called out — those are the parts that cost debugging
time.

Each claim that the relay depends on is pinned by a test in
[`test/live_test.go`](../test/live_test.go), which runs against a real server
with `ZULIP_LIVE=1`. If Zulip changes, that is where it surfaces.

## Auth

HTTP Basic, `email:api_key`. That is the whole story:

```
Authorization: Basic base64(bot@realm.example:APIKEY)
```

No signing secret, no request-timestamp verification, no OAuth refresh, no
webhook shared secret. Slack's `internal/verify` has **no analog here** — there
is nothing to verify.

```bash
curl -u bot@realm.example:APIKEY https://zulip.example/api/v1/users/me
```

Create a bot at `<site>/#organization/bots`, type **Generic**, then subscribe it
to the channels it should serve.

## Message length — the one that matters

**`MAX_MESSAGE_LENGTH` is 10000 Unicode code points, and Zulip TRUNCATES
SILENTLY.**

| sent (code points) | API result | stored | tail |
|---|---|---|---|
| 9999 | `success` | 9999 | intact |
| 10000 | `success` | 10000 | intact |
| 10001 | `success` | **10000** | `…\n[message truncated]` |
| 12000 | `success` | **10000** | `…\n[message truncated]` |

There is **no error at any layer**. A relay that streams a long turn into one
message loses the tail and is never told. This single fact is why
`internal/rollover` exists and why `zulipproto` refuses to put an oversized body
on the wire at all.

**Count code points, never bytes.** Zulip counts Python `len(str)`: not bytes,
not UTF-16 units. 4000 CJK characters is 12000 bytes and 4000 code points, and
is stored whole.

### Length is not the only limit

A body that is legal by length can still be refused:

```json
{"result":"error","msg":"Unable to render message","code":"BAD_REQUEST"}   // HTTP 400
```

Measured: **~1000 consecutive emoji** trips this, while **9000 CJK characters**
render fine. The server-side markdown/Pygments pass is what fails, not the
length check.

This failure is **loud**, which makes it far kinder than silent truncation. The
relay surfaces it and falls back to uploading the answer as a file.

## Sending and editing

```bash
# send
curl -u ... -X POST https://zulip.example/api/v1/messages \
  -d type=stream -d to=4 \
  --data-urlencode 'topic=session: fix the parser' \
  --data-urlencode 'content=hello'
# → {"result":"success","id":42}

# edit
curl -u ... -X PATCH https://zulip.example/api/v1/messages/42 \
  --data-urlencode 'content=hello, again'
```

`to` takes the channel id as a plain integer for a `type=stream` message. (Note
the contrast with the `/register` narrow below, where a numeric operand means
something else entirely.)

A **direct message** is the same endpoint with `type=private` and `to` as a
**JSON array of user ids** — 1:1 and group DMs differ only in how many ids are
in the array. The comma-separated and email forms are deprecated:

```bash
curl -u ... -X POST https://zulip.example/api/v1/messages \
  -d type=private -d 'to=[4,9]' \
  --data-urlencode 'content=hello'
```

Including the bot's own id in `to` is harmless — Zulip ignores it — so the
participant set from `display_recipient` can be passed straight through.
Editing is identical for both: `PATCH /messages/<id>` does not care how the
message was addressed. `MAX_MESSAGE_LENGTH` and its silent truncation apply to
DMs exactly as to channel messages.

### ⚠️ Trap: `display_recipient` is polymorphic

The same field is a JSON **string** (the channel name) on a channel message and
a JSON **array of user objects** on a DM:

```json
{"type":"stream",  "display_recipient":"fleet"}
{"type":"private", "display_recipient":[{"id":4,...},{"id":9,...}]}
```

A typed Go field would fail to decode one of the two shapes and take the
**whole `/events` response** down with it, silently wedging the queue. It is
therefore kept as `json.RawMessage` and decoded lazily by
`Message.Recipients()`. The array holds every participant, sender and bot
included; its order is **not** contractual, so treat it as a set.

**Edits are fast and effectively unthrottled**: 40 consecutive PATCHes measured
at **15–19 edits/sec**, zero rate-limit responses, p50 ~84ms. Slack's
`chat.update` 1/sec/channel throttle has no counterpart here. The relay still
coalesces at ~300ms, purely because every edit re-renders the whole message
server-side and again on the reader's phone.

> ⚠️ **Deployment prerequisite.** A fresh Zulip realm sets
> `message_content_edit_limit_seconds = 600`. A streaming relay PATCHes the same
> message for the whole turn, so any turn longer than ten minutes starts failing
> with HTTP 400 mid-stream. Set message editing to **unlimited** (Organization
> settings → Message editing) before running the relay.

## Widgets (`widget_content`)

A bot can attach an interactive form to a message by sending `widget_content`
alongside `content`. The relay uses exactly one shape, `zform` / `choices`:

```bash
curl -u ... -X POST https://zulip.example/api/v1/messages \
  -d type=stream -d to=4 --data-urlencode 'topic=opts' \
  --data-urlencode 'content=**⚙️ opus** — `!model <id>` to switch' \
  --data-urlencode 'widget_content={"widget_type":"zform","extra_data":{
       "type":"choices","heading":"Options","choices":[
       {"type":"multiple_choice","short_name":"opus","long_name":"Claude Opus 4.5",
        "reply":"!model anthropic/claude-opus-4-5"}]}}'
```

Clicking a button sends its `reply` string **as an ordinary message from the
clicking user**, which is what makes buttons safe to build on: they are sugar
over whatever already parses typed text.

Three facts, all measured on Zulip 12.2:

- **Only the web app renders it.** Every other client, the phone app included,
  shows the message's plain `content`. The markdown body must therefore stand
  on its own; the widget is decoration on top of it.
- **It is a dev-docs subsystem**
  (`zulip.readthedocs.io/en/stable/subsystems/widgets.html`), not a versioned
  API. Keep the coupling thin and never make correctness depend on it.
- **A message with a widget can NEVER be content-edited** — see below.

### ⚠️ Trap: `Widgets cannot be edited.`

```bash
curl -u ... -X PATCH https://zulip.example/api/v1/messages/404 \
  --data-urlencode 'content=updated'
# → {"result":"error","msg":"Widgets cannot be edited.","code":"BAD_REQUEST"}
```

A zform is stored as a **submessage**, and Zulip refuses a content edit on any
message that has one. There is no opt-out. A self-updating control message
built on widgets is therefore impossible to do by editing: it must be
**re-posted**, with the old one deleted (`DELETE /messages/<id>`, subject to
`delete_own_message_policy` and `message_content_delete_limit_seconds`) or left
behind.

This one fails in the cruellest possible direction — the *degraded*
plain-markdown path, on a server with widgets disabled, edits perfectly well.
An implementation that PATCHes will therefore look correct exactly where the
feature is doing nothing, and break exactly where it works.

## Reading back

Always fetch with `apply_markdown=false` — the relay wants the raw markdown it
wrote, not Zulip's rendered HTML.

```bash
curl -u ... 'https://zulip.example/api/v1/messages/42?apply_markdown=false'

curl -u ... -G 'https://zulip.example/api/v1/messages' \
  --data-urlencode 'anchor=newest' -d num_before=50 -d num_after=0 \
  -d apply_markdown=false \
  --data-urlencode 'narrow=[{"operator":"channel","operand":4},{"operator":"topic","operand":"session: fix the parser"}]'
```

Note that the **`/messages` narrow** takes objects with `operator`/`operand`,
and a channel operand there **is** the numeric id. The `/register` narrow is a
different shape with different semantics — see below.

## Reactions

Zulip has no typing indicator. A reaction on the user's own message is the
closest thing: instant, invisible in the topic's message flow, and — crucially —
**retractable**.

```bash
# add
curl -u ... -X POST 'https://zulip.example/api/v1/messages/42/reactions' \
  --data-urlencode 'emoji_name=eyes'

# remove
curl -u ... -X DELETE 'https://zulip.example/api/v1/messages/42/reactions' \
  --data-urlencode 'emoji_name=eyes'
```

`emoji_name` alone is enough for a standard unicode emoji; `emoji_code` and
`reaction_type` are only needed for custom realm emoji.

Both calls are **idempotent in spirit but not in status**: adding a reaction
that is already there fails with `REACTION_ALREADY_EXISTS`, removing one that
is not there fails with `REACTION_DOES_NOT_EXIST`, both HTTP 400. For a relay
that uses a reaction as a transient ack, both mean "already in the state you
asked for" — `zulipproto` treats them as success.

A reaction produces a `reaction` event on the queue. The relay registers only
for `message` and `update_message`, and its event dispatch switches on the
event type, so its own ack cannot be re-ingested.

## Events: `/register` + `/events`

There is **no inbound HTTP**. The relay dials out only: no tunnel, no webhook,
no public exposure.

```bash
# 1. create a queue
curl -u ... -X POST https://zulip.example/api/v1/register \
  --data-urlencode 'event_types=["message","update_message"]' \
  --data-urlencode 'narrow=[["channel","fleet"]]' \
  -d apply_markdown=false
# → {"queue_id":"…","last_event_id":-1,"max_message_id":30,"idle_queue_timeout_secs":600}

# 2. long-poll it
curl -u ... 'https://zulip.example/api/v1/events?queue_id=…&last_event_id=-1'

# 3. tear it down
curl -u ... -X DELETE https://zulip.example/api/v1/events -d queue_id=…
```

### ⚠️ Trap: the `/register` narrow operand must be the channel NAME

`narrow=[["channel","4"]]` is **accepted**. `POST /register` returns a perfectly
good `queue_id`. And then the queue **delivers nothing, forever**, because Zulip
matches the operand as a channel *name*. There is no error at any layer; the
relay simply never receives an event.

| narrow | register | delivers |
|---|---|---|
| `[["channel","4"]]` | ✅ success | ❌ **nothing** |
| `[["channel",4]]` | ❌ `narrow[0][1] is not a string` | — |
| `[["channel","fleet"]]` | ✅ success | ✅ yes |
| `[["stream","fleet"]]` | ✅ success | ✅ yes |

### ⚠️ Trap: narrow terms are a CONJUNCTION

`narrow=[["channel","fleet"],["channel","Zulip"]]` also registers happily and
also **delivers nothing** — it means "in `fleet` **and** in `Zulip`", which no
message satisfies. There is no way to express a channel *union* in a `/register`
narrow.

Verified live: a queue narrowed to two channels received no event after a
message was posted in one of them.

So a relay serving more than one channel must register **without** a channel
narrow and filter for itself. Over-delivery is cheap; under-delivery is silent
and unrecoverable. Both traps are handled by
`zulipproto.NarrowChannels(names, serveDMs)`, which narrows exactly one channel
and otherwise returns nil.

### ⚠️ Trap: a channel narrow excludes direct messages

Same conjunction, third consequence: a DM is in no channel, so it can never
satisfy a `["channel", …]` term. A queue narrowed to one channel delivers **no
DM at all** — again silently. A relay that serves DMs must register without a
channel narrow, which is what the `serveDMs` argument above is for.

### ⚠️ Trap: there is no `timeout` parameter

`GET /events?…&timeout=90` returns

```json
"ignored_parameters_unsupported":["timeout"]
```

The server holds the request open for roughly 90s of its own accord. **The poll
is bounded by the client's HTTP timeout alone** — the relay uses 110s.

### Cursor discipline

This is the real contract, and it replaces Slack's Socket Mode acks entirely.

- Event ids are per-queue and monotonic. Advance `last_event_id` past every
  event you see, including heartbeats.
- **Skip any event with `id <= last_event_id`.** The server can redeliver across
  a reconnect; this is your dedup.
- `heartbeat` events carry no payload. Advancing the cursor *is* their purpose;
  they are also your liveness signal. No events at all — not even a heartbeat —
  for ~2× the long-poll window means the connection is wedged even though
  nothing errored: tear down and re-register.
- `BAD_EVENT_QUEUE_ID` is **routine**, not an error. Queues die on server
  restart and on idle GC (`idle_queue_timeout_secs`, 600 by default).
  Re-register, reset `last_event_id = -1`, log at info.
- Persisting `queue_id` / `last_event_id` across a relay restart is **false
  comfort** — the queue is very likely already gone.
- Backoff exponentially with jitter (cap ~30s) on network errors; retry
  **immediately** after a clean long-poll expiry, which is the normal idle
  outcome.

### Event shapes

A `message` event:

```json
{"id":1,"type":"message","message":{
  "id":33,"sender_id":9,"sender_full_name":"fir-relay",
  "sender_email":"fir-relay-bot@realm","sender_realm_str":"",
  "client":"curl","content":"raw markdown","content_type":"text/x-markdown",
  "type":"stream","stream_id":4,"subject":"the topic","timestamp":1788119752}}
```

Note `subject` is the **topic** (a historical name), and `content` is raw
markdown only because the queue was registered with `apply_markdown=false`.

A topic rename arrives as `update_message`:

```json
{"id":3,"type":"update_message","message_id":33,"stream_id":4,
 "orig_subject":"untitled","subject":"session: the real task",
 "propagate_mode":"change_all"}
```

`orig_subject` → `subject` is the pair that keeps a renamed topic from orphaning
its session.

### ⚠️ Trap: system bots post into your topics

Renaming a topic makes Zulip's **Notification Bot** post into it:

```json
{"sender_id":6,"sender_full_name":"Notification Bot",
 "sender_email":"notification-bot@zulip.com",
 "sender_realm_str":"zulipinternal","client":"Internal",
 "content":"This topic was moved here from #**fleet>old-name** by @_**kfet|8**."}
```

Left unfiltered, that is a full agent turn spent on a move notice. **Cross-realm
system bots do not appear in `GET /users`**, so a bot-id set built from the user
list will not catch them — match `sender_realm_str == "zulipinternal"` instead.

## Uploads

One multipart round-trip, versus Slack's three-step
`getUploadURLExternal` → `PUT` → `completeUploadExternal`:

```bash
curl -u ... -X POST https://zulip.example/api/v1/user_uploads -F 'file=@report.log'
# → {"url":"/user_uploads/2/ab/…/report.log"}
```

You interpolate the returned relative URL into message markdown yourself:
`[report.log](/user_uploads/…)`. Downloading it back requires the same Basic
auth. Round trips are byte-identical, with **no extension allowlist and no MIME
sniffing** — `.zzq` full of `/dev/urandom` is accepted.

## Markdown

Zulip renders CommonMark-flavoured markdown **server-side at post time**,
including Pygments highlighting, and stores the resulting HTML.

- Fenced blocks keep their language tag as `data-code-language`, so clients get
  pre-highlighted markup and web and iOS render identically.
- `*italic*`, `**bold**`, `> quote`, tables and nested lists all work.
- An **unterminated** fence renders as code to the end of the message — which is
  exactly the right look for a still-streaming answer, so the relay leaves the
  live tail's open fence alone.
- Mentions in raw markdown are `@**Full Name**`, `@**Full Name|123**`, or
  `@_**Full Name**_` for a silent mention.

The cost of server-side rendering: every PATCH re-runs the whole
markdown+Pygments pipeline over the full body. That is the ~84ms p50, and it
grows with body size and fence count. It is also the mechanism behind the
"Unable to render message" failure above.

## Push notifications

`GET /api/v1/server_settings` on a self-hosted realm reports:

```json
"push_notifications_enabled": false
```

Self-hosted Zulip **cannot** push to iOS directly — Apple only accepts pushes
from the app's own signing identity — so every self-hosted server must relay
through Zulip's Mobile Push Notification Service (`manage.py register_server`).
That bouncer sees notification metadata: sender, channel, topic, volume. Message
bodies can be withheld.

The relay takes no position on this and does nothing about it. It is a privacy
trade-off for the operator to make deliberately.
