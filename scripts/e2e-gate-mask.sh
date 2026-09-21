#!/usr/bin/env bash
# e2e-gate-mask.sh — the single definition of how $(E2E_GATE_TESTS) becomes the
# regexp `make e2e-live-gate` selects with, and the guard that every name in it
# is really a test.
#
#   e2e-gate-mask.sh mask   <name>...   print the -run regexp (no toolchain)
#   e2e-gate-mask.sh verify <name>...   assert the mask selects exactly those
#
# Why this is a script and not two lines of make (NIM-507). E2E_GATE_TESTS used
# to hold PREFIXES: `TestL3bPluginChannel` for `TestL3bPluginChannel_CatalogAndAllow`,
# and likewise for two of its other eight entries. `go test -run` takes an
# UNANCHORED regexp, so the gate did select the right nine tests and ran them
# correctly — which is why this survived. The damage was in the other two places
# the same list is used, both of which read it as a NAME:
#
#   - the gate's own `--- PASS: <name>` loop matched by prefix, so it could be
#     satisfied by a NEIGHBOUR's line. Add `TestL3bPluginChannel_Other` and the
#     named test may skip while the guard stays happy and the recipe exits 0 —
#     precisely the false-green NIM-45 built that loop to prevent.
#   - scripts/classify-e2e-live-failure.py matches names exactly, so on every
#     red run it printed three NOT-RUN lines for tests that had just passed. A
#     tool whose whole job is making a red gate legible was adding three lies to
#     each one.
#
# So the mask is anchored HERE and the names are checked against the toolchain
# HERE, in one place. A guard that built its own copy of the mask would stop
# saying anything about the gate the moment the two spellings diverged — and
# that divergence is the defect, not a side effect of it.
#
# `verify` is docker-free: `go test -list` builds the tagged binary and asks it
# for names; nothing in TestMain reaches for a daemon. That is what lets it sit
# in `make check` (via check-e2e-set) rather than only in the 20-minute job it
# guards.
set -euo pipefail

usage() {
  echo "usage: e2e-gate-mask.sh mask|verify <test-name>..." >&2
}

mode="${1:-}"
if [ "$#" -ge 1 ]; then shift; fi
case "${mode}" in
  mask | verify) ;;
  *)
    usage
    exit 2
    ;;
esac

if [ "$#" -eq 0 ]; then
  echo "e2e-gate-mask: the gate test list is EMPTY." >&2
  echo "e2e-gate-mask:   An empty -run mask matches EVERY test, and the gate's per-test" >&2
  echo "e2e-gate-mask:   '--- PASS' loop would iterate zero times — it would assert nothing" >&2
  echo "e2e-gate-mask:   and still exit 0. 'nothing is named' and 'nothing is missing' must" >&2
  echo "e2e-gate-mask:   not share an outcome (NIM-392)." >&2
  exit 1
fi

# A test name goes into a regexp AND into a `grep "^--- PASS: <name> ("` line.
# A `.` or a `|` that slipped in would silently widen the mask in the first and
# match the wrong line in the second, and both stay green while doing it.
bad=""
for n in "$@"; do
  case "${n}" in
    Test*[!A-Za-z0-9_]*) bad="${bad} ${n}" ;;
    Test*) ;;
    *) bad="${bad} ${n}" ;;
  esac
done
if [ -n "${bad}" ]; then
  echo "e2e-gate-mask: not a Go test identifier:${bad}" >&2
  echo "e2e-gate-mask:   Every entry must be a bare Test<Name> — it is used as a regexp and" >&2
  echo "e2e-gate-mask:   as a literal name, and a metacharacter widens the first while" >&2
  echo "e2e-gate-mask:   quietly mismatching the second." >&2
  exit 1
fi

dupes="$(printf '%s\n' "$@" | sort | uniq -d)"
if [ -n "${dupes}" ]; then
  echo "e2e-gate-mask: listed more than once:" >&2
  printf '  %s\n' ${dupes} >&2
  echo "e2e-gate-mask:   Harmless to the mask, but the list is also how many tests the gate" >&2
  echo "e2e-gate-mask:   claims to cover, and a duplicate inflates that count." >&2
  exit 1
fi

mask="^($(
  IFS='|'
  echo "$*"
))\$"

if [ "${mode}" = "mask" ]; then
  printf '%s\n' "${mask}"
  exit 0
fi

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${ROOT}"

# A `go test -list` that FAILS is a failure here, not an empty answer: "this
# suite has no such test" and "the tool that answers that question broke" are
# different events (NIM-392, same rule as check-e2e-set.sh).
if ! listed="$(cd tests/e2e-live && go test -tags=e2e_live -list "${mask}" . 2>&1)"; then
  echo "e2e-gate-mask: 'go test -tags=e2e_live -list' failed — the selected set is UNKNOWN," >&2
  echo "e2e-gate-mask:   which is not the same as empty. The toolchain output follows." >&2
  printf '%s\n' "${listed}" >&2
  exit 1
fi
selected="$(printf '%s\n' "${listed}" | grep -E '^Test[A-Za-z0-9_]*$' | sort -u || true)"

missing=""
for n in "$@"; do
  printf '%s\n' "${selected}" | grep -qx "${n}" || missing="${missing} ${n}"
done

extra=""
while IFS= read -r n; do
  [ -n "${n}" ] || continue
  case " $* " in
    *" ${n} "*) ;;
    *) extra="${extra} ${n}" ;;
  esac
done <<<"${selected}"

status=0
if [ -n "${missing}" ]; then
  echo "e2e-gate-mask: named by the gate, selected by nothing:${missing}" >&2
  echo "e2e-gate-mask:   Either the test was renamed or deleted, or the entry is a PREFIX of" >&2
  echo "e2e-gate-mask:   a real name rather than the name. Both are silent in the gate's" >&2
  echo "e2e-gate-mask:   own eyes: -run still selects something, so the run looks normal," >&2
  echo "e2e-gate-mask:   while the '--- PASS' guard matches a neighbour's line and the" >&2
  echo "e2e-gate-mask:   failure classifier reports the entry as NOT-RUN forever." >&2
  status=1
fi
if [ -n "${extra}" ]; then
  echo "e2e-gate-mask: selected by the mask, named by nobody:${extra}" >&2
  echo "e2e-gate-mask:   The gate would run these and then not check that they passed." >&2
  echo "e2e-gate-mask:   The mask is anchored, so reaching this means the anchoring was" >&2
  echo "e2e-gate-mask:   lost — fix that, do not add the names." >&2
  status=1
fi

if [ "${status}" -eq 0 ]; then
  echo "e2e-gate-mask: $# gate test(s), each an exact name the e2e_live suite really has"
fi
exit "${status}"
