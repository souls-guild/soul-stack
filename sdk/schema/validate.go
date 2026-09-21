package schema

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
)

// Validation lives here, in the SDK, and not only on the host.
//
// One validator, three callers: `soul-mod stamp` refuses to stamp a document that
// would be rejected later, `shared/plugin` maps the same [Issue] list onto
// `shared/diag` for Keeper and `soul-lint`, and an author sees the identical message
// at build time and at approval time. A rule that exists in two places is a rule that
// eventually disagrees with itself.

// Level is the severity of an [Issue]. It maps 1:1 onto `diag.Level` on the host side.
type Level string

const (
	LevelError   Level = "error"
	LevelWarning Level = "warning"
)

// Phase says whether an issue is structural (types, enums, required fields) or
// semantic (regex grammars, cross-field invariants). It maps 1:1 onto `diag.Phase`.
type Phase string

const (
	PhaseSchema   Phase = "schema"
	PhaseSemantic Phase = "semantic"
)

// Issue is one validator finding. Path is a JSON path into the document
// (`$.modules[acl].states.present.input.host.type`); there is no line or column, since
// the document is generated and nobody reads it as text.
type Issue struct {
	Level   Level
	Phase   Phase
	Code    string
	Message string
	Hint    string
	Path    string
}

func (i Issue) String() string { return i.Code + ": " + i.Message }

// HasErrors reports whether the list holds at least one error-level issue.
func HasErrors(issues []Issue) bool {
	for _, i := range issues {
		if i.Level == LevelError {
			return true
		}
	}
	return false
}

var (
	// reName — module and state names: kebab-case, lowercase, starts with a
	// letter, at most 63 characters.
	reName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	// reVersionGrammar — plain MAJOR.MINOR.PATCH, the same grammar as a compat
	// bound (ADR-0076(c)); the two are compared against each other, so one syntax
	// only.
	reVersionGrammar = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)
)

// validCapabilities — the closed capability set as a lookup.
var validCapabilities = func() map[Capability]struct{} {
	m := make(map[Capability]struct{}, len(AllCapabilities))
	for _, c := range AllCapabilities {
		m[c] = struct{}{}
	}
	return m
}()

// validParamTypes — the closed set of parameter types, canonical spellings plus the
// `docs/input.md` synonyms.
var validParamTypes = map[ParamType]struct{}{
	String: {}, Int: {}, Bool: {}, List: {}, Map: {},
	Integer: {}, Number: {}, Boolean: {}, Array: {}, Object: {},
}

// validFormats — the closed set of string formats for a form field (ADR-045 S1),
// including `sid`, the FQDN form of a SID (ADR-044 S-T1).
var validFormats = map[string]struct{}{
	"hostname": {}, "fqdn": {}, "ipv4": {}, "ipv6": {}, "cidr": {},
	"email": {}, "uri": {}, "uuid": {}, "semver": {}, "duration": {},
	"sid": {},
}

// secretPattern is the only pattern a secret parameter may declare. Widening it is a
// separate decision about the secret-ref form, not a per-module choice.
const secretPattern = `^vault:.*`

