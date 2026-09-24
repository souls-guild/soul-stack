package config

// The `id_template:` → `id:` compatibility window (ADR-0079 amendment 2026-09-24,
// NIM-899). Both spellings load; the scalar folds into [ScenarioManifest.ID] ONCE, at
// the top of schemaValidateScenario, so nothing downstream learns which one the file
// used. The pre-[ADR-0085] `name_template:` is not read at all — it sits in
// [deprecatedScenarioKeys], which answers `unknown_key` with the replacement.
//
// [ADR-0085]: ../../docs/adr/0085-entity-id-and-label.md

import (
	"github.com/goccy/go-yaml/ast"

	"github.com/souls-guild/soul-stack/shared/diag"
)

const (
	idBlockKey = "id"
	// idTemplateKey — the scalar spelling of `id.template`, read for the window.
	idTemplateKey = "id_template"
	// idBlockTemplateKey — the canonical key as a diagnostic spells it.
	idBlockTemplateKey = idBlockKey + ".template"
)

// writtenIDTemplateKey names the spelling the file used, so a diagnostic points at a
// key the author can find. The block wins when both are present — that is already an
// error, and the canonical key is the one to keep.
func (m *ScenarioManifest) writtenIDTemplateKey(topKeys map[string]bool) string {
	if topKeys[idBlockKey] {
		return idBlockTemplateKey
	}
	if topKeys[idTemplateKey] {
		return idTemplateKey
	}
	return idBlockTemplateKey
}

// normalizeIDTemplate folds the scalar into [ScenarioManifest.ID] and reports the
// window's two diagnostics: both spellings in one file → ERROR id_template_conflict
// (not resolved by precedence — picking one silently would make the run disagree with
// half the file), the scalar alone → WARNING id_template_legacy_spelling. Both address
// the key the file wrote.
func (m *ScenarioManifest) normalizeIDTemplate(root *ast.MappingNode) []diag.Diagnostic {
	topKeys := topLevelKeys(root)
	hasBlock, hasScalar := topKeys[idBlockKey], topKeys[idTemplateKey]

	if hasBlock && hasScalar {
		return []diag.Diagnostic{atPath(root, "$."+idTemplateKey, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code: "id_template_conflict",
			Message: "the scenario declares both id: and " + idTemplateKey + ": — " +
				idTemplateKey + " is the scalar spelling of the same key, not a second template",
			Hint: "delete " + idTemplateKey + ": and keep the id: block",
		})}
	}
	if !hasScalar {
		return nil
	}

	m.ID.Template = m.LegacyIDTemplate
	return []diag.Diagnostic{atPath(root, "$."+idTemplateKey, diag.Diagnostic{
		Level: diag.LevelWarning, Phase: diag.PhaseSchemaValidate,
		Code: "id_template_legacy_spelling",
		Message: idTemplateKey + ": is the scalar spelling of the id: block — the block carries the bounds " +
			"a composed id must satisfy (id.max_length) beside the template that builds it",
		Hint: "rewrite it as an id: block with template: under it (the value is unchanged); the scalar is " +
			"read for a compatibility window and will be removed",
	})}
}
