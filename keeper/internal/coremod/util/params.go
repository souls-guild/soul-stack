// Package util provides common helpers for keeper-side core-modules
// (`core.soul.registered`, `core.cloud.provisioned`, `core.vault.kv-read`).
//
// Contents are typed accessors over `*structpb.Struct` (ApplyRequest
// params) and helpers to send final ApplyEvent-s on gRPC-stream.
//
// Analog of Soul-side `soul/internal/coremod/util/`: names and signatures are symmetric
// so keeper-side module author writes code same way as Soul-side.
// Cross-import between sides forbidden (Soul-isolation — ADR-011), so
// helpers are duplicated.
package util

import (
	"fmt"

	"google.golang.org/protobuf/types/known/structpb"
)

// StringParam is required string parameter. Returns error with field name
// (proxied outward as `failed`-event with clear message).
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

// OptStringParam is optional string. "", nil if key missing or null.
func OptStringParam(params *structpb.Struct, key string) (string, error) {
	v, ok := optLookup(params, key)
	if !ok {
		return "", nil
	}
	s, ok := v.Kind.(*structpb.Value_StringValue)
	if !ok {
		return "", fmt.Errorf("param %q: expected string, got %T", key, v.Kind)
	}
	return s.StringValue, nil
}

// StringSliceParam is required string list. Used by
// `core.soul.registered` for `params.coven` (min_items=1).
func StringSliceParam(params *structpb.Struct, key string) ([]string, error) {
	v, err := lookup(params, key)
	if err != nil {
		return nil, err
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

// StringOrSliceParam is required parameter taking string OR list of
// strings, normalized to []string. Used by `core.soul.registered` for
// `params.sid` (ADR-061): single SID as string remains valid (backward
// compatibility), list registers + awaits N hosts in single barrier-step.
// Empty list / empty elements are error (each SID must be non-empty).
func StringOrSliceParam(params *structpb.Struct, key string) ([]string, error) {
	v, err := lookup(params, key)
	if err != nil {
		return nil, err
	}
	switch k := v.Kind.(type) {
	case *structpb.Value_StringValue:
		if k.StringValue == "" {
			return nil, fmt.Errorf("param %q: empty string", key)
		}
		return []string{k.StringValue}, nil
	case *structpb.Value_ListValue:
		vals := k.ListValue.Values
		if len(vals) == 0 {
			return nil, fmt.Errorf("param %q: empty list", key)
		}
		out := make([]string, 0, len(vals))
		for i, item := range vals {
			s, ok := item.Kind.(*structpb.Value_StringValue)
			if !ok {
				return nil, fmt.Errorf("param %q[%d]: expected string, got %T", key, i, item.Kind)
			}
			if s.StringValue == "" {
				return nil, fmt.Errorf("param %q[%d]: empty string", key, i)
			}
			out = append(out, s.StringValue)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("param %q: expected string or list of strings, got %T", key, v.Kind)
	}
}

// ListParam is required list of arbitrary Value (for `hosts`
// list-of-objects `core.bootstrap.delivered`). Returns raw elements;
// parsing each (type/fields) — caller's responsibility. Empty list valid at
// accessor level (caller decides "nothing to process" semantics).
func ListParam(params *structpb.Struct, key string) ([]*structpb.Value, error) {
	v, err := lookup(params, key)
	if err != nil {
		return nil, err
	}
	lv, ok := v.Kind.(*structpb.Value_ListValue)
	if !ok {
		return nil, fmt.Errorf("param %q: expected list, got %T", key, v.Kind)
	}
	return lv.ListValue.Values, nil
}

// OptStringSliceParam is optional string list (for cloud `fields`,
// `vm_ids` etc.). nil if key missing.
func OptStringSliceParam(params *structpb.Struct, key string) ([]string, error) {
	v, ok := optLookup(params, key)
	if !ok {
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

// OptBoolParam is optional bool. (false,false,nil) if key missing/null.
// Second bool is presence flag (distinguishes default from explicit false).
func OptBoolParam(params *structpb.Struct, key string) (bool, bool, error) {
	v, ok := optLookup(params, key)
	if !ok {
		return false, false, nil
	}
	b, ok := v.Kind.(*structpb.Value_BoolValue)
	if !ok {
		return false, false, fmt.Errorf("param %q: expected bool, got %T", key, v.Kind)
	}
	return b.BoolValue, true, nil
}

// OptIntParam is optional integer (proto json marshals as float64).
func OptIntParam(params *structpb.Struct, key string) (int64, bool, error) {
	v, ok := optLookup(params, key)
	if !ok {
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

// OptStructParam is optional object (for cloud-provider `profile`).
// Returns nil if key missing; error only on wrong-type.
func OptStructParam(params *structpb.Struct, key string) (*structpb.Struct, error) {
	v, ok := optLookup(params, key)
	if !ok {
		return nil, nil
	}
	sv, ok := v.Kind.(*structpb.Value_StructValue)
	if !ok {
		return nil, fmt.Errorf("param %q: expected object, got %T", key, v.Kind)
	}
	return sv.StructValue, nil
}

func lookup(params *structpb.Struct, key string) (*structpb.Value, error) {
	if params == nil || params.Fields == nil {
		return nil, fmt.Errorf("param %q: missing (params is empty)", key)
	}
	v, ok := params.Fields[key]
	if !ok || v == nil {
		return nil, fmt.Errorf("param %q: missing", key)
	}
	if _, isNull := v.Kind.(*structpb.Value_NullValue); isNull {
		return nil, fmt.Errorf("param %q: missing (null)", key)
	}
	return v, nil
}

func optLookup(params *structpb.Struct, key string) (*structpb.Value, bool) {
	if params == nil || params.Fields == nil {
		return nil, false
	}
	v, ok := params.Fields[key]
	if !ok || v == nil {
		return nil, false
	}
	if _, isNull := v.Kind.(*structpb.Value_NullValue); isNull {
		return nil, false
	}
	return v, true
}
