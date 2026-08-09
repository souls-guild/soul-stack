// Module-catalog handler for the Operator API (`GET /v1/modules`) — publishes the
// modules available for a run + their input metadata. Purpose — module search in
// the Run→Command UI (replacing the free-text "custom module"): the operator picks
// a module from the catalog instead of typing a name by hand.
//
// Two sources:
//   - core — the static doc table [coreModuleDocs] (keeper does not see
//     soul/internal/coremod per ADR-011; the implementations carry no declarative
//     input schema — core params are empty, see modulecatalog_coredata.go);
//   - plugin — active (non-revoked) plugin_sigils grants, params read from the
//     grant's signed schema document (`modules[*].states[*].input`). One entry per
//     MODULE, named `<alias>.<module>`: the artifact contributes level 2 only.
//
// RBAC — service.list (read-only catalog; read without audit, the service.list /
// role.list / plugin.list pattern). The permission is reused, no new one is added.
package handlers

import (
	"context"
	"io"
	"log/slog"
	"sort"

	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	"github.com/souls-guild/soul-stack/shared/coremanifest"
	"github.com/souls-guild/soul-stack/shared/diag"
	"github.com/souls-guild/soul-stack/shared/plugin"
)

// PluginCatalogEntry — an active plugin grant for the catalog: the registration
// alias, what it was granted on, and the byte-exact schema document the signature
// covers (which is where the modules and their params come from). Returned by
// [ModuleCatalogPlugins].
type PluginCatalogEntry struct {
	// Alias is address level 1 — the operator's registration, and the only name the
	// catalog can prefix a module with: the artifact carries none.
	Alias string
	// Source / Ref are the artifact's signed identity, shown so an operator can see
	// which repository the modules in their catalog actually came from.
	Source string
	Ref    string
	// Schema is the canonical schema document from the grant, not from the cache: the
	// catalog must describe what was APPROVED, not what a resolver last wrote to disk.
	Schema []byte
}

// ModuleCatalogPlugins — the read surface for active plugin grants for the
// catalog. Implemented by an adapter over the sigil store (production wire-up). If
// nil in [ModuleCatalogHandler], the catalog returns core only (the plugin section
// is empty) — a keeper without Sigil stays functional (the optional-Deps pattern).
type ModuleCatalogPlugins interface {
	// ActivePlugins returns the active (non-revoked) plugin grants. Order does not
	// matter — the handler sorts the output itself.
	ActivePlugins(ctx context.Context) ([]PluginCatalogEntry, error)
}

// ModuleCatalogHandler — `GET /v1/modules` + `GET /v1/modules/{name}`.
//
// Dependencies are immutable; safe for concurrent use — it holds no state between
// requests (the core table is read-only, the plugin lister is thread-safe by
// contract).
type ModuleCatalogHandler struct {
	plugins ModuleCatalogPlugins
	logger  *slog.Logger
}

// NewModuleCatalogHandler creates the handler. plugins is optional (nil → core
// only). logger nil → io.Discard.
func NewModuleCatalogHandler(plugins ModuleCatalogPlugins, logger *slog.Logger) *ModuleCatalogHandler {
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	return &ModuleCatalogHandler{plugins: plugins, logger: logger}
}

