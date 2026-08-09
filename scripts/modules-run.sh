#!/usr/bin/env bash
# modules-run.sh — runs one command across every Go module and reports which
# modules ran, which failed, which had nothing to run, and which were never
# reached (NIM-494).
#
# Why it exists. Every per-module loop in the Makefile was written the same way:
#
#   for m in $(MODULES); do (cd $$m && go test ./...) || exit 1; done
#
# and `|| exit 1` is the NIM-373 defect one level down. `MODULES` is eight
# entries in a fixed order, so a failure in `shared` — the third — meant `sdk`,
# `keeper`, `soul`, `soul-lint` and `soulctl` were never tested, and nothing in
# the output said so. The tier printed one `FAIL test`, and `gate.sh` reported
# exactly what it was told: one failed tier, zero not run. True about tiers,
# false about modules.
#
# The cost lands where NIM-380's did — at release acceptance, on a red run read
# in a hurry. "Oh, that's shared, we know about that" is a reasonable thing to
# think, and it is compatible with the run having said nothing whatsoever about
# keeper. An unperformed check is indistinguishable from a passed one unless
# something names it, so this names it.
#
# Four states, not two. PASS and FAIL are the module's own verdict. SKIPPED
# means the probe found nothing to run there — a real answer, and the reason is
# printed, because a silent skip is the same defect in different clothes
# (NIM-481). NOT RUN means this sweep has no verdict: fail-fast was requested,
# the module was never reached, or it was still running when the run was cut
# off — the three say so in different words. It is never folded into a pass.
#
# Usage (from the repository root):
#   scripts/modules-run.sh <label> <modules> <command>
#
# Environment:
#   MODULES_PROBE       command run inside each module to decide whether it has
#                       anything to run. Empty stdout with a zero exit means
#                       SKIPPED. Set to the empty string to run every module
#                       unconditionally. Default: `go list ./...`.
#   MODULES_PROBE_FAIL  what a probe that EXITS NON-ZERO means: `fail` (default)
#                       or `skip`. This distinction is the point of having a
#                       probe at all — see the note at probe_module below.
#   MODULES_SKIP_NOTE   reason printed beside a module the probe found empty.
#   MODULES_PROBE_SKIP_NOTE
#                       reason printed beside a module skipped because its probe
#                       FAILED under MODULES_PROBE_FAIL=skip. Separate from the
#                       above on purpose: "there is nothing here" and "we could
#                       not find out what is here" are different answers, and a
#                       report that gives them the same words is back to the
#                       conflation this script exists to remove. Defaults to
#                       MODULES_SKIP_NOTE.
#   MODULES_FAIL_FAST   non-empty: stop after the first failing module. The
#                       remaining ones are reported BY NAME as NOT RUN, which is
#                       the whole difference from `|| exit 1`.
set -uo pipefail

label="${1:-}"
modules_arg="${2:-}"
command_arg="${3:-}"

# Argument COUNT, not emptiness, decides whether this is a usage error. An empty
# second argument is a caller that expanded a glob to nothing — `test-plugins`
# passes `$(wildcard examples/module/*/go.mod)` — and telling that caller it got
# the calling convention wrong sends them to read the invocation instead of
# looking for the missing plugins. Both refuse; only one of them says what
# actually happened, and this script exists to keep those apart.
if [ "$#" -ne 3 ] || [ -z "${label}" ]; then
  echo "modules-run.sh: usage: modules-run.sh <label> <modules> <command>" >&2
  exit 2
fi

# A command that is only whitespace passes an emptiness test, runs as a no-op in
# every module, and reports a green sweep over work nobody did — the same shape
# as the empty corpus below, one argument along.
if [ -z "${command_arg//[[:space:]]/}" ]; then
  echo "modules-run.sh: ${label}: blank command — refusing to report a sweep that runs nothing" >&2
  exit 2
fi

# `read -r -a` reads ONE line, so a list carrying a newline would be silently
# truncated to its first line and the sweep would report on part of the corpus
# as if it were all of it. Nothing calls it that way today; the script refuses a
# sweep over nothing, and quietly sweeping over half is the same claim.
modules_arg="${modules_arg//$'\n'/ }"
read -r -a modules <<<"${modules_arg}"
if [ "${#modules[@]}" -eq 0 ]; then
  echo "modules-run.sh: ${label}: no modules given — refusing to report a sweep over nothing" >&2
  exit 2
fi

probe="${MODULES_PROBE-go list ./...}"
probe_fail="${MODULES_PROBE_FAIL:-fail}"
skip_note="${MODULES_SKIP_NOTE:-the probe found nothing to run here}"
probe_skip_note="${MODULES_PROBE_SKIP_NOTE:-${skip_note}}"
fail_fast="${MODULES_FAIL_FAST:-}"

