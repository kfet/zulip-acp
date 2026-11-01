---
builtin: true
name: widgets
description: Post a live, interactive HTML widget or an MCP App inside a Zulip message. Read before you show, embed or share something interactive or animated here — a demo, chart, calculator, game, HTML page or MCP app — or when the user says "widget" or "share it here".
---

# Inline widgets

This realm runs the zulip-lite widgets patch. The patched web app shows a
`widget-html` block in a BOT message as a live, sandboxed frame. When the
deliverable is interactive or animated, post a widget. Do not offer a PNG or
a link instead, and do not say that you cannot embed.

## Format

`````
Here is a counter:

````spoiler 🧩 Interactive widget: open in the web app
[Open in the Zulip web app](https://REALM/) to use this widget.

```widget-html
<!-- zlite-widget height=120 -->
<style>body{font:16px sans-serif;margin:12px}</style>
<button id="b">click me</button> <span id="n">0</span> clicks
<script>let c=0;b.onclick=()=>{n.textContent=++c};</script>
```
````
`````

Rules:
- Always wrap the block in the spoiler. Unpatched clients (mobile apps,
  terminal, e-mail) then show one collapsed line with the link. Without the
  spoiler they show raw HTML.
- The spoiler fence must be longer than the inner fence: four backticks
  around three. `~~~spoiler` … `~~~` also works.
- The link is the realm root URL (the link in your system prompt). Use it
  as it is.
- The block holds the BODY of an HTML document. Inline `<script>` and
  `<style>` run.
- The first line `<!-- zlite-widget height=N -->` sets the start height,
  40 to 9999 px (default 300). An MCP App that sends `size-changed` can
  leave out `height=` and gets auto height up to 800 px.
- One message holds at most 10000 characters, the fences included. For a
  bigger widget, use `src=` (below).

## Limits of the sandbox

- No network: no fetch, no remote scripts, fonts or images, no CDN.
  Inline all code, styles and data. Images must be `data:` or `blob:` URLs.
- No forms, popups, or top navigation. No access to Zulip, its cookies or
  the user's session.
- Only bot senders render. Do not ask a user to post a widget.
- An edit or a re-render resets the widget state. Do not depend on it.
- The widget cannot post back into the chat. Tell the user to reply in the
  topic when you need their input.

## MCP Apps

The frame host speaks MCP Apps (spec 2026-01-26) over `postMessage`. An app
built with `@modelcontextprotocol/ext-apps` runs unchanged. To give it the
tool call, put a `widget-data` block right after the `widget-html` block,
inside the same spoiler:

````
```widget-data
{"tool": {"name": "get-time", "inputSchema": {"type": "object"}},
 "input": {},
 "result": {"content": [{"type": "text", "text": "2026-10-10T09:00:00Z"}],
            "structuredContent": {"time": "2026-10-10T09:00:00Z"}}}
```
````

All keys are optional. Copy `input` and `result` from the tool call you
made. `tools/call` and `resources/read` from the app get an error ("host has
no server connection yet"). `ui/open-link` works after the user confirms.
`ui/message` is refused.

## Big apps: `src=` an upload

1. Write the app as one self-contained `.html` file into `./outbox/`. End
   the turn. The relay uploads it and links it under **Attachments:** in
   your reply. That link also keeps the upload alive.
2. In a LATER turn (or a scheduled prompt), read the `/user_uploads/…` path
   from that link (use the `history` tool if you have it), then post:

`````
````spoiler 🧩 Interactive widget: open in the web app
[Open in the Zulip web app](https://REALM/) to use this widget.

```widget-html
<!-- zlite-widget src=/user_uploads/2/10/abc/app.html -->
```
````
`````

Only `/user_uploads/` paths work. The app runs in the same sandbox, with
the same no-network rule, so it must still be single-file.

## Checklist before you send

- Spoiler with four backticks, link line, then `widget-html`.
- First line of the block is the `zlite-widget` comment.
- Nothing loads from the network.
- Message under 10000 characters, or `src=`.
