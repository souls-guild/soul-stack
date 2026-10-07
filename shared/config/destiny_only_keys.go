package config

import (
	"fmt"
	"regexp"

	"github.com/goccy/go-yaml/ast"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// CodeAsyncOutsideDestiny — `async:` on a scenario task, at any block depth or in a
// scenario's included body. Concurrency on a host, a loop's iterations included,
// lives in a destiny (ADR NIM-907, decision 7).
const CodeAsyncOutsideDestiny = "async_outside_destiny"

// CodeRequireOutsideDestiny — `require:` on any scenario task, at any block depth or
// in a scenario's included body. The barrier joins async tasks of the same message,
// and a scenario has none of its own; an applied destiny's are reached from inside
// the destiny or through a `register:` on the applier (ADR NIM-907, decision 7).
const CodeRequireOutsideDestiny = "require_outside_destiny"

// validateDestinyOnlyKeys refuses `async:` and `require:` on a scenario task and on
// every block descendant of it. Inside a destiny both keys keep their ADR-0075
// meaning, so this runs only where the caller knows the tasks are a scenario's:
// [validateTaskNode] is shared with destiny files and cannot tell.
//
// Both are refused on the key's presence, like every `<key>_on_block_invalid`:
// a key that stays legal and stops having an effect is the silence the ADR removes.
// `async:` on an applier, a block or a keeper-side task keeps its more specific code.
func validateDestinyOnlyKeys(item ast.Node, pathPrefix string) []diag.Diagnostic {
	mm, ok := item.(*ast.MappingNode)
	if !ok {
		return nil
	}
	present := map[string]*ast.MappingValueNode{}
	for _, kv := range mm.Values {
		if tok := kv.Key.GetToken(); tok != nil {
			present[tok.Value] = kv
		}
	}

	var out []diag.Diagnostic
	if kv, ok := present["async"]; ok && !asyncRefusedByKind(present) {
		out = append(out, diagAtKV(kv, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     CodeAsyncOutsideDestiny,
			Message:  "async: is allowed only on a task inside a destiny — concurrency on a host, a loop's iterations included, lives in a destiny (docs/destiny/tasks.md §6)",
			Hint:     "drop async:; to run tasks concurrently on a host, write them with async: in a destiny and apply it",
			YAMLPath: pathPrefix + ".async",
		}))
	}
	if kv, ok := present["require"]; ok {
		out = append(out, diagAtKV(kv, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     CodeRequireOutsideDestiny,
			Message:  "require: is allowed only on a task inside a destiny — a scenario has no async: tasks of its own for the barrier to join (docs/destiny/tasks.md §8)",
			Hint:     "drop require:; to keep a wait on an applied destiny's async: tasks, give each apply: before the waiting point a register: (its terminal waits for every task of its destiny), or write require: inside the destiny",
			YAMLPath: pathPrefix + ".require",
		}))
	}
	if kv, ok := present["block"]; ok {
		if seq, isSeq := kv.Value.(*ast.SequenceNode); isSeq {
			for i, child := range seq.Values {
				out = append(out, validateDestinyOnlyKeys(child, fmt.Sprintf("%s.block[%d]", pathPrefix, i))...)
			}
		}
	}
	return out
}

// asyncRefusedByKind mirrors the kinds whose `async:` [validateTaskNode] already
// refuses in a destiny and a scenario alike, so one key draws one diagnostic.
func asyncRefusedByKind(present map[string]*ast.MappingValueNode) bool {
	if _, ok := present["apply"]; ok {
		return true
	}
	if _, ok := present["block"]; ok {
		return true
	}
	_, keeper := keeperSideTask(present)
	return keeper
}

var reRequireElemPath = regexp.MustCompile(`\.require\[\d+\]$`)

// dropRequireRefDiags drops what validateTaskRefs says about the names inside a
// scenario's `require:` list. The key itself is refused (require_outside_destiny),
// and a hint to fix a name or move its source would lead back to that refusal.
func dropRequireRefDiags(diags []diag.Diagnostic) []diag.Diagnostic {
	out := diags[:0]
	for _, d := range diags {
		if (d.Code == "require_forward_reference" || d.Code == "unknown_register_reference") && reRequireElemPath.MatchString(d.YAMLPath) {
			continue
		}
		out = append(out, d)
	}
	return out
}
