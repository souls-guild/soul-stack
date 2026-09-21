package audit

import (
	"reflect"
	"strings"
	"testing"
)

func TestMaskSecrets_KnownKeys(t *testing.T) {
	in := map[string]any{
		"password":        "p4ssw0rd",
		"jwt":             "eyJhbGciOi...",
		"token":           "ghp_abc",
		"secret":          "shh",
		"private_key":     "-----BEGIN...",
		"credentials_ref": "vault:secret/x",
		"signing_key":     "abc",
		"signing_key_ref": "vault:secret/y",
		"path":            "/etc/keeper.yml",
		"count":           42,
	}
	out := MaskSecrets(in)

	for _, k := range []string{"password", "jwt", "token", "secret", "private_key", "credentials_ref", "signing_key", "signing_key_ref"} {
		if out[k] != maskedValue {
			t.Errorf("%s = %v, want %q", k, out[k], maskedValue)
		}
	}
	if out["path"] != "/etc/keeper.yml" {
		t.Errorf("path = %v, want passthrough", out["path"])
	}
	if out["count"] != 42 {
		t.Errorf("count = %v, want passthrough", out["count"])
	}
}

func TestMaskSecrets_KeyCaseInsensitive(t *testing.T) {
	in := map[string]any{
		"Password":    "p",
		"JWT":         "j",
		"Secret":      "s",
		"PRIVATE_KEY": "k",
	}
	out := MaskSecrets(in)
	for k, v := range out {
		if v != maskedValue {
			t.Errorf("%s = %v, want %q (case-insensitive match)", k, v, maskedValue)
		}
	}
}

func TestMaskSecrets_VaultRefValue(t *testing.T) {
	in := map[string]any{
		"dsn_ref":    "vault:secret/keeper/postgres",
		"plain_dsn":  "postgres://keeper:keeper@localhost:5432/keeper",
		"some_other": "vault:secret/keeper/redis",
	}
	out := MaskSecrets(in)
	if out["dsn_ref"] != maskedValue {
		t.Errorf("dsn_ref = %v, want masked (vault: prefix)", out["dsn_ref"])
	}
	if out["some_other"] != maskedValue {
		t.Errorf("some_other = %v, want masked (vault: prefix)", out["some_other"])
	}
	if out["plain_dsn"] != "postgres://keeper:keeper@localhost:5432/keeper" {
		t.Errorf("plain_dsn = %v, want passthrough", out["plain_dsn"])
	}
}

func TestMaskSecrets_VaultRefSubstring(t *testing.T) {
	// vault-ref glued inside a string (typical for status_details.error /
	// error_summary: a gRPC/render error echoed a vault path, not as a prefix). The
	// prefix filter used to let this through → plaintext leak into an observable channel.
	in := map[string]any{
		"error":     "scenario: render task \"db\": resolve vault:secret/keeper/db failed",
		"reason":    "render_failed",
		"plain_msg": "incarnation has no connected hosts",
	}
	out := MaskSecrets(in)
	if out["error"] != maskedValue {
		t.Errorf("error = %v, want masked (contains vault: marker)", out["error"])
	}
	if out["reason"] != "render_failed" {
		t.Errorf("reason = %v, want passthrough", out["reason"])
	}
	if out["plain_msg"] != "incarnation has no connected hosts" {
		t.Errorf("plain_msg = %v, want passthrough (no vault: marker)", out["plain_msg"])
	}
}

