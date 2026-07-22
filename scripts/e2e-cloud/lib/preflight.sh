#!/usr/bin/env bash
# Gate BEFORE touching the cloud (failure -> exit 2 in the runbook). Only a PRESENCE
# check, no building: local tooling (jq/curl), teleport login when EXEC_MODE=tsh,
# executability of bring-up scripts and presence of pre-built artifacts. Prints a
# checklist ✓/✗.

# preflight -> 0 all ok / 2 failure.
preflight() {
	local fails=0
	_e2e_log "preflight:"

	if [[ "${DRY_RUN:-0}" == 1 ]]; then
		command -v jq >/dev/null 2>&1 && _e2e_log "  ✓ jq" || { _e2e_log "  ✗ jq (not in PATH)"; fails=1; }
		_e2e_log "  · DRY-RUN: cloud checks skipped"
		[[ $fails -eq 0 ]] && return 0 || return 2
	fi

	command -v jq >/dev/null 2>&1 && _e2e_log "  ✓ jq" || { _e2e_log "  ✗ jq (not in PATH)"; fails=1; }
	command -v curl >/dev/null 2>&1 && _e2e_log "  ✓ curl" || { _e2e_log "  ✗ curl (not in PATH)"; fails=1; }

	if [[ "${EXEC_MODE:-tsh}" == local ]]; then
		if [[ -r "${JWT_FILE:-/tmp/keeper-dev/archon-alice.jwt}" ]]; then
			_e2e_log "  ✓ JWT_FILE readable (${JWT_FILE:-/tmp/keeper-dev/archon-alice.jwt})"
		else
			_e2e_log "  ✗ JWT_FILE unavailable (${JWT_FILE:-/tmp/keeper-dev/archon-alice.jwt})"; fails=1
		fi
	fi

	if [[ "${EXEC_MODE:-tsh}" == tsh ]]; then
		if command -v tsh >/dev/null 2>&1; then
			_e2e_log "  ✓ tsh"
			if TELEPORT_HOME="${TELEPORT_HOME:-/mnt/c/Users/stf20/.tsh}" tsh status >/dev/null 2>&1; then
				_e2e_log "  ✓ teleport login is fresh (tsh status ok)"
				local proxy
				proxy="$(TELEPORT_HOME="${TELEPORT_HOME:-/mnt/c/Users/stf20/.tsh}" tsh status 2>/dev/null | awk -F'[ :]+' '/Proxy/{print $2; exit}')"
				if [[ -n "$proxy" ]]; then
					getent hosts "$proxy" >/dev/null 2>&1 && _e2e_log "  ✓ proxy resolves ($proxy)" || _e2e_log "  · proxy '$proxy' does not resolve via getent (may be DNS/hosts outside getent)"
				fi
			else
				_e2e_log "  ✗ teleport login expired/missing (tsh status != 0) - run tsh login"; fails=1
			fi
		else
			_e2e_log "  ✗ tsh (not in PATH)"; fails=1
		fi
	fi

	# bring-up: scripts are executable + artifacts are in place (only when the step list is non-empty).
	if [[ -n "${E2E_BRINGUP_STEPS:-}" ]]; then
		local scripts_dir="${SCRIPTS_DIR:-.pm/scripts}" step
		for step in ${E2E_BRINGUP_STEPS}; do
			if [[ -x "${scripts_dir}/${step}" ]]; then
				_e2e_log "  ✓ bring-up script is executable: ${scripts_dir}/${step}"
			else
				_e2e_log "  ✗ bring-up script not found/not executable: ${scripts_dir}/${step}"; fails=1
			fi
		done
		local art_dir="${ARTIFACTS_DIR:-/opt/soul-stack}" art
		for art in ${E2E_ARTIFACTS:-soul-cloud-example-linux soul-mod-redis mod-manifest.yaml}; do
			if [[ -e "${art_dir}/${art}" ]]; then
				_e2e_log "  ✓ artifact is in place: ${art_dir}/${art}"
			else
				_e2e_log "  ✗ artifact missing (pre-build it, this script does not build): ${art_dir}/${art}"; fails=1
			fi
		done
	fi

	[[ $fails -eq 0 ]] && return 0 || return 2
}
