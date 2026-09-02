package config

// Reusable named types of the input schema (`service/<name>/types.yml`).
//
// A service may declare a type catalog — the `types:` section (name → schema in the
// same InputSchema DSL). Scenarios reference a type via the discriminator key `$type`:
// as a standalone field (`<param>: { $type: T }`) or an array element
// (`items: { $type: T }`). At service load the references are resolved —
// [ResolveTypeRefs] substitutes the type's schema in place of the reference node.
//
// Resolution is pure (no I/O): the caller (artifact/soul-lint) reads types.yml and
// the input schemas, parses them with `shared/config`, then calls [ParseTypeCatalog] +
// [ResolveTypeRefs]. This parallels resolving vars/templates resources at service
// load (see artifact/service.go): the catalog schema is validated once,
// the result is a self-contained input schema without `$type`.
//
// MVP scope (user's decision): object + array-of-type + type→type nesting
// with cycle-detection. WITHOUT scalar-alias, cross-service catalogs, and
// local-per-scenario types.

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/goccy/go-yaml"
	"github.com/goccy/go-yaml/ast"
	"github.com/goccy/go-yaml/parser"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// typesCatalogFile — the type-catalog file name at the Service repo root, next to
// `service.yml`/`scenario/`/`migrations/`. An exported constant — the caller
// (artifact/soul-lint) reads the file itself, the config package stays I/O-free.
const TypesCatalogFile = "types.yml"

// typesSectionKey — the only top-level key in `types.yml`.
const typesSectionKey = "types"

// reInputTypeName — a named type's name (the key in `types:` and the `$type:` value).
// A separate namespace from input params (snake_case): a type is a
// logical "class" of shape, hence strictly PascalCase `^[A-Z][A-Za-z0-9]*$`
// (naming-rules.md §"Named input types"). Starts with an uppercase letter; inside —
// only letters/digits (no `_`/dashes/dots/spaces): the name is written verbatim in
// `$type:` and must be an unambiguous class token.
var reInputTypeName = regexp.MustCompile(`^[A-Z][A-Za-z0-9]*$`)

// typeRefResolveLimit — a safety cap on the number of type-ref HOPS
// (A→$type B→$type C…) in case cycle-detection misses a pathology
// (it should not). ONLY a type-ref hop is counted — structural descent into items/
// properties/additional_properties does NOT increment depth (otherwise a deeply
// nested plain object gives a false input_type_cycle). The type-graph depth in
// real catalogs is single digits; 64 is amply sufficient and catches a runaway over
// the ref-graph without a stack overflow.
const typeRefResolveLimit = 64

