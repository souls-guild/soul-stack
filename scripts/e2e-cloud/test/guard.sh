#!/usr/bin/env bash
# Docker-free guard-тесты несущей логики: classify/poll/assert/run_scenario гоняются
# со СТАБОМ keeper_api на JSON-фикстурах (testdata/). Мутац-пары RED/GREEN: сбитое
# ожидание ломает ровно один ассерт. Детерминировано, без сети.

set -o pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(dirname "$HERE")"
export TESTDATA="${ROOT}/testdata"
export POLL_INTERVAL=0 POLL_MAX=3

command -v jq >/dev/null 2>&1 || { echo "guard: требуется jq в PATH"; exit 2; }

. "${HERE}/_harness.sh"
. "${ROOT}/lib/keeper-api.sh" # http_body/http_code + seam-декларация (переопределим стабом)
. "${ROOT}/lib/poll.sh"
. "${ROOT}/lib/assert.sh"
. "${ROOT}/suites/operations.sh" # run_scenario

# --- стаб keeper_api: очередь "code:fixture" (fixture без .json; '-' = пустое тело).
# keeper_api зовётся в $()-подстановке (субшелл) → индекс держим в ФАЙЛЕ, иначе
# инкремент теряется на возврате из субшелла (массив субшелл наследует, читаем из него).
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
# RED: sabotage множества успеха — success больше НЕ PASS (ловит ровно этот ассерт).
it "RED: success-токен сбит → уже НЕ PASS"
red="$(E2E_STATUS_SUCCESS=succeeded classify_status success)"
[[ "$red" != PASS ]] && assert_eq ok ok "мутация ломает GREEN-ассерт" || fail "мутация НЕ поймана (осталось PASS)"

echo "== poll_until_terminal (стаб по вызовам) =="
it "applying,applying,success→0"
stub_reset "200:run_applying" "200:run_applying" "200:run_success"
poll_until_terminal redis-auto A; assert_eq 0 "$?"
it "applying,failed→1"
stub_reset "200:run_applying" "200:run_failed"
poll_until_terminal redis-auto A; assert_eq 1 "$?"
it "все applying→timeout=2"
stub_reset "200:run_applying" "200:run_applying" "200:run_applying"
poll_until_terminal redis-auto A; assert_eq 2 "$?"

echo "== assert_run_success (фикстуры) =="
it "run_success.json→0"
assert_run_success "$(cat "${TESTDATA}/run_success.json")" >/dev/null 2>&1; assert_eq 0 "$?"
it "run_success_no_match.json (success + benign no_match)→0 (GREEN)"
assert_run_success "$(cat "${TESTDATA}/run_success_no_match.json")" >/dev/null 2>&1; assert_eq 0 "$?"
it "run_failed.json (host failed + failed_task_idx)→1 (RED)"
assert_run_success "$(cat "${TESTDATA}/run_failed.json")" >/dev/null 2>&1; assert_eq 1 "$?"

echo "== assert_state_field (RED/GREEN) =="
INC_ADD_USER="$(cat "${TESTDATA}/state_add_user.json")"
it "путь юзера == alice → 0 (GREEN)"
assert_state_field "$INC_ADD_USER" '.state.users[]?.name' alice >/dev/null 2>&1; assert_eq 0 "$?"
it "RED: expected сбит на bob → 1"
assert_state_field "$INC_ADD_USER" '.state.users[]?.name' bob >/dev/null 2>&1; assert_eq 1 "$?"

echo "== incarnation.status вторичный ассерт (ready — единственный здоровый терминал) =="
it "inc_ready.json .status==ready → 0"
assert_state_field "$(cat "${TESTDATA}/inc_ready.json")" '.status' ready >/dev/null 2>&1; assert_eq 0 "$?"
it "inc_error_locked.json .status==ready → 1 (залочена, НЕ здоровый терминал)"
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
it "POST не 202 (409) ⇒ 1 (async≠успех)"
stub_reset "409:-"
run_scenario redis-auto add_user '{"name":"alice"}' >/dev/null 2>&1; assert_eq 1 "$?"

echo "== dry-run synth + SCENARIOS-split (регрессы QA) =="
it "synth GET .../runs?limit=1 → объект с .items (script-create-mode; query не рвёт матч)"
syn="$(_dryrun_synth GET "/v1/incarnations/redis-auto/runs?limit=1")"
synaid="$(http_body "$syn" | jq -r '.items[0].apply_id // empty' 2>/dev/null)"
[[ -n "$synaid" ]] && assert_eq ok ok "apply_id извлечён из synth" || fail "нет .items[0].apply_id — query сломал матч"
it "SCENARIOS: ';' внутри JSON-input не рвёт запись (построчный split)"
mapfile -t SPL < <(_split_scenarios "$(printf 'add_user::{"acl":"a;b"}\nrestart')")
assert_eq 2 "${#SPL[@]}" "ровно две записи"
it "SCENARIOS: JSON-input с ';' сохранён целиком"
assert_eq 'add_user::{"acl":"a;b"}' "${SPL[0]:-}"

harness_summary
