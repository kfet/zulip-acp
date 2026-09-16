#!/usr/bin/env bash
# converge.sh — the only sanctioned way to change a zulip-acp bot host.
#
# Reads bots/<bot>.json + dist.lock and makes the target host match. Version
# moves go through the lock, never through a hand-typed command on a host.
#
# Usage:
#   converge.sh <bot>                          dry-run (default): show what would change
#   converge.sh <bot> --apply                  actually converge the host
#   converge.sh <bot> --local                  act on THIS machine directly (no ssh);
#                                              refuses unless the spec host is us
#   converge.sh <bot> --target-root DIR        act on a local fake host rooted at DIR
#                                              (no ssh; for testing). DIR/proc stands in
#                                              for /proc so a test can model a running image
#   converge.sh --tot                          resolve the newest release that
#                                              satisfies EVERY spec's `require`
#                                              block and rewrite dist.lock;
#                                              NEVER converges
#   converge.sh render <bot> <artefact>        render one artefact to stdout
#                                              artefact: config | execstart | unit
#   converge.sh plan-recycle <unit_changed> <running> <version> <has_reload>
#                                              print the recycle mechanism that state
#                                              selects (test hook)
#   converge.sh plan-stale <running> <want> <running_ver>
#                                              print whether the RUNNING image is stale
#                                              w.r.t. want (test hook)
#   converge.sh semver-cmp <a> <b>             print -1|0|1 (test hook)
#   converge.sh semver-sat <version> <constraint>
#                                              print yes/no, exit 0/1 (test hook)
#   converge.sh resolve <component> <ver...>   print the newest given version that
#                                              satisfies every spec's require for
#                                              <component> (test hook)
#
# Spec vs lock: a bot spec declares REQUIREMENTS (`require`), dist.lock records
# the RESOLUTION. --tot is the only resolver; --apply is a reader that refuses
# to move a host to a locked version its own spec forbids.
#
# Conventions:
#   - a spec.service key that is absent (or false/null) emits no flag at all;
#     present-and-true booleans emit a bare flag
#   - spec paths use ~/...; rendered as %h/... in systemd units and
#     "$HOME"/... over the wire
#   - rendering is canonical: flag order and dash style are normalised
#     (--flag, fixed order). The first --apply on a host may rewrite its unit
#     into canonical form with identical semantics.
#
# Only systemd user services are supported. zulip-acp dials out — there is no
# listener, no funnel and no port — so a host is just a binary, a config, a
# secret and a unit. launchd is deliberately absent: no macOS zulip host
# exists, and the graceful reload cannot be verified without /proc (see
# recycle_plan). Add it when there is a host to test it against.
set -euo pipefail

REPO_ROOT=$(cd "$(dirname "$0")/.." && pwd)
BOTS_DIR="$REPO_ROOT/bots"
LOCK="$REPO_ROOT/dist.lock"
STAMP=$(date +%Y%m%d-%H%M%S)
TMPD=$(mktemp -d)
trap 'rm -rf "$TMPD"' EXIT

ZULIP_ACP_REPO=kfet/zulip-acp
FIR_DIST_REPO="https://github.com/kfet/fir-dist"

