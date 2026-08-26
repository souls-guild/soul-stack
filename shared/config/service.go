package config

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/goccy/go-yaml/ast"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// ServiceManifest is the typed representation of the root `service.yml`
// (spec: [`docs/service/manifest.md`]).
//
// Holds only service metadata (name/description), the `state_schema` contract
// for `incarnation.state` in Postgres, and a flat list of git dependencies.
// Scenarios are auto-discovered from `scenario/<name>/main.yml`, so there is no
// `scenarios:` section here.
type ServiceManifest struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description,omitempty"`

	// StateSchemaVersion — version of the `incarnation.state` structure. Bumped
	// explicitly on breaking schema changes; the migration chain lives in
	// `migrations/` (chain validation is out of scope for MVP, M1.5).
	StateSchemaVersion int `yaml:"state_schema_version"`

	// StateSchema is kept as a flat `map[string]any` (PM decision): JSON Schema
	// draft-07 is a large standard, full Go typing is separate work. MVP validate
	// (see validateStateSchema) checks the minimum: `type: object` on root,
	// `required` as []string, `properties` as map<string, recursive>. Extended
	// JSON-Schema validation (`enum`/`pattern`/`min`/`max`/`items`) is a separate
	// backlog item.
	StateSchema map[string]any `yaml:"state_schema"`

	Destiny []DependencyRef `yaml:"destiny,omitempty"`
	Modules []DependencyRef `yaml:"modules,omitempty"`

	// Lifecycle — optional lifecycle policy for the service's incarnations
	// (architecture.md → "Service — structure and manifest" § lifecycle:).
	// A missing block (nil) means both flags default to true (backcompat): create
	// auto-runs scenario `create`, destroy runs teardown per the usual
	// allow_destroy logic. Read the flags via [LifecycleConfig.AutoCreateEnabled] /
	// [LifecycleConfig.AutoDestroyEnabled] (nil-safe: both a nil block and a nil
	// flag are treated as true).
	Lifecycle *LifecycleConfig `yaml:"lifecycle,omitempty"`

	// CertificateRotation — optional auto-rotation policy for the service's TLS certs
	// (NIM-99). nil = rotation off; enable:false/omitted = the section is inert.
	CertificateRotation *CertificateRotationConfig `yaml:"certificate_rotation,omitempty"`

	// Telemetry — optional host-vitals telemetry policy (ADR-072, NIM-87).
	// Absence of the block (nil) = default: enabled, interval 30s, all collectors.
	// Dereference via the nil-safe getters [TelemetryConfig.EnabledOrDefault] /
	// [TelemetryConfig.IntervalOrDefault] / [TelemetryConfig.CollectorsOrDefault].
	Telemetry *TelemetryConfig `yaml:"telemetry,omitempty"`

	// Compat — optional engine-compatibility window (ADR-0076): the keeper
	// versions this definition was authored and tested against. A missing block
	// (nil) = unbounded, so existing manifests keep working with no migration.
	// The window in force for a run is the intersection with every destiny the
	// run resolves. Read via the nil-safe [CompatConfig.KeeperWindow].
	Compat *CompatConfig `yaml:"compat,omitempty"`
}

// LifecycleConfig — the `lifecycle:` block of the service manifest. Both flags
// are `*bool` (nil → default true): distinguishes "operator didn't set it" from
// "explicitly false".
type LifecycleConfig struct {
	// AutoCreate — `POST /v1/incarnations` auto-runs scenario `create` (nil/true).
	// false — the incarnation is created in `ready` without a run; the operator
	// runs `create` manually from the Run form.
	AutoCreate *bool `yaml:"auto_create,omitempty"`

	// AutoDestroy — deleting an incarnation runs the `destroy` teardown scenario
	// per the usual `allow_destroy` logic (nil/true). false — deletion is always
	// direct, without teardown, taking priority over `allow_destroy`.
	AutoDestroy *bool `yaml:"auto_destroy,omitempty"`
}

