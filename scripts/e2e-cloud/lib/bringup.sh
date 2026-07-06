#!/usr/bin/env bash
# Прогон конфигурируемого упорядоченного списка bring-up-скрутов ($E2E_BRINGUP_STEPS
# из $SCRIPTS_DIR). Последовательность НЕ хардкодится (local-track и cloud-track keeper
# различаются; канонические списки — в runbook-доке). Стоп при первом non-zero.

# run_bringup → 0 все шаги ок / 1 первый упавший.
run_bringup() {
	local scripts_dir="${SCRIPTS_DIR:-.pm/scripts}"
	local log_dir="${LOG_DIR:-${REPORT_DIR:-.pm/e2e-reports}/logs}"
	[[ -z "${E2E_BRINGUP_STEPS:-}" ]] && { _e2e_log "bring-up: шагов нет — пропуск"; return 0; }
	mkdir -p "$log_dir"
	local step start rc t0
	for step in ${E2E_BRINGUP_STEPS}; do
		if [[ "${DRY_RUN:-0}" == 1 ]]; then
			_e2e_log "  [dry-run] bring-up: ${scripts_dir}/${step}"
			report_step "bring-up: ${step}" - "$(_utc_now)" 0 - - "dry-run" SKIP
			continue
		fi
		if [[ ! -x "${scripts_dir}/${step}" ]]; then
			report_step "bring-up: ${step}" - "$(_utc_now)" - - - "не исполняем" FAIL
			return 1
		fi
		start="$(_utc_now)"; t0=$SECONDS
		"${scripts_dir}/${step}" >"${log_dir}/${step}.log" 2>&1
		rc=$?
		if [[ $rc -eq 0 ]]; then
			report_step "bring-up: ${step}" - "$start" "$((SECONDS - t0))" - - "rc=0" PASS
		else
			report_step "bring-up: ${step}" - "$start" "$((SECONDS - t0))" - - "rc=${rc} (лог ${log_dir}/${step}.log)" FAIL
			return 1
		fi
	done
	return 0
}
