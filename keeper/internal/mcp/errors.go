package mcp

import (
	"errors"
	"strings"

	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/keeper/internal/augur"
	"github.com/souls-guild/soul-stack/keeper/internal/bootstraptoken"
	"github.com/souls-guild/soul-stack/keeper/internal/herald"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/operator"
	"github.com/souls-guild/soul-stack/keeper/internal/oracle"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac"
	"github.com/souls-guild/soul-stack/keeper/internal/serviceregistry"
	"github.com/souls-guild/soul-stack/keeper/internal/sigil"
	"github.com/souls-guild/soul-stack/keeper/internal/soul"
)

// MCP error code suffixes from docs/keeper/mcp-tools.md § Errors.
// Stable URN suffixes for the Operator API (`https://soul-stack.com/errors/<suffix>`),
// surfaced in MCP-error `data.code`. Source of truth: operator-api.md § Error Types.
const (
	mcpCodeUnauthenticated     = "unauthenticated"
	mcpCodeForbidden           = "forbidden"
	mcpCodeNotFound            = "not-found"
	mcpCodeValidationFailed    = "validation-failed"
	mcpCodeMalformedRequest    = "malformed-request"
	mcpCodeOperatorExists      = "operator-already-exists"
	mcpCodeOperatorRevoked     = "operator-revoked"
	mcpCodeWouldLockOutCluster = "would-lock-out-cluster"
	mcpCodeInternalError       = "internal-error"
	mcpCodeNotImplemented      = "not-implemented"

	// Incarnation codes from docs/keeper/mcp-tools.md § Errors (stable URN
	// suffixes). incarnation-locked covers resource state conflicts
	// (error_locked / busy / downgrade / schema-mismatch — REST maps all to one
	// problem-type TypeIncarnationLocked). rerun-input-unavailable is separate
	// (mirrors REST TypeRerunInputUnavailable): rerun-last can't recover a failed
	// run's input (causes — see sentinel [incarnation.ErrRerunInputUnavailable]),
	// a machine-readable distinction from "status isn't error_locked".
	mcpCodeIncarnationExists     = "incarnation-already-exists"
	mcpCodeIncarnationLocked     = "incarnation-locked"
	mcpCodeRerunInputUnavailable = "rerun-input-unavailable"

	// Role codes (RBAC CRUD, Slice 2b). role-already-exists — UNIQUE violation
	// on rbac_roles.name; role-builtin — delete/update attempted on a builtin
	// role (cluster-admin). would-lock-out-cluster reuses mcpCodeWouldLockOutCluster
	// (shared problem-type for operator and role self-lockout). not-found /
	// validation-failed / forbidden are the common codes.
	mcpCodeRoleExists  = "role-already-exists"
	mcpCodeRoleBuiltin = "role-builtin"

	// Synod codes (ADR-049, parity with REST /v1/synods*). synod-already-exists —
	// UNIQUE violation on synods.name (REST TypeSynodExists); synod-not-found —
	// no such synod (REST TypeSynodNotFound); synod-builtin — synod.delete on a
	// builtin (REST TypeSynodBuiltin). would-lock-out-cluster / not-found /
	// validation-failed / forbidden are shared with role-tools.
	mcpCodeSynodExists   = "synod-already-exists"
	mcpCodeSynodNotFound = "synod-not-found"
	mcpCodeSynodBuiltin  = "synod-builtin"

	// Soul codes (onboarding, parity with REST POST /v1/souls + issue-token).
	// soul-already-exists — UNIQUE violation on souls.sid (REST TypeSoulExists);
	// bootstrap-token-active — SID already has an active bootstrap token and
	// force wasn't set (REST TypeBootstrapTokenActive). not-found /
	// validation-failed / forbidden are the common codes.
	mcpCodeSoulExists           = "soul-already-exists"
	mcpCodeBootstrapTokenActive = "bootstrap-token-active"

	// Sigil codes (plugin allow-list, S4b — parity with REST POST/DELETE
	// /v1/plugins/sigils*). plugin-not-in-cache — plugin (ns, name) not in the
	// host's single-slot cache (REST TypePluginNotInCache, 404); sigil-already-
	// active — an active allow entry for (ns, name, ref) already exists (REST
	// TypeSigilActive, 409); sigil-not-found — no active entry to revoke (REST
	// TypeSigilNotFound, 404). validation-failed / forbidden are common codes.
	mcpCodePluginNotInCache = "plugin-not-in-cache"
	mcpCodeSigilActive      = "sigil-already-active"
	mcpCodeSigilNotFound    = "sigil-not-found"

	// Sigil signing-key codes (key rotation, R3-S7 — parity with REST
	// /v1/sigil/keys*). sigil-key-not-found — no such key (REST
	// TypeSigilKeyNotFound, 404); sigil-key-last-active — last active key (REST
	// TypeSigilKeyLastActive, 409); sigil-key-primary — direct retire of primary
	// (REST TypeSigilKeyPrimary, 409); sigil-key-concurrent-change — race on
	// primary, or set-primary on a retired key (REST
	// TypeSigilKeyConcurrentChange, 409).
	mcpCodeSigilKeyNotFound         = "sigil-key-not-found"
	mcpCodeSigilKeyLastActive       = "sigil-key-last-active"
	mcpCodeSigilKeyPrimary          = "sigil-key-primary"
	mcpCodeSigilKeyConcurrentChange = "sigil-key-concurrent-change"

	// Service code (Service registry, ADR-028 S3 — parity with REST
	// POST/PATCH/DELETE /v1/services*). service-already-exists — UNIQUE
	// violation on service_registry.name (REST TypeServiceExists, 409).
	// not-found (no such record / CallerAID missing from operators) /
	// validation-failed (bad name/git/ref/refresh) are common codes.
	mcpCodeServiceExists = "service-already-exists"

	// mcpCodeOmenExists — UNIQUE violation on omens.name (REST TypeOmenExists,
	// 409). Omen/Rite not-found share mcpCodeNotFound; validation shares
	// mcpCodeValidationFailed. Augur CRUD (ADR-025, augur.md).
	mcpCodeOmenExists = "omen-already-exists"

	// mcpCodeVigilExists / mcpCodeDecreeExists — UNIQUE violation on vigils.name /
	// decrees.name (REST TypeVigilExists / TypeDecreeExists, 409). Vigil/Decree
	// not-found share mcpCodeNotFound; validation shares mcpCodeValidationFailed.
	// Oracle CRUD (ADR-030, beacons S3).
	mcpCodeVigilExists  = "vigil-already-exists"
	mcpCodeDecreeExists = "decree-already-exists"

	// mcpCodePushProviderExists — UNIQUE violation on push_providers.name (REST
	// TypePushProviderExists, 409). ADR-032 amendment 2026-05-26, S7-2.
	mcpCodePushProviderExists = "push-provider-already-exists"

	// mcpCodeProviderExists / mcpCodeProfileExists — UNIQUE violation on
	// providers.name / profiles.name (REST TypeProviderExists /
	// TypeProfileExists, 409). Provider/Profile not-found (incl. FK
	// Profile→missing Provider, which MCP profile.create reports as
	// validation-failed, parity with REST 422) share mcpCodeNotFound; validation
	// shares mcpCodeValidationFailed. Cloud CRUD (ADR-017).
	mcpCodeProviderExists = "provider-already-exists"
	mcpCodeProfileExists  = "profile-already-exists"

	// mcpCodeProviderHasProfiles — Provider deletion blocked by dependent
	// Profiles (REST TypeProviderHasProfiles, 409, FK RESTRICT). ADR-017.
	mcpCodeProviderHasProfiles = "provider-has-profiles"

	// mcpCodeHeraldExists / mcpCodeTidingExists — UNIQUE violation on
	// heralds.name / tidings.name (REST TypeHeraldExists / TypeTidingExists,
	// 409). Herald/Tiding not-found (incl. FK Tiding→missing Herald) share
	// mcpCodeNotFound; validation shares mcpCodeValidationFailed. ADR-052, S4.
	mcpCodeHeraldExists = "herald-already-exists"
	mcpCodeTidingExists = "tiding-already-exists"

	// mcpCodeErrandNotCancellable — attempted cancel of an Errand already in a
	// terminal status (REST TypeErrandNotCancellable, 409). ADR-033 slice E5.
	mcpCodeErrandNotCancellable = "errand-not-cancellable"

	// mcpCodeMigrationFailed — reserved code from mcp-tools.md § Errors.
	// DELIBERATELY unused in mapIncarnationErrorToMCP: upgrading an incarnation
	// in migration_failed status returns [incarnation.ErrIncarnationLocked] (the
	// same sentinel as error_locked, see the upgradeTx switch), which both REST
	// and MCP map to incarnation-locked. A failure in migration-Apply itself
	// returns a wrapped internal error → internal-error (parity with REST's
	// default Upgrade branch). Dedicated migration_failed classification needs
	// its own sentinel (a public error-mapping contract change — post-MVP,
	// symmetric with REST). The constant stays as the canonical URN suffix for
	// that future wiring; its presence in docs/keeper/mcp-tools.md § Errors is
	// enforced by a test invariant (see reservedMCPCodes below).
	mcpCodeMigrationFailed = "migration-failed"
)

