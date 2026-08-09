#!/usr/bin/env bash
# check-webui-freshness-test.sh - guard tests for scripts/check-webui-freshness.sh.
#
# What is being guarded, and why it needs guarding at all. The check under test
# exists to red exactly one situation - the vendored UI bundle is behind the
# companion - and everything else it can encounter must not red. That makes it a
# gate whose two failure modes are both silent:
#
#   too lenient  - the stale case exits 0 and we are back to two releases shipping
#                  a stale /ui with a green pipeline;
#   too strict   - some ordinary situation (a feature branch, an offline machine,
#                  a fresh clone) exits 1, and a gate that reds for everyone gets
#                  routed around, which is the same as not having it.
#
# The ticket's own acceptance is the pair "roll the provenance back one commit ->
# red; restore -> green". That pair is run by hand once; the cases below are that
# same pair plus its neighbours, written down so the guarantee survives the next
# edit instead of resting on someone having checked it in August.
#
# NO STUBS, AND NO NETWORK BEYOND LOOPBACK. Each case builds a throwaway core
# tree with a COPY of the real script (which resolves its root from BASH_SOURCE,
# so the copy reads the fixture's provenance and the fixture's git branch) and
# points it at a real local bare repository standing in for the companion. So the
# resolver, the branch detection, the remote-URL derivation and the ls-remote are
# the production code paths, not test doubles - a stub of "what the companion tip
# is" would leave the most breakable half of the check untested. The three
# transport cases dial 127.0.0.1 and expect to be refused; nothing leaves the
# machine.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${ROOT}"

. "${ROOT}/scripts/e2e-cloud/test/_harness.sh"

# Same reason GITHUB_REF_NAME is scrubbed per-invocation below, for the pair that
# outranks it: when this suite runs on a pull_request, GITHUB_BASE_REF holds the
# real base branch and every fixture would answer for that instead of for its own
# branch. Unset once here rather than in twelve `env -u` lists - the PR cases set
# them explicitly on the command line, which still wins.
unset GITHUB_EVENT_NAME GITHUB_BASE_REF

command -v git >/dev/null 2>&1 || {
	echo "check-webui-freshness-test: git is required" >&2
	exit 2
}

tmp="$(mktemp -d)"
trap 'rm -rf "${tmp}"' EXIT

# --- companion fixture -------------------------------------------------------
# A real history: master(C1) -> release/TEST(C2, C3) and feat/ui-thing(C4) off C1.
# release/TEST tip is C3 and C2 is "one web commit back" - the ticket's own
# non-degeneracy pair.
web="${tmp}/web-src"
git init -q "${web}"
gitw() { git -C "${web}" -c user.email=guard@example.invalid -c user.name=guard -c commit.gpgsign=false "$@"; }
echo one >"${web}/a.txt" && gitw add -A && gitw commit -q -m one
C1="$(gitw rev-parse HEAD)"
gitw checkout -q -b release/TEST
echo two >"${web}/a.txt" && gitw add -A && gitw commit -q -m two
C2="$(gitw rev-parse HEAD)"
echo three >"${web}/a.txt" && gitw add -A && gitw commit -q -m three
C3="$(gitw rev-parse HEAD)"
gitw checkout -q -b feat/ui-thing "${C1}"
echo four >"${web}/a.txt" && gitw add -A && gitw commit -q -m four
C4="$(gitw rev-parse HEAD)"

mkdir -p "${tmp}/remote"
# The core remote is ${tmp}/remote/core.git and the check derives the companion by
# appending -web, so the bare clone has to land on that derived name. If the
# derivation ever changes, these cases stop resolving and go red - which is the
# point: the derivation is load-bearing and untested elsewhere.
git clone -q --bare "${web}" "${tmp}/remote/core-web.git"
# release/OPENED is the shape of a release train opening in the companion: a new
# branch created AT the commit the current bundle was vendored from. It exists
# only in the bare clone because that is the only place the check ever looks.
git -C "${tmp}/remote/core-web.git" branch release/OPENED "${C3}"
# A branch whose name merely STARTS with the merge-queue namespace, without the
# slash that makes it that namespace - so it is an ordinary branch, not a queue
# ref. It exists so the queue-ref rewrite has a case that separates "anchored on
# the whole prefix" from "starts with those characters" - the two are
# indistinguishable on every well-formed queue ref.
git -C "${tmp}/remote/core-web.git" branch gh-readonly-queue-notes/pr-5-abc "${C3}"
# A second companion, one commit behind the first, reachable through a remote
# whose name sorts BEFORE origin. This is the fork shape: two repositories that
# both answer, disagreeing about what "the tip" is. Which of them the check
# believes is a decision, and `alpha` is what makes that decision observable -
# with only origin present every ordering produces the same answer.
git clone -q --bare "${web}" "${tmp}/remote/alpha-web.git"
git -C "${tmp}/remote/alpha-web.git" update-ref refs/heads/release/TEST "${C2}"
# A non-release base, created by name rather than inherited from whatever
# `init.defaultBranch` happens to be on this machine (it is `master` on the box
# this was written on). The cases that derive `main` assert on the name that
# reached the companion, so the name has to be the fixture's decision and not the
# environment's; `-f` keeps that true where the default already is `main`.
git -C "${tmp}/remote/core-web.git" branch -f main "${C1}"

# --- core fixture ------------------------------------------------------------
case_n=0

# make_core <core-branch> <provenance-commit|-> [provenance-branch]
# `-` means "no WEBUI_SOURCE file at all". An explicitly EMPTY third argument
# means "branch= is present but empty", which is what sync-webui.sh writes for a
# detached companion - hence `${3-$1}` and not `${3:-$1}`, which would silently
# turn that case back into the default and leave it untested. Set CORE_NO_REMOTE=1
# to build a checkout with no git remote.
make_core() {
	local cbranch="$1" pcommit="$2" pbranch="${3-$1}"
	case_n=$((case_n + 1))
	core="${tmp}/core-${case_n}"
	mkdir -p "${core}/scripts" "${core}/keeper/internal/webui"
	cp "${ROOT}/scripts/check-webui-freshness.sh" "${core}/scripts/"
	chmod +x "${core}/scripts/check-webui-freshness.sh"
	echo fixture >"${core}/README.md"
	# A vendored bundle, because a tree with no assets/ is skipped outright and
	# every case below would then pass for the wrong reason. CORE_NO_BUNDLE is
	# how the skip itself gets its own case.
	if [[ -z "${CORE_NO_BUNDLE:-}" ]]; then
		mkdir -p "${core}/keeper/internal/webui/assets"
		printf '<!doctype html><title>fixture bundle</title>\n' \
			>"${core}/keeper/internal/webui/assets/index.html"
	fi
	if [[ "${pcommit}" != "-" ]]; then
		cat >"${core}/keeper/internal/webui/WEBUI_SOURCE" <<EOF
# fixture provenance
commit=${pcommit}
describe=fixture
branch=${pbranch}
assets_sha256=0000000000000000000000000000000000000000000000000000000000000000
EOF
	fi
	git -C "${core}" init -q
	git -C "${core}" add -A
	git -C "${core}" -c user.email=guard@example.invalid -c user.name=guard \
		-c commit.gpgsign=false commit -q -m fixture
	git -C "${core}" checkout -q -b "${cbranch}"
	[[ -n "${CORE_NO_REMOTE:-}" ]] || git -C "${core}" remote add "${CORE_REMOTE_NAME:-origin}" \
		"${CORE_REMOTE_URL:-${tmp}/remote/core.git}"
}