// Validate checks a document against everything the format itself can decide. It
// returns issues in a stable order — module order, then sorted state and parameter
// names — so a caller can print or diff them without re-sorting.
//
// Errors mean the document must be rejected; warnings are advisory (a state with no
// description is the only one today).
func Validate(doc Document) []Issue {
	var out []Issue

	switch doc.Kind {
	case "":
		out = append(out, Issue{
			Level: LevelError, Phase: PhaseSchema, Path: "$.kind",
			Code:    "missing_required_field",
			Message: "kind is required at top-level",
			Hint:    "set kind: soul_module | ssh_provider | soul_beacon",
		})
	case KindSoulModule, KindSSHProvider, KindSoulBeacon:
	default:
		out = append(out, Issue{
			Level: LevelError, Phase: PhaseSchema, Path: "$.kind",
			Code:    "kind_invalid",
			Message: fmt.Sprintf("kind=%q is not in {soul_module,ssh_provider,soul_beacon}", doc.Kind),
		})
	}

	switch {
	case doc.ProtocolVersion <= 0:
		out = append(out, Issue{
			Level: LevelError, Phase: PhaseSchema, Path: "$.protocol_version",
			Code:    "protocol_version_invalid",
			Message: fmt.Sprintf("protocol_version=%d must be a positive int32", doc.ProtocolVersion),
		})
	case !containsInt32(SupportedProtocolVersions, doc.ProtocolVersion):
		out = append(out, Issue{
			Level: LevelError, Phase: PhaseSchema, Path: "$.protocol_version",
			Code:    "protocol_version_unsupported",
			Message: fmt.Sprintf("protocol_version=%d not in supported %v", doc.ProtocolVersion, SupportedProtocolVersions),
			Hint:    "upgrade the soul-stack toolchain or set protocol_version to a supported value",
		})
	}

	switch doc.Kind {
	case KindSoulModule:
		out = append(out, validateModules(doc)...)
		out = append(out, rejectForeignKindFields(doc, KindSoulModule)...)
	case KindSSHProvider:
		if doc.ProviderKind == "" {
			out = append(out, Issue{
				Level: LevelError, Phase: PhaseSchema, Path: "$.provider_kind",
				Code:    "provider_kind_missing",
				Message: "provider_kind is required for kind=ssh_provider",
				Hint:    "set provider_kind to a convention value (vault_ssh_ca / static_key / teleport) or your own",
			})
		}
		out = append(out, rejectForeignKindFields(doc, KindSSHProvider)...)
	case KindSoulBeacon:
		// params_schema is optional: a beacon with no parameters (a systemd
		// monotonic health check, say) is a valid beacon.
		out = append(out, rejectForeignKindFields(doc, KindSoulBeacon)...)
	}

	return out
}

// rejectForeignKindFields catches a document carrying a block that belongs to another
// kind — `modules` on an ssh_provider, `provider_kind` on a beacon. Each kind gets
// exactly the fields it can act on, so a misplaced block is a mistake rather than
// ignorable noise.
func rejectForeignKindFields(doc Document, kind Kind) []Issue {
	var out []Issue
	if len(doc.Modules) > 0 && kind != KindSoulModule {
		out = append(out, Issue{
			Level: LevelError, Phase: PhaseSchema, Path: "$.modules",
			Code:    "modules_not_allowed",
			Message: "modules is only valid for kind=soul_module",
		})
	}
	if doc.ProviderKind != "" && kind != KindSSHProvider {
		out = append(out, Issue{
			Level: LevelError, Phase: PhaseSchema, Path: "$.provider_kind",
			Code:    "provider_kind_not_allowed",
			Message: "provider_kind is only valid for kind=ssh_provider",
		})
	}
	if doc.ParamsSchema != nil && kind != KindSSHProvider && kind != KindSoulBeacon {
		out = append(out, Issue{
			Level: LevelError, Phase: PhaseSchema, Path: "$.params_schema",
			Code:    "params_schema_not_allowed",
			Message: "params_schema is only valid for kind=ssh_provider or kind=soul_beacon",
		})
	}
	return out
}

