package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
)

// fakeCatalogPlugins — мок [ModuleCatalogPlugins] для транспорт-тестов.
type fakeCatalogPlugins struct {
	entries []PluginCatalogEntry
	err     error
}

func (f fakeCatalogPlugins) ActivePlugins(context.Context) ([]PluginCatalogEntry, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.entries, nil
}

// soulModuleManifest — минимальный валидный soul_module manifest.yaml с двумя
// state-ами и пересекающимся param-ом (vault-secret в обоих).
const soulModuleManifest = `kind: soul_module
protocol_version: 1
namespace: official
name: postgres-user
spec:
  states:
    present:
      description: ensure user exists
      input:
        username:
          type: string
          required: true
          description: имя роли
        password:
          type: string
          secret: true
          pattern: "^vault:.*"
    absent:
      description: drop user
      input:
        username:
          type: string
          required: true
`

func findItem(items []moduleCatalogItem, name string) (moduleCatalogItem, bool) {
	for _, it := range items {
		if it.Name == name {
			return it, true
		}
	}
	return moduleCatalogItem{}, false
}

// catalogProblemType извлекает problem.Type из ошибки ListTyped/GetTyped (nil → "").
func catalogProblemType(t *testing.T, err error) string {
	t.Helper()
	if err == nil {
		return ""
	}
	d, ok := AsProblemDetails(err)
	if !ok {
		t.Fatalf("ошибка не *problemError: %T %v", err, err)
	}
	return d.Type
}

func TestModuleCatalog_ListTyped_CoreAndPlugin(t *testing.T) {
	h := NewModuleCatalogHandler(fakeCatalogPlugins{
		entries: []PluginCatalogEntry{
			{Namespace: "official", Name: "postgres-user", Ref: "v1.0.0", ManifestRaw: []byte(soulModuleManifest)},
		},
	}, nil)

	resp, err := h.ListTyped(context.Background(), false)
	if err != nil {
		t.Fatalf("ListTyped: %v", err)
	}

	// core: все 21 (18 soul-side + 3 keeper-side) присутствуют.
	if len(resp.Items) != len(coreModuleDocs)+1 {
		t.Fatalf("ожидали %d записей (core + 1 plugin), получили %d", len(coreModuleDocs)+1, len(resp.Items))
	}

	// Сортировка по name.
	for i := 1; i < len(resp.Items); i++ {
		if resp.Items[i-1].Name > resp.Items[i].Name {
			t.Fatalf("выдача не отсортирована: %q > %q", resp.Items[i-1].Name, resp.Items[i].Name)
		}
	}

	cmd, ok := findItem(resp.Items, "core.cmd")
	if !ok {
		t.Fatal("core.cmd отсутствует в каталоге")
	}
	if cmd.Kind != "core" {
		t.Errorf("core.cmd kind=%q, ожидали core", cmd.Kind)
	}
	if !cmd.ErrandSafe {
		t.Error("core.cmd должен быть errand_safe (whitelist core.cmd.shell)")
	}
	// core params теперь читаются из coremanifest (ADR-045 S2): core.cmd несёт
	// cmd/cwd/env/timeout/onlyif/unless; cmd — required.
	if len(cmd.Params) == 0 {
		t.Error("core.cmd params должны заполняться из coremanifest, получили 0")
	}
	if cp := findParam(cmd.Params, "cmd"); cp == nil || !cp.Required {
		t.Errorf("core.cmd должен нести required-param cmd: %+v", cp)
	}

	pkg, _ := findItem(resp.Items, "core.pkg")
	if pkg.ErrandSafe {
		t.Error("core.pkg НЕ errand_safe")
	}

	// plugin: имя <ns>.<name>, params из manifest, дедуп username.
	pg, ok := findItem(resp.Items, "official.postgres-user")
	if !ok {
		t.Fatal("plugin official.postgres-user отсутствует")
	}
	if pg.Kind != "plugin" || pg.Namespace != "official" {
		t.Errorf("plugin kind=%q ns=%q", pg.Kind, pg.Namespace)
	}
	if len(pg.States) != 2 {
		t.Errorf("ожидали 2 state-а (present/absent), получили %v", pg.States)
	}
	if len(pg.Params) != 2 {
		t.Fatalf("ожидали 2 уникальных param-а (username/password), получили %d: %+v", len(pg.Params), pg.Params)
	}
	uname, pword := findParam(pg.Params, "username"), findParam(pg.Params, "password")
	if uname == nil || !uname.Required {
		t.Errorf("username должен быть required: %+v", uname)
	}
	if pword == nil || !pword.Secret {
		t.Errorf("password должен быть secret: %+v", pword)
	}
}

// sourceModuleManifest — soul_module manifest с обоими source-дискриминаторами
// (ADR-044/ADR-045): incarnation_hosts (bool) и choir (string). Guard на
// snake_case wire-сериализацию moduleParam.Source.
const sourceModuleManifest = `kind: soul_module
protocol_version: 1
namespace: official
name: with-source
spec:
  states:
    present:
      description: source-bearing state
      input:
        host:
          type: string
          source:
            incarnation_hosts: true
        voice:
          type: string
          source:
            choir: alpha
`

