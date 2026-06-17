package config

import (
	"errors"
	"fmt"

	"github.com/goccy/go-yaml"
	"github.com/goccy/go-yaml/ast"
	"github.com/goccy/go-yaml/parser"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// LoadDestinyTasksFromBytes парсит `tasks/main.yml` destiny — плоский
// top-level YAML-sequence задач DSL-ядра (docs/destiny/tasks.md). В отличие от
// scenario, у destiny-tasks нет манифест-обёртки (`name:`/`input:`/`tasks:`):
// корень файла — сразу список задач.
//
// Контракт возврата симметричен прочим Load*FromBytes:
//   - parse-fatal (`yaml_parse_error`/`empty_document`/`type_mismatch` на корне)
//     → tasks=nil, в diagnostics одна запись.
//   - schema-errors задач → tasks частично заполнен, diagnostics несут все
//     найденные ошибки (caller отбраковывает через diag.HasErrors).
//   - error != nil — никогда (зарезервировано под I/O fatal в файловой обёртке).
func LoadDestinyTasksFromBytes(filename string, data []byte, opts ValidateOptions) ([]Task, []diag.Diagnostic, error) {
	_ = opts // зарезервировано

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
			// UnmarshalYAML почти всегда возвращает nil (ошибки формы ловит
			// validateTaskNode по AST с line/col). Ненулевую ошибку фиксируем
			// отдельно, чтобы не потерять её молча.
			diags = append(diags, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate, File: filename,
				Code:     "type_mismatch",
				Message:  fmt.Sprintf("tasks[%d]: %v", i, err),
				YAMLPath: fmt.Sprintf("$[%d]", i),
			})
		}
		diags = append(diags, validateTaskNode(item, fmt.Sprintf("$[%d]", i))...)
	}
	// Cross-task инварианты по всему списку (дубли register, неизвестные
	// register-ссылки в onchanges/onfail/require). См. validateTaskRefs.
	diags = append(diags, validateTaskRefs(seq, "$")...)
	for j := range diags {
		if diags[j].File == "" {
			diags[j].File = filename
		}
	}
	return tasks, diags, nil
}