// reservedMCPCodes — canonical URN suffixes declared in mcp-tools.md
// § Errors but not yet wired to a sentinel in mapIncarnationErrorToMCP /
// mapServiceErrorToMCP (wired when needed, symmetric with REST).
//
// Used by the [TestReservedMCPCodes_PresentInDocs] test invariant: every
// reserved code must appear in the mcp-tools.md § Errors table. Catches
// code↔doc drift (code declared but missing from docs, or vice versa) and
// keeps the constant actually consumed rather than dead code.
var reservedMCPCodes = []string{
	mcpCodeMigrationFailed,
}

// mcpToolError — payload the transport puts into JSON-RPC `error.data` on a
// tool-execution failure. Matches the MCP-tool error shape from
// mcp-tools.md: `code` is the stable URN suffix, `instance` is the tool
// path (for audit).
//
// The JSON-RPC-level `error.code` is kept as rpcCodeInternalError for all
// tool-execution errors (the MCP spec defines no JSON-RPC codes for
// application errors); the meaningful code lives in data.code.
type mcpToolError struct {
	Code     string `json:"code"`
	Instance string `json:"instance,omitempty"`
}

// mapServiceErrorToMCP converts an [operator.Service] error into a
// (MCP-code, public-message) pair. The public message is safe to return to
// the client (no internal stack / SQL detail).
//
// Unknown errors return `internal-error` + a generic detail — raw
// err.Error() is never forwarded (oracle-attack surface via distinguishing
// internal messages); logging that detail is the caller's responsibility.
func mapServiceErrorToMCP(err error) (code, detail string) {
	switch {
	case err == nil:
		return "", ""
	case errors.Is(err, operator.ErrOperatorAlreadyExists):
		return mcpCodeOperatorExists, "operator with this AID already exists"
	case errors.Is(err, operator.ErrOperatorNotFound):
		return mcpCodeNotFound, "operator not found"
	case errors.Is(err, operator.ErrOperatorAlreadyRevoked):
		return mcpCodeOperatorRevoked, "operator is already revoked"
	case errors.Is(err, operator.ErrWouldLockOutCluster):
		return mcpCodeWouldLockOutCluster, "target is the last active cluster-admin; revoking would lock out the cluster"
	case errors.Is(err, rbac.ErrPermissionDenied):
		return mcpCodeForbidden, "operator lacks required permission"
	}
	// Validation errors from service.Create/Revoke/IssueToken are fmt.Errorf
	// with prefix "operator: invalid AID …" / "operator: ... is empty".
	// Recognized by message rather than a dedicated sentinel — it's already a
	// public message formed in one place (service.go). The `operator: ` prefix
	// is an internal package name; trim it before returning to the client.
	if msg := err.Error(); strings.HasPrefix(msg, "operator: invalid AID ") ||
		strings.HasPrefix(msg, "operator: CallerAID is empty") {
		return mcpCodeValidationFailed, strings.TrimPrefix(msg, "operator: ")
	}
	return mcpCodeInternalError, "internal error"
}

