package artifact

import (
	"errors"
	"io/fs"
	"log/slog"

	yaml "gopkg.in/yaml.v3"

	"github.com/souls-guild/soul-stack/shared/config"
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
		// Properties written NEXT TO the reference are added on top of the type body
		// — the `state_schema` exception to ADR-062 ([NIM-740]), where a use of a
		// shared type owns the declared secret that use addresses. Add-only: a name
		// the type already declares is a load-time error the config validator
		// reports, and letting the reference win here would render a shape the engine
		// refuses. In `input:` the same node is refused outright, so nothing reaches
		// this branch from there.
		if own, ok := m["properties"].(map[string]any); ok {
			merged, _ := rm["properties"].(map[string]any)
			if merged == nil {
				merged = map[string]any{}
			}
			for pn, pv := range own {
				if _, taken := merged[pn]; taken {
					continue
				}
				merged[pn] = resolveTypeNode(pv, catalog, stack, depth+1)
			}
			rm["properties"] = merged
		}
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
		case "items", "additional_properties":
			// Both hold ONE schema, not a bag of named ones. Walking
			// additional_properties as a name→schema map resolved nothing and quietly
			// mangled the common form: `{$type: T}` iterated as the single pair
			// `"$type" → "T"`, so the reference reached the UI unresolved — which is
			// the failure this resolve exists to prevent.
			out[k] = resolveTypeNode(v, catalog, stack, depth+1)
		case "properties":
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

// stripFormSecrets removes every declared secret (`type: secret`) from a
// scenario's resolved input schema before it is projected as the operator FORM
// ([ADR-0086] §5, NIM-751). The platform mints such a value; asking a human for it
// is the one thing the declaration means it must never do.
//
// It runs AFTER [resolveScenarioTypeRefs] and only on the `input:` path, and both
// halves of that are load-bearing:
//
//   - after, because the secret arrives THROUGH `$type` — a shared type carrying
//     one is legal ([ADR-0086] §5) and a `type: secret` written directly in
//     `input:` is refused at load (`input_type_invalid`). Before the substitution
//     there is nothing to strip;
//   - only on `input:`, because `rawStateSchema` (state_schema.go) projects through
//     the SAME resolver and must keep its declared secrets — that projection is the
//     state contract, where a declared secret is the point.
//
// This is the raw-map twin of the typed rule in shared/config/input_secret_type.go
// ([config.InputSchema.NotAskedOfOperator]). Two walks, because the form travels as
// `map[string]any` re-emitted from YAML on a code path that never builds a
// [config.InputSchema] — the split predates this and is why the strip cannot live in
// one place. They have already drifted apart twice, so they are pinned against each
// other by TestStripFormSecrets_AgreesWithTypedPredicate rather than by this comment.
//
// What survives is decided by EMPTINESS, not by contagion: see [stripFormSecretNode],
// which is where that rule and its three positions are written out.
func stripFormSecrets(schema map[string]any) map[string]any {
	if schema == nil {
		return nil
	}
	out := make(map[string]any, len(schema))
	for name, node := range schema {
		if kept, keep := stripFormSecretNode(node); keep {
			out[name] = kept
		}
	}
	return out
}

// stripFormSecretNode returns the node without its declared secrets, and whether
// the node survives at all. A non-map node (a malformed schema the best-effort
// projection tolerates) is returned untouched — this walk removes, it does not
// judge.
//
// **The survival rule is emptiness, not contagion.** A container goes only when the
// strip left it with nothing an operator could fill AND it described something
// before: an object carrying one minted secret among ordinary properties keeps its
// place and loses that property, while an object whose every property is minted is a
// widget with no inputs and goes. The three positions differ, and each for its own
// reason:
//
//   - `items:` is an array's WHOLE content. A minted element schema leaves no
//     element to author, so the array goes with it.
//   - `additional_properties:` is one of two ways an object describes content
//     ([ADR-0086] §14). A minted one loses the KEY, so the form stops offering the
//     open half, and the object survives on its `properties:` if it has any. Dropping
//     the object outright took its fillable properties with it, which is the
//     over-reach this rule replaced. ⚠ Losing the key is a FORM fact: the value gate
//     never checked undescribed keys in depth and still does not — what it refuses
//     under a dropped open map is the minted VALUE, not the arbitrary key.
//   - `properties:` is stripped member by member.
//
// A `properties: {}` the author wrote is not "emptied by the strip" and does not
// trigger the rule — hence `hadContent`, which counts only content that was there to
// lose. `additional_properties: false` is not content either: it forbids keys rather
// than describing them, and rides through untouched.
func stripFormSecretNode(node any) (any, bool) {
	m, ok := node.(map[string]any)
	if !ok {
		return node, true
	}
	if t, isStr := stringValue(m["type"]); isStr && t == config.SecretTypeName {
		return nil, false
	}

	out := make(map[string]any, len(m))
	var hadContent, hasContent bool
	for k, v := range m {
		switch k {
		case "items":
			kept, keep := stripFormSecretNode(v)
			if !keep {
				return nil, false
			}
			out[k] = kept
		case "additional_properties":
			sub, isMap := v.(map[string]any)
			if !isMap {
				out[k] = v
				continue
			}
			hadContent = true
			kept, keep := stripFormSecretNode(sub)
			if !keep {
				continue
			}
			out[k] = kept
			hasContent = true
		case "properties":
			pm, isMap := v.(map[string]any)
			if !isMap {
				out[k] = v
				continue
			}
			if len(pm) > 0 {
				hadContent = true
			}
			kept := stripFormSecrets(pm)
			out[k] = kept
			if len(kept) > 0 {
				hasContent = true
			}
		default:
			out[k] = v
		}
	}
	if hadContent && !hasContent {
		return nil, false
	}
	return out, true
}

// dropStrippedFormFields removes from the `form:` projection every field naming an
// input parameter [stripFormSecrets] just took away, and every section that had
// fields and has none left ([ADR-0086] §5, NIM-751).
//
// It compares the two schemas rather than filtering against the surviving one,
// deliberately. A `form:` field naming a parameter that never existed is an AUTHOR
// error, reported by soul-lint as `form_field_unknown`
// (shared/config/form_layout.go); silently hiding it here would take that error's
// only visible symptom away. Only names this strip removed are dropped.
//
// A section emptied by the drop goes with it: a heading with no fields under it is
// a hole in the form where the UI expects a group. A section that was already empty
// is left exactly as authored — this function removes, it does not tidy.
func dropStrippedFormFields(form *ScenarioForm, before, after map[string]any) *ScenarioForm {
	if form == nil || len(form.Sections) == 0 {
		return form
	}
	stripped := make(map[string]bool)
	for name := range before {
		if _, kept := after[name]; !kept {
			stripped[name] = true
		}
	}
	if len(stripped) == 0 {
		return form
	}

	out := &ScenarioForm{Sections: make([]ScenarioFormSection, 0, len(form.Sections))}
	for _, sec := range form.Sections {
		kept := make([]ScenarioFormField, 0, len(sec.Fields))
		for _, f := range sec.Fields {
			if !stripped[f.Name] {
				kept = append(kept, f)
			}
		}
		if len(sec.Fields) > 0 && len(kept) == 0 {
			continue
		}
		sec.Fields = kept
		out.Sections = append(out.Sections, sec)
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
