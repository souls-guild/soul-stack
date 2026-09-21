#!/usr/bin/env bash
# gate-slot-test.sh — docker-free guard tests for scripts/gate-slot.sh (NIM-801).
#
# What is being guarded, and why it needs a guard at all. gate-slot.sh has no
# output of its own to be wrong: it either serialises the gates or it does not,
# and BOTH failures are quiet. A semaphore that stopped locking leaves every
# gate green and brings back the load average that reddens
# TestConsole_ThrottledSessionStillTearsDownFast — a phantom red in someone
# else's session, days later, blamed on their diff. A budget that stopped being
# exported does exactly the same, while the locking still looks like it works.
# So the two halves are asserted separately: the arithmetic by pinning
# GATE_SLOT_CPUS, the exclusion by actually running two gates at once.
#
# The pairs are red/green by construction: at GATE_SLOTS=1 the second gate must
# NOT leave its marker, at GATE_SLOTS=2 it must — the same helper, the same
# holder, one variable apart. A lock that silently stopped locking passes
# neither.
#
# Reuses the micro-harness of the e2e-cloud guard (it/assert_eq/harness_summary),
# as scripts/gate-test.sh does.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${ROOT}"

. "${ROOT}/scripts/e2e-cloud/test/_harness.sh"

# This guard is itself a tier of `check`, so it runs INSIDE a slot and inherits
# everything that slot exported. Left in place, GATE_SLOT_HELD sends every case
# below down the reentrancy shortcut (asserting nothing about locking at all)
# and the inherited budget appends itself to every GOFLAGS assertion. The cases
# are about acquisition from a clean environment, so the environment is cleaned.
unset GATE_SLOT_HELD GATE_SLOTS GATE_SLOT_DIR GOFLAGS GOMAXPROCS

# The script under test, and the seam that lets this guard be pointed at a
# deliberately broken COPY under /tmp. That is how the mutation evidence in the
# commit message was produced without ever leaving a mutated script in the
# worktree — where an interrupted run would hand the next session a gate that
# does not lock. Unset, it is the worktree's own script and nothing changes.
SLOT_SH="${GATE_SLOT_SH:-${ROOT}/scripts/gate-slot.sh}"

# ★ Every invocation below is wrapped in `timeout`, including the ones that
# cannot block today. A regression in this script does not make the guard fail,
# it makes the guard HANG — measured: with the child exec'd instead of forked,
# the leak case sat forever on a lock nobody was holding. An unbounded guard is
# worse than a red one, because `check-gate-slot` is a tier of `make check`: it
# would wedge the gate it exists to protect, in every worktree at once.
CAP="timeout 10"

tmp="$(mktemp -d)"
holder_pid=""
# The holder spins until its release file appears, and `rm -rf` deletes the
# directory that file would appear in — so tearing down without killing it first
# leaves a process waiting on a path that can never exist. It would then be an
# orphan holding a slot, which is the very defect the leak case below asserts is
# impossible.
trap 'kill "${holder_pid}" 2>/dev/null; rm -rf "${tmp}"' EXIT

slot_dirs=0

# new_dir — a private slot directory, so cases cannot inherit each other's locks.
new_dir() {
  slot_dirs=$((slot_dirs + 1))
  dir="${tmp}/slots-${slot_dirs}"
  mkdir -p "${dir}"
  printf '%s' "${dir}"
}

# run_slot <dir> <slots> <command...> — gate-slot.sh with a pinned CPU count.
# Sets `out` and `rc`. GATE_SLOT_CPUS is pinned so the budget assertions state a
# number rather than restating the script's own division of nproc.
run_slot() {
  local dir="$1" slots="$2"
  shift 2
  out="$(GATE_SLOT_DIR="${dir}" GATE_SLOTS="${slots}" GATE_SLOT_CPUS=24 \
    ${CAP} "${SLOT_SH}" "$@" 2>&1)"
  rc=$?
}

# budget_of <dir> <slots> <cpus> — what the WRAPPED command actually inherits,
# as "<GOMAXPROCS>|<GOFLAGS>".
#
# It reads the child's environment through a marker rather than matching the
# whole output, and the difference is the guard's own known trap: gate-slot.sh
# announces the budget in a line of its own, so a substring search over the
# output passes on a budget that is printed and never exported. Measured — with
# the two `export`s removed and again with the `budget=1` floor removed, a
# `contains` assertion stayed green on the announcement alone.
budget_of() {
  local dir="$1" slots="$2" cpus="$3" line
  line="$(GATE_SLOT_DIR="${dir}" GATE_SLOTS="${slots}" GATE_SLOT_CPUS="${cpus}" \
    ${CAP} "${SLOT_SH}" \
    sh -c 'echo "budget-marker ${GOMAXPROCS:-unset}|${GOFLAGS:-unset}"' 2>/dev/null |
    grep '^budget-marker ')"
  printf '%s' "${line#budget-marker }"
}