// mapIncarnationErrorToMCP converts sentinel errors from the incarnation
// layer (CRUD tx + [incarnation.PrepareUpgrade] prepare phase) into a
// (MCP-code, public-message) pair. Mirrors the REST IncarnationHandler:
// same sentinels, same codes.
//
// REST problem-type ↔ MCP-code (docs/keeper/mcp-tools.md § Errors):
//   - TypeNotFound              → not-found.
//   - TypeIncarnationExists     → incarnation-already-exists.
//   - TypeIncarnationLocked     → incarnation-locked (resource state conflicts:
//     not-unlockable / not-error-locked / busy / locked / downgrade / schema-mismatch).
//   - TypeRerunInputUnavailable → rerun-input-unavailable (rerun-last: failed
//     run's input unavailable — causes in [incarnation.ErrRerunInputUnavailable]).
//   - TypeValidationFailed      → validation-failed (no-op upgrade / broken chain).
//
// Internal resolve failures (service-not-registered / load-failed /
// no-manifest / chain-load-failed / evaluator-failed) → internal-error: an
// "unplanned error" for the client, diagnosed via logs/OTel caller-side.
//
// Unknown errors → internal-error + generic detail (raw err.Error() isn't
// forwarded — oracle-attack protection, as in mapServiceErrorToMCP).
func mapIncarnationErrorToMCP(err error) (code, detail string) {
	switch {
	case err == nil:
		return "", ""
	case errors.Is(err, incarnation.ErrIncarnationNotFound):
		return mcpCodeNotFound, "incarnation not found"
	case errors.Is(err, incarnation.ErrIncarnationAlreadyExists):
		return mcpCodeIncarnationExists, "incarnation with this name already exists"
	case errors.Is(err, incarnation.ErrIncarnationNotLocked):
		return mcpCodeIncarnationLocked, "incarnation is not in an unlockable status — nothing to unlock"
	case errors.Is(err, incarnation.ErrIncarnationNotErrorLocked):
		return mcpCodeIncarnationLocked, "incarnation is not error_locked — rerun-last requires error_locked"
	case errors.Is(err, incarnation.ErrRerunInputUnavailable):
		return mcpCodeRerunInputUnavailable, "rerun-last: failed run's input is unavailable (run failed before dispatch, no recipe recorded / recipe purged by retention / legacy run) — use unlock + manual run with explicit input"
	case errors.Is(err, incarnation.ErrIncarnationBusy):
		return mcpCodeIncarnationLocked, "incarnation is applying — operation rejected until run completes"
	case errors.Is(err, incarnation.ErrIncarnationLocked):
		return mcpCodeIncarnationLocked, "incarnation is locked — unlock required before this operation"
	case errors.Is(err, incarnation.ErrDowngradeUnsupported),
		errors.Is(err, incarnation.ErrDowngradeViaRef):
		return mcpCodeIncarnationLocked, "to_version downgrades state_schema_version — forward-only (ADR-019)"
	case errors.Is(err, incarnation.ErrSchemaVersionMismatch):
		return mcpCodeIncarnationLocked, "incarnation schema changed concurrently — retry upgrade"
	case errors.Is(err, incarnation.ErrUpgradeNoop):
		return mcpCodeValidationFailed, "to_version matches current incarnation version — nothing to upgrade"
	case errors.Is(err, incarnation.ErrIncarnationNotDestroyable):
		// Status doesn't allow destroy (applying / destroying) — resource state
		// conflict, same problem-type as error_locked (REST TypeIncarnationLocked).
		return mcpCodeIncarnationLocked, "incarnation status does not allow destroy (applying / destroying)"
	case errors.Is(err, incarnation.ErrDestroyScenarioMissing):
		// allow_destroy=false and the snapshot has no `destroy` scenario —
		// nothing to run for teardown (REST TypeValidationFailed, 422).
		return mcpCodeValidationFailed, "service snapshot has no `destroy` scenario — pass allow_destroy=true to force destroy without teardown"
	case errors.Is(err, artifact.ErrMigrationChainBroken):
		return mcpCodeValidationFailed, "migration chain to target version is broken"
	case errors.Is(err, incarnation.ErrServiceNotRegistered):
		return mcpCodeInternalError, "service is not registered"
	case errors.Is(err, incarnation.ErrLoadTargetSnapshot),
		errors.Is(err, incarnation.ErrTargetSnapshotInvalid),
		errors.Is(err, incarnation.ErrLoadMigrationChain),
		errors.Is(err, incarnation.ErrBuildEvaluator):
		return mcpCodeInternalError, "internal error"
	case errors.Is(err, rbac.ErrPermissionDenied):
		return mcpCodeForbidden, "operator lacks required permission"
	}
	return mcpCodeInternalError, "internal error"
}

