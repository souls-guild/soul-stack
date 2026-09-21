package config

import (
	"reflect"
	"strconv"
	"strings"

	"github.com/goccy/go-yaml/ast"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// inputSchemaType / inputSchemaMapType are reflect-walker stop points. The
// InputSchema / InputSchemaMap types have their own (recursive) validation in
// `input_schema.go`: map keys are arbitrary, and InputSchema's `required` has two
// meanings. The generic reflect walker does not fit and must not descend into them.
var (
	inputSchemaType    = reflect.TypeOf(InputSchema{})
	inputSchemaMapType = reflect.TypeOf(InputSchemaMap(nil))
)

// destinyManifestType / serviceManifestType / scenarioManifestType suppress the
// generic `unknown_key` for top-level deprecated keys. schemaValidateDestiny /
// schemaValidateService / schemaValidateScenario raise a diagnostic for them with
// a meaningful hint; otherwise the reflect walker would add a second `unknown_key`
// without a hint — a duplicate in the JSON output at the same line/col.
var (
	destinyManifestType  = reflect.TypeOf(DestinyManifest{})
	serviceManifestType  = reflect.TypeOf(ServiceManifest{})
	scenarioManifestType = reflect.TypeOf(ScenarioManifest{})
)

// covenantFragmentType — covenant.yml. The generic walker over ScenarioFragment's
// reflect fields (only 4 sections) would flag any foreign top-level key
// (name/tasks/create/form/extends) as `unknown_key`; the covenant validator
// validateCovenantFragment raises a precise `covenant_unexpected_key` with a hint.
// To avoid a duplicate at the same line/col, the walker stays silent about
// top-level unknowns for covenant (suppressAll), leaving the analysis to the validator.
var covenantFragmentType = reflect.TypeOf(ScenarioFragment{})

// keeperPluginsType — `keeper.yml::plugins`. Its removed keys are handled here
// rather than in a second validator pass, because keeper.yml has none: the walker
// IS the schema phase for it, and an unhandled removed key is a boot-stopping
// `unknown_key` error (keeper/cmd/keeper/main.go exits on the first error).
var keeperPluginsType = reflect.TypeOf(KeeperPlugins{})

// removedKeys — keys a struct no longer has, but which a config written against
// an older release still carries. Unlike deprecatedDestinyKeys and friends, which
// are suppressed here so a SECOND pass can raise them, these have no second pass:
// the entry carries the hint and the walker raises it directly, as a WARNING.
//
// A warning, not an error, on purpose. `plugins.cloud_drivers` named a plugin kind
// this release deleted (NIM-761); a keeper that refused to start on it would turn
// an upgrade into an outage for every deployment that has the block, to enforce
// the removal of a list that is now simply ignored. The operator is told to delete
// it and the run continues.
var removedKeys = map[reflect.Type]map[string]string{
	keeperPluginsType: {
		"cloud_drivers": "cloud_drivers: removed (NIM-761) together with the CloudDriver plugin contract and " +
			"the `kind: cloud_driver` discriminator. A cloud driver is now an ordinary SoulModule plugin " +
			"declaring `side: keeper`, so it is registered under `plugins.soul_modules` instead. The block is " +
			"IGNORED — delete it, and move any entry you still need to `soul_modules`",
	},
}

// taskType is a reflect-walker stop point. Task has its own discriminated
// UnmarshalYAML and its own validation (validateTaskNode); a generic reflect walk
// would catch `module:`/`include:` as unknown_key (they are tagged `yaml:"-"` in
// Task) or descend into polymorphic branches. Like inputSchemaType — the standard
// "own Unmarshal → stop" pattern.
var taskType = reflect.TypeOf(Task{})

// computeBlockType is a reflect-walker stop point. `compute:` is a YAML mapping
// `<name>: <expression>` with its own UnmarshalYAML (ComputeBlock) and validator
// validateComputeBlock. Without suppression the generic walker would see
// []ComputeVar as a sequence-of-struct and fail to match the mapping node (false unknown_key).
var computeBlockType = reflect.TypeOf(ComputeBlock(nil))

// validateRuleSliceType is a reflect-walker stop point for `validate:`. The block
// has its own AST validator validateValidateBlock (requires that/message, compiles
// that input-only). The generic slice-of-struct walker would catch only
// unknown_key but duplicate it with validateValidateBlock at the same line/col —
// suppress (like taskType/computeBlockType).
var validateRuleSliceType = reflect.TypeOf([]ValidateRule(nil))

// formLayoutType is a reflect-walker stop point for `form:`. The block has its own
// AST validator validateFormLayout (cross-invariants against input: + unknown_key
// with a meaningful hint on sections/fields). The generic walker over the
// FormLayout struct would duplicate unknown_key at the same line/col — suppress
// (like validateRuleSliceType/computeBlockType).
var formLayoutType = reflect.TypeOf(FormLayout{})

// walkUnknownKeys walks the AST mapping `root` against the yaml tags of type
// `cfg`, collecting `unknown_key` diagnostics for keys absent from the Go struct.
//
// Used instead of `yaml.Strict()` because Strict stops on the first error — we
// need to surface all unknowns at once.
//
// Descends recursively into sub-mappings/slices. For a slice it walks each element
// against the same element type.
func walkUnknownKeys(_ string, m *ast.MappingNode, cfg any, pathPrefix string) []diag.Diagnostic {
	t := reflect.TypeOf(cfg)
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return nil
	}
	return walkMappingAgainstStruct(m, t, pathPrefix)
}

