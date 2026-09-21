#!/usr/bin/env bash
# integration-packages.sh — prints the import paths of packages that actually
# carry `integration`-tagged test files, for the Go module in the current
# directory.
#
# Why this exists (NIM-349). `-tags=integration` does not NARROW a package set,
# it WIDENS it: `go test -tags=integration ./...` runs the tagged suites AND
# every untagged unit test in the tree. `make test-integration` also passes
# `-race`, so the entire unit corpus was being run under the race detector,
# inside the one job that is also starting ~40 container sets. A test that
# synchronises on a wall-clock interval then fails on instrumentation speed
# rather than on the behaviour it asserts, and the job stops being trustworthy in
# BOTH directions: red no longer means regression, green no longer means "no
# race". Two attempts of the same sha failed in two different packages and
# `DATA RACE` appeared in neither.
#
# The fix is to run L1 on the packages L1 is about. The set is DERIVED from the
# tree on every invocation — a package is in it when its test-file set grows
# under the tag — so it cannot rot the way a hand-kept list does. That matters
# more than the convenience: a list is how the tag-guarded suites stopped
# compiling unnoticed (NIM-207) and how a skipped suite kept reporting success
# (NIM-238).
#
# Nothing stops being tested. The excluded packages are exactly those with no
# integration-tagged file; their tests run in `make test` and `make test-race`,
# and `make vet-tags` still COMPILES the whole tree under the tag, so a tagged
# file that stops building is caught without docker. No non-test source file in
# this repository is guarded by `integration` outside a package that already
# carries tagged tests, and no file is guarded by `!integration`, so an excluded
# package builds identically with and without the tag.
#
# Usage (from inside a module directory):
#   scripts/integration-packages.sh                    # pattern ./...
#   scripts/integration-packages.sh ./internal/scenario/
set -euo pipefail

PATTERN="${1:-./...}"

# Number of test files per package, with and without the tag. A package whose
# count grows has files that only build under `integration`.
FMT='{{.ImportPath}} {{len .TestGoFiles}} {{len .XTestGoFiles}}'

# list [go flags] — sorted `FMT` lines on stdout.
#
# stderr is filtered, not swallowed. "the pattern does not exist in this module"
# is expected and says nothing: the Makefile walks all eight modules with one
# pattern, so `PKG=./internal/scenario/` is absent from six of them by
# construction, and printing that six times per run teaches the reader to skip
# this stream. Every OTHER diagnostic is passed through — a package that fails to
# load under the tag must be loud, because the alternative is a suite that
# quietly leaves the set and is never missed again (NIM-207, NIM-238).
list() {
  local err
  err="$(mktemp)"
  go list "$@" -f "${FMT}" "${PATTERN}" 2>"${err}" | sort || true
  grep -v -e 'directory not found' -e 'matched no packages' "${err}" >&2 || true
  rm -f "${err}"
}

tagged="$(list -tags=integration)"
plain="$(list)"

# -23: lines present in `tagged` only. A package identical in both has no tagged
# files and drops out; one absent from `plain` entirely (every Go file in it is
# tagged) stays in.
comm -23 <(printf '%s\n' "${tagged}") <(printf '%s\n' "${plain}") | cut -d' ' -f1