// mapRoleErrorToMCP converts sentinel errors from the RBAC CRUD layer
// ([rbac.Service]) into a (MCP-code, public-message) pair. Mirrors
// mapServiceErrorToMCP / mapIncarnationErrorToMCP: same sentinels as the
// REST role-tools handler (Slice 2a), same codes.
//
// sentinel ↔ MCP-code:
//   - ErrRoleNotFound / ErrRoleOperatorNotFound / ErrOperatorNotFound → not-found.
//   - ErrRoleAlreadyExists                                            → role-already-exists.
//   - ErrRoleBuiltin                                                  → role-builtin.
//   - ErrWouldLockOutCluster                                         → would-lock-out-cluster
//     (shared code with operator self-lockout).
//   - ErrInvalidRoleName + wrapped ParsePermission error              → validation-failed.
//   - ErrPermissionNotHeld (least-privilege subset check)             → forbidden.
//   - ErrPermissionDenied                                             → forbidden.
//
// Unknown errors → internal-error + generic detail (raw err.Error() isn't
// forwarded — oracle-attack protection, as in the neighboring mappers).
func mapRoleErrorToMCP(err error) (code, detail string) {
	switch {
	case err == nil:
		return "", ""
	case errors.Is(err, rbac.ErrRoleNotFound):
		return mcpCodeNotFound, "role not found"
	case errors.Is(err, rbac.ErrRoleOperatorNotFound):
		return mcpCodeNotFound, "role-operator membership not found"
	case errors.Is(err, rbac.ErrOperatorNotFound):
		return mcpCodeNotFound, "operator (AID) not found"
	case errors.Is(err, rbac.ErrRoleAlreadyExists):
		return mcpCodeRoleExists, "role with this name already exists"
	case errors.Is(err, rbac.ErrRoleBuiltin):
		return mcpCodeRoleBuiltin, "role is builtin — delete/update forbidden"
	case errors.Is(err, rbac.ErrWouldLockOutCluster):
		return mcpCodeWouldLockOutCluster, "operation would leave the cluster without an active operator holding '*' permission"
	case errors.Is(err, rbac.ErrInvalidRoleName):
		return mcpCodeValidationFailed, "invalid role name"
	case errors.Is(err, rbac.ErrPermissionNotHeld):
		return mcpCodeForbidden, "cannot grant a permission you do not hold yourself"
	case errors.Is(err, rbac.ErrPermissionDenied):
		return mcpCodeForbidden, "operator lacks required permission"
	}
	// A malformed permission is a wrapped ParsePermission error prefixed
	// "rbac: invalid permission …" (formed in one place — service.go/crud.go).
	// It's already a public message; trim the internal package prefix before
	// returning it. No dedicated sentinel — the text itself carries the diagnosis.
	if msg := err.Error(); strings.HasPrefix(msg, "rbac: invalid permission ") {
		return mcpCodeValidationFailed, strings.TrimPrefix(msg, "rbac: ")
	}
	return mcpCodeInternalError, "internal error"
}

