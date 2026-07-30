#!/usr/bin/env bash
# check-integration-set.sh — cross-checks the L1 package set against a second,
# independent derivation of the same fact.
#
# Why this is not paranoia. `make test-integration` gets its package list from
# scripts/integration-packages.sh, which asks `go list` for the test-file count of
# each package with and without `-tags=integration`. If that ever returns nothing —
# a changed `-f` template field, a `go list` behaviour change, a typo — the target
# prints "skip <module>" eight times and exits 0. A total loss of L1 would look
# exactly like a tree with no integration tests: green, fast, and silent. That is
# NIM-238 with a different cause, and it is a risk the previous `./...` did not
# carry, so it has to be paid for here.
#
# The second derivation deliberately shares nothing with the first: it greps the
# tree for build-constraint lines instead of asking the toolchain. One directory is
# one Go package, so the count of directories holding a tagged file is the number
# of packages the suite must contain. The two numbers agreeing means the set is
# real; a shortfall means the derivation broke, and this exits non-zero instead of
# letting a green run assert something nobody checked.
#
# Run from the repository root.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${ROOT}"

status=0

for m in proto proto/plugin shared sdk keeper soul soul-lint soulctl; do
  [ -d "${m}" ] || continue

  # Derivation A — the toolchain, via the script the Makefile actually uses.
  got="$(cd "${m}" && "${ROOT}/scripts/integration-packages.sh" | grep -c . || true)"

  # Derivation B — the tree. Directories under this module holding at least one
  # file whose build constraint names `integration`. `-maxdepth`-free on purpose:
  # nested packages (keeper/internal/coremod/cloud) count too. Submodule
  # directories are excluded so proto/plugin is not counted inside proto.
  want="$(grep -rl --include='*.go' -E '^//go:build([^/]*[[:space:]])?integration([[:space:]]|$|&|\|)' "${m}" 2>/dev/null \
    | while IFS= read -r f; do d="$(dirname "${f}")"; \
        case "${d}" in "${m}"/plugin|"${m}"/plugin/*) [ "${m}" = proto ] && continue ;; esac; \
        printf '%s\n' "${d}"; \
      done | sort -u | grep -c . || true)"

  if [ "${got}" -lt "${want}" ]; then
    echo "check-integration-set: ${m}: the L1 set has ${got} package(s), the tree has ${want} with an integration-tagged file."
    echo "check-integration-set:   scripts/integration-packages.sh is under-reporting, so 'make test-integration'"
    echo "check-integration-set:   would run less than L1 and still exit 0. Fix the derivation, do not lower this check."
    status=1
  else
    echo "check-integration-set: ${m}: ${got} L1 package(s), tree agrees (${want})"
  fi
done

if [ "${status}" -eq 0 ]; then
  echo "check-integration-set: the L1 package set matches the tree — a green test-integration covers every tagged package"
fi
exit "${status}"
