package statemigrate

import (
	"fmt"
	"strings"
)

// applyOps applies a list of operations sequentially to the mutable state.
// Each operation sees the state as modified by previous ones (same migration/same
// foreach-do). scope carries the root state and active foreach variables.
//
// state — root of incarnation.state (mutated in place; deep-copy is done in
// [Apply] before the first operation, the caller's map is not touched).
func applyOps(ops []Op, state map[string]any, ev Evaluator, loop map[string]any) error {
	scope := Scope{State: state, Loop: loop}
	for i := range ops {
		if err := applyOp(ops[i], state, ev, scope); err != nil {
			return err
		}
	}
	return nil
}

// applyOp dispatches a single operation by its discriminator.
func applyOp(op Op, state map[string]any, ev Evaluator, scope Scope) error {
	switch {
	case op.Rename != nil:
		return applyRename(op.Rename, state, ev, scope)
	case op.Set != nil:
		return applySet(op.Set, state, ev, scope)
	case op.Delete != nil:
		return applyDelete(op.Delete, state, ev, scope)
	case op.Foreach != nil:
		return applyForeach(op.Foreach, state, ev, scope)
	default:
		// Discriminator is guaranteed by the parser; this guards against a programming error.
		return &EvalError{Class: ClassPathSegment, Msg: "empty operation (no discriminator)"}
	}
}

// applyRename moves a value from → to (move = the same code path). Missing
// source → nothing to move (no-op); existing to → error
// (explicit delete before rename, [docs/migrations.md]).
func applyRename(op *RenameOp, state map[string]any, ev Evaluator, scope Scope) error {
	fromKeys, err := resolvePath(op.From, ev, scope)
	if err != nil {
		return err
	}
	toKeys, err := resolvePath(op.To, ev, scope)
	if err != nil {
		return err
	}

	val, ok, err := getPath(state, fromKeys)
	if err != nil {
		return err
	}
	if !ok {
		// No source — nothing to move (no-op, symmetric to delete).
		return nil
	}

	if _, exists, err := getPath(state, toKeys); err != nil {
		return err
	} else if exists {
		return &EvalError{Class: ClassRenameToExists, Path: op.To, Msg: "rename/move target path already exists (explicit delete required)"}
	}

	if err := setPath(state, toKeys, val); err != nil {
		return err
	}
	return deletePath(state, fromKeys)
}

// applySet writes value to path (with overwrite). value is recursively
// interpolated: each string leaf with embedded `${ … }` is resolved
// via Evaluator (a narrow copy of the render-pipeline logic, keeping the core clean).
func applySet(op *SetOp, state map[string]any, ev Evaluator, scope Scope) error {
	keys, err := resolvePath(op.Path, ev, scope)
	if err != nil {
		return err
	}
	val, err := interpolateValue(op.Value, ev, scope)
	if err != nil {
		return err
	}
	return setPath(state, keys, val)
}

// applyDelete removes the value at path. Non-existent path → no-op.
func applyDelete(op *DeleteOp, state map[string]any, ev Evaluator, scope Scope) error {
	keys, err := resolvePath(op.Path, ev, scope)
	if err != nil {
		return err
	}
	return deletePath(state, keys)
}

// applyForeach iterates over the result of In (list → element, map → value),
// binds As, and recursively applies Do with the updated scope. Nested foreach
// blocks add their As on top of the outer ones (a new loop map per iteration).
func applyForeach(op *ForeachOp, state map[string]any, ev Evaluator, scope Scope) error {
	if op.As == "" {
		return &EvalError{Class: ClassForeachType, Msg: "foreach without as:"}
	}
	coll, err := evalCollection(op.In, ev, scope)
	if err != nil {
		return &EvalError{Class: ClassCELInterp, Msg: fmt.Sprintf("foreach in: %q", op.In), Err: err}
	}

	items, err := iterItems(coll, op.In)
	if err != nil {
		return err
	}
	for _, item := range items {
		childLoop := make(map[string]any, len(scope.Loop)+1)
		for k, v := range scope.Loop {
			childLoop[k] = v
		}
		childLoop[op.As] = item
		if err := applyOps(op.Do, state, ev, childLoop); err != nil {
			return err
		}
	}
	return nil
}

