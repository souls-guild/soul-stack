#!/usr/bin/env bash
# modules-run-test.sh — docker-free, Go-free guard tests for scripts/modules-run.sh.
#
# What is being guarded. modules-run.sh exists so a red module stops silencing
# the modules behind it (NIM-494, the NIM-373 defect one level down), and the way
# that guarantee breaks is quiet: someone restores `|| exit 1` in the loop, or
# folds NOT RUN back into the pass count, and the sweep still ends in a summary
# line that looks exactly like a complete one. A reporter reporting on a reporter
# has to be checked by something that trusts neither, so these cases run
# modules-run.sh over throwaway directories with fake commands and assert on the
# exit code AND on which module markers actually reached the output.
#
# The pairs are red/green by construction: `RAN-c` present proves the sweep
# continued past a failure, `RAN-c` absent under MODULES_FAIL_FAST proves the
# opt-in stop still stops — and in both cases the table has to name c either way.
#
# The probe fixtures use `cat pkgs` rather than `go list ./...` on purpose: it
# reproduces the exact three-way shape (output + rc 0 / empty + rc 0 / rc != 0)
# with no toolchain in the loop, so this guard cannot go green or red for a
# reason that has nothing to do with the script.
#
# Reuses the micro-harness of the e2e-cloud guard (it/assert_eq/harness_summary)
# rather than growing a second one.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${ROOT}"

. "${ROOT}/scripts/e2e-cloud/test/_harness.sh"

tmp="$(mktemp -d)"
trap 'rm -rf "${tmp}"' EXIT

# The module command: leaves a marker naming the module it ran in, and fails if
# that module carries a `fail` file. Fixtures are built per case below.
CMD='printf "RAN-%s\n" "${PWD##*/}"; ! [ -e fail ]'

# fixture <case> <spec>... — build a case directory. Each spec is `<name>[:flags]`
# where flags may contain `fail` (the command exits non-zero there), `nopkgs` (an
# EMPTY pkgs file: the probe succeeds and finds nothing), `noprobe` (NO pkgs file
# at all: the probe itself fails, loudly) and `silentprobe` (a `quiet` marker the
# probe checks first, so it fails writing NOTHING). A module with none of them
# gets a pkgs file with content, the ordinary "there is work here" answer.
fixture() {
  local case_name="$1" spec name flags dir
  shift
  rm -rf "${tmp:?}/${case_name}"
  for spec in "$@"; do
    name="${spec%%:*}"
    flags="${spec#*:}"
    [ "${flags}" != "${spec}" ] || flags=""
    dir="${tmp}/${case_name}/${name}"
    mkdir -p "${dir}"
    case "${flags}" in
      *nopkgs*) : >"${dir}/pkgs" ;;
      *noprobe*) ;;
      *silentprobe*) echo "./..." >"${dir}/pkgs"; : >"${dir}/quiet" ;;
      *) echo "./..." >"${dir}/pkgs" ;;
    esac
    case "${flags}" in *fail*) : >"${dir}/fail" ;; esac
  done
}

# run_modules <case> <modules> — modules-run.sh over the case directory. Sets
# `out` and `rc`. Environment for the run is inherited from the caller, so a case
# can set MODULES_* inline.
run_modules() {
  local case_name="$1" mods="$2"
  out="$(cd "${tmp}/${case_name}" && "${ROOT}/scripts/modules-run.sh" guard "${mods}" "${CMD}" 2>&1)"
  rc=$?
}

contains() { case "${out}" in *"$1"*) printf 'yes' ;; *) printf 'no' ;; esac; }
lacks() { case "${out}" in *"$1"*) printf 'no' ;; *) printf 'yes' ;; esac; }

# ---------------------------------------------------------------------------
# The invariant the ticket is about: a failure early in the list must not decide
# what the rest of the list gets to say.
# ---------------------------------------------------------------------------

it "a failing module does not stop the modules behind it"
fixture past a b:fail c
MODULES_PROBE='' run_modules past "a b c"
assert_eq 1 "${rc}" "exit code"
assert_eq yes "$(contains 'RAN-c')" "the module behind the failure executed"
assert_eq yes "$(contains '2 passed, 1 failed, 0 skipped, 0 not run (of 3 modules)')" "summary counts"