// Marker narrowed to vault:secret/ (review-minor of the vault() pilot): a real
// vault-ref to the default KV mount is masked; legitimate strings with the
// substring "vault:" but no secret (endpoint / docker tag / diagnostic) are NOT masked.
func TestMaskSecrets_VaultRefMarkerNarrowed(t *testing.T) {
	in := map[string]any{
		"real_ref":      "vault:secret/keeper/db",
		"real_in_error": "render: resolve vault:secret/redis/admin failed",
		"endpoint":      "https://vault:8200",
		"image":         "hashicorp/vault:1.18",
		"kv_diag":       "vault: KV error",
	}
	out := MaskSecrets(in)

	if out["real_ref"] != maskedValue {
		t.Errorf("real_ref = %v, want masked (vault:secret/ marker)", out["real_ref"])
	}
	if out["real_in_error"] != maskedValue {
		t.Errorf("real_in_error = %v, want masked (vault:secret/ concatenated into a string)", out["real_in_error"])
	}
	if out["endpoint"] != "https://vault:8200" {
		t.Errorf("endpoint = %v, want passthrough (vault: but not vault:secret/)", out["endpoint"])
	}
	if out["image"] != "hashicorp/vault:1.18" {
		t.Errorf("image = %v, want passthrough (docker tag)", out["image"])
	}
	if out["kv_diag"] != "vault: KV error" {
		t.Errorf("kv_diag = %v, want passthrough (diagnostics, not a ref)", out["kv_diag"])
	}
}

// K5 (security audit): custom KV mount. An operator may configure a mount other
// than the default `secret` (config.Vault.KVMount). The `vault:secret/` marker did
// NOT catch such refs → plaintext leak of the vault path into audit/OTel/SSE/error.
// After the K5 fix, masking uses the form `vault:<mount>/` (any mount).
func TestMaskSecrets_VaultRefCustomMount(t *testing.T) {
	in := map[string]any{
		"kv_ref":       "vault:kv/keeper/db",
		"dbcreds_ref":  "vault:db-creds/role/app",
		"kv_in_error":  "render: resolve vault:kv-v2/redis/admin failed",
		"dotted_mount": "vault:secret.v2/x",
		// passthrough invariants must hold even with the regexp marker.
		"endpoint": "https://vault:8200",
		"image":    "hashicorp/vault:1.18",
		"kv_diag":  "vault: KV error",
	}
	out := MaskSecrets(in)

	for _, k := range []string{"kv_ref", "dbcreds_ref", "kv_in_error", "dotted_mount"} {
		if out[k] != maskedValue {
			t.Errorf("%s = %v, want masked (custom mount vault-ref)", k, out[k])
		}
	}
	if out["endpoint"] != "https://vault:8200" {
		t.Errorf("endpoint = %v, want passthrough", out["endpoint"])
	}
	if out["image"] != "hashicorp/vault:1.18" {
		t.Errorf("image = %v, want passthrough", out["image"])
	}
	if out["kv_diag"] != "vault: KV error" {
		t.Errorf("kv_diag = %v, want passthrough", out["kv_diag"])
	}
}

func TestMaskSecrets_NestedMap(t *testing.T) {
	in := map[string]any{
		"auth": map[string]any{
			"jwt":      "eyJ...",
			"password": "p",
			"issuer":   "soul-stack",
		},
		"transport": map[string]any{
			"endpoint": "https://example",
		},
	}
	out := MaskSecrets(in)

	auth, ok := out["auth"].(map[string]any)
	if !ok {
		t.Fatalf("auth: not a map, got %T", out["auth"])
	}
	if auth["jwt"] != maskedValue || auth["password"] != maskedValue {
		t.Errorf("nested auth.jwt/password not masked: %v", auth)
	}
	if auth["issuer"] != "soul-stack" {
		t.Errorf("nested auth.issuer changed: %v", auth["issuer"])
	}
	tr, ok := out["transport"].(map[string]any)
	if !ok {
		t.Fatalf("transport: not a map, got %T", out["transport"])
	}
	if tr["endpoint"] != "https://example" {
		t.Errorf("transport.endpoint changed: %v", tr["endpoint"])
	}
}

