// Package plugin is the typed parser and validator for a plugin's
// `manifest.yaml`, per the normative spec [`docs/keeper/plugins.md`] and
// [ADR-020].
//
// The wire-format source of truth is the proto message
// `soulstack.plugin.v1.Manifest` (see `proto/plugin/v1/manifest.proto`). On the
// host and in the linter the manifest is read straight from disk as YAML, so
// precise line/column errors from goccy/go-yaml matter more than protojson
// wire-compat (the manifest is never serialized, only read).
//
// The package is used by both `soul/internal/pluginhost` and `soul-lint`, so the
// same parser validates a plugin during discovery in the Soul daemon and during
// the offline `soul-lint validate-manifest` check.
package plugin

import (
	"errors"
	"fmt"
	"os"
	"regexp"

	"github.com/goccy/go-yaml"
	"github.com/goccy/go-yaml/ast"
	"github.com/goccy/go-yaml/parser"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"github.com/souls-guild/soul-stack/shared/diag"
)

// Manifest is the typed representation of `manifest.yaml`.
//
// Fields mirror the proto message `soulstack.plugin.v1.Manifest` with YAML tags.
// `Spec` is the kind-specific block: `soul_module` fills `States`,
// `cloud_driver`/`ssh_provider` fill `ProfileSchema`/`ParamsSchema`/`ProviderKind`.
//
// Parsing the full input schema inside `Spec.States[*].Input` is limited to
// formal checks: value type (`string`/`int`/`bool`/`list`/`map`), the `required`
// flag, the `secret` flag + `pattern` (for secret — `^vault:.*`). The full input
// DSL (`docs/input.md`) is validated by the destiny/scenario parser when checking
// a step's `params:` against the manifest schema — a different phase.
type Manifest struct {
	Kind                 string          `yaml:"kind"`
	ProtocolVersion      int32           `yaml:"protocol_version"`
	Namespace            string          `yaml:"namespace"`
	Name                 string          `yaml:"name"`
	RequiredCapabilities []string        `yaml:"required_capabilities,omitempty"`
	SideEffects          []SideEffectRaw `yaml:"side_effects,omitempty"`
	Spec                 ManifestSpec    `yaml:"spec"`
}

// ManifestSpec is the kind-specific block. It structurally unions the fields of
// all four kinds; validate() checks which are relevant for the current `kind`.
type ManifestSpec struct {
	// soul_module:
	States map[string]StateDef `yaml:"states,omitempty"`

	// cloud_driver / ssh_provider:
	ProviderKind string `yaml:"provider_kind,omitempty"`
	// ProfileSchema / ParamsSchema — JSON Schema (any arbitrary YAML object).
	// Semantic JSON Schema validation is out of scope for the manifest validator
	// (a post-MVP JSON Schema validator's job).
	//
	// ParamsSchema is shared by ssh_provider and soul_beacon (V5-2): both carry a
	// params schema in the same YAML key `spec.params_schema`, differing only in
	// semantics (SSH-provider params vs Vigil).
	ProfileSchema map[string]any `yaml:"profile_schema,omitempty"`
	ParamsSchema  map[string]any `yaml:"params_schema,omitempty"`
}

// StateDef describes one state in `spec.states`.
type StateDef struct {
	Description string                   `yaml:"description,omitempty"`
	Input       map[string]InputParamDef `yaml:"input,omitempty"`
}