# hold_slot <dir> <slots> <release-file> — take a slot in the background and keep
# it until <release-file> appears. Returns once the slot is provably held.
hold_slot() {
  local dir="$1" slots="$2" release="$3" waited=0
  GATE_SLOT_DIR="${dir}" GATE_SLOTS="${slots}" GATE_SLOT_CPUS=24 \
    "${SLOT_SH}" \
    bash -c 'touch "$1"; while [ ! -e "$2" ]; do sleep 0.05; done' \
    holder "${dir}/held" "${release}" >/dev/null 2>&1 &
  holder_pid=$!
  while [ ! -e "${dir}/held" ]; do
    sleep 0.05
    waited=$((waited + 1))
    if [ "${waited}" -gt 200 ]; then
      fail "the background holder never acquired its slot"
      return 1
    fi
  done
}

contains() { case "${out}" in *"$1"*) printf 'yes' ;; *) printf 'no' ;; esac; }
exists() { [ -e "$1" ] && printf 'yes' || printf 'no'; }

it "the CPU budget is the machine divided by the slots"
dir="$(new_dir)"
assert_eq '8|-p=8' "$(budget_of "${dir}" 3 24)" "24 cores over 3 slots is 8 each, in both knobs"

it "the default is 2 slots when GATE_SLOTS says nothing"
dir="$(new_dir)"
out="$(GATE_SLOT_DIR="${dir}" GATE_SLOT_CPUS=24 \
  ${CAP} "${SLOT_SH}" \
  sh -c 'echo "budget-marker ${GOMAXPROCS:-unset}"' 2>/dev/null |
  grep '^budget-marker ')"
assert_eq 'budget-marker 12' "${out}" "half the machine"

it "the budget never rounds down to zero"
dir="$(new_dir)"
assert_eq '1|-p=1' "$(budget_of "${dir}" 8 2)" "more slots than cores still leaves one core each"

# Every other budget case pins GATE_SLOT_CPUS, which means none of them exercises
# the detection itself: replacing it with `cpus=""` leaves a gate that prints
# `cannot determine the CPU count` and runs with NO budget, and every pinned case
# stays green. `nproc` here is a second, independent invocation — if nproc itself
# lies both sides move together, but that is not the regression being guarded.
it "the budget comes off the real machine when nothing pins it"
dir="$(new_dir)"
expected=$(( $(nproc) / 2 ))
[ "${expected}" -ge 1 ] || expected=1
out="$(GATE_SLOT_DIR="${dir}" GATE_SLOTS=2 \
  ${CAP} "${SLOT_SH}" \
  sh -c 'echo "budget-marker ${GOMAXPROCS:-unset}"' 2>/dev/null |
  grep '^budget-marker ')"
assert_eq "budget-marker ${expected}" "${out}" "detection ran, and half the box came out"

it "a zero-padded slot count means what it looks like"
dir="$(new_dir)"
assert_eq '3|-p=3' "$(budget_of "${dir}" 08 24)" "08 is eight, not an arithmetic base error"

it "a malformed GATE_SLOT_CPUS is fatal, not a silent loss of the budget"
dir="$(new_dir)"
out="$(GATE_SLOT_DIR="${dir}" GATE_SLOTS=2 GATE_SLOT_CPUS=abc \
  ${CAP} "${SLOT_SH}" sh -c 'touch "'"${dir}"'/ran"' 2>&1)"
rc=$?
assert_eq 2 "${rc}" "exit code"
assert_eq no "$(exists "${dir}/ran")" "the gate did NOT run unbudgeted behind a typo"

it "an outer GOFLAGS survives and still wins"
dir="$(new_dir)"
out="$(GATE_SLOT_DIR="${dir}" GATE_SLOTS=3 GATE_SLOT_CPUS=24 GOFLAGS='-p=99' \
  ${CAP} "${SLOT_SH}" \
  sh -c 'echo "budget-marker $GOFLAGS"' 2>/dev/null |
  grep '^budget-marker ')"
assert_eq 'budget-marker -p=8 -p=99' "${out}" "the budget goes first, so a later flag overrides it"

it "the wrapped command's exit code is the script's"
dir="$(new_dir)"
run_slot "${dir}" 2 sh -c 'exit 7'
assert_eq 7 "${rc}" "not folded into 0 or 1"

it "a malformed GATE_SLOTS is fatal, not silently unbounded"
dir="$(new_dir)"
run_slot "${dir}" two sh -c 'touch "'"${dir}"'/ran"'
assert_eq 2 "${rc}" "exit code"
assert_eq no "$(exists "${dir}/ran")" "the gate did NOT run under a broken slot count"

it "GATE_SLOTS=1 serialises: the second gate waits instead of running"
dir="$(new_dir)"
hold_slot "${dir}" 1 "${dir}/release"
out="$(GATE_SLOT_DIR="${dir}" GATE_SLOTS=1 GATE_SLOT_CPUS=24 \
  timeout 3 "${SLOT_SH}" \
  sh -c 'touch "'"${dir}"'/second"' 2>&1)"
