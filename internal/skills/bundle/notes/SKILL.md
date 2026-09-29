---
builtin: true
name: notes
description: Persistent scratch notes across conversations. Read first when you do not know where to find or how to access something.
---

# Notes

`~/.local/state/zulip-acp/notes/` is your persistent scratch across conversations. Read and write freely.

`notes/fleet/` MAY be a shared directory, but only if this host's operator
set up sync for it (for example Syncthing). The relay does not sync it.
Do not assume it is shared. Before you say a note reaches other hosts, check:
look for a `.stfolder` marker in `notes/fleet/` and a running sync process.
If you find no sync, `notes/fleet/` is local only, like every other note.

When it is shared, put facts that are true for the whole fleet (hosts,
credentials layout, runbooks) there. Anything specific to this relay or host
stays outside it.
