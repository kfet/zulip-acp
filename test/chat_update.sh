#!/usr/bin/env bash
# chat_update.sh — offline tests for scripts/chat-update.sh.
#
# A throwaway remote and clone stand in for the fleet checkout, and a fake
# converge.sh stands in for the real one: `--tot` writes the lock from
# $NEXT_FIR, `<bot> --apply` records that it ran.
#
# Run from anywhere: ./test/chat_update.sh
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
pass=0 fail=0
ok()  { pass=$((pass + 1)); echo "  ok   $1"; }
bad() { fail=$((fail + 1)); echo "  FAIL $1"; }

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
export GIT_AUTHOR_NAME=t GIT_AUTHOR_EMAIL=t@t GIT_COMMITTER_NAME=t GIT_COMMITTER_EMAIL=t@t

git init --quiet --bare -b main "$tmp/remote.git"
git clone --quiet "$tmp/remote.git" "$tmp/wc" 2>/dev/null
mkdir -p "$tmp/wc/scripts"
cp "$ROOT/scripts/chat-update.sh" "$tmp/wc/scripts/"
cat > "$tmp/wc/scripts/converge.sh" <<'FAKE'
#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
case "$1" in
--tot) printf '{"zulip_acp":"0.1.0","fir":"%s","resolved_at":"%s"}\n' "$NEXT_FIR" "$RANDOM$RANDOM" > dist.lock ;;
*) echo "applied $1 $2" ;;
esac
FAKE
chmod +x "$tmp/wc/scripts/converge.sh"
printf '{"zulip_acp":"0.1.0","fir":"1.0.0","resolved_at":"a"}\n' > "$tmp/wc/dist.lock"
git -C "$tmp/wc" add -A
git -C "$tmp/wc" commit --quiet -m init
git -C "$tmp/wc" push --quiet origin main 2>/dev/null

commits() { git -C "$tmp/remote.git" rev-list --count main; }

out=$(NEXT_FIR=1.0.0 "$tmp/wc/scripts/chat-update.sh" zbox)
if [ "$(commits)" = 1 ] && [ -z "$(git -C "$tmp/wc" status --porcelain)" ] &&
	grep -q "no version moved" <<<"$out" && grep -q "applied zbox --apply" <<<"$out"; then
	ok "unchanged versions: lock restored, nothing committed, applied"
else
	bad "unchanged versions: $out"
fi

out=$(NEXT_FIR=2.0.0 "$tmp/wc/scripts/chat-update.sh" zbox)
if [ "$(commits)" = 2 ] && [ "$(git -C "$tmp/remote.git" log -1 --format=%s main)" = "dist.lock: zulip-acp 0.1.0, fir 2.0.0" ] &&
	grep -q "applied zbox --apply" <<<"$out"; then
	ok "moved version: lock committed, pushed, applied"
else
	bad "moved version: $out"
fi

# A push that fails leaves neither a local commit nor a dirty lock.
mv "$tmp/remote.git" "$tmp/gone.git"
if NEXT_FIR=4.0.0 "$tmp/wc/scripts/chat-update.sh" zbox >/dev/null 2>&1; then
	bad "failed pull/push reported success"
elif [ -z "$(git -C "$tmp/wc" status --porcelain)" ] && [ "$(git -C "$tmp/wc" rev-list --count HEAD)" = 2 ]; then
	ok "failure restores the checkout"
else
	bad "failure left the checkout dirty or ahead"
fi
git -C "$tmp/wc" remote set-url --push origin "$tmp/nowhere.git"
mv "$tmp/gone.git" "$tmp/remote.git"
if NEXT_FIR=5.0.0 "$tmp/wc/scripts/chat-update.sh" zbox >/dev/null 2>&1; then
	bad "failed push reported success"
elif [ -z "$(git -C "$tmp/wc" status --porcelain)" ] && [ "$(git -C "$tmp/wc" rev-list --count HEAD)" = 2 ]; then
	ok "failed push drops the local lock commit"
else
	bad "failed push left a local commit or a dirty lock"
fi

if NEXT_FIR=3.0.0 "$tmp/wc/scripts/chat-update.sh" >/dev/null 2>&1; then
	bad "missing bot accepted"
else
	ok "missing bot refused"
fi

echo "chat_update: $pass passed, $fail failed"
[ "$fail" = 0 ]
