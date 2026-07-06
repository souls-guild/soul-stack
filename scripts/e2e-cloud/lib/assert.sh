#!/usr/bin/env bash
# Ассерты успеха прогона и полей state. Чисто (jq над JSON-телом, без сети).
# assert_run_success — источник истины: успех ⟺ агрегат .status==success И каждый
# .hosts[].status ∈ {success, no_match}. no_match — benign-терминал (таргетинг
# подмножества: add_user на master → реплики no_match), считается успехом и
# бэкендом (applyrun.AggregateRunStatus, runsview.go). На фейле — форензика из
# failed_task_idx / error_summary реально упавших хостов (no_match не листится).

# assert_run_success <run_json> → 0 при полном успехе, иначе 1 + форензика в stderr.
assert_run_success() {
	local json="$1"
	if printf '%s' "$json" | jq -e '.status=="success" and all((.hosts // [])[]; .status=="success" or .status=="no_match")' >/dev/null 2>&1; then
		return 0
	fi
	local agg
	agg="$(printf '%s' "$json" | jq -r '.status // "?"' 2>/dev/null)"
	_e2e_log "    ✗ прогон НЕ success: агрегат .status=${agg}"
	printf '%s' "$json" | jq -r '
		.hosts[]? | select(.status != "success" and .status != "no_match")
		| "    ✗ host=\(.sid) status=\(.status) failed_task_idx=\(.failed_task_idx // "-") plan_index=\(.failed_plan_index // "-") error=\(.error_summary // "-")"' 2>/dev/null >&2
	return 1
}

# assert_state_field <inc_json> <jq-path> <expected> → 0 если хоть одно извлечённое
# jq-значение равно expected (contains-семантика для стримов вроде
# .state.users[]?.name), иначе 1. Пример: assert_state_field "$inc" '.state.users[]?.name' alice
assert_state_field() {
	local json="$1" path="$2" expected="$3" v found=1
	while IFS= read -r v; do
		[[ "$v" == "$expected" ]] && { found=0; break; }
	done < <(printf '%s' "$json" | jq -r "$path" 2>/dev/null)
	if [[ $found -eq 0 ]]; then
		return 0
	fi
	local got
	got="$(printf '%s' "$json" | jq -rc "[$path]" 2>/dev/null)"
	_e2e_log "    ✗ state-поле '${path}': ожидал '${expected}', среди значений ${got:-[]} не найдено"
	return 1
}
