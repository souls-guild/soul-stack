package config

import (
	"errors"
	"fmt"

	"github.com/goccy/go-yaml"
	"github.com/goccy/go-yaml/ast"
	"github.com/goccy/go-yaml/parser"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// LoadDestinyTasksFromBytes parses destiny `tasks/main.yml` — the flat top-level
// YAML sequence of DSL-core tasks (docs/destiny/tasks.md). Unlike scenario,
// destiny-tasks has no manifest wrapper (`name:`/`input:`/`tasks:`): the file root
// is the task list directly.
//
// The return contract is symmetric to the other Load*FromBytes:
//   - parse-fatal (`yaml_parse_error`/`empty_document`/`type_mismatch` at the root)
//     → tasks=nil, one entry in diagnostics.
//   - task schema-errors → tasks is partially filled, diagnostics carry all found
//     errors (the caller rejects via diag.HasErrors).
//   - error != nil — never (reserved for I/O fatal in the file wrapper).
func LoadDestinyTasksFromBytes(filename string, data []byte, opts ValidateOptions) ([]Task, []diag.Diagnostic, error) {
	_ = opts // reserved

	file, err := parser.ParseBytes(stripBOM(data), parser.ParseComments)
	if err != nil {
		return nil, []diag.Diagnostic{yamlParseDiag(filename, err)}, nil
	}
	if len(file.Docs) == 0 {
		return nil, []diag.Diagnostic{{
			Level: diag.LevelError, Phase: diag.PhaseParse, File: filename,
			Code:    "empty_document",
			Message: "tasks/main.yml is empty or contains no task list",
		}}, nil
	}
	if len(file.Docs) > 1 {
		return nil, []diag.Diagnostic{{
			Level: diag.LevelError, Phase: diag.PhaseParse, File: filename,
			Code:    "multi_document_not_allowed",
			Message: fmt.Sprintf("tasks/main.yml must contain exactly one YAML document; got %d", len(file.Docs)),
			Hint:    "remove '---' separators",
		}}, nil
	}

	body := file.Docs[0].Body
	if body == nil {
		return nil, []diag.Diagnostic{{
			Level: diag.LevelError, Phase: diag.PhaseParse, File: filename,
			Code:    "empty_document",
			Message: "tasks/main.yml is empty or contains no task list",
		}}, nil
	}

	seq, ok := body.(*ast.SequenceNode)
	if !ok {
		t := body.GetToken()
		line, col := 0, 0
		if t != nil {
			line, col = t.Position.Line, t.Position.Column
		}
		return nil, []diag.Diagnostic{{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate, File: filename,
			Line: line, Column: col,
			Code:     "type_mismatch",
			Message:  fmt.Sprintf("tasks/main.yml must be a sequence of tasks, got %T", body),
			Hint:     "destiny tasks file is a top-level YAML list; no name:/input: wrapper",
			YAMLPath: "$",
		}}, nil
	}

	tasks := make([]Task, len(seq.Values))
	var diags []diag.Diagnostic
	for i, item := range seq.Values {
		if err := tasks[i].UnmarshalYAML(item); err != nil && !errors.Is(err, yaml.ErrInvalidQuery) {
			// UnmarshalYAML almost always returns nil (shape errors are caught by
			// validateTaskNode over the AST with line/col). We record a non-nil
			// error separately so it isn't lost silently.
			diags = append(diags, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate, File: filename,
				Code:     "type_mismatch",
				Message:  fmt.Sprintf("tasks[%d]: %v", i, err),
				YAMLPath: fmt.Sprintf("$[%d]", i),
			})
		}
		diags = append(diags, validateTaskNode(item, fmt.Sprintf("$[%d]", i))...)
	}
	// Cross-task invariants over the whole list (duplicate register, unknown
	// register references in onchanges/onfail/require). See validateTaskRefs.
	diags = append(diags, validateTaskRefs(seq, "$")...)
	for j := range diags {
		if diags[j].File == "" {
			diags[j].File = filename
		}
	}
	return tasks, diags, nil
}
