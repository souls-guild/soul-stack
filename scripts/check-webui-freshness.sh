#!/usr/bin/env bash
# check-webui-freshness.sh - is the vendored UI bundle's provenance still at the
# tip of the companion branch this tree is assembling? (NIM-485, closing the open
# half of NIM-341.)
#
# WHAT IT ANSWERS, and what it deliberately does not. The question is "does a
# re-sync remain owed", not "are the embedded bytes correct". Those are different
# claims and only the first is answerable without building the companion:
#
#   check-webui         needs the companion checked out AND built - compares bytes.
#   check-webui-embed   needs neither, but compares the bundle against a fingerprint
#                       IT wrote, so a stale bundle matches its own record perfectly.
#   this check          needs neither, and asks the one question the other two
#                       structurally cannot: has the companion branch moved past the
#                       commit these bytes were vendored from?
#
# That gap is not theoretical. An unpaired web merge - web merged, core never
# re-synced - happened on two consecutive web merges (NIM-450 -> df5d4f1,
# NIM-475 -> 3989b48) and BOTH times it was found by a human reading a SHA, once
# on the release gate and once by accident. The only machine that could have seen
# it was never asked.
#
# WHY THIS IS NOW POSSIBLE WITHOUT A TOKEN. NIM-341 parked this work on "the
# companion is private, so it needs an org secret and an owner for it". The
# companion is public today - anonymous `git ls-remote` and the anonymous GitHub
# API both answer for souls-guild/soul-stack-web - so the blocker that deferred
# the check to R6 is simply gone. No secret, no npm, no cross-repo checkout.
#
# WHY POINTER EQUALITY AND NOT "did anything bundle-relevant change". The tempting
# refinement is to red only when the companion commits between here and the tip
# touch files that feed the build, so an inert commit (a test, a doc) does not
# demand a re-sync. Two reasons not to:
#
#   1. The paths do not separate. `vite.config.ts` carries both the build config
#      and the vitest config; NIM-467/468 changed the test half of that exact file.
#      No path list can tell those two edits apart, so such a list would answer
#      confidently and sometimes wrongly.
#   2. A list that must be extended when the companion grows a directory is the
#      failure mode of NIM-547 - a guard resting on hardcoded names, silently
#      emptied by a rename or an addition. Fail-closed with no list cannot be
#      emptied that way.
#
# The cost is stated honestly: this check goes red after a companion commit that
# could not have changed a single byte of the bundle - three of the last forty
# companion commits were of that kind. The remedy is `make sync-webui`, and
# when the bundle really is unchanged the whole diff is two provenance lines - but
# it is not the one keystroke it looks like: it needs the companion checked out
# next to core, on the right branch, with its node_modules installed, because
# sync-webui.sh builds but does not `npm ci`. Every message below that offers it
# says so, because a remedy someone cannot actually run is how a gate stops being
# read.
#
# WHAT THIS MUST NOT BECOME is the permanently-red gate the Makefile warns about
# next to check-webui-provenance - a red nobody can clear teaches everyone to stop
# reading every red. Two shapes of that were found in review and are closed below:
# a release train that starts in core, before the companion has opened the branch,
# must not demand a branch that does not exist yet (see the fallback around
# resolve_branch); and a tree with no bundle at all has nothing to be stale, so it
# is skipped outright - while a tag and a tree that is not a git checkout still
# report and are merely advisory.
#
# WHERE IT IS BINDING. On the branches an artefact is assembled from - release/*
# and hotfix/*. That is where both incidents landed and where the R5 demo stand is
# built from; hotfix/* is there because it reaches main without passing a release
# branch and the companion does carry hotfix branches of its own. Everywhere else -
# feature branches, PRs, tags, third-party clones - it reports and exits zero, so
# nobody's unrelated branch turns red because someone else merged web. Deciding
# that here, once, is the point of the --context mode: the Makefile asks this
# script rather than growing a second copy of the rule.
#
# Usage:
#   scripts/check-webui-freshness.sh            # the check
#   scripts/check-webui-freshness.sh --context  # `required` | `unknown` | `advisory`
#
# Environment:
#   WEBUI_REMOTE_URL      companion remote (default: derived from this repo's remotes)
#   WEBUI_FRESHNESS_SKIP  non-empty: skip even where binding - declared, not accidental
#   GITHUB_REF_NAME       preferred branch source (a CI checkout is detached); a
#                         `gh-readonly-queue/<base>/pr-…` merge-queue ref answers
#                         with <base>, which is what that run assembles
#   GITHUB_EVENT_NAME     `pull_request` switches the two below on
#   GITHUB_BASE_REF       on a pull_request, the branch being merged INTO - it
#                         outranks GITHUB_REF_NAME, which is `<n>/merge` there
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SOURCE_FILE="${ROOT}/keeper/internal/webui/WEBUI_SOURCE"
ASSETS_DIR="${ROOT}/keeper/internal/webui/assets"

