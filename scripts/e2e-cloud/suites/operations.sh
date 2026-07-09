#!/usr/bin/env bash
# Generic-движок операционных сценариев: любой service-defined сценарий через POST
# /v1/incarnations/{name}/scenarios/{scenario} → poll → assert_run_success. Имя
# сценария — в PATH (add_user/update_config/restart/rotate_tls/...). run_scenario —
# чистая (сеть только keeper_api), тестируется guard-ом на фикстурах.

# run_scenario <name> <scenario> [input_json] → 0 success / 1 fail / 2 timeout.
# Побочно: RUN_APPLY_ID / RUN_STATUS / RUN_HTTP.
run_scenario() {
	local name="$1" scenario="$2" input="${3:-}"
	local body="" resp code apply_id rc
	RUN_APPLY_ID=""; RUN_STATUS=""; RUN_HTTP=""
	if [[ -n "$input" ]]; then
		body="$(jq -nc --argjson inp "$input" '{input:$inp}' 2>/dev/null)" || {
			_e2e_log "run_scenario: невалидный input JSON: ${input}"
			RUN_HTTP=000; RUN_STATUS=bad_input; return 1
		}
	fi
	resp="$(keeper_api POST "/v1/incarnations/${name}/scenarios/${scenario}" "$body")"
	code="$(http_code "$resp")"; RUN_HTTP="$code"
	if [[ "$code" != 202 ]]; then
		_e2e_log "run_scenario: POST scenarios/${scenario} → http=${code} (ожидал 202)"
		RUN_STATUS="http_${code}"; return 1
	fi
	apply_id="$(http_body "$resp" | jq -r '.apply_id // empty' 2>/dev/null)"
	RUN_APPLY_ID="$apply_id"
	if [[ -z "$apply_id" ]]; then
		_e2e_log "run_scenario: 202 без apply_id"
		RUN_STATUS="no_apply_id"; return 1
	fi
	poll_until_terminal "$name" "$apply_id"; rc=$?
	RUN_STATUS="${POLL_LAST_STATUS:-?}"
	case $rc in
	0) assert_run_success "$POLL_LAST_JSON"; return $? ;;
	1) assert_run_success "$POLL_LAST_JSON" >/dev/null 2>&1 || true; return 1 ;;
	*) _e2e_log "run_scenario: timeout опроса ${scenario}/${apply_id}"; return 2 ;;
	esac
}

# _split_scenarios <raw> — записи списка $SCENARIOS по одной в строке. Разделитель
# записей — ПЕРЕВОД СТРОКИ (не ';'), чтобы ';' внутри JSON-input не рвал запись.
# Ведущие пробелы и пустые строки отбрасываются.
_split_scenarios() {
	local e
	while IFS= read -r e; do
		e="${e#"${e%%[![:space:]]*}"}"
		[[ -n "$e" ]] && printf '%s\n' "$e"
	done <<<"$1"
}

# suite_operations — одиночный сценарий ($SCENARIO + $SCENARIO_INPUT) или построчный список
# $SCENARIOS (одна запись = одна строка; запись: `name` или `name::<json-input>`;
# перевод строки — разделитель записей, чтобы ';' в JSON не рвал запись). Опц.
# state-ассерт $STATE_ASSERT_PATH / $STATE_ASSERT_EXPECTED после успешного сценария.
suite_operations() {
	local name="${INCARNATION:-redis-auto}"
	local -a entries=()
	if [[ -n "${SCENARIOS:-}" ]]; then
		mapfile -t entries < <(_split_scenarios "${SCENARIOS}")
	elif [[ -n "${SCENARIO:-}" ]]; then
		entries=("${SCENARIO}${SCENARIO_INPUT:+::${SCENARIO_INPUT}}")
	else
		_e2e_log "operations: задай SCENARIO=<имя> [SCENARIO_INPUT=<json>] или SCENARIOS=\$'s1\\ns2::{...}' (запись на строку)"
		report_step "operations: параметры" - "$(_utc_now)" - - - "нет SCENARIO/SCENARIOS" FAIL
		return 2
	fi
	local e scenario input start rc overall=0
	for e in "${entries[@]}"; do
		e="${e#"${e%%[![:space:]]*}"}"
		[[ -z "$e" ]] && continue
		scenario="${e%%::*}"; input=""
		[[ "$e" == *"::"* ]] && input="${e#*::}"
		start="$(_utc_now)"; local t0=$SECONDS
		run_scenario "$name" "$scenario" "$input"; rc=$?
		if [[ $rc -eq 0 ]]; then
			report_step "operations: ${scenario}" "${RUN_APPLY_ID:--}" "$start" "$((SECONDS - t0))" "${RUN_HTTP:--}" "${RUN_STATUS:--}" "run=success" PASS
		else
			report_step "operations: ${scenario}" "${RUN_APPLY_ID:--}" "$start" "$((SECONDS - t0))" "${RUN_HTTP:--}" "${RUN_STATUS:--}" "run=fail:rc${rc}" FAIL
			overall=1
		fi
		if [[ $rc -eq 0 && -n "${STATE_ASSERT_PATH:-}" && "${DRY_RUN:-0}" == 1 ]]; then
			report_step "operations: state-assert" - "$(_utc_now)" - - - "dry-run (синтетический state)" SKIP
		elif [[ $rc -eq 0 && -n "${STATE_ASSERT_PATH:-}" ]]; then
			local inc
			inc="$(http_body "$(keeper_api GET "/v1/incarnations/${name}")")"
			if assert_state_field "$inc" "${STATE_ASSERT_PATH}" "${STATE_ASSERT_EXPECTED:-}"; then
				report_step "operations: state-assert" - "$(_utc_now)" - 200 - "${STATE_ASSERT_PATH}==${STATE_ASSERT_EXPECTED}" PASS
			else
				report_step "operations: state-assert" - "$(_utc_now)" - 200 - "${STATE_ASSERT_PATH}!=${STATE_ASSERT_EXPECTED}" FAIL
				overall=1
			fi
		fi
	done
	return $overall
}
