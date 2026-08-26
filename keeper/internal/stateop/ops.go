// Package stateop applies the state verbs to an incarnation state map: the
// ADR-057 CRUD four (`set` / `add` / `modify` / `remove`, orchestration.md §7)
// plus `present` / `append` / `unset`, which only a `core.state.<verb>` capture
// step can reach ([ADR-0084]). It is a package
// rather than scenario-private code so that the keeper-side `core.state` module
// can capture state through the same verbs ([ADR-0084]): one implementation, so
// a verb cannot mean two different things depending on which path wrote it.
//
// The trial harness merges through this same function ([ADR-0084] F-C): it used
// to carry a deliberate duplicate (`trial.mergeStateChanges`) pinned to this one
// by Mirror tests, and a duplicate that has to be pinned is a duplicate that can
// drift between the pinning runs.
package stateop

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/souls-guild/soul-stack/keeper/internal/render"
	"github.com/souls-guild/soul-stack/shared/config"
)

// Merge applies the ordered list of rendered operations ([render.RenderedOp],
// built by [BuildOp] from a dispatched capture step's params) on top of
// stateBefore and returns the new state.
//
// deep-copy stateBefore (a commit snapshot doesn't hold a reference to the
// source map) → sequential application of operations to the intermediate state.
// matchEval is the CEL evaluator for add's list-dedup match predicate, opEval
// the one for modify/remove match+patch with the full scenario context; both
// come as a bound pair from render.Pipeline.StateOpEvaluators (ctx + the §7
// own-namespace fence); schema is the service's state_schema (collection type
// for materializing a missing field). Empty/nil ops → state unchanged.
//
// Operation semantics:
//   - set:    out[field] = value (whole-field overwrite, last-wins);
//   - add:    materialize the collection (from the existing value / schema) →
//     identity check (map: by Key; list: Match predicate) → append/insert OR
//     no-op/replace/error per OnConflict (default skip — idempotent);
//   - modify: patch ALL collection elements matching Match (all-by-default).
//     map: match sees key/value, patches the entry; list: match sees elem,
//     patches the element. patch is a merge at a path-in-element (nested dotted
//     path), not a whole-record overwrite. expect → cardinality assert before
//     commit;
//   - remove: delete ALL elements matching Match. empty-match → no-op for both.
//   - present: out[field] = value ONLY if the field is absent or null;
//   - append:  append value to the list field, no identity check;
//   - unset:   drop the field itself (`remove` drops elements inside it).
func Merge(stateBefore map[string]any, ops []render.RenderedOp, schema map[string]any, matchEval render.StateMatchFunc, opEval render.StateOpEvalFunc) (map[string]any, error) {
	out := deepCopyMap(stateBefore)
	for i := range ops {
		if err := applyOp(out, ops[i], schema, matchEval, opEval); err != nil {
			return nil, fmt.Errorf("state op[%d] %w", i, err)
		}
	}
	// A declared secret never lands in the state record ([ADR-0083] §4): its value
	// lives in Vault and state carries only the key that addresses it.
	config.StripDeclaredSecrets(out, schema)
	return out, nil
}

// DeepCopy returns a structural copy of a decoded-JSON value. Exported for a
// caller that hands a value to [Merge] and KEEPS it: `set` stores the value by
// reference and [config.StripDeclaredSecrets] then edits it in place, so an
// uncopied value would come back with its secret properties removed.
func DeepCopy(v any) any { return deepCopyValue(v) }

