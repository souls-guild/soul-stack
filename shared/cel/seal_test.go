package cel

import "testing"

func newSealEngine(t *testing.T) *Engine {
	t.Helper()
	// vault() must be registered (the detector does not call ReadKV — it only
	// needs vault() to parse as a function); we reuse stubKV from vault_test.go.
	e, err := New(WithVault(&stubKV{}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return e
}

func TestDetectSealed_SecretInputDirect(t *testing.T) {
	e := newSealEngine(t)
	src := SealSources{Fields: map[string]bool{FieldAddr("input", "password"): true}}

	if !e.DetectSealed("${ input.password }", src) {
		t.Fatal("input.password should be sealed")
	}
	if e.DetectSealed("${ input.hostname }", src) {
		t.Fatal("non-secret input.hostname should NOT be sealed")
	}
}

func TestDetectSealed_PlainLiteralNotSealed(t *testing.T) {
	e := newSealEngine(t)
	src := SealSources{Fields: map[string]bool{FieldAddr("input", "password"): true}}
	if e.DetectSealed("just a config line", src) {
		t.Fatal("a plain literal without ${} is not sealed")
	}
}

func TestDetectSealed_VaultCall(t *testing.T) {
	e := newSealEngine(t)
	if !e.DetectSealed("${ vault('secret/redis/admin').password }", SealSources{}) {
		t.Fatal("vault(...) should be sealed")
	}
	if !e.DetectSealed("${ vault('secret/redis/admin#password') }", SealSources{}) {
		t.Fatal("vault(... #field) should be sealed")
	}
}

func TestDetectSealed_TernaryReadsSecret(t *testing.T) {
	e := newSealEngine(t)
	src := SealSources{Fields: map[string]bool{FieldAddr("input", "tls_cert"): true}}
	// Ternary: the secret is read only in the then branch — the whole cell is still sealed.
	if !e.DetectSealed("${ has(input.tls_cert) ? input.tls_cert : '' }", src) {
		t.Fatal("a ternary reading a secret-input in either branch is sealed")
	}
	// A ternary without a secret input — not sealed.
	if e.DetectSealed("${ input.enabled ? 'on' : 'off' }", SealSources{Fields: map[string]bool{FieldAddr("input", "tls_cert"): true}}) {
		t.Fatal("a ternary without a secret read is NOT sealed")
	}
}

func TestDetectSealed_MixedLiteralAndSecret(t *testing.T) {
	e := newSealEngine(t)
	src := SealSources{Fields: map[string]bool{FieldAddr("input", "password"): true}}
	// Concatenating literal + secret → the whole result is sealed (whole-value taint).
	if !e.DetectSealed("requirepass ${ input.password }", src) {
		t.Fatal("mixing a literal and secret-input -> the whole thing is sealed")
	}
}

func TestDetectSealed_TransitiveVarsCompute(t *testing.T) {
	e := newSealEngine(t)
	src := SealSources{Fields: map[string]bool{
		FieldAddr("vars", "pw"):       true,
		FieldAddr("compute", "token"): true,
	}}
	if !e.DetectSealed("${ vars.pw }", src) {
		t.Fatal("vars.<sealed> is transitively sealed")
	}
	if !e.DetectSealed("${ compute.token }", src) {
		t.Fatal("compute.<sealed> is transitively sealed")
	}
	if e.DetectSealed("${ vars.public }", src) {
		t.Fatal("vars.<not-sealed> is not sealed")
	}
}

func TestDetectSealed_NestedSelect(t *testing.T) {
	e := newSealEngine(t)
	// Nesting: vars.tls_cert inside indexing/access — the traversal visits the
	// vars.tls_cert pair, whose address is in Fields → sealed.
	src := SealSources{Fields: map[string]bool{FieldAddr("vars", "tls_cert"): true}}
	if !e.DetectSealed("${ vars.tls_cert.length() > 0 ? vars.tls_cert : '' }", src) {
		t.Fatal("a nested reference to sealed vars -> sealed")
	}
}

// TestDetectSealed_VarsLiteralIndexIsTheField — ADR-0082 amendment 2026-10-15, point
// 15: on the `vars` root a literal index is the field the lazy view reads for it.
func TestDetectSealed_VarsLiteralIndexIsTheField(t *testing.T) {
	e := newSealEngine(t)
	src := SealSources{Fields: map[string]bool{FieldAddr("vars", "pw"): true}}
	for raw, want := range map[string]bool{
		"${ vars['pw'] }":           true,
		"${ vars['pw'].size() }":    true,
		"${ vars['public'] }":       false,
		"${ vars['public']['pw'] }": false,
	} {
		if got := e.DetectSealed(raw, src); got != want {
			t.Errorf("DetectSealed(%s) = %v, want %v", raw, got, want)
		}
	}
}

// TestDetectSealed_AVarsReadNamingNoFieldIsTaintedWhenAnyVarsNameIs — point 15: a
// whole read of `vars` and an index by a key the text does not spell name no field the
// walk can follow, so they are tainted when any `vars` name is, and only then: a
// secret under another root does not make them so.
func TestDetectSealed_AVarsReadNamingNoFieldIsTaintedWhenAnyVarsNameIs(t *testing.T) {
	e := newSealEngine(t)
	tainted := SealSources{Fields: map[string]bool{FieldAddr("vars", "pw"): true}}
	clean := SealSources{Fields: map[string]bool{FieldAddr("input", "pw"): true}}
	for _, raw := range []string{
		"${ vars }",
		"${ merge(vars, {'a': 1}) }",
		"${ vars[input.key] }",
		"${ vars[vars.public] }",
		"${ vars.filter(k, k != 'x').map(k, vars[k]) }",
		"prefix-${ vars == {} }",
	} {
		if !e.DetectSealed(raw, tainted) {
			t.Errorf("DetectSealed(%s) with a tainted vars name = false, want true", raw)
		}
		if e.DetectSealed(raw, clean) {
			t.Errorf("DetectSealed(%s) with no tainted vars name = true, want false", raw)
		}
	}
	if !e.PredicateReadsSecret("vars == {}", tainted) || e.PredicateReadsSecret("vars == {}", clean) {
		t.Error("a predicate comparing vars whole must follow the same rule")
	}
	// Every field the text names is accounted for, so naming only untainted ones is
	// not a whole read.
	if e.DetectSealed("${ vars.public + vars['other'] + string(has(vars.x)) }", tainted) {
		t.Error("reads that each name an untainted field were sealed as a whole read")
	}
}