# run_check [VAR=value]... - run the copy in the fixture.
#
# GITHUB_REF_NAME is scrubbed on purpose: this suite itself runs inside CI, where
# that variable is set to the real branch, and leaving it in place would make
# every fixture answer for release/R5 instead of for its own branch - the whole
# suite would then pass or fail together for reasons unrelated to the fixtures.
run_check() {
	out="$(env -u GITHUB_REF_NAME -u WEBUI_FRESHNESS_SKIP -u WEBUI_REMOTE_URL \
		"$@" "${core}/scripts/check-webui-freshness.sh" 2>&1)"
	rc=$?
}

contains() { case "${out}" in *"$1"*) printf 'yes' ;; *) printf 'no' ;; esac; }

# --- where the check is binding ----------------------------------------------
# The Makefile asks the script this question instead of re-deciding it, so if
# these two answers drift, check-webui's skip branch drifts with them.

it "context: a release branch is where the companion is mandatory"
make_core release/TEST "${C3}"
out="$(env -u GITHUB_REF_NAME "${core}/scripts/check-webui-freshness.sh" --context 2>&1)"
assert_eq required "${out}" "release/TEST"

it "context: a ticket branch is not"
make_core feat/NIM-000 "${C3}" release/TEST
out="$(env -u GITHUB_REF_NAME "${core}/scripts/check-webui-freshness.sh" --context 2>&1)"
assert_eq advisory "${out}" "feat/NIM-000"

# main is advisory by decision, not by oversight: content reaches main through a
# release branch or a hotfix branch, and both of those ARE binding, so by the time
# it is on main the question has already been asked where it can still be acted on.
# Making main binding as well would red every third-party clone that runs `make
# check` on the default branch, which buys nothing and costs the gate its
# credibility with the people least able to fix it. Asserted so the next reader
# sees a choice rather than a gap.
it "context: main is advisory by decision"
make_core main "${C3}" release/TEST
out="$(env -u GITHUB_REF_NAME "${core}/scripts/check-webui-freshness.sh" --context 2>&1)"
assert_eq advisory "${out}" "main"

# --- the pair the ticket demands ---------------------------------------------

it "release branch, provenance at the companion tip: green"
make_core release/TEST "${C3}"
run_check
assert_eq 0 "${rc}" "exit code"
assert_eq yes "$(contains 'at the tip of companion')" "says why it is green"

it "release branch, provenance one web commit back: RED"
make_core release/TEST "${C2}"
run_check
assert_eq 1 "${rc}" "exit code"
assert_eq yes "$(contains 'does not match the companion tip')" "names the condition"

it "the red output carries both SHAs, so a CI reader can act without a checkout"
assert_eq yes "$(contains "${C2}")" "the vendored commit"
assert_eq yes "$(contains "${C3}")" "the companion tip"
assert_eq yes "$(contains 'make sync-webui')" "the remedy"
# The tip is only as good as the repository it came from, and that repository is
# derived from this checkout's remotes rather than chosen: on a fork-based
# checkout the first answering remote can be one that trails or leads upstream,
# and then `make sync-webui` cannot clear the red at all. Naming it, and naming
# the way to pin another one, is what keeps that red diagnosable.
assert_eq yes "$(contains "${tmp}/remote/core-web.git")" "which repository answered"
assert_eq yes "$(contains 'WEBUI_REMOTE_URL=')" "and how to point at a different one"
# "behind" would be an ancestry claim, and ls-remote returns one SHA - it answers
# equality and nothing else. On a force-pushed or rewritten branch the bundle is
# not behind at all, and a headline that states it anyway is a gate caught lying.
assert_eq no "$(contains 'bundle is behind')" "no ancestry claim the check never made"

# The same line prints for a second, opposite state: a bundle vendored from a
# companion commit that was never pushed. There sync-webui cannot help - from the
# tip it REWINDS the UI past the work that was vendored, and from the local
# checkout it re-records the same unpublished SHA - so a message offering only
# sync-webui sends the one person who hits this to the one action that destroys
# their work. This is live on release/R5 today: core c93940f8 records web
# 1d48211, which is the tip of no published ref.
it "the red offers the remedy for the unpushed-companion shape too, not only re-vendoring"
assert_eq yes "$(contains 'never pushed')" "the second shape is named"
assert_eq yes "$(contains 'Push the companion branch')" "and its remedy, which is not sync-webui"
assert_eq yes "$(contains 'rewinds the UI')" "and why sync-webui is the wrong move there"
# A separator that does not separate is worse than none: an ancestor IS published
# and is the tip of nothing, so `ls-remote | grep` is silent for both shapes. The
# message must send the reader to a command that actually answers.
assert_eq yes "$(contains 'branch -r --contains')" "the command that does separate them"
assert_eq no "$(contains 'ls-remote | grep <')" "and not one that cannot"

# --- containment: everywhere else this must not red --------------------------

# `contains 'NOTE'` on its own would be an assertion that cannot fail: EVERY
# advisory verdict prints NOTE, and on an advisory context the script cannot exit
# non-zero by construction. So the pair rc=0 + NOTE catches a crash and nothing
# else. What has to be asserted is that the FINDING is still stated - an advisory
# path that quietly stopped noticing the stale bundle would pass both of those.
it "ticket branch, same stale provenance: reports but does not fail"
make_core feat/NIM-000 "${C2}" release/TEST
run_check
assert_eq 0 "${rc}" "exit code"
assert_eq yes "$(contains 'NOTE')" "still visible"
assert_eq yes "$(contains 'does not match the companion tip')" "and the finding itself is stated"
assert_eq yes "$(contains "${C3}")" "with the tip it does not match"

it "a pull_request ref is advisory (GITHUB_REF_NAME is <n>/merge, not a branch)"
make_core release/TEST "${C2}"
run_check GITHUB_REF_NAME=123/merge
assert_eq 0 "${rc}" "exit code"
assert_eq yes "$(contains 'NOTE')" "reported"
assert_eq yes "$(contains 'does not match the companion tip')" "and the finding itself is stated"

# A PR is advisory because `<n>/merge` is not a release branch - but a PR INTO a
# release branch is the last review before the merge that would land the stale
# bundle, so basing the answer on the ref name alone would switch the check off
# exactly there. The base ref is what a merge commit assembles.
it "a pull_request INTO a release branch is binding, ref name notwithstanding"
make_core feat/whatever "${C2}" release/TEST
run_check GITHUB_REF_NAME=123/merge GITHUB_EVENT_NAME=pull_request GITHUB_BASE_REF=release/TEST
assert_eq 1 "${rc}" "exit code"
assert_eq yes "$(contains 'does not match the companion tip')" "names the condition"

# The other direction of the same rule, and it needs more than `rc` to mean
# anything. Every name this case could derive - the base `main`, the ref name
# `123/merge`, the checked-out `feat/whatever` - classifies as advisory, so an
# exit code of 0 is what it returns however badly the derivation is broken, up to
# and including deleting the pull_request source outright. Provenance is left
# blank so the derived name is what reaches the companion, and the assertion is
# on the name it asked for; then deleting that source makes the check ask for
# `123/merge`, a branch the companion has never heard of, and the case notices.
it "and a pull_request into main stays advisory"
make_core feat/whatever "${C2}" ""
run_check GITHUB_REF_NAME=123/merge GITHUB_EVENT_NAME=pull_request GITHUB_BASE_REF=main
assert_eq 0 "${rc}" "exit code"
assert_eq yes "$(contains '(main via')" "the base is what it asked the companion for"
assert_eq no "$(contains 'has no branch')" "not a name the companion never heard of"

