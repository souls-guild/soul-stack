#!/usr/bin/env bash
# Микро-harness guard-тестов (bats в репо нет — свой ~30 строк): it/assert_eq/fail.

_T_PASS=0
_T_FAIL=0
_T_NAME=""

# it <name> — назвать текущий кейс.
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

# fail <msg> — безусловный провал текущего кейса.
fail() {
	_T_FAIL=$((_T_FAIL + 1))
	printf '  FAIL %s — %s\n' "$_T_NAME" "$1"
}

# harness_summary — печать итога, exit-status 0 только если провалов нет.
harness_summary() {
	printf '\n== guard: %d passed, %d failed ==\n' "$_T_PASS" "$_T_FAIL"
	[[ "$_T_FAIL" -eq 0 ]]
}
