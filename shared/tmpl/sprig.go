package tmpl

import (
	"fmt"
	"text/template"

	"github.com/Masterminds/sprig/v3"
)

// allowedSprig is the closed allowlist of sprig functions per [templating.md §3.3].
// Whitelist, not denylist: anything not in this set is unavailable to templates. On a
// sprig upgrade the allowlist is reviewed explicitly — new functions are denied by default.
//
// Deliberately excluded (out of the set, for reference — denylist in [templating.md §3.3]):
//   - environment/execution/network: env, expandenv, exec, getHostByName;
//   - crypto generation: derivePassword, genCA, genPrivateKey,
//     genSelfSignedCert, genSignedCert, buildCustomCert;
//   - randomness (non-deterministic render): randAlphaNum, randAlpha,
//     randAscii, randNumeric, randBytes;
//   - metaprogramming (SSTI): tpl, include.
//
// [templating.md §3.3]: docs/templating.md
var allowedSprig = []string{
	// Nil-handling.
	"default", "coalesce", "empty",

	// Strings.
	"upper", "lower", "trim", "trimAll", "trimPrefix", "trimSuffix",
	"quote", "squote", "replace", "repeat", "split", "splitList", "join",

	// Conversion.
	// toYaml/fromYaml are NOT in this list: they are absent from upstream sprig
	// (Helm-only). They are implemented as Soul Stack's own functions in
	// yaml_funcs.go ([customFuncs]) and mixed into the FuncMap separately
	// ([templating.md §3.3]).
	"toString", "int", "int64", "float64",
	"toJson", "fromJson",

	// Arithmetic.
	"add", "sub", "mul", "div", "mod",

	// Base64 / hash (no secret generation).
	"b64enc", "b64dec", "sha256sum",
}

// buildFuncMap builds the FuncMap for Engine: functions from the sprig allowlist (taken
// from sprig.TxtFuncMap()) plus Soul Stack's own functions ([customFuncs] —
// toYaml/fromYaml, which sprig lacks). Go text/template built-ins (eq, index, len,
// printf, …) are added by the engine automatically and are not in this set
// ([templating.md §3.3]).
//
// If an allowlist name is absent from the current sprig version, that is an
// allowlist/build mismatch and an error is returned (a bug, not user input).
func buildFuncMap() (template.FuncMap, error) {
	src := sprig.TxtFuncMap()
	custom := customFuncs()
	funcs := make(template.FuncMap, len(allowedSprig)+len(custom))
	for _, name := range allowedSprig {
		fn, ok := src[name]
		if !ok {
			return nil, fmt.Errorf("tmpl: function %q from allowlist missing in sprig", name)
		}
		funcs[name] = fn
	}
	for name, fn := range custom {
		funcs[name] = fn
	}
	return funcs, nil
}
