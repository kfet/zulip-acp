#!/usr/bin/env bash
# shellcheck disable=SC2015  # `check && ok || bad` is the assertion idiom here
# converge_test.sh — offline end-to-end tests for scripts/converge.sh.
#
# CONVERGE_RSH points the transport at a fake: every "host" is a directory
# that serves as $HOME, with stub zulip-acp, fir, git, systemctl and
# journalctl on PATH and a fake /proc. Every stub appends its argv to one
# call log, so each case asserts the exact call sequence.
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
W=$(mktemp -d)
trap 'rm -rf "$W"' EXIT

pass=0 fail=0
ok()  { pass=$((pass + 1)); echo "  ok   $1"; }
bad() { fail=$((fail + 1)); echo "  FAIL $1"; }

WANT=0.62.0 FIRV=1.42.0 REV=1111111111111111111111111111111111111111

# A private repo copy, so the test owns dist.lock.
mkdir -p "$W/repo/scripts" "$W/bots" "$W/stubs"
cp "$ROOT/scripts/converge.sh" "$W/repo/scripts/"
cp "$ROOT/distro.json" "$W/repo/"
jq -n --arg za "$WANT" --arg fir "$FIRV" --arg ext "$REV" \
  '{zulip_acp: $za, fir: $fir, exts: {"github.com/kfet/fir-exts": $ext}}' >"$W/repo/dist.lock"
C="$W/repo/scripts/converge.sh"
export FLEET_BOTS_DIR="$W/bots" CALLS="$W/calls" VERIFY_WAIT=2 CONVERGE_RSH="$W/rsh"

# Two bots on host-a (two units), one on host-b.
base="$ROOT/test/fixtures/bots/bot-a.json"
jq '.host = "host-a"' "$base" >"$W/bots/bot-a.json"
jq '.name = "bot-c" | .host = "host-a" | .unit = "zulip-acp-c"
    | .service.config_path = "~/.config/zulip-acp-c/config.json"' "$base" >"$W/bots/bot-c.json"
jq '.name = "bot-b" | .host = "host-b"' "$base" >"$W/bots/bot-b.json"

cat >"$W/rsh" <<'EOF'
#!/usr/bin/env bash
H="$W/hosts/$1"
[ -d "$H" ] || { echo "no route to $1" >&2; exit 255; }
HOME=$H FAKEHOST=$1 PATH="$H/.local/bin:$W/stubs:$PATH" CONVERGE_PROC="$H/proc" bash -c "$2"
EOF
sed -i "2i W='$W'" "$W/rsh"

cat >"$W/stubs/za" <<'EOF'
#!/bin/sh
s=$HOME/st
case "$1" in
  --version) cat "$s/za" ;;
  update) if [ "${2:-}" = -h ]; then
            [ "$(cat "$s/za")" = 0.40.0 ] || echo "  -binary string"; exit 0
          fi
          echo "$FAKEHOST zulip-acp $*" >>"$CALLS"
          [ -f "$s/warn" ] && echo "warn:   /usr/local/bin/zulip-acp is 0.1.0 but its directory is not writable"
          while [ $# -gt 0 ]; do [ "$1" = --version ] && echo "${2#v}" >"$s/za"; shift; done ;;
esac
EOF
cat >"$W/stubs/fir" <<'EOF'
#!/bin/sh
s=$HOME/st
case "$1 ${2:-}" in
  "--version "*) echo "fir $(cat "$s/fir")" ;;
  "update "*) echo "$FAKEHOST fir update" >>"$CALLS"; cat "$s/fir_latest" >"$s/fir" ;;
  "packages list") [ -f "$s/rev" ] && echo "github.com/kfet/fir-exts  git  ~/.config/fir/pkg" ;;
  "packages update") echo "$FAKEHOST fir packages update" >>"$CALLS"; echo "$REV" >"$s/rev" ;;
  "install "*) echo "$FAKEHOST fir install" >>"$CALLS"; echo "$REV" >"$s/rev" ;;
esac
EOF
cat >"$W/stubs/git" <<'EOF'
#!/bin/sh
cat "$HOME/st/rev"
EOF
cat >"$W/stubs/journalctl" <<'EOF'
#!/bin/sh
echo "journal: stub line for $*"
EOF
# systemctl: ActiveState and MainPID per unit; a recycle points the pid's
# image at the binary on disk, unless st/hang says the drain never ends.
cat >"$W/stubs/systemctl" <<'EOF'
#!/bin/sh
s=$HOME/st
shift # --user
case "$1" in
  show) u=$5
        case "$3" in
          ActiveState) cat "$s/state.$u" 2>/dev/null || echo inactive ;;
          MainPID)     cat "$s/pid.$u" 2>/dev/null || echo 0 ;;
        esac
        exit 0 ;;
