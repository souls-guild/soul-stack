#!/usr/bin/env bash
# Entrypoint облачного live-E2E оркестратора Soul Stack (NIM-31): параметры → preflight
# → suite → инкрементальный отчёт → exit-code. Единственная сетевая граница —
# lib/keeper-api.sh::keeper_api; classify/poll/assert/report чисты (guard-тесты офлайн).
#
# ⚠️ Это НЕ `make e2e-live` (локальный docker-гейт), а облачный путь через teleport.
# Bring-up-скрипты (WB-специфика) живут локально в $SCRIPTS_DIR, в git НЕ коммитятся —
# раннер зовёт их рантайм. Креды — только из env/VM, НИКОГДА из ~/.zsh_wb.
#
# Usage: [ENV...] runbook.sh <create|create-destroy|operations>
#   DRY_RUN=1 — печать последовательности вызовов + отчёт-скелет без сети.
#   Полный список параметров — docs/testing (docs-writer) и delegation NIM-31.

set -uo pipefail

SELF_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# --- параметры (env, дефолты) ---
: "${EXEC_MODE:=tsh}"
: "${KEEPER_API:=http://127.0.0.1:8080}"
: "${FQDN_SUFFIX:=fedorovstepan2-dev.vm.xc.clv3}"
: "${TSH_NODE:=root@soul-keeper-1.${FQDN_SUFFIX}}"
: "${TELEPORT_HOME:=/mnt/c/Users/stf20/.tsh}"
: "${REMOTE_JWT:=/opt/soul-stack/archon-cloud.jwt}"
: "${REMOTE_KEEPER_API:=http://localhost:8080}"
: "${JWT_FILE:=/tmp/keeper-dev/archon-alice.jwt}"
: "${INCARNATION:=redis-auto}"
: "${SERVICE:=example-cloud-bootstrap}"
: "${CREATE_SCENARIO:=create}"
: "${PROVIDER:=wb-prod}"
: "${PROFILE:=redis-debian-12}"
: "${SCRIPTS_DIR:=.pm/scripts}"
: "${ARTIFACTS_DIR:=/opt/soul-stack}"
: "${REPORT_DIR:=.pm/e2e-reports}"
: "${LOG_DIR:=${REPORT_DIR}/logs}"
: "${POLL_INTERVAL:=30}"
: "${POLL_MAX:=40}"
: "${AID:=archon-alice}"
: "${E2E_CREATE_MODE:=engine}"
: "${ALLOW_DESTROY:=true}"
: "${DRY_RUN:=0}"
: "${INSECURE_TLS:=0}"
# опциональные (объявляем для set -u безопасности).
: "${E2E_BRINGUP_STEPS:=}"
: "${COVENS:=}"
: "${E2E_CREATE_INPUT:=}"
: "${SCENARIO:=}"
: "${SCENARIO_INPUT:=}"
: "${SCENARIOS:=}"
: "${STATE_ASSERT_PATH:=}"
: "${STATE_ASSERT_EXPECTED:=}"
: "${HEALTHY_TERMINAL:=ready}"
export EXEC_MODE KEEPER_API FQDN_SUFFIX TSH_NODE TELEPORT_HOME REMOTE_JWT REMOTE_KEEPER_API \
	JWT_FILE INCARNATION SERVICE CREATE_SCENARIO PROVIDER PROFILE SCRIPTS_DIR ARTIFACTS_DIR \
	REPORT_DIR LOG_DIR POLL_INTERVAL POLL_MAX AID E2E_CREATE_MODE ALLOW_DESTROY DRY_RUN \
	INSECURE_TLS E2E_BRINGUP_STEPS COVENS E2E_CREATE_INPUT SCENARIO SCENARIO_INPUT SCENARIOS \
	STATE_ASSERT_PATH STATE_ASSERT_EXPECTED HEALTHY_TERMINAL

# shellcheck source=lib/keeper-api.sh
. "${SELF_DIR}/lib/keeper-api.sh"
. "${SELF_DIR}/lib/poll.sh"
. "${SELF_DIR}/lib/assert.sh"
. "${SELF_DIR}/lib/report.sh"
. "${SELF_DIR}/lib/preflight.sh"
. "${SELF_DIR}/lib/bringup.sh"
. "${SELF_DIR}/suites/operations.sh"
. "${SELF_DIR}/suites/create.sh"
. "${SELF_DIR}/suites/create-destroy.sh"

SUITE="${1:-${SUITE:-create-destroy}}"

report_init "$SUITE"

preflight
if [[ $? -ne 0 ]]; then
	report_step "preflight" - "$(_utc_now)" - - - "внешние предусловия не выполнены" FAIL
	report_summary "FAIL (preflight)" 2
	exit 2
fi

case "$SUITE" in
create) suite_create; rc=$? ;;
create-destroy) suite_create_destroy; rc=$? ;;
operations) suite_operations; rc=$? ;;
*)
	_e2e_log "неизвестный suite: ${SUITE} (create|create-destroy|operations)"
	report_summary "FAIL (bad suite)" 2
	exit 2
	;;
esac

if [[ $rc -eq 0 ]]; then
	report_summary "PASS" 0
	exit 0
elif [[ $rc -eq 2 ]]; then
	report_summary "FAIL (preflight/infra)" 2
	exit 2
else
	report_summary "FAIL" 1
	exit 1
fi