# The release that first shipped `zulip-acp update`. At or above it the host
# updates ITSELF (checksum-verified, ETXTBSY-safe atomic swap); below it there
# is no self-update to call and converge fetches the asset itself.
SELFUPDATE_MIN=0.19.0
# The release that first shipped the SIGHUP graceful reload. Below it a SIGHUP
# just kills the relay, so the first cutover must be a hard restart.
GRACEFUL_MIN=0.12.0
# How long (seconds) to wait for a reload's re-exec after the SIGHUP is
# accepted. The relay drains in-flight turns first (-reload-drain-deadline,
# 30m), so a timeout here is "still draining", not a failure. Overridable
# from the environment for a host that is known to be mid-turn.
RELOAD_WAIT=${RELOAD_WAIT:-60}
case "$RELOAD_WAIT" in ''|*[!0-9]*) echo "converge: error: RELOAD_WAIT must be a whole number of seconds" >&2; exit 1 ;; esac

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
  for f in "$BOTS_DIR"/*.json; do
    [ -f "$f" ] || continue
    bot=$(basename "$f" .json)
    c=$(spec_require "$f" "$1")
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
    *) die "$SPEC: .require must be an object like {\"zulip_acp\": \">=0.31.3\"} (got $t)" ;;
  esac
  # while-read, not `for k in $(...)`: a key that word-splits to nothing (the
  # empty key) would otherwise skip the loop body entirely and validate.
  while IFS= read -r k; do
    case " $REQUIRE_COMPONENTS " in
      *" $k "*) : ;;
      *) die "$SPEC: unknown .require key \"$k\" (known: $REQUIRE_COMPONENTS)" ;;
    esac
    c=$(jq -r --arg k "$k" '.require[$k]' "$SPEC")
    [ "$(jq -r --arg k "$k" '.require[$k] | type' "$SPEC")" = string ] \
      || die "$SPEC: .require.$k must be a string constraint (e.g. \">=0.31.3\")"
    valid_constraint "$c" \
      || die "$SPEC: invalid .require.$k constraint \"$c\" — expected whitespace-separated terms of >=X.Y.Z, <=X.Y.Z, >X.Y.Z, <X.Y.Z, ~>X.Y.Z or an exact X.Y.Z"
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
  die "$host ($bot): dist.lock has $comp $locked, which does not satisfy bots/$bot.json require.$comp \"$c\". The lock is a RESOLUTION of the specs, not an override: re-resolve with \`scripts/converge.sh --tot\` (or fix the lock by hand and commit it). Nothing was changed on $host."
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

# ---------------------------------------------------------------------------
# Remote execution (ssh by host alias, or a local fake root for testing)
# ---------------------------------------------------------------------------
HOST=""
TARGET_ROOT=""
LOCAL=0
FORCE_LOCAL=0
PROC_ROOT=/proc

rsh() { # run a shell command on the target; stdin is forwarded
  if [ "$LOCAL" = 1 ]; then
    # We ARE the target. Run through a LOGIN bash so PATH matches what the ssh
    # path would produce — otherwise a perfectly installed `fir` in
    # ~/.local/bin probes as "not found". Unlike --target-root this does NOT
    # rewrite HOME: local mode is a transport swap, not a test harness.
    bash -lc "$1"
  elif [ -n "$TARGET_ROOT" ]; then
    # A real target is reached through a LOGIN shell, so ~/.local/bin is on
    # PATH. Mirror that for the fake root — it is also how a test supplies a
    # stub `systemctl` for the recycle path.
    HOME="$TARGET_ROOT" PATH="$TARGET_ROOT/.local/bin:$PATH" bash -c "$1"
  else
    # Force a LOGIN BASH on the far side: `ssh host cmd` otherwise runs the
    # account's default shell, non-login, so ~/.local/bin is off PATH and the
    # bash-written payload may be handed to zsh. Single-quote the payload with
    # sed (bash 5.3 changed backslash handling in ${x//} replacements, which
    # double-escapes and dies with "unmatched '" on the far side).
    local esc
    esc=$(printf '%s' "$1" | sed "s/'/'\\\\''/g")
    # BatchMode + ConnectTimeout: converge runs unattended (often from an
    # agent turn), and an ssh that stops to ask for a passphrase or an
    # unknown host key would hang forever instead of failing.
    # shellcheck disable=SC2029  # remote-side expansion is intended
    ssh -o BatchMode=yes -o ConnectTimeout=10 "$HOST" "bash -lc '$esc'"
  fi
}

# rcat <spec-path> — print remote file; empty if missing; die on transport error
rcat() {
  local rc=0
  rsh "p=\"$(p_home "$1")\"; if [ -f \"\$p\" ]; then cat \"\$p\"; else exit 42; fi" || rc=$?
  case "$rc" in 0|42) : ;; *) die "failed reading $1 (rc=$rc — ssh/transport error?)" ;; esac
}

# rwrite <spec-path> <local-content-file> — backup + write
rwrite() {
  rsh "p=\"$(p_home "$1")\"; mkdir -p \"\$(dirname \"\$p\")\"; \
       [ -f \"\$p\" ] && cp -p \"\$p\" \"\$p.bak-$STAMP\"; cat > \"\$p\"" <"$2"
}

# diff_artifact <label> <spec-path> <desired-content-file>
# Prints a diff if different. 0 if identical, 1 if different.
diff_artifact() {
  local label=$1 rpath=$2 want=$3 have
  have=$(mktemp "$TMPD/have.XXXXXX")
  rcat "$rpath" >"$have"
  if diff -u --label "$label (current)" --label "$label (desired)" "$have" "$want"; then
    rm -f "$have"; return 0
  fi
  rm -f "$have"; return 1
}

# ---------------------------------------------------------------------------
# Recycle mechanism selection
#
# zulip-acp has no master/worker supervisor and needs none: it is a long-poll
# CLIENT, and while it is not polling the Zulip event queue buffers
# server-side (docs/graceful-reload.md). A SIGHUP therefore makes the ONE
# tracked process stop polling without deleting its queue, drain in-flight
# turns, and re-exec the new binary IN PLACE — same PID, same queue cursor.
#
#   unit changed                 -> hard (systemd needs daemon-reload+restart)
#   service not running          -> hard (nothing to signal)
#   unit has no ExecReload       -> hard (reload would fail)
#   running image < GRACEFUL_MIN -> hard (no SIGHUP handler; a HUP would kill it)
#   running version unreadable   -> hard (cannot prove the live image can reload)
#   otherwise                    -> graceful reload (SIGHUP)
#
# The decision is made on the RUNNING image, never the on-disk binary: what
# matters is what the live process can do.
# ---------------------------------------------------------------------------

# Basename of the tracked binary (spec `.binary`), used to be sure a pid is
# really running our image before executing it. Set per-bot in converge().
ZA_EXE=zulip-acp

ver_ge() { # <a> <b> — true if version a >= b
  [ -n "$1" ] || return 1
  [ "$(printf '%s\n%s\n' "$2" "$1" | sort -V | head -1)" = "$2" ]
}

# exe_name_snippet — remote shell defining `exe_name <pid>`: the BASENAME of
# the image that pid is EXECUTING, empty if unknowable. $proc/<pid>/exe is the
# in-memory inode, still correct after the on-disk file was replaced or
# unlinked (hence stripping a " (deleted)" suffix). A binary moved aside as
# `zulip-acp.running-<ver>` still counts as ours, so match the name exactly or
# followed by a dot.
exe_name_snippet() {
  cat <<'EOS'
exe_name() {
  _e=$(readlink "$proc/$1/exe" 2>/dev/null || true)
  _e=${_e% (deleted)}
  printf '%s\n' "${_e##*/}"
}
is_ours() {
  case "$1" in "$zaname"|"$zaname".*) return 0 ;; esac
  return 1
}
EOS
}

