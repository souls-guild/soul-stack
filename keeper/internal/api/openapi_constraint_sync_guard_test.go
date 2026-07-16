// Guard against drift between the huma struct tags of OpenAPI constraints and
// the AUTHORITATIVE runtime sources of validation.
//
// THE PROBLEM (see delegation): the huma tag `pattern:"…"` / `minimum:"…"` /
// `maximum:"…"` only accepts a string LITERAL — it cannot reference
// const operator.AIDPattern / rbac.RoleNamePattern. So the literal in the tag and
// the runtime const are synced MANUALLY; when one side changes, the other silently
// drifts, and the assembled OpenAPI spec starts lying about the contract.
//
// This test extracts the tag value by REFLECTION over the same op-input structs
// that huma compiles into the spec (reflect.StructTag fields), and checks it
// VERBATIM against the authoritative runtime source (a const pattern or the numeric
// bound of a domain validator). A red test = the tag is stale relative to
// the runtime (or vice versa).
//
// ROLLOUT: before this returns ~145 constraints, each new (field, tag) ↔
// (runtime source) pair is added as one line to constraintSyncCases below.
// This test is the stop invariant of the mass operation: until a pair has been checked
// against the runtime, drift passes silently.
package api

import (
	"reflect"
	"strconv"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/augur"
	"github.com/souls-guild/soul-stack/keeper/internal/errand"
	"github.com/souls-guild/soul-stack/keeper/internal/herald"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/operator"
	"github.com/souls-guild/soul-stack/keeper/internal/oracle"
	"github.com/souls-guild/soul-stack/keeper/internal/pushprovider"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac"
	"github.com/souls-guild/soul-stack/keeper/internal/serviceregistry"
	"github.com/souls-guild/soul-stack/keeper/internal/soul"
)

// Numeric bounds of ssh_target.ssh_port. The authoritative runtime source is the
// domain validator SoulHandler.UpdateSshTargetTyped (handlers/soul.go:1526:
// `req.SSHPort < 1 || req.SSHPort > 65535`). There is no separate const in the code:
// we duplicate the literals here next to the reference, so that when the validator's bound
// changes the test goes red and forces the tag to be synced.
const (
	sshPortRuntimeMin = "1"
	sshPortRuntimeMax = "65535"
)

// Numeric bounds of run batch parameters (Voyage create/preview, Cadence
// create/patch). The authoritative runtime sources are the domain validators:
//   - voyage/cadence batch_size / concurrency / fail_threshold: `*v <= 0` →
//     minimum 1 (handlers/voyage.go:430/436/443, voyage/crud.go:107/110/119,
//     cadence/crud.go:116/122/125; all 422). There is no upper bound for batch_size/
//     fail_threshold (the handler does NOT 422 from above) → the tag has minimum only.
//   - batch_percent: `< 1 || > 100` → [1, 100] (handlers/voyage.go:433,
//     voyage/crud.go:116, cadence/crud.go:119; 422).
//   - VOYAGE concurrency upper bound — voyageMaxConcurrency=500 (handlers/voyage.go:443:
//     `concurrency > voyageMaxConcurrency` → 422). CADENCE concurrency's upper bound is NOT
//     constrained by the runtime (cadence/crud.go only validates `<= 0`) → for Cadence
//     the concurrency maximum tag is NOT set, so as not to 422 an otherwise-accepted value.
//
// There is no exported const for these bounds (literals live in the validators) — we duplicate them here
// next to the reference, like ssh_port in the pilot. Candidates for export (voyage.MinBatchValue
// etc) if the bounds start changing.
const (
	batchValueRuntimeMin   = "1"   // batch_size / concurrency / fail_threshold: <= 0 → 422
	batchPercentRuntimeMin = "1"   // batch_percent: < 1 → 422
	batchPercentRuntimeMax = "100" // batch_percent: > 100 → 422
)

// voyageConcurrencyRuntimeMax — the upper bound of concurrency ONLY for Voyage
// (handlers.voyageMaxConcurrency = 500, handlers/voyage.go:158/443). The const
// is unexported (package handlers) — we duplicate the literal. A candidate for export.
const voyageConcurrencyRuntimeMax = "500"

// ID formats of output fields (ROLLOUT BATCH 3: a documentation pattern on machine-
// generated response-Body IDs). Authoritative sources without an EXPORTED const —
// we duplicate the literal next to the reference (like ssh_port/batch above); candidates for export.
//
//   - ulidRuntimePattern: audit.NewULID → Crockford base32, 26 characters. The authority —
//     the unexported audit.ulidPattern (shared/audit/ulid.go:30, IsValidULID). The same
//     literal is also carried by errandAccepted.ErrandID (huma_errand_accepted.go). A candidate for
//     export as audit.ULIDPattern.
//   - sha256RuntimePattern: hex(sha256), lowercase 64 chars. The authority — hex.EncodeToString
//     over sha256.Sum256 (pluginhost/slot.go:173 for the plugin binary; keyservice.go:287
//     for key_id = SPKI-DER). No exported const (the format is hex.EncodeToString). A candidate
//     for export as sigil.SHA256HexPattern.
//
// SID/AID have EXPORTED consts → the cases reference soul.SIDPattern / operator.AIDPattern
// directly (we don't duplicate the literal).
const (
	ulidRuntimePattern   = "^[0-9A-HJKMNP-TV-Z]{26}$" // = audit.ulidPattern (unexported)
	sha256RuntimePattern = "^[0-9a-f]{64}$"           // = hex(sha256), lowercase 64 chars
)

// Runtime sources of INPUT patterns WITHOUT an exported const — we duplicate the literal next
// to the reference (like ssh_port/batch above); candidates for export.
//
//   - sigilSegmentRuntimePattern: closed-charset path segments of Sigil (namespace/name/ref).
//     The authority — the unexported reSigilSegment (api/handlers/sigil.go:39 + mcp/
//     sigil_revoke.go:17, validateSigilTriple → 422 BEFORE svc.Allow/Revoke). ref here is
//     validated by that SAME validator as a tag-ref (NOT an arbitrary git ref): a slash → 422. A candidate for
//     export as sigil.SegmentPattern.
//   - choirNameRuntimePattern: the Choir name, kebab + `_`. The authority — the unexported
//     choir.choirNamePattern (choir.go:35, ValidChoirName); CreateTyped 422s BEFORE
//     INSERT. The handler inlines the same literal into the error text (handlers/choir.go:156).
//     A candidate for export as choir.NamePattern.
//   - soulPathRuntimePattern: soul_path must start with `/` (an absolute Unix path).
//     The runtime — `req.SoulPath == "" || req.SoulPath[0] != '/'` → 422 (handlers/soul.go:1532).
//     Equivalent = `^/` (start anchor): an empty string doesn't match (422), a bare `/` does match
//     (the runtime ACCEPTS it) — NOT `^/.+`, otherwise a valid `/` would falsely 422.
const (
	sigilSegmentRuntimePattern = "^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$" // = reSigilSegment (unexported, ×2 copies)
	choirNameRuntimePattern    = "^[a-z][a-z0-9_-]*$"                 // = choir.choirNamePattern (unexported)
	soulPathRuntimePattern     = "^/"                                 // = SoulPath[0]!='/' (start-with-slash, non-empty)
)

// constraintTagKind — which huma constraint tag is checked in the case.
type constraintTagKind string

const (
	tagPattern   constraintTagKind = "pattern"
	tagMinimum   constraintTagKind = "minimum"
	tagMaximum   constraintTagKind = "maximum"
	tagMinLength constraintTagKind = "minLength"
	tagMaxLength constraintTagKind = "maxLength"
)

// Length bounds (ROLLOUT BATCH 6). Authoritative runtime sources without an EXPORTED
// const — we duplicate the literal next to the reference (like ssh_port/batch above).
//
//   - reasonRuntimeMinLen: unlock/rerun-last reason — the runtime 422s on
//     `reason == ""` (UnlockTyped / RerunLastTyped; both TypeValidationFailed → 422).
//   - covenRuntimeMaxLen: the length of a Coven label — soul.ValidCoven len>63 → 422
//     (soul.go:81, covenMaxLen=63). The same limit applies to the declared role
//     (validHostRole len>63, incarnation.go:177) and to the incarnation name via the
//     pattern `{0,62}` (max 63). There is no exported const for covenMaxLen (the literal
//     is in ValidCoven) — a candidate for export as soul.CovenMaxLen.
//
// The upper bound of reason is the EXPORTED incarnation.ReasonMaxLen (=500): both the
// maxLength tag and the runtime validator (UnlockTyped / RerunLastTyped 422 on
// `len(reason) > ReasonMaxLen`) reference this single const, so the case's
// runtime value is taken directly via strconv.Itoa (not a literal).
const (
	reasonRuntimeMinLen = "1"  // reason == "" → 422 (lower bound)
	covenRuntimeMaxLen  = "63" // ValidCoven / validHostRole len > 63 → 422
)