// InputParamDef is the formal description of one parameter in a manifest input.
//
// This is **not** the full `docs/input.md` DSL — the manifest validator checks
// only what `manifest.yaml` expresses: type (∈ {string,int,bool,list,map}),
// required/secret/default/pattern/description plus the ADR-045 form-DSL fields
// from which the backend builds the module form: enum (closed set of allowed
// values), pattern (regex constraint), format+source (cluster-aware picker, e.g.
// sid), items (list element type / map value type), multiline+example (textarea
// UI hints). The rest of the full schema (object with properties, numeric bounds)
// is allowed — the parser keeps unknown keys in `Extra`, does not validate them.
type InputParamDef struct {
	Type        string `yaml:"type,omitempty"`
	Required    bool   `yaml:"required,omitempty"`
	Secret      bool   `yaml:"secret,omitempty"`
	Pattern     string `yaml:"pattern,omitempty"`
	Description string `yaml:"description,omitempty"`
	Default     any    `yaml:"default,omitempty"`

	// UI-form fields (ADR-045 S1). They duplicate the semantics of
	// `config.InputSchema` (Enum/Format/Source) but describe the parameter at the
	// manifest.yaml level — the backend builds the module form from them. We
	// validate the semantics here structurally (see validateInputParam); the full
	// input DSL is validated by the destiny/scenario parser.
	Enum   []any        `yaml:"enum,omitempty"`
	Format string       `yaml:"format,omitempty"`
	Source *InputSource `yaml:"source,omitempty"`

	// Multiline and Example (ADR-045 B3) — purely declarative UI hints (a large
	// textarea + placeholder), not checked by the validator.
	Multiline bool   `yaml:"multiline,omitempty"`
	Example   string `yaml:"example,omitempty"`

	// Items — list element type or map value type (ADR-045 S7 + amend). A recursive
	// *InputParamDef, mirror of `config.InputSchema.Items`:
	// `type: list, items: {type: int}` yields list[int], from which the backend
	// builds a typed list in the form (not a free list of strings). For list/array
	// — the ELEMENT type; for map/object — the VALUE type (`map[string]<items>`).
	// Validated structurally in validateInputParam (known element type).
	Items *InputParamDef `yaml:"items,omitempty"`
}

// InputSource is the discriminator object for the source catalog of a form
// field's values (ADR-044 S-T1, ADR-045 S1). Exactly one sub-key defines the set:
//   - IncarnationHosts (`incarnation_hosts: true`) — all SIDs of the current
//     incarnation;
//   - Choir (`choir: <name>`) — the SIDs of a specific Choir part of the
//     incarnation.
//
// Duplicates `config.InputSource` intentionally: `shared/config` already imports
// `shared/plugin` (config/module_params.go), so a back-import would create a
// cycle. Precedent for cycle-breaking duplication — `SupportedProtocolVersions`.
// Both definitions are structurally identical and must change in lockstep.
type InputSource struct {
	IncarnationHosts bool   `yaml:"incarnation_hosts,omitempty" json:"incarnation_hosts,omitempty"`
	Choir            string `yaml:"choir,omitempty" json:"choir,omitempty"`
}

// SideEffectRaw is one `side_effects[]` entry (exactly one
// `<resource-type>: <value>` pair, see docs/keeper/plugins.md → side_effects).
type SideEffectRaw map[string]any

// Manifest kind constants. They duplicate the `pluginv1.Kind` proto-enum values
// so the YAML form (lowercase snake_case) does not depend on proto serialization.
const (
	KindSoulModule  = "soul_module"
	KindCloudDriver = "cloud_driver"
	KindSSHProvider = "ssh_provider"
	KindSoulBeacon  = "soul_beacon"

	// FileName — the manifest file name next to the plugin binary (ADR-020(a)).
	FileName = "manifest.yaml"
)

// SupportedProtocolVersions — plugin-protocol versions understood by the host and
// the linter (ADR-020(c) → naming-rules.md). MVP is v1 only. Forward-compat
// only-add: when v2 appears, `2` is added here while keeping `1`.
//
// Duplicates the `pluginhost.SupportedProtocolVersions` constant (same invariant)
// — both arrays must change in lockstep. The duplication is deliberate: soul-host
// does not import soul-lint, soul-lint does not import pluginhost; shared/plugin
// is the shared source of truth for static manifest checking.
var SupportedProtocolVersions = []int32{1}

// Closed enum per docs/keeper/plugins.md → `required_capabilities` table.
var validCapabilities = map[string]pluginv1.Capability{
	"run_as_root":      pluginv1.Capability_CAPABILITY_RUN_AS_ROOT,
	"network_outbound": pluginv1.Capability_CAPABILITY_NETWORK_OUTBOUND,
	"network_inbound":  pluginv1.Capability_CAPABILITY_NETWORK_INBOUND,
	"vault_access":     pluginv1.Capability_CAPABILITY_VAULT_ACCESS,
	"fs_write_root":    pluginv1.Capability_CAPABILITY_FS_WRITE_ROOT,
	"exec_subprocess":  pluginv1.Capability_CAPABILITY_EXEC_SUBPROCESS,
}

