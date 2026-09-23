#!/usr/bin/env bash
# chat-update.sh <bot> — the `!update` flow of a fleet host.
#
# A fleet host's relay runs this as its `update_converge_cmd`, from the
# checkout that holds its bots/ spec and dist.lock. It is the same
# sequence an operator types by hand:
#
#   1. git pull --ff-only             pick up spec and lock changes
#   2. scripts/converge.sh --tot      resolve the newest versions
#   3. commit + push dist.lock        only when a version moved
#   4. scripts/converge.sh <bot> --apply
#
# `--tot` always rewrites `resolved_at`. A lock where only that key moved
# is restored, not committed: it resolves to the same versions.
set -euo pipefail

bot=${1:?usage: chat-update.sh <bot>}
cd "$(dirname "$0")/.."

# A failure must not leave the lock dirty or the checkout ahead of
# origin: the next run would then die at `git pull --ff-only`. Only
# dist.lock and our own commit are undone — the checkout may be a
# worktree somebody works in, so other changes are left alone.
base=$(git rev-parse HEAD)
restore() {
	[ "$(git rev-parse HEAD)" = "$base" ] || git reset --quiet --keep "$base"
	git checkout --quiet -- dist.lock
	echo "chat-update: failed; checkout restored to ${base:0:12}" >&2
}

git pull --ff-only --quiet
base=$(git rev-parse HEAD)
trap restore ERR
before=$(jq -S 'del(.resolved_at)' dist.lock)
scripts/converge.sh --tot
if [ "$(jq -S 'del(.resolved_at)' dist.lock)" = "$before" ]; then
	git checkout --quiet -- dist.lock
	echo "dist.lock: no version moved"
else
	msg=$(jq -r '"dist.lock: zulip-acp \(.zulip_acp), fir \(.fir)"' dist.lock)
	git commit --quiet -m "$msg" -- dist.lock
	git push --quiet
	echo "$msg (committed and pushed)"
fi
trap - ERR
scripts/converge.sh "$bot" --apply