it "the summary names every module and its own verdict"
assert_eq yes "$(contains 'PASS     a')" "the module before the failure"
assert_eq yes "$(contains 'FAIL     b')" "the failure itself"
assert_eq yes "$(contains 'PASS     c')" "the module behind it"
assert_eq yes "$(lacks 'the sweep ended before reaching')" "no module carries a reason it did not earn"
# The in-flight reason is pre-filled BEFORE the command and cleared after it, so
# every module that finishes has to hand it back. Drop the clearing line and a
# sweep that ran to the end still prints "interrupted while this module was
# running" beside three verdicts it plainly earned — a table that lies about a
# green run, which nothing else here would notice.
assert_eq yes "$(lacks 'interrupted while this module was running')" "and none keeps the in-flight reason once it has a verdict"

it "an all-green sweep exits 0 and reports nothing as not run"
fixture green a b c
MODULES_PROBE='' run_modules green "a b c"
assert_eq 0 "${rc}" "exit code"
assert_eq yes "$(contains '3 passed, 0 failed, 0 skipped, 0 not run (of 3 modules)')" "summary counts"
assert_eq yes "$(lacks 'NOT RUN')" "nothing is reported as not run"

it "every module in the list reaches the command, not just the ones before a failure"
fixture allfail a:fail b:fail c:fail
MODULES_PROBE='' run_modules allfail "a b c"
assert_eq 1 "${rc}" "exit code"
assert_eq yes "$(contains 'RAN-a')" "first module ran"
assert_eq yes "$(contains 'RAN-b')" "second module ran"
assert_eq yes "$(contains 'RAN-c')" "third module ran"
assert_eq yes "$(contains '0 passed, 3 failed, 0 skipped, 0 not run (of 3 modules)')" "summary counts"

# ---------------------------------------------------------------------------
# The probe's three answers. Collapsing the last two is the NIM-481 defect that
# `[ -z "$(go list ./... 2>/dev/null)" ]` shipped for seven call sites.
# ---------------------------------------------------------------------------

it "a probe that finds nothing is a SKIPPED module, named and explained"
fixture empty a b:nopkgs c
MODULES_PROBE='cat pkgs' MODULES_SKIP_NOTE='nothing generated here yet' \
  run_modules empty "a b c"
assert_eq 0 "${rc}" "exit code — an empty module is a real answer, not a failure"
assert_eq yes "$(contains 'SKIPPED  b')" "the skipped module is named in the table"
assert_eq yes "$(contains 'nothing generated here yet')" "with the reason"
assert_eq yes "$(lacks 'RAN-b')" "and the command did not run there"
assert_eq yes "$(contains '2 passed, 0 failed, 1 skipped, 0 not run (of 3 modules)')" "summary counts"

it "a probe that BREAKS is a failure, not a skip"
fixture broken a b:noprobe c
MODULES_PROBE='cat pkgs' MODULES_SKIP_NOTE='nothing generated here yet' \
  run_modules broken "a b c"
assert_eq 1 "${rc}" "exit code — 'we could not find out' is not 'there is nothing'"
assert_eq yes "$(contains 'FAIL     b')" "the module is reported as failed"
assert_eq yes "$(lacks 'SKIPPED  b')" "and never as skipped"
assert_eq yes "$(contains 'No such file')" "the probe's own diagnostic reaches the operator"
assert_eq yes "$(contains 'RAN-c')" "the modules behind it still ran"

it "MODULES_PROBE_FAIL=skip skips in its own words, not the empty-module ones"
fixture optout a b:noprobe c
MODULES_PROBE='cat pkgs' MODULES_PROBE_FAIL=skip \
  MODULES_SKIP_NOTE='nothing generated here yet' \
  MODULES_PROBE_SKIP_NOTE="doesn't resolve offline" \
  run_modules optout "a b c"
assert_eq 0 "${rc}" "exit code — the caller opted into this"
assert_eq yes "$(contains 'SKIPPED  b')" "the module is skipped"
assert_eq yes "$(contains "doesn't resolve offline")" "with the probe-failure reason"
assert_eq yes "$(lacks 'nothing generated here yet')" "and not with the empty-module reason"

# ---------------------------------------------------------------------------
# NOT RUN is a third state. It is never folded into either verdict.
# ---------------------------------------------------------------------------

it "MODULES_FAIL_FAST stops the sweep and names what it did not check"
fixture ff a b:fail c
MODULES_PROBE='' MODULES_FAIL_FAST=1 run_modules ff "a b c"
assert_eq 1 "${rc}" "exit code"
assert_eq yes "$(lacks 'RAN-c')" "the module behind the failure did NOT execute"
assert_eq yes "$(contains 'NOT RUN  c')" "and the table says so by name"
assert_eq yes "$(contains 'b failed and MODULES_FAIL_FAST is set')" "with the reason"
assert_eq yes "$(contains '1 passed, 1 failed, 0 skipped, 1 not run (of 3 modules)')" "three states, not two"
assert_eq yes "$(contains 'a NOT RUN module is not a passed one')" "the summary refuses to round it up"

