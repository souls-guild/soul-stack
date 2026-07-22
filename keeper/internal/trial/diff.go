package trial

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	"github.com/souls-guild/soul-stack/keeper/internal/render"
	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/diag"
)

// deepEqualJSON compares two Go structs that passed through structpb
// (map[string]any / []any / float64 / string / bool / nil). Direct
// reflect.DeepEqual is sufficient: both sides are normalized identically
// by structpb.AsMap (numbers → float64, map traversal is deterministic).
func deepEqualJSON(a, b any) bool {
	return reflect.DeepEqual(a, b)
}

// mergeStateChanges — ★ logic identical to scenario.mergeStateChanges (state.go):
// applies an ordered list of rendered `state_changes` operations on top of
// base state (orchestration.md §7). Trial must yield the same state_after as
// prod-commit; any divergence would split Trial from prod (anti-drift for duplicate,
// keeps Mirror-test). Semantics — see scenario.mergeStateChanges.
func mergeStateChanges(stateBefore map[string]any, ops []render.RenderedOp, schema map[string]any, matchEval render.StateMatchFunc, opEval render.StateOpEvalFunc) (map[string]any, error) {
	out := deepCopyState(stateBefore)
	for i := range ops {
		op := ops[i]
		switch op.Verb {
		case config.VerbSet:
			out[op.Field] = op.Value
		case config.VerbAdd:
			if err := applyAddOp(out, op, schema, matchEval, opEval); err != nil {
				return nil, fmt.Errorf("state_changes[%d] add %q: %w", i, op.Field, err)
			}
		case config.VerbModify:
			if err := applyModifyOp(out, op, opEval); err != nil {
				return nil, fmt.Errorf("state_changes[%d] modify %q: %w", i, op.Field, err)
			}
		case config.VerbRemove:
			if err := applyRemoveOp(out, op, opEval); err != nil {
				return nil, fmt.Errorf("state_changes[%d] remove %q: %w", i, op.Field, err)
			}
		default:
			return nil, fmt.Errorf("state_changes[%d]: verb %q not supported", i, op.Verb)
		}
	}
	return out, nil
}

// applyModifyOp — ★ logic identical to scenario.applyModifyOp.
func applyModifyOp(out map[string]any, op render.RenderedOp, opEval render.StateOpEvalFunc) error {
	existing, present := out[op.Field]
	if !present {
		return checkExpect(op, 0)
	}
	switch coll := existing.(type) {
	case map[string]any:
		matched := 0
		for k, v := range coll {
			binds := map[string]any{"key": k, "value": v}
			ok, err := evalOpBool(opEval, op.Match, op.Context, binds)
			if err != nil {
				return err
			}
			if !ok {
				continue
			}
			matched++
			patched, err := applyPatch(v, op.Patch, op.Context, binds, opEval)
			if err != nil {
				return err
			}
			coll[k] = patched
		}
		if err := checkExpect(op, matched); err != nil {
			return err
		}
		out[op.Field] = coll
		return nil
	case []any:
		matched := 0
		for i := range coll {
			binds := map[string]any{"elem": coll[i]}
			ok, err := evalOpBool(opEval, op.Match, op.Context, binds)
			if err != nil {
				return err
			}
			if !ok {
				continue
			}
			matched++
			patched, err := applyPatch(coll[i], op.Patch, op.Context, binds, opEval)
			if err != nil {
				return err
			}
			coll[i] = patched
		}
		if err := checkExpect(op, matched); err != nil {
			return err
		}
		out[op.Field] = coll
		return nil
	}
	return fmt.Errorf("field %q is not a collection (map/list)", op.Field)
}

// applyRemoveOp — ★ logic identical to scenario.applyRemoveOp.
func applyRemoveOp(out map[string]any, op render.RenderedOp, opEval render.StateOpEvalFunc) error {
	existing, present := out[op.Field]
	if !present {
		return checkExpect(op, 0)
	}
	switch coll := existing.(type) {
	case map[string]any:
		matched := 0
		drop := make([]string, 0, len(coll))
		for k, v := range coll {
			ok, err := evalOpBool(opEval, op.Match, op.Context, map[string]any{"key": k, "value": v})
			if err != nil {
				return err
			}
			if ok {
				matched++
				drop = append(drop, k)
			}
		}
		if err := checkExpect(op, matched); err != nil {
			return err
		}
		for _, k := range drop {
			delete(coll, k)
		}
		out[op.Field] = coll
		return nil
	case []any:
		kept := make([]any, 0, len(coll))
		matched := 0
		for i := range coll {
			ok, err := evalOpBool(opEval, op.Match, op.Context, map[string]any{"elem": coll[i]})
			if err != nil {
				return err
			}
			if ok {
				matched++
				continue
			}
			kept = append(kept, coll[i])
		}
		if err := checkExpect(op, matched); err != nil {
			return err
		}
		out[op.Field] = kept
		return nil
	}
	return fmt.Errorf("field %q is not a collection (map/list)", op.Field)
}