esac
echo "$FAKEHOST systemctl $*" >>"$CALLS"
case "$1" in
  daemon-reload) exit 0 ;;
  enable) u=$3 ;;
  *) u=$2 ;;
esac
pid=$(cat "$s/pid.$u" 2>/dev/null || echo 0)
[ "$1" = reload ] || pid=$((pid + 100))
[ -f "$s/hang" ] && exit 0
echo active >"$s/state.$u"; echo "$pid" >"$s/pid.$u"
mkdir -p "$HOME/proc/$pid"
cp "$s/za" "$s/run.$pid"
printf '#!/bin/sh\ncat "%s"\n' "$s/run.$pid" >"$HOME/proc/$pid/exe"; chmod +x "$HOME/proc/$pid/exe"
EOF
sed -i "2i REV=$REV" "$W/stubs/fir"
chmod +x "$W"/rsh "$W"/stubs/*

# mkhost <host> <za-version> — a provisioned host: binary, fir, ext, secret.
mkhost() {
  local H="$W/hosts/$1"
  rm -rf "$H"; mkdir -p "$H/st" "$H/.local/bin" "$H/.config/zulip-acp" "$H/proc/self"
  cp "$W/stubs/za" "$H/.local/bin/zulip-acp"
  echo "$2" >"$H/st/za"; echo "$FIRV" >"$H/st/fir"; echo "$FIRV" >"$H/st/fir_latest"; echo "$REV" >"$H/st/rev"
  echo ZULIP_API_KEY=x >"$H/.config/zulip-acp/env"
  echo "0::/user.slice/session-1.scope" >"$H/proc/self/cgroup"
}
# running <host> <unit> <version> — the unit is active on an image of version.
running() {
  local H="$W/hosts/$1" pid=$((RANDOM + 1000))
  echo active >"$H/st/state.$2.service"; echo "$pid" >"$H/st/pid.$2.service"
  mkdir -p "$H/proc/$pid"; echo "$3" >"$H/st/run.$pid"
  printf '#!/bin/sh\ncat "%s"\n' "$H/st/run.$pid" >"$H/proc/$pid/exe"; chmod +x "$H/proc/$pid/exe"
}
# converged <host> <bot>... — files and units already match.
converged() {
  local h=$1; shift
  "$C" "$@" --apply >/dev/null 2>&1 || true
  : >"$CALLS"
}
calls() { cat "$CALLS" 2>/dev/null || true; }
run() { : >"$CALLS"; out=$("$C" "$@" 2>&1) && rc=0 || rc=$?; }
has() { case "$out" in *"$1"*) ok "$2" ;; *) bad "$2: no '$1' in:"; echo "$out" ;; esac; }
seq_is() { # <label> <expected call log>
  if [ "$(calls)" = "$2" ]; then ok "$1"; else bad "$1"; diff <(echo "$2") <(calls) || true; fi
}

echo "== recycle-action (pure)"
xa() { local got; got=$("$C" recycle-action "${@:2}"); [ "$got" = "$1" ] && ok "$1 <- ${*:2}" || bad "wanted $1, got $got for ${*:2}"; }
xa start   0 inactive '' 1.0.0 0
xa start   1 failed   '' 1.0.0 1
xa restart 1 active   1.0.0 1.0.0 1
xa pending 0 reloading 0.9.0 1.0.0 0
xa reload  0 active   1.0.0 1.0.0 1
xa reload  0 active   0.9.0 1.0.0 0
xa none    0 active   1.0.0 1.0.0 0

echo "== fresh host: install config + unit, daemon-reload, enable --now, no reload"
mkhost host-b 0.61.0
run bot-b
[ "$rc" = 0 ] && ok "dry run exits 0" || bad "dry run rc=$rc"
has "DRY RUN" "dry run is the default"
[ -z "$(calls)" ] && [ ! -f "$W/hosts/host-b/.config/zulip-acp/config.json" ] && ok "dry run writes and calls nothing" || bad "dry run touched the host"
run bot-b --apply
[ "$rc" = 0 ] && ok "apply exits 0" || { bad "apply rc=$rc"; echo "$out"; }
seq_is "fresh host call sequence" "host-b zulip-acp update --binary $W/hosts/host-b/.local/bin/zulip-acp --version v$WANT --no-reload -y
host-b systemctl daemon-reload
host-b systemctl enable --now zulip-acp.service"
has "running $WANT ✓" "start verified against the running image"
plan_at=${out%%== apply*}
case "$plan_at" in *"PLAN write unit"*) ok "the whole plan prints before the first write" ;; *) bad "plan not before apply"; echo "$out" ;; esac

echo "== idempotent rerun: writes nothing, reloads nothing"
run bot-b --apply
[ "$rc" = 0 ] && ok "rerun exits 0" || bad "rerun rc=$rc"
has "converged: nothing to do" "rerun says nothing to do"
seq_is "rerun makes no call" ""
[ -z "$(find "$W/hosts/host-b" -name '*.bak-*')" ] && ok "no backups written" || bad "spurious backups"

echo "== version bump: update, then ONE reload, verified"
echo 0.61.5 >"$W/hosts/host-b/st/za"; running host-b zulip-acp 0.61.5
run bot-b --apply
seq_is "bump call sequence" "host-b zulip-acp update --binary $W/hosts/host-b/.local/bin/zulip-acp --version v$WANT --no-reload -y
host-b systemctl reload zulip-acp.service"

echo "== config-only change: write + reload, no update, backup kept"
echo '{"x":1}' >"$W/hosts/host-b/.config/zulip-acp/config.json"
run bot-b --apply
seq_is "config-only call sequence" "host-b systemctl reload zulip-acp.service"
[ -n "$(find "$W/hosts/host-b/.config/zulip-acp" -name 'config.json.bak-*')" ] && ok "backup on overwrite" || bad "no backup"

echo "== unit change: daemon-reload + ONE restart, no reload"
sed -i 's/^Restart=.*/Restart=always/' "$W/hosts/host-b/.config/systemd/user/zulip-acp.service"
run bot-b --apply
seq_is "unit-change call sequence" "host-b systemctl daemon-reload
host-b systemctl restart zulip-acp.service"
has "WARNING: drops in-flight turns" "restart is announced loudly"