it "a sweep that reached nothing is not a green sweep"
fixture ffirst a:fail b c
MODULES_PROBE='' MODULES_FAIL_FAST=1 run_modules ffirst "a b c"
assert_eq 1 "${rc}" "exit code"
assert_eq yes "$(contains '0 passed, 1 failed, 0 skipped, 2 not run (of 3 modules)')" "summary counts"

it "an interrupted sweep still reports how far it got"
fixture intr a b c
slow='touch started; sleep 1'
(cd "${tmp}/intr" && MODULES_PROBE='' "${ROOT}/scripts/modules-run.sh" guard "a b c" "${slow}") \
  >"${tmp}/intr.out" 2>&1 &
victim=$!
for _ in $(seq 1 400); do
  [ -e "${tmp}/intr/a/started" ] && break
  sleep 0.02
done
kill -TERM "${victim}" 2>/dev/null
wait "${victim}"
rc=$?
out="$(cat "${tmp}/intr.out")"
assert_eq 130 "${rc}" "exit code"
assert_eq yes "$(contains 'interrupted')" "the run says it was cut short"
assert_eq yes "$(contains 'NOT RUN  c')" "the module it never reached is named"
assert_eq yes "$(contains 'a NOT RUN module is not a passed one')" "and is not rounded up"

# Bash holds a trap until the foreground command returns, so the handler runs
# with the in-flight module's verdict still unwritten. Left to the pre-filled
# reason, the table then says "the sweep ended before reaching this module"
# about the module that was running — and on a CI timeout that is the one module
# the reader is looking for. Both are NOT RUN, and they must not read alike.
it "the module that was running when the sweep was cut off says so, not that it was never reached"
assert_eq yes "$(contains 'NOT RUN  a')" "the in-flight module has no verdict either"
assert_eq yes "$(contains 'interrupted while this module was running')" "and says which of the two it is"
assert_eq yes "$(contains 'the sweep ended before reaching this module')" "the module behind it keeps the other reason"

# ---------------------------------------------------------------------------
# Degenerate inputs. An empty module list is not a green sweep over nothing —
# that is the shape `test-plugins` printed its success line over.
# ---------------------------------------------------------------------------

it "a module list that expanded to nothing is a refusal, and says so in those words"
out="$("${ROOT}/scripts/modules-run.sh" guard "" "${CMD}" 2>&1)"
rc=$?
assert_eq 2 "${rc}" "exit code"
assert_eq yes "$(contains 'refusing to report a sweep over nothing')" "the reason is the empty corpus"
assert_eq yes "$(lacks 'usage:')" "not the calling convention, which was fine"
assert_eq yes "$(contains 'guard')" "and the sweep that found nothing is named"
assert_eq yes "$(lacks 'what ran and what it said')" "nothing is summarised"

it "a module list that expands to whitespace is the same refusal"
out="$("${ROOT}/scripts/modules-run.sh" guard "   " "${CMD}" 2>&1)"
rc=$?
assert_eq 2 "${rc}" "exit code"
assert_eq yes "$(contains 'refusing to report a sweep over nothing')" "and says why"

it "a call that is genuinely missing an argument is a usage error"
out="$("${ROOT}/scripts/modules-run.sh" guard "a b" 2>&1)"
rc=$?
assert_eq 2 "${rc}" "exit code"
assert_eq yes "$(contains 'usage:')" "the calling convention is what is wrong here"
assert_eq yes "$(lacks 'refusing to report a sweep over nothing')" "and not the corpus, which was given"

it "a blank command is a refusal, not a green sweep over work nobody did"
fixture blankcmd a
out="$(cd "${tmp}/blankcmd" && MODULES_PROBE='' "${ROOT}/scripts/modules-run.sh" guard "a" "   " 2>&1)"
rc=$?
assert_eq 2 "${rc}" "exit code"
assert_eq yes "$(contains 'refusing to report a sweep that runs nothing')" "the blank command is what is named"
assert_eq yes "$(lacks 'PASS')" "and no module is reported as having passed anything"

it "extra positional arguments are a usage error rather than silently dropped"
out="$("${ROOT}/scripts/modules-run.sh" guard "a" "true" "EXTRA" 2>&1)"
rc=$?
assert_eq 2 "${rc}" "exit code"
assert_eq yes "$(contains 'usage:')" "the call does not match the convention"