func TestMaskSecrets_SliceWalk(t *testing.T) {
	in := map[string]any{
		"roles": []any{
			map[string]any{"name": "admin", "token": "t1"},
			map[string]any{"name": "ops", "secret": "s1"},
			"vault:secret/ref-in-slice",
			"plain",
		},
	}
	out := MaskSecrets(in)
	roles, ok := out["roles"].([]any)
	if !ok || len(roles) != 4 {
		t.Fatalf("roles shape lost: %#v", out["roles"])
	}
	r0 := roles[0].(map[string]any)
	if r0["token"] != maskedValue {
		t.Errorf("roles[0].token not masked: %v", r0)
	}
	r1 := roles[1].(map[string]any)
	if r1["secret"] != maskedValue {
		t.Errorf("roles[1].secret not masked: %v", r1)
	}
	if roles[2] != maskedValue {
		t.Errorf("roles[2] vault-ref not masked: %v", roles[2])
	}
	if roles[3] != "plain" {
		t.Errorf("roles[3] passthrough lost: %v", roles[3])
	}
}

func TestMaskSecrets_SubstringKeys(t *testing.T) {
	// Security-review H1: exact-match missed composite keys. Now a substring regex
	// masks any key containing a secret fragment.
	in := map[string]any{
		"bootstrap_token":       "secret123",
		"aws_secret_access_key": "AKIA...",
		"db_password":           "pg-pass",
		"tls_private_key":       "-----BEGIN",
		"jwt_signing_key":       "hmac-bytes",
		"refresh_token":         "rt-abc",
		"api_key":               "ak-1",
		"credentials_ref":       "vault:secret/x",
		// Non-secret keys that overlap by letters but not by fragment pass without
		// masking.
		"description": "human text",
		"keyboard":    "qwerty-layout",
		"count":       42,
	}
	out := MaskSecrets(in)

	masked := []string{
		"bootstrap_token", "aws_secret_access_key", "db_password",
		"tls_private_key", "jwt_signing_key", "refresh_token",
		"api_key", "credentials_ref",
	}
	for _, k := range masked {
		if out[k] != maskedValue {
			t.Errorf("%s = %v, want %q (substring-match)", k, out[k], maskedValue)
		}
	}
	if out["description"] != "human text" {
		t.Errorf("description = %v, want passthrough", out["description"])
	}
	if out["keyboard"] != "qwerty-layout" {
		t.Errorf("keyboard = %v, want passthrough (no secret fragment)", out["keyboard"])
	}
	if out["count"] != 42 {
		t.Errorf("count = %v, want passthrough", out["count"])
	}
}

func TestMaskSecrets_TLSPEMKeys(t *testing.T) {
	// Redis-consolidation TLS (redis.instance): connection PEM material arrives in
	// params under keys tls_key / tls_cert / tls_ca. The bare fragment key/cert/ca
	// is not in the catalog, so tls[_-]?(key|cert|ca) was added — otherwise a whole
	// private key would leak plaintext into logs/OTel/RunResult (the masking model
	// keys on the NAME). This is a BLOCKER masking-guard of the merge-masking class.
	const pemKey = "-----BEGIN PRIVATE KEY-----\nMIIE...\n-----END PRIVATE KEY-----"
	const pemCert = "-----BEGIN CERTIFICATE-----\nMIID...\n-----END CERTIFICATE-----"
	in := map[string]any{
		"tls_key":     pemKey,
		"tls_cert":    pemCert,
		"tls_ca":      pemCert,
		"tls-key":     pemKey,  // dashed form
		"tls_ca_data": pemCert, // *-data form
		// Non-secret TLS connection params are NOT masked (booleans/numbers/names).
		"tls":             true,
		"tls_enable":      true,
		"tls_port":        7379,
		"tls_skip_verify": false,
	}
	out := MaskSecrets(in)

	for _, k := range []string{"tls_key", "tls_cert", "tls_ca", "tls-key", "tls_ca_data"} {
		if out[k] != maskedValue {
			t.Errorf("%s = %v, want %q (PEM material must be masked)", k, out[k], maskedValue)
		}
	}
	if out["tls"] != true {
		t.Errorf("tls = %v, want passthrough (flag, not a secret)", out["tls"])
	}
	if out["tls_enable"] != true {
		t.Errorf("tls_enable = %v, want passthrough (flag)", out["tls_enable"])
	}
	if out["tls_port"] != 7379 {
		t.Errorf("tls_port = %v, want passthrough (number)", out["tls_port"])
	}
	if out["tls_skip_verify"] != false {
		t.Errorf("tls_skip_verify = %v, want passthrough (boolean verify flag)", out["tls_skip_verify"])
	}
}

