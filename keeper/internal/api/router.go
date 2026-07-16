package api

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
	"github.com/souls-guild/soul-stack/keeper/internal/api/health"
	apimiddleware "github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/toll"
	"github.com/souls-guild/soul-stack/keeper/internal/webui"
	"github.com/souls-guild/soul-stack/shared/audit"
	"github.com/souls-guild/soul-stack/shared/obs"
)

// buildRouter assembles the Operator API chi router.
//
// Routing:
//
//	GET    /healthz                                  — liveness, no auth.
//	GET    /readyz                                   — readiness (PG+Vault), no auth.
//	GET    /openapi.yaml                             — served huma spec dump (YAML), behind JWT (outside /v1).
//	GET    /openapi.json                             — served huma spec dump (JSON, for /docs), behind JWT.
//	GET    /docs                                     — public RapiDoc viewer (shell, no auth).
//	POST   /v1/operators                             — create Archon (M0.6b).
//	GET    /v1/operators                              — list Archons (UI iter 2).
//	GET    /v1/operators/{aid}                       — get Archon detail (UI iter 2).
//	POST   /v1/operators/{aid}/revoke                — revoke Archon (M0.6b).
//	POST   /v1/operators/{aid}/issue-token           — issue new JWT (M0.6b).
//	GET    /v1/audit                                  — list audit events (UI iter 2).
//	POST   /v1/roles                                 — create role (RBAC Slice 2a).
//	GET    /v1/roles                                  — list roles (RBAC Slice 2a).
//	DELETE /v1/roles/{name}                          — delete role (RBAC Slice 2a).
//	PATCH  /v1/roles/{name}/permissions              — replace permissions (RBAC Slice 2a).
//	POST   /v1/roles/{name}/operators                — grant operator (RBAC Slice 2a).
//	DELETE /v1/roles/{name}/operators/{aid}          — revoke operator (RBAC Slice 2a).
//	POST   /v1/synods                                — create synod (ADR-049).
//	GET    /v1/synods                                — list synods (ADR-049).
//	PATCH  /v1/synods/{name}                         — update synod description (ADR-049).
//	DELETE /v1/synods/{name}                         — delete synod (ADR-049).
//	POST   /v1/synods/{name}/operators               — add operator (ADR-049).
//	DELETE /v1/synods/{name}/operators/{aid}         — remove operator (ADR-049).
//	POST   /v1/synods/{name}/roles                   — grant role (ADR-049).
//	DELETE /v1/synods/{name}/roles/{role_name}       — revoke role (ADR-049).
//	POST   /v1/incarnations                          — create incarnation, stub (M0.6c-1).
//	GET    /v1/incarnations                          — list incarnations (M0.6c-1).
//	GET    /v1/incarnations/{name}                   — get incarnation (M0.6c-1).
//	GET    /v1/incarnations/{name}/history           — state_history (M0.6c-1).
//	POST   /v1/incarnations/{name}/scenarios/{scenario} — run named scenario (M0.6c).
//	POST   /v1/incarnations/{name}/scenarios/{scenario}/form-prefill — day-2 form prefill from state (docs/input.md).
//	POST   /v1/incarnations/{name}/unlock            — clear error_locked (M0.6c).
//	POST   /v1/incarnations/{name}/upgrade           — migrate state_schema_version (ADR-019).
//	GET    /v1/incarnations/{name}/upgrade-paths     — upgrade paths: tags + on-demand ?to= (ADR-0068 §6).
//	DELETE /v1/incarnations/{name}                   — destroy incarnation (S-D4).
//	PATCH  /v1/incarnations/{name}/hosts             — edit declared spec.hosts[] (ADR-008).
//	PUT    /v1/incarnations/{name}/traits            — replace operator-set trait labels (ADR-060).
//	POST   /v1/voyages                               — create Voyage (ADR-043 S5, RBAC-by-kind).
//	POST   /v1/voyages/preview                       — dry-resolve scope without creating a Voyage (ADR-043 amendment §4).
//	GET    /v1/voyages                                — list Voyage runs (ADR-043 S5).
//	GET    /v1/voyages/{id}                          — snapshot Voyage (ADR-043 S5).
//	GET    /v1/voyages/{id}/targets                  — All-runs drill (ADR-043 S5).
//	DELETE /v1/voyages/{id}                          — cancel pending/scheduled Voyage (ADR-043 S5).
//	POST   /v1/cadences                              — create Cadence (ADR-046 S4, two-level RBAC-by-kind).
//	GET    /v1/cadences                              — list Cadence schedules (ADR-046 S4).
//	GET    /v1/cadences/{id}                         — Cadence detail (ADR-046 S4).
//	PATCH  /v1/cadences/{id}                         — update Cadence (ADR-046 S4).
//	DELETE /v1/cadences/{id}                         — remove Cadence (ADR-046 S4).
//	POST   /v1/cadences/{id}/enable                  — enable Cadence (ADR-046 S4).
//	POST   /v1/cadences/{id}/disable                 — disable Cadence (ADR-046 S4).
//	GET    /v1/cadences/{id}/runs                    — child Voyages of a Cadence (ADR-046 S4).
//	GET    /v1/push-runs                              — global list of push runs (UI-4).
//	POST   /v1/souls                                 — register soul + token.
//	GET    /v1/souls                                  — list souls (filters: coven/status/transport).
//	GET    /v1/souls/{sid}                           — get one soul (detail-page).
//	GET    /v1/souls/{sid}/soulprint                 — last typed-Soulprint (ADR-018).
//	GET    /v1/souls/{sid}/history                   — per-host operation timeline (scenario+errand).
//	POST   /v1/souls/{sid}/issue-token               — reissue bootstrap token.
//	POST   /v1/plugins/sigils                        — allow plugin Sigil (ADR-026 S4a).
//	GET    /v1/plugins/sigils                        — list active Sigils (ADR-026 S4a).
//	DELETE /v1/plugins/sigils/{namespace}/{name}/{ref} — revoke Sigil (ADR-026 S4a).
//	POST   /v1/sigil/keys                            — introduce signing key (ADR-026(h) R3-S7).
//	GET    /v1/sigil/keys                             — list active signing keys (R3-S7).
//	POST   /v1/sigil/keys/{key_id}/primary           — set primary signing key (R3-S7).
//	DELETE /v1/sigil/keys/{key_id}                   — retire signing key (R3-S7).
//	POST   /v1/services                              — register Service (ADR-028 S3).
//	GET    /v1/services                               — list Services (ADR-028 S3).
//	GET    /v1/services/{name}                       — get Service (ADR-028 S3).
//	PATCH  /v1/services/{name}                       — update Service (ADR-028 S3).
//	DELETE /v1/services/{name}                       — deregister Service (ADR-028 S3).
//	GET    /v1/services/{name}/refs                  — list git-tags + branches (UI upgrade-modal).
//	GET    /v1/services/{name}/dependencies          — destiny/module git-refs (UI Service Detail).
//	POST   /v1/augur/omens                           — create Omen (ADR-025).
//	GET    /v1/augur/omens                            — list Omens (ADR-025).
//	GET    /v1/augur/omens/{name}                    — get Omen (ADR-025).
//	DELETE /v1/augur/omens/{name}                    — delete Omen (ADR-025).
//	POST   /v1/augur/rites                           — create Rite (ADR-025).
//	GET    /v1/augur/rites                            — list Rites by omen (ADR-025).
//	DELETE /v1/augur/rites/{id}                      — delete Rite (ADR-025).
//	POST   /v1/vigils                                — create Vigil (ADR-030).
//	GET    /v1/vigils                                 — list Vigils (ADR-030).
//	GET    /v1/vigils/{name}                         — get Vigil (ADR-030).
//	DELETE /v1/vigils/{name}                         — delete Vigil (ADR-030).
//	POST   /v1/decrees                               — create Decree (ADR-030).
//	GET    /v1/decrees                                — list Decrees (ADR-030).
//	GET    /v1/decrees/{name}                        — get Decree (ADR-030).
//	DELETE /v1/decrees/{name}                        — delete Decree (ADR-030).
//	POST   /v1/push-providers                        — create Push-Provider (ADR-032 amend S7-2).
//	GET    /v1/push-providers                         — list Push-Providers (S7-2).
//	GET    /v1/push-providers/{name}                 — read Push-Provider (S7-2).
//	PUT    /v1/push-providers/{name}                 — update Push-Provider (S7-2).
//	DELETE /v1/push-providers/{name}                 — delete Push-Provider (S7-2).
//	POST   /v1/providers                             — create Cloud-Provider (ADR-017).
//	GET    /v1/providers                             — list Cloud-Providers (ADR-017).
//	GET    /v1/providers/{name}                      — read Cloud-Provider (ADR-017).
//	DELETE /v1/providers/{name}                      — delete Cloud-Provider (ADR-017).
//	POST   /v1/profiles                              — create Cloud-Profile (ADR-017).
//	GET    /v1/profiles                              — list Cloud-Profiles (ADR-017).
//	GET    /v1/profiles/{name}                       — read Cloud-Profile (ADR-017).
//	DELETE /v1/profiles/{name}                       — delete Cloud-Profile (ADR-017).
//	POST   /v1/modules/{name}/form-prep              — resolver of source catalogs for the module UI form (ADR-045 S3).
//	GET    /v1/permissions                           — catalog of RBAC permissions (auth-only, fixes UI hardcode).
//	GET    /v1/event-types                           — catalog of event-types for Tiding subscription (auth-only, fixes UI hardcode).
//	GET    /v1/herald-types                          — catalog of Herald channel types and config fields (auth-only, fixes UI hardcode).
//	GET    /v1/me/permissions                        — effective permissions of the current Archon (auth-only, permission-aware UI).
//	/v1/*                                            — catch-all 404 behind the auth chain.
//
// tempoBucketVoyageCreate / tempoBucketVoyagePreview — logical names of the
// Tempo buckets for the resolver-heavy voyage-write paths (ADR-050(c) + amendment
// 2026-06-17). They match the metric label `endpoint` and the config keys
// `tempo.voyage_create` / `tempo.voyage_preview`.
//
// SEPARATE bucket keys (per-AID Redis key `tempo:<aid>:<bucket>`): preview
// and create do NOT share a quota — exhausting one does not 429 the other. Before the
// amendment preview reused voyage_create (a single limit), but preview is read-like in
// effect (no persist/audit) and deserves a softer limit of its own, while still being
// resolver-heavy → not unlimited.
const (
	tempoBucketVoyageCreate  = "voyage_create"
	tempoBucketVoyagePreview = "voyage_preview"
)