// TestModuleCatalog_Source_SnakeCaseWire — guard на wire-контракт source-picker
// формы модуля (BUG-FIX): сырой JSON ответа должен нести snake_case-ключи
// `incarnation_hosts`/`choir`, а НЕ PascalCase-имена Go-полей. Регрессия (потеря
// json-тегов на shared.InputSource) вернёт PascalCase и сломает форму — тест
// ассертит на сырых байтах (marshal native reply), поэтому ловит её мутационно.
func TestModuleCatalog_Source_SnakeCaseWire(t *testing.T) {
	h := NewModuleCatalogHandler(fakeCatalogPlugins{
		entries: []PluginCatalogEntry{
			{Namespace: "official", Name: "with-source", Ref: "v1.0.0", ManifestRaw: []byte(sourceModuleManifest)},
		},
	}, nil)

	resp, err := h.ListTyped(context.Background(), false)
	if err != nil {
		t.Fatalf("ListTyped: %v", err)
	}
	rawBytes, _ := json.Marshal(resp)
	raw := string(rawBytes)

	for _, key := range []string{`"incarnation_hosts"`, `"choir"`} {
		if !strings.Contains(raw, key) {
			t.Errorf("wire-JSON не содержит snake_case-ключ %s; source-picker формы сломан:\n%s", key, raw)
		}
	}
	for _, bad := range []string{`"IncarnationHosts"`, `"Choir"`} {
		if strings.Contains(raw, bad) {
			t.Errorf("wire-JSON содержит PascalCase-ключ %s (регресс json-тегов InputSource); ожидался snake_case", bad)
		}
	}

	// Семантика тоже на месте: значения source корректны.
	mod, ok := findItem(resp.Items, "official.with-source")
	if !ok {
		t.Fatal("official.with-source отсутствует в каталоге")
	}
	host := findParam(mod.Params, "host")
	if host == nil || host.Source == nil || host.Source.IncarnationHosts == nil || !*host.Source.IncarnationHosts {
		t.Errorf("host.source.incarnation_hosts должен быть true: %+v", host)
	}
	voice := findParam(mod.Params, "voice")
	if voice == nil || voice.Source == nil || voice.Source.Choir == nil || *voice.Source.Choir != "alpha" {
		t.Errorf("voice.source.choir должен быть \"alpha\": %+v", voice)
	}
}

func findParam(ps []moduleParam, name string) *moduleParam {
	for i := range ps {
		if ps[i].Name == name {
			return &ps[i]
		}
	}
	return nil
}

func TestModuleCatalog_ListTyped_ErrandSafeFilter(t *testing.T) {
	h := NewModuleCatalogHandler(nil, nil)

	resp, err := h.ListTyped(context.Background(), true)
	if err != nil {
		t.Fatalf("ListTyped: %v", err)
	}

	want := map[string]bool{"core.cmd": true, "core.exec": true, "core.http": true}
	if len(resp.Items) != len(want) {
		t.Fatalf("ожидали %d errand-safe core, получили %d: %+v", len(want), len(resp.Items), resp.Items)
	}
	for _, it := range resp.Items {
		if !want[it.Name] {
			t.Errorf("неожиданный errand_safe модуль: %q", it.Name)
		}
		if !it.ErrandSafe {
			t.Errorf("%q попал в errand_safe-фильтр без флага", it.Name)
		}
	}
}

func TestModuleCatalog_ListTyped_NoPlugins(t *testing.T) {
	h := NewModuleCatalogHandler(nil, nil)

	resp, err := h.ListTyped(context.Background(), false)
	if err != nil {
		t.Fatalf("ListTyped: %v", err)
	}
	if len(resp.Items) != len(coreModuleDocs) {
		t.Fatalf("без plugins ожидали %d core-модулей, получили %d", len(coreModuleDocs), len(resp.Items))
	}
	for _, it := range resp.Items {
		if it.Kind != "core" {
			t.Errorf("при nil plugins запись %q kind=%q (ожидали только core)", it.Name, it.Kind)
		}
	}
}

func TestModuleCatalog_ListTyped_RevokedPluginNotShown(t *testing.T) {
	// ActivePlugins возвращает ТОЛЬКО активные (revoked отфильтрованы на уровне
	// store.ListActive); каталог не должен изобретать revoked-плагины.
	h := NewModuleCatalogHandler(fakeCatalogPlugins{entries: nil}, nil)

	resp, err := h.ListTyped(context.Background(), false)
	if err != nil {
		t.Fatalf("ListTyped: %v", err)
	}
	for _, it := range resp.Items {
		if it.Kind == "plugin" {
			t.Errorf("revoked/нет активных plugin → не должно быть plugin-записей, нашли %q", it.Name)
		}
	}
}

func TestModuleCatalog_ListTyped_PluginStoreError(t *testing.T) {
	h := NewModuleCatalogHandler(fakeCatalogPlugins{err: errors.New("pg down")}, nil)

	_, err := h.ListTyped(context.Background(), false)
	if got := catalogProblemType(t, err); !strings.Contains(got, "internal") {
		t.Fatalf("при сбое реестра ожидали internal (500), problem.Type = %q", got)
	}
}

func TestModuleCatalog_GetTyped_Found(t *testing.T) {
	h := NewModuleCatalogHandler(nil, nil)

	it, err := h.GetTyped(context.Background(), "core.service")
	if err != nil {
		t.Fatalf("GetTyped: %v", err)
	}
	if it.Name != "core.service" || it.Kind != "core" {
		t.Errorf("получили %+v", it)
	}
}

func TestModuleCatalog_GetTyped_NotFound(t *testing.T) {
	h := NewModuleCatalogHandler(nil, nil)

	_, err := h.GetTyped(context.Background(), "core.nonexistent")
	if got := catalogProblemType(t, err); got != problem.TypeNotFound {
		t.Fatalf("ожидали not-found (404), problem.Type = %q", got)
	}
}
