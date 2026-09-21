#!/usr/bin/env bash
# The .debs about to be mirrored are the bytes goreleaser produced (NIM-882).
#
# check-release-provenance.sh answers "did the gated pipeline CREATE this release". That is
# a different question from "are these the assets it built", and the gap between them is one
# command: `gh release upload v1.2.3 evil.deb` onto the pipeline's own release leaves the
# author `github-actions[bot]`, the release.yml run green, and the provenance check happy.
# apt-publish.yml then mirrors evil.deb into a public pool under our apt signature.
#
# So the assets are checked against the signature chain the release already carries:
#
#   checksums.txt        — goreleaser's digest of every artifact (.goreleaser.yaml `checksum:`)
#   checksums.txt.sig    — cosign keyless signature over it (`signs:` → artifacts: checksum)
#   checksums.txt.pem    — the Fulcio cert, whose SAN is the workflow identity
#
# The identity is pinned to release.yml AT THIS TAG, which is what makes the chain worth
# anything: a signature made by any other workflow, any other repository, or the same
# workflow at a different ref does not verify. Then every .deb must APPEAR in checksums.txt
# and match. Appearing is checked separately and first, because `sha256sum -c` says nothing
# about a file nobody listed — an unlisted evil.deb is not a mismatch, it is a silence.
#
# Usage: TAG=v1.2.3 DEB_DIR=dist/pkg scripts/verify-release-assets.sh
#        scripts/verify-release-assets.sh --self-test   # stubbed cosign, real sha256sum
set -euo pipefail

: "${GITHUB_REPOSITORY:=souls-guild/soul-stack}"
OIDC_ISSUER="https://token.actions.githubusercontent.com"

die() {
	printf 'verify-release-assets: %s\n' "$1" >&2
	exit 1
}

