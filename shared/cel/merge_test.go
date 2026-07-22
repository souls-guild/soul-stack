package cel

import (
	"errors"
	"reflect"
	"testing"

	"github.com/souls-guild/soul-stack/shared/audit"
)

// TestMerge_LastWins — the right argument overrides the left on a matching
// top-level key; non-matching keys are merged.
func TestMerge_LastWins(t *testing.T) {
	e := newEngine(t)

	out, err := e.EvalInterpolation(
		`${ merge(input.a, input.b) }`,
		Vars{Input: map[string]any{
			"a": map[string]any{"x": int64(1), "y": int64(2)},
			"b": map[string]any{"y": int64(20), "z": int64(30)},
		}},
	)
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	got, ok := out.(map[string]any)
	if !ok {
		t.Fatalf("result type %T, want map[string]any", out)
	}
	want := map[string]any{"x": int64(1), "y": int64(20), "z": int64(30)}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("merge = %v, want %v (last-wins)", got, want)
	}
}

// TestMerge_Shallow — a nested map is NOT merged deeply: the right argument
// wholly replaces the value of a matching top key (even if it is a map).
func TestMerge_Shallow(t *testing.T) {
	e := newEngine(t)

	out, err := e.EvalInterpolation(
		`${ merge(input.a, input.b) }`,
		Vars{Input: map[string]any{
			"a": map[string]any{"nested": map[string]any{"keep": int64(1), "drop": int64(2)}},
			"b": map[string]any{"nested": map[string]any{"keep": int64(99)}},
		}},
	)
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	got := out.(map[string]any)
	nested, ok := got["nested"].(map[string]any)
	if !ok {
		t.Fatalf("nested type %T, want map[string]any", got["nested"])
	}
	// SHALLOW: the right wholly replaced — the `drop` key from the left is gone.
	want := map[string]any{"keep": int64(99)}
	if !reflect.DeepEqual(nested, want) {
		t.Fatalf("nested = %v, want %v (shallow, right wholly replaces)", nested, want)
	}
}

// TestMerge_EmptyMaps — empty maps: merging empties yields empty; an empty does
// not overwrite a non-empty.
func TestMerge_EmptyMaps(t *testing.T) {
	e := newEngine(t)

	out, err := e.EvalInterpolation(
		`${ merge(input.empty, input.filled, input.empty2) }`,
		Vars{Input: map[string]any{
			"empty":  map[string]any{},
			"filled": map[string]any{"k": "v"},
			"empty2": map[string]any{},
		}},
	)
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	got := out.(map[string]any)
	want := map[string]any{"k": "v"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("merge with empty = %v, want %v", got, want)
	}

	// merge of two empties → empty map.
	out2, err := e.EvalInterpolation(`${ merge(input.empty, input.empty2) }`, Vars{Input: map[string]any{
		"empty":  map[string]any{},
		"empty2": map[string]any{},
	}})
	if err != nil {
		t.Fatalf("eval (both empty): %v", err)
	}
	if got2 := out2.(map[string]any); len(got2) != 0 {
		t.Fatalf("merge of two empties = %v, want empty map", got2)
	}
}

// TestMerge_SingleArg — single argument: merge returns a copy of it (a valid
// variadic form with a minimum of 1 argument).
func TestMerge_SingleArg(t *testing.T) {
	e := newEngine(t)

	out, err := e.EvalInterpolation(`${ merge(input.a) }`, Vars{Input: map[string]any{
		"a": map[string]any{"x": int64(1), "y": int64(2)},
	}})
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	got := out.(map[string]any)
	want := map[string]any{"x": int64(1), "y": int64(2)}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("merge(one) = %v, want %v", got, want)
	}
}

