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

	// RevealableSecrets — incarnation secrets revealable by the operator via the
	// reveal endpoint under the incarnation.view-secrets right (NIM-74). Generic:
	// the service declares what may be revealed; the mechanism is not redis-specific.
	RevealableSecrets []RevealableSecret `yaml:"revealable_secrets,omitempty"`

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

// KnownCollectors — the closed set of host-vitals collectors (ADR-072, NIM-87).
var KnownCollectors = []string{"cpu", "mem", "disk", "load", "uptime"}

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
// `name` — a destiny name (kebab-case, single-level) or a module name (two-level
// `<namespace>.<module>`); a different regex applies per context (see
// schemaValidateService → pass over the slices).
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

// RevealableSecret — an entry of the manifest's `revealable_secrets[]` section
// (NIM-74): declaration of an operator-revealable incarnation secret.
//
//   - ID — stable identifier (kebab/snake, unique); the client sends it as
//     `secret_id` on reveal;
//   - Label — UI caption;
//   - Enumerate — state path of an object array (`state.<segment>`); the key is
//     the element's `name` field (redis AclUser.name convention) — the set of
//     valid `key`s;
//   - VaultRef — Vault-path template with `{incarnation}`/`{key}` placeholders
//     (literal strings.ReplaceAll substitution; both values are validated and
//     vault.ParseRef strips traversal). Optional `#field` selects a secret field.
type RevealableSecret struct {
	ID        string `yaml:"id"`
	Label     string `yaml:"label"`
	Enumerate string `yaml:"enumerate"`
	VaultRef  string `yaml:"vault_ref"`
}