case "${probe_fail}" in
  fail | skip) ;;
  *)
    echo "modules-run.sh: MODULES_PROBE_FAIL must be 'fail' or 'skip', got '${probe_fail}'" >&2
    exit 2
    ;;
esac

# Pre-filled with the answer that is true before anything runs. Every module
# starts as NOT RUN and is overwritten by its own verdict, so an interrupted
# sweep reports the truth about the modules it never reached rather than
# vanishing along with them.
outcomes=()
seconds=()
reasons=()
for _ in "${modules[@]}"; do
  outcomes+=("NOT RUN")
  seconds+=("-")
  reasons+=("the sweep ended before reaching this module")
done

summarised=""

summarise() {
  [ -z "${summarised}" ] || return 0
  summarised="yes"

  local passed=0 failed=0 skipped=0 notrun=0 i took

  # The name column is sized to the names actually in this sweep rather than
  # pinned to a guessed width: $(MODULES) entries are short, but test-plugins
  # passes paths like examples/module/soul-cloud-openstack, and a table whose
  # columns tear apart on the longest row is a table people stop reading.
  local width=0
  for i in "${!modules[@]}"; do
    [ "${#modules[$i]}" -le "${width}" ] || width="${#modules[$i]}"
  done

  echo ""
  echo "modules ${label}: what ran and what it said"
  for i in "${!modules[@]}"; do
    case "${outcomes[$i]}" in
      PASS) passed=$((passed + 1)) ;;
      FAIL) failed=$((failed + 1)) ;;
      SKIPPED) skipped=$((skipped + 1)) ;;
      *) notrun=$((notrun + 1)) ;;
    esac
    took="${seconds[$i]}"
    [ "${took}" = "-" ] || took="${took}s"
    printf '  %-8s %-*s %6s   %s\n' \
      "${outcomes[$i]}" "${width}" "${modules[$i]}" "${took}" "${reasons[$i]}"
  done
  echo ""
  printf 'modules %s: %d passed, %d failed, %d skipped, %d not run (of %d modules)\n' \
    "${label}" "${passed}" "${failed}" "${skipped}" "${notrun}" "${#modules[@]}"

  if [ "${notrun}" -gt 0 ]; then
    echo "modules ${label}: a NOT RUN module is not a passed one — this sweep is silent about it."
  fi
  # Every module skipped is a real answer for some callers — test-plugins offline
  # skips every cloud and ssh plugin — so it cannot be a failure. But it is a
  # sweep that exits 0 having executed the command nowhere, and that reads as a
  # pass by the same mechanic NOT RUN gets a sentence for. Symmetry, not a verdict.
  if [ "${passed}" -eq 0 ] && [ "${failed}" -eq 0 ] && [ "${notrun}" -eq 0 ] && [ "${skipped}" -gt 0 ]; then
    echo "modules ${label}: every module was skipped — the command did not run anywhere."
  fi
}

# An interrupted sweep is exactly when the question "how far did it get?" is
# worth most: a CI job killed on its timeout otherwise leaves a log that stops
# mid-module and says nothing about the rest.
on_signal() {
  echo ""
  echo "modules ${label}: interrupted"
  summarise
  exit 130
}
trap on_signal INT TERM

# probe_module answers three ways, and keeping them apart is the reason a probe
# exists rather than just running the command everywhere:
#
#   run   — the module has packages
#   skip  — the probe succeeded and found nothing (proto/plugin before `make
#           gen` has no .go files at all, and `go test ./...` there fails with
#           "matched no packages")
#   fail  — the probe itself broke
#
# The loops this replaces collapsed the last two: `[ -z "$(go list ./... 2>/dev/null)" ]`
# is empty both when a module has no packages and when `go list` could not run —
# a broken go.mod, an unresolvable import, a module directory that is not there.
# So a module that could not even be enumerated was reported as "no Go packages"
# and skipped, and the sweep stayed green. Whether that is the right answer
# depends on the caller (test-plugins skips deliberately on an unresolvable
# probe), which is why it is a knob and not a hardcoded rule — but it is never
# the silent default.
probe_module() {
  local dir="$1"
  local out rc

  # Answered before the probe and whether or not there is one: "the module is
  # not on disk" and "the probe failed" are different facts, and a module that
  # was never there is the one case where a probe's diagnostic explains nothing.
  [ -d "${dir}" ] || { printf 'nodir'; return 0; }
  # A directory that cannot be entered is the same class one step along, and it
  # matters most under MODULES_PROBE_FAIL=skip: there the `cd` failure would be
  # read as "does not resolve offline", reported as SKIPPED and exit 0, with the
  # "permission denied" never printed because a skip quotes nothing.
  [ -x "${dir}" ] || { printf 'noaccess'; return 0; }

  [ -n "${probe}" ] || { printf 'run'; return 0; }

  # One scratch file carries the probe's stderr for every module in the sweep, so
  # what keeps a module's quoted complaint its own is that `2>` truncates on open.
  # A probe is free to fail without writing a word — `exit 1` on a precondition is
  # the ordinary shape — and an APPENDING redirect would then quote the previous
  # module's stderr under this module's name. That is a report confidently about
  # the wrong module, which is worse than one that says nothing and is the defect
  # this whole script exists to stop. Guarded, with the `2>>` known-bad measured:
  # see "a probe that fails without a word" in modules-run-test.sh.
  #
  # The redirect covers the `cd` for the same reason one step along. The checks
  # above are not a promise — the directory can go away between them and here —
  # and a `cd` failing under a narrower redirect would print loose to the sweep's
  # own stderr while leaving the file holding somebody else's complaint to quote.
  out="$( { cd "${dir}" && bash -c "${probe}"; } 2>"${probe_err}" )"
  rc=$?

  if [ "${rc}" -ne 0 ]; then
    if [ "${probe_fail}" = "skip" ]; then
      printf 'probe-skip'
    else
      printf 'fail'
    fi
    return 0
  fi
  if [ -z "${out}" ]; then
    printf 'skip'
    return 0
  fi
  printf 'run'
}