// Closed enum per docs/keeper/plugins.md → `side_effects` table.
// The map value is unused — this is just a set.
var validSideEffectTypes = map[string]struct{}{
	"service":   {},
	"file":      {},
	"package":   {},
	"port":      {},
	"user":      {},
	"group":     {},
	"directory": {},
	"cron":      {},
	"mount":     {},
}

// Closed enum per the delegation spec (docs/keeper/plugins.md → manifest.spec.states.<state>.input).
var validInputTypes = map[string]struct{}{
	"string": {}, "int": {}, "bool": {}, "list": {}, "map": {},
	// `docs/input.md` uses an extended set (`integer`/`number`/`boolean`/`array`/
	// `object`); we accept them as synonyms so existing manifests do not break. The
	// drift between the two DSLs is noted; normalization is a separate task (see
	// observations).
	"integer": {}, "number": {}, "boolean": {}, "array": {}, "object": {},
}

var (
	reNamespace = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	reName      = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	reStateName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
)

// validInputFormats — the closed set of string formats for a form field
// (ADR-045 S1). Mirror of `config.inputFormatEnum` (docs/input.md), including
// `sid` (the FQDN form of a SID, ADR-044 S-T1). The duplication is forced — see
// InputSource.
var validInputFormats = map[string]struct{}{
	"hostname": {}, "fqdn": {}, "ipv4": {}, "ipv6": {}, "cidr": {},
	"email": {}, "uri": {}, "uuid": {}, "semver": {}, "duration": {},
	"sid": {},
}

// Load reads and validates `manifest.yaml` at `path` through the diag pipeline.
//
// The return contract is symmetric with `shared/config.Load*`:
//   - `error != nil` — only an I/O fatal (open/read). Manifest = nil.
//   - parse-fatal → `error == nil`, Manifest = nil, one diagnostics entry with
//     `Phase=PhaseParse`.
//   - schema/semantic errors → Manifest partially filled, diagnostics hold all
//     validation errors found.
func Load(path string) (*Manifest, []diag.Diagnostic, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, []diag.Diagnostic{{
			Level:   diag.LevelError,
			Phase:   diag.PhaseParse,
			File:    path,
			Code:    "io_error",
			Message: err.Error(),
		}}, err
	}
	m, diags := LoadFromBytes(path, src)
	return m, diags, nil
}

// LoadFromBytes is the main I/O-free entry point. Useful in tests with in-memory
// fixtures and in soul-lint (which reads the bytes itself). `filename` is only the
// `Diagnostic.File` label.
func LoadFromBytes(filename string, src []byte) (*Manifest, []diag.Diagnostic) {
	src = stripBOM(src)
	file, err := parser.ParseBytes(src, parser.ParseComments)
	if err != nil {
		return nil, []diag.Diagnostic{yamlParseDiag(filename, err)}
	}
	if len(file.Docs) == 0 || file.Docs[0].Body == nil {
		return nil, []diag.Diagnostic{{
			Level: diag.LevelError, Phase: diag.PhaseParse,
			File:    filename,
			Code:    "empty_document",
			Message: "manifest is empty or contains no mapping",
		}}
	}
	if len(file.Docs) > 1 {
		return nil, []diag.Diagnostic{{
			Level: diag.LevelError, Phase: diag.PhaseParse,
			File:    filename,
			Code:    "multi_document_not_allowed",
			Message: fmt.Sprintf("manifest must contain exactly one YAML document; got %d", len(file.Docs)),
			Hint:    "remove '---' separators",
		}}
	}
	root, ok := file.Docs[0].Body.(*ast.MappingNode)
	if !ok {
		t := file.Docs[0].Body.GetToken()
		line, col := 0, 0
		if t != nil {
			line, col = t.Position.Line, t.Position.Column
		}
		return nil, []diag.Diagnostic{{
			Level:    diag.LevelError,
			Phase:    diag.PhaseSchemaValidate,
			File:     filename,
			Line:     line,
			Column:   col,
			Code:     "type_mismatch",
			Message:  "root of manifest must be a mapping",
			YAMLPath: "$",
		}}
	}

	m := &Manifest{}
	var diags []diag.Diagnostic
	if err := yaml.NodeToValue(root, m, yaml.Strict()); err != nil {
		diags = append(diags, decodeErrorDiag(filename, err))
		// On a strict error the partially filled Manifest is kept; we validate as
		// far as possible (the fields that did decode).
	}
	diags = append(diags, validateManifest(filename, root, m)...)
	for i := range diags {
		if diags[i].File == "" {
			diags[i].File = filename
		}
	}
	return m, diags
}

