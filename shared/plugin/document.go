// Package plugin is the host-side reader of a plugin artifact's schema document.
//
// The document replaced `manifest.yaml` in NIM-377. It is generated from the Go
// [github.com/souls-guild/soul-stack/sdk/module.Def] values that sit next to a module's
// code, serialized canonically by `sdk/schema`, and published twice: stamped into the
// artifact as a trailer, and written next to it as `schema.json`. Nobody writes it by
// hand, so this package parses rather than lints — precise line/column diagnostics
// stopped earning their cost, and byte-determinism (the document is hashed and signed,
// ADR-026) started mattering more.
//
// The model itself lives in `sdk/schema` and is re-exported here as aliases: one
// definition of a parameter, a state and a module across the SDK, the host and the
// linter. What this package adds is the two ways a host gets at a document —
// [ReadArtifact] for the trailer, [ReadSchemaFile] for the published copy — and the
// mapping of validator findings onto `shared/diag`, which Keeper and `soul-lint` both
// render.
//
// # Fail closed
//
// An artifact with no trailer, or a trailer that does not describe a payload, has no
// readable disclosure — and disclosure is what an operator approves. Every reader here
// treats that as an error on both channels (a returned error AND an error-level
// diagnostic). There is no fallback to a sibling file and no empty-schema default: a
// caller that forgets one of the two checks still fails closed.
//
// # The artifact has no self-name
//
// There is no `namespace:`, no `name:`, no [Document] method that computes a binary
// name — `dist/` holds exactly one executable and the host takes it. Address level 1
// comes from the registration alias an operator chooses, so the same bytes registered
// as `redis` and as `redis-community` never collide.
package plugin

import (
	"errors"
	"os"
	"strings"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"github.com/souls-guild/soul-stack/sdk/schema"
	"github.com/souls-guild/soul-stack/shared/diag"
)

// The model, re-exported from `sdk/schema`. Aliases rather than copies: a plugin author
// and the host that approves their plugin must be describing the same thing, and two
// structs that "mirror" each other drift the first time one of them gains a field.
type (
	Document      = schema.Document
	ModuleDef     = schema.Module
	StateDef      = schema.State
	Input         = schema.Input
	Output        = schema.Output
	InputParamDef = schema.Param
	ParamType     = schema.ParamType
	InputSource   = schema.InputSource
	DeprecatedDef = schema.Deprecated
	Capability    = schema.Capability
	SideEffect    = schema.SideEffect
	Compat        = schema.Compat
	Kind          = schema.Kind
)

// Plugin kinds, re-exported.
const (
	KindSoulModule  = schema.KindSoulModule
	KindSSHProvider = schema.KindSSHProvider
	KindSoulBeacon  = schema.KindSoulBeacon
)

// SchemaFileName is the published document next to the artifact — what `soul-lint`
// reads so it need not download a binary. Its bytes are identical to the trailer's.
const SchemaFileName = schema.SchemaFileName

// DeprecationMinMinors — the minimum deprecation window in MINOR releases (ADR-0076).
const DeprecationMinMinors = schema.DeprecationMinMinors

// SupportedProtocolVersions — plugin-protocol versions the host and the linter
// understand (ADR-020(c)). MVP is v1 only; forward-compat is only-add.
var SupportedProtocolVersions = schema.SupportedProtocolVersions

// ReadArtifact reads the schema document stamped into the artifact at path.
//
// This is the path Keeper takes at `plugin.allow`, and it deliberately does NOT execute
// the artifact: at that moment the binary is not yet approved, and running it to ask
// what it does would defeat the approval it is waiting for. The trailer is read by
// seeking from the end (see `sdk/schema`).
//
// Return contract:
//   - err != nil — there is no readable document: the file could not be opened, or it
//     carries no trailer, or the trailer is malformed. The same fact is also reported
//     as an error-level diagnostic, so a caller checking either channel fails closed.
//   - err == nil, doc == nil — the payload was there but is not a parseable document;
//     diagnostics hold one PhaseParse entry.
//   - err == nil, doc != nil — parsed. Diagnostics may still hold validation errors;
//     check with [diag.HasErrors] before trusting the content.
func ReadArtifact(path string) (*Document, []diag.Diagnostic, error) {
	payload, err := schema.ReadTrailerFile(path)
	if err != nil {
		var code, hint string
		switch {
		case errors.Is(err, schema.ErrNoTrailer):
			code = "schema_trailer_missing"
			hint = "the artifact was never stamped; run `soul-mod stamp <artifact>` in its build"
		case errors.Is(err, schema.ErrTrailerMalformed):
			code = "schema_trailer_malformed"
			hint = "the artifact is truncated or was modified after stamping; rebuild and re-stamp it"
		default:
			code = "io_error"
		}
		return nil, []diag.Diagnostic{{
			Level: diag.LevelError, Phase: diag.PhaseParse,
			File:    path,
			Code:    code,
			Message: err.Error(),
			Hint:    hint,
		}}, err
	}
	doc, diags := ParseDocument(path, payload)
	return doc, diags, nil
}

// ReadSchemaFile reads a published `schema.json`. Same return contract as
// [ReadArtifact], with err != nil only for I/O.
//
// This is the `soul-lint` path: an offline check of a destiny's `params:` needs the
// contract, not the binary. It is NOT a fallback for [ReadArtifact] — a host that
// cannot read an artifact's trailer must refuse the artifact, not go looking for a file
// next to it that anyone could have put there.
func ReadSchemaFile(path string) (*Document, []diag.Diagnostic, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, []diag.Diagnostic{{
			Level: diag.LevelError, Phase: diag.PhaseParse,
			File:    path,
			Code:    "io_error",
			Message: err.Error(),
		}}, err
	}
	doc, diags := ParseDocument(path, src)
	return doc, diags, nil
}