it "a module list carrying a newline sweeps the whole list, not its first line"
fixture multiline a b
MODULES_PROBE='' run_modules multiline "$(printf 'a\nb')"
assert_eq 0 "${rc}" "exit code"
assert_eq yes "$(contains 'RAN-b')" "the module past the newline ran"
assert_eq yes "$(contains '2 passed, 0 failed, 0 skipped, 0 not run (of 2 modules)')" "and the corpus was not truncated"

it "a module directory that is not there is a failure, not a silent nothing"
fixture missing a c
MODULES_PROBE='' run_modules missing "a b c"
assert_eq 1 "${rc}" "exit code"
assert_eq yes "$(contains 'FAIL     b')" "the absent module is named"
assert_eq yes "$(contains 'no such module directory')" "with the fact that explains it"
assert_eq yes "$(contains 'RAN-c')" "and the sweep carried on"

# A module that is not on disk is answered before the probe — `[ -d ]` returns
# `nodir` nine lines above the probe runs — so nothing about it can be explained
# by a probe. That is the invariant this case pins: the early return, not the
# redirect below it. Move the `-d` check after the probe and the absent module
# starts being described in the probe's words instead of its own.
it "an absent module is not explained by the previous module's probe failure"
fixture stale a:noprobe c
MODULES_PROBE='cat pkgs' run_modules stale "a b c"
assert_eq 1 "${rc}" "exit code"
assert_eq yes "$(contains 'FAIL     a')" "the module whose probe broke is failed"
assert_eq yes "$(contains 'FAIL     b')" "and so is the module that is not there"
assert_eq yes "$(contains 'no such module directory')" "which is explained by its own fact"
assert_eq yes "$(lacks 'b: the probe failed')" "and not by a probe that never ran there"
assert_eq 1 "$(printf '%s\n' "${out}" | grep -c 'cat: pkgs')" \
  "the broken probe's complaint is quoted once, under the module that produced it"

# One scratch file carries the probe's stderr for the whole sweep, and what keeps
# a module's quoted complaint its own is that `2>` truncates on open. A probe is
# free to fail without writing a word — `exit 1` on a precondition is the ordinary
# shape — and then a file still holding the previous module's stderr would be
# quoted under this module's name: a report confidently about the WRONG module,
# which is worse than one that says nothing.
#
# Known-bad, measured on a copy of the tree: turn that `2>` into `2>>` and this
# case goes red — b is handed a's `cat: pkgs: No such file or directory`. The
# explicit `: >"${probe_err}"` that used to sit above the redirect is gone with
# it; it could never fire, because the redirect on the next line had already
# truncated the file, and a second guard that cannot fail is not defence.
it "a probe that fails without a word is not explained by the previous module's stderr"
fixture quiet a:noprobe b:silentprobe c
MODULES_PROBE='[ -e quiet ] && exit 1; cat pkgs' run_modules quiet "a b c"
assert_eq 1 "${rc}" "exit code"
assert_eq yes "$(contains 'FAIL     a')" "the module whose probe broke loudly is failed"
assert_eq yes "$(contains 'FAIL     b')" "and so is the one whose probe failed silently"
assert_eq 1 "$(printf '%s\n' "${out}" | grep -c 'cat: pkgs')" \
  "the loud complaint is quoted once, under the module that produced it"
assert_eq yes "$(contains 'RAN-c')" "and the sweep carried on"

# The same class as the absent directory, one step along, and it bites hardest
# where the caller has opted into skipping: a `cd` that fails on permissions
# would be read as "does not resolve offline", reported SKIPPED, exit 0, with
# the real diagnostic never printed because a skip quotes nothing.
#
# This case is the one thing here that depends on WHO runs it: `chmod 000` does
# not stop root, which holds CAP_DAC_OVERRIDE, so as root the module enters fine
# and three assertions below fail. CI is `ubuntu-latest` and not root, and the
# failure is loud rather than a false green, so it is left as is — but somebody
# debugging under sudo should know the red is theirs and not the script's.
it "a module directory that cannot be entered is a failure even where skipping was opted into"
fixture noperm a b c
chmod 000 "${tmp}/noperm/b"
MODULES_PROBE='cat pkgs' MODULES_PROBE_FAIL=skip \
  MODULES_PROBE_SKIP_NOTE="doesn't resolve offline" \
  run_modules noperm "a b c"
chmod 755 "${tmp}/noperm/b"
assert_eq 1 "${rc}" "exit code — the caller opted into an offline skip, not into this"
assert_eq yes "$(contains 'FAIL     b')" "the module is failed"
assert_eq yes "$(lacks 'SKIPPED  b')" "and never skipped"
assert_eq yes "$(contains 'cannot be entered')" "with the fact that explains it"
assert_eq yes "$(contains 'RAN-c')" "and the sweep carried on"