// applyOp applies one operation to out in place. The error carries verb+field
// but no position — the caller owns the index, which the module path lacks.
func applyOp(out map[string]any, op render.RenderedOp, schema map[string]any, matchEval render.StateMatchFunc, opEval render.StateOpEvalFunc) error {
	switch op.Verb {
	case config.VerbSet:
		out[op.Field] = op.Value
	case config.VerbAdd:
		if err := applyAddOp(out, op, schema, matchEval); err != nil {
			return fmt.Errorf("add %q: %w", op.Field, err)
		}
	case config.VerbModify:
		if err := applyModifyOp(out, op, opEval); err != nil {
			return fmt.Errorf("modify %q: %w", op.Field, err)
		}
	case config.VerbRemove:
		if err := applyRemoveOp(out, op, opEval); err != nil {
			return fmt.Errorf("remove %q: %w", op.Field, err)
		}
	case config.VerbPresent:
		// [ADR-0084]: writes only into an empty slot, and an existing value wins —
		// including one written earlier in the same run, since state accumulates.
		// An empty list IS a value: a field deliberately emptied stays empty.
		if cur, ok := out[op.Field]; !ok || cur == nil {
			out[op.Field] = op.Value
		}
	case config.VerbAppend:
		if err := applyAppendOp(out, op, schema); err != nil {
			return fmt.Errorf("append %q: %w", op.Field, err)
		}
	case config.VerbUnset:
		delete(out, op.Field)
	default:
		return fmt.Errorf("verb %q not supported by the engine", op.Verb)
	}
	return nil
}

// applyModifyOp patches ALL elements of collection op.Field matching op.Match
// (all-by-default). map: match sees
// key/value, the patch merges into the entry's value; list: match sees elem, the
// patch merges into the element. Matched cardinality is checked against
// op.Expect BEFORE mutation (expect failure → error, state not committed).
// Empty-match → no-op.
func applyModifyOp(out map[string]any, op render.RenderedOp, opEval render.StateOpEvalFunc) error {
	existing, present := out[op.Field]
	if !present {
		// Field absent — nothing to patch. empty-match no-op (field is
		// semantically an empty collection); not an error.
		return checkExpect(op, 0)
	}
	switch coll := existing.(type) {
	case map[string]any:
		matched := 0
		for k, v := range coll {
			binds := map[string]any{"key": k, "value": v}
			ok, err := evalOpBool(opEval, op.Match, binds)
			if err != nil {
				return err
			}
			if !ok {
				continue
			}
			matched++
			patched, err := applyPatch(v, op.Patch, binds, opEval)
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
			ok, err := evalOpBool(opEval, op.Match, binds)
			if err != nil {
				return err
			}
			if !ok {
				continue
			}
			matched++
			patched, err := applyPatch(coll[i], op.Patch, binds, opEval)
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

// applyRemoveOp deletes ALL elements of collection op.Field matching op.Match.
// Cardinality is checked against
// op.Expect BEFORE mutation. Empty-match → no-op. Field absent → no-op (nothing
// to delete).
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
			ok, err := evalOpBool(opEval, op.Match, map[string]any{"key": k, "value": v})
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
			ok, err := evalOpBool(opEval, op.Match, map[string]any{"elem": coll[i]})
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

// evalOpBool evaluates the modify/remove match predicate via opEval (element
// bindings) and coerces it to bool.
func evalOpBool(opEval render.StateOpEvalFunc, match string, binds map[string]any) (bool, error) {
	if match == "" {
		// match: is optional at every layer (soul-lint only WARNs, state_wide_match),
		// so an empty predicate does reach here. Fail-safe: it matches nothing, and
		// the step is a no-op - a forgotten line must not delete the collection.
		// "every element" is written explicitly, as match: "true".
		return false, nil
	}
	res, err := opEval(match, binds, true)
	if err != nil {
		return false, fmt.Errorf("match predicate %q: %w", match, err)
	}
	b, ok := res.(bool)
	if !ok {
		return false, fmt.Errorf("match predicate %q returned %T, want bool", match, res)
	}
	return b, nil
}

// applyPatch merges a patch map into a collection record/element (a dotted path
// is a nested merge, not a whole-record overwrite). Each patch value is a
// CEL/literal, evaluated via opEval (element bindings). The record is
// deep-copied before mutation (the source state element isn't touched until the
// chain succeeds).
func applyPatch(elem any, patch, binds map[string]any, opEval render.StateOpEvalFunc) (any, error) {
	target, ok := deepCopyValue(elem).(map[string]any)
	if !ok {
		// A scalar list element (list of scalars) can't be patched by dotted path.
		return nil, fmt.Errorf("patch applies only to a record object (element %T is not an object)", elem)
	}
	for path, rawVal := range patch {
		val, err := renderPatchValue(rawVal, binds, opEval)
		if err != nil {
			return nil, fmt.Errorf("patch %q: %w", path, err)
		}
		if err := setNestedPath(target, path, val); err != nil {
			return nil, fmt.Errorf("patch %q: %w", path, err)
		}
	}
	return target, nil
}

// renderPatchValue evaluates a single patch value: string → CEL/literal via
// opEval (interpolation, native type); everything else (number/bool from a YAML
// literal) passes through as-is.
func renderPatchValue(raw any, binds map[string]any, opEval render.StateOpEvalFunc) (any, error) {
	s, ok := raw.(string)
	if !ok {
		return raw, nil
	}
	return opEval(s, binds, false)
}

// setNestedPath places a value at a dotted path (`config.maxmemory`) into a map,
// materializing MISSING intermediate objects (ADR-057 §f). A dotted path is a
// nested merge (sibling fields of the record stay intact); a flat path is
// top-level. No twin in trial — the L0 fold calls [Merge] itself, so only the
// helpers trial re-implements carry a ★ sync marker.
//
// An intermediate segment that ALREADY exists and is NOT a map (scalar/list) is
// an error (the capture fails, not a silent clobber): pushing a nested
// path through a non-object would lose the node's prior value. Difference from
// §f: a missing intermediate node is materialized (ok); an existing non-map node
// is an explicit rejection (operator is patching an incompatible shape).
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
			return fmt.Errorf("intermediate node %q already exists and is not an object (%T) - patch of nested path %q would clobber it", seg, existing, path)
		}
		cur = next
	}
	cur[parts[len(parts)-1]] = val
	return nil
}

