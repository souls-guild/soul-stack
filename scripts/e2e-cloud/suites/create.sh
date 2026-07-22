#!/usr/bin/env bash
# Suite create: [bring-up] -> create (engine=POST /v1/incarnations, or
# E2E_CREATE_MODE=script - creation is done by the bring-up scripts, the engine picks up
# the last apply_id via GET /runs) -> poll -> assert_run_success -> secondary assert of
# incarnation.status==ready (the only healthy terminal, huma_enums.go).

# _create_body - builds the POST /v1/incarnations body from parameters.
_create_body() {
	local covens_json="[]" input_json="null"
	[[ -n "${COVENS:-}" ]] && covens_json="$(printf '%s' "${COVENS}" | jq -Rc 'split(",")')"
	[[ -n "${E2E_CREATE_INPUT:-}" ]] && input_json="${E2E_CREATE_INPUT}"
	jq -nc \
		--arg name "${INCARNATION:-redis-auto}" \
		--arg svc "${SERVICE:-example-cloud-bootstrap}" \
		--arg cs "${CREATE_SCENARIO:-create}" \
		--argjson covens "$covens_json" \
		--argjson input "$input_json" '
		{name: $name, service: $svc}
		+ (if $cs == "" then {} else {create_scenario: $cs} end)
		+ (if ($covens | length) > 0 then {covens: $covens} else {} end)
		+ (if $input == null then {} else {input: $input} end)'
}

# _assert_incarnation_ready <name> - secondary status assert (ready).
_assert_incarnation_ready() {
	local name="$1" resp code inc start
	start="$(_utc_now)"
	resp="$(keeper_api GET "/v1/incarnations/${name}")"
	code="$(http_code "$resp")"; inc="$(http_body "$resp")"
	if assert_state_field "$inc" '.status' "${HEALTHY_TERMINAL:-ready}"; then
		report_step "assert incarnation.status==${HEALTHY_TERMINAL:-ready}" - "$start" 0 "$code" - "ok" PASS
		return 0
	fi
	report_step "assert incarnation.status==${HEALTHY_TERMINAL:-ready}" - "$start" 0 "$code" - "got!=${HEALTHY_TERMINAL:-ready}" FAIL
	return 1
}

# suite_create -> 0 everything PASSed / 1 assert or run failure.
suite_create() {
	local name="${INCARNATION:-redis-auto}"
	run_bringup || { _e2e_log "create: bring-up failed - stopping"; return 1; }

	local apply_id start rc t0
	start="$(_utc_now)"; t0=$SECONDS

	if [[ "${E2E_CREATE_MODE:-engine}" == script ]]; then
		apply_id="$(http_body "$(keeper_api GET "/v1/incarnations/${name}/runs?limit=1")" | jq -r '.items[0].apply_id // empty' 2>/dev/null)"
		if [[ -z "$apply_id" ]]; then
			report_step "create (script): pick up apply_id" - "$start" "$((SECONDS - t0))" - - "no runs in /runs" FAIL
			return 1
		fi
	else
		local resp code
		resp="$(keeper_api POST /v1/incarnations "$(_create_body)")"
		code="$(http_code "$resp")"
		if [[ "$code" != 202 ]]; then
			report_step "create (engine): POST /incarnations" - "$start" "$((SECONDS - t0))" "$code" - "expected 202" FAIL
			return 1
		fi
		apply_id="$(http_body "$resp" | jq -r '.apply_id // empty' 2>/dev/null)"
		if [[ -z "$apply_id" ]]; then
			# lifecycle.auto_create:false -> bare incarnation (ready without a run).
			report_step "create (engine): POST /incarnations" - "$start" "$((SECONDS - t0))" "$code" - "202 without apply_id (bare)" SKIP
			_assert_incarnation_ready "$name"; return $?
		fi
	fi

	poll_until_terminal "$name" "$apply_id"; rc=$?
	if [[ $rc -ne 0 ]]; then
		assert_run_success "$POLL_LAST_JSON" >/dev/null 2>&1 || true
		report_step "create: ${CREATE_SCENARIO:-create}" "$apply_id" "$start" "$((SECONDS - t0))" "${POLL_LAST_HTTP:--}" "${POLL_LAST_STATUS:--}" "run=fail:rc${rc}" FAIL
		return 1
	fi
	if assert_run_success "$POLL_LAST_JSON"; then
		report_step "create: ${CREATE_SCENARIO:-create}" "$apply_id" "$start" "$((SECONDS - t0))" "${POLL_LAST_HTTP}" "${POLL_LAST_STATUS}" "run=success" PASS
	else
		report_step "create: ${CREATE_SCENARIO:-create}" "$apply_id" "$start" "$((SECONDS - t0))" "${POLL_LAST_HTTP}" "${POLL_LAST_STATUS}" "hosts!=success" FAIL
		return 1
	fi
	_assert_incarnation_ready "$name"
}
