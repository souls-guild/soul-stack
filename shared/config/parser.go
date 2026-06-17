package config

import (
	"errors"
	"fmt"
	"os"

	"github.com/goccy/go-yaml"
	"github.com/goccy/go-yaml/ast"
	"github.com/goccy/go-yaml/parser"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// LoadKeeper читает и валидирует `keeper.yml`.
//
// Контракт возврата (diagnostics-only):
//   - `error != nil` — только I/O fatal (open/read). config и document могут
//     быть nil.
//   - parse-fatal (`yaml_parse_error`) → `error == nil`, config = nil,
//     document = nil, в diagnostics — одна запись с Phase=PhaseParse.
//   - schema/semantic errors → config частично заполнен, document заполнен,
//     diagnostics содержат все найденные validation-errors.
//
// `opts.AllowNetworkCalls` в MVP не используется — закладка под reach-проверки.
func LoadKeeper(path string, opts ValidateOptions) (*KeeperConfig, *Document, []diag.Diagnostic, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, []diag.Diagnostic{{
			Level:   diag.LevelError,
			Phase:   diag.PhaseParse,
			File:    path,
			Code:    "io_error",
			Message: err.Error(),
		}}, err
	}
	return LoadKeeperFromBytes(path, src, opts)
}

// LoadKeeperFromBytes — основная точка входа без I/O. Полезна, когда вызывающий
// уже прочитал файл (auto-detect kind, тесты с in-memory фикстурами).
// `filename` нужен только как метка `Diagnostic.File` и в сообщениях парсера.
func LoadKeeperFromBytes(filename string, data []byte, opts ValidateOptions) (*KeeperConfig, *Document, []diag.Diagnostic, error) {
	cfg := &KeeperConfig{}
	doc, diags := parseAndValidate(filename, stripBOM(data), cfg, opts, semanticValidateKeeper)
	return cfg, doc, diags, nil
}

// LoadSoul — то же для `soul.yml`. Контракт идентичен LoadKeeper.
func LoadSoul(path string, opts ValidateOptions) (*SoulConfig, *Document, []diag.Diagnostic, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, []diag.Diagnostic{{
			Level:   diag.LevelError,
			Phase:   diag.PhaseParse,
			File:    path,
			Code:    "io_error",
			Message: err.Error(),
		}}, err
	}
	return LoadSoulFromBytes(path, src, opts)
}

// LoadSoulFromBytes — основная точка входа без I/O. См. LoadKeeperFromBytes.
func LoadSoulFromBytes(filename string, data []byte, opts ValidateOptions) (*SoulConfig, *Document, []diag.Diagnostic, error) {
	cfg := &SoulConfig{}
	doc, diags := parseAndValidate(filename, stripBOM(data), cfg, opts, semanticValidateSoul)
	return cfg, doc, diags, nil
}

// LoadDestinyManifest — то же для `destiny.yml` (корневой манифест destiny по
// [`docs/destiny/manifest.md`]). Контракт идентичен LoadKeeper.
func LoadDestinyManifest(path string, opts ValidateOptions) (*DestinyManifest, *Document, []diag.Diagnostic, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, []diag.Diagnostic{{
			Level:   diag.LevelError,
			Phase:   diag.PhaseParse,
			File:    path,
			Code:    "io_error",
			Message: err.Error(),
		}}, err
	}
	return LoadDestinyManifestFromBytes(path, src, opts)
}

// LoadDestinyManifestFromBytes — основная точка входа без I/O. См.
// LoadKeeperFromBytes.
func LoadDestinyManifestFromBytes(filename string, data []byte, opts ValidateOptions) (*DestinyManifest, *Document, []diag.Diagnostic, error) {
	cfg := &DestinyManifest{}
	doc, diags := parseAndValidate(filename, stripBOM(data), cfg, opts, semanticValidateDestiny)
	return cfg, doc, diags, nil
}

// LoadServiceManifest — то же для `service.yml` (корневой манифест сервиса по
// [`docs/service/manifest.md`]). Контракт идентичен LoadKeeper.
func LoadServiceManifest(path string, opts ValidateOptions) (*ServiceManifest, *Document, []diag.Diagnostic, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, []diag.Diagnostic{{
			Level:   diag.LevelError,
			Phase:   diag.PhaseParse,
			File:    path,
			Code:    "io_error",
			Message: err.Error(),
		}}, err
	}
	return LoadServiceManifestFromBytes(path, src, opts)
}

