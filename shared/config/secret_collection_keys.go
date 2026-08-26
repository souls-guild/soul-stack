package config

// The uniqueness of a declared secret collection's keys ([ADR-0083] §1, NIM-704).
//
// A collection secret's Vault path ends in the element's `key` value, so two
// elements carrying the same key derive the SAME path. `core.state.*` resolves them
// in order: the first mints, the second finds a value already there and keeps it
// (present-semantics, §4), and both elements come back holding one reference. Two
// accounts share one credential, and nothing in the state, the register or the
// audit event says so.
//
// The check that catches every case lives where the collection is actually
// resolved (keeper/internal/coremod/state.planCollection): it sees the elements as
// they are, computed ones included, and fails the run before the first write.
//
// This is the offline half, and it sees strictly less — a LITERAL list written out
// in the scenario. Today that is a narrow window, and worth knowing before trusting
// it: `core.state.*` declares `value:` as `string` (coremanifest.stateValueParam),
// because the author form is a CEL expression in every corpus case, so a literal
// list ALSO trips param_type_mismatch, and inside an `include:` the whole branch is
// dropped by the expander before any rule sees it. What this adds there is a
// specific second diagnostic naming the two elements, next to a generic one about
// the cell's type. The real collection — the one a `.map()` builds out of operator
// input — is decidable only at apply, and it is the apply-time half that guards it.
//
// A scenario never states which service owns it, so the state_schema comes from the
// caller (soul-lint reads `../../service.yml`, the keeper has the artifact manifest
// in hand) — the same asymmetry [ScanOwnNamespaceVault] carries for the service
// NAME, and it fails the same way: no schema, no check.

import (
	"fmt"
	"strconv"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// SecretKeyDuplicateCode — two elements of a literal collection addressing one
// declared secret with the same key.
const SecretKeyDuplicateCode = "secret_collection_key_duplicate"

// ScanDuplicateSecretKeys walks a scenario's `core.state.<verb>` tasks for a
// literal collection whose elements share the key addressing a declared secret.
//
// tasks is passed separately from the schema so the caller decides whether includes
// are already expanded, exactly as [ScanOwnNamespaceVault].
func ScanDuplicateSecretKeys(file string, schema map[string]any, tasks []Task) []diag.Diagnostic {
	if len(schema) == 0 || len(tasks) == 0 {
		return nil
	}
	// Refused declarations are dropped: they are load-time errors of service.yml,
	// reported there by validateSecretFields, and a second copy of them here would
	// name a scenario that did nothing wrong.
	fields, _ := CollectSecretFields(schema)
	if len(fields) == 0 {
		return nil
	}
	var out []diag.Diagnostic
	scanTasksForDuplicateKeys(file, fields, tasks, "$.tasks", &out)
	return out
}

// scanTasksForDuplicateKeys walks the task list, recursing through `block:` — a
// capture inside a block writes the same field through the same module.
func scanTasksForDuplicateKeys(file string, fields []SecretField, tasks []Task, prefix string, out *[]diag.Diagnostic) {
	for i := range tasks {
		t := &tasks[i]
		where := prefix + "[" + strconv.Itoa(i) + "]"
		if t.Block != nil {
			scanTasksForDuplicateKeys(file, fields, t.Block.Block, where+".block", out)
		}
		if _, isCapture := stateCaptureVerb(t); !isCapture {
			continue
		}
		field, known := captureField(t)
		if !known {
			continue
		}
		// Only a whole-field write carries a list. `add`/`append` hand over ONE
		// element, which cannot collide with itself; a duplicate they create
		// against the STORED collection is invisible here AND to the apply-time
		// half, which never reads the stored value either. Neither guards it: the
		// shared credential is already live by the time a later whole-field write
		// would report it, and that write may never come.
		items, isList := t.Module.Params["value"].([]any)
		if !isList {
			continue
		}
		for _, f := range fields {
			if f.State != field || !f.Collection() {
				continue
			}
			*out = append(*out, duplicateKeyDiags(file, f, items, where+".params.value")...)
		}
	}
}

// duplicateKeyDiags reports the second and every later element carrying a key an
// earlier one already used.
//
// An element whose key is absent, non-string or not a path segment on its own is
// SKIPPED rather than reported: what it derives is not decided here. The predicate
// is [ValidVaultPathSegment] because that is the one [SecretField.VaultPath]
// applies, so two keys compare equal here exactly when they derive one path — and
// an interpolated `${ … }` key fails it, which is what keeps the rule off the normal
// shape, a collection computed by CEL where every key is an expression.
func duplicateKeyDiags(file string, f SecretField, items []any, where string) []diag.Diagnostic {
	seen := make(map[string]int, len(items))
	var out []diag.Diagnostic
	for i, raw := range items {
		elem, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		key, isString := elem[f.Key].(string)
		if !isString || !ValidVaultPathSegment(key) {
			continue
		}
		first, dup := seen[key]
		if !dup {
			seen[key] = i
			continue
		}
		out = append(out, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSemanticValidate,
			File: file, Code: SecretKeyDuplicateCode,
			Message: fmt.Sprintf("element [%d] addresses the secret %q with %s: %q, already used by element [%d] -- both derive the same Vault path, so the second would keep the first one's secret",
				i, f.ID(), f.Key, key, first),
			Hint:     "`key:` addresses one element's secret; two elements sharing it means two accounts holding one credential",
			YAMLPath: where + "[" + strconv.Itoa(i) + "]." + f.Key,
		})
	}
	return out
}