// moduleParam — one module parameter in the output. Filled from the schema
// document: for a plugin from the grant's signed bytes, for core from the
// coremanifest registry. The Enum/Pattern/Format/Source fields mirror
// [plugin.InputParamDef] (ADR-045) — the backend builds the module's UI form from
// them.
type moduleParam struct {
	Name        string `json:"name"`
	Type        string `json:"type,omitempty"`
	Required    bool   `json:"required"`
	Secret      bool   `json:"secret,omitempty"`
	Description string `json:"description,omitempty"`

	// UI-form fields (ADR-045 S2). omitempty — params without an extended schema
	// stay compact.
	Enum    []any              `json:"enum,omitempty"`
	Pattern string             `json:"pattern,omitempty"`
	Format  string             `json:"format,omitempty"`
	Source  *moduleInputSource `json:"source,omitempty"`

	// Multiline/Example — declarative UI hints (ADR-045 B3): a large textarea +
	// placeholder. omitempty — fields without hints stay compact.
	Multiline bool   `json:"multiline,omitempty"`
	Example   string `json:"example,omitempty"`

	// Items — the list element type (ADR-045 S7). For type=list/array it tells the
	// UI to build a typed list (e.g. list[int]) rather than a free-form list of
	// strings.
	Items *moduleParam `json:"items,omitempty"`

	// IntroducedIn — the engine release that added this parameter (ADR-0076(i)).
	// omitempty — a parameter that has been there since the baseline says nothing.
	IntroducedIn string `json:"introduced_in,omitempty"`

	// Deprecated — the other end of the same axis (ADR-0076 deprecation policy):
	// the parameter still works, but there is a release where it stops. Published
	// beside IntroducedIn because the catalog is where an author looks BEFORE
	// writing the task — a deprecation that only reaches them as a lint warning
	// arrives after the definition is already written. omitempty — a parameter
	// not on its way out says nothing.
	Deprecated *moduleDeprecation `json:"deprecated,omitempty"`
}

// moduleDeprecation — the wire shape of a param's `deprecated:` block
// (ADR-0076 deprecation policy, mirror of [plugin.DeprecatedDef]). An object
// rather than the rendered sentence: `use` drives the UI's "switch to this
// instead" affordance, and `removed_in` is what an operator sorts a migration
// plan by — a prose string would have to be parsed back apart to do either.
//
// The type name = the contract schema name huma derives (DefaultSchemaNamer
// capitalizes the first letter → "ModuleDeprecation").
type moduleDeprecation struct {
	// Since — the release that marked the param deprecated (INCLUSIVE).
	Since string `json:"since,omitempty"`
	// RemovedIn — the first release that no longer honors it (EXCLUSIVE, so it
	// reads exactly like a compat: window's max).
	RemovedIn string `json:"removed_in,omitempty"`
	// Use — the replacement param in the same state; empty when the param is
	// going away with no successor.
	Use string `json:"use,omitempty"`
}

// ModuleDeprecation — an exported alias for the deprecation wire type, through
// which the huma route (package api) references the schema without forking the
// wire shape (same pattern as [ModuleInputSource]).
type ModuleDeprecation = moduleDeprecation

// toModuleDeprecation projects the manifest block into the wire type. nil → nil
// (omitempty): the overwhelming majority of params carry no deprecation, and the
// catalog output for them stays byte-for-byte what it was.
func toModuleDeprecation(d *plugin.DeprecatedDef) *moduleDeprecation {
	if d == nil {
		return nil
	}
	return &moduleDeprecation{Since: d.Since, RemovedIn: d.RemovedIn, Use: d.Use}
}

// moduleCatalogItem — one catalog entry. The type name = the contract schema name
// from the hand-written spec (docs/keeper/openapi.yaml :5392 → ModuleCatalogItem):
// huma DefaultSchemaNamer capitalizes the first letter → "ModuleCatalogItem".
type moduleCatalogItem struct {
	Name        string        `json:"name"`
	Kind        string        `json:"kind"` // "core" | "plugin"
	Namespace   string        `json:"namespace,omitempty"`
	Description string        `json:"description,omitempty"`
	States      []string      `json:"states"`
	ErrandSafe  bool          `json:"errand_safe"`
	Params      []moduleParam `json:"params"`

	// errandSafeStates is internal projection metadata. The public catalog
	// keeps the historical module-level errand_safe bool, but a filtered list
	// must expose only states that the Soul runner will actually admit. It is
	// deliberately unexported so the wire/OpenAPI shape does not change.
	errandSafeStates []string

	// IntroducedIn — the engine release that added this module (ADR-0076(i)),
	// carried outward so an author can see the floor a module implies before
	// declaring a `compat:` window against it. omitempty — a module that predates
	// the metadata says nothing. Per-STATE versions are not surfaced here: the
	// `states` field is a flat name list, and widening it into objects would break
	// the contract the UI reads.
	IntroducedIn string `json:"introduced_in,omitempty"`
}

