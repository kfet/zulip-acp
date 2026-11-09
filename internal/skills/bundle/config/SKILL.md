---
builtin: true
name: config
description: Change the zulip-acp relay config (config.json) safely — find which keys exist, back up, edit, check, reload, verify. Use before you add or change any key, e.g. a delete block, channels, owners or quick_models.
---

# Config

The relay reads one JSON file: the `-config` flag of its service (default
`~/.config/zulip-acp/config.json`). Unknown keys are an error.

Never print a key or a key file into the conversation or a log: no `cat` of a
key file or of a config that holds `bot_api_key`. Use `jq 'del(.bot_api_key)'`
or `jq keys` to look at a config.

## 1. Find the keys — do not guess, do not `strings` the binary

```bash
zulip-acp config-schema            # every key, its type and its doc
zulip-acp config-schema delete.    # only the keys under a prefix
```

Read the doc of every key you set. A key often needs another one: for
example `delete.owners_only` (true when unset) needs `update_owner_ids`.

A fleet host (a bot spec in the fleet registry) is NOT edited by hand:
change the spec and run `scripts/converge.sh <bot> --apply` (see `update`).

## 2. Back up, edit, check

```bash
F=~/.config/zulip-acp/config.json      # the file the service uses
cp -p "$F" "$F.bak.$(date +%s)"
# edit $F
zulip-acp check-config -config "$F"    # exit 0 and "OK" — or the error
```

Do not reload until `check-config` prints `OK`. It runs the same checks the
relay runs at start and on reload. The server-side checks (login, channel
names, the delete bot's login) run only at start, so verify in step 4.

## 3. Reload

```bash
systemctl --user reload zulip-acp      # Linux; launchd: see the update skill
```

To reload the relay you run inside, see the `update` skill §4: it is safe,
your reply survives.

If the file is invalid after all, the relay refuses the reload, keeps running
on its current config, and posts the error into each topic with a turn in
flight. A start with an invalid file runs on `<file>.last-good` and DMs the
owners. In both cases your edit is NOT live.

## 4. Verify — before you tell the user it worked

```bash
journalctl --user -u zulip-acp -n 30 --no-pager | grep -E 'config|ERROR|delete|authenticated'
```

Look for the line of the feature you changed (for a delete block:
`topic delete (:fire:) is on: …`) and for no `config: ERROR` or
`reload refused`. Report what the log says, not what you expect.
