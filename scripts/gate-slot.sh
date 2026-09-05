#!/usr/bin/env bash
# gate-slot.sh — runs a gate inside one of N machine-wide slots, with a CPU
# budget attached to the slot (NIM-801).
#
# Why it exists. Parallel ticket sessions never conflict over files — each has
# its own worktree and its own branch. They conflict over the MACHINE, and the
# gate does not survive it. Measured 2026-09-05 (session NIM-790): 24 cores,
# three concurrent `make check`, load average 25–33, and a red
# TestConsole_ThrottledSessionStillTearsDownFast in soul/internal/runtime/
# consolerunner. That test asserts a process produced output within 5 wall-clock
# seconds; under a 1.5x oversubscribed box the process simply does not get
# scheduled. Its own failure text says `do NOT raise the timeout`, and that is
# right: the budget is what makes it able to catch a real stall, so widening it
# to survive load is trading a phantom red for a blind test. The load is what
# has to go.
#
# The cost is not the lost time. A phantom red teaches sessions to ignore a red
# gate, and that is dearer than any flake.
#
# ★ TWO HALVES, AND THE SECOND IS NOT OPTIONAL.
#
#   1. N slots, not a mutex. A hard unit of one would separate the gates and
#      then underuse the box: while one gate downloads a vulnerability database
#      in check-vuln, 24 cores idle. So: N lock files, `flock -n` over them in
#      turn, run the gate in the free one.
#   2. A CPU budget per slot: GOMAXPROCS and `go -p` = nproc / GATE_SLOTS.
#      WITHOUT THIS THE SEMAPHORE FIXES NOTHING — two gates of 24 threads each
#      on 24 cores produce exactly the oversubscription measured above; the
#      semaphore would only have bounded how many gates were doing it. With the
#      budget the slots sum to the machine.
#
# ★ It is a BUDGET, not a hard cap, and the difference is worth stating rather
# than being discovered. `-p` bounds how many packages build/test at once;
# GOMAXPROCS bounds the runnable threads inside each test binary. Their product
# can briefly exceed the slot's share, and a test that spawns its own processes
# (this tree has several) is outside both. The goal is to remove a 1.5x
# oversubscription, not to guarantee a quota.
#
# ★ WHY THE GATE IS A CHILD AND NOT AN `exec`. The first version exec'd into the
# gate and let it inherit the locked descriptor — the classic flock idiom, and
# wrong here. A descriptor is inherited by EVERY descendant, so one orphaned
# child outliving the gate keeps the slot locked with no gate behind it, and at
# the documented GATE_SLOTS=1 fallback that wedges every other worktree's `make
# check` forever, for a reason unrelated to their diff — the exact outcome this
# script exists to prevent. Measured: a gate whose recipe left `sleep 30 &`
# exited 0 and the next gate waited on nothing. This tree leaks children (see
# scripts/check-makefile-recipes.py's docstring on the orphan `dev-stop` failed
# to kill), so it is not hypothetical. So: this shell keeps the lock, the gate
# runs as a child with fd 9 CLOSED, and the slot is held for exactly as long as
# this process lives. The mirror failure — SIGKILL this supervisor and the lock
# frees while the gate runs on — is the lesser one by far: it costs a transient
# overload, not a permanent wedge.
#
# Waiting is PRINTED, deliberately. A gate blocked on a lock and a gate hung on
# a tier look identical from outside, and a session that cannot tell them apart
# kills the gate by hand — which is the same lost run this script exists to
# prevent, arrived at from the other side.
#
# Escape hatches: GATE_SLOTS=1 is the strict serialisation this replaces (and
# the fallback if 2 turns out to be too many); running scripts/gate.sh directly
# takes no slot at all.
#
# Usage (from the repository root):
#   scripts/gate-slot.sh <command> [args...]
set -uo pipefail

self="gate-slot.sh"

if [ "$#" -eq 0 ]; then
  echo "${self}: usage: gate-slot.sh <command> [args...]" >&2
  exit 2
fi