// validateTypeRefNode — structural validity of one reference node `{$type: T}`.
// The node is already identified as a reference (has a `$type` key); per-type
// validation is skipped for it. We check the REFERENCE INVARIANTS:
//   - `$type` — a non-empty string of valid form (reInputTypeName);
//   - no conflict with an own shape: `type:`/`properties:`/`items:`
//     TOGETHER with `$type` → input_type_ref_conflict (unclear which wins).
//     `items: { $type: T }` is a PARENT-level items REFERENCE; it does NOT land
//     here (the parent = an array with items, not a node with `$type`); here any
//     `items` next to `$type` is exactly a conflict.
//
// In dialectState `properties:` is NOT a conflict but the deliberate exception
// ([NIM-740]): `state_schema` describes declared secrets as well as shape, and the
// secret of a collection element exists only at the point of USE — one AclUser sits
// under `redis_users` and under `system_acl_users`, and writing its password into
// the type would fix an address the type has no business fixing. So the reference
// may ADD properties on top of the type. The relaxation is scoped to this dialect on
// purpose: applied to `input:` it would reopen the deep-merge ADR-062 refused.
//
// The very existence of type T in the catalog is checked by [ResolveTypeRefs] (needs
// the catalog) — here only the node's local shape (symmetric to validateSource:
// shape locally, membership by the resolver).
func validateTypeRefNode(s *InputSchema, refKV *ast.MappingValueNode, present map[string]*ast.MappingValueNode, path string, dialect schemaDialect) []diag.Diagnostic {
	var out []diag.Diagnostic

	// $type — must be a non-empty string of valid form.
	if _, ok := refKV.Value.(*ast.StringNode); !ok {
		tok := refKV.Value.GetToken()
		out = append(out, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "type_mismatch",
			Message:  "$type must be a string (named type reference)",
			Hint:     "$type: <TypeName> — references an entry in service/<name>/types.yml",
			YAMLPath: path + ".$type",
		}))
	} else if ref := s.typeRef(); !reInputTypeName.MatchString(ref) {
		tok := refKV.Value.GetToken()
		msg := fmt.Sprintf("$type %q does not match %s", ref, reInputTypeName)
		if ref == "" {
			msg = "$type must be a non-empty type name"
		}
		out = append(out, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "input_type_ref_name_invalid",
			Message:  msg,
			Hint:     "type name: PascalCase — starts with an uppercase letter; only [A-Za-z0-9]; no underscores/dots/dashes/spaces",
			YAMLPath: path + ".$type",
		}))
	}

	// Conflict: $type TOGETHER with the node's own shape. Any of these keys
	// next to $type makes the node ambiguous.
	//
	// `additional_properties` is in the list because the resolver substitutes the
	// TYPE for a reference node and keeps none of the node's own shape: written here
	// it is not merged, not refused, just gone. A `type: secret` under it therefore
	// vanished with no diagnostic at any stage — the one outcome
	// [CollectSecretFields] exists to rule out. Refusing it is what makes that
	// impossible to write by accident.
	conflicting := []string{"type", "properties", "items", "additional_properties"}
	if dialect == dialectState {
		// `properties:` is the scoped exception (see the doc comment); the rest stay
		// ambiguous — the named type already supplies the shape.
		conflicting = []string{"type", "items", "additional_properties"}
	}
	for _, key := range conflicting {
		kv, ok := present[key]
		if !ok {
			continue
		}
		tok := kv.Key.GetToken()
		out = append(out, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSemanticValidate,
			Code:     "input_type_ref_conflict",
			Message:  fmt.Sprintf("$type cannot be combined with %q on the same node", key),
			Hint:     "a $type node is a reference — drop " + strings.Join(conflicting, ":/") + ": (the named type provides the shape)",
			YAMLPath: path + "." + key,
		}))
	}

	return out
}

// typeRef — a safe TypeRef getter (nil-safe).
func (s *InputSchema) typeRef() string {
	if s == nil {
		return ""
	}
	return s.TypeRef
}

// TypeCatalog — a service's resolved named-type catalog: name → schema.
// The values are self-contained input schemas (their `$type` references are already
// resolved when the catalog is built, with cycle-detection).
type TypeCatalog map[string]*InputSchema