verify() {
	local dir="$DEB_DIR" sums identity deb name listed=0

	sums="$dir/checksums.txt"
	for f in "$sums" "$sums.sig" "$sums.pem"; do
		[ -f "$f" ] ||
			die "$(basename "$f") is not among the release assets. Every release this pipeline builds carries the signed checksums file; one without it was not built by it."
	done

	identity="https://github.com/${GITHUB_REPOSITORY}/.github/workflows/release.yml@refs/tags/${TAG}"
	cosign verify-blob "$sums" \
		--certificate "$sums.pem" \
		--signature "$sums.sig" \
		--certificate-identity "$identity" \
		--certificate-oidc-issuer "$OIDC_ISSUER" >/dev/null 2>&1 ||
		die "cosign could not verify checksums.txt against ${identity}. The signature is missing, malformed, or was made by something other than release.yml at this tag."

	# Membership before digests. `sha256sum -c` walks the LIST, so a file present on disk but
	# absent from it is never mentioned — the exact shape an uploaded extra asset has.
	#
	# Compared as a whole field, not searched for. `grep -F "  $name"` is unanchored at both
	# ends, so `keeper_1.2.3_amd64.deb` would find itself inside a listed
	# `soul-stack-keeper_1.2.3_amd64.deb` and then be skipped by --ignore-missing — a name
	# collision away from a silent pass. The separator is two spaces in text mode and ` *` in
	# binary mode; both are consumed so either listing is read rather than failing open.
	for deb in "$dir"/*.deb; do
		[ -e "$deb" ] || die "no .deb assets in $dir to verify."
		name="$(basename "$deb")"
		# Through ENVIRON, not `-v`: an `-v` assignment runs escape processing on the value, so
		# a disk name containing a backslash would be compared after decoding and could match a
		# listed entry it is not.
		WANT="$name" awk '
			match($0, /^[0-9a-fA-F]+[ ][ *]/) && substr($0, RLENGTH + 1) == ENVIRON["WANT"] { found = 1 }
			END { exit !found }
		' "$sums" ||
			die "$name is not listed in the signed checksums.txt. It was added to the release after goreleaser built it, and nothing vouches for its contents."
		listed=$((listed + 1))
	done

	# --ignore-missing: checksums.txt covers archives and images too, which are not downloaded
	# here. Every line that names a file we DO have must match.
	(cd "$dir" && sha256sum --ignore-missing -c checksums.txt >/dev/null 2>&1) ||
		die "a .deb does not match its digest in the signed checksums.txt."

	printf 'verify-release-assets: %s .deb(s) match the cosign-verified checksums.txt for %s\n' "$listed" "$TAG"
}

# --- self-test ---------------------------------------------------------------------------
#
# cosign is stubbed (its verdict is the one thing this script does not compute); sha256sum,
# grep and the file layout are real, so the membership and digest halves are exercised for
# real rather than modelled.

self_test() {
	local me tmp pass=0 fail=0
	me="$(cd "$(dirname "$0")" && pwd)/$(basename "$0")"
	tmp="$(mktemp -d)"
	trap "rm -rf '$tmp'" EXIT
	mkdir -p "$tmp/bin"
	# The stub asserts the two arguments that carry the whole security property before it
	# answers: without them `cosign verify-blob` accepts a signature from any identity, and a
	# stub that ignored them would let that deletion pass every case below.
	cat >"$tmp/bin/cosign" <<'COSIGN'
#!/usr/bin/env bash
args="$*"
case "$args" in
  *"--certificate-identity https://github.com/souls-guild/soul-stack/.github/workflows/release.yml@refs/tags/v1.2.3"*) ;;
  *) echo "stub cosign: certificate identity not pinned to release.yml at this tag" >&2; exit 3 ;;
esac
case "$args" in
  *"--certificate-oidc-issuer https://token.actions.githubusercontent.com"*) ;;
  *) echo "stub cosign: oidc issuer not pinned" >&2; exit 3 ;;
esac
exit "${COSIGN_RC:-0}"
COSIGN
	chmod +x "$tmp/bin/cosign"

	baseline() {
		rm -rf "$tmp/pkg"
		mkdir -p "$tmp/pkg"
		printf 'keeper payload\n' >"$tmp/pkg/keeper_1.2.3_amd64.deb"
		printf 'soul payload\n' >"$tmp/pkg/soul_1.2.3_amd64.deb"
		# Real digests, and a line for an archive that is deliberately not downloaded — the
		# case --ignore-missing exists for.
		(cd "$tmp/pkg" && sha256sum ./*.deb | sed 's|\./||' >checksums.txt)
		printf '%s  soul-stack_1.2.3_linux_amd64.tar.gz\n' "$(printf 0 | sha256sum | cut -d' ' -f1)" >>"$tmp/pkg/checksums.txt"
		: >"$tmp/pkg/checksums.txt.sig"
		: >"$tmp/pkg/checksums.txt.pem"
	}

	run_case() { # <label> <expected exit> <expected text> [COSIGN_RC]
		local label="$1" want_rc="$2" want="$3" rc_env="${4:-0}" out rc
		set +e
		out="$(PATH="$tmp/bin:$PATH" COSIGN_RC="$rc_env" TAG=v1.2.3 DEB_DIR="$tmp/pkg" \
			GITHUB_REPOSITORY=souls-guild/soul-stack "$me" 2>&1)"
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
	run_case "the pipeline's own assets verify" 0 "2 .deb(s) match"

	# The hole this script exists for: an extra asset uploaded onto a legitimate release.
	baseline
	printf 'evil\n' >"$tmp/pkg/evil_1.2.3_amd64.deb"
	run_case "an asset absent from checksums.txt" 1 "is not listed in the signed checksums.txt"

	# And the other direction: a listed name whose bytes were swapped.
	baseline
	printf 'swapped\n' >"$tmp/pkg/keeper_1.2.3_amd64.deb"
	run_case "a listed asset whose bytes changed" 1 "does not match its digest"

	baseline
	run_case "an unverifiable signature" 1 "cosign could not verify" 1

	# On disk: `keeper_1.2.3_amd64.deb`, unlisted. In checksums.txt: `…deb.orig`, absent from
	# disk. An `grep -F "  $name"` membership test is unanchored at the END, so the short name
	# is found INSIDE the long entry and passes; --ignore-missing then skips the long entry
	# because no such file is here, and the unvouched .deb is mirrored with nothing said.
	baseline
	sed -i 's|  keeper_1.2.3_amd64.deb$|  keeper_1.2.3_amd64.deb.orig|' "$tmp/pkg/checksums.txt"
	run_case "a name that is a PREFIX of a listed one" 1 "is not listed in the signed checksums.txt"

	baseline
	rm "$tmp/pkg/checksums.txt.sig"
	run_case "a release with no signature at all" 1 "checksums.txt.sig is not among"

	baseline
	rm "$tmp/pkg/checksums.txt.pem"
	run_case "a release with no certificate" 1 "checksums.txt.pem is not among"

	baseline
	rm "$tmp/pkg/checksums.txt"
	run_case "a release with no checksums file" 1 "checksums.txt is not among"

	baseline
	rm "$tmp"/pkg/*.deb
	run_case "nothing to mirror" 1 "no .deb assets"

	if [ "$fail" -gt 0 ]; then
		printf 'verify-release-assets: self-test FAILED (%s case(s))\n' "$fail"
		return 1
	fi
	printf 'verify-release-assets: self-test OK (%s cases)\n' "$pass"
}

if [ "${1:-}" = "--self-test" ]; then
	self_test
	exit $?
fi

: "${TAG:?TAG is required}"
: "${DEB_DIR:?DEB_DIR is required}"
verify
