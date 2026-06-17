package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/diag"
)

func TestLoadKeeper_GoldenExample(t *testing.T) {
	path := filepath.FromSlash("../../examples/keeper/keeper.yml")
	cfg, doc, diags, err := LoadKeeper(path, ValidateOptions{})
	if err != nil {
		t.Fatalf("io error: %v", err)
	}
	if cfg == nil {
		t.Fatal("cfg == nil")
	}
	if doc == nil {
		t.Fatal("doc == nil")
	}
	if diag.HasErrors(diags) {
		for _, d := range diags {
			t.Logf("%s:%d:%d [%s] %s %s", d.File, d.Line, d.Column, d.Code, d.Message, d.YAMLPath)
		}
		t.Fatalf("expected 0 errors, got %d diagnostics", len(diags))
	}
	if cfg.KID != "keeper-eu-west-01" {
		t.Errorf("kid: got %q, want keeper-eu-west-01", cfg.KID)
	}
	if cfg.Plugins == nil || cfg.Plugins.CacheRoot != "/var/lib/soul-stack-keeper/plugins" {
		t.Errorf("plugins.cache_root: got %+v, want /var/lib/soul-stack-keeper/plugins", cfg.Plugins)
	}
}

// plugins.cache_root: пустое значение допустимо (default подставляется
// в keeper/cmd/keeper/main.go::pluginCacheRoot). Не-абсолютный путь
// schema-фаза отвергает с `path_not_absolute`.
func TestLoadKeeperFromBytes_PluginsCacheRootRelative(t *testing.T) {
	src := []byte(`kid: keeper-eu-west-01
listen:
  grpc:
    bootstrap:    { addr: "0.0.0.0:9442", tls: { cert: /c, key: /k } }
    event_stream: { addr: "0.0.0.0:8443", tls: { cert: /c, key: /k, ca: /a } }
  openapi: { addr: "0.0.0.0:8080" }
  mcp:     { addr: "0.0.0.0:8081" }
  metrics: { addr: "0.0.0.0:9090" }
postgres:
  dsn_ref: vault:secret/keeper/postgres
  pool: { min: 1, max: 5 }
redis:
  addr: "r:6379"
  password_ref: vault:secret/keeper/redis
vault:
  addr: "https://v:8200"
  auth: { method: token }
  pki_mount: pki/x
plugins:
  cache_root: relative/path
`)
	_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", src, ValidateOptions{})
	if !hasCode(diags, "path_not_absolute") {
		dump(t, diags)
		t.Fatalf("expected path_not_absolute for plugins.cache_root")
	}
}

func TestLoadSoul_GoldenExample(t *testing.T) {
	path := filepath.FromSlash("../../examples/soul/soul.yml")
	cfg, doc, diags, err := LoadSoul(path, ValidateOptions{})
	if err != nil {
		t.Fatalf("io error: %v", err)
	}
	if cfg == nil {
		t.Fatal("cfg == nil")
	}
	if doc == nil {
		t.Fatal("doc == nil")
	}
	if diag.HasErrors(diags) {
		for _, d := range diags {
			t.Logf("%s:%d:%d [%s] %s %s", d.File, d.Line, d.Column, d.Code, d.Message, d.YAMLPath)
		}
		t.Fatalf("expected 0 errors, got %d diagnostics", len(diags))
	}
	if len(cfg.Keeper.Endpoints) != 5 {
		t.Errorf("expected 5 endpoints, got %d", len(cfg.Keeper.Endpoints))
	}
}

func TestLoadKeeper_UnknownKeyReactor(t *testing.T) {
	src := `
kid: keeper-eu-west-01
listen:
  grpc:
    bootstrap:    { addr: "0.0.0.0:9442", tls: { cert: /c, key: /k } }
    event_stream: { addr: "0.0.0.0:8443", tls: { cert: /c, key: /k, ca: /a } }
  openapi: { addr: "0.0.0.0:8080" }
  mcp:     { addr: "0.0.0.0:8081" }
  metrics: { addr: "0.0.0.0:9090" }
postgres:
  dsn_ref: vault:secret/keeper/postgres
  pool: { min: 1, max: 5 }
redis:
  addr: "r:6379"
  password_ref: vault:secret/keeper/redis
vault:
  addr: "https://v:8200"
  auth: { method: token }
  pki_mount: pki/x
reactor:
  rules: []
`
	tmp := writeTemp(t, "keeper.yml", src)
	_, _, diags, _ := LoadKeeper(tmp, ValidateOptions{})
	if !hasCode(diags, "unknown_key") {
		dump(t, diags)
		t.Fatalf("expected unknown_key for reactor")
	}
}