// mapSynodErrorToMCP converts sentinel errors from the Synod CRUD layer
// ([rbac.Service], ADR-049) into a (MCP-code, public-message) pair. Mirrors
// mapRoleErrorToMCP: same codes as the REST synod-tools handler.
//
// sentinel ↔ MCP-code:
//   - ErrSynodNotFound / ErrSynodOperatorNotFound / ErrSynodRoleNotFound /
//     ErrOperatorNotFound / ErrRoleNotFound                        → not-found.
//   - ErrSynodAlreadyExists                                        → synod-already-exists.
//   - ErrSynodBuiltin                                              → synod-builtin.
//   - ErrWouldLockOutCluster                                       → would-lock-out-cluster.
//   - ErrInvalidSynodName                                          → validation-failed.
//   - ErrPermissionNotHeld (least-privilege subset)                → forbidden.
//   - ErrPermissionDenied                                          → forbidden.
//
// ErrSynodNotFound and ErrSynodOperatorNotFound/ErrSynodRoleNotFound share
// the not-found code but differ in detail (diagnostics). Unknown errors →
// internal-error + generic detail (oracle-attack protection).
func mapSynodErrorToMCP(err error) (code, detail string) {
	switch {
	case err == nil:
		return "", ""
	case errors.Is(err, rbac.ErrSynodNotFound):
		return mcpCodeSynodNotFound, "synod not found"
	case errors.Is(err, rbac.ErrSynodOperatorNotFound):
		return mcpCodeNotFound, "synod-operator membership not found"
	case errors.Is(err, rbac.ErrSynodRoleNotFound):
		return mcpCodeNotFound, "synod-role bundle entry not found"
	case errors.Is(err, rbac.ErrOperatorNotFound):
		return mcpCodeNotFound, "operator (AID) not found"
	case errors.Is(err, rbac.ErrRoleNotFound):
		return mcpCodeNotFound, "role not found"
	case errors.Is(err, rbac.ErrSynodAlreadyExists):
		return mcpCodeSynodExists, "synod with this name already exists"
	case errors.Is(err, rbac.ErrSynodBuiltin):
		return mcpCodeSynodBuiltin, "synod is builtin — delete forbidden"
	case errors.Is(err, rbac.ErrWouldLockOutCluster):
		return mcpCodeWouldLockOutCluster, "operation would leave the cluster without an active operator holding '*' permission"
	case errors.Is(err, rbac.ErrInvalidSynodName):
		return mcpCodeValidationFailed, "invalid synod name"
	case errors.Is(err, rbac.ErrPermissionNotHeld):
		return mcpCodeForbidden, "cannot grant permissions you do not hold yourself"
	case errors.Is(err, rbac.ErrPermissionDenied):
		return mcpCodeForbidden, "operator lacks required permission"
	}
	return mcpCodeInternalError, "internal error"
}

