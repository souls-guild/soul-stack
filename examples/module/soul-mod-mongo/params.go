// Parameter typing at the artifact's boundary — a value whose type is not the one
// the object declares is REFUSED, never coerced (NIM-778 on redis, NIM-800 here).
//
// The rule it replaces coerced: [boolOrDefault] returned its default on anything
// that was not a boolean, so `tls: "true"` written as a string read as `tls: false`
// and the connection — the admin password included — went out to mongod in
// plaintext, reported as reconciled. An author's typo became a silent leak instead
// of a refusal, and the direction of the fallback was what made it one: `false` is
// the insecure side of that parameter. Nothing upstream catches it either. The
// runtime calls Apply, not Validate, and the Keeper's static `checkParamType`
// returns nil on a `${…}` cell, so `tls: "${ vars.mongo_tls }"` over a string var
// lints clean. The plugin is the last place that can say no.
//
// It is checked against the DECLARATION rather than at each read site on purpose.
// A rule derived from [object.decl] cannot drift the way a hand-written check does,
// and a parameter added later inherits it without being remembered.
//
// This file is the redis artifact's `params.go` carried across unchanged in
// substance: nothing in [checkParamTypes] is redis-specific, it reads only
// `module.Input` and `structpb.Value`. The nested readers below the divider are
// this artifact's own, because the specs they read are.
package main

import (
	"fmt"
	"sort"

	"github.com/souls-guild/soul-stack/sdk/module"
	"google.golang.org/protobuf/types/known/structpb"
)

// checkParamTypes reports every param of f whose value does not match the type
// decl gives it, addressed as `params.<name>`. Deterministic order.
//
// Three things it deliberately does NOT do. An UNDECLARED key is left alone — the
// engine refuses one as `unknown_param` (NIM-204) and duplicating that here would
// only give it a second wording. An ABSENT key is left alone — that is what a
// default is for. And a NULL is read as absent, because `tls:` written with nothing
// after it arrives as one and means "unset", not "a value of the wrong type".
//
// Nothing here is about a value's CONTENT: `wait_primary_seconds: -1` and an
// unparseable PEM are the action's own business. This answers one question, the one
// that was answered by guessing.
func checkParamTypes(decl module.Input, f map[string]*structpb.Value) []string {
	if len(decl) == 0 || len(f) == 0 {
		return nil
	}
	names := make([]string, 0, len(f))
	for name := range f {
		names = append(names, name)
	}
	sort.Strings(names)

	var errs []string
	for _, name := range names {
		p, declared := decl[name]
		if !declared || p.Type == "" {
			continue
		}
		v := f[name]
		if v == nil || isNull(v) {
			continue
		}
		if want, ok := paramTypeMismatch(p.Type, v); !ok {
			errs = append(errs, fmt.Sprintf("params.%s: must be %s, got %s", name, want, valueTypeName(v)))
		}
	}
	return errs
}

// paramTypeMismatch answers whether v carries the declared type, and names that
// type for the message when it does not.
//
// The synonyms are the ones `sdk/schema` admits (`docs/input.md` spellings), so a
// module declaring `module.Integer` is held to the same rule as one declaring
// `module.Int`. An INT is not merely a number: 7.5 where an int is declared is
// refused rather than truncated to 7, for the reason the whole file exists —
// truncation is a guess, and `votes: 0.5` silently becoming a non-voting member is
// the same class of surprise as `tls: "true"` addressing plaintext.
func paramTypeMismatch(t module.ParamType, v *structpb.Value) (string, bool) {
	switch t {
	case module.Bool, module.Boolean:
		_, ok := v.GetKind().(*structpb.Value_BoolValue)
		return "a boolean (true/false)", ok
	case module.Int, module.Integer:
		n, ok := v.GetKind().(*structpb.Value_NumberValue)
		return "an integer", ok && n.NumberValue == float64(int64(n.NumberValue))
	case module.Number:
		_, ok := v.GetKind().(*structpb.Value_NumberValue)
		return "a number", ok
	case module.String:
		_, ok := v.GetKind().(*structpb.Value_StringValue)
		return "a string", ok
	case module.List, module.Array:
		_, ok := v.GetKind().(*structpb.Value_ListValue)
		return "a list", ok
	case module.Map, module.Object:
		_, ok := v.GetKind().(*structpb.Value_StructValue)
		return "a map", ok
	default:
		// A type this build does not know is not a licence to guess, but it is
		// also not this function's to refuse: it can only come from a newer
		// sdk/schema, and the schema validator is what reports that.
		return "", true
	}
}