# git_path - an absolute path to a file inside this worktree's git directory.
# `rev-parse --git-path` answers in one of two shapes: absolute in a linked
# worktree (it points into the main repository's .git/worktrees/<name>/), and
# relative in an ordinary checkout - relative to the git process's own directory,
# which `-C "${ROOT}"` sets to the repo root and NOT to the caller's cwd. This
# script runs from wherever make happens to be, so testing the raw answer with -f
# would resolve the relative shape against the wrong directory and report "no
# rebase in progress" for a tree that is mid-rebase.
git_path() {
	local p
	p="$(git -C "${ROOT}" rev-parse --git-path "$1" 2>/dev/null)" || return 1
	[[ -z "${p}" ]] && return 1
	case "${p}" in
	/*) printf '%s' "${p}" ;;
	*) printf '%s/%s' "${ROOT}" "${p}" ;;
	esac
}

# current_branch - the branch this tree is assembling, or empty when that cannot
# be determined. Four sources, in order of authority:
#
#   GITHUB_BASE_REF  on a pull_request GITHUB_REF_NAME is `<n>/merge`, which is
#                    not a release branch and never will be - so a PR INTO a
#                    release branch would be advisory, and the one review where
#                    an unpaired web merge is still cheap to catch is the one
#                    place this check would say nothing. A merge commit assembles
#                    its base, so the base ref is the honest answer. (Today the
#                    workflow's pull_request trigger only fires on main, so this
#                    changes nothing yet; it means the rule holds if that list
#                    ever grows, rather than the rule being right by accident.)
#   GITHUB_REF_NAME  an actions/checkout is detached, so asking git there answers
#                    for a commit and every CI run would look like a feature
#                    branch. On a push it is the branch short name. In a merge
#                    queue it is `gh-readonly-queue/<base>/pr-<n>-<sha>`, which
#                    matches neither release/* nor "" - so the queue run, the last
#                    gate before the commit lands on the release branch, would be
#                    the one run that says nothing. The base is spelled out inside
#                    that ref, so read it out rather than take the ref at face
#                    value. Recognised by shape, not by GITHUB_EVENT_NAME: the
#                    queue's temporary branch also fires ordinary `push` runs, and
#                    those carry the same ref under a different event. Matching by
#                    shape cannot weaken the gate even if a branch of that name is
#                    made by hand: such a name begins with neither `release/` nor
#                    `hotfix/`, so at face value it is always advisory - the
#                    rewrite can only turn advisory into required, never required
#                    into advisory. The base may itself contain slashes
#                    (`release/R5`), so strip the SHORTEST `/pr-*` suffix: a base
#                    that ends in a `pr-…` segment of its own still resolves to
#                    itself.
#   rebase head-name a conflicted rebase detaches HEAD, but git still records the
#                    branch being rebased. Without this, `make check` in the
#                    middle of resolving conflicts would answer "unknown" for a
#                    tree whose branch is perfectly well known. Only a value that
#                    IS a branch ref counts: a rebase started from a checkout that
#                    was already detached records the literal string `detached
#                    HEAD` in that file, and taking it at face value would answer
#                    with a branch name nobody is on - one that matches neither
#                    release/* nor "", i.e. it downgrades a binding tree to
#                    advisory. `git rebase` in a detached release worktree would
#                    then switch this check AND check-webui off together.
#   symbolic-ref     the ordinary case. NOT `rev-parse --abbrev-ref HEAD`: that
#                    prints the literal string `HEAD` on a detached checkout AND
#                    EXITS 0, so it cannot be distinguished from a branch actually
#                    named HEAD, and a `|| fallback` after it never fires. That is
#                    how a detached release worktree would silently drop to
#                    advisory - this check's own failure mode, one level up.
current_branch() {
	if [[ "${GITHUB_EVENT_NAME:-}" == "pull_request" && -n "${GITHUB_BASE_REF:-}" ]]; then
		printf '%s' "${GITHUB_BASE_REF}"
		return
	fi
	if [[ -n "${GITHUB_REF_NAME:-}" ]]; then
		local name="${GITHUB_REF_NAME}" base
		if [[ "${name}" == gh-readonly-queue/*/pr-* ]]; then
			base="${name#gh-readonly-queue/}"
			base="${base%/pr-*}"
			if [[ -n "${base}" ]]; then
				name="${base}"
			fi
		fi
		printf '%s' "${name}"
		return
	fi
	local f p ref
	for f in rebase-merge/head-name rebase-apply/head-name; do
		p="$(git_path "${f}")" || continue
		[[ -n "${p}" && -f "${p}" ]] || continue
		read -r ref <"${p}" || continue
		[[ "${ref}" == refs/heads/?* ]] || continue
		printf '%s' "${ref#refs/heads/}"
		return
	done
	git -C "${ROOT}" symbolic-ref --quiet --short HEAD 2>/dev/null || printf ''
}

