package config

// InputSchema / InputSchemaMap is the typed representation of the `input:` block
// (and the symmetric `output:`) per the normative spec [`docs/input.md`]. The same
// DSL is used in destiny.yml / scenario/<name>/main.yml / a module manifest; the
// implementation is shared.
//
// `required` in the DSL has two context-split meanings:
//   - at the parameter level (any type) — a bool "is the parameter required";
//   - inside type=object — a []string list of required `properties` sub-keys.
//
// So downstream code need not parse `any`, we split these in Go: fields `Required`
// and `RequiredProps`. A custom `UnmarshalYAML` decides by YAML node type
// (`!!bool` → Required, `!!seq` → RequiredProps); semantic-validate checks
// consistency with `type`.

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/goccy/go-yaml"
	"github.com/goccy/go-yaml/ast"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// InputSchemaMap maps a parameter name → schema.
type InputSchemaMap map[string]*InputSchema

// InputSchema is the schema of one parameter per [`docs/input.md`].
//
// All fields are form-valid across types at once (YAML accepts an "extra key for the
// type"), but semantic-validate rejects them as `input_key_invalid_for_type`.
// Pointers (*int/*float64) distinguish "absent" from "zero-value"; bool flags are
// stored as bool (absent = false).
//
// `Required` and `RequiredProps` are tagged `yaml:"-"` — filled via `UnmarshalYAML`
// below so the reflect-walker does not complain about a missing `required` among the
// known keys.
type InputSchema struct {
	Type        string `yaml:"type"`
	Default     any    `yaml:"default,omitempty"`
	Enum        []any  `yaml:"enum,omitempty"`
	Secret      bool   `yaml:"secret,omitempty"`
	Description string `yaml:"description,omitempty"`

	Required      bool     `yaml:"-"`
	RequiredProps []string `yaml:"-"`
	requiredKind  requiredKind

	// RequiredWhen is a CEL predicate over `input.*`: the parameter is required WHEN
	// the predicate is true (docs/input.md → "Conditional requiredness"). Applies to
	// any type. The conditional counterpart of the unconditional Required: that one is
	// always required, this one only on a true predicate. The predicate context is
	// input.* only (this is input validation, not render): other names → compile error
	// of the narrow env (see requireInputValues / input_required_when.go). Empty = key
	// absent.
	RequiredWhen string `yaml:"required_when,omitempty"`

	// PrefillFromState is a `state.<path>` (dot notation) reference into
	// incarnation.state whose CURRENT value the UI pre-fills into the scenario's
	// operational form (docs/input.md → "Pre-fill from state"). It is a form HINT, NOT
	// part of value resolution: the key deliberately does NOT participate in
	// [ResolveInputValues] / [mergeInputDefaults] (incarnation.state must not leak into
	// the effective input — else the operational prefill would quietly become a create
	// default). Boundary with `default`: `default` is a static create default (in
	// merge, in the effective input); `prefill_from_state` is an operational UI hint,
	// visible only to the form-prefill endpoint. They coexist on one field.
	//
	// The `state.<path>` root reuses statepredicate ADR-047 (same `state` root + dot
	// notation), but here it is a LITERAL path reference to one value, not a CEL
	// predicate. Empty = key absent.
	PrefillFromState string `yaml:"prefill_from_state,omitempty"`
	// rawRequired keeps the original `required:` AST node before classification into
	// `requiredKind`. Used in `validateInputSchemaNode` to raise
	// `input_required_value_invalid` when the value is neither a bool nor a sequence of
	// strings (see UnmarshalYAML, default branch).
	rawRequired ast.Node

	Pattern    string `yaml:"pattern,omitempty"`
	Format     string `yaml:"format,omitempty"`
	MinLength  *int   `yaml:"min_length,omitempty"`
	MaxLength  *int   `yaml:"max_length,omitempty"`
	AllowEmpty bool   `yaml:"allow_empty,omitempty"`

	// VaultScope is the single prefix-glob limiting which Vault KV paths the operator
	// may resolve via a `vault:` ref in THIS field's value (docs/input.md →
	// "vault_scope"). Applies only to `type: string` + `secret: true`. Without a
	// declared scope, a `vault:` ref in the field value is a resolve error
	// (default-deny). This does NOT concern author `vault:` refs in task params —
	// those use a separate trusted channel (ADR-010).
	VaultScope string `yaml:"vault_scope,omitempty"`

	Min          *float64 `yaml:"min,omitempty"`
	Max          *float64 `yaml:"max,omitempty"`
	ExclusiveMin *float64 `yaml:"exclusive_min,omitempty"`
	ExclusiveMax *float64 `yaml:"exclusive_max,omitempty"`

	Items    *InputSchema `yaml:"items,omitempty"`
	MinItems *int         `yaml:"min_items,omitempty"`
	MaxItems *int         `yaml:"max_items,omitempty"`
	Unique   bool         `yaml:"unique,omitempty"`

	// Source is the catalog of allowed field values (ADR-044 S-T1). An
	// object-discriminator: exactly one sub-key defines the set from which the backend
	// builds the operator's choice form (e.g. the incarnation's SID list). Applies to
	// type=string (single choice) and type=array with items.type=string (multi choice;
	// limits via min_items/max_items). Schema validation checks only the STRUCTURE of
	// source (known sub-key + value type); the set itself is resolved by the backend at
	// form preparation — not here.
	Source *InputSource `yaml:"source,omitempty"`

	Properties           InputSchemaMap `yaml:"properties,omitempty"`
	AdditionalProperties any            `yaml:"additional_properties,omitempty"`

	// TypeRef is the name of a reusable named type from `service/<name>/types.yml`
	// (the `$type` discriminator key in the input DSL). A node `{$type: <Name>}` is a
	// reference: on service resolution the type schema is substituted for the node
	// (ResolveTypeRefs). It is the standalone form of a field (`<param>: { $type: T }`)
	// OR of array elements (`items: { $type: T }`). `$type` TOGETHER with any of
	// `type:`/`properties:`/`items:` → input_type_ref_conflict (unclear which wins;
	// items:{$type} is an items REFERENCE at the parent level, not a conflict).
	//
	// Tagged `yaml:"-"`: filled in UnmarshalYAML from the `$type` node (the name
	// `$type` is invalid as a Go-yaml tag). Empty = no reference.
	TypeRef string `yaml:"-"`
}

