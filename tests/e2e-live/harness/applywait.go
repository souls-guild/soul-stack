package harness

// applyRunTerminalFailures — non-success terminals of apply_runs (keeper crud.go):
// the task failed permanently → WaitApplySuccess fails immediately with a matrix dump.
var applyRunTerminalFailures = map[string]bool{
	"failed":    true,
	"cancelled": true,
	"orphaned":  true,
	"no_match":  true,
}

// ApplyRunRow — a snapshot of an apply_runs row (PK = apply_id+sid) used to decide WaitApplySuccess.
type ApplyRunRow struct {
	SID    string
	Status string
}

// applySettled decides, from a snapshot of run rows plus the "apply still in
// flight" flag (incarnation.applying_apply_id == applyID), whether a
// SUCCESSFUL terminal has been reached.
//
// NIM-46: apply_runs fills incrementally — the keeper row (sid="keeper",
// on:keeper tasks) is inserted and reaches success STRICTLY BEFORE soul rows
// are planned (run.go: keeper-tasks → host-dispatch). So "all visible rows are
// success" alone does NOT mean "the run is complete" — soul rows might not
// have appeared yet (the NIM-45 race). The authoritative signal that "keeper
// won't insert any more rows" is the release of the apply bracket
// applying_apply_id (set at apply start by lockApplyingWithEpochSQL before
// dispatch, released at the single terminal point UpdateStateFromRun). While
// the bracket holds THIS applyID — keep waiting, even if all visible rows are
// success.
//
// Return: done — successful terminal; on a terminal failure — (false, sid, status)
// for the caller's diagnostic Fatal.
func applySettled(rows []ApplyRunRow, applyInFlight bool) (done bool, failSID, failStatus string) {
	if len(rows) == 0 {
		return false, "", "" // no rows yet — waiting for the keeper row to be inserted
	}
	allSuccess := true
	for _, r := range rows {
		if r.Status == "success" {
			continue
		}
		if applyRunTerminalFailures[r.Status] {
			return false, r.SID, r.Status
		}
		allSuccess = false // planned/claimed/running/dispatched — still in progress
	}
	if !allSuccess {
		return false, "", ""
	}
	// All rows success — done only if the bracket is released (otherwise keeper
	// hasn't finished planning soul rows in the keeper window, see the invariant above).
	return !applyInFlight, "", ""
}