// TestMerge_ManyArgs — >2 arguments: merged left to right, the last wins over all.
func TestMerge_ManyArgs(t *testing.T) {
	e := newEngine(t)

	out, err := e.EvalInterpolation(
		`${ merge(input.a, input.b, input.c, input.d) }`,
		Vars{Input: map[string]any{
			"a": map[string]any{"k": int64(1), "a_only": "A"},
			"b": map[string]any{"k": int64(2), "b_only": "B"},
			"c": map[string]any{"k": int64(3), "c_only": "C"},
			"d": map[string]any{"k": int64(4), "d_only": "D"},
		}},
	)
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	got := out.(map[string]any)
	want := map[string]any{
		"k": int64(4), "a_only": "A", "b_only": "B", "c_only": "C", "d_only": "D",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("merge(4) = %v, want %v", got, want)
	}
}

// TestMergeList_Flatten — the merge(list(map)) form: a single list-of-maps
// argument is flattened left to right, last-wins (a later list element beats an earlier).
func TestMergeList_Flatten(t *testing.T) {
	e := newEngine(t)

	out, err := e.EvalInterpolation(
		`${ merge(input.layers) }`,
		Vars{Input: map[string]any{
			"layers": []any{
				map[string]any{"k": int64(1), "a_only": "A"},
				map[string]any{"k": int64(2), "b_only": "B"},
				map[string]any{"k": int64(3), "c_only": "C"},
			},
		}},
	)
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	got, ok := out.(map[string]any)
	if !ok {
		t.Fatalf("result type %T, want map[string]any", out)
	}
	want := map[string]any{"k": int64(3), "a_only": "A", "b_only": "B", "c_only": "C"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("merge(list) = %v, want %v (flatten left-to-right last-wins)", got, want)
	}
}

// TestMergeList_FromComprehension — the main use case: the collection comes from
// .map(...) over a map (a CEL comprehension yields a LIST of maps), and merge(list)
// folds it into a "name→object" map. This is the deterministic users.acl pattern:
// a list from .map() → a map that the template ranges over by sorted keys.
func TestMergeList_FromComprehension(t *testing.T) {
	e := newEngine(t)

	out, err := e.EvalInterpolation(
		`${ merge(input.users.map(name, {name: {'perms': input.users[name].perms}})) }`,
		Vars{Input: map[string]any{
			"users": map[string]any{
				"zeta":  map[string]any{"perms": "~* +@all"},
				"alpha": map[string]any{"perms": "~app:* +@read"},
			},
		}},
	)
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	got, ok := out.(map[string]any)
	if !ok {
		t.Fatalf("result type %T, want map[string]any", out)
	}
	want := map[string]any{
		"zeta":  map[string]any{"perms": "~* +@all"},
		"alpha": map[string]any{"perms": "~app:* +@read"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("merge(comprehension) = %v, want %v", got, want)
	}
}

// TestMergeList_LastWinsWithinList — last-wins within a list: the same key in
// different list elements → the last wins.
func TestMergeList_LastWinsWithinList(t *testing.T) {
	e := newEngine(t)

	out, err := e.EvalInterpolation(
		`${ merge(input.layers) }`,
		Vars{Input: map[string]any{
			"layers": []any{
				map[string]any{"dup": "first", "x": int64(1)},
				map[string]any{"dup": "second"},
				map[string]any{"dup": "third", "y": int64(2)},
			},
		}},
	)
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	got := out.(map[string]any)
	want := map[string]any{"dup": "third", "x": int64(1), "y": int64(2)}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("merge(list) last-wins = %v, want %v", got, want)
	}
}

// TestMergeList_Empty — an empty list → an empty map (a valid degenerate form;
// precedent: empty users → users.acl with no users).
func TestMergeList_Empty(t *testing.T) {
	e := newEngine(t)

	out, err := e.EvalInterpolation(`${ merge(input.empty) }`, Vars{Input: map[string]any{
		"empty": []any{},
	}})
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	got, ok := out.(map[string]any)
	if !ok {
		t.Fatalf("result type %T, want map[string]any", out)
	}
	if len(got) != 0 {
		t.Fatalf("merge(empty list) = %v, want empty map", got)
	}
}

