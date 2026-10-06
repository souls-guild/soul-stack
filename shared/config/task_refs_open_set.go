package config

import (
	"fmt"

	"github.com/goccy/go-yaml/ast"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// Two refusals that keep "everything but register.self" well defined for a predicate
// the Keeper renders (NIM-907 decision 4, docs/scenario/predicate-rendering.md §3):
//
//   - `register: self` — a register named like the open set (validateRegisterField);
//   - `when: register.self.*` — `when:` runs before the module, so the task's own
//     result does not exist yet (on the Soul today it is a guaranteed no-such-key).
//
// The check is textual like the rest of this file, so a comprehension variable named
// `register` reads as the root here.

// registerSelfName — the open register of every post-module predicate.
const registerSelfName = "self"

const codeWhenRegisterSelfUnsupported = "when_register_self_unsupported"

// readsRegisterSelf reports `register.self` in the dot form [ExtractRegisterRefs] reads.
func readsRegisterSelf(expr string) bool {
	stripped := celStringLiteral.ReplaceAllString(expr, `""`)
	for _, m := range reRegisterCELRef.FindAllStringSubmatch(stripped, -1) {
		if m[2] == "" && m[3] == registerSelfName {
			return true
		}
	}
	return false
}

// checkWhenRegisterSelf refuses a `when:` that reads the task's own register.
func checkWhenRegisterSelf(value ast.Node, taskPath string) []diag.Diagnostic {
	sn, ok := value.(*ast.StringNode)
	if !ok || !readsRegisterSelf(sn.Value) {
		return nil
	}
	return []diag.Diagnostic{predicateDiagAt(sn, diag.Diagnostic{
		Level: diag.LevelError, Phase: diag.PhaseSemanticValidate,
		Code:     codeWhenRegisterSelfUnsupported,
		Message:  fmt.Sprintf("when: reads register.%s, the task's own result, which does not exist before its module runs", registerSelfName),
		Hint:     "judge the task's own result in changed_when:/failed_when:/retry.until:; to gate on an earlier task, read register.<its register name>",
		YAMLPath: taskPath + ".when",
	})}
}

func predicateDiagAt(sn *ast.StringNode, d diag.Diagnostic) diag.Diagnostic {
	line, col := 0, 0
	if tok := sn.GetToken(); tok != nil {
		line, col = tok.Position.Line, tok.Position.Column
	}
	return diagAt(line, col, d)
}
