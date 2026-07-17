#!/usr/bin/env bash
# Docker-free guard tests of the load-bearing logic: classify/poll/assert/run_scenario run
# against a STUB keeper_api on JSON fixtures (testdata/). RED/GREEN mutation pairs: a broken
# expectation breaks exactly one assert. Deterministic, no network.

set -o pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(dirname "$HERE")"
export TESTDATA="${ROOT}/testdata"
export POLL_INTERVAL=0 POLL_MAX=3

command -v jq >/dev/null 2>&1 || { echo "guard: jq is required in PATH"; exit 2; }

. "${HERE}/_harness.sh"
. "${ROOT}/lib/keeper-api.sh" # http_body/http_code + seam declaration (we override it with the stub)
. "${ROOT}/lib/poll.sh"
. "${ROOT}/lib/assert.sh"
. "${ROOT}/suites/operations.sh" # run_scenario

# --- keeper_api stub: a "code:fixture" queue (fixture without .json; '-' = empty body).
# keeper_api is called inside $() substitution (a subshell) → we keep the index in a FILE,
# otherwise the increment is lost on return from the subshell (the subshell inherits the
# array, we read from it).
_STUB=()
_STUB_IDX_FILE="$(mktemp)"
trap 'rm -f "$_STUB_IDX_FILE"' EXIT
stub_reset() {
	_STUB=("$@")
	printf '0' >"$_STUB_IDX_FILE"
}
keeper_api() {
	local i e code fx body=""
	i="$(cat "$_STUB_IDX_FILE")"
	printf '%s' "$((i + 1))" >"$_STUB_IDX_FILE"
	e="${_STUB[$i]:-}"
	code="${e%%:*}"; fx="${e#*:}"
	[[ -n "$fx" && "$fx" != "-" ]] && body="$(cat "${TESTDATA}/${fx}.json")"
	printf '%s\n%s' "$body" "$code"
}

echo "== classify_status (RED/GREEN) =="
it "success→PASS (GREEN)"; assert_eq PASS "$(classify_status success)"
it "failed→FAIL"; assert_eq FAIL "$(classify_status failed)"
it "cancelled→FAIL"; assert_eq FAIL "$(classify_status cancelled)"
it "applying→CONTINUE"; assert_eq CONTINUE "$(classify_status applying)"
# RED: sabotage the success set - success is no longer PASS (catches exactly this assert).
it "RED: success token flipped → no longer PASS"
red="$(E2E_STATUS_SUCCESS=succeeded classify_status success)"
[[ "$red" != PASS ]] && assert_eq ok ok "mutation breaks the GREEN assert" || fail "mutation NOT caught (still PASS)"

echo "== poll_until_terminal (call-based stub) =="
it "applying,applying,success→0"
stub_reset "200:run_applying" "200:run_applying" "200:run_success"
poll_until_terminal redis-auto A; assert_eq 0 "$?"
it "applying,failed→1"
stub_reset "200:run_applying" "200:run_failed"
poll_until_terminal redis-auto A; assert_eq 1 "$?"
it "all applying→timeout=2"
stub_reset "200:run_applying" "200:run_applying" "200:run_applying"
poll_until_terminal redis-auto A; assert_eq 2 "$?"

echo "== assert_run_success (fixtures) =="
it "run_success.json→0"
assert_run_success "$(cat "${TESTDATA}/run_success.json")" >/dev/null 2>&1; assert_eq 0 "$?"
it "run_success_no_match.json (success + benign no_match)→0 (GREEN)"
assert_run_success "$(cat "${TESTDATA}/run_success_no_match.json")" >/dev/null 2>&1; assert_eq 0 "$?"
it "run_failed.json (host failed + failed_task_idx)→1 (RED)"
assert_run_success "$(cat "${TESTDATA}/run_failed.json")" >/dev/null 2>&1; assert_eq 1 "$?"

echo "== assert_state_field (RED/GREEN) =="
INC_ADD_USER="$(cat "${TESTDATA}/state_add_user.json")"
it "user path == alice → 0 (GREEN)"
assert_state_field "$INC_ADD_USER" '.state.users[]?.name' alice >/dev/null 2>&1; assert_eq 0 "$?"
it "RED: expected flipped to bob → 1"
assert_state_field "$INC_ADD_USER" '.state.users[]?.name' bob >/dev/null 2>&1; assert_eq 1 "$?"

echo "== incarnation.status secondary assert (ready is the only healthy terminal) =="
it "inc_ready.json .status==ready → 0"
assert_state_field "$(cat "${TESTDATA}/inc_ready.json")" '.status' ready >/dev/null 2>&1; assert_eq 0 "$?"
it "inc_error_locked.json .status==ready → 1 (locked, NOT a healthy terminal)"
assert_state_field "$(cat "${TESTDATA}/inc_error_locked.json")" '.status' ready >/dev/null 2>&1; assert_eq 1 "$?"
it "inc_error_locked.json .status==error_locked → 0"
assert_state_field "$(cat "${TESTDATA}/inc_error_locked.json")" '.status' error_locked >/dev/null 2>&1; assert_eq 0 "$?"

echo "== run_scenario end-to-end (POST 202 → poll → assert) =="
it "run_reply(202)→applying→success ⇒ 0"
stub_reset "202:run_reply" "200:run_applying" "200:run_success"
run_scenario redis-auto add_user '{"name":"alice"}' >/dev/null 2>&1; assert_eq 0 "$?"
it "run_reply(202)→failed ⇒ 1"
stub_reset "202:run_reply" "200:run_failed"
run_scenario redis-auto add_user '{"name":"alice"}' >/dev/null 2>&1; assert_eq 1 "$?"
it "POST not 202 (409) ⇒ 1 (async≠success)"
stub_reset "409:-"
run_scenario redis-auto add_user '{"name":"alice"}' >/dev/null 2>&1; assert_eq 1 "$?"

echo "== dry-run synth + SCENARIOS-split (QA regressions) =="
it "synth GET .../runs?limit=1 → an object with .items (script-create-mode; query doesn't break the match)"
syn="$(_dryrun_synth GET "/v1/incarnations/redis-auto/runs?limit=1")"
synaid="$(http_body "$syn" | jq -r '.items[0].apply_id // empty' 2>/dev/null)"
[[ -n "$synaid" ]] && assert_eq ok ok "apply_id extracted from synth" || fail "no .items[0].apply_id - query broke the match"
it "SCENARIOS: ';' inside JSON input doesn't break the record (line-based split)"
mapfile -t SPL < <(_split_scenarios "$(printf 'add_user::{"acl":"a;b"}\nrestart')")
assert_eq 2 "${#SPL[@]}" "exactly two records"
it "SCENARIOS: JSON input with ';' preserved intact"
assert_eq 'add_user::{"acl":"a;b"}' "${SPL[0]:-}"

harness_summary