# A merge queue run is the LAST gate before the commit lands on the release
# branch, and its ref name is `gh-readonly-queue/<base>/pr-<n>-<sha>` - which is
# not release/* and not empty, so taken at face value it reads as an ordinary
# feature branch and the check goes quiet exactly there.
it "a merge-queue ref is judged by the base it is queued for, not by its own name"
make_core feat/whatever "${C2}" release/TEST
run_check GITHUB_REF_NAME=gh-readonly-queue/release/TEST/pr-485-a1b2c3d4
assert_eq 1 "${rc}" "exit code"
assert_eq yes "$(contains 'does not match the companion tip')" "names the condition"
assert_eq yes "$(contains 'release/TEST')" "and reports the base, not the queue ref"
assert_eq no "$(contains 'gh-readonly-queue')" "the queue ref itself is not what it asks the remote for"

# Same shape, same trap: an unrewritten queue ref is not release/* either, so the
# advisory outcome survives removing the rewrite entirely. What separates the two
# is which name the companion was asked about, so that is what is asserted - and
# the provenance branch stays blank, because a recorded one would supply `main`
# by itself and the rewrite could be deleted with the assertions still green.
it "and a merge-queue ref for a non-release base stays advisory"
make_core feat/whatever "${C2}" ""
run_check GITHUB_REF_NAME=gh-readonly-queue/main/pr-485-a1b2c3d4
assert_eq 0 "${rc}" "exit code"
assert_eq yes "$(contains '(main via')" "the base is what it asked the companion for"
assert_eq no "$(contains 'gh-readonly-queue')" "not the queue ref itself"

# The base can carry slashes, so the suffix strip has to be the shortest match:
# a base whose own last segment starts with `pr-` must survive intact.
it "a base that itself ends in a pr-… segment is not truncated"
make_core feat/whatever "${C2}" release/pr-cleanup
run_check GITHUB_REF_NAME=gh-readonly-queue/release/pr-cleanup/pr-7-deadbeef
assert_eq 1 "${rc}" "exit code — release/pr-cleanup is still a release branch"
assert_eq yes "$(contains 'release/pr-cleanup')" "the full base survives the strip"

# …and the OTHER direction of that strip, which the case above cannot see. It
# records a provenance branch, so with the suffix strip removed the derived name
# `release/pr-cleanup/pr-7-deadbeef` is simply absent from the companion, the
# fallback compares against the recorded branch instead, and the same red comes
# back by another road with both assertions still green. Removing the strip
# altogether killed nothing in this suite; over-stripping was the only direction
# anyone had gated.
#
# So: blank provenance, nothing to fall back to. Both the intact and the
# unstripped script exit 1 here - what tells them apart is WHICH question reached
# the companion, so the assertions are on the message and not on the code. The
# production shape being guarded is a merge-queue run, the LAST gate before the
# commit lands on the release branch, going quiet on a bundle that is stale.
it "an unstripped queue ref asks the companion the wrong question"
make_core feat/whatever "${C2}" ""
run_check GITHUB_REF_NAME=gh-readonly-queue/release/TEST/pr-1-abcdef
assert_eq 1 "${rc}" "exit code"
assert_eq yes "$(contains '(release/TEST via')" "the base is what it asked the companion for"
assert_eq no "$(contains 'has no branch')" "not a branch the companion never heard of"
assert_eq yes "$(contains 'does not match the companion tip')" "and the staleness itself is what failed"

# Only the reserved namespace is rewritten, and it is reserved by its SLASH. A
# name that merely starts with those characters is an ordinary branch and must
# reach the remote verbatim. Every well-formed queue ref survives a prefix-only
# match unchanged - the later strips are anchored on the slash too - so a name
# outside the namespace is the only input that tells the two matches apart.
#
# The provenance branch is deliberately left unrecorded, and that blank is the
# case rather than a detail of it. Record one and the derived name stops reaching
# the remote at all: off a release branch the check asks for
# `${REC_BRANCH:-${BRANCH}}`, where the recorded branch simply wins, and on a
# release branch the same value arrives by the fallback for a companion that has
# no such branch. Either way the parse result stops being observable and the case
# passes whatever the match does. Both paths are right for their own job and
# stay; a case that tests branch resolution just has to stand above them rather
# than under them.
it "a branch that only resembles the queue namespace is asked for verbatim"
make_core feat/whatever "${C2}" ""
run_check GITHUB_REF_NAME=gh-readonly-queue-notes/pr-5-abc
assert_eq 0 "${rc}" "exit code — advisory, it is not a release branch either way"
assert_eq yes "$(contains 'gh-readonly-queue-notes/pr-5-abc')" "the whole name reached the remote, untruncated"
assert_eq yes "$(contains 'does not match the companion tip')" "so the branch resolved and its tip was compared"
assert_eq no "$(contains "has no branch 'gh-readonly-queue-notes'")" "nothing asked for the truncated name"

it "declared skip is honoured even where binding"
make_core release/TEST "${C2}"
run_check WEBUI_FRESHNESS_SKIP=1
assert_eq 0 "${rc}" "exit code"
assert_eq yes "$(contains 'declared, not accidental')" "and says it was declared"

# --- CI shape ----------------------------------------------------------------

it "GITHUB_REF_NAME wins over the checked-out branch (a CI checkout is detached)"
make_core feat/local-name "${C2}" release/TEST
run_check GITHUB_REF_NAME=release/TEST
assert_eq 1 "${rc}" "exit code — CI on release/TEST is binding regardless of the local branch"

# --- fail-closed: "could not check" must never read as "checked, fine" -------
# This is the failure the ticket is about, one level up: a check that answers 0
# when it could not look is indistinguishable from a check that looked and found
# nothing, which is precisely how `check-webui: skipping` hid two web merges.

it "release branch, companion branch does not exist: RED, not silently fine"
make_core release/NOPE "${C3}"
run_check
assert_eq 1 "${rc}" "exit code"
assert_eq yes "$(contains "has no branch 'release/NOPE'")" "names what is missing"

# "the companion answered and has no such branch" and "the companion was never
# reached" need opposite actions from whoever reads the red - open the branch, or
# get on the network. A single message covering both is a message that tells a
# reader nothing, so the two are asserted to be distinguishable.
it "a companion that answers is not reported the same way as one that cannot be reached"
assert_eq no "$(contains 'could not be reached')" "the reachable case does not claim unreachable"
CORE_REMOTE_URL="${tmp}/remote/does-not-exist.git" make_core release/TEST "${C3}"
run_check
assert_eq 1 "${rc}" "exit code"
assert_eq yes "$(contains 'could not be reached')" "the unreachable case says so"
assert_eq no "$(contains 'has no branch')" "and does not claim the branch is missing"

# Several conditions state their reason ONLY through the verdict line - there is
# no detail block above them to fall back on. Asserting one of those off a
# release branch is what keeps `verdict` from degrading into a bare "NOTE" that
# tells the reader a check ran and nothing about what it found.
it "the advisory verdict carries the reason, not just the word NOTE"
CORE_NO_REMOTE=1 make_core feat/NIM-000 "${C3}" release/TEST
run_check
assert_eq 0 "${rc}" "exit code"
assert_eq yes "$(contains 'no git remote')" "the reason survives off a release branch too"