// TestMergeList_NonMapElement — a non-map list element → a clear error (not
// silent swallowing). Any error class is acceptable.
func TestMergeList_NonMapElement(t *testing.T) {
	e := newEngine(t)

	_, err := e.EvalExpression(`merge(input.layers)`, Vars{Input: map[string]any{
		"layers": []any{
			map[string]any{"x": int64(1)},
			"i-am-a-string",
		},
	}})
	if err == nil {
		t.Fatal("merge(list) with non-map element: expected error, got nil")
	}
	var ce *ErrCompile
	var ee *ErrEval
	if !errors.As(err, &ce) && !errors.As(err, &ee) {
		t.Fatalf("merge(list) non-map element: error = %v, want *ErrCompile or *ErrEval", err)
	}
}

// TestMergeList_AvailableInFlowControl — the list form of merge() is likewise
// available in the Soul-side flow-control sandbox (same pure function, same env).
func TestMergeList_AvailableInFlowControl(t *testing.T) {
	e := newFlowControlEngine(t)

	out, err := e.EvalExpression(
		`merge(register.layers).k == "v2"`,
		Vars{Register: map[string]any{
			"layers": []any{
				map[string]any{"k": "v1"},
				map[string]any{"k": "v2"},
			},
		}},
	)
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	if got := out.Value(); got != true {
		t.Fatalf("result = %v, want true (list last-wins in flow-control)", got)
	}
}

// TestMerge_VarargsBackCompat — additive guarantee: the varargs form merge(m, m...)
// is not broken by introducing the list overload. A direct back-compat regression guard.
func TestMerge_VarargsBackCompat(t *testing.T) {
	e := newEngine(t)

	out, err := e.EvalInterpolation(
		`${ merge(input.a, input.b) }`,
		Vars{Input: map[string]any{
			"a": map[string]any{"x": int64(1)},
			"b": map[string]any{"x": int64(2), "y": int64(3)},
		}},
	)
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	got := out.(map[string]any)
	want := map[string]any{"x": int64(2), "y": int64(3)}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("varargs merge = %v, want %v (back-compat broken)", got, want)
	}
}

// TestMerge_TooManyVarargs — merge CONTRACT (flagged, QA gap 2026-06-22): the
// varargs form is declared for 1..mergeMaxArity (=8) map arguments. >8 arguments
// → a no-such-overload compile error (extensible without a breaking change by
// adding overloads). For arbitrarily large collections the merge(list(map)) form
// exists — it is NOT arity-limited. This test pins the boundary at 8: to merge 9+
// layers, wrap them in a list and call merge(list).
func TestMerge_TooManyVarargs(t *testing.T) {
	e := newEngine(t)

	// 9 map arguments: mergeMaxArity=8 → no overload for 9.
	expr := `merge(input.a, input.b, input.c, input.d, input.e, input.f, input.g, input.h, input.i)`
	in := map[string]any{}
	for _, k := range []string{"a", "b", "c", "d", "e", "f", "g", "h", "i"} {
		in[k] = map[string]any{k: int64(1)}
	}
	_, err := e.EvalExpression(expr, Vars{Input: in})
	if err == nil {
		t.Fatal("merge(9 arguments): expected a compile error no-such-overload, got nil")
	}
	var ce *ErrCompile
	if !errors.As(err, &ce) {
		t.Fatalf("merge(9): error = %v, want *ErrCompile (no such overload)", err)
	}
}

// TestMerge_NonMapArg — a non-map argument → an error (a no-such-overload compile
// error for a statically known type, or an eval error on dyn concatenation). Any
// error class is acceptable — the point is that merge does not silently swallow a non-map.
func TestMerge_NonMapArg(t *testing.T) {
	e := newEngine(t)

	_, err := e.EvalExpression(`merge(input.a, input.notmap)`, Vars{Input: map[string]any{
		"a":      map[string]any{"x": int64(1)},
		"notmap": "i-am-a-string",
	}})
	if err == nil {
		t.Fatal("merge with non-map argument: expected error, got nil")
	}
	var ce *ErrCompile
	var ee *ErrEval
	if !errors.As(err, &ce) && !errors.As(err, &ee) {
		t.Fatalf("merge with non-map: error = %v, want *ErrCompile or *ErrEval", err)
	}
}