// TestLoadKeeper_UnknownKeyServiceRegistry — реестр Service-ов и скаляры
// перенесены в Postgres (ADR-029): ключи services: / default_destiny_source: /
// default_module_source: больше не часть схемы конфига → reflect-walker поднимает
// unknown_key на каждый (как rbac: / reactor:).
func TestLoadKeeper_UnknownKeyServiceRegistry(t *testing.T) {
	base := `
kid: keeper-eu-west-01
listen:
  grpc:
    bootstrap:    { addr: "0.0.0.0:9442", tls: { cert: /c, key: /k } }
    event_stream: { addr: "0.0.0.0:8443", tls: { cert: /c, key: /k, ca: /a } }
  openapi: { addr: "0.0.0.0:8080" }
  mcp:     { addr: "0.0.0.0:8081" }
  metrics: { addr: "0.0.0.0:9090" }
postgres:
  dsn_ref: vault:secret/keeper/postgres
  pool: { min: 1, max: 5 }
redis:
  addr: "r:6379"
  password_ref: vault:secret/keeper/redis
vault:
  addr: "https://v:8200"
  auth: { method: token }
  pki_mount: pki/x
`
	cases := map[string]string{
		"services":               "services:\n  - { name: redis, git: \"file:///r\", ref: main }\n",
		"default_destiny_source": "default_destiny_source: \"git@x:{name}.git\"\n",
		"default_module_source":  "default_module_source: \"git@x:{name}.git\"\n",
	}
	for key, block := range cases {
		t.Run(key, func(t *testing.T) {
			tmp := writeTemp(t, "keeper.yml", base+block)
			_, _, diags, _ := LoadKeeper(tmp, ValidateOptions{})
			if !hasCode(diags, "unknown_key") {
				dump(t, diags)
				t.Fatalf("expected unknown_key for %s", key)
			}
		})
	}
}

func TestLoadKeeper_BadKID(t *testing.T) {
	src := `
kid: KEEPER_01
listen:
  grpc:
    bootstrap:    { addr: "0.0.0.0:9442", tls: { cert: /c, key: /k } }
    event_stream: { addr: "0.0.0.0:8443", tls: { cert: /c, key: /k, ca: /a } }
  openapi: { addr: "0.0.0.0:8080" }
  mcp:     { addr: "0.0.0.0:8081" }
  metrics: { addr: "0.0.0.0:9090" }
postgres:
  dsn_ref: vault:secret/keeper/postgres
  pool: { min: 1, max: 5 }
redis:
  addr: "r:6379"
  password_ref: vault:secret/keeper/redis
vault:
  addr: "https://v:8200"
  auth: { method: token }
  pki_mount: pki/x
`
	tmp := writeTemp(t, "keeper.yml", src)
	_, _, diags, _ := LoadKeeper(tmp, ValidateOptions{})
	if !hasCode(diags, "kid_invalid_format") {
		dump(t, diags)
		t.Fatalf("expected kid_invalid_format")
	}
}

