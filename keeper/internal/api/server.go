// Package api is the HTTP facade of the Keeper Operator API
// (M0.6a: framework + auth + health/meta; M0.6b/c adds endpoints).
//
// It isolates the router choice (chi) from the rest of the keeper code: external code
// depends only on the [Server] and [Deps] types.
package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
	"github.com/souls-guild/soul-stack/keeper/internal/api/health"
	apimiddleware "github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
	"github.com/souls-guild/soul-stack/keeper/internal/applybus"
	"github.com/souls-guild/soul-stack/keeper/internal/auditpg"
	"github.com/souls-guild/soul-stack/keeper/internal/augur"
	"github.com/souls-guild/soul-stack/keeper/internal/errand"
	"github.com/souls-guild/soul-stack/keeper/internal/herald"
	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/operator"
	"github.com/souls-guild/soul-stack/keeper/internal/oracle"
	"github.com/souls-guild/soul-stack/keeper/internal/profile"
	"github.com/souls-guild/soul-stack/keeper/internal/provider"
	"github.com/souls-guild/soul-stack/keeper/internal/pushorch"
	"github.com/souls-guild/soul-stack/keeper/internal/pushprovider"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac"
	"github.com/souls-guild/soul-stack/keeper/internal/serviceregistry"
	"github.com/souls-guild/soul-stack/keeper/internal/sigil"
	"github.com/souls-guild/soul-stack/keeper/internal/toll"
	"github.com/souls-guild/soul-stack/shared/audit"
	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/obs"
)

