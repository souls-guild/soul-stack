#!/usr/bin/env bash
# Generic day-2 engine: any service-defined scenario via POST
# /v1/incarnations/{name}/scenarios/{scenario} -> poll -> assert_run_success. The
# scenario name is in the PATH (add_user/update_config/restart/rotate_tls/...).
# run_scenario is pure (network only touches keeper_api), tested by a guard on
# fixtures.

# run_scenario <name> <scenario> [input_json] -> 0 success / 1 fail / 2 timeout.
# Side effects: RUN_APPLY_ID / RUN_STATUS / RUN_HTTP.
run_scenario() {
	local name="$1" scenario="$2" input="${3:-}"
	local body="" resp code apply_id rc
	RUN_APPLY_ID=""; RUN_STATUS=""; RUN_HTTP=""
	if [[ -n "$input" ]]; then
		body="$(jq -nc --argjson inp "$input" '{input:$inp}' 2>/dev/null)" || {
			_e2e_log "run_scenario: invalid input JSON: ${input}"
			RUN_HTTP=000; RUN_STATUS=bad_input; return 1
		}
	fi
	resp="$(keeper_api POST "/v1/incarnations/${name}/scenarios/${scenario}" "$body")"
	code="$(http_code "$resp")"; RUN_HTTP="$code"
	if [[ "$code" != 202 ]]; then
		_e2e_log "run_scenario: POST scenarios/${scenario} -> http=${code} (expected 202)"
		RUN_STATUS="http_${code}"; return 1
	fi
	apply_id="$(http_body "$resp" | jq -r '.apply_id // empty' 2>/dev/null)"
	RUN_APPLY_ID="$apply_id"
	if [[ -z "$apply_id" ]]; then
		_e2e_log "run_scenario: 202 without apply_id"
		RUN_STATUS="no_apply_id"; return 1
	fi
	poll_until_terminal "$name" "$apply_id"; rc=$?
	RUN_STATUS="${POLL_LAST_STATUS:-?}"
	case $rc in
	0) assert_run_success "$POLL_LAST_JSON"; return $? ;;
	1) assert_run_success "$POLL_LAST_JSON" >/dev/null 2>&1 || true; return 1 ;;
	*) _e2e_log "run_scenario: timeout polling ${scenario}/${apply_id}"; return 2 ;;
	esac
}

# _split_scenarios <raw> -- entries of the $SCENARIOS list, one per line. The
# entry separator is a NEWLINE (not ';'), so that a ';' inside JSON-input
# doesn't break an entry. Leading whitespace and empty lines are dropped.
_split_scenarios() {
	local e
	while IFS= read -r e; do
		e="${e#"${e%%[![:space:]]*}"}"
		[[ -n "$e" ]] && printf '%s\n' "$e"
	done <<<"$1"
}

# suite_day2 -- a single scenario ($SCENARIO + $SCENARIO_INPUT) or a line-by-line
# list $SCENARIOS (one entry = one line; entry: `name` or `name::<json-input>`;
# newline is the entry separator, so a ';' in JSON doesn't break an entry).
# Optional state assertion $STATE_ASSERT_PATH / $STATE_ASSERT_EXPECTED after a
# successful scenario.
suite_day2() {
	local name="${INCARNATION:-redis-auto}"
	local -a entries=()
	if [[ -n "${SCENARIOS:-}" ]]; then
		mapfile -t entries < <(_split_scenarios "${SCENARIOS}")
	elif [[ -n "${SCENARIO:-}" ]]; then
		entries=("${SCENARIO}${SCENARIO_INPUT:+::${SCENARIO_INPUT}}")
	else
		_e2e_log "day2: set SCENARIO=<name> [SCENARIO_INPUT=<json>] or SCENARIOS=\$'s1\\ns2::{...}' (one entry per line)"
		report_step "day2: parameters" - "$(_utc_now)" - - - "missing SCENARIO/SCENARIOS" FAIL
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
			report_step "day2: ${scenario}" "${RUN_APPLY_ID:--}" "$start" "$((SECONDS - t0))" "${RUN_HTTP:--}" "${RUN_STATUS:--}" "run=success" PASS
		else
			report_step "day2: ${scenario}" "${RUN_APPLY_ID:--}" "$start" "$((SECONDS - t0))" "${RUN_HTTP:--}" "${RUN_STATUS:--}" "run=fail:rc${rc}" FAIL
			overall=1
		fi
		if [[ $rc -eq 0 && -n "${STATE_ASSERT_PATH:-}" && "${DRY_RUN:-0}" == 1 ]]; then
			report_step "day2: state-assert" - "$(_utc_now)" - - - "dry-run (synthetic state)" SKIP
		elif [[ $rc -eq 0 && -n "${STATE_ASSERT_PATH:-}" ]]; then
			local inc
			inc="$(http_body "$(keeper_api GET "/v1/incarnations/${name}")")"
			if assert_state_field "$inc" "${STATE_ASSERT_PATH}" "${STATE_ASSERT_EXPECTED:-}"; then
				report_step "day2: state-assert" - "$(_utc_now)" - 200 - "${STATE_ASSERT_PATH}==${STATE_ASSERT_EXPECTED}" PASS
			else
				report_step "day2: state-assert" - "$(_utc_now)" - 200 - "${STATE_ASSERT_PATH}!=${STATE_ASSERT_EXPECTED}" FAIL
				overall=1
			fi
		fi
	done
	return $overall
}
