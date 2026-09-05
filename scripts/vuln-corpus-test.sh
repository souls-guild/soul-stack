#!/usr/bin/env bash
# vuln-corpus-test.sh — the guard on what `check-vuln` actually scans (NIM-774).
#
# What is being guarded. The gate's whole claim is a negative one — "no reachable
# advisory in this repository" — and a negative claim is falsified by a module
# nobody looked at, not by a wrong verdict. Before this ticket the corpus was
# $(MODULES), eight of the twenty go.mod files in the tree, and every one of the
# twelve absent modules was absent in the quietest possible way: no row, no
# skip, no name, followed by the line "govulncheck is clean across all modules".
#
# That regression cannot be caught by reading a green run, so it is caught here.
# The cases below are of three kinds:
#
#   the corpus     — vuln-modules.sh answers with the tree, not with a list, and
#                    a module carrying no `toolchain` line is in it like any
#                    other. A future "only scan what resolves" filter is exactly
#                    the shape that would pass every other check in the repo.
#   the mode       — vuln-scan.sh scans a go.work member inside the workspace and
#                    everything else standalone, under the module's build tag,
#                    and says which. Fixtures only, no Go, no network: the stub
#                    reports its own argv and GOWORK.
#   the recipe     — `make check-vuln` itself, over fixture modules, with a stub
#                    scanner. This is the half that matters: it proves the target
#                    passes the derived corpus and the mode-aware probe, rather
#                    than proving that two scripts nobody calls would be correct
#                    if they were called.
#
# Offline and docker-free. The recipe cases need `go list` over a stdlib-only
# fixture; nothing here reaches the network or the vulnerability database.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${ROOT}"

. "${ROOT}/scripts/e2e-cloud/test/_harness.sh"

tmp="$(mktemp -d)"
trap 'rm -rf "${tmp}"' EXIT

contains() { case "${out}" in *"$1"*) printf 'yes' ;; *) printf 'no' ;; esac; }
lacks() { case "${out}" in *"$1"*) printf 'no' ;; *) printf 'yes' ;; esac; }

# ---------------------------------------------------------------------------
# The corpus. `find` is what the script uses, so asserting the same `find` twice
# proves little on its own — what it does catch is the corpus turning back into
# a written-down list, which is the only way this has ever broken. The named
# members below carry the weight: one per family that the old $(MODULES) missed.
# ---------------------------------------------------------------------------

corpus="$(scripts/vuln-modules.sh 2>"${tmp}/corpus.err")"
corpus_rc=$?
out="${corpus}"

it "the corpus is every go.mod in the tree"
assert_eq 0 "${corpus_rc}" "exit code"
expected="$(find . -name go.mod -not -path './.git/*' |
  sed -e 's|/go\.mod$||' -e 's|^\./||' | LC_ALL=C sort)"
assert_eq "${expected}" "${corpus}" "module list"

it "each family the old eight-module corpus never opened is in it"
assert_eq yes "$(contains 'tests/e2e')" "the e2e harnesses"
assert_eq yes "$(contains 'examples/module/soul-mod-redis')" "the community plugins"
assert_eq yes "$(contains 'keeper/internal/pluginhost/testdata/ssh-plugin')" "the pluginhost fixtures"

it "a module whose go.mod names no toolchain is in the corpus like any other"
missing=""
while IFS= read -r m; do
  [ -n "${m}" ] || continue
  grep -q '^toolchain[[:space:]]' "${m}/go.mod" 2>/dev/null && continue
  case $'\n'"${corpus}"$'\n' in
    *$'\n'"${m}"$'\n'*) ;;
    *) missing="${missing}${m} " ;;
  esac
done < <(find . -name go.mod -not -path './.git/*' | sed -e 's|/go\.mod$||' -e 's|^\./||')
assert_eq "" "${missing}" "toolchain-less modules absent from the corpus"

it "Go files belonging to no module are named, and not silently absent"
err="$(cat "${tmp}/corpus.err")"
orphan_dirs="$(find . -name '*.go' -not -path './.git/*' |
  sed -e 's|/[^/]*$||' -e 's|^\./||' | LC_ALL=C sort -u |
  while IFS= read -r d; do
    p="${d}"
    while :; do
      [ -f "${p}/go.mod" ] && break
      [ "${p}" != "." ] || { echo "${d}"; break; }
      p="$(dirname "${p}")"
    done
  done)"
