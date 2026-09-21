#!/usr/bin/env bash
# check-approle-template.sh — holds the shipped Vault AppRole role template to a
# periodic token.
#
# Why this exists (NIM-429). The role we tell operators to create carried
# `token_ttl=1h token_max_ttl=24h`. Keeper logs in through AppRole exactly once,
# at startup, and TokenRenewer afterwards only calls renew-self — which extends a
# token but cannot carry it past its maximum lifetime. That template therefore
# took Vault away from a perfectly healthy Keeper after a day of uptime, and the
# instances that got there had to be restarted.
#
# Why a grep guard and not a test. Nothing in this repo executes the template.
# No provisioning path creates a Keeper AppRole role at all — dev/provision.sh and
# all three e2e harnesses run Vault on a root token — so the only thing that ever
# runs these snippets is a human copying them out of the docs. No unit test, no
# build, no integration or live tier can observe a regression here: the template
# can rot all the way back to the 24-hour cliff and every tier stays green. A
# grep is the only mechanism that sees it at all.
#
# Two assertions, deliberately pointing in both directions. A guard keyed only to
# the new spelling would catch a future regression and stay blind to a site that
# never got fixed in the first place:
#
#   1. Every known role-creation snippet carries `token_period=`,
#      `token_max_ttl=0` and `token_explicit_max_ttl=0`.
#   2. The literal `token_ttl=` survives nowhere in the tree, except the CHANGELOG,
#      which quotes the old template on purpose in order to describe the bug. This
#      is the half that catches a NEW snippet pasted from an old copy.
#
# Of the three fields in assertion 1 only two can actually break the promise, and
# they are not the obvious ones. `token_period=` is the fix. `token_explicit_max_ttl`
# is the one field that still caps a token that is already periodic, so a snippet
# that grows a non-zero one silently restores the cliff. `token_max_ttl=0` is
# hygiene: once the period is set Vault consults token_max_ttl only to cap the
# period VALUE, never to bound the token's life — it is asserted so the shipped
# role does not go on saying two things at once.
#
# Fixing a failure: docs/keeper/prod-setup.md -> "Why token_period and not
# token_max_ttl".
set -euo pipefail

# Every place that hands an operator a `vault write auth/approle/role/...`.
# Add a site here when you add one to the docs. An unlisted site is invisible to
# assertion 1 — assertion 2 still catches it, but only if it carries the old
# spelling.
#
# docs/keeper/prod-setup.md is the one with the reasoning; the other three
# repeat the command. docs/keeper/config.md is deliberately NOT here: it points
# at prod-setup.md and carries no command. Put it on the list the day it does.
SITES=(
	docs/keeper/prod-setup.md
	docs/operations/infra.md
	docs/operations/deb-onboarding.md
	examples/keeper/vault-policy.hcl
)

fail=0

for site in "${SITES[@]}"; do
	if [ ! -f "$site" ]; then
		echo "check-approle-template: $site is missing — the site list above is stale"
		fail=1
		continue
	fi

	# Assert on the template LINE, not on the file. Every one of these documents
	# also *talks about* token_max_ttl=0 in prose a few lines away, so a
	# file-scoped `grep -q token_max_ttl=0` is satisfied by the explanation even
	# when the command right above it still hands out a 24-hour ceiling. That
	# failure mode is the very trap the ticket is about, and it passes a
	# file-scoped guard silently.
	#
	# `secret_id_ttl=` is what identifies the parameter line: it is on every
	# role-creation snippet and nowhere else in these files.
	params=$(grep -n 'secret_id_ttl=' "$site" || true)
	if [ -z "$params" ]; then
		echo "check-approle-template: $site has no role-parameter line (no secret_id_ttl=) — either the snippet was removed or it was reworded past recognition; update the site list or the matcher"
		fail=1
		continue
	fi
	while IFS= read -r line; do
		case "$line" in
		*token_period=*) ;;
		*)
			echo "check-approle-template: $site:${line%%:*} — role parameters without token_period=; the role must issue a PERIODIC token, or Keeper loses Vault the moment the token's maximum lifetime runs out"
			fail=1
			;;
		esac
		case "$line" in
		*token_explicit_max_ttl=0*) ;;
		*)
			echo "check-approle-template: $site:${line%%:*} — role parameters without token_explicit_max_ttl=0; that field is a hard cap even on a PERIODIC token, so a non-zero one puts the cliff straight back, and \`vault write\` on an existing role leaves any field you do not name exactly as it was"
			fail=1
			;;
		esac
		case "$line" in
		*token_max_ttl=0*) ;;
		*)
			echo "check-approle-template: $site:${line%%:*} — role parameters without token_max_ttl=0; a periodic token is not bounded by it, but leaving the old ceiling on the role makes the role state two contradictory things, and it comes back the moment token_period is removed"
			fail=1
			;;
		esac
	done <<<"$params"
done

# Exactly two files may contain the literal: the CHANGELOG entry for NIM-429
# quotes the broken template in order to describe the bug, and this script has to
# name what it forbids. The consequence is that assertion 2 cannot police these
# two — an allowlist entry is a hole, so keep it to these and state why.
#
# The sweep is over TRACKED files, because the subject is what the repository
# ships. `grep -r .` also walked ignored paths, so an untracked local scratch
# note under .pm/ reddened this tier in one checkout while CI and every worktree
# stayed green — a verdict about the checkout, not about the tree. `-H` is
# required: xargs may hand grep a final batch of one file, and grep omits the
# filename then, which would break both the allowlist and the report.
ALLOWED_RE='^(CHANGELOG\.md|scripts/check-approle-template\.sh):'
survivors=$(git ls-files -z | xargs -0 grep -nIH 'token_ttl=' | grep -Ev "$ALLOWED_RE" || true)
if [ -n "$survivors" ]; then
	echo "check-approle-template: the old role template survives:"
	echo "$survivors"
	echo ""
	echo "  token_ttl= puts a maximum lifetime under Keeper's Vault token, and"
	echo "  renew-self cannot carry a token past it. Use token_period= instead —"
	echo "  see docs/keeper/prod-setup.md."
	fail=1
fi

if [ "$fail" -ne 0 ]; then
	exit 1
fi
echo "approle-template: the role template issues a periodic token in all ${#SITES[@]} sites"