it "same unresolvable companion on a ticket branch: not a failure"
make_core feat/NIM-000 "${C3}" release/NOPE
run_check
assert_eq 0 "${rc}" "exit code"
assert_eq yes "$(contains "has no branch 'release/NOPE'")" "and still says what it could not resolve"

it "release branch, no git remote at all: RED"
CORE_NO_REMOTE=1 make_core release/TEST "${C3}"
run_check
assert_eq 1 "${rc}" "exit code"
assert_eq yes "$(contains 'no git remote')" "names the reason"

it "release branch, provenance file missing entirely: RED"
make_core release/TEST -
run_check
assert_eq 1 "${rc}" "exit code"
assert_eq yes "$(contains 'no provenance file')" "names the reason"

# --- the trap that a naive implementation walks into -------------------------
# Without the branch assertion, a bundle vendored from a feature branch would be
# compared against THAT branch's tip, match perfectly, and certify a release built
# from a UI that never went through the release branch. The fixture is exactly
# that shape: C4 really is the tip of feat/ui-thing.

it "release branch, bundle vendored from another companion branch at ITS tip: RED"
make_core release/TEST "${C4}" feat/ui-thing
run_check
assert_eq 1 "${rc}" "exit code"
assert_eq yes "$(contains "vendored from companion branch 'feat/ui-thing'")" "names the mismatch"

it "the same bundle off a release branch is nobody's defect"
make_core feat/NIM-000 "${C4}" feat/ui-thing
run_check
assert_eq 0 "${rc}" "exit code — it is at the tip of the branch it was vendored from"
assert_eq yes "$(contains 'at the tip of companion feat/ui-thing')" "and says which branch it measured"
assert_eq no "$(contains 'FAIL')" "no finding at all here, not a downgraded one"

# The branch assertion above has to fire on a WRONG branch, not on a DIFFERENT
# one. A release train opens the companion's next branch at the commit the bundle
# was already vendored from - release/OPENED is C3, which is exactly what the
# bundle records - so at that moment the recorded label says release/TEST while
# the bytes ARE the tip of the branch being assembled. Judging the label before
# the commit reds a bundle that is precisely current, at the busiest hour of the
# train, with a cure that would rewrite two provenance lines and nothing else.
it "a bundle sitting at the tip of the branch being assembled is not stale for wearing an older label"
make_core release/OPENED "${C3}" release/TEST
run_check
assert_eq 0 "${rc}" "exit code"
assert_eq yes "$(contains 'both name the same commit')" "says why the label mismatch is not a finding"
assert_eq no "$(contains 'FAIL')" "and does not red a tree that is exactly up to date"

# --- the trap the FIX walks into: a red nobody can clear ---------------------
# The mismatch assertion above, made before asking the companion, turned every
# release train that starts in core into a permanent red. `release/NIM-595` is
# live in this repository today with a bundle recorded from `release/R5`; the
# companion has no `release/NIM-595` and should not have one until the UI work
# for it starts. Demanding one produced a failure whose stated cure - re-vendor
# with the companion on that branch - cannot be carried out at all, and a red
# with no reachable cure is how a gate stops being read. So the mismatch is a
# failure only once the companion is known to HAVE the branch.

it "a release branch the companion has not opened yet falls back to the recorded branch"
make_core release/NEXT "${C3}" release/TEST
run_check
assert_eq 0 "${rc}" "exit code — this is not the mismatch case, the branch simply does not exist there"
assert_eq yes "$(contains "companion has no 'release/NEXT' yet")" "says what it could not find"
assert_eq yes "$(contains "Comparing against 'release/TEST'")" "and what it measured instead"
assert_eq no "$(contains 'FAIL')" "no failure a re-vendor could not clear"

# The fallback must not be an escape hatch: measured against the recorded branch,
# a stale bundle is still stale and still reds. Otherwise the fix above would have
# bought silence on every new release train instead of a working cure.
it "and the fallback still reds when the bundle is behind THAT branch"
make_core release/NEXT "${C2}" release/TEST
run_check
assert_eq 1 "${rc}" "exit code"
assert_eq yes "$(contains 'does not match the companion tip')" "the finding survives the fallback"
assert_eq yes "$(contains 'release/TEST')" "against the branch it was actually vendored from"

# The mismatch failure itself has to keep firing where it is legitimate - the
# companion has both branches and the bundle came from the wrong one. Without
# this pair, "resolve first" could quietly degrade into "never assert".
it "the mismatch is still a failure when the companion DOES have this branch"
make_core release/TEST "${C4}" feat/ui-thing
run_check
assert_eq 1 "${rc}" "exit code"
assert_eq yes "$(contains 'which the companion does have')" "and says on what grounds"

# --- hotfix/* assembles an artefact too --------------------------------------
# "content reaches main only through a release branch" is not true: hotfix
# branches merge to main directly, and the companion carries hotfix branches of
# its own (hotfix/R5-I-run-input today). Leaving them advisory would have left a
# path to main with the gate off.
it "context: a hotfix branch is binding"
make_core hotfix/R5-I-thing "${C3}" release/TEST
out="$(env -u GITHUB_REF_NAME "${core}/scripts/check-webui-freshness.sh" --context 2>&1)"
assert_eq required "${out}" "hotfix/R5-I-thing"

# --- three shapes that must not red: one skipped, two advisory ----------------
# Each of these is a tree that CANNOT have a stale bundle, and reding them would
# hit exactly the people with no way to act: someone building a published tag,
# someone unpacking a source tarball, or a branch that carries no UI at all. Only
# the last is skipped outright - a tag and a tree that is not a git checkout
# still report, they just exit zero.

it "a release branch with no vendored bundle at all is skipped"
CORE_NO_BUNDLE=1 make_core release/TEST "${C2}"
run_check
assert_eq 0 "${rc}" "exit code — nothing embedded, nothing to be stale"
assert_eq yes "$(contains 'no vendored bundle')" "and says why it did not look"

# But a bundle WITH no provenance is still a defect, so the skip above must key on
# the bundle rather than becoming a general-purpose way out.
it "a bundle with no provenance file is still a failure"
make_core release/TEST -
run_check
assert_eq 1 "${rc}" "exit code"
assert_eq yes "$(contains 'next to a vendored bundle')" "names the inconsistency"

it "an exact tag is advisory: a released bundle is supposed to be frozen"
make_core release/TEST "${C2}"
git -C "${core}" -c tag.gpgSign=false -c tag.forceSignAnnotated=false tag v9.9.9
git -C "${core}" checkout -q --detach v9.9.9
out="$(env -u GITHUB_REF_NAME "${core}/scripts/check-webui-freshness.sh" --context 2>&1)"
assert_eq advisory "${out}" "detached at a tag"
run_check
assert_eq 0 "${rc}" "exit code — building v9.9.9 from source must not red"

it "a tree that is not a git checkout is advisory, not unknown"
make_core release/TEST "${C2}"
rm -rf "${core}/.git"
out="$(env -u GITHUB_REF_NAME "${core}/scripts/check-webui-freshness.sh" --context 2>&1)"
assert_eq advisory "${out}" "an unpacked tarball has no branch and no remote to ask"
run_check
assert_eq 0 "${rc}" "exit code"