// moduleCatalogReply — the body of `GET /v1/modules`. The type name = the contract
// schema name from the hand-written spec (docs/keeper/openapi.yaml :5424 →
// ModuleCatalogReply): huma DefaultSchemaNamer capitalizes the first letter →
// "ModuleCatalogReply".
type moduleCatalogReply struct {
	Items []moduleCatalogItem `json:"items"`
}

// ModuleCatalogItem / ModuleCatalogReply — exported aliases for the catalog's
// internal wire types, through which the huma routes (package api) type the output
// without forking the wire shape (the fields carry the same json tags; huma builds
// the 200-body schema from them). The category-C equivalent of the module domain:
// the local types stay unexported for the handler test, the aliases give access
// from outside.
type (
	ModuleCatalogItem  = moduleCatalogItem
	ModuleCatalogReply = moduleCatalogReply
)

// ModuleCatalogSpecStub — a non-nil *ModuleCatalogHandler stub for generating the
// huma OpenAPI fragment (HumaModuleSpecYAML): on dump the domain handler is not
// called, but huma.Register requires non-nil for its nil check (parity with
// [RoleSpecStub]). plugins nil — the handler never executes in spec mode.
func ModuleCatalogSpecStub() *ModuleCatalogHandler {
	return &ModuleCatalogHandler{logger: slog.New(slog.NewJSONHandler(io.Discard, nil))}
}

// ListTyped — the extracted domain function of `GET /v1/modules` (FULL-TYPED unfold
// of ADR-054 §Pattern): the catalog without http.ResponseWriter/*http.Request.
// onlyErrandSafe — the `?errand_safe=true` filter. A plugin-registry read error →
// *problemError (500).
func (h *ModuleCatalogHandler) ListTyped(ctx context.Context, onlyErrandSafe bool) (ModuleCatalogReply, error) {
	items, err := h.buildCatalog(ctx)
	if err != nil {
		h.logger.Error("module.catalog: list plugins failed", slog.Any("error", err))
		return ModuleCatalogReply{}, &problemError{problem.New(problem.TypeInternalError, "", "list modules failed")}
	}
	out := make([]moduleCatalogItem, 0, len(items))
	for _, it := range items {
		if onlyErrandSafe && !it.ErrandSafe {
			continue
		}
		if onlyErrandSafe && len(it.errandSafeStates) > 0 {
			it.States = append([]string(nil), it.errandSafeStates...)
		}
		out = append(out, it)
	}
	return ModuleCatalogReply{Items: out}, nil
}

// GetTyped — the extracted domain function of `GET /v1/modules/{name}`. Errors —
// *problemError (404 no module / 500 registry failure); success — [ModuleCatalogItem].
func (h *ModuleCatalogHandler) GetTyped(ctx context.Context, name string) (ModuleCatalogItem, error) {
	items, err := h.buildCatalog(ctx)
	if err != nil {
		h.logger.Error("module.catalog: get plugins failed", slog.Any("error", err))
		return ModuleCatalogItem{}, &problemError{problem.New(problem.TypeInternalError, "", "get module failed")}
	}
	for _, it := range items {
		if it.Name == name {
			return it, nil
		}
	}
	return ModuleCatalogItem{}, &problemError{problem.New(problem.TypeNotFound, "", "no such module: "+name)}
}

// buildCatalog assembles the full catalog (core + plugin), sorted by name.
// Returns an error only on a plugin-registry read failure (core is static).
func (h *ModuleCatalogHandler) buildCatalog(ctx context.Context) ([]moduleCatalogItem, error) {
	items := make([]moduleCatalogItem, 0, len(coreModuleDocs))
	for _, c := range coreModuleDocs {
		params := []moduleParam{}
		introducedIn := ""
		if m, ok := coremanifest.Default().Lookup(c.Name); ok {
			params = moduleToParams(m)
			introducedIn = m.IntroducedIn
		}
		items = append(items, moduleCatalogItem{
			Name:             c.Name,
			Kind:             "core",
			Description:      c.Description,
			States:           c.States,
			ErrandSafe:       len(c.ErrandSafeStates) > 0,
			Params:           params,
			IntroducedIn:     introducedIn,
			errandSafeStates: append([]string(nil), c.ErrandSafeStates...),
		})
	}

	if h.plugins != nil {
		entries, err := h.plugins.ActivePlugins(ctx)
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			items = append(items, pluginCatalogItems(e)...)
		}
	}

	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
	return items, nil
}

