#!/usr/bin/env bash
# Единственная сетевая граница оркестратора: весь HTTP к Operator API идёт ТОЛЬКО
# через keeper_api. classify/poll/assert/report чисты и тестируются подменой этой
# функции стабом (test/guard.sh). Инвариант: никакой другой код сети не делает.

# _e2e_log — трейс в stderr (не загрязняет stdout-тело ответа).
_e2e_log() { printf '%s\n' "$*" >&2; }

# _utc_now — RFC3339Nano UTC (форма date-time reply-структур keeper).
_utc_now() { date -u +%Y-%m-%dT%H:%M:%S.000000000Z; }

# http_code/http_body — split ответа keeper_api («тело\nHTTP-код», код = последняя
# строка). Чистые, без сети.
http_code() { local r="$1"; printf '%s' "${r##*$'\n'}"; }
http_body() { local r="$1"; if [[ "$r" == *$'\n'* ]]; then printf '%s' "${r%$'\n'*}"; else printf '%s' ""; fi; }

# keeper_api <method> <path> [body_json] — печатает тело, последняя строка = HTTP-код.
# EXEC_MODE=local: прямой curl. EXEC_MODE=tsh: curl на VM через teleport (тело POST —
# base64 в env, чтобы не тонуть во вложенном квотинге). DRY_RUN=1: синтетический ответ
# без сети (печатает намеренный вызов в stderr).
keeper_api() {
	local method="$1" path="$2" body="${3:-}"
	_e2e_log "  → ${method} ${path}${body:+  body=${body}}"

	if [[ "${DRY_RUN:-0}" == 1 ]]; then
		_e2e_log "    [dry-run] сеть не тронута, синтетический ответ"
		_dryrun_synth "$method" "$path"
		return 0
	fi

	case "${EXEC_MODE:-tsh}" in
	local) _keeper_api_local "$method" "$path" "$body" ;;
	tsh) _keeper_api_tsh "$method" "$path" "$body" ;;
	*)
		_e2e_log "keeper_api: неизвестный EXEC_MODE='${EXEC_MODE}'"
		printf '%s\n%s' '{"error":"bad EXEC_MODE"}' 000
		;;
	esac
}

# _keeper_api_local — curl напрямую на $KEEPER_API, JWT из $JWT_FILE.
_keeper_api_local() {
	local method="$1" path="$2" body="$3"
	local url="${KEEPER_API:-http://127.0.0.1:8080}${path}"
	local -a args=(-sS -X "$method" "$url" -H "Authorization: Bearer $(cat "${JWT_FILE:-/tmp/keeper-dev/archon-alice.jwt}")")
	[[ "${INSECURE_TLS:-0}" == 1 ]] && args+=(-k)
	[[ -n "$body" ]] && args+=(-H "Content-Type: application/json" -d "$body")
	curl "${args[@]}" -w $'\n%{http_code}'
}

# _keeper_api_tsh — curl на localhost:8080 внутри VM через `tsh ssh`. Тело — base64
# в env-переменной BODY_B64 (урок autoprov-run2.sh: вложенный квотинг тонет).
# Teleport-шум фильтруется grep-ом.
_keeper_api_tsh() {
	local method="$1" path="$2" body="$3"
	local b64=""
	[[ -n "$body" ]] && b64="$(printf '%s' "$body" | base64 | tr -d '\n')"
	local insecure=""
	[[ "${INSECURE_TLS:-0}" == 1 ]] && insecure="-k"
	TELEPORT_HOME="${TELEPORT_HOME:-/mnt/c/Users/stf20/.tsh}" \
		SSL_CERT_FILE="${SSL_CERT_FILE:-}" \
		tsh ssh "${TSH_NODE:?TSH_NODE не задан}" \
		M="$method" P="$path" BODY_B64="$b64" \
		JWTF="${REMOTE_JWT:-/opt/soul-stack/archon-cloud.jwt}" \
		KAPI="${REMOTE_KEEPER_API:-http://localhost:8080}" INSECURE="$insecure" \
		bash -s <<'REMOTE' 2>/dev/null | grep -viE 'WARNING|authority|proxy|self-signed|malicious|contact|ignore'
body=""
[ -n "$BODY_B64" ] && body="$(printf '%s' "$BODY_B64" | base64 -d)"
if [ -n "$body" ]; then
	curl -sS $INSECURE -X "$M" "$KAPI$P" -H "Authorization: Bearer $(cat "$JWTF")" -H "Content-Type: application/json" -d "$body" -w $'\n%{http_code}'
else
	curl -sS $INSECURE -X "$M" "$KAPI$P" -H "Authorization: Bearer $(cat "$JWTF")" -w $'\n%{http_code}'
fi
REMOTE
}

# _dryrun_synth — синтетический успешный ответ по (method,path), моделируя reply-DTO
# keeper (huma_incarnation_reply.go). Порядок веток важен: /runs/{id} до /runs.
# Матч по пути БЕЗ query (${path%%\?*}), иначе `?limit=1` (script-create-mode) уходит
# в catch-all и `.items` пуст → ложный «нет прогонов».
_dryrun_synth() {
	local method="$1" path="$2"
	local aid="dryrun00000000000000000000" now
	now="$(_utc_now)"
	case "$method ${path%%\?*}" in
	"POST /v1/incarnations")
		printf '{"apply_id":"%s","incarnation":"%s"}\n%s' "$aid" "${INCARNATION:-redis-auto}" 202 ;;
	"POST "*/unlock)
		printf '{"name":"%s","previous_status":"error_locked","status":"ready","unlocked_at":"%s","unlocked_by_aid":"%s"}\n%s' \
			"${INCARNATION:-redis-auto}" "$now" "${AID:-archon-alice}" 200 ;;
	"POST "*/scenarios/*)
		local sc="${path##*/scenarios/}"
		printf '{"apply_id":"%s","incarnation":"%s","scenario":"%s"}\n%s' "$aid" "${INCARNATION:-redis-auto}" "$sc" 202 ;;
	"DELETE "*)
		printf '{"apply_id":"%s"}\n%s' "$aid" 202 ;;
	"GET "*/runs/*)
		printf '{"apply_id":"%s","scenario":"dry","status":"success","started_at":"%s","finished_at":"%s","started_by_aid":"%s","hosts":[{"sid":"%s-1","status":"success","passage":0,"attempt":1,"cancel_requested":false}]}\n%s' \
			"$aid" "$now" "$now" "${AID:-archon-alice}" "${INCARNATION:-redis-auto}" 200 ;;
	"GET "*/runs)
		printf '{"items":[{"apply_id":"%s","scenario":"dry","status":"success","started_at":"%s"}],"offset":0,"limit":50,"total":1}\n%s' "$aid" "$now" 200 ;;
	"GET "*/history)
		printf '{"items":[],"offset":0,"limit":50,"total":0}\n%s' 200 ;;
	"GET "*)
		printf '{"covens":["%s"],"created_at":"%s","created_by_aid":"%s","name":"%s","service":"%s","service_version":"dry","spec":null,"state":{"users":[]},"state_schema_version":1,"status":"ready","status_details":null,"updated_at":"%s"}\n%s' \
			"${INCARNATION:-redis-auto}" "$now" "${AID:-archon-alice}" "${INCARNATION:-redis-auto}" "${SERVICE:-example-cloud-bootstrap}" "$now" 200 ;;
	*)
		printf '{}\n%s' 200 ;;
	esac
}