// Address — `<namespace>.<name>`; used in logs and OTel tags.
func (m *Manifest) Address() string {
	return m.Namespace + "." + m.Name
}

// BinaryName — binary naming convention by kind (docs/keeper/plugins.md →
// kind-host-binary table):
//
//   - kind=soul_module   → `soul-mod-<name>`
//   - kind=cloud_driver  → `soul-cloud-<name>`
//   - kind=ssh_provider  → `soul-ssh-<name>`
//   - kind=soul_beacon   → `soul-beacon-<name>` (ADR-030 V5-2)
//
// Used by host discovery when locating the binary next to manifest.yaml. Returns
// "" for an unknown kind (defensive — the manifest already passes closed-enum
// validation in validateManifest).
func (m *Manifest) BinaryName() string {
	switch m.Kind {
	case KindSoulModule:
		return "soul-mod-" + m.Name
	case KindCloudDriver:
		return "soul-cloud-" + m.Name
	case KindSSHProvider:
		return "soul-ssh-" + m.Name
	case KindSoulBeacon:
		return "soul-beacon-" + m.Name
	default:
		return ""
	}
}

// ProtoKind maps Manifest.Kind to pluginv1.Kind for cross-check with the
// handshake (which encodes the enum as a string via protojson).
func (m *Manifest) ProtoKind() pluginv1.Kind {
	switch m.Kind {
	case KindSoulModule:
		return pluginv1.Kind_KIND_SOUL_MODULE
	case KindCloudDriver:
		return pluginv1.Kind_KIND_CLOUD_DRIVER
	case KindSSHProvider:
		return pluginv1.Kind_KIND_SSH_PROVIDER
	case KindSoulBeacon:
		return pluginv1.Kind_KIND_SOUL_BEACON
	default:
		return pluginv1.Kind_KIND_UNSPECIFIED
	}
}

// CapabilityFromString maps the YAML capability forms (lowercase snake_case) to
// `pluginv1.Capability`. Returns (cap, true) on a match, (_, false) for an unknown
// value. Used by the host to compare against `allowed_capabilities`.
func CapabilityFromString(s string) (pluginv1.Capability, bool) {
	c, ok := validCapabilities[s]
	return c, ok
}