// TestLoadKeeper_DaysDurationNonReaper фиксирует, что convention `duration`
// (`<N>d`) применяется ко всем duration-полям, не только к reaper.rules.*.
// Регрессия: до слияния helpers `auth.jwt.ttl_default: 30d` падал с
// `duration_invalid: unknown unit "d"` (time.ParseDuration напрямую).
func TestLoadKeeper_DaysDurationNonReaper(t *testing.T) {
	src := `
kid: keeper-eu-west-01
listen:
  grpc:
    bootstrap:    { addr: "0.0.0.0:9442", tls: { cert: /c, key: /k } }
    event_stream: { addr: "0.0.0.0:8443", tls: { cert: /c, key: /k, ca: /a } }
  openapi: { addr: "0.0.0.0:8080" }
  mcp:     { addr: "0.0.0.0:8081" }
  metrics: { addr: "0.0.0.0:9090" }
postgres:
  dsn_ref: vault:secret/keeper/postgres
  pool: { min: 1, max: 5 }
redis:
  addr: "r:6379"
  password_ref: vault:secret/keeper/redis
vault:
  addr: "https://v:8200"
  auth: { method: token }
  pki_mount: pki/x
auth:
  jwt:
    signing_key_ref: vault:secret/keeper/jwt-signing-key
    issuer: keeper-eu-west-01
    ttl_default: 30d
    ttl_bootstrap: 7d
acolyte_lease: 1d
`
	tmp := writeTemp(t, "keeper.yml", src)
	_, _, diags, err := LoadKeeper(tmp, ValidateOptions{})
	if err != nil {
		t.Fatalf("io error: %v", err)
	}
	if hasCode(diags, "duration_invalid") {
		dump(t, diags)
		t.Fatalf("expected 0 duration_invalid for <N>d on non-reaper fields")
	}
}

func TestLoadSoul_BadSID(t *testing.T) {
	src := `
sid: "not.a.fqdn..weird"
keeper:
  endpoints: [{ addr: "k:8443" }]
  tls: { ca: /var/lib/soul-stack/seed/ca.crt }
`
	tmp := writeTemp(t, "soul.yml", src)
	_, _, diags, _ := LoadSoul(tmp, ValidateOptions{})
	if !hasCode(diags, "sid_invalid_format") {
		dump(t, diags)
		t.Fatalf("expected sid_invalid_format")
	}
}

// EventStreamAddr/BootstrapAddr должны корректно склеивать IPv6-литерал:
// net.JoinHostPort оборачивает host в скобки (`[::1]:9443`). Ручная склейка
// `host + ":" + port` дала бы невалидный target `::1:9443`. docs объявляют
// host как «FQDN или IP» → IPv6 обещан и обязан работать.
func TestSoulKeeperEndpoint_AddrIPv6(t *testing.T) {
	cases := []struct {
		host          string
		wantEventStrm string
		wantBootstrap string
	}{
		{"::1", "[::1]:9443", "[::1]:9442"},
		{"2001:db8::1", "[2001:db8::1]:9443", "[2001:db8::1]:9442"},
		{"k1.dc1.example", "k1.dc1.example:9443", "k1.dc1.example:9442"},
		{"10.0.0.1", "10.0.0.1:9443", "10.0.0.1:9442"},
	}
	for _, c := range cases {
		ep := SoulKeeperEndpoint{Host: c.host, EventStreamPort: 9443, BootstrapPort: 9442}
		if got := ep.EventStreamAddr(); got != c.wantEventStrm {
			t.Errorf("EventStreamAddr(host=%q) = %q, want %q", c.host, got, c.wantEventStrm)
		}
		if got := ep.BootstrapAddr(); got != c.wantBootstrap {
			t.Errorf("BootstrapAddr(host=%q) = %q, want %q", c.host, got, c.wantBootstrap)
		}
	}
}