// sshUserRuntimeMinLen — ssh_user non-emptiness. The runtime 422s on `req.SSHUser == ""`
// (UpdateSshTargetTyped handlers/soul.go:1529, TypeValidationFailed → 422).
// SoulSshTarget — class-A shared input↔output: minLength:1 on the single schema
// (like the committed manuscript :6378); INPUT really does 422 on empty, OUTPUT is doc-only.
const sshUserRuntimeMinLen = "1"

// synodDescRuntimeMin — Synod description non-emptiness for PATCH. The runtime
// UpdateTyped 422s on `req.Description == ""` (handlers/synod.go). CreateTyped
// ACCEPTS empty (Description *string is optional) → minLength only on Update.
// The upper bound is the EXPORTED rbac.SynodDescriptionMaxLen (=1024); both CreateTyped
// and UpdateTyped 422 on `len > SynodDescriptionMaxLen`.
const synodDescRuntimeMin = "1"

// constraintSyncCase — one checked pair "op-input field tag ↔ runtime source".
//
//	structPtr  — a pointer to a zero value of the op-input struct (for reflect.Type).
//	fieldPath  — the path to the field: a single name ("AID") or a chain through embedded/Body
//	             ("Body", "AID"). Matches how huma descends the struct.
//	tag        — which constraint tag exactly to read.
//	runtime    — the expected value: verbatim the const pattern or the numeric bound
//	             of the domain runtime validator. The SOURCE OF TRUTH is the runtime, not the tag.
//	source     — a human-readable reference to the runtime source, for the error text.
type constraintSyncCase struct {
	name      string
	structPtr any
	fieldPath []string
	tag       constraintTagKind
	runtime   string
	source    string
}

