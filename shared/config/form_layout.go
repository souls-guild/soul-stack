package config

// The presentation layer of the input form — the optional top-level `form:` key
// in the scenario manifest (`scenario/<name>/main.yml`). Pure PRESENTATION: how
// the UI groups and labels the `input:` fields in the operational/create form.
// The input contract (types, validation, requiredness) lives EXCLUSIVELY in
// `input:` — `form:` neither duplicates nor changes it, it references fields by name.
//
// Responsibility boundary:
//   - `input:` — which fields, of what type, whether required (API/validation);
//   - `form:` — in which sections and under which labels the UI renders them.
//
// FORWARD-COMPAT: the key is optional. No `form:` → Form==nil, listing projection
// without the field, the UI renders input flat (as before the feature). A new
// `input:` field not placed in any section does NOT break the form — the UI
// appends it to a default section at the end (so "input field without a section"
// is a WARNING, not an ERROR; see validateFormLayout).

import (
	"fmt"
	"regexp"

	"github.com/goccy/go-yaml/ast"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// FormLayout is the content of the top-level `form:`: an ordered list of sections.
// Declaration order = section render order in the UI.
type FormLayout struct {
	Sections []FormSection `yaml:"sections,omitempty"`
}

// FormSection is one visual group of form fields.
//
// Key is the stable machine id of the section (to remember collapsed-state in the
// UI across runs); must be unique within the form. Title is the group heading.
// Description is an optional note under the heading. Collapsed is the initial
// "collapsed" state (default false). Fields are the input fields rendered in this
// section, in declaration order.
//
// ShowWhen is an optional CEL predicate over `input.*`: the section is visible in
// the UI WHEN it is true (no key → always visible, bit-for-bit forward-compat).
// CAVEAT: this is PRESENTATION, NOT a validation gate. Evaluated client-side in
// the UI (variant A): the backend serves the string as-is and does not evaluate
// the predicate. Hiding a section does not cancel backend validation and
// resolution of its fields — if a value is sent, it is checked as usual. Pairs
// with required_when: show_when hides, required_when requires.
type FormSection struct {
	Key         string      `yaml:"key"`
	Title       string      `yaml:"title,omitempty"`
	Description string      `yaml:"description,omitempty"`
	Collapsed   bool        `yaml:"collapsed,omitempty"`
	ShowWhen    string      `yaml:"show_when,omitempty"`
	Fields      []FormField `yaml:"fields,omitempty"`
}

// FormField references one `input:` field with an optional human-readable label.
//
// Name is the key name from `input:` (must exist there; cross-invariant
// form_field_unknown). Label is the UI caption; optional: empty → the UI uses a
// fallback (the input field's description or the name itself).
//
// ShowWhen is an optional CEL predicate over `input.*` for conditional field
// visibility (semantics and caveat as for FormSection.ShowWhen: presentation,
// client-side eval, NOT a validation gate). Placeholder / Hint are pure UI-widget
// presentation (text in an empty field / hint below the field); they do NOT
// duplicate the `input:` contract (the field's description stays in `input:`, so
// do types/requiredness). All three are optional; the absence of any → bit-for-bit
// as before the feature.
type FormField struct {
	Name        string `yaml:"name"`
	Label       string `yaml:"label,omitempty"`
	ShowWhen    string `yaml:"show_when,omitempty"`
	Placeholder string `yaml:"placeholder,omitempty"`
	Hint        string `yaml:"hint,omitempty"`
}

// reFormSectionKey is the stable machine id of a section: a kebab/snake
// identifier (letter/digit/dash/underscore, starting with a letter). Matches the
// form of coven/section names — usable as a persistent UI-key without escaping.
var reFormSectionKey = regexp.MustCompile(`^[a-z][a-z0-9]*([_-][a-z0-9]+)*$`)

// validateFormLayout is the structural + cross-invariant check of the `form:`
// block in the SEMANTIC phase (non-extends scenario): inputKeys come from the
// already-decoded `m.Input`. A thin wrapper over [validateFormAgainstInputKeys] —
// the only difference from the post-merge path is the source of the input-key set.
//
// The caller invokes it on topKeys["form"] AND only when extends is NOT set: for a
// covenant scenario the effective input exists only AFTER merging the fragment
// (keeper-side, needs the FS), so form there is checked post-merge by the same
// core on the merged `m.Input` (see ResolveScenarioCovenant). Gate — scenario.go
// schemaValidateScenario.
func validateFormLayout(root *ast.MappingNode, m *ScenarioManifest, pathPrefix string) []diag.Diagnostic {
	inputKeys := make(map[string]bool, len(m.Input))
	for name := range m.Input {
		inputKeys[name] = true
	}
	return validateFormAgainstInputKeys(root, inputKeys, pathPrefix)
}

// validateFormAgainstInputKeys is the CORE form check with a PARAMETERIZED
// inputKeys source: the set of effective `input:` names is passed in (from the
// AST/typed `m.Input` of a non-extends scenario OR from the MERGED `m.Input` of a
// covenant scenario post-merge). All invariants are ERROR, EXCEPT
// form_field_uncovered and an empty label/placeholder/hint (WARNING):
//
//   - the block is a mapping with a single meaningful key `sections:` (sequence);
//   - section.key — required, reFormSectionKey format, UNIQUE (ERROR on duplicate);
//   - field.name — required, exists as a key in input (ERROR form_field_unknown);
//   - a field name appears in no more than 1 section total (ERROR form_field_duplicate);
//   - field.label/placeholder/hint — empty string → WARNING (fallback / drop key);
//   - section/field.show_when — if present, CEL over input.* that compiles (else
//     ERROR form_show_when_invalid; input-only sandbox, like required_when);
//   - an input field placed in no section → WARNING form_field_uncovered.
//
// inputKeys is the set of effective input names (nil-safe: nil → form_field_unknown
// on every field, uncovered is not emitted — nothing to cover). root is the AST of
// the manifest/document root (the `form:` node is found through it; positions/anchors
// come from it too, so diagnostics point at real source lines).
func validateFormAgainstInputKeys(root *ast.MappingNode, inputKeys map[string]bool, pathPrefix string) []diag.Diagnostic {
	node := findValueNode(root, "form")
	mm, ok := node.(*ast.MappingNode)
	if !ok {
		line, col := 0, 0
		if vt := node.GetToken(); vt != nil {
			line, col = vt.Position.Line, vt.Position.Column
		}
		return []diag.Diagnostic{diagAt(line, col, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "type_mismatch",
			Message:  "form must be a mapping with a sections: list",
			Hint:     "form: { sections: [ { key: ..., title: ..., fields: [...] } ] }",
			YAMLPath: pathPrefix,
		})}
	}

	sectionsNode, out := formSectionsNode(mm, pathPrefix)
	if sectionsNode == nil {
		return out
	}

	seenKeys := make(map[string]bool, len(sectionsNode.Values))
	// fieldToSection — field name → index of the first section where it is declared
	// (for form_field_duplicate). Covered fields are collected in covered in parallel.
	covered := make(map[string]bool, len(inputKeys))
	fieldOwner := make(map[string]int, len(inputKeys))

	for si, item := range sectionsNode.Values {
		secPath := fmt.Sprintf("%s.sections[%d]", pathPrefix, si)
		out = append(out, validateFormSection(item, secPath, seenKeys, inputKeys, covered, fieldOwner, si)...)
	}

	// form_field_uncovered — input fields with no section (WARNING, not ERROR): the
	// UI appends them to a default section at the end; a new input field must not
	// break the form. Anchored at the `form` key (the field is declared in input,
	// not in form — there is no exact position inside form). Emitted only when input
	// is known (inputKeys non-empty).
	if len(inputKeys) > 0 {
		formTok := findValueNode(root, "form").GetToken()
		line, col := 0, 0
		if formTok != nil {
			line, col = formTok.Position.Line, formTok.Position.Column
		}
		for name := range inputKeys {
			if covered[name] {
				continue
			}
			out = append(out, diagAt(line, col, diag.Diagnostic{
				Level: diag.LevelWarning, Phase: diag.PhaseSchemaValidate,
				Code:     "form_field_uncovered",
				Message:  fmt.Sprintf("input.%s is not placed in any form section", name),
				Hint:     "add it to a form section, or leave it — the UI appends uncovered fields to a default section",
				YAMLPath: pathPrefix + ".sections",
			}))
		}
	}

	return out
}