// validateManifest is the main validator. It walks the AST root node and the
// already-decoded struct m, returning a diag list.
func validateManifest(path string, root *ast.MappingNode, m *Manifest) []diag.Diagnostic {
	var out []diag.Diagnostic

	// (1) kind: required + closed enum.
	switch m.Kind {
	case "":
		out = append(out, atPath(root, "$.kind", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "missing_required_field",
			Message: "kind is required at top-level",
			Hint:    "set kind: soul_module | cloud_driver | ssh_provider | soul_beacon",
		}))
	case KindSoulModule, KindCloudDriver, KindSSHProvider, KindSoulBeacon:
		// ok
	default:
		out = append(out, atPath(root, "$.kind", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "kind_invalid",
			Message: fmt.Sprintf("kind=%q is not in {soul_module,cloud_driver,ssh_provider,soul_beacon}", m.Kind),
		}))
	}

	// (2) protocol_version: > 0 + ∈ SupportedProtocolVersions.
	if m.ProtocolVersion <= 0 {
		out = append(out, atPath(root, "$.protocol_version", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "protocol_version_invalid",
			Message: fmt.Sprintf("protocol_version=%d must be a positive int32", m.ProtocolVersion),
		}))
	} else if !containsInt32(SupportedProtocolVersions, m.ProtocolVersion) {
		out = append(out, atPath(root, "$.protocol_version", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "protocol_version_unsupported",
			Message: fmt.Sprintf("protocol_version=%d not in supported %v", m.ProtocolVersion, SupportedProtocolVersions),
			Hint:    "upgrade soul-stack toolchain or set protocol_version to a supported value",
		}))
	}

	// (3) namespace / name: kebab-case lowercase, ≤63 chars.
	if m.Namespace == "" {
		out = append(out, atPath(root, "$.namespace", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code: "missing_required_field", Message: "namespace is required at top-level",
		}))
	} else if !reNamespace.MatchString(m.Namespace) {
		out = append(out, atPath(root, "$.namespace", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSemanticValidate,
			Code:    "namespace_invalid_format",
			Message: fmt.Sprintf("namespace=%q does not match %s", m.Namespace, reNamespace),
			Hint:    "kebab-case: lowercase letters, digits, dashes; must start with letter; ≤63 chars",
		}))
	}
	if m.Name == "" {
		out = append(out, atPath(root, "$.name", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code: "missing_required_field", Message: "name is required at top-level",
		}))
	} else if !reName.MatchString(m.Name) {
		out = append(out, atPath(root, "$.name", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSemanticValidate,
			Code:    "name_invalid_format",
			Message: fmt.Sprintf("name=%q does not match %s", m.Name, reName),
			Hint:    "kebab-case: lowercase letters, digits, dashes; must start with letter; ≤63 chars",
		}))
	}

	// (4) required_capabilities[] — closed enum.
	for i, c := range m.RequiredCapabilities {
		if _, ok := validCapabilities[c]; !ok {
			out = append(out, atPath(root, fmt.Sprintf("$.required_capabilities[%d]", i), diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:    "capability_unknown",
				Message: fmt.Sprintf("required_capabilities[%d]=%q is not a known capability", i, c),
				Hint:    "see docs/keeper/plugins.md → required_capabilities table",
			}))
		}
	}

	// (5) side_effects[] — closed enum keys, exactly one pair per entry.
	for i, e := range m.SideEffects {
		switch len(e) {
		case 0:
			out = append(out, atPath(root, fmt.Sprintf("$.side_effects[%d]", i), diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:    "side_effect_empty_entry",
				Message: fmt.Sprintf("side_effects[%d] must have exactly one resource-type entry, got 0", i),
			}))
		case 1:
			for k := range e {
				if _, ok := validSideEffectTypes[k]; !ok {
					out = append(out, atPath(root, fmt.Sprintf("$.side_effects[%d]", i), diag.Diagnostic{
						Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
						Code:    "side_effect_type_unknown",
						Message: fmt.Sprintf("side_effects[%d] resource-type %q is not a known side-effect", i, k),
						Hint:    "see docs/keeper/plugins.md → side_effects table",
					}))
				}
			}
		default:
			out = append(out, atPath(root, fmt.Sprintf("$.side_effects[%d]", i), diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:    "multiple_resource_types_in_side_effect_entry",
				Message: fmt.Sprintf("side_effects[%d] must have exactly one resource-type entry, got %d", i, len(e)),
				Hint:    "split multi-resource entry into separate list items",
			}))
		}
	}

	// (6) kind-specific spec.
	switch m.Kind {
	case KindSoulModule:
		out = append(out, validateSoulModuleSpec(root, m)...)
	case KindCloudDriver:
		out = append(out, validateCloudDriverSpec(root, m)...)
	case KindSSHProvider:
		out = append(out, validateSSHProviderSpec(root, m)...)
	case KindSoulBeacon:
		out = append(out, validateSoulBeaconSpec(root, m)...)
	}

	return out
}

func validateSoulModuleSpec(root *ast.MappingNode, m *Manifest) []diag.Diagnostic {
	var out []diag.Diagnostic
	if len(m.Spec.States) == 0 {
		out = append(out, atPath(root, "$.spec.states", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "spec_states_empty",
			Message: "spec.states is required and must be non-empty for kind=soul_module",
			Hint:    "declare at least one state (e.g. installed/running/promoted)",
		}))
		return out
	}
	for state, def := range m.Spec.States {
		statePath := "$.spec.states." + state
		if !reStateName.MatchString(state) {
			out = append(out, atPath(root, statePath, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSemanticValidate,
				Code:    "state_name_invalid",
				Message: fmt.Sprintf("spec.states key %q does not match %s", state, reStateName),
				Hint:    "kebab-case: lowercase letters, digits, dashes; must start with letter",
			}))
		}
		if def.Description == "" {
			out = append(out, atPath(root, statePath+".description", diag.Diagnostic{
				Level: diag.LevelWarning, Phase: diag.PhaseSchemaValidate,
				Code:    "state_description_missing",
				Message: fmt.Sprintf("spec.states.%s.description is empty", state),
				Hint:    "human-readable description helps operators and UI",
			}))
		}
		for paramName, p := range def.Input {
			paramPath := statePath + ".input." + paramName
			out = append(out, validateInputParam(root, paramPath, paramName, p)...)
		}
	}
	return out
}