# running_version_snippet — sets `ver` to the version of the image pid is
# EXECUTING (Linux only: it execs $proc/<pid>/exe, never the on-disk file).
running_version_snippet() {
  cat <<'EOS'
ver=''
if is_ours "$(exe_name "$pid")"; then
  ver=$("$proc/$pid/exe" --version 2>/dev/null | head -1 || true)
fi
EOS
}

# probe_state <unit> — prints "<running>|<pid>|<version>|<has_reload>"
#   running     1 if the service is active with a pid
#   pid         MainPID, 0 if unknown
#   version     version of the RUNNING image; empty if unreadable
#   has_reload  how many ExecReload lines the loaded unit defines (0 = none;
#               >1 means a drop-in duplicates it)
probe_state() {
  local unit=$1 script
  # A fake root has no supervisor unless the test stubs one; never probe the
  # REAL local systemd from --target-root mode.
  if [ -n "$TARGET_ROOT" ] && [ ! -x "$TARGET_ROOT/.local/bin/systemctl" ]; then
    echo '0|0||0'; return 0
  fi
  script="u='$unit'; zaname='$ZA_EXE'; proc='$PROC_ROOT'
$(exe_name_snippet)
$(cat <<'EOS'
pid=$(systemctl --user show -p MainPID --value "$u" 2>/dev/null || echo 0)
case "$pid" in ''|*[!0-9]*) pid=0 ;; esac
# `reloading` is what systemd reports while ExecReload runs — a state the
# graceful path passes through by definition, not a stopped service.
act=$(systemctl --user is-active "$u" 2>/dev/null || true)
rel=$(systemctl --user show -p ExecReload --value "$u" 2>/dev/null || true)
run=0
case "$act" in active|reloading) [ "$pid" -gt 0 ] && run=1 ;; esac
ver=''
EOS
)
if [ \"\$run\" = 1 ]; then
$(running_version_snippet)
$(cat <<'EOS'
fi
hr=0; [ -n "$rel" ] && hr=$(printf '%s\n' "$rel" | grep -c '[^[:space:]]')
printf '%s|%s|%s|%s\n' "$run" "$pid" "$ver" "$hr"
EOS
)"
  rsh "$script"
}

