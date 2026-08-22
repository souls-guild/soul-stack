package cel

import (
	"fmt"
	"reflect"

	"github.com/souls-guild/soul-stack/shared/secretpolicy"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
)

// The CEL generate_secret() function ([ADR-0083] §3). Declares that a secret is WANTED
// here under a given policy; it does not mint one:
//
//	${ compute.acl_inventory.map(u, merge(u, {
//	     'password': generate_secret({ 'length': 32, 'charset': 'alphanumeric' })
//	   })) }
//
// `core.state.present` resolves the request at write time — an existing value is kept,
// an empty field is minted per the policy ([ADR-0083] §4). The function itself is pure,
// which is the whole point: render is re-run per passage and per retry, so a function
// that returned a string would mint a different one on every evaluation and make
// correctness rest entirely on the writer refusing to overwrite.
//
// The argument is a MAP because CEL has no keyword arguments — `=` is not a token, so
// `generate_secret(length=32)` fails in the lexer, before anything could intervene
// ([ADR-0083] §Rejected). `generate_secret({})` is the all-defaults spelling.
//
// The policy is parsed EAGERLY, here, against the same grammar `core.vault.kv-present`
// parses ([secretpolicy]): a typo is a render error pointing at the expression the
// author wrote, not a surprise surfacing inside a module two phases later.
//
// Unlike vault(), registration needs no macro: vault() has a macro solely to inject the
// hidden `__vault_resolver` argument, and a request carries nothing hidden.

// generateSecretFuncName — the function name in the CEL env. There is no expanded
// internal form; the author's spelling is the only one.
const generateSecretFuncName = "generate_secret"

// secretRequestType — the CEL type of a request. Opaque on purpose: a request has no
// fields to read and no string form, so `${ 'pw=' + generate_secret({}) }` and
// `generate_secret({}).length` are compile/eval errors rather than something that
// renders. It leaves CEL only as data, via [secretpolicy.Marker].
var secretRequestType = types.NewOpaqueType("soulstack.SecretRequest")

// secretRequest — the ref.Val carrying one parsed policy through an expression. It
// holds the marker form directly ([secretpolicy.Marker]) because that is what has to
// come out at the CEL→params boundary: EvalInterpolation takes .Value() and normalises
// it with toNative, so the request lands in params as plain data that structpb can
// carry, with no Go type on either side of the boundary.
type secretRequest struct {
	policy secretpolicy.Policy
	native map[string]any
}

// Value returns the marker map — the request's travelling form (see [secretpolicy]).
func (r *secretRequest) Value() any { return r.native }

// Type returns [secretRequestType].
func (r *secretRequest) Type() ref.Type { return secretRequestType }

// Equal compares two requests by the policy they carry. Requests are values, not
// identities: two calls with the same policy describe the same request.
func (r *secretRequest) Equal(other ref.Val) ref.Val {
	o, ok := other.(*secretRequest)
	if !ok {
		return types.False
	}
	return types.Bool(r.policy.Length == o.policy.Length && string(r.policy.Alphabet) == string(o.policy.Alphabet))
}

// ConvertToType supports only the type-of query. In particular a conversion to string
// is an ERROR, and that is load-bearing: [stringify] falls back to ConvertToType for a
// cell that mixes literal text with `${ … }`, so refusing here is what turns
// `password: "pw-${ generate_secret({}) }"` into the "cannot be concatenated with a
// string" eval error instead of rendering a marker into a config file.
func (r *secretRequest) ConvertToType(t ref.Type) ref.Val {
	if t == types.TypeType {
		return secretRequestType
	}
	return types.NewErr("generate_secret(): a SecretRequest cannot be converted to %s — it is resolved by core.state.present, not rendered", t.TypeName())
}

// ConvertToNative yields the marker map for map/any targets and an error for anything
// else. The render path reads .Value() rather than calling this, but a ref.Val is
// reachable by other adapters and a wrong-shaped conversion must fail rather than
// produce something that looks like a value.
func (r *secretRequest) ConvertToNative(t reflect.Type) (any, error) {
	switch t {
	case reflect.TypeOf(map[string]any{}), reflect.TypeOf((*any)(nil)).Elem():
		return r.native, nil
	}
	return nil, fmt.Errorf("generate_secret(): a SecretRequest cannot be converted to %s", t)
}

// secretRequestEnvOptions returns the EnvOptions registering generate_secret(). One
// overload: map argument, SecretRequest result.
func secretRequestEnvOptions() []cel.EnvOption {
	return []cel.EnvOption{
		cel.Function(generateSecretFuncName,
			cel.Overload(generateSecretFuncName+"_map_secretrequest",
				[]*cel.Type{cel.MapType(cel.StringType, cel.DynType)}, secretRequestType,
				cel.UnaryBinding(callGenerateSecret),
			),
		),
	}
}

// callGenerateSecret — the binding of generate_secret(policy). The argument is
// normalised with toNative (the same unwrap the render boundary uses, so a map from a
// literal, from a variable or from a comprehension all arrive the same shape) and
// parsed against the shared grammar. A policy error is a normal CEL eval error; there
// is no secret material anywhere in it, so the text is quoted as-is.
func callGenerateSecret(arg ref.Val) ref.Val {
	m, ok := toNative(arg.Value()).(map[string]any)
	if !ok {
		return types.NewErr("generate_secret(): the policy argument must be a map, got %s", arg.Type().TypeName())
	}
	p, err := secretpolicy.Parse(m, secretpolicy.Default())
	if err != nil {
		return types.NewErr("generate_secret(): %v", err)
	}
	return &secretRequest{policy: p, native: secretpolicy.Marker(p)}
}
