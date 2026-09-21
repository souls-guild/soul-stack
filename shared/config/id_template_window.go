package config

// The `name_template:` → `id_template:` compatibility window ([ADR-0085] §"CEL
// reach and the compatibility window", NIM-730).
//
// The identifier of a registry entity is spelled `id` everywhere else since
// NIM-729; the create-scenario key that composes one was the last place still
// saying `name`. Renaming it outright would break every service repository at
// once — a scenario is where the key is actually written, and those repositories
// are invisible from here — so both spellings load for a window and the old one
// warns with a line and a replacement. Removing [ScenarioManifest.LegacyNameTemplate]
// closes the window and is its own ticket.
//
// The fold happens ONCE, at the top of schemaValidateScenario, before any rule
// reads the template. Everything downstream — the schema check here, the
// post-merge covenant check, the keeper's create path, the live preview — sees
// [ScenarioManifest.IDTemplate] and never learns which spelling the file used.
//
// [ADR-0085]: ../../docs/adr/0085-entity-id-and-label.md

import (
	"github.com/goccy/go-yaml/ast"

	"github.com/souls-guild/soul-stack/shared/diag"
)

const (
	// idTemplateKey — the top-level scenario key composing the incarnation id.
	idTemplateKey = "id_template"
	// nameTemplateKey — its pre-[ADR-0085] spelling, accepted for the window.
	nameTemplateKey = "name_template"
)

// writtenIDTemplateKey names the spelling the file actually used, so a
// diagnostic about the template points at a key the author can find in their own
// file. `id_template` wins when both are present: that case is already an error,
// and the canonical key is the one to keep.
func (m *ScenarioManifest) writtenIDTemplateKey(topKeys map[string]bool) string {
	if topKeys[idTemplateKey] {
		return idTemplateKey
	}
	if topKeys[nameTemplateKey] {
		return nameTemplateKey
	}
	return idTemplateKey
}

// normalizeIDTemplate folds the legacy spelling into [ScenarioManifest.IDTemplate]
// and reports the window's own two diagnostics:
//
//   - both keys in one file → ERROR `id_template_conflict`. Not merged and not
//     resolved by precedence: two templates composing one id is an authoring
//     mistake whichever value the loader picked, and picking one silently would
//     make the run disagree with half the file.
//   - the legacy key alone → WARNING `id_template_legacy_spelling`, carrying the
//     line and the replacement. This warning is the whole static half of the
//     window: nothing else tells a service author their file is on the old
//     spelling before the key stops being read.
//
// Both diagnostics address the key the file wrote, never the canonical one.
func (m *ScenarioManifest) normalizeIDTemplate(root *ast.MappingNode) []diag.Diagnostic {
	topKeys := topLevelKeys(root)
	hasNew, hasOld := topKeys[idTemplateKey], topKeys[nameTemplateKey]

	if hasNew && hasOld {
		return []diag.Diagnostic{atPath(root, "$."+nameTemplateKey, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code: "id_template_conflict",
			Message: "the scenario declares both id_template: and name_template: — " +
				nameTemplateKey + " is the old spelling of the same key, not a second template",
			Hint: "delete name_template: and keep id_template:",
		})}
	}
	if !hasOld {
		return nil
	}

	m.IDTemplate = m.LegacyNameTemplate
	return []diag.Diagnostic{atPath(root, "$."+nameTemplateKey, diag.Diagnostic{
		Level: diag.LevelWarning, Phase: diag.PhaseSchemaValidate,
		Code: "id_template_legacy_spelling",
		Message: "name_template: is the pre-ADR-0085 spelling of id_template: — a registry entity's identifier " +
			"is spelled id, and the key composing one follows it",
		Hint: "rename the key to id_template: (the value is unchanged); the old spelling is read for a " +
			"compatibility window and will be removed",
	})}
}
