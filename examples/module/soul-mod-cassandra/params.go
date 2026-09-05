// Parameter typing at the artifact's boundary — a value whose type is not the one
// the object declares is REFUSED, never coerced (NIM-778).
//
// The rule was paid for on the redis artifact: [boolOrDefault] there returned its
// default on anything that was not a boolean, so `tls: "true"` written as a string
// read as `tls: false` and the connection — password included — went out in
// plaintext, reported as reconciled. An author's typo became a silent leak instead
// of a refusal, and the DIRECTION of the fallback is what made it one: `false` is
// the insecure side of that parameter. Nothing upstream catches it either. The
// runtime calls Apply, not Validate, and the Keeper's static `checkParamType`
// returns nil on a `${…}` cell, so `tls: "${ vars.cassandra_tls }"` over a string
// var lints clean. The plugin is the last place that can say no.
//
// It is checked against the DECLARATION rather than at each read site on purpose: a
// rule derived from [object.decl] cannot drift, and a parameter added later inherits
// it without anyone having to remember.
package main

import (
	"fmt"

	"github.com/souls-guild/soul-stack/sdk/module"
	"google.golang.org/protobuf/types/known/structpb"
)

// checkParamTypes reports every param of f whose value does not match the type decl
// gives it, addressed as `params.<name>`. Deterministic order.
//
// Three things it deliberately does NOT do. An UNDECLARED key is left alone — the
// engine refuses one as `unknown_param` (NIM-204) and duplicating that here would
// only give it a second wording. An ABSENT key is left alone — that is what a
// default is for. And a NULL is read as absent, because `tls:` written with nothing
// after it arrives as one and means "unset", not "a value of the wrong type".
//
// Nothing here is about a value's CONTENT: an unparseable PEM and a keyspace name
// with a space in it are the action's own business. This answers one question, the
// one that was answered by guessing.
func checkParamTypes(decl module.Input, f map[string]*structpb.Value) []string {
	if len(decl) == 0 || len(f) == 0 {
		return nil
	}
	var errs []string
	for _, name := range sortedKeys(f) {
		p, declared := decl[name]
		if !declared || p.Type == "" {
			continue
		}
		v := f[name]
		if v == nil {
			continue
		}
		if _, isNull := v.GetKind().(*structpb.Value_NullValue); isNull {
			continue
		}
		if want, ok := paramTypeMismatch(p.Type, v); !ok {
			errs = append(errs, fmt.Sprintf("params.%s: must be %s, got %s", name, want, valueTypeName(v)))
		}
	}
	return errs
}

// paramTypeMismatch answers whether v carries the declared type, and names that type
// for the message when it does not.
//
// The synonyms are the ones `sdk/schema` admits (`docs/input.md` spellings), so a
// module declaring `module.Integer` is held to the same rule as one declaring
// `module.Int`. An INT is not merely a number: 7.5 where an int is declared is
// refused rather than truncated to 7, for the reason the whole file exists —
// truncation is a guess, and a replication factor of 2.5 silently becoming 2 is the
// same class of surprise as `tls: "true"` addressing plaintext.
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
		// A type this build does not know is not a licence to guess, but it is also
		// not this function's to refuse: it can only come from a newer sdk/schema,
		// and the schema validator is what reports that.
		return "", true
	}
}

// valueTypeName names what actually arrived, in the same vocabulary the message asks
// for. Without it "must be a boolean" leaves an author to work out which of their
// values is the string.
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