if [ -n "${orphan_dirs}" ]; then
  out="${err}"
  assert_eq yes "$(contains 'belong to no module')" "the warning is printed"
  while IFS= read -r d; do
    assert_eq yes "$(contains "${d}")" "${d} is named"
  done <<<"${orphan_dirs}"
else
  assert_eq "" "${err}" "nothing orphaned, nothing to warn about"
fi

# ---------------------------------------------------------------------------
# The mode. A fixture repository with its own go.work: one member, one module
# outside it. The stub scanner prints its argv and GOWORK, so the assertions are
# about what govulncheck would have been run as, not about what a comment says.
# ---------------------------------------------------------------------------

repo="${tmp}/repo"
mkdir -p "${repo}/inws" "${repo}/outws"
cat >"${repo}/go.work" <<'EOF'
go 1.26.4

toolchain go1.26.6

use (
	./inws
)
EOF
printf 'module inws\n\ngo 1.26.4\n\ntoolchain go1.26.6\n' >"${repo}/inws/go.mod"
printf 'module outws\n\ngo 1.26.4\n' >"${repo}/outws/go.mod"

stub="${tmp}/govulncheck-stub"
cat >"${stub}" <<'EOF'
#!/usr/bin/env bash
printf 'STUB argv=[%s] GOWORK=[%s] pwd=[%s]\n' "$*" "${GOWORK:-<unset>}" "${PWD}"
EOF
chmod +x "${stub}"

scan() {
  local dir="$1"
  shift
  out="$(cd "${repo}/${dir}" && VULN_ROOT="${repo}" GOVULNCHECK="${stub}" "$@" \
    "${ROOT}/scripts/vuln-scan.sh" 2>&1)"
}

it "a module go.work does not list is scanned standalone"
scan outws
assert_eq yes "$(contains 'GOWORK=[off]')" "GOWORK=off reaches the scanner"
assert_eq yes "$(contains 'scanned standalone')" "and the log says so"
assert_eq yes "$(lacks 'scanned in the workspace')" "and does not claim the other one"

it "a module without a toolchain line says whose stdlib verdict this is"
assert_eq yes "$(contains 'names no toolchain')" "the note is printed"

it "a go.work member is scanned inside the workspace"
scan inws
assert_eq yes "$(contains 'GOWORK=[<unset>]')" "the workspace is left in place"
assert_eq yes "$(contains 'scanned in the workspace')" "and the log says so"
assert_eq yes "$(lacks 'names no toolchain')" "a member that declares one gets no note"

it "the module's build tag reaches govulncheck"
scan outws env VULN_TAGS='outws:mytag other:othertag'
assert_eq yes "$(contains 'argv=[-tags=mytag ./...]')" "its own tag, and only its own"
scan inws env VULN_TAGS='outws:mytag'
assert_eq yes "$(contains 'argv=[./...]')" "a module with no entry gets no tag"

it "a go.work member without a toolchain line gets no note either — the workspace answers for it"
printf 'module bare\n\ngo 1.26.4\n' >"${repo}/inws/go.mod"
scan inws
assert_eq yes "$(lacks 'names no toolchain')" "the note is about standalone modules only"
printf 'module inws\n\ngo 1.26.4\n\ntoolchain go1.26.6\n' >"${repo}/inws/go.mod"

it "the probe runs in the same mode and prints nothing of its own"
out="$(cd "${repo}/outws" && VULN_ROOT="${repo}" GOVULNCHECK="${stub}" \
  "${ROOT}/scripts/vuln-scan.sh" --probe 2>/dev/null)"
assert_eq "" "${out}" "probe stdout is go list's alone — modules-run.sh reads emptiness as 'nothing here'"

# ---------------------------------------------------------------------------
# The recipe. `make check-vuln` over fixture modules with the stub scanner: the
# corpus and the probe are the target's own, only the scanner is replaced.
# SKIP_VULNCHECK= on the command line neutralises an inherited offline opt-out —
# without it a developer's exported variable would turn this guard into a test
# that asserts the skip message, and pass.
# ---------------------------------------------------------------------------