// mapSoulErrorToMCP converts sentinel errors from soul onboarding
// ([soul.*] + [bootstraptoken.*]) into a (MCP-code, public-message) pair.
// Mirrors the REST SoulHandler (Create / IssueToken): same sentinels, same
// codes.
//
// sentinel ↔ MCP-code (REST problem-type → MCP-code):
//   - ErrSoulAlreadyExists    → soul-already-exists (REST TypeSoulExists).
//   - ErrSoulCreatorNotFound  → validation-failed (REST TypeValidationFailed:
//     creator AID missing from the operators registry).
//   - ErrSoulNotFound         → not-found (REST TypeNotFound).
//   - ErrTokenActiveExists    → bootstrap-token-active (REST
//     TypeBootstrapTokenActive: an active token exists and force wasn't set).
//
// Unknown errors → internal-error + generic detail (raw err.Error() isn't
// forwarded — oracle-attack protection, as in the neighboring mappers).
func mapSoulErrorToMCP(err error) (code, detail string) {
	switch {
	case err == nil:
		return "", ""
	case errors.Is(err, soul.ErrSoulAlreadyExists):
		return mcpCodeSoulExists, "soul with this SID already exists"
	case errors.Is(err, soul.ErrSoulCreatorNotFound):
		return mcpCodeValidationFailed, "creator AID not found in operators registry"
	case errors.Is(err, soul.ErrSoulNotFound):
		return mcpCodeNotFound, "soul not found"
	case errors.Is(err, bootstraptoken.ErrTokenActiveExists):
		return mcpCodeBootstrapTokenActive, "soul already has an active bootstrap token; pass force=true to expire it and reissue"
	}
	return mcpCodeInternalError, "internal error"
}

// mapSigilErrorToMCP converts sentinel errors from the Sigil allow-list
// layer ([sigil.Service]) into a (MCP-code, public-message) pair. Mirrors
// the REST SigilHandler (Allow / Revoke): same sentinels, same codes.
//
// sentinel ↔ MCP-code (REST problem-type → MCP-code):
//   - ErrPluginNotInCache    → plugin-not-in-cache (REST TypePluginNotInCache).
//   - ErrSigilAlreadyActive  → sigil-already-active (REST TypeSigilActive).
//   - ErrSigilNotFound       → sigil-not-found (REST TypeSigilNotFound).
//
// Unknown errors → internal-error + generic detail (raw err.Error() isn't
// forwarded — oracle-attack protection, as in the neighboring mappers).
func mapSigilErrorToMCP(err error) (code, detail string) {
	switch {
	case err == nil:
		return "", ""
	case errors.Is(err, sigil.ErrPluginNotInCache):
		return mcpCodePluginNotInCache, "plugin not found in host cache"
	case errors.Is(err, sigil.ErrSigilAlreadyActive):
		return mcpCodeSigilActive, "an active sigil already exists for (namespace, name, ref)"
	case errors.Is(err, sigil.ErrSigilNotFound):
		return mcpCodeSigilNotFound, "no active sigil for (namespace, name, ref)"
	}
	return mcpCodeInternalError, "internal error"
}