// valueTypeName names what actually arrived, in the same vocabulary the message
// asks for. Without it "must be a boolean" leaves an author to work out which of
// their values is the string.
func valueTypeName(v *structpb.Value) string {
	switch v.GetKind().(type) {
	case *structpb.Value_BoolValue:
		return "a boolean"
	case *structpb.Value_NumberValue:
		return "a number"
	case *structpb.Value_StringValue:
		return "a string"
	case *structpb.Value_ListValue:
		return "a list"
	case *structpb.Value_StructValue:
		return "a map"
	default:
		return "nothing"
	}
}

// isNull is the one spelling of "written with nothing after it". A YAML key with an
// empty value arrives as a null, and every reader here treats that as absent rather
// than as a value of the wrong type.
func isNull(v *structpb.Value) bool {
	_, ok := v.GetKind().(*structpb.Value_NullValue)
	return ok
}

// --- nested specs ---
//
// `members`, `privileges`, `keys` and `options` are declared as a map or a list and
// nothing declares what is INSIDE them, so [checkParamTypes] reaches the outer value
// and stops. The readers below carry the rule the rest of the way, and they exist
// for the same reason the file does rather than for symmetry: a coerced
// `priority: "0"` on a hidden replica-set member would fall back to the default 1,
// and a hidden member with priority 1 is a member that can win an election.
//
// Each takes the full parameter address for the message, since the caller is the
// only one that knows it.

// stringField reads one string out of a nested spec, refusing a value of another
// type rather than defaulting.
func stringField(spec map[string]*structpb.Value, key, addr, def string) (string, error) {
	v := spec[key]
	if v == nil || isNull(v) {
		return def, nil
	}
	s, ok := v.GetKind().(*structpb.Value_StringValue)
	if !ok {
		return "", fmt.Errorf("%s: must be a string, got %s", addr, valueTypeName(v))
	}
	return s.StringValue, nil
}

// intField reads one integer out of a nested spec. A number with a fractional part
// is refused rather than truncated — see [paramTypeMismatch].
func intField(spec map[string]*structpb.Value, key, addr string, def int) (int, error) {
	v := spec[key]
	if v == nil || isNull(v) {
		return def, nil
	}
	n, ok := v.GetKind().(*structpb.Value_NumberValue)
	if !ok || n.NumberValue != float64(int64(n.NumberValue)) {
		return 0, fmt.Errorf("%s: must be an integer, got %s", addr, valueTypeName(v))
	}
	return int(n.NumberValue), nil
}

// numberField reads one number out of a nested spec. `priority` is the reason this
// is separate from [intField]: mongod accepts a fractional priority.
func numberField(spec map[string]*structpb.Value, key, addr string, def float64) (float64, error) {
	v := spec[key]
	if v == nil || isNull(v) {
		return def, nil
	}
	n, ok := v.GetKind().(*structpb.Value_NumberValue)
	if !ok {
		return 0, fmt.Errorf("%s: must be a number, got %s", addr, valueTypeName(v))
	}
	return n.NumberValue, nil
}

// boolField reads one boolean out of a nested spec. This is the nested spelling of
// the coercion NIM-778 removed: without it `hidden: "true"` reads as false.
func boolField(spec map[string]*structpb.Value, key, addr string, def bool) (bool, error) {
	v := spec[key]
	if v == nil || isNull(v) {
		return def, nil
	}
	b, ok := v.GetKind().(*structpb.Value_BoolValue)
	if !ok {
		return false, fmt.Errorf("%s: must be a boolean (true/false), got %s", addr, valueTypeName(v))
	}
	return b.BoolValue, nil
}

// mapField reads a nested map out of a spec, refusing a value of another type. An
// absent key gives nil, which every caller reads as "not set".
func mapField(spec map[string]*structpb.Value, key, addr string) (map[string]*structpb.Value, error) {
	v := spec[key]
	if v == nil || isNull(v) {
		return nil, nil
	}
	sv, ok := v.GetKind().(*structpb.Value_StructValue)
	if !ok {
		return nil, fmt.Errorf("%s: must be a map, got %s", addr, valueTypeName(v))
	}
	return sv.StructValue.GetFields(), nil
}

// listFieldOf reads a nested list out of a spec, refusing a value of another type.
func listFieldOf(spec map[string]*structpb.Value, key, addr string) ([]*structpb.Value, error) {
	v := spec[key]
	if v == nil || isNull(v) {
		return nil, nil
	}
	lv, ok := v.GetKind().(*structpb.Value_ListValue)
	if !ok {
		return nil, fmt.Errorf("%s: must be a list, got %s", addr, valueTypeName(v))
	}
	return lv.ListValue.GetValues(), nil
}

// sortedKeys is the deterministic order every nested reader walks a map in, so an
// error report and a built document are byte-stable across runs.
func sortedKeys(m map[string]*structpb.Value) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
