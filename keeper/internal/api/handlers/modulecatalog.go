// Module-catalog handler for the Operator API (`GET /v1/modules`) — publishes the
// modules available for a run + their input metadata. Purpose — module search in
// the Run→Command UI (replacing the free-text "custom module"): the operator picks
// a module from the catalog instead of typing a name by hand.
//
// Two sources:
//   - core — the static doc table [coreModuleDocs] (keeper does not see
//     soul/internal/coremod per ADR-011; the implementations carry no declarative
//     input schema — core params are empty, see modulecatalog_coredata.go);
//   - plugin — active (non-revoked) plugin_sigils records, params read from the
//     manifest `spec.states[*].input` (shared/plugin parser).
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
	"github.com/souls-guild/soul-stack/shared/plugin"
)

// PluginCatalogEntry — an active plugin grant for the catalog: coordinates +
// byte-exact manifest (for parsing params). Returned by [ModuleCatalogPlugins].
type PluginCatalogEntry struct {
	Namespace   string
	Name        string
	Ref         string
	ManifestRaw []byte
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

// moduleParam — one module parameter in the output. Filled from the manifest
// schema: for plugin — from manifest.yaml, for core — from the coremanifest
// registry. The Enum/Pattern/Format/Source fields mirror [plugin.InputParamDef]
// (ADR-045) — the backend builds the module's UI form from them.
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
		var params []moduleParam
		if m, ok := coremanifest.Default().Lookup(c.Name); ok {
			params = manifestToParams(m.Spec)
		} else {
			params = []moduleParam{}
		}
		items = append(items, moduleCatalogItem{
			Name:        c.Name,
			Kind:        "core",
			Description: c.Description,
			States:      c.States,
			ErrandSafe:  len(c.ErrandSafeStates) > 0,
			Params:      params,
		})
	}

	if h.plugins != nil {
		entries, err := h.plugins.ActivePlugins(ctx)
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			items = append(items, pluginCatalogItem(e))
		}
	}

	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
	return items, nil
}

// pluginCatalogItem builds a catalog entry from an active plugin grant. The name is
// `<namespace>.<name>` (the Soul Stack address form). States and params are read
// from the manifest; an invalid/unreadable manifest yields an entry with empty
// states/params (the plugin is granted → visible in the catalog, but without
// metadata, rather than silently hiding it).
func pluginCatalogItem(e PluginCatalogEntry) moduleCatalogItem {
	it := moduleCatalogItem{
		Name:      e.Namespace + "." + e.Name,
		Kind:      "plugin",
		Namespace: e.Namespace,
		States:    []string{},
		Params:    []moduleParam{},
	}

	m, _ := plugin.LoadFromBytes(plugin.FileName, e.ManifestRaw)
	if m == nil || m.Kind != plugin.KindSoulModule {
		// soul_module catalog: cloud_driver/ssh_provider/soul_beacon are not
		// applied as a Destiny step in Run→Command. Manifest unreadable →
		// an entry without states/params (the grant coordinates remain).
		return it
	}

	states := make([]string, 0, len(m.Spec.States))
	for state := range m.Spec.States {
		states = append(states, state)
	}
	sort.Strings(states)
	it.States = states
	it.Params = manifestToParams(m.Spec)
	return it
}

// manifestToParams flattens the input schema of all manifest states into a flat,
// deduplicated list of catalog params. Shared by core (coremanifest) and plugin: a
// single param may appear in several states (a vault-secret in installed and
// promoted) — we surface it in the catalog once. required/secret = true if it is so
// in at least one state; Type/Description/Pattern/Format/Enum/Source are taken from
// the first state where they are set (determinism thanks to sorting the order).
// Returns a non-nil slice (empty when there is no input).
func manifestToParams(spec plugin.ManifestSpec) []moduleParam {
	type pdef struct {
		typ, desc, pattern, format, example string
		required, secret, multiline         bool
		enum                                []any
		source                              *plugin.InputSource
		items                               *plugin.InputParamDef
	}
	seen := make(map[string]*pdef)
	order := make([]string, 0)
	for _, def := range spec.States {
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
			Name:        pname,
			Type:        d.typ,
			Required:    d.required,
			Secret:      d.secret,
			Description: d.desc,
			Enum:        d.enum,
			Pattern:     d.pattern,
			Format:      d.format,
			Source:      toModuleInputSource(d.source),
			Multiline:   d.multiline,
			Example:     d.example,
			Items:       toModuleParamItems(d.items),
		})
	}
	return params
}

// toModuleParamItems recursively propagates the list element type (ADR-045 S7)
// into the DTO. The element's name carries no meaning in the form — left empty.
func toModuleParamItems(it *plugin.InputParamDef) *moduleParam {
	if it == nil {
		return nil
	}
	return &moduleParam{
		Type:        it.Type,
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