echo "== stale running image with current files: reload"
running host-b zulip-acp 0.61.5
run bot-b --apply
seq_is "stale image reloads" "host-b systemctl reload zulip-acp.service"

echo "== stray root-owned install: update warns, converge succeeds"
echo 0.61.5 >"$W/hosts/host-b/st/za"; touch "$W/hosts/host-b/st/warn"
run bot-b --apply
[ "$rc" = 0 ] && ok "a warning from update does not fail converge" || { bad "rc=$rc"; echo "$out"; }
rm "$W/hosts/host-b/st/warn"

echo "== binary without --binary: the no-tty single-binary update"
echo 0.40.0 >"$W/hosts/host-b/st/za"
run bot-b --apply
seq_is "legacy update call" "host-b zulip-acp update --version v$WANT
host-b systemctl reload zulip-acp.service"

echo "== below SELFUPDATE_MIN: refuse with the install.sh line, touch nothing"
echo 0.18.0 >"$W/hosts/host-b/st/za"
run bot-b --apply
[ "$rc" != 0 ] && ok "refused" || bad "an old binary must refuse"
has "install.sh" "refusal gives the bootstrap command"
seq_is "refusal makes no call" ""
echo "$WANT" >"$W/hosts/host-b/st/za"

echo "== missing secret: refuse before any write"
mkhost host-c "$WANT"; jq '.name = "bot-e" | .host = "host-c"' "$base" >"$W/bots/bot-e.json"
rm "$W/hosts/host-c/.config/zulip-acp/env"
run bot-e --apply
[ "$rc" != 0 ] && ok "refused" || bad "missing env must refuse"
has "ZULIP_API_KEY" "refusal names the secret"
rm "$W/bots/bot-e.json"

echo "== two bots on one host: one update, one fir update, two reloads"
mkhost host-a 0.61.0
echo 1.41.0 >"$W/hosts/host-a/st/fir"
converged host-a bot-a bot-c
echo 0.61.5 >"$W/hosts/host-a/st/za"; echo 1.41.0 >"$W/hosts/host-a/st/fir"
running host-a zulip-acp 0.61.5; running host-a zulip-acp-c 0.61.5
run bot-a bot-c --apply
seq_is "grouped call sequence" "host-a zulip-acp update --binary $W/hosts/host-a/.local/bin/zulip-acp --version v$WANT --no-reload -y
host-a fir update
host-a systemctl reload zulip-acp.service
host-a systemctl reload zulip-acp-c.service"
run --all
has "== plan host-a" "--all plans host-a"
has "== plan host-b" "--all plans host-b"

