#!/usr/bin/env bash
# Extract the CHANGELOG.md section for one version, as GitHub release notes.
#
# GoReleaser's github-native changelog produces a commit list and a diff link,
# which is the raw material of release notes, not release notes: the reasoning
# is already written in CHANGELOG.md and gets thrown away. This script makes
# the changelog section the release body, so the notes and the file cannot
# drift apart.
#
# Usage: scripts/release-notes.sh [version] [changelog]
#   version   defaults to the contents of VERSION
#   changelog defaults to CHANGELOG.md
#
# Prints the body to stdout. Exits non-zero when the section is missing or
# empty — a release with no changelog entry is a release nobody can read.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
version="${1:-$(cat "$root/VERSION")}"
version="${version#v}"
changelog="${2:-$root/CHANGELOG.md}"

body="$(awk -v v="$version" '
  # Section headings look like: ## [0.35.1] - 2026-09-17
  /^## / {
    if (inside) exit
    if (index($0, "[" v "]") > 0) { inside = 1; next }
  }
  inside { print }
' "$changelog")"

# Trim leading/blank-only and trailing blank lines.
body="$(printf '%s\n' "$body" | sed -e '/./,$!d' | awk '{ lines[NR] = $0 }
  END { last = 0; for (i = 1; i <= NR; i++) if (lines[i] ~ /./) last = i
        for (i = 1; i <= last; i++) print lines[i] }')"

if [ -z "$body" ]; then
  echo "ABORT: no CHANGELOG.md section for $version." >&2
  echo "Add a '## [$version] - <date>' section before releasing." >&2
  exit 1
fi

printf '%s\n' "$body"