# bisecting - a bisect is a deliberate walk through history, and every commit it
# lands on carries the bundle that was current THEN. Reporting each of them as a
# release-assembly failure would red every step of a bisect for a reason the
# bisector already knows, which is how a gate gets routed around.
bisecting() {
	local p
	p="$(git_path BISECT_LOG)" || return 1
	[[ -n "${p}" && -f "${p}" ]]
}

# at_tag - HEAD is exactly a tag. A released tag's bundle is SUPPOSED to be
# frozen: it is what that release shipped. Comparing it against a companion
# branch that has moved on since would red every build of every past release,
# which is a false red aimed at exactly the people least able to act on it -
# whoever is building a published version from source.
at_tag() {
	git -C "${ROOT}" describe --exact-match --tags HEAD >/dev/null 2>&1
}

# context - where the companion is mandatory. Single definition; check-webui asks
# this script rather than keeping a second copy of the rule.
#
#   required  a branch an artefact is assembled from - release/* and hotfix/*.
#             Both incidents landed on a release branch and the demo stand is
#             built from there. hotfix/* is here because it reaches main WITHOUT
#             passing a release branch and it does carry UI (the companion has
#             its own hotfix/R5-I-run-input today), so "content reaches main only
#             through a release branch" is not true and must not be relied on.
#   unknown   the branch could not be determined and this is not a tag, a bisect
#             or a rebase: an explicit `git checkout <sha>`, `git worktree add
#             <path> <sha>`. Binding, because "I could not tell where I am" must
#             not be the same exit code as "checked, in sync" - that equivalence
#             IS the defect this check exists for. One env var states the answer.
#   advisory  everything else: feature branches, main, PRs, tags, a bisect, and a
#             tree that is not a git checkout at all (a tarball build cannot be
#             assembling a release, and has no remote to ask anyway).
context() {
	git -C "${ROOT}" rev-parse --git-dir >/dev/null 2>&1 || {
		printf 'advisory'
		return
	}
	case "$(current_branch)" in
	release/* | hotfix/*) printf 'required' ;;
	"")
		if bisecting || at_tag; then printf 'advisory'; else printf 'unknown'; fi
		;;
	*) printf 'advisory' ;;
	esac
}

if [[ "${1:-}" == "--context" ]]; then
	context
	exit 0
fi

# A mistyped flag must not look like a clean run. `--contxt` silently ignored
# would make check-webui's `ctx=$(... --context)` read the empty string and fall
# back to its default - a wrong answer arrived at quietly, which is the failure
# mode this whole check exists to remove.
if [[ -n "${1:-}" ]]; then
	echo "check-webui-freshness: unknown argument '$1' (the only accepted argument is --context)" >&2
	exit 2
fi

CTX="$(context)"
REQUIRED=no
[[ "${CTX}" != "advisory" ]] && REQUIRED=yes

# verdict <message> - fail where binding, note where not. One place decides, so a
# later edit cannot make the binding case quietly lenient while the message still
# reads like a finding.
#
# Every failing path names the declared escape. Most of the conditions that reach
# here - offline, no remote configured, no provenance file - are NOT cured by
# `make sync-webui`, so a red that only ever suggested sync-webui would be a red
# with no stated way out for the person hitting it. That is the shape this check
# exists to avoid, so it must not be the shape this check has.
verdict() {
	if [[ "${REQUIRED}" == "yes" ]]; then
		echo "check-webui-freshness: FAIL - $1"
		echo "  If this cannot be resolved here (offline, no access to the companion), declare it:"
		echo "      make check WEBUI_FRESHNESS_SKIP=1"
		return 1
	fi
	echo "check-webui-freshness: NOTE - $1"
	echo "  Not a failure off a release branch: the companion moving ahead is not this"
	echo "  branch's defect. On release/* and hotfix/* the same condition is a failure."
	return 0
}

if [[ -n "${WEBUI_FRESHNESS_SKIP:-}" ]]; then
	echo "check-webui-freshness: skipped by WEBUI_FRESHNESS_SKIP - declared, not accidental"
	exit 0
fi

# No embedded bundle, nothing to be stale. A branch that carries no assets/ cannot
# serve a stale /ui, so demanding provenance there would be a red with no defect
# behind it.
#
# This exit is unconditional - it fires on release/* too - and that is safe for a
# reason outside this script rather than by its own construction: `keeper` embeds
# the bundle with `//go:embed all:assets`, and go REFUSES to build an empty or
# missing directory ("cannot embed directory assets: contains no embeddable
# files", verified). So "delete assets/ and this gate goes quiet" is not a route
# around it - the `build` tier reds first, and every branch that compiles has a
# bundle for this check to judge. If that embed ever becomes optional, this exit
# has to learn about ${CTX}.
#
# Which is also why the branches below do NOT reach here: `main` and
# hotfix/R5-I-provision-hardening each carry 31 files under assets/ and neither
# has a WEBUI_SOURCE, so they land on the provenance failure underneath - binding
# on the hotfix branch, advisory on main. That is a real inconsistency (bytes with
# no recorded origin), not an absence, and it is cleared the first time either
# branch takes a re-vendored bundle from a release.
if [[ -z "$(find "${ASSETS_DIR}" -type f -print -quit 2>/dev/null)" ]]; then
	echo "check-webui-freshness: no vendored bundle (keeper/internal/webui/assets is absent or empty) - nothing to check"
	exit 0
fi

if [[ ! -f "${SOURCE_FILE}" ]]; then
	verdict "no provenance file (keeper/internal/webui/WEBUI_SOURCE) next to a vendored bundle - run 'make sync-webui'" || exit 1
	exit 0
fi

# Trimmed, because a CRLF checkout of this file - `core.autocrlf`, or an editor
# that rewrote it - leaves a carriage return on the end of every value. That
# character is invisible in the output, so the red it produces prints the SAME
# sha on both sides of "does not match the companion tip": a gate contradicting
# itself, and the one shape of red a reader has no way to debug.
read_field() {
	sed -n "s/^$1=//p" "${SOURCE_FILE}" | sed -e 's/^[[:space:]]*//' -e 's/[[:space:]]*$//'
}
REC_COMMIT="$(read_field commit)"
REC_BRANCH="$(read_field branch)"