# --- provenance written from a detached companion ----------------------------
# `rev-parse --abbrev-ref HEAD` prints the literal `HEAD` on a detached checkout
# and exits 0, so an older sync-webui.sh recorded `branch=HEAD`. Asking the remote
# for refs/heads/HEAD answers a question nobody asked, and on a release branch it
# would red with a message about a missing branch nobody ever created.
it "a recorded branch of literal HEAD is read as 'no branch recorded'"
make_core release/TEST "${C2}" HEAD
run_check
assert_eq 1 "${rc}" "exit code — still stale against the branch being assembled"
assert_eq yes "$(contains 'does not match the companion tip')" "measured against release/TEST"
assert_eq no "$(contains "has no branch 'HEAD'")" "and never asks for refs/heads/HEAD"

# The same file arriving with CRLF endings - `core.autocrlf`, or an editor that
# rewrote it - puts a carriage return on the end of every value. Untrimmed, the
# recorded commit never equals a tip it is actually equal to, and the red prints
# the same forty characters on both sides of "does not match": a gate that
# contradicts itself in its own output, which no reader can act on. The bundle
# here IS at the tip, so the only thing that can make this case red is the
# invisible character.
it "a provenance file with CRLF endings is read, not turned into a red against itself"
make_core release/TEST "${C3}"
crlf="${core}/keeper/internal/webui/WEBUI_SOURCE"
awk '{printf "%s\r\n", $0}' "${crlf}" >"${crlf}.tmp" && mv "${crlf}.tmp" "${crlf}"
run_check
assert_eq 0 "${rc}" "exit code"
assert_eq yes "$(contains 'at the tip of companion')" "it compared the value, not the value plus a CR"

# --- every failing path names a way out --------------------------------------
# Most conditions that red here are NOT cured by `make sync-webui` - offline, no
# remote, no provenance. A red whose only suggestion is a command that cannot fix
# it is a red the reader has no move against, which is the shape this whole check
# exists to remove rather than to reproduce.
it "an unreachable companion names the declared escape, not just the re-vendor"
CORE_REMOTE_URL="${tmp}/remote/does-not-exist.git" make_core release/TEST "${C3}"
run_check
assert_eq 1 "${rc}" "exit code"
assert_eq yes "$(contains 'WEBUI_FRESHNESS_SKIP=1')" "the escape is named on a path sync-webui cannot cure"

it "so does a tree with no remote at all"
CORE_NO_REMOTE=1 make_core release/TEST "${C3}"
run_check
assert_eq yes "$(contains 'WEBUI_FRESHNESS_SKIP=1')" "the escape is named"

# --- which branch is this tree on, when git will not say plainly -------------
# `git rev-parse --abbrev-ref HEAD` answers the literal string `HEAD` on a
# detached checkout AND exits 0, so the naive implementation silently files a
# detached release worktree under "not a release branch" and turns itself off.
# The same answer feeds check-webui's skip branch, so one detached HEAD would
# disarm both. Detached is therefore its own outcome, not a synonym for advisory.

it "context: a detached checkout is unknown, not advisory"
make_core release/TEST "${C3}"
git -C "${core}" checkout -q --detach
out="$(env -u GITHUB_REF_NAME "${core}/scripts/check-webui-freshness.sh" --context 2>&1)"
assert_eq unknown "${out}" "detached HEAD"

it "a detached checkout with a stale bundle is RED, and says why it could not tell"
make_core release/TEST "${C2}"
git -C "${core}" checkout -q --detach
run_check
assert_eq 1 "${rc}" "exit code — unknown is binding, not silently fine"
assert_eq yes "$(contains 'not on a branch')" "names the situation"
assert_eq yes "$(contains 'GITHUB_REF_NAME=release/')" "and how to state the answer"
assert_eq yes "$(contains 'does not match the companion tip')" "and still reports the finding itself"
# Binding is not the same as complete, and the difference is worth stating rather
# than letting a green here read like a green on release/*. Without the branch,
# "were these bytes vendored from the branch being assembled" cannot be asked at
# all - only staleness against the recorded branch can. A detached checkout of the
# fixture from the mismatch case below exits 0 for that reason.
assert_eq yes "$(contains 'What CANNOT be checked')" "and what the detachment costs"

# The banner belongs to a check that then runs. A tree with no bundle exits before
# any of that, and announcing a limitation of a check nobody performed is how the
# output of this script becomes something people scroll past.
it "a detached checkout with nothing vendored says nothing about branches"
CORE_NO_BUNDLE=1 make_core release/TEST "${C2}"
git -C "${core}" checkout -q --detach
run_check
assert_eq 0 "${rc}" "exit code — no bundle, nothing to be stale"
assert_eq no "$(contains 'not on a branch')" "no banner for a check that did not run"

# A conflicted rebase detaches HEAD too, and running the gate while resolving
# conflicts is ordinary. git still records the branch being rebased, so this case
# must resolve to that branch rather than to "unknown" - otherwise every rebase
# in a ticket worktree would red for a tree whose branch is perfectly known.
it "a conflicted rebase resolves to the branch being rebased, not to unknown"
make_core feat/NIM-000 "${C2}" release/TEST
git -C "${core}" checkout -q -b other HEAD~0
echo conflict-a >"${core}/README.md"
git -C "${core}" -c user.email=guard@example.invalid -c user.name=guard \
	-c commit.gpgsign=false commit -q -am other
git -C "${core}" checkout -q feat/NIM-000
echo conflict-b >"${core}/README.md"
git -C "${core}" -c user.email=guard@example.invalid -c user.name=guard \
	-c commit.gpgsign=false commit -q -am mine
git -C "${core}" -c user.email=guard@example.invalid -c user.name=guard \
	-c commit.gpgsign=false rebase other >/dev/null 2>&1
assert_eq "" "$(git -C "${core}" symbolic-ref --quiet --short HEAD 2>/dev/null)" "fixture really is detached"
out="$(env -u GITHUB_REF_NAME "${core}/scripts/check-webui-freshness.sh" --context 2>&1)"
assert_eq advisory "${out}" "feat/NIM-000 mid-rebase"

# The case above passes whether or not the recorded ref is turned into a branch
# NAME - `refs/heads/feat/NIM-000` is not a release branch either, so advisory
# comes out both ways and the assertion certifies nothing about the parsing. The
# same fixture on a release branch is where the two answers differ, and it is the
# one that matters: a release worktree mid-rebase must stay binding.
it "a rebase of a release branch is still binding"
make_core release/TEST "${C2}"
git -C "${core}" checkout -q -b other
echo conflict-a >"${core}/README.md"
git -C "${core}" -c user.email=guard@example.invalid -c user.name=guard \
	-c commit.gpgsign=false commit -q -am other
git -C "${core}" checkout -q release/TEST
echo conflict-b >"${core}/README.md"
git -C "${core}" -c user.email=guard@example.invalid -c user.name=guard \
	-c commit.gpgsign=false commit -q -am mine
git -C "${core}" -c user.email=guard@example.invalid -c user.name=guard \
	-c commit.gpgsign=false rebase other >/dev/null 2>&1
assert_eq "" "$(git -C "${core}" symbolic-ref --quiet --short HEAD 2>/dev/null)" "fixture really is detached"
out="$(env -u GITHUB_REF_NAME "${core}/scripts/check-webui-freshness.sh" --context 2>&1)"
assert_eq required "${out}" "release/TEST mid-rebase"

