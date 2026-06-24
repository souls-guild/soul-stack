package audit

import (
	"testing"
)

// (e) schema-объявленное secret-поле → MASKED на read-path (MaskSecretsWithSchema).
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
		t.Errorf("несекретный hostname = %v, want passthrough", out["hostname"])
	}
}

// (f) обычное generic-поле (несекретный content с конфигом) → НЕ MASKED.
// КРИТИЧНО: нет over-masking. Имя ключа `content`/`config` не sensitive-by-regex,
// схема его не объявляет, vault-ref нет → значение проходит насквозь.
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
		t.Errorf("generic content замаскирован: %v (over-masking!)", out["content"])
	}
	cfg, ok := out["config"].(map[string]any)
	if !ok {
		t.Fatalf("config не map: %T", out["config"])
	}
	if cfg["loglevel"] != "notice" || cfg["port"] != 6379 {
		t.Errorf("generic config поля замаскированы: %v (over-masking!)", cfg)
	}
}

// schema по вложенному пути + обобщённый индекс массива.
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
	// Обобщённая idx-форма `acl[].password` ловит оба элемента; `tls.key` — точный.
	schema := SecretPathSet{"acl[].password": true, "tls.key": true}
	out := MaskSecretsWithSchema(in, schema)

	acl := out["acl"].([]any)
	for i, el := range acl {
		m := el.(map[string]any)
		if m["password"] != maskedValue {
			t.Errorf("acl[%d].password = %v, want masked", i, m["password"])
		}
		if m["name"] == maskedValue {
			t.Errorf("acl[%d].name замаскирован — over-masking", i)
		}
	}
	tls := out["tls"].(map[string]any)
	if tls["key"] != maskedValue {
		t.Errorf("tls.key = %v, want masked", tls["key"])
	}
	if tls["port"] != 6379 {
		t.Errorf("tls.port замаскирован — over-masking")
	}
}

// schema=nil → деградирует к MaskSecrets (vault+regex), schema-слой выключен.
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
		t.Errorf("несекретные поля замаскированы: %v", out)
	}
}

// (g) regex-fallback аларм инкрементится, когда regex ловит класс-(ii) секрет
// (bootstrap_token без схемы), НЕ пойманный декларативом.
func TestMaskSecretsSealed_RegexFallbackAlarm(t *testing.T) {
	var fired []string
	opts := SealOpts{
		RegexFallback: func(path string) { fired = append(fired, path) },
	}
	in := map[string]any{
		"bootstrap_token": "tok-abc", // класс ii: regex ловит, схемы нет
		"plain":           "ok",
	}
	out := MaskSecretsSealed(in, opts)

	if out["bootstrap_token"] != maskedValue {
		t.Errorf("bootstrap_token = %v, want masked", out["bootstrap_token"])
	}
	if len(fired) != 1 || fired[0] != "bootstrap_token" {
		t.Fatalf("regex-fallback аларм: got %v, want [bootstrap_token]", fired)
	}
}

// Аларм НЕ срабатывает, когда secret поймал декларатив (schema) — regex не
// «единственный сработавший слой».
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
		t.Fatalf("аларм сработал при schema-catch: %v (regex не единственный)", fired)
	}
}

// Аларм НЕ срабатывает на vault-ref под sensitive-ключом — vault-слой поймал бы
// сам, regex не единственный.
func TestMaskSecretsSealed_NoAlarmOnVaultRefValue(t *testing.T) {
	var fired []string
	opts := SealOpts{RegexFallback: func(path string) { fired = append(fired, path) }}
	in := map[string]any{"db_password": "vault:secret/db#password"}
	out := MaskSecretsSealed(in, opts)
	if out["db_password"] != maskedValue {
		t.Errorf("db_password = %v, want masked", out["db_password"])
	}
	if len(fired) != 0 {
		t.Fatalf("аларм на vault-ref-значении: %v (vault-слой поймал бы сам)", fired)
	}
}

// (a)/(b)/(d) seal-слой: sealed-путь generic-поля → MASKED; vault-ref → MASKED;
// смешанное значение по sealed-пути → MASKED.
func TestMaskSecretsSealed_SealedPaths(t *testing.T) {
	opts := SealOpts{
		Sealed: map[string]bool{
			"content":      true, // (a) generic-поле, помечено sealed на render-фазе
			"requirepass":  true, // (d) смешанное literal+secret-значение
			"config.token": true,
		},
	}
	in := map[string]any{
		"content":     "tls private material", // generic ключ, но sealed-путь
		"requirepass": "requirepass s3cr3t",   // склейка literal+secret
		"config":      map[string]any{"token": "x", "port": 6379},
		"public":      "not sealed",
	}
	out := MaskSecretsSealed(in, opts)

	if out["content"] != maskedValue {
		t.Errorf("sealed content = %v, want masked (a)", out["content"])
	}
	if out["requirepass"] != maskedValue {
		t.Errorf("sealed смешанное = %v, want masked (d)", out["requirepass"])
	}
	cfg := out["config"].(map[string]any)
	if cfg["token"] != maskedValue {
		t.Errorf("sealed config.token = %v, want masked", cfg["token"])
	}
	if cfg["port"] != 6379 {
		t.Errorf("config.port замаскирован — over-masking")
	}
	if out["public"] != "not sealed" {
		t.Errorf("не-sealed public замаскирован — over-masking")
	}
}

// (b) vault-ref-значение в произвольном generic-ключе → MASKED (vault-слой 2).
func TestMaskSecretsSealed_VaultRefAnyKey(t *testing.T) {
	in := map[string]any{"connection": "host=db pass=vault:kv/app/db#pw"}
	out := MaskSecretsSealed(in, SealOpts{})
	if out["connection"] != maskedValue {
		t.Errorf("vault-ref-значение = %v, want masked", out["connection"])
	}
}

func TestMaskSecretsSealed_NilInput(t *testing.T) {
	if MaskSecretsSealed(nil, SealOpts{}) != nil {
		t.Fatal("nil-вход → nil-выход")
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