// AutoCreateEnabled — nil-safe read of the auto_create policy: a nil block OR a
// nil flag → true (backcompat per architecture.md).
func (l *LifecycleConfig) AutoCreateEnabled() bool {
	if l == nil || l.AutoCreate == nil {
		return true
	}
	return *l.AutoCreate
}

// AutoDestroyEnabled — nil-safe read of the auto_destroy policy: a nil block OR
// a nil flag → true (backcompat per architecture.md).
func (l *LifecycleConfig) AutoDestroyEnabled() bool {
	if l == nil || l.AutoDestroy == nil {
		return true
	}
	return *l.AutoDestroy
}

// CertificateRotationConfig — the `certificate_rotation:` manifest block (NIM-99):
// whether the service supports auto-rotation of TLS certs, with which operational
// scenario, and which Vault PKI role. No section (nil) → rotation off. `enable:false`/omitted →
// the section is inert (explicit opt-in, security-first).
type CertificateRotationConfig struct {
	Enable    bool   `yaml:"enable"`              // enables auto-rotation of the service's certs
	Scenario  string `yaml:"scenario,omitempty"`  // rotation scenario; required when enable:true
	Threshold string `yaml:"threshold,omitempty"` // margin before expiry (`30d`); default, currently informational
	PKIRole   string `yaml:"pki_role,omitempty"`  // Vault PKI role for signing; required when enable:true
}

// KnownCollectors — the closed set of host-vitals collectors (ADR-072, NIM-87;
// `net` added by the NIM-127 amendment — inode rides `disk`, same statvfs).
var KnownCollectors = []string{"cpu", "mem", "disk", "load", "uptime", "net"}

// TelemetryIntervalFloor — the lower bound of telemetry.interval (anti-DoS floor).
const TelemetryIntervalFloor = 10 * time.Second

// IsKnownCollector — whether name belongs to the closed KnownCollectors set.
func IsKnownCollector(name string) bool {
	return contains(KnownCollectors, name)
}

// TelemetryConfig — the `telemetry:` block of the service manifest (ADR-072, NIM-87).
// Enabled — `*bool` (nil → default true): distinguishes "not set" from "explicitly false".
type TelemetryConfig struct {
	Enabled    *bool    `yaml:"enabled,omitempty"`
	Interval   *string  `yaml:"interval,omitempty"`
	Collectors []string `yaml:"collectors,omitempty"`
}

// EnabledOrDefault — nil-safe: a nil block OR a nil flag → true (backcompat).
func (t *TelemetryConfig) EnabledOrDefault() bool {
	if t == nil || t.Enabled == nil {
		return true
	}
	return *t.Enabled
}

// IntervalOrDefault — nil-safe: a nil block OR a nil/empty Interval → "30s".
func (t *TelemetryConfig) IntervalOrDefault() string {
	if t == nil || t.Interval == nil || *t.Interval == "" {
		return "30s"
	}
	return *t.Interval
}

// CollectorsOrDefault — nil-safe: a nil block OR an empty list → a copy of KnownCollectors.
func (t *TelemetryConfig) CollectorsOrDefault() []string {
	if t == nil || len(t.Collectors) == 0 {
		out := make([]string, len(KnownCollectors))
		copy(out, KnownCollectors)
		return out
	}
	return t.Collectors
}

// DependencyRef — an entry in `destiny[]` / `modules[]`: `{name, ref}` + optional `git`.
//
// `name` — a destiny name (kebab-case, single-level) or a module address (two-level
// `<alias>.<module>`, level 1 being the registration alias); a different regex
// applies per context (see schemaValidateService → pass over the slices).
// `ref` — a git tag or branch (ADR-007). MVP accepts any non-empty string;
// detailed ref-form checks (semver-tag / branch-naming) are backlog.
// `git` — optional per-entry override of the dependency's full git URL. Supported
// only for `destiny[]` (hybrid resolution: name → substitution into
// `default_destiny_source`, git → direct URL without a template). Forbidden for
// `modules[]` (see validateDependencyRef) — deferred to a separate decision.
type DependencyRef struct {
	Name string `yaml:"name"`
	Ref  string `yaml:"ref"`
	Git  string `yaml:"git,omitempty"`
}