# A provenance file written by an older sync-webui.sh from a detached companion
# checkout records the literal `HEAD` as the branch (rev-parse --abbrev-ref prints
# that and exits 0). Asking the remote for refs/heads/HEAD would then report a
# missing branch, which is a true statement about a question nobody asked. Treat
# it as what it is: no branch recorded.
[[ "${REC_BRANCH}" == "HEAD" ]] && REC_BRANCH=""

# Said here rather than at the top of the script so it does not fire for a tree
# with no bundle at all, or on top of a declared skip - both exit above, and a
# banner about a check that then does not run is noise that teaches people to
# scroll past this check's output.
#
# The second paragraph is a limitation, not a formality. Everything below still
# binds, but ONE of the two questions cannot be asked: "were these bytes vendored
# from the branch being assembled" needs to know that branch, and here we do not.
# The check falls back to the branch recorded in the bundle, so a bundle vendored
# from a foreign companion branch and sitting at that branch's tip passes here
# while the same tree on `release/*` would fail. Detaching therefore does weaken
# this check - it cannot be closed from inside, only stated, and stating it is
# what keeps a green from meaning more than it does.
if [[ "${CTX}" == "unknown" ]]; then
	echo "check-webui-freshness: this checkout is not on a branch (detached HEAD), so whether it is"
	echo "  assembling a release cannot be determined - and an undeterminable answer is treated as"
	echo "  binding here rather than as 'not a release branch'. State the branch, or state the skip:"
	echo "      GITHUB_REF_NAME=release/<REL> make check-webui-freshness"
	echo "      make check WEBUI_FRESHNESS_SKIP=1"
	echo "  (A bisect and a tag are both recognised and stay advisory; this is the explicit-sha case.)"
	echo "  Note: staleness is still checked, against '${REC_BRANCH:-<none recorded>}' from WEBUI_SOURCE."
	echo "  What CANNOT be checked without knowing the branch is whether the bundle was vendored from"
	echo "  the branch being assembled - on release/* that mismatch is its own failure."
