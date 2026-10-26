#!/usr/bin/env bash
# converge.sh — the only sanctioned way to change a zulip-acp bot host.
#
# Reads distro.json <- $BOTS_DIR/<bot>.json (deep-merged; relay=zulip-acp)
# plus dist.lock, and makes each target host match. Version moves go through
# the lock, never through a hand-typed command on a host.
#
# OWNERSHIP — the one rule this script is built on:
#   the Go binary owns the FILE SWAP (`zulip-acp update --binary B --no-reload`:
#   checksum-verified, atomic, ETXTBSY-safe); converge owns RENDERING, ONE
#   recycle per unit, and VERIFICATION. Nothing reloads twice.
# See docs/converge-design.md.
#
# Usage:
#   converge.sh <bot>... [--apply] [--local]   plan (default) or apply
#   converge.sh --all [--apply]                every managed bot of this relay
#   converge.sh --tot                          resolve dist.lock; NEVER converges
#   converge.sh render <bot> <artefact>        config | execstart | unit
#   converge.sh recycle-action <unit_changed> <active_state> <running> <want> <files_changed>
#   converge.sh semver-cmp <a> <b> | semver-sat <ver> <constraint>
#   converge.sh resolve <component> <ver...> | self-host <host>   (test hooks)
#
# Bots are grouped by .host: the binary, fir and fir extensions are updated
# ONCE per host; config, unit and the recycle are per bot. --apply prints the
# whole plan for every host before it writes anything.
#
# Transport: ssh by spec host alias; a host that is certainly THIS machine
# (or every host, with --local) runs through a local login bash, because a
# relay host usually cannot ssh to itself. CONVERGE_RSH=<prog> replaces the
# transport (`<prog> <host> <script>`) — the offline test harness uses it.
#
# Only systemd user services are supported.
set -euo pipefail

