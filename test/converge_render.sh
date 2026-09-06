#!/usr/bin/env bash
# shellcheck disable=SC2015  # `check && ok || bad` is the assertion idiom here; ok/bad never fail
# converge_render.sh — offline tests for scripts/converge.sh.
#
# 1. Golden-fixture tests: render config/execstart/unit for every bot spec and
#    compare against test/golden/.
# 2. Absent-key edges: an absent service key must emit no flag at all.
# 3. Fake-target converge: dry-run against a --target-root dir, apply, then
#    verify idempotence, backups, and both recycle mechanisms against a
#    stubbed systemd (including a stubbed /proc, so the "running image"
#    probe — the thing that decides graceful vs hard — is exercised offline).
#
# Run from anywhere: ./test/converge_render.sh
# Regenerate goldens after an intentional renderer change:
#   REGEN=1 ./test/converge_render.sh
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
CONVERGE="$ROOT/scripts/converge.sh"
GOLDEN="$ROOT/test/golden"
mapfile -t BOTS < <(cd "$ROOT/bots" && ls ./*.json | sed 's|^\./||; s|\.json$||')

pass=0 fail=0
ok()  { pass=$((pass + 1)); echo "  ok   $1"; }
bad() { fail=$((fail + 1)); echo "  FAIL $1"; }

check_golden() { # <name> <golden-file> <actual-content-file>
  if [ "${REGEN:-}" = 1 ]; then
    cp "$3" "$2"; echo "  regen $1"; return
  fi
  if diff -u "$2" "$3" >/dev/null 2>&1; then ok "$1"; else
    bad "$1"; diff -u "$2" "$3" || true
  fi
}

echo "== golden renders"
mkdir -p "$GOLDEN"
for bot in "${BOTS[@]}"; do
  for art in config execstart unit; do
    t=$(mktemp); "$CONVERGE" render "$bot" "$art" >"$t"
    check_golden "$bot.$art" "$GOLDEN/$bot.$art" "$t"; rm -f "$t"
  done
done

echo "== absent keys emit no flag"
tmpd=$(mktemp -d)
trap 'rm -rf "$tmpd"' EXIT
mkdir -p "$tmpd/repo/bots" "$tmpd/repo/scripts" "$tmpd/repo/test"
cp "$CONVERGE" "$tmpd/repo/scripts/"
cp "$ROOT/dist.lock" "$tmpd/repo/"
FAKE_CONVERGE="$tmpd/repo/scripts/converge.sh"
DEFAULT_AGENT='{"cmd": "fir --mode acp", "kind": "fir"}'
mkspec() { # <name> <service-json-extra> [agent-json]
  local agent=${3:-$DEFAULT_AGENT}
  cat >"$tmpd/repo/bots/$1.json" <<EOF
{
  "name": "$1", "host": "nowhere", "platform": "linux/amd64",
  "supervisor": "systemd-user", "unit": "zulip-acp-$1",
  "binary": "~/.local/bin/zulip-acp",
  "agent": $agent,
  "fir": {"exts": []},
  "service": {"config_path": "~/.config/zulip-acp/bot-$1/config.json"$2},
  "systemd": {"restart": "on-failure"},
  "config": {"site": "https://example.invalid", "channels": ["fleet"]},
  "credentials": {"env_file": "~/.config/zulip-acp/bot-$1/env"}
}
EOF
}
mkspec bare ""
mkspec full ', "state_dir": "~/.config/zulip-acp/bot-full/state", "channels": "fleet,ask-fir", "reload_drain_deadline": "45m", "drain_deadline": "30s"'
mkspec noagent "" '{"kind": "fir"}'
bare=$("$FAKE_CONVERGE" render bare execstart)
full=$("$FAKE_CONVERGE" render full execstart)
noagent=$("$FAKE_CONVERGE" render noagent execstart)
for flag in --state-dir --channels --reload-drain-deadline --drain-deadline; do
  case "$bare" in *"$flag"*) bad "absent key must not emit $flag" ;; *) ok "absent key emits no $flag" ;; esac
  case "$full" in *"$flag"*) ok "present key emits $flag" ;; *) bad "present key must emit $flag" ;; esac
done
case "$noagent" in
  *--agent-cmd*) bad "absent agent.cmd must not emit --agent-cmd" ;;
  *) ok "absent agent.cmd emits no flag (config.json owns it)" ;;
esac
# Every unit must carry ExecReload: without it every recycle is a
# message-losing hard restart.
case "$("$FAKE_CONVERGE" render bare unit)" in
  *"ExecReload=/bin/kill -HUP \$MAINPID"*) ok "unit always defines ExecReload" ;;
  *) bad "unit must define ExecReload" ;;
esac

echo "== misc guards"
if "$CONVERGE" --tot --apply >/dev/null 2>&1; then
  bad "--tot must refuse extra arguments"
else
  ok "--tot refuses extra arguments"
fi
# Every spec string that reaches a remote shell or a unit line is validated
# up front — including the ones consumed inside nested command substitutions,
# where a check would otherwise die in a subshell and be swallowed.
mkspec eguard ', "channels": "bad \" quote"'
if "$FAKE_CONVERGE" render eguard execstart >/dev/null 2>&1; then
  bad "channels with a double quote must be rejected"
else
  ok "spec guard rejects a double quote in channels"
fi
# A channel name may legitimately contain shell metacharacters: it only ever
# reaches a systemd ExecStart line, never a shell.
mkspec eamp ', "channels": "R&D,ask-fir"'
case "$("$FAKE_CONVERGE" render eamp execstart)" in
  *'--channels "R&D,ask-fir"'*) ok "an & in a channel name is allowed" ;;
  *) bad "channels must allow &" ;;
esac
guard_case() { # <label> <spec-json-mutation-jq>
  jq "$2" "$tmpd/repo/bots/bare.json" >"$tmpd/repo/bots/eguard2.json"
  if "$FAKE_CONVERGE" render eguard2 unit >/dev/null 2>&1; then
    bad "$1 must be rejected"
  else
    ok "spec guard rejects $1"
  fi
}
guard_case "a quote in .binary"                '.binary = "~/.local/bin/x\"y"'
guard_case "a command substitution in .credentials.env_file" '.credentials.env_file = "~/x$(id)"'
guard_case "a quote in .unit"                  '.unit = "zulip-acp'"'"'x"'
guard_case "a newline in .systemd.env_path"    '.systemd.env_path = "%h/.local/bin\n[Service]"'
guard_case "a newline in .name"                '.name = "x\nExecStartPre=/bin/rm"'
guard_case "a % specifier in .name"            '.name = "%h"'
# ... while the % specifier env_path exists to carry is still accepted.
case "$("$FAKE_CONVERGE" render bare unit)" in
  *"Environment=PATH="*|*"EnvironmentFile=%h/"*) ok "%h specifiers survive validation" ;;
  *) bad "a %h path must render" ;;
esac
cat >"$tmpd/repo/bots/emac.json" <<'EOF'
{
  "name": "emac", "host": "nowhere", "platform": "darwin/arm64",
  "supervisor": "launchd", "unit": "dev.kfet.zulip-acp",
  "binary": "~/.local/bin/zulip-acp",
  "agent": {"cmd": "fir --mode acp", "kind": "fir"},
  "fir": {"exts": []},
  "service": {}, "systemd": {},
  "config": {}, "credentials": {"env_file": "~/.config/zulip-acp/env"}
}
EOF
if "$FAKE_CONVERGE" emac >/dev/null 2>&1; then
  bad "an unsupported supervisor must be refused"
else
  ok "unsupported supervisor refused (systemd-user only)"
fi

echo "== recycle mechanism selection"
expect() { # <expected-mech> <label> <args...>
  local want=$1 label=$2; shift 2
  local got; got=$("$CONVERGE" plan-recycle "$@")
  case "$got" in
    "$want|"*) ok "$label ($got)" ;;
    *) bad "$label: wanted $want, got $got" ;;
  esac
}
#                                              unit_changed running version has_reload
expect graceful "binary-only, running 0.17.1"  0 1 0.17.1 1
expect graceful "exactly 0.12.0 qualifies"     0 1 0.12.0 1
expect hard     "unit changed"                 1 1 0.17.1 1
expect hard     "running 0.11.0 has no SIGHUP handler" 0 1 0.11.0 1
expect hard     "service not running"          0 0 ''     1
expect hard     "unit has no ExecReload"       0 1 0.17.1 0
expect hard     "running version unreadable"   0 1 ''     1
expect graceful "duplicated ExecReload still reloads" 0 1 0.17.1 2

echo "== running-image staleness (files can match while the process does not)"
xstale() { # <expected-verdict> <label> <args...>
  local want=$1 label=$2; shift 2
  local got; got=$("$CONVERGE" plan-stale "$@")
  case "$got" in
    "$want|"*) ok "$label ($got)" ;;
    *) bad "$label: wanted $want, got $got" ;;
  esac
}
#                                                     running want   running_ver
xstale current "running image is the wanted one"      1 0.17.1 0.17.1
xstale stale   "binary swapped but never recycled"    1 0.17.1 0.16.0
xstale current "unreadable version is never evidence" 1 0.17.1 ''
xstale current "a stopped bot is not stale"           0 0.17.1 ''

echo "== fake-target converge (dry-run, apply, idempotence)"
fake="$tmpd/fakehost"
mkdir -p "$fake/.local/bin"
LOCKED=$(jq -r .zulip_acp "$ROOT/dist.lock")
printf '#!/bin/sh\necho %s\n' "$LOCKED" >"$fake/.local/bin/zulip-acp"
chmod +x "$fake/.local/bin/zulip-acp"
BOT=${BOTS[0]}
env_file=$(jq -r '.credentials.env_file' "$ROOT/bots/$BOT.json" | sed "s|^~|$fake|")

# A unit whose EnvironmentFile is missing starts and dies with no usable
# journal line, so --apply must refuse before it recycles into that.
out=$("$CONVERGE" "$BOT" --target-root "$fake" --apply 2>&1) && rc=0 || rc=$?
[ "$rc" -ne 0 ] && ok "apply refuses a host with no env file" || bad "missing env file must abort"
case "$out" in *"ZULIP_API_KEY"*) ok "the refusal names the secret" ;; *) bad "expected a ZULIP_API_KEY hint"; echo "$out" ;; esac
mkdir -p "$(dirname "$env_file")"
echo 'ZULIP_API_KEY=stub' >"$env_file"

out=$("$CONVERGE" "$BOT" --target-root "$fake")
case "$out" in
  *"DRY RUN"*) ok "dry run is the default" ;;
  *) bad "expected DRY RUN banner"; echo "$out" ;;
esac
case "$out" in
  *"zulip-acp $LOCKED ✓"*) ok "stub binary version matches lock" ;;
  *) bad "expected zulip-acp version ✓"; echo "$out" ;;
esac
cfg_path=$(jq -r '.service.config_path' "$ROOT/bots/$BOT.json" | sed "s|^~|$fake|")
unit_name=$(jq -r '.unit' "$ROOT/bots/$BOT.json")
[ ! -f "$cfg_path" ] && ok "dry run wrote nothing" || bad "dry run must not write config"

out=$("$CONVERGE" "$BOT" --target-root "$fake" --apply)
[ -f "$cfg_path" ] && ok "apply wrote config" || bad "apply must write config"
[ -f "$fake/.config/systemd/user/$unit_name.service" ] \
  && ok "apply wrote unit" || bad "apply must write unit"
diff <("$CONVERGE" render "$BOT" config) "$cfg_path" >/dev/null \
  && ok "written config matches render" || bad "config on fake host differs from render"

out=$("$CONVERGE" "$BOT" --target-root "$fake" --apply)
case "$out" in *"config"*"✓"*) ok "second apply: config already converged" ;; *) bad "second apply should show config ✓"; echo "$out" ;; esac
case "$out" in *"unit"*"✓"*) ok "second apply: unit already converged" ;; *) bad "second apply should show unit ✓"; echo "$out" ;; esac
nbaks=$(find "$fake" -name "*.bak-*" | wc -l)
[ "$nbaks" -eq 0 ] && ok "no spurious backups" || bad "expected 0 backups, got $nbaks"

echo '{"x":1}' >"$cfg_path"
"$CONVERGE" "$BOT" --target-root "$fake" --apply >/dev/null
nbaks=$(find "$fake" -name "*.bak-*" | wc -l)
[ "$nbaks" -eq 1 ] && ok "backup created on overwrite" || bad "expected 1 backup, got $nbaks"

echo "== fake-target recycle (stubbed systemd + /proc: graceful reload vs hard restart)"
# The stub models zulip-acp's actual shape: ONE tracked process whose pid never
# moves across a reload, whose in-memory image (proc/<pid>/exe) is what the
# version probe reads, and which only changes pid on a hard restart.
fake3="$tmpd/fakehost3"
mkdir -p "$fake3/.local/bin" "$fake3/.stub" "$fake3/proc" "$(dirname "${env_file/$fake/$tmpd/fakehost3}")"
echo 'ZULIP_API_KEY=stub' >"${env_file/$fake/$tmpd/fakehost3}"
printf '%s\n' 999000 >"$fake3/.stub/pid"
printf '#!/bin/sh\necho %s\n' "$LOCKED" >"$fake3/.local/bin/zulip-acp"
chmod +x "$fake3/.local/bin/zulip-acp"
# An OLD image is what the tracked pid starts out executing.
printf '#!/bin/sh\necho 0.16.0\n' >"$fake3/.local/bin/zulip-acp.running-old"
chmod +x "$fake3/.local/bin/zulip-acp.running-old"
relink() { # <pid> <image>
  mkdir -p "$fake3/proc/$1"
  ln -sfn "$fake3/.local/bin/$2" "$fake3/proc/$1/exe"
}
relink 999000 zulip-acp.running-old
cat >"$fake3/.local/bin/systemctl" <<'STUB'
#!/bin/sh
s="$HOME/.stub"
echo "$*" >>"$s/log"
pid=$(cat "$s/pid")
case "$*" in
  *"show -p MainPID"*)    echo "$pid" ;;
  *"show -p ExecReload"*) echo "/bin/kill -HUP \$MAINPID" ;;
  *is-active*)            echo active ;;
  *daemon-reload*)        : ;;
  # A reload re-execs IN PLACE: same pid, new image.
  *reload*)               ln -sfn "$HOME/.local/bin/zulip-acp" "$HOME/proc/$pid/exe" ;;
  # A restart replaces the process: new pid, new image.
  *restart*)              new=$((pid + 1)); echo "$new" >"$s/pid"
                          mkdir -p "$HOME/proc/$new"
                          ln -sfn "$HOME/.local/bin/zulip-acp" "$HOME/proc/$new/exe" ;;
esac
STUB
chmod +x "$fake3/.local/bin/systemctl"

out=$("$CONVERGE" "$BOT" --target-root "$fake3")
case "$out" in
  *"running image is 0.16.0, wanted $LOCKED"*) ok "running image read from the stubbed /proc" ;;
  *) bad "expected a staleness note from the running image"; echo "$out" ;;
esac
case "$out" in
  *"would hard restart"*) ok "dry run previews the mechanism (unit missing => hard)" ;;
  *) bad "dry run must preview the mechanism"; echo "$out" ;;
esac

# First apply writes config+unit => unit changed => hard restart.
out=$("$CONVERGE" "$BOT" --target-root "$fake3" --apply)
case "$out" in
  *"hard restart (daemon-reload + restart) ✓"*) ok "unit change forces a hard restart" ;;
  *) bad "expected a hard restart on a unit change"; echo "$out" ;;
esac
case "$out" in *"unit changed"*) ok "hard restart reason reported" ;; *) bad "expected reason 'unit changed'" ;; esac
# A hard restart drops in-flight turns and every message posted before the
# fresh queue registers — it must never happen quietly.
case "$out" in
  *"WARNING: a hard restart drops"*) ok "hard restart warns about the message loss" ;;
  *) bad "hard restart must warn"; echo "$out" ;;
esac
grep -q -- "--user daemon-reload" "$fake3/.stub/log" && ok "daemon-reload issued for a unit change" \
  || bad "unit change must daemon-reload"
case "$out" in
  *"pid 999000 → 999001"*) ok "hard restart moved the pid" ;;
  *) bad "expected the pid to move across a hard restart"; echo "$out" ;;
esac

# Binary stale again, unit untouched => graceful reload, pid held.
: >"$fake3/.stub/log"
pid=$(cat "$fake3/.stub/pid")
relink "$pid" zulip-acp.running-old
out=$("$CONVERGE" "$BOT" --target-root "$fake3")
case "$out" in
  *"would graceful reload"*) ok "dry run previews a graceful reload for a binary-only change" ;;
  *) bad "dry run must preview the graceful reload"; echo "$out" ;;
esac
out=$("$CONVERGE" "$BOT" --target-root "$fake3" --apply)
case "$out" in
  *"graceful reload ✓ (pid $pid held, running $LOCKED)"*) ok "binary-only change reloads in place" ;;
  *) bad "expected a graceful reload holding the pid"; echo "$out" ;;
esac
grep -q -- "--user reload" "$fake3/.stub/log" && ok "reload issued" || bad "expected systemctl reload"
grep -q -- "restart" "$fake3/.stub/log" && bad "graceful path must not restart" || ok "no restart on the graceful path"
grep -q -- "daemon-reload" "$fake3/.stub/log" && bad "graceful path must not daemon-reload" \
  || ok "no daemon-reload when the unit did not change"

# A config-only change still recycles, and reports the accepted reload even
# though no version move is observable.
: >"$fake3/.stub/log"
echo '{"x":2}' >"$fake3$(jq -r '.service.config_path' "$ROOT/bots/$BOT.json" | sed 's|^~||')"
out=$("$CONVERGE" "$BOT" --target-root "$fake3" --apply)
case "$out" in
  *"reload accepted"*"nothing observable to verify"*) ok "config-only change reloads, and says what it could not verify" ;;
  *) bad "config-only change must reload"; echo "$out" ;;
esac

# A reload that never re-execs is NOT a failure: the relay drains in-flight
# turns first. It must report the pending exec instead of dying.
: >"$fake3/.stub/log"
pid=$(cat "$fake3/.stub/pid")
relink "$pid" zulip-acp.running-old
cat >"$fake3/.local/bin/systemctl" <<'STUB'
#!/bin/sh
case "$*" in
  *"show -p MainPID"*)    cat "$HOME/.stub/pid" ;;
  *"show -p ExecReload"*) echo "/bin/kill -HUP \$MAINPID" ;;
  *is-active*)            echo active ;;
esac
STUB
chmod +x "$fake3/.local/bin/systemctl"
out=$(RELOAD_WAIT=2 "$CONVERGE" "$BOT" --target-root "$fake3" --apply 2>&1) && rc=0 || rc=$?
[ "${rc:-0}" -eq 0 ] && ok "a still-draining reload is not an error" || { bad "drain wait must not fail"; echo "$out"; }
case "$out" in
  *"still draining"*) ok "pending re-exec reported" ;;
  *) bad "expected a draining note"; echo "$out" ;;
esac

# A reload whose pid MOVES is a restart in disguise: messages were dropped, so
# it must fail loudly.
cat >"$fake3/.local/bin/systemctl" <<'STUB'
#!/bin/sh
s="$HOME/.stub"
pid=$(cat "$s/pid")
case "$*" in
  *"show -p MainPID"*)    cat "$s/pid" ;;
  *"show -p ExecReload"*) echo "/bin/kill -HUP \$MAINPID" ;;
  *is-active*)            echo active ;;
  *reload*)               new=$((pid + 1)); echo "$new" >"$s/pid"
                          mkdir -p "$HOME/proc/$new"
                          ln -sfn "$HOME/.local/bin/zulip-acp" "$HOME/proc/$new/exe" ;;
esac
STUB
chmod +x "$fake3/.local/bin/systemctl"
pid=$(cat "$fake3/.stub/pid")
relink "$pid" zulip-acp.running-old
if RELOAD_WAIT=2 "$CONVERGE" "$BOT" --target-root "$fake3" --apply >/dev/null 2>&1; then
  bad "a reload that moves the pid must fail"
else
  ok "pid move during a reload fails loudly"
fi

echo
echo "passed=$pass failed=$fail"
[ "$fail" -eq 0 ]
