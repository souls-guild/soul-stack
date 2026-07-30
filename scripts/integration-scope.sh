#!/usr/bin/env bash
# integration-scope.sh — refuses to let a narrowed L1 run report success on an
# empty selection.
#
# Why this exists. `make test-integration PKG=./internal/<pkg>/` is the everyday
# form for running one package's L1 suite, and it is in the brief of every ticket
# session. When the pattern matches nothing, the target prints one "skip <module>"
# line per module and exits 0:
#
#   $ make test-integration PKG=./internal/no-such-package/ ; echo rc=$?
#   skip keeper (no package under ./internal/no-such-package/ carries integration-tagged tests)
#   ...
#   rc=0
#
# A typo in the package name therefore reads as "L1 for my package is green"
# while nothing ran at all — the same shape as NIM-221 (a whole suite skipping
# quietly for months) and NIM-238 (an absent verdict read as a verdict), reached
# through the argument instead of through the tag.
#
# check-integration-set does not cover this: it is attached to test-integration
# only when PKG is ./..., i.e. exactly when the whole set runs anyway. The two
# guards answer different questions and both are needed — that one asks whether
# the DERIVATION is under-reporting across the tree, this one asks whether YOUR
# selection selected anything.
#
# The two ways to select nothing are different mistakes and get different
# messages: a path that does not exist is a typo, a path that exists but carries
# no tagged tests is a misunderstanding of which packages have L1 suites. Only
# the second is legitimate under ./..., where a module without tagged tests is
# simply skipped.
#
# Usage (from the repository root):
#   scripts/integration-scope.sh              # pattern ./...
#   scripts/integration-scope.sh ./internal/scenario/
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${ROOT}"

PATTERN="${1:-./...}"
MODULES="proto proto/plugin shared sdk keeper soul soul-lint soulctl"

total=0
path_exists=0

for m in ${MODULES}; do
  [ -d "${m}" ] || continue

  n="$(cd "${m}" && "${ROOT}/scripts/integration-packages.sh" "${PATTERN}" | grep -c . || true)"
  total=$((total + n))

  # Does the pattern name a directory that exists in this module at all? Only
  # meaningful for a narrowed pattern; ./... always "exists".
  rel="${PATTERN#./}"
  rel="${rel%/}"
  if [ "${PATTERN}" = "./..." ] || [ -d "${m}/${rel}" ]; then
    path_exists=1
  fi
done

if [ "${total}" -gt 0 ]; then
  echo "integration-scope: ${PATTERN} selects ${total} package(s) with integration-tagged tests"
  exit 0
fi

if [ "${PATTERN}" = "./..." ]; then
  echo "integration-scope: the whole tree selects NO package with integration-tagged tests."
  echo "integration-scope:   L1 would print a skip line per module and exit 0, which every gate"
  echo "integration-scope:   above reads as a pass. Either the suite is gone or the derivation in"
  echo "integration-scope:   scripts/integration-packages.sh broke — see also 'make check-integration-set'."
  exit 1
fi

if [ "${path_exists}" -eq 0 ]; then
  echo "integration-scope: PKG=${PATTERN} matches no directory in any module — this is a typo."
  echo "integration-scope:   Nothing would run, and the run would exit 0 and read as 'L1 is green"
  echo "integration-scope:   for my package'. Check the path against the module you meant:"
  echo "integration-scope:   e.g. PKG=./internal/scenario/ inside keeper."
  exit 1
fi

echo "integration-scope: PKG=${PATTERN} exists, but no package under it carries an integration-tagged test."
echo "integration-scope:   Its tests (if any) run in 'make test' and 'make test-race'; L1 has nothing"
echo "integration-scope:   to run here, so a green L1 for this package would mean nothing. Pick a"
echo "integration-scope:   package that has a tagged suite, or run the whole set with PKG=./..."
exit 1