// ParseTypeCatalog parses `types.yml` (top-level `types:` section: name → schema in
// the InputSchema DSL) and resolves `$type` references BETWEEN types with cycle-detection.
// data is the raw types.yml content; filename is for the diagnostics' File field.
//
// Returns the catalog (names → resolved schemas) and diagnostics. When there are
// error diagnostics the catalog may be partial/nil — the caller checks
// diag.HasErrors and does not use a broken catalog.
//
// Errors:
//   - input_type_duplicate — two types with the same name (key collision in `types:`);
//   - input_type_unknown   — `$type` references a type absent from the catalog;
//   - input_type_cycle     — a cyclic reference (A→$type B→$type A);
//   - plus the usual schema/semantic errors of the type schemas themselves (via
//     validateInputSchemaMap — type-required, name format, etc.).
//
// An empty/absent `types:` → an empty catalog without errors (a service without types
// is valid).
func ParseTypeCatalog(filename string, data []byte) (TypeCatalog, []diag.Diagnostic) {
	data = stripBOM(data)
	// AllowDuplicateMapKey: goccy would otherwise reject a duplicate type name with a
	// parse error; we allow the parse to raise the domain input_type_duplicate via the AST.
	file, err := parser.ParseBytes(data, parser.ParseComments, parser.AllowDuplicateMapKey())
	if err != nil {
		return nil, []diag.Diagnostic{yamlParseDiag(filename, err)}
	}
	if len(file.Docs) == 0 || file.Docs[0].Body == nil {
		// Empty file — empty catalog. types.yml is optional.
		return TypeCatalog{}, nil
	}
	root, ok := file.Docs[0].Body.(*ast.MappingNode)
	if !ok {
		// A comment-only / whitespace-only file → not a mapping, but a valid
		// "no types" (types.yml is optional). A scalar/sequence at the root — an error.
		switch file.Docs[0].Body.(type) {
		case *ast.CommentGroupNode, *ast.NullNode:
			return TypeCatalog{}, nil
		}
		tok := file.Docs[0].Body.GetToken()
		line, col := 0, 0
		if tok != nil {
			line, col = tok.Position.Line, tok.Position.Column
		}
		return nil, []diag.Diagnostic{{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			File: filename, Line: line, Column: col,
			Code:     "type_mismatch",
			Message:  fmt.Sprintf("types.yml root must be a mapping, got %T", file.Docs[0].Body),
			YAMLPath: "$",
		}}
	}

	typesNode := findInputMapping(root, typesSectionKey)
	// Decode the section into InputSchemaMap (the same DSL as input:). Decode the whole
	// file into a wrapper to reuse InputSchema's UnmarshalYAML.
	type typesFileYAML struct {
		Types InputSchemaMap `yaml:"types"`
	}
	var decoded typesFileYAML
	// AllowDuplicateMapKey at the decode phase: a duplicate type name is already allowed at
	// the parse phase (see above); without it NodeToValue would reject the decode with
	// `type_mismatch`, masking the domain input_type_duplicate (raised by the AST scan below).
	if derr := yaml.NodeToValue(root, &decoded, yaml.AllowDuplicateMapKey()); derr != nil {
		return nil, []diag.Diagnostic{decodeErrorDiag(filename, derr)}
	}

	var diags []diag.Diagnostic

	// Unknown top-level keys (only `types:` is allowed).
	for _, kv := range root.Values {
		tok := kv.Key.GetToken()
		if tok == nil || tok.Value == typesSectionKey {
			continue
		}
		diags = append(diags, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			File: filename, Code: "unknown_key",
			Message:  `unknown field "` + tok.Value + `"`,
			Hint:     "types.yml has a single top-level section: types:",
			YAMLPath: "$." + tok.Value,
		}))
	}

	if typesNode == nil {
		// No types: section (or it is not a mapping) → empty catalog.
		return TypeCatalog{}, withFile(diags, filename)
	}

	// input_type_duplicate — two types with the same name. goccy decodes the map
	// last-wins, losing the duplicate; catch it via the AST before it collapses.
	seen := map[string]bool{}
	for _, kv := range typesNode.Values {
		tok := kv.Key.GetToken()
		if tok == nil {
			continue
		}
		name := tok.Value
		if seen[name] {
			diags = append(diags, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSemanticValidate,
				File: filename, Code: "input_type_duplicate",
				Message:  fmt.Sprintf("type %q is declared more than once", name),
				Hint:     "each type name in types: must be unique",
				YAMLPath: "$.types." + name,
			}))
			continue
		}
		seen[name] = true
		if !reInputTypeName.MatchString(name) {
			diags = append(diags, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				File: filename, Code: "input_type_ref_name_invalid",
				Message:  fmt.Sprintf("type name %q does not match %s", name, reInputTypeName),
				Hint:     "type name: PascalCase — uppercase first letter, only [A-Za-z0-9]",
				YAMLPath: "$.types." + name,
			}))
		}
	}

	// Schema/semantic validation of the type schemas themselves (type-required, field format,
	// $type-ref-conflict inside a type, etc.). NOT via validateInputSchemaMap:
	// that applies the input-param snake_case regex to keys, but type names are
	// PascalCase (their own name check is already done in the duplicate loop above). Here
	// we validate only the BODY of each type. A $type reference INSIDE a type to another type
	// is allowed (type→type nesting); its existence is checked by the resolve below.
	for _, kv := range typesNode.Values {
		tok := kv.Key.GetToken()
		if tok == nil {
			continue
		}
		bodyNode, isMap := kv.Value.(*ast.MappingNode)
		if !isMap {
			diags = append(diags, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				File: filename, Code: "type_mismatch",
				Message:  fmt.Sprintf("type %q must be a schema (mapping)", tok.Value),
				YAMLPath: "$.types." + tok.Value,
			}))
			continue
		}
		diags = append(diags, validateInputSchemaNode(decoded.Types[tok.Value], bodyNode, "$.types."+tok.Value, dialectTypes)...)
	}

	// Resolve `$type` references between types with cycle-detection. We work on a COPY
	// of the schemas (resolveSchemaRefs mutates the subtree) so the catalog returns
	// self-contained values.
	catalog := make(TypeCatalog, len(decoded.Types))
	for name, schema := range decoded.Types {
		catalog[name] = schema
	}
	for name := range catalog {
		resolved, rdiags := resolveOneType(name, catalog, "$.types."+name)
		diags = append(diags, withFile(rdiags, filename)...)
		if resolved != nil {
			catalog[name] = resolved
		}
	}

	return catalog, withFile(diags, filename)
}