// Deps — the HTTP server's external dependencies. M0.6b adds JWTIssuer
// (token issuance via `POST /v1/operators` and `issue-token`),
// AuditWriter (writing events after an RBAC-passed operation),
// RBAC (permission-check middleware + handler-side ClusterAdmins/RolesOf),
// OperatorPool (handler access to the registry with BeginTx for the self-lockout tx)
// and TTLDefault (TTL of JWTs issued by the API).
//
// RBAC is passed as [RBACProvider] — the common surface for
// [rbac.Enforcer] and [rbac.Holder]; the production wire-up in `keeper run` gives a
// Holder, which re-derives the Enforcer on every Reload swap of the Store
// (hot-reload of the `rbac:` block, ADR-021 + docs/keeper/config.md).
//
// Incarnation/Soul/Push/Cloud handlers — M0.6c+. M0.6c-1: IncarnationDB
// added; scenario-execution / migrate-executor — M0.6c-2/3.
type Deps struct {
	JWTVerifier *jwt.Verifier
	JWTIssuer   handlers.JWTIssuer
	PGPinger    health.Pinger
	RedisPinger health.Pinger
	VaultPinger health.Pinger
	AuditWriter audit.Writer
	RBAC        RBACProvider

	// RBACSvc — the RBAC-CRUD business logic (roles / permissions / membership)
	// for the future role.* endpoints (Slice 2a). DIFFERS from RBAC above:
	// that one is a read-only enforcer surface (Check / ClusterAdmins / RolesOf
	// for middleware and the operator handler), this one is the mutating CRUD facade
	// ([rbac.Service]). When nil the role.* routes simply aren't wired (Slice
	// 1.5 threads the field, Slice 2a registers the routes).
	RBACSvc *rbac.Service

	// SigilSvc — the Sigil allow-list business logic (plugin.allow/revoke/list,
	// ADR-026 S4a). When nil the plugin.* routes aren't wired (the production wire-up
	// in `keeper run` passes *sigil.Service over Signer+Store+the host cache;
	// unit/integration tests without Sigil remain valid). Symmetric to
	// [RBACSvc].
	SigilSvc *sigil.Service

	// SigilKeySvc — the business logic for rotating Sigil signing trust-anchor keys
	// (sigil.key-introduce/retire/list/set-primary, ADR-026(h) R3-S7). When nil
	// the sigil/keys routes aren't wired (the production wire-up with Sigil enabled
	// passes *sigil.KeyService). Symmetric to [SigilSvc].
	SigilKeySvc *sigil.KeyService

	// ServiceSvc — the Service registry business logic (service.register/update/
	// list/deregister, ADR-028 RBAC-storage pattern). When nil the service.* routes
	// aren't wired (the production wire-up in `keeper run` passes the same
	// *serviceregistry.Service that carries the S2-invalidate hook; unit/integration
	// tests without the registry remain valid). Symmetric to [RBACSvc] / [SigilSvc].
	ServiceSvc *serviceregistry.Service

	// ServiceRefs — a TTL cache of the git-ls-remote tag/branch listing for
	// `GET /v1/services/{name}/refs` (UI Upgrade-modal dropdown). Optional:
	// when nil the /refs endpoint answers 500 (feature not configured); service
	// CRUD itself stays functional. The production wire-up in `keeper run`
	// passes *serviceregistry.RefsCache over artifact.RefsListerFunc(
	// artifact.ListRefs).
	ServiceRefs handlers.ServiceRefsLister

	// ServiceScenarios — a TTL cache of the scenario listing from the materialized
	// snapshot of the Service's git repo, for `GET /v1/services/{name}/scenarios` (UI
	// Run-modal dropdown). Optional: when nil the /scenarios endpoint answers 500
	// (feature not configured); service CRUD itself stays functional.
	// The production wire-up in `keeper run` passes *serviceregistry.ScenariosCache
	// over ScenarioListerFunc, which resolves `(name,gitURL,ref)` via
	// *artifact.ServiceLoader.Load → artifact.ListScenarios.
	ServiceScenarios handlers.ServiceScenarioLister

	// ServiceStateSchema — a TTL cache of the state_schema metadata listing
	// (`state_schema_version` + an optional state-structure declaration + the
	// migration chain) from the materialized snapshot of the Service's git repo, for
	// `GET /v1/services/{name}/state-schema` (UI Schema explorer).
	// Optional: when nil the /state-schema endpoint answers 500 (feature not
	// configured); service CRUD itself stays functional.
	// The production wire-up in `keeper run` passes *serviceregistry.StateSchemaCache
	// over StateSchemaListerFunc, which resolves `(name,gitURL,ref)` via
	// *artifact.ServiceLoader.Load → artifact.ListStateSchema.
	ServiceStateSchema handlers.ServiceStateSchemaLister

	// ServiceDependencies — a TTL cache of the git-dependency listing (destiny/modules
	// from `service.yml`) for `GET /v1/services/{name}/dependencies` (UI Service
	// Detail). Optional: when nil the /dependencies endpoint answers 500 (feature not
	// configured); service CRUD itself stays functional.
	// The production wire-up in `keeper run` passes *serviceregistry.DependenciesCache
	// over DependenciesListerFunc, which resolves `(name,gitURL,ref)` via
	// *artifact.ServiceLoader.Load → artifact.ListDependencies.
	ServiceDependencies handlers.ServiceDependenciesLister

	// ServiceDirectives — a TTL cache of the catalog of valid redis.conf directives by version
	// (essence.redis_directives) for `GET /v1/services/{name}/directives` (the UI
	// redis_settings editor). Optional: when nil the /directives endpoint answers
	// 500 (feature not configured); service CRUD itself stays functional.
	// The production wire-up in `keeper run` passes *serviceregistry.DirectivesCache
	// over DirectiveListerFunc, which resolves `(name,gitURL,ref)` via
	// *artifact.ServiceLoader.Load → artifact.LoadDirectiveCatalog.
	ServiceDirectives handlers.ServiceDirectivesLister

	// ServiceTelemetry — TTL cache of the default (per-service, no essence) host-vitals
	// telemetry config (manifest `telemetry:` -> effective defaults) + the allowed
	// set of collectors for `GET /v1/services/{name}/telemetry` (UI editor,
	// ADR-042/072). Optional: when nil the /telemetry endpoint responds 500 (feature not
	// configured); service-CRUD itself stays operational. Production wire-up
	// in `keeper run` passes *serviceregistry.TelemetryCache over TelemetryListerFunc,
	// resolving `(name,gitURL,ref)` via *artifact.ServiceLoader.Load →
	// essence.ResolveEffectiveTelemetry.
	ServiceTelemetry handlers.ServiceTelemetryLister

	// AugurSvc — the Augur registry management logic (omen.create/list/delete +
	// rite.create/list/delete, ADR-025). When nil the augur.* routes aren't wired
	// (the production wire-up in `keeper run` passes the same *augur.Service as MCP).
	// Symmetric to [ServiceSvc]. Do NOT confuse with the Augur broker (resolve/broker) —
	// that lives in the EventStream path, not the Operator API.
	AugurSvc *augur.Service

	// OracleSvc — the Oracle registries management logic (vigil.create/list/delete +
	// decree.create/list/delete, ADR-030 beacons). When nil the vigil.*/decree.* routes
	// aren't wired (the production wire-up in `keeper run` passes the same
	// *oracle.Service as MCP). Symmetric to [AugurSvc]. Do NOT confuse with the
	// reactor router (match/enqueue) — that lives in the EventStream path.
	OracleSvc *oracle.Service

	OperatorDB    handlers.OperatorPool
	IncarnationDB handlers.IncarnationDB
	SoulDB        handlers.SoulPool
	TTLDefault    time.Duration

	// ApplyBus — the pub/sub bus of apply events for the run's live SSE (ADR-068 §A3,
	// GET /v1/incarnations/{name}/runs/{apply_id}/events). The same bus as the
	// grpc handlers + the scenario-runner (publishers). When nil the SSE route is not
	// mounted (opt-in wire-up, VoyageDB pattern).
	ApplyBus *applybus.EventBus

	// ChoirDB — the CRUD surface of the Choir/Voice registry (ADR-044, S-T3). When nil
	// the choir.* routes aren't wired (PushProviderSvc pattern).
	// The production wire-up in `keeper run` passes the same *pgxpool.Pool as
	// IncarnationDB (the Choir tables live in the same DB).
	ChoirDB handlers.ChoirDB

	// SoulPresence — a lease-overlay presence for `GET /v1/souls` (ADR-006(a)):
	// the `status` field of the List/Get response is derived from the live Redis SID lease, not
	// served as the lazily-reconciled PG snapshot `souls.status` (otherwise a
	// reconnected Soul hangs `disconnected` until the next Reaper tick).
	// Optional: when nil the overlay is off (single-instance dev / unit without Redis),
	// the PG snapshot is served. The production wire-up in `keeper run` passes a wrapper over
	// the same Redis client as the topology resolver (keeperredis.SoulsStreamAlive).
	SoulPresence handlers.SoulPresence

	// UtilizationReader — Redis layer of host-vitals for the telemetry endpoints
	// (NIM-86, ADR-006): GET /v1/souls/{sid}/telemetry and
	// /v1/incarnations/{name}/telemetry read the utilization snapshot from Redis, NOT
	// from PG. Optional: when nil (single-instance dev / unit without Redis) the reader
	// is a no-op (stale/empty). Production wire-up in `keeper run` passes a wrapper
	// over the same Redis client (keeperredis.ReadUtilization).
	UtilizationReader handlers.UtilizationReader

	// SoulStatsStaleFn — the provider of the "stale" last_seen_at threshold for
	// stale_count in `GET /v1/souls/stats` (the same Reaper mark_disconnected.stale_after).
	// The function reads FRESH config (hot-reload) on every request,
	// symmetric to TempoVoyageCreateLimits. nil → registration substitutes the default
	// (90s, parity reaper.defaultMarkDisconnectedStale) — valid for unit tests.
	SoulStatsStaleFn func() time.Duration

	// ClusterRegistry / ClusterLeaderReader / SelfKID — dependencies of `GET /v1/cluster`
	// (HA topology from Conclave + the Reaper leader). Optional: when nil ClusterRegistry
	// the route isn't mounted (single-Keeper dev without Redis — no cluster view needed).
	// The production wire-up in `keeper run` passes wrappers over the same Redis client
	// as Conclave renewal (keeperredis.LiveKIDs / ReadInstanceMeta /
	// PeekLeaseHolder(reaper.LeaderLeaseKey)); SelfKID = soulstack.kid from config.
	ClusterRegistry     handlers.ClusterRegistry
	ClusterLeaderReader handlers.ClusterLeaderReader
	SelfKID             string

	// ScenarioRunner / ServiceRegistry — optional: when both are non-nil
	// `POST /v1/incarnations` runs the scenario `create` (production);
	// when nil — Create stays a stub (insert row, without apply).
	ScenarioRunner  handlers.ScenarioStarter
	ServiceRegistry handlers.ServiceResolver

	// ScenarioDestroyer — optional: needed for `DELETE /v1/incarnations/{name}`
	// (async teardown of the scenario `destroy` in TerminalDestroy, S-D2b). A separate
	// field from ScenarioRunner — the narrow interface [handlers.DestroyStarter]
	// (StartDestroy), though the production wire-up passes the same *scenario.Runner.
	// When nil Destroy answers 500 (endpoint not configured).
	ScenarioDestroyer handlers.DestroyStarter

	// ScenarioDrift — optional: needed for `POST /v1/incarnations/{name}/check-drift`
	// (the Scry on-demand pilot, ADR-031). The narrow interface [handlers.DriftChecker]
	// (CheckDrift + MarkDriftStatus); the production wire-up passes the same
	// *scenario.Runner. When nil check-drift answers 500.
	ScenarioDrift handlers.DriftChecker

	// ServiceLoader — optional: needed for `POST /v1/incarnations/{name}/upgrade`
	// (materializing the snapshot of the target service-ref + assembling the
	// migration chain). When nil Upgrade answers 500. The production wire-up
	// passes *artifact.ServiceLoader.
	ServiceLoader handlers.ServiceSnapshotLoader

	// VaultClient — the Vault KV read surface for the incarnation secret reveal
	// endpoint (NIM-74, POST/GET /v1/incarnations/{name}/secrets/*). Optional:
	// when nil RevealSecretTyped answers 404 (endpoint not configured).
	// The production wire-up in `keeper run` passes *vault.Client (the same d.vc).
	VaultClient handlers.VaultKVReader

	// PushRun — the multi-host push orchestrator (Variant C, ADR-004 push-flow +
	// docs/keeper/push.md). When nil the push.* routes aren't wired (the
	// SigilSvc/AugurSvc/OracleSvc pattern): keeper starts without SSH plugins, and
	// `POST /v1/push/apply` / `GET /v1/push/{apply_id}` stay 404.
	// The production wire-up in the daemon assembles PushRun via setupPushOrchestrator
	// (after bringing up SshDispatcher from push S1+S5).
	PushRun *pushorch.PushRun

	// PushProviderSvc — the CRUD business logic of the Push-Provider registry
	// (push-provider.create/update/delete/list/read, ADR-032 amendment 2026-05-26,
	// S7-2). When nil the push-provider.* routes aren't wired (the ServiceSvc/
	// AugurSvc pattern). The production wire-up in `keeper run` passes *pushprovider.Service
	// over pgxpool.Pool + a Redis publisher (push-providers:changed).
	PushProviderSvc *pushprovider.Service

	// HeraldSvc — the CRUD business logic of the Herald (channels) / Tiding (rules)
	// notification registries (herald.*/tiding.*, ADR-052, S4). When nil the herald.*/tiding.*
	// routes aren't wired (the PushProviderSvc/AugurSvc pattern). The production wire-up
	// in `keeper run` passes *herald.Service over pgxpool.Pool + the dispatcher
	// invalidator + a Redis publisher (herald:invalidate).
	HeraldSvc *herald.Service

	// ProviderSvc / ProfileSvc — operator-facing CRUD of the Cloud-Provider
	// (`providers`) and Cloud-Profile (`profiles`, ADR-017, docs/keeper/cloud.md) registries.
	// When nil the corresponding provider.*/profile.* routes aren't wired (the
	// PushProviderSvc/AugurSvc pattern). credentials_ref is served as a vault path, the secret
	// is not resolved. WITHOUT a Redis publisher: Cloud-Provider/Profile are read on-demand
	// at the scenario layer (`core.cloud.provisioned`), not hot-reloaded.
	ProviderSvc *provider.Service
	ProfileSvc  *profile.Service

	// ErrandDispatcher / ErrandStore — the pull-ad-hoc Errand contour (ADR-033).
	// When both are nil the errand.* routes aren't wired (the PushRun pattern). Wire-up
	// — setupErrandDispatcher (after setupGRPCEventStream: the dispatcher needs
	// Outbound). The dispatcher and store are created together from one PG pool
	// and a single ApplyBus, so both references are passed as one zone (not
	// one shared Service object): the Dispatcher carries the write path (Insert+Mark+
	// audit), the Store reads (Get/List). Symmetric to RBACProvider/CovenScoper
	// for SoulHandler (two roles around one pool).
	ErrandDispatcher *errand.Dispatcher
	ErrandStore      *errand.Store

	// VoyageDB / VoyageScenarioResolver / VoyageCommandResolver — the Voyage contour
	// (ADR-043, S5): a unified batched run (kind=scenario|command). When nil
	// VoyageDB the voyage.* routes aren't wired (the ErrandRunStore pattern).
	// VoyageDB — the same *pgxpool.Pool that carries IncarnationDB (the
	// voyages/voyage_targets tables in the same DB). Resolvers:
	//   - VoyageScenarioResolver → incarnation names (production: NewVoyage
	//     ScenarioPGResolver(d.pool));
	//   - VoyageCommandResolver → SID snapshot (production: NewVoyageCommandPG
	//     Resolver(d.pool)).
	// When any resolver is nil — the corresponding create kind-branch answers 500.
	VoyageDB               handlers.VoyageStore
	VoyageScenarioResolver handlers.VoyageScenarioResolver
	VoyageCommandResolver  handlers.VoyageCommandResolver
	// VoyageMaxScope — the upper limit on the resolved scope size of one Voyage
	// (DoS-guard S-med-3). 0 → unlimited. Source — cfg.Voyage.ResolvedMaxScope().
	VoyageMaxScope int
	// VoyageMaxBatchSize — the upper limit on the batch/window size of one Voyage
	// (DoS-guard S-W4). 0 → no limit. Source — cfg.Voyage.ResolvedMaxBatchSize().
	VoyageMaxBatchSize int

	// CadenceDB — the CRUD surface of the Cadence-schedules registry (`cadences`,
	// ADR-046, S4). When nil the cadence.* routes aren't wired (the VoyageDB pattern).
	// The same *pgxpool.Pool that carries VoyageDB/IncarnationDB (the cadences table and
	// the back-link voyages.cadence_id in the same DB). The two-tier RBAC-by-kind
	// (ADR-046 §7) uses the same enforcer (RBAC).
	CadenceDB handlers.CadenceStore

	// CadencePollFloorSeconds — the lower limit on an interval-Cadence period (floor limit,
	// ADR-046 Pass B): create/update with `interval_seconds < floor` → 422. A SINGLE
	// source with the Conductor's adaptive polling — `cfg.CadenceScheduler.ResolvedPollFloor()`
	// (not a hardcoded 30 in two places). 0 → the floor check is off (dev/test).
	CadencePollFloorSeconds int

	// AuditReader — the read side of `audit_log` for `GET /v1/audit` (UI iteration 2).
	// When nil the audit route isn't wired (the PushRun/Errand pattern). The production
	// wire-up passes *auditpg.NewReader(pgPool) — the same pool that carries
	// auditWriter (writer + reader live over one table, separated
	// only by direction for type-safety).
	AuditReader *auditpg.Reader

	// MetricsHTTP — keeper_http_* instrumentation of `/v1/*` (registered
	// over *obs.Registry via [obs.RegisterHTTPMetrics]). When nil
	// HTTP metrics aren't collected (see router.go) — acceptable in unit tests.
	//
	// The `/metrics` endpoint itself is NOT served here: it is moved to a
	// dedicated listener (`listen.metrics.addr`, ADR-024) in
	// keeper/cmd/keeper; the openapi router only instruments /v1/*.
	MetricsHTTP *obs.HTTPMetrics

	// ModuleCatalogPlugins — the read surface of active plugin allowances for
	// the module-catalog (`GET /v1/modules`, UI Run→Command module-search).
	// Optional: when nil the catalog returns only core modules (the static doc
	// table is always available), the plugin section is empty. The production wire-up in
	// `keeper run` passes an adapter over the sigil store (ListActive → ManifestRaw).
	// The `/v1/modules` route itself is wired ALWAYS (the core catalog needs no
	// external dependencies), unlike the opt-in plugin.* routes.
	ModuleCatalogPlugins handlers.ModuleCatalogPlugins
	// ModuleFormPrepH — the resolver of source catalogs for the module UI form (ADR-045 S3).
	// Pre-built in the daemon over pgxpool (the VoyageH pattern); nil → the route
	// /v1/modules/{name}/form-prep isn't mounted (the drift-test keeps the allowlist).
	ModuleFormPrepH *handlers.ModuleFormPrepHandler

	// TollDegraded — the Toll cluster-detector read flag (ADR-038). The middleware on
	// blocked routes (POST scenarios/run, POST push/apply) checks it via IsDegraded
	// on every request and blocks with 503 + Retry-After when the flag
	// is set. When nil — the middleware isn't attached (single-instance/
	// dev without Redis: no blocking needed, no one sets the flag).
	TollDegraded toll.DegradedReader

	// TempoLimiter — the Tempo per-AID rate-limiter (ADR-050) for the resolver-heavy
	// `POST /v1/voyages`. When nil (no Redis / Tempo disabled) the middleware is
	// passthrough — the targeted attachment on the route just lets requests through. The production
	// wire-up passes *redis.TokenBucket over the live Redis client.
	TempoLimiter apimiddleware.RateLimiter

	// TempoMetrics — keeper_tempo_* counters (ADR-050(g)). nil-safe (nil →
	// emit no-op). The production wire-up passes *TempoMetrics from the metrics registry.
	TempoMetrics apimiddleware.RateLimitMetrics

	// TempoVoyageCreateLimits — the provider of the live rate/burst of the voyage-create bucket
	// (hot-reload, ADR-050(f)/ADR-021): reads the config.Store snapshot on every
	// request. nil → defaults [config.DefaultTempoVoyageCreate*] (resolved at
	// router assembly). Used only when TempoLimiter is non-nil.
	TempoVoyageCreateLimits func() apimiddleware.RateLimitLimits

	// TempoVoyagePreviewLimits — the provider of the live rate/burst of the voyage-preview bucket
	// (hot-reload, ADR-050(f)/ADR-021 + amendment 2026-06-17 — a separate bucket).
	// Reads the config.Store snapshot on every request. nil → defaults
	// [config.DefaultTempoVoyagePreview*] (resolved at router assembly).
	// Used only when TempoLimiter is non-nil.
	TempoVoyagePreviewLimits func() apimiddleware.RateLimitLimits

	// WebUIEnabled — the resolved toggle of the embedded UI on the `/ui` route
	// (ADR-055): true → the go:embed static is mounted (publicly, OUTSIDE /v1, parity with
	// /docs); false → /ui isn't wired. Resolved by the daemon from
	// [config.KeeperConfig.WebUIMounted] (default-ON: nil-config → true).
	// A zero value (false) for callers that don't set the field (unit tests without UI)
	// is a deliberate "don't mount": they don't need /ui, and mount requires the embed
	// tree. The toggle needs no external backend (the UI is baked into the binary).
	WebUIEnabled bool

	// LDAPAuth — federated LDAP authentication of operators (ADR-058,
	// POST /auth/ldap/login). When nil the endpoint isn't mounted (an opt-in domain,
	// the pushH/errandH pattern): keeper.yml::auth.ldap unset → the login method
	// is unavailable, Keeper starts (ADR-053 OPTIONAL tier). The daemon assembles the field
	// when auth.ldap is present (resolving bind_password_ref/ca_ref from Vault).
	LDAPAuth *LDAPAuthDeps

	// OIDCAuth — federated OIDC authentication of operators (ADR-058 stage 2,
	// GET /auth/oidc/{login,callback}). When nil the endpoints aren't mounted (opt-in,
	// like LDAPAuth): keeper.yml::auth.oidc unset → the method is unavailable, Keeper
	// starts (ADR-053 OPTIONAL tier). The daemon assembles the field when auth.oidc is present
	// AND Redis is live (the flow-state store is cluster-shared): without Redis OIDC is unavailable.
	OIDCAuth *OIDCAuthDeps

	// AuthToken — exchange of session-cookie for a short-lived Bearer (POST /auth/token,
	// NIM-77/ADR-058 Variant B). When nil the endpoint is not mounted (opt-in, same
	// pattern as LDAPAuth); production wire-up supplies shared verifier+issuer+rbacHolder.
	AuthToken *AuthTokenDeps

	// AuthMethods — booleans of the available login methods for the public
	// GET /auth/methods (UI login form). Meaning: /auth/methods is mounted
	// unconditionally (password is always available).
	AuthMethods AuthMethodsDeps

	// LoginGuard — anti-bruteforce primitive for public login endpoints (ADR-058(g),
	// HIGH-3): per-IP+per-username throttle + lockout. Implemented by *redis.LoginGuard.
	// nil (no Redis) -> login endpoints without throttle (passthrough, same as Tempo with
	// a nil limiter). The daemon assembles it when Redis is live. Used only when
	// /auth routes are mounted (non-nil LDAPAuth/OIDCAuth).
	LoginGuard apimiddleware.LoginGuard

	// LoginLimitCfg — the static parameters of the anti-bruteforce limit (resolved from
	// config.KeeperAuth.ResolvedLoginRateLimit()). Read once at middleware
	// assembly (login is rare, not a hot path).
	LoginLimitCfg apimiddleware.AuthLoginLimitConfig

	// ProvisioningPolicyReader — the read snapshot of the provisioning_allowed_methods policy
	// for GET /v1/provisioning-policy (ADR-058 Part B). Implemented by
	// *serviceregistry.Holder (a cluster-consistent atomic snapshot). PUT writes via
	// [ServiceSvc].SetSetting. The provisioning-policy routes are mounted only when
	// ProvisioningPolicyReader is non-nil AND ServiceSvc is non-nil (both needed: read +
	// write). nil → the group isn't wired (unit tests without serviceregistry).
	ProvisioningPolicyReader handlers.ProvisioningPolicyReader
}

