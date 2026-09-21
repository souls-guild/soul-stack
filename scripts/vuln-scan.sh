#!/usr/bin/env bash
# vuln-scan.sh — the check-vuln scan of ONE module, run from inside that
# module's directory by scripts/modules-run.sh (NIM-774).
#
# Why a script and not a command string. modules-run.sh runs the same command in
# every module, and this corpus needs two things decided per module:
#
#   the workspace.  go.work lists twelve of the twenty modules. Inside them the
#     workspace resolves the intra-repo `replace`s and lends its `toolchain`
#     line; outside them a go command run under the workspace refuses the
#     directory outright, so `examples/module/*` and the pluginhost fixtures
#     have to be scanned with GOWORK=off. It is not a free choice either way:
#     `keeper` and `tests/load` do NOT resolve standalone (`go: updates to
#     go.mod needed`), and the plugins do not resolve any other way.
#
#   build tags.  `tests/e2e` keeps its whole harness behind `//go:build e2e`, so
#     an untagged scan there reports "no packages matched" — and `go list ./...`
#     answers the probe with a warning, no output and exit 0, which modules-run.sh
#     reads as a module with nothing in it. Adding tests/ to the corpus without
#     the tags would have bought a SKIPPED row reading "no Go packages here yet"
#     over four thousand lines of harness: the same false green, now with a row
#     in the table to make it look checked. The tags come from $(TAGGED_DIRS),
#     which already carries this mapping for vet-tags, via VULN_TAGS.
#
# What it deliberately does NOT do is fall back. A module that will not resolve
# in its own mode fails; it is never retried in the other one and never demoted
# to a skip. `check-vuln` is the gate that answers "no reachable advisory here",
# and the one answer it must never produce is that sentence about a module it
# could not scan.
#
# Usage (cwd = the module):
#   VULN_ROOT=<repo root> [VULN_TAGS='<dir>:<tag> ...'] [GOVULNCHECK=<path>] \
#     scripts/vuln-scan.sh [--probe]
#
# --probe replaces govulncheck with `go list`, in the SAME mode and under the
# same tags, for modules-run.sh's probe. Probe mode prints nothing of its own:
# its stdout is the signal ("empty means there is nothing here"), so a friendly
# line from this script would be read as a package list.
set -uo pipefail

probe=""
[ "${1:-}" != "--probe" ] || probe="yes"

root="${VULN_ROOT:-}"
bin="${GOVULNCHECK:-govulncheck}"
tagmap="${VULN_TAGS:-}"

if [ -z "${root}" ]; then
  echo "vuln-scan.sh: VULN_ROOT is unset — cannot tell a go.work member from a standalone module" >&2
  exit 2
fi

# A module outside the repository root keeps its absolute path as its name: the
# guard's fixtures live in a temp directory, and a name that silently became the
# whole path only for them would make the guard's assertions unlike the real
# thing in exactly the place they are asserting about.
rel="${PWD#"${root}"/}"

member=""
if [ -f "${root}/go.work" ]; then
  while IFS= read -r used; do
    [ "${used}" != "${rel}" ] || { member="yes"; break; }
  done < <(sed -n 's|^[[:space:]]*\(use[[:space:]]*\)\{0,1\}\./\([^[:space:]]*\).*|\2|p' "${root}/go.work")
fi

tags=""
for spec in ${tagmap}; do
  [ "${spec%%:*}" = "${rel}" ] || continue
  tags="${spec##*:}"
  break
done

cmd=()
[ -n "${member}" ] || cmd+=(env GOWORK=off)
if [ -n "${probe}" ]; then
  cmd+=(go list)
else
  cmd+=("${bin}")
fi
[ -z "${tags}" ] || cmd+=("-tags=${tags}")
cmd+=(./...)

if [ -n "${probe}" ]; then
  # An EMPTY package list is modules-run.sh's signal for "there is nothing to run
  # here", and it answers SKIPPED — a legitimate answer for proto/plugin before
  # `make gen`, and a false green for everything else. `go list ./...` in a module
  # whose every file is excluded by build tags prints a warning, nothing on stdout
  # and exits 0, so one wrong VULN_TAGS entry would turn tests/e2e's whole harness
  # into a row reading "no Go files here at all" and the gate would stay green over
  # it — this ticket's defect, reintroduced by this ticket's fix and wearing its
  # table. So emptiness is only accepted from a module that really has no Go in it.
  list="$("${cmd[@]}")" || exit $?
  if [ -n "${list}" ]; then
    printf '%s\n' "${list}"
    exit 0
  fi
  if [ -n "$(find . -name '*.go' -print -quit 2>/dev/null)" ]; then
    echo "vuln-scan.sh: ${rel}: has Go files, but this build configuration${tags:+ (-tags=${tags})} reaches no package —" >&2
    echo "vuln-scan.sh: ${rel}: refusing to report it as a module with nothing in it. Its code is behind a tag" >&2
    echo "vuln-scan.sh: ${rel}: VULN_TAGS does not name, or names wrongly." >&2
    exit 1
  fi
  exit 0
fi

if [ -n "${member}" ]; then
  echo "check-vuln: ${rel}: scanned in the workspace${tags:+, -tags=${tags}} — go.work lends it the toolchain"
else
  echo "check-vuln: ${rel}: scanned standalone (GOWORK=off, outside go.work)${tags:+, -tags=${tags}}"
  # The stdlib half of every finding NIM-774 uncovered was this line missing.
  # Standalone, govulncheck judges the standard library by what the module itself
  # declares, so a `go 1.26.4` with no toolchain is scanned as go1.26.4 and
  # reports everything fixed in 1.26.5 and 1.26.6 — while the same code inside
  # the workspace reports nothing, because go.work says go1.26.6. Both answers
  # are about the toolchain, not about the code, and the module's own is the one
  # that travels to whoever builds it standalone. Said out loud so a reader of a
  # red run can tell which of the two they are holding. A go.work member gets no
  # such line: the workspace answers for it, and that IS how this repository
  # builds it.
  grep -q '^toolchain[[:space:]]' go.mod 2>/dev/null ||
    echo "check-vuln: ${rel}: its go.mod names no toolchain — the stdlib verdict below is its 'go' directive's, not go.work's"
fi

exec "${cmd[@]}"