// TestMaskSecrets_RedisRenderContextTLSVars pins the redis-consolidation security
// fix (medium-secrets): the PEM render-vars of destiny `redis` were renamed
// cert/key/ca → tls_cert/tls_key/tls_ca so the masking catalog catches them BY
// NAME. The test models the core.file.rendered payload shape (render_context.vars
// with PEM) and proves the contrast:
//   - bare cert/key/ca (as BEFORE the fix) are NOT masked → leak into logs/OTel/
//     RunResult (demonstrates the closed hole);
//   - tls_cert/tls_key/tls_ca (as AFTER the fix) are masked.
func TestMaskSecrets_RedisRenderContextTLSVars(t *testing.T) {
	const pemKey = "-----BEGIN PRIVATE KEY-----\nSERVERKEY\n-----END PRIVATE KEY-----"
	const pemCert = "-----BEGIN CERTIFICATE-----\nSERVERCERT\n-----END CERTIFICATE-----"
	const pemCA = "-----BEGIN CERTIFICATE-----\nCACERT\n-----END CERTIFICATE-----"

	// payload shaped like core.file.rendered: params.render_context.vars.{tls_*}.
	payload := map[string]any{
		"path": "/etc/redis/tls/redis.key",
		"render_context": map[string]any{
			"vars": map[string]any{
				"tls_cert": pemCert,
				"tls_key":  pemKey,
				"tls_ca":   pemCA,
			},
		},
	}
	out := MaskSecrets(payload)
	vars := out["render_context"].(map[string]any)["vars"].(map[string]any)
	for _, k := range []string{"tls_cert", "tls_key", "tls_ca"} {
		if vars[k] != maskedValue {
			t.Errorf("render_context.vars.%s = %v, want %q (PEM must be masked by name)", k, vars[k], maskedValue)
		}
	}
	if out["path"] != "/etc/redis/tls/redis.key" {
		t.Errorf("path = %v, want passthrough", out["path"])
	}

	// Contrast: the FORMER bare names cert/key/ca do NOT match the filter (the hole
	// the rename closed). If this assert ever fails — the catalog widened and the
	// var names can revert to bare; until then it guards the reason the names must
	// carry the tls_ prefix.
	bareOut := MaskSecrets(map[string]any{"cert": pemCert, "key": pemKey, "ca": pemCA})
	for _, k := range []string{"cert", "key", "ca"} {
		if bareOut[k] == maskedValue {
			t.Errorf("bare %q got masked - catalog expanded, check the tls_ prefix rationale", k)
		}
	}
}