// RBACProvider — the common rbac-service surface needed by both the middleware
// (Check / HoldsAction) and the handler (ClusterAdmins / RolesOf). Implemented by
// [rbac.Enforcer] (a static snapshot, for unit tests) and [rbac.Holder]
// (a hot-reload-aware wrapper over [config.Store], production).
//
// ActionHolder (ADR-047 §g G1) — the existence gate of read endpoints
// ([apimiddleware.RequireAction]): read-souls routes are gated on "does the operator
// hold soul.list AT ALL", with scope narrowing done by the handler after fetching the rows.
type RBACProvider interface {
	apimiddleware.PermissionChecker
	apimiddleware.ActionHolder
	handlers.RBACSource
	handlers.PurviewResolver
	handlers.PermissionsLister
}

// Server — a wrapper over http.Server with a pre-computed listener and a logger.
// The constructor doesn't bind to a port until Start, so that NewServer is
// cheap and can't finish with a bind race condition.
//
// The addr field is guarded by mu — Start updates it with the actual address (important
// for `:0` in tests), Addr() reads it; without mu the Go race detector catches a
// write-vs-read goroutine boundary.
type Server struct {
	srv        *http.Server
	configAddr string

	// operatorHandler is held by reference so the caller (keeper/cmd/keeper)
	// can obtain the inner [operator.Service] via [Server.OperatorService]
	// and reuse it in the MCP listener — a single source of truth for
	// the Operator-CRUD business logic (M0.7, PM-decision delegation.md #6).
	operatorHandler *handlers.OperatorHandler

	mu     sync.Mutex
	addr   string
	logger *slog.Logger
}