// resolveOneType resolves the schema of one type `name` from the catalog: recursively
// substitutes all `$type` references inside it, tracking the traversal path for
// cycle-detection. `path` is the yaml-path for diagnostics.
func resolveOneType(name string, catalog TypeCatalog, path string) (*InputSchema, []diag.Diagnostic) {
	schema := catalog[name]
	if schema == nil {
		return nil, nil
	}
	// The stack of active types on the current traversal branch — for cycle detection.
	stack := map[string]bool{name: true}
	// dialectTypes: a type body may declare a secret ([ADR-0086] §5) but does NOT get
	// the `$type` overlay. A type referencing another type and adding properties to it
	// is the deep merge ADR-062 refused — the exception is granted to the USE site,
	// where the added property's Vault address is readable, not to the catalog.
	return resolveSchemaRefs(schema, catalog, stack, path, 0, dialectTypes)
}

// resolveSchemaRefs returns a copy of `schema` with all `$type` references
// resolved by substituting schemas from the catalog. `stack` is the set of types on
// the current traversal branch (cycle-detection: re-entering a type on the stack =
// input_type_cycle). Resolution does NOT mutate the source schema — it builds a new one,
// so a shared type used twice in different branches resolves
// independently without a false cycle.
//
// `depth` is the number of type-ref HOPS traversed on the current branch (a cycle is a
// property of the ref-graph): incremented ONLY when entering a reference's target type.
// Structural descent into items/properties/additional_properties leaves depth untouched,
// otherwise a deeply nested plain object would falsely exceed the limit and give input_type_cycle.
func resolveSchemaRefs(schema *InputSchema, catalog TypeCatalog, stack map[string]bool, path string, depth int, dialect schemaDialect) (*InputSchema, []diag.Diagnostic) {
	if schema == nil {
		return nil, nil
	}
	if depth > typeRefResolveLimit {
		return schema, []diag.Diagnostic{{
			Level: diag.LevelError, Phase: diag.PhaseSemanticValidate,
			Code:     "input_type_cycle",
			Message:  fmt.Sprintf("type reference resolution exceeded %d ref-hops (possible cycle)", typeRefResolveLimit),
			YAMLPath: path,
		}}
	}

	// Reference node: substitute the target type's schema (recursively resolved).
	if schema.TypeRef != "" {
		ref := schema.TypeRef
		if stack[ref] {
			return nil, []diag.Diagnostic{{
				Level: diag.LevelError, Phase: diag.PhaseSemanticValidate,
				Code:     "input_type_cycle",
				Message:  fmt.Sprintf("type reference cycle detected at %q", ref),
				Hint:     "a type must not reference itself transitively (A → $type B → $type A)",
				YAMLPath: path + ".$type",
			}}
		}
		target, ok := catalog[ref]
		if !ok {
			return nil, []diag.Diagnostic{{
				Level: diag.LevelError, Phase: diag.PhaseSemanticValidate,
				Code:     "input_type_unknown",
				Message:  fmt.Sprintf("$type %q is not declared in types.yml", ref),
				Hint:     "declare the type under types: or fix the reference name",
				YAMLPath: path + ".$type",
			}}
		}
		stack[ref] = true
		resolved, diags := resolveSchemaRefs(target, catalog, stack, path, depth+1, dialect)
		delete(stack, ref)
		// The reference node's own keys (description/required/required_when) over the
		// resolved type — applyRefOverlay (does not touch the type's shape). Conflicting
		// keys (type/items, and properties outside dialectState) are already rejected
		// by the conflict check.
		if resolved != nil {
			applyRefOverlay(schema, resolved)
			if dialect == dialectState && len(schema.Properties) > 0 {
				diags = append(diags, applyRefProperties(schema, resolved, catalog, stack, path, depth)...)
			}
		}
		return resolved, diags
	}

	// Plain node: copy and recursively resolve items/properties/AP.
	// Structural descent does NOT increment depth — the limit counts only type-ref hops
	// (see the doc comment): a deep plain object must not trip it falsely.
	out := *schema
	var diags []diag.Diagnostic

	if schema.Items != nil {
		ri, d := resolveSchemaRefs(schema.Items, catalog, stack, path+".items", depth, dialect)
		out.Items = ri
		diags = append(diags, d...)
	}
	if len(schema.Properties) > 0 {
		props := make(InputSchemaMap, len(schema.Properties))
		for pn, ps := range schema.Properties {
			rp, d := resolveSchemaRefs(ps, catalog, stack, path+".properties."+pn, depth, dialect)
			props[pn] = rp
			diags = append(diags, d...)
		}
		out.Properties = props
	}
	if ap, ok := schema.AdditionalProperties.(*InputSchema); ok {
		rap, d := resolveSchemaRefs(ap, catalog, stack, path+".additional_properties", depth, dialect)
		out.AdditionalProperties = rap
		diags = append(diags, d...)
	}

	return &out, diags
}