// evalOpBool — ★ logic identical to scenario.evalOpBool.
func evalOpBool(opEval render.StateOpEvalFunc, match string, ctx, binds map[string]any) (bool, error) {
	if match == "" {
		return false, nil
	}
	res, err := opEval(match, ctx, binds, true)
	if err != nil {
		return false, fmt.Errorf("match-predicate %q: %w", match, err)
	}
	b, ok := res.(bool)
	if !ok {
		return false, fmt.Errorf("match-predicate %q returned %T, expected bool", match, res)
	}
	return b, nil
}

// applyPatch — ★ logic identical to scenario.applyPatch.
func applyPatch(elem any, patch, ctx, binds map[string]any, opEval render.StateOpEvalFunc) (any, error) {
	target, ok := deepCopyValue(elem).(map[string]any)
	if !ok {
		return nil, fmt.Errorf("patch applicable only to record objects (element %T is not an object)", elem)
	}
	for path, rawVal := range patch {
		val, err := renderPatchValue(rawVal, ctx, binds, opEval)
		if err != nil {
			return nil, fmt.Errorf("patch %q: %w", path, err)
		}
		if err := setNestedPath(target, path, val); err != nil {
			return nil, fmt.Errorf("patch %q: %w", path, err)
		}
	}
	return target, nil
}

// renderPatchValue — ★ logic identical to scenario.renderPatchValue.
func renderPatchValue(raw any, ctx, binds map[string]any, opEval render.StateOpEvalFunc) (any, error) {
	s, ok := raw.(string)
	if !ok {
		return raw, nil
	}
	return opEval(s, ctx, binds, false)
}

// setNestedPath — ★ logic identical to scenario.setNestedPath. Missing
// intermediate nodes are materialized (ADR-057 §f); existing non-map nodes →
// error (not silent clobber). Divergence from prod branch would split Trial from
// prod — keeps Mirror-test.
func setNestedPath(m map[string]any, path string, val any) error {
	parts := splitPath(path)
	cur := m
	for i := 0; i < len(parts)-1; i++ {
		seg := parts[i]
		existing, present := cur[seg]
		if !present {
			next := map[string]any{}
			cur[seg] = next
			cur = next
			continue
		}
		next, ok := existing.(map[string]any)
		if !ok {
			return fmt.Errorf("intermediate node %q already exists and is not an object (%T) — patch of nested path %q would clobber it", seg, existing, path)
		}
		cur = next
	}
	cur[parts[len(parts)-1]] = val
	return nil
}

// splitPath — ★ logic identical to scenario.splitPath.
func splitPath(path string) []string {
	return strings.Split(path, ".")
}

// checkExpect — ★ logic identical to scenario.checkExpect.
func checkExpect(op render.RenderedOp, matched int) error {
	switch op.Expect {
	case "", config.ExpectAny:
		return nil
	case config.ExpectOne:
		if matched != 1 {
			return fmt.Errorf("expect: one — match caught %d elements (expected exactly one)", matched)
		}
	case config.ExpectAtMostOne:
		if matched > 1 {
			return fmt.Errorf("expect: at_most_one — match caught %d elements (expected ≤1)", matched)
		}
	}
	return nil
}

// deepCopyValue — ★ logic identical to scenario.deepCopyValue.
func deepCopyValue(v any) any {
	b, err := json.Marshal(v)
	if err != nil {
		return v
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		return v
	}
	return out
}

