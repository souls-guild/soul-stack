package audit

import (
	"reflect"
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
	// vault-ref склеен внутри строки (типичный кейс для status_details.error /
	// error_summary: gRPC/render-ошибка эхнула vault-путь не префиксом). Раньше
	// prefix-фильтр это пропускал → plaintext leak в наблюдаемый канал.
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

// Сужение маркера до vault:secret/ (review-минор vault()-pilot): реальный
// vault-ref на дефолтный KV-mount маскируется; легитимные строки с подстрокой
// "vault:" но без секрета (endpoint / docker-тег / диагностика) — НЕ маскируются.
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
		t.Errorf("real_in_error = %v, want masked (vault:secret/ склеен в строку)", out["real_in_error"])
	}
	if out["endpoint"] != "https://vault:8200" {
		t.Errorf("endpoint = %v, want passthrough (vault: но не vault:secret/)", out["endpoint"])
	}
	if out["image"] != "hashicorp/vault:1.18" {
		t.Errorf("image = %v, want passthrough (docker-тег)", out["image"])
	}
	if out["kv_diag"] != "vault: KV error" {
		t.Errorf("kv_diag = %v, want passthrough (диагностика, не ref)", out["kv_diag"])
	}
}

// K5 (security-аудит): кастомный KV-mount. Оператор вправе настроить mount,
// отличный от дефолтного `secret` (config.Vault.KVMount). Маркер `vault:secret/`
// такие ref-ы НЕ ловил → plaintext-leak vault-пути в audit/OTel/SSE/error.
// После K5-фикса маскинг идёт по форме `vault:<mount>/` (любой mount).
func TestMaskSecrets_VaultRefCustomMount(t *testing.T) {
	in := map[string]any{
		"kv_ref":       "vault:kv/keeper/db",
		"dbcreds_ref":  "vault:db-creds/role/app",
		"kv_in_error":  "render: resolve vault:kv-v2/redis/admin failed",
		"dotted_mount": "vault:secret.v2/x",
		// passthrough-инварианты должны сохраниться и при регэксп-маркере.
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
	// Security-review H1: exact-match пропускал составные ключи. Теперь
	// substring-regex маскирует любой ключ, содержащий секрет-фрагмент.
	in := map[string]any{
		"bootstrap_token":       "secret123",
		"aws_secret_access_key": "AKIA...",
		"db_password":           "pg-pass",
		"tls_private_key":       "-----BEGIN",
		"jwt_signing_key":       "hmac-bytes",
		"refresh_token":         "rt-abc",
		"api_key":               "ak-1",
		"credentials_ref":       "vault:secret/x",
		// Несекретные ключи, частично пересекающиеся по буквам, но не по
		// фрагменту — проходят без маскировки.
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
	// redis-консолидация TLS (community.redis): PEM-материал коннекта приходит в
	// params под ключами tls_key / tls_cert / tls_ca. Голый фрагмент key/cert/ca
	// в каталог не входит, поэтому добавлен tls[_-]?(key|cert|ca) — иначе целый
	// приватный ключ утекал бы plaintext в логи/OTel/RunResult (модель маскинга —
	// ИМЯ ключа). Это BLOCKER masking-guard класса merge-masking.
	const pemKey = "-----BEGIN PRIVATE KEY-----\nMIIE...\n-----END PRIVATE KEY-----"
	const pemCert = "-----BEGIN CERTIFICATE-----\nMIID...\n-----END CERTIFICATE-----"
	in := map[string]any{
		"tls_key":     pemKey,
		"tls_cert":    pemCert,
		"tls_ca":      pemCert,
		"tls-key":     pemKey,  // дефисная форма
		"tls_ca_data": pemCert, // *-data форма
		// Несекретные TLS-параметры коннекта — НЕ маскируются (булевы/числа/имена).
		"tls":             true,
		"tls_enable":      true,
		"tls_port":        7379,
		"tls_skip_verify": false,
	}
	out := MaskSecrets(in)

	for _, k := range []string{"tls_key", "tls_cert", "tls_ca", "tls-key", "tls_ca_data"} {
		if out[k] != maskedValue {
			t.Errorf("%s = %v, want %q (PEM-материал обязан маскироваться)", k, out[k], maskedValue)
		}
	}
	if out["tls"] != true {
		t.Errorf("tls = %v, want passthrough (флаг, не секрет)", out["tls"])
	}
	if out["tls_enable"] != true {
		t.Errorf("tls_enable = %v, want passthrough (флаг)", out["tls_enable"])
	}
	if out["tls_port"] != 7379 {
		t.Errorf("tls_port = %v, want passthrough (число)", out["tls_port"])
	}
	if out["tls_skip_verify"] != false {
		t.Errorf("tls_skip_verify = %v, want passthrough (булев флаг verify)", out["tls_skip_verify"])
	}
}

// TestMaskSecrets_RedisRenderContextTLSVars фиксирует ИБ-фикс redis-консолидации
// (medium-secrets): PEM render-vars destiny `redis` переименованы cert/key/ca →
// tls_cert/tls_key/tls_ca, чтобы каталог маскинга ловил их ПО ИМЕНИ. Тест
// моделирует форму payload core.file.rendered (render_context.vars с PEM) и
// доказывает контраст:
//   - голые cert/key/ca (как было ДО фикса) НЕ маскируются → leak в логи/OTel/
//     RunResult (демонстрация закрытой дыры);
//   - tls_cert/tls_key/tls_ca (как стало ПОСЛЕ фикса) маскируются.
func TestMaskSecrets_RedisRenderContextTLSVars(t *testing.T) {
	const pemKey = "-----BEGIN PRIVATE KEY-----\nSERVERKEY\n-----END PRIVATE KEY-----"
	const pemCert = "-----BEGIN CERTIFICATE-----\nSERVERCERT\n-----END CERTIFICATE-----"
	const pemCA = "-----BEGIN CERTIFICATE-----\nCACERT\n-----END CERTIFICATE-----"

	// payload в форме core.file.rendered: params.render_context.vars.{tls_*}.
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
			t.Errorf("render_context.vars.%s = %v, want %q (PEM обязан маскироваться по имени)", k, vars[k], maskedValue)
		}
	}
	if out["path"] != "/etc/redis/tls/redis.key" {
		t.Errorf("path = %v, want passthrough", out["path"])
	}

	// Контраст: ПРЕЖНИЕ голые имена cert/key/ca под фильтр НЕ попадают (дыра,
	// которую закрыло переименование). Если этот ассерт когда-нибудь упадёт —
	// каталог расширился и var-имена можно вернуть к голым; пока он стережёт
	// причину, по которой имена обязаны нести tls_-префикс.
	bareOut := MaskSecrets(map[string]any{"cert": pemCert, "key": pemKey, "ca": pemCA})
	for _, k := range []string{"cert", "key", "ca"} {
		if bareOut[k] == maskedValue {
			t.Errorf("голое %q замаскировалось — каталог расширился, проверь обоснование префикса tls_", k)
		}
	}
}

func TestMaskSecrets_BootstrapTokenExact(t *testing.T) {
	// Конкретный кейс из verification-плана: {"bootstrap_token":"secret123"}
	// → {"bootstrap_token":"***MASKED***"}.
	out := MaskSecrets(map[string]any{"bootstrap_token": "secret123"})
	if out["bootstrap_token"] != maskedValue {
		t.Fatalf("bootstrap_token = %v, want %q", out["bootstrap_token"], maskedValue)
	}
}

func TestMaskSecrets_TypedStringMap(t *testing.T) {
	// map[string]string внутри payload — раньше walker не обходил его.
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
	// map[string]string вложен в []any — reflect-fallback должен сработать
	// на уровне slice-элемента.
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