var (
	// reServiceName — canonical kebab-case: dash only between alphanumerics, no
	// trailing/leading/double dash. Symmetric with `reDestinyName`.
	reServiceName = regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)*$`)

	// reDependencyDestinyName — kebab-case single-level destiny name in
	// `destiny[]`. Same as `reDestinyName` (destiny.go), reused directly — a
	// separate regex copy was a source of drift.
	reDependencyDestinyName = reDestinyName

	// reDependencyModuleName — strict two-level form `<alias>.<module>` for custom
	// modules in `service.yml → modules[]`. Level 1 is the registration alias the
	// artifact is expected to be installed under (it stopped being an artifact-declared
	// namespace in NIM-377); level 2 is one of the modules that artifact serves.
	// Symmetric with `reRequiredModule` (destiny.go); canonical kebab-case in each half
	// (no trailing/leading/double dash), no underscore, naming-rules.md §57/§186.
	reDependencyModuleName = regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)*\.[a-z][a-z0-9]*(-[a-z0-9]+)*$`)
)

// deprecatedServiceKeys — deprecated top-level keys in `service.yml`. Each gets
// a specific hint explaining "where it actually lives" (see
// docs/service/manifest.md → "What service.yml does NOT hold"). Symmetric with
// `deprecatedDestinyKeys` in destiny.go.
var deprecatedServiceKeys = map[string]string{
	"version":   "version is a git ref under which service is committed, not a manifest field; see ADR-007",
	"tasks":     "tasks live in scenario/<name>/main.yml (auto-discover); service.yml is manifest-only",
	"steps":     "tasks live in scenario/<name>/main.yml (auto-discover); service.yml is manifest-only",
	"input":     "input lives in scenario/<name>/main.yml (input:-block per docs/input.md), not service.yml",
	"scenarios": "scenarios are auto-discovered from scenario/<name>/ directory; do not enumerate them in service.yml",
	"revealable_secrets": "revealable_secrets: removed (ADR-0083 §1); declare the secret as a `state_schema` field with `type: secret` " +
		"(plus `key:` inside `items` for a collection, and the optional `label:`) — the Vault path is derived from " +
		"(service, incarnation, field, key), so there is none left to write. Reveal, the `incarnation.view-secrets` right " +
		"and the audit event are unchanged",
}