// formSectionsNode extracts the `sections:` sequence node from the `form:`
// mapping, rejecting other keys (unknown_key) and a wrong form. Returns (nil, diags)
// when `sections:` is missing/malformed (the caller stops the walk).
func formSectionsNode(mm *ast.MappingNode, pathPrefix string) (*ast.SequenceNode, []diag.Diagnostic) {
	var out []diag.Diagnostic
	var sections *ast.MappingValueNode
	for _, kv := range mm.Values {
		tok := kv.Key.GetToken()
		if tok == nil {
			continue
		}
		if tok.Value == "sections" {
			sections = kv
			continue
		}
		out = append(out, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "unknown_key",
			Message:  `unknown field "` + tok.Value + `" in form`,
			Hint:     "form accepts only sections:",
			YAMLPath: pathPrefix + "." + tok.Value,
		}))
	}
	if sections == nil {
		out = append(out, diagAt(lineOf(mm), colOf(mm), diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "missing_required_field",
			Message:  "form requires sections: [ ... ]",
			YAMLPath: pathPrefix + ".sections",
		}))
		return nil, out
	}
	seq, ok := sections.Value.(*ast.SequenceNode)
	if !ok {
		out = append(out, diagAtKV(sections, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "type_mismatch",
			Message:  "form.sections must be a list of sections",
			YAMLPath: pathPrefix + ".sections",
		}))
		return nil, out
	}
	if len(seq.Values) == 0 {
		out = append(out, diagAtKV(sections, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "empty_value",
			Message:  "form.sections must contain at least one section (drop form: for no layout)",
			YAMLPath: pathPrefix + ".sections",
		}))
		return nil, out
	}
	return seq, out
}

