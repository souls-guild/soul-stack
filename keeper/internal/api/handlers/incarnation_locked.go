package handlers

import (
	"strconv"
	"strings"
)

// ErrorLockedDetail is the operator sentence for a run refused because the
// incarnation is error_locked: which run locked it and the two ways out. The
// single-incarnation run route, a Voyage refused at creation and a Voyage target
// refused at spawn (voyage_targets.error_message) all say the same thing.
// lockingApplyID "" — the row does not name the run.
func ErrorLockedDetail(id, lockingApplyID string) string {
	return "incarnation " + id + " is error_locked" + lockedBy(lockingApplyID) +
		": unlock it (POST /v1/incarnations/" + id + "/unlock) or rerun its last run (POST /v1/incarnations/" +
		id + "/rerun-last) first"
}

// LockedIncarnation is one error_locked incarnation of a refused Voyage target.
type LockedIncarnation struct {
	ID             string
	LockingApplyID string
}

// maxLockedNamed caps how many locked incarnations a refusal names; a broad
// target can resolve to the whole max_scope.
const maxLockedNamed = 10

// errorLockedTargetDetail is the refusal of a Voyage whose resolved target holds
// error_locked incarnations. locked is non-empty.
func errorLockedTargetDetail(locked []LockedIncarnation) string {
	if len(locked) == 1 {
		return ErrorLockedDetail(locked[0].ID, locked[0].LockingApplyID)
	}
	named := locked
	if len(named) > maxLockedNamed {
		named = named[:maxLockedNamed]
	}
	parts := make([]string, len(named))
	for i, l := range named {
		parts[i] = l.ID + lockedBy(l.LockingApplyID)
	}
	list := strings.Join(parts, ", ")
	if rest := len(locked) - len(named); rest > 0 {
		list += " and " + strconv.Itoa(rest) + " more"
	}
	return strconv.Itoa(len(locked)) + " target incarnations are error_locked: " + list +
		"; unlock each (POST /v1/incarnations/{id}/unlock) or rerun its last run (POST /v1/incarnations/{id}/rerun-last), or leave it out of the target"
}

func lockedBy(applyID string) string {
	if applyID == "" {
		return ""
	}
	return " by run " + applyID
}
