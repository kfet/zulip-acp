#!/usr/bin/env bash
# installsh_guard.sh — offline tests for the generated root install.sh.
#
# install.sh is generated from distkit's template, and `make check-installsh`
# already proves the checked-in copy matches the generator. What it does not
# prove is that the generator still emits the VERSION guard: distkit v0.1.6
# added it because the tag was pasted into two URL paths unvalidated, so
# VERSION=../../other/repo/releases/download/v1 walked out of this repo and
# installed another project's binary — verifying against a checksums.txt
# fetched from that same traversed location, which agreed with itself.
#
# These tests pin the *behaviour* this repo ships to `curl | sh` users, so a
# downgrade of the distkit pin fails the build rather than the fleet.
#
# Run from anywhere: ./test/installsh_guard.sh
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
INSTALL="$ROOT/install.sh"

pass=0 fail=0
ok()  { pass=$((pass + 1)); echo "  ok   $1"; }
bad() { fail=$((fail + 1)); echo "  FAIL $1"; }

# A network call from any of these would mean the guard ran too late. Point
# the installer at an unroutable host so a fetch cannot quietly succeed, and
# assert on the output that it never got that far.
run_installer() {
	env -u GITHUB_TOKEN -u GH_TOKEN \
		GITHUB_HOST="http://127.0.0.1:1" GITHUB_API="http://127.0.0.1:1" \
		VERSION="$1" PREFIX=/nonexistent \
		sh "$INSTALL" 2>&1 || true
}

echo "install.sh VERSION guard"

# The traversal itself: two path segments up and into another repo.
out=$(run_installer '../../other/repo/releases/download/v1')
case "$out" in
	*"bad VERSION"*) ok "traversing VERSION is refused" ;;
	*) bad "traversing VERSION not refused: $out" ;;
esac
case "$out" in
	*downloading*|*"checksum ok"*|*resolving*)
		bad "installer acted before refusing: $out" ;;
	*) ok "refused before any download" ;;
esac

# Every other shape that would land somewhere unintended. An empty VERSION is
# not among them: it defaults to `latest` at the top of the script, long before
# the guard sees it.
for v in 'v1/../../elsewhere' 'v1 v2' '-oops' 'v1;rm -rf /' 'v1$(id)' 'release/v1'; do
	out=$(run_installer "$v")
	case "$out" in
		*"bad VERSION"*) ok "refused VERSION '$v'" ;;
		*) bad "accepted VERSION '$v': $out" ;;
	esac
done

# The guard must not reject the tags people actually pass. Each must reach the
# fetch stage and fail against the unroutable host — reaching it is the proof
# it got past the guard, and distinguishes that from dying for some other
# reason before the network.
for v in v0.21.0 v1.2.3-rc1 v1.2.3+build.4 latest; do
	out=$(run_installer "$v")
	case "$out" in
		*"bad VERSION"*) bad "guard rejected the valid tag '$v': $out" ;;
		*downloading*|*resolving*) ok "accepted VERSION '$v'" ;;
		*) bad "VERSION '$v' never reached the fetch: $out" ;;
	esac
done

echo
echo "  $pass passed, $fail failed"
[ "$fail" -eq 0 ]