# Already inside a slot — do not take a second one. Without this a tier that
# calls `make check` deadlocks itself at GATE_SLOTS=1 and quietly doubles the
# CPU budget above it at any other value. The gate exports this before running
# the child, so it covers everything running underneath.
if [ -n "${GATE_SLOT_HELD:-}" ]; then
  echo "gate: already inside slot ${GATE_SLOT_HELD} — not taking a second"
  exec "$@"
fi

# positive_int <name> <value> — echo the value in base 10, or fail.
#
# `10#` and not bare arithmetic: `08` passes the digits-only test above and then
# dies inside $(( )) with `value too great for base`, taking the gate with it
# after a slot has already been taken. A padded count is reachable from any
# wrapper that builds one, and it must mean eight.
positive_int() {
  case "$2" in
    '' | *[!0-9]*) return 1 ;;
  esac
  local n=$((10#$2))
  [ "${n}" -ge 1 ] || return 1
  printf '%s' "${n}"
}

# A typo must not become "unbounded". GATE_SLOTS reaches here through make and
# the environment, so `GATE_SLOTS=two` and `GATE_SLOTS=` are both reachable by
# accident, and both would otherwise degrade into the free-for-all above.
if ! slots="$(positive_int GATE_SLOTS "${GATE_SLOTS:-2}")"; then
  echo "${self}: GATE_SLOTS must be a positive integer, got '${GATE_SLOTS:-}'" >&2
  exit 2
fi

# The slot directory is MACHINE-wide on purpose: the whole point is that gates
# in different worktrees see each other, so it cannot live under the checkout.
# /tmp is spelled out rather than taken from TMPDIR, because a session with its
# own TMPDIR (a per-session tmpdir wrapper, PrivateTmp, a container) would get a
# private set of locks and a silent free-for-all in which every gate still
# prints `slot 1/2`. Keyed by uid so two users on one box do not fight over each
# other's locks. GATE_SLOT_DIR is the deliberate override.
slot_dir="${GATE_SLOT_DIR:-/tmp/soul-stack-gate.$(id -u)}"
if ! mkdir -p "${slot_dir}" 2>/dev/null; then
  echo "${self}: cannot create the slot directory ${slot_dir}" >&2
  exit 2
fi

# CPU budget. GATE_SLOT_CPUS overrides the detected count — it is what the guard
# test pins so its assertions do not restate the script's own arithmetic, and it
# doubles as the knob for a session that wants a different share.
#
# A malformed GATE_SLOT_CPUS is fatal for the same reason a malformed GATE_SLOTS
# is: the alternative is that `GATE_SLOT_CPUS=abc` reports "nproc failed" (untrue
# — nproc was never asked) and runs the gate with no budget at all, which is the
# half of this fix that does the work, silently absent.
if [ -n "${GATE_SLOT_CPUS:-}" ]; then
  if ! cpus="$(positive_int GATE_SLOT_CPUS "${GATE_SLOT_CPUS}")"; then
    echo "${self}: GATE_SLOT_CPUS must be a positive integer, got '${GATE_SLOT_CPUS}'" >&2
    exit 2
  fi
else
  cpus="$(nproc 2>/dev/null || getconf _NPROCESSORS_ONLN 2>/dev/null || true)"
  if ! cpus="$(positive_int nproc "${cpus}")"; then
    # Loud, and no budget rather than a guessed one: pinning to 1 would make
    # every gate on this machine crawl, and silently skipping the half that
    # does the work would leave the semaphore looking like a fix.
    echo "gate: WARNING — cannot determine the CPU count (nproc failed), so this slot"
    echo "gate:   runs WITHOUT a CPU budget. The semaphore alone does not fix the"
    echo "gate:   oversubscription it exists for. Set GATE_SLOT_CPUS=<n> to restore it."
    cpus=""
  fi
fi

if [ -n "${cpus}" ]; then
  budget=$((cpus / slots))
  [ "${budget}" -ge 1 ] || budget=1
else
  budget=""
fi

# holder_of <lock> — "pid <pid> — <cwd>" for the gate holding that slot.
#
# The record is written by the holder and never cleaned up, so a line can outlive
# its gate; `kill -0` is what separates the two. Since the lock is held by the
# supervisor process whose pid is recorded here, a locked slot always has a live
# process behind it — a dead holder means the lock is already gone, so a slot
# that reads as "held" by nobody is a transient, not a wedge.
holder_of() {
  local line pid
  line="$(cat "$1" 2>/dev/null)" || return 0
  pid="${line%% *}"
  case "${pid}" in
    '' | *[!0-9]*) return 0 ;;
  esac
  kill -0 "${pid}" 2>/dev/null || return 0
  printf 'pid %s — %s' "${pid}" "${line#* }"
}

