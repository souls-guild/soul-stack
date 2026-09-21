#!/usr/bin/env bash
# Refuse to mirror a release the release pipeline did not produce (NIM-882).
#
# apt-publish.yml fires on `release: published` — ANY release, including one a human makes
# with `gh release create` from locally built .debs. release.yml's live-gate blocks
# goreleaser, not this workflow, so of the three ways past the gate that NIM-879 named,
# the one with a durable outward effect — packages in the apt pool, signed with the apt key
# and served to every `apt install` — was the one nothing checked. The repository ruleset
# that was supposed to close it does not exist: GitHub rulesets target branches, tags and
# pushes, never releases (RELEASING.md step (e)).
#
# Two questions, asked because neither answers the other:
#
#   1. Did the gate pass on this tag's commit, in a run that stayed clean? A release.yml run
#      for that sha in which at least one job has succeeded and none has concluded anything
#      but success. A red gate concluded `failure` and is caught. A gate that never ran
#      leaves no run at all and is caught. A gate still queued has nothing concluded and is
#      caught by the "at least one succeeded" half, which is also what stops a run that has
#      only just started from reading as a pass.
#      That last one leans on a property of release.yml rather than of this script: nothing
#      in that workflow may conclude before the gate. It is not left to chance —
#      check-release-gate.py refuses a job that can, and says this is why.
#      This is STRICTER than "the gate job went green", and knowingly so: a goreleaser that
#      fails after a green gate also refuses here, even though question 1 narrowly answered
#      yes. The alternative is naming the gate job, which breaks the day someone edits its
#      `name:` — and a release run with a failure in it is not one whose .debs should reach
#      the pool unexamined anyway. The price is a false refusal, paid by re-running the
#      release workflow against the tag, which is the documented remedy for that run already.
#   2. Did a workflow create this release? `author` is whoever's token created it, and
#      `github-actions[bot]` is obtainable only inside a run of this repository — though not
#      only inside release.yml, so this narrows the author to "some workflow here" rather
#      than to the pipeline. It is the half that catches a release a HUMAN made on an
#      already-green sha, which question 1 answers `yes` to and should.
#      It does NOT catch an asset uploaded onto a release the pipeline made — the author is
#      unchanged by `gh release upload`. That is verify-release-assets.sh's question, and
#      apt-publish.yml runs both.
#
# Anchoring on the run's jobs rather than on the gate job's name is deliberate: at the
# moment `release: published` fires, the publishing run is still in progress — goreleaser
# creates the release from inside it — so "the run concluded success" is never true here,
# and "the job named `e2e-live-gate (…)` succeeded" would break the day someone edits that
# `name:`. What is asserted is the property the name was standing in for.
#
# Every unreadable answer is a refusal: no tag, no run, an API error, an author this script
# does not recognise. The cost of a false refusal is a re-dispatch; the cost of a false pass
# is unsigned packages in a public pool, which nothing downstream re-checks.
#
# Usage: GH_TOKEN=… GITHUB_REPOSITORY=owner/repo TAG=v1.2.3 scripts/check-release-provenance.sh
#        scripts/check-release-provenance.sh --self-test   # canned API, no network
set -euo pipefail

# The identity goreleaser publishes under: release.yml hands it `secrets.GITHUB_TOKEN`, and
# a release created with that token is authored by the Actions app. Switching release.yml to
# a PAT would change this to a human login and red every publish until this name is updated
# with it — a loud failure, and the right direction for the one to be.
PIPELINE_AUTHOR="github-actions[bot]"
RELEASE_WORKFLOW="release.yml"

die() {
	printf 'check-release-provenance: %s\n' "$1" >&2
	exit 1
}

