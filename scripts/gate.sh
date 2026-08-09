#!/usr/bin/env bash
# gate.sh — runs the gate's tiers independently and reports which ran, which did
# not, and why (NIM-373).
#
# Why it exists. `check` was a make prerequisite chain, so the first red tier
# stopped every tier behind it, and the ones that never executed left no trace
# whatsoever: make prints the failure and stops. Three times in one release the
# result was a claim nobody had earned:
#
#   1. `e2e needs: check` in CI — a red `check` reported L3a as `skipped`, and a
#      skipped job reads like a passed one at a glance (fixed in 9053b6b4).
#   2. `test` is the fifth tier of `check` and `check-dev-stand-build` the
#      thirteenth, so the guard NIM-342 added had never once run in CI while both
#      sides believed it was covered.
#   3. `check-webui` sits in the middle: an embedded-bundle drift left L1 and L3a
#      unexecuted, and "the gate was run" was true of the first half only.
#
# Each of those was a correct local decision — the failure was real, fixing the
# cheap early tier first is right — and the conclusion drawn from it ("we
# checked") was wrong. That is the NIM-238 class one level up: absence of a
# result reading as a result.
#
# So the tiers run independently, every one of them reports its own outcome, and
# the summary distinguishes three states rather than two: PASS, FAIL, NOT RUN.
# A tier that did not execute is never folded into either verdict.
#
# ★ Real dependencies are kept, invented ones are not. A tier may declare ONE
# causal predecessor with `tier@predecessor`, and the only one the gate uses is
# compilation: if `build` fails, running the test tiers produces a wall of output
# that says nothing the build failure did not already say. A bundle drift, a
# broken doc link or a lint finding, by contrast, tells you nothing about whether
# the integration suite passes — those tiers are independent and now run like it.
#
# Usage (from the repository root):
#   scripts/gate.sh <gate-name> <tier>[@<predecessor>]...
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${ROOT}"

# MAKE may carry arguments (`make -C <dir>`), which is how the guard test in
# scripts/gate-test.sh points this at a throwaway Makefile instead of the repo's.
read -r -a make_cmd <<<"${MAKE:-make}"

gate_name="${1:-}"
if [ -z "${gate_name}" ]; then
  echo "gate.sh: usage: gate.sh <gate-name> <tier>[@<predecessor>]..." >&2
  exit 2
fi
shift

if [ "$#" -eq 0 ]; then
  echo "gate.sh: ${gate_name}: no tiers given — refusing to report a gate that checks nothing" >&2
  exit 2
fi

names=()
outcomes=()
seconds=()
reasons=()

# outcome_of — the recorded outcome of an already-run tier, empty if this gate
# never listed it. An unknown predecessor is a typo in the tier list, and it must
# not silently degrade into "no dependency" (that is the failure mode this whole
# script exists to remove), so run_tier below treats it as fatal.
outcome_of() {
  local want="$1" i
  for i in "${!names[@]}"; do
    if [ "${names[$i]}" = "${want}" ]; then
      printf '%s' "${outcomes[$i]}"
      return 0
    fi
  done
  return 1
}

total=$#
index=0

for spec in "$@"; do
  index=$((index + 1))
  tier="${spec%%@*}"
  predecessor=""
  if [ "${spec}" != "${tier}" ]; then
    predecessor="${spec#*@}"
  fi

  if [ -n "${predecessor}" ]; then
    if ! predecessor_outcome="$(outcome_of "${predecessor}")"; then
      echo "gate.sh: ${tier} declares '${predecessor}' as its predecessor, but no tier by that" >&2
      echo "gate.sh:   name runs before it in this gate. Fix the tier list — a dependency the" >&2
      echo "gate.sh:   gate cannot resolve would silently become no dependency at all." >&2
      exit 2
    fi
    if [ "${predecessor_outcome}" != "PASS" ]; then
      names+=("${tier}")
      outcomes+=("NOT RUN")
      seconds+=("-")
      reasons+=("${predecessor} did not pass")
      printf '\n=== gate %s [%d/%d] %s — NOT RUN (%s did not pass)\n' \
        "${gate_name}" "${index}" "${total}" "${tier}" "${predecessor}"
      continue
    fi
  fi

  printf '\n=== gate %s [%d/%d] %s\n' "${gate_name}" "${index}" "${total}" "${tier}"
  started="${SECONDS}"
  if "${make_cmd[@]}" "${tier}"; then
    outcome="PASS"
  else
    outcome="FAIL"
  fi
  # SECONDS is derived from the wall clock, which can step backwards — an NTP
  # correction, and under WSL2 a resume — so this difference can come out
  # negative. Seen twice in one session: `PASS proxmox -1s` in a make
  # test-plugins table and `PASS check-gate -1s` in this very summary, which is
  # the table a release run is read from. The column is an integer-second aid,
  # not a measurement, so clamping costs it nothing; a negative number costs the
  # whole table its credibility, and for a reporter whose only job is to be
  # believed that is the expensive half. Same clamp as scripts/modules-run.sh
  # (NIM-611).
  #
  # KNOWN BOUNDARY — no guard covers this, and the absence is deliberate.
  # SECONDS cannot be made to step backwards between two reads in the same
  # shell: a subshell does not move the parent's, and seeding it from the
  # environment shifts both reads equally. A cheap imitation was written and
  # measured on NIM-494: it stayed green with the clamp and without it, so it
  # gated nothing — the very defect class NIM-481 is about. Reproducing it needs
  # libfaketime, which is not worth a dependency for a cosmetic column.
  elapsed=$((SECONDS - started))
  [ "${elapsed}" -ge 0 ] || elapsed=0

  names+=("${tier}")
  outcomes+=("${outcome}")
  seconds+=("${elapsed}")
  reasons+=("")

  if [ "${outcome}" = "FAIL" ]; then
    printf '=== gate %s [%d/%d] %s FAILED after %ds — continuing with the rest\n' \
      "${gate_name}" "${index}" "${total}" "${tier}" "${elapsed}"
  fi
done

passed=0
failed=0
notrun=0

echo ""
echo "gate ${gate_name}: what ran and what it said"
for i in "${!names[@]}"; do
  case "${outcomes[$i]}" in
    PASS) passed=$((passed + 1)) ;;
    FAIL) failed=$((failed + 1)) ;;
    *) notrun=$((notrun + 1)) ;;
  esac
  if [ "${seconds[$i]}" = "-" ]; then
    took="-"
  else
    took="${seconds[$i]}s"
  fi
  printf '  %-8s %-26s %6s   %s\n' \
    "${outcomes[$i]}" "${names[$i]}" "${took}" "${reasons[$i]}"
done
echo ""
printf 'gate %s: %d passed, %d failed, %d not run (of %d tiers)\n' \
  "${gate_name}" "${passed}" "${failed}" "${notrun}" "${total}"

if [ "${failed}" -eq 0 ] && [ "${notrun}" -eq 0 ]; then
  exit 0
fi

if [ "${notrun}" -gt 0 ]; then
  echo "gate ${gate_name}: a NOT RUN tier is not a passed one — this gate is silent about it."
fi
echo "gate ${gate_name}: FAILED"
exit 1
