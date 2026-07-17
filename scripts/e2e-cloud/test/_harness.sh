#!/usr/bin/env bash
# Micro-harness for guard tests (bats isn't in the repo - a homegrown ~30 lines): it/assert_eq/fail.

_T_PASS=0
_T_FAIL=0
_T_NAME=""

# it <name> - name the current test case.
it() { _T_NAME="$1"; }

# assert_eq <expected> <actual> [msg]
assert_eq() {
	if [[ "$1" == "$2" ]]; then
		_T_PASS=$((_T_PASS + 1))
		printf '  ok   %s\n' "${_T_NAME}${3:+ — $3}"
	else
		_T_FAIL=$((_T_FAIL + 1))
		printf '  FAIL %s\n       expected=[%s] actual=[%s]\n' "${_T_NAME}${3:+ — $3}" "$1" "$2"
	fi
}

# fail <msg> - unconditional failure of the current test case.
fail() {
	_T_FAIL=$((_T_FAIL + 1))
	printf '  FAIL %s — %s\n' "$_T_NAME" "$1"
}

# harness_summary - print the summary; exit status 0 only if there are no failures.
harness_summary() {
	printf '\n== guard: %d passed, %d failed ==\n' "$_T_PASS" "$_T_FAIL"
	[[ "$_T_FAIL" -eq 0 ]]
}