check() {
	local ref_json sha run_ids id jobs bad succeeded green report author

	# An annotated tag object points at itself; the commit is one dereference further, and
	# the runs below are indexed by the commit.
	ref_json="$(gh api "repos/${GITHUB_REPOSITORY}/git/ref/tags/${TAG}" 2>/dev/null)" ||
		die "no tag ref \`${TAG}\` in ${GITHUB_REPOSITORY}. A release whose tag does not exist is not one this pipeline made."
	sha="$(jq -r '.object.sha' <<<"$ref_json")"
	if [ "$(jq -r '.object.type' <<<"$ref_json")" = "tag" ]; then
		sha="$(gh api "repos/${GITHUB_REPOSITORY}/git/tags/${sha}" --jq '.object.sha')" ||
			die "could not dereference the annotated tag ${TAG}."
	fi

	run_ids="$(gh api "repos/${GITHUB_REPOSITORY}/actions/workflows/${RELEASE_WORKFLOW}/runs?head_sha=${sha}&per_page=100" \
		--jq '.workflow_runs[].id')" || die "could not list ${RELEASE_WORKFLOW} runs for ${sha}."
	[ -n "$run_ids" ] ||
		die "no ${RELEASE_WORKFLOW} run for ${sha} (tag ${TAG}). The live gate never ran on the commit this release claims, so there is no result to have waited for."

	green=""
	report=""
	for id in $run_ids; do
		jobs="$(gh api --paginate "repos/${GITHUB_REPOSITORY}/actions/runs/${id}/jobs?per_page=100" \
			--jq '.jobs[] | "\(.conclusion // "running")\t\(.name)"')" ||
			die "could not read the jobs of ${RELEASE_WORKFLOW} run ${id}."
		bad="$(awk -F'\t' '$1 != "success" && $1 != "running" && NF' <<<"$jobs" || true)"
		succeeded="$(awk -F'\t' '$1 == "success"' <<<"$jobs" | wc -l | tr -d ' ')"
		if [ -z "$bad" ] && [ "$succeeded" -gt 0 ]; then
			green="$id"
			break
		fi
		report="${report}
  run ${id}: ${succeeded} job(s) green, not green: ${bad:-none concluded yet}"
	done

	[ -n "$green" ] || die "no ${RELEASE_WORKFLOW} run for ${sha} (tag ${TAG}) is green.${report}
  The live gate is what makes a tag releasable; mirroring .debs for a sha it did not pass would
  put packages in the apt pool that no green tier stands behind."

	author="$(gh api "repos/${GITHUB_REPOSITORY}/releases/tags/${TAG}" --jq '.author.login')" ||
		die "no published release for tag ${TAG}."
	[ "$author" = "$PIPELINE_AUTHOR" ] ||
		die "release ${TAG} was created by \`${author}\`, not by \`${PIPELINE_AUTHOR}\`. Its assets were built somewhere this pipeline cannot see — goreleaser did not produce them, so they carry no cosign signature and no SBOM from this sha. Re-run the release workflow against the tag (\`gh workflow run ${RELEASE_WORKFLOW} --ref ${TAG}\`) instead of attaching artifacts by hand."

	printf 'check-release-provenance: %s is from %s run %s on %s, gate green, author %s\n' \
		"$TAG" "$RELEASE_WORKFLOW" "$green" "${sha:0:12}" "$author"
}

# --- self-test ---------------------------------------------------------------------------
#
# Against a canned API on PATH rather than the network, so `make check` runs it. Each case is
# the baseline with one answer changed, and the pairs that matter are the ones that used to
# collapse: a run whose gate FAILED and a run whose gate has not FINISHED are both "not
# green", but only the first is a red gate, so they assert different text.

STUB='#!/usr/bin/env bash
set -uo pipefail
path=""; jqexpr=""
while [ $# -gt 0 ]; do
  case "$1" in
    api|--paginate) ;;
    --jq) shift; jqexpr="$1" ;;
    *) if [ -z "$path" ]; then path="$1"; fi ;;
  esac
  shift
