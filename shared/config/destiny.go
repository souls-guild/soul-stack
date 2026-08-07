package config

import (
	"fmt"
	"regexp"

	"github.com/goccy/go-yaml/ast"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// DestinyManifest is the typed representation of `destiny.yml` per the normative
// spec [`docs/destiny/manifest.md`].
//
// The manifest holds only the declaration (name, description, input/output
// contract, custom-module list). The task list lives in `tasks/main.yml`,
// `vars:` in `vars.yml`; the manifest has neither section (see deprecated keys
// below).
type DestinyManifest struct {
	Name        string         `yaml:"name"`
	Description string         `yaml:"description,omitempty"`
	Input       InputSchemaMap `yaml:"input,omitempty"`
	Output      InputSchemaMap `yaml:"output,omitempty"`

	// Validate — declarative invariants over the destiny's own `input:`, the same
	// `[{that, message}]` grammar and input-only CEL sandbox as scenario
	// (ADR-009 amendment 2026-07-26, NIM-167). Enforced by the render pass when
	// the parent apply: task hands over its input, so a scenario passing a
	// contract-violating value fails the render instead of reaching the hosts.
	// `assert:` tasks stay for topology/register checks — validate: complements
	// them, it does not replace them.
	Validate []ValidateRule `yaml:"validate,omitempty"`

	RequiredModules []string `yaml:"required_modules,omitempty"`

	// Compat — optional engine-compatibility window (ADR-0076). A destiny is a
	// separate git artifact pinned at its own ref (ADR-007), so it states its own
	// tested keeper range; the run's effective window is the intersection with
	// the service's. A missing block (nil) = unbounded. Read via the nil-safe
	// [CompatConfig.KeeperWindow].
	Compat *CompatConfig `yaml:"compat,omitempty"`
}

// reDestinyName — canonical kebab-case destiny name (`redis`, `cert-rotation`).
// Dash only between alphanumerics; no leading/trailing/double-dash. Matches the
// `destiny-<name>/` folder name without the prefix.
var reDestinyName = regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)*$`)

// reRequiredModule — two-level `<namespace>.<module>` form for custom modules in
// `required_modules:`. Core modules are not listed. Kebab-case, no underscore
// (naming-rules.md §57/§186). Single source of truth with
// `reDependencyModuleName` (service.go) — a duplicate regex was a drift source.
var reRequiredModule = reDependencyModuleName

// deprecatedDestinyKeys — obsolete top-level keys for which we emit an explicit
// hint instead of a generic "unknown_key". See docs/destiny/manifest.md →
// "What destiny.yml does NOT hold".
var deprecatedDestinyKeys = map[string]string{
	"tasks":     "task list moved to tasks/main.yml per ADR-009; destiny.yml is manifest-only",
	"steps":     "task list moved to tasks/main.yml per ADR-009; destiny.yml is manifest-only",
	"vars":      "destiny-locals moved to vars.yml (sibling of destiny.yml); see docs/destiny/vars.md",
	"version":   "version is a git ref under which destiny is committed, not a manifest field; see ADR-007",
	"templates": "templates/ is a folder, not a manifest field; .tmpl files are picked up by convention",
	"tests":     "tests/ is a folder, not a manifest field; molecule-style tests are picked up by convention",
}

// schemaValidateDestiny runs post-decode checks on a DestinyManifest.
func schemaValidateDestiny(path string, root *ast.MappingNode, m *DestinyManifest) []diag.Diagnostic {
	_ = path
	var out []diag.Diagnostic

	topKeys := topLevelKeys(root)

	// 1) deprecated keys — special hint, raised via the AST to get line/col.
	for _, kv := range root.Values {
		tok := kv.Key.GetToken()
		if tok == nil {
			continue
		}
		hint, dep := deprecatedDestinyKeys[tok.Value]
		if !dep {
			continue
		}
		out = append(out, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "unknown_key",
			Message:  `unknown field "` + tok.Value + `"`,
			Hint:     hint,
			YAMLPath: "$." + tok.Value,
		}))
	}

	// 2) `name:` required + format.
	// The `topKeys["name"]` branch distinguishes "key absent" from "key present
	// but empty/null/invalid". An empty string and a `null` value are type-valid
	// for type=string but violate the destiny-name format — we raise
	// `name_invalid_format`, not `missing_required_field`, so the operator does
	// not hunt for a missing key.
	if !topKeys["name"] {
		out = append(out, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "missing_required_field",
			Message:  "name is required at top-level",
			Hint:     "set name: <kebab-case>, matching destiny-<name>/ folder",
			YAMLPath: "$.name",
		})
	} else if !reDestinyName.MatchString(m.Name) {
		msg := fmt.Sprintf("name %q does not match %s", m.Name, reDestinyName)
		if m.Name == "" {
			msg = "name must be non-empty kebab-case string"
		}
		out = append(out, atPath(root, "$.name", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "name_invalid_format",
			Message: msg,
			Hint:    "kebab-case: lowercase letters, digits, dashes; must start with letter",
		}))
	}

	// 3) required_modules — two-level `<namespace>.<module>` form, and level 1 must
	// not be a reserved name. The reserved check runs FIRST because `core.haproxy`
	// is regex-valid: reporting it as a format error would send the author looking
	// for a typo in a string that is spelled exactly as they meant it.
	for i, mod := range m.RequiredModules {
		yamlPath := fmt.Sprintf("$.required_modules[%d]", i)
		switch {
		case reservedModuleAddr(mod):
			out = append(out, reservedModuleDiag(root, yamlPath, fmt.Sprintf("required_modules[%d]", i), mod))
		case !reRequiredModule.MatchString(mod):
			out = append(out, atPath(root, yamlPath, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:    "required_module_invalid_format",
				Message: fmt.Sprintf("required_modules[%d] = %q does not match <namespace>.<module>", i, mod),
				Hint:    "two-level address per architecture.md -> \"Module addressing\"; core-modules are not listed here",
			}))
		}
	}

	// 4) input: / output: — recursive validation via the shared validator.
	if topKeys["input"] {
		out = append(out, validateInputSchemaMap(m.Input, findInputMapping(root, "input"), "$.input")...)
	}
	if topKeys["output"] {
		out = append(out, validateInputSchemaMap(m.Output, findInputMapping(root, "output"), "$.output")...)
	}

	// 4a) `validate:` — top-level input invariants, shared validator with
	// scenario/covenant (only when the key is present).
	if topKeys["validate"] {
		out = append(out, validateValidateBlock(root, "$.validate")...)
	}

	// 5) compat: — optional engine-compatibility window (ADR-0076). Same grammar
	// as service.yml; a nil block is valid (unbounded).
	out = append(out, validateCompat(root, m.Compat)...)

	return out
}

// semanticValidateDestiny has no dedicated semantic invariants at M1.2.a. The
// signature is kept for symmetry with Keeper/Soul in case cross-field checks are
// added (e.g. a default CEL/template expression checked against input-type at
// M1.3+).
func semanticValidateDestiny(_ *DestinyManifest, _ *ast.MappingNode) []diag.Diagnostic {
	return nil
}