// constraintSyncCases — the registry of pairs. ROLLOUT: add a line here for each
// constraint returned in the spec. If the runtime source is a new const,
// import it and reference it in runtime (don't copy the literal by hand
// if the const is available from this package).
//
// Covered in the current code (pilot tags):
//   - AID on operator.create / operator.get / operator.revoke /
//     operator.issue-token (×4 fields, all ← operator.AIDPattern);
//   - AID on role.revoke-operator (path) and GrantOperatorRequest.AID (body,
//     a shared type for role.grant-operator + synod.add-operator) ← operator.AIDPattern;
//   - role name on role.create ← rbac.RoleNamePattern;
//   - ssh_port minimum/maximum on soul.ssh-target ← the bounds of UpdateSshTargetTyped.
var constraintSyncCases = []constraintSyncCase{
	// --- AID pattern (operator.AIDPattern) ---
	{
		name:      "operator.create AID",
		structPtr: &operatorCreateInput{},
		fieldPath: []string{"Body", "AID"},
		tag:       tagPattern,
		runtime:   operator.AIDPattern,
		source:    "operator.AIDPattern",
	},
	{
		name:      "operator.get AID (path)",
		structPtr: &operatorGetInput{},
		fieldPath: []string{"AID"},
		tag:       tagPattern,
		runtime:   operator.AIDPattern,
		source:    "operator.AIDPattern",
	},
	{
		name:      "operator.revoke AID (path)",
		structPtr: &operatorRevokeInput{},
		fieldPath: []string{"AID"},
		tag:       tagPattern,
		runtime:   operator.AIDPattern,
		source:    "operator.AIDPattern",
	},
	{
		name:      "operator.issue-token AID (path)",
		structPtr: &operatorIssueTokenInput{},
		fieldPath: []string{"AID"},
		tag:       tagPattern,
		runtime:   operator.AIDPattern,
		source:    "operator.AIDPattern",
	},
	{
		name:      "role.revoke-operator AID (path)",
		structPtr: &roleRevokeOperatorInput{},
		fieldPath: []string{"AID"},
		tag:       tagPattern,
		runtime:   operator.AIDPattern,
		source:    "operator.AIDPattern",
	},
	{
		name:      "GrantOperatorRequest AID (role.grant-operator / synod.add-operator body)",
		structPtr: &GrantOperatorRequest{},
		fieldPath: []string{"AID"},
		tag:       tagPattern,
		runtime:   operator.AIDPattern,
		source:    "operator.AIDPattern",
	},

	// --- role name pattern (rbac.RoleNamePattern) ---
	{
		name:      "role.create name",
		structPtr: &roleCreateInput{},
		fieldPath: []string{"Body", "Name"},
		tag:       tagPattern,
		runtime:   rbac.RoleNamePattern,
		source:    "rbac.RoleNamePattern",
	},

	// --- ssh_port bounds (UpdateSshTargetTyped, handlers/soul.go:1526) ---
	{
		name:      "soul.ssh-target ssh_port minimum",
		structPtr: &soulSshTargetInput{},
		fieldPath: []string{"Body", "SSHPort"},
		tag:       tagMinimum,
		runtime:   sshPortRuntimeMin,
		source:    "SoulHandler.UpdateSshTargetTyped (ssh_port >= 1)",
	},
	{
		name:      "soul.ssh-target ssh_port maximum",
		structPtr: &soulSshTargetInput{},
		fieldPath: []string{"Body", "SSHPort"},
		tag:       tagMaximum,
		runtime:   sshPortRuntimeMax,
		source:    "SoulHandler.UpdateSshTargetTyped (ssh_port <= 65535)",
	},

	// --- kebab name name/on_beacon (oracle.NamePattern ^[a-z0-9-]{1,63}$) ---
	// The authority — oracle.NamePattern (oracle/validate.go); the same literal is also carried by
	// augur.NamePattern / herald.NamePattern (see below). Each source is its own —
	// we check the field against ITS OWN domain validator, not another same-shaped one.
	{
		name:      "vigil.create name",
		structPtr: &vigilCreateInput{},
		fieldPath: []string{"Body", "Name"},
		tag:       tagPattern,
		runtime:   oracle.NamePattern,
		source:    "oracle.NamePattern (CreateVigilTyped)",
	},
	{
		name:      "decree.create name",
		structPtr: &decreeCreateInput{},
		fieldPath: []string{"Body", "Name"},
		tag:       tagPattern,
		runtime:   oracle.NamePattern,
		source:    "oracle.NamePattern (CreateDecreeTyped)",
	},
	{
		name:      "decree.create on_beacon",
		structPtr: &decreeCreateInput{},
		fieldPath: []string{"Body", "OnBeacon"},
		tag:       tagPattern,
		runtime:   oracle.NamePattern,
		source:    "oracle.NamePattern (on_beacon = Vigil name, CreateDecreeTyped)",
	},
	{
		name:      "omen.create name",
		structPtr: &omenCreateInput{},
		fieldPath: []string{"Body", "Name"},
		tag:       tagPattern,
		runtime:   augur.NamePattern,
		source:    "augur.NamePattern (CreateOmenTyped)",
	},
	{
		name:      "herald.create name",
		structPtr: &heraldCreateInput{},
		fieldPath: []string{"Body", "Name"},
		tag:       tagPattern,
		runtime:   herald.NamePattern,
		source:    "herald.NamePattern (CreateHeraldTyped)",
	},
	{
		name:      "tiding.create name",
		structPtr: &tidingCreateInput{},
		fieldPath: []string{"Body", "Name"},
		tag:       tagPattern,
		runtime:   herald.NamePattern,
		source:    "herald.NamePattern (CreateTidingTyped)",
	},
	{
		name:      "VoyageNotify herald (voyage.create / cadence.create notify[].herald)",
		structPtr: &VoyageNotify{},
		fieldPath: []string{"Herald"},
		tag:       tagPattern,
		runtime:   herald.NamePattern,
		source:    "herald.NamePattern (prepareNotifyTidingsErr, voyage_notify.go:103)",
	},

	// --- service/synod name (^[a-z][a-z0-9-]*$) ---
	// service name — its own serviceregistry.NamePattern (NOT rbac.RoleNamePattern,
	// even though the literal matches: different domains, different SQL CHECKs). synod name — reRoleName
	// (rbac.RoleNamePattern), shared with role name by synod.go's decision.
	{
		name:      "service.register name",
		structPtr: &serviceRegisterInput{},
		fieldPath: []string{"Body", "Name"},
		tag:       tagPattern,
		runtime:   serviceregistry.NamePattern,
		source:    "serviceregistry.NamePattern (CreateService)",
	},
	{
		name:      "synod.create name",
		structPtr: &synodCreateInput{},
		fieldPath: []string{"Body", "Name"},
		tag:       tagPattern,
		runtime:   rbac.RoleNamePattern,
		source:    "rbac.RoleNamePattern (reRoleName, CreateSynod)",
	},

	// --- coven label (soul.CovenPattern ^[a-z][a-z0-9]*(-[a-z0-9]+)*$) ---
	// covens[] / labels[] — per-element ValidCoven (an empty element is rejected by both the huma
	// pattern and ValidCoven → matching). label (append/remove) and selector.coven are NOT
	// tagged: label validity depends on mode (replace requires an EMPTY label,
	// the pattern would falsely 422 a valid replace), selector.coven is a filter.
	{
		name:      "soul.create covens[]",
		structPtr: &soulCreateInput{},
		fieldPath: []string{"Body", "Covens"},
		tag:       tagPattern,
		runtime:   soul.CovenPattern,
		source:    "soul.CovenPattern (CreateTyped, soul.go:221)",
	},
	{
		name:      "soul.coven-assign labels[]",
		structPtr: &soulCovenAssignInput{},
		fieldPath: []string{"Body", "Labels"},
		tag:       tagPattern,
		runtime:   soul.CovenPattern,
		source:    "soul.CovenPattern (replace-mode per-element, soul.go:1253)",
	},
	{
		name:      "incarnation.create covens[]",
		structPtr: &incCreateInput{},
		fieldPath: []string{"Body", "Covens"},
		tag:       tagPattern,
		runtime:   soul.CovenPattern,
		source:    "soul.CovenPattern (incarnation CreateTyped, incarnation_typed.go:95)",
	},

	// --- timeout_seconds upper bound (errand.MaxTimeoutSeconds = 300) ---
	// ErrandRunRequest.timeout_seconds: the runtime 422s ONLY on `> MaxTimeoutSeconds`
	// (dispatcher.go:685, dispatchError → ErrTimeoutOutOfRange). There is NO LOWER
	// bound: timeout_seconds=0 (or omitted) → DefaultTimeoutSeconds (dispatcher.go:683,
	// valid) → minimum:"1" would falsely 422 a valid 0 (the zero trap). Source is the
	// EXPORTED const errand.MaxTimeoutSeconds (not a literal).
	{
		name:      "errand exec timeout_seconds maximum",
		structPtr: &errandExecInput{},
		fieldPath: []string{"Body", "TimeoutSeconds"},
		tag:       tagMaximum,
		runtime:   strconv.Itoa(errand.MaxTimeoutSeconds),
		source:    "errand.MaxTimeoutSeconds (dispatcher.go:685, ErrTimeoutOutOfRange → 422)",
	},

	// --- Voyage create batch bounds (handlers/voyage.go + voyage/crud.go) ---
	// All fields are *int omitempty: nil/omitted → default (no 422); explicit 0/<1 → 422
	// (the same predicate as huma minimum). batch_size/fail_threshold have only a minimum
	// (no upper-bound validation). batch_percent is [1,100]. concurrency is [1,500] (the
	// upper bound voyageMaxConcurrency applies to Voyage ONLY).
	{
		name:      "voyage.create batch_size minimum",
		structPtr: &voyageCreateInput{},
		fieldPath: []string{"Body", "BatchSize"},
		tag:       tagMinimum,
		runtime:   batchValueRuntimeMin,
		source:    "validateVoyageRequest (handlers/voyage.go:430, batch_size > 0)",
	},
	{
		name:      "voyage.create batch_percent minimum",
		structPtr: &voyageCreateInput{},
		fieldPath: []string{"Body", "BatchPercent"},
		tag:       tagMinimum,
		runtime:   batchPercentRuntimeMin,
		source:    "validateVoyageRequest (handlers/voyage.go:433, batch_percent in [1,100])",
	},
	{
		name:      "voyage.create batch_percent maximum",
		structPtr: &voyageCreateInput{},
		fieldPath: []string{"Body", "BatchPercent"},
		tag:       tagMaximum,
		runtime:   batchPercentRuntimeMax,
		source:    "validateVoyageRequest (handlers/voyage.go:433, batch_percent in [1,100])",
	},
	{
		name:      "voyage.create concurrency minimum",
		structPtr: &voyageCreateInput{},
		fieldPath: []string{"Body", "Concurrency"},
		tag:       tagMinimum,
		runtime:   batchValueRuntimeMin,
		source:    "validateVoyageRequest (handlers/voyage.go:443, concurrency in [1,500])",
	},
	{
		name:      "voyage.create concurrency maximum",
		structPtr: &voyageCreateInput{},
		fieldPath: []string{"Body", "Concurrency"},
		tag:       tagMaximum,
		runtime:   voyageConcurrencyRuntimeMax,
		source:    "validateVoyageRequest (handlers/voyage.go:443, concurrency <= voyageMaxConcurrency=500)",
	},
	{
		name:      "voyage.create fail_threshold minimum",
		structPtr: &voyageCreateInput{},
		fieldPath: []string{"Body", "FailThreshold"},
		tag:       tagMinimum,
		runtime:   batchValueRuntimeMin,
		source:    "validateVoyageRequest (handlers/voyage.go:436, fail_threshold > 0)",
	},

	// --- Cadence create batch bounds (cadence/crud.go validate, via Insert) ---
	// Cadence's concurrency has NO runtime upper bound → we don't tag a maximum.
	{
		name:      "cadence.create batch_size minimum",
		structPtr: &cadenceCreateInput{},
		fieldPath: []string{"Body", "BatchSize"},
		tag:       tagMinimum,
		runtime:   batchValueRuntimeMin,
		source:    "cadence.validate (cadence/crud.go:116, batch_size > 0)",
	},
	{
		name:      "cadence.create batch_percent minimum",
		structPtr: &cadenceCreateInput{},
		fieldPath: []string{"Body", "BatchPercent"},
		tag:       tagMinimum,
		runtime:   batchPercentRuntimeMin,
		source:    "cadence.validate (cadence/crud.go:119, batch_percent in [1,100])",
	},
	{
		name:      "cadence.create batch_percent maximum",
		structPtr: &cadenceCreateInput{},
		fieldPath: []string{"Body", "BatchPercent"},
		tag:       tagMaximum,
		runtime:   batchPercentRuntimeMax,
		source:    "cadence.validate (cadence/crud.go:119, batch_percent in [1,100])",
	},
	{
		name:      "cadence.create concurrency minimum",
		structPtr: &cadenceCreateInput{},
		fieldPath: []string{"Body", "Concurrency"},
		tag:       tagMinimum,
		runtime:   batchValueRuntimeMin,
		source:    "cadence.validate (cadence/crud.go:122, concurrency > 0)",
	},
	{
		name:      "cadence.create fail_threshold minimum",
		structPtr: &cadenceCreateInput{},
		fieldPath: []string{"Body", "FailThreshold"},
		tag:       tagMinimum,
		runtime:   batchValueRuntimeMin,
		source:    "cadence.validate (cadence/crud.go:125, fail_threshold > 0)",
	},

	// --- Cadence PATCH batch bounds (same cadence.validate, via Update) ---
	{
		name:      "cadence.patch batch_size minimum",
		structPtr: &cadencePatchInput{},
		fieldPath: []string{"Body", "BatchSize"},
		tag:       tagMinimum,
		runtime:   batchValueRuntimeMin,
		source:    "cadence.validate (cadence/crud.go:116 via Update, batch_size > 0)",
	},
	{
		name:      "cadence.patch batch_percent minimum",
		structPtr: &cadencePatchInput{},
		fieldPath: []string{"Body", "BatchPercent"},
		tag:       tagMinimum,
		runtime:   batchPercentRuntimeMin,
		source:    "cadence.validate (cadence/crud.go:119 via Update, batch_percent in [1,100])",
	},
	{
		name:      "cadence.patch batch_percent maximum",
		structPtr: &cadencePatchInput{},
		fieldPath: []string{"Body", "BatchPercent"},
		tag:       tagMaximum,
		runtime:   batchPercentRuntimeMax,
		source:    "cadence.validate (cadence/crud.go:119 via Update, batch_percent in [1,100])",
	},
	{
		name:      "cadence.patch concurrency minimum",
		structPtr: &cadencePatchInput{},
		fieldPath: []string{"Body", "Concurrency"},
		tag:       tagMinimum,
		runtime:   batchValueRuntimeMin,
		source:    "cadence.validate (cadence/crud.go:122 via Update, concurrency > 0)",
	},
	{
		name:      "cadence.patch fail_threshold minimum",
		structPtr: &cadencePatchInput{},
		fieldPath: []string{"Body", "FailThreshold"},
		tag:       tagMinimum,
		runtime:   batchValueRuntimeMin,
		source:    "cadence.validate (cadence/crud.go:125 via Update, fail_threshold > 0)",
	},

	// ====================================================================
	// ID FORMATS OF OUTPUT FIELDS (ROLLOUT BATCH 3). A documentation pattern: huma does
	// NOT validate the response body (empirically 200, not 500) → the tag is purely
	// format documentation for client codegen. The case's goal is "documented format ==
	// the canonical runtime source of the generator/validator". structPtr is the EXPORTED
	// reply struct directly (fieldPath = field name, WITHOUT a Body wrapper — the output
	// Body IS the struct itself). For []string fields (covens-style) the pattern sits on items[].
	// ====================================================================

	// --- ULID on machine-generated IDs (audit.NewULID) ---
	{
		name:      "errandAccepted errand_id ULID",
		structPtr: &errandAccepted{},
		fieldPath: []string{"ErrandID"},
		tag:       tagPattern,
		runtime:   ulidRuntimePattern,
		source:    "audit.NewULID (errand, dispatcher.go:262)",
	},
	{
		name:      "ErrandResult errand_id ULID",
		structPtr: &ErrandResult{},
		fieldPath: []string{"ErrandID"},
		tag:       tagPattern,
		runtime:   ulidRuntimePattern,
		source:    "audit.NewULID (errand, dispatcher.go:262)",
	},
	{
		name:      "IncarnationCreateReply apply_id ULID",
		structPtr: &IncarnationCreateReply{},
		fieldPath: []string{"ApplyID"},
		tag:       tagPattern,
		runtime:   ulidRuntimePattern,
		source:    "audit.NewULID (incarnation_typed.go:163)",
	},
	{
		name:      "IncarnationRunReply apply_id ULID",
		structPtr: &IncarnationRunReply{},
		fieldPath: []string{"ApplyID"},
		tag:       tagPattern,
		runtime:   ulidRuntimePattern,
		source:    "audit.NewULID (incarnation_typed.go:265)",
	},
	{
		name:      "IncarnationUpgradeReply apply_id ULID",
		structPtr: &IncarnationUpgradeReply{},
		fieldPath: []string{"ApplyID"},
		tag:       tagPattern,
		runtime:   ulidRuntimePattern,
		source:    "audit.NewULID (incarnation upgrade)",
	},
	{
		name:      "IncarnationRerunLastReply apply_id ULID",
		structPtr: &IncarnationRerunLastReply{},
		fieldPath: []string{"ApplyID"},
		tag:       tagPattern,
		runtime:   ulidRuntimePattern,
		source:    "audit.NewULID (incarnation rerun-last)",
	},
	{
		name:      "IncarnationDestroyReply apply_id ULID",
		structPtr: &IncarnationDestroyReply{},
		fieldPath: []string{"ApplyID"},
		tag:       tagPattern,
		runtime:   ulidRuntimePattern,
		source:    "audit.NewULID (incarnation destroy)",
	},
	{
		name:      "StateHistoryEntry apply_id ULID",
		structPtr: &StateHistoryEntry{},
		fieldPath: []string{"ApplyID"},
		tag:       tagPattern,
		runtime:   ulidRuntimePattern,
		source:    "audit.NewULID (state_history.apply_id)",
	},
	{
		name:      "StateHistoryEntry history_id ULID",
		structPtr: &StateHistoryEntry{},
		fieldPath: []string{"HistoryID"},
		tag:       tagPattern,
		runtime:   ulidRuntimePattern,
		source:    "ULID (миграция 006, history_id)",
	},
	{
		name:      "PushApplyReply apply_id ULID",
		structPtr: &PushApplyReply{},
		fieldPath: []string{"ApplyID"},
		tag:       tagPattern,
		runtime:   ulidRuntimePattern,
		source:    "audit.NewULID (pushorch/run.go:182)",
	},
	{
		name:      "PushApplyView apply_id ULID",
		structPtr: &PushApplyView{},
		fieldPath: []string{"ApplyID"},
		tag:       tagPattern,
		runtime:   ulidRuntimePattern,
		source:    "audit.NewULID (pushorch/run.go:182)",
	},
	{
		name:      "PushRunListEntry apply_id ULID",
		structPtr: &PushRunListEntry{},
		fieldPath: []string{"ApplyID"},
		tag:       tagPattern,
		runtime:   ulidRuntimePattern,
		source:    "audit.NewULID (pushorch/run.go:182)",
	},
	{
		name:      "Voyage voyage_id ULID",
		structPtr: &Voyage{},
		fieldPath: []string{"VoyageID"},
		tag:       tagPattern,
		runtime:   ulidRuntimePattern,
		source:    "audit.NewULID (handlers/voyage.go:988)",
	},
	{
		name:      "VoyageTargetsReply voyage_id ULID",
		structPtr: &VoyageTargetsReply{},
		fieldPath: []string{"VoyageID"},
		tag:       tagPattern,
		runtime:   ulidRuntimePattern,
		source:    "audit.NewULID (handlers/voyage.go:988)",
	},
	{
		name:      "VoyageCreateReply voyage_id ULID",
		structPtr: &VoyageCreateReply{},
		fieldPath: []string{"VoyageID"},
		tag:       tagPattern,
		runtime:   ulidRuntimePattern,
		source:    "audit.NewULID (handlers/voyage.go:988)",
	},
	{
		name:      "VoyageCancelReply voyage_id ULID",
		structPtr: &VoyageCancelReply{},
		fieldPath: []string{"VoyageID"},
		tag:       tagPattern,
		runtime:   ulidRuntimePattern,
		source:    "audit.NewULID (handlers/voyage.go:988)",
	},
	{
		name:      "CadenceCreateReply cadence_id ULID",
		structPtr: &CadenceCreateReply{},
		fieldPath: []string{"CadenceID"},
		tag:       tagPattern,
		runtime:   ulidRuntimePattern,
		source:    "audit.NewULID (handlers/cadence.go:327)",
	},
	{
		name:      "CadenceEnabledReply cadence_id ULID",
		structPtr: &CadenceEnabledReply{},
		fieldPath: []string{"CadenceID"},
		tag:       tagPattern,
		runtime:   ulidRuntimePattern,
		source:    "audit.NewULID (handlers/cadence.go:327)",
	},
	{
		name:      "cadence (element) cadence_id ULID",
		structPtr: &cadence{},
		fieldPath: []string{"CadenceID"},
		tag:       tagPattern,
		runtime:   ulidRuntimePattern,
		source:    "audit.NewULID (handlers/cadence.go:327)",
	},
	{
		name:      "SoulHistoryItem id ULID",
		structPtr: &SoulHistoryItem{},
		fieldPath: []string{"ID"},
		tag:       tagPattern,
		runtime:   ulidRuntimePattern,
		source:    "ULID (apply_id|errand_id, soul/history.go:55)",
	},
	{
		name:      "SoulHistoryItem voyage_id ULID",
		structPtr: &SoulHistoryItem{},
		fieldPath: []string{"VoyageID"},
		tag:       tagPattern,
		runtime:   ulidRuntimePattern,
		source:    "audit.NewULID (voyage_id)",
	},
	{
		name:      "AuditEvent id ULID",
		structPtr: &AuditEvent{},
		fieldPath: []string{"ID"},
		tag:       tagPattern,
		runtime:   ulidRuntimePattern,
		source:    "ULID (миграция 001, audit_id)",
	},
	{
		name:      "AuditEvent correlation_id ULID",
		structPtr: &AuditEvent{},
		fieldPath: []string{"CorrelationID"},
		tag:       tagPattern,
		runtime:   ulidRuntimePattern,
		source:    "ULID (миграция 001, correlation_id)",
	},

	// --- sha256 hex on hash-derived IDs ---
	{
		name:      "PluginSigilAllowReply sha256 hex",
		structPtr: &PluginSigilAllowReply{},
		fieldPath: []string{"SHA256"},
		tag:       tagPattern,
		runtime:   sha256RuntimePattern,
		source:    "hex(sha256) binary (pluginhost/slot.go:173)",
	},
	{
		name:      "PluginSigilView sha256 hex",
		structPtr: &PluginSigilView{},
		fieldPath: []string{"SHA256"},
		tag:       tagPattern,
		runtime:   sha256RuntimePattern,
		source:    "hex(sha256) binary (pluginhost/slot.go:173)",
	},
	{
		name:      "SigilKeyIntroduceReply key_id hex",
		structPtr: &SigilKeyIntroduceReply{},
		fieldPath: []string{"KeyID"},
		tag:       tagPattern,
		runtime:   sha256RuntimePattern,
		source:    "hex(sha256(SPKI-DER)) (keyservice.go:287)",
	},
	{
		name:      "SigilKeyView key_id hex",
		structPtr: &SigilKeyView{},
		fieldPath: []string{"KeyID"},
		tag:       tagPattern,
		runtime:   sha256RuntimePattern,
		source:    "hex(sha256(SPKI-DER)) (keyservice.go:287)",
	},

	// --- SID on sid output fields (← soul.SIDPattern) ---
	{
		name:      "ErrandResult sid",
		structPtr: &ErrandResult{},
		fieldPath: []string{"SID"},
		tag:       tagPattern,
		runtime:   soul.SIDPattern,
		source:    "soul.SIDPattern",
	},
	{
		name:      "SoulCreateReply sid",
		structPtr: &SoulCreateReply{},
		fieldPath: []string{"SID"},
		tag:       tagPattern,
		runtime:   soul.SIDPattern,
		source:    "soul.SIDPattern",
	},
	{
		name:      "SoulIssueTokenReply sid",
		structPtr: &SoulIssueTokenReply{},
		fieldPath: []string{"SID"},
		tag:       tagPattern,
		runtime:   soul.SIDPattern,
		source:    "soul.SIDPattern",
	},
	{
		name:      "SoulSshTargetReply sid",
		structPtr: &SoulSshTargetReply{},
		fieldPath: []string{"SID"},
		tag:       tagPattern,
		runtime:   soul.SIDPattern,
		source:    "soul.SIDPattern",
	},
	{
		name:      "SoulListEntry sid",
		structPtr: &SoulListEntry{},
		fieldPath: []string{"SID"},
		tag:       tagPattern,
		runtime:   soul.SIDPattern,
		source:    "soul.SIDPattern",
	},
	{
		name:      "SoulHistoryReply sid",
		structPtr: &SoulHistoryReply{},
		fieldPath: []string{"SID"},
		tag:       tagPattern,
		runtime:   soul.SIDPattern,
		source:    "soul.SIDPattern",
	},
	{
		name:      "Voice sid",
		structPtr: &Voice{},
		fieldPath: []string{"SID"},
		tag:       tagPattern,
		runtime:   soul.SIDPattern,
		source:    "soul.SIDPattern",
	},

	// --- AID on output *_by_aid / aid / archon_aid / operators[] (← operator.AIDPattern) ---
	// Safe: huma does NOT validate output (200, not 500); migration 058 — the current AID
	// pattern is a superset of the old one, legacy AIDs match too.
	{
		name:      "OperatorCreateReply aid",
		structPtr: &OperatorCreateReply{},
		fieldPath: []string{"AID"},
		tag:       tagPattern,
		runtime:   operator.AIDPattern,
		source:    "operator.AIDPattern",
	},
	{
		name:      "OperatorCreateReply created_by_aid",
		structPtr: &OperatorCreateReply{},
		fieldPath: []string{"CreatedByAID"},
		tag:       tagPattern,
		runtime:   operator.AIDPattern,
		source:    "operator.AIDPattern",
	},
	{
		name:      "Operator aid",
		structPtr: &Operator{},
		fieldPath: []string{"AID"},
		tag:       tagPattern,
		runtime:   operator.AIDPattern,
		source:    "operator.AIDPattern",
	},
	{
		name:      "Operator created_by_aid",
		structPtr: &Operator{},
		fieldPath: []string{"CreatedByAID"},
		tag:       tagPattern,
		runtime:   operator.AIDPattern,
		source:    "operator.AIDPattern",
	},
	{
		name:      "IssueTokenReply aid",
		structPtr: &IssueTokenReply{},
		fieldPath: []string{"AID"},
		tag:       tagPattern,
		runtime:   operator.AIDPattern,
		source:    "operator.AIDPattern",
	},
	{
		name:      "IncarnationUnlockReply unlocked_by_aid",
		structPtr: &IncarnationUnlockReply{},
		fieldPath: []string{"UnlockedByAID"},
		tag:       tagPattern,
		runtime:   operator.AIDPattern,
		source:    "operator.AIDPattern",
	},
	{
		name:      "IncarnationGetReply created_by_aid",
		structPtr: &IncarnationGetReply{},
		fieldPath: []string{"CreatedByAID"},
		tag:       tagPattern,
		runtime:   operator.AIDPattern,
		source:    "operator.AIDPattern",
	},
	{
		name:      "StateHistoryEntry changed_by_aid",
		structPtr: &StateHistoryEntry{},
		fieldPath: []string{"ChangedByAID"},
		tag:       tagPattern,
		runtime:   operator.AIDPattern,
		source:    "operator.AIDPattern",
	},
	{
		name:      "Voyage started_by_aid",
		structPtr: &Voyage{},
		fieldPath: []string{"StartedByAID"},
		tag:       tagPattern,
		runtime:   operator.AIDPattern,
		source:    "operator.AIDPattern",
	},
	{
		name:      "cadence (element) created_by_aid",
		structPtr: &cadence{},
		fieldPath: []string{"CreatedByAID"},
		tag:       tagPattern,
		runtime:   operator.AIDPattern,
		source:    "operator.AIDPattern",
	},
	{
		name:      "PushApplyView started_by_aid",
		structPtr: &PushApplyView{},
		fieldPath: []string{"StartedByAID"},
		tag:       tagPattern,
		runtime:   operator.AIDPattern,
		source:    "operator.AIDPattern",
	},
	{
		name:      "PushRunListEntry started_by_aid",
		structPtr: &PushRunListEntry{},
		fieldPath: []string{"StartedByAID"},
		tag:       tagPattern,
		runtime:   operator.AIDPattern,
		source:    "operator.AIDPattern",
	},
	{
		name:      "PushProvider created_by_aid",
		structPtr: &PushProvider{},
		fieldPath: []string{"CreatedByAID"},
		tag:       tagPattern,
		runtime:   operator.AIDPattern,
		source:    "operator.AIDPattern",
	},
	{
		name:      "PushProvider updated_by_aid",
		structPtr: &PushProvider{},
		fieldPath: []string{"UpdatedByAID"},
		tag:       tagPattern,
		runtime:   operator.AIDPattern,
		source:    "operator.AIDPattern",
	},
	{
		name:      "PluginSigilView allowed_by_aid",
		structPtr: &PluginSigilView{},
		fieldPath: []string{"AllowedByAID"},
		tag:       tagPattern,
		runtime:   operator.AIDPattern,
		source:    "operator.AIDPattern",
	},
	{
		name:      "SoulCreateReply created_by_aid",
		structPtr: &SoulCreateReply{},
		fieldPath: []string{"CreatedByAID"},
		tag:       tagPattern,
		runtime:   operator.AIDPattern,
		source:    "operator.AIDPattern",
	},
	{
		name:      "SoulListEntry created_by_aid",
		structPtr: &SoulListEntry{},
		fieldPath: []string{"CreatedByAID"},
		tag:       tagPattern,
		runtime:   operator.AIDPattern,
		source:    "operator.AIDPattern",
	},
	{
		name:      "Choir created_by_aid",
		structPtr: &Choir{},
		fieldPath: []string{"CreatedByAID"},
		tag:       tagPattern,
		runtime:   operator.AIDPattern,
		source:    "operator.AIDPattern",
	},
	{
		name:      "Voice added_by_aid",
		structPtr: &Voice{},
		fieldPath: []string{"AddedByAID"},
		tag:       tagPattern,
		runtime:   operator.AIDPattern,
		source:    "operator.AIDPattern",
	},
	{
		name:      "AuditEvent archon_aid",
		structPtr: &AuditEvent{},
		fieldPath: []string{"ArchonAID"},
		tag:       tagPattern,
		runtime:   operator.AIDPattern,
		source:    "operator.AIDPattern",
	},
	{
		name:      "SynodView operators[] AID (per-element)",
		structPtr: &SynodView{},
		fieldPath: []string{"Operators"},
		tag:       tagPattern,
		runtime:   operator.AIDPattern,
		source:    "operator.AIDPattern (member AID)",
	},

	// ====================================================================
	// INPUT PATTERNS WITH RUNTIME-422 (ROLLOUT BATCH 4). Every field's runtime 422s on
	// a bad FORMAT BEFORE any other checks (existence/FK/whitelist → 400/404/500 are NOT
	// covered by the tag). Body fields go through the Body wrapper; path params directly.
	// ====================================================================

	// --- Sigil triple namespace/name/ref (reSigilSegment, 422 in validateSigilTriple) ---
	// ref is a tag-ref per this SAME validator (NOT an arbitrary git ref): a slash → 422.
	{
		name:      "sigil.allow namespace",
		structPtr: &sigilAllowInput{},
		fieldPath: []string{"Body", "Namespace"},
		tag:       tagPattern,
		runtime:   sigilSegmentRuntimePattern,
		source:    "reSigilSegment (validateSigilTriple, AllowTyped)",
	},
	{
		name:      "sigil.allow name",
		structPtr: &sigilAllowInput{},
		fieldPath: []string{"Body", "Name"},
		tag:       tagPattern,
		runtime:   sigilSegmentRuntimePattern,
		source:    "reSigilSegment (validateSigilTriple, AllowTyped)",
	},
	{
		name:      "sigil.allow ref",
		structPtr: &sigilAllowInput{},
		fieldPath: []string{"Body", "Ref"},
		tag:       tagPattern,
		runtime:   sigilSegmentRuntimePattern,
		source:    "reSigilSegment (validateSigilTriple, AllowTyped — tag-ref, слеш→422)",
	},
	{
		name:      "sigil.revoke namespace (path)",
		structPtr: &sigilRevokeInput{},
		fieldPath: []string{"Namespace"},
		tag:       tagPattern,
		runtime:   sigilSegmentRuntimePattern,
		source:    "reSigilSegment (validateSigilTriple, RevokeTyped)",
	},
	{
		name:      "sigil.revoke name (path)",
		structPtr: &sigilRevokeInput{},
		fieldPath: []string{"Name"},
		tag:       tagPattern,
		runtime:   sigilSegmentRuntimePattern,
		source:    "reSigilSegment (validateSigilTriple, RevokeTyped)",
	},
	{
		name:      "sigil.revoke ref (path)",
		structPtr: &sigilRevokeInput{},
		fieldPath: []string{"Ref"},
		tag:       tagPattern,
		runtime:   sigilSegmentRuntimePattern,
		source:    "reSigilSegment (validateSigilTriple, RevokeTyped — tag-ref, слеш→422)",
	},

	// --- incarnation name/service (incarnation.NamePattern, 422 in CreateTyped) ---
	// service is VALIDATED by the same incarnation.NamePattern (handler reuse, not
	// serviceregistry.NamePattern) and 422s BEFORE service-resolve (the FK → 422 "not
	// registered" comes LATER, format-422 comes first). NOT the coven pattern.
	{
		name:      "incarnation.create name",
		structPtr: &incCreateInput{},
		fieldPath: []string{"Body", "Name"},
		tag:       tagPattern,
		runtime:   incarnation.NamePattern,
		source:    "incarnation.NamePattern (CreateTyped, incarnation_typed.go:85)",
	},
	{
		name:      "incarnation.create service",
		structPtr: &incCreateInput{},
		fieldPath: []string{"Body", "Service"},
		tag:       tagPattern,
		runtime:   incarnation.NamePattern,
		source:    "incarnation.NamePattern (CreateTyped service-format, incarnation_typed.go:91)",
	},

	// --- push-provider name (pushprovider.NamePattern, 422 BEFORE existence) ---
	// create body + get/update/delete path — all 422 the format BEFORE ErrAlreadyExists/404.
	{
		name:      "push-provider.create name",
		structPtr: &pushProviderCreateInput{},
		fieldPath: []string{"Body", "Name"},
		tag:       tagPattern,
		runtime:   pushprovider.NamePattern,
		source:    "pushprovider.NamePattern (CreateTyped, pushprovider.go:141)",
	},
	{
		name:      "push-provider.get name (path)",
		structPtr: &pushProviderGetInput{},
		fieldPath: []string{"Name"},
		tag:       tagPattern,
		runtime:   pushprovider.NamePattern,
		source:    "pushprovider.NamePattern (GetTyped, pushprovider.go:271)",
	},
	{
		name:      "push-provider.update name (path)",
		structPtr: &pushProviderUpdateInput{},
		fieldPath: []string{"Name"},
		tag:       tagPattern,
		runtime:   pushprovider.NamePattern,
		source:    "pushprovider.NamePattern (UpdateTyped, pushprovider.go:178)",
	},
	{
		name:      "push-provider.delete name (path)",
		structPtr: &pushProviderDeleteInput{},
		fieldPath: []string{"Name"},
		tag:       tagPattern,
		runtime:   pushprovider.NamePattern,
		source:    "pushprovider.NamePattern (DeleteTyped, pushprovider.go:219)",
	},

	// --- choir_name (choir.choirNamePattern, 422 BEFORE INSERT) ---
	{
		name:      "choir.create choir_name",
		structPtr: &choirCreateInput{},
		fieldPath: []string{"Body", "ChoirName"},
		tag:       tagPattern,
		runtime:   choirNameRuntimePattern,
		source:    "choir.ValidChoirName (CreateTyped, choir.go:154)",
	},

	// --- Voice sid (soul.SIDPattern, 422 BEFORE membership check) ---
	// add-voice body sid + remove-voice path sid — format 422 comes before "SID not a member".
	{
		name:      "voice.add sid",
		structPtr: &voiceAddInput{},
		fieldPath: []string{"Body", "SID"},
		tag:       tagPattern,
		runtime:   soul.SIDPattern,
		source:    "soul.SIDPattern (AddVoiceTyped, choir.go:287)",
	},
	{
		name:      "voice.remove sid (path)",
		structPtr: &voiceRemoveInput{},
		fieldPath: []string{"SID"},
		tag:       tagPattern,
		runtime:   soul.SIDPattern,
		source:    "soul.SIDPattern (RemoveVoiceTyped, choir.go:399)",
	},

	// --- decree incarnation_name/action_scenario (oracle.*Pattern, 422 BEFORE INSERT) ---
	{
		name:      "decree.create incarnation_name",
		structPtr: &decreeCreateInput{},
		fieldPath: []string{"Body", "IncarnationName"},
		tag:       tagPattern,
		runtime:   oracle.IncarnationPattern,
		source:    "oracle.IncarnationPattern (CreateDecree, service.go:179)",
	},
	{
		name:      "decree.create action_scenario",
		structPtr: &decreeCreateInput{},
		fieldPath: []string{"Body", "ActionScenario"},
		tag:       tagPattern,
		runtime:   oracle.ScenarioPattern,
		source:    "oracle.ScenarioPattern (CreateDecree, service.go:182)",
	},

	// --- soul_path absoluteness (start-with-slash, 422 in UpdateSshTargetTyped) ---
	// SoulSshTarget is class-A shared input↔output; the pattern documents BOTH (output
	// soul_path from the DB is always `/`-absolute, via the same validator on write). `^/`
	// (NOT `^/.+`): the runtime ACCEPTS a bare `/`, `^/.+` would falsely 422 it.
	{
		name:      "soul.ssh-target soul_path",
		structPtr: &soulSshTargetInput{},
		fieldPath: []string{"Body", "SoulPath"},
		tag:       tagPattern,
		runtime:   soulPathRuntimePattern,
		source:    "UpdateSshTargetTyped (soul.go:1532, SoulPath[0]!='/' → 422)",
	},

	// ====================================================================
	// NAME PATTERNS OF OUTPUT FIELDS (ROLLOUT BATCH 5). A documentation pattern: huma does
	// NOT validate the response body (like the ID formats of batch 3) → the tag is purely
	// format documentation for client codegen. The case's goal is "documented name format ==
	// the canonical runtime source (the same const as the like-named INPUT field)". structPtr
	// is the reply/view struct directly (fieldPath = field name, WITHOUT a Body wrapper — the
	// output Body IS the struct itself). For []string fields (covens/roles/labels style) the
	// pattern sits on items[]. All these types are output-only (request Body is a separate
	// *Request/*Input) → no input-422 risk.
	// ====================================================================

	// --- kebab name name/omen/on_beacon (oracle/augur/herald.NamePattern ^[a-z0-9-]{1,63}$) ---
	{
		name:      "OmenView name (augur.NamePattern)",
		structPtr: &OmenView{},
		fieldPath: []string{"Name"},
		tag:       tagPattern,
		runtime:   augur.NamePattern,
		source:    "augur.NamePattern (output omen name)",
	},
	{
		name:      "RiteView omen (augur.NamePattern, FK)",
		structPtr: &RiteView{},
		fieldPath: []string{"Omen"},
		tag:       tagPattern,
		runtime:   augur.NamePattern,
		source:    "augur.NamePattern (output rite.omen — FK on omens.name)",
	},
	{
		name:      "VigilView name (oracle.NamePattern)",
		structPtr: &VigilView{},
		fieldPath: []string{"Name"},
		tag:       tagPattern,
		runtime:   oracle.NamePattern,
		source:    "oracle.NamePattern (output vigil name)",
	},
	{
		name:      "DecreeView name (oracle.NamePattern)",
		structPtr: &DecreeView{},
		fieldPath: []string{"Name"},
		tag:       tagPattern,
		runtime:   oracle.NamePattern,
		source:    "oracle.NamePattern (output decree name)",
	},
	{
		name:      "DecreeView on_beacon (oracle.NamePattern, FK on Vigil)",
		structPtr: &DecreeView{},
		fieldPath: []string{"OnBeacon"},
		tag:       tagPattern,
		runtime:   oracle.NamePattern,
		source:    "oracle.NamePattern (output decree.on_beacon — Vigil name)",
	},
	{
		name:      "Herald name (herald.NamePattern)",
		structPtr: &Herald{},
		fieldPath: []string{"Name"},
		tag:       tagPattern,
		runtime:   herald.NamePattern,
		source:    "herald.NamePattern (output herald name)",
	},
	{
		name:      "Tiding name (herald.NamePattern)",
		structPtr: &Tiding{},
		fieldPath: []string{"Name"},
		tag:       tagPattern,
		runtime:   herald.NamePattern,
		source:    "herald.NamePattern (output tiding name)",
	},
	{
		name:      "Tiding herald (herald.NamePattern, FK)",
		structPtr: &Tiding{},
		fieldPath: []string{"Herald"},
		tag:       tagPattern,
		runtime:   herald.NamePattern,
		source:    "herald.NamePattern (output tiding.herald — FK on heralds.name)",
	},

	// --- role-name (rbac.RoleNamePattern ^[a-z][a-z0-9-]*$) ---
	// RoleView.name + SynodView.name (synod name via the same reRoleName) + SynodView.roles[]
	// (per-element role names). RoleView.operators[]/SynodView.operators[] are AID, NOT name.
	{
		name:      "RoleView name (rbac.RoleNamePattern)",
		structPtr: &RoleView{},
		fieldPath: []string{"Name"},
		tag:       tagPattern,
		runtime:   rbac.RoleNamePattern,
		source:    "rbac.RoleNamePattern (output role name)",
	},
	{
		name:      "SynodView name (rbac.RoleNamePattern, reRoleName)",
		structPtr: &SynodView{},
		fieldPath: []string{"Name"},
		tag:       tagPattern,
		runtime:   rbac.RoleNamePattern,
		source:    "rbac.RoleNamePattern (output synod name — единый reRoleName)",
	},
	{
		name:      "SynodView roles[] (rbac.RoleNamePattern, per-element)",
		structPtr: &SynodView{},
		fieldPath: []string{"Roles"},
		tag:       tagPattern,
		runtime:   rbac.RoleNamePattern,
		source:    "rbac.RoleNamePattern (output synod.roles[] — names ролей)",
	},

	// --- service name (serviceregistry.NamePattern ^[a-z][a-z0-9-]*$) ---
	{
		name:      "ServiceView name (serviceregistry.NamePattern)",
		structPtr: &ServiceView{},
		fieldPath: []string{"Name"},
		tag:       tagPattern,
		runtime:   serviceregistry.NamePattern,
		source:    "serviceregistry.NamePattern (output service name)",
	},

	// --- coven label (soul.CovenPattern ^[a-z][a-z0-9]*(-[a-z0-9]+)*$, per-element) ---
	// output covens[]/labels[] in Soul*/Incarnation* View/Reply. *[]string and []string —
	// the reflect tag reads the same way (the helper descends via Field, the slice wrapper is transparent).
	{
		name:      "SoulCreateReply covens[] (soul.CovenPattern)",
		structPtr: &SoulCreateReply{},
		fieldPath: []string{"Covens"},
		tag:       tagPattern,
		runtime:   soul.CovenPattern,
		source:    "soul.CovenPattern (output covens[], per-element)",
	},
	{
		name:      "SoulListEntry covens[] (soul.CovenPattern)",
		structPtr: &SoulListEntry{},
		fieldPath: []string{"Covens"},
		tag:       tagPattern,
		runtime:   soul.CovenPattern,
		source:    "soul.CovenPattern (output covens[], per-element)",
	},
	{
		name:      "IncarnationGetReply covens[] (soul.CovenPattern)",
		structPtr: &IncarnationGetReply{},
		fieldPath: []string{"Covens"},
		tag:       tagPattern,
		runtime:   soul.CovenPattern,
		source:    "soul.CovenPattern (output covens[], per-element)",
	},
	{
		name:      "soulCovenAssignReply labels[] (soul.CovenPattern, replace-эхо)",
		structPtr: &soulCovenAssignReply{},
		fieldPath: []string{"Labels"},
		tag:       tagPattern,
		runtime:   soul.CovenPattern,
		source:    "soul.CovenPattern (output replace labels[], per-element)",
	},

	// --- incarnation_name (incarnation.NamePattern ^[a-z0-9][a-z0-9-]{0,62}$) ---
	// IncarnationGetReply.name + the Incarnation echo in create/run/rerun-last + choir/voice
	// incarnation_name. DecreeView.incarnation_name uses a separate const, oracle.IncarnationPattern
	// (the value is identical, but decree is its own domain — checked against ITS OWN source).
	{
		name:      "IncarnationGetReply name (incarnation.NamePattern)",
		structPtr: &IncarnationGetReply{},
		fieldPath: []string{"Name"},
		tag:       tagPattern,
		runtime:   incarnation.NamePattern,
		source:    "incarnation.NamePattern (output incarnation name)",
	},
	{
		name:      "IncarnationCreateReply incarnation (incarnation.NamePattern, echo)",
		structPtr: &IncarnationCreateReply{},
		fieldPath: []string{"Incarnation"},
		tag:       tagPattern,
		runtime:   incarnation.NamePattern,
		source:    "incarnation.NamePattern (output incarnation echo)",
	},
	{
		name:      "IncarnationRunReply incarnation (incarnation.NamePattern, echo)",
		structPtr: &IncarnationRunReply{},
		fieldPath: []string{"Incarnation"},
		tag:       tagPattern,
		runtime:   incarnation.NamePattern,
		source:    "incarnation.NamePattern (output incarnation echo)",
	},
	{
		name:      "IncarnationRerunLastReply incarnation (incarnation.NamePattern, echo)",
		structPtr: &IncarnationRerunLastReply{},
		fieldPath: []string{"Incarnation"},
		tag:       tagPattern,
		runtime:   incarnation.NamePattern,
		source:    "incarnation.NamePattern (output incarnation echo)",
	},
	{
		name:      "Choir incarnation_name (incarnation.NamePattern)",
		structPtr: &Choir{},
		fieldPath: []string{"IncarnationName"},
		tag:       tagPattern,
		runtime:   incarnation.NamePattern,
		source:    "incarnation.NamePattern (output choir.incarnation_name — FK)",
	},
	{
		name:      "Voice incarnation_name (incarnation.NamePattern)",
		structPtr: &Voice{},
		fieldPath: []string{"IncarnationName"},
		tag:       tagPattern,
		runtime:   incarnation.NamePattern,
		source:    "incarnation.NamePattern (output voice.incarnation_name — FK)",
	},
	{
		name:      "DecreeView incarnation_name (oracle.IncarnationPattern)",
		structPtr: &DecreeView{},
		fieldPath: []string{"IncarnationName"},
		tag:       tagPattern,
		runtime:   oracle.IncarnationPattern,
		source:    "oracle.IncarnationPattern (output decree.incarnation_name — тот же const, which INPUT)",
	},

	// ====================================================================
	// LENGTH BOUNDS OF INPUT (ROLLOUT BATCH 6 + cleanup). minLength/maxLength where the
	// runtime REALLY 422s on that same bound. reason has both bounds: non-emptiness
	// (minLength:1) + the upper bound incarnation.ReasonMaxLen (maxLength:500, the runtime
	// validator is UnlockTyped/RerunLastTyped, PM decision option (a)).
	// ====================================================================

	// --- reason non-emptiness + upper bound ReasonMaxLen (UnlockTyped/RerunLastTyped → 422) ---
	{
		name:      "incarnation.unlock reason minLength",
		structPtr: &incUnlockInput{},
		fieldPath: []string{"Body", "Reason"},
		tag:       tagMinLength,
		runtime:   reasonRuntimeMinLen,
		source:    "UnlockTyped (incarnation_typed.go, reason == \"\" → 422)",
	},
	{
		name:      "incarnation.unlock reason maxLength",
		structPtr: &incUnlockInput{},
		fieldPath: []string{"Body", "Reason"},
		tag:       tagMaxLength,
		runtime:   strconv.Itoa(incarnation.ReasonMaxLen),
		source:    "incarnation.ReasonMaxLen (UnlockTyped, len(reason) > 500 → 422)",
	},
	{
		name:      "incarnation.rerun-last reason minLength",
		structPtr: &incRerunInput{},
		fieldPath: []string{"Body", "Reason"},
		tag:       tagMinLength,
		runtime:   reasonRuntimeMinLen,
		source:    "RerunLastTyped (incarnation_typed.go, reason == \"\" → 422)",
	},
	{
		name:      "incarnation.rerun-last reason maxLength",
		structPtr: &incRerunInput{},
		fieldPath: []string{"Body", "Reason"},
		tag:       tagMaxLength,
		runtime:   strconv.Itoa(incarnation.ReasonMaxLen),
		source:    "incarnation.ReasonMaxLen (RerunLastTyped, len(reason) > 500 → 422)",
	},

	// --- ssh_user non-emptiness (UpdateSshTargetTyped soul.go:1529, "" → 422) ---
	// SoulSshTarget is class-A shared input↔output: INPUT 422s on empty, OUTPUT is
	// doc-only (huma doesn't validate output). The case checks the SINGLE shared schema.
	{
		name:      "soul.ssh-target ssh_user minLength",
		structPtr: &soulSshTargetInput{},
		fieldPath: []string{"Body", "SSHUser"},
		tag:       tagMinLength,
		runtime:   sshUserRuntimeMinLen,
		source:    "UpdateSshTargetTyped (handlers/soul.go:1529, ssh_user == \"\" → 422)",
	},

	// --- coven/role length 63 (ValidCoven / validHostRole len>63 → 422) ---
	// maxLength on []string fields sits on items (covens/labels). There is NO tag for
	// the lower bound: an empty role/coven/incarnation-selector is valid (opt/no-op),
	// minLength:1 would falsely 422 a valid empty value.
	{
		// maxLength sits on the nested IncarnationSpecHost.Role (the elem of
		// PATCH .../hosts body.hosts[]) — we reference the element struct
		// directly (constraintTag doesn't descend into a slice element).
		name:      "IncarnationSpecHost role maxLength",
		structPtr: &IncarnationSpecHost{},
		fieldPath: []string{"Role"},
		tag:       tagMaxLength,
		runtime:   covenRuntimeMaxLen,
		source:    "validHostRole (handlers/incarnation.go:177, len(role) > 63 → 422)",
	},
	{
		name:      "soul.create covens[] maxLength",
		structPtr: &soulCreateInput{},
		fieldPath: []string{"Body", "Covens"},
		tag:       tagMaxLength,
		runtime:   covenRuntimeMaxLen,
		source:    "soul.ValidCoven (soul.go:81, len(label) > 63 → 422; CreateTyped soul.go:221)",
	},
	{
		// Symmetry with soul.create covens[]: runtime incarnation CreateTyped
		// already 422s len>63 per-element (ValidCoven, incarnation_typed.go:95) —
		// tag simply restores boundary in spec.
		name:      "incarnation.create covens[] maxLength",
		structPtr: &incCreateInput{},
		fieldPath: []string{"Body", "Covens"},
		tag:       tagMaxLength,
		runtime:   covenRuntimeMaxLen,
		source:    "soul.ValidCoven (incarnation CreateTyped, incarnation_typed.go:95, len(label) > 63 → 422)",
	},
	{
		name:      "soul.coven-assign label maxLength",
		structPtr: &soulCovenAssignInput{},
		fieldPath: []string{"Body", "Label"},
		tag:       tagMaxLength,
		runtime:   covenRuntimeMaxLen,
		source:    "soul.ValidCoven (append/remove, soul.go:1239; пустой замен при replace → valid)",
	},
	{
		name:      "soul.coven-assign labels[] maxLength",
		structPtr: &soulCovenAssignInput{},
		fieldPath: []string{"Body", "Labels"},
		tag:       tagMaxLength,
		runtime:   covenRuntimeMaxLen,
		source:    "soul.ValidCoven (replace per-element, soul.go:1253)",
	},
	{
		name:      "soul.coven-assign selector.coven maxLength",
		structPtr: &soulCovenAssignInput{},
		fieldPath: []string{"Body", "Selector", "Coven"},
		tag:       tagMaxLength,
		runtime:   covenRuntimeMaxLen,
		source:    "soul.ValidCoven (selector.coven != \"\", soul.go:1281)",
	},
	{
		name:      "soul.coven-assign selector.incarnation maxLength",
		structPtr: &soulCovenAssignInput{},
		fieldPath: []string{"Body", "Selector", "Incarnation"},
		tag:       tagMaxLength,
		runtime:   covenRuntimeMaxLen,
		source:    "incarnation.ValidName pattern {0,62}=63 (selector.incarnation != \"\", soul.go:1284)",
	},

	// --- synod description (rbac.SynodDescriptionMaxLen=1024) ---
	// CreateTyped: description is optional (*string) → maxLength only. UpdateTyped:
	// description is required, == "" → 422 → minLength:1 + maxLength:1024.
	{
		name:      "synod.create description maxLength",
		structPtr: &synodCreateInput{},
		fieldPath: []string{"Body", "Description"},
		tag:       tagMaxLength,
		runtime:   strconv.Itoa(rbac.SynodDescriptionMaxLen),
		source:    "rbac.SynodDescriptionMaxLen (CreateTyped synod.go:133, len > 1024 → 422)",
	},
	{
		name:      "synod.update description minLength",
		structPtr: &synodUpdateInput{},
		fieldPath: []string{"Body", "Description"},
		tag:       tagMinLength,
		runtime:   synodDescRuntimeMin,
		source:    "UpdateTyped (synod.go, description == \"\" → 422)",
	},
	{
		name:      "synod.update description maxLength",
		structPtr: &synodUpdateInput{},
		fieldPath: []string{"Body", "Description"},
		tag:       tagMaxLength,
		runtime:   strconv.Itoa(rbac.SynodDescriptionMaxLen),
		source:    "rbac.SynodDescriptionMaxLen (UpdateTyped synod.go, len > 1024 → 422)",
	},
}