probe_err="$(mktemp "${TMPDIR:-/tmp}/modules-run-probe.XXXXXX")" || exit 2
trap 'rm -f "${probe_err}"' EXIT

stopped=""
for i in "${!modules[@]}"; do
  m="${modules[$i]}"

  if [ -n "${stopped}" ]; then
    reasons[i]="${stopped} failed and MODULES_FAIL_FAST is set"
    continue
  fi

  verdict="$(probe_module "${m}")"
  case "${verdict}" in
    nodir)
      echo "modules ${label}: ${m}: no such module directory — this module was NOT checked" >&2
      outcomes[i]="FAIL"
      reasons[i]="no such module directory"
      [ -z "${fail_fast}" ] || stopped="${m}"
      continue
      ;;
    noaccess)
      echo "modules ${label}: ${m}: the module directory cannot be entered — this module was NOT checked" >&2
      outcomes[i]="FAIL"
      reasons[i]="the module directory cannot be entered"
      [ -z "${fail_fast}" ] || stopped="${m}"
      continue
      ;;
    skip)
      echo "modules ${label}: skip ${m} (${skip_note})"
      outcomes[i]="SKIPPED"
      reasons[i]="${skip_note}"
      continue
      ;;
    probe-skip)
      echo "modules ${label}: skip ${m} (${probe_skip_note})"
      outcomes[i]="SKIPPED"
      reasons[i]="${probe_skip_note}"
      continue
      ;;
    fail)
      echo "modules ${label}: ${m}: the probe failed — this module was NOT checked:" >&2
      sed 's/^/modules '"${label}"':   /' "${probe_err}" >&2
      outcomes[i]="FAIL"
      reasons[i]="the probe (${probe}) failed — the module was never enumerated"
      [ -z "${fail_fast}" ] || stopped="${m}"
      continue
      ;;
  esac

  echo "modules ${label}: ${m}: ${command_arg}"
  started="${SECONDS}"
  # Set BEFORE the command, not after. Bash defers a trap until the foreground
  # command finishes, so a sweep killed mid-module reaches on_signal with this
  # module's verdict still unwritten — and the pre-filled reason would then say
  # "the sweep ended before reaching this module" about the module that was
  # running. On a CI timeout that is exactly the module the reader needs, and
  # pointing away from it is worse than saying nothing.
  reasons[i]="the sweep was interrupted while this module was running — it has no verdict"
  if (cd "${m}" && bash -c "${command_arg}"); then
    outcomes[i]="PASS"
  else
    outcomes[i]="FAIL"
    [ -z "${fail_fast}" ] || stopped="${m}"
  fi
  # SECONDS counts off the wall clock, which can step backwards — an NTP
  # correction, and under WSL2 a resume — so the difference can come out
  # negative. Seen twice while this script was being written: `PASS proxmox -1s`
  # in a `make test-plugins` table, and `PASS check-gate -1s` in gate.sh's own
  # summary, which is the most-read table in the repo and computed the same
  # unclamped way until NIM-611 put this clamp there too. The column is an
  # integer-second aid, not a measurement, and clamping costs it nothing;
  # printing a negative duration costs the whole table its credibility, which
  # for a reporter whose only job is to be believed is the expensive half.
  seconds[i]=$((SECONDS - started))
  [ "${seconds[i]}" -ge 0 ] || seconds[i]=0
  reasons[i]=""
done

summarise

for outcome in "${outcomes[@]}"; do
  case "${outcome}" in
    FAIL | "NOT RUN") exit 1 ;;
  esac
done
exit 0