// TestMerge_AvailableInFlowControl — the Soul-side flow-control sandbox
// ([ADR-012(d)]) gets merge(): a pure function with no external context,
// symmetric with scenario expressions.
func TestMerge_AvailableInFlowControl(t *testing.T) {
	e := newFlowControlEngine(t)

	out, err := e.EvalExpression(
		`merge(register.a, register.b).k == "v2"`,
		Vars{Register: map[string]any{
			"a": map[string]any{"k": "v1"},
			"b": map[string]any{"k": "v2"},
		}},
	)
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	if got := out.Value(); got != true {
		t.Fatalf("result = %v, want true (last-wins in flow-control)", got)
	}
}

// TestMerge_SecretMaskedSameAsDirectVault — BLOCKER guard ([ADR-010 Amendment
// 2026-06-22], security/architect): a secret that entered a merged map via
// vault() is masked by the output layer (shared/audit.MaskSecrets) IDENTICALLY to
// a direct ${ vault(...) } — it does NOT leak into logs/OTel/RunResult.
//
// Masking mechanism: vault() resolves to real plaintext keeper-side (the Soul
// gets the actual secret), and masking happens on output by (a) the destination
// key's sensitive name and (b) the vault-ref marker. merge() keeps top-level keys
// without renaming, so a secret under the `password` key is masked the same as by
// direct substitution. The test proves both branches via MaskSecrets.
func TestMerge_SecretMaskedSameAsDirectVault(t *testing.T) {
	kv := &stubKV{secrets: map[string]map[string]any{
		"secret/redis/admin": {"password": "s3cr3t-plaintext"},
	}}
	e := newVaultEngine(t, kv)

	// Direct ${ vault(...) } under the `password` key — the masking baseline.
	direct, err := e.EvalInterpolation("${ vault('secret/redis/admin#password') }", Vars{})
	if err != nil {
		t.Fatalf("eval direct vault: %v", err)
	}
	if direct != "s3cr3t-plaintext" {
		t.Fatalf("direct vault resolved %v, want real plaintext (keeper-side resolve)", direct)
	}
	directPayload := map[string]any{"password": direct}
	maskedDirect := audit.MaskSecrets(directPayload)
	if maskedDirect["password"] == "s3cr3t-plaintext" {
		t.Fatal("baseline: direct vault secret NOT masked by MaskSecrets - masking layer broken")
	}

	// The same secret, but via merge(defaults, {password: vault(...)}).
	merged, err := e.EvalInterpolation(
		`${ merge(input.defaults, {'password': vault('secret/redis/admin#password')}) }`,
		Vars{Input: map[string]any{
			"defaults": map[string]any{"maxmemory": "256mb", "appendonly": "yes"},
		}},
	)
	if err != nil {
		t.Fatalf("eval merge+vault: %v", err)
	}
	mergedMap, ok := merged.(map[string]any)
	if !ok {
		t.Fatalf("merge result type %T, want map[string]any", merged)
	}
	// Control: the secret really landed in the merged map as plaintext (pre-masking).
	if mergedMap["password"] != "s3cr3t-plaintext" {
		t.Fatalf("merged.password = %v, want plaintext secret (vault resolves in merge)", mergedMap["password"])
	}

	maskedMerged := audit.MaskSecrets(mergedMap)
	// Main assertion: the merged secret is masked IDENTICALLY to the direct one.
	if maskedMerged["password"] != maskedDirect["password"] {
		t.Fatalf("merged.password masked as %v, direct as %v - MISMATCH (secret leaks through merge)",
			maskedMerged["password"], maskedDirect["password"])
	}
	// And literally: no plaintext secret remains in the masked output.
	if maskedMerged["password"] == "s3cr3t-plaintext" {
		t.Fatal("merged.password NOT masked - secret leaks into the output layer through merge()")
	}
	// Non-secret keys of the merged map pass through (no over-masking).
	if maskedMerged["maxmemory"] != "256mb" || maskedMerged["appendonly"] != "yes" {
		t.Fatalf("non-secret keys are masked: %v (over-masking)", maskedMerged)
	}
}