# stale_decision <running> <want> <running_ver>
# Answers "is this host still SERVING an old image?" from the running state
# alone, so a host whose FILES already match is not called converged while the
# live process executes the previous release (the normal state after a binary
# swap that was never recycled). Prints "stale|<why>" or "current|<why>".
# A version we could not read is never evidence of staleness: silence must not
# manufacture a restart. Exposed as `plan-stale` for tests.
stale_decision() {
  local running=$1 want=$2 ver=$3
  if [ "$running" != 1 ]; then echo "current|not running"; return 0; fi
  if [ -z "$ver" ]; then echo "current|running version unreadable"; return 0; fi
  if [ "$ver" = "$want" ]; then echo "current|running image is $want"; return 0; fi
  echo "stale|running image is $ver, wanted $want"
}

# recycle_plan <unit_changed> <running> <version> <has_reload>
# Prints "<graceful|hard>|<reason>". Pure: no I/O, unit-testable via
# `plan-recycle`.
recycle_plan() {
  local unit_changed=$1 running=$2 version=$3 has_reload=$4
  if [ "$unit_changed" = 1 ]; then echo "hard|unit changed"; return 0; fi
  if [ "$running" != 1 ]; then echo "hard|service not running"; return 0; fi
  if [ "${has_reload:-0}" -lt 1 ]; then echo "hard|unit defines no ExecReload"; return 0; fi
  if [ -z "$version" ]; then
    echo "hard|running version unreadable — cannot prove the live image handles SIGHUP"; return 0
  fi
  ver_ge "$version" "$GRACEFUL_MIN" \
    || { echo "hard|running image $version < $GRACEFUL_MIN (no SIGHUP handler)"; return 0; }
  echo "graceful|running image $version >= $GRACEFUL_MIN"
}

mech_label() { # <mech>
  case "$1" in
    graceful) echo "graceful reload (SIGHUP: drain, then re-exec in place)" ;;
    hard)     echo "hard restart (daemon-reload + restart)" ;;
  esac
}

# ---------------------------------------------------------------------------
# Binary installation
#
# The self-update subcommand is the canonical mechanism and the host runs it
# itself: it resolves the release over the GitHub API (which works on a
# PRIVATE repo with a token, where the plain download URL and `brew install`
# both 404), verifies the sha256, and swaps the file atomically under the
# running process. converge only falls back to fetching the asset when the
# installed binary is too old to have `update` at all.
# ---------------------------------------------------------------------------
install_binary() { # <binary-spec-path> <current-version> <want-version> <platform>
  local binary=$1 cur=$2 want=$3 platform=$4 os arch b
  b=$(p_home "$binary")
  if ver_ge "$cur" "$SELFUPDATE_MIN"; then
    note "self-update: $b update --version v$want"
    rsh "\"$b\" update --version v$want" || die "self-update failed on $HOST"
    return 0
  fi
  os=${platform%%/*}; arch=${platform##*/}
  if rsh "command -v gh >/dev/null 2>&1"; then
    note "fetching zulip-acp-$os-$arch v$want with gh (no self-update in $cur)"
    rsh "set -e; b=\"$b\"; t=\$(mktemp); \
         gh release download v$want --repo $ZULIP_ACP_REPO \
            --pattern 'zulip-acp-$os-$arch' --output \"\$t\" --clobber; \
         chmod +x \"\$t\"; [ -f \"\$b\" ] && cp -p \"\$b\" \"\$b.bak-$STAMP\"; mv \"\$t\" \"\$b\""
    return 0
  fi
  die "no way to install v$want on $HOST: the installed binary (${cur:-missing}) predates \`zulip-acp update\` (>= $SELFUPDATE_MIN) and there is no \`gh\` on the host. Install gh, or bootstrap once with install.sh (\`curl -fsSL https://raw.githubusercontent.com/$ZULIP_ACP_REPO/main/install.sh | BIN_DIR=\$HOME/.local/bin VERSION=v$want sh\`) and let it self-update from then on."
}