// InputSource is the object-discriminator of the field-value source catalog (ADR-044
// S-T1). Exactly one sub-key defines the set:
//   - IncarnationHosts (`incarnation_hosts: true`) — all SIDs of the current incarnation;
//   - Choir (`choir: <name>`) — the SIDs of a specific Choir part of the incarnation.
//
// Schema validation checks only structural validity (known sub-keys, value types);
// resolving the set and the "value ∈ set" check are done by the backend at form
// preparation (see input_value.go).
type InputSource struct {
	IncarnationHosts bool   `yaml:"incarnation_hosts,omitempty" json:"incarnation_hosts,omitempty"`
	Choir            string `yaml:"choir,omitempty" json:"choir,omitempty"`
}

type requiredKind int

const (
	requiredAbsent requiredKind = iota
	requiredBool                // top-level `required: true/false`
	requiredList                // `required: [name1, name2]` inside an object
)

// Allowed `type` and `format` values — fixed by [`docs/input.md`].
var (
	inputTypeEnum   = []string{"string", "integer", "number", "boolean", "array", "object"}
	inputFormatEnum = []string{
		"hostname", "fqdn", "ipv4", "ipv6", "cidr",
		"email", "uri", "uuid", "semver", "duration",
		"sid", // FQDN form of SID (ADR-044 S-T1), validator reSID in input_value.go
	}

	// reInputParamName — input parameter names (map keys). A parameter is available in
	// templates as `input.<name>` (CEL / text/template); dots, spaces, a leading digit
	// break template resolution, so the form is strictly snake_case.
	reInputParamName = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

	// rePrefillFromStatePath — the `prefill_from_state` path (ADR-047 statepredicate
	// `state` root + dot notation, but a literal path, not CEL): a required `state`
	// root + ≥1 segment. Each segment is snake_case `[a-z][a-z0-9_]*`, matching the
	// form of state field names (statePathQueryPattern in keeper) and input parameters
	// (reInputParamName). Array indices / map keys with other characters are out of MVP
	// — prefill targets a named state field.
	rePrefillFromStatePath = regexp.MustCompile(`^state(\.[a-z][a-z0-9_]*)+$`)

	// The closed set of YAML keys inside one InputSchema node. Used for the unknown_key
	// check (outside the reflect-walker, because InputSchema has a special `required`
	// meaning and recursive fields).
	inputSchemaKnownKeys = map[string]bool{
		"type": true, "required": true, "required_when": true, "default": true, "enum": true,
		"secret": true, "description": true, "prefill_from_state": true,

		"pattern": true, "format": true, "min_length": true,
		"max_length": true, "allow_empty": true, "vault_scope": true,

		"min": true, "max": true, "exclusive_min": true, "exclusive_max": true,

		"items": true, "min_items": true, "max_items": true, "unique": true,

		"properties": true, "additional_properties": true,

		"source": true,

		// $type — the discriminator key of a reference to a reusable named type
		// (service/<name>/types.yml). A node with `$type` is a reference, resolved on
		// service load (ResolveTypeRefs). A conflict with type:/properties:/items: is
		// caught by validateInputSchemaNode (input_type_ref_conflict).
		"$type": true,
	}

	// inputSourceKnownKeys — the closed sub-key set of the source: object-discriminator
	// (ADR-044 S-T1). Any other sub-key is unknown_key.
	inputSourceKnownKeys = map[string]bool{
		"incarnation_hosts": true,
		"choir":             true,
	}
)

// UnmarshalYAML — custom decode to resolve the two meanings of `required`.
//
// goccy calls this method instead of the standard reflect-decode, passing the AST
// node. We:
//  1. peel off the sub-mapping of known keys, resolving `required` separately;
//  2. for the rest, call the shared goccy decoder via a temporary alias type (to
//     avoid recursion into UnmarshalYAML).
//
// If the `required` value is a bool → `Required` (kind=requiredBool). If a sequence of
// strings → `RequiredProps` (kind=requiredList). If anything else → kind=requiredAbsent
// and `input_required_value_invalid` is raised in semantic-validate (by inspecting the AST).
func (s *InputSchema) UnmarshalYAML(node ast.Node) error {
	m, ok := node.(*ast.MappingNode)
	if !ok {
		return fmt.Errorf("input schema must be a mapping, got %T", node)
	}
	// Snapshot the `required`, `additional_properties` and `$type` nodes and cut them
	// out of the mapping before the shared decode: all three are fields with special
	// semantics (see doc comments) that goccy cannot type correctly (`$type` is also an
	// invalid Go-yaml tag with `$`).
	var reqNode, apNode, typeRefNode ast.Node
	filtered := &ast.MappingNode{
		BaseNode:    m.BaseNode,
		Start:       m.Start,
		End:         m.End,
		IsFlowStyle: m.IsFlowStyle,
		Values:      make([]*ast.MappingValueNode, 0, len(m.Values)),
	}
	for _, kv := range m.Values {
		tok := kv.Key.GetToken()
		if tok != nil {
			switch tok.Value {
			case "required":
				reqNode = kv.Value
				continue
			case "additional_properties":
				apNode = kv.Value
				continue
			case "$type":
				typeRefNode = kv.Value
				continue
			}
		}
		filtered.Values = append(filtered.Values, kv)
	}

	// Decode "everything except required/additional_properties" via the alias type to
	// avoid recursion into UnmarshalYAML. We use yaml.NodeToValue the same way as the
	// shared parser.
	type rawSchema InputSchema
	var raw rawSchema
	if err := yaml.NodeToValue(filtered, &raw); err != nil {
		return err
	}
	*s = InputSchema(raw)

	// additional_properties: bool | schema. A schema needs recursive decode via
	// *InputSchema, else recurseItemsProperties does not know the child schema's type
	// (see Bug 2 — pattern on integer inside an AP schema was not caught).
	switch n := apNode.(type) {
	case nil:
		// not present
	case *ast.BoolNode:
		s.AdditionalProperties = n.Value
	case *ast.MappingNode:
		sub := &InputSchema{}
		if err := sub.UnmarshalYAML(n); err != nil {
			return err
		}
		s.AdditionalProperties = sub
	default:
		// Any other type — `validateObjectSchema` raises type_mismatch over the AST;
		// leave nil here.
	}

	// $type: <Name> — a reference to a named type. Only a string value is allowed;
	// other node kinds (mapping/sequence/number) leave TypeRef empty, and
	// validateInputSchemaNode raises type_mismatch over the AST.
	if sn, ok := typeRefNode.(*ast.StringNode); ok {
		s.TypeRef = sn.Value
	}

	// Parse `required` by node type.
	s.rawRequired = reqNode
	switch n := reqNode.(type) {
	case nil:
		s.requiredKind = requiredAbsent
	case *ast.BoolNode:
		s.Required = n.Value
		s.requiredKind = requiredBool
	case *ast.SequenceNode:
		s.RequiredProps = make([]string, 0, len(n.Values))
		for _, item := range n.Values {
			if sn, ok := item.(*ast.StringNode); ok {
				s.RequiredProps = append(s.RequiredProps, sn.Value)
				continue
			}
			tok := item.GetToken()
			if tok != nil {
				s.RequiredProps = append(s.RequiredProps, tok.Value)
			}
		}
		s.requiredKind = requiredList
	default:
		// Any other type (number, mapping, scalar string, null) — leave requiredAbsent;
		// `validateInputSchemaNode` raises `input_required_value_invalid` from the saved
		// rawRequired node.
		s.requiredKind = requiredAbsent
	}

	return nil
}