// TestMerge_TLSKeyMaskedSameAsDirectVault — BLOCKER masking-guard (redis TLS
// consolidation): a PEM client-key that entered a merged map via
// merge(defaults, {tls_key: vault(...)}) is masked by the output layer
// (shared/audit.MaskSecrets) IDENTICALLY to a direct ${ vault(...) } under the
// tls_key key — it does NOT leak into logs/OTel/RunResult. Proves that merge()
// neither widens nor narrows the masking boundary for TLS PEM: tls_key is a
// sensitive key name (sensitiveKeyRe extended by the fragment tls[_-]?(key|cert|ca)),
// so masking is the same under merge and by direct substitution. merge-masking-guard class.
func TestMerge_TLSKeyMaskedSameAsDirectVault(t *testing.T) {
	const pem = "-----BEGIN PRIVATE KEY-----\nMIIE-plaintext\n-----END PRIVATE KEY-----"
	kv := &stubKV{secrets: map[string]map[string]any{
		"secret/services/redis/tls": {"key": pem, "cert": "CERTPEM", "ca": "CAPEM"},
	}}
	e := newVaultEngine(t, kv)

	// Baseline: direct ${ vault(...) } under the tls_key key.
	direct, err := e.EvalInterpolation("${ vault('secret/services/redis/tls#key') }", Vars{})
	if err != nil {
		t.Fatalf("eval direct vault: %v", err)
	}
	if direct != pem {
		t.Fatalf("direct vault resolved %v, want PEM plaintext (keeper-side)", direct)
	}
	maskedDirect := audit.MaskSecrets(map[string]any{"tls_key": direct})
	if maskedDirect["tls_key"] == pem {
		t.Fatal("baseline: direct tls_key NOT masked - TLS masking layer broken (sensitiveKeyRe misses tls_key)")
	}

	// The same PEM via merge(defaults, {tls_key/tls_cert/tls_ca: vault(...)}).
	merged, err := e.EvalInterpolation(
		`${ merge(input.defaults, {
			'tls_key':  vault('secret/services/redis/tls#key'),
			'tls_cert': vault('secret/services/redis/tls#cert'),
			'tls_ca':   vault('secret/services/redis/tls#ca')
		}) }`,
		Vars{Input: map[string]any{
			"defaults": map[string]any{"tls-port": "7379"},
		}},
	)
	if err != nil {
		t.Fatalf("eval merge+vault: %v", err)
	}
	mergedMap := merged.(map[string]any)
	// Control: the PEM really landed in the merged map as plaintext (pre-masking).
	if mergedMap["tls_key"] != pem {
		t.Fatalf("merged.tls_key = %v, want PEM plaintext (vault resolves in merge)", mergedMap["tls_key"])
	}

	maskedMerged := audit.MaskSecrets(mergedMap)
	if maskedMerged["tls_key"] != maskedDirect["tls_key"] {
		t.Fatalf("merged.tls_key masked as %v, direct as %v - MISMATCH (PEM leaks through merge)",
			maskedMerged["tls_key"], maskedDirect["tls_key"])
	}
	if maskedMerged["tls_key"] == pem {
		t.Fatal("merged.tls_key NOT masked - PEM client-key leaks through merge()")
	}
	// cert/ca are masked too (secret names); the non-secret tls-port passes through.
	if maskedMerged["tls_cert"] == "CERTPEM" || maskedMerged["tls_ca"] == "CAPEM" {
		t.Fatalf("tls_cert/tls_ca NOT masked: cert=%v ca=%v", maskedMerged["tls_cert"], maskedMerged["tls_ca"])
	}
	if maskedMerged["tls-port"] != "7379" {
		t.Fatalf("non-secret tls-port masked: %v (over-masking)", maskedMerged["tls-port"])
	}
}

