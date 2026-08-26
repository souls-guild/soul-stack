package config

// Covenant is a reusable fragment of the shared contract for scenario sections
// (`covenant.yml` at the service-repo root). A scenario pulls it in via
// `extends: <covenant-name>` (ScenarioManifest.Extends) and inherits the MINIMUM
// input/compute/validate sections; the scenario's own sections are ADDED on top
// (add-only merge, mergeSections).
//
// Layer boundary (S1 — this file): the config level provides the types
// (ScenarioFragment), the merge operation (mergeSections) and FORM validation of
// the fragment/extends. Resolving the fragment against the snapshot file system
// (reading covenant.yml by the name from extends) is keeper-side
// (S2, LoadScenarioManifestResolved): it decodes the fragment via
// [LoadCovenantFragmentFromBytes] and merges it into the manifest via
// [MergeCovenant].

import (
	"fmt"
	"os"
	"regexp"

	"github.com/goccy/go-yaml/ast"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// ScenarioFragment is a typed covenant.yml. It carries ONLY the shared contract
// of three sections; a scenario's identity/tasks/form/inheritance do NOT belong
// to the fragment (a covenant is a contract, not a standalone scenario):
//
//   - name/tasks/create — the fragment has none (it is not an executable scenario);
//   - form — the presentation layer stays strictly local (mergeSections does not
//     touch it): one covenant serves many scenarios with different forms;
//   - extends — covenant→covenant recursion is forbidden (a covenant has no
//     extends), else the chain would need resolving and cycle-breaking;
//   - state_changes — removed by [ADR-0084]: a state field is now written by a
//     `core.state.<verb>` task, and tasks are exactly what the fragment does not
//     carry. A shared capture sequence is shared the way any other task sequence
//     is, with `include:`.
//
// Any of the listed keys in covenant.yml → covenant_unexpected_key
// (validateCovenantFragment). Decode uses the same UnmarshalYAML as the
// like-named ScenarioManifest sections (Input/Compute reuse their decoders).
type ScenarioFragment struct {
	Input    InputSchemaMap `yaml:"input,omitempty"`
	Compute  ComputeBlock   `yaml:"compute,omitempty"`
	Validate []ValidateRule `yaml:"validate,omitempty"`
}

// reCovenantName — the covenant-fragment name in `extends:` (also the path of the
// `covenant.yml` family relative to the service root, extension stripped). Built
// from [refPathSegment], with at most ONE subdirectory level: `covenant`,
// `scenario_create`, `shared/scenario_create`. By construction it excludes `.`,
// `..` and absolute paths — the traversal clamp comes from the name grammar, not
// a post-hoc filepath check (the name CANNOT express escaping the service root);
// readCovenantFile securejoins on top of that.
var reCovenantName = regexp.MustCompile(`^(?:` + refPathSegment + `/)?` + refPathSegment + `$`)

// ValidExtendsName reports whether a name in `extends:` is valid as a covenant
// reference (form + traversal clamp). Empty string → false (that is "no
// inheritance", not a valid name — the caller distinguishes emptiness before the
// check). The single source of truth for covenant-name form, for the config
// validator (semantic layer) and the keeper-side resolver (S2): the resolver
// builds the path to covenant.yml ONLY for a name that passed this check.
func ValidExtendsName(name string) bool {
	return reCovenantName.MatchString(name)
}

// covenantFragmentKnownKeys is the closed set of top-level covenant.yml keys.
// Keys that belong to a scenario but NOT to the fragment (name/tasks/create/form/
// extends/description/vars) are absent here — their presence yields
// covenant_unexpected_key (the fragment carries only the three-section contract).
// `state_changes` is absent too, and deliberately falls into the same bucket
// ([ADR-0084] removed it from the grammar entirely).
var covenantFragmentKnownKeys = map[string]bool{
	"input":    true,
	"compute":  true,
	"validate": true,
}

// validateCovenantFragment is the schema-time FORM check for covenant.yml on top
// of already-validated sections (input/compute/validate structure is validated by
// their own validators in schemaValidateCovenant). Here — the
// covenant-specific invariant: an extra key that belongs to a scenario, not the
// fragment → covenant_unexpected_key (fail-closed). In particular `extends:`
// inside a covenant → covenant_unexpected_key: covenant→covenant recursion is
// outside the grammar.
func validateCovenantFragment(root *ast.MappingNode) []diag.Diagnostic {
	if root == nil {
		return nil
	}
	var out []diag.Diagnostic
	for _, kv := range root.Values {
		tok := kv.Key.GetToken()
		if tok == nil {
			continue
		}
		key := tok.Value
		if covenantFragmentKnownKeys[key] {
			continue
		}
		out = append(out, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "covenant_unexpected_key",
			Message:  fmt.Sprintf("covenant.yml carries only the shared section contract; unexpected field %q", key),
			Hint:     "covenant.yml allows only input / compute / validate — name/tasks/create/form/extends belong to the scenario, not the fragment",
			YAMLPath: "$." + key,
		}))
	}
	return out
}