// pluginCatalogItems builds the catalog entries of ONE active plugin grant — one per
// module the artifact serves, because one module is what a task addresses.
//
// The name is `<alias>.<module>`: level 1 is the registration alias the operator
// chose, level 2 the module's own name. The artifact contributes only level 2 — it
// carries no self-name — so the same bytes granted under two aliases would appear here
// as two independent module sets, which is exactly what registering them twice means.
//
// A grant whose schema does not parse yields ONE entry named after the alias with empty
// states/params: the plugin is granted, so hiding it would misreport the cluster, but
// there is nothing to say about what it accepts.
func pluginCatalogItems(e PluginCatalogEntry) []moduleCatalogItem {
	bare := moduleCatalogItem{
		Name:      e.Alias,
		Kind:      "plugin",
		Namespace: e.Alias,
		States:    []string{},
		Params:    []moduleParam{},
	}

	doc, diags := plugin.ParseDocument(plugin.SchemaFileName, e.Schema)
	if doc == nil || diag.HasErrors(diags) || doc.Kind != plugin.KindSoulModule {
		// soul_module catalog: cloud_driver/ssh_provider/soul_beacon are not applied
		// as a Destiny step in Run→Command, and an unreadable schema leaves nothing to
		// describe — either way the grant's coordinates remain.
		return []moduleCatalogItem{bare}
	}

	out := make([]moduleCatalogItem, 0, len(doc.Modules))
	for _, m := range doc.Modules {
		states := make([]string, 0, len(m.States))
		for state := range m.States {
			states = append(states, state)
		}
		sort.Strings(states)
		out = append(out, moduleCatalogItem{
			Name:         e.Alias + "." + m.Name,
			Kind:         "plugin",
			Namespace:    e.Alias,
			Description:  m.Description,
			States:       states,
			Params:       moduleToParams(m),
			IntroducedIn: m.IntroducedIn,
		})
	}
	if len(out) == 0 {
		return []moduleCatalogItem{bare}
	}
	return out
}

