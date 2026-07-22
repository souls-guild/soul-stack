// Package problem — RFC 7807 "Problem Details for HTTP APIs"
// (application/problem+json) for the Operator API.
//
// The normative mapping of error types is [docs/keeper/operator-api.md →
// Error types]. The list of `Type*` constants is only-add: new types can
// be added, existing ones must not be renamed (machine clients parse the
// `type` URL as a stable identifier).
//
// M0.6a covers only the base 4xx/5xx types (auth/not-found/malformed/
// internal). Domain types (`incarnation-locked`, `would-lock-out-cluster`,
// `operator-already-exists`, etc) — M0.6b/c as the corresponding
// endpoints appear.
package problem

import (
	"encoding/json"
	"net/http"
)

// ContentType — RFC 7807 §3 media type for problem responses.
const ContentType = "application/problem+json"

// Catalog of `type` URNs. Stable, versioned only-add.
// The domain `https://soul-stack.com/errors/<suffix>` is fixed in
// [operator-api.md → § Error format (RFC 7807)].
const (
	TypeUnauthenticated  = "https://soul-stack.com/errors/unauthenticated"
	TypeForbidden        = "https://soul-stack.com/errors/forbidden"
	TypeNotFound         = "https://soul-stack.com/errors/not-found"
	TypeMethodNotAllowed = "https://soul-stack.com/errors/method-not-allowed"
	TypeMalformedRequest = "https://soul-stack.com/errors/malformed-request"
	TypeValidationFailed = "https://soul-stack.com/errors/validation-failed"
	TypeInternalError    = "https://soul-stack.com/errors/internal-error"
	TypeOperatorExists   = "https://soul-stack.com/errors/operator-already-exists"
	TypeOperatorRevoked  = "https://soul-stack.com/errors/operator-revoked"
	// TypeOperatorRevokedToken — the JWT is valid by signature/exp, but the Archon's
	// AID has been revoked in the `operators` registry (ADR-014 Amendment 2026-05-27, JWT
	// immediate revoke). 401 (parity with an expired JWT), NOT 403 — the token is no longer
	// trusted. A separate URN from [TypeOperatorRevoked] (409 on the write
	// side IssueToken/Revoke for an already-revoked AID), so the UI/SDK can
	// distinguish "my own token expired → time to log in" from "cannot
	// perform the operation on someone else's revoked AID".
	TypeOperatorRevokedToken = "https://soul-stack.com/errors/operator-revoked-token"
	TypeWouldLockOutCluster  = "https://soul-stack.com/errors/would-lock-out-cluster"
	TypeIncarnationExists    = "https://soul-stack.com/errors/incarnation-already-exists"
	TypeIncarnationLocked    = "https://soul-stack.com/errors/incarnation-locked"
	// TypeRerunInputUnavailable — `rerun-last` cannot restore the input of a failed
	// run: the recipe (`apply_runs.recipe`) is unavailable (recipe IS NULL on the
	// legacy dispatchWave path, OR the row was purged by Reaper retention). 409 Conflict,
	// same as [TypeIncarnationLocked] (the same HTTP class "target state
	// unreachable"), but a separate URN: both rerun-last cases were indistinguishable
	// machine-readable (the UI had to match a substring of detail). Separates
	// "the input of a failed run is lost → unlock + manual run" from "status is not
	// error_locked → unlock" (the latter stays on [TypeIncarnationLocked]).
	TypeRerunInputUnavailable = "https://soul-stack.com/errors/rerun-input-unavailable"
	TypeSoulExists            = "https://soul-stack.com/errors/soul-already-exists"
	TypeBootstrapTokenActive  = "https://soul-stack.com/errors/bootstrap-token-active"
	TypeRoleNotFound          = "https://soul-stack.com/errors/role-not-found"
	TypeRoleExists            = "https://soul-stack.com/errors/role-already-exists"
	TypeRoleBuiltin           = "https://soul-stack.com/errors/role-builtin"
	TypeSynodNotFound         = "https://soul-stack.com/errors/synod-not-found"
	TypeSynodExists           = "https://soul-stack.com/errors/synod-already-exists"
	TypeSynodBuiltin          = "https://soul-stack.com/errors/synod-builtin"
	TypeSigilActive           = "https://soul-stack.com/errors/sigil-already-active"
	TypeSigilNotFound         = "https://soul-stack.com/errors/sigil-not-found"
	TypePluginNotInCache      = "https://soul-stack.com/errors/plugin-not-in-cache"
	TypeServiceExists         = "https://soul-stack.com/errors/service-already-exists"
	// Augur — the Omen / Rite registry (ADR-025, augur.md). omen-already-exists —
	// UNIQUE on omens.name (409). not-found Omen / Rite — the shared TypeNotFound.
	TypeOmenExists = "https://soul-stack.com/errors/omen-already-exists"
	// Oracle — the Vigil / Decree registries (ADR-030, beacons S3). *-already-exists —
	// UNIQUE on vigils.name / decrees.name (409). not-found — the shared TypeNotFound.
	TypeVigilExists  = "https://soul-stack.com/errors/vigil-already-exists"
	TypeDecreeExists = "https://soul-stack.com/errors/decree-already-exists"
	// Sigil signing key rotation (ADR-026(h), R3-S7).
	TypeSigilKeyNotFound         = "https://soul-stack.com/errors/sigil-key-not-found"
	TypeSigilKeyLastActive       = "https://soul-stack.com/errors/sigil-key-last-active"
	TypeSigilKeyPrimary          = "https://soul-stack.com/errors/sigil-key-primary"
	TypeSigilKeyConcurrentChange = "https://soul-stack.com/errors/sigil-key-concurrent-change"
	// `GET /v1/souls/{sid}/soulprint`: the Soul's record exists, but a typed
	// SoulprintReport has never arrived (410 Gone). Distinguishes from 404: the Soul itself
	// is registered, but there are no facts yet (just onboarded / `transport: ssh` without an agent).
	TypeSoulprintNotReceived = "https://soul-stack.com/errors/soulprint-not-received"
	// TypeClusterDegraded — the cluster is in degraded mode (Toll, ADR-038): the rate
	// of mass Soul churn exceeded the threshold within the window, the write API is
	// temporarily blocked (503 Service Unavailable + Retry-After). The read API, RBAC,
	// destroy, and Errand remain available (recovery actions).
	TypeClusterDegraded = "https://soul-stack.com/errors/cluster-degraded"
	// TypePushProviderExists — a UNIQUE violation on push_providers.name (409,
	// ADR-032 amendment 2026-05-26, S7-2). Symmetric with TypeServiceExists /
	// TypeOperatorExists.
	TypePushProviderExists = "https://soul-stack.com/errors/push-provider-already-exists"
	// Cloud Provider / Profile — operator-facing CRUD for the providers /
	// profiles registries (ADR-017, docs/keeper/cloud.md).
	// *-already-exists — a UNIQUE violation on providers.name / profiles.name
	// (409, symmetric with TypePushProviderExists / TypeServiceExists). not-found
	// Provider / Profile — the shared TypeNotFound; FK Profile→missing Provider —
	// also TypeNotFound (ErrProviderNotFound from profile, parity with Tiding→Herald);
	// a broken name/type/credentials_ref — the shared TypeValidationFailed (422).
	TypeProviderExists = "https://soul-stack.com/errors/provider-already-exists"
	TypeProfileExists  = "https://soul-stack.com/errors/profile-already-exists"
	// TypeProviderHasProfiles — deleting the Provider is blocked by dependent
	// Profiles (FK profiles_provider_fk ON DELETE RESTRICT, migration 020).
	// 409 Conflict: "target state unreachable, dependencies exist" —
	// the operator must delete the profiles first.
	TypeProviderHasProfiles = "https://soul-stack.com/errors/provider-has-profiles"
	// TypeErrandNotCancellable — an attempt to cancel an Errand that is already in a
	// terminal status (DELETE /v1/errands/{errand_id}, ADR-033 slice E5).
	// 409 Conflict — the correct code for "target state unreachable".
	TypeErrandNotCancellable = "https://soul-stack.com/errors/errand-not-cancellable"
	// TypeBadGateway — keeper itself is healthy, but the external git source returned an
	// error (`GET /v1/services/{name}/refs` → ls-remote). 502 Bad Gateway — the correct
	// code for "upstream service unavailable"; detail carries through the original
	// cause (DNS / auth / unsupported scheme — all "not our fault").
	TypeBadGateway = "https://soul-stack.com/errors/bad-gateway"
	// Choir/Voice — host topology within an incarnation (ADR-044, S-T3).
	// *-already-exists — a UNIQUE violation on PK `incarnation_choirs` / `incarnation_choir_voices`
	// (409). not-found Choir / Voice / incarnation — the shared TypeNotFound. SIDs outside
	// incarnation membership (ErrNotMembers) — the shared TypeValidationFailed (422).
	TypeChoirExists = "https://soul-stack.com/errors/choir-already-exists"
	TypeVoiceExists = "https://soul-stack.com/errors/voice-already-exists"
	// TypeTempoExceeded — the per-AID Tempo rate limit was exceeded (ADR-050): the operator
	// is hitting a resolver-heavy write endpoint too frequently. 429 Too Many
	// Requests + a Retry-After header (seconds until at least one
	// token refills). A separate URN and code from [TypeClusterDegraded] (503 cluster-wide
	// on cluster health): Tempo is 429 per-AID by frequency; a shared problem+json/
	// Retry-After framework, different risk.
	TypeTempoExceeded = "https://soul-stack.com/errors/tempo-exceeded"
	// TypeAuthThrottled — the anti-bruteforce limit of the public login endpoint was
	// exceeded (ADR-058(g), HIGH-3): too many attempts from an IP/username, OR
	// a lockout after a series of failures. 429 Too Many Requests + Retry-After (seconds
	// until it lifts). A separate URN from [TypeTempoExceeded] (per-AID, post-JWT):
	// auth-throttle is pre-JWT by IP/username; anti-oracle — detail carries no reason
	// (we don't disclose whether it was by IP or by username, locked or throttled).
	TypeAuthThrottled = "https://soul-stack.com/errors/auth-throttled"
	// Herald/Tiding — notifications about run events (ADR-052, S4).
	// *-already-exists — a UNIQUE violation on heralds.name / tidings.name (409,
	// symmetric with TypeOmenExists / TypePushProviderExists). not-found Herald /
	// Tiding — the shared TypeNotFound; FK Tiding→missing Herald — also TypeNotFound
	// (ErrHeraldNotFound, parity with Rite→missing Omen). A broken config / event_types /
	// secret_ref — the shared TypeValidationFailed (422).
	TypeHeraldExists = "https://soul-stack.com/errors/herald-already-exists"
	TypeTidingExists = "https://soul-stack.com/errors/tiding-already-exists"
	// TypeProvisioningMethodDisabled — the provisioning_allowed_methods policy
	// (ADR-058 Part B) forbade CREATING an operator by this method. 403 Forbidden:
	// the gate on the creation branch (POST /v1/operators → user; federated auto-provision →
	// ldap/oidc). A separate URN from the generic [TypeForbidden] (no RBAC permission) — this
	// is a policy refusal by provisioning method, not a lack of permission; the UI/SDK can
	// distinguish "method disabled by policy" from "insufficient permissions".
	TypeProvisioningMethodDisabled = "https://soul-stack.com/errors/provisioning-method-disabled"
	// TypeAssertFailed — a scenario `assert:` predicate failed at the pre-flight
	// gate for CREATING a run (ADR-009/ADR-027 amendment 2026-06-23, form A):
	// the run's roster does not satisfy a topology invariant (e.g. a cluster size
	// guard). 422 Unprocessable Entity — the request is syntactically valid, but a
	// MODEL precondition is not satisfied (parity with `validation-failed`/input_invalid:
	// the same 422 class "input semantics don't add up"). A separate URN from
	// `validation-failed`: the UI/SDK distinguishes "topology doesn't add up" (blame the
	// roster/scope) from "the input field doesn't match the schema". The incarnation is NOT
	// created, no fail status (error_locked) is set — the refusal happens at the model stage BEFORE commit.
	// NOT 412 Precondition Failed (reserved for conditional headers
	// If-Match/If-None-Match — must not be confused).
	TypeAssertFailed = "https://soul-stack.com/errors/assert-failed"
)

