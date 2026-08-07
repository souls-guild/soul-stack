#!/usr/bin/env bash
# ci-status-test.sh — docker-free, network-free guard tests for scripts/ci-status.sh.
#
# What is being guarded (NIM-393). ci-status.sh answers "has CI verified THIS
# commit?", and it answered it by taking the best outcome per workflow on the
# sha. For `cancelled` that is right — an evicted run is not a verdict. For
# `failure` the same rule deleted one: a red attempt followed by a green rerun
# printed VERIFIED with no trace of the red, so the tool we use to check whether
# a commit was verified was itself the thing hiding what happened to it.
#
# The pair "red attempt + green rerun on ONE sha" is the strongest evidence of a
# flake there is — between the attempts nothing changed at all — so this is not a
# cosmetic omission, it is the deletion of the most informative signal the API
# carries.
#
# Why a stub and not the network. The guard has to run inside `make check`, which
# is offline and docker-free, and it has to be deterministic — a test whose
# subject is "the report does not omit things" cannot itself depend on which runs
# GitHub still retains. So `gh` is replaced by a stub that serves pinned fixtures
# taken from the real API (run 30526743869 on sha 0324b5a2, the case in the
# ticket) and applies the caller's own `-q` filter with real jq. That last part
# matters: the jq expressions in ci-status.sh are under test rather than mocked
# out, so a filter that stops selecting `run_attempt` fails here.
#
# The RED cases at the end are the point of the whole file. Each applies a change
# a developer could plausibly make, for a plausible reason, to a copy of
# ci-status.sh, and asserts the GREEN case above it stops holding. Two conditions
# are what make them mean anything: the mutant must still PARSE, and the rest of
# its report must come out unchanged. A mutant killed by a syntax error prints no
# note either, so without those two it would satisfy "the note is gone" while
# proving only that a broken script prints nothing.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${ROOT}"

command -v jq >/dev/null 2>&1 || { echo "ci-status-test: jq is required in PATH"; exit 2; }

. "${ROOT}/scripts/e2e-cloud/test/_harness.sh"

tmp="$(mktemp -d)"
trap 'rm -rf "${tmp}"' EXIT

# --- stub `gh`. Serves fixtures from $GH_FIXTURES and runs the real jq filter.
# A path with no fixture exits non-zero, which is how the "cannot read the
# attempt" case is produced — the same way the real CLI fails on a 404.
cat >"${tmp}/gh" <<'STUB'
#!/usr/bin/env bash
set -uo pipefail
FIX="${GH_FIXTURES:?}"
case "${1:-}" in
  auth) exit 0 ;;
  repo) echo "souls-guild/soul-stack"; exit 0 ;;
  api) ;;
  *) exit 1 ;;
esac
path="${2:-}"
shift 2
filter="."
while [ "$#" -gt 0 ]; do
  case "$1" in
    -q) filter="${2:-.}"; shift 2 ;;
    *) shift ;;
  esac
done
case "${path}" in
  *"/actions/runs?head_sha="*)
    file="${FIX}/runs.json" ;;
  *"/actions/runs/"*"/attempts/"*)
    rest="${path#*/actions/runs/}"
    file="${FIX}/attempt-${rest%%/*}-${path##*/}.json" ;;
  *) exit 1 ;;
esac
[ -f "${file}" ] || exit 1
jq -r "${filter}" <"${file}"
STUB
chmod +x "${tmp}/gh"

# fixture <case> <run-attempt> <conclusion> — the runs list the API returns for
# the sha: one CI workflow, real field names and shapes.
fixture() {
  mkdir -p "${tmp}/$1"
  cat >"${tmp}/$1/runs.json" <<EOF
{"workflow_runs":[{"name":"CI","status":"completed","conclusion":"$3",
  "html_url":"https://github.com/souls-guild/soul-stack/actions/runs/30526743869",
  "id":30526743869,"run_attempt":$2}]}
EOF
}

# fixture_runs <case> — the same file, but the body of `workflow_runs` comes in
# on stdin. For the shapes one run cannot express; see the two-run case below.
fixture_runs() {
  mkdir -p "${tmp}/$1"
  { echo '{"workflow_runs":['; cat; echo ']}'; } >"${tmp}/$1/runs.json"
}