func validateInputParam(root *ast.MappingNode, path, name string, p InputParamDef) []diag.Diagnostic {
	var out []diag.Diagnostic
	if p.Type == "" {
		out = append(out, atPath(root, path+".type", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "input_type_missing",
			Message: fmt.Sprintf("input parameter %q has no type", name),
			Hint:    "set type: string | int | bool | list | map",
		}))
	} else if _, ok := validInputTypes[p.Type]; !ok {
		out = append(out, atPath(root, path+".type", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "input_type_unknown",
			Message: fmt.Sprintf("input parameter %q type=%q is not in {string,int,bool,list,map}", name, p.Type),
		}))
	}
	if p.Secret {
		// The secret flag means the value arrives via a `vault:` ref; the pattern
		// must enforce that. If the operator set no explicit pattern we raise an
		// error: a secret without a vault-ref slips past audit easily.
		if p.Pattern == "" {
			out = append(out, atPath(root, path, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSemanticValidate,
				Code:    "input_secret_without_vault_pattern",
				Message: fmt.Sprintf("input parameter %q is secret but has no pattern", name),
				Hint:    `set pattern: "^vault:.*" for secrets`,
			}))
		} else if p.Pattern != "^vault:.*" {
			// Hard check: the only pattern allowed for a secret. Extending it is a
			// separate ADR on the secret-ref form.
			out = append(out, atPath(root, path+".pattern", diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSemanticValidate,
				Code:    "input_secret_pattern_invalid",
				Message: fmt.Sprintf("input parameter %q secret pattern=%q must be ^vault:.*", name, p.Pattern),
			}))
		}
	}

	// enum — if set, must be non-empty and each element compatible with Type.
	// (config.validateCommonInvariants checks the same; here structurally, without
	//  per-element AST positions: an enum literal has no per-item YAML-path.)
	if p.Enum != nil {
		if len(p.Enum) == 0 {
			out = append(out, atPath(root, path+".enum", diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:    "input_enum_empty",
				Message: fmt.Sprintf("input parameter %q has empty enum", name),
				Hint:    "drop enum or list at least one allowed value",
			}))
		}
		for i, v := range p.Enum {
			if !enumValueMatchesType(v, p.Type) {
				out = append(out, atPath(root, path+".enum", diag.Diagnostic{
					Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
					Code:    "input_enum_type_mismatch",
					Message: fmt.Sprintf("input parameter %q enum[%d] does not match type %q", name, i, p.Type),
				}))
			}
		}
	}

	// format — closed set (string formats incl. sid).
	if p.Format != "" {
		if _, ok := validInputFormats[p.Format]; !ok {
			out = append(out, atPath(root, path+".format", diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:    "input_format_invalid",
				Message: fmt.Sprintf("input parameter %q format=%q is not a known format", name, p.Format),
				Hint:    "see docs/input.md → format enum (hostname/fqdn/ipv4/.../sid)",
			}))
		}
	}

	// items — the type descriptor for collections (ADR-045 S7 + amend). For
	// list/array — the ELEMENT type; for map/object — the VALUE type
	// (`map[string]<items>`). When set, the type must be ∈ validInputTypes. Mirror
	// of config: items is meaningful only for collections. We do not validate
	// deeper than one level — a manifest input carries no nested collections.
	if p.Items != nil {
		switch p.Type {
		case "list", "array", "map", "object":
			if p.Items.Type == "" {
				out = append(out, atPath(root, path+".items.type", diag.Diagnostic{
					Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
					Code:    "input_items_type_missing",
					Message: fmt.Sprintf("input parameter %q items has no type", name),
					Hint:    "set items.type: string | int | bool | ...",
				}))
			} else if _, ok := validInputTypes[p.Items.Type]; !ok {
				out = append(out, atPath(root, path+".items.type", diag.Diagnostic{
					Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
					Code:    "input_items_type_unknown",
					Message: fmt.Sprintf("input parameter %q items.type=%q is not a known type", name, p.Items.Type),
				}))
			}
		default:
			out = append(out, atPath(root, path+".items", diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSemanticValidate,
				Code:    "input_items_invalid_for_type",
				Message: fmt.Sprintf("input parameter %q has items but type=%q is not a collection (list/array/map/object)", name, p.Type),
				Hint:    "items applies only to collection types: list/array (element type) or map/object (value type)",
			}))
		}
	}

	// source — structural validity of the discriminator (exactly one active source:
	// incarnation_hosts XOR choir). Mirror of config.validateSource — only the
	// "active==1" invariant; the backend resolves the set.
	if p.Source != nil {
		active := 0
		if p.Source.IncarnationHosts {
			active++
		}
		if p.Source.Choir != "" {
			active++
		}
		if active != 1 {
			out = append(out, atPath(root, path+".source", diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSemanticValidate,
				Code:    "input_source_invalid",
				Message: fmt.Sprintf("input parameter %q source must declare exactly one active catalog, got %d", name, active),
				Hint:    "set exactly one: incarnation_hosts: true OR choir: <name>",
			}))
		}
	}
	return out
}

