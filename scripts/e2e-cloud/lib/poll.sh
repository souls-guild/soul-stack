#!/usr/bin/env bash
# Классификация статуса прогона и опрос до терминала. Чисто (сеть — только через
# keeper_api). Источник истины по статусам apply-прогона —
# keeper/internal/applyrun/applyrun.go (агрегат RunDetailReply.status:
# applying|success|failed|cancelled). Множества data-driven, засеяны из enum.

# Множества классификации (override для мутац-тестов guard).
: "${E2E_STATUS_SUCCESS:=success}"
: "${E2E_STATUS_FAIL:=failed cancelled}"
: "${E2E_STATUS_TRANSIENT:=applying}"

# _in_set <needle> <space-list>
_in_set() {
	local needle="$1" w
	for w in $2; do [[ "$w" == "$needle" ]] && return 0; done
	return 1
}

# classify_status <run_status> → PASS|FAIL|CONTINUE. Неизвестный статус → CONTINUE
# (безопасно: опрос дойдёт до timeout, а не ложно засчитает успех).
classify_status() {
	local s="${1:-}"
	if _in_set "$s" "${E2E_STATUS_SUCCESS}"; then echo PASS
	elif _in_set "$s" "${E2E_STATUS_FAIL}"; then echo FAIL
	elif _in_set "$s" "${E2E_STATUS_TRANSIENT}"; then echo CONTINUE
	else echo CONTINUE; fi
}

# poll_until_terminal <name> <apply_id> → 0 success / 1 failed|cancelled / 2 timeout.
# Каждые $POLL_INTERVAL сек тянет GET /runs/{apply_id}, максимум $POLL_MAX итераций.
# Побочно выставляет POLL_LAST_JSON / POLL_LAST_STATUS / POLL_LAST_HTTP.
poll_until_terminal() {
	local name="$1" apply_id="$2"
	local interval="${POLL_INTERVAL:-30}" maxp="${POLL_MAX:-40}"
	local i resp code body status verdict
	POLL_LAST_JSON=""; POLL_LAST_STATUS=""; POLL_LAST_HTTP=""
	for ((i = 1; i <= maxp; i++)); do
		resp="$(keeper_api GET "/v1/incarnations/${name}/runs/${apply_id}")"
		code="$(http_code "$resp")"; body="$(http_body "$resp")"
		POLL_LAST_JSON="$body"; POLL_LAST_HTTP="$code"
		if [[ "$code" != 200 ]]; then
			_e2e_log "    [$(date -u +%H:%M:%S) #${i}] http=${code} (прогон ещё не виден / транзиент) — жду"
			[[ $i -lt $maxp ]] && sleep "$interval"
			continue
		fi
		status="$(printf '%s' "$body" | jq -r '.status // empty' 2>/dev/null)"
		POLL_LAST_STATUS="$status"
		verdict="$(classify_status "$status")"
		_e2e_log "    [$(date -u +%H:%M:%S) #${i}] status=${status:-?} → ${verdict}"
		case "$verdict" in
		PASS) return 0 ;;
		FAIL) return 1 ;;
		CONTINUE) [[ $i -lt $maxp ]] && sleep "$interval" ;;
		esac
	done
	return 2
}