// applyRefOverlay overlays the reference node's own keys `{ $type: T, … }`
// on top of the resolved type schema — surgically, without changing the type's shape:
//   - description — the reference's label;
//   - the reference's field-level requiredness (`required: <bool>`) → the Required
//     field. Written on the REFERENCE it says "this field is mandatory"; the type's own
//     properties keep whatever each of them declares. Since [ADR-0086] §2 there is one
//     spelling, so this is a plain override of one bool rather than the former dance
//     between a field-level flag and an object-level list;
//   - required_when — the reference's CEL-conditional requiredness, if the type did not set it.
func applyRefOverlay(ref, resolved *InputSchema) {
	if ref.Description != "" {
		resolved.Description = ref.Description
	}
	// Only a real bool overrides. Gating on "the key was written" would let a malformed
	// value — a leftover list, a null — carry its zero `false` over the requiredness the
	// TYPE declares, turning a mandatory field optional with no diagnostic anywhere.
	// The malformed value is refused separately ([validateRequiredShape]); this is the
	// second half of that pair, for the resolve that runs even when validation found
	// something.
	if _, isBool := ref.rawRequired.(*ast.BoolNode); isBool {
		resolved.Required = ref.Required
	}
	if ref.RequiredWhen != "" && resolved.RequiredWhen == "" {
		resolved.RequiredWhen = ref.RequiredWhen
	}
}

// TypeRefOverlayConflictCode — a `$type` reference in `state_schema` adding a property
// the referenced type already declares ([ADR-0086] §4). The name is the ADR's: the
// mechanism is the ADR-062 overlay, widened by one key, not a state-schema invention.
const TypeRefOverlayConflictCode = "input_type_ref_overlay_conflict"

// applyRefProperties merges the properties a `$type` reference in `state_schema`
// writes NEXT TO the reference into the resolved type ([NIM-740]). It is the whole
// of the ADR-062 exception, and it is deliberately a shallow ADD:
//
//   - a name the type does not declare is added — this is how `redis_users:
//     { items: { $type: AclUser, properties: { password: { type: secret, key: name } } } }`
//     says "an AclUser, plus the secret this particular use of it owns";
//   - a name the type DOES declare is refused ([TypeRefOverlayConflictCode]). Adopted
//     by reference from ADR-009's `extends:` covenant, which resolves the same question
//     the same way: two authors disagreeing about one property is a fact worth
//     reporting, and silently preferring either produces a schema neither of them wrote.
//     Two declarations of one property is exactly the drift the epic exists to
//     remove: silently letting one win would put the reader back where they
//     started, unable to tell which shape applies.
//
// A reference to a type that is not an object has nowhere to put them, and is
// refused for the same reason a `type:` next to `$type` is: the node means two
// contradictory things at once.
//
// The added properties are themselves resolved (they may reference types too), and
// they land in a map built here, never in the catalog's own — a type used under two
// state fields must not collect the other one's secret.
func applyRefProperties(ref, resolved *InputSchema, catalog TypeCatalog, stack map[string]bool, path string, depth int) []diag.Diagnostic {
	if resolved.Type != "object" {
		return []diag.Diagnostic{{
			Level: diag.LevelError, Phase: diag.PhaseSemanticValidate,
			Code: "input_type_ref_conflict",
			Message: fmt.Sprintf("$type %q resolves to type %q, so the properties written next to the reference have nowhere to go",
				ref.TypeRef, resolved.Type),
			Hint:     "only an object type can be extended at the point of use; drop properties: or reference an object type",
			YAMLPath: path + ".properties",
		}}
	}

	merged := make(InputSchemaMap, len(resolved.Properties)+len(ref.Properties))
	for name, sub := range resolved.Properties {
		merged[name] = sub
	}

	var diags []diag.Diagnostic
	for _, name := range sortedMapKeys(ref.Properties) {
		propPath := path + ".properties." + name
		if _, taken := merged[name]; taken {
			diags = append(diags, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSemanticValidate,
				Code:     TypeRefOverlayConflictCode,
				Message:  fmt.Sprintf("property %q is already declared by type %q", name, ref.TypeRef),
				Hint:     "a $type reference may only ADD properties — rename this one, or move the declaration into types.yml",
				YAMLPath: propPath,
			})
			continue
		}
		sub, d := resolveSchemaRefs(ref.Properties[name], catalog, stack, propPath, depth, dialectState)
		diags = append(diags, d...)
		merged[name] = sub
	}
	resolved.Properties = merged
	return diags
}