// evalCollection evaluates foreach.in. The collection expression in fixtures carries
// the `${ … }` marker (docs/migrations.md), so when present we resolve it via
// Interpolate (literal+block, native type). Without the marker we treat the whole string as
// bare CEL (Eval) — tolerating both forms of notation.
func evalCollection(in string, ev Evaluator, scope Scope) (any, error) {
	if strings.Contains(in, "${") {
		return ev.Interpolate(in, scope)
	}
	return ev.Eval(in, scope)
}

// iterItems unpacks the result of foreach.in into an ordered list of
// iterable values: list → its elements (order preserved); map → its
// VALUES. Other types (scalar/null) → ClassForeachType error.
//
// For a map, value order is made deterministic by sorting keys (a migration is
// a pure reproducible function; map iteration in Go is non-deterministic).
func iterItems(coll any, expr string) ([]any, error) {
	switch t := coll.(type) {
	case []any:
		return t, nil
	case map[string]any:
		keys := sortedKeys(t)
		out := make([]any, 0, len(t))
		for _, k := range keys {
			out = append(out, t[k])
		}
		return out, nil
	default:
		return nil, &EvalError{Class: ClassForeachType, Msg: fmt.Sprintf("foreach in: %q gave %T, expected a list or map", expr, coll)}
	}
}

// resolvePath parses and resolves ${ … } segments of the address into a flat list of keys.
func resolvePath(raw string, ev Evaluator, scope Scope) ([]string, error) {
	segs, err := parsePath(raw)
	if err != nil {
		return nil, err
	}
	if len(segs) == 0 {
		return nil, &EvalError{Class: ClassPathSegment, Path: raw, Msg: "empty address (operations on the state root are not supported)"}
	}
	return resolveSegments(segs, ev, scope)
}

// getPath navigates state by keys. Returns (value, found, error).
// An intermediate non-map segment (scalar/list) on a not-yet-fully-traversed path →
// ClassPathTraverse error. Missing key → (nil, false, nil).
func getPath(state map[string]any, keys []string) (any, bool, error) {
	cur := state
	for i, k := range keys {
		v, ok := cur[k]
		if !ok {
			return nil, false, nil
		}
		if i == len(keys)-1 {
			return v, true, nil
		}
		next, ok := v.(map[string]any)
		if !ok {
			return nil, false, &EvalError{Class: ClassPathTraverse, Path: joinKeys(keys[:i+1]), Msg: fmt.Sprintf("intermediate segment is %T, not a map", v)}
		}
		cur = next
	}
	return nil, false, nil
}

// setPath writes val at keys, creating intermediate maps where missing.
// An existing intermediate non-map → ClassPathTraverse error (no silent
// overwrite of foreign structure).
func setPath(state map[string]any, keys []string, val any) error {
	cur := state
	for i, k := range keys {
		if i == len(keys)-1 {
			cur[k] = val
			return nil
		}
		v, ok := cur[k]
		if !ok {
			next := map[string]any{}
			cur[k] = next
			cur = next
			continue
		}
		next, ok := v.(map[string]any)
		if !ok {
			return &EvalError{Class: ClassPathTraverse, Path: joinKeys(keys[:i+1]), Msg: fmt.Sprintf("intermediate segment is %T, not a map", v)}
		}
		cur = next
	}
	return nil
}

// deletePath removes the value at keys. Any missing segment → no-op
// (not an error, [docs/migrations.md]). Intermediate non-map → no-op (since the path
// doesn't exist, there's nothing to delete).
func deletePath(state map[string]any, keys []string) error {
	cur := state
	for i, k := range keys {
		if i == len(keys)-1 {
			delete(cur, k)
			return nil
		}
		next, ok := cur[k].(map[string]any)
		if !ok {
			return nil // path doesn't exist at all — no-op
		}
		cur = next
	}
	return nil
}