// titles — fixed English headings for each known `type`.
// Match the normative specification (operator-api.md).
var titles = map[string]string{
	TypeUnauthenticated:            "Authentication required",
	TypeForbidden:                  "Forbidden",
	TypeNotFound:                   "Resource not found",
	TypeMethodNotAllowed:           "Method not allowed",
	TypeMalformedRequest:           "Malformed request",
	TypeValidationFailed:           "Validation failed",
	TypeInternalError:              "Internal server error",
	TypeOperatorExists:             "Operator already exists",
	TypeOperatorRevoked:            "Operator is revoked",
	TypeOperatorRevokedToken:       "Operator revoked",
	TypeWouldLockOutCluster:        "Operation would lock out the cluster",
	TypeIncarnationExists:          "Incarnation already exists",
	TypeIncarnationLocked:          "Incarnation is locked",
	TypeRerunInputUnavailable:      "Rerun input unavailable",
	TypeSoulExists:                 "Soul already exists",
	TypeBootstrapTokenActive:       "Bootstrap token already active",
	TypeRoleNotFound:               "Role not found",
	TypeRoleExists:                 "Role already exists",
	TypeRoleBuiltin:                "Role is builtin",
	TypeSynodNotFound:              "Synod not found",
	TypeSynodExists:                "Synod already exists",
	TypeSynodBuiltin:               "Synod is builtin",
	TypeSigilActive:                "Sigil already active",
	TypeSigilNotFound:              "Sigil not found",
	TypePluginNotInCache:           "Plugin not found in host cache",
	TypeServiceExists:              "Service already exists",
	TypeOmenExists:                 "Omen already exists",
	TypeVigilExists:                "Vigil already exists",
	TypeDecreeExists:               "Decree already exists",
	TypeSigilKeyNotFound:           "Sigil signing key not found",
	TypeSigilKeyLastActive:         "Cannot retire the last active sigil signing key",
	TypeSigilKeyPrimary:            "Cannot retire the primary sigil signing key",
	TypeSigilKeyConcurrentChange:   "Concurrent primary-key change; retry",
	TypeSoulprintNotReceived:       "Soulprint not yet received",
	TypeClusterDegraded:            "Cluster is in degraded mode",
	TypePushProviderExists:         "Push provider already exists",
	TypeProviderExists:             "Cloud provider already exists",
	TypeProfileExists:              "Cloud profile already exists",
	TypeProviderHasProfiles:        "Cloud provider has dependent profiles",
	TypeErrandNotCancellable:       "Errand is not cancellable",
	TypeBadGateway:                 "Bad gateway",
	TypeChoirExists:                "Choir already exists",
	TypeVoiceExists:                "Voice already exists",
	TypeTempoExceeded:              "Too many requests",
	TypeAuthThrottled:              "Too many login attempts",
	TypeHeraldExists:               "Herald already exists",
	TypeTidingExists:               "Tiding already exists",
	TypeProvisioningMethodDisabled: "Provisioning method disabled by policy",
	TypeAssertFailed:               "Assertion failed",
}