// OperatorService returns the [operator.Service] encapsulated in the server's
// inner OperatorHandler. Used by the MCP listener wire-up
// in keeper/cmd/keeper to reuse the single-source-of-truth logic
// (delegation.md PM-decision #6). Returns nil only if NewServer
// didn't/couldn't build the handler — the production path is always non-nil.
func (s *Server) OperatorService() *operator.Service {
	if s.operatorHandler == nil {
		return nil
	}
	return s.operatorHandler.Service()
}

// maxHeaderBytes — the limit on HTTP header size (request-line + headers).
// stdlib default is 1 MiB; the Operator API never has headers that big
// (Bearer JWT ~1 KiB), 16 KiB closes the "huge headers" DoS vector.
const maxHeaderBytes = 16 * 1024

// v1RequestBodyLimit — the limit on request body size under `/v1/*`. The MVP Operator
// endpoints accept compact JSON (POST /v1/operators ~200 bytes, revoke ~80,
// issue-token — empty body); 1 MiB closes the "multi-gigabyte payload" DoS
// with plenty of headroom for future incarnation endpoints
// (essence-yaml in spec.fragments + module-list). Exceeding it → MaxBytesError
// at Decode → 400 problem+json (TypeMalformedRequest).
const v1RequestBodyLimit = 1 << 20