rc=$?
assert_eq 124 "${rc}" "it was still waiting when the timeout fired"
assert_eq no "$(exists "${dir}/second")" "the second gate did not start"
assert_eq yes "$(contains 'waiting for a slot, 1 in flight')" "and says so, with the count"
assert_eq yes "$(contains 'QUEUE, not a hang')" "so a session does not kill it by hand"
# The count alone is not enough to act on. `holder_pid` is the backgrounded
# gate-slot.sh, and that is the process the lock is held by and the pid it
# records, so this asserts the waiter names something real and killable rather
# than printing a number. Deleting the holder line leaves every other assertion
# in this case green.
assert_eq yes "$(contains "held by pid ${holder_pid} ")" "and names the gate it is behind"

it "the slot is released when its gate finishes"
touch "${dir}/release"
wait "${holder_pid}"
run_slot "${dir}" 1 sh -c 'touch "'"${dir}"'/third"'
assert_eq 0 "${rc}" "exit code"
assert_eq yes "$(exists "${dir}/third")" "the freed slot is takeable"
assert_eq no "$(contains 'waiting for a slot')" "and taken without waiting"

it "GATE_SLOTS=2 admits a second gate — this is a semaphore, not a mutex"
dir="$(new_dir)"
hold_slot "${dir}" 2 "${dir}/release"
out="$(GATE_SLOT_DIR="${dir}" GATE_SLOTS=2 GATE_SLOT_CPUS=24 \
  timeout 3 "${SLOT_SH}" \
  sh -c 'touch "'"${dir}"'/second"' 2>&1)"
rc=$?
assert_eq 0 "${rc}" "exit code"
assert_eq yes "$(exists "${dir}/second")" "it ran alongside the holder"
assert_eq yes "$(contains 'slot 2/2')" "in the second slot"
touch "${dir}/release"
wait "${holder_pid}"

# ★ The blocker this script's `exec`-based first version had. A descriptor is
# inherited by every descendant, so a gate that leaves one child behind used to
# keep its slot locked with no gate behind it — and at GATE_SLOTS=1 that wedges
# every other worktree's `make check` forever. This case is the reason the gate
# runs as a child with fd 9 closed instead of being exec'd into.
it "a child outliving the gate does not keep the slot"
dir="$(new_dir)"
GATE_SLOT_DIR="${dir}" GATE_SLOTS=1 GATE_SLOT_CPUS=24 \
  ${CAP} "${SLOT_SH}" \
  bash -c 'sleep 300 & echo "$!" >"$1"' orphan "${dir}/orphan.pid" >/dev/null 2>&1
run_slot "${dir}" 1 sh -c 'touch "'"${dir}"'/after-orphan"'
assert_eq 0 "${rc}" "exit code — the slot was free the moment the gate exited"
assert_eq yes "$(exists "${dir}/after-orphan")" "the next gate ran"
assert_eq no "$(contains 'waiting for a slot')" "and never waited on a lock nobody was holding"
kill "$(cat "${dir}/orphan.pid" 2>/dev/null)" 2>/dev/null

# The case below injects GATE_SLOT_HELD from outside, which only exercises the
# READ side. Dropping the `export` that puts it there leaves that case green and
# every real nested gate deadlocked against its own parent, so the propagation is
# observed here instead: an inner gate-slot.sh, at GATE_SLOTS=1, with the outer
# one holding the only slot.
it "the held slot propagates, so a nested gate does not deadlock on its parent"
dir="$(new_dir)"
out="$(GATE_SLOT_DIR="${dir}" GATE_SLOTS=1 GATE_SLOT_CPUS=24 \
  timeout 5 "${SLOT_SH}" \
  "${SLOT_SH}" sh -c 'touch "'"${dir}"'/nested-ran"' 2>&1)"
rc=$?
assert_eq 0 "${rc}" "exit code — 124 here means the inner gate queued behind the outer"
assert_eq yes "$(exists "${dir}/nested-ran")" "the nested command ran"
assert_eq yes "$(contains 'already inside slot 1')" "and said why it took no slot"

it "a gate already holding a slot does not take a second one"
dir="$(new_dir)"
hold_slot "${dir}" 1 "${dir}/release"
out="$(GATE_SLOT_DIR="${dir}" GATE_SLOTS=1 GATE_SLOT_CPUS=24 GATE_SLOT_HELD=1 \
  timeout 3 "${SLOT_SH}" \
  sh -c 'touch "'"${dir}"'/nested"' 2>&1)"
rc=$?
assert_eq 0 "${rc}" "a nested gate would otherwise deadlock against its own parent"
assert_eq yes "$(exists "${dir}/nested")" "it ran"
assert_eq yes "$(contains 'already inside slot 1')" "and said why it took no slot"
touch "${dir}/release"
wait "${holder_pid}"

harness_summary
