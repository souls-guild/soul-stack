// Package schema is the canonical description of what a plugin artifact offers.
//
// It replaces the hand-written `manifest.yaml` (NIM-377). The source of truth is
// now a Go value declared next to the module's code — [github.com/souls-guild/soul-stack/sdk/module.Def] —
// from which exactly one document is generated and published twice:
//
//   - stamped into the artifact as a trailer (`soul-mod stamp`), so Keeper always
//     has it and can read it at `plugin.allow` WITHOUT executing the artifact;
//   - written to `dist/schema.json`, so `soul-lint` need not download the binary.
//
// The document is hashed and signed (ADR-026 Sigil), so its bytes must be
// reproducible from the same input on any machine, in any Go map iteration order.
// [Marshal] is the ONE serializer that produces them; the `schema` subcommand of an
// artifact and the `soul-mod` stamping tool both go through it.
//
// # The artifact carries no self-name
//
// There is no `namespace:`, no `name:`, no publisher — an artifact knows only its own
// modules (`acl`, `config`, `info`). Address level 1 comes entirely from registration:
// the alias an operator picks for the source is what turns module `acl` into
// `redis.acl.present`. The same artifact registered twice under two aliases yields two
// address spaces with no change to the bytes.
//
// # Layering
//
// This package holds data types, the canonical serializer, the trailer format and the
// validator, and nothing else — no gRPC, no host-side policy. `sdk/module` re-exports
// the types for authoring ergonomics (`module.String`, `module.NetworkOutbound`);
// `shared/plugin` re-exports them for the host and maps [Issue] onto `shared/diag`.
// One definition, three surfaces.
package schema

// Kind is the plugin kind an artifact implements. All modules in one artifact share
// it: a bundle serves one contract (SoulModule / SshProvider / SoulBeacon), never a
// mix.
type Kind string

// Kind values. Lowercase snake_case, matching the `pluginv1.Kind` proto enum values
// without depending on proto serialization.
const (
	KindSoulModule  Kind = "soul_module"
	KindSSHProvider Kind = "ssh_provider"
	KindSoulBeacon  Kind = "soul_beacon"
)

// ProtocolVersion is the plugin-protocol version this SDK speaks (ADR-020(c)). It is
// the SoulModule API compat flag, NOT a version of the schema document: the document
// format is versioned by the trailer magic ([TrailerMagic]).
const ProtocolVersion int32 = 1

// SupportedProtocolVersions — plugin-protocol versions the host and the linter
// understand. MVP is v1 only; forward-compat is only-add (when v2 appears, `2` joins
// `1` here while `1` stays).
var SupportedProtocolVersions = []int32{1}

// SchemaFileName is the name of the published document next to the artifact in
// `dist/`. `soul-lint` reads this file instead of downloading the binary; the bytes
// are identical to the trailer payload.
const SchemaFileName = "schema.json"

// SchemaSubcommand is the argument that makes an artifact print its own document to
// stdout and exit 0. It is therefore a reserved module name — a module called
// `schema` would be unreachable, so [Validate] rejects it.
const SchemaSubcommand = "schema"

// Document is the whole schema of one artifact: the contract it implements, the
// engine window it declares, and the modules it serves.
//
// Modules is filled for [KindSoulModule]. ProviderKind / ParamsSchema carry the other
// two kinds, which describe a single endpoint rather than a set of named modules (see
// [Validate] for which field belongs to which kind).
type Document struct {
	Kind            Kind   `json:"kind"`
	ProtocolVersion int32  `json:"protocol_version"`
	Compat          Compat `json:"compat,omitzero"`

	// Modules — kind=soul_module. Order is the author's; [Validate] rejects
	// duplicate names, so the set is addressable by name regardless.
	Modules []Module `json:"modules,omitempty"`

	// ProviderKind — kind=ssh_provider: the convention value (`vault_ssh_ca` /
	// `static_key` / `teleport`) or the author's own.
	ProviderKind string `json:"provider_kind,omitempty"`

	// ParamsSchema — kind=ssh_provider and kind=soul_beacon: JSON Schema of the
	// endpoint parameters. An arbitrary object; semantic JSON Schema validation is
	// out of scope here.
	ParamsSchema map[string]any `json:"params_schema,omitempty"`
}

// Compat is the engine window the artifact declares (ADR-0076(c)): the range of
// Keeper releases whose module contract it was written against.
type Compat struct {
	// Keeper — a range expression such as ">=0.9 <2.0". Empty means "no declared
	// bound", which an operator reads as "the author made no promise".
	Keeper string `json:"keeper,omitempty"`
}

// Side is the half of the platform that executes a module: a Soul on the target
// host, or the Keeper itself. It is a property OF THE MODULE — `core.state` can
// only be applied against the incarnation row, `core.pkg` only on a host — so
// the module declares it once and every task addressing that module inherits it.
//
// The zero value is the empty string, read as [SideSoul]: a module that declares
// nothing runs where modules have always run.
type Side string