// enumValueMatchesType — compatibility of an enum literal value with a
// manifest-input type. Accepts synonyms (`int`/`integer`, `bool`/`boolean`, …)
// from validInputTypes. list/map/array/object are transparent (we do not validate
// a composite enum per element, like config). An unknown type is transparent
// (input_type_unknown already flagged it).
func enumValueMatchesType(v any, t string) bool {
	switch t {
	case "string":
		_, ok := v.(string)
		return ok
	case "bool", "boolean":
		_, ok := v.(bool)
		return ok
	case "int", "integer":
		switch x := v.(type) {
		case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
			return true
		case float64:
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
	default:
		// list/map/array/object and unknown — transparent.
		return true
	}
}

func validateCloudDriverSpec(root *ast.MappingNode, m *Manifest) []diag.Diagnostic {
	var out []diag.Diagnostic
	if m.Spec.ProfileSchema == nil {
		out = append(out, atPath(root, "$.spec.profile_schema", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "spec_profile_schema_missing",
			Message: "spec.profile_schema is required for kind=cloud_driver",
			Hint:    "embed JSON Schema (draft 2020-12) describing VM profile parameters",
		}))
	}
	if len(m.Spec.States) > 0 {
		out = append(out, atPath(root, "$.spec.states", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "spec_states_not_allowed",
			Message: "spec.states is only valid for kind=soul_module",
		}))
	}
	return out
}

func validateSSHProviderSpec(root *ast.MappingNode, m *Manifest) []diag.Diagnostic {
	var out []diag.Diagnostic
	if m.Spec.ProviderKind == "" {
		out = append(out, atPath(root, "$.spec.provider_kind", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "spec_provider_kind_missing",
			Message: "spec.provider_kind is required for kind=ssh_provider",
			Hint:    "set provider_kind to a convention value (vault_ssh_ca / static_key / teleport) or your own",
		}))
	}
	if len(m.Spec.States) > 0 {
		out = append(out, atPath(root, "$.spec.states", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "spec_states_not_allowed",
			Message: "spec.states is only valid for kind=soul_module",
		}))
	}
	return out
}

// validateSoulBeaconSpec — kind-specific validator for `kind: soul_beacon`
// (ADR-030 V5-2). spec.params_schema is optional (a beacon with no params, e.g. a
// systemd-monotonic health check, is valid); spec.states/provider_kind/
// profile_schema are not allowed — soul_beacon has a single operation type (Check)
// and does not map to SoulModule state semantics.
func validateSoulBeaconSpec(root *ast.MappingNode, m *Manifest) []diag.Diagnostic {
	var out []diag.Diagnostic
	if len(m.Spec.States) > 0 {
		out = append(out, atPath(root, "$.spec.states", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "spec_states_not_allowed",
			Message: "spec.states is only valid for kind=soul_module",
		}))
	}
	if m.Spec.ProviderKind != "" {
		out = append(out, atPath(root, "$.spec.provider_kind", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "spec_provider_kind_not_allowed",
			Message: "spec.provider_kind is only valid for kind=ssh_provider",
		}))
	}
	if m.Spec.ProfileSchema != nil {
		out = append(out, atPath(root, "$.spec.profile_schema", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "spec_profile_schema_not_allowed",
			Message: "spec.profile_schema is only valid for kind=cloud_driver",
		}))
	}
	return out
}