// ParseDocument is the I/O-free entry point: parse, then validate. filename is only the
// `Diagnostic.File` label.
//
// A parse failure is fatal — the document is nil and the single diagnostic says why.
// Validation failures are not: the parsed document comes back so a caller can report
// everything wrong with it at once, which is why [diag.HasErrors] and not `doc != nil`
// is the test for "may I use this".
func ParseDocument(filename string, src []byte) (*Document, []diag.Diagnostic) {
	doc, err := schema.Unmarshal(src)
	if err != nil {
		return nil, []diag.Diagnostic{{
			Level: diag.LevelError, Phase: diag.PhaseParse,
			File:    filename,
			Code:    "schema_parse_error",
			Message: err.Error(),
			Hint:    "the document is generated - do not edit it by hand; rebuild the artifact and re-stamp it",
		}}
	}
	return &doc, Diagnostics(filename, schema.Validate(doc))
}

// Diagnostics maps validator findings onto `shared/diag`, the form Keeper and
// `soul-lint` render. The validator is the SDK's, so an author sees the same code and
// the same sentence at `soul-mod stamp` time that an operator sees at approval time.
//
// Line and column stay zero: the document is generated JSON that nobody reads as text.
// The path (`$.modules[acl].states.present.input.host.type`) is what locates a finding.
func Diagnostics(filename string, issues []schema.Issue) []diag.Diagnostic {
	if len(issues) == 0 {
		return nil
	}
	out := make([]diag.Diagnostic, 0, len(issues))
	for _, i := range issues {
		out = append(out, diag.Diagnostic{
			Level:    diagLevel(i.Level),
			Phase:    diagPhase(i.Phase),
			File:     filename,
			Code:     i.Code,
			Message:  i.Message,
			Hint:     i.Hint,
			YAMLPath: i.Path,
		})
	}
	return out
}

func diagLevel(l schema.Level) diag.Level {
	if l == schema.LevelWarning {
		return diag.LevelWarning
	}
	return diag.LevelError
}

func diagPhase(p schema.Phase) diag.Phase {
	if p == schema.PhaseSemantic {
		return diag.PhaseSemanticValidate
	}
	return diag.PhaseSchemaValidate
}

// ValidateSimple is the convenience form for callers that want an `error` instead of a
// diagnostic list: the first error-level finding, or nil.
func ValidateSimple(doc *Document) error {
	if doc == nil {
		return errors.New("plugin: no schema document")
	}
	for _, i := range schema.Validate(*doc) {
		if i.Level == schema.LevelError {
			return errors.New(i.Code + ": " + i.Message)
		}
	}
	return nil
}

// FirstError joins every error-level diagnostic into one error, or returns nil when
// there are none. For callsites (plugin discovery, the git resolver) that report a
// broken artifact as a single warning line rather than a diagnostic list.
func FirstError(ds []diag.Diagnostic) error {
	var msgs []string
	for _, d := range ds {
		if d.Level != diag.LevelError {
			continue
		}
		msgs = append(msgs, d.Code+": "+d.Message)
	}
	if len(msgs) == 0 {
		return nil
	}
	return errors.New(strings.Join(msgs, "; "))
}

// ProtoKind maps a document kind onto the proto enum, for the cross-check against the
// handshake (which encodes the enum as a string via protojson).
func ProtoKind(k Kind) pluginv1.Kind {
	switch k {
	case KindSoulModule:
		return pluginv1.Kind_KIND_SOUL_MODULE
	case KindSSHProvider:
		return pluginv1.Kind_KIND_SSH_PROVIDER
	case KindSoulBeacon:
		return pluginv1.Kind_KIND_SOUL_BEACON
	default:
		return pluginv1.Kind_KIND_UNSPECIFIED
	}
}

// CapabilityFromString maps a declared capability onto `pluginv1.Capability`, returning
// ok=false for anything outside the closed set. The host uses it to compare a module's
// declaration against `allowed_capabilities`.
func CapabilityFromString(s string) (pluginv1.Capability, bool) {
	switch schema.Capability(s) {
	case schema.RunAsRoot:
		return pluginv1.Capability_CAPABILITY_RUN_AS_ROOT, true
	case schema.NetworkOutbound:
		return pluginv1.Capability_CAPABILITY_NETWORK_OUTBOUND, true
	case schema.NetworkInbound:
		return pluginv1.Capability_CAPABILITY_NETWORK_INBOUND, true
	case schema.VaultAccess:
		return pluginv1.Capability_CAPABILITY_VAULT_ACCESS, true
	case schema.FSWriteRoot:
		return pluginv1.Capability_CAPABILITY_FS_WRITE_ROOT, true
	case schema.ExecSubprocess:
		return pluginv1.Capability_CAPABILITY_EXEC_SUBPROCESS, true
	default:
		return pluginv1.Capability_CAPABILITY_UNSPECIFIED, false
	}
}

// StateOf returns a module's state declaration from a document —
// `doc, "acl", "present"` — the lookup every params check starts with.
func StateOf(doc *Document, module, state string) (StateDef, bool) {
	if doc == nil {
		return StateDef{}, false
	}
	m, ok := doc.Module(module)
	if !ok {
		return StateDef{}, false
	}
	def, ok := m.States[state]
	return def, ok
}
