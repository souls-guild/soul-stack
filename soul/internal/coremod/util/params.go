package util

import (
	"fmt"

	"google.golang.org/protobuf/types/known/structpb"
)

// StringParam extracts a required string parameter from
// ApplyRequest.Params. Returns an error naming the parameter — it's
// surfaced as a failed-event with a clear message.
func StringParam(params *structpb.Struct, key string) (string, error) {
	v, err := lookup(params, key)
	if err != nil {
		return "", err
	}
	s, ok := v.Kind.(*structpb.Value_StringValue)
	if !ok {
		return "", fmt.Errorf("param %q: expected string, got %T", key, v.Kind)
	}
	return s.StringValue, nil
}

// OptStringParam is the same, but optional. Returns "", nil if the key is absent.
func OptStringParam(params *structpb.Struct, key string) (string, error) {
	if params == nil || params.Fields == nil {
		return "", nil
	}
	v, ok := params.Fields[key]
	if !ok || v == nil {
		return "", nil
	}
	if _, isNull := v.Kind.(*structpb.Value_NullValue); isNull {
		return "", nil
	}
	s, ok := v.Kind.(*structpb.Value_StringValue)
	if !ok {
		return "", fmt.Errorf("param %q: expected string, got %T", key, v.Kind)
	}
	return s.StringValue, nil
}

// OptIntParam is an optional integer (for uid/gid). proto json marshals
// numbers as float64, so we accept NumberValue and check integrality.
func OptIntParam(params *structpb.Struct, key string) (int64, bool, error) {
	if params == nil || params.Fields == nil {
		return 0, false, nil
	}
	v, ok := params.Fields[key]
	if !ok || v == nil {
		return 0, false, nil
	}
	if _, isNull := v.Kind.(*structpb.Value_NullValue); isNull {
		return 0, false, nil
	}
	n, ok := v.Kind.(*structpb.Value_NumberValue)
	if !ok {
		return 0, false, fmt.Errorf("param %q: expected integer, got %T", key, v.Kind)
	}
	f := n.NumberValue
	if f != float64(int64(f)) {
		return 0, false, fmt.Errorf("param %q: expected integer, got %v", key, f)
	}
	return int64(f), true, nil
}

// OptBoolParam is an optional bool (for flags like core.line.create).
// Missing key or null → false, nil. Non-bool value → error.
func OptBoolParam(params *structpb.Struct, key string) (bool, error) {
	if params == nil || params.Fields == nil {
		return false, nil
	}
	v, ok := params.Fields[key]
	if !ok || v == nil {
		return false, nil
	}
	if _, isNull := v.Kind.(*structpb.Value_NullValue); isNull {
		return false, nil
	}
	b, ok := v.Kind.(*structpb.Value_BoolValue)
	if !ok {
		return false, fmt.Errorf("param %q: expected bool, got %T", key, v.Kind)
	}
	return b.BoolValue, nil
}

// TriBoolParam is an optional bool that distinguishes "key absent" from
// "false". Mirrors OptIntParam: returns (value, present, error). Needed
// where three outcomes are semantically distinct — e.g. core.service.running's
// `enabled` param: omitted = don't manage enabled, true = enable,
// false = disable. Plain OptBoolParam doesn't work here (gives false for
// both absence and an explicit false).
func TriBoolParam(params *structpb.Struct, key string) (value bool, present bool, err error) {
	if params == nil || params.Fields == nil {
		return false, false, nil
	}
	v, ok := params.Fields[key]
	if !ok || v == nil {
		return false, false, nil
	}
	if _, isNull := v.Kind.(*structpb.Value_NullValue); isNull {
		return false, false, nil
	}
	b, ok := v.Kind.(*structpb.Value_BoolValue)
	if !ok {
		return false, false, fmt.Errorf("param %q: expected bool, got %T", key, v.Kind)
	}
	return b.BoolValue, true, nil
}

// OptStringMapParam is an optional map string→string (for core.exec.env).
// All map values must be strings; a non-string value → error.
func OptStringMapParam(params *structpb.Struct, key string) (map[string]string, error) {
	if params == nil || params.Fields == nil {
		return nil, nil
	}
	v, ok := params.Fields[key]
	if !ok || v == nil {
		return nil, nil
	}
	if _, isNull := v.Kind.(*structpb.Value_NullValue); isNull {
		return nil, nil
	}
	sv, ok := v.Kind.(*structpb.Value_StructValue)
	if !ok {
		return nil, fmt.Errorf("param %q: expected map of strings, got %T", key, v.Kind)
	}
	out := make(map[string]string, len(sv.StructValue.Fields))
	for k, item := range sv.StructValue.Fields {
		s, ok := item.Kind.(*structpb.Value_StringValue)
		if !ok {
			return nil, fmt.Errorf("param %q[%q]: expected string, got %T", key, k, item.Kind)
		}
		out[k] = s.StringValue
	}
	return out, nil
}