// validateInputSchemaMap is the public entry point for recursive validation of an
// input: (or output:) block. `pathPrefix` is the yaml-path to the block itself (e.g.
// `$.input` or `$.output`). `node` is the corresponding AST MappingNode (nil is safe,
// no checks run).
func validateInputSchemaMap(m InputSchemaMap, node *ast.MappingNode, pathPrefix string) []diag.Diagnostic {
	if m == nil && node == nil {
		return nil
	}
	if node == nil {
		// schema checks without positions — better nothing than a crash.
		return nil
	}
	var out []diag.Diagnostic
	for _, kv := range node.Values {
		keyTok := kv.Key.GetToken()
		if keyTok == nil {
			continue
		}
		paramName := keyTok.Value
		paramPath := pathPrefix + "." + paramName
		if !reInputParamName.MatchString(paramName) {
			out = append(out, diagAt(keyTok.Position.Line, keyTok.Position.Column, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:     "input_param_name_invalid",
				Message:  fmt.Sprintf("input parameter name %q does not match %s", paramName, reInputParamName),
				Hint:     "snake_case: starts with lowercase letter; only [a-z0-9_]; no dots/spaces — they break input.<name> in templates",
				YAMLPath: paramPath,
			}))
		}
		paramNode, ok := kv.Value.(*ast.MappingNode)
		if !ok {
			out = append(out, diagAt(keyTok.Position.Line, keyTok.Position.Column, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:     "type_mismatch",
				Message:  fmt.Sprintf("input parameter %q must be a mapping", paramName),
				YAMLPath: paramPath,
			}))
			continue
		}
		var schema *InputSchema
		if m != nil {
			schema = m[paramName]
		}
		out = append(out, validateInputSchemaNode(schema, paramNode, paramPath)...)
	}
	return out
}

// validateInputSchemaNode validates one schema (one parameter). Does unknown_key +
// per-key schema checks + recursion into items/properties.
func validateInputSchemaNode(s *InputSchema, node *ast.MappingNode, path string) []diag.Diagnostic {
	if node == nil {
		return nil
	}
	var out []diag.Diagnostic

	// Collect present keys and their AST positions.
	present := map[string]*ast.MappingValueNode{}
	for _, kv := range node.Values {
		keyTok := kv.Key.GetToken()
		if keyTok == nil {
			continue
		}
		name := keyTok.Value
		if !inputSchemaKnownKeys[name] {
			// `x-*` — vendor extension (OpenAPI/JSON-Schema convention): the backend
			// passes such keys into the raw input_schema DTO as UI annotations (e.g.
			// `x-directives: redis` — validate keys against the directive catalog,
			// NIM-76). Not validated — passthrough, not unknown_key.
			if strings.HasPrefix(name, "x-") {
				continue
			}
			out = append(out, diagAt(keyTok.Position.Line, keyTok.Position.Column, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:     "unknown_key",
				Message:  `unknown field "` + name + `"`,
				YAMLPath: path + "." + name,
			}))
			continue
		}
		present[name] = kv
	}

	// $type — a reference to a named type. A node with `$type` does NOT declare `type`/
	// its own shape — it is replaced by the type schema on service resolution
	// (ResolveTypeRefs). So such a node skips the per-type validation below
	// (type-required + per-key checks) — here only the REFERENCE's structural
	// invariants: a non-empty string name + no conflicting keys.
	if refKV, ok := present["$type"]; ok {
		out = append(out, validateTypeRefNode(s, refKV, present, path)...)
		return out
	}

	// type — required.
	if _, ok := present["type"]; !ok {
		out = append(out, diagAt(node.GetToken().Position.Line, node.GetToken().Position.Column, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "missing_required_field",
			Message:  "input parameter must declare type",
			Hint:     "type: one of string|integer|number|boolean|array|object",
			YAMLPath: path + ".type",
		}))
		// Without type, further per-type checks are impossible. But we still validate
		// items/properties to surface their errors too.
		out = append(out, recurseItemsProperties(s, present, path)...)
		return out
	}

	if s == nil {
		// no schema (decode-fatal earlier — the diag was already emitted).
		return out
	}

	// `required:` with a non-bool / non-sequence value (e.g. `required: "foo"`,
	// `required: 1`, `required: null`). UnmarshalYAML classifies that as requiredAbsent
	// and keeps the original node in rawRequired; here we raise the diagnostic. A nil
	// node (key absent) and BoolNode / SequenceNode are skipped.
	if s.rawRequired != nil {
		switch s.rawRequired.(type) {
		case *ast.BoolNode, *ast.SequenceNode:
			// valid forms — already parsed
		default:
			if kv, ok := present["required"]; ok {
				tok := kv.Value.GetToken()
				out = append(out, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
					Level: diag.LevelError, Phase: diag.PhaseSemanticValidate,
					Code:     "input_required_value_invalid",
					Message:  "required must be bool (parameter-level) or sequence of strings (object property list)",
					Hint:     "use `required: true/false` for any type; `required: [a, b]` only inside type=object",
					YAMLPath: path + ".required",
				}))
			}
		}
	}

	// required_when — a statically parsable CEL over input.* (docs/input.md →
	// "Conditional requiredness"). Applies to any type, so checked before the
	// per-type fork. An unparsable/invalid expression → input_required_when_invalid.
	if kv, ok := present["required_when"]; ok {
		out = append(out, validateRequiredWhen(s, kv, path)...)
	}

	// prefill_from_state — a `state.<path>` (dot notation) operational UI prefill hint
	// (docs/input.md → "Pre-fill from state"). Applies to any type (the field shape
	// resolves from a state value of any type), so checked before the per-type fork. An
	// invalid path → input_prefill_from_state_invalid.
	if kv, ok := present["prefill_from_state"]; ok {
		out = append(out, validatePrefillFromState(s, kv, path)...)
	}

	// type — enum.
	if !contains(inputTypeEnum, s.Type) {
		tkv := present["type"]
		tt := tkv.Value.GetToken()
		out = append(out, diagAt(tt.Position.Line, tt.Position.Column, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "input_type_invalid",
			Message:  fmt.Sprintf("type %q is not in %v", s.Type, inputTypeEnum),
			YAMLPath: path + ".type",
		}))
		// on an unknown type, keep validating recursion without the per-type fork.
		out = append(out, recurseItemsProperties(s, present, path)...)
		return out
	}

	// per-key checks "key applicable to this type".
	out = append(out, checkKeyTypeApplicability(s, present, path)...)

	// per-type checks.
	switch s.Type {
	case "string":
		out = append(out, validateStringSchema(s, present, path)...)
	case "integer", "number":
		out = append(out, validateNumericSchema(s, present, path)...)
	case "boolean":
		// nothing specific; only the common constraints.
	case "array":
		out = append(out, validateArraySchema(s, present, path)...)
	case "object":
		out = append(out, validateObjectSchema(s, present, path)...)
	}

	// source — structural validity of the source catalog (ADR-044 S-T1). Applicability
	// by type is already checked by checkKeyTypeApplicability; here — the discriminator
	// form + (for array) items.type=string.
	if _, has := present["source"]; has {
		out = append(out, validateSource(s, present, path)...)
	}

	// common cross-invariants.
	out = append(out, validateCommonInvariants(s, present, path)...)

	// recurse into items/properties — after the other checks of the current level.
	out = append(out, recurseItemsProperties(s, present, path)...)

	return out
}