// validateFormSection validates one section + accumulates cross-state (seenKeys
// for section.key uniqueness; covered/fieldOwner for field coverage/duplicates).
func validateFormSection(
	node ast.Node, path string,
	seenKeys map[string]bool, inputKeys, covered map[string]bool, fieldOwner map[string]int, secIdx int,
) []diag.Diagnostic {
	mm, ok := node.(*ast.MappingNode)
	if !ok {
		return []diag.Diagnostic{diagAt(lineOf(node), colOf(node), diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "type_mismatch",
			Message:  "form section must be a mapping { key: ..., fields: [...] }",
			YAMLPath: path,
		})}
	}

	var out []diag.Diagnostic
	var keyKV, fieldsKV, showWhenKV *ast.MappingValueNode
	for _, kv := range mm.Values {
		tok := kv.Key.GetToken()
		if tok == nil {
			continue
		}
		switch tok.Value {
		case "key":
			keyKV = kv
		case "fields":
			fieldsKV = kv
		case "show_when":
			showWhenKV = kv
		case "title", "description", "collapsed":
			// presentation scalars — form checked by the reflect-walker via tags;
			// here only cross-invariants.
		default:
			out = append(out, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:     "unknown_key",
				Message:  `unknown field "` + tok.Value + `" in form section`,
				Hint:     "section accepts key / title / description / collapsed / show_when / fields",
				YAMLPath: path + "." + tok.Value,
			}))
		}
	}

	out = append(out, validateFormSectionKey(keyKV, path, seenKeys)...)
	out = append(out, validateFormShowWhen(showWhenKV, path)...)
	out = append(out, validateFormFields(fieldsKV, path, inputKeys, covered, fieldOwner, secIdx)...)
	return out
}