# attempt <case> <n> <conclusion> [run-id] — one earlier attempt of a run.
attempt() {
  cat >"${tmp}/$1/attempt-${4:-30526743869}-$2.json" <<EOF
{"status":"completed","conclusion":"$3","run_attempt":$2}
EOF
}

# run_ci_status <case> [script] — sets `out` (both streams, the way a human sees
# it), `stdout` alone, and `rc`. HEAD is used as the ref so `git rev-parse`
# resolves against this checkout; the stub ignores the sha.
run_ci_status() {
  stdout="$(GH="${tmp}/gh" GH_FIXTURES="${tmp}/$1" \
    "${2:-${ROOT}/scripts/ci-status.sh}" HEAD 2>"${tmp}/stderr")"
  rc=$?
  out="${stdout}$(printf '\n')$(cat "${tmp}/stderr")"
}

contains() { case "${out}" in *"$1"*) printf 'yes' ;; *) printf 'no' ;; esac; }
lacks() { case "${out}" in *"$1"*) printf 'no' ;; *) printf 'yes' ;; esac; }
on_stdout() { case "${stdout}" in *"$1"*) printf 'yes' ;; *) printf 'no' ;; esac; }

# --- the subject: green only on the rerun.
fixture green-rerun 2 success
attempt green-rerun 1 failure

it "a red attempt behind a green rerun is reported"
run_ci_status green-rerun
assert_eq yes "$(contains "attempt 1 of run 30526743869 concluded failure on 'CI'")" "the note names it"
assert_eq yes "$(contains 'flake, not a fix')" "and says what it means"

it "the verdict itself does NOT change — green on rerun IS verified"
assert_eq 0 "${rc}" "exit code"
assert_eq yes "$(contains 'VERIFIED')" "still verified"

# --- control: without it, "the note is printed" could be true unconditionally.
fixture first-try 1 success

it "a run that was green on its first attempt gets no note"
run_ci_status first-try
assert_eq 0 "${rc}" "exit code"
assert_eq yes "$(lacks 'NOTE')" "nothing to report"
assert_eq yes "$(contains 'VERIFIED')" "verified"

# --- an evicted attempt is still not a verdict: that distinction is the
# original design (NIM-339) and must survive this change.
fixture evicted 2 success
attempt evicted 1 cancelled

it "a cancelled earlier attempt is not reported as a red one"
run_ci_status evicted
assert_eq 0 "${rc}" "exit code"
assert_eq yes "$(lacks 'NOTE')" "eviction is not a failed attempt"

# --- more than one prior attempt: each is named, not just the first.
fixture twice-red 3 success
attempt twice-red 1 failure
attempt twice-red 2 timed_out

it "every prior red attempt is named, not only the first"
run_ci_status twice-red
assert_eq yes "$(contains 'attempt 1 of run 30526743869 concluded failure')" "attempt 1"
assert_eq yes "$(contains 'attempt 2 of run 30526743869 concluded timed_out')" "attempt 2"

# --- more than one RUN on the sha, which is the ordinary case here and not an
# exotic one: every push to this repo produces two runs both named `CI` (the
# push one and the pull_request one) alongside `CLA Assistant` — copied from
# sha fc8f0ebc. Two runs sharing a name is why the attempts pass keys on the run
# id and not on the workflow name: keying on the name would look correct against
# any single-run fixture and silently drop half the reruns on every real sha.
fixture_runs two-runs <<'EOF'
{"name":"CI","status":"completed","conclusion":"success",
 "html_url":"https://x/runs/30997270425","id":30997270425,"run_attempt":2},
{"name":"CI","status":"completed","conclusion":"success",
 "html_url":"https://x/runs/30997274150","id":30997274150,"run_attempt":2},
{"name":"CLA Assistant","status":"completed","conclusion":"success",
 "html_url":"https://x/runs/30997271988","id":30997271988,"run_attempt":2}
EOF
attempt two-runs 1 failure 30997270425
attempt two-runs 1 timed_out 30997274150
attempt two-runs 1 failure 30997271988

it "each rerun run is reported, including the second one sharing a workflow name"
run_ci_status two-runs
assert_eq yes "$(contains 'attempt 1 of run 30997270425 concluded failure')" "the push run"
assert_eq yes "$(contains 'attempt 1 of run 30997274150 concluded timed_out')" "the pull_request run, same name"
assert_eq yes "$(contains "concluded failure on 'CLA Assistant'")" "and the note carries the real workflow name"