// checkKeyTypeApplicability catches keys applied to an inapplicable type. Acts only
// when the type is valid (else false positives).
func checkKeyTypeApplicability(s *InputSchema, present map[string]*ast.MappingValueNode, path string) []diag.Diagnostic {
	type rule struct {
		key       string
		onlyTypes []string
	}
	rules := []rule{
		{"pattern", []string{"string"}},
		{"format", []string{"string"}},
		{"min_length", []string{"string"}},
		{"max_length", []string{"string"}},
		{"allow_empty", []string{"string"}},
		{"vault_scope", []string{"string"}},

		{"min", []string{"integer", "number"}},
		{"max", []string{"integer", "number"}},
		{"exclusive_min", []string{"integer", "number"}},
		{"exclusive_max", []string{"integer", "number"}},

		{"items", []string{"array"}},
		{"min_items", []string{"array"}},
		{"max_items", []string{"array"}},
		{"unique", []string{"array"}},

		{"properties", []string{"object"}},
		{"additional_properties", []string{"object"}},

		// source applies to single choice (string) and multi choice (array); for array
		// it additionally requires items.type=string — checked in validateSource
		// (structural validity of the source).
		{"source", []string{"string", "array"}},
	}
	var out []diag.Diagnostic
	for _, r := range rules {
		kv, ok := present[r.key]
		if !ok {
			continue
		}
		if !contains(r.onlyTypes, s.Type) {
			tok := kv.Key.GetToken()
			out = append(out, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:     "input_key_invalid_for_type",
				Message:  fmt.Sprintf("key %q is not applicable to type %q", r.key, s.Type),
				Hint:     fmt.Sprintf("allowed only for type %v", r.onlyTypes),
				YAMLPath: path + "." + r.key,
			}))
		}
	}
	// `required` as []string — only for type=object.
	if s.requiredKind == requiredList && s.Type != "object" {
		if kv, ok := present["required"]; ok {
			tok := kv.Key.GetToken()
			out = append(out, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:     "input_key_invalid_for_type",
				Message:  fmt.Sprintf(`"required" as list is only allowed for type "object", got type %q`, s.Type),
				Hint:     "use `required: true/false` (bool) for parameter-level requirement; list form is for object properties",
				YAMLPath: path + ".required",
			}))
		}
	}
	return out
}

func validateStringSchema(s *InputSchema, present map[string]*ast.MappingValueNode, path string) []diag.Diagnostic {
	var out []diag.Diagnostic
	if s.Pattern != "" {
		if _, err := regexp.Compile(s.Pattern); err != nil {
			kv := present["pattern"]
			tok := kv.Value.GetToken()
			out = append(out, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:     "input_pattern_invalid",
				Message:  fmt.Sprintf("pattern does not compile as RE2: %v", err),
				YAMLPath: path + ".pattern",
			}))
		}
	}
	if s.Format != "" && !contains(inputFormatEnum, s.Format) {
		kv := present["format"]
		tok := kv.Value.GetToken()
		out = append(out, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "input_format_invalid",
			Message:  fmt.Sprintf("format %q is not in %v", s.Format, inputFormatEnum),
			YAMLPath: path + ".format",
		}))
	}
	if s.Pattern != "" && s.Format != "" {
		kv := present["pattern"]
		tok := kv.Key.GetToken()
		out = append(out, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
			Level: diag.LevelWarning, Phase: diag.PhaseSchemaValidate,
			Code:     "input_pattern_format_conflict",
			Message:  "both `pattern` and `format` declared; format takes precedence",
			Hint:     "use one — `format` for known kinds, `pattern` for custom",
			YAMLPath: path + ".pattern",
		}))
	}
	if s.MinLength != nil && *s.MinLength < 0 {
		out = append(out, diagAtKV(present["min_length"], diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code: "value_out_of_range", Message: "min_length must be >= 0",
			YAMLPath: path + ".min_length",
		}))
	}
	if s.MaxLength != nil && *s.MaxLength < 0 {
		out = append(out, diagAtKV(present["max_length"], diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code: "value_out_of_range", Message: "max_length must be >= 0",
			YAMLPath: path + ".max_length",
		}))
	}
	if s.MinLength != nil && s.MaxLength != nil && *s.MinLength > *s.MaxLength {
		out = append(out, diagAtKV(present["max_length"], diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "value_out_of_range",
			Message:  fmt.Sprintf("max_length (%d) must be >= min_length (%d)", *s.MaxLength, *s.MinLength),
			YAMLPath: path + ".max_length",
		}))
	}
	if s.AllowEmpty && s.MinLength != nil && *s.MinLength >= 1 {
		out = append(out, diagAtKV(present["allow_empty"], diag.Diagnostic{
			Level: diag.LevelWarning, Phase: diag.PhaseSchemaValidate,
			Code:     "input_allow_empty_min_length_conflict",
			Message:  fmt.Sprintf("allow_empty: true conflicts with min_length: %d", *s.MinLength),
			Hint:     "drop allow_empty if min_length >= 1; empty string would never pass",
			YAMLPath: path + ".allow_empty",
		}))
	}
	if _, has := present["vault_scope"]; has {
		out = append(out, validateVaultScope(s, present, path)...)
	}
	return out
}