// MergeCovenant merges a covenant fragment into a scenario manifest ADD-ONLY: the
// fragment is the BASE (minimum), the scenario ADDS the delta. Shallow by the
// section's top key; a key present in both (one input-field name, one compute
// name) → error (fail-closed, NOT last-wins/override — this guards against
// silently overriding the shared contract). Append order is covenant-FIRST
// (compute/validate): the shared contract logically precedes the scenario delta.
//
// `form` is NOT touched (stays local — the fragment does not carry it). The
// keeper-side resolver (S2) calls this AFTER both sides have separately passed
// schema validation: errors here are only key conflicts, not structural.
//
// local must be non-nil (the resolver calls it on an already-decoded manifest).
// Returns the first conflict error found (deterministically by section order
// input → compute → validate, within a section by the fragment's key order), so
// the operator fixes conflicts one at a time.
func MergeCovenant(fragment ScenarioFragment, local *ScenarioManifest) error {
	if err := mergeInputSections(fragment.Input, local); err != nil {
		return err
	}
	if err := mergeComputeSections(fragment.Compute, local); err != nil {
		return err
	}
	mergeValidateSections(fragment.Validate, local)
	return nil
}

// mergeInputSections is a shallow input union by field name. A name present in
// both → section_key_conflict (NOT last-wins: the covenant sets a shared field,
// the scenario may not silently override its schema). Fragment fields absent from
// the scenario are added as-is (covenant is the minimum).
func mergeInputSections(fragment InputSchemaMap, local *ScenarioManifest) error {
	if len(fragment) == 0 {
		return nil
	}
	if local.Input == nil {
		local.Input = make(InputSchemaMap, len(fragment))
	}
	for name, schema := range fragment {
		if _, dup := local.Input[name]; dup {
			return &SectionKeyConflict{Section: "input", Key: name}
		}
		local.Input[name] = schema
	}
	return nil
}

// mergeComputeSections appends covenant-FIRST: result = fragment ++ local, order
// within each side preserved (compute[i] references an earlier-declared
// compute[j], j<i — covenant declarations become available to the scenario
// delta). A duplicate compute name → section_key_conflict.
func mergeComputeSections(fragment ComputeBlock, local *ScenarioManifest) error {
	if len(fragment) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(fragment)+len(local.Compute))
	for _, cv := range fragment {
		seen[cv.Name] = true
	}
	for _, cv := range local.Compute {
		if seen[cv.Name] {
			return &SectionKeyConflict{Section: "compute", Key: cv.Name}
		}
	}
	merged := make(ComputeBlock, 0, len(fragment)+len(local.Compute))
	merged = append(merged, fragment...)
	merged = append(merged, local.Compute...)
	local.Compute = merged
	return nil
}