# --- the blind spot must not look like an all-clear. No fixture for the prior
# attempt → the stub fails the way the real CLI does on a 404.
fixture unreadable 2 success

it "an attempt that cannot be read says so instead of reading as no-red-attempt"
run_ci_status unreadable
assert_eq yes "$(contains 'UNKNOWN')" "the gap is named"
assert_eq yes "$(lacks 'concluded failure')" "and is not dressed up as a verdict"

# --- a red attempt and an unreadable one in the same run. Reporting only the
# red one reads as "and that was all of it", which is this script's own class:
# the finding does not license dropping the gap next to it.
fixture red-and-gap 3 success
attempt red-and-gap 1 failure

it "a readable red attempt does not silence the unreadable one beside it"
run_ci_status red-and-gap
assert_eq yes "$(contains 'attempt 1 of run 30526743869 concluded failure')" "the red one"
assert_eq yes "$(contains 'attempt 2 of run 30526743869')" "the gap is still named"
assert_eq yes "$(contains 'UNKNOWN')" "and still says UNKNOWN"

# --- a red attempt is reported even when the sha is not verified either.
fixture still-red 2 failure
attempt still-red 1 failure

it "a prior red attempt is reported even when the sha itself is not verified"
run_ci_status still-red
assert_eq 1 "${rc}" "exit code — still a failure"
assert_eq yes "$(contains 'attempt 1 of run 30526743869 concluded failure')" "the note is not tied to VERIFIED"

# --- and it lands on stdout, with the verdict it belongs to. On stderr it would
# still be visible from `make check-ci` and gone from anything that keeps the
# report — including the `2>/dev/null` this very script uses on its own calls.
it "the note goes to stdout, where the verdict is"
run_ci_status green-rerun
assert_eq yes "$(on_stdout 'NOTE - attempt 1 of run 30526743869')" "on stdout"

# --- ★ RED. Each mutation below is a line of ci-status.sh rewritten into another
# line a developer could have written on purpose, and every one of them puts the
# NIM-393 bug back: a green rerun reports exactly what a green first attempt
# reports. Deleting a line instead would not do — dropping the note's `echo`
# leaves `for … do` against `done`, and bash refuses to parse the result, so the
# mutant prints no note for the same reason a truncated file prints no note.
#
# mutant <name> <old> <new> — literal substitution on a copy, held to both
# conditions before it is allowed to count as a mutation.
mutant() {
  local name="$1" old="$2" new="$3" path="${tmp}/mut-$1.sh" s
  s="$(cat "${ROOT}/scripts/ci-status.sh")"
  case "${s}" in
    *"${old}"*) ;;
    *) fail "mutation '${name}' matched nothing — the code it rewrites is gone"; return 1 ;;
  esac
  printf '%s\n' "${s//"${old}"/"${new}"}" >"${path}"
  chmod +x "${path}"
  bash -n "${path}" 2>/dev/null && return 0
  fail "mutation '${name}' does not parse — a syntax break gates nothing"
  return 1
}

# survives_mutation — the mutant must lose the note and keep everything else. The
# second half is the load-bearing one: it is what distinguishes a script that
# changed its mind from a script that fell over.
assert_bug_is_back() {
  assert_eq yes "$(lacks 'NOTE')" "the red attempt is erased again"
  assert_eq yes "$(contains 'VERIFIED')" "while the rest of the report is intact"
  assert_eq 0 "${rc}" "and the exit code is unchanged"
}

it "RED: a report that keeps quiet when the verdict is green"
if mutant quiet-on-green \
  'if [[ "${#notes[@]}" -gt 0 ]]; then' \
  'if [[ "${#notes[@]}" -gt 0 && "${verdict}" -ne 0 ]]; then'; then
  run_ci_status green-rerun "${tmp}/mut-quiet-on-green.sh"
  assert_bug_is_back
fi

it "RED: a classifier that treats a reruns-to-green failure as no failure"
if mutant rerun-green-is-not-a-failure \
  '        failure|timed_out|startup_failure|action_required)' \
  '        timed_out|startup_failure|action_required)'; then
  run_ci_status green-rerun "${tmp}/mut-rerun-green-is-not-a-failure.sh"
  assert_bug_is_back
fi

harness_summary
