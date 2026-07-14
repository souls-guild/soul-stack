package config

// Covenant FS resolve (Resolved layer). `*FromBytes` stay NO-I/O: covenant.yml is
// read ONLY here, by the snapshot's serviceRoot. This file is the single source of
// truth for merging a covenant fragment into a manifest for ALL consumers (keeper
// runtime-load, trial-harness, soul-lint scenario path): the logic used to live in
// keeper/internal/artifact, now it's here, and the rest call [ResolveScenarioCovenant].
//
// Why not in the schema/semantic phase of `*FromBytes`: form validation (`form` ⊆ the
// effective `input`) is correct only over the MERGED input, and the effective input
// exists only post-merge (needs the FS). So the covenant scenario's form is checked
// HERE, after MergeCovenant, with the same core [validateFormAgainstInputKeys] the
// non-extends path runs in the semantic phase.

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	securejoin "github.com/cyphar/filepath-securejoin"

	"github.com/goccy/go-yaml/ast"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// covenantFileExt — the covenant-fragment file extension in the service-repo root.
// File name = `<extends>.yml` (the `extends:` name + this extension), sibling to
// types.yml/service.yml/scenario/.
const covenantFileExt = ".yml"

// ResolveScenarioCovenant merges the covenant fragment into the scenario manifest IN
// PLACE (MergeCovenant mutates *m) and validates `form` against the MERGED input. Call
// strictly on a manifest born from bytes in this same load pass (not a shared cache
// object): MergeCovenant copies the fragment's input schemas by pointer, so the
// fragment must stay read-only — it is local to this call and reused nowhere.
//
// No-op when `m.Extends` is empty (a scenario without inheritance — forward-compat
// bit-for-bit, no FS access). Otherwise: the covenant.yml path in the snapshot ROOT
// (`<serviceRoot>/<extends>.yml`) is built from the extends name, the fragment is read
// by a securejoin reader (traversal clamp: the name is a single-segment kebab per
// [ValidExtendsName], securejoin clamps additionally), decoded by
// [LoadCovenantFragmentFromBytes] and merged add-only. After merge — post-merge form
// validation over the merged `m.Input`.
//
// serviceRoot — absolute path to the service snapshot root (keeper: art.LocalDir;
// trial: the test tree root; soul-lint: the linted repo root). doc — the Document born
// from the same `*FromBytes` call: its AST provides the `form:` node for the post-merge
// check (diagnostic positions point at real source lines). doc==nil OR no `form:` →
// the post-merge form check is skipped (nothing to check).
//
// Diagnostics (all diag.LevelError, the operator fixes one at a time):
//   - covenant_extends_invalid          — the extends name failed [ValidExtendsName];
//   - covenant_extends_target_not_found — no covenant.yml by that name;
//   - io_error                          — covenant.yml exists but is unreadable;
//   - <fragment-diag>                   — covenant.yml itself is invalid (its
//     decode/schema errors passed through as-is, tagged with the covenant's File);
//   - section_key_conflict              — a section key in both covenant AND scenario
//     (add-only merge forbids override);
//   - state_changes_form_mismatch       — covenant and scenario declared state_changes
//     in different forms (list vs deprecated map);
//   - covenant_merge_failed             — other (unexpected) merge errors;
//   - form_field_unknown/duplicate/…    — the post-merge form check (see the core).
func ResolveScenarioCovenant(m *ScenarioManifest, doc *Document, serviceRoot string) []diag.Diagnostic {
	if m == nil || m.Extends == "" {
		return nil
	}
	name := m.Extends
	scenarioPath := docPath(doc)

	if !ValidExtendsName(name) {
		return []diag.Diagnostic{{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			File: scenarioPath, Code: "covenant_extends_invalid",
			Message: fmt.Sprintf("extends: %q — недопустимое имя covenant-фрагмента", name),
			Hint:    "single-segment kebab-case (^[a-z][a-z0-9-]*$), без разделителей пути",
		}}
	}

	covenantFile := name + covenantFileExt
	data, err := readCovenantFile(serviceRoot, covenantFile)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return []diag.Diagnostic{{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				File: scenarioPath, Code: "covenant_extends_target_not_found",
				Message: fmt.Sprintf("extends: %q — covenant-файл %s в корне сервиса не найден", name, covenantFile),
				Hint:    "covenant.yml-семейство (<extends>.yml) лежит в корне service-репо, сиблинг service.yml/types.yml",
			}}
		}
		return []diag.Diagnostic{{
			Level: diag.LevelError, Phase: diag.PhaseParse,
			File: covenantFile, Code: "io_error", Message: err.Error(),
			Hint: "covenant-файл присутствует, но не читается — extends не резолвится",
		}}
	}

	// FRESH fragment per scenario resolve (not cached between scenarios):
	// MergeCovenant copies the fragment's input schemas by POINTER, so the fragment
	// must stay READ-ONLY after merge — guaranteed by it being local to this call.
	fragment, _, fdiags := LoadCovenantFragmentFromBytes(covenantFile, data, ValidateOptions{})
	if diag.HasErrors(fdiags) {
		// covenant.yml is invalid: pass its own errors through as-is (File is already
		// = covenantFile from decode), skip merge (fragment is broken).
		return fdiags
	}

	// Cross-form state_changes: on an IsList mismatch MergeCovenant doesn't detect
	// `set <field>` conflicts (it takes the local form). Mixing list↔map is different
	// grammars; reject explicitly BEFORE merge, else covenant sets of the other form
	// would be silently lost.
	if fragment.StateChanges != nil && m.StateChanges != nil &&
		fragment.StateChanges.IsList != m.StateChanges.IsList {
		return append(fdiags, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			File: scenarioPath, Code: "state_changes_form_mismatch",
			Message: fmt.Sprintf("extends: %q — covenant и сценарий объявили state_changes в разных формах (list vs map)", name),
			Hint:    "приведите обе стороны к list-форме state_changes (map-форма deprecated)",
		})
	}

	if err := MergeCovenant(*fragment, m); err != nil {
		var conflict *SectionKeyConflict
		if errors.As(err, &conflict) {
			return append(fdiags, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				File: scenarioPath, Code: conflict.Code(),
				Message: fmt.Sprintf("extends: %q — секция %s.%s объявлена и в covenant, и в сценарии (add-only merge запрещает override)",
					name, conflict.Section, conflict.Key),
				Hint: "уберите дубль ключа из одной из сторон — covenant задаёт общий контракт, сценарий добавляет дельту",
			})
		}
		// Other merge errors (not expected for already-validated sections) — pass
		// through as a generic diagnostic, not losing them.
		return append(fdiags, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			File: scenarioPath, Code: "covenant_merge_failed",
			Message: fmt.Sprintf("extends: %q — слияние covenant не удалось: %s", name, err.Error()),
		})
	}

	// POST-MERGE form validation: `form` ⊆ the EFFECTIVE (merged) `input`. Not possible
	// before merge — covenant fields would be absent from m.Input (false
	// form_field_unknown); so the covenant scenario's form is gated out of the semantic
	// phase (scenario.go) and checked here. Same core as the non-extends path, on the
	// same AST (the form: node from doc).
	fdiags = append(fdiags, resolveCovenantFormDiags(m, doc, scenarioPath)...)
	return fdiags
}