// mergeValidateSections appends covenant-FIRST. Rules ACCUMULATE (no
// duplicate-detection): two rules may match textually and that is not an error —
// validate is a conjunction of invariants, a redundant rule just rechecks the
// same precondition. covenant invariants are evaluated first.
func mergeValidateSections(fragment []ValidateRule, local *ScenarioManifest) {
	if len(fragment) == 0 {
		return
	}
	merged := make([]ValidateRule, 0, len(fragment)+len(local.Validate))
	merged = append(merged, fragment...)
	merged = append(merged, local.Validate...)
	local.Validate = merged
}

// schemaValidateCovenant runs post-decode checks on covenant.yml (ScenarioFragment).
// First the covenant-specific form invariant (validateCovenantFragment: only the
// 3 sections, a foreign key → covenant_unexpected_key), then the STRUCTURE of each
// present section via the same validators as the like-named scenario sections
// (the covenant carries the same DSL — reused, not duplicated).
func schemaValidateCovenant(_ string, root *ast.MappingNode, m *ScenarioFragment) []diag.Diagnostic {
	out := validateCovenantFragment(root)

	topKeys := topLevelKeys(root)
	if topKeys["input"] {
		out = append(out, validateInputSchemaMap(m.Input, findInputMapping(root, "input"), "$.input")...)
	}
	if topKeys["compute"] {
		out = append(out, validateComputeBlock(root, "$.compute")...)
	}
	if topKeys["validate"] {
		out = append(out, validateValidateBlock(root, "$.validate")...)
	}
	return out
}

// LoadCovenantFragment is the entry point with I/O: reads `covenant.yml` at the
// path and decodes+validates the fragment. Contract identical to
// LoadScenarioManifest. The keeper-side resolver (S2) builds the path to
// covenant.yml from the extends name (via ValidExtendsName) and calls this.
func LoadCovenantFragment(path string, opts ValidateOptions) (*ScenarioFragment, *Document, []diag.Diagnostic, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, []diag.Diagnostic{{
			Level: diag.LevelError, Phase: diag.PhaseParse,
			File: path, Code: "io_error", Message: err.Error(),
		}}, err
	}
	frag, doc, diags := LoadCovenantFragmentFromBytes(path, src, opts)
	return frag, doc, diags, nil
}

// LoadCovenantFragmentFromBytes is the main entry point without I/O (for tests
// and keeper-side resolution from an in-memory snapshot). Decode + covenant form
// validation. The covenant semantic phase is empty (form is checked in the schema
// phase via schemaValidateCovenant); covenantNoSemantic keeps the
// parseAndValidate signature.
func LoadCovenantFragmentFromBytes(filename string, data []byte, opts ValidateOptions) (*ScenarioFragment, *Document, []diag.Diagnostic) {
	cfg := &ScenarioFragment{}
	doc, diags := parseAndValidate(filename, stripBOM(data), cfg, opts, covenantNoSemantic)
	return cfg, doc, diags
}

// covenantNoSemantic is the empty covenant semantic phase (all covenant
// validation is schema-time). Parallels semanticValidateScenario, but the
// fragment has nothing to check cross-field (no tasks/register graph; form is in
// the schema phase).
func covenantNoSemantic(_ *ScenarioFragment, _ *ast.MappingNode) []diag.Diagnostic {
	return nil
}

// SectionKeyConflict is an add-only merge conflict: a section key is declared in
// both the covenant and the scenario. Carries the section (input/compute) and key
// (field name / compute name) for targeted diagnostics.
type SectionKeyConflict struct {
	Section string
	Key     string
}

// Code is the machine diagnostic code (for mapping into the S2 diag/HTTP layer).
func (e *SectionKeyConflict) Code() string { return "section_key_conflict" }

func (e *SectionKeyConflict) Error() string {
	return fmt.Sprintf("section_key_conflict: %s.%s declared in both covenant and scenario (add-only merge forbids override)", e.Section, e.Key)
}