// maxBodyMiddleware wraps Request.Body in [http.MaxBytesReader],
// limiting the number of bytes read. Applied on /v1/* (see router.go).
// http.Server doesn't limit the body itself — it must be explicit (anti-DoS, see RFC 9110
// §10.2).
func maxBodyMiddleware(limit int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.Body = http.MaxBytesReader(w, r.Body, limit)
			next.ServeHTTP(w, r)
		})
	}
}

// NewServer assembles the HTTP server. Returns an error on an invalid cfg
// (empty addr) or nil deps (the JWT verifier is mandatory — without it
// /v1/* loses authentication, which violates the RFC 7807 facade requirement).
//
// http.Server timeouts:
//
//   - ReadHeaderTimeout=5s — protection from Slowloris;
//   - ReadTimeout=30s — for the POST body (incarnation create / migration);
//   - WriteTimeout=60s — list endpoints may return hundreds of records;
//   - IdleTimeout=120s — keep-alive (matches default LB defaults);
//   - MaxHeaderBytes=16 KiB — anti-DoS on large headers.
//
// These are starting values; config-driven tuning — M0.6+ (a separate
// keeper.yml::listen.openapi.timeouts block).
//
// The body limit — [v1RequestBodyLimit], applied via
// [maxBodyMiddleware] on `/v1/*` (see router.go).
func NewServer(cfg config.KeeperListenSimple, deps Deps, logger *slog.Logger) (*Server, error) {
	if cfg.Addr == "" {
		return nil, errors.New("api: listen.openapi.addr is empty")
	}
	if deps.JWTVerifier == nil {
		return nil, errors.New("api: JWTVerifier is required")
	}
	if logger == nil {
		return nil, errors.New("api: logger is required")
	}
	if deps.RBAC == nil {
		return nil, errors.New("api: RBAC enforcer is required")
	}
	if deps.AuditWriter == nil {
		return nil, errors.New("api: AuditWriter is required")
	}
	if deps.OperatorDB == nil {
		return nil, errors.New("api: OperatorDB is required")
	}
	if deps.IncarnationDB == nil {
		return nil, errors.New("api: IncarnationDB is required")
	}
	if deps.SoulDB == nil {
		return nil, errors.New("api: SoulDB is required")
	}
	if deps.JWTIssuer == nil {
		return nil, errors.New("api: JWTIssuer is required")
	}
	if deps.TTLDefault <= 0 {
		return nil, errors.New("api: TTLDefault must be positive")
	}

	healthH := health.NewHandler(health.Deps{
		PG:    deps.PGPinger,
		Redis: deps.RedisPinger,
		Vault: deps.VaultPinger,
	})
	opH := handlers.NewOperatorHandler(deps.OperatorDB, deps.JWTIssuer, deps.RBAC, deps.TTLDefault, logger)
	// Gate for the provisioning_allowed_methods policy on POST /v1/operators (ADR-058
	// Part B): the same policy snapshot (Holder) as for the GET endpoint.
	// ProvisioningPolicyReader (Holder) also implements the narrow gate interface
	// (ProvisioningMethodAllowed) — we set it via a type-assert. nil reader / non-
	// Holder → gate not set (back-compat: CreateTyped skips it).
	if gate, ok := deps.ProvisioningPolicyReader.(handlers.ProvisioningGate); ok && gate != nil {
		opH.SetProvisioningGate(gate)
	}
	incH := handlers.NewIncarnationHandler(deps.IncarnationDB, deps.ScenarioRunner, deps.ScenarioDestroyer, deps.ScenarioDrift, deps.ServiceRegistry, deps.ServiceLoader, deps.AuditWriter, deps.RBAC, logger)
	// refs-lister (the same ls-remote cache as ServiceHandler) for the cheap mode of
	// GET .../upgrade-paths (ADR-0068 §6); late-binding, the constructor isn't extended.
	incH.SetServiceRefs(deps.ServiceRefs)
	// the read side of audit_log for GET .../runs/{apply_id}/tasks (per-host task
	// results of a run, NIM-37); late-binding, the same *auditpg.Reader as GET /v1/audit. nil
	// AuditReader → /tasks returns the plan without per-host results.
	if deps.AuditReader != nil {
		incH.SetRunTasksAuditReader(deps.AuditReader)
	}
	// Vault KV reader for the secret reveal endpoint (NIM-74); late-binding. nil →
	// RevealSecretTyped answers 404 (endpoint not configured).
	if deps.VaultClient != nil {
		incH.SetVaultReader(deps.VaultClient)
	}
	soulH := handlers.NewSoulHandler(deps.SoulDB, deps.RBAC, deps.SoulPresence, logger)

	// telemetryH — host-vitals read endpoints (NIM-86). Reuses soulH
	// (scope gate + coven listing); reader nil (dev/unit without Redis) -> no-op.
	telemetryH := handlers.NewTelemetryHandler(deps.UtilizationReader, soulH, logger)

	// clusterH is optional: when nil ClusterRegistry `GET /v1/cluster` isn't mounted
	// (single-Keeper dev without Redis — no cluster view needed). self_health uses
	// the same PG/Redis/Vault pingers as `/readyz` (health.Check — single source).
	var clusterH *handlers.ClusterHandler
	if deps.ClusterRegistry != nil {
		clusterH = handlers.NewClusterHandler(
			deps.ClusterRegistry, deps.ClusterLeaderReader,
			health.Deps{PG: deps.PGPinger, Redis: deps.RedisPinger, Vault: deps.VaultPinger},
			deps.SelfKID, logger)
	}

	// roleH is optional: when nil RBACSvc the role.* routes aren't wired (Slice
	// 1.5 threads the field, the production wire-up in `keeper run` passes
	// *rbac.Service). NewServer doesn't require RBACSvc — unit/integration tests
	// without RBAC-CRUD remain valid.
	var roleH *handlers.RoleHandler
	if deps.RBACSvc != nil {
		roleH = handlers.NewRoleHandler(deps.RBACSvc, logger)
	}

	// synodH is optional: when nil RBACSvc the synod.* routes aren't wired (ADR-049).
	// The same *rbac.Service as roleH (the Synod methods are on it).
	var synodH *handlers.SynodHandler
	if deps.RBACSvc != nil {
		synodH = handlers.NewSynodHandler(deps.RBACSvc, logger)
	}

	// sigilH is optional: when nil SigilSvc the plugin.* routes aren't wired
	// (the production wire-up in `keeper run` passes *sigil.Service). Symmetric to
	// roleH.
	var sigilH *handlers.SigilHandler
	if deps.SigilSvc != nil {
		sigilH = handlers.NewSigilHandler(deps.SigilSvc, logger)
	}

	// sigilKeyH is optional: when nil SigilKeySvc the sigil/keys routes aren't wired
	// (the production wire-up with Sigil enabled passes *sigil.KeyService).
	// Symmetric to sigilH.
	var sigilKeyH *handlers.SigilKeyHandler
	if deps.SigilKeySvc != nil {
		sigilKeyH = handlers.NewSigilKeyHandler(deps.SigilKeySvc, logger)
	}

	// serviceH is optional: when nil ServiceSvc the service.* routes aren't wired
	// (the production wire-up in `keeper run` passes *serviceregistry.Service).
	// Symmetric to roleH / sigilH.
	var serviceH *handlers.ServiceHandler
	if deps.ServiceSvc != nil {
		serviceH = handlers.NewServiceHandler(deps.ServiceSvc, deps.ServiceRefs, deps.ServiceScenarios, deps.ServiceStateSchema, deps.ServiceDependencies, deps.ServiceDirectives, deps.ServiceTelemetry, logger)
	}

	// provisioningPolicyH is optional: GET reads the policy snapshot (Holder), PUT
	// writes it via the same ServiceSvc.SetSetting (+ cluster-invalidate). Both are
	// needed — when either is nil the provisioning-policy routes aren't mounted (ADR-058 Part B).
	var provisioningPolicyH *handlers.ProvisioningPolicyHandler
	if deps.ProvisioningPolicyReader != nil && deps.ServiceSvc != nil {
		provisioningPolicyH = handlers.NewProvisioningPolicyHandler(deps.ProvisioningPolicyReader, deps.ServiceSvc, logger)
	}

	// augurH is optional: when nil AugurSvc the augur.* routes aren't wired
	// (the production wire-up in `keeper run` passes *augur.Service). Symmetric to
	// serviceH.
	var augurH *handlers.AugurHandler
	if deps.AugurSvc != nil {
		augurH = handlers.NewAugurHandler(deps.AugurSvc, logger)
	}

	// oracleH is optional: when nil OracleSvc the vigil.*/decree.* routes aren't
	// wired (the production wire-up passes *oracle.Service). Symmetric to
	// augurH.
	var oracleH *handlers.OracleHandler
	if deps.OracleSvc != nil {
		oracleH = handlers.NewOracleHandler(deps.OracleSvc, logger)
	}

	// pushH is optional: when nil PushRun the push.* routes aren't wired
	// (the production wire-up in `keeper run` passes *pushorch.PushRun if
	// SshDispatcher is configured — see setupPushOrchestrator). Symmetric to
	// oracleH/augurH.
	var pushH *handlers.PushHandler
	if deps.PushRun != nil {
		pushH = handlers.NewPushHandler(deps.PushRun, logger)
	}

	// errandH is optional: when nil ErrandDispatcher / ErrandStore the errand.* routes
	// aren't wired (the pushH/sigilH pattern). The production wire-up passes both
	// references in one wave from setupErrandDispatcher. The constructor is thin: nil
	// dispatcher/store are zeroed in the handler, and with nil errandH the router
	// skips registering the whole routing block (router.go).
	var errandH *handlers.ErrandHandler
	if deps.ErrandDispatcher != nil && deps.ErrandStore != nil {
		errandH = handlers.NewErrandHandler(deps.ErrandDispatcher, deps.ErrandStore, logger)
	}

	// auditH is optional: when nil AuditReader the audit route isn't wired (the
	// errandH/pushH pattern). The production wire-up passes *auditpg.NewReader(pgPool) —
	// the same pool that carries auditWriter.
	var auditH *handlers.AuditHandler
	if deps.AuditReader != nil {
		auditH = handlers.NewAuditHandler(deps.AuditReader, logger)
	}

	// pushProviderH is optional: when nil PushProviderSvc the push-provider.* routes aren't
	// wired (the serviceH/augurH/oracleH pattern).
	var pushProviderH *handlers.PushProviderHandler
	if deps.PushProviderSvc != nil {
		pushProviderH = handlers.NewPushProviderHandler(deps.PushProviderSvc, logger)
	}

	// heraldH is optional: when nil HeraldSvc the herald.*/tiding.* routes aren't wired
	// (the pushProviderH pattern). One handler serves both registries (Herald + Tiding).
	var heraldH *handlers.HeraldHandler
	if deps.HeraldSvc != nil {
		heraldH = handlers.NewHeraldHandler(deps.HeraldSvc, logger)
	}

	// providerH / profileH are optional: when the corresponding Svc is nil the provider.*/
	// profile.* routes aren't wired (the pushProviderH pattern). Cloud-CRUD (ADR-017).
	var providerH *handlers.ProviderHandler
	if deps.ProviderSvc != nil {
		providerH = handlers.NewProviderHandler(deps.ProviderSvc, logger)
	}
	var profileH *handlers.ProfileHandler
	if deps.ProfileSvc != nil {
		profileH = handlers.NewProfileHandler(deps.ProfileSvc, logger)
	}

	// moduleCatalogH is mounted ALWAYS: the core catalog (`GET /v1/modules`) needs
	// no external dependencies (a static doc table). ModuleCatalogPlugins
	// is optional — when nil the plugin section of the catalog is empty.
	moduleCatalogH := handlers.NewModuleCatalogHandler(deps.ModuleCatalogPlugins, logger)

	// permCatalogH is mounted ALWAYS: the RBAC-permissions catalog (`GET /v1/permissions`)
	// — static data from the rbac package, no external dependencies. Auth-only (without
	// RequirePermission, see router.go).
	permCatalogH := handlers.NewPermissionCatalogHandler(logger)

	// eventTypeCatalogH is mounted ALWAYS: the event-types catalog for Tiding subscription
	// (`GET /v1/event-types`, ADR-052(b)) — static data from the herald package (the same source
	// of truth that validates CRUD). Auth-only (without RequirePermission, see router.go).
	eventTypeCatalogH := handlers.NewEventTypeCatalogHandler(logger)

	// heraldTypeCatalogH is mounted ALWAYS: the catalog of Herald-channel types and their
	// config fields (`GET /v1/herald-types`, ADR-052 amendment) — static data from the herald
	// package (the same source that validates CRUD). Auth-only (without RequirePermission).
	heraldTypeCatalogH := handlers.NewHeraldTypeCatalogHandler(logger)

	// meH is mounted ALWAYS: the effective permissions of the current Archon
	// (`GET /v1/me/permissions`) are resolved from the RBAC snapshot (deps.RBAC non-nil
	// guaranteed above). Auth-only (without RequirePermission, see router.go).
	meH := handlers.NewMyPermissionsHandler(deps.RBAC, logger)

	// choirH is optional: when nil ChoirDB the choir.* routes aren't wired (the
	// pushProviderH pattern). AuditWriter — the same one as the incarnation handler
	// (handler-side mutating events choir.created/deleted/voice_*).
	var choirH *handlers.ChoirHandler
	if deps.ChoirDB != nil {
		choirH = handlers.NewChoirHandler(deps.ChoirDB, deps.AuditWriter, logger)
	}

	// voyageH is optional: when nil VoyageDB the voyage.* routes aren't wired (the
	// errandRunH pattern). enforcer (deps.RBAC) — for the in-handler RBAC-by-kind
	// guard (ADR-043 §6); IncarnationDB — the per-incarnation scope check of scenario
	// create. Resolvers — scenario→incarnation names, command→SID snapshot.
	var voyageH *handlers.VoyageHandler
	if deps.VoyageDB != nil {
		voyageH = handlers.NewVoyageHandler(
			deps.VoyageDB,
			deps.VoyageScenarioResolver,
			deps.VoyageCommandResolver,
			deps.IncarnationDB,
			deps.RBAC,
			// scoper: target ∩ Purview command-path (ADR-047 S4). FOOTGUN: the scoper
			// MUST be non-nil in prod — when nil the command path falls back to a cluster-
			// wide resolve (silent scope-leak: a scoped Archon would run a command on
			// someone else's coven). Zeroing this argument is caught by e2e in
			// voyage_scope_integration_test.go (#2/#4/#6 turn red).
			deps.RBAC,
			deps.AuditWriter,
			// tidingInvalidator: the same *herald.Service (single source of truth)
			// that REST/MCP use for Herald/Tiding CRUD. After the commit of the
			// voyage-tx with ephemeral-notify it drops the dispatcher's TTL snapshot
			// (ADR-052(g) race-fix). nil (dev without herald) → no-op, degrading to
			// TTL convergence.
			deps.HeraldSvc,
			deps.VoyageMaxScope,
			deps.VoyageMaxBatchSize,
			logger,
		)
	}

	// cadenceH is optional: when nil CadenceDB the cadence.* routes aren't wired
	// (the voyageH pattern). enforcer (deps.RBAC) — for the two-tier RBAC-by-kind
	// guard (ADR-046 §7); scenarioResolver/IncarnationDB — the per-target coven
	// scope check of a kind=scenario recipe (the same instances as voyageH —
	// security parity of create/patch Cadence ↔ create Voyage); AuditWriter —
	// handler-side mutating events cadence.created/updated/deleted.
	var cadenceH *handlers.CadenceHandler
	if deps.CadenceDB != nil {
		// tidingInvalidator: the same *herald.Service that REST/MCP use for Herald/Tiding
		// CRUD — after the tx-creation of a Cadence with notify rules it drops the
		// dispatcher's TTL snapshot (ADR-052 §m, parity voyageH). nil (dev without herald)
		// → no-op, degrading to TTL convergence.
		cadenceH = handlers.NewCadenceHandler(deps.CadenceDB, deps.VoyageScenarioResolver, deps.IncarnationDB, deps.RBAC, deps.AuditWriter, deps.HeraldSvc, deps.CadencePollFloorSeconds, logger)
	}

	// Tempo voyage-create/preview limits providers: when nil from the caller
	// (unit tests) we degrade to the config defaults — the middleware reads them on every
	// request. preview — a separate bucket with its own defaults (ADR-050
	// amendment 2026-06-17).
	tempoVoyageCreateLimits := deps.TempoVoyageCreateLimits
	if tempoVoyageCreateLimits == nil {
		tempoVoyageCreateLimits = func() apimiddleware.RateLimitLimits {
			rate, burst := config.DefaultTempoVoyageCreateRate, config.DefaultTempoVoyageCreateBurst
			return apimiddleware.RateLimitLimits{Rate: rate, Burst: burst}
		}
	}
	tempoVoyagePreviewLimits := deps.TempoVoyagePreviewLimits
	if tempoVoyagePreviewLimits == nil {
		tempoVoyagePreviewLimits = func() apimiddleware.RateLimitLimits {
			rate, burst := config.DefaultTempoVoyagePreviewRate, config.DefaultTempoVoyagePreviewBurst
			return apimiddleware.RateLimitLimits{Rate: rate, Burst: burst}
		}
	}

	// SSE run-events — opt-in (when nil ApplyBus/pool/RBAC newRunEventsDeps gives nil →
	// the route isn't mounted, the VoyageDB pattern), ADR-068 §A3. Auth for the SSE route — Bearer
	// via the `*/events` chain (fetch-streaming, A0); there is no separate minting endpoint.
	runEventsDeps := newRunEventsDeps(deps.ApplyBus, deps.IncarnationDB, deps.RBAC, logger)

	handler := buildRouter(deps.JWTVerifier, healthH, opH, incH, soulH, telemetryH, roleH, synodH, sigilH, sigilKeyH, serviceH, provisioningPolicyH, augurH, oracleH, pushH, pushProviderH, providerH, profileH, errandH, voyageH, cadenceH, auditH, choirH, heraldH, moduleCatalogH, deps.ModuleFormPrepH, permCatalogH, eventTypeCatalogH, heraldTypeCatalogH, meH, deps.RBAC, deps.AuditWriter, deps.MetricsHTTP, deps.TollDegraded, deps.TempoLimiter, deps.TempoMetrics, tempoVoyageCreateLimits, tempoVoyagePreviewLimits, deps.WebUIEnabled, deps.LDAPAuth, deps.OIDCAuth, deps.AuthToken, deps.AuthMethods, deps.LoginGuard, deps.LoginLimitCfg, deps.SoulStatsStaleFn, clusterH, runEventsDeps, logger)

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    maxHeaderBytes,
	}

	return &Server{
		srv:             srv,
		configAddr:      cfg.Addr,
		operatorHandler: opH,
		addr:            cfg.Addr,
		logger:          logger,
	}, nil
}

