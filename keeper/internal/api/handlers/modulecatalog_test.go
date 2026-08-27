package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	"github.com/souls-guild/soul-stack/sdk/schema"
	"github.com/souls-guild/soul-stack/shared/coremanifest"
)

// catalogSource is the git remote the fixture grants were issued on. It is the artifact's
// signed identity; the catalog shows it so an operator can see which repository the
// modules in their catalog actually came from.
const catalogSource = "https://example.com/soul-mod-postgres.git"

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

// The catalog fixtures are Go [schema.Document] values marshalled through the real
// serializer, not hand-written text. Since NIM-377 nobody writes a schema by hand — it
// is generated from `module.Def` — so a fixture written any other way would be testing
// a form the system never produces.
//
// None of them declares a namespace or a name for the ARTIFACT: there is nowhere in
// the format to put one. Address level 1 comes from the registration alias the catalog
// entry carries, level 2 from the module.

// docBytes marshals a document to the canonical bytes a grant would hold.
func docBytes(t *testing.T, doc schema.Document) []byte {
	t.Helper()
	out, err := schema.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal schema: %v", err)
	}
	return out
}

// twoStateModuleDoc — a soul_module with two states and an overlapping param (a vault
// secret in both), so the catalog's per-param flattening is exercised.
func twoStateModuleDoc() schema.Document {
	return schema.Document{
		Kind:            schema.KindSoulModule,
		ProtocolVersion: 1,
		Modules: []schema.Module{{
			Name:        "postgres-user",
			Description: "a postgres role",
			States: map[string]schema.State{
				"present": {
					Description: "ensure user exists",
					Input: schema.Input{
						"username": {Type: schema.String, Required: true, Description: "role name"},
						"password": {Type: schema.String, Secret: true, Pattern: `^vault:.*`},
					},
				},
				"absent": {
					Description: "drop user",
					Input: schema.Input{
						"username": {Type: schema.String, Required: true},
					},
				},
			},
		}},
	}
}

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
			{Alias: "official", Source: catalogSource, Ref: "v1.0.0", Schema: docBytes(t, twoStateModuleDoc())},
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
	// cmd/cwd/env/creates/onlyif/unless/exit_codes; cmd is required. The list is
	// illustrative — the assertion below is on the count and on `cmd`, so a new
	// param does not have to be added here to keep the test honest.
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

// sourceModuleDoc — a soul_module with both source discriminators (ADR-044/ADR-045):
// incarnation_hosts (bool) and choir (string). Guard for the snake_case wire
// serialization of moduleParam.Source.
func sourceModuleDoc() schema.Document {
	return schema.Document{
		Kind:            schema.KindSoulModule,
		ProtocolVersion: 1,
		Modules: []schema.Module{{
			Name: "with-source",
			States: map[string]schema.State{
				"present": {
					Description: "source-bearing state",
					Input: schema.Input{
						"host":  {Type: schema.String, Source: &schema.InputSource{IncarnationHosts: true}},
						"voice": {Type: schema.String, Source: &schema.InputSource{Choir: "alpha"}},
					},
				},
			},
		}},
	}
}

