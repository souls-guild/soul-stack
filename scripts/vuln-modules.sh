#!/usr/bin/env bash
# vuln-modules.sh — prints the check-vuln corpus: every Go module in this
# repository, one repo-relative path per line (NIM-774).
#
# Why the corpus is derived and not written down. `check-vuln` swept $(MODULES)
# — the eight entries the Makefile names for test/vet/build — and the tree holds
# twenty `go.mod` files. The twelve it never opened were not reported as
# skipped, because a corpus cannot report what it does not contain: the sweep
# printed "govulncheck is clean across all modules" over a list that was missing
# every `examples/module/*` plugin, all four `tests/*` harnesses and the three
# plugin fixtures under `internal/pluginhost/testdata`.
#
# That is NIM-494's defect one level further out. There the loop stopped early
# and the modules behind it went unnamed; modules-run.sh fixed that by giving
# every module in the list a row. But a module that is not in the list has no
# row to be wrong — nothing on screen is even the wrong shape — so the fix
# stopped exactly at the edge of the list it was handed. The list was the floor
# and the summary line called it the total.
#
# A hand-kept list drifts in one direction only: a new module is a new
# directory, and nothing anywhere fails when nobody adds it. `find` cannot drift
# that way. The corpus is whatever the tree holds, so adding a module adds it to
# the gate, and removing one removes it — both without an edit here.
#
# Usage (from anywhere; the script resolves the repository root itself):
#   scripts/vuln-modules.sh
#
# Exit 2 and print nothing if the tree holds no module at all: an empty corpus
# would make modules-run.sh refuse the sweep one step later with a message about
# its calling convention, which sends the reader to the wrong question.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${ROOT}" || exit 2

modules="$(find . -name go.mod -not -path './.git/*' -print |
  sed -e 's|/go\.mod$||' -e 's|^\./||' | LC_ALL=C sort)"

if [ -z "${modules}" ]; then
  echo "vuln-modules.sh: no go.mod under ${ROOT} — refusing to hand check-vuln an empty corpus" >&2
  exit 2
fi

# Go files that belong to no module at all. `find -name go.mod` cannot see them
# by construction — that is the point — and they are the one blind spot this
# script cannot close by widening its own answer: no module means no `go list`,
# no `go build` and no scan, from any tool, ever. Two live cases:
# examples/module/redis-failover (main.go + schema.json, no go.mod), which
# `test-plugins` therefore does not sweep and no scan reaches — stated without a
# count, because the count moved twice while this comment stood still; and
# dev/stamp-artifact.go, a `go run` file deliberately
# outside the workspace. Named on stderr, never fatal — whether a directory is
# meant to be a module is a question for a person, and a gate that failed on it
# would be answering it.
orphans=""
while IFS= read -r dir; do
  [ -n "${dir}" ] || continue
  probe="${dir}"
  covered=""
  while :; do
    if [ -f "${probe}/go.mod" ]; then
      covered="yes"
      break
    fi
    [ "${probe}" != "." ] || break
    probe="$(dirname "${probe}")"
  done
  [ -n "${covered}" ] || orphans="${orphans}${dir}"$'\n'
done < <(find . -name '*.go' -not -path './.git/*' -print |
  sed -e 's|/[^/]*$||' -e 's|^\./||' | LC_ALL=C sort -u)

if [ -n "${orphans}" ]; then
  echo "vuln-modules.sh: Go files here belong to no module, so no scan of any kind reaches them:" >&2
  printf '%s' "${orphans}" | sed 's|^|vuln-modules.sh:   |' >&2
fi

printf '%s\n' "${modules}"