// walkMappingAgainstStruct recursively walks a MappingNode against a reflect.Type
// (struct). Returns the list of unknown_key errors found.
//
// At each level:
//   - for each key→value pair, look up the struct field by yaml tag;
//   - if no field — `unknown_key`;
//   - if there is one and the sub-value is a mapping/sequence — recurse into the
//     corresponding element type.
func walkMappingAgainstStruct(m *ast.MappingNode, t reflect.Type, path string) []diag.Diagnostic {
	if m == nil {
		return nil
	}
	known := yamlFieldIndex(t)
	// Suppress duplicates: for DestinyManifest, deprecated top-level keys get a
	// more informative diagnostic in schemaValidateDestiny (with a hint). Do not
	// emit a second `unknown_key` for them here — set comparison in tests hides
	// the duplicate, but in the JSON output it shows up as a twin line.
	var suppress map[string]bool
	var suppressAllUnknown bool
	removed := removedKeys[t]
	switch t {
	case destinyManifestType:
		suppress = make(map[string]bool, len(deprecatedDestinyKeys))
		for k := range deprecatedDestinyKeys {
			suppress[k] = true
		}
	case serviceManifestType:
		suppress = make(map[string]bool, len(deprecatedServiceKeys))
		for k := range deprecatedServiceKeys {
			suppress[k] = true
		}
	case scenarioManifestType:
		suppress = make(map[string]bool, len(deprecatedScenarioKeys))
		for k := range deprecatedScenarioKeys {
			suppress[k] = true
		}
	case covenantFragmentType:
		suppressAllUnknown = true
	}
	var out []diag.Diagnostic
	for _, kv := range m.Values {
		key := kv.Key.GetToken()
		if key == nil {
			continue
		}
		keyName := key.Value
		fieldType, ok := known[keyName]
		if !ok {
			if suppressAllUnknown || suppress[keyName] {
				continue
			}
			if hint, gone := removed[keyName]; gone {
				out = append(out, diag.Diagnostic{
					Level:    diag.LevelWarning,
					Phase:    diag.PhaseSchemaValidate,
					Line:     key.Position.Line,
					Column:   key.Position.Column,
					Code:     "removed_key",
					Message:  `removed field "` + keyName + `"`,
					Hint:     hint,
					YAMLPath: path + "." + keyName,
				})
				continue
			}
			out = append(out, diag.Diagnostic{
				Level:    diag.LevelError,
				Phase:    diag.PhaseSchemaValidate,
				Line:     key.Position.Line,
				Column:   key.Position.Column,
				Code:     "unknown_key",
				Message:  `unknown field "` + keyName + `"`,
				YAMLPath: path + "." + keyName,
			})
			continue
		}
		out = append(out, walkValueAgainstType(kv.Value, fieldType, path+"."+keyName)...)
	}
	return out
}