// validateVaultScope — semantics of the `vault_scope` key (docs/input.md →
// "vault_scope"). Applies only to a secret field: vault_scope grants the operator a
// scoped Vault read via an input ref, which is meaningful only for secrets. The glob
// itself must be non-empty and carry a prefix (`<mount>/...`), else there is no
// narrowing. type=string is already guaranteed by checkKeyTypeApplicability.
func validateVaultScope(s *InputSchema, present map[string]*ast.MappingValueNode, path string) []diag.Diagnostic {
	var out []diag.Diagnostic
	if !s.Secret {
		out = append(out, diagAtKV(present["vault_scope"], diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSemanticValidate,
			Code:     "input_vault_scope_requires_secret",
			Message:  "vault_scope is only allowed on secret: true fields",
			Hint:     "add `secret: true` — vault_scope grants scoped Vault read for the field value",
			YAMLPath: path + ".vault_scope",
		}))
	}
	if !validVaultScopeGlob(s.VaultScope) {
		out = append(out, diagAtKV(present["vault_scope"], diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSemanticValidate,
			Code:     "input_vault_scope_invalid",
			Message:  fmt.Sprintf("vault_scope %q is not a valid prefix-glob", s.VaultScope),
			Hint:     "form: `<mount>/<path-prefix>/*` (one trailing `*`), e.g. `secret/services/redis/*`",
			YAMLPath: path + ".vault_scope",
		}))
	}
	return out
}

// validatePrefillFromState statically checks the syntax of the `prefill_from_state`
// path (docs/input.md → "Pre-fill from state"). Applies to any type. The path must be
// a non-empty dot form `state.<path>` (`state` root + ≥1 snake_case segment): without
// the `state` root / empty / with invalid segments → input_prefill_from_state_invalid.
// Resolving the value from incarnation.state and the path whitelist is done by the
// form-prefill endpoint's backend — only syntax here (symmetric with validateSource:
// the schema checks the form, the backend resolves the set). kv is the
// `prefill_from_state` MappingValueNode.
func validatePrefillFromState(s *InputSchema, kv *ast.MappingValueNode, path string) []diag.Diagnostic {
	if s == nil {
		return nil
	}
	if !rePrefillFromStatePath.MatchString(s.PrefillFromState) {
		return []diag.Diagnostic{diagAtKV(kv, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSemanticValidate,
			Code:     "input_prefill_from_state_invalid",
			Message:  fmt.Sprintf("prefill_from_state %q is not a valid state.<path> reference", s.PrefillFromState),
			Hint:     `form: state.<field>[.<field>…], snake_case segments — e.g. prefill_from_state: state.redis_version`,
			YAMLPath: path + ".prefill_from_state",
		})}
	}
	return nil
}

// validateSource — structural validity of the `source:` catalog (ADR-044 S-T1).
// Checks ONLY the form: source is a mapping; sub-keys from inputSourceKnownKeys; each
// sub-key's value type is correct (`incarnation_hosts` — bool, `choir` — string). The
// set itself (resolving incarnation / Choir-part SIDs) and the "value ∈ set" check are
// done by the backend at form preparation — only syntax here.
//
// Also: for type=array, source is meaningful only when items.type=string (multi choice
// of SIDs). Applicability of source to the type itself is already checked by
// checkKeyTypeApplicability (string|array); here the array→items part is added.
func validateSource(s *InputSchema, present map[string]*ast.MappingValueNode, path string) []diag.Diagnostic {
	var out []diag.Diagnostic
	kv := present["source"]
	srcNode, ok := kv.Value.(*ast.MappingNode)
	if !ok {
		out = append(out, diagAtKV(kv, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "input_source_invalid",
			Message:  "source must be a mapping (object-discriminator)",
			Hint:     "source: { incarnation_hosts: true } or source: { choir: <name> }",
			YAMLPath: path + ".source",
		}))
		return out
	}

	for _, sub := range srcNode.Values {
		keyTok := sub.Key.GetToken()
		if keyTok == nil {
			continue
		}
		name := keyTok.Value
		if !inputSourceKnownKeys[name] {
			out = append(out, diagAt(keyTok.Position.Line, keyTok.Position.Column, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:     "unknown_key",
				Message:  `unknown field "` + name + `" in source`,
				Hint:     "known source keys: incarnation_hosts (bool), choir (string)",
				YAMLPath: path + ".source." + name,
			}))
			continue
		}
		out = append(out, validateSourceSubKey(name, sub, path)...)
	}

	// Discriminator invariant: EXACTLY ONE active source (see the InputSource doc
	// comment). incarnation_hosts counts as active only when true, and choir only when
	// a non-empty string: `incarnation_hosts: false` / `choir: ""` / an empty
	// `source: {}` give 0 active, two set give 2. Anything != 1 is an error. (An empty
	// choir is caught separately by validateSourceSubKey with a meaningful message; here
	// we just do not count it active, to avoid duplicating the diagnostic.)
	active := 0
	if s.Source != nil {
		if s.Source.IncarnationHosts {
			active++
		}
		if s.Source.Choir != "" {
			active++
		}
	}
	if active != 1 {
		out = append(out, diagAtKV(kv, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSemanticValidate,
			Code:     "input_source_invalid",
			Message:  fmt.Sprintf("source must declare exactly one active catalog, got %d", active),
			Hint:     "set exactly one: incarnation_hosts: true OR choir: <name>",
			YAMLPath: path + ".source",
		}))
	}

	// For array, source requires items.type=string (multi choice of SIDs).
	if s.Type == "array" && (s.Items == nil || s.Items.Type != "string") {
		out = append(out, diagAtKV(kv, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSemanticValidate,
			Code:     "input_source_invalid_for_type",
			Message:  "source on type=array requires items.type=string",
			Hint:     "multi-select from catalog - array of strings (SID); set items: { type: string }",
			YAMLPath: path + ".source",
		}))
	}

	return out
}