// resolveCovenantFormDiags runs the covenant scenario's post-merge form check if the
// manifest declares `form:`. Source of input keys is the MERGED m.Input; the form:
// node's AST comes from doc. No doc / no form: key → zero diagnostics (nothing to
// check, forward-compat bit-for-bit). Diagnostics without a File get the scenario path.
func resolveCovenantFormDiags(m *ScenarioManifest, doc *Document, scenarioPath string) []diag.Diagnostic {
	root := rootMapping(doc)
	if root == nil || !topLevelKeys(root)["form"] {
		return nil
	}
	inputKeys := make(map[string]bool, len(m.Input))
	for k := range m.Input {
		inputKeys[k] = true
	}
	out := validateFormAgainstInputKeys(root, inputKeys, "$.form")
	for i := range out {
		if out[i].File == "" {
			out[i].File = scenarioPath
		}
	}
	return out
}

// readCovenantFile reads covenant.yml from the serviceRoot snapshot by file name
// (`<extends>.yml`). securejoin clamps any escape outside serviceRoot (defence-in-
// depth on top of the covenant name grammar). Returns fs.ErrNotExist transparently —
// the caller tells "covenant not found" from other I/O errors.
func readCovenantFile(serviceRoot, name string) ([]byte, error) {
	// securejoin requires a root without `..` components: callers (trial/soul-lint) may
	// pass a relative serviceRoot (`../examples/...`) — make it absolute. This does NOT
	// weaken the clamp outside serviceRoot (the covenant name is a single-segment
	// kebab, securejoin clamps additionally).
	if abs, aerr := filepath.Abs(serviceRoot); aerr == nil {
		serviceRoot = abs
	}
	full, err := securejoin.SecureJoin(serviceRoot, name)
	if err != nil {
		return nil, fmt.Errorf("config: небезопасный путь covenant %q: %w", name, err)
	}
	// os.ReadFile wraps a missing file in *PathError with fs.ErrNotExist — the
	// caller's errors.Is catches it, telling "no covenant" from other I/O errors.
	return os.ReadFile(full)
}

// rootMapping extracts the root mapping AST node from the opaque Document
// (package-private access to doc.file). nil-safe: nil doc / empty or non-mapping root
// → nil (the caller treats it as "AST unavailable", the post-merge form check is
// skipped).
func rootMapping(doc *Document) *ast.MappingNode {
	if doc == nil || doc.file == nil || len(doc.file.Docs) == 0 {
		return nil
	}
	body := doc.file.Docs[0].Body
	if mm, ok := body.(*ast.MappingNode); ok {
		return mm
	}
	return nil
}

// docPath — the file path associated with a Document (for the File tag on
// diagnostics). nil-safe (nil doc → "").
func docPath(doc *Document) string {
	if doc == nil {
		return ""
	}
	return doc.path
}