REPO_ROOT=$(cd "$(dirname "$0")/.." && pwd)
# ONE bot registry for the whole relay fleet, shared with poe-acp, slack-acp
# and fleet.sh. Only entries with .relay == zulip-acp are ours.
BOTS_DIR="${FLEET_BOTS_DIR:-$HOME/sync/shared/fleet/bots}"
RELAY=zulip-acp
# bot_specs: our relay's spec files, one per line.
bot_specs() {
  local f
  for f in "$BOTS_DIR"/*.json; do
    [ -f "$f" ] && [ "$(jq -r '.relay // empty' "$f")" = "$RELAY" ] && echo "$f"
  done
  return 0
}
# distro.json (next to dist.lock) holds the defaults shared by every bot of
# this relay; a bot file holds only what differs. merged_spec deep-merges
# distro <- bot: the bot wins, objects merge recursively, any other value
# (arrays included) is replaced wholesale, and a bot null unsets a default.
# Merged objects keep the BOT's key order with default-only keys appended,
# so a bot that needs a rendered key order (config.json) can restate keys.
# Bots with "managed": false are tracked, not distro instances: no merge.
DISTRO="${FLEET_DISTRO:-$REPO_ROOT/distro.json}"
MERGE_JQ='def dmerge($d; $b):
  if ($d|type) == "object" and ($b|type) == "object" then
    (reduce ($b|keys_unsorted[]) as $k ({};
       .[$k] = (if ($d|has($k)) then dmerge($d[$k]; $b[$k]) else $b[$k] end))) as $r
    | reduce ($d|keys_unsorted[]) as $k ($r; if has($k) then . else .[$k] = $d[$k] end)
  else $b end;
if $bot[0].managed == false then $bot[0] else dmerge($distro[0]; $bot[0]) end'
# merged_spec <bot-file> — print the path of the bot's merged spec.
merged_spec() {
  local out
  out=$(mktemp "$TMPD/spec.XXXXXX")
  if [ -f "$DISTRO" ]; then
    jq -n --slurpfile distro "$DISTRO" --slurpfile bot "$1" "$MERGE_JQ" >"$out" \
      || die "cannot merge $DISTRO <- $1"
  else
    cp "$1" "$out"
  fi
  echo "$out"
}
# bot_spec <name>: path of that bot's MERGED spec; dies unless it is ours.
bot_spec() {
  local spec="$BOTS_DIR/$1.json" r
  [ -f "$spec" ] || die "no such bot spec: $spec"
  r=$(jq -r '.relay // empty' "$spec")
  [ "$r" = "$RELAY" ] || die "$1 is a ${r:-relay-less} bot, not $RELAY: $spec"
  merged_spec "$spec"
}
LOCK="$REPO_ROOT/dist.lock"
STAMP=$(date +%Y%m%d-%H%M%S)
TMPD=$(mktemp -d)
trap 'rm -rf "$TMPD"' EXIT

ZULIP_ACP_REPO=kfet/zulip-acp
FIR_DIST_REPO="https://github.com/kfet/fir-dist"

# The release that first shipped `zulip-acp update`. Below it the host cannot
# update itself, and converge refuses (bootstrap once with install.sh).
SELFUPDATE_MIN=0.19.0
# How long (seconds) to wait for the recycled unit to run the wanted image.
VERIFY_WAIT=${VERIFY_WAIT:-30}
case "$VERIFY_WAIT" in ''|*[!0-9]*) echo "converge: error: VERIFY_WAIT must be a whole number of seconds" >&2; exit 1 ;; esac

die()  { echo "converge: error: $*" >&2; exit 1; }
note() { echo "  $*"; }
need() { command -v "$1" >/dev/null 2>&1 || die "missing dependency: $1"; }

need jq

# ---------------------------------------------------------------------------
# Semantic version comparison
#
# Hand-written on purpose. `sort -V` is not a semver comparator: it happily
# orders `v0.9.0` after `v0.10.0` only by accident of the leading `v`, and it
# ranks `0.31.3-dev+abc` ABOVE `0.31.3`, which is exactly backwards — a dev
# build is NOT the release. Shelling out to python/node is worse: converge
# must run on a bare host with nothing but coreutils, jq and ssh.
#
# The rule that matters: only a clean X.Y.Z is a release. Any prerelease or
# build-metadata suffix (`-dev+sha`, `-rc1`, `.dirty`) satisfies NO constraint,
# so a dev binary can never be locked into the fleet by accident.
# ---------------------------------------------------------------------------

# is_release_version <s> — true iff s is exactly X.Y.Z, all numeric.
is_release_version() {
  local v=$1 a b c d
  case "$v" in ''|*[!0-9.]*|.*|*.) return 1 ;; esac
  IFS=. read -r a b c d <<<"$v"
  [ -n "$a" ] && [ -n "$b" ] && [ -n "$c" ] && [ -z "${d:-}" ]
}

# ver_cmp <a> <b> — print -1, 0 or 1. Both must be release versions.
ver_cmp() {
  local a1 a2 a3 b1 b2 b3 x y i
  is_release_version "$1" || die "not a release version: $1"
  is_release_version "$2" || die "not a release version: $2"
  IFS=. read -r a1 a2 a3 <<<"$1"
  IFS=. read -r b1 b2 b3 <<<"$2"
  for i in 1 2 3; do
    case $i in
      1) x=$a1; y=$b1 ;;
      2) x=$a2; y=$b2 ;;
      3) x=$a3; y=$b3 ;;
    esac
    # 10# forces decimal: an 08 segment is not octal here.
    if [ "$((10#$x))" -lt "$((10#$y))" ]; then echo -1; return 0; fi
    if [ "$((10#$x))" -gt "$((10#$y))" ]; then echo 1; return 0; fi
  done
  echo 0
}

# constraint_terms <expr> — normalise a constraint into "<op> <version>" lines,
# or fail. A constraint is whitespace-separated terms, ANDed:
#   >=X.Y.Z  <=X.Y.Z  >X.Y.Z  <X.Y.Z  =X.Y.Z  X.Y.Z (exact)  ~>X.Y.Z
# ~>X.Y.Z is the pessimistic operator: >=X.Y.Z and <X.(Y+1).0.
constraint_terms() {
  local expr=$1 term op want out=""
  for term in $expr; do
    case "$term" in
      '>='*)  op=ge; want=${term#>=} ;;
      '<='*)  op=le; want=${term#<=} ;;
      '~>'*)  op=tw; want=${term#'~>'} ;;
      '>'*)   op=gt; want=${term#>} ;;
      '<'*)   op=lt; want=${term#<} ;;
      '='*)   op=eq; want=${term#=} ;;
      *)      op=eq; want=$term ;;
    esac
    is_release_version "$want" || return 1
    out+="$op $want"$'\n'
  done
  # A constraint that yielded no terms — empty, or nothing but whitespace — is
  # a failure, never a vacuous truth: it would otherwise be satisfied by every
  # version, which is the opposite of what writing it down meant.
  [ -n "$out" ] || return 1
  printf '%s' "$out"
}

# valid_constraint <expr>
valid_constraint() { constraint_terms "$1" >/dev/null 2>&1; }

# ver_satisfies <version> <constraint> — true iff version meets every term.
# A non-release version (prerelease / build metadata) satisfies nothing.
ver_satisfies() {
  local v=$1 expr=$2 terms op want c w1 w2
  is_release_version "$v" || return 1
  terms=$(constraint_terms "$expr") || return 1
  while read -r op want; do
    [ -n "$op" ] || continue
    c=$(ver_cmp "$v" "$want")
    case "$op" in
      ge) [ "$c" -ge 0 ] || return 1 ;;
      gt) [ "$c" -gt 0 ] || return 1 ;;
      le) [ "$c" -le 0 ] || return 1 ;;
      lt) [ "$c" -lt 0 ] || return 1 ;;
      eq) [ "$c" -eq 0 ] || return 1 ;;
      tw) [ "$c" -ge 0 ] || return 1
          IFS=. read -r w1 w2 _ <<<"$want"
          [ "$(ver_cmp "$v" "$((10#$w1)).$((10#$w2 + 1)).0")" -lt 0 ] || return 1 ;;
    esac
  done <<<"$terms"
  return 0
}

# ---------------------------------------------------------------------------
# require blocks: the constraint side of constraint-vs-resolution
#
# A spec's `require` is OPTIONAL and names components, not hosts:
#   "require": { "zulip_acp": ">=0.31.3", "fir": ">=1.11.0" }
# A spec without one behaves exactly as before.
# ---------------------------------------------------------------------------
REQUIRE_COMPONENTS="zulip_acp fir"

# spec_require <spec-file> <component> — the constraint, or empty.
spec_require() { jq -r --arg k "$2" '.require[$k] // empty' "$1"; }

# all_requires <component> — "<bot>\t<constraint>" for every spec that has one.
all_requires() {
  local f bot c
  for f in $(bot_specs); do
    [ -f "$f" ] || continue
    bot=$(basename "$f" .json)
    c=$(spec_require "$(merged_spec "$f")" "$1")
    [ -n "$c" ] && printf '%s\t%s\n' "$bot" "$c"
  done
  return 0
}

# resolve_satisfying <component> <version...> — the NEWEST given version that
# satisfies every spec's constraint for that component. Empty if none does.
resolve_satisfying() {
  local comp=$1 v best="" reqs c
  shift
  reqs=$(all_requires "$comp")
  for v in "$@"; do
    is_release_version "$v" || continue
    if [ -n "$reqs" ]; then
      while IFS=$'\t' read -r _ c; do
        [ -n "$c" ] || continue
        ver_satisfies "$v" "$c" || continue 2
      done <<<"$reqs"
    fi
    if [ -z "$best" ] || [ "$(ver_cmp "$v" "$best")" -gt 0 ]; then best=$v; fi
  done
  printf '%s\n' "$best"
}

# ---------------------------------------------------------------------------
# Spec access ($SPEC is set in main)
# ---------------------------------------------------------------------------
SPEC=""
# SPEC_SRC: the bot file SPEC was merged from — what error messages name.
SPEC_SRC=""

jqs() { jq -r "$1" "$SPEC"; }

# validate_spec — refuse a spec whose strings would break out of where they
# are interpolated. This runs ONCE, at top level, before anything is rendered
# or shipped: a check buried inside $(...) would die in a subshell and the
# caller would carry on with an empty value.
#
# Two rule sets, because the destinations differ:
#   shell   values reach a remote bash payload (rsh wraps it in double quotes)
#           AND a systemd unit, so a quote, $(...), backtick, ;, &, | or a
#           systemd % specifier is out.
#   literal values only reach a unit line (never a shell), so they may contain
#           shell metacharacters — a Zulip channel named "R&D" is legitimate —
#           but not a quote, a % specifier, or a newline.
validate_spec() {
  local k v
  for k in .binary .unit .credentials.env_file .service.config_path \
           .service.state_dir .service.reload_drain_deadline \
           .service.drain_deadline .agent.cmd; do
    v=$(jq -r "$k // empty" "$SPEC")
    case "$v" in
      *'"'*|*'$'*|*'`'*|*'%'*|*';'*|*'&'*|*'|'*|*"'"*|*'
'*) die "$k must not contain a quote, \$, backtick, %, ;, &, | or a newline (got: $v)" ;;
    esac
  done
  # env_path is the one unit value that MUST carry % specifiers (%h).
  # .name reaches the unit's Description and its generated-by comment.
  for k in .name .service.channels .systemd.env_path; do
    v=$(jq -r "$k // empty" "$SPEC")
    case "$v" in
      *'"'*|*'
'*) die "$k must not contain a double quote or a newline (got: $v)" ;;
    esac
  done
  for k in .service.channels .name; do
    case "$(jq -r "$k // empty" "$SPEC")" in
      *'%'*) die "$k must not contain % (systemd specifier)" ;;
    esac
  done
  validate_require
  validate_quick_catalog
}

# validate_quick_catalog — `quick_models_catalog` (optional) is the list
# converge picks a host's default `quick_models` from. Each entry needs a
# provider, an emoji and a model, and no two entries may share an emoji: the
# relay refuses to start on a duplicate, so a bad catalog must die here.
validate_quick_catalog() {
  local t
  t=$(jq -r 'if has("quick_models_catalog") then (.quick_models_catalog | type) else "absent" end' "$SPEC")
  case "$t" in
    absent) return 0 ;;
    array) : ;;
    *) die "quick_models_catalog must be an array (got $t)" ;;
  esac
  jq -e '.quick_models_catalog | all(
           (.provider|type)=="string" and .provider!="" and
           (.emoji|type)=="string" and .emoji!="" and
           (.model|type)=="string" and .model!="")' "$SPEC" >/dev/null \
    || die "quick_models_catalog: every entry needs a non-empty provider, emoji and model"
  jq -e '.quick_models_catalog | (map(.emoji) | length) == (map(.emoji) | unique | length)' "$SPEC" >/dev/null \
    || die "quick_models_catalog: duplicate emoji"
}

# host_fir_providers — the providers the target's fir has a login for, one
# per line: the top-level keys of ~/.config/fir/auth.json, account suffix
# (provider#account) stripped. Only the key names cross the wire, never a
# secret. Empty when there is no auth file or no jq on the target.
host_fir_providers() {
  rsh 'f="$HOME/.config/fir/auth.json"; if [ -f "$f" ] && command -v jq >/dev/null 2>&1; then jq -r "keys[] | split(\"#\")[0]" "$f" | sort -u; fi' \
    || die "failed reading the fir logins on the target (ssh/transport error?)"
}

# fill_quick_models <want-file> <config-path> — complete the desired
# config.json with a `quick_models` block, without ever replacing one the
# user owns:
#   1. the spec's config has quick_models  -> it is the user's block; keep it.
#   2. the host's config.json has one      -> keep the host's block (and its
#                                             menu/sweep emoji keys).
#   3. otherwise                           -> build one from the spec's
#      quick_models_catalog, keeping only the providers this host's fir has a
#      login for, at most 5 entries. No catalog or no match: no block.
fill_quick_models() {
  local want=$1 cfg=$2 have tmp provs
  jq -e '.quick_models' "$want" >/dev/null 2>&1 && return 0
  have=$(mktemp "$TMPD/have.XXXXXX"); rcat "$cfg" >"$have"
  tmp=$(mktemp "$TMPD/want.XXXXXX")
  if jq -e '.quick_models' "$have" >/dev/null 2>&1; then
    jq --slurpfile h "$have" '. + ($h[0] | with_entries(select(.key |
          IN("quick_models", "quick_models_menu_emoji", "quick_models_sweep_emoji"))))' \
      "$want" >"$tmp" && mv "$tmp" "$want"
    note "quick_models: kept the host's own block"
    rm -f "$have"; return 0
  fi
  rm -f "$have"
  [ "$(jqs '.agent.kind')" = "fir" ] || return 0
  jq -e '(.quick_models_catalog // []) | length > 0' "$SPEC" >/dev/null || return 0
  provs=$(host_fir_providers | jq -R . | jq -sc .)
  jq --slurpfile s "$SPEC" --argjson p "$provs" '
      ([ ($s[0].quick_models_catalog // [])[] | select(.provider as $x | $p | index($x)) ][:5]
       | map({emoji, model} + (if .label then {label} else {} end))) as $q
      | if ($q | length) > 0 then . + {quick_models: $q} else . end' "$want" >"$tmp" && mv "$tmp" "$want"
  if jq -e '.quick_models' "$want" >/dev/null; then
    note "quick_models: default built from the host's fir logins ($(jq -r '[.quick_models[].model] | join(", ")' "$want"))"
  else
    note "quick_models: no catalog provider has a fir login on this host; none written"
  fi
}

# validate_require — the `require` block, if present, must be an object whose
# keys are known components and whose values are parseable constraints. A typo
# that is silently ignored is worse than no constraint at all: it reads like a
# guarantee and enforces nothing.
validate_require() {
  local t k c
  t=$(jq -r 'if has("require") then (.require | type) else "absent" end' "$SPEC")
  case "$t" in
    absent|null) return 0 ;;
    object) : ;;
    *) die "$SPEC_SRC: .require must be an object like {\"zulip_acp\": \">=0.31.3\"} (got $t)" ;;
  esac
  # while-read, not `for k in $(...)`: a key that word-splits to nothing (the
  # empty key) would otherwise skip the loop body entirely and validate.
  while IFS= read -r k; do
    case " $REQUIRE_COMPONENTS " in
      *" $k "*) : ;;
      *) die "$SPEC_SRC: unknown .require key \"$k\" (known: $REQUIRE_COMPONENTS)" ;;
    esac
    c=$(jq -r --arg k "$k" '.require[$k]' "$SPEC")
    [ "$(jq -r --arg k "$k" '.require[$k] | type' "$SPEC")" = string ] \
      || die "$SPEC_SRC: .require.$k must be a string constraint (e.g. \">=0.31.3\")"
    valid_constraint "$c" \
      || die "$SPEC_SRC: invalid .require.$k constraint \"$c\" — expected whitespace-separated terms of >=X.Y.Z, <=X.Y.Z, >X.Y.Z, <X.Y.Z, ~>X.Y.Z or an exact X.Y.Z"
  done < <(jq -r '.require | keys[]' "$SPEC")
}

# enforce_require <bot> <host> <component> <locked-version>
# Hard-fail when the LOCKED version violates this spec's constraint.
# Deterministic, no network, and called before anything touches the host.
enforce_require() {
  local bot=$1 host=$2 comp=$3 locked=$4 c
  c=$(spec_require "$SPEC" "$comp")
  [ -n "$c" ] || return 0
  ver_satisfies "$locked" "$c" && return 0
  die "$host ($bot): dist.lock has $comp $locked, which does not satisfy $SPEC_SRC require.$comp \"$c\". The lock is a RESOLUTION of the specs, not an override: re-resolve with \`scripts/converge.sh --tot\` (or fix the lock by hand and commit it). Nothing was changed on $host."
}

# service key helpers: "absent or false or null" => not emitted
skey() { jq -r --arg k "$1" '.service[$k] // empty' "$SPEC"; }

# ---------------------------------------------------------------------------
# Path style conversion (spec uses ~/...)
# ---------------------------------------------------------------------------
p_systemd() { printf '%s' "${1/#\~/%h}"; }     # ~/x -> %h/x
p_home()    { printf '%s' "${1/#\~/\$HOME}"; } # ~/x -> $HOME/x (remote shell)

# ---------------------------------------------------------------------------
# Renderers
# ---------------------------------------------------------------------------
render_config() { jq '.config' "$SPEC"; }

# render_execstart — the unit's ExecStart line. systemd style throughout
# (~ -> %h): the only consumer is the unit, so there is no second style.
render_execstart() {
  local out v
  out=$(p_systemd "$(jqs '.binary')")
  v=$(skey config_path);  [ -n "$v" ] && out+=" --config $(p_systemd "$v")"
  v=$(skey state_dir);    [ -n "$v" ] && out+=" --state-dir $(p_systemd "$v")"
  v=$(skey channels);     [ -n "$v" ] && out+=" --channels \"$v\""
  # An absent agent.cmd emits no flag: that host configures the agent in
  # config.json instead, and a rendered default would silently override it.
  v=$(jqs '.agent.cmd // empty'); [ -n "$v" ] && out+=" --agent-cmd \"$v\""
  v=$(skey reload_drain_deadline); [ -n "$v" ] && out+=" --reload-drain-deadline $v"
  v=$(skey drain_deadline);        [ -n "$v" ] && out+=" --drain-deadline $v"
  printf '%s\n' "$out"
}

render_unit() {
  local v
  cat <<EOF
# GENERATED by scripts/converge.sh from bots/$(jqs '.name').json — do not edit
# on the host: the next converge overwrites it (a backup is kept). Change the
# spec and re-run \`scripts/converge.sh $(jqs '.name') --apply\`.
#
# ExecReload is the graceful upgrade path: SIGHUP makes the relay stop polling
# the Zulip event queue WITHOUT deleting it, drain in-flight turns, then
# re-exec the new binary in place (same PID). See docs/graceful-reload.md.
[Unit]
Description=zulip-acp ($(jqs '.name'))
After=network-online.target
Wants=network-online.target

[Service]
EOF
  v=$(jqs '.systemd.env_path // empty')
  [ -n "$v" ] && echo "Environment=PATH=$v"
  echo "EnvironmentFile=$(p_systemd "$(jqs '.credentials.env_file')")"
  echo "ExecStart=$(render_execstart)"
  # The graceful reload IS the upgrade mechanism: SIGHUP stops polling without
  # deleting the Zulip event queue, drains in-flight turns, then re-execs the
  # new binary in place (same PID). Emitted unconditionally — a unit without
  # it forces every recycle onto a message-losing hard restart.
  # shellcheck disable=SC2016  # $MAINPID is expanded by systemd, not the shell
  echo 'ExecReload=/bin/kill -HUP $MAINPID'
  v=$(jqs '.systemd.restart // empty')
  [ -n "$v" ] && echo "Restart=$v"
  v=$(jqs '.systemd.restart_sec // empty')
  [ -n "$v" ] && echo "RestartSec=$v"
  # A stop drains in-flight turns (-drain-deadline); systemd must not SIGKILL
  # the cgroup mid-drain.
  v=$(jqs '.systemd.timeout_stop_sec // empty')
  [ -n "$v" ] && echo "TimeoutStopSec=$v"
  cat <<'EOF'

[Install]
WantedBy=default.target
EOF
}

# host_is_self <spec-host> — is the spec's host THIS machine?
#
# Converge must not need ssh to reach the box it already runs on. A relay host
# very often has no ssh key for itself (no `Host` stanza, no entry in its own
# authorized_keys), so the self-ssh is refused and converge stops on a host it
# could write to directly.
#
# The answer must be certain. A false yes points every write at the wrong
# machine, so an unknown answer is NO, and the caller falls back to ssh.
#
# Sets SELF_WHY with the evidence, for the line converge prints.
SELF_WHY=""
host_is_self() {
  local host=$1 real ip mine
  SELF_WHY=""

  # The spec host is an ssh_config ALIAS. Resolve it the way ssh would, before
  # DNS — a valid alias is not a DNS name.
  real=$(ssh -G "$host" 2>/dev/null | awk '/^hostname /{print $2; exit}' || true)
  [ -n "$real" ] || real="$host"

  case "$real" in
    localhost|127.0.0.1|::1) SELF_WHY="$host -> $real is the loopback"; return 0 ;;
  esac

  # A plain name match is enough, and it works with no network at all.
  if [ "$real" = "$(hostname 2>/dev/null)" ] || [ "$real" = "$(hostname -f 2>/dev/null)" ]; then
    SELF_WHY="$host -> $real matches this machine's hostname"
    return 0
  fi

  ip=$(getent hosts "$real" 2>/dev/null | awk '{print $1; exit}' || true)
  [ -n "$ip" ] || return 1

  # Every address this machine holds: tailscale first (the fleet talks over
  # it), then the kernel's own list. `ip` is absent on macOS, so ifconfig is
  # the fallback.
  mine=$( { tailscale ip 2>/dev/null || true
            ip -o addr show 2>/dev/null | awk '{print $4}' | cut -d/ -f1
            ifconfig 2>/dev/null | awk '/inet /{print $2}'
          } | sort -u | tr '\n' ' ')
  [ -n "$mine" ] || return 1

  if printf '%s' "$mine" | grep -qw -- "$ip"; then
    SELF_WHY="$host -> $real ($ip) is an address of this machine"
    return 0
  fi
  return 1
}
# ---------------------------------------------------------------------------
# Transport
# ---------------------------------------------------------------------------
HOST=""
FORCE_LOCAL=0
LOCAL_HOSTS=" " # hosts reached through a local bash, space-delimited

# pick_transport <host> — decide, once, how to reach host.
pick_transport() {
  [ -n "${CONVERGE_RSH:-}" ] && return 0
  if [ "$FORCE_LOCAL" = 1 ]; then
    LOCAL_HOSTS+="$1 "; echo "== $1: --local — this machine, no ssh"
  elif host_is_self "$1"; then
    LOCAL_HOSTS+="$1 "; echo "== $1: local target ($SELF_WHY) — no ssh"
  fi
  return 0
}

rsh() { # <script> — run on $HOST; stdin is forwarded
  if [ -n "${CONVERGE_RSH:-}" ]; then
    "$CONVERGE_RSH" "$HOST" "$1"; return
  fi
  case "$LOCAL_HOSTS" in
    # A LOGIN bash, so PATH matches what the ssh path gives.
    *" $HOST "*) bash -lc "$1"; return ;;
  esac
  # Force a LOGIN BASH on the far side, single-quoting the payload with sed
  # (bash 5.3 changed backslash handling in ${x//} replacements). BatchMode +
  # ConnectTimeout: converge runs unattended and must fail, not hang.
  local esc
  esc=$(printf '%s' "$1" | sed "s/'/'\\\\''/g")
  # shellcheck disable=SC2029  # remote-side expansion is intended
  ssh -o BatchMode=yes -o ConnectTimeout=10 "$HOST" "bash -lc '$esc'"
}

# rcat <spec-path> — print remote file; empty if missing; die on transport error
rcat() {
  local rc=0
  rsh "p=\"$(p_home "$1")\"; if [ -f \"\$p\" ]; then cat \"\$p\"; else exit 42; fi" || rc=$?
  case "$rc" in 0|42) : ;; *) die "failed reading $1 on $HOST (rc=$rc — ssh/transport error?)" ;; esac
}

# rwrite <spec-path> <local-content-file> — backup + write
rwrite() {
  rsh "p=\"$(p_home "$1")\"; mkdir -p \"\$(dirname \"\$p\")\"; \
       [ -f \"\$p\" ] && cp -p \"\$p\" \"\$p.bak-$STAMP\"; cat > \"\$p\"" <"$2"
}

# diff_artifact <label> <spec-path> <desired-file> — 0 identical, 1 differs (diff printed)
diff_artifact() {
  local have
  have=$(mktemp "$TMPD/have.XXXXXX")
  rcat "$2" >"$have"
  diff -u --label "$1 (current)" --label "$1 (desired)" "$have" "$3"
}

# ---------------------------------------------------------------------------
# Recycle: ONE action per unit
#
# A SIGHUP reload drains and re-execs the new binary in place with the SAME
# argv and environment, so it picks up a new binary and a new config.json —
# but never a changed unit (ExecStart flags, EnvironmentFile, PATH). Hence:
#   not active            -> start   (daemon-reload, enable --now)
#   unit changed          -> restart (daemon-reload, restart; drops turns)
#   reloading             -> pending (a SIGHUP is already draining)
#   files changed, or the running image is not the wanted one
#                         -> reload
#   otherwise             -> none
# ---------------------------------------------------------------------------
recycle_action() { # <unit_changed> <active_state> <running_ver> <want> <files_changed>
  case "$2" in active|reloading) : ;; *) echo start; return 0 ;; esac
  if [ "$1" = 1 ]; then echo restart; return 0; fi
  if [ "$2" = reloading ]; then echo pending; return 0; fi
  if [ "$5" = 1 ] || [ "$3" != "$4" ]; then echo reload; return 0; fi
  echo none
}

# probe_unit <unit> — "<active_state>|<running_version>|<self>". The running
# version is read from <proc>/<MainPID>/exe: the in-memory image, not the
# file on disk. self=1 when this very script runs inside that unit (its
# cgroup), i.e. converge was started by the relay it is about to recycle.
probe_unit() {
  rsh "u='$1'; P=\${CONVERGE_PROC:-/proc}
$(cat <<'EOS'
st=$(systemctl --user show -p ActiveState --value "$u" 2>/dev/null || true)
pid=$(systemctl --user show -p MainPID --value "$u" 2>/dev/null || true)
case "$pid" in ''|*[!0-9]*) pid=0 ;; esac
run=''
[ "$pid" -gt 0 ] && run=$("$P/$pid/exe" --version 2>/dev/null | head -1 || true)
self=0
grep -qs "/$u\$" "$P/self/cgroup" && self=1
printf '%s|%s|%s\n' "${st:-unknown}" "$run" "$self"
EOS
)" || die "cannot probe $1 on $HOST"
}

# verify_unit <unit> <want> — the unit must run the wanted image within
# VERIFY_WAIT seconds. A timeout is a failure.
verify_unit() {
  local u=$1 want=$2 i st run pr
  for i in $(seq 1 "$VERIFY_WAIT"); do
    pr=$(probe_unit "$u")
    IFS='|' read -r st run _ <<<"$pr"
    if [ "$st" = active ] && [ "$run" = "$want" ]; then
      note "$u: running $want ✓"; return 0
    fi
    [ "$i" = "$VERIFY_WAIT" ] || sleep 1
  done
  echo "converge: error: $HOST $u: state ${st:-?}, running ${run:-unreadable} after ${VERIFY_WAIT}s, wanted $want. Last journal lines:" >&2
  rsh "journalctl --user -u '$u' -n 20 --no-pager 2>&1 || true" >&2 || true
  exit 1
}

# ---------------------------------------------------------------------------
# Plan, then apply
#
# Phase 1 probes every host and records what must change; nothing is
# written. Phase 2 (--apply) performs exactly that plan.
# ---------------------------------------------------------------------------
declare -A B_SPEC B_HOST B_UNIT B_CFG B_ACT B_SELF B_FILES
declare -A K_ZA K_NEW K_Y K_NR H_FIR H_EXT H_DR
HOSTS=() BOTS=() KEYS=()
CHANGES=0

want_za="" want_fir="" want_ext=""

load_lock() {
  [ -f "$LOCK" ] || die "missing $LOCK"
  want_za=$(jq -r '.zulip_acp' "$LOCK")
  want_fir=$(jq -r '.fir' "$LOCK")
  want_ext=$(jq -r '.exts["github.com/kfet/fir-exts"]' "$LOCK")
}

fir_version() { rsh "fir --version 2>/dev/null | head -1 | awk '{print \$2}' || true"; }
ext_path() { rsh "fir packages list 2>/dev/null | awk -v s=\"$1\" 'index(\$1, s) {print \$NF}' || true"; }
ext_rev() { rsh "git -C \"$(p_home "$1")\" rev-parse HEAD 2>/dev/null || true"; }

# plan_binary <host> <binary> — once per (host, binary).
plan_binary() {
  local key="$1|$2" cur b
  [ -n "${K_ZA[$key]+x}" ] && return 0
  KEYS+=("$key")
  b=$(p_home "$2")
  cur=$(rsh "\"$b\" --version 2>/dev/null | head -1 || true")
  if [ "$cur" = "$want_za" ]; then
    K_ZA[$key]=""; note "zulip-acp $2 $cur ✓"; return 0
  fi
  if [ -z "$cur" ] || ! is_release_version "$cur" || [ "$(ver_cmp "$cur" "$SELFUPDATE_MIN")" -lt 0 ]; then
    die "$1: $2 is ${cur:-missing}, which cannot update itself (needs >= $SELFUPDATE_MIN). Bootstrap once: curl -fsSL https://raw.githubusercontent.com/$ZULIP_ACP_REPO/main/install.sh | BIN_DIR=\$HOME/.local/bin VERSION=v$want_za sh"
  fi
  # Capability probes, not version guesses. A binary that knows --binary
  # gets the single-file swap. An older one gets `update --version vX`, plus
  # -y if it knows -y (it prompts even with no tty and reads EOF as "no"),
  # plus --no-reload if it knows that (converge recycles the units itself).
  local help
  help=$(rsh "\"$b\" update -h 2>&1 || true")
  K_NEW[$key]=0; K_Y[$key]=0; K_NR[$key]=0
  grep -qE -- '-binary([[:space:]]|$)' <<<"$help" && K_NEW[$key]=1
  grep -qE -- '(^|[[:space:]])-y([[:space:]]|$)' <<<"$help" && K_Y[$key]=1
  grep -qE -- '-no-reload([[:space:]]|$)' <<<"$help" && K_NR[$key]=1
  K_ZA[$key]=$cur; CHANGES=$((CHANGES + 1))
  note "PLAN zulip-acp $2: $cur → $want_za"
}

# plan_fir <host> <spec> — fir and its extensions, once per host.
plan_fir() {
  local h=$1 cur ext p rev
  if [ -z "${H_FIR[$h]+x}" ]; then
    cur=$(fir_version)
    if [ "$cur" = "$want_fir" ]; then H_FIR[$h]=""; note "fir $cur ✓"
    else H_FIR[$h]=${cur:-missing}; CHANGES=$((CHANGES + 1)); note "PLAN fir: ${cur:-missing} → $want_fir"; fi
  fi
  for ext in $(jq -r '.fir.exts[]? // empty' "$2"); do
    [ -n "${H_EXT[$h|$ext]+x}" ] && continue
    p=$(ext_path "$ext")
    if [ -z "$p" ]; then
      H_EXT[$h|$ext]=install; CHANGES=$((CHANGES + 1)); note "PLAN ext $ext: install @ ${want_ext:0:12}"; continue
    fi
    rev=$(ext_rev "$p")
    if [ "$rev" = "$want_ext" ]; then H_EXT[$h|$ext]=""; note "ext $ext @ ${rev:0:12} ✓"
    elif [ -z "$rev" ]; then H_EXT[$h|$ext]=""; note "WARN: ext $ext at $p: rev unreadable — verify by hand"
    else H_EXT[$h|$ext]=update; CHANGES=$((CHANGES + 1)); note "PLAN ext $ext: ${rev:0:12} → ${want_ext:0:12}"; fi
  done
}

# host_changed <host> <binary> — did a host-wide component move?
host_changed() {
  local k
  [ -n "${K_ZA[$1|$2]}" ] && return 0
  [ -n "${H_FIR[$1]:-}" ] && return 0
  for k in "${!H_EXT[@]}"; do
    case "$k" in "$1|"*) [ -n "${H_EXT[$k]}" ] && return 0 ;; esac
  done
  return 1
}

plan_bot() { # <bot>
  local bot=$1 h binary cfg unit uc=0 fc=0 st run self act pr
  SPEC=${B_SPEC[$bot]}; SPEC_SRC="$BOTS_DIR/$bot.json"
  h=${B_HOST[$bot]}; HOST=$h
  binary=$(jqs '.binary'); unit="$(jqs '.unit').service"; B_UNIT[$bot]=$unit
  echo "-- $bot ($unit)"
  [ "$APPLY" = 1 ] && [ -z "$(rcat "$(jqs '.credentials.env_file')")" ] && \
    die "$h: $(jqs '.credentials.env_file') is missing or empty — create it first (see the deploy skill, 'Install config + secret'); the unit will not start without ZULIP_API_KEY. Nothing was changed."

  cfg=$(skey config_path)
  # shellcheck disable=SC2088  # spec-style path; expanded later via p_home
  [ -n "$cfg" ] || cfg="~/.config/zulip-acp/config.json"
  B_CFG[$bot]=$cfg
  render_config >"$TMPD/$bot.config"
  fill_quick_models "$TMPD/$bot.config" "$cfg"
  if diff_artifact "config.json" "$cfg" "$TMPD/$bot.config"; then note "config $cfg ✓"
  else fc=1; CHANGES=$((CHANGES + 1)); note "PLAN write $cfg"; fi

  render_unit >"$TMPD/$bot.unit"
  # shellcheck disable=SC2088
  if diff_artifact "systemd unit" "~/.config/systemd/user/$unit" "$TMPD/$bot.unit"; then note "unit $unit ✓"
  else uc=1; CHANGES=$((CHANGES + 1)); H_DR[$h]=1; note "PLAN write unit $unit"; fi

  host_changed "$h" "$binary" && fc=1
  B_FILES[$bot]="$fc$uc"
  pr=$(probe_unit "$unit")
  IFS='|' read -r st run self <<<"$pr"
  B_SELF[$bot]=$self
  act=$(recycle_action "$uc" "$st" "$run" "$want_za" "$fc")
  [ "$act" = start ] && H_DR[$h]=1
  B_ACT[$bot]=$act
  case "$act" in
    none)    note "$unit: running ${run:-?} ✓" ;;
    pending) note "$unit: a reload is already draining (running ${run:-?}); nothing to signal" ;;
    restart) CHANGES=$((CHANGES + 1))
             note "PLAN restart $unit (unit changed) — WARNING: drops in-flight turns and messages posted before the fresh event queue registers"
             if [ "$self" = 1 ]; then note "WARNING: converge runs inside $unit; --apply writes the files, then refuses this restart"; fi ;;
    *)       CHANGES=$((CHANGES + 1)); note "PLAN $act $unit (state $st, running ${run:-?})" ;;
  esac
}

apply_host() { # <host>
  local h=$1 key b cur k ext bot rc=0
  HOST=$h
  echo "== apply $h"
  for key in "${KEYS[@]}"; do
    case "$key" in "$h|"*) : ;; *) continue ;; esac
    cur=${K_ZA[$key]}; [ -n "$cur" ] || continue
    b=$(p_home "${key#*|}")
    if [ "${K_NEW[$key]}" = 1 ]; then
      note "\"$b\" update --binary \"$b\" --version v$want_za --no-reload -y"
      rsh "\"$b\" update --binary \"$b\" --version v$want_za --no-reload -y" || die "$h: update failed"
    else
      local y=""
      [ "${K_NR[$key]}" = 1 ] && y=" --no-reload"
      [ "${K_Y[$key]}" = 1 ] && y="$y -y"
      note "\"$b\" update --version v$want_za$y (no --binary yet: legacy update)"
      rsh "\"$b\" update --version v$want_za$y </dev/null" || die "$h: update failed"
    fi
    cur=$(rsh "\"$b\" --version 2>/dev/null | head -1 || true")
    [ "$cur" = "$want_za" ] || die "$h: ${key#*|} is ${cur:-unreadable} after update, wanted $want_za"
    note "zulip-acp now $cur ✓"
  done
  if [ -n "${H_FIR[$h]:-}" ]; then
    rsh "fir update" || true
    cur=$(fir_version)
    [ "$cur" = "$want_fir" ] || die "$h: fir is ${cur:-missing} after 'fir update', lock wants $want_fir — re-run --tot, or fetch v$want_fir from $FIR_DIST_REPO/releases"
    note "fir now $cur ✓"
  fi
  for k in "${!H_EXT[@]}"; do
    case "$k" in "$h|"*) : ;; *) continue ;; esac
    ext=${k#*|}
    case "${H_EXT[$k]}" in
      install) rsh "fir install \"https://$ext\"" || die "$h: fir install $ext failed" ;;
      update)  rsh "fir packages update \"$ext\"" || die "$h: fir packages update $ext failed" ;;
      *) continue ;;
    esac
    note "ext $ext: ${H_EXT[$k]} done"
  done
  for bot in "${BOTS[@]}"; do
    [ "${B_HOST[$bot]}" = "$h" ] || continue
    case "${B_FILES[$bot]}" in 1?) rwrite "${B_CFG[$bot]}" "$TMPD/$bot.config"; note "config written (backup: ${B_CFG[$bot]}.bak-$STAMP)" ;; esac
    # shellcheck disable=SC2088
    case "${B_FILES[$bot]}" in ?1) rwrite "~/.config/systemd/user/${B_UNIT[$bot]}" "$TMPD/$bot.unit"; note "unit ${B_UNIT[$bot]} written" ;; esac
  done
  if [ -n "${H_DR[$h]:-}" ]; then rsh "systemctl --user daemon-reload" || die "$h: daemon-reload failed"; fi
  for bot in "${BOTS[@]}"; do
    [ "${B_HOST[$bot]}" = "$h" ] || continue
    local u=${B_UNIT[$bot]} self=${B_SELF[$bot]}
    case "${B_ACT[$bot]}" in
      none) continue ;;
      pending) note "reload pending: $u re-execs when its current drain ends"; continue ;;
      start) rsh "systemctl --user enable --now '$u'" || die "$h: cannot start $u" ;;
      restart)
        if [ "$self" = 1 ]; then
          echo "converge: error: $h $u: the unit changed, and this converge runs inside that relay. The files are written; run \`systemctl --user restart $u\` from outside the relay." >&2
          rc=1; continue
        fi
        rsh "systemctl --user restart '$u'" || die "$h: cannot restart $u" ;;
      reload) rsh "systemctl --user reload '$u'" || die "$h: reload of $u failed" ;;
    esac
    if [ "$self" = 1 ]; then
      note "reload pending: this relay reloads when the current turn ends"; continue
    fi
    verify_unit "$u" "$want_za"
  done
  [ "$rc" = 0 ] || exit "$rc"
  return 0
}

converge() { # <bot>...
  local bot spec h
  load_lock
  for bot in "$@"; do
    spec=$(bot_spec "$bot")
    SPEC=$spec; SPEC_SRC="$BOTS_DIR/$bot.json"
    validate_spec
    [ "$(jqs '.supervisor')" = systemd-user ] || die "$bot: unsupported supervisor $(jqs '.supervisor') (only systemd-user)"
    h=$(jqs '.host')
    # The lock must be acceptable to the spec before any host is touched.
    enforce_require "$bot" "$h" zulip_acp "$want_za"
    [ "$(jqs '.agent.kind')" = fir ] && enforce_require "$bot" "$h" fir "$want_fir"
    B_SPEC[$bot]=$spec; B_HOST[$bot]=$h; BOTS+=("$bot")
    case " ${HOSTS[*]} " in *" $h "*) : ;; *) HOSTS+=("$h") ;; esac
  done
  [ "$FORCE_LOCAL" = 1 ] && [ "${#HOSTS[@]}" -gt 1 ] && die "--local names ONE host (this machine), got: ${HOSTS[*]}"

  [ "$APPLY" = 1 ] || echo "== DRY RUN — nothing is changed (use --apply)"
  for h in "${HOSTS[@]}"; do
    HOST=$h
    echo "== plan $h"
    pick_transport "$h"
    rsh true || die "cannot reach $h"
    for bot in "${BOTS[@]}"; do
      [ "${B_HOST[$bot]}" = "$h" ] || continue
      SPEC=${B_SPEC[$bot]}
      plan_binary "$h" "$(jqs '.binary')"
      [ "$(jqs '.agent.kind')" = fir ] && plan_fir "$h" "$SPEC"
      plan_bot "$bot"
    done
  done
  if [ "$CHANGES" = 0 ]; then
    echo "== converged: nothing to do"; return 0
  fi
  if [ "$APPLY" != 1 ]; then
    echo "== $CHANGES change(s) planned (dry run; re-run with --apply)"; return 0
  fi
  for h in "${HOSTS[@]}"; do apply_host "$h"; done
  echo "== converged ($CHANGES change(s) applied)"
}

# ---------------------------------------------------------------------------
# --tot: resolve latest-of-everything ONCE into dist.lock, then STOP.
# Resolution never happens on a host, and tot never converges.
# ---------------------------------------------------------------------------
# list_tags_git <repo-url> — every vX.Y.Z tag, v stripped, oldest first.
# The `v` is stripped BEFORE sorting: `sort -V` on tags that still carry it
# sorts the prefix, not the version. The grep keeps prereleases (v1.2.0-rc1)
# out entirely — they can never satisfy a constraint anyway.
list_tags_git() {
  git ls-remote --tags "$1" \
    | awk '{print $2}' | sed 's|^refs/tags/||' | grep -v '\^{}$' \
    | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' | sed 's/^v//' | sort -V
}

list_tags_zulip_acp() {
  # gh first: it carries a token, and GitHub's unauthenticated API limit is
  # per IP, so a NAT'd fleet exhausts it. Plain git ls-remote is the fallback
  # (the repo is public), and it is also what runs when gh is absent.
  local tags=""
  if command -v gh >/dev/null 2>&1; then
    # Capture before filtering: a pipeline's status is the LAST command's, so
    # `gh | sed` would report success for a logged-out gh and silently skip
    # the git fallback. Drafts and prereleases are excluded here rather than
    # by the version regex: a draft tag may not exist on the remote at all,
    # and locking one would send every host chasing an asset that is not
    # published.
    tags=$(gh release list --repo "$ZULIP_ACP_REPO" --limit 200 \
             --json tagName,isDraft,isPrerelease \
             -q '.[] | select(.isDraft == false and .isPrerelease == false) | .tagName' \
           2>/dev/null || true)
  fi
  if [ -n "$tags" ]; then
    printf '%s\n' "$tags" | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' | sed 's/^v//' | sort -V
    return 0
  fi
  list_tags_git "https://github.com/$ZULIP_ACP_REPO"
}

head_rev() { # <repo-url>
  git ls-remote "$1" HEAD | awk 'NR==1 {print $1}'
}

# resolve_component <component> <label> <versions...> — the NEWEST version
# satisfying EVERY spec's constraint, with a diagnosis when nothing does.
# This is where "latest" became "latest ACCEPTABLE": the fleet's specs, not
# the registry, decide what the lock may say.
resolve_component() {
  local comp=$1 label=$2 picked newest reqs
  shift 2
  [ $# -gt 0 ] || die "could not list any $label release (rate-limited? is gh logged in?)"
  newest=$(printf '%s\n' "$@" | tail -1)
  picked=$(resolve_satisfying "$comp" "$@")
  if [ -z "$picked" ]; then
    reqs=$(all_requires "$comp" | sed 's/^/     /')
    die "no $label release satisfies every spec's require.$comp (newest available: $newest). Constraints:
$reqs"
  fi
  [ "$picked" = "$newest" ] || note "$label: held at $picked by a spec constraint (newest release is $newest)" >&2
  printf '%s\n' "$picked"
}

tot() {
  need git
  local old_za old_fir old_ext new_za new_fir new_ext moved=0 f
  if [ -f "$LOCK" ]; then
    old_za=$(jq -r '.zulip_acp' "$LOCK")
    old_fir=$(jq -r '.fir' "$LOCK")
    old_ext=$(jq -r '.exts["github.com/kfet/fir-exts"]' "$LOCK")
  else
    old_za=none; old_fir=none; old_ext=none
  fi

  # Every spec's require block is validated BEFORE any network call: a lock
  # resolved against a malformed constraint would be resolved against nothing.
  for f in $(bot_specs); do
    [ -f "$f" ] || continue
    SPEC=$(merged_spec "$f"); SPEC_SRC=$f; validate_require
  done
  SPEC=""

  echo "== tot: resolving the newest releases that satisfy every spec's require"
  # shellcheck disable=SC2046  # word splitting of the version list is intended
  new_za=$(resolve_component zulip_acp zulip-acp $(list_tags_zulip_acp))
  # shellcheck disable=SC2046
  new_fir=$(resolve_component fir fir $(list_tags_git "$FIR_DIST_REPO"))
  # fir-exts is pinned by git rev, not by semver: there is no version to
  # constrain, so `require` has no key for it.
  new_ext=$(head_rev "https://github.com/kfet/fir-exts")
  [ -n "$new_za" ]  || die "could not resolve latest zulip-acp release (rate-limited? is gh logged in?)"
  [ -n "$new_fir" ] || die "could not resolve latest fir-dist tag"
  [ -n "$new_ext" ] || die "could not resolve fir-exts HEAD"

  [ "$old_za" != "$new_za" ]   && { note "zulip-acp: $old_za → $new_za"; moved=1; }
  [ "$old_fir" != "$new_fir" ] && { note "fir:       $old_fir → $new_fir"; moved=1; }
  [ "$old_ext" != "$new_ext" ] && { note "fir-exts:  ${old_ext:0:12} → ${new_ext:0:12}"; moved=1; }
  [ "$moved" = 0 ] && { echo "== tot: lock already at latest, nothing moved"; return 0; }

  jq -n --arg za "$new_za" --arg fir "$new_fir" --arg ext "$new_ext" \
        --arg at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
        '{zulip_acp: $za, fir: $fir, exts: {"github.com/kfet/fir-exts": $ext}, resolved_at: $at}' \
    >"$LOCK"
  echo "== tot: dist.lock rewritten. Review the diff, commit it, then converge the fleet:"
  echo "   git diff dist.lock"
  echo "   scripts/converge.sh --all --apply"
}

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------
usage() {
  sed -n '/^# Usage:/,/^#$/p' "$0" | sed 's/^# \{0,1\}//' >&2
  exit 1
}

# all_managed — every bot of this relay that converge owns (not managed=false).
all_managed() {
  local f
  for f in $(bot_specs); do
    [ "$(jq -r '.managed // true' "$f")" = false ] || basename "$f" .json
  done
}

[ $# -ge 1 ] || usage
APPLY=0
case "$1" in
  --tot)
    [ $# -eq 1 ] || die "--tot takes no other arguments (tot never converges)"
    tot ;;
  render)
    [ $# -eq 3 ] || usage
    SPEC=$(bot_spec "$2"); SPEC_SRC="$BOTS_DIR/$2.json"
    validate_spec
    case "$3" in
      config)    render_config ;;
      execstart) render_execstart ;;
      unit)      render_unit ;;
      *)         usage ;;
    esac ;;
  recycle-action)
    [ $# -eq 6 ] || usage
    recycle_action "$2" "$3" "$4" "$5" "$6" ;;
  semver-cmp)
    [ $# -eq 3 ] || usage
    ver_cmp "$2" "$3" ;;
  semver-sat)
    [ $# -eq 3 ] || usage
    if ver_satisfies "$2" "$3"; then echo yes; else echo no; exit 1; fi ;;
  self-host)
    [ $# -eq 2 ] || usage
    if host_is_self "$2"; then echo "self: $SELF_WHY"; else echo "remote"; exit 1; fi ;;
  resolve)
    [ $# -ge 2 ] || usage
    COMP=$2; shift 2
    resolve_component "$COMP" "$COMP" "$@" ;;
  *)
    TARGETS=()
    while [ $# -gt 0 ]; do
      case "$1" in
        --apply) APPLY=1 ;;
        --dry-run) APPLY=0 ;;
        --local) FORCE_LOCAL=1 ;;
        --all) mapfile -t -O "${#TARGETS[@]}" TARGETS < <(all_managed) ;;
        -*) usage ;;
        *) TARGETS+=("$1") ;;
      esac
      shift
    done
    [ "${#TARGETS[@]}" -gt 0 ] || usage
    converge "${TARGETS[@]}" ;;
esac