// Health/meta are placed outside `/v1/*` per operator-api.md § Health / Meta.
// chi.NotFound and chi.MethodNotAllowed are replaced with problem+json handlers,
// so 404/405 do not arrive in the stdlib default text/plain format.
func buildRouter(verifier *jwt.Verifier, healthH *health.Handler, opH *handlers.OperatorHandler, incH *handlers.IncarnationHandler, soulH *handlers.SoulHandler, roleH *handlers.RoleHandler, synodH *handlers.SynodHandler, sigilH *handlers.SigilHandler, sigilKeyH *handlers.SigilKeyHandler, serviceH *handlers.ServiceHandler, provisioningPolicyH *handlers.ProvisioningPolicyHandler, augurH *handlers.AugurHandler, oracleH *handlers.OracleHandler, pushH *handlers.PushHandler, pushProviderH *handlers.PushProviderHandler, providerH *handlers.ProviderHandler, profileH *handlers.ProfileHandler, errandH *handlers.ErrandHandler, voyageH *handlers.VoyageHandler, cadenceH *handlers.CadenceHandler, auditH *handlers.AuditHandler, choirH *handlers.ChoirHandler, heraldH *handlers.HeraldHandler, moduleCatalogH *handlers.ModuleCatalogHandler, moduleFormPrepH *handlers.ModuleFormPrepHandler, permCatalogH *handlers.PermissionCatalogHandler, eventTypeCatalogH *handlers.EventTypeCatalogHandler, heraldTypeCatalogH *handlers.HeraldTypeCatalogHandler, meH *handlers.MyPermissionsHandler, enforcer RBACProvider, auditWriter audit.Writer, metricsHTTP *obs.HTTPMetrics, tollDegraded toll.DegradedReader, tempoLimiter apimiddleware.RateLimiter, tempoMetrics apimiddleware.RateLimitMetrics, tempoVoyageCreateLimits func() apimiddleware.RateLimitLimits, tempoVoyagePreviewLimits func() apimiddleware.RateLimitLimits, webUIEnabled bool, ldapAuth *LDAPAuthDeps, oidcAuth *OIDCAuthDeps, loginGuard apimiddleware.LoginGuard, loginLimitCfg apimiddleware.AuthLoginLimitConfig, soulStatsStaleFn func() time.Duration, clusterH *handlers.ClusterHandler, runEventsDeps *runEventsDeps, logger *slog.Logger) http.Handler {
	r := chi.NewRouter()

	// huma error-override (ADR-054, FULL-TYPED): global huma.NewError →
	// our problem+json. The SINGLE install POINT is here, at router assembly (not in
	// each huma.API factory): one install for the ~20-domain rollout, not per domain.
	installHumaErrorOverride()

	r.NotFound(func(w http.ResponseWriter, req *http.Request) {
		apimiddleware.WriteNotFound(w, req, "no such endpoint")
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, req *http.Request) {
		// chi automatically filters methods for routes that have registered
		// handlers; for POST-only /v1/operators a GET → 405. We do not set the
		// Allow header (chi does not provide the list of allowed methods itself);
		// omitting it is permitted by RFC 7231.
		apimiddleware.Write405(w, req)
	})

	// Health / Meta / Docs — outside /v1.
	//
	// `/metrics` is NOT mounted here: the Prometheus endpoint lives on a
	// dedicated listener (`listen.metrics.addr`, ADR-024, see
	// keeper/cmd/keeper) with optional basic-auth. The keeper_http_* metrics still
	// remain — collected by the middleware on /v1/* (below) and exposed by the
	// same *obs.Registry on the metrics listener.
	//
	// SECURITY (mechanism A, ADR-054 doc-viewer):
	//   - /healthz, /readyz — PUBLIC (liveness/readiness, not written to audit).
	//   - /docs + /docs/assets/* — PUBLIC shell + RapiDoc static (they carry no
	//     API data/description; the sensitive part arrives only after fetching the spec
	//     behind JWT). See docs_viewer.go.
	//   - /openapi.yaml + /openapi.json — BEHIND JWT. /openapi.yaml used to be
	//     public but exposed the full API surface to everyone; now both
	//     require Bearer (the same RequireJWT as /v1), but WITHOUT the /v1 wiring
	//     (maxBody/metrics/audit/RBAC): the spec is static, mounted OUTSIDE /v1. The
	//     /docs page fetches .json with a Bearer header (RapiDoc renders the object inline).
	//
	// /openapi.yaml and /openapi.json serve the runtime dump of the huma aggregator (3.1,
	// "truth in code") from a SINGLE source-of-truth (servedOpenAPIHandler /
	// servedOpenAPIJSONHandler) — the cache is built once. YAML for humans/tools,
	// JSON for the /docs viewer. The committed docs/keeper/openapi.yaml is a derived
	// huma artifact for the UI vendor (make gen-openapi); it is NOT served and NOT embedded.
	r.Get("/healthz", healthH.Healthz)
	r.Get("/readyz", healthH.Readyz)
	r.With(apimiddleware.RequireJWT(verifier)).Get("/openapi.yaml", servedOpenAPIHandler)
	r.With(apimiddleware.RequireJWT(verifier)).Get("/openapi.json", servedOpenAPIJSONHandler)
	mountDocsViewer(r)

	// /ui — the embedded UI (ADR-055), a public mount OUTSIDE /v1 (parity with /docs):
	// go:embed static without JWT/RBAC/audit; the API is protected, not the static. Mounted
	// ONLY when the web_ui_enabled toggle is on (default-ON, resolved via
	// [config.KeeperConfig.WebUIMounted]); with an explicit `false` /ui is not attached
	// → 404 (the /v1 API perimeter is not affected in any case).
	if webUIEnabled {
		webui.Mount(r)
	}

	// /auth/* — federated authentication (ADR-058) OUTSIDE /v1: public login
	// (the login itself, no JWT yet — RequireJWT does not apply, parity with /healthz). Mounted
	// when ldapAuth is non-nil (POST /auth/ldap/login) AND/OR oidcAuth is non-nil
	// (GET /auth/oidc/{login,callback}); otherwise the login method is unavailable (ADR-053
	// OPTIONAL tier). The anti-DoS body-limit is in place (credentials/callback-query are small),
	// but without metrics/RBAC/audit-middleware (the /v1 wiring): the login audit is written by the
	// handler itself (operator.login).
	//
	// ANTI-BRUTEFORCE (ADR-058(g), HIGH-3): AuthLoginLimit per-IP+per-username
	// throttle + lockout (loginGuard, fail-closed on lockout, fail-open on throttle).
	// loginGuard=nil (no Redis) → passthrough. Each method has its OWN chi subgroup,
	// to attach a different username extractor (LDAP — from the body; OIDC — none) and
	// a shared guard; separate huma.API dump targets in fullSpecGroups.
	if ldapAuth != nil || oidcAuth != nil {
		r.Route("/auth", func(r chi.Router) {
			r.Use(maxBodyMiddleware(v1RequestBodyLimit))
			if ldapAuth != nil {
				// LDAP: throttle+lockout with per-username (from the JSON body) + recording
				// of failures (401/403). Its own chi group under the middleware.
				r.Group(func(r chi.Router) {
					r.Use(apimiddleware.AuthLoginLimit(loginGuard, loginLimitCfg, apimiddleware.LDAPUsernameExtractor, true, logger))
					registerHumaLDAPLogin(newHumaAuthAPI(r), ldapAuth)
				})
			}
			if oidcAuth != nil {
				// OIDC: throttle+lockout per-IP (the username comes from the IdP, not from
				// the request → extractUsername=nil). recordFailures=true: on /login
				// (302) there is no failure (isAuthFailure(302)=false → no-op), on /callback
				// a 401/403 increments the counter. The /login throttle dampens the flow-state flood.
				r.Group(func(r chi.Router) {
					r.Use(apimiddleware.AuthLoginLimit(loginGuard, loginLimitCfg, nil, true, logger))
					registerHumaOIDCLogin(newHumaAuthAPI(r), oidcAuth)
				})
			}
		})
	}

	// /v1/* — auth + RBAC + audit. The selector-extractor for operator
	// endpoints is NoSelector (rbac.md does not define selectors for
	// permission `operator.*`).
	r.Route("/v1", func(r chi.Router) {
		// Anti-DoS: a limit on the Request body. Operator endpoints are JSON
		// of ~200 bytes; we set v1RequestBodyLimit with headroom and
		// at the same time cut off "I'll send a gigabyte of junk".
		// on exceeding the limit MaxBytesReader substitutes Read with
		// http.MaxBytesError; json.Decoder receives it and the handler returns
		// 400 problem+json (TypeMalformedRequest).
		r.Use(maxBodyMiddleware(v1RequestBodyLimit))
		// HTTP metrics — inside /v1, so chi already knows the RoutePattern
		// (without it the `path` label = raw URL → cardinality blow-up). The path
		// extractor reads chi.RouteContext, filled by the chi router
		// after the match; for the catch-all `/v1/*` below the RoutePattern will be
		// `/v1/*` — that is acceptable (cardinality is stable).
		if metricsHTTP != nil {
			r.Use(metricsHTTP.MiddlewareForPath(routePatternFromChi))
		}
		r.Use(apimiddleware.RequireJWT(verifier))

		// FULL-TYPED huma (ADR-054, ROLLOUT BATCH 2a of the entire operator domain over 5
		// references): create/revoke/issue-token — WRITE+AUDIT variant B (huma-audit-
		// middleware: full-typed huma writes the response ITSELF, the StatusRecorder from
		// apimiddleware.Audit does not apply — audit holds hctx.Status() + a carrier
		// payload, otherwise an S6 relapse); list — read with typed query (no audit, bad
		// auth_method enum→422, revoked bool→400, pagination int32→400); get —
		// read with path. Each write route has its OWN chi group with its own event
		// type (newHumaOperatorAPI(evt)). RequirePermission is the group chi-middleware
		// (huma inherits it). huma serves all operator routes. MCP operator-tools
		// call operator.Service directly (bypassing the handler).
		r.Route("/operators", func(r chi.Router) {
			r.With(
				apimiddleware.RequirePermission(enforcer, "operator", "create", apimiddleware.NoSelector),
			).Group(func(r chi.Router) {
				registerHumaOperatorCreate(newHumaOperatorAPI(r, auditWriter, audit.EventOperatorCreated, logger), opH)
			})

			// GET /v1/operators — list (UI iteration 2 /archons-list).
			// Permission operator.list, NoSelector. Read-only — no audit.
			r.With(
				apimiddleware.RequirePermission(enforcer, "operator", "list", apimiddleware.NoSelector),
			).Group(func(r chi.Router) {
				registerHumaOperatorList(newHumaCadenceAPI(r), opH)
			})

			// GET /v1/operators/{aid} — detail. Permission operator.list (one
			// permission covers list+get, the soul.list/service.list pattern — read
			// without a separate operator.read in MVP). Read-only — no audit. The huma op
			// carries the full path /{aid} (NOT nested in r.Route("/{aid}") — otherwise chi
			// would double the prefix).
			r.With(
				apimiddleware.RequirePermission(enforcer, "operator", "list", apimiddleware.NoSelector),
			).Group(func(r chi.Router) {
				registerHumaOperatorGet(newHumaCadenceAPI(r), opH)
			})

			r.With(
				apimiddleware.RequirePermission(enforcer, "operator", "revoke", apimiddleware.NoSelector),
			).Group(func(r chi.Router) {
				registerHumaOperatorRevoke(newHumaOperatorAPI(r, auditWriter, audit.EventOperatorRevoked, logger), opH)
			})

			r.With(
				apimiddleware.RequirePermission(enforcer, "operator", "issue-token", apimiddleware.NoSelector),
			).Group(func(r chi.Router) {
				registerHumaOperatorIssueToken(newHumaOperatorAPI(r, auditWriter, audit.EventOperatorTokenIssued, logger), opH)
			})
		})

		// /v1/audit — read-only feed of audit events for UI iteration 2 (/audit).
		// Permission audit.read, NoSelector. Read without Audit-middleware (avoids
		// recursion: every read would write its own record into audit_log).
		// Mounted ONLY when auditH is non-nil (the pushH/errandH pattern); the
		// drift-test assembles the router with auditH=nil → the route lands in pathAllowlist.
		//
		// FULL-TYPED huma (ADR-054 §Pattern FOURTH tier — read-with-typed-query,
		// the REFERENCE for ~13-15 list endpoints): huma binds/validates the typed query →
		// ListTyped → typed envelope-output. READ variant (WITHOUT huma-audit-middleware).
		// Contract preserved (decision A, continuation of ADR-051 Amendment): bad date-time/
		// offset/limit query → 400 TypeMalformedRequest (error-override
		// hasQueryParseError); bad source-enum → 422 TypeValidationFailed (schema-
		// validate enum-mismatch, not parse). audit is served by huma.
		// RequirePermission(audit.read) — the group chi-middleware (huma inherits it).
		if auditH != nil {
			r.With(
				apimiddleware.RequirePermission(enforcer, "audit", "read", apimiddleware.NoSelector),
			).Group(func(r chi.Router) {
				registerHumaAuditList(newHumaCadenceAPI(r), auditH)
			})
		}

		// /v1/roles — RBAC CRUD (roles / permissions / membership), Slice 2a.
		// Mounted only when roleH is non-nil (Deps.RBACSvc wired in).
		// The selector-extractor is NoSelector: rbac.md does not define selectors for
		// the `role.*` permission (nor for `operator.*`).
		//
		// FULL-TYPED huma (ADR-054, the FIRST ROLLOUT BATCH of the entire domain over the two
		// references pilot-1/pilot-2): ALL role routes on huma. READ (list) — per the
		// pilot-1 READ variant (typed output, no audit). WRITE (create/delete/
		// update-permissions/grant/revoke-operator) — per pilot-2 (typed I/O +
		// huma-audit-middleware variant B: full-typed huma writes the response ITSELF, so
		// the StatusRecorder from apimiddleware.Audit does not apply — audit holds
		// humaAuditMiddleware, which reads hctx.Status() + a carrier payload, otherwise
		// an S6 relapse). Each write route has its OWN chi group with its own event type
		// (newHumaRoleAPI(evt)). RequirePermission is the group chi-middleware (huma
		// inherits it). huma serves all role routes.
		if roleH != nil {
			r.Route("/roles", func(r chi.Router) {
				r.With(
					apimiddleware.RequirePermission(enforcer, "role", "create", apimiddleware.NoSelector),
				).Group(func(r chi.Router) {
					registerHumaRole(newHumaRoleAPI(r, auditWriter, audit.EventRoleCreated, logger), roleH)
				})

				// GET /v1/roles — READ, no audit (the role.list pattern).
				r.With(
					apimiddleware.RequirePermission(enforcer, "role", "list", apimiddleware.NoSelector),
				).Group(func(r chi.Router) {
					registerHumaRoleList(newHumaCadenceAPI(r), roleH)
				})

				r.With(
					apimiddleware.RequirePermission(enforcer, "role", "delete", apimiddleware.NoSelector),
				).Group(func(r chi.Router) {
					registerHumaRoleDelete(newHumaRoleAPI(r, auditWriter, audit.EventRoleDeleted, logger), roleH)
				})

				r.With(
					apimiddleware.RequirePermission(enforcer, "role", "update", apimiddleware.NoSelector),
				).Group(func(r chi.Router) {
					registerHumaRoleUpdatePermissions(newHumaRoleAPI(r, auditWriter, audit.EventRolePermissionsUpdated, logger), roleH)
				})

				r.With(
					apimiddleware.RequirePermission(enforcer, "role", "grant-operator", apimiddleware.NoSelector),
				).Group(func(r chi.Router) {
					registerHumaRoleGrantOperator(newHumaRoleAPI(r, auditWriter, audit.EventRoleOperatorGranted, logger), roleH)
				})

				r.With(
					apimiddleware.RequirePermission(enforcer, "role", "revoke-operator", apimiddleware.NoSelector),
				).Group(func(r chi.Router) {
					registerHumaRoleRevokeOperator(newHumaRoleAPI(r, auditWriter, audit.EventRoleOperatorRevoked, logger), roleH)
				})
			})
		}

		// /v1/synods — Synod CRUD (groups / membership / bundle), ADR-049.
		// Mounted only when synodH is non-nil (Deps.RBACSvc wired in).
		// Selector — NoSelector: synod.* is a cluster-level operation with no scope
		// by coven/host (like role.* / operator.*; ADR-049 does NOT introduce group-scope).
		//
		// Audit-middleware on the 7 mutating routes (the RBAC topology is audited,
		// ADR-022). `synod.list` — read-only, no audit. The business logic
		// (builtin boundary, least-privilege subset on add-operator/grant-role,
		// self-lockout on delete/remove-operator/revoke-role) — in rbac.Service.
		//
		// FULL-TYPED huma (ADR-054, ROLLOUT BATCH 2d of the entire synod domain over the
		// role/operator/augur/herald): create/update/delete + add/remove-operator +
		// grant/revoke-role — WRITE+AUDIT variant B (huma-audit-middleware: full-typed
		// huma writes the response ITSELF, the StatusRecorder from apimiddleware.Audit does not apply —
		// audit holds hctx.Status() + a carrier payload, otherwise an S6 relapse); list —
		// read (no audit). Sub-resource routes (/operators, /roles[/...]) carry the full
		// path in the huma operation (the role-domain form: a single resource-group). Each
		// write route has its OWN chi group with its own event type (newHumaSynodAPI(evt)).
		// RequirePermission is the group chi-middleware (huma inherits it). huma serves all synod
		// routes. MCP synod-tools call rbac.Service directly (bypassing the handler).
		if synodH != nil {
			r.Route("/synods", func(r chi.Router) {
				r.With(
					apimiddleware.RequirePermission(enforcer, "synod", "create", apimiddleware.NoSelector),
				).Group(func(r chi.Router) {
					registerHumaSynodCreate(newHumaSynodAPI(r, auditWriter, audit.EventSynodCreated, logger), synodH)
				})

				r.With(
					apimiddleware.RequirePermission(enforcer, "synod", "list", apimiddleware.NoSelector),
				).Group(func(r chi.Router) {
					registerHumaSynodList(newHumaCadenceAPI(r), synodH)
				})

				r.With(
					apimiddleware.RequirePermission(enforcer, "synod", "update", apimiddleware.NoSelector),
				).Group(func(r chi.Router) {
					registerHumaSynodUpdate(newHumaSynodAPI(r, auditWriter, audit.EventSynodUpdated, logger), synodH)
				})

				r.With(
					apimiddleware.RequirePermission(enforcer, "synod", "delete", apimiddleware.NoSelector),
				).Group(func(r chi.Router) {
					registerHumaSynodDelete(newHumaSynodAPI(r, auditWriter, audit.EventSynodDeleted, logger), synodH)
				})

				r.With(
					apimiddleware.RequirePermission(enforcer, "synod", "add-operator", apimiddleware.NoSelector),
				).Group(func(r chi.Router) {
					registerHumaSynodAddOperator(newHumaSynodAPI(r, auditWriter, audit.EventSynodOperatorAdded, logger), synodH)
				})

				r.With(
					apimiddleware.RequirePermission(enforcer, "synod", "remove-operator", apimiddleware.NoSelector),
				).Group(func(r chi.Router) {
					registerHumaSynodRemoveOperator(newHumaSynodAPI(r, auditWriter, audit.EventSynodOperatorRemoved, logger), synodH)
				})

				r.With(
					apimiddleware.RequirePermission(enforcer, "synod", "grant-role", apimiddleware.NoSelector),
				).Group(func(r chi.Router) {
					registerHumaSynodGrantRole(newHumaSynodAPI(r, auditWriter, audit.EventSynodRoleGranted, logger), synodH)
				})

				r.With(
					apimiddleware.RequirePermission(enforcer, "synod", "revoke-role", apimiddleware.NoSelector),
				).Group(func(r chi.Router) {
					registerHumaSynodRevokeRole(newHumaSynodAPI(r, auditWriter, audit.EventSynodRoleRevoked, logger), synodH)
				})
			})
		}

		// /v1/incarnations — Create + Get + List + History + Run/Unlock/Upgrade/Destroy.
		//
		// RBAC selector strategy (ADR-008 amendment a + ADR-047 §d):
		//   - List/Get/History — [RequireAction] existence-gate (ADR-047 §d):
		//     the scope-aware [RequirePermission]/[RequirePermissionMulti] denies a
		//     scoped operator when the selector dimension does NOT resolve in the
		//     request context (state/regex/soulprint are not extracted from the
		//     incarnation row at all; coven-scoped matches, but state-scoped does not),
		//     cutting the operator off from their own visibility BEFORE the handler. RequireAction
		//     only asks whether the permission EXISTS (`incarnation.{list,get,history}`);
		//     the scope narrowing is done by the handler after fetching the row
		//     (ResolveListScopeFor for list, GetInScopeFor for get/history —
		//     coven∪{name} + state-CEL). Revoked coverage — via the same
		//     revoked-aware [rbac.Enforcer.ResolvePurview] (gate HoldsAction→Deny
		//     →403, handler Deny→Empty→404).
		//   - Create — [handlers.IncarnationCreateScopeSelector]: scope from the body
		//     (service= + multi-value coven= from declared covens ∪ {name}) —
		//     a coven-scoped operator cannot create an incarnation with a tag outside its scope.
		//   - Run/Unlock/Upgrade/Destroy/… — [handlers.IncarnationScopeSelector]
		//     (multi-context): reads the incarnation by path-{name} and lands
		//     incarnation= + service= + multi-value coven= (covens ∪ {name}).
		//     RequirePermissionMulti ORs the contexts — roles `incarnation.* on
		//     coven=…` / `on service=…` match (mutate routes have no state-scoped
		//     read hole: their scope in MVP is coven/service/incarnation, not state).
		//
		// FULL-TYPED huma (ADR-054, ROLLOUT BATCH 2g of the ENTIRE incarnation domain — MIXED
		// audit class): create/run/unlock/upgrade — WRITE-MIDDLEWARE-AUDIT variant B
		// (newHumaIncarnationAPI(evt) — huma writes the response ITSELF, audit holds hctx.Status()
		// + a carrier payload from *Typed-reply.AuditPayload, otherwise an S6 relapse); rerun-last/
		// check-drift/destroy/update-hosts — WRITE-SELF-AUDIT (audit is written by the handler ITSELF
		// INSIDE *Typed via h.auditW.Write — the payload is assembled after the domain operation;
		// audit-middleware is NOT wired, newHumaCadenceAPI); list/get/history — read (no
		// audit). TOPOLOGY: chi.Route("/{name}") is REMOVED — all incarnation ops carry the FULL
		// path /{name}[/...] on the /v1/incarnations group (otherwise sibling-shadowing of the
		// /{name} node → 405, the blocker of batch-2f). Coexists with the choir-mount (batch-2f) on the SAME
		// group. Each write route has its OWN chi group with its own RBAC/event (Toll on run);
		// huma inherits the chi-middleware. MCP incarnation-tools call the incarnation.*
		// domain directly (bypassing the handler) — integrity preserved.
		incScope := handlers.IncarnationScopeSelector(incH.ContextReader())
		r.Route("/incarnations", func(r chi.Router) {
			r.With(
				apimiddleware.RequirePermissionMulti(enforcer, "incarnation", "create", handlers.IncarnationCreateScopeSelector),
			).Group(func(r chi.Router) {
				registerHumaIncarnationCreate(newHumaIncarnationAPI(r, auditWriter, audit.EventIncarnationCreated, logger), incH)
			})

			r.With(
				stashRawQuery,
				apimiddleware.RequireAction(enforcer, "incarnation", "list"),
			).Group(func(r chi.Router) {
				registerHumaIncarnationList(newHumaCadenceAPI(r), incH)
			})

			r.With(
				apimiddleware.RequireAction(enforcer, "incarnation", "get"),
			).Group(func(r chi.Router) {
				registerHumaIncarnationGet(newHumaCadenceAPI(r), incH)
			})

			// POST /v1/incarnations/{name}/scenarios/{scenario}/form-prefill — day-2
			// pre-fill of the scenario UI form from incarnation.state (docs/input.md). A READ
			// resolve (not a mutation): audit is NOT wired, newHumaCadenceAPI. Permission
			// incarnation.get (reuse: whoever reads the incarnation also gets the prefill of its
			// form); per-{name} scope — the in-handler inScope predicate (GetInScopeFor,
			// action=get), as in Get/History. The path-whitelist and secret exclusion —
			// are inside FormPrefillTyped.
			r.With(
				apimiddleware.RequireAction(enforcer, "incarnation", "get"),
			).Group(func(r chi.Router) {
				registerHumaIncarnationFormPrefill(newHumaCadenceAPI(r), incH)
			})

			// POST /v1/incarnations/{name}/secrets/reveal — reveal a plaintext secret
			// (NIM-74). WRITE-SELF-AUDIT: incarnation.secret_revealed is written by the handler itself
			// inside RevealSecretTyped AFTER ReadKV (the value is NOT in the payload; audit-middleware
			// is NOT wired, newHumaCadenceAPI). Permission incarnation.view-secrets (unmasking,
			// more privileged than incarnation.get), scope incScope.
			r.With(
				apimiddleware.RequirePermissionMulti(enforcer, "incarnation", "view-secrets", incScope),
			).Group(func(r chi.Router) {
				registerHumaIncarnationRevealSecret(newHumaCadenceAPI(r), incH)
			})

			// GET /v1/incarnations/{name}/secrets/revealable — discovery of revealable
			// secrets + keys from state (NIM-74). READ (no audit, newHumaCadenceAPI).
			// Existence-gate RequireAction(view-secrets); per-{name} scope — in-handler
			// inScope (GetInScopeFor, action=view-secrets), as get/form-prefill.
			r.With(
				apimiddleware.RequireAction(enforcer, "incarnation", "view-secrets"),
			).Group(func(r chi.Router) {
				registerHumaIncarnationRevealableSecrets(newHumaCadenceAPI(r), incH)
			})

			// GET /v1/incarnations/{name}/upgrade-paths — read analysis of upgrade paths
			// (ADR-0068 §6): a cheap list of registry tags + on-demand ?to= per-target.
			// READ (no audit, newHumaCadenceAPI). Permission incarnation.upgrade (the read
			// facet, the same as POST .../upgrade); existence-gate RequireAction(action=
			// upgrade), per-{name} scope — in-handler inScope (GetInScopeFor, action=
			// upgrade), as get/form-prefill.
			r.With(
				apimiddleware.RequireAction(enforcer, "incarnation", "upgrade"),
			).Group(func(r chi.Router) {
				registerHumaIncarnationUpgradePaths(newHumaCadenceAPI(r), incH)
			})

			r.With(
				apimiddleware.RequireAction(enforcer, "incarnation", "history"),
			).Group(func(r chi.Router) {
				registerHumaIncarnationHistory(newHumaCadenceAPI(r), incH)
			})

			// GET /v1/incarnations/{name}/runs[/{apply_id}] — read-view of runs
			// (apply_runs), for the UI "execution status / current job". A run
			// (apply_run) is NOT a Voyage: closes the UI bug apply_id→/voyages/ 404. READ
			// (no audit, newHumaCadenceAPI). Permission incarnation.history (reuse of the
			// read-tier: whoever sees the incarnation history also sees its runs); per-
			// {name} scope — the in-handler inScope predicate (GetInScopeFor, action=history),
			// as in History; a WHERE on incarnation_name in the store layer cuts off runs
			// of another incarnation (cross-incarnation apply_id → 404).
			r.With(
				apimiddleware.RequireAction(enforcer, "incarnation", "history"),
			).Group(func(r chi.Router) {
				registerHumaIncarnationRuns(newHumaCadenceAPI(r), incH)
				registerHumaIncarnationRunDetail(newHumaCadenceAPI(r), incH)
				// GET .../runs/{apply_id}/tasks — the run's task plan + per-host
				// results from audit (NIM-37). The same incarnation.history group/scope
				// as RunDetail (in-handler inScope, action=history).
				registerHumaIncarnationRunTasks(newHumaCadenceAPI(r), incH)
			})

			// GET /v1/incarnations/{name}/runs/{apply_id}/events — live SSE of a run
			// (ADR-068 §A3). NO chi-RequireAction: the RBAC "initiator OR incarnation.get/
			// history" is not expressible via an existence-gate (the initiator may lack the permission) —
			// all authorization is in-handler (parity with /mcp/events authorizeSSE). Inherits
			// only /v1 RequireJWT (query-token */events per the canon). runEventsDeps nil →
			// the route is not mounted (opt-in, the voyageH pattern; in the drift-test pathAllowlist).
			if runEventsDeps != nil {
				r.Group(func(r chi.Router) {
					registerHumaIncarnationRunEvents(newHumaCadenceAPI(r), runEventsDeps)
				})
			}

			// POST /v1/incarnations/{name}/scenarios/{scenario} — run a named
			// scenario. Blocked by the Toll-middleware on cluster:degraded (ADR-038):
			// 503 + Retry-After. The Toll-middleware is FIRST in the chain (outermost), so a 503
			// on a degraded cluster returns BEFORE RBAC/Audit: a blocked request must
			// neither spend a permission-check nor write the scenario_started audit event.
			r.With(
				toll.DegradedMiddleware(tollDegraded, logger),
				apimiddleware.RequirePermissionMulti(enforcer, "incarnation", "run", incScope),
			).Group(func(r chi.Router) {
				registerHumaIncarnationRun(newHumaIncarnationAPI(r, auditWriter, audit.EventIncarnationScenarioStarted, logger), incH)
			})

			r.With(
				apimiddleware.RequirePermissionMulti(enforcer, "incarnation", "unlock", incScope),
			).Group(func(r chi.Router) {
				registerHumaIncarnationUnlock(newHumaIncarnationAPI(r, auditWriter, audit.EventIncarnationUnlocked, logger), incH)
			})

			r.With(
				apimiddleware.RequirePermissionMulti(enforcer, "incarnation", "upgrade", incScope),
			).Group(func(r chi.Router) {
				registerHumaIncarnationUpgrade(newHumaIncarnationAPI(r, auditWriter, audit.EventIncarnationUpgradeStarted, logger), incH)
			})

			// POST /v1/incarnations/{name}/rerun-last — clear error_locked + rerun the
			// last failed scenario. WRITE-SELF-AUDIT: incarnation.rerun_last is written by the handler
			// itself (the payload is known only after UnlockForRerun; audit-middleware is NOT
			// wired). Permission incarnation.rerun-last, scope incScope.
			r.With(
				apimiddleware.RequirePermissionMulti(enforcer, "incarnation", "rerun-last", incScope),
			).Group(func(r chi.Router) {
				registerHumaIncarnationRerunLast(newHumaCadenceAPI(r), incH)
			})

			// POST /v1/incarnations/{name}/check-drift — Scry on-demand (ADR-031, Slice B).
			// WRITE-SELF-AUDIT: incarnation.drift_checked is written by the handler itself (the payload —
			// drift_summary — after CheckDrift; audit-middleware is NOT wired). Permission
			// incarnation.check-drift, scope incScope.
			r.With(
				apimiddleware.RequirePermissionMulti(enforcer, "incarnation", "check-drift", incScope),
			).Group(func(r chi.Router) {
				registerHumaIncarnationCheckDrift(newHumaCadenceAPI(r), incH)
			})

			// DELETE /v1/incarnations/{name} — destroy (S-D4). WRITE-SELF-AUDIT:
			// destroy_started is written by the service layer [incarnation.Destroy] itself (it needs
			// source/previous_status/force, not uniformly available to the middleware);
			// audit-middleware is NOT wired. Permission incarnation.destroy, scope incScope.
			r.With(
				apimiddleware.RequirePermissionMulti(enforcer, "incarnation", "destroy", incScope),
			).Group(func(r chi.Router) {
				registerHumaIncarnationDestroy(newHumaCadenceAPI(r), incH)
			})

			// PATCH /v1/incarnations/{name}/hosts — edit declared spec.hosts[]
			// (ADR-008). Permission incarnation.update-hosts (narrowed from incarnation.update,
			// PM-decision 2026-06-02; the backcompat alias is canonicalized on snapshot load), scope
			// incScope. WRITE-SELF-AUDIT: incarnation.hosts_updated is written by the handler itself (payload
			// old/new snapshot after UpdateHosts; audit-middleware is NOT wired).
			r.With(
				apimiddleware.RequirePermissionMulti(enforcer, "incarnation", "update-hosts", incScope),
			).Group(func(r chi.Router) {
				registerHumaIncarnationUpdateHosts(newHumaCadenceAPI(r), incH)
			})

			// PUT /v1/incarnations/{name}/traits — wholesale replacement of operator-set
			// trait labels (ADR-060 amend R1, relocation per-soul → per-incarnation).
			// incarnation.traits is the source of truth, projected into souls.traits of
			// the member hosts. Permission incarnation.traits-set, scope incScope.
			// WRITE-SELF-AUDIT: incarnation.traits_changed is written by the handler itself (payload
			// old/new keys after UpdateTraits; audit-middleware is NOT wired).
			r.With(
				apimiddleware.RequirePermissionMulti(enforcer, "incarnation", "traits-set", incScope),
			).Group(func(r chi.Router) {
				registerHumaIncarnationSetTraits(newHumaCadenceAPI(r), incH)
			})

			// /v1/incarnations/{name}/choirs — CRUD of the Choir/Voice topology (ADR-044,
			// S-T3). A Choir belongs to an incarnation → the same scope selector incScope
			// (incarnation/service/coven by path-{name}) as incarnation mutations.
			// resource — `choir`; actions — create / delete / list + add-voice /
			// remove-voice. Mounted ONLY when choirH is non-nil (the errandH pattern):
			// a keeper without a ChoirDB pool returns 404, the drift-test keeps the paths in
			// pathAllowlist.
			//
			// FULL-TYPED huma (ADR-054, BATCH-2f WRITE-SELF-AUDIT): create/delete/
			// add-voice/remove-voice write audit (choir.created/.deleted/.voice_added/
			// .voice_removed) by the handler ITSELF via writeAuditCtx INSIDE CreateTyped/
			// DeleteTyped/AddVoiceTyped/RemoveVoiceTyped — audit-middleware is NOT wired
			// (unlike the middleware-audit domains role/operator). newHumaCadenceAPI
			// (no audit wiring). Multi-resource: voices — a sub-resource; the huma op carries
			// the FULL path /{name}/choirs[/...] relative to the /v1/incarnations group (NOT
			// nested in chi.Route("/{name}") — otherwise chi would double the {name} prefix,
			// the soul/synod multi-resource pattern; huma binds {name}/{choir}/{sid} itself,
			// the chi-RBAC selector incScope reads them from the humachi pattern). list/list-voices
			// — read (no audit). Each route has its OWN chi group with its own RBAC; huma
			// inherits. No MCP choir.
			if choirH != nil {
				r.With(
					apimiddleware.RequirePermissionMulti(enforcer, "choir", "create", incScope),
				).Group(func(r chi.Router) {
					registerHumaChoirCreate(newHumaCadenceAPI(r), choirH)
				})

				r.With(
					apimiddleware.RequirePermissionMulti(enforcer, "choir", "delete", incScope),
				).Group(func(r chi.Router) {
					registerHumaChoirDelete(newHumaCadenceAPI(r), choirH)
				})

				r.With(
					apimiddleware.RequirePermissionMulti(enforcer, "choir", "add-voice", incScope),
				).Group(func(r chi.Router) {
					registerHumaVoiceAdd(newHumaCadenceAPI(r), choirH)
				})

				r.With(
					apimiddleware.RequirePermissionMulti(enforcer, "choir", "remove-voice", incScope),
				).Group(func(r chi.Router) {
					registerHumaVoiceRemove(newHumaCadenceAPI(r), choirH)
				})

				// list (choirs) + list-voices — read under a single choir.list RBAC, a shared
				// huma.API (the distinct path rules out a collision of the two GETs).
				r.With(
					apimiddleware.RequirePermissionMulti(enforcer, "choir", "list", incScope),
				).Group(func(r chi.Router) {
					choirReadAPI := newHumaCadenceAPI(r)
					registerHumaChoirList(choirReadAPI, choirH)
					registerHumaVoiceList(choirReadAPI, choirH)
				})
			}
		})

		// /v1/runs — global read-view of runs (the UI "All Runs" page):
		// a rollup of apply_runs by apply_id ACROSS ALL incarnations + summary counters
		// /stats. READ (no audit, newHumaCadenceAPI). Permission incarnation.history
		// (reuse read-tier per-incarnation runs, RequireAction existence-gate);
		// Purview narrowing — in-handler (fail-closed: empty scope → empty
		// list / zero aggregate, parity with souls/stats).
		r.Route("/runs", func(r chi.Router) {
			r.With(
				apimiddleware.RequireAction(enforcer, "incarnation", "history"),
			).Group(func(r chi.Router) {
				runsAPI := newHumaCadenceAPI(r)
				registerHumaRunsList(runsAPI, incH)
				registerHumaRunsStats(runsAPI, incH)
			})
		})

		// /v1/souls — onboarding + registry (M2.x): Create + List + issue-token.
		//
		// Selector strategy:
		//   - Create — NoSelector (RBAC decides on the bare permission; a coven selector
		//     will come once per-coven RBAC on registration exists).
		//   - List / Get / soulprint / history — [RequireAction] existence-gate
		//     (ADR-047 §d G1): the scope-aware [RequirePermission] denies a scoped
		//     operator on an empty context (there is no selector key in a nil context),
		//     cutting them off from their own list BEFORE the handler. RequireAction
		//     only asks whether `soul.list` EXISTS; the scope narrowing is done by the handler
		//     after fetching the rows (resolveListScope / readScope + soulpurview).
		//   - issue-token — [handlers.SoulSIDSelector] (`host=<sid>`), RBAC
		//     can restrict re-issuance to a specific host.
		//
		// FULL-TYPED huma (ADR-054, ROLLOUT BATCH 2e of the soul domain over the role/operator +
		// audit-endpoint references): create/coven-assign/issue-token/ssh-target/exec — WRITE+AUDIT
		// variant B (newHumaSoulAPI(evt)); list/get/soulprint/history — read (no audit).
		// Each write route has its OWN chi group with its own RBAC+event; reads are grouped by
		// RBAC. huma inherits the group chi-middleware. ALL soul-detail routes
		// (/souls/{sid}/*) on huma. MCP soul-tools call soul.Service/bootstraptoken
		// directly (bypassing the handler). POST /souls/{sid}/exec — now huma (errand.invoked,
		// dual-status 200/202 + Location, handler *handlers.ErrandHandler.ExecTyped).
		r.Route("/souls", func(r chi.Router) {
			r.With(
				apimiddleware.RequirePermission(enforcer, "soul", "create", apimiddleware.NoSelector),
			).Group(func(r chi.Router) {
				registerHumaSoulCreate(newHumaSoulAPI(r, auditWriter, audit.EventSoulCreated, logger), soulH)
			})

			r.With(
				apimiddleware.RequireAction(enforcer, "soul", "list"),
			).Group(func(r chi.Router) {
				soulListReadAPI := newHumaCadenceAPI(r)
				registerHumaSoulList(soulListReadAPI, soulH)
				// GET /v1/souls/stats — the Souls Overview aggregate. The same existence-gate
				// `RequireAction(soul, list)` and the same read-API group as list/get:
				// one registry-read permission (soul.list) covers the aggregate too; the scope
				// narrowing is done by the handler (StatsTyped via the same Purview resolve as
				// the list). staleFn — a hot-reload provider of the disconnect threshold from
				// fresh config (nil in unit tests → default 90s).
				registerHumaSoulStats(soulListReadAPI, soulH, soulStatsStaleFn)
			})

			// POST /v1/souls/coven — bulk coven-assign (pilot spec). Two-layer
			// authorization:
			//   1. middleware RequirePermission(soul, coven-assign) — the first
			//      gate "is there the permission at all". Selector — SoulCovenLabelSelector
			//      (`coven=<label>` from the body): a scope check of the assigned label
			//      (gate b) — a coven-scoped operator passes only for a label in
			//      their scope; bare/`*` — for any.
			//   2. service layer soul.BulkAssignCoven — scope-intersection (gate a):
			//      target hosts ⊆ the operator's coven-scope (CovenScope from the enforcer).
			// Audit — EventSoulCovenChanged with source=api (distinguished from the scenario
			// path by source); the payload is set by the handler via SetAuditPayload.
			r.With(
				apimiddleware.RequirePermission(enforcer, "soul", "coven-assign", handlers.SoulCovenLabelSelector),
			).Group(func(r chi.Router) {
				registerHumaSoulCovenAssign(newHumaSoulAPI(r, auditWriter, audit.EventSoulCovenChanged, logger), soulH)
			})

			// POST /v1/souls/traits — bulk trait-assign (ADR-060, write-path Slice 2).
			// Existence-gate `RequireAction(soul, traits-assign)` — "is there the permission in
			// ANY scope dimension", WITHOUT a selector: a trait KEY is NOT an RBAC
			// scope dimension (unlike a Coven label — which has gate b via
			// SoulCovenLabelSelector with `{coven: label}`). A selector RequirePermission
			// here would cut off a coven-scoped operator (their `coven=dev` permission would not
			// match a request without a coven context), even though they ARE entitled to change traits on
			// their dev hosts. Hence the `soul.list` pattern: an existence-gate on the presence of
			// the permission, while least-privilege narrows ONE gate (a) — the service layer
			// (soul.BulkAssignTraits/BulkReplaceTraits) intersects the target hosts with the
			// operator's coven-scope (the same BulkScope as coven-assign). Audit —
			// EventSoulTraitsChanged with source=api; payload — variant B (SetHumaAuditPayload).
			r.With(
				apimiddleware.RequireAction(enforcer, "soul", "traits-assign"),
			).Group(func(r chi.Router) {
				registerHumaSoulTraitsAssign(newHumaSoulAPI(r, auditWriter, audit.EventSoulTraitsChanged, logger), soulH)
			})

			// GET /v1/souls/{sid} + /soulprint + /history — single-soul read for the UI
			// detail-page. Permission `soul.list` covers list+get+soulprint+history
			// (the service.list / omen.list pattern — one permission for reading the registry;
			// `soul.get` is deliberately deferred, rbac.md §Souls). [RequireAction] existence-gate
			// (ADR-047 §d G1): a scope-aware gate would cut off a scoped operator (the host context
			// resolves from a DB row that does not exist before the fetch); the scope narrowing is done by the handler
			// (readScopeForClaims + soulpurview.InScope → 404 outside scope). Read-only — no
			// Audit. The huma ops carry the full path /{sid}[/…] (NOT nested in r.Route("/{sid}") —
			// otherwise chi would double the {sid} prefix, the operator-domain pattern); huma binds {sid}
			// itself, the chi-RBAC selectors read it from the registered humachi pattern.
			r.With(
				apimiddleware.RequireAction(enforcer, "soul", "list"),
			).Group(func(r chi.Router) {
				soulDetailAPI := newHumaCadenceAPI(r)
				registerHumaSoulGet(soulDetailAPI, soulH)
				registerHumaSoulSoulprint(soulDetailAPI, soulH)
				registerHumaSoulHistory(soulDetailAPI, soulH)
			})

			r.With(
				apimiddleware.RequirePermission(enforcer, "soul", "issue-token", handlers.SoulSIDSelector),
			).Group(func(r chi.Router) {
				registerHumaSoulIssueToken(newHumaSoulAPI(r, auditWriter, audit.EventSoulTokenIssued, logger), soulH)
			})

			// PUT /v1/souls/{sid}/ssh-target — update per-host SSH credentials for the push-flow
			// (ADR-032 amendment 2026-05-26, S7-1). Permission `soul.ssh-target-update`
			// (action — hyphenated). Selector SoulSIDSelector — `host=<sid>`. Audit
			// EventSoulSshTargetUpdated; payload — huma variant B (SetHumaAuditPayload).
			r.With(
				apimiddleware.RequirePermission(enforcer, "soul", "ssh-target-update", handlers.SoulSIDSelector),
			).Group(func(r chi.Router) {
				registerHumaSoulSshTarget(newHumaSoulAPI(r, auditWriter, audit.EventSoulSshTargetUpdated, logger), soulH)
			})

			// POST /v1/souls/{sid}/exec — pull-ad-hoc Errand (ADR-033, slice E2). Permission
			// errand.run, selector `host=<sid>` (rbac.md §Errand). FULL-TYPED huma (ADR-054,
			// BATCH-2e): WRITE+AUDIT variant B (newHumaSoulAPI with event errand.invoked) with dual-
			// status 200 sync / 202 async + Location header. The audit-middleware writes
			// EventTypeErrandInvoked on BOTH 2xx; the dispatcher itself writes the audit event inside
			// Dispatch (single source of truth) — the middleware event is a navigation-trail. When
			// errandH is nil it is not mounted. The huma op carries the full path /{sid}/exec (NOT nested in
			// r.Route("/{sid}") — otherwise chi would double the {sid} prefix; huma binds {sid} itself,
			// the chi-RBAC selector ErrandSIDSelector reads it from the humachi pattern). All
			// soul-detail routes on huma.
			if errandH != nil {
				r.With(
					apimiddleware.RequirePermission(enforcer, "errand", "run", handlers.ErrandSIDSelector),
				).Group(func(r chi.Router) {
					registerHumaSoulExec(newHumaSoulAPI(r, auditWriter, audit.EventTypeErrandInvoked, logger), errandH)
				})
			}
		})

		// /v1/plugins/sigils — Sigil allow-list for plugin integrity
		// (plugin.allow/revoke/list, ADR-026 S4a). Mounted only when
		// sigilH is non-nil (Deps.SigilSvc wired in). Selector — NoSelector:
		// rbac.md does not define selectors for plugin.* (like operator.*/role.*).
		//
		// Audit on allow/revoke (supply-chain mutations are always audited).
		// list — read-only, no audit. the handlers set the payload via
		// SetAuditPayload (caller AID, namespace/name/ref, sha256; without
		// signature/manifest).
		//
		// FULL-TYPED huma (ADR-054, ROLLOUT BATCH 2a of the entire sigil domain over the
		// role reference): allow/revoke — WRITE+AUDIT variant B (huma-audit-middleware; event
		// domain — permission `plugin`, events plugin.allowed/plugin.revoked); list —
		// read-bare (no audit). Each write route has its OWN chi group with its own
		// event type (newHumaSigilAPI(evt)). RequirePermission is the group chi-
		// middleware (huma inherits it). MCP plugin-tools call sigil.Service directly
		// (bypassing the handler).
		if sigilH != nil {
			r.Route("/plugins/sigils", func(r chi.Router) {
				r.With(
					apimiddleware.RequirePermission(enforcer, "plugin", "allow", apimiddleware.NoSelector),
				).Group(func(r chi.Router) {
					registerHumaSigilAllow(newHumaSigilAPI(r, auditWriter, audit.EventPluginAllowed, logger), sigilH)
				})

				r.With(
					apimiddleware.RequirePermission(enforcer, "plugin", "list", apimiddleware.NoSelector),
				).Group(func(r chi.Router) {
					registerHumaSigilList(newHumaCadenceAPI(r), sigilH)
				})

				r.With(
					apimiddleware.RequirePermission(enforcer, "plugin", "revoke", apimiddleware.NoSelector),
				).Group(func(r chi.Router) {
					registerHumaSigilRevoke(newHumaSigilAPI(r, auditWriter, audit.EventPluginRevoked, logger), sigilH)
				})
			})
		}

		// /v1/sigil/keys — rotation of the Sigil SIGNING trust-anchor keys (ADR-026(h),
		// R3-S7). A separate area from /v1/plugins/sigils (that one is about binary allow-lists,
		// this one is about their signing keys). Mounted only when sigilKeyH is non-nil
		// (Deps.SigilKeySvc wired in — production wire-up when Sigil is enabled).
		// Selector — NoSelector (like plugin.*/operator.*).
		//
		// Audit on introduce/set-primary/retire (signing-key rotation is
		// supply-chain-critical). list — read-only, no audit. the handlers set the
		// payload via SetAuditPayload (key_id + caller AID; WITHOUT the private key).
		//
		// FULL-TYPED huma (ADR-054, ROLLOUT BATCH 2a of the entire sigil-key domain over
		// the role reference): introduce/set-primary/retire — WRITE+AUDIT variant B
		// (huma-audit-middleware; events sigil.key-introduced/sigil.key-primary-set/
		// sigil.key-retired); list — read-bare (no audit). Each write route has its OWN
		// chi group with its own event type (newHumaSigilKeyAPI(evt)).
		// RequirePermission is the group chi-middleware (huma inherits it). MCP
		// sigil-key-tools call sigil.KeyService directly (bypassing the handler).
		if sigilKeyH != nil {
			r.Route("/sigil/keys", func(r chi.Router) {
				r.With(
					apimiddleware.RequirePermission(enforcer, "sigil", "key-introduce", apimiddleware.NoSelector),
				).Group(func(r chi.Router) {
					registerHumaSigilKeyIntroduce(newHumaSigilKeyAPI(r, auditWriter, audit.EventSigilKeyIntroduced, logger), sigilKeyH)
				})

				r.With(
					apimiddleware.RequirePermission(enforcer, "sigil", "key-list", apimiddleware.NoSelector),
				).Group(func(r chi.Router) {
					registerHumaSigilKeyList(newHumaCadenceAPI(r), sigilKeyH)
				})

				r.With(
					apimiddleware.RequirePermission(enforcer, "sigil", "key-set-primary", apimiddleware.NoSelector),
				).Group(func(r chi.Router) {
					registerHumaSigilKeySetPrimary(newHumaSigilKeyAPI(r, auditWriter, audit.EventSigilKeyPrimarySet, logger), sigilKeyH)
				})

				r.With(
					apimiddleware.RequirePermission(enforcer, "sigil", "key-retire", apimiddleware.NoSelector),
				).Group(func(r chi.Router) {
					registerHumaSigilKeyRetire(newHumaSigilKeyAPI(r, auditWriter, audit.EventSigilKeyRetired, logger), sigilKeyH)
				})
			})
		}

		// /v1/services — Service registry (service.register/update/list/
		// deregister, the ADR-028 RBAC-storage pattern). Mounted only when
		// serviceH is non-nil (Deps.ServiceSvc wired in). Selector — NoSelector:
		// service.* CRUD operates on the registry itself (register/list/deregister
		// entries), without targeting by service name in S3 (like operator.*/role.*).
		//
		// Audit on the 3 mutating routes (register/update/deregister). list/get —
		// read-only, no audit (like role.list / plugin.list). the handlers set the
		// payload via SetAuditPayload (name + git/ref + caller AID; the git URL
		// is not a secret).
		//
		// Permission mapping: POST→service.register, GET→service.list (both for
		// list and for get-{name}), PATCH→service.update, DELETE→service.deregister.
		//
		// FULL-TYPED huma (ADR-054, ROLLOUT BATCH 2d of the entire service domain over the
		// role/operator/augur/herald): register/update/deregister — WRITE+AUDIT
		// variant B (huma-audit-middleware: full-typed huma writes the response ITSELF,
		// the StatusRecorder from apimiddleware.Audit does not apply — audit holds
		// hctx.Status() + a carrier payload, otherwise an S6 relapse; register/update — 201/200
		// WITH BODY); list/get + refs/scenarios/state-schema/dependencies — read (no
		// audit; sub-reads carry the full path /{name}/<...> in the huma operation + optional
		// ?ref=, tier 502 on the git-loader). Each write route has its OWN chi group with
		// its own event type (newHumaServiceAPI(evt)). RequirePermission is the
		// group chi-middleware (huma inherits it). huma serves all service routes.
		// MCP service-tools call serviceregistry.Service directly (bypassing the handler).
		if serviceH != nil {
			r.Route("/services", func(r chi.Router) {
				r.With(
					apimiddleware.RequirePermission(enforcer, "service", "register", apimiddleware.NoSelector),
				).Group(func(r chi.Router) {
					registerHumaServiceRegister(newHumaServiceAPI(r, auditWriter, audit.EventServiceRegistered, logger), serviceH)
				})

				r.With(
					apimiddleware.RequirePermission(enforcer, "service", "list", apimiddleware.NoSelector),
				).Group(func(r chi.Router) {
					registerHumaServiceList(newHumaCadenceAPI(r), serviceH)
				})

				// GET /v1/services/{name} — detail. Permission service.list (read
				// covered by the list permission). The huma op carries the full path /{name} (NOT nested in
				// r.Route("/{name}") — otherwise chi would double the prefix).
				r.With(
					apimiddleware.RequirePermission(enforcer, "service", "list", apimiddleware.NoSelector),
				).Group(func(r chi.Router) {
					registerHumaServiceGet(newHumaCadenceAPI(r), serviceH)
				})

				r.With(
					apimiddleware.RequirePermission(enforcer, "service", "update", apimiddleware.NoSelector),
				).Group(func(r chi.Router) {
					registerHumaServiceUpdate(newHumaServiceAPI(r, auditWriter, audit.EventServiceUpdated, logger), serviceH)
				})

				r.With(
					apimiddleware.RequirePermission(enforcer, "service", "deregister", apimiddleware.NoSelector),
				).Group(func(r chi.Router) {
					registerHumaServiceDeregister(newHumaServiceAPI(r, auditWriter, audit.EventServiceDeregistered, logger), serviceH)
				})

				// /refs — git tags + branches for the UI Upgrade-modal (read-only,
				// permission service.list — refs are a projection of the Service record, no
				// audit, like Get/List). 502 → the external git source failed.
				r.With(
					apimiddleware.RequirePermission(enforcer, "service", "list", apimiddleware.NoSelector),
				).Group(func(r chi.Router) {
					registerHumaServiceRefs(newHumaCadenceAPI(r), serviceH)
				})

				// /scenarios — listing scenarios from the materialized snapshot of the Service
				// git repo for the UI Run-modal dropdown. permission service.list. 502 →
				// the loader (git-clone / parse) failed.
				r.With(
					apimiddleware.RequirePermission(enforcer, "service", "list", apimiddleware.NoSelector),
				).Group(func(r chi.Router) {
					registerHumaServiceScenarios(newHumaCadenceAPI(r), serviceH)
				})

				// /state-schema — the service state_schema metadata for the UI Schema
				// explorer. permission service.list. 502 → the loader failed.
				r.With(
					apimiddleware.RequirePermission(enforcer, "service", "list", apimiddleware.NoSelector),
				).Group(func(r chi.Router) {
					registerHumaServiceStateSchema(newHumaCadenceAPI(r), serviceH)
				})

				// /dependencies — the service git dependencies (destiny building-blocks + custom
				// modules from service.yml) for the UI Service Detail. permission service.list.
				// 502 → the loader failed.
				r.With(
					apimiddleware.RequirePermission(enforcer, "service", "list", apimiddleware.NoSelector),
				).Group(func(r chi.Router) {
					registerHumaServiceDependencies(newHumaCadenceAPI(r), serviceH)
				})

				// /directives — catalog of valid redis.conf directives by version
				// (essence.redis_directives) for the redis_settings UI editor.
				// permission service.list. ETag=snapshot SHA1 + immutable. 502 →
				// the loader failed.
				r.With(
					apimiddleware.RequirePermission(enforcer, "service", "list", apimiddleware.NoSelector),
				).Group(func(r chi.Router) {
					registerHumaServiceDirectives(newHumaCadenceAPI(r), serviceH)
				})
			})
		}

		// /v1/provisioning-policy — runtime policy for the methods of CREATING operators
		// (provisioning_allowed_methods, ADR-058 Part B). Mounted only when
		// non-nil provisioningPolicyH (Deps.ProvisioningPolicyReader + ServiceSvc
		// are wired in). Selector — NoSelector: the policy is cluster-level (like
		// operator.* / role.*).
		//
		// GET — read (permission provisioning.read, no audit, the service.list pattern).
		// PUT — WRITE+AUDIT variant B (permission provisioning.update, event
		// provisioning.policy_changed; huma-audit-middleware on its own chi group,
		// like service.update). Each route has its OWN chi group with its own RBAC; huma
		// inherits the group chi-middleware.
		if provisioningPolicyH != nil {
			r.Route("/provisioning-policy", func(r chi.Router) {
				r.With(
					apimiddleware.RequirePermission(enforcer, "provisioning", "read", apimiddleware.NoSelector),
				).Group(func(r chi.Router) {
					registerHumaProvisioningPolicyGet(newHumaCadenceAPI(r), provisioningPolicyH)
				})

				r.With(
					apimiddleware.RequirePermission(enforcer, "provisioning", "update", apimiddleware.NoSelector),
				).Group(func(r chi.Router) {
					registerHumaProvisioningPolicyPut(newHumaProvisioningAPI(r, auditWriter, audit.EventProvisioningPolicyChanged, logger), provisioningPolicyH)
				})
			})
		}

		// /v1/modules — module-catalog (core registry doc-data + active plugin
		// allow-lists), UI Run→Command module-search. Permission service.list (read-only
		// catalog, no audit — the service.list / plugin.list pattern); a new
		// permission is not created (reuse is preferred). Selector — NoSelector
		// (the catalog is global, not per-resource). moduleCatalogH is always non-nil
		// (the core catalog needs no external dependencies), so the routes in the spec and
		// the router match without an allowlist (unlike the opt-in plugin.*).
		//
		// FULL-TYPED huma (ADR-054, ROLLOUT BATCH 2e of the entire module domain over the
		// catalog read-bare + form-prep read-with-body reference): list/get — read the catalog; form-prep
		// — a read-resolve of SIDs for the form. ALL THREE — READ-only, audit is NOT wired. Each
		// route has its OWN chi group with its own RBAC; huma inherits the group chi-middleware. MCP
		// module domain — none (the catalog has no MCP tools).
		if moduleCatalogH != nil {
			r.Route("/modules", func(r chi.Router) {
				r.With(
					apimiddleware.RequirePermission(enforcer, "service", "list", apimiddleware.NoSelector),
				).Group(func(r chi.Router) {
					moduleReadAPI := newHumaCadenceAPI(r)
					registerHumaModuleList(moduleReadAPI, moduleCatalogH)
					registerHumaModuleGet(moduleReadAPI, moduleCatalogH)
				})

				// /{name}/form-prep — resolver of source catalogs for the module UI form
				// (ADR-045 S3): source incarnation_hosts/choir → live SIDs for
				// autocomplete of the Run→Command form. Permission incarnation.run —
				// the endpoint serves command-run preparation (whoever runs the
				// run also resolves the SIDs for its fields); reuse of the run-related
				// permission, a new one is not created. Selector — NoSelector (the resolve is
				// cluster-wide over souls, not a per-resource RBAC scope). No audit
				// (a read-only resolve, the soul.list / service.list pattern).
				// Mounted only when moduleFormPrepH is non-nil (Deps.Pool
				// wired in); the drift-test assembles the router with nil → the route is in the allowlist.
				if moduleFormPrepH != nil {
					r.With(
						apimiddleware.RequirePermission(enforcer, "incarnation", "run", apimiddleware.NoSelector),
					).Group(func(r chi.Router) {
						registerHumaModuleFormPrep(newHumaCadenceAPI(r), moduleFormPrepH)
					})
				}
			})
		}

		// /v1/permissions — machine-readable catalog of RBAC permissions (source —
		// rbac.catalog.go). The UI fetches the real names for assigning permissions to a role
		// (fixes the hardcoded-permission → unknown_permission bug). RBAC — auth-ONLY
		// (RequireJWT on /v1/* above), WITHOUT RequirePermission: the catalog is self-
		// describing, requiring a permission to read the permission list = chicken-and-egg
		// (architect verdict). Read-only, no audit (the health/meta pattern). permCatalogH
		// is always non-nil (static from the rbac package, no external dependencies),
		// so the route in the spec and the router match without an allowlist (like /v1/modules).
		//
		// FULL-TYPED huma (ADR-054, BATCH-1 read-tier): three READ catalogs
		// (permissions / event-types / me-permissions) on ONE huma.API over
		// the /v1 group (auth-only — RequireJWT on /v1/* above, WITHOUT RequirePermission:
		// self-describing, requiring a permission to read the list = "chicken-and-egg",
		// architect verdict). The operations carry absolute-under-/v1 paths
		// (/permissions / /event-types / /me/permissions) → chi.Walk sees
		// /v1/<path>, drift-test green; the distinct path rules out a collision of the three
		// operations on the shared API. Read-only — WITHOUT audit-middleware. The strict methods
		// ListPermissions/ListEventTypes/ListMyPermissions remain generated (until
		// the final removal), unmounted.
		//
		// /v1/permissions — catalog of RBAC permissions (source rbac.catalog.go); the UI
		// fetches the real names for assigning permissions to a role (fixes unknown_permission).
		// /v1/event-types — catalog of event-types for Tiding subscription (source
		// herald/eventtypes.go; fixes the ADR-042 UI hardcode).
		// /v1/me/permissions — effective permissions of the CURRENT Archon (AID from claims, not
		// query; does not return others'), for a permission-aware UI; nil-claims → 500
		// problem+json (parity with the domain Get). All three handlers are always non-nil
		// (static rbac/herald + an enforcer snapshot), so the routes in the spec and the router
		// match without an allowlist (like /v1/modules).
		catalogAPI := newHumaCadenceAPI(r)
		registerHumaPermissionsList(catalogAPI, permCatalogH)
		registerHumaEventTypesList(catalogAPI, eventTypeCatalogH)
		registerHumaHeraldTypesList(catalogAPI, heraldTypeCatalogH)
		registerHumaMyPermissionsList(catalogAPI, meH)

		// GET /v1/cluster — HA topology of the Keeper cluster from Conclave (ADR-006 amend)
		// + self-health of the current instance (Souls Overview UI). Read-only, no audit
		// (the health/meta + catalog pattern). Existence-gate `RequireAction(soul, list)`:
		// we reuse the existing registry-read permission (no new one) — whoever
		// can see Souls also sees the cluster instances. clusterH nil (dev/tests without
		// a Redis wire-up) → the route is not mounted (register-func no-op). Its own
		// chi group with an RBAC gate; huma inherits the middleware.
		if clusterH != nil {
			r.With(
				apimiddleware.RequireAction(enforcer, "soul", "list"),
			).Group(func(r chi.Router) {
				registerHumaClusterGet(newHumaCadenceAPI(r), clusterH)
			})
		}

		// /v1/augur — Augur registries (omens / rites, ADR-025). Mounted only
		// when augurH is non-nil (Deps.AugurSvc wired in). Selector —
		// NoSelector: omen.*/rite.* operate on the registry itself, without targeting by
		// omen name in MVP (like service.*/role.*).
		//
		// Audit on the 4 mutating routes (omen create/delete + rite create/delete).
		// list/get — read-only, no audit. the handlers set the payload via
		// SetAuditPayload (name/source_type/endpoint/auth_ref for omen — not a
		// secret; omen/subject/delegate for rite — not a secret; allow / secret
		// values are NOT included, augur.md §8).
		//
		// Permission mapping: POST omens→omen.create, GET omens(+{name})→omen.list,
		// DELETE omens/{name}→omen.delete; POST rites→rite.create, GET rites→
		// rite.list, DELETE rites/{id}→rite.delete.
		//
		// FULL-TYPED huma (ADR-054, ROLLOUT BATCH 2b of the entire augur domain over the
		// role/operator references): omen create/delete + rite create/delete — WRITE+AUDIT
		// variant B (huma-audit-middleware; full-typed huma writes the response ITSELF, so
		// the StatusRecorder from apimiddleware.Audit does not apply — audit holds
		// hctx.Status() + a carrier payload, otherwise an S6 relapse). omen list/get + rite
		// list — read (WITHOUT audit; list — read-with-typed-query int32-pagination→400,
		// rite list — required omen=query→422). Each write route has its OWN chi group
		// with its own event type (newHumaAugurAPI(evt)). RequirePermission is the
		// group chi-middleware (huma inherits it). MCP augur-tools call augur.Service
		// directly (bypassing the handler).
		//
		// chi group /v1/augur + relative huma-op paths /omens[/{name}] and
		// /rites[/{id}] (NOT nested chi.Route /omens //rites): each route's huma op
		// carries the full sub-/augur path → chi.Walk sees /v1/augur/omens etc.
		// (drift-test green), distinct-path rules out a collision of omen-POST/rite-POST
		// on the shared spec-dump-API (otherwise both would land on the same "/" path).
		if augurH != nil {
			r.Route("/augur", func(r chi.Router) {
				r.With(
					apimiddleware.RequirePermission(enforcer, "omen", "create", apimiddleware.NoSelector),
				).Group(func(r chi.Router) {
					registerHumaOmenCreate(newHumaAugurAPI(r, auditWriter, audit.EventOmenCreated, logger), augurH)
				})

				r.With(
					apimiddleware.RequirePermission(enforcer, "omen", "list", apimiddleware.NoSelector),
				).Group(func(r chi.Router) {
					registerHumaOmenList(newHumaCadenceAPI(r), augurH)
				})

				r.With(
					apimiddleware.RequirePermission(enforcer, "omen", "list", apimiddleware.NoSelector),
				).Group(func(r chi.Router) {
					registerHumaOmenGet(newHumaCadenceAPI(r), augurH)
				})

				r.With(
					apimiddleware.RequirePermission(enforcer, "omen", "delete", apimiddleware.NoSelector),
				).Group(func(r chi.Router) {
					registerHumaOmenDelete(newHumaAugurAPI(r, auditWriter, audit.EventOmenRevoked, logger), augurH)
				})

				r.With(
					apimiddleware.RequirePermission(enforcer, "rite", "create", apimiddleware.NoSelector),
				).Group(func(r chi.Router) {
					registerHumaRiteCreate(newHumaAugurAPI(r, auditWriter, audit.EventRiteCreated, logger), augurH)
				})

				r.With(
					apimiddleware.RequirePermission(enforcer, "rite", "list", apimiddleware.NoSelector),
				).Group(func(r chi.Router) {
					registerHumaRiteList(newHumaCadenceAPI(r), augurH)
				})

				r.With(
					apimiddleware.RequirePermission(enforcer, "rite", "delete", apimiddleware.NoSelector),
				).Group(func(r chi.Router) {
					registerHumaRiteDelete(newHumaAugurAPI(r, auditWriter, audit.EventRiteRevoked, logger), augurH)
				})
			})
		}

		// /v1/vigils + /v1/decrees — Oracle registries (beacons, ADR-030 S3).
		// Mounted only when oracleH is non-nil (Deps.OracleSvc wired in).
		// Selector — NoSelector: vigil.*/decree.* operate on the registry itself, without
		// targeting by name in MVP (like augur.*/service.*).
		//
		// Audit on the 4 mutating routes (vigil create/delete + decree create/delete).
		// list/get — read-only, no audit. the handlers set the payload via
		// SetAuditPayload (name/check/interval/subject for vigil; name/on_beacon/
		// incarnation/scenario/subject for decree — not a secret; params / where-CEL /
		// action_input are NOT included, action_input may carry a vault-ref in transit).
		//
		// Permission mapping: POST vigils→vigil.create, GET vigils(+{name})→vigil.list,
		// DELETE vigils/{name}→vigil.delete; symmetric for decrees.
		//
		// FULL-TYPED huma (ADR-054, ROLLOUT BATCH 2b of the entire oracle domain over the
		// role/operator/augur references): vigil create/delete + decree create/delete — WRITE+AUDIT
		// variant B (huma-audit-middleware; full-typed huma writes the response ITSELF, so
		// the StatusRecorder from apimiddleware.Audit does not apply — audit holds
		// hctx.Status() + a carrier payload, otherwise an S6 relapse). vigil/decree list/get —
		// read (WITHOUT audit; list — read-with-typed-query int32-pagination→400). Each
		// write route has its OWN chi group with its own event type (newHumaOracleAPI(evt)).
		// The huma op carries the FULL path /vigils[/{name}]//decrees[/{name}] → the groups
		// are mounted directly on /v1 (distinct-path for the spec-dump, otherwise vigil-POST and
		// decree-POST would land on the same "/"). RequirePermission is the group chi-middleware
		// (huma inherits it). MCP oracle-tools call oracle.Service directly (bypassing the handler).
		if oracleH != nil {
			r.With(
				apimiddleware.RequirePermission(enforcer, "vigil", "create", apimiddleware.NoSelector),
			).Group(func(r chi.Router) {
				registerHumaVigilCreate(newHumaOracleAPI(r, auditWriter, audit.EventVigilCreated, logger), oracleH)
			})

			r.With(
				apimiddleware.RequirePermission(enforcer, "vigil", "list", apimiddleware.NoSelector),
			).Group(func(r chi.Router) {
				registerHumaVigilList(newHumaCadenceAPI(r), oracleH)
			})

			r.With(
				apimiddleware.RequirePermission(enforcer, "vigil", "list", apimiddleware.NoSelector),
			).Group(func(r chi.Router) {
				registerHumaVigilGet(newHumaCadenceAPI(r), oracleH)
			})

			r.With(
				apimiddleware.RequirePermission(enforcer, "vigil", "delete", apimiddleware.NoSelector),
			).Group(func(r chi.Router) {
				registerHumaVigilDelete(newHumaOracleAPI(r, auditWriter, audit.EventVigilDeleted, logger), oracleH)
			})

			r.With(
				apimiddleware.RequirePermission(enforcer, "decree", "create", apimiddleware.NoSelector),
			).Group(func(r chi.Router) {
				registerHumaDecreeCreate(newHumaOracleAPI(r, auditWriter, audit.EventDecreeCreated, logger), oracleH)
			})

			r.With(
				apimiddleware.RequirePermission(enforcer, "decree", "list", apimiddleware.NoSelector),
			).Group(func(r chi.Router) {
				registerHumaDecreeList(newHumaCadenceAPI(r), oracleH)
			})

			r.With(
				apimiddleware.RequirePermission(enforcer, "decree", "list", apimiddleware.NoSelector),
			).Group(func(r chi.Router) {
				registerHumaDecreeGet(newHumaCadenceAPI(r), oracleH)
			})

			r.With(
				apimiddleware.RequirePermission(enforcer, "decree", "delete", apimiddleware.NoSelector),
			).Group(func(r chi.Router) {
				registerHumaDecreeDelete(newHumaOracleAPI(r, auditWriter, audit.EventDecreeDeleted, logger), oracleH)
			})
		}

		// /v1/push — multi-host push-orchestrator (Variant C, ADR-004 push-flow +
		// docs/keeper/push.md). Mounted only when pushH is non-nil (Deps.PushRun
		// wired in). Selector — NoSelector: push.apply/push.read operate on the apply_id,
		// without targeting by incarnation/coven name in MVP (like augur.*/service.*).
		// Coven-scope filtering by inventory hosts — a separate slice (RBAC
		// extension, not covered in this slice per architect verdict a58e).
		//
		// Audit on apply (mutating): the payload handler sets it via
		// SetAuditPayload (apply_id, destiny-ref, inventory_size, ssh_provider,
		// cleanup_stale); SIDs as a whole are NOT included (there can be many, they live in
		// push_runs.inventory_sids). GET — read-only, no audit.
		//
		// Permission mapping: POST→push.apply, GET→push.read. push.read — a new
		// permission (see catalog.go), separate from push.apply: the read operation does not
		// require mutate rights.
		//
		// FULL-TYPED huma (ADR-054, ROLLOUT BATCH 2e of the entire push domain over the
		// operator issue-token + audit-endpoint references): apply — WRITE+AUDIT variant B
		// (newHumaPushAPI(evt) with event push.applied; 202+body async); get/push-runs —
		// read (WITHOUT audit). The apply group keeps the Toll DegradedMiddleware (503 on
		// cluster:degraded) FIRST — huma inherits the group chi-middleware. The MCP push-tool
		// keeper.push.apply calls pushorch.PushRun directly (bypassing the handler).
		if pushH != nil {
			r.Route("/push", func(r chi.Router) {
				// POST /v1/push/apply — blocked by Toll on cluster:degraded
				// (ADR-038): parity with POST /v1/incarnations/{name}/scenarios/{scenario},
				// outermost-middleware → 503 BEFORE RBAC/Audit. GET /v1/push/{apply_id}
				// (below) — a read-API, NOT blocked (recovery-friendly reading of the
				// run status while degraded).
				r.With(
					toll.DegradedMiddleware(tollDegraded, logger),
					apimiddleware.RequirePermission(enforcer, "push", "apply", apimiddleware.NoSelector),
				).Group(func(r chi.Router) {
					registerHumaPushApply(newHumaPushAPI(r, auditWriter, audit.EventPushApplied, logger), pushH)
				})

				r.With(
					apimiddleware.RequirePermission(enforcer, "push", "read", apimiddleware.NoSelector),
				).Group(func(r chi.Router) {
					registerHumaPushGet(newHumaCadenceAPI(r), pushH)
				})
			})

			// /v1/push-runs — the global list of push runs (UI-4 Push-runs page).
			// A separate zone from /v1/push/{apply_id} (that one is per-id detail; this
			// one is a list with pagination/filters). RBAC — incarnation.history (push is
			// incarnation history, parity with list); a separate permission
			// `push.list` is not introduced until an operator requests it. NoSelector — a global
			// list without a path-{id} target.
			r.With(
				apimiddleware.RequirePermission(enforcer, "incarnation", "history", apimiddleware.NoSelector),
			).Group(func(r chi.Router) {
				registerHumaPushRunsList(newHumaCadenceAPI(r), pushH)
			})
		}

		// /v1/push-providers — CRUD of the Push-Provider registry (ADR-032 amendment
		// 2026-05-26, S7-2). Mounted only when pushProviderH is non-nil
		// (Deps.PushProviderSvc wired in). Selector — NoSelector: push-provider.*
		// operates on the registry itself (like provider.* / service.* / role.*).
		//
		// Audit on the 3 mutating routes (create/update/delete). list/get — read-only,
		// no audit. the handler sets the payload via SetAuditPayload (name +
		// params_keys without values; the sensitive invariant — vault-refs are validated
		// by the service).
		//
		// Permission mapping: POST→push-provider.create, GET list→push-provider.list,
		// GET {name}→push-provider.read, PUT→push-provider.update, DELETE→
		// push-provider.delete.
		//
		// FULL-TYPED huma (ADR-054, ROLLOUT BATCH 2b of the entire push-provider domain over
		// the role/operator references): create/update/delete — WRITE+AUDIT variant B
		// (huma-audit-middleware; full-typed huma writes the response ITSELF, so
		// the StatusRecorder from apimiddleware.Audit does not apply — audit holds
		// hctx.Status() + a carrier payload, otherwise an S6 relapse). list/get — read (WITHOUT
		// audit; list — read-with-typed-query int32-pagination→400 + name_pattern;
		// update — PUT replace semantics, NOT a presence-tier). Each write route has its OWN
		// chi group with its own event type (newHumaPushProviderAPI(evt)).
		// RequirePermission is the group chi-middleware (huma inherits it). MCP
		// push-provider-tools call pushprovider.Service directly (bypassing the handler).
		if pushProviderH != nil {
			r.Route("/push-providers", func(r chi.Router) {
				r.With(
					apimiddleware.RequirePermission(enforcer, "push-provider", "create", apimiddleware.NoSelector),
				).Group(func(r chi.Router) {
					registerHumaPushProviderCreate(newHumaPushProviderAPI(r, auditWriter, audit.EventPushProviderCreated, logger), pushProviderH)
				})

				r.With(
					apimiddleware.RequirePermission(enforcer, "push-provider", "list", apimiddleware.NoSelector),
				).Group(func(r chi.Router) {
					registerHumaPushProviderList(newHumaCadenceAPI(r), pushProviderH)
				})

				r.With(
					apimiddleware.RequirePermission(enforcer, "push-provider", "read", apimiddleware.NoSelector),
				).Group(func(r chi.Router) {
					registerHumaPushProviderGet(newHumaCadenceAPI(r), pushProviderH)
				})

				r.With(
					apimiddleware.RequirePermission(enforcer, "push-provider", "update", apimiddleware.NoSelector),
				).Group(func(r chi.Router) {
					registerHumaPushProviderUpdate(newHumaPushProviderAPI(r, auditWriter, audit.EventPushProviderUpdated, logger), pushProviderH)
				})

				r.With(
					apimiddleware.RequirePermission(enforcer, "push-provider", "delete", apimiddleware.NoSelector),
				).Group(func(r chi.Router) {
					registerHumaPushProviderDelete(newHumaPushProviderAPI(r, auditWriter, audit.EventPushProviderDeleted, logger), pushProviderH)
				})
			})
		}

		// /v1/providers — CRUD of the Cloud-Provider registry (ADR-017,
		// docs/keeper/cloud.md). Mounted only when providerH is non-nil
		// (Deps.ProviderSvc wired in). Selector — NoSelector: provider.* operates on
		// the registry itself (like push-provider.* / service.*).
		//
		// Audit on the 2 mutating routes (create/delete). list/get — read-only, no
		// audit. credentials_ref is returned as a vault path, the secret is not resolved.
		//
		// Permission mapping: POST→provider.create, GET list/{name}→provider.read,
		// DELETE→provider.delete. create/delete — WRITE+AUDIT variant B (huma-audit-
		// middleware, its own chi group with its own event type). MCP provider-tools
		// call provider.Service directly (bypassing the handler).
		if providerH != nil {
			r.Route("/providers", func(r chi.Router) {
				r.With(
					apimiddleware.RequirePermission(enforcer, "provider", "create", apimiddleware.NoSelector),
				).Group(func(r chi.Router) {
					registerHumaProviderCreate(newHumaProviderAPI(r, auditWriter, audit.EventProviderCreated, logger), providerH)
				})

				r.With(
					apimiddleware.RequirePermission(enforcer, "provider", "read", apimiddleware.NoSelector),
				).Group(func(r chi.Router) {
					registerHumaProviderList(newHumaCadenceAPI(r), providerH)
					registerHumaProviderGet(newHumaCadenceAPI(r), providerH)
				})

				r.With(
					apimiddleware.RequirePermission(enforcer, "provider", "delete", apimiddleware.NoSelector),
				).Group(func(r chi.Router) {
					registerHumaProviderDelete(newHumaProviderAPI(r, auditWriter, audit.EventProviderDeleted, logger), providerH)
				})
			})
		}

		// /v1/profiles — CRUD of the Cloud-Profile registry (ADR-017, docs/keeper/cloud.md).
		// Mounted only when profileH is non-nil (Deps.ProfileSvc wired in).
		// Selector — NoSelector. Audit on create/delete; list/get — read-only.
		// VALUE params are NOT written to audit (keys only).
		//
		// Permission mapping: POST→profile.create, GET list/{name}→profile.read,
		// DELETE→profile.delete.
		if profileH != nil {
			r.Route("/profiles", func(r chi.Router) {
				r.With(
					apimiddleware.RequirePermission(enforcer, "profile", "create", apimiddleware.NoSelector),
				).Group(func(r chi.Router) {
					registerHumaProfileCreate(newHumaProfileAPI(r, auditWriter, audit.EventProfileCreated, logger), profileH)
				})

				r.With(
					apimiddleware.RequirePermission(enforcer, "profile", "read", apimiddleware.NoSelector),
				).Group(func(r chi.Router) {
					registerHumaProfileList(newHumaCadenceAPI(r), profileH)
					registerHumaProfileGet(newHumaCadenceAPI(r), profileH)
				})

				r.With(
					apimiddleware.RequirePermission(enforcer, "profile", "delete", apimiddleware.NoSelector),
				).Group(func(r chi.Router) {
					registerHumaProfileDelete(newHumaProfileAPI(r, auditWriter, audit.EventProfileDeleted, logger), profileH)
				})
			})
		}

		// /v1/heralds + /v1/tidings — CRUD of the Herald (channels) / Tiding
		// (subscription rules) notification registries for run events (ADR-052, S4). Mounted
		// ONLY when heraldH is non-nil (Deps.HeraldSvc wired in). Selector —
		// NoSelector: management is cluster-level (like push-provider.* / omen.* /
		// role.*).
		//
		// Permission mapping: POST→herald.create / GET list→herald.list / GET
		// {name}→herald.read / PUT→herald.update / DELETE→herald.delete (and
		// tiding.* symmetrically). Audit on the 3 mutating routes of each registry
		// (create/update/delete); list/get — read-only without audit (the
		// push-provider pattern). the handler sets the payload via SetHumaAuditPayload.
		//
		// FULL-TYPED huma (ADR-054, ROLLOUT BATCH 2c of the entire herald domain over the
		// role/operator/augur/push-provider references): create/update/delete — WRITE+AUDIT
		// variant B (huma-audit-middleware; full-typed huma writes the response ITSELF, so
		// the StatusRecorder from apimiddleware.Audit does not apply — audit holds
		// hctx.Status() + a carrier payload, otherwise an S6 relapse). list/get — read (WITHOUT
		// audit; list — read-with-typed-query int32-pagination→400, tiding-list +
		// include_ephemeral bool→400; update — PUT replace semantics, NOT a presence-tier).
		// Each write route has its OWN chi group with its own event type
		// (newHumaHeraldAPI(evt)). The huma op carries the FULL path /heralds[/{name}]//tidings
		// [/{name}] → the groups are mounted directly on /v1 (distinct-path for the spec-dump,
		// otherwise herald-POST and tiding-POST would land on the same "/"). RequirePermission is the
		// group chi-middleware (huma inherits it). CRUD mutations trigger herald.Service,
		// which invalidates the dispatcher-cache snapshot
		// (in-process + cross-keeper via Redis `herald:invalidate`).
		if heraldH != nil {
			r.With(
				apimiddleware.RequirePermission(enforcer, "herald", "create", apimiddleware.NoSelector),
			).Group(func(r chi.Router) {
				registerHumaHeraldCreate(newHumaHeraldAPI(r, auditWriter, audit.EventHeraldCreated, logger), heraldH)
			})

			r.With(
				apimiddleware.RequirePermission(enforcer, "herald", "list", apimiddleware.NoSelector),
			).Group(func(r chi.Router) {
				registerHumaHeraldList(newHumaCadenceAPI(r), heraldH)
			})

			r.With(
				apimiddleware.RequirePermission(enforcer, "herald", "read", apimiddleware.NoSelector),
			).Group(func(r chi.Router) {
				registerHumaHeraldGet(newHumaCadenceAPI(r), heraldH)
			})

			r.With(
				apimiddleware.RequirePermission(enforcer, "herald", "update", apimiddleware.NoSelector),
			).Group(func(r chi.Router) {
				registerHumaHeraldUpdate(newHumaHeraldAPI(r, auditWriter, audit.EventHeraldUpdated, logger), heraldH)
			})

			r.With(
				apimiddleware.RequirePermission(enforcer, "herald", "delete", apimiddleware.NoSelector),
			).Group(func(r chi.Router) {
				registerHumaHeraldDelete(newHumaHeraldAPI(r, auditWriter, audit.EventHeraldDeleted, logger), heraldH)
			})

			r.With(
				apimiddleware.RequirePermission(enforcer, "tiding", "create", apimiddleware.NoSelector),
			).Group(func(r chi.Router) {
				registerHumaTidingCreate(newHumaHeraldAPI(r, auditWriter, audit.EventTidingCreated, logger), heraldH)
			})

			r.With(
				apimiddleware.RequirePermission(enforcer, "tiding", "list", apimiddleware.NoSelector),
			).Group(func(r chi.Router) {
				registerHumaTidingList(newHumaCadenceAPI(r), heraldH)
			})

			r.With(
				apimiddleware.RequirePermission(enforcer, "tiding", "read", apimiddleware.NoSelector),
			).Group(func(r chi.Router) {
				registerHumaTidingGet(newHumaCadenceAPI(r), heraldH)
			})

			r.With(
				apimiddleware.RequirePermission(enforcer, "tiding", "update", apimiddleware.NoSelector),
			).Group(func(r chi.Router) {
				registerHumaTidingUpdate(newHumaHeraldAPI(r, auditWriter, audit.EventTidingUpdated, logger), heraldH)
			})

			r.With(
				apimiddleware.RequirePermission(enforcer, "tiding", "delete", apimiddleware.NoSelector),
			).Group(func(r chi.Router) {
				registerHumaTidingDelete(newHumaHeraldAPI(r, auditWriter, audit.EventTidingDeleted, logger), heraldH)
			})
		}

		// /v1/errands — the Errand registry (ADR-033). The mutating POST lives under
		// /v1/souls/{sid}/exec (above, on huma — registerHumaSoulExec); here — Get/List + DELETE
		// (slice E5 cancel). Permission `errand.list` for read, `errand.cancel` for
		// DELETE; the selector for cancel — NoSelector (per-row host=<sid>-scope in RBAC
		// will be added once a multi-tenant scenario appears; the SID is known only
		// after looking up the errand row, which is incompatible with a pre-handler-middleware
		// check). Audit is NOT wired on the read endpoints (the push.read /
		// role.list pattern — read without audit); DELETE writes EventTypeErrandCancelled.
		//
		// FULL-TYPED huma (ADR-054, ROLLOUT BATCH 2c of the errand domain over the augur/
		// audit-endpoint/role references): list — read-with-typed-query (started_after date-time→
		// 400 on huma-bind — the sole source now, the former domain 422 is unreachable, ADR-051
		// Amendment 2026-06-10; offset/limit int32→400 via CheckPageBounds; status enum
		// →422; sid format→422); get — read-with-path (200 ErrandResult / 202 running
		// ErrandAccepted, dual success code); cancel — WRITE+AUDIT variant B (huma-
		// audit-middleware; full-typed huma writes the response ITSELF, the StatusRecorder from
		// apimiddleware.Audit does not apply — audit holds hctx.Status() + a carrier payload,
		// otherwise an S6 relapse; the dispatcher also writes its own audit event inside Cancel,
		// the middleware event is a security navigation-trail). The huma op carries the FULL path
		// /errands[/{errand_id}] → the groups are mounted directly on /v1 (distinct-path for the
		// spec-dump). RequirePermission is the group chi-middleware (huma inherits it).
		// MCP errand-tools call errand.Dispatcher/Store directly.
		if errandH != nil {
			r.With(
				apimiddleware.RequirePermission(enforcer, "errand", "list", apimiddleware.NoSelector),
			).Group(func(r chi.Router) {
				registerHumaErrandList(newHumaCadenceAPI(r), errandH)
			})

			r.With(
				apimiddleware.RequirePermission(enforcer, "errand", "list", apimiddleware.NoSelector),
			).Group(func(r chi.Router) {
				registerHumaErrandGet(newHumaCadenceAPI(r), errandH)
			})

			r.With(
				apimiddleware.RequirePermission(enforcer, "errand", "cancel", apimiddleware.NoSelector),
			).Group(func(r chi.Router) {
				registerHumaErrandCancel(newHumaErrandAPI(r, auditWriter, audit.EventTypeErrandCancelled, logger), errandH)
			})
		}

		// /v1/voyages — the unified batch run (ADR-043, S5).
		// Mounted only when voyageH is non-nil (the errandRunH pattern).
		//
		// RBAC-by-kind (ADR-043 §6, security-critical fail-closed): POST and DELETE
		// multiplex kind=scenario (incarnation.run) and kind=command
		// (errand.run) — a middleware-route cannot pick the permission BEFORE decoding the
		// body (kind is visible only from the body / from the loaded row), so the
		// permission check lives INSIDE VoyageHandler.Create / .Cancel. Here
		// only base auth (RequireJWT at the /v1 level) + the audit trail is wired
		// via SetAuditPayload (the handler writes scenario_run.*/command_run.*
		// directly, the payload depends on kind/resolve — middleware.Audit could not assemble it).
		//
		// GET/list/detail/targets — read of the run state; permission
		// `incarnation.history` (the All-runs vista — read of runtime state).
		// Selector — NoSelector (a global read without a path target;
		// per-kind/coven-scope read is deferred).
		//
		// FULL-TYPED huma (ADR-054, BATCH-2f WRITE-SELF-AUDIT): create/cancel —
		// self-audit INSIDE CreateTyped/CancelTyped (emitCreated/emitCancelled),
		// audit-middleware is NOT wired. preview — a read-like dry-resolve WITHOUT audit.
		// list binds typed pagination (offset/limit int32) → CheckPageBounds 400;
		// kind/status enum → 422. MCP voyage-tools call the (w,r)-handler through the
		// httptest-recorder.
		if voyageH != nil {
			r.Route("/voyages", func(r chi.Router) {
				// POST — RBAC-by-kind in the handler (see above). Auth (/v1
				// RequireJWT) + Tempo per-AID rate-limit (ADR-050(c)):
				// the resolver-heavy create is the sole MVP coverage. The middleware
				// runs AFTER RequireJWT (takes claims.Subject = AID from the context);
				// tempoLimiter=nil (no Redis / Tempo disabled) → passthrough.
				// Wired ONLY on create — GET/list/cancel are cheap and not rate-limited.
				r.With(
					apimiddleware.RateLimit(tempoLimiter, tempoBucketVoyageCreate, tempoVoyageCreateLimits, tempoMetrics, logger),
				).Group(func(r chi.Router) {
					registerHumaVoyageCreate(newHumaCadenceAPI(r), voyageH)
				})

				// POST /v1/voyages/preview — dry-resolve scope WITHOUT creating a Voyage
				// (ADR-043 amendment §4). RBAC-by-kind in the handler (like Create).
				// The Tempo wiring is on a SEPARATE bucket voyage_preview (ADR-050 amendment
				// 2026-06-17): preview is read-like in effect (no persist/audit) →
				// its own, softer limit, not sharing the quota with create. Read-like
				// — WITHOUT audit.
				r.With(
					apimiddleware.RateLimit(tempoLimiter, tempoBucketVoyagePreview, tempoVoyagePreviewLimits, tempoMetrics, logger),
				).Group(func(r chi.Router) {
					registerHumaVoyagePreview(newHumaCadenceAPI(r), voyageH)
				})

				// list/get/targets — read (incarnation.history) on ONE huma.API
				// (distinct-path rules out a collision of operations on the shared spec-dump-API).
				r.With(
					apimiddleware.RequirePermission(enforcer, "incarnation", "history", apimiddleware.NoSelector),
				).Group(func(r chi.Router) {
					voyageReadAPI := newHumaCadenceAPI(r)
					registerHumaVoyageList(voyageReadAPI, voyageH)
					registerHumaVoyageGet(voyageReadAPI, voyageH)
					registerHumaVoyageTargets(voyageReadAPI, voyageH)
				})

				// DELETE — RBAC-by-kind in the handler (kind is visible from the row). Only
				// base auth (/v1 RequireJWT) — a separate chi group without RequirePermission.
				r.Group(func(r chi.Router) {
					registerHumaVoyageCancel(newHumaCadenceAPI(r), voyageH)
				})
			})
		}

		// /v1/cadences — recurring runs (Cadence, ADR-046 S4).
		// Mounted only when cadenceH is non-nil (the voyageH pattern).
		//
		// Two-level RBAC (ADR-046 §7, security-critical fail-closed): the first
		// level — cadence.* (middleware-route, NoSelector); the second — the Voyage
		// permission by the recipe kind (scenario→incarnation.run / command→errand.run)
		// is checked INSIDE CadenceHandler.Create (kind is visible only from the body). POST
		// wires cadence.create via middleware + audit via SetAuditPayload
		// (the handler writes cadence.created/updated/deleted directly).
		//
		// PATCH — edits the recipe → cadence.update; enable/disable — the toggle →
		// granular cadence.enable/disable OR backcompat cadence.update
		// (OR-gate RequireAnyPermission, ADR-046 amendment 2026-06-02); DELETE →
		// cadence.delete; list/get — cadence.list (read). /runs — child Voyages,
		// permission incarnation.history (read of run runtime state, parity with
		// Voyage-list). All selectors — NoSelector (CRUD of the schedule registry without
		// a path target; per-name scope is deferred, parity with push-provider).
		if cadenceH != nil {
			r.Route("/cadences", func(r chi.Router) {
				// POST /v1/cadences — a huma operation (code-first, ADR-054) on THIS
				// chi group under the RequirePermission(cadence.create) wiring. The huma-handler
				// delegates to the domain cadenceH.CreateTyped (tx+notify+invalidation+audit)
				// through a thin envelope (see huma_cadence.go HUMA-PATTERN).
				r.With(
					apimiddleware.RequirePermission(enforcer, "cadence", "create", apimiddleware.NoSelector),
				).Group(func(r chi.Router) {
					registerHumaCadence(newHumaCadenceAPI(r), cadenceH)
				})

				// GET /v1/cadences (list) — READ-with-typed-query (cadence.list, WITHOUT
				// audit; Teardown T1 — the last live strict-mount /v1 moved to
				// huma). TOPOLOGY: GET / on the /v1/cadences group route — a separate
				// chi group; does not conflict with POST / (create) — different methods on the same
				// path; and does not shadow the /{id} routes (huma-op on a distinct path). Query
				// (enabled/kind enum → 422; offset/limit int32 → 400/CheckPageBounds).
				r.With(
					apimiddleware.RequirePermission(enforcer, "cadence", "list", apimiddleware.NoSelector),
				).Group(func(r chi.Router) {
					registerHumaCadenceList(newHumaCadenceAPI(r), cadenceH)
				})

				// GET/{id} + GET/{id}/runs — FULL-TYPED huma (ADR-054, BATCH-2f, moving
				// the read routes completes the cadence domain entirely). READ (WITHOUT audit). CRITICAL
				// for the blocker: the read routes are ALSO on a huma-op with the full path /{id}[/runs]
				// relative to the /v1/cadences group — the sibling sub-router r.Route("/{id}")
				// is REMOVED. Previously chi gave the ENTIRE /{id} node to the strict sub-router (which had
				// only GET / + GET /runs) → the PATCH/DELETE huma-ops were unreachable (405).
				// Now GET/{id}, GET/{id}/runs, PATCH/{id}, DELETE/{id} — four huma-ops
				// on the same /{id} node of the group, with no chi.Route on it. GET/{id} — RBAC
				// cadence.list (read-tier); /runs — incarnation.history (run history,
				// legacy parity). /runs is paginated (int32 offset/limit →
				// CheckPageBounds→400 in RunsTyped; status[] enum→422).
				r.With(
					apimiddleware.RequirePermission(enforcer, "cadence", "list", apimiddleware.NoSelector),
				).Group(func(r chi.Router) {
					registerHumaCadenceGet(newHumaCadenceAPI(r), cadenceH)
				})

				r.With(
					apimiddleware.RequirePermission(enforcer, "incarnation", "history", apimiddleware.NoSelector),
				).Group(func(r chi.Router) {
					registerHumaCadenceRuns(newHumaCadenceAPI(r), cadenceH)
				})

				// PATCH/DELETE/enable/disable — FULL-TYPED huma (ADR-054, BATCH-2f
				// self-audit): WRITE-SELF-AUDIT (the handler writes cadence.updated/.deleted
				// ITSELF via emitWrite/emitDeleted/emitEnabledToggle INSIDE PatchTyped/
				// DeleteTyped/SetEnabledTyped — audit-middleware is NOT wired, unlike the
				// middleware-audit domains role/operator). newHumaCadenceAPI (WITHOUT audit
				// wiring). The huma op carries the FULL path /{id}[/...] relative to the
				// /v1/cadences group (NOT nested in chi.Route("/{id}") — otherwise chi would double
				// the {id} prefix, the soul/operator-domain pattern; huma binds {id} itself,
				// the chi-RBAC group is inherited). PATCH — *T omitempty presence (omitted=
				// keep), NOT a presence-tier Optional[T]. No MCP cadence.
				r.With(
					apimiddleware.RequirePermission(enforcer, "cadence", "update", apimiddleware.NoSelector),
				).Group(func(r chi.Router) {
					registerHumaCadencePatch(newHumaCadenceAPI(r), cadenceH)
				})

				r.With(
					apimiddleware.RequirePermission(enforcer, "cadence", "delete", apimiddleware.NoSelector),
				).Group(func(r chi.Router) {
					registerHumaCadenceDelete(newHumaCadenceAPI(r), cadenceH)
				})

				// enable/disable — granular cadence.enable/disable OR the backcompat
				// grant cadence.update (roles with the old update do not lose the toggle, ADR-046
				// amendment 2026-06-02). An OR-gate over the action set — RequireAnyPermission.
				r.With(
					apimiddleware.RequireAnyPermission(enforcer, "cadence", []string{"enable", "update"}, apimiddleware.NoSelector),
				).Group(func(r chi.Router) {
					registerHumaCadenceEnable(newHumaCadenceAPI(r), cadenceH)
				})

				r.With(
					apimiddleware.RequireAnyPermission(enforcer, "cadence", []string{"disable", "update"}, apimiddleware.NoSelector),
				).Group(func(r chi.Router) {
					registerHumaCadenceDisable(newHumaCadenceAPI(r), cadenceH)
				})
			})
		}

		// Catch-all 404 for non-existent /v1/ paths behind the auth chain
		// (no token → 401, valid token → 404).
		r.HandleFunc("/*", func(w http.ResponseWriter, req *http.Request) {
			apimiddleware.WriteNotFound(w, req, "no such endpoint")
		})
	})

	return r
}

// routePatternFromChi returns the chi RoutePattern (`/v1/operators/{aid}/revoke`)
// for the `path` metric label. Injected into the shared/obs middleware so that
// shared/obs does not depend on chi (per [ADR-011] shared/ is cross-cutting code,
// not tied to the router).
//
// Returns an empty string if the chi-RouteContext is not initialized
// (the request did not go through the chi router; should not happen in production, but
// is possible in a unit test) — this is acceptable, the label will be recorded as `path=""`.
func routePatternFromChi(r *http.Request) string {
	rc := chi.RouteContext(r.Context())
	if rc == nil {
		return ""
	}
	return rc.RoutePattern()
}