const (
	// SideSoul — executed by a Soul on each targeted host. The default.
	SideSoul Side = "soul"
	// SideKeeper — executed by the Keeper itself, against no host. A task
	// addressing such a module has no roster, so the orchestration keys that
	// select or fan out over hosts do not apply to it.
	SideKeeper Side = "keeper"
)

// AllSides lists the declarable sides, for diagnostics and for the validator.
var AllSides = []Side{SideSoul, SideKeeper}

// Module is one subject the artifact serves — `acl`, `config`, `info`. Its name is
// address level 2 (`<alias>.<module>.<state>`); level 1 is the registration alias and
// is deliberately absent from this document.
//
// Capabilities and SideEffects sit PER MODULE, not per artifact: invoking `acl` must
// not disclose what `config` touches, and an operator approving one module should see
// exactly that module's footprint.
//
// Both are DISCLOSURE for the operator before approval, not controls. Nothing at
// spawn time sandboxes a plugin — the one real control is the approved sha256, and
// the host refuses to exec anything whose digest differs (ADR-020(g), ADR-026(c) as
// corrected by NIM-377).
type Module struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`

	// Side — which half of the platform executes this module. The module
	// declares it; a task never restates it, and the address alone is what
	// routes the step (docs/scenario/orchestration.md §3).
	//
	// Empty means [SideSoul], and that default is what keeps every plugin
	// written before this field host-side without an edit.
	//
	// The Keeper routes by it (NIM-758): a keeper-side task address its core
	// registry does not know resolves against the discovered plugins, and one
	// declaring [SideKeeper] executes in the Keeper's own process. So this is a
	// switch, and the value is load-bearing — a module declaring the Soul side
	// on such an address is refused by name rather than run.
	Side Side `json:"side,omitempty"`

	// IntroducedIn — the engine release in which this module first appeared
	// (ADR-0076(i)), plain MAJOR.MINOR.PATCH. Empty = at or before the baseline,
	// contributing no floor. Declared per module, per state and per parameter,
	// whichever granularity the addition actually had.
	IntroducedIn string `json:"introduced_in,omitempty"`

	Capabilities []Capability `json:"capabilities,omitempty"`
	SideEffects  []SideEffect `json:"side_effects,omitempty"`

	States map[string]State `json:"states,omitempty"`
}

// State is one state of a module — the `present` in `redis.acl.present`.
type State struct {
	Description string `json:"description,omitempty"`

	// IntroducedIn — the engine release that added this state (ADR-0076(i)). See
	// [Module.IntroducedIn].
	IntroducedIn string `json:"introduced_in,omitempty"`

	Input  Input  `json:"input,omitempty"`
	Output Output `json:"output,omitempty"`
}

// Input is the set of parameters a task's `params:` may carry for this state.
type Input map[string]Param

// Output is the set of fields the state publishes to the caller's `register:`,
// declared with the same shape as [Input] — one description scheme for both ends of
// the contract, as in destiny (docs/destiny/output.md).
//
// Unlike [Input], this block is load-bearing on one field. `secret: true` on an
// output field is the module's declaration that the value it returns there is a
// secret ([ADR-0083] §8) — the replacement for the per-task `no_log:` key, and the
// only signal the platform has, since the module is the only party that knows the
// shape of what it returns. It drives [config.SecretOutputFields], which masks that
// field in the task's observable event and seals the whole register against any
// later cell that reads it. A module author who leaves it unset gets no masking.
//
// Granularity is the whole field: masking is whole-cell, so a secret nested one level
// down is declared by marking what contains it. `secret:` under `items:` is rejected
// rather than ignored.
type Output map[string]Param

// ParamType is the closed set of value types a parameter may declare.
type ParamType string

// Canonical parameter types, plus the `docs/input.md` spellings kept as accepted
// synonyms so nothing a manifest could express becomes inexpressible.
const (
	String ParamType = "string"
	Int    ParamType = "int"
	Bool   ParamType = "bool"
	List   ParamType = "list"
	Map    ParamType = "map"

	// Synonyms from the full input DSL (docs/input.md). Prefer the five above.
	Integer ParamType = "integer"
	Number  ParamType = "number"
	Boolean ParamType = "boolean"
	Array   ParamType = "array"
	Object  ParamType = "object"
)

// Param is the formal description of one parameter.
//
// This is NOT the full `docs/input.md` DSL: an object with nested properties or
// numeric bounds is not expressible here, and decoding is strict, so an unrecognized
// key is an error rather than a passthrough. Adding a field to this struct is a
// forward-compat event — an older engine reading a newer document fails on the key it
// does not know (ADR-0076(i)/(q)).
type Param struct {
	Type        ParamType `json:"type,omitempty"`
	Required    bool      `json:"required,omitempty"`
	Secret      bool      `json:"secret,omitempty"`
	Pattern     string    `json:"pattern,omitempty"`
	Description string    `json:"description,omitempty"`
	Default     any       `json:"default,omitempty"`

	// Form fields (ADR-045 S1): Enum is the closed set of allowed values, Format a
	// string format from the closed catalog, Source a cluster-aware value picker.
	// The backend builds the module form from them.
	Enum   []any        `json:"enum,omitempty"`
	Format string       `json:"format,omitempty"`
	Source *InputSource `json:"source,omitempty"`

	// Multiline and Example (ADR-045 B3) — declarative UI hints (a large textarea +
	// placeholder), not checked by the validator.
	Multiline bool   `json:"multiline,omitempty"`
	Example   string `json:"example,omitempty"`

	// IntroducedIn / Deprecated are the two ends of a parameter's life, and the two
	// halves of ADR-0076's answer to changing a module contract: what a definition
	// using this parameter requires of an engine, and how long an engine keeps
	// honoring it. `unknown_param` is what both converge on — before IntroducedIn
	// and at or after Deprecated.RemovedIn the very same task text is rejected, in
	// between it works.
	IntroducedIn string      `json:"introduced_in,omitempty"`
	Deprecated   *Deprecated `json:"deprecated,omitempty"`

	// Items — list element type or map value type (ADR-045 S7 + amend): for
	// list/array the ELEMENT type, for map/object the VALUE type
	// (`map[string]<items>`).
	Items *Param `json:"items,omitempty"`
}

// InputSource is the discriminator for the catalog a form field's values come from
// (ADR-044 S-T1, ADR-045 S1). Exactly one sub-key defines the set: all SIDs of the
// current incarnation, or the SIDs of one Choir part of it.
type InputSource struct {
	IncarnationHosts bool   `json:"incarnation_hosts,omitempty"`
	Choir            string `json:"choir,omitempty"`
}

// Capability is one entry of the closed capability set a module declares. It is
// disclosure shown to the operator before approval — the host compares the strings
// and refuses to spawn on a mismatch, but nothing confines the process afterwards.
type Capability string

// The closed capability set.
const (
	RunAsRoot       Capability = "run_as_root"
	NetworkOutbound Capability = "network_outbound"
	NetworkInbound  Capability = "network_inbound"
	VaultAccess     Capability = "vault_access"
	FSWriteRoot     Capability = "fs_write_root"
	ExecSubprocess  Capability = "exec_subprocess"
)

// AllCapabilities is the closed set in declaration order, for catalogs and error
// hints.
var AllCapabilities = []Capability{
	RunAsRoot, NetworkOutbound, NetworkInbound, VaultAccess, FSWriteRoot, ExecSubprocess,
}

// SideEffect is one resource a module touches — `{User: "redis_acl_user"}`.
//
// The resource type is a FIELD rather than a map key, which makes the closed enum a
// compile-time fact for the author: a typo is a build error, not a validation error
// found by someone else later. Exactly one field must be set per entry; [Validate]
// rejects zero and rejects two.
type SideEffect struct {
	Service   string `json:"service,omitempty"`
	File      string `json:"file,omitempty"`
	Package   string `json:"package,omitempty"`
	Port      string `json:"port,omitempty"`
	User      string `json:"user,omitempty"`
	Group     string `json:"group,omitempty"`
	Directory string `json:"directory,omitempty"`
	Cron      string `json:"cron,omitempty"`
	Mount     string `json:"mount,omitempty"`
}

// resources returns the (type, value) pairs actually set on the entry, in a fixed
// order. Used by [Validate] for the exactly-one rule and by hosts that render the
// disclosure list.
func (s SideEffect) resources() []struct{ Type, Value string } {
	all := []struct{ Type, Value string }{
		{"service", s.Service},
		{"file", s.File},
		{"package", s.Package},
		{"port", s.Port},
		{"user", s.User},
		{"group", s.Group},
		{"directory", s.Directory},
		{"cron", s.Cron},
		{"mount", s.Mount},
	}
	out := all[:0:0]
	for _, r := range all {
		if r.Value != "" {
			out = append(out, r)
		}
	}
	return out
}

// Resource returns the resource type and value of a well-formed entry. ok is false
// when the entry sets zero or more than one field — a shape [Validate] rejects.
func (s SideEffect) Resource() (resourceType, value string, ok bool) {
	rs := s.resources()
	if len(rs) != 1 {
		return "", "", false
	}
	return rs[0].Type, rs[0].Value, true
}

// Module returns the module with the given name. Names are unique in a valid
// document, so the first match is the only match.
func (d *Document) Module(name string) (Module, bool) {
	for i := range d.Modules {
		if d.Modules[i].Name == name {
			return d.Modules[i], true
		}
	}
	return Module{}, false
}

// ModuleNames returns the declared module names in document order.
func (d *Document) ModuleNames() []string {
	out := make([]string, 0, len(d.Modules))
	for i := range d.Modules {
		out = append(out, d.Modules[i].Name)
	}
	return out
}
