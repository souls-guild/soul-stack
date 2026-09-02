package validate

// Offline validation of reusable named types (`service/<name>/types.yml`
// + `$type` references in a scenario's input:). Goal — catch these BEFORE
// the keeper:
//   - a broken type catalog (input_type_duplicate / input_type_cycle between
//     types);
//   - a reference from a scenario's input: to a nonexistent type
//     (input_type_unknown);
//   - a cyclic reference visible from the consumer's side (input_type_cycle).
//
// Structural invariants of a SINGLE node ($type-ref-conflict, an invalid
// name) are already caught by the config validator at scenario parse time —
// this only adds what requires the type CATALOG (which the scenario's
// config parser alone can't see).
//
// The catalog comes from the sibling `../../types.yml` relative to main.yml:
// service layout is `<service>/scenario/<name>/main.yml`, types.yml sits at
// the service root, `<service>/types.yml`. A missing types.yml → the check
// is skipped (types are optional; a type reference with no catalog still
// yields input_type_unknown when resolved against an empty catalog). Any
// catalog I/O/parse error doesn't fail scenario lint catastrophically — it's
// surfaced as the catalog's own diagnostic.

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/diag"
)

// typeRefDiagnostics checks a scenario's input: `$type` references against
// the service's type catalog. scenarioPath is the path to main.yml; m is the
// parsed scenario (m==nil → nil: nothing to resolve without input). Returns
// catalog diagnostics (duplicate/cycle between types) + input: resolve
// diagnostics (unknown/cycle).
func typeRefDiagnostics(scenarioPath string, m *config.ScenarioManifest) []diag.Diagnostic {
	if m == nil || len(m.Input) == 0 {
		return nil
	}
	if !inputHasTypeRef(m.Input) {
		// The scenario doesn't use $type — no need to read the catalog (a
		// service with no types may legitimately lack types.yml).
		return nil
	}

	// Layout: <service>/scenario/<name>/main.yml → <service>/types.yml.
	serviceRoot := filepath.Dir(filepath.Dir(filepath.Dir(scenarioPath)))
	typesPath := filepath.Join(serviceRoot, config.TypesCatalogFile)

	data, err := os.ReadFile(typesPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// No catalog, but the scenario references a type → resolving against
			// an empty catalog will yield input_type_unknown (below), pointing at
			// the specific broken reference.
			data = nil
		} else {
			return []diag.Diagnostic{{
				Level:   diag.LevelWarning,
				Phase:   diag.PhaseParse,
				File:    typesPath,
				Code:    "io_error",
				Message: err.Error(),
				Hint:    "types.yml is present but unreadable - $type references not checked offline",
			}}
		}
	}

	catalog, catDiags := config.ParseTypeCatalog(typesPath, data)
	out := catDiags

	// Resolve the scenario's input: against the catalog: catches
	// input_type_unknown (a reference to a missing type) and input_type_cycle
	// visible from the consumer's side. The File field is set to the scenario
	// path (the diagnostic is about its input:).
	_, refDiags := config.ResolveTypeRefs(m.Input, catalog)
	for i := range refDiags {
		if refDiags[i].File == "" {
			refDiags[i].File = scenarioPath
		}
	}
	out = append(out, refDiags...)
	return out
}

// stateSchemaTypeRefDiags resolves the `$type` references of a service manifest's
// `state_schema:` against the sibling `types.yml`, IN PLACE on svc, and returns the
// catalog's and the resolve's diagnostics.
//
// It is the state-side twin of [typeRefDiagnostics], and it does more than report:
// the resolved schema is what [scenarioSecretKeyDiags] and every other consumer of
// svc.StateSchema then reads. An unresolved `{$type: AclUser}` node declares no
// shape, so a declared secret inside a referenced type would come back as "no
// secrets here" — the linter would go quiet on exactly the file it exists to check.
// The keeper does the same thing at the same point (artifact.parseManifest); this is
// the offline half.
//
// servicePath is the path to service.yml; the catalog is its sibling. A missing
// types.yml is not an error — resolving against an empty catalog still points
// input_type_unknown at the broken reference.
func stateSchemaTypeRefDiags(servicePath string, svc *config.ServiceManifest) []diag.Diagnostic {
	if svc == nil || !config.SchemaHasTypeRef(svc.StateSchema) {
		return nil
	}
	typesPath := filepath.Join(filepath.Dir(servicePath), config.TypesCatalogFile)

	data, err := os.ReadFile(typesPath)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return []diag.Diagnostic{{
				Level:   diag.LevelWarning,
				Phase:   diag.PhaseParse,
				File:    typesPath,
				Code:    "io_error",
				Message: err.Error(),
				Hint:    "types.yml is present but unreadable - the $type references in state_schema are not checked offline",
			}}
		}
		data = nil
	}

	catalog, out := config.ParseTypeCatalog(typesPath, data)
	resolved, refDiags := config.ResolveStateSchemaTypeRefs(svc.StateSchema, catalog)
	// The CATALOG's own errors gate the substitution too, not just the resolve's: a
	// types.yml that declares one name twice still resolves — to the last body that
	// won the collision — and judging a secret's `key:` against the wrong element
	// would report a second, invented mistake on top of the real one. Same gate the
	// keeper's loader applies (artifact.resolveManifestStateSchemaTypeRefs).
	if !diag.HasErrors(out) && !diag.HasErrors(refDiags) {
		// A declared secret reaching through `$type` is judged only now: at parse the
		// element shape was still a reference, so its `key:` had no sibling to name.
		refDiags = append(refDiags, config.ValidateStateSchemaSecrets(svc.StateSchema, resolved)...)
		svc.StateSchema = resolved
	}
	for i := range refDiags {
		if refDiags[i].File == "" {
			refDiags[i].File = servicePath
		}
	}
	return append(out, refDiags...)
}

// inputHasTypeRef reports true if at least one input: node (recursively
// through items/properties/additional_properties) carries a `$type`
// reference. A cheap short-circuit: no references means no need to read the
// catalog.
func inputHasTypeRef(m config.InputSchemaMap) bool {
	return config.SchemaHasTypeRef(m)
}
