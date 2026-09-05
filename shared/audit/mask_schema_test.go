package audit

import (
	"testing"
)

// (e) schema-declared secret field → MASKED on read-path (MaskSecretsWithSchema).
func TestMaskSecretsWithSchema_DeclaredSecretMasked(t *testing.T) {
	in := map[string]any{
		"password": "p4ss",
		"hostname": "node-01",
	}
	schema := SecretPathSet{"password": true}
	out := MaskSecretsWithSchema(in, schema)

	if out["password"] != maskedValue {
		t.Errorf("schema-secret password = %v, want masked", out["password"])
	}
	if out["hostname"] != "node-01" {
		t.Errorf("non-secret hostname = %v, want passthrough", out["hostname"])
	}
}

// (f) plain generic field (non-secret content with config) → NOT MASKED.
// CRITICAL: no over-masking. Key names `content`/`config` are not sensitive-by-regex,
// the schema does not declare them, no vault-ref → the value passes through.
func TestMaskSecretsWithSchema_GenericFieldNotMasked(t *testing.T) {
	in := map[string]any{
		"content": "maxmemory 256mb\nappendonly yes\n",
		"config": map[string]any{
			"port":     6379,
			"loglevel": "notice",
		},
	}
	out := MaskSecretsWithSchema(in, SecretPathSet{})

	if out["content"] != "maxmemory 256mb\nappendonly yes\n" {
		t.Errorf("generic content masked: %v (over-masking!)", out["content"])
	}
	cfg, ok := out["config"].(map[string]any)
	if !ok {
		t.Fatalf("config is not a map: %T", out["config"])
	}
	if cfg["loglevel"] != "notice" || cfg["port"] != 6379 {
		t.Errorf("generic config fields masked: %v (over-masking!)", cfg)
	}
}

// schema by nested path + generalized array index.
func TestMaskSecretsWithSchema_NestedAndArrayIndex(t *testing.T) {
	in := map[string]any{
		"acl": []any{
			map[string]any{"name": "alice", "password": "s3cr3t"},
			map[string]any{"name": "bob", "password": "h0nk"},
		},
		"tls": map[string]any{
			"key":  "-----BEGIN-----",
			"port": 6379,
		},
	}
	// Generalized idx-form `acl[].password` catches both elements; `tls.key` is exact.
	schema := SecretPathSet{"acl[].password": true, "tls.key": true}
	out := MaskSecretsWithSchema(in, schema)

	acl := out["acl"].([]any)
	for i, el := range acl {
		m := el.(map[string]any)
		if m["password"] != maskedValue {
			t.Errorf("acl[%d].password = %v, want masked", i, m["password"])
		}
		if m["name"] == maskedValue {
			t.Errorf("acl[%d].name masked - over-masking", i)
		}
	}
	tls := out["tls"].(map[string]any)
	if tls["key"] != maskedValue {
		t.Errorf("tls.key = %v, want masked", tls["key"])
	}
	if tls["port"] != 6379 {
		t.Errorf("tls.port masked - over-masking")
	}
}

// schema=nil → degrades to MaskSecrets (vault+regex), schema layer off.
func TestMaskSecretsWithSchema_NilSchemaDegradesToMaskSecrets(t *testing.T) {
	in := map[string]any{
		"password":  "p", // regex-last-resort
		"dsn_ref":   "vault:secret/db",
		"plain":     "ok",
		"some_data": "value",
	}
	out := MaskSecretsWithSchema(in, nil)
	if out["password"] != maskedValue {
		t.Errorf("password = %v, want masked (regex)", out["password"])
	}
	if out["dsn_ref"] != maskedValue {
		t.Errorf("dsn_ref = %v, want masked (vault)", out["dsn_ref"])
	}
	if out["plain"] != "ok" || out["some_data"] != "value" {
		t.Errorf("non-secret fields masked: %v", out)
	}
}

// (g) regex-fallback alarm increments when regex catches a class-(ii) secret
// (bootstrap_token with no schema) not caught by the declarative layer.
func TestMaskSecretsSealed_RegexFallbackAlarm(t *testing.T) {
	var fired []string
	opts := SealOpts{
		RegexFallback: func(path string) { fired = append(fired, path) },
	}
	in := map[string]any{
		"bootstrap_token": "tok-abc", // class ii: regex catches, no schema
		"plain":           "ok",
	}
	out := MaskSecretsSealed(in, opts)

	if out["bootstrap_token"] != maskedValue {
		t.Errorf("bootstrap_token = %v, want masked", out["bootstrap_token"])
	}
	if len(fired) != 1 || fired[0] != "bootstrap_token" {
		t.Fatalf("regex-fallback alarm: got %v, want [bootstrap_token]", fired)
	}
}