// validateFormShowWhen is the schema-time check of the optional `show_when` key
// (section or field): if present, it is a non-empty CEL string over `input.*` that
// compiles via [compileRequiredWhen] (the same input-only sandbox as
// required_when). Referencing essence/soulprint/register/vault/now →
// undeclared-reference compile error → form_show_when_invalid (mirror of
// input_required_when_invalid).
//
// CAVEAT (variant A, client-side eval): show_when is PRESENTATION, not a validation
// gate. The linter only checks that it compiles; the UI evaluates the predicate. A
// hidden field is still validated/resolved by the backend if a value is sent.
//
// kv == nil (no key) → always visible, zero diagnostics (forward-compat).
func validateFormShowWhen(kv *ast.MappingValueNode, path string) []diag.Diagnostic {
	if kv == nil {
		return nil
	}
	sn, isStr := kv.Value.(*ast.StringNode)
	if !isStr || sn.Value == "" {
		// Empty string / non-string — a meaningless predicate: silently "never
		// visible" — an author footgun. Rejected explicitly (symmetry with required_when).
		return []diag.Diagnostic{diagAtKV(kv, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSemanticValidate,
			Code:     "form_show_when_invalid",
			Message:  "show_when must be a non-empty CEL predicate over input.*",
			Hint:     `e.g. show_when: "input.tls_enabled"`,
			YAMLPath: path + ".show_when",
		})}
	}
	if _, err := compileRequiredWhen(sn.Value); err != nil {
		return []diag.Diagnostic{diagAtKV(kv, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSemanticValidate,
			Code:     "form_show_when_invalid",
			Message:  fmt.Sprintf("show_when does not compile as CEL over input.*: %v", err),
			Hint:     "predicate may reference only input.* (no essence/soulprint/register/vault/now)",
			YAMLPath: path + ".show_when",
		})}
	}
	return nil
}

// validateFormSectionKey — key is required, of reFormSectionKey format, unique.
func validateFormSectionKey(keyKV *ast.MappingValueNode, path string, seenKeys map[string]bool) []diag.Diagnostic {
	if keyKV == nil {
		return []diag.Diagnostic{diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "missing_required_field",
			Message:  "form section requires key: <stable id for UI collapsed-state>",
			YAMLPath: path + ".key",
		}}
	}
	sn, isStr := keyKV.Value.(*ast.StringNode)
	if !isStr || sn.Value == "" {
		return []diag.Diagnostic{diagAtKV(keyKV, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "empty_value",
			Message:  "form section.key must be a non-empty string",
			YAMLPath: path + ".key",
		})}
	}
	var out []diag.Diagnostic
	if !reFormSectionKey.MatchString(sn.Value) {
		out = append(out, diagAtKV(keyKV, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "name_invalid_format",
			Message:  fmt.Sprintf("form section.key %q does not match %s", sn.Value, reFormSectionKey),
			Hint:     "kebab/snake id: lowercase letters, digits, dashes/underscores; start with a letter",
			YAMLPath: path + ".key",
		}))
	}
	if seenKeys[sn.Value] {
		out = append(out, diagAtKV(keyKV, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "duplicate_key",
			Message:  fmt.Sprintf("form section.key %q is declared more than once", sn.Value),
			YAMLPath: path + ".key",
		}))
	}
	seenKeys[sn.Value] = true
	return out
}

// validateFormFields — a section's fields[]: each field.name ∈ input (else
// form_field_unknown), not in >1 section (form_field_duplicate), label non-empty
// (else WARNING). Accumulates covered/fieldOwner for cross-section invariants.
func validateFormFields(
	fieldsKV *ast.MappingValueNode, path string,
	inputKeys, covered map[string]bool, fieldOwner map[string]int, secIdx int,
) []diag.Diagnostic {
	if fieldsKV == nil {
		return []diag.Diagnostic{diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "missing_required_field",
			Message:  "form section requires fields: [ { name: <input-key> } ]",
			YAMLPath: path + ".fields",
		}}
	}
	seq, ok := fieldsKV.Value.(*ast.SequenceNode)
	if !ok {
		return []diag.Diagnostic{diagAtKV(fieldsKV, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "type_mismatch",
			Message:  "form section.fields must be a list of { name, label }",
			YAMLPath: path + ".fields",
		})}
	}

	var out []diag.Diagnostic
	for fi, item := range seq.Values {
		fieldPath := fmt.Sprintf("%s.fields[%d]", path, fi)
		out = append(out, validateFormField(item, fieldPath, inputKeys, covered, fieldOwner, secIdx)...)
	}
	return out
}