fi

if [[ -z "${REC_COMMIT}" ]]; then
	verdict "WEBUI_SOURCE records no commit= - nothing ties the bundle to a companion commit; run 'make sync-webui'" || exit 1
	exit 0
fi

BRANCH="$(current_branch)"

if [[ -z "${BRANCH}" && -z "${REC_BRANCH}" ]]; then
	verdict "no companion branch to compare against: this checkout is not on a branch and WEBUI_SOURCE records no branch= either" || exit 1
	exit 0
fi

# remote_candidates - the companion URLs to try, in order, one per line. Derived
# rather than hardcoded: ci.yml states owner/repo is nowhere in the tree because
# the repository is temporary and will move. The companion is this repo's URL with
# `-web` appended, which is the same relationship the Makefile already encodes as
# `../soul-stack-web`.
#
# Why a LIST and not one URL. The transport that works depends on who is asking,
# and getting it wrong produces the exact failure this check exists to prevent -
# a red that means "could not ask" while looking like a finding. A developer's
# checkout carries the ssh remote and their key answers; CI carries the https
# remote and answers anonymously because the companion is public; an automated
# session or a sandboxed step has the ssh remote but no key, and would otherwise
# get a false red on a release branch. Trying each form and then its https
# equivalent covers all three without asking anyone to configure anything.
#
# Why EVERY remote and not just `origin`. This repo's remote is named `souls`,
# not origin, so a single hardcoded name would have to be right about a
# convention this tree does not follow - the NIM-547 shape, a guard resting on a
# name. Origin is tried first where it exists (a fork-based checkout means it),
# then the rest, and the first URL that answers wins.
remote_url_forms() {
	local base="$1" hostpath
	printf '%s\n' "${base}"
	# Order matters: a URL with a scheme is matched first, so that `ssh://git@host`
	# is not also read as the scp-style shorthand below (both contain `@` and `:`).
	case "${base}" in
	ssh://*)
		hostpath="${base#ssh://}"
		hostpath="${hostpath#*@}"
		local host="${hostpath%%/*}" path="${hostpath#*/}"
		printf 'https://%s/%s\n' "${host%%:*}" "${path}"
		;;
	*://*) ;; # https/git/file: already a fetchable form, nothing to derive
	# scp-style, and NOT hardcoded to the `git@` user: a deploy account or a
	# self-hosted forge with its own user (`forgejo@`, `gitea@`) is the same
	# shorthand, and matching only `git@` would silently drop the https fallback
	# for exactly the checkouts most likely to need it.
	*@*:*)
		hostpath="${base#*@}"
		printf 'https://%s\n' "${hostpath/:/\/}"
		;;
	esac
}

