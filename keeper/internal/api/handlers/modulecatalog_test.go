package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
)

// fakeCatalogPlugins — mock [ModuleCatalogPlugins] for the transport tests.
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

// soulModuleManifest — a minimal valid soul_module manifest.yaml with two
// states and an overlapping param (a vault secret in both).
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
          description: role name
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

// catalogProblemType extracts problem.Type from a ListTyped/GetTyped error (nil → "").
func catalogProblemType(t *testing.T, err error) string {
	t.Helper()
	if err == nil {
		return ""
	}
	d, ok := AsProblemDetails(err)
	if !ok {
		t.Fatalf("error is not *problemError: %T %v", err, err)
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

	// core: all 21 (18 soul-side + 3 keeper-side) are present.
	if len(resp.Items) != len(coreModuleDocs)+1 {
		t.Fatalf("expected %d entries (core + 1 plugin), got %d", len(coreModuleDocs)+1, len(resp.Items))
	}

	// Sorted by name.
	for i := 1; i < len(resp.Items); i++ {
		if resp.Items[i-1].Name > resp.Items[i].Name {
			t.Fatalf("output not sorted: %q > %q", resp.Items[i-1].Name, resp.Items[i].Name)
		}
	}

	cmd, ok := findItem(resp.Items, "core.cmd")
	if !ok {
		t.Fatal("core.cmd missing from catalog")
	}
	if cmd.Kind != "core" {
		t.Errorf("core.cmd kind=%q, expected core", cmd.Kind)
	}
	if !cmd.ErrandSafe {
		t.Error("core.cmd must be errand_safe (whitelist core.cmd.shell)")
	}
	// core params are now read from coremanifest (ADR-045 S2): core.cmd carries
	// cmd/cwd/env/timeout/onlyif/unless; cmd is required.
	if len(cmd.Params) == 0 {
		t.Error("core.cmd params must be populated from coremanifest, got 0")
	}
	if cp := findParam(cmd.Params, "cmd"); cp == nil || !cp.Required {
		t.Errorf("core.cmd must carry required-param cmd: %+v", cp)
	}

	pkg, _ := findItem(resp.Items, "core.pkg")
	if pkg.ErrandSafe {
		t.Error("core.pkg is NOT errand_safe")
	}

	// plugin: name <ns>.<name>, params from manifest, username dedup.
	pg, ok := findItem(resp.Items, "official.postgres-user")
	if !ok {
		t.Fatal("plugin official.postgres-user missing")
	}
	if pg.Kind != "plugin" || pg.Namespace != "official" {
		t.Errorf("plugin kind=%q ns=%q", pg.Kind, pg.Namespace)
	}
	if len(pg.States) != 2 {
		t.Errorf("expected 2 states (present/absent), got %v", pg.States)
	}
	if len(pg.Params) != 2 {
		t.Fatalf("expected 2 unique params (username/password), got %d: %+v", len(pg.Params), pg.Params)
	}
	uname, pword := findParam(pg.Params, "username"), findParam(pg.Params, "password")
	if uname == nil || !uname.Required {
		t.Errorf("username must be required: %+v", uname)
	}
	if pword == nil || !pword.Secret {
		t.Errorf("password must be secret: %+v", pword)
	}
}

// sourceModuleManifest — a soul_module manifest with both source discriminators
// (ADR-044/ADR-045): incarnation_hosts (bool) and choir (string). Guard for the
// snake_case wire serialization of moduleParam.Source.
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

// TestModuleCatalog_Source_SnakeCaseWire — guard for the wire contract of the module
// source-picker form (BUG-FIX): the raw JSON response must carry snake_case keys
// `incarnation_hosts`/`choir`, NOT the PascalCase Go field names. A regression (loss of
// json tags on shared.InputSource) would return PascalCase and break the form — the test
// asserts on raw bytes (marshal native reply), so it catches it mutationally.
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
			t.Errorf("wire-JSON does not contain snake_case key %s; source-picker form broken:\n%s", key, raw)
		}
	}
	for _, bad := range []string{`"IncarnationHosts"`, `"Choir"`} {
		if strings.Contains(raw, bad) {
			t.Errorf("wire-JSON contains PascalCase key %s (InputSource json-tag regression); expected snake_case", bad)
		}
	}

	// Semantics are in place too: the source values are correct.
	mod, ok := findItem(resp.Items, "official.with-source")
	if !ok {
		t.Fatal("official.with-source missing from catalog")
	}
	host := findParam(mod.Params, "host")
	if host == nil || host.Source == nil || host.Source.IncarnationHosts == nil || !*host.Source.IncarnationHosts {
		t.Errorf("host.source.incarnation_hosts must be true: %+v", host)
	}
	voice := findParam(mod.Params, "voice")
	if voice == nil || voice.Source == nil || voice.Source.Choir == nil || *voice.Source.Choir != "alpha" {
		t.Errorf("voice.source.choir must be \"alpha\": %+v", voice)
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
		t.Fatalf("expected %d errand-safe core, got %d: %+v", len(want), len(resp.Items), resp.Items)
	}
	for _, it := range resp.Items {
		if !want[it.Name] {
			t.Errorf("unexpected errand_safe module: %q", it.Name)
		}
		if !it.ErrandSafe {
			t.Errorf("%q ended up in errand_safe filter without the flag", it.Name)
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
		t.Fatalf("without plugins expected %d core modules, got %d", len(coreModuleDocs), len(resp.Items))
	}
	for _, it := range resp.Items {
		if it.Kind != "core" {
			t.Errorf("with nil plugins entry %q kind=%q (expected core only)", it.Name, it.Kind)
		}
	}
}

func TestModuleCatalog_ListTyped_RevokedPluginNotShown(t *testing.T) {
	// ActivePlugins returns ONLY active plugins (revoked are filtered at the
	// store.ListActive level); the catalog must not invent revoked plugins.
	h := NewModuleCatalogHandler(fakeCatalogPlugins{entries: nil}, nil)

	resp, err := h.ListTyped(context.Background(), false)
	if err != nil {
		t.Fatalf("ListTyped: %v", err)
	}
	for _, it := range resp.Items {
		if it.Kind == "plugin" {
			t.Errorf("revoked/no active plugins -> there must be no plugin entries, found %q", it.Name)
		}
	}
}

func TestModuleCatalog_ListTyped_PluginStoreError(t *testing.T) {
	h := NewModuleCatalogHandler(fakeCatalogPlugins{err: errors.New("pg down")}, nil)

	_, err := h.ListTyped(context.Background(), false)
	if got := catalogProblemType(t, err); !strings.Contains(got, "internal") {
		t.Fatalf("on registry failure expected internal (500), problem.Type = %q", got)
	}
}

func TestModuleCatalog_GetTyped_Found(t *testing.T) {
	h := NewModuleCatalogHandler(nil, nil)

	it, err := h.GetTyped(context.Background(), "core.service")
	if err != nil {
		t.Fatalf("GetTyped: %v", err)
	}
	if it.Name != "core.service" || it.Kind != "core" {
		t.Errorf("got %+v", it)
	}
}

func TestModuleCatalog_GetTyped_NotFound(t *testing.T) {
	h := NewModuleCatalogHandler(nil, nil)

	_, err := h.GetTyped(context.Background(), "core.nonexistent")
	if got := catalogProblemType(t, err); got != problem.TypeNotFound {
		t.Fatalf("expected not-found (404), problem.Type = %q", got)
	}
}
