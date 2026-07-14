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

// LoadKeeper reads and validates `keeper.yml`.
//
// Return contract (diagnostics-only):
//   - `error != nil` — only I/O fatal (open/read). config and document may be nil.
//   - parse-fatal (`yaml_parse_error`) → `error == nil`, config = nil,
//     document = nil, with one Phase=PhaseParse entry in diagnostics.
//   - schema/semantic errors → config partially filled, document filled,
//     diagnostics contain all validation errors found.
//
// `opts.AllowNetworkCalls` is unused in MVP — a placeholder for reach checks.
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

// LoadKeeperFromBytes — the main entry point without I/O. Useful when the caller has
// already read the file (auto-detect kind, tests with in-memory fixtures). `filename`
// is only a `Diagnostic.File` label and used in parser messages.
func LoadKeeperFromBytes(filename string, data []byte, opts ValidateOptions) (*KeeperConfig, *Document, []diag.Diagnostic, error) {
	cfg := &KeeperConfig{}
	doc, diags := parseAndValidate(filename, stripBOM(data), cfg, opts, semanticValidateKeeper)
	return cfg, doc, diags, nil
}

// LoadSoul — same for `soul.yml`. Contract identical to LoadKeeper.
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

// LoadSoulFromBytes — the main entry point without I/O. See LoadKeeperFromBytes.
func LoadSoulFromBytes(filename string, data []byte, opts ValidateOptions) (*SoulConfig, *Document, []diag.Diagnostic, error) {
	cfg := &SoulConfig{}
	doc, diags := parseAndValidate(filename, stripBOM(data), cfg, opts, semanticValidateSoul)
	return cfg, doc, diags, nil
}

// LoadDestinyManifest — same for `destiny.yml` (the root destiny manifest per
// [`docs/destiny/manifest.md`]). Contract identical to LoadKeeper.
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

// LoadDestinyManifestFromBytes — the main entry point without I/O. See
// LoadKeeperFromBytes.
func LoadDestinyManifestFromBytes(filename string, data []byte, opts ValidateOptions) (*DestinyManifest, *Document, []diag.Diagnostic, error) {
	cfg := &DestinyManifest{}
	doc, diags := parseAndValidate(filename, stripBOM(data), cfg, opts, semanticValidateDestiny)
	return cfg, doc, diags, nil
}

// LoadServiceManifest — same for `service.yml` (the root service manifest per
// [`docs/service/manifest.md`]). Contract identical to LoadKeeper.
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

// LoadServiceManifestFromBytes — the main entry point without I/O. See
// LoadKeeperFromBytes.
func LoadServiceManifestFromBytes(filename string, data []byte, opts ValidateOptions) (*ServiceManifest, *Document, []diag.Diagnostic, error) {
	cfg := &ServiceManifest{}
	doc, diags := parseAndValidate(filename, stripBOM(data), cfg, opts, semanticValidateService)
	return cfg, doc, diags, nil
}

// LoadScenarioManifest — same for `scenario/<name>/main.yml` per the normative spec
// [`docs/scenario/orchestration.md`]. Contract identical to LoadKeeper.
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

// LoadScenarioManifestFromBytes — the main entry point without I/O. See
// LoadKeeperFromBytes.
func LoadScenarioManifestFromBytes(filename string, data []byte, opts ValidateOptions) (*ScenarioManifest, *Document, []diag.Diagnostic, error) {
	cfg := &ScenarioManifest{}
	doc, diags := parseAndValidate(filename, stripBOM(data), cfg, opts, semanticValidateScenario)
	return cfg, doc, diags, nil
}

// stripBOM removes a leading UTF-8 BOM (EF BB BF) — a typical Windows-editor artifact.
// YAML 1.2 recommends stripping it; otherwise the parser reads the first letter as part
// of the key and emits `unknown_key` with an invisible character.
func stripBOM(data []byte) []byte {
	if len(data) >= 3 && data[0] == 0xEF && data[1] == 0xBB && data[2] == 0xBF {
		return data[3:]
	}
	return data
}

// parseAndValidate — the shared part for Keeper and Soul.
//
// Steps:
//  1. parse → AST. Error → diag{phase:parse, code:yaml_parse_error}, return.
//  2. unknown_keys walker (schema_validate, by-reflect over cfg).
//  3. NodeToValue without strict (we already collected unknown_keys ourselves). Fills cfg.
//     Decoder errors (type_mismatch, etc.) → schema_validate.
//  4. enum/range checks (schema_validate, on cfg).
//  5. semantic validation (regex, cross-field).
func parseAndValidate[T any](
	path string,
	src []byte,
	cfg *T,
	opts ValidateOptions,
	semantic func(*T, *ast.MappingNode) []diag.Diagnostic,
) (*Document, []diag.Diagnostic) {
	_ = opts // reserved

	file, err := parser.ParseBytes(src, parser.ParseComments)
	if err != nil {
		return nil, []diag.Diagnostic{yamlParseDiag(path, err)}
	}
	if len(file.Docs) == 0 {
		// Document.file is deliberately nil for invalid Documents — Patch/Save don't apply.
		// Semantically the same class as doc.Body == nil below — a single `empty_document` code.
		return &Document{source: src, path: path}, []diag.Diagnostic{{
			Level:   diag.LevelError,
			Phase:   diag.PhaseParse,
			File:    path,
			Code:    "empty_document",
			Message: "document is empty or contains no mapping",
		}}
	}
	if len(file.Docs) > 1 {
		// Document.file is deliberately nil for invalid Documents — Patch/Save don't apply.
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
	// `parser.ParseBytes` on empty or whitespace-only input returns `file.Docs` of
	// length 1 with `doc.Body == nil` — a distinct case from a "completely empty file"
	// (len(file.Docs) == 0): for editors doing truncate-then-write, the "file exists,
	// empty" window is typical. Without an explicit check the type-assert below gives
	// `ok=false`, and the defensive branch calls `doc.Body.GetToken()` on a nil interface
	// → segfault.
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
		// Top-level is not a mapping (e.g. a scalar or sequence).
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
	// (2) Collect all unknown_keys via the reflect walker.
	diags = append(diags, walkUnknownKeys(path, root, cfg, "$")...)

	// (3) Decode (without strict — strict catches only the first error, we want the
	// full picture).
	if err := yaml.NodeToValue(root, cfg); err != nil {
		// NodeToValue types "everything it could"; collect the remaining errors.
		diags = append(diags, decodeErrorDiag(path, err))
	}

	// (4) schema checks over the already-filled struct.
	diags = append(diags, schemaValidate(path, root, cfg)...)

	// (5) semantic validation: regex, cross-field.
	diags = append(diags, semantic(cfg, root)...)
	for i := range diags {
		if diags[i].File == "" {
			diags[i].File = path
		}
	}

	return &Document{source: src, file: file, path: path}, diags
}

// yamlParseDiag converts a goccy parser error into a diag.
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

// decodeErrorDiag — a goccy.NodeToValue error after unknown_keys are already collected.
//
// goccy exposes a common `yaml.Error` interface with `GetToken()` for all its typed
// errors (TypeError/SyntaxError/OverflowError/DuplicateKeyError/UnknownFieldError/
// UnexpectedNodeTypeError). We use it — this covers all cases in one branch and doesn't
// break when new types appear on upgrade. `GetMessage()` returns the clean message
// without the `[L:C]` prefix.
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
