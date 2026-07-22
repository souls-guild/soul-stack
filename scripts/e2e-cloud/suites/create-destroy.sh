#!/usr/bin/env bash
# Suite create-destroy: pre-clean (idempotency -- acceptance criterion!) -> suite_create ->
# destroy -> confirmation that "the incarnation is gone". A repeat run must pass without
# manual intervention.

# _destroy <name> -> 0 (202+apply_id) / 1 (not 202) / 3 (already 404). Side effects:
# DESTROY_APPLY_ID / DESTROY_HTTP.
_destroy() {
	local name="$1" allow="${ALLOW_DESTROY:-true}" resp code
	DESTROY_APPLY_ID=""; DESTROY_HTTP=""
	resp="$(keeper_api DELETE "/v1/incarnations/${name}?allow_destroy=${allow}")"
	code="$(http_code "$resp")"; DESTROY_HTTP="$code"
	case "$code" in
	404) return 3 ;;
	202) DESTROY_APPLY_ID="$(http_body "$resp" | jq -r '.apply_id // empty' 2>/dev/null)"; return 0 ;;
	*) return 1 ;;
	esac
}

# _wait_gone <name> -> 0 incarnation is gone (GET -> 404) / 2 timeout. The authoritative
# criterion for destroy success (more reliable than the teardown-run status: it also covers
# allow_destroy=true with no teardown run).
_wait_gone() {
	local name="$1" interval="${POLL_INTERVAL:-30}" maxp="${POLL_MAX:-40}" i code
	[[ "${DRY_RUN:-0}" == 1 ]] && { _e2e_log "    [dry-run] destroy-wait: counting it as gone"; return 0; }
	for ((i = 1; i <= maxp; i++)); do
		code="$(http_code "$(keeper_api GET "/v1/incarnations/${name}")")"
		_e2e_log "    [$(date -u +%H:%M:%S) #${i}] destroy-wait http=${code}"
		[[ "$code" == 404 ]] && return 0
		[[ $i -lt $maxp ]] && sleep "$interval"
	done
	return 2
}

# _destroy_and_wait <name> [label] -> 0 destroyed / 1 failure.
_destroy_and_wait() {
	local name="$1" label="${2:-destroy: $1}" start t0
	start="$(_utc_now)"; t0=$SECONDS
	_destroy "$name"; local drc=$?
	if [[ $drc -eq 3 ]]; then
		report_step "$label" - "$start" 0 "${DESTROY_HTTP}" - "nothing to destroy (404)" SKIP
		return 0
	fi
	if [[ $drc -ne 0 ]]; then
		report_step "$label" - "$start" "$((SECONDS - t0))" "${DESTROY_HTTP}" - "DELETE expected 202" FAIL
		return 1
	fi
	_wait_gone "$name"; local wrc=$?
	if [[ $wrc -eq 0 ]]; then
		report_step "$label" "${DESTROY_APPLY_ID:--}" "$start" "$((SECONDS - t0))" "${DESTROY_HTTP}" - "incarnation destroyed (404)" PASS
		return 0
	fi
	report_step "$label" "${DESTROY_APPLY_ID:--}" "$start" "$((SECONDS - t0))" "${DESTROY_HTTP}" - "did not get destroyed (timeout)" FAIL
	return 1
}

# _pre_clean <name> -- clear a stuck incarnation before create (idempotency).
_pre_clean() {
	local name="$1" resp code inc status
	resp="$(keeper_api GET "/v1/incarnations/${name}")"
	code="$(http_code "$resp")"
	if [[ "$code" == 404 ]]; then
		report_step "pre-clean: ${name}" - "$(_utc_now)" 0 404 - "doesn't exist -- clean" SKIP
		return 0
	fi
	if [[ "$code" != 200 ]]; then
		report_step "pre-clean: ${name}" - "$(_utc_now)" 0 "$code" - "unexpected code -- continuing" SKIP
		return 0
	fi
	inc="$(http_body "$resp")"
	status="$(printf '%s' "$inc" | jq -r '.status // empty' 2>/dev/null)"
	_e2e_log "pre-clean: ${name} exists (status=${status}) -- clearing the lock and destroying"
	if [[ "$status" == error_locked || "$status" == migration_failed ]]; then
		keeper_api POST "/v1/incarnations/${name}/unlock" '{"reason":"e2e-cloud pre-clean"}' >/dev/null
	fi
	_destroy_and_wait "$name" "pre-clean destroy: ${name}"
	return 0
}

# suite_create_destroy -> 0 create AND destroy passed / 1 otherwise.
suite_create_destroy() {
	local name="${INCARNATION:-redis-auto}"
	if [[ "${DRY_RUN:-0}" == 1 ]]; then
		_e2e_log "create-destroy: DRY-RUN -- pre-clean skipped"
	else
		_pre_clean "$name"
	fi
	suite_create; local crc=$?
	[[ $crc -ne 0 ]] && _e2e_log "create-destroy: create failed (rc=${crc}) -- destroying anyway for cleanup"
	_destroy_and_wait "$name" "destroy: ${name}"; local drc=$?
	[[ $crc -eq 0 && $drc -eq 0 ]] && return 0 || return 1
}
