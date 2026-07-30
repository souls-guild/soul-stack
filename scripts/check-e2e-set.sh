#!/usr/bin/env bash
# check-e2e-set.sh — cross-checks each e2e suite's package set against a second,
# independent derivation of the same fact. Same shape and same reason as
# scripts/check-integration-set.sh, applied to the tiers L1 already had covered
# and L3a did not.
#
# Why this is not paranoia. `make e2e` asks `go list -tags=e2e ./...` for its
# packages and, when that comes back empty, prints "skip tests/e2e" and exits 0.
# So a total loss of L3a — a renamed tag, a moved directory, a `go list` that
# errored for any reason at all (the Makefile sends its stderr to /dev/null) —
# is indistinguishable from a tree that simply has no e2e tests: green, fast and
# silent. That is NIM-238 with a different cause, and it is exactly what NIM-221
# found L1 doing for months before check-integration-set existed. L3a carries the
# claim "the product works end to end", which makes an empty one the most
# expensive kind of quiet.
#
# The second derivation shares nothing with the first: it greps the tree for
# build-constraint lines instead of asking the toolchain. One directory is one Go
# package, so the number of directories holding a tagged test file is the number
# of packages the suite must contain.
#
# Unlike the Makefile's loops, a `go list` that FAILS is a failure here, not an
# empty answer. "This module has no packages" and "the tool that answers that
# question broke" are different events and must not share an outcome (NIM-392).
#
# Run from the repository root.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${ROOT}"

# suite = <module-dir>:<build-tag>. Every tier that gates on a tagged corpus
# belongs here; a suite missing from this list is a suite nobody counts.
suites=(
  "tests/e2e:e2e"
  "tests/e2e-live:e2e_live"
  "tests/e2e-k8s:e2e_k8s"
)

status=0

for suite in "${suites[@]}"; do
  m="${suite%%:*}"
  tag="${suite#*:}"

  if [ ! -d "${m}" ]; then
    echo "check-e2e-set: ${m}: the module directory is gone — the ${tag} suite cannot run at all."
    echo "check-e2e-set:   If that is intentional, remove it from this script's suite list too,"
    echo "check-e2e-set:   so the gate stops claiming to count something that no longer exists."
    status=1
    continue
  fi

  # Derivation A — the toolchain, asked the same way the Makefile asks it. A
  # non-zero exit is fatal on purpose: it means the answer is unknown, not zero.
  if ! listed="$(cd "${m}" && go list -tags="${tag}" -f '{{if or .TestGoFiles .XTestGoFiles}}{{.ImportPath}}{{end}}' ./...)"; then
    echo "check-e2e-set: ${m}: 'go list -tags=${tag}' failed — the set is unknown, which is not the same"
    echo "check-e2e-set:   as empty. Fix the toolchain error above; do not let it read as 'no tests here'."
    status=1
    continue
  fi
  got="$(printf '%s\n' "${listed}" | grep -c . || true)"

  # Derivation B — the tree. Directories holding at least one _test.go whose
  # build constraint names this tag. `testdata/` and `_`/`.`-prefixed directories
  # are dropped because the toolchain does not see them either: counting them
  # would make derivation B disagree with A for a reason that is not a defect,
  # and a guard that cries wolf gets lowered.
  want="$(grep -rl --include='*_test.go' -E "^//go:build([^/]*[[:space:]])?${tag}([[:space:]]|\$|&|\|)" "${m}" 2>/dev/null \
    | while IFS= read -r f; do dirname "${f}"; done \
    | grep -vE '(^|/)(testdata|_[^/]*|\.[^/]+)(/|$)' \
    | sort -u | grep -c . || true)"

  if [ "${want}" -eq 0 ]; then
    echo "check-e2e-set: ${m}: no file in the tree carries the ${tag} build tag."
    echo "check-e2e-set:   'make ${m##*/}' would print a skip line and exit 0, so the suite would be gone"
    echo "check-e2e-set:   and every gate reading that exit code would call it a pass."
    status=1
    continue
  fi

  if [ "${got}" -lt "${want}" ]; then
    echo "check-e2e-set: ${m}: the ${tag} set has ${got} package(s), the tree has ${want} with a tagged test file."
    echo "check-e2e-set:   The toolchain is under-reporting, so the suite would run less than it claims"
    echo "check-e2e-set:   and still exit 0. Fix the derivation, do not lower this check."
    status=1
  else
    echo "check-e2e-set: ${m}: ${got} ${tag} package(s), tree agrees (${want})"
  fi
done

if [ "${status}" -eq 0 ]; then
  echo "check-e2e-set: every e2e suite is non-empty and matches the tree — a green run covers real tests"
fi
exit "${status}"
