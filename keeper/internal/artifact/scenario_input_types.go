package artifact

import (
	"errors"
	"io/fs"

	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/diag"
)

// LoadScenarioManifestResolved is keeper's single RUNTIME entry point for
// parsing `scenario/<name>/main.yml` from a service snapshot. It does the same
// as [config.LoadScenarioManifestFromBytes], and additionally resolves the
// input schema's `$type` references against the service's type catalog
// (`<service>/types.yml`, a sibling of service.yml). After the resolve,
// `scn.Input` carries a self-contained schema with no `$type` left — the type
// shape (object/array/properties/required) is substituted in place of the
// reference.
//
// Why here, not at each consumer: `$type` resolution is needed EVERYWHERE
// scn.Input is consumed at runtime — submitted-input value validation
// ([scenario.ValidateInput] → config.ResolveInputValues), form-prefill,
// secret-schema, the render pipeline. Without the resolve, a `{$type: T}` node
// has an empty `Type` → config.ResolveInputValues skips it WITHOUT form
// validation (a submitted "non-object" was silently accepted in an
// object-typed field). A single chokepoint at load time (instead of a
// per-consumer duplicate resolve) guarantees enforcement is present on every
// path — this is the "resolve on service-load" contract (a parallel to
// ResolveTypeRefs in soul-lint).
//
// art carries the snapshot's LocalDir — types.yml is read through the
// securejoin reader. rel is the relative path of main.yml (the label for File
// diagnostics / parser messages).
//
// The return contract is identical to config.LoadScenarioManifestFromBytes:
// error only on an unparseable input; diags carry all validation errors,
// INCLUDING input_type_unknown / input_type_cycle from the reference resolve
// (the consumer checks diag.HasErrors as before). A service without
// types.yml / a scenario without `$type` → the schema passes through
// unchanged (back-compat).
// modules is the plugin-manifest resolver for this parse (NIM-228), or nil when
// the caller has none — see [PluginManifestSource]. Nil is not a silent pass:
// every plugin module in the definition then reports `plugin_params_unchecked`.
func LoadScenarioManifestResolved(art *ServiceArtifact, rel string, data []byte, modules config.ModuleManifestResolver) (*config.ScenarioManifest, *config.Document, []diag.Diagnostic, error) {
	scn, doc, diags, err := config.LoadScenarioManifestFromBytes(rel, data, config.ValidateOptions{ModuleManifests: modules})
	if err != nil {
		return scn, doc, diags, err
	}
	if scn == nil {
		return scn, doc, diags, nil
	}

	// covenant-merge comes FIRST: config.ResolveScenarioCovenant merges covenant.yml
	// (by scn.Extends, read via the securejoin reader from art.LocalDir) into this
	// FRESH manifest in place, AND validates the form post-merge on the merged
	// input. The merged input (covenant fields included) must go through the
	// $type resolve below — that's why covenant comes before it, not after. The
	// fresh fragment is loaded inside the resolver on every call: one
	// covenant.yml is resolved independently across different scenarios,
	// cross-scenario aliasing is impossible (a read-only fragment contract). One
	// shared resolver in shared/config: keeper-runtime, trial, and soul-lint all
	// call the same one.
	diags = append(diags, config.ResolveScenarioCovenant(scn, doc, art.LocalDir)...)

	// The own-namespace fence ([ADR-0083] §7). Here because this is the one place a
	// scenario is parsed with its service manifest in scope — the fence needs the
	// service NAME, which the scenario file never states. It runs before the
	// early return below: a scenario with no `input:` is fenced too.
	//
	// This sees the main file only; an `include:` body is parsed later, inside
	// config.ExpandIncludes. The scan over the expanded list runs at
	// render.Pipeline.Render, which no dispatch path can bypass.
	if art.Manifest != nil {
		diags = append(diags, config.ScanOwnNamespaceVault(rel, art.Manifest.Name, scn, scn.Tasks)...)
	}

	if len(scn.Input) == 0 {
		return scn, doc, diags, nil
	}
	resolved, rdiags := resolveScenarioInputTypeRefs(art, scn.Input, rel)
	if resolved != nil {
		scn.Input = resolved
	}
	diags = append(diags, rdiags...)
	return scn, doc, diags, nil
}

// resolveScenarioInputTypeRefs resolves the `$type` references of input schema
// `in` against the service's type catalog (`<art.LocalDir>/types.yml`). Returns
// a NEW schema map with the types substituted in, plus resolve diagnostics
// (input_type_unknown / input_type_cycle / the catalog's own errors).
//
// types.yml absent → an empty catalog: a schema without `$type` passes through
// as-is (types are optional), while a type reference against an empty catalog
// yields input_type_unknown (pointing at the broken reference, symmetric with
// soul-lint). Any other catalog read I/O error is diag.LevelError (a broken
// snapshot must not silently skip the type shape).
func resolveScenarioInputTypeRefs(art *ServiceArtifact, in config.InputSchemaMap, scenarioPath string) (config.InputSchemaMap, []diag.Diagnostic) {
	data, err := readSnapshotFile(art.LocalDir, config.TypesCatalogFile)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return nil, []diag.Diagnostic{{
				Level: diag.LevelError, Phase: diag.PhaseParse,
				File: config.TypesCatalogFile, Code: "io_error",
				Message: err.Error(),
				Hint:    "types.yml exists but cannot be read — $type references will not resolve",
			}}
		}
		// No catalog: a type reference still yields input_type_unknown via the
		// empty-catalog resolve below (it points at the specific broken reference).
		data = nil
	}

	catalog, catDiags := config.ParseTypeCatalog(config.TypesCatalogFile, data)
	resolved, refDiags := config.ResolveTypeRefs(in, catalog)
	for i := range refDiags {
		if refDiags[i].File == "" {
			refDiags[i].File = scenarioPath
		}
	}
	return resolved, append(catDiags, refDiags...)
}