// walkValueAgainstType recurses over an AST node against the reflect.Type of the
// expected field.
//
// Semantics by field type:
//   - struct → the value must be a mapping; recurse into walkMappingAgainstStruct.
//   - pointer-to-struct → unwrap the element, then treat as struct.
//   - slice — iterate elements (if the element is a struct, walk each against the
//     element type).
//   - map → not validated recursively (used only for reaper.rules, where keys are
//     arbitrary). If the value type is a struct, walk each value.
//   - leaf types (string/int/bool) — not walked.
func walkValueAgainstType(n ast.Node, t reflect.Type, path string) []diag.Diagnostic {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}

	// Stop recursion at types with special validation (see the doc-comment on
	// inputSchemaType): validateInputSchemaMap/Node in input_schema.go handles
	// unknown_keys and schema checks.
	if t == inputSchemaType || t == inputSchemaMapType {
		return nil
	}
	// Task — own UnmarshalYAML + validateTaskNode over the AST.
	if t == taskType {
		return nil
	}
	// ComputeBlock — own UnmarshalYAML (mapping name→expression) + validator
	// validateComputeBlock; the generic slice-of-struct walker does not descend here.
	if t == computeBlockType {
		return nil
	}
	// []ValidateRule — own AST validator validateValidateBlock; the generic
	// slice-of-struct walker does not descend here (would duplicate unknown_key).
	if t == validateRuleSliceType {
		return nil
	}
	// FormLayout — own AST validator validateFormLayout (cross-invariants +
	// unknown_key over sections/fields); the generic walker does not descend here.
	if t == formLayoutType {
		return nil
	}

	switch t.Kind() {
	case reflect.Struct:
		mm, ok := n.(*ast.MappingNode)
		if !ok {
			return nil
		}
		return walkMappingAgainstStruct(mm, t, path)

	case reflect.Slice, reflect.Array:
		elem := t.Elem()
		for elem.Kind() == reflect.Pointer {
			elem = elem.Elem()
		}
		if elem.Kind() != reflect.Struct {
			return nil
		}
		// stop-types: Task has its own UnmarshalYAML and its own validator
		// validateTaskNode over the AST — the generic walker must not descend.
		if elem == taskType {
			return nil
		}
		seq, ok := n.(*ast.SequenceNode)
		if !ok {
			return nil
		}
		var out []diag.Diagnostic
		for i, item := range seq.Values {
			itemPath := path + "[" + strconv.Itoa(i) + "]"
			if mm, ok := item.(*ast.MappingNode); ok {
				out = append(out, walkMappingAgainstStruct(mm, elem, itemPath)...)
			}
		}
		return out

	case reflect.Map:
		valT := t.Elem()
		for valT.Kind() == reflect.Pointer {
			valT = valT.Elem()
		}
		if valT.Kind() != reflect.Struct {
			return nil
		}
		mm, ok := n.(*ast.MappingNode)
		if !ok {
			return nil
		}
		var out []diag.Diagnostic
		for _, kv := range mm.Values {
			key := kv.Key.GetToken()
			if key == nil {
				continue
			}
			subPath := path + "." + key.Value
			if subMM, ok := kv.Value.(*ast.MappingNode); ok {
				out = append(out, walkMappingAgainstStruct(subMM, valT, subPath)...)
			}
		}
		return out
	}
	return nil
}

// yamlFieldIndex returns a map of yaml name → field reflect.Type. Handles
// `omitempty`/other modifiers (only the first part of the tag is taken). Fields
// tagged "-" are skipped.
func yamlFieldIndex(t reflect.Type) map[string]reflect.Type {
	out := make(map[string]reflect.Type, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		tag := f.Tag.Get("yaml")
		if tag == "-" {
			continue
		}
		name := tag
		if i := strings.IndexByte(name, ','); i >= 0 {
			name = name[:i]
		}
		if name == "" {
			name = f.Name
		}
		out[name] = f.Type
	}
	return out
}