func validateModules(doc Document) []Issue {
	var out []Issue
	if len(doc.Modules) == 0 {
		return []Issue{{
			Level: LevelError, Phase: PhaseSchema, Path: "$.modules",
			Code:    "modules_empty",
			Message: "modules is required and must be non-empty for kind=soul_module",
			Hint:    "declare at least one module (e.g. acl/config/info)",
		}}
	}
	seen := make(map[string]struct{}, len(doc.Modules))
	for i, m := range doc.Modules {
		path := modulePath(i, m.Name)
		switch {
		case m.Name == "":
			out = append(out, Issue{
				Level: LevelError, Phase: PhaseSchema, Path: path + ".name",
				Code:    "missing_required_field",
				Message: fmt.Sprintf("modules[%d] has no name", i),
			})
		case !reName.MatchString(m.Name):
			out = append(out, Issue{
				Level: LevelError, Phase: PhaseSemantic, Path: path + ".name",
				Code:    "module_name_invalid",
				Message: fmt.Sprintf("module name %q does not match %s", m.Name, reName),
				Hint:    "kebab-case: lowercase letters, digits, dashes; must start with a letter; at most 63 characters",
			})
		case m.Name == SchemaSubcommand:
			// Dispatch is a subcommand (`redis acl`), and `schema` is
			// taken by the document dump — a module of that name could never be
			// invoked.
			out = append(out, Issue{
				Level: LevelError, Phase: PhaseSemantic, Path: path + ".name",
				Code:    "module_name_reserved",
				Message: fmt.Sprintf("module name %q is reserved for the schema subcommand", m.Name),
				Hint:    "rename the module - the artifact dispatches on argv[1], so `schema` can never reach it",
			})
		}
		if _, dup := seen[m.Name]; dup && m.Name != "" {
			out = append(out, Issue{
				Level: LevelError, Phase: PhaseSemantic, Path: path + ".name",
				Code:    "module_name_duplicate",
				Message: fmt.Sprintf("module name %q is declared more than once", m.Name),
			})
		}
		seen[m.Name] = struct{}{}

		out = append(out, validateVersionField(path+".introduced_in", m.IntroducedIn)...)

		// `side:` is an enum, and the empty string is the declared default
		// ([SideSoul]) rather than a missing value — a module that says nothing
		// runs where modules have always run. Anything else is refused rather
		// than folded into the default: `side: Keeper` silently meaning "soul"
		// is the failure this check exists to prevent.
		if m.Side != "" && m.Side != SideSoul && m.Side != SideKeeper {
			out = append(out, Issue{
				Level: LevelError, Phase: PhaseSchema, Path: path + ".side",
				Code:    "module_side_invalid",
				Message: fmt.Sprintf("side=%q is not a known side", m.Side),
				Hint:    fmt.Sprintf("one of %v; omit the key for the default (%s)", AllSides, SideSoul),
			})
		}

		for ci, c := range m.Capabilities {
			if _, ok := validCapabilities[c]; !ok {
				out = append(out, Issue{
					Level: LevelError, Phase: PhaseSchema,
					Path:    fmt.Sprintf("%s.capabilities[%d]", path, ci),
					Code:    "capability_unknown",
					Message: fmt.Sprintf("capabilities[%d]=%q is not a known capability", ci, c),
					Hint:    fmt.Sprintf("one of %v", AllCapabilities),
				})
			}
		}

		for si, se := range m.SideEffects {
			sePath := fmt.Sprintf("%s.side_effects[%d]", path, si)
			switch n := len(se.resources()); n {
			case 1:
			case 0:
				out = append(out, Issue{
					Level: LevelError, Phase: PhaseSchema, Path: sePath,
					Code:    "side_effect_empty_entry",
					Message: fmt.Sprintf("side_effects[%d] must set exactly one resource type, got 0", si),
				})
			default:
				out = append(out, Issue{
					Level: LevelError, Phase: PhaseSchema, Path: sePath,
					Code:    "multiple_resource_types_in_side_effect_entry",
					Message: fmt.Sprintf("side_effects[%d] must set exactly one resource type, got %d", si, n),
					Hint:    "split a multi-resource entry into separate list items",
				})
			}
		}

		out = append(out, validateStates(path, m)...)
	}
	return out
}