// schemaValidateService — post-decode checks of ServiceManifest.
func schemaValidateService(path string, root *ast.MappingNode, m *ServiceManifest) []diag.Diagnostic {
	_ = path
	var out []diag.Diagnostic

	topKeys := topLevelKeys(root)

	// 1) deprecated top-level keys (via AST for line/col).
	for _, kv := range root.Values {
		tok := kv.Key.GetToken()
		if tok == nil {
			continue
		}
		hint, dep := deprecatedServiceKeys[tok.Value]
		if !dep {
			continue
		}
		out = append(out, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "unknown_key",
			Message:  `unknown field "` + tok.Value + `"`,
			Hint:     hint,
			YAMLPath: "$." + tok.Value,
		}))
	}

	// 2) name — required + format. The `topKeys["name"]` branch distinguishes
	// "key absent" from "key present with empty/null string" (symmetric with destiny.go).
	if !topKeys["name"] {
		out = append(out, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "missing_required_field",
			Message:  "name is required at top-level",
			Hint:     "set name: <kebab-case>, matching service-<name>/ folder",
			YAMLPath: "$.name",
		})
	} else if !reServiceName.MatchString(m.Name) {
		msg := fmt.Sprintf("name %q does not match %s", m.Name, reServiceName)
		if m.Name == "" {
			msg = "name must be non-empty kebab-case string"
		}
		out = append(out, atPath(root, "$.name", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "name_invalid_format",
			Message: msg,
			Hint:    "kebab-case: lowercase letters, digits, dashes; must start with letter",
		}))
	} else if IsReservedVaultNamespace(m.Name) {
		// NIM-706. Offline half of the rule serviceregistry.validateFields enforces at
		// registration: the service name becomes the first path segment of every secret
		// the platform derives for it, and these words already name a path family the
		// platform writes under itself. Reported here as well as there because the
		// artifact is authored long before anyone registers it, and a name is the one
		// mistake that is cheap now and a rename later.
		out = append(out, atPath(root, "$.name", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    ServiceNameReservedCode,
			Message: fmt.Sprintf("name %q is reserved: the platform derives its own secrets under `<mount>/%s/`", m.Name, m.Name),
			Hint:    "pick another name — reserved names are " + strings.Join(ReservedVaultNamespaceNames(), ", "),
		}))
	}

	// 3) state_schema_version — required + integer ≥ 1.
	// Also catch a float (`1.5`): goccy silently truncates when decoding into
	// `int`, so we check the AST explicitly — otherwise the operator thinks they
	// wrote "1.5" while Keeper stores "1" (silent truncation).
	if !topKeys["state_schema_version"] {
		out = append(out, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "missing_required_field",
			Message:  "state_schema_version is required at top-level",
			Hint:     "set state_schema_version: 1 for fresh services; bump on breaking state schema changes (ADR-019)",
			YAMLPath: "$.state_schema_version",
		})
	} else if vn := findScalarValue(root, "state_schema_version"); vn != nil {
		if _, isFloat := vn.(*ast.FloatNode); isFloat {
			tok := vn.GetToken()
			out = append(out, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:     "type_mismatch",
				Message:  fmt.Sprintf("state_schema_version must be an integer, got float %q", tok.Value),
				Hint:     "use integer like 1, 2, 3 — version is monotonic, not a semver fraction",
				YAMLPath: "$.state_schema_version",
			}))
		} else if _, isInt := vn.(*ast.IntegerNode); !isInt {
			// Non-integer non-float (string/bool/sequence/mapping/null): decode
			// already raised `type_mismatch`; an extra `value_out_of_range "got 0"`
			// from the zero-value `m.StateSchemaVersion` would be misleading.
		} else if m.StateSchemaVersion < 1 {
			out = append(out, atPath(root, "$.state_schema_version", diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:    "value_out_of_range",
				Message: fmt.Sprintf("state_schema_version must be >= 1, got %d", m.StateSchemaVersion),
			}))
		}
	}

	// 4) state_schema — required + structural validation.
	if !topKeys["state_schema"] {
		out = append(out, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "missing_required_field",
			Message:  "state_schema is required at top-level",
			Hint:     "declare state_schema: { type: object, properties: {...} } — see docs/service/manifest.md → state_schema",
			YAMLPath: "$.state_schema",
		})
	} else {
		out = append(out, validateStateSchema(root, findInputMapping(root, "state_schema"), "$.state_schema")...)
		out = append(out, validateSecretFields(root, m.StateSchema, "$.state_schema")...)
	}

	// 5) destiny[] / modules[] — each entry is valid as `{name, ref}`.
	for i, dep := range m.Destiny {
		out = append(out, validateDependencyRef(root, "destiny", i, dep, reDependencyDestinyName)...)
	}
	// Entries sharing an alias are the normal way to declare one artifact serving
	// several modules, and they all land in the SAME slot — so they must name the
	// same ref. Left unchecked, the synthesized install (NIM-524) silently picks
	// the first entry's ref and the pin check fails later, naming a ref the reader
	// never wrote next to the module that failed.
	aliasRef := make(map[string]string, len(m.Modules))
	aliasAt := make(map[string]int, len(m.Modules))
	for i, dep := range m.Modules {
		out = append(out, validateDependencyRef(root, "modules", i, dep, reDependencyModuleName)...)
		alias, ok := ModuleAlias(dep.Name)
		if !ok || dep.Ref == "" {
			continue
		}
		if prev, seen := aliasRef[alias]; seen && prev != dep.Ref {
			out = append(out, atPath(root, fmt.Sprintf("$.modules[%d].ref", i), diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code: "conflicting_module_ref",
				Message: fmt.Sprintf("modules[%d] %q pins ref %q, but modules[%d] pins ref %q for the same alias %q",
					i, dep.Name, dep.Ref, aliasAt[alias], prev, alias),
				Hint: "modules sharing address level 1 are served by ONE artifact in ONE slot; give them one ref, or register the second artifact under a different alias",
			}))
			continue
		}
		aliasRef[alias], aliasAt[alias] = dep.Ref, i
	}

	// 7) certificate_rotation — optional rotation policy (NIM-99).
	out = append(out, validateCertificateRotation(root, m.CertificateRotation)...)

	// 8) telemetry — optional host-vitals policy (ADR-072, NIM-87). A nil block
	// is skipped (backcompat). Enabled is not validated; there are no cross-field invariants.
	if m.Telemetry != nil {
		for _, c := range m.Telemetry.Collectors {
			if !IsKnownCollector(c) {
				out = append(out, atPath(root, "$.telemetry.collectors", diag.Diagnostic{
					Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
					Code:    "unknown_collector",
					Message: fmt.Sprintf("telemetry.collectors: unknown collector %q; known set: %s", c, strings.Join(KnownCollectors, ", ")),
					Hint:    "allowed collectors: " + strings.Join(KnownCollectors, ", "),
				}))
			}
		}
		if m.Telemetry.Interval != nil && *m.Telemetry.Interval != "" {
			if d, err := ParseDuration(*m.Telemetry.Interval); err != nil {
				out = append(out, atPath(root, "$.telemetry.interval", diag.Diagnostic{
					Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
					Code:    "duration_invalid",
					Message: fmt.Sprintf("telemetry.interval: invalid duration %q: %v", *m.Telemetry.Interval, err),
					Hint:    "use Go-duration (e.g. 30s, 1m) or <N>d for days",
				}))
			} else if d < TelemetryIntervalFloor {
				out = append(out, atPath(root, "$.telemetry.interval", diag.Diagnostic{
					Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
					Code:    "value_out_of_range",
					Message: fmt.Sprintf("telemetry.interval must be >= 10s (anti-DoS floor), got %s", d),
				}))
			}
		}
	}

	// 9) compat: — optional engine-compatibility window (ADR-0076). A nil block
	// is valid (unbounded); the same grammar applies to destiny.yml.
	out = append(out, validateCompat(root, m.Compat)...)

	return out
}