// TestMaskSecrets_MigrateClusterSecrets — security blocker of the migrate_cluster
// S1 pilot (redis.instance): the migration task receives secret fields of the
// source/master (master_*/source_*) that land in the connection task's params and
// through RunResult/audit-payload would leak plaintext into logs/OTel/UI. The names
// carry password / tls_(key|cert|ca) fragments, so they are caught by the substring
// catalog WITHOUT extending it — the test proves coverage as a behavioral invariant
// (regression on leak), not by introducing new keys. master_username is NOT a secret
// (no user fragment in the catalog) → passthrough.
func TestMaskSecrets_MigrateClusterSecrets(t *testing.T) {
	const pemKey = "-----BEGIN PRIVATE KEY-----\nMIGRSRC\n-----END PRIVATE KEY-----"
	const pemCert = "-----BEGIN CERTIFICATE-----\nMIGRSRC\n-----END CERTIFICATE-----"
	in := map[string]any{
		"master_password": "m-p4ss",
		"source_password": "s-p4ss",
		"master_tls_key":  pemKey,
		"master_tls_cert": pemCert,
		"master_tls_ca":   pemCert,
		"source_tls_key":  pemKey,
		"source_tls_cert": pemCert,
		"source_tls_ca":   pemCert,
		// Non-secret migration connection fields — passthrough.
		"master_username": "admin",
		"master_host":     "10.0.0.1",
		"master_port":     6379,
	}
	out := MaskSecrets(in)

	for _, k := range []string{
		"master_password", "source_password",
		"master_tls_key", "master_tls_cert", "master_tls_ca",
		"source_tls_key", "source_tls_cert", "source_tls_ca",
	} {
		if out[k] != maskedValue {
			t.Errorf("%s = %v, want %q (migration secret must be masked)", k, out[k], maskedValue)
		}
	}
	if out["master_username"] != "admin" {
		t.Errorf("master_username = %v, want passthrough (not a secret)", out["master_username"])
	}
	if out["master_host"] != "10.0.0.1" {
		t.Errorf("master_host = %v, want passthrough", out["master_host"])
	}
	if out["master_port"] != 6379 {
		t.Errorf("master_port = %v, want passthrough", out["master_port"])
	}
}

// TestMaskSecrets_MigrateClusterRunResultShape — the same secret fields, but in the
// RunResult/audit-payload shape of the migration task: TaskEvent.params nested under
// the task name. Proves the recursive walk masks master_password/source_password/
// source_tls_ca at the nested level (not only in a top-level map), i.e. the invariant
// holds in the real observable channel.
func TestMaskSecrets_MigrateClusterRunResultShape(t *testing.T) {
	const pemCA = "-----BEGIN CERTIFICATE-----\nCACERT\n-----END CERTIFICATE-----"
	payload := map[string]any{
		"task":   "migrate from source cluster",
		"module": "redis.cluster.external-joined",
		"params": map[string]any{
			"master_username": "admin",
			"master_password": "m-p4ss",
			"source_password": "s-p4ss",
			"source_tls_ca":   pemCA,
		},
	}
	out := MaskSecrets(payload)
	params, ok := out["params"].(map[string]any)
	if !ok {
		t.Fatalf("params: not map[string]any, got %T", out["params"])
	}
	for _, k := range []string{"master_password", "source_password", "source_tls_ca"} {
		if params[k] != maskedValue {
			t.Errorf("params.%s = %v, want %q (secret must be masked in RunResult payload)", k, params[k], maskedValue)
		}
	}
	if params["master_username"] != "admin" {
		t.Errorf("params.master_username = %v, want passthrough", params["master_username"])
	}
}

func TestMaskSecrets_BootstrapTokenExact(t *testing.T) {
	// Concrete case from the verification plan: {"bootstrap_token":"secret123"}
	// → {"bootstrap_token":"***MASKED***"}.
	out := MaskSecrets(map[string]any{"bootstrap_token": "secret123"})
	if out["bootstrap_token"] != maskedValue {
		t.Fatalf("bootstrap_token = %v, want %q", out["bootstrap_token"], maskedValue)
	}
}