// applyAddOp — ★ logic identical to scenario.applyAddOp.
func applyAddOp(out map[string]any, op render.RenderedOp, schema map[string]any, matchEval render.StateMatchFunc, opEval render.StateOpEvalFunc) error {
	existing, present := out[op.Field]
	kind := collectionKind(existing, present, schema, op.Field)

	switch kind {
	case collKindMap:
		if op.Key == "" {
			return fmt.Errorf("add to map-collection requires key:")
		}
		coll, _ := existing.(map[string]any)
		if coll == nil {
			coll = map[string]any{}
		}
		if _, exists := coll[op.Key]; exists {
			switch op.OnConflict {
			case config.OnConflictError:
				// Without resolving op.Key in reason (BUG-3, security) — see
				// scenario.applyAddOp. Print only the field collection name.
				return fmt.Errorf("add %q: key already exists (on_conflict: error)", op.Field)
			case config.OnConflictReplace:
				coll[op.Key] = op.Value
			default: // skip (default) — idempotent no-op
			}
		} else {
			coll[op.Key] = op.Value
		}
		out[op.Field] = coll
		return nil

	case collKindList:
		coll, _ := existing.([]any)
		idx, err := findListMatch(coll, op, matchEval, opEval)
		if err != nil {
			return err
		}
		if idx >= 0 {
			switch op.OnConflict {
			case config.OnConflictError:
				// Without resolving op.Value in reason (BUG-3, security).
				return fmt.Errorf("add %q: element with such identity already exists (on_conflict: error)", op.Field)
			case config.OnConflictReplace:
				coll[idx] = op.Value
			default: // skip (default) — idempotent no-op
			}
		} else {
			coll = append(coll, op.Value)
		}
		out[op.Field] = coll
		return nil
	}
	return fmt.Errorf("field %q is not a collection (map/list) and type cannot be inferred from schema", op.Field)
}

// findListMatch — ★ logic identical to scenario.findListMatch.
func findListMatch(coll []any, op render.RenderedOp, matchEval render.StateMatchFunc, opEval render.StateOpEvalFunc) (int, error) {
	for i := range coll {
		if op.Match != "" {
			var ok bool
			var err error
			if op.Context != nil {
				ok, err = evalOpBool(opEval, op.Match, op.Context, map[string]any{"elem": coll[i], "value": op.Value})
			} else {
				ok, err = matchEval(op.Match, coll[i], op.Value)
			}
			if err != nil {
				return -1, fmt.Errorf("match-predicate %q: %w", op.Match, err)
			}
			if ok {
				return i, nil
			}
			continue
		}
		if reflect.DeepEqual(coll[i], op.Value) {
			return i, nil
		}
	}
	return -1, nil
}

// setOpsProjection projects set-operations of rendered list into map
// field→value for back-compat assert `assert.state_changes` (add-operations are not
// included in projection — they are checked via assert.state_after). Logic
// identical to render.Pipeline.RenderStateChanges-projection.
func setOpsProjection(ops []render.RenderedOp) map[string]any {
	out := make(map[string]any, len(ops))
	for i := range ops {
		if ops[i].Verb == config.VerbSet {
			out[ops[i].Field] = ops[i].Value
		}
	}
	return out
}

// collKind/collectionKind/schemaFieldType — ★ logic identical to scenario.* (same names).
type collKind int

const (
	collKindUnknown collKind = iota
	collKindList
	collKindMap
)

func collectionKind(existing any, present bool, schema map[string]any, field string) collKind {
	if present {
		switch existing.(type) {
		case []any:
			return collKindList
		case map[string]any:
			return collKindMap
		}
		return collKindUnknown
	}
	switch schemaFieldType(schema, field) {
	case "array":
		return collKindList
	case "object":
		return collKindMap
	}
	return collKindUnknown
}

func schemaFieldType(schema map[string]any, field string) string {
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		return ""
	}
	fieldSchema, ok := props[field].(map[string]any)
	if !ok {
		return ""
	}
	t, _ := fieldSchema["type"].(string)
	return t
}

// deepCopyState — deep-copy base state via JSON round-trip (expected result
// must not hold reference to fixtures.state). nil/empty → empty map. Data is
// JSON-safe (YAML fixtures); on marshal failure — empty base, verification catches
// divergence. ★ Logic identical to scenario.deepCopyMap (name local to avoid collision).
func deepCopyState(m map[string]any) map[string]any {
	out := map[string]any{}
	if len(m) > 0 {
		if b, err := json.Marshal(m); err == nil {
			_ = json.Unmarshal(b, &out)
		}
	}
	return out
}

// hasErrors — are there error-level diagnostics.
func hasErrors(ds []diag.Diagnostic) bool {
	return diag.HasErrors(ds)
}

// formatDiags merges diagnostics into one string for error message.
func formatDiags(ds []diag.Diagnostic) string {
	var b strings.Builder
	for _, d := range ds {
		if d.Level != diag.LevelError {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("; ")
		}
		b.WriteString(d.Code)
		if d.YAMLPath != "" {
			b.WriteString(" @ " + d.YAMLPath)
		}
		b.WriteString(": " + d.Message)
	}
	return b.String()
}