# ---------------------------------------------------------------------------
# Converge
# ---------------------------------------------------------------------------
converge() {
  local bot=$1 apply=$2 changes=0 unit_changed=0
  local want_za want_fir want_ext cur host supervisor binary

  [ -f "$LOCK" ] || die "missing $LOCK"
  want_za=$(jq -r '.zulip_acp' "$LOCK")
  want_fir=$(jq -r '.fir' "$LOCK")
  want_ext=$(jq -r '.exts["github.com/kfet/fir-exts"]' "$LOCK")

  host=$(jqs '.host')
  supervisor=$(jqs '.supervisor')
  binary=$(jqs '.binary')
  ZA_EXE=${binary##*/}
  HOST="$host"

  [ "$supervisor" = systemd-user ] \
    || die "unsupported supervisor: $supervisor (only systemd-user; see the header)"

  echo "== converge $bot (host=$host supervisor=$supervisor)$([ -n "$TARGET_ROOT" ] && echo " [fake root: $TARGET_ROOT]")$([ "$LOCAL" = 1 ] && echo " [local]")"
  [ "$apply" = 1 ] || echo "== DRY RUN — no changes will be made (use --apply)"

  # The lock must be ACCEPTABLE to this spec before anything is copied. This
  # is deterministic and offline: a hand-edited or stale lock that would
  # silently downgrade the host stops here, in dry run as well as --apply.
  enforce_require "$bot" "$host" zulip_acp "$want_za"
  if [ "$(jqs '.agent.kind')" = "fir" ]; then
    enforce_require "$bot" "$host" fir "$want_fir"
  fi

  [ "$LOCAL" = 1 ] && [ -n "$TARGET_ROOT" ] && die "--local and --target-root are mutually exclusive"

  # --local is a loaded gun: it points every write at THIS machine while the
  # spec still names some other host. Refuse unless the spec's host really is
  # us. Best-effort by design — an unresolvable name warns rather than blocks.
  if [ "$LOCAL" = 1 ] && [ "$FORCE_LOCAL" != 1 ]; then
    local _tsips _hostip _realhost
    _tsips=$( (tailscale ip 2>/dev/null || true) | tr '\n' ' ')
    # The spec's host is an ssh_config ALIAS, so resolve it the way ssh would
    # before touching DNS — otherwise a valid alias looks unresolvable.
    _realhost=$(ssh -G "$host" 2>/dev/null | awk '/^hostname /{print $2; exit}' || true)
    [ -n "$_realhost" ] || _realhost="$host"
    _hostip=$(getent hosts "$_realhost" 2>/dev/null | awk '{print $1; exit}' || true)
    if [ -z "$_hostip" ] || [ -z "$_tsips" ]; then
      die "--local refused: cannot confirm spec host '$host' is this machine (resolved='${_hostip:-?}' local='${_tsips:-?}'). Re-run with --force-local if you are certain."
    elif ! printf '%s' "$_tsips" | grep -qw -- "$_hostip"; then
      die "--local refused: spec host '$host' ($_realhost) resolves to $_hostip, not this machine ($_tsips)"
    else
      echo "== --local: confirmed '$host' -> $_realhost ($_hostip) is this machine"
    fi
  fi

  # Preflight: fail loudly on an unreachable target rather than mistaking
  # transport failure for missing files.
  rsh true || die "cannot reach target ($host)"

  # A unit whose EnvironmentFile is missing starts and dies with nothing in
  # the journal that names the cause. Check before writing anything, so a
  # host that was never provisioned aborts untouched.
  if [ "$apply" = 1 ] && [ -z "$(rcat "$(jqs '.credentials.env_file')")" ]; then
    die "$(jqs '.credentials.env_file') is missing or empty on $host — create it first (see the deploy skill, 'Install config + secret'); the unit will not start without ZULIP_API_KEY"
  fi

  # -- 1. zulip-acp version -------------------------------------------------
  cur=$(rsh "\"$(p_home "$binary")\" --version 2>/dev/null || true")
  if [ "$cur" = "$want_za" ]; then
    note "zulip-acp $cur ✓"
  else
    changes=$((changes + 1))
    note "zulip-acp: ${cur:-missing} → $want_za"
    if [ "$apply" = 1 ]; then
      if [ -n "$TARGET_ROOT" ]; then
        note "skipping zulip-acp install (fake root)"
      else
        install_binary "$binary" "$cur" "$want_za" "$(jqs '.platform')"
        cur=$(rsh "\"$(p_home "$binary")\" --version")
        [ "$cur" = "$want_za" ] || die "zulip-acp still $cur after install (wanted $want_za)"
        note "zulip-acp now $cur ✓"
      fi
    fi
  fi

  # -- 2. fir version -------------------------------------------------------
  if [ "$(jqs '.agent.kind')" = "fir" ]; then
    cur=$(rsh "fir --version 2>/dev/null | head -1 | awk '{print \$2}' || true")
    if [ "$cur" = "$want_fir" ]; then
      note "fir $cur ✓"
    else
      changes=$((changes + 1))
      note "fir: ${cur:-missing} → $want_fir"
      if [ "$apply" = 1 ]; then
        if [ -n "$TARGET_ROOT" ]; then
          note "skipping fir update (fake root)"
        else
          rsh "fir update" || true
          cur=$(rsh "fir --version 2>/dev/null | head -1 | awk '{print \$2}' || true")
          [ "$cur" = "$want_fir" ] || die "fir is $cur after 'fir update', lock wants $want_fir — releases moved past the lock? Re-run --tot, or fetch v$want_fir from $FIR_DIST_REPO/releases manually"
          note "fir now $cur ✓"
        fi
      fi
    fi

    # -- 3. fir extensions at the locked rev --------------------------------
    local ext rev pkg_path
    for ext in $(jqs '.fir.exts[]? // empty'); do
      # SOURCE column may be the bare slug or the full install URL — substring.
      pkg_path=$(rsh "fir packages list 2>/dev/null | awk -v s=\"$ext\" 'index(\$1, s) {print \$NF}' || true")
      if [ -z "$pkg_path" ]; then
        changes=$((changes + 1))
        note "ext $ext: not installed → install @ ${want_ext:0:12}"
        if [ "$apply" = 1 ] && [ -z "$TARGET_ROOT" ]; then
          rsh "fir install \"https://$ext\""
          pkg_path=$(rsh "fir packages list 2>/dev/null | awk -v s=\"$ext\" 'index(\$1, s) {print \$NF}' || true")
        fi
      fi
      if [ -n "$pkg_path" ]; then
        rev=$(rsh "git -C \"$(p_home "$pkg_path")\" rev-parse HEAD 2>/dev/null || true")
        if [ "$rev" = "$want_ext" ]; then
          note "ext $ext @ ${rev:0:12} ✓"
        elif [ -z "$rev" ]; then
          note "WARN: ext $ext installed at $pkg_path but rev unreadable — verify manually"
        else
          changes=$((changes + 1))
          note "ext $ext: ${rev:0:12} → ${want_ext:0:12}"
          if [ "$apply" = 1 ] && [ -z "$TARGET_ROOT" ]; then
            rsh "fir packages update \"$ext\""
            rev=$(rsh "git -C \"$(p_home "$pkg_path")\" rev-parse HEAD 2>/dev/null || true")
            if [ "$rev" != "$want_ext" ]; then
              note "WARN: ext $ext is at ${rev:0:12} after update, lock wants ${want_ext:0:12} (fir cannot pin an arbitrary rev; re-run --tot if upstream moved)"
            fi
          fi
        fi
      fi
    done
  fi

  # -- 4. config.json -------------------------------------------------------
  local cfg_path want_file
  cfg_path=$(skey config_path)
  # shellcheck disable=SC2088  # spec-style path; expanded later via p_home
  [ -n "$cfg_path" ] || cfg_path="~/.config/zulip-acp/config.json"
  want_file=$(mktemp "$TMPD/want.XXXXXX")
  render_config >"$want_file"
  if diff_artifact "config.json" "$cfg_path" "$want_file"; then
    note "config $cfg_path ✓"
  else
    changes=$((changes + 1))
    if [ "$apply" = 1 ]; then
      rwrite "$cfg_path" "$want_file"
      note "config written (backup: $cfg_path.bak-$STAMP)"
    fi
  fi
  rm -f "$want_file"

  # -- 5. unit --------------------------------------------------------------
  local unit_path unit
  unit="$(jqs '.unit').service"
  # shellcheck disable=SC2088  # spec-style path; expanded later via p_home
  unit_path="~/.config/systemd/user/$unit"
  want_file=$(mktemp "$TMPD/want.XXXXXX"); render_unit >"$want_file"
  if diff_artifact "systemd unit" "$unit_path" "$want_file"; then
    note "unit $unit_path ✓"
  else
    changes=$((changes + 1)); unit_changed=1
    if [ "$apply" = 1 ]; then
      rwrite "$unit_path" "$want_file"
      note "unit written (backup: $unit_path.bak-$STAMP)"
    fi
  fi
  rm -f "$want_file"

  # -- 6. recycle + verify --------------------------------------------------
  # Probe what is RUNNING (not what is on disk) BEFORE concluding there is
  # nothing to do: a binary swapped on disk without a recycle leaves the files
  # converged while the live process still executes the old image.
  local state run_before pid_before ver_before has_reload plan mech reason stale
  state=$(probe_state "$unit")
  IFS='|' read -r run_before pid_before ver_before has_reload <<<"$state"

  if [ "${has_reload:-0}" -gt 1 ]; then
    note "WARNING: the loaded unit defines $has_reload ExecReload lines (a drop-in repeats it) — one reload fires $has_reload SIGHUPs"
  fi
  stale=$(stale_decision "$run_before" "$want_za" "$ver_before")
  if [ "${stale%%|*}" = stale ]; then
    note "running: ${stale#*|} — recycle needed despite matching files"
  fi

  if [ "$changes" = 0 ] && [ "${stale%%|*}" != stale ]; then
    echo "== $bot: already converged, nothing to do${ver_before:+ (running $ver_before)}"
    return 0
  fi

  plan=$(recycle_plan "$unit_changed" "$run_before" "$ver_before" "$has_reload")
  mech=${plan%%|*}; reason=${plan#*|}
  note "recycle: $(mech_label "$mech") — $reason"
  if [ "$mech" = hard ]; then
    note "WARNING: a hard restart drops every in-flight turn AND every message posted before the fresh event queue registers. An agent hosted by this relay does not survive it — run this from outside the relay."
  fi

  if [ "$apply" != 1 ]; then
    echo "== $bot: $changes change(s) pending, would $(mech_label "$mech") (dry run; re-run with --apply)"
    return 0
  fi
  # A fake root has no real service. Recycle/verify only runs there when the
  # test supplies a systemctl STUB — never against the host's real systemd.
  if [ -n "$TARGET_ROOT" ] && [ ! -x "$TARGET_ROOT/.local/bin/systemctl" ]; then
    echo "== $bot: applied to fake root; no systemctl stub, skipping recycle/verify"
    return 0
  fi

  local i st run pid ver hr
  if [ "$mech" = graceful ]; then
    rsh "systemctl --user reload $unit"
    # The re-exec happens AFTER the drain, so poll for the running image to
    # become the wanted one. The PID must never move: if it does, something
    # restarted the service and in-flight turns were dropped.
    #
    # Only a VERSION move is observable. When the running image is already the
    # wanted one — a config-only change — there is nothing to poll for, so the
    # accepted reload is the whole report.
    local swapped=0 config_only=0
    if [ "$ver_before" = "$want_za" ]; then
      swapped=1; config_only=1; pid=$pid_before; ver=$ver_before
    fi
    for i in $(seq 1 "$RELOAD_WAIT"); do
      [ "$swapped" = 1 ] && break
      st=$(probe_state "$unit")
      IFS='|' read -r run pid ver hr <<<"$st"
      [ "$run" = 1 ] || die "$unit stopped during a graceful reload"
      [ "$pid" = "$pid_before" ] \
        || die "$unit: pid moved $pid_before → $pid during a graceful reload — that was a restart, and messages in the window were dropped"
      if [ "$ver" = "$want_za" ]; then swapped=1; break; fi
      sleep 1
    done
    if [ "$config_only" = 1 ]; then
      note "$unit: reload accepted (pid $pid_before, running $ver) — nothing observable to verify: the image is already $want_za, so the change takes effect when the drain completes"
    elif [ "$swapped" = 1 ]; then
      note "$unit: graceful reload ✓ (pid $pid_before held, running ${ver:-unreadable})"
    else
      # Not a failure: the relay waits for every in-flight turn to finish
      # posting (up to -reload-drain-deadline, 30m) before it re-execs.
      note "$unit: reload accepted; still draining an in-flight turn after ${RELOAD_WAIT}s — the re-exec happens when it finishes (pid $pid_before, running $ver)"
      echo "== $bot: converged ($changes change(s) applied; re-exec pending drain)"
      return 0
    fi
  else
    if [ "$unit_changed" = 1 ]; then
      rsh "systemctl --user daemon-reload && systemctl --user restart $unit"
    else
      rsh "systemctl --user restart $unit"
    fi
    # Services need a moment after a restart.
    local up=0
    for i in 1 2 3 4 5; do
      rsh "systemctl --user is-active $unit" >/dev/null 2>&1 && { up=1; break; }
      sleep 1
    done
    [ "$up" = 1 ] || die "$unit not active after restart"
    st=$(probe_state "$unit")
    IFS='|' read -r run pid ver hr <<<"$st"
    [ "$run" = 1 ] || die "$unit not running after hard restart"
    if [ "$run_before" = 1 ] && [ "$pid" = "$pid_before" ]; then
      die "$unit: pid $pid_before did not move across a hard restart"
    fi
    if [ -n "$ver" ] && [ "$ver" != "$want_za" ]; then
      die "$unit: restarted service runs $ver, wanted $want_za"
    fi
    note "$unit: $(mech_label hard) ✓ ($reason; pid ${pid_before} → ${pid})"
  fi
  cur=$(rsh "\"$(p_home "$binary")\" --version")
  [ "$cur" = "$want_za" ] || die "post-recycle version check failed: $cur != $want_za"
  echo "== $bot: converged ($changes change(s) applied)"
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
  for f in "$BOTS_DIR"/*.json; do
    [ -f "$f" ] || continue
    SPEC="$f"; validate_require
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
  echo "== tot: dist.lock rewritten. Review the diff, commit it, then converge each bot:"
  echo "   git diff dist.lock"
  local f
  for f in "$BOTS_DIR"/*.json; do
    echo "   scripts/converge.sh $(basename "$f" .json) --apply"
  done
}

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------
usage() {
  cat >&2 <<'EOF'
usage:
  converge.sh <bot>                     dry-run (default): show what would change
  converge.sh <bot> --apply             actually converge the host
  converge.sh <bot> --dry-run           the default; stated explicitly
  converge.sh <bot> --local             act on THIS machine directly (no ssh)
  converge.sh <bot> --force-local       --local without the "is this host us" check
  converge.sh <bot> --target-root DIR   act on a local fake host rooted at DIR (no ssh)
  converge.sh --tot                     resolve the newest releases satisfying every
                                        spec's require and rewrite dist.lock;
                                        NEVER converges
  converge.sh render <bot> <artefact>   render one artefact: config | execstart | unit
  converge.sh plan-recycle <unit_changed> <running> <version> <has_reload>
                                        print the recycle mechanism that state selects
  converge.sh plan-stale <running> <want> <running_ver>
                                        print whether the running image is stale
  converge.sh semver-cmp <a> <b>        print -1 | 0 | 1
  converge.sh semver-sat <ver> <constraint>
                                        print yes/no (exit 0/1)
  converge.sh resolve <component> <ver...>
                                        print the newest version satisfying every
                                        spec's require.<component>
EOF
  exit 1
}

[ $# -ge 1 ] || usage

case "$1" in
  --tot)
    [ $# -eq 1 ] || die "--tot takes no other arguments (tot never converges)"
    tot ;;
  render)
    [ $# -eq 3 ] || usage
    SPEC="$BOTS_DIR/$2.json"
    [ -f "$SPEC" ] || die "no such bot spec: $SPEC"
    validate_spec
    case "$3" in
      config)    render_config ;;
      execstart) render_execstart ;;
      unit)      render_unit ;;
      *)         usage ;;
    esac ;;
  plan-recycle)
    [ $# -eq 5 ] || usage
    recycle_plan "$2" "$3" "$4" "$5" ;;
  plan-stale)
    [ $# -eq 4 ] || usage
    stale_decision "$2" "$3" "$4" ;;
  semver-cmp)
    [ $# -eq 3 ] || usage
    ver_cmp "$2" "$3" ;;
  semver-sat)
    [ $# -eq 3 ] || usage
    if ver_satisfies "$2" "$3"; then echo yes; else echo no; exit 1; fi ;;
  resolve)
    [ $# -ge 2 ] || usage
    COMP=$2; shift 2
    resolve_component "$COMP" "$COMP" "$@" ;;
  -*) usage ;;
  *)
    BOT=$1; shift
    SPEC="$BOTS_DIR/$BOT.json"
    [ -f "$SPEC" ] || die "no such bot spec: $SPEC"
    validate_spec
    APPLY=0
    while [ $# -gt 0 ]; do
      case "$1" in
        --apply) APPLY=1; shift ;;
        --dry-run) APPLY=0; shift ;;
        --target-root)
          TARGET_ROOT=$(cd "$2" && pwd) || die "bad --target-root"
          PROC_ROOT="$TARGET_ROOT/proc"; shift 2 ;;
        --local) LOCAL=1; shift ;;
        --force-local) LOCAL=1; FORCE_LOCAL=1; shift ;;
        *) usage ;;
      esac
    done
    converge "$BOT" "$APPLY" ;;
esac