echo "== reload that never takes effect: fail with the journal"
echo 0.61.5 >"$W/hosts/host-b/st/za"; running host-b zulip-acp 0.61.5
touch "$W/hosts/host-b/st/hang"
run bot-b --apply
[ "$rc" != 0 ] && ok "verify timeout fails" || bad "timeout must fail"
has "journal: stub line" "failure shows the journal"
rm "$W/hosts/host-b/st/hang"

echo "== already reloading: do not signal again"
echo reloading >"$W/hosts/host-b/st/state.zulip-acp.service"
run bot-b --apply
seq_is "no second SIGHUP" ""
has "already draining" "pending reload reported"

echo "== self unit: reload, then 'reload pending', exit 0"
running host-b zulip-acp 0.61.5
echo "0::/user.slice/user@1000.service/app.slice/zulip-acp.service" >"$W/hosts/host-b/proc/self/cgroup"
run bot-b --apply
[ "$rc" = 0 ] && ok "self reload exits 0" || { bad "rc=$rc"; echo "$out"; }
has "reload pending: this relay reloads when the current turn ends" "self case says pending"
seq_is "self case reloads once" "host-b systemctl reload zulip-acp.service"

echo "== self unit + unit change: write the files, refuse the restart"
sed -i 's/^Restart=.*/Restart=always/' "$W/hosts/host-b/.config/systemd/user/zulip-acp.service"
run bot-b --apply
[ "$rc" != 0 ] && ok "self restart refused" || bad "self restart must refuse"
grep -q '^Restart=on-failure' "$W/hosts/host-b/.config/systemd/user/zulip-acp.service" && ok "unit still written" || bad "unit not written"
case "$(calls)" in *restart*) bad "restarted itself" ;; *) ok "no restart issued" ;; esac

echo "== the rendered unit keeps ExecReload"
grep -qxF 'ExecReload=/bin/kill -HUP $MAINPID' "$W/hosts/host-b/.config/systemd/user/zulip-acp.service" && ok "ExecReload present" || bad "ExecReload missing"

echo "== quick_models: default from the host's fir logins, never over a user block"
mkhost host-q "$WANT"; jq '.name = "bot-q" | .host = "host-q"' "$base" >"$W/bots/bot-q.json"
cfg="$W/hosts/host-q/.config/zulip-acp/config.json"
run bot-q --apply
has "no catalog provider has a fir login" "no logins: no block, and it says so"
jq -e 'has("quick_models") | not' "$cfg" >/dev/null && ok "no logins: no quick_models" || bad "no logins must write no block"
mkdir -p "$W/hosts/host-q/.config/fir"
echo '{"anthropic":{"access":"SECRET"},"poe#work":{},"nosuchprovider":{}}' >"$W/hosts/host-q/.config/fir/auth.json"
run bot-q --apply
got=$(jq -c '[.quick_models[].emoji]' "$cfg")
want=$(jq -c '[.quick_models_catalog[] | select(.provider == "anthropic" or .provider == "poe") | .emoji]' "$ROOT/distro.json")
[ "$got" = "$want" ] && ok "default block = catalog entries with a login" || bad "wanted $want, got $got"
grep -q SECRET "$cfg" && bad "a secret leaked" || ok "no secret crosses into config.json"
run bot-q --apply
has "converged: nothing to do" "the default block is stable"
jq '.quick_models = [{"emoji":"fish","model":"m/x"}] | .quick_models_menu_emoji = "tools"' "$cfg" >"$cfg.n" && mv "$cfg.n" "$cfg"
run bot-q --apply
jq -e '.quick_models == [{"emoji":"fish","model":"m/x"}] and .quick_models_menu_emoji == "tools"' "$cfg" >/dev/null \
  && ok "a user block on the host is kept" || bad "user block overwritten"
for badcat in '"x"' '[{"provider":"a","emoji":"","model":"m"}]' \
              '[{"provider":"a","emoji":"e","model":"m"},{"provider":"b","emoji":"e","model":"n"}]'; do
  jq --argjson c "$badcat" '.quick_models_catalog = $c' "$ROOT/distro.json" >"$W/distro-bad.json"
  FLEET_DISTRO="$W/distro-bad.json" "$C" bot-q >/dev/null 2>&1 && bad "bad catalog accepted: $badcat" || ok "bad catalog rejected: $badcat"
done
rm "$W/bots/bot-q.json"

echo "== unreachable host"
jq '.name = "bot-z" | .host = "host-z"' "$base" >"$W/bots/bot-z.json"
run bot-z
[ "$rc" != 0 ] && ok "unreachable fails" || bad "unreachable must fail"
has "cannot reach host-z" "names the host"

echo
echo "passed=$pass failed=$fail"
[ "$fail" -eq 0 ]