func writeTemp(t *testing.T, name, content string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func hasCode(ds []diag.Diagnostic, code string) bool {
	for _, d := range ds {
		if d.Code == code {
			return true
		}
	}
	return false
}

func hasCodeAt(ds []diag.Diagnostic, code, yamlPath string) bool {
	for _, d := range ds {
		if d.Code == code && d.YAMLPath == yamlPath {
			return true
		}
	}
	return false
}

func dump(t *testing.T, ds []diag.Diagnostic) {
	t.Helper()
	for _, d := range ds {
		t.Logf("[%s] %s:%d:%d %s %s", d.Code, d.File, d.Line, d.Column, d.Message, d.YAMLPath)
	}
}

func TestIsFQDN(t *testing.T) {
	good := []string{"a", "redis-01.prod.example.com", "host.local"}
	bad := []string{"", ".example", "example.", "x..y", "X.example.com", "-bad.com"}
	for _, g := range good {
		if !isFQDN(g) {
			t.Errorf("expected fqdn ok: %q", g)
		}
	}
	for _, b := range bad {
		if isFQDN(b) {
			t.Errorf("expected fqdn bad: %q", b)
		}
	}
}

func TestVaultRefRegex(t *testing.T) {
	good := []string{"vault:secret/keeper/postgres", "vault:secret/x#field"}
	bad := []string{"plain", "vault:", "vault:#field", "consul:secret/x"}
	for _, g := range good {
		if !reVaultRef.MatchString(g) {
			t.Errorf("expected vault-ref ok: %q", g)
		}
	}
	for _, b := range bad {
		if reVaultRef.MatchString(b) {
			t.Errorf("expected vault-ref bad: %q", b)
		}
	}
}

func TestSplitYAMLPath(t *testing.T) {
	segs, err := splitYAMLPath("$.foo.bar[2].baz")
	if err != nil {
		t.Fatal(err)
	}
	if len(segs) != 4 {
		t.Fatalf("got %d segs", len(segs))
	}
	if !segs[2].isIndex || segs[2].index != 2 {
		t.Errorf("seg[2]: %+v", segs[2])
	}
	if segs[3].name != "baz" {
		t.Errorf("seg[3]: %+v", segs[3])
	}
}

// «Контракт ошибок»: на отсутствующем файле возвращается ошибка + diag io_error.
func TestLoadKeeper_IOError(t *testing.T) {
	_, _, diags, err := LoadKeeper("/no/such/file.yml", ValidateOptions{})
	if err == nil {
		t.Fatal("expected io error")
	}
	if !hasCode(diags, "io_error") {
		t.Fatalf("expected io_error diag, got %+v", diags)
	}
}

func TestLoadKeeper_YAMLParseError(t *testing.T) {
	src := "kid: keeper-01\nlisten: {invalid yaml here\n"
	tmp := writeTemp(t, "k.yml", src)
	_, _, diags, err := LoadKeeper(tmp, ValidateOptions{})
	if err != nil {
		t.Fatalf("unexpected io error: %v", err)
	}
	if !hasCode(diags, "yaml_parse_error") {
		dump(t, diags)
		t.Fatalf("expected yaml_parse_error")
	}
}

// Empty/whitespace-only документ — `parser.ParseBytes` возвращает `file.Docs`
// длины 1 с `doc.Body == nil`. До фикса type-assert ниже вылетал в защитную
// ветку, вызывавшую `GetToken()` на nil-interface → segfault (qa.1 blocker
// bug 1: typical truncate-then-write окно у редакторов и `sed -i`).
func TestLoadKeeperFromBytes_EmptyDocumentNoPanic(t *testing.T) {
	for _, content := range []string{"", "   ", "   \n\t  \n", "\n\n"} {
		_, _, diags, err := LoadKeeperFromBytes("k.yml", []byte(content), ValidateOptions{})
		if err != nil {
			t.Fatalf("io error on content=%q: %v", content, err)
		}
		if !hasCode(diags, "empty_document") {
			dump(t, diags)
			t.Fatalf("expected empty_document on content=%q", content)
		}
	}
}

// Multi-document YAML отвергается явным diagnostic-ом `multi_document_not_allowed`
// (см. delegation-qa-fixes Bug 1). Один config-файл = один документ.
func TestLoadKeeperFromBytes_MultiDocumentRejected(t *testing.T) {
	src := []byte(`kid: keeper-01
listen:
  grpc:
    bootstrap:    { addr: "0.0.0.0:9442", tls: { cert: /c, key: /k } }
    event_stream: { addr: "0.0.0.0:8443", tls: { cert: /c, key: /k, ca: /a } }
  openapi: { addr: "0.0.0.0:8080" }
  mcp:     { addr: "0.0.0.0:8081" }
  metrics: { addr: "0.0.0.0:9090" }
postgres:
  dsn_ref: vault:secret/keeper/postgres
  pool: { min: 1, max: 5 }
redis:
  addr: "r:6379"
  password_ref: vault:secret/keeper/redis
vault:
  addr: "https://v:8200"
  auth: { method: token }
  pki_mount: pki/x
---
kid: keeper-02
`)
	_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", src, ValidateOptions{})
	if !hasCode(diags, "multi_document_not_allowed") {
		dump(t, diags)
		t.Fatalf("expected multi_document_not_allowed")
	}
}

// UTF-8 BOM (EF BB BF) — strip на входе, конфиг должен валидироваться как
// обычный. До фикса: BOM попадал в первый ключ и вылетал unknown_key с
// невидимым префиксом перед `kid`.
func TestLoadKeeperFromBytes_BOMStripped(t *testing.T) {
	body := `kid: keeper-eu-west-01
listen:
  grpc:
    bootstrap:    { addr: "0.0.0.0:9442", tls: { cert: /c, key: /k } }
    event_stream: { addr: "0.0.0.0:8443", tls: { cert: /c, key: /k, ca: /a } }
  openapi: { addr: "0.0.0.0:8080" }
  mcp:     { addr: "0.0.0.0:8081" }
  metrics: { addr: "0.0.0.0:9090" }
postgres:
  dsn_ref: vault:secret/keeper/postgres
  pool: { min: 1, max: 5 }
redis:
  addr: "r:6379"
  password_ref: vault:secret/keeper/redis
vault:
  addr: "https://v:8200"
  auth: { method: token }
  pki_mount: pki/x
`
	src := append([]byte{0xEF, 0xBB, 0xBF}, []byte(body)...)
	_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", src, ValidateOptions{})
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("expected 0 errors for BOM-prefixed valid keeper.yml")
	}
}

