#!/usr/bin/env bash
# Asserts for run success and state fields. Pure (jq over the JSON body, no network).
# assert_run_success - the source of truth: success iff the aggregate .status==success AND every
# .hosts[].status is in {success, no_match}. no_match is a benign terminal state (targeting
# a subset: add_user on master -> replicas get no_match), counted as success by the
# backend too (applyrun.AggregateRunStatus, runsview.go). On failure - forensics come from
# failed_task_idx / error_summary of the actually-failed hosts (no_match isn't listed).

# assert_run_success <run_json> -> 0 on full success, otherwise 1 + forensics on stderr.
assert_run_success() {
	local json="$1"
	if printf '%s' "$json" | jq -e '.status=="success" and all((.hosts // [])[]; .status=="success" or .status=="no_match")' >/dev/null 2>&1; then
		return 0
	fi
	local agg
	agg="$(printf '%s' "$json" | jq -r '.status // "?"' 2>/dev/null)"
	_e2e_log "    ✗ run is NOT success: aggregate .status=${agg}"
	printf '%s' "$json" | jq -r '
		.hosts[]? | select(.status != "success" and .status != "no_match")
		| "    ✗ host=\(.sid) status=\(.status) failed_task_idx=\(.failed_task_idx // "-") plan_index=\(.failed_plan_index // "-") error=\(.error_summary // "-")"' 2>/dev/null >&2
	return 1
}

# assert_state_field <inc_json> <jq-path> <expected> -> 0 if at least one extracted
# jq value equals expected (contains semantics for streams like
# .state.users[]?.name), otherwise 1. Example: assert_state_field "$inc" '.state.users[]?.name' alice
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
	_e2e_log "    ✗ state field '${path}': expected '${expected}', not found among values ${got:-[]}"
	return 1
}
