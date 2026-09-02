---
builtin: true
name: notes
description: Persistent scratch notes across conversations. Read first when you do not know where to find or how to access something.
---

# Notes

`~/.local/state/zulip-acp/notes/` is your persistent scratch across conversations. Read and write freely.

`notes/fleet/` is a Syncthing-shared directory: it is the same set of files
every relay on every host sees. Facts that are true for the whole fleet
(hosts, credentials layout, runbooks) belong there; anything specific to this
relay or host stays outside it.