// Required top-level блоки (kid/listen/postgres/redis/vault) обязаны
// присутствовать (docs/keeper/config.md). Пустой keeper.yml → набор
// `missing_required_field` diagnostic-ов.
func TestLoadKeeperFromBytes_EmptyMissingAllRequired(t *testing.T) {
	src := []byte("postgres: {}\n") // один голос для возможного auto-detect, но почти пустой
	_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", src, ValidateOptions{})
	want := []string{"kid", "listen", "redis", "vault"}
	for _, k := range want {
		found := false
		for _, d := range diags {
			if d.Code == "missing_required_field" && d.YAMLPath == "$."+k {
				found = true
				break
			}
		}
		if !found {
			dump(t, diags)
			t.Fatalf("expected missing_required_field for $.%s", k)
		}
	}
}

// Каждый required-блок keeper-а проверяется индивидуально.
func TestLoadKeeperFromBytes_MissingPerField(t *testing.T) {
	full := map[string]string{
		"kid":      `kid: keeper-eu-west-01`,
		"listen":   "listen:\n  grpc:\n    bootstrap:    { addr: \"0.0.0.0:9442\", tls: { cert: /c, key: /k } }\n    event_stream: { addr: \"0.0.0.0:8443\", tls: { cert: /c, key: /k, ca: /a } }\n  openapi: { addr: \"0.0.0.0:8080\" }\n  mcp:     { addr: \"0.0.0.0:8081\" }\n  metrics: { addr: \"0.0.0.0:9090\" }",
		"postgres": "postgres:\n  dsn_ref: vault:secret/keeper/postgres\n  pool: { min: 1, max: 5 }",
		"redis":    "redis:\n  addr: \"r:6379\"\n  password_ref: vault:secret/keeper/redis",
		"vault":    "vault:\n  addr: \"https://v:8200\"\n  auth: { method: token }\n  pki_mount: pki/x",
	}
	for omit := range full {
		omit := omit
		t.Run("omit_"+omit, func(t *testing.T) {
			var b strings.Builder
			for k, block := range full {
				if k == omit {
					continue
				}
				b.WriteString(block)
				b.WriteString("\n")
			}
			_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", []byte(b.String()), ValidateOptions{})
			found := false
			for _, d := range diags {
				if d.Code == "missing_required_field" && d.YAMLPath == "$."+omit {
					found = true
					break
				}
			}
			if !found {
				dump(t, diags)
				t.Fatalf("expected missing_required_field for $.%s", omit)
			}
		})
	}
}