// OptStructMapParam is an optional arbitrary map (for core.file.rendered
// vars). Unlike OptStringMapParam, values need not be strings: vars arrive
// already CEL-rendered and can hold numbers/bools/nested objects/lists,
// which text/template handles natively. Returns structpb.AsMap() (recursive
// conversion to map[string]any). Missing key or null → nil, nil, no error.
func OptStructMapParam(params *structpb.Struct, key string) (map[string]any, error) {
	if params == nil || params.Fields == nil {
		return nil, nil
	}
	v, ok := params.Fields[key]
	if !ok || v == nil {
		return nil, nil
	}
	if _, isNull := v.Kind.(*structpb.Value_NullValue); isNull {
		return nil, nil
	}
	sv, ok := v.Kind.(*structpb.Value_StructValue)
	if !ok {
		return nil, fmt.Errorf("param %q: expected object, got %T", key, v.Kind)
	}
	return sv.StructValue.AsMap(), nil
}

// OptStringSliceParam is an optional list of strings (for core.user.groups).
func OptStringSliceParam(params *structpb.Struct, key string) ([]string, error) {
	if params == nil || params.Fields == nil {
		return nil, nil
	}
	v, ok := params.Fields[key]
	if !ok || v == nil {
		return nil, nil
	}
	if _, isNull := v.Kind.(*structpb.Value_NullValue); isNull {
		return nil, nil
	}
	lv, ok := v.Kind.(*structpb.Value_ListValue)
	if !ok {
		return nil, fmt.Errorf("param %q: expected list of strings, got %T", key, v.Kind)
	}
	out := make([]string, 0, len(lv.ListValue.Values))
	for i, item := range lv.ListValue.Values {
		s, ok := item.Kind.(*structpb.Value_StringValue)
		if !ok {
			return nil, fmt.Errorf("param %q[%d]: expected string, got %T", key, i, item.Kind)
		}
		out = append(out, s.StringValue)
	}
	return out, nil
}

// OptIntSliceParam is an optional list of integers (for core.http.probe's
// status_codes). proto json marshals numbers as float64, so each element is
// checked for integrality (as in OptIntParam). Missing key or null →
// nil, nil.
func OptIntSliceParam(params *structpb.Struct, key string) ([]int64, error) {
	if params == nil || params.Fields == nil {
		return nil, nil
	}
	v, ok := params.Fields[key]
	if !ok || v == nil {
		return nil, nil
	}
	if _, isNull := v.Kind.(*structpb.Value_NullValue); isNull {
		return nil, nil
	}
	lv, ok := v.Kind.(*structpb.Value_ListValue)
	if !ok {
		return nil, fmt.Errorf("param %q: expected list of integers, got %T", key, v.Kind)
	}
	out := make([]int64, 0, len(lv.ListValue.Values))
	for i, item := range lv.ListValue.Values {
		n, ok := item.Kind.(*structpb.Value_NumberValue)
		if !ok {
			return nil, fmt.Errorf("param %q[%d]: expected integer, got %T", key, i, item.Kind)
		}
		f := n.NumberValue
		if f != float64(int64(f)) {
			return nil, fmt.Errorf("param %q[%d]: expected integer, got %v", key, i, f)
		}
		out = append(out, int64(f))
	}
	return out, nil
}

// ParamPresent reports whether a key is present in params with a meaningful
// value. An explicit null is treated as absent — mirrors the Opt*Param
// getters. Unlike them, it distinguishes "key absent" from "empty string
// present": needed for cross-field XOR invariants (e.g. content vs src in
// core.file.present), where `content: ""` together with `src:` must be
// caught as a conflict, not masked by the empty string.
func ParamPresent(params *structpb.Struct, key string) bool {
	if params == nil || params.Fields == nil {
		return false
	}
	v, ok := params.Fields[key]
	if !ok || v == nil {
		return false
	}
	if _, isNull := v.Kind.(*structpb.Value_NullValue); isNull {
		return false
	}
	return true
}

func lookup(params *structpb.Struct, key string) (*structpb.Value, error) {
	if params == nil || params.Fields == nil {
		return nil, fmt.Errorf("param %q: missing (params is empty)", key)
	}
	v, ok := params.Fields[key]
	if !ok || v == nil {
		return nil, fmt.Errorf("param %q: missing", key)
	}
	return v, nil
}