func validateStates(modPath string, m Module) []Issue {
	var out []Issue
	if len(m.States) == 0 {
		return []Issue{{
			Level: LevelError, Phase: PhaseSchema, Path: modPath + ".states",
			Code:    "module_states_empty",
			Message: fmt.Sprintf("module %q declares no states", m.Name),
			Hint:    "declare at least one state (e.g. present/absent)",
		}}
	}
	for _, state := range sortedKeys(m.States) {
		def := m.States[state]
		statePath := modPath + ".states." + state
		if !reName.MatchString(state) {
			out = append(out, Issue{
				Level: LevelError, Phase: PhaseSemantic, Path: statePath,
				Code:    "state_name_invalid",
				Message: fmt.Sprintf("state name %q does not match %s", state, reName),
				Hint:    "kebab-case: lowercase letters, digits, dashes; must start with a letter",
			})
		}
		if def.Description == "" {
			out = append(out, Issue{
				Level: LevelWarning, Phase: PhaseSchema, Path: statePath + ".description",
				Code:    "state_description_missing",
				Message: fmt.Sprintf("state %q has no description", state),
				Hint:    "a human-readable description is what the operator and the UI show",
			})
		}
		out = append(out, validateVersionField(statePath+".introduced_in", def.IntroducedIn)...)

		for _, block := range []struct {
			key    string
			params map[string]Param
		}{{blockInput, def.Input}, {blockOutput, def.Output}} {
			for _, name := range sortedKeys(block.params) {
				p := block.params[name]
				pPath := statePath + "." + block.key + "." + name
				out = append(out, validateParam(pPath, block.key, name, p)...)
				// `use:` must name a parameter of the SAME block — a replacement
				// that does not exist sends the author looking for it.
				if p.Deprecated != nil && p.Deprecated.Use != "" {
					if _, ok := block.params[p.Deprecated.Use]; !ok {
						out = append(out, Issue{
							Level: LevelError, Phase: PhaseSemantic, Path: pPath + ".deprecated.use",
							Code:    "deprecated_replacement_unknown",
							Message: fmt.Sprintf("parameter %q points at replacement %q, which state %q does not declare", name, p.Deprecated.Use, state),
							Hint:    "name a parameter declared in the same state, or drop use: when there is no successor",
						})
					}
				}
			}
		}
	}
	return out
}