# A sweep where every module skipped is a real answer — test-plugins offline
# skips every cloud and ssh plugin — so it exits 0 and must keep doing so. What
# it must NOT do is look like work: `0 passed, 0 failed, 3 skipped` with no other
# sentence is a green line over a command that ran nowhere, which is the shape
# this whole script exists to stop, one state along from NOT RUN.
it "a sweep that skipped everything says the command ran nowhere"
fixture allskip a:nopkgs b:nopkgs c:nopkgs
MODULES_PROBE='cat pkgs' MODULES_SKIP_NOTE='nothing generated here yet' \
  run_modules allskip "a b c"
assert_eq 0 "${rc}" "exit code — skipping everything is still a real answer"
assert_eq yes "$(contains '0 passed, 0 failed, 3 skipped, 0 not run (of 3 modules)')" "summary counts"
assert_eq yes "$(contains 'the command did not run anywhere')" "and the sweep says so instead of just reading green"
assert_eq yes "$(lacks 'RAN-')" "nothing executed, which is what makes the line necessary"

it "a sweep with real work does not claim the command ran nowhere"
fixture someskip a b:nopkgs c
MODULES_PROBE='cat pkgs' MODULES_SKIP_NOTE='nothing generated here yet' \
  run_modules someskip "a b c"
assert_eq 0 "${rc}" "exit code"
assert_eq yes "$(lacks 'the command did not run anywhere')" "the line is about the all-skipped sweep, not any skip"

# ★ KNOWN BOUNDARY, second of two, written down rather than faked.
#
# The duration clamp in modules-run.sh (`[ "${seconds[i]}" -ge 0 ] || seconds[i]=0`)
# has no case here, because nothing this file can do reaches the thing it guards.
# SECONDS is derived from the wall clock, so the difference goes negative only if
# the clock steps back BETWEEN the two reads in the script's own shell — observed
# twice for real: `PASS  examples/module/soul-cloud-proxmox  -1s` in a `make
# test-plugins` table, and `PASS  check-gate  -1s` in gate.sh's own summary, which
# does the same arithmetic unclamped (NIM-611). So the clamp guards something that
# happens, not something imagined — it is the SIMULATION that is out of reach, not
# the event. A subshell cannot move the parent's SECONDS, and seeding it
# from the environment shifts both reads together, so the obvious simulation is a
# case that passes whether or not the clamp is there: measured, on a copy, the
# whole file stays at 94 passed with the clamp deleted. Faking it properly needs
# libfaketime, which is a dependency a Go-free docker-free guard should not take
# for a cosmetic column.
#
# Written here so the next reader knows the clamp is unexercised rather than
# assuming the guard covers it.

it "an unusable MODULES_PROBE_FAIL is rejected rather than guessed at"
fixture badknob a
MODULES_PROBE='cat pkgs' MODULES_PROBE_FAIL=maybe run_modules badknob "a"
assert_eq 2 "${rc}" "exit code"
assert_eq yes "$(lacks 'RAN-a')" "nothing ran under a setting the script does not understand"

# ★ KNOWN BOUNDARY, asserted so it stays written down instead of being assumed.
#
# modules-run.sh ends with `case ... FAIL | "NOT RUN") exit 1`, and the second
# arm is defence-in-depth that nothing here exercises: today NOT RUN has exactly
# two sources, and neither can produce it alone. MODULES_FAIL_FAST needs a
# failure to stop on, so a FAIL is always in the same table; the interrupt path
# exits 130 from its own handler and never reaches that case at all.
#
# Measured, on a copy of the tree: deleting `| "NOT RUN"` from that case leaves
# this file green — 94 passed, 0 failed, the same score as without the mutation.
# So the arm is a rule about a state that cannot currently occur, not a guarded
# behaviour, and it must not be read as one.
#
# The case below pins the invariant that makes it unreachable rather than the
# unreachable arm. Whoever later adds a third source of NOT RUN — a per-module
# timeout, a preflight that declines to run one — will land here, and this is
# where they find out that the exit code for a NOT-RUN-only sweep has never once
# been exercised and needs its own case before it can be relied on.
it "known boundary: NOT RUN never arrives without a FAIL beside it"
fixture alone a b:fail c
MODULES_PROBE='' MODULES_FAIL_FAST=1 run_modules alone "a b c"
assert_eq yes "$(contains 'NOT RUN  c')" "the sweep does report NOT RUN"
assert_eq yes "$(contains 'FAIL     b')" "but only ever alongside the failure that caused it"
assert_eq 1 "${rc}" "so the exit code is already 1 for the FAIL, whatever NOT RUN contributes"

harness_summary