mkdir -p "${tmp}/plain" "${tmp}/broken" "${tmp}/tagged" "${tmp}/nogo"
printf 'module plain\n\ngo 1.26.4\n' >"${tmp}/plain/go.mod"
printf 'package plain\n\nimport "net/url"\n\nvar _ = url.Parse\n' >"${tmp}/plain/plain.go"
printf 'this is not a go.mod\n' >"${tmp}/broken/go.mod"
printf 'package broken\n' >"${tmp}/broken/broken.go"
printf 'module tagged\n\ngo 1.26.4\n' >"${tmp}/tagged/go.mod"
printf '//go:build sometag\n\npackage tagged\n' >"${tmp}/tagged/tagged.go"
printf 'module nogo\n\ngo 1.26.4\n' >"${tmp}/nogo/go.mod"

run_recipe() {
  out="$(${MAKE:-make} --no-print-directory check-vuln SKIP_VULNCHECK= \
    GOVULNCHECK="${stub}" VULN_MODULES="$*" 2>&1)"
  rc=$?
}

it "a module whose go.mod names no toolchain is SCANNED, not skipped"
run_recipe "${tmp}/plain"
assert_eq 0 "${rc}" "exit code"
assert_eq yes "$(contains 'STUB argv=[./...]')" "govulncheck ran there"
assert_eq yes "$(contains "PASS     ${tmp}/plain")" "and the table records a verdict"
assert_eq yes "$(lacks "SKIPPED  ${tmp}/plain")" "never a skip"

it "the summary names every module the sweep was given"
run_recipe "${tmp}/plain" "${tmp}/broken"
assert_eq yes "$(contains "${tmp}/plain")" "the module that passed"
assert_eq yes "$(contains "${tmp}/broken")" "the module that did not"
assert_eq yes "$(contains '(of 2 modules)')" "and the count is the corpus, not the survivors"

it "a module that cannot be enumerated FAILS — 'we could not scan it' is not a pass"
# Non-zero rather than a number: modules-run.sh exits 1 and make reports a failed
# recipe as 2, and which of the two reaches here is make's business, not this
# guard's. What is being guarded is that the gate refuses, not how it spells it.
assert_eq yes "$([ "${rc}" -ne 0 ] && printf yes || printf no)" "the target fails"
assert_eq yes "$(contains "FAIL     ${tmp}/broken")" "the broken module is a failure"
assert_eq yes "$(lacks "SKIPPED  ${tmp}/broken")" "and never a skip"
assert_eq yes "$(lacks 'govulncheck reached nothing it calls')" "the green line is not printed over it"

# The hole this closes is the ticket's own defect wearing the fix's clothes: a
# module whose every file sits behind a tag VULN_TAGS does not name answers
# `go list ./...` with a warning, no output and exit 0 — which modules-run.sh
# reads as "nothing here" and reports as a green SKIPPED row. One typo in
# VULN_TAGS would have done that to the whole tests/e2e harness.
it "a module whose Go is all behind an unnamed build tag FAILS — it is not 'nothing here'"
run_recipe "${tmp}/tagged"
assert_eq yes "$([ "${rc}" -ne 0 ] && printf yes || printf no)" "the target fails"
assert_eq yes "$(contains "FAIL     ${tmp}/tagged")" "the module is a failure"
assert_eq yes "$(lacks "SKIPPED  ${tmp}/tagged")" "and never a skip"
assert_eq yes "$(contains 'reaches no package')" "with the reason quoted from the probe"
assert_eq yes "$(lacks 'STUB argv')" "and govulncheck was not run there"

it "a module holding no Go file at all is the one legitimate SKIPPED"
run_recipe "${tmp}/nogo"
assert_eq 0 "${rc}" "exit code — proto/plugin before \`make gen\` is a real answer"
assert_eq yes "$(contains "SKIPPED  ${tmp}/nogo")" "reported as skipped"
assert_eq yes "$(contains 'no Go file in this module at all')" "with the reason"

# ---------------------------------------------------------------------------
# The corpus the RECIPE uses, not the one the script prints. Every case above
# overrides VULN_MODULES, so all of them stay green after the corpus is narrowed
# back to $(MODULES) — which is the defect. `make -n` expands the recipe without
# running it, and the expansion carries the corpus verbatim.
# ---------------------------------------------------------------------------

it "check-vuln's own corpus is the derived one, module for module"
out="$(${MAKE:-make} -n check-vuln SKIP_VULNCHECK= 2>/dev/null)"
missing=""
while IFS= read -r m; do
  [ -n "${m}" ] || continue
  case "${out}" in
    *"${m}"*) ;;
    *) missing="${missing}${m} " ;;
  esac
done <<<"${corpus}"
assert_eq "" "${missing}" "modules vuln-modules.sh derives that the recipe would not sweep"

harness_summary
