#!/usr/bin/env bash
# Гейт ДО касания облака (провал → exit 2 в runbook). Только ПРОВЕРКА наличия, без
# сборки: локальный инструментарий (jq/curl), teleport-логин при EXEC_MODE=tsh,
# исполняемость bring-up-скриптов и наличие пред-собранных артефактов. Печатает
# чеклист ✓/✗.

# preflight → 0 всё ок / 2 провал.
preflight() {
	local fails=0
	_e2e_log "preflight:"

	if [[ "${DRY_RUN:-0}" == 1 ]]; then
		command -v jq >/dev/null 2>&1 && _e2e_log "  ✓ jq" || { _e2e_log "  ✗ jq (нет в PATH)"; fails=1; }
		_e2e_log "  · DRY-RUN: облачные проверки пропущены"
		[[ $fails -eq 0 ]] && return 0 || return 2
	fi

	command -v jq >/dev/null 2>&1 && _e2e_log "  ✓ jq" || { _e2e_log "  ✗ jq (нет в PATH)"; fails=1; }
	command -v curl >/dev/null 2>&1 && _e2e_log "  ✓ curl" || { _e2e_log "  ✗ curl (нет в PATH)"; fails=1; }

	if [[ "${EXEC_MODE:-tsh}" == local ]]; then
		if [[ -r "${JWT_FILE:-/tmp/keeper-dev/archon-alice.jwt}" ]]; then
			_e2e_log "  ✓ JWT_FILE читаем (${JWT_FILE:-/tmp/keeper-dev/archon-alice.jwt})"
		else
			_e2e_log "  ✗ JWT_FILE недоступен (${JWT_FILE:-/tmp/keeper-dev/archon-alice.jwt})"; fails=1
		fi
	fi

	if [[ "${EXEC_MODE:-tsh}" == tsh ]]; then
		if command -v tsh >/dev/null 2>&1; then
			_e2e_log "  ✓ tsh"
			if TELEPORT_HOME="${TELEPORT_HOME:-/mnt/c/Users/stf20/.tsh}" tsh status >/dev/null 2>&1; then
				_e2e_log "  ✓ teleport-логин свеж (tsh status ok)"
				local proxy
				proxy="$(TELEPORT_HOME="${TELEPORT_HOME:-/mnt/c/Users/stf20/.tsh}" tsh status 2>/dev/null | awk -F'[ :]+' '/Proxy/{print $2; exit}')"
				if [[ -n "$proxy" ]]; then
					getent hosts "$proxy" >/dev/null 2>&1 && _e2e_log "  ✓ proxy резолвится ($proxy)" || _e2e_log "  · proxy '$proxy' не резолвится через getent (может, DNS/hosts вне getent)"
				fi
			else
				_e2e_log "  ✗ teleport-логин протух/нет (tsh status != 0) — сделай tsh login"; fails=1
			fi
		else
			_e2e_log "  ✗ tsh (нет в PATH)"; fails=1
		fi
	fi

	# bring-up: скрипты исполняемы + артефакты на месте (только при непустом списке шагов).
	if [[ -n "${E2E_BRINGUP_STEPS:-}" ]]; then
		local scripts_dir="${SCRIPTS_DIR:-.pm/scripts}" step
		for step in ${E2E_BRINGUP_STEPS}; do
			if [[ -x "${scripts_dir}/${step}" ]]; then
				_e2e_log "  ✓ bring-up скрипт исполняем: ${scripts_dir}/${step}"
			else
				_e2e_log "  ✗ bring-up скрипт не найден/не исполняем: ${scripts_dir}/${step}"; fails=1
			fi
		done
		local art_dir="${ARTIFACTS_DIR:-/opt/soul-stack}" art
		for art in ${E2E_ARTIFACTS:-soul-cloud-wb-linux soul-mod-redis mod-manifest.yaml}; do
			if [[ -e "${art_dir}/${art}" ]]; then
				_e2e_log "  ✓ артефакт на месте: ${art_dir}/${art}"
			else
				_e2e_log "  ✗ артефакт отсутствует (пред-собери, я не собираю): ${art_dir}/${art}"; fails=1
			fi
		done
	fi

	[[ $fails -eq 0 ]] && return 0 || return 2
}