// splitPath splits a patch's dotted path into segments.
func splitPath(path string) []string {
	return strings.Split(path, ".")
}

// checkExpect checks the actual match cardinality against op.Expect (ADR-057
// §c). ""/any → no assert. one → exactly 1; at_most_one → 0 or 1. Violation →
// error (run.go → error_locked, state not committed). No twin in trial.
func checkExpect(op render.RenderedOp, matched int) error {
	switch op.Expect {
	case "", config.ExpectAny:
		return nil
	case config.ExpectOne:
		if matched != 1 {
			return fmt.Errorf("expect: one - match hit %d elements (expected exactly one)", matched)
		}
	case config.ExpectAtMostOne:
		if matched > 1 {
			return fmt.Errorf("expect: at_most_one - match hit %d elements (expected <=1)", matched)
		}
	}
	return nil
}

// applyAppendOp appends op.Value to list field op.Field unconditionally. `add`
// is identity-checked and idempotent, which is what a set of records wants;
// this verb is for a sequence where the same element may legitimately occur
// twice. The kind lookup exists only to REJECT a map field — append has no
// meaning there, and silently coercing one would lose data.
func applyAppendOp(out map[string]any, op render.RenderedOp, schema map[string]any) error {
	switch existing := out[op.Field].(type) {
	case []any:
		out[op.Field] = append(existing, op.Value)
		return nil
	case map[string]any:
		return fmt.Errorf("field is a map collection — append works on lists, use add with key:")
	case nil: // absent or explicitly null — materialize a one-element list
		if schemaFieldType(schema, op.Field) == "object" {
			return fmt.Errorf("state_schema declares this field an object — append works on lists, use add with key:")
		}
		out[op.Field] = []any{op.Value}
		return nil
	}
	return fmt.Errorf("field already holds a scalar — append works on lists")
}