// validateCertificateRotation — validation of the optional `certificate_rotation:` section
// (NIM-99): when enable:true, scenario (snake/kebab, folder
// scenario/<name>/) and pki_role are required; threshold — per the `duration` convention. A nil
// section = rotation off, valid.
func validateCertificateRotation(root *ast.MappingNode, crt *CertificateRotationConfig) []diag.Diagnostic {
	if crt == nil {
		return nil
	}
	var out []diag.Diagnostic
	base := "$.certificate_rotation"

	if crt.Enable && crt.Scenario == "" {
		out = append(out, atPath(root, base+".scenario", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "missing_required_field",
			Message: "certificate_rotation.scenario is required when enable: true",
			Hint:    "declare scenario: <name> matching scenario/<name>/main.yml",
		}))
	}
	if crt.Enable && crt.PKIRole == "" {
		out = append(out, atPath(root, base+".pki_role", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "missing_required_field",
			Message: "certificate_rotation.pki_role is required when enable: true",
			Hint:    "declare pki_role: <vault-pki-role> used to sign this service's certs",
		}))
	}
	if crt.Scenario != "" && !reScenarioName.MatchString(crt.Scenario) {
		out = append(out, atPath(root, base+".scenario", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "name_invalid_format",
			Message: fmt.Sprintf("certificate_rotation.scenario %q does not match %s", crt.Scenario, reScenarioName),
			Hint:    "snake_case or kebab-case: lowercase letters/digits with _/- separators; must start with letter",
		}))
	}
	if crt.Threshold != "" {
		if _, err := ParseDuration(crt.Threshold); err != nil {
			out = append(out, atPath(root, base+".threshold", diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:    "duration_invalid",
				Message: fmt.Sprintf("certificate_rotation.threshold %q is not a valid duration: %v", crt.Threshold, err),
				Hint:    "use convention like 30d, 720h",
			}))
		}
	}
	return out
}

