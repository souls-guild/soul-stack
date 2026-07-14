package config

// Static model of `soulprint.self.<...>` paths for static-checking CEL predicates
// (`where:`/`when:`/`changed_when:`/`failed_when:`/`until:`/`loop.when:`).
//
// Source of truth — [ADR-018](docs/adr/0018-soulprint-typed.md) and
// [docs/soul/soulprint.md]: the typed `SoulprintFacts` schema
// (`os`/`kernel`/`cpu`/`memory`/`network`) plus root `sid`/`hostname` and registry
// projections (`covens`, `choirs`, `role`). The canonical form is mandatory: a bare
// `soulprint.<x>` without `.self`/`.hosts`/`.where` is an error (see soulprint.md
// "Canonical form").
//
// The linter checks ONLY the prefix of known fields. The tail after an array field
// (`soulprint.self.network.interfaces[0].ipv4`) and the optionally-nested `extra`
// branches (a reserved ADR-018 extension) are accepted as valid: they are either
// dynamic in shape (arrays, interface keys) or belong to future fields. The goal is
// to catch typos (`os.familly`/`memmory.total_mb`), not to be a full type system.

// soulprintSelfTopLevel — closed whitelist of bare first-level segments under
// `soulprint.self.<segment>`. Deeper descent is checked via soulprintSelfSubPaths
// (nested messages).
//
// Only what exists in SoulprintFacts (ADR-018) is registered + registry projections
// (`covens`/`choirs`/`role`, see docs/soul/soulprint.md "Boundary
// `Soulprint`↔`souls`-registry").
var soulprintSelfTopLevel = map[string]bool{
	"sid":      true, // string, registry+collected
	"hostname": true, // string
	"covens":   true, // list<string>, registry projection
	"choirs":   true, // list<string>, registry projection (ADR-044, mirror of covens)
	"traits":   true, // map<string, scalar|list>, registry projection (ADR-060); keys are dynamic — the third segment is not static-checked
	"role":     true, // string|null, declared (bootstrap-create only)
	"os":       true, // OsFacts
	"kernel":   true, // KernelFacts
	"cpu":      true, // CpuFacts
	"memory":   true, // MemoryFacts
	"network":  true, // NetworkFacts
}

// soulprintSelfSubPaths — exact two-segment paths under `soulprint.self.<msg>.<field>`.
// Used to check typos in nested-message fields. If the top-level segment exists but
// the second is unknown — flag it (a clearly checkable typo like `os.familly`). If
// the top-level is an array/string (`covens`/`sid`/…), the second check does not run
// (any suffix is accepted as dynamic access or a runtime index).
var soulprintSelfSubPaths = map[string]map[string]bool{
	"os": {
		"family":      true,
		"distro":      true,
		"version":     true,
		"codename":    true,
		"arch":        true,
		"pkg_mgr":     true,
		"init_system": true,
	},
	"kernel": {
		"version": true,
		"release": true,
	},
	"cpu": {
		"count":  true,
		"model":  true,
		"vendor": true,
	},
	"memory": {
		"total_mb":     true,
		"available_mb": true,
		"swap_mb":      true,
	},
	"network": {
		"primary_ip": true,
		"fqdn":       true,
		"interfaces": true, // list — deeper descent is dynamic
	},
}

// soulprintScalarTopLevel — top-level segments with a non-message type: no descent
// past the register (no field name to mistype). Used so that
// `soulprint.self.sid.startsWith(...)` (a method call) passes without flagging the
// "third segment `startsWith`".
var soulprintScalarTopLevel = map[string]bool{
	"sid":      true,
	"hostname": true,
	"covens":   true,
	"choirs":   true,
	"role":     true,
}

// CovenLabelValidator — a hook validating a coven label in `on: [coven, …]`
// literals beyond the format (regex). Mirrors the interface from
// keeper/internal/soul (which shared/config deliberately does not depend on); a
// no-op in the pilot, since the environment registry (Q1b ADR-008-amend) does not
// exist yet. Once it does, its client swaps [activeCovenLabelValidator] via
// [SetCovenLabelValidator] at binary startup; soul-lint without the swap keeps
// working as format-only (regex).
type CovenLabelValidator interface {
	Validate(label string) error
}

// NoopCovenLabelValidator — format-only check (the regex is done by [reCovenName]).
// Shape-compatible with keeper/internal/soul.NoopCovenLabelValidator.
type NoopCovenLabelValidator struct{}

// Validate always passes.
func (NoopCovenLabelValidator) Validate(string) error { return nil }

// activeCovenLabelValidator — package-level hook applied inside [validateOnField]
// over the regex form (regex stays the first line). nil → no-op (see
// [covenLabelValidator]). Tests may install a custom hook via
// [SetCovenLabelValidator]; soul-lint in the main flow never calls Set and sees the
// no-op. The "registry from DB on the keeper side" flow is a separate consumer that
// shared/config knows nothing about.
var activeCovenLabelValidator CovenLabelValidator

// SetCovenLabelValidator swaps the global hook. Returns the previous one for
// deterministic restore in tests. A nil argument resets to the no-op.
func SetCovenLabelValidator(v CovenLabelValidator) CovenLabelValidator {
	prev := activeCovenLabelValidator
	activeCovenLabelValidator = v
	return prev
}

// covenLabelValidator — the currently active hook (or no-op if unset). Returned to
// [validateOnField] to check each non-CEL-wrapped label.
func covenLabelValidator() CovenLabelValidator {
	if activeCovenLabelValidator == nil {
		return NoopCovenLabelValidator{}
	}
	return activeCovenLabelValidator
}