# And the shape that reading head-name naively gets WRONG in the dangerous
# direction: a rebase started from a checkout that was already detached records
# the literal string `detached HEAD` in that file. Taken as a branch name it
# matches neither release/* nor "", so the tree drops to advisory - one `git
# rebase` in a detached release worktree switching off this check and, through
# --context, check-webui with it. There is no branch here, and saying so is the
# only honest answer.
it "a rebase started from a detached HEAD is unknown, not a branch nobody is on"
make_core release/TEST "${C2}"
git -C "${core}" checkout -q -b other
echo conflict-a >"${core}/README.md"
git -C "${core}" -c user.email=guard@example.invalid -c user.name=guard \
	-c commit.gpgsign=false commit -q -am other
git -C "${core}" checkout -q --detach release/TEST
echo conflict-b >"${core}/README.md"
git -C "${core}" -c user.email=guard@example.invalid -c user.name=guard \
	-c commit.gpgsign=false commit -q -am mine
git -C "${core}" -c user.email=guard@example.invalid -c user.name=guard \
	-c commit.gpgsign=false rebase other >/dev/null 2>&1
assert_eq "detached HEAD" "$(cat "${core}/.git/rebase-merge/head-name" 2>/dev/null)" \
	"fixture really records the literal string, which is what makes this case real"
out="$(env -u GITHUB_REF_NAME "${core}/scripts/check-webui-freshness.sh" --context 2>&1)"
assert_eq unknown "${out}" "no branch is being rebased"
run_check
assert_eq 1 "${rc}" "exit code — and it stays binding, not just labelled"

# A bisect walks through history on purpose and every commit it lands on carries
# the bundle that was current then. Reporting each step as a release failure is
# how a gate gets routed around, so a bisect is recognised rather than lumped in
# with an accidental detach.
it "a bisect is recognised and stays advisory"
make_core release/TEST "${C2}"
# `git bisect start` with no arguments does NOT detach - it only opens the
# session. Naming both endpoints is what makes git check out a midpoint, and a
# detached midpoint is the whole situation under test.
for n in 2 3; do
	echo "step-${n}" >"${core}/README.md"
	git -C "${core}" -c user.email=guard@example.invalid -c user.name=guard \
		-c commit.gpgsign=false commit -q -am "step-${n}"
done
git -C "${core}" bisect start "$(git -C "${core}" rev-parse HEAD)" \
	"$(git -C "${core}" rev-parse HEAD~2)" >/dev/null 2>&1
assert_eq "" "$(git -C "${core}" symbolic-ref --quiet --short HEAD 2>/dev/null)" "fixture really is detached"
out="$(env -u GITHUB_REF_NAME "${core}/scripts/check-webui-freshness.sh" --context 2>&1)"
assert_eq advisory "${out}" "during a bisect"
run_check
assert_eq 0 "${rc}" "exit code"
git -C "${core}" bisect reset >/dev/null 2>&1

# --- deriving the companion from whatever remotes exist ----------------------
# The real repository's remote is named `souls`, not `origin`. A resolver that
# knows one name would be right about a convention this tree does not follow -
# the NIM-547 shape, a guard resting on a hardcoded name.

it "a remote that is not named origin still resolves the companion"
CORE_REMOTE_NAME=souls make_core release/TEST "${C3}"
run_check
assert_eq 0 "${rc}" "exit code"
assert_eq yes "$(contains 'at the tip of companion')" "it really resolved, not skipped"

it "with several remotes, the one that answers wins"
CORE_REMOTE_NAME=origin CORE_REMOTE_URL="${tmp}/remote/nowhere.git" make_core release/TEST "${C2}"
git -C "${core}" remote add souls "${tmp}/remote/core.git"
run_check
assert_eq 1 "${rc}" "exit code — resolved through the second remote and found it stale"
assert_eq yes "$(contains 'does not match the companion tip')" "and reports the real finding"

# When two remotes both answer and disagree, the order is the answer, and the
# failure text tells the reader in as many words that it is "origin first". That
# sentence is a claim about behaviour, so it needs a case; without one, both the
# ordering and the stop-at-the-first-answer can be deleted with the suite still
# green, and the check would quietly start measuring the release bundle against
# a fork - the reading where `make sync-webui` cannot clear the red at all.
#
# The bundle here is vendored from origin's tip, so origin says green and alpha
# says stale: the exit code alone separates "asked origin" from "asked whoever
# sorted first", and the URL in the output says which one it was.
it "with two remotes that disagree, origin is the authority the failure text promises"
make_core release/TEST "${C3}"
git -C "${core}" remote add alpha "${tmp}/remote/alpha.git"
run_check
assert_eq 0 "${rc}" "exit code — origin answered, and origin is at the tip"
assert_eq yes "$(contains 'at the tip of companion')" "green because origin is what it asked"
assert_eq no "$(contains 'alpha-web.git')" "the remote that sorts first is not the authority"

# --- the escape hatch is overridable, so a fork is not stuck -----------------
# The override has to be asserted against a companion the derivation CANNOT
# reach, or the case passes whether or not the override is honoured at all: with
# a resolvable origin, both paths land on the same repository and deleting the
# override branch outright leaves the suite green.

it "WEBUI_REMOTE_URL overrides the derived companion URL"
CORE_REMOTE_URL="${tmp}/remote/nowhere.git" make_core release/TEST "${C3}"
run_check
assert_eq 1 "${rc}" "the derived URL alone cannot resolve"
run_check WEBUI_REMOTE_URL="${tmp}/remote/core-web.git"
assert_eq 0 "${rc}" "exit code — the override is what resolved it"
assert_eq yes "$(contains 'at the tip of companion')" "through the override"

# --- transport fallback ------------------------------------------------------
# An ssh remote with no usable key is the shape an automated session or a
# sandboxed step has, and without the https fallback it produced a red on a
# release branch that said "stale bundle" while actually meaning "could not ask".
# Both candidates are unreachable here on purpose: what is asserted is that BOTH
# were attempted, which is what the fallback consists of.
it "an scp-style ssh remote is retried over https before giving up"
CORE_REMOTE_URL="git@127.0.0.1:fixture/core.git" make_core release/TEST "${C3}"
run_check
assert_eq 1 "${rc}" "exit code — unreachable on a release branch is a failure"
assert_eq yes "$(contains 'git@127.0.0.1:fixture/core-web.git')" "the ssh candidate was tried"
assert_eq yes "$(contains 'https://127.0.0.1/fixture/core-web.git')" "and its https equivalent too"

# The scp-style form is not the only way an ssh remote is written; `git clone
# ssh://…` and an `insteadOf` rewrite both produce the URL form, and covering
# only one of them leaves the other with a red that means "could not ask".
it "an ssh:// remote with a port is retried over https too"
CORE_REMOTE_URL="ssh://git@127.0.0.1:2/fixture/core.git" make_core release/TEST "${C3}"
run_check
assert_eq 1 "${rc}" "exit code"
assert_eq yes "$(contains 'ssh://git@127.0.0.1:2/fixture/core-web.git')" "the ssh candidate was tried"
assert_eq yes "$(contains 'https://127.0.0.1/fixture/core-web.git')" "and the port is dropped for https"

# `git@` is a convention, not the syntax: a deploy account or a self-hosted forge
# writes the same scp-style shorthand with its own user. Matching only `git@`
# would drop the https fallback for exactly the checkouts most likely to need it -
# and the resulting red says "could not ask" while looking like a stale bundle.
it "the scp-style fallback is not tied to the user being named git"
CORE_REMOTE_URL="deploy@127.0.0.1:fixture/core.git" make_core release/TEST "${C3}"
run_check
assert_eq 1 "${rc}" "exit code"
assert_eq yes "$(contains 'https://127.0.0.1/fixture/core-web.git')" "the https equivalent was derived"