// block is the declaring block, "input" or "output". It decides what
// `secret: true` MEANS: on input the value arrives as a vault ref and the
// pattern is what enforces it; on output the module RETURNS the secret, and
// the marker says the platform must mask that field wherever the output is
// observable ([ADR-0083] §8). Requiring a ref pattern there would be asking a
// module to declare that it never returns what it does return.
func validateParam(path, block, name string, p Param) []Issue {
	var out []Issue
	out = append(out, validateVersionField(path+".introduced_in", p.IntroducedIn)...)

	switch {
	case p.Type == "":
		out = append(out, Issue{
			Level: LevelError, Phase: PhaseSchema, Path: path + ".type",
			Code:    "input_type_missing",
			Message: fmt.Sprintf("parameter %q has no type", name),
			Hint:    "set type: string | int | bool | list | map",
		})
	default:
		if _, ok := validParamTypes[p.Type]; !ok {
			out = append(out, Issue{
				Level: LevelError, Phase: PhaseSchema, Path: path + ".type",
				Code:    "input_type_unknown",
				Message: fmt.Sprintf("parameter %q type=%q is not in {string,int,bool,list,map}", name, p.Type),
			})
		}
	}

	if p.Secret && block == blockInput {
		// A secret means the value arrives as a vault ref; the pattern is what
		// enforces it. A secret with no pattern slips past audit too easily.
		switch p.Pattern {
		case "":
			out = append(out, Issue{
				Level: LevelError, Phase: PhaseSemantic, Path: path,
				Code:    "input_secret_without_vault_pattern",
				Message: fmt.Sprintf("parameter %q is secret but has no pattern", name),
				Hint:    `set pattern: "^vault:.*" for secrets`,
			})
		case secretPattern:
		default:
			out = append(out, Issue{
				Level: LevelError, Phase: PhaseSemantic, Path: path + ".pattern",
				Code:    "input_secret_pattern_invalid",
				Message: fmt.Sprintf("parameter %q secret pattern=%q must be %s", name, p.Pattern, secretPattern),
			})
		}
	}

	if p.Enum != nil {
		if len(p.Enum) == 0 {
			out = append(out, Issue{
				Level: LevelError, Phase: PhaseSchema, Path: path + ".enum",
				Code:    "input_enum_empty",
				Message: fmt.Sprintf("parameter %q has an empty enum", name),
				Hint:    "drop enum or list at least one allowed value",
			})
		}
		for i, v := range p.Enum {
			if !enumValueMatchesType(v, p.Type) {
				out = append(out, Issue{
					Level: LevelError, Phase: PhaseSchema, Path: path + ".enum",
					Code:    "input_enum_type_mismatch",
					Message: fmt.Sprintf("parameter %q enum[%d] does not match type %q", name, i, p.Type),
				})
			}
		}
	}

	if p.Format != "" {
		if _, ok := validFormats[p.Format]; !ok {
			out = append(out, Issue{
				Level: LevelError, Phase: PhaseSchema, Path: path + ".format",
				Code:    "input_format_invalid",
				Message: fmt.Sprintf("parameter %q format=%q is not a known format", name, p.Format),
				Hint:    "see docs/input.md → format enum (hostname/fqdn/ipv4/.../sid)",
			})
		}
	}

	if p.Items != nil {
		// `secret:` on the ELEMENT is a shape nothing reads, and staying silent about
		// it is the failure the key it replaces was retired for ([ADR-0083] §8: an
		// author writes a marking and gets nothing). The trap is baited by §1 of that
		// same ADR, which puts `type: secret` inside `items:` next to `key:` for a
		// state_schema field — a plausible thing to carry over to a manifest, where
		// the granularity is the whole field: masking here is whole-cell, so a secret
		// one level down is declared by marking what contains it.
		if p.Items.Secret {
			out = append(out, Issue{
				Level: LevelError, Phase: PhaseSemantic, Path: path + ".items.secret",
				Code:    "items_secret_not_supported",
				Message: fmt.Sprintf("parameter %q declares secret on its element type, which nothing reads", name),
				Hint:    fmt.Sprintf("mark the containing field instead: %s.%s.secret: true", block, name),
			})
		}
		switch p.Type {
		case List, Array, Map, Object:
			if p.Items.Type == "" {
				out = append(out, Issue{
					Level: LevelError, Phase: PhaseSchema, Path: path + ".items.type",
					Code:    "input_items_type_missing",
					Message: fmt.Sprintf("parameter %q items has no type", name),
					Hint:    "set items.type: string | int | bool | ...",
				})
			} else if _, ok := validParamTypes[p.Items.Type]; !ok {
				out = append(out, Issue{
					Level: LevelError, Phase: PhaseSchema, Path: path + ".items.type",
					Code:    "input_items_type_unknown",
					Message: fmt.Sprintf("parameter %q items.type=%q is not a known type", name, p.Items.Type),
				})
			}
		default:
			out = append(out, Issue{
				Level: LevelError, Phase: PhaseSemantic, Path: path + ".items",
				Code:    "input_items_invalid_for_type",
				Message: fmt.Sprintf("parameter %q has items but type=%q is not a collection (list/array/map/object)", name, p.Type),
				Hint:    "items applies only to collection types: list/array (element type) or map/object (value type)",
			})
		}
	}

	out = append(out, validateDeprecated(path, name, p.Deprecated)...)

	if p.Source != nil {
		active := 0
		if p.Source.IncarnationHosts {
			active++
		}
		if p.Source.Choir != "" {
			active++
		}
		if active != 1 {
			out = append(out, Issue{
				Level: LevelError, Phase: PhaseSemantic, Path: path + ".source",
				Code:    "input_source_invalid",
				Message: fmt.Sprintf("parameter %q source must declare exactly one active catalog, got %d", name, active),
				Hint:    "set exactly one: incarnation_hosts: true OR choir: <name>",
			})
		}
	}
	return out
}

