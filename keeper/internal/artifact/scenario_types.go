package artifact

import (
	"errors"
	"io/fs"
	"log/slog"

	yaml "gopkg.in/yaml.v3"
)

// typesCatalogFile — catalog of reusable named types at the root of the
// Service repo (`types.yml`), a sibling of service.yml/scenario/. Parallels
// scenarioMainFile/serviceManifestFile.
const typesCatalogFile = "types.yml"

// typesSectionKey — the sole top-level key in types.yml.
const typesSectionKey = "types"

// typeRefKey — discriminator key for a reference to a named type in the input DSL.
const typeRefKey = "$type"

// typeAnnotationKey — forward-compat annotation the backend attaches next to
// the resolved node: `x-type: <Name>` — the original type name, so the UI can
// render a widget/label "this is a value of type X". WITHOUT the resolve the
// UI would get the raw `$type` and fail silently — so the resolve is strictly
// backend-side BEFORE the projection.
const typeAnnotationKey = "x-type"

// typeRequiredAnnotationKey — field-level requiredness of a `$type` reference
// node (NIM-72). The DTO key `required` on the resolved object node is taken
// by the object-level list of the type's required CHILDREN (an array of
// names), so "this field itself is required" is expressed by a separate
// `x-required: true` annotation — the UI puts a `*` on the field without
// confusing it with the required-children list.
const typeRequiredAnnotationKey = "x-required"

// typeRefResolveDepthLimit — a safety cap on substitution depth (cycle
// detection catches the pathological case earlier; this limit is a second
// line of defense against runaway recursion).
const typeRefResolveDepthLimit = 64

// typeCatalog — raw (untyped) catalog of types: name → schema body as
// map[string]any (the shape from types.yml). The DTO side works with a
// raw-map InputSchema (the UI renders the form without server-side typing),
// so the catalog is raw too — resolving means substituting the type body for
// the reference node, annotated with x-type. Cycle detection happens during
// the resolve pass (loadTypeCatalog doesn't expand nesting, only parses).
type typeCatalog map[string]map[string]any

// loadTypeCatalog reads `<serviceRoot>/types.yml` and returns the raw type
// catalog (name → schema body). Missing file → empty catalog, no error
// (types are optional). Invalid YAML / unexpected shape → warning to the
// logger + empty catalog (like ListScenarios' partial-success: the catalog
// doesn't fail the whole listing, $type references just stay unresolved and
// the UI survives that better than a 500). Full catalog validation
// (duplicate/cycle/unknown) is done by soul-lint and the render pipeline —
// this is a best-effort projection for the UI.
func loadTypeCatalog(serviceRoot string, logger *slog.Logger) typeCatalog {
	data, err := readSnapshotFile(serviceRoot, typesCatalogFile)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			logger.Warn("artifact: types.yml skipped — read error",
				slog.Any("error", err))
		}
		return typeCatalog{}
	}

	var raw struct {
		Types map[string]map[string]any `yaml:"types"`
	}
	if err := yaml.Unmarshal(data, &raw); err != nil {
		logger.Warn("artifact: types.yml skipped — invalid YAML",
			slog.Any("error", err))
		return typeCatalog{}
	}
	if raw.Types == nil {
		return typeCatalog{}
	}
	return typeCatalog(raw.Types)
}

// resolveScenarioTypeRefs resolves `$type` references in a scenario's raw-map
// InputSchema against the type catalog: each `{$type: T}` node is replaced
// with type T's body from the catalog + an `x-type: T` annotation. The
// resolve is recursive (items/properties/additional_properties) and
// cycle-safe: re-entering a type on the current traversal branch stops
// (the node is left as-is with `$type` — the UI survives that; the full
// cycle error is raised by soul-lint/render). Returns a NEW map (the source
// isn't mutated). nil catalog / nil schema → schema as-is.
func resolveScenarioTypeRefs(schema map[string]any, catalog typeCatalog) map[string]any {
	if schema == nil {
		return nil
	}
	out := make(map[string]any, len(schema))
	for name, node := range schema {
		out[name] = resolveTypeNode(node, catalog, map[string]bool{}, 0)
	}
	return out
}

// resolveTypeNode resolves a single input node. `stack` is the set of type
// names on the current traversal branch (cycle detection). `depth` guards
// against runaway recursion.
func resolveTypeNode(node any, catalog typeCatalog, stack map[string]bool, depth int) any {
	m, ok := node.(map[string]any)
	if !ok || depth > typeRefResolveDepthLimit {
		return node
	}

	// Reference node: substitute the type body + the x-type annotation.
	if ref, isRef := stringValue(m[typeRefKey]); isRef {
		if stack[ref] {
			// Cycle — leave the node as-is (best-effort; soul-lint raises
			// input_type_cycle). Avoid infinite recursion.
			return cloneNode(m)
		}
		body, found := catalog[ref]
		if !found {
			// Unknown type — node as-is (soul-lint raises input_type_unknown).
			return cloneNode(m)
		}
		stack[ref] = true
		resolved := resolveTypeNode(cloneNode(body), catalog, stack, depth+1)
		delete(stack, ref)

		rm, _ := resolved.(map[string]any)
		if rm == nil {
			rm = map[string]any{}
		}
		// Type-name annotation for the UI + a presentational overlay of the
		// reference node on top of the type body. We don't put field-level
		// `required: <bool>` into the DTO key `required` (it's taken by the
		// object-level array of the type's required children) — instead a
		// separate x-required annotation (NIM-72): the UI puts a `*` on the
		// field itself without confusing it with the required-children list.
		// description/required_when are separate keys, safe even if the type
		// didn't set them.
		rm[typeAnnotationKey] = ref
		if rb, ok := m["required"].(bool); ok && rb {
			rm[typeRequiredAnnotationKey] = true
		}
		if d, ok := stringValue(m["description"]); ok && d != "" {
			rm["description"] = d
		}
		if _, taken := rm["required_when"]; !taken {
			if rw, ok := stringValue(m["required_when"]); ok && rw != "" {
				rm["required_when"] = rw
			}
		}
		return rm
	}

	// Regular node: recurse into items/properties/additional_properties.
	out := make(map[string]any, len(m))
	for k, v := range m {
		switch k {
		case "items":
			out[k] = resolveTypeNode(v, catalog, stack, depth+1)
		case "properties", "additional_properties":
			if pm, ok := v.(map[string]any); ok {
				resolvedProps := make(map[string]any, len(pm))
				for pn, pv := range pm {
					resolvedProps[pn] = resolveTypeNode(pv, catalog, stack, depth+1)
				}
				out[k] = resolvedProps
			} else {
				out[k] = v
			}
		default:
			out[k] = v
		}
	}
	return out
}

// stringValue — safe extraction of a string from any.
func stringValue(v any) (string, bool) {
	s, ok := v.(string)
	return s, ok
}

// cloneNode — deep copy of a raw-map node (map/slice recursively), so the
// resolve doesn't mutate the catalog and a shared type used twice doesn't
// get "corrupted" between consumers. Scalars are copied by value.
func cloneNode(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			out[k] = cloneNode(val)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, val := range x {
			out[i] = cloneNode(val)
		}
		return out
	default:
		return v
	}
}
