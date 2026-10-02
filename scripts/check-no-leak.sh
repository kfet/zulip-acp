#!/usr/bin/env bash
# check-no-leak.sh — fail if any bot-instance identifier from the private
# fleet registry appears in the tracked tree. This repo is public: it holds
# the distro spec (distro.json), never bot instances.
#
# Identifiers are read from every $FLEET_BOTS_DIR/*.json: the string values
# of identity keys (name, host, aliases, site, emails, channels, bot names,
# ...) plus the host part of URLs and both halves of e-mail addresses.
# Matching is case-insensitive, fixed-string, whole-word (git grep -iwF).
# When the registry is absent (CI, a fresh clone) the check is skipped.
set -euo pipefail

BOTS_DIR="${FLEET_BOTS_DIR:-$HOME/sync/shared/fleet/bots}"
cd "$(dirname "$0")/.."

if [ ! -d "$BOTS_DIR" ] || ! compgen -G "$BOTS_DIR/*.json" >/dev/null; then
  echo "no-leak: skipped — no bot registry at $BOTS_DIR"
  exit 0
fi

KEYS='["name","host","hosts","aliases","site","site_aliases","bot_email","bot_name","email","emails","channels","ambient_channels","autotopic_channels","agent_ssh_host","value"]'
# Values that are not identifying on their own (a channel wildcard, the
# built-in "local" host choice).
STOP='["*","local"]'

pats=$(mktemp); trap 'rm -f "$pats"' EXIT
jq -r --argjson keys "$KEYS" --argjson stop "$STOP" '
  [ paths(type == "string") as $p
    | select([$p[] | strings] | any(. as $k | $keys | index($k)))
    | getpath($p) ]
  | map(., (capture("^[a-z]+://(?<h>[^/:]+)").h // empty),
           (select(test("@")) | split("@")[]))
  | .[] | select(length >= 3) | select(. as $v | $stop | index($v) | not)
' "$BOTS_DIR"/*.json | sort -u >"$pats"

[ -s "$pats" ] || { echo "no-leak: registry has no identifiers"; exit 0; }

if git grep -n -I -i -w -F -f "$pats" -- . ; then
  echo "no-leak: FAIL — bot-instance identifiers from $BOTS_DIR appear above" >&2
  exit 1
fi
echo "no-leak: ok ($(wc -l <"$pats") identifiers, none in the tracked tree)"
