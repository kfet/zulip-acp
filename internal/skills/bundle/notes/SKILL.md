---
builtin: true
name: notes
description: Persistent notes across conversations — the record of how this host, the fleet and the user actually work. Read the matching notes BEFORE you pick a host, a repo, a build, deploy or update path, a config change, or anything else about the environment; write one when you learn such a fact.
---

# Notes

`~/.local/state/zulip-acp/notes/` is your persistent memory across conversations. Read and write freely.

(`$XDG_STATE_HOME/zulip-acp/notes/` when that is set.) It does not move with
the relay's `state_dir`.

When the store has notes, the system prompt holds an index of every note,
with a one-line summary each. The relay builds it from this directory at the
start of each session.

## When to read

Read the notes BEFORE you act on a fact about the environment, not only when
you feel lost. Typical cases: which host to use, where a repository lives,
where and how to build, how to deploy, update or restart something, how to
reach a service, which config a host uses, what the user prefers.

You do not know these facts. You have a plausible guess, and a plausible guess
feels like knowledge. Notes exist because an earlier guess was wrong. So:

1. Scan the index in the system prompt for notes on the subject.
2. Read each matching note. If no summary matches, grep the directory for the
   key words of the task (host names, repo names, tool names).
3. A note overrides your guess. Follow it, or say why you do not.
4. Ask the user only for what the notes do not answer.

## When to write

When you learn a fact about the environment that a later session would have
to guess or ask for, write it down: a new file, or an edit to the note that
covers the subject. Start every note with a `# Heading` (or a frontmatter
`description:`) that says what it covers — that line is its summary in the
index. One subject per file.

## Shared notes

`notes/fleet/` MAY be a shared directory, but only if this host's operator
set up sync for it (for example Syncthing). The relay does not sync it.
Do not assume it is shared. Before you say a note reaches other hosts, check:
look for a `.stfolder` marker in `notes/fleet/` and a running sync process.
If you find no sync, `notes/fleet/` is local only, like every other note.

When it is shared, put facts that are true for the whole fleet (hosts,
credentials layout, runbooks) there. Anything specific to this relay or host
stays outside it.
