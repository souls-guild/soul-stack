package secretpolicy

import "fmt"

// The secret-request marker ([ADR-0083] §3). `generate_secret({…})` in CEL evaluates
// to a REQUEST, not to a value: render is re-run per passage and per retry, so a
// function that minted a string would produce a different one on every evaluation.
// The request has to cross two boundaries that carry no Go types — the CEL→params
// boundary (native data) and params→protobuf (structpb) — so its travelling form is
// an ordinary map under one reserved key:
//
//	{"__secret_request": {"length": 32, "allowed_chars": "abc…"}}
//
// `core.state.present` recognises the key on a property declared `type: secret` and
// mints a value only when the field is empty ([ADR-0083] §4). Anywhere else the map
// is inert data.
//
// The inner form is the RESOLVED policy, not the author's spelling: the CEL call
// parses eagerly, so `{'charset': 'hexx'}` is a render error at the call site rather
// than a surprise inside the module, and what travels has exactly one reading. The
// alphabet is spelled out for the same reason — a charset NAME would have to mean the
// same thing in two places that could drift.
//
// An author can of course write the marker map out by hand. It is a forgery with no
// privilege: the effect is identical to calling the function, and [FromMarker] puts it
// through the same [Parse] bounds. There is nothing to gain by faking a request for
// something the platform grants on request.

// MarkerKey — the reserved key identifying a secret-request marker. The `__` prefix is
// the CEL layer's reserved namespace ([internalIdentGuard], shared/cel/functions.go).
const MarkerKey = "__secret_request"

// Marker renders p into its travelling form (see the package comment). Round-trips
// through [FromMarker] exactly: the alphabet is written literally, so no charset name
// has to be resolved on the way back.
func Marker(p Policy) map[string]any {
	return map[string]any{
		MarkerKey: map[string]any{
			"length":        p.Length,
			"allowed_chars": string(p.Alphabet),
		},
	}
}

// FromMarker reports whether v is a secret-request marker and returns the policy it
// carries. Not a marker → (zero, false, nil): the caller keeps the value as data. A
// marker whose payload doesn't parse → an error, never a silent default — a malformed
// request must not quietly mint a secret under a policy nobody wrote.
func FromMarker(v any) (Policy, bool, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return Policy{}, false, nil
	}
	raw, ok := m[MarkerKey]
	if !ok {
		return Policy{}, false, nil
	}
	if len(m) != 1 {
		return Policy{}, true, fmt.Errorf("%s: the marker must be the only key of its object, got %d", MarkerKey, len(m))
	}
	inner, ok := raw.(map[string]any)
	if !ok {
		return Policy{}, true, fmt.Errorf("%s: expected an object, got %T", MarkerKey, raw)
	}
	p, err := Parse(inner, Default())
	if err != nil {
		return Policy{}, true, fmt.Errorf("%s: %w", MarkerKey, err)
	}
	return p, true, nil
}
