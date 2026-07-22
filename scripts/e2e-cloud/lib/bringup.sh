#!/usr/bin/env bash
# Runs a configurable ordered list of bring-up scripts ($E2E_BRINGUP_STEPS
# from $SCRIPTS_DIR). The sequence is NOT hardcoded (local-track and cloud-track keeper
# differ; canonical lists live in the runbook doc). Stops on the first non-zero.

# run_bringup -> 0 all steps ok / 1 first one failed.
run_bringup() {
	local scripts_dir="${SCRIPTS_DIR:-.pm/scripts}"
	local log_dir="${LOG_DIR:-${REPORT_DIR:-.pm/e2e-reports}/logs}"
	[[ -z "${E2E_BRINGUP_STEPS:-}" ]] && { _e2e_log "bring-up: no steps - skipping"; return 0; }
	mkdir -p "$log_dir"
	local step start rc t0
	for step in ${E2E_BRINGUP_STEPS}; do
		if [[ "${DRY_RUN:-0}" == 1 ]]; then
			_e2e_log "  [dry-run] bring-up: ${scripts_dir}/${step}"
			report_step "bring-up: ${step}" - "$(_utc_now)" 0 - - "dry-run" SKIP
			continue
		fi
		if [[ ! -x "${scripts_dir}/${step}" ]]; then
			report_step "bring-up: ${step}" - "$(_utc_now)" - - - "not executable" FAIL
			return 1
		fi
		start="$(_utc_now)"; t0=$SECONDS
		"${scripts_dir}/${step}" >"${log_dir}/${step}.log" 2>&1
		rc=$?
		if [[ $rc -eq 0 ]]; then
			report_step "bring-up: ${step}" - "$start" "$((SECONDS - t0))" - - "rc=0" PASS
		else
			report_step "bring-up: ${step}" - "$start" "$((SECONDS - t0))" - - "rc=${rc} (log ${log_dir}/${step}.log)" FAIL
			return 1
		fi
	done
	return 0
}
