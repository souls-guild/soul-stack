#!/usr/bin/env bash
# ci-status.sh - answers "has CI verified THIS commit?" for a sha this script
# derives itself (NIM-339).
#
# Why it exists. The question used to be answered by reading `gh run list`, and
# that list is keyed by branch, not by commit. Three ways to get it wrong:
#
#   1. The green run belongs to an older sha, because the branch moved after it.
#      Exactly what happened on release/R5: run 30507875869 was green on
#      7af81dc7 while the branch had already reached 02cedf61 — two commits CI
#      had never seen, one of them a render change.
#   2. The run for your sha exists but was EVICTED by the next push. GitHub
#      reports `cancelled`, which is neither pass nor fail; in the list it sits
#      next to `failure`, and in a week nobody remembers it was not a verdict.
#   3. The sha was never pushed, so no run exists at all — and an absent run
#      looks the same as an unread one.
#
# All three are NIM-238 one level up: the absence of a verdict reading as a
# verdict. So this script never takes a sha from a human, never summarises a
# branch, and prints `cancelled` as its own outcome instead of folding it into a
# pass or a fail.
#
# Usage:
#   scripts/ci-status.sh            # verdict for HEAD
#   scripts/ci-status.sh <ref>      # verdict for any ref (release/R5, a sha, a tag)
#
# Exit codes:
#   0  every workflow that ran on this sha concluded success
#   1  a workflow concluded failure/timed_out/... on this sha
#   2  no verdict: sha not pushed, no run, still running, or cancelled/skipped
#   3  cannot ask (no gh, not authenticated, no GitHub remote)
set -euo pipefail

REF="${1:-HEAD}"

if ! command -v gh >/dev/null 2>&1; then
  echo "ci-status: gh CLI not found - cannot ask GitHub for a verdict." >&2
  echo "ci-status: install it, or look up the run for the exact sha by hand:" >&2
  echo "ci-status:   git rev-parse ${REF}" >&2
  exit 3
fi
if ! gh auth status >/dev/null 2>&1; then
  echo "ci-status: gh is not authenticated (gh auth login) - cannot ask for a verdict." >&2
  exit 3
fi

SHA="$(git rev-parse "${REF}" 2>/dev/null || true)"
if [[ -z "${SHA}" ]]; then
  echo "ci-status: '${REF}' does not resolve to a commit." >&2
  exit 3
fi

# owner/repo is deliberately NOT hardcoded (see the ci.yml header: this
# repository is temporary and will move). gh expands {owner}/{repo} from the
# local remote, so this keeps working after a move.
if ! REPO="$(gh repo view --json nameWithOwner -q .nameWithOwner 2>/dev/null)"; then
  echo "ci-status: no GitHub remote resolvable from this checkout - cannot ask for a verdict." >&2
  exit 3
fi

echo "ci-status: ${REPO} @ ${SHA:0:8}  $(git log -1 --format=%s "${SHA}")"

RUNS="$(gh api "repos/{owner}/{repo}/actions/runs?head_sha=${SHA}&per_page=100" \
  -q '.workflow_runs[] | "\(.name)\t\(.status)\t\(.conclusion // "-")\t\(.html_url)"' 2>/dev/null || true)"

if [[ -z "${RUNS}" ]]; then
  echo "ci-status: NO VERDICT - no workflow run exists for this sha."
  # An unpushed commit cannot have one, and saying that is more useful than
  # "no runs found".
  remote="$(git remote | head -1)"
  if [[ -n "${remote}" ]] && ! git ls-remote "${remote}" 2>/dev/null | grep -q "^${SHA}"; then
    echo "ci-status:   the sha is not a ref tip on '${remote}' either - push it first:"
    echo "ci-status:     git push ${remote} ${REF}"
  fi
  echo "ci-status:   'CI is green' about this commit would be a claim about a different one."
  exit 2
fi

while IFS=$'\t' read -r name status conclusion url; do
  printf 'ci-status:   %-10s %-10s %s\n              %s\n' "${status}" "${conclusion}" "${name}" "${url}"
done <<< "${RUNS}"

# Per workflow NAME, take the best outcome present on this sha: a workflow that
# ended success is verified even if an earlier attempt of it was evicted. Only
# when no success exists do we classify what we actually got.
verdict=0
while IFS= read -r name; do
  [[ -z "${name}" ]] && continue
  if awk -F'\t' -v n="${name}" '$1==n && $3=="success"' <<< "${RUNS}" | grep -q .; then
    continue
  fi
  line="$(awk -F'\t' -v n="${name}" '$1==n {print; exit}' <<< "${RUNS}")"
  status="$(cut -f2 <<< "${line}")"
  conclusion="$(cut -f3 <<< "${line}")"
  case "${status}:${conclusion}" in
    completed:failure|completed:timed_out|completed:action_required|completed:startup_failure)
      echo "ci-status: FAILED - '${name}' concluded ${conclusion} on this sha."
      verdict=1
      ;;
    completed:cancelled)
      echo "ci-status: NO VERDICT - '${name}' was cancelled on this sha: it neither passed nor failed."
      echo "ci-status:   Usually evicted by the next push. Re-run it, or push again and let it finish:"
      echo "ci-status:     gh run rerun <run-id>"
      [[ "${verdict}" -eq 0 ]] && verdict=2
      ;;
    completed:skipped|completed:neutral)
      echo "ci-status: NO VERDICT - '${name}' was ${conclusion} on this sha; nothing was checked."
      [[ "${verdict}" -eq 0 ]] && verdict=2
      ;;
    *)
      echo "ci-status: NO VERDICT YET - '${name}' is ${status}."
      [[ "${verdict}" -eq 0 ]] && verdict=2
      ;;
  esac
done < <(cut -f1 <<< "${RUNS}" | awk '!seen[$0]++')

case "${verdict}" in
  0) echo "ci-status: VERIFIED - every workflow on ${SHA:0:8} concluded success." ;;
  1) echo "ci-status: this sha is NOT verified - a workflow failed on it." ;;
  2) echo "ci-status: this sha is NOT verified - no completed verdict for at least one workflow." ;;
esac
exit "${verdict}"