# A trailing slash makes `${url%.git}` a no-op, and the companion is then derived
# as `…/core.git/-web.git`: unreachable, so a release branch reds with "could not
# reach the companion" for a tree whose remote is perfectly fine. Hermetic - the
# derived path exists locally, so a correct derivation resolves and goes green.
it "a remote URL with a trailing slash still derives the companion"
CORE_REMOTE_URL="${tmp}/remote/core.git/" make_core release/TEST "${C3}"
run_check
assert_eq 0 "${rc}" "exit code — the derivation landed on the real companion"
assert_eq yes "$(contains 'at the tip of companion')" "and resolved its tip"

# BatchMode is what keeps an unreachable or key-protected host from parking the
# gate on a passphrase prompt, and `GIT_SSH_COMMAND=ssh -i …` is an ordinary
# thing to have exported. Defaulting the whole variable would drop BatchMode in
# exactly those environments - the ones that configured ssh deliberately, where a
# prompt is likeliest. A stand-in for ssh records what it was handed; both halves
# have to be in there, and neither assertion holds if the other side is dropped.
it "a caller's own GIT_SSH_COMMAND keeps its flags and still gets BatchMode"
spy="${tmp}/ssh-spy.log"
: >"${spy}"
cat >"${tmp}/ssh-spy.sh" <<EOF
#!/bin/sh
echo "\$@" >>"${spy}"
exit 255
EOF
chmod +x "${tmp}/ssh-spy.sh"
CORE_REMOTE_URL="ssh://git@127.0.0.1:2/fixture/core.git" make_core release/TEST "${C3}"
run_check GIT_SSH_COMMAND="${tmp}/ssh-spy.sh -i /nonexistent/fixture.key"
assert_eq 1 "${rc}" "exit code — the stand-in refuses, so the companion is unreachable"
assert_eq yes "$(grep -q -- '-i /nonexistent/fixture.key' "${spy}" && echo yes || echo no)" \
	"the caller's own flags survived"
assert_eq yes "$(grep -q -- 'BatchMode=yes' "${spy}" && echo yes || echo no)" \
	"and BatchMode was added rather than replaced"

# A mistyped flag must not look like a clean run: check-webui reads this script's
# stdout for the context, so an ignored argument would hand it an empty answer to
# fall back from - a wrong answer arrived at quietly, one level up.
it "an unknown argument is an error, not a silent pass"
make_core release/TEST "${C3}"
out="$(env -u GITHUB_REF_NAME "${core}/scripts/check-webui-freshness.sh" --contxt 2>&1)"
rc=$?
assert_eq 2 "${rc}" "exit code"
assert_eq yes "$(contains 'unknown argument')" "and it names what was not understood"

# --- the consumer: check-webui reads this script's answer --------------------
# The whole point of --context is that check-webui does not keep a second copy of
# the rule, which makes the coupling load-bearing: mis-reading one answer turns
# the byte-comparison gate back into the unconditional `skipping` it used to be,
# and nothing in this suite would have noticed. So the real recipe is run, from
# the real Makefile, against these fixtures - `make -C <fixture> -f <Makefile>`
# puts a fixture repository under a Makefile that resolves scripts/ and
# ../soul-stack-web relative to it.
#
# MAKEFLAGS is scrubbed because this suite itself runs as a `make check` tier,
# and an inherited jobserver would make the child warn instead of run.
run_make_webui() {
	out="$(env -u GITHUB_REF_NAME -u CI -u WEBUI_SKIP -u MAKEFLAGS -u MFLAGS -u MAKELEVEL \
		"$@" make --no-print-directory -C "${core}" -f "${ROOT}/Makefile" check-webui 2>&1)"
	rc=$?
}

# make reports a failed recipe as exit 2, not as the recipe's own 1. What these
# cases are about is red-versus-green, so they assert that rather than pinning a
# number that belongs to make's convention instead of to this gate.
gate() { if [[ "${rc}" -eq 0 ]]; then printf green; else printf red; fi; }

it "check-webui: no companion on a release branch is a failure, not a skip"
make_core release/TEST "${C3}"
assert_eq no "$([[ -e "${tmp}/soul-stack-web" ]] && printf yes || printf no)" \
	"the companion really is absent next to the fixture"
run_make_webui
assert_eq red "$(gate)" "exit status"
assert_eq yes "$(contains 'FAIL - companion')" "and it says so"

it "check-webui: WEBUI_SKIP is a declared escape, and it exits zero"
make_core release/TEST "${C3}"
run_make_webui WEBUI_SKIP=1
assert_eq green "$(gate)" "exit status"
assert_eq yes "$(contains 'skipped by WEBUI_SKIP')" "named as declared"
assert_eq no "$(contains 'FAIL')" "and the failure banner is not printed alongside a zero exit"

# The reason `unknown` exists at all: a detached release worktree used to answer
# "not a release branch", and that answer disarmed THIS gate too. One assertion,
# and it is the one that would have caught it.
it "check-webui: a detached checkout is mandatory here as well"
make_core release/TEST "${C3}"
git -C "${core}" checkout -q --detach
run_make_webui
assert_eq red "$(gate)" "exit status"
assert_eq yes "$(contains 'FAIL - companion')" "unknown is treated as mandatory, not as advisory"

it "check-webui: off a release branch a missing companion still skips quietly"
make_core feat/NIM-000 "${C3}"
run_make_webui
assert_eq green "$(gate)" "exit status"
assert_eq yes "$(contains 'not present - skipping')" "the third-party-clone case is untouched"

it "check-webui: CI is a stated exception and says which check covers it there"
make_core release/TEST "${C3}"
run_make_webui CI=true
assert_eq green "$(gate)" "exit status"
assert_eq yes "$(contains 'not present in CI')" "named as the exception"
assert_eq yes "$(contains 'check-webui-freshness')" "and points at what is binding instead"

# The context query is the single point that decides whether ANYTHING is enforced,
# so its failure default decides it too. `|| echo advisory` would answer "not a
# release branch" for a tree where the question could not be asked at all - the
# same conflation, moved one level up and now covering both gates at once.
it "check-webui: an unanswerable context is treated as unknown, not as advisory"
make_core feat/NIM-000 "${C3}"
rm -f "${core}/scripts/check-webui-freshness.sh"
run_make_webui
assert_eq red "$(gate)" "exit status — cannot tell must not default to lenient"
assert_eq yes "$(contains 'FAIL - companion')" "it falls closed"

# --- the remedy: sync-webui.sh -----------------------------------------------
# Every red above recommends `make sync-webui`, which makes that script part of
# this guard's surface: if the remedy can produce a green over a stale bundle,
# the gate is defeated through its own advice. It used to be able to. Building
# only when dist/index.html was absent meant a companion whose dist/ predated its
# own HEAD got mirrored as-is while commit= was read fresh from HEAD - old bytes,
# new SHA, and all three webui checks satisfied.
#
# npm is a PATH shim here on purpose: what is under test is whether the script
# builds at all, not what vite produces, and a shim makes "did it build" directly
# observable. Nothing installs and nothing leaves the machine.
make_web_fixture() {
	wf="${tmp}/web-fix-${case_n}"
	mkdir -p "${wf}/dist" "${wf}/bin"
	printf 'OLD\n' >"${wf}/dist/index.html"
	cat >"${wf}/bin/npm" <<'SHIM'
#!/usr/bin/env bash
[[ "$1" == run && "$2" == build ]] || exit 0
printf 'NEW\n' >"$(pwd)/dist/index.html"
SHIM
	chmod +x "${wf}/bin/npm"
	# dist/ is gitignored in the real companion, and it has to be here too: the
	# script refuses to vendor from a dirty tree, so a committed dist/ would make
	# every rebuild look like uncommitted work and the case would pass by refusing.
	printf 'dist/\nbin/\n' >"${wf}/.gitignore"
	echo src >"${wf}/app.ts"
	git -C "${wf}" init -q
	git -C "${wf}" add -A
	git -C "${wf}" -c user.email=guard@example.invalid -c user.name=guard \
		-c commit.gpgsign=false commit -q -m web
}