remote_candidates() {
	if [[ -n "${WEBUI_REMOTE_URL:-}" ]]; then
		printf '%s\n' "${WEBUI_REMOTE_URL}"
		return 0
	fi
	local names name url any=no
	names="$(git -C "${ROOT}" remote 2>/dev/null)"
	[[ -z "${names}" ]] && return 1
	names="$({
		printf '%s\n' "${names}" | grep -x origin
		printf '%s\n' "${names}" | grep -vx origin
	})"
	while IFS= read -r name; do
		[[ -z "${name}" ]] && continue
		url="$(git -C "${ROOT}" remote get-url "${name}" 2>/dev/null)" || continue
		[[ -z "${url}" ]] && continue
		any=yes
		url="${url%/}" # a trailing slash would make `%.git` a no-op and derive `…/.git-web.git`
		remote_url_forms "${url%.git}-web.git"
	done <<<"${names}"
	[[ "${any}" == "yes" ]]
}

CANDIDATES="$(remote_candidates)" || CANDIDATES=""
if [[ -z "${CANDIDATES}" ]]; then
	verdict "this checkout has no git remote, so the companion tip cannot be resolved (tarball build?)" || exit 1
	exit 0
fi

# Fail fast instead of hanging the gate: a passphrase prompt or an unreachable
# host must not turn a check into a stall. BatchMode kills the ssh prompt,
# GIT_TERMINAL_PROMPT kills the https credential prompt, timeout kills the rest.
#
# The options are APPENDED to whatever the caller set rather than replacing it.
# `GIT_SSH_COMMAND=ssh -i ~/some.key` is an ordinary thing to have exported, and
# defaulting the whole value would mean the only environments that lose BatchMode
# are the ones that configured ssh on purpose - precisely where a prompt is
# likeliest. A caller who names BatchMode explicitly still wins: ssh takes the
# first value it is given, and theirs comes first.
resolve_tip() {
	local cmd=(git ls-remote --heads "${URL}" "refs/heads/${TARGET_BRANCH}")
	command -v timeout >/dev/null 2>&1 && cmd=(timeout 30 "${cmd[@]}")
	GIT_TERMINAL_PROMPT=0 \
		GIT_SSH_COMMAND="${GIT_SSH_COMMAND:-ssh} -o BatchMode=yes -o ConnectTimeout=10" \
		"${cmd[@]}" 2>/dev/null
}

# resolve_branch <branch> - ask each candidate URL for one branch tip, first answer
# wins. Sets TARGET_BRANCH, TIP (empty when the branch is not there), REACHED,
# TRIED, MATCHED_URL.
#
# First-answer-wins makes whichever repository replies first the authority on what
# "the tip" is, and on a fork-based checkout that is origin - a repository that
# may trail upstream, or lead it. Then the tip reported below is not the tip the
# bundle should be measured against, and `make sync-webui` cannot clear the red.
# That is why MATCHED_URL is recorded and printed with every mismatch: the answer
# is only as good as the repository it came from, so the reader is told which one
# that was and how to pin a different one (WEBUI_REMOTE_URL).
#
# Two very different things look alike from here, and telling them apart is the
# whole point of separating the exit status from the output: `ls-remote` exiting
# 0 with nothing to print means the companion ANSWERED and has no such branch,
# while a non-zero exit means it was never asked. They need opposite actions from
# whoever reads the red, so they get opposite messages.
resolve_branch() {
	TARGET_BRANCH="$1"
	TIP=""
	TRIED=""
	MATCHED_URL=""
	REACHED=no
	while IFS= read -r URL; do
		[[ -z "${URL}" ]] && continue
		TRIED="${TRIED}${TRIED:+, }${URL}"
		LS="$(resolve_tip)" || continue
		REACHED=yes
		MATCHED_URL="${URL}"
		TIP="$(printf '%s\n' "${LS}" | awk 'NR==1{print $1}')"
		[[ -n "${TIP}" ]] && break
	done <<<"${CANDIDATES}"
}