// Start binds to addr, begins serving requests, and blocks until
// ctx is cancelled or Serve returns a fatal error. On ctx cancel it does a
// graceful shutdown via [Shutdown].
//
// It uses the "listen first, serve second" pattern: net.Listen is
// synchronous — if the port is taken, the error is returned before the
// goroutine starts. This gives the caller a clear fail-fast on a
// port conflict.
func (s *Server) Start(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.configAddr)
	if err != nil {
		return fmt.Errorf("api: listen %q: %w", s.configAddr, err)
	}
	// The actual address may differ from the requested one (e.g. with
	// `:0` the kernel assigns an ephemeral port — needed for integration tests).
	actual := ln.Addr().String()
	s.mu.Lock()
	s.addr = actual
	s.mu.Unlock()
	s.logger.Info("operator API listening", slog.String("addr", actual))

	errCh := make(chan error, 1)
	go func() {
		if err := s.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case <-ctx.Done():
		s.logger.Info("operator API received shutdown signal")
		shutErr := s.Shutdown(context.Background())
		// After Shutdown the Serve goroutine finishes (returns
		// ErrServerClosed). We wait for it and log a non-standard exit
		// (panic-recovery / accept-loop crash) with a separate WARN,
		// so such a case doesn't dissolve into the logs.
		select {
		case serveErr := <-errCh:
			if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
				s.logger.Warn("operator API Serve returned non-ErrServerClosed after shutdown",
					slog.Any("error", serveErr),
				)
			}
		case <-time.After(2 * time.Second):
			s.logger.Warn("operator API Serve did not exit within 2s after shutdown — leak suspected")
		}
		return shutErr
	case err := <-errCh:
		return err
	}
}

// Addr returns the actual address the server listens on. Before the
// first Start call it returns the cfg value (as-is), after — the
// resolved address (for `:0` — the concrete port).
func (s *Server) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addr
}

// Shutdown initiates a graceful stop with a 10s grace period. The caller usually
// doesn't call this directly — Start does Shutdown itself on ctx.Done().
func (s *Server) Shutdown(ctx context.Context) error {
	shutCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := s.srv.Shutdown(shutCtx); err != nil {
		return fmt.Errorf("api: shutdown: %w", err)
	}
	s.logger.Info("operator API stopped")
	return nil
}