// TestMerge_SecretUnderNonSensitiveKeyNotMasked — a NEGATIVE boundary-invariant
// guard: a secret that entered a merged map under a NON-sensitive key is NOT
// masked by the output layer. vault() resolves to plaintext keeper-side, and the
// secret value lands in the map (without a `vault:` marker) — the vault-ref branch
// of MaskSecrets does not fire, and the sensitive-key branch does not match a
// non-secret name. This is the SCENARIO AUTHOR's RESPONSIBILITY (to put a secret
// under a secret-named key), not merge()'s. The test pins that merge() neither
// widens nor narrows the masking boundary — behavior is symmetric with a direct
// ${ vault(...) } under the same key.
func TestMerge_SecretUnderNonSensitiveKeyNotMasked(t *testing.T) {
	kv := &stubKV{secrets: map[string]map[string]any{
		"secret/redis/admin": {"password": "s3cr3t-plaintext"},
	}}
	e := newVaultEngine(t, kv)

	// Baseline: a direct vault under the NON-sensitive `maxmemory` key is also
	// NOT masked — merge() must behave the same.
	direct, err := e.EvalInterpolation("${ vault('secret/redis/admin#password') }", Vars{})
	if err != nil {
		t.Fatalf("eval direct vault: %v", err)
	}
	directPayload := map[string]any{"maxmemory": direct}
	if audit.MaskSecrets(directPayload)["maxmemory"] != "s3cr3t-plaintext" {
		t.Fatal("baseline: direct vault under a NON-sensitive key is masked - masking model changed")
	}

	// The same secret via merge() under the NON-sensitive `maxmemory` key.
	merged, err := e.EvalInterpolation(
		`${ merge(input.defaults, {'maxmemory': vault('secret/redis/admin#password')}) }`,
		Vars{Input: map[string]any{
			"defaults": map[string]any{"appendonly": "yes"},
		}},
	)
	if err != nil {
		t.Fatalf("eval merge+vault: %v", err)
	}
	mergedMap := merged.(map[string]any)
	maskedMerged := audit.MaskSecrets(mergedMap)
	// Invariant: under a NON-sensitive key the secret passes through (merge adds
	// no masking, just like a direct vault). Correctness is on the scenario author.
	if maskedMerged["maxmemory"] != "s3cr3t-plaintext" {
		t.Fatalf("merged.maxmemory = %v, want plaintext (non-secret key - merge does not mask, symmetric with direct vault)",
			maskedMerged["maxmemory"])
	}
}

// TestMerge_ZeroArgs — the lower arity bound: merge() with no arguments → a
// no-such-overload compile error (overloads are declared for 1..mergeMaxArity;
// zero is not). Symmetric with the upper bound TestMerge_TooManyVarargs.
func TestMerge_ZeroArgs(t *testing.T) {
	e := newEngine(t)

	_, err := e.EvalExpression(`merge()`, Vars{})
	if err == nil {
		t.Fatal("merge() with no arguments: expected a compile error no-such-overload, got nil")
	}
	var ce *ErrCompile
	if !errors.As(err, &ce) {
		t.Fatalf("merge(): error = %v, want *ErrCompile (no such overload)", err)
	}
}

// TestMerge_UndeclaredInMigration — migration-CEL ([ADR-019]) is hermetic:
// merge() is NOT registered (see buildEngine). A call → a no-such-overload
// compile error, symmetric with the glob()/vault() guard of the migration env
// (minimal surface area; extending it requires a separate ADR).
func TestMerge_UndeclaredInMigration(t *testing.T) {
	e := newMigrationEngine(t)

	_, err := e.EvalExpression(`merge(state.a, state.b)`, Vars{
		State: map[string]any{
			"a": map[string]any{"x": int64(1)},
			"b": map[string]any{"y": int64(2)},
		},
	})
	var ce *ErrCompile
	if !errors.As(err, &ce) {
		t.Fatalf("merge() in migration-env: error = %v, want *ErrCompile (no such overload)", err)
	}
}