// TestOpenAPIConstraintSyncWithRuntime — for every case, the op-input field's tag
// must match the authoritative runtime source VERBATIM. A mismatch means the spec
// is lying about the contract → a red test.
func TestOpenAPIConstraintSyncWithRuntime(t *testing.T) {
	for _, c := range constraintSyncCases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := constraintTag(t, c.structPtr, c.fieldPath, c.tag)
			if !ok {
				t.Fatalf("field %v типа %s NOT несёт тега %q — пилотbutе ограничение исчезло from спеки (drift спека<runtime)",
					c.fieldPath, reflect.TypeOf(c.structPtr).Elem().Name(), c.tag)
			}
			if got != c.runtime {
				t.Fatalf("DRIFT тег<>runtime for %s:\n  huma-тег %q = %q\n  runtime-источник %s = %q\n→ синхронfromируй литерал тега с runtime-валидатором (ручonя синхронfromация, see шапку файла)",
					c.name, c.tag, got, c.source, c.runtime)
			}
		})
	}
}

// constraintTag descends fieldPath within the structPtr struct and returns
// the value of the requested constraint tag on the final field. The path mirrors
// how huma recursively walks the nested/Body structs of an op-input.
func constraintTag(t *testing.T, structPtr any, fieldPath []string, kind constraintTagKind) (string, bool) {
	t.Helper()
	typ := reflect.TypeOf(structPtr)
	if typ.Kind() == reflect.Ptr {
		typ = typ.Elem()
	}
	var last reflect.StructField
	for i, name := range fieldPath {
		if typ.Kind() != reflect.Struct {
			t.Fatalf("сегмент %q пути %v: предыдущий тип %s не структура", name, fieldPath, typ.Name())
		}
		f, ok := typ.FieldByName(name)
		if !ok {
			t.Fatalf("field %q не onйдеbut в %s (путь %v) — структура op-input переимеbutваon? обbutви кейс",
				name, typ.Name(), fieldPath)
		}
		last = f
		if i < len(fieldPath)-1 {
			ft := f.Type
			if ft.Kind() == reflect.Ptr {
				ft = ft.Elem()
			}
			typ = ft
		}
	}
	return last.Tag.Lookup(string(kind))
}