// Details — the JSON shape of the RFC 7807 object. Fields strictly follow the RFC; custom
// extensions (e.g. `errors[]` for validation fields) will be added as
// explicit additional fields in follow-up slices as the need arises.
type Details struct {
	Type     string `json:"type"`
	Title    string `json:"title"`
	Status   int    `json:"status"`
	Detail   string `json:"detail,omitempty"`
	Instance string `json:"instance,omitempty"`
}

// Write writes p to the ResponseWriter with Content-Type=`application/problem+json`.
// Does not call log.* and does not wrap p — that is the caller's responsibility
// (logging is done by the error middleware, not the problem package itself).
func Write(w http.ResponseWriter, p Details) {
	w.Header().Set("Content-Type", ContentType)
	w.WriteHeader(p.Status)
	// json.NewEncoder will not return an error for a well-typed struct; if
	// the transport write fails (client disconnected) — that's already
	// after WriteHeader, there's nowhere to log the error. Ignored deliberately.
	_ = json.NewEncoder(w).Encode(p)
}

// New assembles Details from a type URN and a detail message. Title and status
// are filled in from [titles] / [statuses]. For an unknown `t` it returns
// Details with an empty Title and status=500 — the caller needs to explicitly use
// one of the Type* constants.
func New(t, instance, detail string) Details {
	return Details{
		Type:     t,
		Title:    titles[t],
		Status:   statuses[t],
		Detail:   detail,
		Instance: instance,
	}
}