// validateDeprecated checks a parameter's deprecation block: both bounds present and
// well-formed, and a window no shorter than the policy minimum. A nil block is valid —
// the parameter is simply not deprecated.
func validateDeprecated(path, name string, d *Deprecated) []Issue {
	if d == nil {
		return nil
	}
	var out []Issue
	for _, b := range []struct{ key, value string }{{"since", d.Since}, {"removed_in", d.RemovedIn}} {
		switch {
		case b.value == "":
			out = append(out, Issue{
				Level: LevelError, Phase: PhaseSchema, Path: path + ".deprecated." + b.key,
				Code:    "deprecated_bound_missing",
				Message: fmt.Sprintf("parameter %q deprecated: has no %s", name, b.key),
				Hint:    "both since (inclusive) and removed_in (EXCLUSIVE) are required - an open-ended deprecation cannot be planned against",
			})
		case !reVersionGrammar.MatchString(b.value):
			out = append(out, Issue{
				Level: LevelError, Phase: PhaseSchema, Path: path + ".deprecated." + b.key,
				Code:    "deprecated_version_invalid",
				Message: fmt.Sprintf("parameter %q deprecated.%s=%q is not a plain MAJOR.MINOR.PATCH version", name, b.key, b.value),
				Hint:    "use 0.6.0 - no v prefix, no pre-release suffix, no >=/< operators (ADR-0076)",
			})
		}
	}
	if HasErrors(out) {
		return out
	}
	since, sok := parseVersion(d.Since)
	removed, rok := parseVersion(d.RemovedIn)
	if !sok || !rok {
		return out
	}
	if !meetsDeprecationWindow(since, removed) {
		out = append(out, Issue{
			Level: LevelError, Phase: PhaseSemantic, Path: path + ".deprecated.removed_in",
			Code: "deprecation_window_too_short",
			Message: fmt.Sprintf(
				"parameter %q is deprecated in %s but removed in %s - the policy grants at least %d minor releases",
				name, d.Since, d.RemovedIn, DeprecationMinMinors),
			Hint: fmt.Sprintf("set removed_in to %d.%d.0 or later, or bump the major (ADR-0076 deprecation policy)",
				since.major, since.minor+DeprecationMinMinors),
		})
	}
	return out
}

// validateVersionField checks optional `introduced_in` metadata (ADR-0076(i)): empty is
// valid (at or before the baseline), anything else must be a plain MAJOR.MINOR.PATCH —
// the same grammar as a compat bound, since the two are compared against each other.
func validateVersionField(path, value string) []Issue {
	if value == "" || reVersionGrammar.MatchString(value) {
		return nil
	}
	return []Issue{{
		Level: LevelError, Phase: PhaseSchema, Path: path,
		Code:    "introduced_in_invalid",
		Message: fmt.Sprintf("introduced_in=%q is not a plain MAJOR.MINOR.PATCH version", value),
		Hint:    "use 0.3.0 - no v prefix, no pre-release suffix, and only a RELEASED version (ADR-0076)",
	}}
}

// enumValueMatchesType reports whether an enum literal is compatible with a parameter
// type. Synonyms are accepted (`int`/`integer`, `bool`/`boolean`); collections and an
// unknown type are transparent, the latter because input_type_unknown already flagged
// it.
func enumValueMatchesType(v any, t ParamType) bool {
	switch t {
	case String:
		_, ok := v.(string)
		return ok
	case Bool, Boolean:
		_, ok := v.(bool)
		return ok
	case Int, Integer:
		switch x := v.(type) {
		case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
			return true
		case float32:
			return x == float32(int64(x))
		case float64:
			return x == float64(int64(x))
		case json.Number:
			_, err := x.Int64()
			return err == nil
		}
		return false
	case Number:
		switch v.(type) {
		case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64,
			float32, float64, json.Number:
			return true
		}
		return false
	default:
		return true
	}
}

func modulePath(i int, name string) string {
	if name == "" {
		return fmt.Sprintf("$.modules[%d]", i)
	}
	return "$.modules[" + name + "]"
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func containsInt32(xs []int32, x int32) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// The two parameter blocks of a state. Named because `secret: true` is read
// differently in each — see [validateParam].
const (
	blockInput  = "input"
	blockOutput = "output"
)