// listen: присутствует, но все четыре addr пусты — тоже missing_required_field.
func TestLoadKeeperFromBytes_ListenAllAddrsEmpty(t *testing.T) {
	src := []byte(`kid: keeper-01
listen: {}
postgres:
  dsn_ref: vault:secret/keeper/postgres
  pool: { min: 1, max: 5 }
redis:
  addr: "r:6379"
  password_ref: vault:secret/keeper/redis
vault:
  addr: "https://v:8200"
  auth: { method: token }
  pki_mount: pki/x
`)
	_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", src, ValidateOptions{})
	found := false
	for _, d := range diags {
		if d.Code == "missing_required_field" && d.YAMLPath == "$.listen" {
			found = true
			break
		}
	}
	if !found {
		dump(t, diags)
		t.Fatalf("expected missing_required_field for empty $.listen")
	}
}

// listen: только mcp.addr пуст (остальные три заданы) — требование
// «встроенный MCP» (requirements.md) даёт error `mcp_listener_required`.
func TestLoadKeeperFromBytes_MCPListenerRequired(t *testing.T) {
	src := []byte(`kid: keeper-01
listen:
  grpc:
    bootstrap:    { addr: "0.0.0.0:9442", tls: { cert: /c, key: /k } }
    event_stream: { addr: "0.0.0.0:8443", tls: { cert: /c, key: /k, ca: /a } }
  openapi: { addr: "0.0.0.0:8080" }
  metrics: { addr: "0.0.0.0:9090" }
postgres:
  dsn_ref: vault:secret/keeper/postgres
  pool: { min: 1, max: 5 }
redis:
  addr: "r:6379"
  password_ref: vault:secret/keeper/redis
vault:
  addr: "https://v:8200"
  auth: { method: token }
  pki_mount: pki/x
`)
	_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", src, ValidateOptions{})
	found := false
	for _, d := range diags {
		if d.Code == "mcp_listener_required" && d.YAMLPath == "$.listen.mcp.addr" {
			found = true
			break
		}
	}
	if !found {
		dump(t, diags)
		t.Fatalf("expected mcp_listener_required for empty $.listen.mcp.addr")
	}
}

// soul: блок keeper отсутствует → missing_required_field для $.keeper.
func TestLoadSoulFromBytes_MissingKeeper(t *testing.T) {
	src := []byte("sid: redis-01.prod.example.com\n")
	_, _, diags, _ := LoadSoulFromBytes("soul.yml", src, ValidateOptions{})
	found := false
	for _, d := range diags {
		if d.Code == "missing_required_field" && d.YAMLPath == "$.keeper" {
			found = true
			break
		}
	}
	if !found {
		dump(t, diags)
		t.Fatalf("expected missing_required_field for $.keeper")
	}
}

// soul: блок keeper есть, но endpoints отсутствует/пуст → missing_required_field
// для $.keeper.endpoints.
func TestLoadSoulFromBytes_EmptyKeeperEndpoints(t *testing.T) {
	src := []byte(`sid: redis-01.prod.example.com
keeper:
  endpoints: []
  tls: { ca: /var/lib/soul-stack/seed/ca.crt }
`)
	_, _, diags, _ := LoadSoulFromBytes("soul.yml", src, ValidateOptions{})
	found := false
	for _, d := range diags {
		if d.Code == "missing_required_field" && d.YAMLPath == "$.keeper.endpoints" {
			found = true
			break
		}
	}
	if !found {
		dump(t, diags)
		t.Fatalf("expected missing_required_field for $.keeper.endpoints")
	}
}

// soul: endpoint без host → missing_required_field на $.keeper.endpoints[0].host.
func TestLoadSoulFromBytes_EndpointMissingHost(t *testing.T) {
	src := []byte(`sid: redis-01.prod.example.com
keeper:
  endpoints:
    - event_stream_port: 9443
      bootstrap_port: 9442
  tls: { ca: /var/lib/soul-stack/seed/ca.crt }
`)
	_, _, diags, _ := LoadSoulFromBytes("soul.yml", src, ValidateOptions{})
	if !hasCodeAt(diags, "missing_required_field", "$.keeper.endpoints[0].host") {
		dump(t, diags)
		t.Fatalf("expected missing_required_field for $.keeper.endpoints[0].host")
	}
}

