#!/usr/bin/env bash
# Entrypoint of the Soul Stack cloud live-E2E orchestrator (NIM-31): parameters → preflight
# → suite → incremental report → exit-code. The only network boundary is
# lib/keeper-api.sh::keeper_api; classify/poll/assert/report are pure (guard tests run offline).
#
# ⚠️ This is NOT `make e2e-live` (the local docker gate), but the cloud path via teleport.
# Bring-up scripts (WB-specific) live locally in $SCRIPTS_DIR and are NOT committed to git -
# the runner invokes them at runtime. Credentials come only from env/VM, NEVER from ~/.zsh_wb.
#
# Usage: [ENV...] runbook.sh <create|create-destroy|day2>
#   DRY_RUN=1 - print the call sequence + a report skeleton without network access.
#   Full parameter list - docs/testing (docs-writer) and delegation NIM-31.

set -uo pipefail

SELF_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# --- parameters (env, defaults) ---
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
# optional (declared for set -u safety).
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
. "${SELF_DIR}/suites/day2.sh"
. "${SELF_DIR}/suites/create.sh"
. "${SELF_DIR}/suites/create-destroy.sh"

SUITE="${1:-${SUITE:-create-destroy}}"

report_init "$SUITE"

preflight
if [[ $? -ne 0 ]]; then
	report_step "preflight" - "$(_utc_now)" - - - "external preconditions not met" FAIL
	report_summary "FAIL (preflight)" 2
	exit 2
fi

case "$SUITE" in
create) suite_create; rc=$? ;;
create-destroy) suite_create_destroy; rc=$? ;;
day2) suite_day2; rc=$? ;;
*)
	_e2e_log "unknown suite: ${SUITE} (create|create-destroy|day2)"
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