run_sync() {
	cp "${ROOT}/scripts/sync-webui.sh" "${core}/scripts/"
	chmod +x "${core}/scripts/sync-webui.sh"
	out="$(PATH="${wf}/bin:${PATH}" "${core}/scripts/sync-webui.sh" "${wf}" 2>&1)"
	rc=$?
}

it "sync-webui rebuilds even when dist/ already exists"
make_core release/TEST "${C2}"
make_web_fixture
run_sync
assert_eq 0 "${rc}" "exit code"
assert_eq NEW "$(cat "${core}/keeper/internal/webui/assets/index.html" 2>/dev/null)" \
	"the mirrored bytes come from a fresh build, not from whatever dist/ held"

# The provenance is what the freshness check reads, so a mirror that skipped the
# build while still stamping HEAD is precisely the green-over-stale case.
it "and the recorded commit belongs to the bytes that were just built"
assert_eq "$(git -C "${wf}" rev-parse HEAD)" \
	"$(sed -n 's/^commit=//p' "${core}/keeper/internal/webui/WEBUI_SOURCE")" "commit="

it "a detached companion records no branch rather than a branch named HEAD"
make_core release/TEST "${C2}"
make_web_fixture
git -C "${wf}" checkout -q --detach
run_sync
assert_eq 0 "${rc}" "exit code"
assert_eq "" "$(sed -n 's/^branch=//p' "${core}/keeper/internal/webui/WEBUI_SOURCE")" \
	"branch= is empty, not the literal HEAD that rev-parse --abbrev-ref prints"

it "a failing companion build leaves the vendored bundle untouched"
make_core release/TEST "${C2}"
make_web_fixture
printf 'BEFORE\n' >"${core}/keeper/internal/webui/assets/index.html"
cat >"${wf}/bin/npm" <<'SHIM'
#!/usr/bin/env bash
exit 1
SHIM
chmod +x "${wf}/bin/npm"
run_sync
assert_eq 2 "${rc}" "exit code"
assert_eq BEFORE "$(cat "${core}/keeper/internal/webui/assets/index.html")" \
	"a half-applied sync would be worse than none"
assert_eq yes "$(contains 'npm ci')" "and it says what a fresh checkout is missing"

# Every red from the freshness check ends in "run make sync-webui", and the person
# most likely to follow that advice is the one who does NOT have the companion
# checked out - the CI reader, the fresh clone. Under `set -e` the default path
# used to be computed with a `cd` into a directory that may not exist, so that
# person got `cd: no such file or directory` and rc=1 instead of the message
# written for exactly them. A remedy that misreports its own precondition sends
# people looking for a bug in the wrong repository.
it "sync-webui with no companion beside it reports that, rather than dying on a cd"
make_core release/TEST "${C2}"
cp "${ROOT}/scripts/sync-webui.sh" "${core}/scripts/"
chmod +x "${core}/scripts/sync-webui.sh"
[[ -e "${tmp}/soul-stack-web" ]] && fail "fixture precondition: ${tmp}/soul-stack-web must not exist"
out="$("${core}/scripts/sync-webui.sh" 2>&1)"
rc=$?
assert_eq 2 "${rc}" "exit code — the documented 'not found', not a shell error"
assert_eq yes "$(contains 'companion soul-stack-web not found')" "names what is missing"
assert_eq yes "$(contains '/path/to/soul-stack-web')" "and how to point it elsewhere"

# The two halves meet here: sync-webui writes an empty branch= for a detached
# companion (the case above), and the freshness check's fallback needs a recorded
# branch to fall back TO. Without this, the red names one cause - "the train
# opened in core first, open the branch in the companion" - while the actual
# state is that these bytes were never tied to a branch at all, and the reader
# goes off to open a branch that would not help.
it "a bundle from a detached companion says so, instead of blaming a branch nobody has to open"
make_core release/NOPE "${C3}" ""
run_check
assert_eq 1 "${rc}" "exit code"
assert_eq yes "$(contains "has no branch 'release/NOPE'")" "the branch it asked for"
assert_eq yes "$(contains 'records no branch=')" "and that there is nothing to fall back to"
assert_eq yes "$(contains 'detached HEAD')" "naming where an empty branch= comes from"

# --- the wiring, which nothing above touches ---------------------------------
# Every case above tests the script. None of them tests that `make` still RUNS
# it, or that its exit status still leaves the recipe. A `-` prefix on the recipe
# line, a trailing `|| true`, or dropping the tier from GATE_CHECK_TIERS silences
# this gate completely while all 130-odd assertions above stay green - and that
# is the ticket's own shape one level up: the check is fine and nobody is asking
# it. MAKEFLAGS and friends are scrubbed because this suite is itself a make
# target, and a nested make would otherwise inherit the jobserver.
#
# WEBUI_FRESHNESS_SKIP is scrubbed for a sharper reason than symmetry with
# run_check. `make check WEBUI_FRESHNESS_SKIP=1` is the offline escape this gate
# prints to the reader on five separate paths, and make EXPORTS a command-line
# variable into every recipe - so without this scrub the one command the script
# recommends reaches the nested make here, that run skips, and the suite reds. The
# declared way out of the gate would break the gate from the other side, hitting
# exactly the person who is offline and doing as they were told.

it "make check-webui-freshness propagates a red rather than swallowing it"
out="$(env -u MAKEFLAGS -u MFLAGS -u MAKELEVEL -u WEBUI_FRESHNESS_SKIP \
	GITHUB_REF_NAME=release/NO-SUCH-BRANCH-ANYWHERE \
	WEBUI_REMOTE_URL="${tmp}/remote/core-web.git" \
	make --no-print-directory check-webui-freshness 2>&1)"
rc=$?
# Non-zero, not `1`: GNU make reports a failed recipe as its own exit 2. The
# invariant being guarded is "the red leaves the recipe", and pinning make's
# number instead would make this case fail the day it changes for reasons that
# have nothing to do with this gate.
assert_eq yes "$([[ ${rc} -ne 0 ]] && echo yes || echo no)" \
	"exit status — no leading '-' on the recipe, no trailing '|| true'"
assert_eq yes "$(contains 'has no branch')" "and the script's own words reach the reader"

# Only the gate.sh argument list, not the whole `make -n` output: the `check`
# recipe also PRINTS the words "check-webui-freshness" in its closing notes, so
# grepping the transcript would report membership for a tier that had been
# deleted from the list - a guard passing on prose about itself.
it "and the tier is still part of what 'make check' runs"
out=" $(env -u MAKEFLAGS -u MFLAGS -u MAKELEVEL make -n check 2>/dev/null |
	sed -n 's|^scripts/gate\.sh check ||p') "
assert_eq yes "$(contains ' check-webui-freshness ')" "GATE_CHECK_TIERS membership"
assert_eq yes "$(contains ' check-webui-freshness-guard ')" "including this suite"

harness_summary
