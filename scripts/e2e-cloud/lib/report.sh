#!/usr/bin/env bash
# Incremental run report into $REPORT_DIR/<date>-<suite>.md: each line is
# written immediately (so the report is not empty on abort). "Environment"
# header + steps table + summary. report_step tracks the PASS/FAIL/SKIP
# counters by verdict.

E2E_STEP_NO=0
E2E_PASS=0
E2E_FAIL=0
E2E_SKIP=0
E2E_REPORT_FILE=""

# report_init <suite> - open the report (truncate), write the header and the table header.
report_init() {
	local suite="$1"
	local dir="${REPORT_DIR:-.pm/e2e-reports}"
	mkdir -p "$dir"
	E2E_REPORT_FILE="${dir}/$(date -u +%Y-%m-%d)-${suite}.md"
	E2E_STEP_NO=0; E2E_PASS=0; E2E_FAIL=0; E2E_SKIP=0
	local canon
	canon="$(git -C "$(dirname "${BASH_SOURCE[0]}")" rev-parse --short HEAD 2>/dev/null || echo "${CANON:-unknown}")"
	{
		echo "# E2E-cloud run: ${suite}"
		echo
		echo "## Environment"
		echo
		echo "- **suite**: ${suite}"
		echo "- **started (UTC)**: $(_utc_now)"
		echo "- **exec_mode**: ${EXEC_MODE:-tsh}${DRY_RUN:+ (DRY_RUN=${DRY_RUN})}"
		[[ "${DRY_RUN:-0}" == 1 ]] && echo "- **MODE**: DRY-RUN - network untouched, responses are synthetic"
		echo "- **endpoint**: $([[ "${EXEC_MODE:-tsh}" == tsh ]] && echo "tsh ${TSH_NODE:-?} → ${REMOTE_KEEPER_API:-http://localhost:8080}" || echo "${KEEPER_API:-http://127.0.0.1:8080}")"
		echo "- **incarnation / service**: ${INCARNATION:-redis-auto} / ${SERVICE:-example-cloud-bootstrap}"
		echo "- **provider / profile**: ${PROVIDER:-example-prod} / ${PROFILE:-redis-debian-12}"
		echo "- **canon (core)**: ${canon}"
		echo "- **operator (aid)**: ${AID:-archon-alice}"
		echo "- **bring-up steps**: ${E2E_BRINGUP_STEPS:-(none)}"
		echo
		echo "## Steps"
		echo
		echo "| # | step / scenario | apply_id | start (UTC) | dur,s | http | run_status | assert | verdict |"
		echo "|---|---|---|---|---|---|---|---|---|"
	} >"$E2E_REPORT_FILE"
	_e2e_log "report: ${E2E_REPORT_FILE}"
}

# report_step <step> <apply_id> <start_utc> <dur_s> <http> <run_status> <assert> <verdict>
report_step() {
	local step="$1" apply_id="${2:--}" start="${3:--}" dur="${4:--}" http="${5:--}" rstatus="${6:--}" assert="${7:--}" verdict="$8"
	E2E_STEP_NO=$((E2E_STEP_NO + 1))
	case "$verdict" in
	PASS) E2E_PASS=$((E2E_PASS + 1)) ;;
	FAIL) E2E_FAIL=$((E2E_FAIL + 1)) ;;
	SKIP) E2E_SKIP=$((E2E_SKIP + 1)) ;;
	esac
	printf '| %d | %s | %s | %s | %s | %s | %s | %s | %s |\n' \
		"$E2E_STEP_NO" "$step" "${apply_id:--}" "$start" "$dur" "$http" "$rstatus" "$assert" "$verdict" \
		>>"$E2E_REPORT_FILE"
	_e2e_log "  [$verdict] step ${E2E_STEP_NO}: ${step} (http=${http} run_status=${rstatus})"
}

# report_summary <result> <exit_code> - append the summary.
report_summary() {
	local result="$1" exit_code="$2"
	{
		echo
		echo "## Summary"
		echo
		echo "- **PASS**: ${E2E_PASS}"
		echo "- **FAIL**: ${E2E_FAIL}"
		echo "- **SKIP**: ${E2E_SKIP}"
		echo "- **RESULT**: ${result}"
		echo "- **exit-code**: ${exit_code}"
	} >>"$E2E_REPORT_FILE"
	_e2e_log "result: ${result} (PASS=${E2E_PASS} FAIL=${E2E_FAIL} SKIP=${E2E_SKIP}, exit=${exit_code})"
}