done
file=""
case "$path" in
  */git/ref/tags/*)            file=ref.json ;;
  */git/tags/*)                file=tagobj.json ;;
  # Keyed by the sha the caller asked about, not a single file: a stub that answers the same
  # runs whatever it is asked cannot tell a dereferenced annotated tag from an undereferenced
  # one, and the deref can then be deleted with every case still green.
  # Keyed by BOTH the workflow asked about and the sha. The workflow name is the constant
  # deciding whose greenness is trusted, and a stub that answered any workflow would let
  # RELEASE_WORKFLOW be changed to something ungated with every case still green.
  */actions/workflows/release.yml/runs*) sha="${path#*head_sha=}"; file="runs-${sha%%&*}.json" ;;
  */actions/runs/*/jobs*)      id="${path#*/actions/runs/}"; file="jobs-${id%%/*}.json" ;;
  */releases/tags/*)           file=release.json ;;
esac
if [ -z "$file" ] || [ ! -f "$STUB_DIR/$file" ]; then
  echo "stub: no canned answer for $path" >&2
  exit 1
fi
if [ -n "$jqexpr" ]; then jq -r "$jqexpr" <"$STUB_DIR/$file"; else cat "$STUB_DIR/$file"; fi
'

self_test() {
	local me tmp pass=0 fail=0 SHA=c0ffeec0ffeec0ffee
	me="$(cd "$(dirname "$0")" && pwd)/$(basename "$0")"
	tmp="$(mktemp -d)"
	# Expanded now, not at trap time: `tmp` is local and out of scope once this returns.
	trap "rm -rf '$tmp'" EXIT
	mkdir -p "$tmp/bin"
	printf '%s' "$STUB" >"$tmp/bin/gh"
	chmod +x "$tmp/bin/gh"

	baseline() {
		rm -rf "$tmp/canned"
		mkdir -p "$tmp/canned"
		printf '{"object":{"sha":"%s","type":"commit"}}' "$SHA" >"$tmp/canned/ref.json"
		printf '{"workflow_runs":[{"id":11}]}' >"$tmp/canned/runs-$SHA.json"
		# The shape at the moment the release is published: the gate is done, the publisher
		# that created the release is still running. "The run concluded success" is false here.
		printf '{"jobs":[{"name":"e2e-live-gate","conclusion":"success"},{"name":"goreleaser","conclusion":null}]}' >"$tmp/canned/jobs-11.json"
		printf '{"author":{"login":"github-actions[bot]"}}' >"$tmp/canned/release.json"
	}

	run_case() { # <label> <expected exit> <expected text>
		local label="$1" want_rc="$2" want="$3" out rc
		set +e
		out="$(PATH="$tmp/bin:$PATH" STUB_DIR="$tmp/canned" GITHUB_REPOSITORY=souls-guild/soul-stack TAG=v1.2.3 "$me" 2>&1)"
		rc=$?
		set -e
		if [ "$rc" != "$want_rc" ] || [[ "$out" != *"$want"* ]]; then
			printf 'self-test: %s: wanted exit %s containing %s, got exit %s:\n%s\n' \
				"$label" "$want_rc" "$want" "$rc" "$out"
			fail=$((fail + 1))
		else
			pass=$((pass + 1))
		fi
	}

	baseline
	run_case "a gated release publishes" 0 "gate green"

	baseline
	printf '{"object":{"sha":"7a97a97a9","type":"tag"}}' >"$tmp/canned/ref.json"
	printf '{"object":{"sha":"%s"}}' "$SHA" >"$tmp/canned/tagobj.json"
	run_case "an annotated tag is dereferenced to its commit" 0 "gate green"

	baseline
	printf '{"workflow_runs":[{"id":9},{"id":11}]}' >"$tmp/canned/runs-$SHA.json"
	printf '{"jobs":[{"name":"e2e-live-gate","conclusion":"failure"}]}' >"$tmp/canned/jobs-9.json"
	run_case "a green re-dispatch after a red run counts" 0 "gate green"

	baseline
	printf '{"workflow_runs":[]}' >"$tmp/canned/runs-$SHA.json"
	run_case "a tag the gate never ran on" 1 "The live gate never ran"

	baseline
	printf '{"jobs":[{"name":"e2e-live-gate","conclusion":"failure"},{"name":"goreleaser","conclusion":"skipped"}]}' >"$tmp/canned/jobs-11.json"
	run_case "a red gate" 1 "not green: failure"

	baseline
	printf '{"jobs":[{"name":"e2e-live-gate","conclusion":null}]}' >"$tmp/canned/jobs-11.json"
	run_case "a gate still queued is not a pass" 1 "none concluded yet"

	# Isolates the "nothing failed" half from the "something succeeded" half. Without it both
	# cases above still refuse on the second half alone, and deleting the first is silent —
	# which is what happened: this case exists because a mutation dropping `bad` passed.
	baseline
	printf '{"jobs":[{"name":"e2e-live-gate","conclusion":"success"},{"name":"goreleaser","conclusion":"failure"}]}' >"$tmp/canned/jobs-11.json"
	run_case "a failure elsewhere in the run still refuses" 1 "not green: failure"

	# `skipped` and `cancelled` each need a case whose ONLY blemish they are. Beside a
	# `failure` they prove nothing: narrowing the awk to `$1 == "failure"` would still refuse,
	# and a SKIPPED gate — what a job-level `if:` produces, the very defeat the python guard
	# catches one level up — would start reading as green.
	baseline
	printf '{"jobs":[{"name":"e2e-live-gate","conclusion":"skipped"},{"name":"goreleaser","conclusion":"success"}]}' >"$tmp/canned/jobs-11.json"
	run_case "a SKIPPED gate is not a passed gate" 1 "not green: skipped"

	baseline
	printf '{"jobs":[{"name":"e2e-live-gate","conclusion":"cancelled"},{"name":"goreleaser","conclusion":"success"}]}' >"$tmp/canned/jobs-11.json"
	run_case "a cancelled gate is not a passed gate" 1 "not green: cancelled"

	baseline
	printf '{"object":{"sha":"7a97a97a9","type":"tag"}}' >"$tmp/canned/ref.json"
	run_case "an annotated tag that cannot be dereferenced" 1 "could not dereference"

	baseline
	rm "$tmp/canned/jobs-11.json"
	run_case "the jobs of a run cannot be read" 1 "could not read the jobs"

	# The one a `needs:` can never catch: the gate is green on this sha and a human attached
	# .debs built somewhere else.
	baseline
	printf '{"author":{"login":"co-cy"}}' >"$tmp/canned/release.json"
	run_case "hand-built artifacts on an already-green sha" 1 "was created by \`co-cy\`"

	baseline
	rm "$tmp/canned/ref.json"
	run_case "a release whose tag does not exist" 1 "no tag ref"

	baseline
	rm "$tmp/canned/release.json"
	run_case "no published release for the tag" 1 "no published release"

	baseline
	printf 'not json' >"$tmp/canned/runs-$SHA.json"
	run_case "an unreadable API answer is a refusal, not a pass" 1 "could not list"

	if [ "$fail" -gt 0 ]; then
		printf 'check-release-provenance: self-test FAILED (%s case(s))\n' "$fail"
		return 1
	fi
	printf 'check-release-provenance: self-test OK (%s cases)\n' "$pass"
}

if [ "${1:-}" = "--self-test" ]; then
	self_test
	exit $?
fi

: "${GITHUB_REPOSITORY:?GITHUB_REPOSITORY is required}"
: "${TAG:?TAG is required}"
check