# Which branch to compare against - decided only AFTER asking the companion, and
# that order is the fix for a real defect rather than a stylistic preference.
#
# On a release branch the bundle must have been vendored from THAT branch: a
# bundle vendored off some other branch can sit at a commit that is not on this
# line at all, and comparing it against this tip would be comparing unrelated
# histories. But asserting that BEFORE resolving turned every release train that
# starts in core into a permanent red: `release/NIM-595` exists here today with a
# bundle recorded from `release/R5`, the companion has no `release/NIM-595` and
# should not, and `make sync-webui` - the cure the message offered - could not
# possibly clear it. A red with no reachable cure is exactly what makes people
# stop reading reds, so the mismatch is only a failure once the companion is known
# to actually HAVE the branch. When it does not, the branch the bundle came from
# is the honest thing to measure against, and the reader is told that is what
# happened.
if [[ "${CTX}" == "required" ]]; then
	resolve_branch "${BRANCH}"
	if [[ -n "${TIP}" && -n "${REC_BRANCH}" && "${REC_BRANCH}" != "${BRANCH}" ]]; then
		# Equal SHAs first, and this order is the whole correctness of the block.
		# A release train opens the companion's release/<next> AT the commit the
		# bundle was vendored from - that is what opening a branch means - so at
		# that moment the recorded branch says release/<prev> while the bytes ARE
		# the tip of release/<next>. Judging the label before the commit calls
		# that stale, on a bundle that is exactly current, at the busiest hour of
		# the train, and the prescribed cure would rewrite two provenance lines
		# and nothing else. A gate that reds on a correct tree teaches people to
		# pass it with the skip.
		if [[ "${TIP}" == "${REC_COMMIT}" ]]; then
			echo "check-webui-freshness: the bundle records companion branch '${REC_BRANCH}', while this tree"
			echo "  is assembling '${BRANCH}' - and both name the same commit (${REC_COMMIT:0:12}), so these"
			echo "  bytes are the tip of '${BRANCH}'. The label lags a branch that was just opened from the"
			echo "  recorded one; only the label is stale, and the next sync-webui rewrites it anyway."
		else
			echo "check-webui-freshness: FAIL - the bundle was vendored from companion branch '${REC_BRANCH}',"
			echo "  but this tree is assembling '${BRANCH}', which the companion does have. A release must"
			echo "  embed a UI built from its own branch; re-vendor with the companion on '${BRANCH}':"
			echo "      make sync-webui"
			echo "  (or, if that cannot be done here, declare it: make check WEBUI_FRESHNESS_SKIP=1)"
			exit 1
		fi
	fi
	if [[ -z "${TIP}" && "${REACHED}" == "yes" && -n "${REC_BRANCH}" && "${REC_BRANCH}" != "${BRANCH}" ]]; then
		echo "check-webui-freshness: the companion has no '${BRANCH}' yet - a release train that starts"
		echo "  in core. Comparing against '${REC_BRANCH}', the branch these bytes were vendored from."
		resolve_branch "${REC_BRANCH}"
	fi
else
	resolve_branch "${REC_BRANCH:-${BRANCH}}"
fi

if [[ -z "${TIP}" && "${REACHED}" == "yes" ]]; then
	echo "check-webui-freshness: the companion answers, but has no branch '${TARGET_BRANCH}'."
	echo "  resolved via : ${TRIED}"
	echo "  Not offline and not a missing key - the repository was reached and the branch is simply"
	echo "  not there. That happens when a release train opens in core before the companion opens"
	echo "  its own '${TARGET_BRANCH}', or when a branch was renamed. Open it in the companion and"
	echo "  re-vendor from it (make sync-webui), or declare the wait:"
	echo "      make check WEBUI_FRESHNESS_SKIP=1"
	if [[ -z "${REC_BRANCH}" ]]; then
		# Without this the red names one cause ("the train opened in core first")
		# while the actual state is a different one, and the reader goes looking
		# for a branch to open that would not help.
		echo "  Note: there is no branch to fall back to either - WEBUI_SOURCE records no branch=, which"
		echo "  is what sync-webui writes when the companion sat on a detached HEAD. These bytes cannot be"
		echo "  measured against the branch they came from, because that branch was never recorded."
	fi
	verdict "companion has no branch '${TARGET_BRANCH}'" || exit 1
	exit 0
fi