// LoadServiceManifestFromBytes — основная точка входа без I/O. См.
// LoadKeeperFromBytes.
func LoadServiceManifestFromBytes(filename string, data []byte, opts ValidateOptions) (*ServiceManifest, *Document, []diag.Diagnostic, error) {
	cfg := &ServiceManifest{}
	doc, diags := parseAndValidate(filename, stripBOM(data), cfg, opts, semanticValidateService)
	return cfg, doc, diags, nil
}

// LoadScenarioManifest — то же для `scenario/<name>/main.yml` по нормативной
// спеке [`docs/scenario/orchestration.md`]. Контракт идентичен LoadKeeper.
func LoadScenarioManifest(path string, opts ValidateOptions) (*ScenarioManifest, *Document, []diag.Diagnostic, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, []diag.Diagnostic{{
			Level:   diag.LevelError,
			Phase:   diag.PhaseParse,
			File:    path,
			Code:    "io_error",
			Message: err.Error(),
		}}, err
	}
	return LoadScenarioManifestFromBytes(path, src, opts)
}

// LoadScenarioManifestFromBytes — основная точка входа без I/O. См.
// LoadKeeperFromBytes.
func LoadScenarioManifestFromBytes(filename string, data []byte, opts ValidateOptions) (*ScenarioManifest, *Document, []diag.Diagnostic, error) {
	cfg := &ScenarioManifest{}
	doc, diags := parseAndValidate(filename, stripBOM(data), cfg, opts, semanticValidateScenario)
	return cfg, doc, diags, nil
}

// stripBOM убирает ведущую UTF-8 BOM (EF BB BF) — типичный артефакт
// Windows-редакторов. YAML 1.2 рекомендует strip; иначе parser ловит первую
// букву как часть ключа и выдаёт `unknown_key` с невидимым символом.
func stripBOM(data []byte) []byte {
	if len(data) >= 3 && data[0] == 0xEF && data[1] == 0xBB && data[2] == 0xBF {
		return data[3:]
	}
	return data
}