var (
	// reServiceName — canonical kebab-case: dash only between alphanumerics, no
	// trailing/leading/double dash. Symmetric with `reDestinyName`.
	reServiceName = regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)*$`)

	// reRevealID — revealable_secrets[] secret id: lowercase identifier with
	// `-`/`_` separators (no trailing/leading/double). Allows snake_case
	// (`user_password`) — the reveal contract fixes secret_id in this form.
	reRevealID = regexp.MustCompile(`^[a-z][a-z0-9]*([_-][a-z0-9]+)*$`)

	// reRevealEnumerate — enumerate form: `state.<segment>[.<segment>…]`
	// (symmetric with rePrefillFromStatePath).
	reRevealEnumerate = regexp.MustCompile(`^state(\.[a-z][a-z0-9_]*)+$`)

	// reRevealPlaceholder — a `{…}` placeholder in vault_ref (for validating the set).
	reRevealPlaceholder = regexp.MustCompile(`\{[^}]*\}`)

	// reDependencyDestinyName — kebab-case single-level destiny name in
	// `destiny[]`. Same as `reDestinyName` (destiny.go), reused directly — a
	// separate regex copy was a source of drift.
	reDependencyDestinyName = reDestinyName

	// reDependencyModuleName — strict two-level form `<namespace>.<module>` for
	// custom modules in `service.yml → modules[]`. Symmetric with `reRequiredModule`
	// (destiny.go); canonical kebab-case in each half (no trailing/leading/double
	// dash), no underscore, naming-rules.md §57/§186.
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
	}

	// 5) destiny[] / modules[] — each entry is valid as `{name, ref}`.
	for i, dep := range m.Destiny {
		out = append(out, validateDependencyRef(root, "destiny", i, dep, reDependencyDestinyName)...)
	}
	for i, dep := range m.Modules {
		out = append(out, validateDependencyRef(root, "modules", i, dep, reDependencyModuleName)...)
	}

	// 6) revealable_secrets[] — reveal declarations (NIM-74).
	seenRevealIDs := make(map[string]int, len(m.RevealableSecrets))
	for i, rs := range m.RevealableSecrets {
		out = append(out, validateRevealableSecret(root, i, rs, seenRevealIDs)...)
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

// validateRevealableSecret — checks one `revealable_secrets[]` entry (NIM-74):
// id (required + reRevealID + unique); enumerate (MVP required + form
// `state.<segment>`); vault_ref (required + contains `{key}` when enumerate is
// set + placeholders only `{incarnation}`/`{key}`).
func validateRevealableSecret(root *ast.MappingNode, idx int, rs RevealableSecret, seen map[string]int) []diag.Diagnostic {
	var out []diag.Diagnostic
	base := fmt.Sprintf("$.revealable_secrets[%d]", idx)

	if rs.ID == "" {
		out = append(out, atPath(root, base+".id", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "missing_required_field",
			Message: fmt.Sprintf("revealable_secrets[%d].id is required", idx),
			Hint:    "declare a stable id (client passes it as secret_id)",
		}))
	} else if !reRevealID.MatchString(rs.ID) {
		out = append(out, atPath(root, base+".id", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "name_invalid_format",
			Message: fmt.Sprintf("revealable_secrets[%d].id %q does not match %s", idx, rs.ID, reRevealID),
			Hint:    "lowercase letters/digits with -/_ separators; must start with letter",
		}))
	} else if prev, dup := seen[rs.ID]; dup {
		out = append(out, atPath(root, base+".id", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "duplicate_id",
			Message: fmt.Sprintf("revealable_secrets[%d].id %q duplicates revealable_secrets[%d].id", idx, rs.ID, prev),
			Hint:    "each revealable secret id must be unique within the list",
		}))
	} else {
		seen[rs.ID] = idx
	}

	if rs.Enumerate == "" {
		out = append(out, atPath(root, base+".enumerate", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "missing_required_field",
			Message: fmt.Sprintf("revealable_secrets[%d].enumerate is required", idx),
			Hint:    "declare enumerate: state.<array> — element .name yields the keys",
		}))
	} else if !reRevealEnumerate.MatchString(rs.Enumerate) {
		out = append(out, atPath(root, base+".enumerate", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "enumerate_invalid_format",
			Message: fmt.Sprintf("revealable_secrets[%d].enumerate %q must have form state.<segment>", idx, rs.Enumerate),
			Hint:    "example: state.redis_users",
		}))
	}

	if rs.VaultRef == "" {
		out = append(out, atPath(root, base+".vault_ref", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "missing_required_field",
			Message: fmt.Sprintf("revealable_secrets[%d].vault_ref is required", idx),
			Hint:    "example: secret/redis/{incarnation}/users/{key}#password",
		}))
		return out
	}
	for _, ph := range reRevealPlaceholder.FindAllString(rs.VaultRef, -1) {
		if ph != "{service}" && ph != "{incarnation}" && ph != "{key}" {
			out = append(out, atPath(root, base+".vault_ref", diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:    "vault_ref_unknown_placeholder",
				Message: fmt.Sprintf("revealable_secrets[%d].vault_ref uses unknown placeholder %s", idx, ph),
				Hint:    "only {service}, {incarnation} and {key} are supported",
			}))
		}
	}
	// enumerate is set (always in MVP) ⇒ reveal is per-element ⇒ the path MUST carry {key}.
	if rs.Enumerate != "" && !strings.Contains(rs.VaultRef, "{key}") {
		out = append(out, atPath(root, base+".vault_ref", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "vault_ref_missing_key",
			Message: fmt.Sprintf("revealable_secrets[%d].vault_ref must contain {key} when enumerate is set", idx),
			Hint:    "per-element reveal requires {key}, e.g. .../users/{key}#password",
		}))
	}
	// {service} AND {incarnation} are REQUIRED (NIM-74 C1 defense-in-depth): the
	// path is bound to the secret namespace of exactly this service and this
	// incarnation (secret/<service>/<incarnation>/…). A static
	// `secret/keeper/jwt-signing-key` without placeholders is rejected at load;
	// the runtime allowlist prefix + floor is the 2nd layer.
	if !strings.Contains(rs.VaultRef, "{service}") || !strings.Contains(rs.VaultRef, "{incarnation}") {
		out = append(out, atPath(root, base+".vault_ref", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "vault_ref_not_service_scoped",
			Message: fmt.Sprintf("revealable_secrets[%d].vault_ref must contain {service} and {incarnation} (per-service/incarnation scoping)", idx),
			Hint:    "scope the path, e.g. secret/{service}/{incarnation}/users/{key}#password",
		}))
	}
	// #<field> is REQUIRED: reveal exposes exactly one scalar secret field
	// (runtime selectRevealField without a field → permanent 404). Caught at load.
	if i := strings.LastIndexByte(rs.VaultRef, '#'); i < 0 || i == len(rs.VaultRef)-1 {
		out = append(out, atPath(root, base+".vault_ref", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "vault_ref_missing_field",
			Message: fmt.Sprintf("revealable_secrets[%d].vault_ref must select a #<field> (single scalar value)", idx),
			Hint:    "append the KV field, e.g. .../users/{key}#password",
		}))
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
	} else if listKey == "modules" && strings.HasPrefix(dep.Name, "core.") {
		// ADR-009 / ADR-015: core modules are available automatically and are NOT
		// listed in `modules:`. A separate code so the operator doesn't confuse it
		// with plain `name_invalid_format` (the name is regex-valid, but the
		// semantics are forbidden).
		out = append(out, atPath(root, base+".name", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "core_module_in_modules_list",
			Message: fmt.Sprintf("%s[%d].name %q is a core module — core modules are always available and must not be listed", listKey, idx, dep.Name),
			Hint:    "Core modules are available automatically - not listed in `modules:` (ADR-009)",
		}))
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
		return "two-level address <namespace>.<module> per architecture.md -> \"Module addressing\"; core-modules are not listed here"
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