if [[ -z "${TIP}" ]]; then
	echo "check-webui-freshness: the companion could not be reached, so freshness was NOT established."
	echo "  tried : ${TRIED}"
	echo "  Offline, no credentials, or a remote this checkout cannot derive the companion from."
	echo "  This is a failure where the check binds on purpose: 'could not ask' must not share an"
	echo "  exit code with 'asked, and in sync'. Working offline is fine, but say so:"
	echo "      make check WEBUI_FRESHNESS_SKIP=1"
	echo "  If the derived URLs above are simply the wrong repository, name the right one instead:"
	echo "      WEBUI_REMOTE_URL=<companion-url> make check-webui-freshness"
	verdict "could not reach the companion to resolve branch '${TARGET_BRANCH}'" || exit 1
	exit 0
fi

if [[ "${TIP}" == "${REC_COMMIT}" ]]; then
	echo "check-webui-freshness: bundle provenance is at the tip of companion ${TARGET_BRANCH} (${REC_COMMIT:0:12})"
	exit 0
fi

# "does not match", not "is behind". `ls-remote` returns one SHA and answers only
# whether the two are equal - it says nothing about ancestry, so claiming the
# bundle is BEHIND would be an inference the check never made. Usually it is
# behind, and the paragraph says so as the usual meaning; but on a force-push, a
# rewritten branch, or a tip that came from a different repository than the bundle
# did, "behind" is simply false, and a gate that states a falsehood in its headline
# is one nobody trusts the second time.
echo "check-webui-freshness: the vendored UI bundle does not match the companion tip."
echo "  vendored from : ${REC_COMMIT}"
echo "  companion tip : ${TIP}  (${TARGET_BRANCH} via ${MATCHED_URL})"
echo ""
echo "  Usually this is the unpaired-web-merge shape: the UI repository moved and the embedded"
echo "  copy in this repository was never re-synced. Nothing here fails at runtime - keeper"
echo "  serves the older /ui happily - which is why it reads as 'the feature was not built'"
echo "  rather than as a gate failure, and why it survived two releases."
echo ""
echo "  Fix: re-vendor from a companion checked out at ${TARGET_BRANCH}:"
echo "      make sync-webui"
echo "  If the UI genuinely did not change between those commits, the rebuild is byte-identical"
echo "  and the entire diff is the two provenance lines. Re-vendoring is still the right move:"
echo "  it is what makes the recorded commit mean 'up to date' rather than 'as far as we looked'."
echo ""
echo "  Re-vendoring is NOT the fix for the other shape that prints this same line, and telling"
echo "  them apart is worth the minute it costs. This check compares the recorded commit against"
echo "  the branch TIP - it answers equality, never ancestry - so from here the two look alike:"
echo ""
echo "    - the companion moved ahead of the bundle. The unpaired-merge case above; sync-webui"
echo "      clears it."
echo "    - the bundle was vendored from a commit that was never pushed. Then the recorded SHA"
echo "      resolves on the machine that vendored it and nowhere else, this bundle cannot be"
echo "      rebuilt from public sources, and sync-webui makes it WORSE either way: from the"
echo "      branch tip it rewinds the UI to before that work, and from the local checkout it"
echo "      records the same unpublished SHA again. Push the companion branch instead."
echo ""
echo "  Ask the companion checkout, not this remote listing: an ancestor is published but is the"
echo "  tip of nothing, so 'ls-remote | grep' stays silent for BOTH cases and separates neither."
echo "      git -C <companion> fetch && git -C <companion> branch -r --contains ${REC_COMMIT:0:12}"
echo "  A remote branch in that output means the commit is published - the first case. No output"
echo "  means nothing published contains it: it never left the checkout that vendored it."
echo "  Ask the checkout that DID the vendoring. Anywhere else the object is simply absent - the"
echo "  fetch cannot bring down a commit nobody pushed - and git says so ('no such commit') with"
echo "  a non-zero status rather than staying silent. That error is not the second case: it is"
echo "  this question being asked somewhere that cannot answer it."
echo ""
echo "  If re-vendoring does not clear this, check the repository named above: the companion is"
echo "  derived from this checkout's remotes (origin first), so a fork whose -web trails or leads"
echo "  upstream is what answered. Pin the one you vendor from:"
echo "      WEBUI_REMOTE_URL=<companion-url> make check-webui-freshness"
verdict "vendored bundle does not match the tip of companion ${TARGET_BRANCH}" || exit 1
exit 0
