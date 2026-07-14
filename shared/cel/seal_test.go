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
	src := SealSources{SecretInputs: map[string]bool{"password": true}}

	if !e.DetectSealed("${ input.password }", src) {
		t.Fatal("input.password должен быть sealed")
	}
	if e.DetectSealed("${ input.hostname }", src) {
		t.Fatal("несекретный input.hostname НЕ должен быть sealed")
	}
}

func TestDetectSealed_PlainLiteralNotSealed(t *testing.T) {
	e := newSealEngine(t)
	src := SealSources{SecretInputs: map[string]bool{"password": true}}
	if e.DetectSealed("just a config line", src) {
		t.Fatal("чистый литерал без ${} не sealed")
	}
}

func TestDetectSealed_VaultCall(t *testing.T) {
	e := newSealEngine(t)
	if !e.DetectSealed("${ vault('secret/redis/admin').password }", SealSources{}) {
		t.Fatal("vault(...) должен быть sealed")
	}
	if !e.DetectSealed("${ vault('secret/redis/admin#password') }", SealSources{}) {
		t.Fatal("vault(... #field) должен быть sealed")
	}
}

func TestDetectSealed_TernaryReadsSecret(t *testing.T) {
	e := newSealEngine(t)
	src := SealSources{SecretInputs: map[string]bool{"tls_cert": true}}
	// Ternary: the secret is read only in the then branch — the whole cell is still sealed.
	if !e.DetectSealed("${ has(input.tls_cert) ? input.tls_cert : '' }", src) {
		t.Fatal("тернарник, читающий secret-input в любой ветке, sealed")
	}
	// A ternary without a secret input — not sealed.
	if e.DetectSealed("${ input.enabled ? 'on' : 'off' }", SealSources{SecretInputs: map[string]bool{"tls_cert": true}}) {
		t.Fatal("тернарник без secret-чтения НЕ sealed")
	}
}

func TestDetectSealed_MixedLiteralAndSecret(t *testing.T) {
	e := newSealEngine(t)
	src := SealSources{SecretInputs: map[string]bool{"password": true}}
	// Concatenating literal + secret → the whole result is sealed (whole-value taint).
	if !e.DetectSealed("requirepass ${ input.password }", src) {
		t.Fatal("смешение литерала и secret-input → весь sealed")
	}
}

func TestDetectSealed_TransitiveVarsCompute(t *testing.T) {
	e := newSealEngine(t)
	src := SealSources{
		SealedVars:    map[string]bool{"pw": true},
		SealedCompute: map[string]bool{"token": true},
	}
	if !e.DetectSealed("${ vars.pw }", src) {
		t.Fatal("vars.<sealed> транзитивно sealed")
	}
	if !e.DetectSealed("${ compute.token }", src) {
		t.Fatal("compute.<sealed> транзитивно sealed")
	}
	if e.DetectSealed("${ vars.public }", src) {
		t.Fatal("vars.<не-sealed> не sealed")
	}
}

func TestDetectSealed_NestedSelect(t *testing.T) {
	e := newSealEngine(t)
	// Nesting: vars.tls_cert inside indexing/access — the traversal visits the
	// vars.tls_cert pair, which is in SealedVars → sealed.
	src := SealSources{SealedVars: map[string]bool{"tls_cert": true}}
	if !e.DetectSealed("${ vars.tls_cert.length() > 0 ? vars.tls_cert : '' }", src) {
		t.Fatal("вложенное обращение к sealed vars → sealed")
	}
}