// parseAndValidate — общая часть для Keeper и Soul.
//
// Шаги:
//  1. parse → AST. Ошибка → diag{phase:parse, code:yaml_parse_error}, return.
//  2. unknown_keys walker (schema_validate, by-reflect от cfg).
//  3. NodeToValue без strict (мы уже собрали unknown_keys сами). Заполняет cfg.
//     Ошибки decoder-а (type_mismatch, и т.п.) → schema_validate.
//  4. enum/range checks (schema_validate, on cfg).
//  5. semantic-валидация (regex, cross-field).
func parseAndValidate[T any](
	path string,
	src []byte,
	cfg *T,
	opts ValidateOptions,
	semantic func(*T, *ast.MappingNode) []diag.Diagnostic,
) (*Document, []diag.Diagnostic) {
	_ = opts // зарезервировано

	file, err := parser.ParseBytes(src, parser.ParseComments)
	if err != nil {
		return nil, []diag.Diagnostic{yamlParseDiag(path, err)}
	}
	if len(file.Docs) == 0 {
		// Document.file намеренно nil для невалидных Document — Patch/Save неприменимы.
		// Семантически тот же класс, что doc.Body == nil ниже — единый код `empty_document`.
		return &Document{source: src, path: path}, []diag.Diagnostic{{
			Level:   diag.LevelError,
			Phase:   diag.PhaseParse,
			File:    path,
			Code:    "empty_document",
			Message: "document is empty or contains no mapping",
		}}
	}
	if len(file.Docs) > 1 {
		// Document.file намеренно nil для невалидных Document — Patch/Save неприменимы.
		return &Document{source: src, path: path}, []diag.Diagnostic{{
			Level:   diag.LevelError,
			Phase:   diag.PhaseParse,
			File:    path,
			Code:    "multi_document_not_allowed",
			Message: fmt.Sprintf("config file must contain exactly one YAML document; got %d", len(file.Docs)),
			Hint:    "remove '---' separators",
		}}
	}
	doc := file.Docs[0]
	// `parser.ParseBytes` на пустом или whitespace-only входе возвращает
	// `file.Docs` длины 1 с `doc.Body == nil` — отдельный случай от
	// «совсем пустой файл» (len(file.Docs) == 0): для редакторов, делающих
	// truncate-then-write, окно «файл существует, пустой» — типичное.
	// Без явной проверки type-assert ниже даёт `ok=false`, защитная ветка
	// зовёт `doc.Body.GetToken()` на nil-interface → segfault.
	if doc.Body == nil {
		return &Document{source: src, path: path}, []diag.Diagnostic{{
			Level:   diag.LevelError,
			Phase:   diag.PhaseParse,
			File:    path,
			Code:    "empty_document",
			Message: "document is empty or contains no mapping",
		}}
	}
	root, ok := doc.Body.(*ast.MappingNode)
	if !ok {
		// Top-level не mapping (например, scalar или sequence).
		t := doc.Body.GetToken()
		line, col := 0, 0
		if t != nil {
			line, col = t.Position.Line, t.Position.Column
		}
		return &Document{source: src, file: file, path: path}, []diag.Diagnostic{{
			Level:    diag.LevelError,
			Phase:    diag.PhaseSchemaValidate,
			File:     path,
			Line:     line,
			Column:   col,
			Code:     "type_mismatch",
			Message:  fmt.Sprintf("root of config must be a mapping, got %T", doc.Body),
			YAMLPath: "$",
		}}
	}

	var diags []diag.Diagnostic
	// (2) Сбор всех unknown_keys через reflect-walker.
	diags = append(diags, walkUnknownKeys(path, root, cfg, "$")...)

	// (3) Декодирование (без strict — strict ловит только первую ошибку,
	// нам важно сложить полную картину).
	if err := yaml.NodeToValue(root, cfg); err != nil {
		// NodeToValue типизирует «всё что смог»; собираем оставшиеся ошибки.
		diags = append(diags, decodeErrorDiag(path, err))
	}

	// (4) schema-проверки по уже заполненной struct.
	diags = append(diags, schemaValidate(path, root, cfg)...)

	// (5) semantic-валидация: regex, cross-field.
	diags = append(diags, semantic(cfg, root)...)
	for i := range diags {
		if diags[i].File == "" {
			diags[i].File = path
		}
	}

	return &Document{source: src, file: file, path: path}, diags
}

// yamlParseDiag конвертирует ошибку парсера goccy в diag.
func yamlParseDiag(path string, err error) diag.Diagnostic {
	d := diag.Diagnostic{
		Level:   diag.LevelError,
		Phase:   diag.PhaseParse,
		File:    path,
		Code:    "yaml_parse_error",
		Message: err.Error(),
	}
	var sErr *yaml.SyntaxError
	if errors.As(err, &sErr) && sErr.Token != nil {
		d.Line = sErr.Token.Position.Line
		d.Column = sErr.Token.Position.Column
		d.Message = sErr.Message
	}
	return d
}

// decodeErrorDiag — ошибка goccy.NodeToValue после уже собранных unknown_keys.
//
// goccy экспонирует общий интерфейс `yaml.Error` с `GetToken()` для всех своих
// типизированных ошибок (TypeError/SyntaxError/OverflowError/DuplicateKeyError/
// UnknownFieldError/UnexpectedNodeTypeError). Используем его — это покрывает
// все случаи одной веткой и не ломается при появлении новых типов в upgrade-е.
// `GetMessage()` отдаёт чистое сообщение без префикса `[L:C]`.
func decodeErrorDiag(path string, err error) diag.Diagnostic {
	d := diag.Diagnostic{
		Level:   diag.LevelError,
		Phase:   diag.PhaseSchemaValidate,
		File:    path,
		Code:    "type_mismatch",
		Message: err.Error(),
	}
	var yErr yaml.Error
	if errors.As(err, &yErr) {
		if tok := yErr.GetToken(); tok != nil {
			d.Line = tok.Position.Line
			d.Column = tok.Position.Column
		}
		if msg := yErr.GetMessage(); msg != "" {
			d.Message = msg
		}
	}
	return d
}