// ResolveStateSchemaTypeRefs resolves `$type` references in a `state_schema:` block
// against the service's type catalog. It is [ResolveTypeRefs] in the state dialect:
// same substitution, same cycle detection, plus the one relaxation the state block
// gets — a reference may carry its own `properties:` (see [applyRefProperties]).
//
// The source `in` is not mutated; the returned map is self-contained (no `$type`
// left). Diagnostics carry `$.state_schema.<field>…` paths, so the caller only has
// to set File.
func ResolveStateSchemaTypeRefs(in InputSchemaMap, catalog TypeCatalog) (InputSchemaMap, []diag.Diagnostic) {
	if in == nil {
		return nil, nil
	}
	out := make(InputSchemaMap, len(in))
	var diags []diag.Diagnostic
	for name, schema := range in {
		stack := map[string]bool{}
		resolved, d := resolveSchemaRefs(schema, catalog, stack, "$.state_schema."+name, 0, dialectState)
		out[name] = resolved
		diags = append(diags, d...)
	}
	return out, diags
}

// SchemaHasTypeRef reports whether a schema map references a named type anywhere,
// through items/properties/additional_properties. A cheap short-circuit for a caller
// deciding whether reading types.yml is worth it — a definition with no references
// needs no catalog. Dialect-neutral: an `input:` block and a `state_schema:` are the
// same map type and reference types the same way.
func SchemaHasTypeRef(m InputSchemaMap) bool {
	for _, s := range m {
		if schemaHasTypeRef(s) {
			return true
		}
	}
	return false
}

func schemaHasTypeRef(s *InputSchema) bool {
	if s == nil {
		return false
	}
	if s.TypeRef != "" {
		return true
	}
	if schemaHasTypeRef(s.Items) {
		return true
	}
	if SchemaHasTypeRef(s.Properties) {
		return true
	}
	ap, ok := s.AdditionalProperties.(*InputSchema)
	return ok && schemaHasTypeRef(ap)
}

// ResolveTypeRefs resolves `$type` references in the input schema `in` (e.g. a scenario's
// input:) against the service's type catalog. Returns a NEW schema map with
// the types substituted and diagnostics (input_type_unknown / input_type_cycle).
//
// The source `in` is not mutated. Nodes without `$type` are copied as-is (back-compat:
// schemas without types pass straight through). The `catalog` is already resolved
// within itself ([ParseTypeCatalog]); here only the consumer side is resolved.
func ResolveTypeRefs(in InputSchemaMap, catalog TypeCatalog) (InputSchemaMap, []diag.Diagnostic) {
	if in == nil {
		return nil, nil
	}
	out := make(InputSchemaMap, len(in))
	var diags []diag.Diagnostic
	for name, schema := range in {
		// A fresh stack per parameter: a reference from a param into a type does not form
		// a cycle by itself; cycles are only BETWEEN types (already caught in
		// ParseTypeCatalog), but we re-check for robustness.
		stack := map[string]bool{}
		resolved, d := resolveSchemaRefs(schema, catalog, stack, "$.input."+name, 0, dialectInput)
		out[name] = resolved
		diags = append(diags, d...)
	}
	return out, diags
}

// withFile sets File on diagnostics where it is empty (unification with
// parseAndValidate). Returns the same slice (mutates in place).
func withFile(ds []diag.Diagnostic, file string) []diag.Diagnostic {
	for i := range ds {
		if ds[i].File == "" {
			ds[i].File = file
		}
	}
	return ds
}