// validateDependencyRef — checks one `{name, ref}` entry in destiny[]/modules[].
// `nameRegex` distinguishes the single- and two-level name form.
func validateDependencyRef(root *ast.MappingNode, listKey string, idx int, dep DependencyRef, nameRegex *regexp.Regexp) []diag.Diagnostic {
	var out []diag.Diagnostic
	base := fmt.Sprintf("$.%s[%d]", listKey, idx)

	if dep.Name == "" {
		out = append(out, atPath(root, base+".name", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "missing_required_field",
			Message: fmt.Sprintf("%s[%d].name is required", listKey, idx),
			Hint:    "dependency entry must declare {name, ref} — both non-empty",
		}))
	} else if listKey == "modules" && reservedModuleAddr(dep.Name) {
		// ADR-009 / ADR-015 for `core.*` (always available, never listed), NIM-377
		// for the rest of the reserved list (no plugin can be registered under one).
		// Separate codes from plain `name_invalid_format`: these names are all
		// regex-valid, and it is the semantics that are forbidden.
		out = append(out, reservedModuleDiag(root, base+".name", fmt.Sprintf("%s[%d].name", listKey, idx), dep.Name))
	} else if !nameRegex.MatchString(dep.Name) {
		out = append(out, atPath(root, base+".name", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "name_invalid_format",
			Message: fmt.Sprintf("%s[%d].name %q does not match %s", listKey, idx, dep.Name, nameRegex),
			Hint:    nameHint(listKey),
		}))
	}

	if dep.Ref == "" {
		out = append(out, atPath(root, base+".ref", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "missing_required_field",
			Message: fmt.Sprintf("%s[%d].ref is required (ADR-007: git tag or branch)", listKey, idx),
			Hint:    "examples: v2.0.0 (tag), main (branch); no semver-range",
		}))
	}

	// git — per-entry override of the full URL, supported only for destiny[].
	// For modules[] we forbid it explicitly so the operator doesn't assume support.
	if listKey == "modules" && dep.Git != "" {
		out = append(out, atPath(root, base+".git", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "unknown_key",
			Message: fmt.Sprintf("modules[%d].git is not supported — per-entry git override is destiny-only", idx),
			Hint:    "per-entry git override is not defined for modules (resolved by name only)",
		}))
	}

	return out
}

func nameHint(listKey string) string {
	if listKey == "modules" {
		return "two-level address <alias>.<module> per architecture.md -> \"Module addressing\"; level 1 is the registration alias, core-modules are not listed here"
	}
	return "kebab-case: lowercase letters, digits, dashes; must start with letter"
}