// Alarm does NOT fire when the declarative layer (schema) caught the secret — regex
// was not the only layer that fired.
func TestMaskSecretsSealed_NoAlarmWhenSchemaCatches(t *testing.T) {
	var fired []string
	opts := SealOpts{
		Schema:        SecretPathSet{"api_secret": true},
		RegexFallback: func(path string) { fired = append(fired, path) },
	}
	in := map[string]any{"api_secret": "x"}
	out := MaskSecretsSealed(in, opts)
	if out["api_secret"] != maskedValue {
		t.Errorf("api_secret = %v, want masked", out["api_secret"])
	}
	if len(fired) != 0 {
		t.Fatalf("alarm fired on schema-catch: %v (regex is not the only one)", fired)
	}
}

// Alarm does NOT fire on a vault-ref under a sensitive key — the vault layer would
// catch it, regex is not the only one.
func TestMaskSecretsSealed_NoAlarmOnVaultRefValue(t *testing.T) {
	var fired []string
	opts := SealOpts{RegexFallback: func(path string) { fired = append(fired, path) }}
	in := map[string]any{"db_password": "vault:secret/db#password"}
	out := MaskSecretsSealed(in, opts)
	if out["db_password"] != maskedValue {
		t.Errorf("db_password = %v, want masked", out["db_password"])
	}
	if len(fired) != 0 {
		t.Fatalf("alarm on a vault-ref value: %v (the vault layer would catch it itself)", fired)
	}
}

// (a)/(b)/(d) seal layer: sealed path of a generic field → MASKED; vault-ref → MASKED;
// mixed value on a sealed path → MASKED.
func TestMaskSecretsSealed_SealedPaths(t *testing.T) {
	opts := SealOpts{
		Sealed: map[string]bool{
			"content":      true, // (a) generic field, marked sealed at render phase
			"requirepass":  true, // (d) mixed literal+secret value
			"config.token": true,
		},
	}
	in := map[string]any{
		"content":     "tls private material", // generic key, but a sealed path
		"requirepass": "requirepass s3cr3t",   // literal+secret concatenation
		"config":      map[string]any{"token": "x", "port": 6379},
		"public":      "not sealed",
	}
	out := MaskSecretsSealed(in, opts)

	if out["content"] != maskedValue {
		t.Errorf("sealed content = %v, want masked (a)", out["content"])
	}
	if out["requirepass"] != maskedValue {
		t.Errorf("sealed mixed = %v, want masked (d)", out["requirepass"])
	}
	cfg := out["config"].(map[string]any)
	if cfg["token"] != maskedValue {
		t.Errorf("sealed config.token = %v, want masked", cfg["token"])
	}
	if cfg["port"] != 6379 {
		t.Errorf("config.port masked - over-masking")
	}
	if out["public"] != "not sealed" {
		t.Errorf("non-sealed public masked - over-masking")
	}
}

// (b) vault-ref value in an arbitrary generic key → MASKED (vault layer 2).
func TestMaskSecretsSealed_VaultRefAnyKey(t *testing.T) {
	in := map[string]any{"connection": "host=db pass=vault:kv/app/db#pw"}
	out := MaskSecretsSealed(in, SealOpts{})
	if out["connection"] != maskedValue {
		t.Errorf("vault-ref value = %v, want masked", out["connection"])
	}
}

func TestMaskSecretsSealed_NilInput(t *testing.T) {
	if MaskSecretsSealed(nil, SealOpts{}) != nil {
		t.Fatal("nil input -> nil output")
	}
}