// validateFormField — one field fields[i].
func validateFormField(
	node ast.Node, path string,
	inputKeys, covered map[string]bool, fieldOwner map[string]int, secIdx int,
) []diag.Diagnostic {
	mm, ok := node.(*ast.MappingNode)
	if !ok {
		return []diag.Diagnostic{diagAt(lineOf(node), colOf(node), diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "type_mismatch",
			Message:  "form field must be a mapping { name: <input-key>, label?: <str> }",
			YAMLPath: path,
		})}
	}

	var out []diag.Diagnostic
	var nameKV, labelKV, showWhenKV, placeholderKV, hintKV *ast.MappingValueNode
	for _, kv := range mm.Values {
		tok := kv.Key.GetToken()
		if tok == nil {
			continue
		}
		switch tok.Value {
		case "name":
			nameKV = kv
		case "label":
			labelKV = kv
		case "show_when":
			showWhenKV = kv
		case "placeholder":
			placeholderKV = kv
		case "hint":
			hintKV = kv
		default:
			out = append(out, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:     "unknown_key",
				Message:  `unknown field "` + tok.Value + `" in form field`,
				Hint:     "form field accepts name / label / show_when / placeholder / hint",
				YAMLPath: path + "." + tok.Value,
			}))
		}
	}

	name := ""
	if nameKV != nil {
		if sn, isStr := nameKV.Value.(*ast.StringNode); isStr {
			name = sn.Value
		}
	}
	if name == "" {
		out = append(out, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "missing_required_field",
			Message:  "form field requires name: <input-key>",
			YAMLPath: path + ".name", Line: lineOf(mm), Column: colOf(mm),
		})
	} else {
		out = append(out, checkFormFieldName(name, nameKV, path, inputKeys, covered, fieldOwner, secIdx)...)
	}

	// label is optional; an empty string (label: "") is a WARNING (fallback to
	// description/name), not an error.
	if labelKV != nil {
		if sn, isStr := labelKV.Value.(*ast.StringNode); isStr && sn.Value == "" {
			out = append(out, diagAtKV(labelKV, diag.Diagnostic{
				Level: diag.LevelWarning, Phase: diag.PhaseSchemaValidate,
				Code:     "form_field_empty_label",
				Message:  fmt.Sprintf("form field %q has an empty label", name),
				Hint:     "drop label to fall back to the input description/name, or set a non-empty label",
				YAMLPath: path + ".label",
			}))
		}
	}

	out = append(out, validateFormShowWhen(showWhenKV, path)...)
	// placeholder / hint — presentational UI hints; an empty string is meaningless
	// (drop the key) → WARNING, like an empty label.
	out = append(out, warnEmptyPresentation(placeholderKV, name, "placeholder", path)...)
	out = append(out, warnEmptyPresentation(hintKV, name, "hint", path)...)
	return out
}

// warnEmptyPresentation — an empty string on an optional presentational key
// (placeholder / hint) is meaningless: drop the key. Emits a form_field_empty_label
// WARNING (the same class as label — an "empty presentational caption"). kv == nil
// → nothing.
func warnEmptyPresentation(kv *ast.MappingValueNode, name, key, path string) []diag.Diagnostic {
	if kv == nil {
		return nil
	}
	if sn, isStr := kv.Value.(*ast.StringNode); isStr && sn.Value == "" {
		return []diag.Diagnostic{diagAtKV(kv, diag.Diagnostic{
			Level: diag.LevelWarning, Phase: diag.PhaseSchemaValidate,
			Code:     "form_field_empty_label",
			Message:  fmt.Sprintf("form field %q has an empty %s", name, key),
			Hint:     "drop the key, or set a non-empty value",
			YAMLPath: path + "." + key,
		})}
	}
	return nil
}

// checkFormFieldName — name ∈ input (form_field_unknown), not a duplicate across
// sections (form_field_duplicate); marks the field covered.
func checkFormFieldName(
	name string, nameKV *ast.MappingValueNode, path string,
	inputKeys, covered map[string]bool, fieldOwner map[string]int, secIdx int,
) []diag.Diagnostic {
	var out []diag.Diagnostic
	if len(inputKeys) > 0 && !inputKeys[name] {
		out = append(out, diagAtKV(nameKV, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "form_field_unknown",
			Message:  fmt.Sprintf("form field name %q is not a key in input:", name),
			Hint:     "form fields reference input keys by name; declare the field in input: or fix the name",
			YAMLPath: path + ".name",
		}))
	}
	if _, dup := fieldOwner[name]; dup {
		out = append(out, diagAtKV(nameKV, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "form_field_duplicate",
			Message:  fmt.Sprintf("form field %q appears in more than one section", name),
			Hint:     "each input field belongs to exactly one form section",
			YAMLPath: path + ".name",
		}))
	} else {
		fieldOwner[name] = secIdx
	}
	covered[name] = true
	return out
}