// statuses — the reverse mapping `type → HTTP status`. Matches the
// table in operator-api.md.
var statuses = map[string]int{
	TypeUnauthenticated:            http.StatusUnauthorized,
	TypeForbidden:                  http.StatusForbidden,
	TypeNotFound:                   http.StatusNotFound,
	TypeMethodNotAllowed:           http.StatusMethodNotAllowed,
	TypeMalformedRequest:           http.StatusBadRequest,
	TypeValidationFailed:           http.StatusUnprocessableEntity,
	TypeInternalError:              http.StatusInternalServerError,
	TypeOperatorExists:             http.StatusConflict,
	TypeOperatorRevoked:            http.StatusConflict,
	TypeOperatorRevokedToken:       http.StatusUnauthorized,
	TypeWouldLockOutCluster:        http.StatusConflict,
	TypeIncarnationExists:          http.StatusConflict,
	TypeIncarnationLocked:          http.StatusConflict,
	TypeRerunInputUnavailable:      http.StatusConflict,
	TypeSoulExists:                 http.StatusConflict,
	TypeBootstrapTokenActive:       http.StatusConflict,
	TypeRoleNotFound:               http.StatusNotFound,
	TypeRoleExists:                 http.StatusConflict,
	TypeRoleBuiltin:                http.StatusConflict,
	TypeSynodNotFound:              http.StatusNotFound,
	TypeSynodExists:                http.StatusConflict,
	TypeSynodBuiltin:               http.StatusConflict,
	TypeSigilActive:                http.StatusConflict,
	TypeSigilNotFound:              http.StatusNotFound,
	TypePluginNotInCache:           http.StatusNotFound,
	TypeServiceExists:              http.StatusConflict,
	TypeOmenExists:                 http.StatusConflict,
	TypeVigilExists:                http.StatusConflict,
	TypeDecreeExists:               http.StatusConflict,
	TypeSigilKeyNotFound:           http.StatusNotFound,
	TypeSigilKeyLastActive:         http.StatusConflict,
	TypeSigilKeyPrimary:            http.StatusConflict,
	TypeSigilKeyConcurrentChange:   http.StatusConflict,
	TypeSoulprintNotReceived:       http.StatusGone,
	TypeClusterDegraded:            http.StatusServiceUnavailable,
	TypePushProviderExists:         http.StatusConflict,
	TypeProviderExists:             http.StatusConflict,
	TypeProfileExists:              http.StatusConflict,
	TypeProviderHasProfiles:        http.StatusConflict,
	TypeErrandNotCancellable:       http.StatusConflict,
	TypeBadGateway:                 http.StatusBadGateway,
	TypeChoirExists:                http.StatusConflict,
	TypeVoiceExists:                http.StatusConflict,
	TypeTempoExceeded:              http.StatusTooManyRequests,
	TypeAuthThrottled:              http.StatusTooManyRequests,
	TypeHeraldExists:               http.StatusConflict,
	TypeTidingExists:               http.StatusConflict,
	TypeProvisioningMethodDisabled: http.StatusForbidden,
	TypeAssertFailed:               http.StatusUnprocessableEntity,
}
