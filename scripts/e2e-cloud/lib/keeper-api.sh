#!/usr/bin/env bash
# The orchestrator's only network boundary: all HTTP to the Operator API goes
# ONLY through keeper_api. classify/poll/assert/report are pure and are tested
# by stubbing this function (test/guard.sh). Invariant: no other code touches
# the network.

# _e2e_log - trace to stderr (doesn't pollute the response's stdout body).
_e2e_log() { printf '%s\n' "$*" >&2; }

# _utc_now - RFC3339Nano UTC (the form of keeper's date-time reply structs).
_utc_now() { date -u +%Y-%m-%dT%H:%M:%S.000000000Z; }

# http_code/http_body - split keeper_api's response ("body\nHTTP-code", the
# code is the last line). Pure, no network.
http_code() { local r="$1"; printf '%s' "${r##*$'\n'}"; }
http_body() { local r="$1"; if [[ "$r" == *$'\n'* ]]; then printf '%s' "${r%$'\n'*}"; else printf '%s' ""; fi; }

# keeper_api <method> <path> [body_json] - prints the body, last line = HTTP code.
# EXEC_MODE=local: direct curl. EXEC_MODE=tsh: curl on the VM via teleport (the
# POST body goes as base64 in an env var, to avoid drowning in nested quoting).
# DRY_RUN=1: synthetic response without network access (prints the intended
# call to stderr).
keeper_api() {
	local method="$1" path="$2" body="${3:-}"
	_e2e_log "  -> ${method} ${path}${body:+  body=${body}}"

	if [[ "${DRY_RUN:-0}" == 1 ]]; then
		_e2e_log "    [dry-run] network untouched, synthetic response"
		_dryrun_synth "$method" "$path"
		return 0
	fi

	case "${EXEC_MODE:-tsh}" in
	local) _keeper_api_local "$method" "$path" "$body" ;;
	tsh) _keeper_api_tsh "$method" "$path" "$body" ;;
	*)
		_e2e_log "keeper_api: unknown EXEC_MODE='${EXEC_MODE}'"
		printf '%s\n%s' '{"error":"bad EXEC_MODE"}' 000
		;;
	esac
}

# _keeper_api_local - curl directly to $KEEPER_API, JWT from $JWT_FILE.
_keeper_api_local() {
	local method="$1" path="$2" body="$3"
	local url="${KEEPER_API:-http://127.0.0.1:8080}${path}"
	local -a args=(-sS -X "$method" "$url" -H "Authorization: Bearer $(cat "${JWT_FILE:-/tmp/keeper-dev/archon-alice.jwt}")")
	[[ "${INSECURE_TLS:-0}" == 1 ]] && args+=(-k)
	[[ -n "$body" ]] && args+=(-H "Content-Type: application/json" -d "$body")
	curl "${args[@]}" -w $'\n%{http_code}'
}

# _keeper_api_tsh - curl to localhost:8080 inside the VM via `tsh ssh`. The
# body goes as base64 in the env var BODY_B64 (lesson from autoprov-run2.sh:
# nested quoting drowns). Teleport noise is filtered out with grep.
_keeper_api_tsh() {
	local method="$1" path="$2" body="$3"
	local b64=""
	[[ -n "$body" ]] && b64="$(printf '%s' "$body" | base64 | tr -d '\n')"
	local insecure=""
	[[ "${INSECURE_TLS:-0}" == 1 ]] && insecure="-k"
	TELEPORT_HOME="${TELEPORT_HOME:-/mnt/c/Users/stf20/.tsh}" \
		SSL_CERT_FILE="${SSL_CERT_FILE:-}" \
		tsh ssh "${TSH_NODE:?TSH_NODE not set}" \
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

# _dryrun_synth - synthetic successful response by (method,path), modeling
# keeper's reply-DTO (huma_incarnation_reply.go). Branch order matters:
# /runs/{id} before /runs. Matched on the path WITHOUT the query
# (${path%%\?*}), otherwise `?limit=1` (script-create-mode) falls into the
# catch-all and `.items` is empty -> a false "no runs".
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