// ValidateSimple is a convenience wrapper for legacy code
// (soul/internal/pluginhost) that expects `error` instead of `[]diag.Diagnostic`.
// Returns the first error-level entry or nil.
func (m *Manifest) ValidateSimple() error {
	diags := validateManifest("", nil, m)
	for _, d := range diags {
		if d.Level == diag.LevelError {
			return errors.New(d.Code + ": " + d.Message)
		}
	}
	return nil
}

func atPath(root *ast.MappingNode, path string, d diag.Diagnostic) diag.Diagnostic {
	d.YAMLPath = path
	if root == nil {
		return d
	}
	if line, col := lookupPathPosition(root, path); line > 0 {
		d.Line, d.Column = line, col
	}
	return d
}

// lookupPathPosition is a simplified walker over the `$.a.b[N]` path form. It
// pulls line/col from the AST. It does not cover every case (escaping,
// .input.<key>); for an unsupported form it returns 0/0 — no position, but with
// YAMLPath.
func lookupPathPosition(root *ast.MappingNode, path string) (int, int) {
	if !looksLikeSimplePath(path) {
		return 0, 0
	}
	// Strip the `$.` prefix.
	rest := path
	if len(rest) >= 2 && rest[0] == '$' && rest[1] == '.' {
		rest = rest[2:]
	}
	var node ast.Node = root
	for rest != "" {
		// Take the next segment up to `.` or `[`.
		segEnd := len(rest)
		for i, ch := range rest {
			if ch == '.' || ch == '[' {
				segEnd = i
				break
			}
		}
		seg := rest[:segEnd]
		rest = rest[segEnd:]
		if rest != "" && rest[0] == '[' {
			// Index not resolved — we return the position of the key seg.
			rest = ""
		}
		if rest != "" && rest[0] == '.' {
			rest = rest[1:]
		}
		m, ok := node.(*ast.MappingNode)
		if !ok {
			return 0, 0
		}
		var matched ast.Node
		for _, kv := range m.Values {
			tok := kv.Key.GetToken()
			if tok == nil {
				continue
			}
			if tok.Value == seg {
				if rest == "" {
					return tok.Position.Line, tok.Position.Column
				}
				matched = kv.Value
				break
			}
		}
		if matched == nil {
			return 0, 0
		}
		node = matched
	}
	return 0, 0
}

func looksLikeSimplePath(path string) bool {
	return len(path) > 2 && path[0] == '$' && path[1] == '.'
}

func yamlParseDiag(path string, err error) diag.Diagnostic {
	d := diag.Diagnostic{
		Level: diag.LevelError, Phase: diag.PhaseParse,
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

func decodeErrorDiag(path string, err error) diag.Diagnostic {
	d := diag.Diagnostic{
		Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
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
		// goccy strict-mode reports an "unknown field ..." message; map it to our code.
		if isUnknownFieldError(err) {
			d.Code = "unknown_key"
		}
	}
	return d
}

func isUnknownFieldError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return containsCI(msg, "unknown field") || containsCI(msg, "unknown key")
}

func containsCI(s, sub string) bool {
	// Simplest case-insensitive contains: lowercase both and search the substring.
	// A full windowed strings.EqualFold is unnecessary — pure ASCII.
	return indexCI(s, sub) >= 0
}

func indexCI(s, sub string) int {
	if len(sub) == 0 {
		return 0
	}
	if len(sub) > len(s) {
		return -1
	}
loop:
	for i := 0; i+len(sub) <= len(s); i++ {
		for j := 0; j < len(sub); j++ {
			c1, c2 := s[i+j], sub[j]
			if 'A' <= c1 && c1 <= 'Z' {
				c1 += 'a' - 'A'
			}
			if 'A' <= c2 && c2 <= 'Z' {
				c2 += 'a' - 'A'
			}
			if c1 != c2 {
				continue loop
			}
		}
		return i
	}
	return -1
}

func stripBOM(data []byte) []byte {
	return StripBOM(data)
}

// StripBOM trims a leading UTF-8 BOM (EF BB BF) if present. Exported for reuse in
// Sigil's manifest-byte canonicalization
// (shared/pluginhost.NormalizeManifestBytes, ADR-026): a single source of truth
// for BOM stripping, so the manifest hash matches on Keeper and Soul.
func StripBOM(data []byte) []byte {
	if len(data) >= 3 && data[0] == 0xEF && data[1] == 0xBB && data[2] == 0xBF {
		return data[3:]
	}
	return data
}

func containsInt32(xs []int32, x int32) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