// mapSigilKeyErrorToMCP converts sentinel errors from signing-key rotation
// ([sigil.KeyService]) into a (MCP-code, public-message) pair. Mirrors the
// REST SigilKeyHandler: same sentinels, same codes.
//
//   - ErrKeyNotFound       → sigil-key-not-found (REST TypeSigilKeyNotFound).
//   - ErrLastActiveKey     → sigil-key-last-active (REST TypeSigilKeyLastActive).
//   - ErrRetirePrimary     → sigil-key-primary (REST TypeSigilKeyPrimary).
//   - ErrConcurrentPrimary → sigil-key-concurrent-change (REST TypeSigilKeyConcurrentChange).
//   - ErrKeyRetired        → sigil-key-concurrent-change (set-primary on a retired key).
//
// Unknown → internal-error + generic detail (raw err isn't forwarded).
func mapSigilKeyErrorToMCP(err error) (code, detail string) {
	switch {
	case err == nil:
		return "", ""
	case errors.Is(err, sigil.ErrKeyNotFound):
		return mcpCodeSigilKeyNotFound, "no signing key with this key_id"
	case errors.Is(err, sigil.ErrLastActiveKey):
		return mcpCodeSigilKeyLastActive, "cannot retire the last active signing key"
	case errors.Is(err, sigil.ErrRetirePrimary):
		return mcpCodeSigilKeyPrimary, "cannot retire the primary key; set another key primary first"
	case errors.Is(err, sigil.ErrConcurrentPrimary):
		return mcpCodeSigilKeyConcurrentChange, "concurrent primary-key change; retry"
	case errors.Is(err, sigil.ErrKeyRetired):
		return mcpCodeSigilKeyConcurrentChange, "signing key is retired; cannot become primary"
	}
	return mcpCodeInternalError, "internal error"
}

// mapServiceRegistryErrorToMCP converts sentinel errors from the Service
// registry ([serviceregistry.Service]) into a (MCP-code, public-message)
// pair. Mirrors the REST ServiceHandler (Register / Update / Deregister):
// same sentinels, same codes.
//
// sentinel ↔ MCP-code (REST problem-type → MCP-code):
//   - ErrAlreadyExists      → service-already-exists (REST TypeServiceExists).
//   - ErrNotFound           → not-found (REST TypeNotFound: no such record).
//   - ErrOperatorNotFound   → not-found (REST TypeNotFound: CallerAID missing
//     from the operators registry, FK violation).
//   - ErrInvalidName / ErrInvalidGit / ErrInvalidRef / ErrInvalidRefresh →
//     validation-failed (REST TypeValidationFailed).
//
// Unknown errors → internal-error + generic detail (raw err.Error() isn't
// forwarded — oracle-attack protection, as in the neighboring mappers).
func mapServiceRegistryErrorToMCP(err error) (code, detail string) {
	switch {
	case err == nil:
		return "", ""
	case errors.Is(err, serviceregistry.ErrAlreadyExists):
		return mcpCodeServiceExists, "service with this name already exists"
	case errors.Is(err, serviceregistry.ErrNotFound):
		return mcpCodeNotFound, "service not found"
	case errors.Is(err, serviceregistry.ErrOperatorNotFound):
		return mcpCodeNotFound, "caller AID not found in operators registry"
	case errors.Is(err, serviceregistry.ErrInvalidName):
		return mcpCodeValidationFailed, "invalid service name"
	case errors.Is(err, serviceregistry.ErrInvalidGit):
		return mcpCodeValidationFailed, "git is empty"
	case errors.Is(err, serviceregistry.ErrInvalidRef):
		return mcpCodeValidationFailed, "ref is empty"
	case errors.Is(err, serviceregistry.ErrInvalidRefresh):
		return mcpCodeValidationFailed, "invalid refresh duration"
	}
	return mcpCodeInternalError, "internal error"
}

// mapAugurErrorToMCP converts sentinel errors from [augur.Service] (Omen /
// Rite CRUD) into a (MCP-code, public-message) pair. Mirrors the REST
// AugurHandler.
//
// sentinel ↔ MCP-code (REST problem-type → MCP-code):
//   - ErrValidation        → validation-failed (REST TypeValidationFailed).
//   - ErrOmenAlreadyExists → omen-already-exists (REST TypeOmenExists).
//   - ErrOmenNotFound      → not-found (REST TypeNotFound: no such Omen).
//   - ErrRiteNotFound      → not-found (REST TypeNotFound: no such Rite).
//
// ErrValidation already carries a public detail (formed in augur.Service
// without internal SQL/stack) — returned as-is. Unknown errors →
// internal-error + generic detail (oracle-attack protection, as elsewhere).
func mapAugurErrorToMCP(err error) (code, detail string) {
	switch {
	case err == nil:
		return "", ""
	case errors.Is(err, augur.ErrValidation):
		return mcpCodeValidationFailed, strings.TrimPrefix(err.Error(), "augur: ")
	case errors.Is(err, augur.ErrOmenAlreadyExists):
		return mcpCodeOmenExists, "omen with this name already exists"
	case errors.Is(err, augur.ErrOmenNotFound):
		return mcpCodeNotFound, "omen not found"
	case errors.Is(err, augur.ErrRiteNotFound):
		return mcpCodeNotFound, "rite not found"
	}
	return mcpCodeInternalError, "internal error"
}