// validateSourceSubKey checks the value type of one known source sub-key.
// incarnation_hosts — bool, choir — a non-empty string.
func validateSourceSubKey(name string, sub *ast.MappingValueNode, path string) []diag.Diagnostic {
	subPath := path + ".source." + name
	switch name {
	case "incarnation_hosts":
		if _, ok := sub.Value.(*ast.BoolNode); !ok {
			tok := sub.Value.GetToken()
			return []diag.Diagnostic{diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:     "type_mismatch",
				Message:  "source.incarnation_hosts must be bool",
				YAMLPath: subPath,
			})}
		}
	case "choir":
		sn, ok := sub.Value.(*ast.StringNode)
		if !ok {
			tok := sub.Value.GetToken()
			return []diag.Diagnostic{diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:     "type_mismatch",
				Message:  "source.choir must be a string (Choir name)",
				YAMLPath: subPath,
			})}
		}
		if sn.Value == "" {
			tok := sub.Value.GetToken()
			return []diag.Diagnostic{diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSemanticValidate,
				Code:     "input_source_invalid",
				Message:  "source.choir must not be empty",
				YAMLPath: subPath,
			})}
		}
	}
	return nil
}

func validateNumericSchema(s *InputSchema, present map[string]*ast.MappingValueNode, path string) []diag.Diagnostic {
	var out []diag.Diagnostic
	if s.Min != nil && s.ExclusiveMin != nil {
		out = append(out, diagAtKV(present["exclusive_min"], diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "input_min_conflict",
			Message:  "min and exclusive_min are mutually exclusive",
			YAMLPath: path + ".exclusive_min",
		}))
	}
	if s.Max != nil && s.ExclusiveMax != nil {
		out = append(out, diagAtKV(present["exclusive_max"], diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "input_max_conflict",
			Message:  "max and exclusive_max are mutually exclusive",
			YAMLPath: path + ".exclusive_max",
		}))
	}
	// min <= max if both set (inclusive bounds).
	if s.Min != nil && s.Max != nil && *s.Min > *s.Max {
		out = append(out, diagAtKV(present["max"], diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "value_out_of_range",
			Message:  fmt.Sprintf("max (%v) must be >= min (%v)", *s.Max, *s.Min),
			YAMLPath: path + ".max",
		}))
	}
	return out
}

func validateArraySchema(s *InputSchema, present map[string]*ast.MappingValueNode, path string) []diag.Diagnostic {
	var out []diag.Diagnostic
	// items — required for array.
	if _, ok := present["items"]; !ok {
		out = append(out, diagAtKV(present["type"], diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "missing_required_field",
			Message:  "array parameter must declare items",
			Hint:     "items: <schema> — defines element shape, recursively",
			YAMLPath: path + ".items",
		}))
	}
	if s.MinItems != nil && *s.MinItems < 0 {
		out = append(out, diagAtKV(present["min_items"], diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code: "value_out_of_range", Message: "min_items must be >= 0",
			YAMLPath: path + ".min_items",
		}))
	}
	if s.MaxItems != nil && *s.MaxItems < 0 {
		out = append(out, diagAtKV(present["max_items"], diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code: "value_out_of_range", Message: "max_items must be >= 0",
			YAMLPath: path + ".max_items",
		}))
	}
	if s.MinItems != nil && s.MaxItems != nil && *s.MinItems > *s.MaxItems {
		out = append(out, diagAtKV(present["max_items"], diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "value_out_of_range",
			Message:  fmt.Sprintf("max_items (%d) must be >= min_items (%d)", *s.MaxItems, *s.MinItems),
			YAMLPath: path + ".max_items",
		}))
	}
	return out
}

func validateObjectSchema(s *InputSchema, present map[string]*ast.MappingValueNode, path string) []diag.Diagnostic {
	var out []diag.Diagnostic
	if _, ok := present["properties"]; !ok {
		out = append(out, diagAtKV(present["type"], diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "missing_required_field",
			Message:  "object parameter must declare properties",
			Hint:     "properties: { <name>: <schema>, ... }",
			YAMLPath: path + ".properties",
		}))
	}
	// additional_properties — bool or mapping (schema). No other types.
	if kv, ok := present["additional_properties"]; ok {
		v := kv.Value
		_, isMap := v.(*ast.MappingNode)
		_, isBool := v.(*ast.BoolNode)
		if !isMap && !isBool {
			tok := v.GetToken()
			out = append(out, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:     "type_mismatch",
				Message:  "additional_properties must be bool or schema (mapping)",
				YAMLPath: path + ".additional_properties",
			}))
		}
	}
	// `required: [name1, name2]` — each name must be among properties.
	if s.requiredKind == requiredList && s.Properties != nil {
		for _, name := range s.RequiredProps {
			if _, ok := s.Properties[name]; !ok {
				out = append(out, diagAtKV(present["required"], diag.Diagnostic{
					Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
					Code:     "missing_required_field",
					Message:  fmt.Sprintf("required references unknown property %q", name),
					Hint:     "every entry of `required` must match a key in `properties`",
					YAMLPath: path + ".required",
				}))
			}
		}
	}
	return out
}