# report_holders — one line per busy slot, for the session doing the waiting.
report_holders() {
  local i held
  for i in $(seq 1 "${slots}"); do
    held="$(holder_of "${slot_dir}/slot-${i}.lock")"
    echo "gate:   slot ${i} held by ${held:-a gate that is already exiting}"
  done
}

# try_acquire — take the first free slot, leaving it held on fd 9. Sets `slot`.
#
# fd 9 is a plain numeric descriptor, held by THIS shell for the whole run; the
# child below is given it CLOSED (see the header). The holder record is written
# with `>` — it truncates, so a concurrent reader can catch the file empty, and
# holder_of degrades to "already exiting" rather than to a wrong name.
try_acquire() {
  local i
  for i in $(seq 1 "${slots}"); do
    exec 9>>"${slot_dir}/slot-${i}.lock" || continue
    if flock -n 9; then
      slot="${i}"
      printf '%s %s\n' "$$" "$(pwd)" >"${slot_dir}/slot-${i}.lock"
      return 0
    fi
  done
  exec 9>&- 2>/dev/null || true
  return 1
}

if ! command -v flock >/dev/null 2>&1; then
  # Running anyway, loudly. Refusing would turn a missing util-linux into a red
  # gate, which is a worse trade than an unserialised one — but this must never
  # be quiet, or the machine goes back to the free-for-all with nothing said.
  echo "gate: WARNING — flock(1) not found, so this gate takes NO slot and runs"
  echo "gate:   unserialised. Concurrent gates will oversubscribe the machine"
  echo "gate:   (NIM-801). Install util-linux to restore the semaphore."
  slot=0
elif ! try_acquire; then
  waited=0
  # The count is OBSERVED, not assumed: this line is only reached after a
  # non-blocking attempt on every one of the N slots failed.
  echo "gate: waiting for a slot, ${slots} in flight (GATE_SLOTS=${slots})"
  report_holders
  echo "gate:   this is a QUEUE, not a hang — it starts as soon as one finishes."
  echo "gate:   to jump it: scripts/gate.sh takes no slot, GATE_SLOTS=<n> widens it."
  until try_acquire; do
    sleep 2
    waited=$((waited + 2))
    # A heartbeat, because the line above scrolls away behind whatever else the
    # session is watching, and its absence for ten minutes reads as death. The
    # holders are re-read rather than repeated, so a wait that has outlived its
    # explanation still names a process someone can go and look at.
    if [ $((waited % 60)) -eq 0 ]; then
      echo "gate: still waiting for a slot, ${slots} in flight (${waited}s)"
      report_holders
    fi
  done
  echo "gate: slot ${slot} of ${slots} acquired after ${waited}s"
fi

if [ -n "${budget}" ]; then
  # GOFLAGS entries are applied only when the flag is known to the command being
  # run, so `-p` reaches build/test/vet/list and is ignored by everything else.
  # Ours goes FIRST: flags later in the list win, so an outer GOFLAGS and any
  # explicit `-p` on a recipe's own command line (test-integration's
  # INTEGRATION_PARALLEL, the `-p 1` of the docker tiers) still override it.
  export GOMAXPROCS="${budget}"
  export GOFLAGS="-p=${budget}${GOFLAGS:+ ${GOFLAGS}}"
  echo "gate: slot ${slot}/${slots}, budget GOMAXPROCS=${budget} go -p=${budget} (of ${cpus} cores)"
  echo "gate:   a budget, not a hard cap — -p bounds packages, GOMAXPROCS bounds threads"
  echo "gate:   inside each one, and their product can exceed the share in bursts."
else
  echo "gate: slot ${slot}/${slots}, no CPU budget"
fi

export GATE_SLOT_HELD="${slot}"
"$@" 9>&-
exit $?