// mapOracleErrorToMCP converts sentinel errors from [oracle.Service] (Vigil /
// Decree CRUD) into a (MCP-code, public-message) pair. Mirrors the REST
// OracleHandler.
//
// sentinel ↔ MCP-code (REST problem-type → MCP-code):
//   - ErrValidation          → validation-failed (REST TypeValidationFailed).
//   - ErrVigilAlreadyExists  → vigil-already-exists (REST TypeVigilExists).
//   - ErrDecreeAlreadyExists → decree-already-exists (REST TypeDecreeExists).
//   - ErrVigilNotFound       → not-found (REST TypeNotFound).
//   - ErrDecreeNotFound      → not-found (REST TypeNotFound).
//
// ErrValidation already carries a public detail (formed in oracle.Service
// without internal SQL/stack) — returned as-is. Unknown errors →
// internal-error + generic detail (oracle-attack protection, as elsewhere).
func mapOracleErrorToMCP(err error) (code, detail string) {
	switch {
	case err == nil:
		return "", ""
	case errors.Is(err, oracle.ErrValidation):
		return mcpCodeValidationFailed, strings.TrimPrefix(err.Error(), "oracle: ")
	case errors.Is(err, oracle.ErrVigilAlreadyExists):
		return mcpCodeVigilExists, "vigil with this name already exists"
	case errors.Is(err, oracle.ErrDecreeAlreadyExists):
		return mcpCodeDecreeExists, "decree with this name already exists"
	case errors.Is(err, oracle.ErrVigilNotFound):
		return mcpCodeNotFound, "vigil not found"
	case errors.Is(err, oracle.ErrDecreeNotFound):
		return mcpCodeNotFound, "decree not found"
	}
	return mcpCodeInternalError, "internal error"
}

// mapHeraldErrorToMCP converts sentinel errors from [herald.Service] (Herald
// CRUD) into a (MCP-code, public-message) pair. Mirrors the REST
// HeraldHandler.
//
// sentinel ↔ MCP-code (REST problem-type → MCP-code):
//   - ErrHeraldExists   → herald-already-exists (REST TypeHeraldExists).
//   - ErrHeraldNotFound → not-found (REST TypeNotFound).
//   - ErrValidation     → validation-failed (REST TypeValidationFailed).
//
// ErrValidation already carries a public detail (formed by validators
// without internal SQL/stack — herald.PublicMessage). Unknown errors →
// internal-error + generic detail (oracle-attack protection, as elsewhere).
func mapHeraldErrorToMCP(err error) (code, detail string) {
	switch {
	case err == nil:
		return "", ""
	case errors.Is(err, herald.ErrHeraldExists):
		return mcpCodeHeraldExists, "herald with this name already exists"
	case errors.Is(err, herald.ErrHeraldNotFound):
		return mcpCodeNotFound, "herald not found"
	case herald.IsValidationError(err):
		return mcpCodeValidationFailed, herald.PublicMessage(err)
	}
	return mcpCodeInternalError, "internal error"
}

// mapTidingErrorToMCP converts sentinel errors from [herald.Service] (Tiding
// CRUD) into a (MCP-code, public-message) pair. Mirrors the REST handler.
//
//   - ErrTidingExists   → tiding-already-exists (REST TypeTidingExists).
//   - ErrTidingNotFound → not-found (no such Tiding).
//   - ErrHeraldNotFound → not-found (FK Tiding→missing Herald, REST TypeNotFound).
//   - ErrValidation     → validation-failed.
//
// ErrHeraldNotFound is checked BEFORE ErrTidingNotFound (FK violation on a
// missing herald has distinct meaning). Unknown → internal-error.
func mapTidingErrorToMCP(err error) (code, detail string) {
	switch {
	case err == nil:
		return "", ""
	case errors.Is(err, herald.ErrTidingExists):
		return mcpCodeTidingExists, "tiding with this name already exists"
	case errors.Is(err, herald.ErrHeraldNotFound):
		return mcpCodeNotFound, "referenced herald not found"
	case errors.Is(err, herald.ErrTidingNotFound):
		return mcpCodeNotFound, "tiding not found"
	case herald.IsValidationError(err):
		return mcpCodeValidationFailed, herald.PublicMessage(err)
	}
	return mcpCodeInternalError, "internal error"
}