func TestMaskSecrets_TypedStringMap(t *testing.T) {
	// map[string]string inside a payload — the walker did not previously descend into it.
	in := map[string]any{
		"attributes": map[string]string{
			"region":          "eu-west-1",
			"bootstrap_token": "tok-xyz",
			"vault_ref":       "vault:secret/db",
		},
	}
	out := MaskSecrets(in)
	attrs, ok := out["attributes"].(map[string]any)
	if !ok {
		t.Fatalf("attributes: not map[string]any, got %T", out["attributes"])
	}
	if attrs["region"] != "eu-west-1" {
		t.Errorf("attributes.region = %v, want passthrough", attrs["region"])
	}
	if attrs["bootstrap_token"] != maskedValue {
		t.Errorf("attributes.bootstrap_token = %v, want masked", attrs["bootstrap_token"])
	}
	if attrs["vault_ref"] != maskedValue {
		t.Errorf("attributes.vault_ref = %v, want masked (vault: prefix)", attrs["vault_ref"])
	}
}

func TestMaskSecrets_TypedStringSlice(t *testing.T) {
	in := map[string]any{
		"refs": []string{"plain", "vault:secret/a", "vault:secret/b"},
	}
	out := MaskSecrets(in)
	refs, ok := out["refs"].([]any)
	if !ok || len(refs) != 3 {
		t.Fatalf("refs shape lost: %#v", out["refs"])
	}
	if refs[0] != "plain" {
		t.Errorf("refs[0] = %v, want passthrough", refs[0])
	}
	if refs[1] != maskedValue || refs[2] != maskedValue {
		t.Errorf("refs vault-refs not masked: %#v", refs)
	}
}

func TestMaskSecrets_Struct(t *testing.T) {
	type creds struct {
		User           string `json:"user"`
		BootstrapToken string `json:"bootstrap_token"`
		Internal       string // exported, no tag → matched by field name `Internal`? no
	}
	in := map[string]any{
		"creds": creds{User: "alice", BootstrapToken: "tok", Internal: "x"},
	}
	out := MaskSecrets(in)
	c, ok := out["creds"].(map[string]any)
	if !ok {
		t.Fatalf("creds: not map[string]any, got %T", out["creds"])
	}
	if c["user"] != "alice" {
		t.Errorf("creds.user = %v, want passthrough", c["user"])
	}
	if c["bootstrap_token"] != maskedValue {
		t.Errorf("creds.bootstrap_token = %v, want masked (json-tag substring)", c["bootstrap_token"])
	}
	if c["Internal"] != "x" {
		t.Errorf("creds.Internal = %v, want passthrough (no secret fragment)", c["Internal"])
	}
}

func TestMaskSecrets_NestedTypedInAny(t *testing.T) {
	// map[string]string nested in []any — the reflect fallback must fire at the
	// slice-element level.
	in := map[string]any{
		"hosts": []any{
			map[string]string{"sid": "h1", "bootstrap_token": "t1"},
		},
	}
	out := MaskSecrets(in)
	hosts := out["hosts"].([]any)
	h0, ok := hosts[0].(map[string]any)
	if !ok {
		t.Fatalf("hosts[0]: not map[string]any, got %T", hosts[0])
	}
	if h0["sid"] != "h1" {
		t.Errorf("hosts[0].sid = %v, want passthrough", h0["sid"])
	}
	if h0["bootstrap_token"] != maskedValue {
		t.Errorf("hosts[0].bootstrap_token = %v, want masked", h0["bootstrap_token"])
	}
}

func TestMaskSecrets_NilAndEmpty(t *testing.T) {
	if got := MaskSecrets(nil); got != nil {
		t.Errorf("MaskSecrets(nil) = %v, want nil", got)
	}
	empty := map[string]any{}
	if got := MaskSecrets(empty); !reflect.DeepEqual(got, map[string]any{}) {
		t.Errorf("MaskSecrets(empty) = %v, want empty map", got)
	}
}

func TestMaskSecrets_DoesNotMutateInput(t *testing.T) {
	in := map[string]any{
		"password": "p",
		"nested":   map[string]any{"jwt": "j"},
	}
	_ = MaskSecrets(in)
	if in["password"] != "p" {
		t.Errorf("input top-level mutated: %v", in)
	}
	nested := in["nested"].(map[string]any)
	if nested["jwt"] != "j" {
		t.Errorf("input nested mutated: %v", nested)
	}
}