// TestModuleCatalog_Source_SnakeCaseWire — guard for the wire contract of the module
// source-picker form (BUG-FIX): the raw JSON response must carry snake_case keys
// `incarnation_hosts`/`choir`, NOT the PascalCase Go field names. A regression (loss of
// json tags on shared.InputSource) would return PascalCase and break the form — the test
// asserts on raw bytes (marshal native reply), so it catches it mutationally.
func TestModuleCatalog_Source_SnakeCaseWire(t *testing.T) {
	h := NewModuleCatalogHandler(fakeCatalogPlugins{
		entries: []PluginCatalogEntry{
			{Alias: "official", Source: catalogSource, Ref: "v1.0.0", Schema: docBytes(t, sourceModuleDoc())},
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
		if it.Name == "core.http" {
			if len(it.States) != 1 || it.States[0] != "probe" {
				t.Errorf("errand-safe core.http states=%v, want exact [probe]", it.States)
			}
		}
	}
}

func TestModuleCatalog_ListTyped_FullHTTPKeepsRequest(t *testing.T) {
	h := NewModuleCatalogHandler(nil, nil)
	resp, err := h.ListTyped(context.Background(), false)
	if err != nil {
		t.Fatalf("ListTyped: %v", err)
	}
	httpItem, ok := findItem(resp.Items, "core.http")
	if !ok {
		t.Fatal("core.http missing")
	}
	if len(httpItem.States) != 2 || httpItem.States[0] != "probe" || httpItem.States[1] != "request" {
		t.Fatalf("full core.http states=%v, want [probe request]", httpItem.States)
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

// introducedInDoc — a module carrying ADR-0076(i) metadata at both granularities: the
// module itself and a parameter added later than the state it hangs on.
func introducedInDoc() schema.Document {
	return schema.Document{
		Kind:            schema.KindSoulModule,
		ProtocolVersion: 1,
		Modules: []schema.Module{{
			Name:         "postgres-user",
			IntroducedIn: "1.4.0",
			States: map[string]schema.State{
				"present": {
					Description: "ensure user exists",
					Input: schema.Input{
						"username":        {Type: schema.String, Required: true},
						"selinux_context": {Type: schema.String, IntroducedIn: "2.5.0"},
					},
				},
			},
		}},
	}
}

// The catalog is where an author looks up what a module implies before declaring
// a compat: window against it (ADR-0076(i)), so introduced_in has to reach the
// wire — module-level and param-level.
func TestModuleCatalog_IntroducedInIsPublished(t *testing.T) {
	h := NewModuleCatalogHandler(fakeCatalogPlugins{
		entries: []PluginCatalogEntry{
			{Alias: "official", Source: catalogSource, Ref: "v1.0.0", Schema: docBytes(t, introducedInDoc())},
		},
	}, nil)

	resp, err := h.ListTyped(context.Background(), false)
	if err != nil {
		t.Fatalf("ListTyped: %v", err)
	}
	it, ok := findItem(resp.Items, "official.postgres-user")
	if !ok {
		t.Fatal("plugin module missing from the catalog")
	}
	if it.IntroducedIn != "1.4.0" {
		t.Errorf("module introduced_in = %q, want 1.4.0", it.IntroducedIn)
	}
	got := map[string]string{}
	for _, p := range it.Params {
		got[p.Name] = p.IntroducedIn
	}
	if got["selinux_context"] != "2.5.0" {
		t.Errorf("param introduced_in = %q, want 2.5.0", got["selinux_context"])
	}
	if got["username"] != "" {
		t.Errorf("a param with no metadata must stay empty, got %q", got["username"])
	}
}

// deprecatedDoc — a module whose param is on its way out (ADR-0076 deprecation policy)
// next to one that is not, so the catalog is proven to carry the block exactly where it
// is declared.
func deprecatedDoc() schema.Document {
	return schema.Document{
		Kind:            schema.KindSoulModule,
		ProtocolVersion: 1,
		Modules: []schema.Module{{
			Name: "postgres-user",
			States: map[string]schema.State{
				"present": {
					Description: "ensure user exists",
					Input: schema.Input{
						"username": {Type: schema.String, Required: true},
						"address": {Type: schema.String, Deprecated: &schema.Deprecated{
							Since: "0.4.0", RemovedIn: "0.6.0", Use: "username",
						}},
					},
				},
			},
		}},
	}
}

// The catalog is the surface an author reads BEFORE writing a task, so a
// deprecation has to reach it: learning about the deadline from a lint warning
// means learning after the definition is already written (NIM-205). The block
// travels as an object, not as the rendered sentence — `use` is what the UI
// offers as the replacement and `removed_in` is what a migration is planned by.
func TestModuleCatalog_DeprecatedIsPublished(t *testing.T) {
	h := NewModuleCatalogHandler(fakeCatalogPlugins{
		entries: []PluginCatalogEntry{
			{Alias: "official", Source: catalogSource, Ref: "v1.0.0", Schema: docBytes(t, deprecatedDoc())},
		},
	}, nil)

	resp, err := h.ListTyped(context.Background(), false)
	if err != nil {
		t.Fatalf("ListTyped: %v", err)
	}
	it, ok := findItem(resp.Items, "official.postgres-user")
	if !ok {
		t.Fatal("plugin module missing from the catalog")
	}
	got := map[string]*ModuleDeprecation{}
	for _, p := range it.Params {
		got[p.Name] = p.Deprecated
	}
	d := got["address"]
	if d == nil {
		t.Fatal("deprecated param carries no deprecation block")
	}
	if d.Since != "0.4.0" || d.RemovedIn != "0.6.0" || d.Use != "username" {
		t.Errorf("deprecation = %+v, want since=0.4.0 removed_in=0.6.0 use=username", *d)
	}
	if got["username"] != nil {
		t.Errorf("a live param must carry no deprecation, got %+v", *got["username"])
	}
}

// The addition must be invisible where nothing is deprecated: omitempty keeps
// today's bytes identical. Proven against the PARSER rather than a hand-kept list
// of params — a list rots silently the moment a core manifest declares its first
// deprecation (the NIM-206 lesson), while this cross-check keeps telling the truth
// and starts requiring the block on exactly the params that gained one.
func TestModuleCatalog_CoreDeprecationMatchesTheManifests(t *testing.T) {
	h := NewModuleCatalogHandler(nil, nil)
	resp, err := h.ListTyped(context.Background(), false)
	if err != nil {
		t.Fatalf("ListTyped: %v", err)
	}
	for _, it := range resp.Items {
		m, ok := coremanifest.Default().Lookup(it.Name)
		if !ok {
			continue // a core module with no embedded manifest carries no params.
		}
		declared := map[string]bool{}
		for _, def := range m.States {
			for name, p := range def.Input {
				if p.Deprecated != nil {
					declared[name] = true
				}
			}
		}
		for _, p := range it.Params {
			if declared[p.Name] && p.Deprecated == nil {
				t.Errorf("%s.%s is deprecated in the manifest but the catalog hides it", it.Name, p.Name)
			}
			if !declared[p.Name] && p.Deprecated != nil {
				t.Errorf("%s.%s carries a deprecation the manifest does not declare", it.Name, p.Name)
			}
		}
	}
}

// Core modules are unstamped today (the catalog has not changed since the
// baseline release) — the field must be absent rather than invented.
func TestModuleCatalog_CoreCarriesNoIntroducedInYet(t *testing.T) {
	h := NewModuleCatalogHandler(nil, nil)
	resp, err := h.ListTyped(context.Background(), false)
	if err != nil {
		t.Fatalf("ListTyped: %v", err)
	}
	for _, it := range resp.Items {
		if it.IntroducedIn != "" {
			t.Errorf("core module %s claims introduced_in=%q; stamp it in RELEASING when that becomes true", it.Name, it.IntroducedIn)
		}
	}
}

// TestModuleCatalog_UnreadableSchemaStaysLabelled — a grant whose schema does not
// parse must still appear as a NAMED entry, never as a blank one.
//
// The catalog now takes every label from the grant's alias, because the artifact
// self-reports no name at all. That removes a whole class of "unlabelled module in
// the UI" bug — but only as long as nothing falls back to a name read out of the
// document. A grant is a fact about the cluster: hiding it would misreport what is
// approved, and showing it without a name would put a row in the operator's form
// that they cannot act on.
func TestModuleCatalog_UnreadableSchemaStaysLabelled(t *testing.T) {
	h := NewModuleCatalogHandler(fakeCatalogPlugins{
		entries: []PluginCatalogEntry{
			{Alias: "redis", Source: catalogSource, Ref: "v1.0.0", Schema: []byte("not a schema document")},
		},
	}, nil)

	resp, err := h.ListTyped(context.Background(), false)
	if err != nil {
		t.Fatalf("ListTyped: %v", err)
	}
	it, ok := findItem(resp.Items, "redis")
	if !ok {
		t.Fatalf("a grant with an unreadable schema vanished from the catalog: %+v", resp.Items)
	}
	if it.Namespace != "redis" || it.Kind != "plugin" {
		t.Errorf("entry = %+v, want it labelled by the registration alias", it)
	}
	// Non-nil empties: the wire carries `[]`, not null, so the form renders an
	// empty module rather than crashing on a missing field.
	if it.States == nil || it.Params == nil {
		t.Errorf("states/params must be non-nil empties, got states=%v params=%v", it.States, it.Params)
	}
}

// TestModuleCatalog_AliasIsTheOnlyLabelSource — the same artifact registered under
// two aliases must produce two independently addressed module sets. Level 2 comes
// from the document, level 1 only ever from the registration.
func TestModuleCatalog_AliasIsTheOnlyLabelSource(t *testing.T) {
	h := NewModuleCatalogHandler(fakeCatalogPlugins{
		entries: []PluginCatalogEntry{
			{Alias: "redis", Source: catalogSource, Ref: "v1.0.0", Schema: docBytes(t, twoStateModuleDoc())},
			{Alias: "redis-community", Source: catalogSource, Ref: "v1.0.0", Schema: docBytes(t, twoStateModuleDoc())},
		},
	}, nil)

	resp, err := h.ListTyped(context.Background(), false)
	if err != nil {
		t.Fatalf("ListTyped: %v", err)
	}
	for _, want := range []string{"redis.postgres-user", "redis-community.postgres-user"} {
		it, ok := findItem(resp.Items, want)
		if !ok {
			t.Fatalf("%q missing — the alias is not reaching address level 1: %+v", want, resp.Items)
		}
		// Each carries the real contract, not a stub: this is the surface the UI
		// builds its form from, so an entry that compiles but lists no states would
		// break the form silently.
		if len(it.States) != 2 {
			t.Errorf("%s states = %v, want the module's two states", want, it.States)
		}
		if len(it.Params) != 2 {
			t.Errorf("%s params = %d, want the module's two params", want, len(it.Params))
		}
	}
}
