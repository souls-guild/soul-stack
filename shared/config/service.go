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
// Holds only service metadata (description), the `state_schema` contract
// for `incarnation.state` in Postgres, and a flat list of git dependencies.
// Scenarios are auto-discovered from `scenario/<name>/main.yml`, so there is no
// `scenarios:` section here.
//
// The manifest carries NO name (NIM-726). Identity is assigned at registration
// and lives in the registry row; every Vault path the platform derives comes
// from the incarnation row's service string, never from this file. A manifest
// name was therefore never checked against the registered one — a mistyped one
// silently fenced the wrong namespace — so the field was removed rather than
// made advisory. Tooling that needs the name takes it as an argument
// (`soul-lint --service-name`).
type ServiceManifest struct {
	Description string `yaml:"description,omitempty"`

	// The manifest carries NO state-schema version either (NIM-735). It is the top
	// of the migration ladder `migrations/<NNN>_<slug>/`, read by
	// [ScanMigrationLadder]; an empty `migrations/` means
	// [BaseStateSchemaVersion]. A hand-written integer beside a ladder that
	// already states the same number is the failure ADR-007 exists to prevent, and
	// `state_schema_version:` was the one exception that ADR carved for itself.

	// StateSchema is the contract for `incarnation.state`, written in the SAME
	// dialect as a scenario's `input:` — hence the same Go type ([NIM-740]).
	//
	// It used to be a JSON-Schema subset in a flat `map[string]any`, which meant a
	// service described one shape three ways: `input:` per field, `types.yml` with
	// a `required:` list, `state_schema` in JSON Schema. Nothing compared the three,
	// and they had already drifted — `AclUser.state` carries a `default` and is
	// optional on the form while the same accounts were listed
	// `required: [name, perms, state]` here. One dialect leaves nothing to drift.
	//
	// So the root is a map of state field → schema: no `type: object` above it, no
	// `required:` list, no `properties:` wrapper. Those three are refused by name and
	// address rather than ignored (see validateStateSchema) — they mean something
	// today, and reading them silently as field names would give a schema different
	// from the one written. Two things are true here and nowhere else: `type: secret`
	// (the declared secret, [ADR-0083] §1) and a `$type` reference that ADDS its own
	// properties on top of the named type.
	StateSchema InputSchemaMap `yaml:"state_schema"`

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

	// Certificate — optional TLS-certificate policy for the service (NIM-99,
	// nested by NIM-745). `pki_role` is what this service's certs are ISSUED
	// with — at first issue (`core.cert.issued`) as much as at rotation — so it
	// hangs off the section itself; `rotate:` is the auto-rotation policy layered
	// on top. nil = neither declared.
	Certificate *CertificateConfig `yaml:"certificate,omitempty"`

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

// CertificateConfig — the `certificate:` manifest block (NIM-99, nested by NIM-745).
// Holds what the service's TLS certs are issued with, and — optionally, under
// `rotate:` — whether they are auto-rotated.
//
// `pki_role` sits here rather than inside `rotate:` because the role is what a cert
// is ISSUED with, and issuance is not only rotation: `core.cert.issued` mints the
// first cert with it too. While the key lived under `certificate_rotation:` it was
// required only when `enable: true`, so a service that wanted its own PKI role
// without auto-rotation had no way to say so. `certificate: { pki_role: X }` with no
// `rotate:` block is that way, and is a complete section.
type CertificateConfig struct {
	// PKIRole — the Vault PKI role that signs THIS service's certs. Read
	// keeper-side on issuance (`core.cert.issued`) and on the Reaper's re-sign.
	// Required when rotation is enabled; legal on its own.
	PKIRole string `yaml:"pki_role,omitempty"`

	// Rotate — the auto-rotation policy. No block (nil) → rotation off, and
	// nothing else in the section is affected.
	Rotate *CertificateRotateConfig `yaml:"rotate,omitempty"`
}

// CertificateRotateConfig — the `certificate.rotate:` block: whether the service
// supports auto-rotation of its TLS certs and with which operational scenario.
// `enable:false`/omitted → the block is inert (explicit opt-in, security-first);
// keeping `scenario`/`threshold` beside a `false` is how rotation is switched off
// without losing the configuration.
type CertificateRotateConfig struct {
	Enable    bool   `yaml:"enable"`              // enables auto-rotation of the service's certs
	Scenario  string `yaml:"scenario,omitempty"`  // rotation scenario; required when enable:true
	Threshold string `yaml:"threshold,omitempty"` // margin before expiry (`30d`); default, currently informational
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
	// The service-name regex left with the manifest's `name:` (NIM-726): it could only
	// judge a name the manifest stated, and the manifest states none. What remains is
	// serviceregistry.NamePattern at registration, and it is NOT the same rule —
	// `^[a-z][a-z0-9-]*$` admits `redis-` and `a--b`, which the retired
	// `^[a-z][a-z0-9]*(-[a-z0-9]+)*$` refused. Deliberately left alone here: tightening
	// it is a new refusal at the mint point, so it belongs to whoever narrows
	// NamePattern, not to the removal of a second copy. Neither form can produce an
	// unsafe Vault segment — both are strict subsets of ADR-064's `^[a-zA-Z0-9_-]+$`.

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
	"name": "name: removed (NIM-726); a service is named once, at registration, and that name is what every derived " +
		"Vault path and RBAC scope is built from. The manifest copy was never compared against it, so a typo here " +
		"fenced the wrong namespace in silence. Offline tools take the name as an argument instead " +
		"(`soul-lint validate-scenario --service-name <name>`)",
	"version": "version is a git ref under which service is committed, not a manifest field; see ADR-007",
	"state_schema_version": "state_schema_version: removed (NIM-735); the state-schema version is the top of the migration ladder " +
		"`migrations/<NNN>_<slug>/` — an empty migrations/ means version 1, and adding a step is what bumps it. " +
		"The runtime column and the API field of that name are unchanged; only the manifest key is gone (ADR-007 amendment 2026-09-01)",
	"tasks":     "tasks live in scenario/<name>/main.yml (auto-discover); service.yml is manifest-only",
	"steps":     "tasks live in scenario/<name>/main.yml (auto-discover); service.yml is manifest-only",
	"input":     "input lives in scenario/<name>/main.yml (input:-block per docs/input.md), not service.yml",
	"scenarios": "scenarios are auto-discovered from scenario/<name>/ directory; do not enumerate them in service.yml",
	"certificate_rotation": "certificate_rotation: renamed (NIM-745); the block is now `certificate:` with the rotation " +
		"policy nested under `rotate:` — `certificate: { pki_role: <role>, rotate: { enable: true, scenario: <name>, " +
		"threshold: 30d } }`. `pki_role` moved up a level because the role is what a cert is ISSUED with and issuance " +
		"is not only rotation, so `certificate: { pki_role: <role> }` with no `rotate:` block is now a legal section",
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

	// 2) name — gone (NIM-726). Nothing to check here: the key is refused above,
	// through deprecatedServiceKeys, and the reserved-namespace rule it used to
	// carry (NIM-706) now lives only where the name is actually assigned —
	// serviceregistry.validateFields. The offline half was dropped with the field
	// rather than moved: it can only judge a name, and the manifest no longer
	// states one.

	// 3) state_schema_version — gone (NIM-735). Nothing to check here: the key is
	// refused above, through deprecatedServiceKeys. What it used to guard — an
	// integer ≥ 1, and not the float goccy would silently truncate — has no subject
	// left, because the number is no longer written by hand at all. It is the top
	// of the ladder in `migrations/`, which is a count of directories and cannot be
	// a float, be zero, or disagree with the steps it counts.

	// 4) state_schema — required + structural validation.
	if !topKeys["state_schema"] {
		out = append(out, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     "missing_required_field",
			Message:  "state_schema is required at top-level",
			Hint:     "declare state_schema: { <field>: { type: … }, … } — see docs/service/manifest.md → state_schema",
			YAMLPath: "$.state_schema",
		})
	} else {
		out = append(out, validateStateSchema(root, findInputMapping(root, "state_schema"), "$.state_schema", m.StateSchema)...)
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

	// 7) certificate — optional cert policy, with rotation nested (NIM-99, NIM-745).
	out = append(out, validateCertificate(root, m.Certificate)...)

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

// validateCertificate — validation of the optional `certificate:` section (NIM-99,
// nested by NIM-745).
//
// `pki_role` alone is a complete section and is checked for nothing: it names the
// Vault role this service's certs are signed with, which is meaningful without any
// rotation policy. Everything below is about `rotate:`, so a section with no
// `rotate:` block has nothing left to validate. When `rotate.enable: true`,
// `rotate.scenario` (snake/kebab, folder scenario/<name>/) and `pki_role` are
// required — the latter reported against its own path a level up, where it is
// written. `rotate.threshold` follows the `duration` convention. A nil section is
// valid.
func validateCertificate(root *ast.MappingNode, crt *CertificateConfig) []diag.Diagnostic {
	if crt == nil || crt.Rotate == nil {
		return nil
	}
	rot := crt.Rotate
	var out []diag.Diagnostic
	base := "$.certificate.rotate"

	if rot.Enable && rot.Scenario == "" {
		out = append(out, atPath(root, base+".scenario", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "missing_required_field",
			Message: "certificate.rotate.scenario is required when enable: true",
			Hint:    "declare scenario: <name> matching scenario/<name>/main.yml",
		}))
	}
	if rot.Enable && crt.PKIRole == "" {
		out = append(out, atPath(root, "$.certificate.pki_role", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "missing_required_field",
			Message: "certificate.pki_role is required when certificate.rotate.enable: true",
			Hint:    "declare pki_role: <vault-pki-role> used to sign this service's certs, beside rotate: (NOT inside it)",
		}))
	}
	if rot.Scenario != "" && !reScenarioName.MatchString(rot.Scenario) {
		out = append(out, atPath(root, base+".scenario", diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "name_invalid_format",
			Message: fmt.Sprintf("certificate.rotate.scenario %q does not match %s", rot.Scenario, reScenarioName),
			Hint:    "snake_case or kebab-case: lowercase letters/digits with _/- separators; must start with letter",
		}))
	}
	if rot.Threshold != "" {
		if _, err := ParseDuration(rot.Threshold); err != nil {
			out = append(out, atPath(root, base+".threshold", diag.Diagnostic{
				Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
				Code:    "duration_invalid",
				Message: fmt.Sprintf("certificate.rotate.threshold %q is not a valid duration: %v", rot.Threshold, err),
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

// StateSchemaLegacyFormCode — a `state_schema:` root still written in the JSON-Schema
// form the input dialect replaced ([ADR-0086] §11).
const StateSchemaLegacyFormCode = "state_schema_legacy_json_schema_form"

// stateSchemaLegacyRootKeys — the three JSON-Schema keys the root no longer takes,
// each with what the author writes instead. They are RESERVED at the root, not
// merely obsolete: under the new dialect a root key is a state field NAME, so a
// `type: object` left in place would read as a field called `type`, `required: [a,
// b]` as a field called `required`, and `properties:` as a field called
// `properties` holding the real ones one level too deep. Every one of those is a
// schema DIFFERENT from the one the author is looking at, which is why they are
// refused by name and address instead of being quietly re-read (a service cannot
// have a state field named any of the three; that is the price, and it is stated).
var stateSchemaLegacyRootKeys = map[string]string{
	"type": "drop it — the root is a map of state field → schema, and `incarnation.state` is an object by construction",
	"required": "drop the list and mark each field itself: `<field>: { type: …, required: true }`" +
		" — the same spelling `input:` uses",
	"properties": "drop the wrapper — the fields go straight under state_schema:",
}

// validateStateSchema validates the `state_schema:` block as the INPUT DIALECT
// ([NIM-740]): the root is a map of state field → schema, exactly as a scenario's
// `input:` is a map of parameter → schema. The differences the state dialect adds
// (`type: secret`, `key:`/`label:`, a `$type` reference that adds properties) live
// in [schemaDialect]; everything else is the one shared grammar.
//
// Before that, the three keys of the JSON-Schema form the dialect replaced are
// refused where they are written (see [stateSchemaLegacyRootKeys]) and then left
// out of the dialect walk, so a manifest that has not migrated gets one clear
// diagnostic per key rather than three obscure ones about malformed fields.
//
// `m` is the decoded block, `node` its AST mapping (nil when the value is not a
// mapping at all).
func validateStateSchema(root *ast.MappingNode, node *ast.MappingNode, pathPrefix string, m InputSchemaMap) []diag.Diagnostic {
	if node == nil {
		// The key is present in YAML, but the value is not a mapping
		// (null/scalar/sequence). goccy won't raise a decode error for null →
		// InputSchemaMap (just yields nil), so a diagnostic is needed here. For
		// scalar/sequence the generic `type_mismatch` was already emitted by the
		// decode phase; but an explicit diagnostic reads better here too.
		return []diag.Diagnostic{atPath(root, pathPrefix, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:    "state_schema_root_not_object",
			Message: "state_schema must be a mapping of state field → schema",
			Hint:    "declare state_schema: { <field>: { type: … }, … }",
		})}
	}
	var out []diag.Diagnostic

	legacy := map[string]bool{}
	for _, kv := range node.Values {
		keyTok := kv.Key.GetToken()
		if keyTok == nil {
			continue
		}
		hint, isLegacy := stateSchemaLegacyRootKeys[keyTok.Value]
		if !isLegacy {
			continue
		}
		legacy[keyTok.Value] = true
		out = append(out, diagAt(keyTok.Position.Line, keyTok.Position.Column, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     StateSchemaLegacyFormCode,
			Message:  fmt.Sprintf("state_schema no longer takes %q at its root — it is written in the same dialect as a scenario's input:", keyTok.Value),
			Hint:     hint,
			YAMLPath: pathPrefix + "." + keyTok.Value,
		}))
	}

	return append(out, validateInputSchemaMap(m, node, pathPrefix, dialectState, legacy)...)
}

// validateSecretFields turns the refusals of [CollectSecretFields] into positional
// diagnostics ([ADR-0083] §1). The rules themselves live there, in a pure function over
// the decoded schema, because keeper resolves the same declarations at runtime — reveal
// and `core.state.*` — and a second implementation would drift from this one.
//
// A declaration sitting under an unresolved `$type` is NOT judged here: the element
// shape lives in types.yml, which this package never reads, and half a schema would
// refuse every correct use of a shared type. Those are validated once the references
// are resolved — [ValidateStateSchemaSecrets], called by whoever did the resolving
// (keeper's artifact loader, soul-lint's service pass).
func validateSecretFields(root *ast.MappingNode, schema InputSchemaMap, pathPrefix string) []diag.Diagnostic {
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

// ValidateStateSchemaSecrets reports the refusals a declared secret could not be
// judged for until its `$type` reference was resolved.
//
// The two halves of validating a declared secret happen at different times. Everything
// written inline is judged at load, positionally, by validateSecretFields. Everything
// reaching through `$type` cannot be: the element shape is in a file `shared/config`
// never reads. So the caller that resolved the references runs this afterwards.
//
// `before` is the schema as parsed, `after` the same schema resolved. An issue the
// parsed schema already produced was reported at load WITH a line and a column, so it
// is dropped here: one mistake is one diagnostic, and the positional copy is the better
// of the two. Only what the resolve made visible survives.
//
// What survives carries a YAML path but no line — the resolved schema is a value, not a
// document. The path names the declaration, which is what the author needs; the file is
// the caller's to stamp.
func ValidateStateSchemaSecrets(before, after InputSchemaMap) []diag.Diagnostic {
	_, already := CollectSecretFields(before)
	reported := make(map[string]bool, len(already))
	for _, iss := range already {
		reported[iss.Code+"\x00"+iss.Path] = true
	}

	_, issues := CollectSecretFields(after)
	out := make([]diag.Diagnostic, 0, len(issues))
	for _, iss := range issues {
		if reported[iss.Code+"\x00"+iss.Path] {
			continue
		}
		out = append(out, diag.Diagnostic{
			Level: diag.LevelError, Phase: diag.PhaseSchemaValidate,
			Code:     iss.Code,
			Message:  iss.Message,
			Hint:     iss.Hint,
			YAMLPath: "$.state_schema" + iss.Path,
		})
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

// semanticValidateService — no semantic invariant is checked over the manifest
// BYTES. Cross-file refs remain out of scope; the migration chain no longer is, but
// it is not checkable from here — the ladder is a directory beside this file, so it
// belongs to a validator that may touch the filesystem ([ValidateMigrationLadder],
// run by `soul-lint validate-service`). Kept for signature symmetry with destiny.go.
func semanticValidateService(_ *ServiceManifest, _ *ast.MappingNode) []diag.Diagnostic {
	return nil
}