// NIM-810 guard. The seal is recorded against the path of the cell that read the
// secret source, and for the canonical `core.exec.run` shape that path is a SLICE
// ELEMENT — `args[1]`, never a map key. A masker that only tested map keys wrote
// the resolved password into apply_run_plan in the clear, and silently: the
// regex-last-resort reads a key NAME, and a list element has no key, so not even
// the fallback alarm fired.
//
// The four sub-cases are the four shapes a sealed element arrives in. Each must
// be masked by the seal layer ALONE — no vault ref in the value, no sensitive key
// name anywhere, no schema.
func TestMaskSecretsSealed_SealedSliceElement(t *testing.T) {
	t.Run("string element", func(t *testing.T) {
		var fired []string
		opts := SealOpts{
			Sealed:        map[string]bool{"args[1]": true},
			RegexFallback: func(p string) { fired = append(fired, p) },
		}
		in := map[string]any{
			"cmd":  "/usr/bin/mysql",
			"args": []any{"--user=root", "--password=hunter2"},
		}
		out := MaskSecretsSealed(in, opts)

		args := out["args"].([]any)
		if args[1] != maskedValue {
			t.Errorf("sealed args[1] = %v, want masked", args[1])
		}
		if args[0] != "--user=root" {
			t.Errorf("args[0] = %v, want passthrough (over-masking)", args[0])
		}
		if out["cmd"] != "/usr/bin/mysql" {
			t.Errorf("cmd = %v, want passthrough (over-masking)", out["cmd"])
		}
		// The declarative layer caught it, so the regex fallback is not the sole
		// hit and the declarative-gap alarm must stay quiet.
		if len(fired) != 0 {
			t.Errorf("alarm fired on a seal-catch: %v", fired)
		}
	})

	t.Run("map element", func(t *testing.T) {
		// A sealed path over an element that resolves to a MAP: the walk must not
		// descend past it. Neither child key is sensitive by name, so descending
		// would emit both in the clear.
		opts := SealOpts{Sealed: map[string]bool{"peers[0]": true}}
		in := map[string]any{
			"peers": []any{
				map[string]any{"user": "alice", "auth": "s3cr3t"},
				map[string]any{"user": "bob", "auth": "public"},
			},
		}
		out := MaskSecretsSealed(in, opts)

		peers := out["peers"].([]any)
		if peers[0] != maskedValue {
			t.Errorf("sealed peers[0] = %#v, want masked whole (no unsealed interior)", peers[0])
		}
		if m, ok := peers[1].(map[string]any); !ok || m["auth"] != "public" {
			t.Errorf("peers[1] = %#v, want passthrough (over-masking)", peers[1])
		}
	})

	t.Run("typed []string element", func(t *testing.T) {
		opts := SealOpts{Sealed: map[string]bool{"args[1]": true}}
		in := map[string]any{"args": []string{"--user=root", "--password=hunter2"}}
		out := MaskSecretsSealed(in, opts)

		args := out["args"].([]any)
		if args[1] != maskedValue {
			t.Errorf("sealed args[1] in []string = %v, want masked", args[1])
		}
		if args[0] != "--user=root" {
			t.Errorf("args[0] = %v, want passthrough (over-masking)", args[0])
		}
	})

	t.Run("generalized idx form", func(t *testing.T) {
		// The seal/schema set may hold the generalized `args[]` form; normalizeIdx
		// must be applied to a slice element's path too, not only to a map key's.
		opts := SealOpts{Sealed: map[string]bool{"args[]": true}}
		in := map[string]any{"args": []any{"a", "b"}}
		out := MaskSecretsSealed(in, opts)

		args := out["args"].([]any)
		for i, el := range args {
			if el != maskedValue {
				t.Errorf("args[%d] = %v, want masked via the generalized form", i, el)
			}
		}
	})
}

// NIM-810 guard, schema layer. TestNormalizeIdx asserts the string rewrite;
// this asserts that an indexed path in the SCHEMA actually masks the cell —
// the schema layer reaches a slice element through the same walk as the seal.
func TestMaskSecretsWithSchema_SecretSliceElement(t *testing.T) {
	in := map[string]any{"acl": []any{"alice:s3cr3t", "bob:public"}}
	out := MaskSecretsWithSchema(in, SecretPathSet{"acl[0]": true})

	acl := out["acl"].([]any)
	if acl[0] != maskedValue {
		t.Errorf("schema-secret acl[0] = %v, want masked", acl[0])
	}
	if acl[1] != "bob:public" {
		t.Errorf("acl[1] = %v, want passthrough (over-masking)", acl[1])
	}
}

func TestNormalizeIdx(t *testing.T) {
	cases := map[string]string{
		"acl[0].password": "acl[].password",
		"a[10].b[2].c":    "a[].b[].c",
		"plain.path":      "plain.path",
		"no_idx":          "no_idx",
	}
	for in, want := range cases {
		if got := normalizeIdx(in); got != want {
			t.Errorf("normalizeIdx(%q) = %q, want %q", in, got, want)
		}
	}
}
