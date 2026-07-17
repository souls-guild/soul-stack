package render

import (
	"errors"
	"fmt"
)

// ErrOnChangesUnknownRegister — `onchanges:` references a register name that
// doesn't exist among the run's task registers. Strict variant (error, not
// warn): a typo'd empty onchanges source would silently turn gating into
// "never changed" → the task would always be skipped, masking the scenario
// author's bug.
var ErrOnChangesUnknownRegister = errors.New("render: onchanges references a non-existent register")

// ErrOnFailUnknownRegister — `onfail:` references a register name that
// doesn't exist among the run's task registers. Mirrors
// ErrOnChangesUnknownRegister: a typo'd empty onfail source would silently
// turn rescue gating into "never failed" → the onfail task would always be
// skipped, masking the scenario author's bug.
var ErrOnFailUnknownRegister = errors.New("render: onfail references a non-existent register")

// resolveOnChanges turns `onchanges:` register names (RenderedTask.
// onChangesNames) into task indexes (RenderedTask.OnChangesIdx) across the
// whole run plan (Variant A: resolved on Keeper, Soul operates on indexes).
// Called by [Pipeline.Render]'s final pass, once the plan is fully assembled
// and all Index/Register values are known (apply:destiny/loop produce
// contiguous indexes).
//
// The register-name → Index map is built over all plan tasks; a self-
// reference (`onchanges: [self]` on a task with `register: self`) isn't
// special-cased — it resolves to its own Index, and Soul gating on its own
// (not-yet-executed) register yields changed==false → skip. This matches the
// "requisites are checked before the task runs" ordering.
//
// Unknown name → [ErrOnChangesUnknownRegister] (strict variant, catches a
// typo'd register id). Empty onChangesNames → OnChangesIdx stays nil
// (unconditional run).
func resolveOnChanges(tasks []*RenderedTask) error {
	byRegister := registerIndex(tasks)
	for _, t := range tasks {
		if len(t.onChangesNames) == 0 {
			continue
		}
		idxs, err := resolveRegisterNames(byRegister, t.onChangesNames, t.Name, "onchanges", ErrOnChangesUnknownRegister)
		if err != nil {
			return err
		}
		t.OnChangesIdx = idxs
	}
	return nil
}

// resolveOnFail turns `onfail:` register names (RenderedTask.onFailNames)
// into task indexes (RenderedTask.OnFailIdx) across the whole run plan. Full
// mirror of resolveOnChanges (Variant A): the only difference is Soul-side
// gating semantics — onfail fires on the source's register.failed (rescue),
// not register.changed.
//
// Unknown name → [ErrOnFailUnknownRegister]. Empty onFailNames → OnFailIdx
// stays nil (not an onfail task, gating doesn't apply).
func resolveOnFail(tasks []*RenderedTask) error {
	byRegister := registerIndex(tasks)
	for _, t := range tasks {
		if len(t.onFailNames) == 0 {
			continue
		}
		idxs, err := resolveRegisterNames(byRegister, t.onFailNames, t.Name, "onfail", ErrOnFailUnknownRegister)
		if err != nil {
			return err
		}
		t.OnFailIdx = idxs
	}
	return nil
}

// registerIndex builds a register-name → Index map over all plan tasks.
// Tasks without register: are excluded from the map (addressed only by their
// own idx).
func registerIndex(tasks []*RenderedTask) map[string]int {
	byRegister := make(map[string]int, len(tasks))
	for _, t := range tasks {
		if t.Register != "" {
			byRegister[t.Register] = t.Index
		}
	}
	return byRegister
}

// resolveRegisterNames resolves a requisite's list of register names into
// task indexes via the byRegister map. Unknown name → wrapped sentinel error
// unknownErr with coordinates (task name, requisite kind, the name itself).
// kind is "onchanges"/"onfail" for the error text.
func resolveRegisterNames(byRegister map[string]int, names []string, taskName, kind string, unknownErr error) ([]int, error) {
	idxs := make([]int, 0, len(names))
	for _, name := range names {
		srcIdx, ok := byRegister[name]
		if !ok {
			return nil, fmt.Errorf("%w: task %q -> %s: [%s] (no task with register: %s)",
				unknownErr, taskName, kind, name, name)
		}
		idxs = append(idxs, srcIdx)
	}
	return idxs, nil
}