// validateCommonInvariants — cross-checks on common keys (enum/default/required).
func validateCommonInvariants(s *InputSchema, present map[string]*ast.MappingValueNode, path string) []diag.Diagnostic {
	var out []diag.Diagnostic

	// enum for array/object is forbidden in MVP. Array and object literals are not
	// comparable in the Go runtime, and reflect.DeepEqual for the rare "composite enum
	// literals" is over-engineering until a real request. See the ADR comment in
	// input.md (post-MVP). The enum check below and `default in enum` below are skipped
	// for these types.
	enumOnComposite := len(s.Enum) > 0 && (s.Type == "array" || s.Type == "object")
	if enumOnComposite {
		out = append(out, diagAtKV(present["enum"], diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSemanticValidate,
			Code:     "input_enum_unsupported_for_type",
			Message:  fmt.Sprintf("enum is unsupported for type=%s in MVP", s.Type),
			Hint:     "enum literals of arrays/objects are post-MVP; use a different type or drop enum",
			YAMLPath: path + ".enum",
		}))
	}

	// enum — each element matches type. For array/object we skip, the diagnostic was
	// already emitted above.
	if len(s.Enum) > 0 && !enumOnComposite {
		for i, v := range s.Enum {
			if !valueMatchesType(v, s.Type) {
				out = append(out, diagAtKV(present["enum"], diag.Diagnostic{
					Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
					Code:     "input_enum_type_mismatch",
					Message:  fmt.Sprintf("enum[%d] = %s does not match type %q", i, formatLiteral(v), s.Type),
					YAMLPath: path + ".enum",
				}))
			}
		}
	}

	// default — matches type. Special case: a string default may be a CEL/template
	// expression (`${ ... }` or `{{ ... }}`); we skip those — for type=string any
	// string is syntactically valid.
	if _, has := present["default"]; has {
		if !defaultMatchesType(s.Default, s.Type) {
			out = append(out, diagAtKV(present["default"], diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:     "input_default_type_mismatch",
				Message:  fmt.Sprintf("default = %s does not match type %q", formatLiteral(s.Default), s.Type),
				YAMLPath: path + ".default",
			}))
		} else {
			// Top-level type ok — descend into content for array/object so we do not
			// miss mismatching elements/fields inside the literal.
			out = append(out, validateDefaultContent(s, present["default"], path+".default")...)
		}

		// default must be in enum if both are set. For array/object enum is forbidden
		// (see above) — we skip, else we would have to compare composite literals.
		// Without this, the hidden-mismatch check "choice only from enum, but default
		// is outside" remains for scalar types.
		if len(s.Enum) > 0 && !enumOnComposite {
			if !enumContains(s.Enum, s.Default) {
				out = append(out, diagAtKV(present["default"], diag.Diagnostic{
					Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
					Code:     "input_default_not_in_enum",
					Message:  fmt.Sprintf("default = %s is not in enum %s", formatLiteral(s.Default), formatEnum(s.Enum)),
					Hint:     "default must be one of enum values (or drop enum)",
					YAMLPath: path + ".default",
				}))
			}
		}
	}

	// required: true + default → conflict (warning).
	if s.requiredKind == requiredBool && s.Required {
		if _, has := present["default"]; has {
			out = append(out, diagAtKV(present["default"], diag.Diagnostic{
				Level: diag.LevelWarning, Phase: diag.PhaseSchemaValidate,
				Code:     "input_required_default_conflict",
				Message:  "required: true together with default is contradictory",
				Hint:     "drop one — `default` already implies optional",
				YAMLPath: path + ".default",
			}))
		}
	}

	return out
}

// recurseItemsProperties recurses into items (array), properties (object) and
// additional_properties (object, when given as a schema, not a bool). Done separately
// from per-type so it also descends into a schema without type (there we already
// emitted missing_required_field, but we still want to surface nested errors in one
// pass).
func recurseItemsProperties(s *InputSchema, present map[string]*ast.MappingValueNode, path string) []diag.Diagnostic {
	var out []diag.Diagnostic
	if kv, ok := present["items"]; ok {
		if itemNode, isMap := kv.Value.(*ast.MappingNode); isMap {
			var sub *InputSchema
			if s != nil {
				sub = s.Items
			}
			out = append(out, validateInputSchemaNode(sub, itemNode, path+".items")...)
		}
	}
	if kv, ok := present["properties"]; ok {
		if propsNode, isMap := kv.Value.(*ast.MappingNode); isMap {
			var sub InputSchemaMap
			if s != nil {
				sub = s.Properties
			}
			out = append(out, validateInputSchemaMap(sub, propsNode, path+".properties")...)
		}
	}
	if kv, ok := present["additional_properties"]; ok {
		// additional_properties: <schema> — the "map of arbitrary keys with a shared
		// value schema" form (see examples/destiny/redis → users). A bare bool is not
		// validated (no nested schema there).
		if apNode, isMap := kv.Value.(*ast.MappingNode); isMap {
			var sub *InputSchema
			if s != nil {
				if ap, ok := s.AdditionalProperties.(*InputSchema); ok {
					sub = ap
				}
			}
			out = append(out, validateInputSchemaNode(sub, apNode, path+".additional_properties")...)
		}
	}
	return out
}

// valueMatchesType — true if a literal Go value matches the input type. Used for enum
// and default. `array` / `object` are deliberately transparent: for enum and default
// literals "type match" means "not contradictory" — the native YAML decode already
// typed the scalar.
func valueMatchesType(v any, t string) bool {
	switch t {
	case "string":
		_, ok := v.(string)
		return ok
	case "boolean":
		_, ok := v.(bool)
		return ok
	case "integer":
		switch x := v.(type) {
		case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
			return true
		case float64:
			// goccy YAML yields `42` as uint64; `42.0` as float64. If the literal is an
			// integer in a float wrapper, still ok.
			return x == float64(int64(x))
		}
		return false
	case "number":
		switch v.(type) {
		case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64,
			float32, float64:
			return true
		}
		return false
	case "array":
		_, ok := v.([]any)
		return ok
	case "object":
		_, ok := v.(map[string]any)
		return ok
	}
	return false
}

// defaultMatchesType wraps valueMatchesType with a relaxation for type=string: a
// literal with `${ ... }` or `{{ ... }}` is a valid expression default — any string
// form is valid.
func defaultMatchesType(v any, t string) bool {
	if t == "string" {
		_, ok := v.(string)
		return ok
	}
	return valueMatchesType(v, t)
}

func formatLiteral(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case string:
		return strconv.Quote(x)
	default:
		return fmt.Sprintf("%v (%T)", x, x)
	}
}