// soul: оба порта обязательны (architect: «безопасность на первом месте»).
// Отсутствие event_stream_port / bootstrap_port → дедицированные diag-коды.
func TestLoadSoulFromBytes_EndpointPortsRequired(t *testing.T) {
	src := []byte(`sid: redis-01.prod.example.com
keeper:
  endpoints:
    - host: k1.dc1.example
  tls: { ca: /var/lib/soul-stack/seed/ca.crt }
`)
	_, _, diags, _ := LoadSoulFromBytes("soul.yml", src, ValidateOptions{})
	if !hasCodeAt(diags, "event_stream_port_required", "$.keeper.endpoints[0].event_stream_port") {
		dump(t, diags)
		t.Fatalf("expected event_stream_port_required for $.keeper.endpoints[0].event_stream_port")
	}
	if !hasCodeAt(diags, "bootstrap_port_required", "$.keeper.endpoints[0].bootstrap_port") {
		dump(t, diags)
		t.Fatalf("expected bootstrap_port_required for $.keeper.endpoints[0].bootstrap_port")
	}
}

// soul: порт вне диапазона 1..65535 → port_out_of_range.
func TestLoadSoulFromBytes_EndpointPortOutOfRange(t *testing.T) {
	src := []byte(`sid: redis-01.prod.example.com
keeper:
  endpoints:
    - host: k1.dc1.example
      event_stream_port: 99999
      bootstrap_port: 9442
  tls: { ca: /var/lib/soul-stack/seed/ca.crt }
`)
	_, _, diags, _ := LoadSoulFromBytes("soul.yml", src, ValidateOptions{})
	if !hasCodeAt(diags, "port_out_of_range", "$.keeper.endpoints[0].event_stream_port") {
		dump(t, diags)
		t.Fatalf("expected port_out_of_range for $.keeper.endpoints[0].event_stream_port")
	}
}

// soul: валидный Form-A endpoint — никаких ошибок по endpoints.
func TestLoadSoulFromBytes_EndpointFormAClean(t *testing.T) {
	src := []byte(`sid: redis-01.prod.example.com
keeper:
  endpoints:
    - host: k1.dc1.example
      event_stream_port: 9443
      bootstrap_port: 9442
  tls: { ca: /var/lib/soul-stack/seed/ca.crt }
`)
	_, _, diags, _ := LoadSoulFromBytes("soul.yml", src, ValidateOptions{})
	for _, d := range diags {
		if d.Level == diag.LevelError {
			dump(t, diags)
			t.Fatalf("expected no errors for valid Form-A endpoint, got %s", d.Code)
		}
	}
}

// Контракт «schema-error не блокирует частичный decode»:
// type_mismatch на одном поле — остальные поля cfg остаются заполненными.
// Заодно проверяет, что type_mismatch diag несёт Line/Column > 0
// (Major #2 в review.1: goccy yaml.Error.GetToken() → Position.Line/Column).
func TestLoadKeeperFromBytes_PartialDecode(t *testing.T) {
	path := filepath.FromSlash("../../soul-lint/testdata/broken/keeper-partial-decode.yml")
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	cfg, doc, diags, ioErr := LoadKeeperFromBytes(path, src, ValidateOptions{})
	if ioErr != nil {
		t.Fatalf("io error: %v", ioErr)
	}
	if cfg == nil || doc == nil {
		t.Fatalf("cfg/doc must be non-nil for partial-decode case")
	}
	if cfg.KID != "keeper-01" {
		t.Errorf("partial decode lost kid: got %q, want %q", cfg.KID, "keeper-01")
	}
	var tm *diag.Diagnostic
	for i := range diags {
		if diags[i].Code == "type_mismatch" {
			tm = &diags[i]
			break
		}
	}
	if tm == nil {
		dump(t, diags)
		t.Fatalf("expected type_mismatch diag for postgres.pool")
	}
	if tm.Line <= 0 || tm.Column <= 0 {
		t.Fatalf("type_mismatch must carry line/column from goccy token; got line=%d col=%d msg=%q",
			tm.Line, tm.Column, tm.Message)
	}
}