// --- free-text masking (NIM-145) ---------------------------------------------

// MaskRefsInText replaces the reference and NOTHING else. The payload masker
// blanks a whole matching value, which is right for a field that IS the secret
// and wrong for a console recording that merely contains one — blanking the
// screen would destroy the record the masking exists to make safe to keep.
func TestMaskRefsInText_ReplacesOnlyTheReference(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"bare ref", "vault:secret/db/password", MaskedValue},
		{"ref in a sentence", "reading vault:secret/db now", "reading " + MaskedValue + " now"},
		{"custom mount", "vault:kv-prod/app/key", MaskedValue},
		{"two refs", "a vault:secret/x b vault:kv/y c", "a " + MaskedValue + " b " + MaskedValue + " c"},
		{"no ref", "vault: KV error", "vault: KV error"},
		{"host:port is not a ref", "https://vault:8200", "https://vault:8200"},
		{"image tag is not a ref", "hashicorp/vault:1.18", "hashicorp/vault:1.18"},
		{"empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := MaskRefsInText(tc.in); got != tc.want {
				t.Fatalf("MaskRefsInText(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// SafeMaskSplit is what keeps a reference masked when it arrives in pieces — a
// pty echoes keystrokes one byte at a time, so per-chunk masking alone would
// mask nothing exactly where an operator types a credential path.
func TestSafeMaskSplit_HoldsBackAPossibleReference(t *testing.T) {
	cases := []struct {
		name string
		buf  string
		want int
	}{
		{"nothing to hold", "plain output\n", len("plain output\n")},
		{"one letter of the marker", "v", 0},
		{"partial marker", "echo va", len("echo ")},
		{"whole marker, no mount yet", "cat vault:", len("cat ")},
		{"mount in progress", "cat vault:secret", len("cat ")},
		{"path in progress", "cat vault:secret/db", len("cat ")},
		{"reference terminated by a space", "cat vault:secret/db ", len("cat vault:secret/db ")},
		{"reference terminated by a newline", "cat vault:secret/db\n", len("cat vault:secret/db\n")},
		{"dead marker releases everything", "vault::x", len("vault::x")},
		{"empty", "", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := SafeMaskSplit([]byte(tc.buf)); got != tc.want {
				t.Fatalf("SafeMaskSplit(%q) = %d, want %d", tc.buf, got, tc.want)
			}
		})
	}
}

// The carry is bounded: a stream that happens to contain the literal `vault:`
// followed by megabytes of reference-shaped bytes must not be held in memory
// forever waiting for a `/` that never comes.
func TestSafeMaskSplit_ReleasesPastTheCarryBound(t *testing.T) {
	buf := []byte("vault:" + strings.Repeat("a", MaxMaskCarryBytes+1))
	if got := SafeMaskSplit(buf); got != len(buf) {
		t.Fatalf("SafeMaskSplit held back %d bytes past the bound, want a full release of %d",
			len(buf)-got, len(buf))
	}
}

// Feeding a reference one byte at a time through split+mask must produce the
// same masked stream as feeding it whole — that equivalence IS the guarantee
// the console recorder relies on.
func TestSafeMaskSplit_ByteAtATimeMatchesWholeInput(t *testing.T) {
	const in = "cat vault:secret/db/password && echo vault:kv/x done\n"

	var carry, out []byte
	for i := 0; i < len(in); i++ {
		carry = append(carry, in[i])
		split := SafeMaskSplit(carry)
		out = append(out, MaskRefsInBytes(append([]byte(nil), carry[:split]...))...)
		carry = carry[split:]
	}
	out = append(out, MaskRefsInBytes(carry)...)

	if want := MaskRefsInText(in); string(out) != want {
		t.Fatalf("streamed masking = %q, want %q", string(out), want)
	}
}