// applyAddOp applies a single add operation to the intermediate state out
// (mutates out in place — out is already a deep copy of the source state). The
// out[field] collection is materialized when absent (type from schema), then
// the element is added idempotently per the OnConflict policy.
func applyAddOp(out map[string]any, op render.RenderedOp, schema map[string]any, matchEval render.StateMatchFunc) error {
	existing, present := out[op.Field]
	kind := collectionKind(existing, present, schema, op.Field)

	switch kind {
	case collKindMap:
		if op.Key == "" {
			if op.Match != "" {
				return errors.New("match: identifies an element of a list field, and this field is a map — write key: instead")
			}
			return fmt.Errorf("add into a map collection requires key:")
		}
		coll, _ := existing.(map[string]any)
		if coll == nil {
			coll = map[string]any{}
		}
		if _, exists := coll[op.Key]; exists {
			switch op.OnConflict {
			case config.OnConflictError:
				// WITHOUT the resolved op.Key in reason: a map key could be
				// `${ vault(...) }` (a resolved secret), and reason travels into
				// incarnation.status_details.error unmasked (audit.MaskSecrets
				// catches `vault:` refs, not plaintext values). Print only the
				// collection-field name (BUG-3, security).
				return errors.New("key already exists (on_conflict: error)")
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
		if op.Key != "" {
			// key: names a property of a MAP entry. On a list it is not an identity
			// the engine can use, and honouring the DeepEqual fallback instead would
			// be worse than failing: a re-run with the same key: and one changed
			// property would append a second element rather than hit on_conflict.
			return errors.New("key: identifies an element of a map field, and this field is a list — write match: instead")
		}
		coll, _ := existing.([]any)
		idx, err := findListMatch(coll, op, matchEval)
		if err != nil {
			return err
		}
		if idx >= 0 {
			switch op.OnConflict {
			case config.OnConflictError:
				// Without the resolved op.Value/elem in reason (BUG-3, security): value
				// could be `${ vault(...) }`. Print only the collection-field name.
				return errors.New("an element with this identity already exists (on_conflict: error)")
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
	return fmt.Errorf("field %q is not a collection (map/list) and the type can't be inferred from schema", op.Field)
}

// findListMatch finds the index of an existing element identical to the one
// being added (op.Value): via the Match predicate if set, else deep-equal.
// Returns -1 if none is identical.
//
// Identity is a pure function of elem+value ([ADR-0084]), so the predicate is
// evaluated by matchEval with those two bindings and nothing else.
func findListMatch(coll []any, op render.RenderedOp, matchEval render.StateMatchFunc) (int, error) {
	for i := range coll {
		if op.Match != "" {
			ok, err := matchEval(op.Match, coll[i], op.Value)
			if err != nil {
				return -1, fmt.Errorf("match predicate %q: %w", op.Match, err)
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

// collKind is the collection kind under a state field (for add materialization).
// ★ Logic identical to trial.collKind*.
type collKind int

const (
	collKindUnknown collKind = iota
	collKindList
	collKindMap
)

// collectionKind determines a field's collection kind: first from the existing
// state value (authoritative — actual shape), and if absent, from state_schema
// (`properties.<field>.type`: array→list, object→map). Unknown → collKindUnknown
// (applyAddOp returns an error). ★ Logic identical to trial.collectionKind.
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

// schemaFieldType extracts `state_schema.properties.<field>.type` from the
// service's flat state_schema map (service.yml shape:
// {type:object, properties:{...}}). "" if schema isn't declared or the field
// isn't described. ★ Logic identical to trial.schemaFieldType.
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

// deepCopyMap deep-copies a map[string]any via a JSON round-trip (values are
// YAML/PG data: maps/slices/scalars, JSON-safe). nil → empty map
// (incarnation.state is never nil in a commit snapshot).
func deepCopyMap(m map[string]any) map[string]any {
	if len(m) == 0 {
		return map[string]any{}
	}
	b, err := json.Marshal(m)
	if err != nil {
		// state is JSON-safe (read from JSONB); marshal doesn't fail.
		return map[string]any{}
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		return map[string]any{}
	}
	return out
}

// deepCopyValue deep-copies an arbitrary JSON-safe value (map/slice/scalar) via
// a JSON round-trip. Needed by applyPatch: a mutated collection element must not
// hold a reference to the source state until the chain succeeds. A marshal
// failure is impossible (state is JSON-safe from JSONB); on error we return the
// original (a mismatch would be caught by verification/tests).
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