// manifestToParams flattens the input schema of all manifest states into a flat,
// deduplicated list of catalog params. Shared by core (coremanifest) and plugin: a
// single param may appear in several states (a vault-secret in installed and
// promoted) — we surface it in the catalog once. required/secret = true if it is so
// in at least one state; Type/Description/Pattern/Format/Enum/Source/IntroducedIn/
// Deprecated are taken from the first state where they are set (determinism thanks
// to sorting the order). Life-cycle metadata inherits that rule rather than getting
// its own: a param deprecated on one state and live on another surfaces as
// deprecated, which is the safe direction — the author is told to look, and the
// per-state truth is one manifest away.
// Returns a non-nil slice (empty when there is no input).
func moduleToParams(m plugin.ModuleDef) []moduleParam {
	type pdef struct {
		typ                            plugin.ParamType
		desc, pattern, format, example string
		introducedIn                   string
		required, secret, multiline    bool
		enum                           []any
		source                         *plugin.InputSource
		items                          *plugin.InputParamDef
		deprecated                     *plugin.DeprecatedDef
	}
	seen := make(map[string]*pdef)
	order := make([]string, 0)
	// States are visited in name order: "the first state where a field is set"
	// only means something with a fixed traversal, and map order is not one.
	stateNames := make([]string, 0, len(m.States))
	for state := range m.States {
		stateNames = append(stateNames, state)
	}
	sort.Strings(stateNames)
	for _, state := range stateNames {
		def := m.States[state]
		for pname, p := range def.Input {
			cur, ok := seen[pname]
			if !ok {
				cur = &pdef{}
				seen[pname] = cur
				order = append(order, pname)
			}
			if p.Type != "" {
				cur.typ = p.Type
			}
			if p.Description != "" {
				cur.desc = p.Description
			}
			if p.Pattern != "" {
				cur.pattern = p.Pattern
			}
			if p.Format != "" {
				cur.format = p.Format
			}
			if cur.enum == nil && p.Enum != nil {
				cur.enum = p.Enum
			}
			if cur.source == nil && p.Source != nil {
				cur.source = p.Source
			}
			if cur.items == nil && p.Items != nil {
				cur.items = p.Items
			}
			if p.Example != "" {
				cur.example = p.Example
			}
			if cur.introducedIn == "" {
				cur.introducedIn = p.IntroducedIn
			}
			if cur.deprecated == nil {
				cur.deprecated = p.Deprecated
			}
			cur.required = cur.required || p.Required
			cur.secret = cur.secret || p.Secret
			cur.multiline = cur.multiline || p.Multiline
		}
	}
	sort.Strings(order)

	params := make([]moduleParam, 0, len(order))
	for _, pname := range order {
		d := seen[pname]
		params = append(params, moduleParam{
			Name:         pname,
			Type:         string(d.typ),
			Required:     d.required,
			Secret:       d.secret,
			Description:  d.desc,
			Enum:         d.enum,
			Pattern:      d.pattern,
			Format:       d.format,
			Source:       toModuleInputSource(d.source),
			Multiline:    d.multiline,
			Example:      d.example,
			Items:        toModuleParamItems(d.items),
			IntroducedIn: d.introducedIn,
			Deprecated:   toModuleDeprecation(d.deprecated),
		})
	}
	return params
}

// toModuleParamItems recursively propagates the list element type (ADR-045 S7)
// into the DTO. The element's name carries no meaning in the form — left empty.
// Life-cycle metadata (introduced_in / deprecated) is deliberately NOT carried
// down: a version boundary belongs to the param an author writes, and a list
// element is not separately writable — half a list cannot be deprecated.
func toModuleParamItems(it *plugin.InputParamDef) *moduleParam {
	if it == nil {
		return nil
	}
	return &moduleParam{
		Type:        string(it.Type),
		Required:    it.Required,
		Secret:      it.Secret,
		Description: it.Description,
		Enum:        it.Enum,
		Pattern:     it.Pattern,
		Format:      it.Format,
		Source:      toModuleInputSource(it.Source),
		Multiline:   it.Multiline,
		Example:     it.Example,
		Items:       toModuleParamItems(it.Items),
	}
}

// moduleInputSource — the NATIVE wire shape of a param's source discriminator
// (handler-native T5d-2c-full, replaces ModuleInputSource). Shape 1:1 with the
// former one: choir (*string omitempty) — the SIDs of a specific Choir part;
// incarnation_hosts (*bool omitempty) — all SIDs of the current incarnation. The
// type name = the contract schema name from the hand-written spec (huma
// DefaultSchemaNamer capitalizes → "ModuleInputSource").
type moduleInputSource struct {
	Choir            *string `json:"choir,omitempty"`
	IncarnationHosts *bool   `json:"incarnation_hosts,omitempty"`
}

// ModuleInputSource — an exported alias for the source-discriminator wire type,
// through which the huma route (package api) references the schema without forking
// the wire shape.
type ModuleInputSource = moduleInputSource

// toModuleInputSource projects the domain [plugin.InputSource] (value fields) into
// the wire type [moduleInputSource] (pointer-optional, ADR-051(c) category C).
// nil source → nil (omitempty). Empty sub-keys are omitted: false/"" are not
// surfaced in the wire, symmetric with the domain form's json-omitempty — the
// operator sees exactly the sub-key set in the manifest.
func toModuleInputSource(s *plugin.InputSource) *moduleInputSource {
	if s == nil {
		return nil
	}
	out := &moduleInputSource{}
	if s.IncarnationHosts {
		v := true
		out.IncarnationHosts = &v
	}
	if s.Choir != "" {
		out.Choir = &s.Choir
	}
	return out
}