// validateStateSchema — MVP JSON Schema validation at the `state_schema:` root.
//
// Checks the minimum that guarantees correct runtime validation of
// `incarnation.state` by Keeper:
//   - the root must be a mapping with `type: object` (an object is the only
//     valid form for top-level state);
//   - `required` (if present) — an array of strings;
//   - `properties` (if present) — map<string, mapping>; recurse into each nested
//     schema by the same rules, but without a mandatory `type: object` (nested
//     schemas may be of any type).
//
// Extended JSON Schema (`enum`/`pattern`/`min`/`max`/`items`/
// `additionalProperties`, etc.) is deliberately NOT typed in MVP — it's a large
// draft-07 standard. We catch a malformed schema but don't validate the
// semantics of each key (PM decision).
func validateStateSchema(root *ast.MappingNode, node *ast.MappingNode, pathPrefix string) []diag.Diagnostic {
	if node == nil {
		// The key is present in YAML, but the value is not a mapping
		// (null/scalar/sequence). goccy won't raise a decode error for null →
		// map[string]any (just yields nil), so a diagnostic is needed here. For
		// scalar/sequence the generic `type_mismatch` was already emitted by the
		// decode phase; but an explicit diagnostic reads better here too.
		return []diag.Diagnostic{atPath(root, pathPrefix, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "state_schema_root_not_object",
			Message: "state_schema must be a mapping with type: object on root",
			Hint:    "declare state_schema: { type: object, properties: {...} }",
		})}
	}
	var out []diag.Diagnostic

	// At the root level `type: object` is mandatory (incarnation.state is always
	// an object). At nested levels type may be any valid JSON Schema type.
	tn := findScalarValue(node, "type")
	if tn == nil {
		out = append(out, diagAt(node.GetToken().Position.Line, node.GetToken().Position.Column, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "state_schema_root_not_object",
			Message:  "state_schema must declare type: object on root",
			Hint:     "incarnation.state is always an object; nested schemas may use other types",
			YAMLPath: pathPrefix + ".type",
		}))
	} else if t, ok := tn.(*ast.StringNode); !ok || t.Value != "object" {
		actual := "<non-string>"
		if t, ok := tn.(*ast.StringNode); ok {
			actual = t.Value
		}
		out = append(out, diagAt(tn.GetToken().Position.Line, tn.GetToken().Position.Column, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "state_schema_root_not_object",
			Message:  fmt.Sprintf("state_schema.type must be \"object\" on root, got %q", actual),
			Hint:     "incarnation.state is always an object",
			YAMLPath: pathPrefix + ".type",
		}))
	}

	out = append(out, validateJSONSchemaNode(node, pathPrefix)...)
	return out
}

// validateSecretFields turns the refusals of [CollectSecretFields] into positional
// diagnostics ([ADR-0083] §1). The rules themselves live there, in a pure function over
// the decoded schema, because keeper resolves the same declarations at runtime — reveal
// and `core.state.*` — and a second implementation would drift from this one.
func validateSecretFields(root *ast.MappingNode, schema map[string]any, pathPrefix string) []diag.Diagnostic {
	_, issues := CollectSecretFields(schema)
	out := make([]diag.Diagnostic, 0, len(issues))
	for _, iss := range issues {
		out = append(out, atPath(root, pathPrefix+iss.Path, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    iss.Code,
			Message: iss.Message,
			Hint:    iss.Hint,
		}))
	}
	return out
}