// formatEnum renders an enum literal for diagnostics (stable order, same as in YAML —
// not sorted).
func formatEnum(es []any) string {
	out := make([]string, 0, len(es))
	for _, v := range es {
		out = append(out, formatLiteral(v))
	}
	return "[" + strings.Join(out, ", ") + "]"
}

// enumContains compares by YAML-scalar semantics: for numbers we treat mixed int/float
// exactly as in valueMatchesType (`42` ≡ `42.0`).
func enumContains(enum []any, v any) bool {
	for _, e := range enum {
		if equalScalar(e, v) {
			return true
		}
	}
	return false
}

// equalScalar — value equality with an int↔float relaxation so `default: 1` and
// `enum: [1, 2]` (where goccy types elements as uint64) match without a false positive.
//
// Defensive: slice/map are not comparable at runtime — `a == b` panics for them (seen
// on array enum literals before `input_enum_unsupported_for_type` was introduced).
// These types should not reach here — validateCommonInvariants cuts them off earlier;
// this is a safeguard for a future extension of enum to composites.
func equalScalar(a, b any) bool {
	if !isComparableScalar(a) || !isComparableScalar(b) {
		return false
	}
	if a == b {
		return true
	}
	af, aok := toFloat64(a)
	bf, bok := toFloat64(b)
	if aok && bok {
		return af == bf
	}
	return false
}

// isComparableScalar — true for values safe to compare with `==` (scalar Go types).
// Slice/map are not comparable at runtime → false.
func isComparableScalar(v any) bool {
	switch v.(type) {
	case nil, bool, string,
		int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64,
		float32, float64:
		return true
	}
	return false
}

func toFloat64(v any) (float64, bool) {
	switch x := v.(type) {
	case int:
		return float64(x), true
	case int8:
		return float64(x), true
	case int16:
		return float64(x), true
	case int32:
		return float64(x), true
	case int64:
		return float64(x), true
	case uint:
		return float64(x), true
	case uint8:
		return float64(x), true
	case uint16:
		return float64(x), true
	case uint32:
		return float64(x), true
	case uint64:
		return float64(x), true
	case float32:
		return float64(x), true
	case float64:
		return x, true
	}
	return 0, false
}

// validateDefaultContent recursively checks a default literal's content against the
// nested schema. Applied for array (each element against items.type) and object (each
// field against properties.<name>.type) at arbitrary nesting depth (qa.1 explicitly
// required recursion: a 1-level check missed mismatches in array[object[array]]).
//
// `kv` is the `default:` MappingValueNode (for line/col diagnostics; we do not descend
// into the AST — default is a literal, sub-elements have no AST positions).
//
// CEL/template wrappers `${...}` / `{{...}}` are left untouched inside
// (defaultMatchesType already skipped them at the top level; in array elements/object
// fields they do not occur in practice, so no wrapper parsing).
func validateDefaultContent(s *InputSchema, kv *ast.MappingValueNode, path string) []diag.Diagnostic {
	if s == nil || s.Default == nil {
		return nil
	}
	return validateDefaultValue(s, s.Default, kv, path)
}

// validateDefaultValue checks one value `v` against schema `sub` and recurses into
// array/object elements. A separate function (not inline in validateDefaultContent) so
// the recursion does not drag the top-level `s.Default` along — each call gets its own
// sub-value.
//
// On the root call the top-level type match is already checked by `defaultMatchesType`
// in validateCommonInvariants — so at the root level type-mismatch is not duplicated.
// The type comparison is done only when descending into child elements (see the
// array/object branches below).
func validateDefaultValue(sub *InputSchema, v any, kv *ast.MappingValueNode, path string) []diag.Diagnostic {
	if sub == nil || sub.Type == "" {
		return nil
	}
	var out []diag.Diagnostic
	switch sub.Type {
	case "array":
		arr, ok := v.([]any)
		if !ok || sub.Items == nil || sub.Items.Type == "" {
			return nil
		}
		for i, el := range arr {
			elPath := fmt.Sprintf("%s[%d]", path, i)
			if !valueMatchesType(el, sub.Items.Type) {
				out = append(out, diagAtKV(kv, diag.Diagnostic{
					Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
					Code:     "input_default_type_mismatch",
					Message:  fmt.Sprintf("default%s = %s does not match items.type %q", strings.TrimPrefix(elPath, path), formatLiteral(el), sub.Items.Type),
					YAMLPath: elPath,
				}))
				continue
			}
			out = append(out, validateDefaultValue(sub.Items, el, kv, elPath)...)
		}
	case "object":
		obj, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		for k, fv := range obj {
			prop, ok := sub.Properties[k]
			if !ok || prop == nil || prop.Type == "" {
				continue
			}
			fieldPath := path + "." + k
			if !valueMatchesType(fv, prop.Type) {
				out = append(out, diagAtKV(kv, diag.Diagnostic{
					Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
					Code:     "input_default_type_mismatch",
					Message:  fmt.Sprintf("default.%s = %s does not match properties.%s.type %q", k, formatLiteral(fv), k, prop.Type),
					YAMLPath: fieldPath,
				}))
				continue
			}
			out = append(out, validateDefaultValue(prop, fv, kv, fieldPath)...)
		}
	}
	return out
}

// diagAt is a convenience wrapper: sets line/col, leaves the rest.
func diagAt(line, col int, d diag.Diagnostic) diag.Diagnostic {
	d.Line = line
	d.Column = col
	return d
}

func diagAtKV(kv *ast.MappingValueNode, d diag.Diagnostic) diag.Diagnostic {
	if kv == nil {
		return d
	}
	tok := kv.Key.GetToken()
	if tok == nil {
		return d
	}
	d.Line = tok.Position.Line
	d.Column = tok.Position.Column
	return d
}

// findInputMapping finds and returns the MappingNode under a top-level block key
// (`input` / `output` / etc.). nil if the key is absent.
func findInputMapping(root *ast.MappingNode, topKey string) *ast.MappingNode {
	if root == nil {
		return nil
	}
	for _, kv := range root.Values {
		tok := kv.Key.GetToken()
		if tok == nil || tok.Value != topKey {
			continue
		}
		if m, ok := kv.Value.(*ast.MappingNode); ok {
			return m
		}
		return nil
	}
	return nil
}
