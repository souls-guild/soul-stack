#!/usr/bin/env bash
# gate-test.sh — docker-free guard tests for scripts/gate.sh.
#
# What is being guarded. gate.sh exists so that a red tier stops silencing the
# tiers behind it (NIM-373), and the way that guarantee breaks is quiet: a change
# that reintroduces an early exit leaves every remaining tier unexecuted and the
# gate still ends in a summary line, which is exactly the shape the whole ticket
# is about. A gate reporting on a gate has to be checked by something that does
# not trust either of them, so these cases run gate.sh against a throwaway
# Makefile with fake tiers and assert on the outcome AND on which recipes left
# their marker in the output.
#
# The pairs are red/green by construction: `RAN-after` present proves execution
# continued past a failure, `RAN-needs` absent proves a causal dependency still
# suppresses what it should.
#
# Reuses the micro-harness of the e2e-cloud guard (it/assert_eq/harness_summary)
# rather than growing a second one.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${ROOT}"

. "${ROOT}/scripts/e2e-cloud/test/_harness.sh"

tmp="$(mktemp -d)"
trap 'rm -rf "${tmp}"' EXIT

cat >"${tmp}/Makefile" <<'EOF'
green: ; @echo RAN-green
red: ; @echo RAN-red; exit 1
after: ; @echo RAN-after
needs: ; @echo RAN-needs
selfskip: ; @echo "skip everything (no packages)"
EOF

# run_gate <tier>... — gate.sh over the throwaway Makefile. Sets `out` and `rc`.
run_gate() {
  out="$(MAKE="make -s -C ${tmp}" "${ROOT}/scripts/gate.sh" guard-test "$@" 2>&1)"
  rc=$?
}

# contains / lacks — presence of a recipe marker or a table cell in the output.
contains() { case "${out}" in *"$1"*) printf 'yes' ;; *) printf 'no' ;; esac; }
lacks() { case "${out}" in *"$1"*) printf 'no' ;; *) printf 'yes' ;; esac; }

it "all tiers pass"
run_gate green after
assert_eq 0 "${rc}" "exit code"
assert_eq yes "$(contains '2 passed, 0 failed, 0 not run')" "summary counts"
assert_eq yes "$(lacks 'NOT RUN')" "nothing is reported as not run"

it "a failing tier does not stop the tiers behind it"
run_gate green red after
assert_eq 1 "${rc}" "exit code"
assert_eq yes "$(contains 'RAN-after')" "the tier behind the failure executed"
assert_eq yes "$(contains '2 passed, 1 failed, 0 not run')" "summary counts"

it "a failure is reported as FAIL, not folded into the tiers that passed"
run_gate red after
assert_eq yes "$(contains 'FAIL     red')" "table row for the failing tier"
assert_eq yes "$(contains 'PASS     after')" "table row for the tier behind it"

it "a causal dependency still suppresses its dependents"
run_gate red needs@red
assert_eq 1 "${rc}" "exit code"
assert_eq yes "$(lacks 'RAN-needs')" "the dependent did NOT execute"
assert_eq yes "$(contains 'NOT RUN')" "and says so"
assert_eq yes "$(contains 'red did not pass')" "with the reason"
assert_eq yes "$(contains '0 passed, 1 failed, 1 not run')" "summary counts"

it "a NOT RUN tier never counts as a pass"
run_gate green red needs@red
assert_eq 1 "${rc}" "exit code"
assert_eq yes "$(contains '1 passed, 1 failed, 1 not run')" "three states, not two"

it "an unresolvable dependency is fatal, not silently no dependency"
run_gate needs@typo
assert_eq 2 "${rc}" "exit code"
assert_eq yes "$(lacks 'RAN-needs')" "the tier did not run under a broken declaration"

it "a gate with no tiers refuses to report success"
run_gate
assert_eq 2 "${rc}" "exit code"

# ★ KNOWN BOUNDARY, asserted so it stays written down instead of being
# remembered by whoever last looked.
#
# gate.sh judges a tier by its exit code, which means it can report what a tier
# ANSWERED and never what a tier DID. A tier that skips its own work and exits 0
# is therefore a PASS in the table, and the table is the strongest-looking claim
# in the output — so the gap reads as coverage.
#
# This is not hypothetical. `make test` takes its package list from
# `go list ./... 2>/dev/null`, and any failure of that command — a broken
# go.work, an unreadable go.mod, a toolchain change — comes back empty and is
# treated as "this module has no Go packages". Measured: with
# GOWORK=/nonexistent/go.work, `make test` prints eight skip lines, exits 0, and
# this gate prints `PASS test` for a tier that ran no tests at all.
#
# Closing it belongs in the tiers, not here: a tier must stop conflating "there
# is nothing to do" with "the tool that would have told me broke" (NIM-392 for
# the seven `go list` sites, NIM-394 for the e2e suites, already done). The case
# below asserts the CURRENT behaviour on purpose. If someone later teaches
# gate.sh to detect self-skips, this assertion is where they will find out that
# the old contract was deliberate rather than overlooked.
it "known boundary: a tier that skips itself internally is still reported PASS"
run_gate selfskip
assert_eq 0 "${rc}" "exit code — the tier answered 0 and the gate believes it"
assert_eq yes "$(contains 'PASS     selfskip')" "the table calls it a pass"
assert_eq yes "$(lacks 'NOT RUN')" "the gate has no third thing to say about it"

harness_summary
