package main

import (
	"fmt"
	"sort"
	"strings"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/structpb"
)

// sendOutcome sends the final event with changed/message/output.
func sendOutcome(stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], changed bool, message string, output map[string]any) error {
	out, err := structpb.NewStruct(output)
	if err != nil {
		return fmt.Errorf("build output struct: %w", err)
	}
	return stream.Send(&pluginv1.ApplyEvent{Message: message, Changed: changed, Output: out})
}

// sendFailure sends the final event with failed=true. message is already sanitized
// (redactError) and carries no secrets.
func sendFailure(stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], message string) error {
	return stream.Send(&pluginv1.ApplyEvent{Message: message, Failed: true})
}

// redactError removes secrets from error text. The driver builds connection and
// auth errors whose text on some paths can carry the credentials it was given;
// fail-safe replaces each secret substring with "***". Variadic: empty secrets are
// a no-op.
func redactError(err error, secrets ...string) string {
	msg := err.Error()
	for _, s := range secrets {
		if s != "" {
			msg = strings.ReplaceAll(msg, s, "***")
		}
	}
	return msg
}

// --- structpb readers ---

func stringValue(v *structpb.Value) (string, bool) {
	if v == nil {
		return "", false
	}
	if sv, ok := v.GetKind().(*structpb.Value_StringValue); ok {
		return sv.StringValue, true
	}
	return "", false
}

// stringOrEmpty returns the string value or "" (for optional fields).
func stringOrEmpty(v *structpb.Value) string {
	s, _ := stringValue(v)
	return s
}

// boolOrDefault returns the boolean value or def. A value of another type reaching
// here is impossible for a DECLARED param: [checkParamTypes] refuses it in both
// Validate and Apply before any read (NIM-778). The default is for an ABSENT key.
func boolOrDefault(v *structpb.Value, def bool) bool {
	if v == nil {
		return def
	}
	if bv, ok := v.GetKind().(*structpb.Value_BoolValue); ok {
		return bv.BoolValue
	}
	return def
}

// intOrDefault returns the integer value or def. See [boolOrDefault] on why a
// wrong-typed value cannot reach this.
func intOrDefault(v *structpb.Value, def int) int {
	if v == nil {
		return def
	}
	if nv, ok := v.GetKind().(*structpb.Value_NumberValue); ok {
		return int(nv.NumberValue)
	}
	return def
}

// mapField returns a nested map's fields. nil / non-map -> nil (the caller treats
// that as "not set").
func mapField(v *structpb.Value) map[string]*structpb.Value {
	if v == nil {
		return nil
	}
	sv, ok := v.GetKind().(*structpb.Value_StructValue)
	if !ok {
		return nil
	}
	return sv.StructValue.GetFields()
}

// listField returns a list's elements. nil / non-list -> nil.
func listField(v *structpb.Value) []*structpb.Value {
	if v == nil {
		return nil
	}
	lv, ok := v.GetKind().(*structpb.Value_ListValue)
	if !ok {
		return nil
	}
	return lv.ListValue.GetValues()
}

// stringList reads a list of strings, refusing an element of another type rather
// than skipping it: a silently dropped element of `partition_key` would change the
// primary key of the table being created.
func stringList(v *structpb.Value, addr string) ([]string, error) {
	elems := listField(v)
	out := make([]string, 0, len(elems))
	for i, e := range elems {
		s, ok := stringValue(e)
		if !ok {
			return nil, fmt.Errorf("%s[%d]: must be a string, got %s", addr, i, valueTypeName(e))
		}
		out = append(out, s)
	}
	return out, nil
}

// valueToNative converts a structpb value to a native Go value for binding into a
// CQL statement (command.run's args). Numbers with no fractional part become int64.
func valueToNative(v *structpb.Value) any {
	if v == nil {
		return nil
	}
	switch k := v.GetKind().(type) {
	case *structpb.Value_NullValue:
		return nil
	case *structpb.Value_StringValue:
		return k.StringValue
	case *structpb.Value_NumberValue:
		if k.NumberValue == float64(int64(k.NumberValue)) {
			return int64(k.NumberValue)
		}
		return k.NumberValue
	case *structpb.Value_BoolValue:
		return k.BoolValue
	case *structpb.Value_StructValue:
		out := make(map[string]any, len(k.StructValue.GetFields()))
		for kk, vv := range k.StructValue.GetFields() {
			out[kk] = valueToNative(vv)
		}
		return out
	case *structpb.Value_ListValue:
		vals := k.ListValue.GetValues()
		out := make([]any, 0, len(vals))
		for _, e := range vals {
			out = append(out, valueToNative(e))
		}
		return out
	default:
		return nil
	}
}

// sortedKeys returns a map's keys in a stable order, so a report over params reads
// the same on every run (determinism, and tests that do not depend on map order).
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// --- shared Validate helpers ---

// requireString reports that a param is missing unless it is a non-empty string.
func requireString(f map[string]*structpb.Value, key string) []string {
	if s, _ := stringValue(f[key]); strings.TrimSpace(s) == "" {
		return []string{fmt.Sprintf("params.%s: must be a non-empty string", key)}
	}
	return nil
}

// validateConnect reports what [parseConnConfig] would refuse, addressed the same
// way. Validate exists to refuse before anything happens, so what Apply rejects at
// its first step is rejected here too (NIM-786).
func validateConnect(f map[string]*structpb.Value) []string {
	var errs []string
	if _, err := parseHosts(f["hosts"]); err != nil {
		errs = append(errs, err.Error())
	}
	if port := intOrDefault(f["port"], defaultPort); port < 1 || port > 65535 {
		errs = append(errs, fmt.Sprintf("params.port: must be a TCP port in 1..65535, got %d", port))
	}
	// The TLS material is BUILT here, not merely looked at. An unparseable tls_ca and
	// a half mTLS pair are refused by [buildTLSConfig], which on the Apply path runs
	// inside the driver's connect — so without this call every one of those refusals
	// would land in the middle of a run, which is exactly the phase-defeating shape
	// NIM-786 named. It is a pure function over the same params, so calling it twice
	// costs nothing and cannot disagree with itself.
	if _, err := buildTLSConfig(parseTLS(f)); err != nil {
		errs = append(errs, err.Error())
	}
	return errs
}