// validateJSONSchemaNode — recursive structural check of one JSON Schema node.
// The `state_schema` root needs no special handling: `type: object` is already
// checked by validateStateSchema, and validation of
// `required`/`properties`/`items`/`additionalProperties` is symmetric at all levels.
func validateJSONSchemaNode(node *ast.MappingNode, path string) []diag.Diagnostic {
	if node == nil {
		return nil
	}
	var out []diag.Diagnostic

	// required: must be a sequence of strings (if the key is present).
	reqKV := findKV(node, "required")
	if reqKV != nil {
		seq, ok := reqKV.Value.(*ast.SequenceNode)
		if !ok {
			tok := reqKV.Value.GetToken()
			out = append(out, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:     "state_schema_invalid",
				Message:  "required must be an array of strings",
				YAMLPath: path + ".required",
			}))
		} else {
			for i, item := range seq.Values {
				if _, isStr := item.(*ast.StringNode); !isStr {
					tok := item.GetToken()
					out = append(out, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
						Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
						Code:     "state_schema_invalid",
						Message:  fmt.Sprintf("required[%d] must be a string", i),
						YAMLPath: fmt.Sprintf("%s.required[%d]", path, i),
					}))
				}
			}
		}
	}

	// properties: map<string, mapping>; recurse into each nested schema.
	propsKV := findKV(node, "properties")
	if propsKV != nil {
		propsNode, ok := propsKV.Value.(*ast.MappingNode)
		if !ok {
			tok := propsKV.Value.GetToken()
			out = append(out, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:     "state_schema_invalid",
				Message:  "properties must be a mapping of name → schema",
				YAMLPath: path + ".properties",
			}))
		} else {
			for _, kv := range propsNode.Values {
				keyTok := kv.Key.GetToken()
				if keyTok == nil {
					continue
				}
				subPath := path + ".properties." + keyTok.Value
				subMap, isMap := kv.Value.(*ast.MappingNode)
				if !isMap {
					tok := kv.Value.GetToken()
					out = append(out, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
						Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
						Code:     "state_schema_invalid",
						Message:  fmt.Sprintf("property %q must be a schema (mapping)", keyTok.Value),
						YAMLPath: subPath,
					}))
					continue
				}
				out = append(out, validateJSONSchemaNode(subMap, subPath)...)
			}
		}
	}

	// items: recursion — appears in nested schemas with type=array. Only a
	// mapping (nested schema) is allowed; scalar / sequence is invalid.
	itemsKV := findKV(node, "items")
	if itemsKV != nil {
		if subMap, ok := itemsKV.Value.(*ast.MappingNode); ok {
			out = append(out, validateJSONSchemaNode(subMap, path+".items")...)
		} else {
			tok := itemsKV.Value.GetToken()
			out = append(out, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:     "state_schema_invalid",
				Message:  "items must be a schema (mapping)",
				YAMLPath: path + ".items",
			}))
		}
	}

	// additionalProperties: schema branch → recursion; a bool branch is valid on
	// its own (true/false per JSON Schema draft-07); other values are invalid.
	apKV := findKV(node, "additionalProperties")
	if apKV != nil {
		switch v := apKV.Value.(type) {
		case *ast.MappingNode:
			out = append(out, validateJSONSchemaNode(v, path+".additionalProperties")...)
		case *ast.BoolNode:
			// valid, needs no recursion
		default:
			tok := apKV.Value.GetToken()
			out = append(out, diagAt(tok.Position.Line, tok.Position.Column, diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:     "state_schema_invalid",
				Message:  "additionalProperties must be a boolean or a schema (mapping)",
				YAMLPath: path + ".additionalProperties",
			}))
		}
	}

	return out
}

// findKV returns the MappingValueNode for the key name, or nil.
func findKV(m *ast.MappingNode, name string) *ast.MappingValueNode {
	if m == nil {
		return nil
	}
	for _, kv := range m.Values {
		tok := kv.Key.GetToken()
		if tok != nil && tok.Value == name {
			return kv
		}
	}
	return nil
}

// findScalarValue — the value node under key `name` at one level (no recursion).
func findScalarValue(m *ast.MappingNode, name string) ast.Node {
	kv := findKV(m, name)
	if kv == nil {
		return nil
	}
	return kv.Value
}

// semanticValidateService — at M1.2.b there are no separate semantic invariants
// (cross-file refs and migration chain are out of scope, M1.5). Kept for
// signature symmetry with destiny.go.
func semanticValidateService(_ *ServiceManifest, _ *ast.MappingNode) []diag.Diagnostic {
	return nil
}
