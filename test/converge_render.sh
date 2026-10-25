#!/usr/bin/env bash
# shellcheck disable=SC2015  # `check && ok || bad` is the assertion idiom here; ok/bad never fail
# converge_render.sh — offline tests for scripts/converge.sh.
#
# 1. Golden-fixture tests: render config/execstart/unit for every bot spec and
#    compare against test/golden/.
# 2. Absent-key edges: an absent service key must emit no flag at all.
# 3. Spec-level gates (require, merge, no-leak). The end-to-end apply flows
#    live in test/converge_test.sh.
#
# Run from anywhere: ./test/converge_render.sh
# Regenerate goldens after an intentional renderer change:
#   REGEN=1 ./test/converge_render.sh
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
CONVERGE="$ROOT/scripts/converge.sh"
GOLDEN="$ROOT/test/golden"
# The live registry is ~/sync/shared/fleet/bots (shared by every relay);
# tests run against a frozen copy so they are hermetic.
FIXTURES="$ROOT/test/fixtures/bots"
export FLEET_BOTS_DIR="$FIXTURES"
# Never reach a real host from this suite.
export CONVERGE_RSH=false
mapfile -t BOTS < <(cd "$FIXTURES" && for f in ./*.json; do
  [ "$(jq -r '.relay // empty' "$f")" = zulip-acp ] && basename "$f" .json; done)

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
FAKE_CONVERGE="$tmpd/fake-converge"
printf '#!/bin/sh\nFLEET_BOTS_DIR=%s exec %s "$@"\n' "$tmpd/repo/bots" "$tmpd/repo/scripts/converge.sh" >"$FAKE_CONVERGE"
chmod +x "$FAKE_CONVERGE"
DEFAULT_AGENT='{"cmd": "fir --mode acp", "kind": "fir"}'
mkspec() { # <name> <service-json-extra> [agent-json]
  local agent=${3:-$DEFAULT_AGENT}
  cat >"$tmpd/repo/bots/$1.json" <<EOF
{
  "name": "$1", "relay": "zulip-acp", "host": "nowhere", "platform": "linux/amd64",
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
mkspec full ', "state_dir": "~/.config/zulip-acp/bot-full/state", "channels": "fleet,chan-a", "reload_drain_deadline": "45m", "drain_deadline": "30s"'
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
mkspec eamp ', "channels": "R&D,chan-a"'
case "$("$FAKE_CONVERGE" render eamp execstart)" in
  *'--channels "R&D,chan-a"'*) ok "an & in a channel name is allowed" ;;
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
  "name": "emac", "relay": "zulip-acp", "host": "nowhere", "platform": "darwin/arm64",
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

echo "== semver comparison"
scmp() { # <expected> <a> <b>
  local got; got=$("$CONVERGE" semver-cmp "$2" "$3")
  [ "$got" = "$1" ] && ok "cmp $2 $3 = $1" || bad "cmp $2 $3: wanted $1, got $got"
}
scmp  1 0.10.0 0.9.0      # the classic sort -V / lexicographic trap
scmp -1 0.9.0  0.10.0
scmp  0 1.2.3  1.2.3
scmp  1 1.2.10 1.2.9
scmp -1 1.2.3  2.0.0
scmp  1 2.0.0  1.99.99
scmp  0 0.08.1 0.8.1      # a leading zero is decimal, never octal
for badv in "1.2" "1.2.3.4" "v1.2.3" "1.2.3-dev+abc" "" "x.y.z" "1.2.3."; do
  if "$CONVERGE" semver-cmp "$badv" 1.0.0 >/dev/null 2>&1; then
    bad "semver-cmp must reject '$badv'"
  else
    ok "semver-cmp rejects '$badv'"
  fi
done

echo "== constraint satisfaction"
sat() { # <yes|no> <version> <constraint>
  local got; got=$("$CONVERGE" semver-sat "$2" "$3" 2>/dev/null || true)
  [ "$got" = "$1" ] && ok "'$2' vs '$3' = $1" || bad "'$2' vs '$3': wanted $1, got '$got'"
}
sat yes 0.31.3 ">=0.31.3"
sat yes 0.32.0 ">=0.31.3"
sat yes 0.40.0 ">=0.9.0"
sat no  0.31.2 ">=0.31.3"
sat no  0.9.0  ">=0.10.0"
sat yes 0.31.3 "0.31.3"          # bare = exact
sat yes 0.31.3 "=0.31.3"
sat no  0.31.4 "0.31.3"
sat yes 0.31.3 "<=0.31.3"
sat no  0.31.4 "<=0.31.3"
sat yes 0.31.2 "<0.31.3"
sat no  0.31.3 "<0.31.3"
sat yes 0.31.4 ">0.31.3"
sat no  0.31.3 ">0.31.3"
sat yes 1.11.4 "~>1.11.0"        # pessimistic: same minor
sat yes 1.11.0 "~>1.11.0"
sat no  1.12.0 "~>1.11.0"
sat no  1.10.9 "~>1.11.0"
sat yes 1.5.0  ">=1.0.0 <2.0.0"  # whitespace-separated terms are ANDed
sat no  2.0.0  ">=1.0.0 <2.0.0"
# A dev/prerelease build is NOT the release, and must satisfy NOTHING — this
# is the whole reason sort -V cannot be used: it ranks 0.31.3-dev ABOVE 0.31.3.
for devv in "0.31.3-dev+abc123" "0.31.3-dev+abc123.dirty" "0.31.4-rc1" "0.32.0-dev" "v0.32.0"; do
  sat no "$devv" ">=0.31.3"
  sat no "$devv" "0.31.3"
  sat no "$devv" "<=9.9.9"
done
for badc in "" ">=" ">=1.2" "~>1" "foo" ">=1.2.3-dev" ">=v1.2.3" " " "	" "
"; do
  if "$CONVERGE" semver-sat 1.2.3 "$badc" >/dev/null 2>&1; then
    bad "an invalid constraint '$badc' must never be satisfied"
  else
    ok "invalid constraint rejected: '$badc'"
  fi
done

echo "== require block validation"
reqspec() { # <name> <require-json>
  jq --argjson r "$2" '.require = $r' "$tmpd/repo/bots/bare.json" >"$tmpd/repo/bots/$1.json"
}
req_bad() { # <label> <require-json>
  reqspec reqbad "$2"
  if out=$("$FAKE_CONVERGE" render reqbad unit 2>&1); then
    bad "$1 must be rejected"
  else
    case "$out" in
      *"reqbad.json"*) ok "$1 rejected, and the message names the file" ;;
      *) bad "$1 rejected but the error does not name the spec: $out" ;;
    esac
  fi
}
req_bad "a non-object require"        '"0.31.3"'
req_bad "an unknown require key"      '{"zulipacp": ">=0.31.3"}'
req_bad "a non-string constraint"     '{"fir": 1}'
req_bad "a malformed constraint"      '{"fir": ">= 1.11"}'
req_bad "a whitespace-only constraint" '{"fir": "  "}'
req_bad "a tab-only constraint"        '{"fir": "\t"}'
req_bad "an empty constraint"          '{"fir": ""}'
req_bad "an empty require key"         '{"": ">=1.0.0"}'
req_bad "a prerelease in a constraint" '{"fir": ">=1.11.0-rc1"}'
reqspec reqok '{"zulip_acp": ">=0.1.0", "fir": "~>1.11.0"}'
"$FAKE_CONVERGE" render reqok unit >/dev/null 2>&1 \
  && ok "a well-formed require block validates" || bad "a valid require must pass"
# Absent require == today's behaviour, exactly.
"$FAKE_CONVERGE" render bare unit >/dev/null \
  && ok "a spec with no require block still renders" || bad "require must stay optional"
# A registry entry for another relay is never ours.
if "$CONVERGE" render bot-d unit >/dev/null 2>&1; then
  bad "a poe-acp registry entry must be refused"
else
  ok "non-zulip-acp registry entry (bot-d) refused"
fi
# Every zulip-acp spec in the registry fixture must validate.
for bot in "${BOTS[@]}"; do
  "$CONVERGE" render "$bot" unit >/dev/null \
    && ok "$bot: require block validates" || bad "$bot: require block rejected"
done

echo "== --apply refuses a lock its spec forbids"
lockfake="$tmpd/repo"
jq '.require = {"zulip_acp": ">=99.0.0"}' "$tmpd/repo/bots/bare.json" >"$tmpd/repo/bots/reqhigh.json"
jq '.host = "nowhere"' "$tmpd/repo/bots/reqhigh.json" >"$tmpd/x" && mv "$tmpd/x" "$tmpd/repo/bots/reqhigh.json"
LOCKED_ZA=$(jq -r .zulip_acp "$lockfake/dist.lock")
for mode in "" "--apply"; do
  # shellcheck disable=SC2086
  out=$("$FAKE_CONVERGE" reqhigh $mode 2>&1) && rc=0 || rc=$?
  [ "$rc" -ne 0 ] && ok "converge ${mode:---dry-run} refuses a violating lock" \
    || { bad "a violating lock must abort (${mode:---dry-run})"; echo "$out"; }
  case "$out" in
    *nowhere*">=99.0.0"*|*">=99.0.0"*nowhere*) ok "refusal names host and constraint (${mode:---dry-run})" ;;
    *) bad "refusal must name host + constraint: $out" ;;
  esac
  case "$out" in
    *"$LOCKED_ZA"*) ok "refusal names the locked version (${mode:---dry-run})" ;;
    *) bad "refusal must name the locked version: $out" ;;
  esac
done
# It must fail BEFORE any network/transport work: a spec pointing at a
# nonexistent host still fails on the constraint, not on ssh.
case $("$FAKE_CONVERGE" reqhigh --apply 2>&1 || true) in
  *"Nothing was changed"*) ok "the refusal states nothing was touched" ;;
  *) bad "refusal must state nothing was changed" ;;
esac
# The fir constraint is enforced too, not just zulip_acp.
jq '.require = {"fir": ">=99.0.0"}' "$tmpd/repo/bots/bare.json" >"$tmpd/repo/bots/reqfir.json"
out=$("$FAKE_CONVERGE" reqfir 2>&1) && rc=0 || rc=$?
[ "$rc" -ne 0 ] && ok "a violated fir constraint aborts too" || { bad "fir constraint must be enforced"; echo "$out"; }
case "$out" in *"require.fir"*) ok "the fir refusal names require.fir" ;; *) bad "expected require.fir in: $out" ;; esac
# ...but only where a fir is actually installed: a non-fir agent has no fir to
# constrain, so the gate must not fire on it.
jq '.require = {"fir": ">=99.0.0"} | .agent = {"cmd": "claude --acp", "kind": "claude"} | .fir = {"exts": []}' \
  "$tmpd/repo/bots/bare.json" >"$tmpd/repo/bots/reqnofir.json"
out=$("$FAKE_CONVERGE" reqnofir 2>&1 || true)
case "$out" in
  *"require.fir"*) bad "a non-fir agent must not be gated on fir" ;;
  *) ok "a non-fir agent skips the fir gate" ;;
esac
# A satisfied constraint does not block anything.jq --arg v "$LOCKED_ZA" '.require = {"zulip_acp": (">=" + $v)}' "$tmpd/repo/bots/bare.json" \
  >"$tmpd/repo/bots/reqok2.json"
out=$("$FAKE_CONVERGE" reqok2 2>&1 || true)
case "$out" in
  *"does not satisfy"*) bad "a satisfied constraint must not block" ;;
  *) ok "a satisfied constraint lets converge proceed" ;;
esac

echo "== --tot resolves the newest ACCEPTABLE release, not the newest"
rm -f "$tmpd/repo/bots/"*.json
mkspec pinned ""
jq '.require = {"zulip_acp": "<=0.31.3", "fir": "~>1.11.0"}' "$tmpd/repo/bots/pinned.json" >"$tmpd/x"
mv "$tmpd/x" "$tmpd/repo/bots/pinned.json"
got=$("$FAKE_CONVERGE" resolve zulip_acp 0.30.0 0.31.0 0.31.3 0.32.0 1.0.0)
[ "$got" = 0.31.3 ] && ok "a <= constraint holds the resolution back" || bad "wanted 0.31.3, got $got"
got=$("$FAKE_CONVERGE" resolve fir 1.10.0 1.11.0 1.11.9 1.12.0)
[ "$got" = 1.11.9 ] && ok "~> resolves within the minor" || bad "wanted 1.11.9, got $got"
# Two specs => the INTERSECTION, and the newest member of it.
mkspec other ""
jq '.require = {"zulip_acp": ">=0.31.0"}' "$tmpd/repo/bots/other.json" >"$tmpd/x"
mv "$tmpd/x" "$tmpd/repo/bots/other.json"
got=$("$FAKE_CONVERGE" resolve zulip_acp 0.30.0 0.31.0 0.31.3 0.32.0)
[ "$got" = 0.31.3 ] && ok "two specs intersect" || bad "wanted 0.31.3, got $got"
# An empty intersection must fail loudly rather than lock something wrong.
jq '.require = {"zulip_acp": ">=9.0.0"}' "$tmpd/repo/bots/other.json" >"$tmpd/x"
mv "$tmpd/x" "$tmpd/repo/bots/other.json"
if out=$("$FAKE_CONVERGE" resolve zulip_acp 0.30.0 0.31.3 0.32.0 2>&1); then
  bad "an unsatisfiable intersection must fail"
else
  case "$out" in
    *"no zulip_acp release satisfies"*) ok "unsatisfiable intersection fails loudly" ;;
    *) bad "expected a diagnosis, got: $out" ;;
  esac
  case "$out" in *pinned*other*|*other*pinned*) ok "the diagnosis lists both specs" ;; *) bad "diagnosis must list the constraints: $out" ;; esac
fi
# A prerelease is never resolvable, even when it is the newest string given.
rm -f "$tmpd/repo/bots/other.json"
jq '.require = {"zulip_acp": ">=0.31.0"}' "$tmpd/repo/bots/pinned.json" >"$tmpd/x"
mv "$tmpd/x" "$tmpd/repo/bots/pinned.json"
got=$("$FAKE_CONVERGE" resolve zulip_acp 0.31.3 0.32.0-dev+abc)
[ "$got" = 0.31.3 ] && ok "a dev build is never resolved into the lock" || bad "wanted 0.31.3, got $got"
# With no require block anywhere, resolution is plain newest-wins (today).
rm -f "$tmpd/repo/bots/"*.json
mkspec plain ""
got=$("$FAKE_CONVERGE" resolve zulip_acp 0.30.0 0.32.0 0.31.3)
[ "$got" = 0.32.0 ] && ok "no constraints => newest wins, as before" || bad "wanted 0.32.0, got $got"
# Restore the fixture spec the later tests rely on.
rm -f "$tmpd/repo/bots/"*.json
mkspec bare ""

echo "== self-host detection (localhost needs no ssh)"
# Only a CERTAIN yes may switch the transport: a wrong yes writes to the wrong
# machine. So the names of this very box answer self, and everything else —
# including an unresolvable name — answers remote and keeps ssh.
xself() { # <self|remote> <label> <host>
  local want=$1 label=$2 host=$3 got
  if got=$("$CONVERGE" self-host "$host" 2>/dev/null); then got=self; else got=remote; fi
  [ "$got" = "$want" ] && ok "$label" || bad "$label: wanted $want, got $got"
}
xself self   "loopback by name"        localhost
xself self   "loopback by address"     127.0.0.1
xself self   "this machine's hostname" "$(hostname)"
xself remote "a name that is not us"   github.com
xself remote "an unresolvable name"    nosuchhost.invalid.example
xself remote "an empty name"           ""

echo "== distro.json <- bot merge"
mtmp=$(mktemp -d)
mkdir -p "$mtmp/bots"
cat >"$mtmp/distro.json" <<'JSON'
{"binary": "~/.local/bin/zulip-acp",
 "config": {"mode": "default", "list": ["d1", "d2"],
            "nested": {"keep": "d", "swap": "d"}, "gone": "d"}}
JSON
cat >"$mtmp/bots/bot-a.json" <<'JSON'
{"name": "bot-a", "relay": "zulip-acp", "host": "host-a",
 "config": {"site": "https://bot-a.example.invalid", "list": ["b1"],
            "nested": {"swap": "b"}, "gone": null}}
JSON
cat >"$mtmp/bots/bot-u.json" <<'JSON'
{"name": "bot-u", "relay": "zulip-acp", "host": "host-a", "managed": false,
 "config": {"site": "https://bot-u.example.invalid"}}
JSON
mrender() { FLEET_DISTRO="$mtmp/distro.json" FLEET_BOTS_DIR="$mtmp/bots" "$CONVERGE" render "$1" config | jq -c "$2"; }
[ "$(mrender bot-a .mode)" = '"default"' ] && ok "merge: a default fills an absent key" || bad "merge: default not used"
[ "$(mrender bot-a .site)" = '"https://bot-a.example.invalid"' ] && ok "merge: a bot-only key survives" || bad "merge: bot-only key lost"
[ "$(mrender bot-a .list)" = '["b1"]' ] && ok "merge: a bot array replaces the default array" || bad "merge: arrays were not replaced"
[ "$(mrender bot-a .nested)" = '{"swap":"b","keep":"d"}' ] && ok "merge: nested objects merge, bot wins, bot key order first" || bad "merge: nested merge wrong: $(mrender bot-a .nested)"
[ "$(mrender bot-a .gone)" = 'null' ] && ok "merge: a bot null overrides a default" || bad "merge: null did not override"
[ "$(mrender bot-a 'keys_unsorted')" = '["site","list","nested","gone","mode"]' ] && ok "merge: bot key order kept, default-only keys appended" || bad "merge: key order $(mrender bot-a keys_unsorted)"
[ "$(mrender bot-u .)" = '{"site":"https://bot-u.example.invalid"}' ] && ok "merge: managed=false bots are not merged" || bad "merge: unmanaged bot got defaults"
[ "$(FLEET_DISTRO="$mtmp/absent.json" FLEET_BOTS_DIR="$mtmp/bots" "$CONVERGE" render bot-a config | jq -c .mode)" = null ] && ok "merge: no distro file means the bot file as-is" || bad "merge: absent distro not handled"
jq -e 'type == "object"' "$ROOT/distro.json" >/dev/null && ok "distro.json is a JSON object" || bad "distro.json is not a JSON object"
leak=$(jq -r '[paths | map(strings) | .[] | select(IN("name","host","unit","state","pin","notes","aliases","site","site_aliases","bot_email","bot_name","allowed_user_ids","state_dir"))] | unique | join(",")' "$ROOT/distro.json")
[ -z "$leak" ] && ok "distro.json holds no per-bot keys" || bad "distro.json holds per-bot keys: $leak"
rm -rf "$mtmp"

echo "== no-leak check"
ltmp=$(mktemp -d)
out=$(FLEET_BOTS_DIR="$ltmp/absent" "$ROOT/scripts/check-no-leak.sh" 2>&1) && case "$out" in *skipped*) ok "no-leak: skipped without a registry" ;; *) bad "no-leak: no skip notice: $out" ;; esac || bad "no-leak: absent registry failed"
mkdir -p "$ltmp/bots"
tok="unseen$RANDOM$RANDOM$RANDOM"  # built at run time so it is in no tracked file
printf '{"name": "bot-%s", "host": "host-%s", "config": {"site": "https://%s.example.invalid"}}' "$tok" "$tok" "$tok" >"$ltmp/bots/bot-a.json"
FLEET_BOTS_DIR="$ltmp/bots" "$ROOT/scripts/check-no-leak.sh" >/dev/null 2>&1 && ok "no-leak: unseen identifiers pass" || bad "no-leak: unseen identifiers flagged"
echo '{"name": "converge", "host": "host-a"}' >"$ltmp/bots/bot-b.json"
FLEET_BOTS_DIR="$ltmp/bots" "$ROOT/scripts/check-no-leak.sh" >/dev/null 2>&1 && bad "no-leak: a tracked identifier passed" || ok "no-leak: a tracked identifier fails the check"
rm -rf "$ltmp"

echo
echo "passed=$pass failed=$fail"
[ "$fail" -eq 0 ]
