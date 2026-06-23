package api

import (
	"log/slog"
	"net/http"

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

// buildRouter собирает chi-роутер Operator API.
//
// Маршрутизация:
//
//	GET    /healthz                                  — liveness, без auth.
//	GET    /readyz                                   — readiness (PG+Vault), без auth.
//	GET    /openapi.yaml                             — served huma-дамп спеки (YAML), за JWT (вне /v1).
//	GET    /openapi.json                             — served huma-дамп спеки (JSON, для /docs), за JWT.
//	GET    /docs                                     — публичный RapiDoc-вьювер (shell, без auth).
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
//	POST   /v1/incarnations/{name}/unlock            — снять error_locked (M0.6c).
//	POST   /v1/incarnations/{name}/upgrade           — перевод state_schema_version (ADR-019).
//	DELETE /v1/incarnations/{name}                   — destroy incarnation (S-D4).
//	PATCH  /v1/incarnations/{name}/hosts             — править declared spec.hosts[] (ADR-008).
//	POST   /v1/voyages                               — создать Voyage (ADR-043 S5, RBAC-by-kind).
//	POST   /v1/voyages/preview                       — dry-resolve scope без создания Voyage (ADR-043 amendment §4).
//	GET    /v1/voyages                                — list Voyage-прогонов (ADR-043 S5).
//	GET    /v1/voyages/{id}                          — snapshot Voyage (ADR-043 S5).
//	GET    /v1/voyages/{id}/targets                  — All-runs drill (ADR-043 S5).
//	DELETE /v1/voyages/{id}                          — cancel pending/scheduled Voyage (ADR-043 S5).
//	POST   /v1/cadences                              — создать Cadence (ADR-046 S4, двухуровневый RBAC-by-kind).
//	GET    /v1/cadences                              — list Cadence-расписаний (ADR-046 S4).
//	GET    /v1/cadences/{id}                         — деталь Cadence (ADR-046 S4).
//	PATCH  /v1/cadences/{id}                         — обновить Cadence (ADR-046 S4).
//	DELETE /v1/cadences/{id}                         — снять Cadence (ADR-046 S4).
//	POST   /v1/cadences/{id}/enable                  — включить Cadence (ADR-046 S4).
//	POST   /v1/cadences/{id}/disable                 — выключить Cadence (ADR-046 S4).
//	GET    /v1/cadences/{id}/runs                    — дочерние Voyage Cadence (ADR-046 S4).
//	GET    /v1/push-runs                              — глобальный list push-прогонов (UI-4).
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
//	POST   /v1/modules/{name}/form-prep              — резолвер source-каталогов UI-формы модуля (ADR-045 S3).
//	GET    /v1/permissions                           — каталог RBAC-permissions (auth-only, фикс UI hardcode).
//	GET    /v1/event-types                           — каталог event-types для Tiding-подписки (auth-only, фикс UI hardcode).
//	GET    /v1/me/permissions                        — эффективные права текущего Архонта (auth-only, permission-aware UI).
//	/v1/*                                            — catch-all 404 за auth-chain.
//
// tempoBucketVoyageCreate / tempoBucketVoyagePreview — логические имена
// Tempo-bucket-ов resolver-тяжёлых voyage-write-путей (ADR-050(c) + amendment
// 2026-06-17). Совпадают с лейблом метрики `endpoint` и config-ключами
// `tempo.voyage_create` / `tempo.voyage_preview`.
//
// ОТДЕЛЬНЫЕ bucket-ключи (per-AID Redis-ключ `tempo:<aid>:<bucket>`): preview
// и create НЕ делят квоту — исчерпание одного не 429-ит другой. До amendment-а
// preview реюзил voyage_create (единый лимит), но preview read-like по эффекту
// (без persist/audit) и заслуживает более мягкого собственного лимита, оставаясь
// при этом resolver-heavy → не безлимит.
const (
	tempoBucketVoyageCreate  = "voyage_create"
	tempoBucketVoyagePreview = "voyage_preview"
)

// Health/meta вынесены вне `/v1/*` по operator-api.md § Health / Meta.
// chi.NotFound и chi.MethodNotAllowed заменены на problem+json-handlers,
// чтобы 404/405 не приходили в text/plain default-формате stdlib.
func buildRouter(verifier *jwt.Verifier, healthH *health.Handler, opH *handlers.OperatorHandler, incH *handlers.IncarnationHandler, soulH *handlers.SoulHandler, roleH *handlers.RoleHandler, synodH *handlers.SynodHandler, sigilH *handlers.SigilHandler, sigilKeyH *handlers.SigilKeyHandler, serviceH *handlers.ServiceHandler, provisioningPolicyH *handlers.ProvisioningPolicyHandler, augurH *handlers.AugurHandler, oracleH *handlers.OracleHandler, pushH *handlers.PushHandler, pushProviderH *handlers.PushProviderHandler, errandH *handlers.ErrandHandler, voyageH *handlers.VoyageHandler, cadenceH *handlers.CadenceHandler, auditH *handlers.AuditHandler, choirH *handlers.ChoirHandler, heraldH *handlers.HeraldHandler, moduleCatalogH *handlers.ModuleCatalogHandler, moduleFormPrepH *handlers.ModuleFormPrepHandler, permCatalogH *handlers.PermissionCatalogHandler, eventTypeCatalogH *handlers.EventTypeCatalogHandler, meH *handlers.MyPermissionsHandler, enforcer RBACProvider, auditWriter audit.Writer, metricsHTTP *obs.HTTPMetrics, tollDegraded toll.DegradedReader, tempoLimiter apimiddleware.RateLimiter, tempoMetrics apimiddleware.RateLimitMetrics, tempoVoyageCreateLimits func() apimiddleware.RateLimitLimits, tempoVoyagePreviewLimits func() apimiddleware.RateLimitLimits, webUIEnabled bool, ldapAuth *LDAPAuthDeps, logger *slog.Logger) http.Handler {
	r := chi.NewRouter()

	// huma error-override (ADR-054, FULL-TYPED): глобальный huma.NewError →
	// наш problem+json. ЕДИНАЯ ТОЧКА install — здесь, при сборке router-а (не в
	// фабрике каждой huma.API): для тиража ~20 доменов один install, не на домен.
	installHumaErrorOverride()

	r.NotFound(func(w http.ResponseWriter, req *http.Request) {
		apimiddleware.WriteNotFound(w, req, "no such endpoint")
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, req *http.Request) {
		// chi автоматически фильтрует методы для маршрутов, у которых
		// есть зарегистрированные handler-ы; для POST-only /v1/operators
		// GET → 405. Allow-header не выставляем (chi не отдаёт список
		// allowed-методов сам); опустить допустимо по RFC 7231.
		apimiddleware.Write405(w, req)
	})

	// Health / Meta / Docs — вне /v1.
	//
	// `/metrics` здесь НЕ монтируется: Prometheus-эндпоинт вынесен на
	// выделенный listener (`listen.metrics.addr`, ADR-024, см.
	// keeper/cmd/keeper) с опц. basic-auth. keeper_http_*-метрики при этом
	// остаются — их собирает middleware на /v1/* (ниже), а экспонирует тот
	// же *obs.Registry на metrics-listener-е.
	//
	// БЕЗОПАСНОСТЬ (механизм A, ADR-054 doc-viewer):
	//   - /healthz, /readyz — ПУБЛИЧНЫЕ (liveness/readiness, не пишутся в audit).
	//   - /docs + /docs/assets/* — ПУБЛИЧНЫЙ shell + статика RapiDoc (не несут
	//     данных/описания API; чувствительное приходит лишь после fetch спеки за
	//     JWT). См. docs_viewer.go.
	//   - /openapi.yaml + /openapi.json — ЗА JWT. Раньше /openapi.yaml был
	//     публичным, но раскрывал полную API-поверхность всем; теперь оба
	//     required Bearer (тот же RequireJWT, что и /v1), но БЕЗ /v1-обвязки
	//     (maxBody/metrics/audit/RBAC): спека статична, mount ВНЕ /v1. Страница
	//     /docs фетчит .json с Bearer-заголовком (RapiDoc рендерит объект инлайн).
	//
	// /openapi.yaml и /openapi.json отдают runtime-дамп huma-агрегатора (3.1,
	// «правда в коде») из ОДНОГО source-of-truth (servedOpenAPIHandler /
	// servedOpenAPIJSONHandler) — кеш собирается один раз. YAML — людям/тулам,
	// JSON — вьюверу /docs. Committed docs/keeper/openapi.yaml — производный
	// huma-генерат для UI-vendor (make gen-openapi), он НЕ served и НЕ embed-ится.
	r.Get("/healthz", healthH.Healthz)
	r.Get("/readyz", healthH.Readyz)
	r.With(apimiddleware.RequireJWT(verifier)).Get("/openapi.yaml", servedOpenAPIHandler)
	r.With(apimiddleware.RequireJWT(verifier)).Get("/openapi.json", servedOpenAPIJSONHandler)
	mountDocsViewer(r)

	// /ui — встроенный UI (ADR-055), публичный mount ВНЕ /v1 (parity /docs):
	// статика go:embed без JWT/RBAC/audit; защищён API, не статика. Монтируется
	// ТОЛЬКО при включённом тоггле web_ui_enabled (default-ON, резолв
	// [config.KeeperConfig.WebUIMounted]); при явном `false` /ui не подключается
	// → 404 (API-периметр /v1 не затрагивается ни в одном случае).
	if webUIEnabled {
		webui.Mount(r)
	}

	// /auth/* — федеративная аутентификация (ADR-058) ВНЕ /v1: публичный вход
	// (сам логин, JWT ещё нет — RequireJWT неприменим, parity /healthz). Монтируется
	// ТОЛЬКО при non-nil ldapAuth (keeper.yml::auth.ldap задан); иначе способ логина
	// недоступен (ADR-053 OPTIONAL-tier). Anti-DoS body-limit стоит (credentials —
	// малый JSON), но без metrics/RBAC/audit-middleware (/v1-обвязка): audit логина
	// пишет сам handler (operator.login).
	if ldapAuth != nil {
		r.Route("/auth", func(r chi.Router) {
			r.Use(maxBodyMiddleware(v1RequestBodyLimit))
			registerHumaLDAPLogin(newHumaAuthAPI(r), ldapAuth)
		})
	}

	// /v1/* — auth + RBAC + audit. Selector-extractor для operator
	// endpoints — NoSelector (rbac.md не определяет селекторы для
	// permission `operator.*`).
	r.Route("/v1", func(r chi.Router) {
		// Anti-DoS: лимит на body Request-а. Operator endpoints — JSON
		// объёмом ~200 байт; ставим v1RequestBodyLimit с запасом и
		// одновременно отсекаем «отправлю гигабайт мусора».
		// MaxBytesReader при превышении лимита подменяет Read на
		// http.MaxBytesError; json.Decoder получит её и handler вернёт
		// 400 problem+json (TypeMalformedRequest).
		r.Use(maxBodyMiddleware(v1RequestBodyLimit))
		// HTTP-метрики — внутри /v1, чтобы chi уже знал RoutePattern
		// (без него label `path` = raw URL → cardinality-blow-up). path-
		// extractor читает chi.RouteContext, заполненный chi-router-ом
		// после match-а; для catch-all `/v1/*` ниже RoutePattern будет
		// `/v1/*` — это допустимо (cardinality стабильная).
		if metricsHTTP != nil {
			r.Use(metricsHTTP.MiddlewareForPath(routePatternFromChi))
		}
		r.Use(apimiddleware.RequireJWT(verifier))

		// FULL-TYPED huma (ADR-054, ТИРАЖ-БАТЧ-2a домена operator целиком по 5
		// эталонам): create/revoke/issue-token — WRITE+AUDIT вариант B (huma-audit-
		// middleware: full-typed huma САМ пишет ответ, StatusRecorder из
		// apimiddleware.Audit неприменим — audit держит hctx.Status() + carrier-
		// payload, иначе рецидив S6); list — read-with-typed-query (БЕЗ audit, bad
		// auth_method enum→422, revoked bool→400, pagination int32→400); get —
		// read-with-path. Каждый write-роут — СВОЯ chi-группа с собственным event-
		// типом (newHumaOperatorAPI(evt)). RequirePermission — chi-middleware группы
		// (huma наследует). Все operator-роуты обслуживает huma. MCP operator-tools
		// зовут operator.Service напрямую (мимо handler).
		r.Route("/operators", func(r chi.Router) {
			r.With(
				apimiddleware.RequirePermission(enforcer, "operator", "create", apimiddleware.NoSelector),
			).Group(func(r chi.Router) {
				registerHumaOperatorCreate(newHumaOperatorAPI(r, auditWriter, audit.EventOperatorCreated, logger), opH)
			})

			// GET /v1/operators — list (UI iteration 2 /archons-list).
			// Permission operator.list, NoSelector. Read-only — без audit.
			r.With(
				apimiddleware.RequirePermission(enforcer, "operator", "list", apimiddleware.NoSelector),
			).Group(func(r chi.Router) {
				registerHumaOperatorList(newHumaCadenceAPI(r), opH)
			})

			// GET /v1/operators/{aid} — detail. Permission operator.list (одна
			// permission покрывает list+get, паттерн soul.list/service.list — read
			// без отдельного operator.read в MVP). Read-only — без audit. huma-op
			// несёт полный путь /{aid} (НЕ вложен в r.Route("/{aid}") — иначе chi
			// удвоил бы префикс).
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

		// /v1/audit — read-only-лента audit-events для UI iteration 2 (/audit).
		// Permission audit.read, NoSelector. Read без Audit-middleware (избегаем
		// рекурсии: каждое чтение писало бы свою же запись в audit_log).
		// Подключается ТОЛЬКО при non-nil auditH (паттерн pushH/errandH); drift-
		// test собирает router с auditH=nil → роут попадает в pathAllowlist.
		//
		// FULL-TYPED huma (ADR-054 §Pattern ЧЕТВЁРТЫЙ tier — read-with-typed-query,
		// ЭТАЛОН ~13-15 list-эндпоинтов): huma биндит/валидирует typed-query →
		// ListTyped → typed envelope-output. READ-вариант (БЕЗ huma-audit-middleware).
		// Контракт сохранён (решение A, продолжение ADR-051 Amendment): bad date-time/
		// offset/limit query → 400 TypeMalformedRequest (error-override
		// hasQueryParseError); bad source-enum → 422 TypeValidationFailed (schema-
		// validate enum-mismatch, не parse). audit обслуживает huma.
		// RequirePermission(audit.read) — chi-middleware группы (huma наследует).
		if auditH != nil {
			r.With(
				apimiddleware.RequirePermission(enforcer, "audit", "read", apimiddleware.NoSelector),
			).Group(func(r chi.Router) {
				registerHumaAuditList(newHumaCadenceAPI(r), auditH)
			})
		}

		// /v1/roles — RBAC-CRUD (роли / permissions / membership), Slice 2a.
		// Подключается только при non-nil roleH (Deps.RBACSvc прокинут).
		// Selector-extractor — NoSelector: rbac.md не определяет селекторы для
		// permission `role.*` (как и для `operator.*`).
		//
		// FULL-TYPED huma (ADR-054, ПЕРВЫЙ ТИРАЖ-БАТЧ домена целиком по двум
		// эталонам pilot-1/pilot-2): ВСЕ role-роуты на huma. READ (list) — по
		// READ-варианту pilot-1 (typed output, БЕЗ audit). WRITE (create/delete/
		// update-permissions/grant/revoke-operator) — по pilot-2 (typed I/O +
		// huma-audit-middleware вариант B: full-typed huma САМ пишет ответ, поэтому
		// StatusRecorder из apimiddleware.Audit неприменим — audit держит
		// humaAuditMiddleware, читающий hctx.Status() + carrier-payload, иначе
		// рецидив S6). Каждый write-роут — СВОЯ chi-группа с собственным event-типом
		// (newHumaRoleAPI(evt)). RequirePermission — chi-middleware группы (huma
		// наследует). Все role-роуты обслуживает huma.
		if roleH != nil {
			r.Route("/roles", func(r chi.Router) {
				r.With(
					apimiddleware.RequirePermission(enforcer, "role", "create", apimiddleware.NoSelector),
				).Group(func(r chi.Router) {
					registerHumaRole(newHumaRoleAPI(r, auditWriter, audit.EventRoleCreated, logger), roleH)
				})

				// GET /v1/roles — READ, без audit (паттерн role.list).
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

		// /v1/synods — Synod-CRUD (группы / membership / bundle), ADR-049.
		// Подключается только при non-nil synodH (Deps.RBACSvc прокинут).
		// Selector — NoSelector: synod.* — кластер-уровневая операция без scope
		// по coven/host (как role.* / operator.*; group-scope ADR-049 НЕ вводит).
		//
		// Audit-middleware на 7 мутирующих роутах (RBAC-топология аудируется,
		// ADR-022). `synod.list` — read-only, без audit. Бизнес-логика
		// (builtin-граница, least-privilege subset на add-operator/grant-role,
		// self-lockout на delete/remove-operator/revoke-role) — в rbac.Service.
		//
		// FULL-TYPED huma (ADR-054, ТИРАЖ-БАТЧ-2d домена synod целиком по эталонам
		// role/operator/augur/herald): create/update/delete + add/remove-operator +
		// grant/revoke-role — WRITE+AUDIT вариант B (huma-audit-middleware: full-typed
		// huma САМ пишет ответ, StatusRecorder из apimiddleware.Audit неприменим —
		// audit держит hctx.Status() + carrier-payload, иначе рецидив S6); list —
		// read (БЕЗ audit). Sub-resource роуты (/operators, /roles[/...]) несут полный
		// путь в huma-операции (форма role-домена: единый resource-group). Каждый
		// write-роут — СВОЯ chi-группа с собственным event-типом (newHumaSynodAPI(evt)).
		// RequirePermission — chi-middleware группы (huma наследует). Все synod-роуты
		// обслуживает huma. MCP synod-tools зовут rbac.Service напрямую (мимо handler).
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
		// Selector-стратегия RBAC (ADR-008 amendment a + ADR-047 §г):
		//   - List/Get/History — [RequireAction] existence-gate (ADR-047 §г):
		//     scope-aware [RequirePermission]/[RequirePermissionMulti] деньит
		//     scoped-оператора, когда selector-измерение НЕ резолвится в
		//     request-контексте (state/regex/soulprint вообще не извлекаются из
		//     incarnation-строки; coven-scoped матчит, но state-scoped — нет),
		//     отрезая оператора от собственной видимости ДО handler-а. RequireAction
		//     спрашивает лишь о НАЛИЧИИ права (`incarnation.{list,get,history}`);
		//     сужение по scope делает handler после фетча строки
		//     (ResolveListScopeFor для list, GetInScopeFor для get/history —
		//     coven∪{name} + state-CEL). Revoked-покрытие — через тот же
		//     revoked-aware [rbac.Enforcer.ResolvePurview] (gate HoldsAction→Deny
		//     →403, handler Deny→Empty→404).
		//   - Create — [handlers.IncarnationCreateScopeSelector]: scope из тела
		//     (service= + multi-value coven= из declared covens ∪ {name}) —
		//     coven-scoped оператор не создаст incarnation с тегом вне scope.
		//   - Run/Unlock/Upgrade/Destroy/… — [handlers.IncarnationScopeSelector]
		//     (multi-context): читает incarnation по path-{name} и приземляет
		//     incarnation= + service= + multi-value coven= (covens ∪ {name}).
		//     RequirePermissionMulti OR-ит контексты — роли `incarnation.* on
		//     coven=…` / `on service=…` матчат (mutate-роуты не имеют state-scoped-
		//     дыры read-а: их scope в MVP — coven/service/incarnation, не state).
		//
		// FULL-TYPED huma (ADR-054, ТИРАЖ-БАТЧ-2g домена incarnation ЦЕЛИКОМ — MIXED
		// audit-класс): create/run/unlock/upgrade — WRITE-MIDDLEWARE-AUDIT вариант B
		// (newHumaIncarnationAPI(evt) — huma САМ пишет ответ, audit держит hctx.Status()
		// + carrier-payload из *Typed-reply.AuditPayload, иначе рецидив S6); rerun-create/
		// check-drift/destroy/update-hosts — WRITE-SELF-AUDIT (audit пишет САМ handler
		// ВНУТРИ *Typed через h.auditW.Write — payload собирается после доменной операции;
		// audit-middleware НЕ навешан, newHumaCadenceAPI); list/get/history — read (БЕЗ
		// audit). ТОПОЛОГИЯ: chi.Route("/{name}") СНЯТ — все incarnation-op несут ПОЛНЫЙ
		// путь /{name}[/...] на группе /v1/incarnations (иначе sibling-затенение узла
		// /{name} → 405, блокер батча-2f). Сосуществует с choir-mount (батч-2f) на ТОЙ ЖЕ
		// группе. Каждый write-роут — СВОЯ chi-группа со своим RBAC/event (Toll на run);
		// huma наследует chi-middleware. MCP incarnation-tools зовут incarnation.*
		// домен напрямую (мимо handler) — целостность сохранена.
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

			r.With(
				apimiddleware.RequireAction(enforcer, "incarnation", "history"),
			).Group(func(r chi.Router) {
				registerHumaIncarnationHistory(newHumaCadenceAPI(r), incH)
			})

			// POST /v1/incarnations/{name}/scenarios/{scenario} — запуск именованного
			// scenario. Блокируется Toll-middleware при cluster:degraded (ADR-038):
			// 503 + Retry-After. Toll-middleware ПЕРВЫМ в chain (outermost), чтобы 503
			// на degraded-кластере вернулся ДО RBAC/Audit: блокированный запрос не
			// должен ни тратить permission-check, ни писать audit-event scenario_started.
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

			// POST /v1/incarnations/{name}/rerun-create — снять error_locked + перезапустить
			// scenario `create`. WRITE-SELF-AUDIT: incarnation.create_rerun пишет сам handler
			// (payload previous_status известен только после UnlockForRerun; audit-middleware
			// НЕ навешан). Permission incarnation.create-rerun, scope incScope.
			r.With(
				apimiddleware.RequirePermissionMulti(enforcer, "incarnation", "create-rerun", incScope),
			).Group(func(r chi.Router) {
				registerHumaIncarnationRerunCreate(newHumaCadenceAPI(r), incH)
			})

			// POST /v1/incarnations/{name}/check-drift — Scry on-demand (ADR-031, Slice B).
			// WRITE-SELF-AUDIT: incarnation.drift_checked пишет сам handler (payload —
			// drift_summary — после CheckDrift; audit-middleware НЕ навешан). Permission
			// incarnation.check-drift, scope incScope.
			r.With(
				apimiddleware.RequirePermissionMulti(enforcer, "incarnation", "check-drift", incScope),
			).Group(func(r chi.Router) {
				registerHumaIncarnationCheckDrift(newHumaCadenceAPI(r), incH)
			})

			// DELETE /v1/incarnations/{name} — destroy (S-D4). WRITE-SELF-AUDIT:
			// destroy_started пишет сам service-слой [incarnation.Destroy] (нужны
			// source/previous_status/force, недоступные middleware-у однообразно);
			// audit-middleware НЕ навешан. Permission incarnation.destroy, scope incScope.
			r.With(
				apimiddleware.RequirePermissionMulti(enforcer, "incarnation", "destroy", incScope),
			).Group(func(r chi.Router) {
				registerHumaIncarnationDestroy(newHumaCadenceAPI(r), incH)
			})

			// PATCH /v1/incarnations/{name}/hosts — редактирование declared spec.hosts[]
			// (ADR-008). Permission incarnation.update-hosts (сужена с incarnation.update,
			// PM-decision 2026-06-02; backcompat-alias канонизируется на load снимка), scope
			// incScope. WRITE-SELF-AUDIT: incarnation.hosts_updated пишет сам handler (payload
			// old/new snapshot после UpdateHosts; audit-middleware НЕ навешан).
			r.With(
				apimiddleware.RequirePermissionMulti(enforcer, "incarnation", "update-hosts", incScope),
			).Group(func(r chi.Router) {
				registerHumaIncarnationUpdateHosts(newHumaCadenceAPI(r), incH)
			})

			// /v1/incarnations/{name}/choirs — CRUD топологии Choir/Voice (ADR-044,
			// S-T3). Choir принадлежит инкарнации → тот же scope-селектор incScope
			// (incarnation/service/coven по path-{name}), что у incarnation-мутаций.
			// resource — `choir`; actions — create / delete / list + add-voice /
			// remove-voice. Подключается ТОЛЬКО при non-nil choirH (паттерн errandH):
			// keeper без ChoirDB-пула отдаёт 404, drift-test держит пути в
			// pathAllowlist.
			//
			// FULL-TYPED huma (ADR-054, БАТЧ-2f WRITE-SELF-AUDIT): create/delete/
			// add-voice/remove-voice пишут audit (choir.created/.deleted/.voice_added/
			// .voice_removed) САМ handler через writeAuditCtx ВНУТРИ CreateTyped/
			// DeleteTyped/AddVoiceTyped/RemoveVoiceTyped — audit-middleware НЕ навешан
			// (отличие от middleware-audit-доменов role/operator). newHumaCadenceAPI
			// (БЕЗ audit-навески). Multi-resource: voices — sub-resource; huma-op несёт
			// ПОЛНЫЙ путь /{name}/choirs[/...] относительно группы /v1/incarnations (НЕ
			// вложен в chi.Route("/{name}") — иначе chi удвоил бы {name}-префикс,
			// паттерн soul/synod multi-resource; huma биндит {name}/{choir}/{sid} сам,
			// chi-RBAC-селектор incScope читает их из humachi-паттерна). list/list-voices
			// — read (БЕЗ audit). Каждый роут — СВОЯ chi-группа со своим RBAC; huma
			// наследует. MCP choir НЕТ.
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

				// list (choirs) + list-voices — read под одним choir.list RBAC, общая
				// huma.API (distinct-path исключает коллизию двух GET-ов).
				r.With(
					apimiddleware.RequirePermissionMulti(enforcer, "choir", "list", incScope),
				).Group(func(r chi.Router) {
					choirReadAPI := newHumaCadenceAPI(r)
					registerHumaChoirList(choirReadAPI, choirH)
					registerHumaVoiceList(choirReadAPI, choirH)
				})
			}
		})

		// /v1/souls — онбординг + реестр (M2.x): Create + List + issue-token.
		//
		// Selector-стратегия:
		//   - Create — NoSelector (RBAC решает bare permission; coven-селектор
		//     придёт при появлении per-coven-RBAC на регистрацию).
		//   - List / Get / soulprint / history — [RequireAction] existence-gate
		//     (ADR-047 §г G1): scope-aware [RequirePermission] деньит scoped-
		//     оператора при пустом контексте (селектор-ключа нет в nil-контексте),
		//     отрезая его от собственного списка ДО handler-а. RequireAction
		//     спрашивает лишь о НАЛИЧИИ `soul.list`; сужение по scope делает handler
		//     после фетча строк (resolveListScope / readScope + soulpurview).
		//   - issue-token — [handlers.SoulSIDSelector] (`host=<sid>`), RBAC
		//     может ограничить ре-выписку по конкретному хосту.
		//
		// FULL-TYPED huma (ADR-054, ТИРАЖ-БАТЧ-2e домена soul по эталонам role/operator +
		// audit-endpoint): create/coven-assign/issue-token/ssh-target/exec — WRITE+AUDIT
		// вариант B (newHumaSoulAPI(evt)); list/get/soulprint/history — read (БЕЗ audit).
		// Каждый write-роут — СВОЯ chi-группа со своим RBAC+event; reads группируются по
		// RBAC. huma наследует chi-middleware группы. ВСЕ soul-detail-роуты
		// (/souls/{sid}/*) на huma. MCP soul-tools зовут soul.Service/bootstraptoken
		// напрямую (мимо handler). POST /souls/{sid}/exec — теперь huma (errand.invoked,
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
				registerHumaSoulList(newHumaCadenceAPI(r), soulH)
			})

			// POST /v1/souls/coven — bulk coven-assign (ТЗ-пилот). Двухслойная
			// авторизация:
			//   1. middleware RequirePermission(soul, coven-assign) — первый
			//      гейт «есть ли право вообще». Селектор — SoulCovenLabelSelector
			//      (`coven=<label>` из body): scope-проверка назначаемой метки
			//      (гейт b) — coven-scoped оператор проходит только для метки в
			//      своём scope; bare/`*` — для любой.
			//   2. service-слой soul.BulkAssignCoven — scope-intersection (гейт a):
			//      целевые хосты ⊆ coven-scope оператора (CovenScope из enforcer).
			// Audit — EventSoulCovenChanged с source=api (различение от scenario-
			// пути по source); payload handler выставляет через SetAuditPayload.
			r.With(
				apimiddleware.RequirePermission(enforcer, "soul", "coven-assign", handlers.SoulCovenLabelSelector),
			).Group(func(r chi.Router) {
				registerHumaSoulCovenAssign(newHumaSoulAPI(r, auditWriter, audit.EventSoulCovenChanged, logger), soulH)
			})

			// GET /v1/souls/{sid} + /soulprint + /history — single-soul read для UI
			// detail-page. Permission `soul.list` покрывает list+get+soulprint+history
			// (паттерн service.list / omen.list — одно permission на чтение реестра;
			// `soul.get` сознательно отложен, rbac.md §Souls). [RequireAction] existence-gate
			// (ADR-047 §г G1): scope-aware gate отрезал бы scoped-оператора (host-контекст
			// резолвится из строки БД, которой нет до фетча); сужение по scope делает handler
			// (readScopeForClaims + soulpurview.InScope → 404 вне scope). Read-only — без
			// Audit. huma-ops несут полный путь /{sid}[/…] (НЕ вложены в r.Route("/{sid}") —
			// иначе chi удвоил бы {sid}-префикс, паттерн operator-домена); huma биндит {sid}
			// сам, chi-RBAC-селекторы читают его из зарегистрированного humachi-паттерна.
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

			// PUT /v1/souls/{sid}/ssh-target — обновить per-host SSH-реквизиты push-flow
			// (ADR-032 amendment 2026-05-26, S7-1). Permission `soul.ssh-target-update`
			// (action — hyphenated). Селектор SoulSIDSelector — `host=<sid>`. Audit
			// EventSoulSshTargetUpdated; payload — huma-вариант B (SetHumaAuditPayload).
			r.With(
				apimiddleware.RequirePermission(enforcer, "soul", "ssh-target-update", handlers.SoulSIDSelector),
			).Group(func(r chi.Router) {
				registerHumaSoulSshTarget(newHumaSoulAPI(r, auditWriter, audit.EventSoulSshTargetUpdated, logger), soulH)
			})

			// POST /v1/souls/{sid}/exec — pull-ad-hoc Errand (ADR-033, slice E2). Permission
			// errand.run, selector `host=<sid>` (rbac.md §Errand). FULL-TYPED huma (ADR-054,
			// БАТЧ-2e): WRITE+AUDIT вариант B (newHumaSoulAPI с event errand.invoked) с dual-
			// status 200 sync / 202 async + Location-header. Audit-middleware пишет
			// EventTypeErrandInvoked на ОБА 2xx; dispatcher сам пишет audit-event внутри
			// Dispatch (single source of truth) — middleware-event navigation-trail. При nil
			// errandH не подключается. huma-op несёт полный путь /{sid}/exec (НЕ вложен в
			// r.Route("/{sid}") — иначе chi удвоил бы {sid}-префикс; huma биндит {sid} сам,
			// chi-RBAC-селектор ErrandSIDSelector читает его из humachi-паттерна). Все
			// soul-detail-роуты на huma.
			if errandH != nil {
				r.With(
					apimiddleware.RequirePermission(enforcer, "errand", "run", handlers.ErrandSIDSelector),
				).Group(func(r chi.Router) {
					registerHumaSoulExec(newHumaSoulAPI(r, auditWriter, audit.EventTypeErrandInvoked, logger), errandH)
				})
			}
		})

		// /v1/plugins/sigils — Sigil allow-list целостности плагинов
		// (plugin.allow/revoke/list, ADR-026 S4a). Подключается только при
		// non-nil sigilH (Deps.SigilSvc прокинут). Selector — NoSelector:
		// rbac.md не определяет селекторы для plugin.* (как operator.*/role.*).
		//
		// Audit на allow/revoke (supply-chain-мутации обязательно аудируются).
		// list — read-only, без audit. payload handler-ы выставляют через
		// SetAuditPayload (caller AID, namespace/name/ref, sha256; без
		// signature/manifest).
		//
		// FULL-TYPED huma (ADR-054, ТИРАЖ-БАТЧ-2a домена sigil целиком по эталонам
		// role): allow/revoke — WRITE+AUDIT вариант B (huma-audit-middleware; event-
		// домен permission `plugin`, события plugin.allowed/plugin.revoked); list —
		// read-bare (БЕЗ audit). Каждый write-роут — СВОЯ chi-группа с собственным
		// event-типом (newHumaSigilAPI(evt)). RequirePermission — chi-middleware
		// группы (huma наследует). MCP plugin-tools зовут sigil.Service напрямую
		// (мимо handler).
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

		// /v1/sigil/keys — ротация trust-anchor-ключей ПОДПИСИ Sigil (ADR-026(h),
		// R3-S7). Отдельная зона от /v1/plugins/sigils (тот про допуски бинарей,
		// этот — про ключи их подписи). Подключается только при non-nil sigilKeyH
		// (Deps.SigilKeySvc прокинут — production-wire-up при включённом Sigil).
		// Selector — NoSelector (как plugin.*/operator.*).
		//
		// Audit на introduce/set-primary/retire (ротация ключей подписи —
		// supply-chain-критично). list — read-only, без audit. payload handler-ы
		// выставляют через SetAuditPayload (key_id + caller AID; БЕЗ приватника).
		//
		// FULL-TYPED huma (ADR-054, ТИРАЖ-БАТЧ-2a домена sigil-key целиком по
		// эталонам role): introduce/set-primary/retire — WRITE+AUDIT вариант B
		// (huma-audit-middleware; события sigil.key-introduced/sigil.key-primary-set/
		// sigil.key-retired); list — read-bare (БЕЗ audit). Каждый write-роут — СВОЯ
		// chi-группа с собственным event-типом (newHumaSigilKeyAPI(evt)).
		// RequirePermission — chi-middleware группы (huma наследует). MCP
		// sigil-key-tools зовут sigil.KeyService напрямую (мимо handler).
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

		// /v1/services — реестр Service-ов (service.register/update/list/
		// deregister, ADR-028-паттерн RBAC-storage). Подключается только при
		// non-nil serviceH (Deps.ServiceSvc прокинут). Selector — NoSelector:
		// service.* CRUD оперирует самим реестром (register/list/deregister
		// записи), без таргетинга по имени-сервиса в S3 (как operator.*/role.*).
		//
		// Audit на 3 мутирующих роутах (register/update/deregister). list/get —
		// read-only, без audit (как role.list / plugin.list). payload handler-ы
		// выставляют через SetAuditPayload (name + git/ref + caller AID; git-URL
		// не секрет).
		//
		// Permission-маппинг: POST→service.register, GET→service.list (и для
		// list, и для get-{name}), PATCH→service.update, DELETE→service.deregister.
		//
		// FULL-TYPED huma (ADR-054, ТИРАЖ-БАТЧ-2d домена service целиком по эталонам
		// role/operator/augur/herald): register/update/deregister — WRITE+AUDIT
		// вариант B (huma-audit-middleware: full-typed huma САМ пишет ответ,
		// StatusRecorder из apimiddleware.Audit неприменим — audit держит
		// hctx.Status() + carrier-payload, иначе рецидив S6; register/update — 201/200
		// С ТЕЛОМ); list/get + refs/scenarios/state-schema/dependencies — read (БЕЗ
		// audit; sub-reads несут полный путь /{name}/<...> в huma-операции + опц.
		// ?ref=, tier 502 на git-loader). Каждый write-роут — СВОЯ chi-группа с
		// собственным event-типом (newHumaServiceAPI(evt)). RequirePermission —
		// chi-middleware группы (huma наследует). Все service-роуты обслуживает huma.
		// MCP service-tools зовут serviceregistry.Service напрямую (мимо handler).
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
				// покрыт list-правом). huma-op несёт полный путь /{name} (НЕ вложен в
				// r.Route("/{name}") — иначе chi удвоил бы префикс).
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

				// /refs — git-tag-и + branch-и для UI Upgrade-modal (read-only,
				// permission service.list — refs суть проекция Service-записи, без
				// audit, как Get/List). 502 → внешний git-источник упал.
				r.With(
					apimiddleware.RequirePermission(enforcer, "service", "list", apimiddleware.NoSelector),
				).Group(func(r chi.Router) {
					registerHumaServiceRefs(newHumaCadenceAPI(r), serviceH)
				})

				// /scenarios — listing scenario из материализованного снапшота git-репо
				// Service-а для UI Run-modal dropdown. permission service.list. 502 →
				// loader (git-clone / parse) упал.
				r.With(
					apimiddleware.RequirePermission(enforcer, "service", "list", apimiddleware.NoSelector),
				).Group(func(r chi.Router) {
					registerHumaServiceScenarios(newHumaCadenceAPI(r), serviceH)
				})

				// /state-schema — state_schema-метаданные сервиса для UI Schema
				// explorer-а. permission service.list. 502 → loader упал.
				r.With(
					apimiddleware.RequirePermission(enforcer, "service", "list", apimiddleware.NoSelector),
				).Group(func(r chi.Router) {
					registerHumaServiceStateSchema(newHumaCadenceAPI(r), serviceH)
				})

				// /dependencies — git-зависимости сервиса (destiny-кирпичики + custom-
				// модули из service.yml) для UI Service Detail. permission service.list.
				// 502 → loader упал.
				r.With(
					apimiddleware.RequirePermission(enforcer, "service", "list", apimiddleware.NoSelector),
				).Group(func(r chi.Router) {
					registerHumaServiceDependencies(newHumaCadenceAPI(r), serviceH)
				})
			})
		}

		// /v1/provisioning-policy — runtime-политика способов СОЗДАНИЯ операторов
		// (provisioning_allowed_methods, ADR-058 Часть B). Подключается только при
		// non-nil provisioningPolicyH (Deps.ProvisioningPolicyReader + ServiceSvc
		// прокинуты). Selector — NoSelector: политика кластер-уровневая (как
		// operator.* / role.*).
		//
		// GET — read (permission provisioning.read, БЕЗ audit, паттерн service.list).
		// PUT — WRITE+AUDIT вариант B (permission provisioning.update, event
		// provisioning.policy_changed; huma-audit-middleware на своей chi-группе,
		// как service.update). Каждый роут — СВОЯ chi-группа со своим RBAC; huma
		// наследует chi-middleware группы.
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

		// /v1/modules — module-catalog (core registry doc-data + активные plugin-
		// допуски), UI Run→Command module-search. Permission service.list (read-
		// only-каталог, без audit — паттерн service.list / plugin.list); новая
		// permission не заводится (reuse предпочтительнее). Selector — NoSelector
		// (каталог глобальный, не per-resource). moduleCatalogH non-nil всегда
		// (core-каталог не требует внешних зависимостей), поэтому роуты в спеке и
		// роутере совпадают без allowlist (в отличие от opt-in plugin.*).
		//
		// FULL-TYPED huma (ADR-054, ТИРАЖ-БАТЧ-2e домена module целиком по эталону
		// catalog read-bare + form-prep read-with-body): list/get — read-каталог; form-prep
		// — read-резолв SID под форму. ВСЕ ТРИ — READ-only, audit НЕ навешивается. Каждый
		// роут — СВОЯ chi-группа со своим RBAC; huma наследует chi-middleware группы. MCP
		// module-домена НЕТ (каталог без MCP-tool-ов).
		if moduleCatalogH != nil {
			r.Route("/modules", func(r chi.Router) {
				r.With(
					apimiddleware.RequirePermission(enforcer, "service", "list", apimiddleware.NoSelector),
				).Group(func(r chi.Router) {
					moduleReadAPI := newHumaCadenceAPI(r)
					registerHumaModuleList(moduleReadAPI, moduleCatalogH)
					registerHumaModuleGet(moduleReadAPI, moduleCatalogH)
				})

				// /{name}/form-prep — резолвер source-каталогов UI-формы модуля
				// (ADR-045 S3): source incarnation_hosts/choir → живые SID-ы для
				// автокомплита формы Run→Command. Permission incarnation.run —
				// эндпоинт обслуживает подготовку прогона команды (кто запускает
				// прогон, тот и резолвит SID-ы под его поля); reuse под-прогонной
				// permission, новая не заводится. Selector — NoSelector (резолв
				// cluster-wide по souls, не per-resource RBAC-scope). Без audit
				// (read-only-резолв, паттерн soul.list / service.list).
				// Монтируется только при non-nil moduleFormPrepH (Deps.Pool
				// прокинут); drift-test собирает router с nil → роут в allowlist.
				if moduleFormPrepH != nil {
					r.With(
						apimiddleware.RequirePermission(enforcer, "incarnation", "run", apimiddleware.NoSelector),
					).Group(func(r chi.Router) {
						registerHumaModuleFormPrep(newHumaCadenceAPI(r), moduleFormPrepH)
					})
				}
			})
		}

		// /v1/permissions — машиночитаемый каталог RBAC-permissions (источник —
		// rbac.catalog.go). UI фетчит реальные имена для назначения прав роли
		// (фикс бага hardcoded-permission → unknown_permission). RBAC — ТОЛЬКО
		// auth (RequireJWT на /v1/* выше), БЕЗ RequirePermission: каталог само-
		// описывающий, требование права на чтение списка прав = курица-яйцо
		// (architect-вердикт). Read-only, без audit (паттерн health/meta). permCatalogH
		// non-nil всегда (статика из пакета rbac, без внешних зависимостей),
		// поэтому роут в спеке и роутере совпадают без allowlist (как /v1/modules).
		//
		// FULL-TYPED huma (ADR-054, БАТЧ-1 read-tier): три READ-каталога
		// (permissions / event-types / me-permissions) на ОДНОЙ huma.API поверх
		// группы /v1 (auth-only — RequireJWT на /v1/* выше, БЕЗ RequirePermission:
		// само-описывающие, требование права на чтение списка = «курица-яйцо»,
		// architect-вердикт). Операции несут абсолютные-под-/v1 пути
		// (/permissions / /event-types / /me/permissions) → chi.Walk видит
		// /v1/<path>, drift-test зелёный; distinct-path исключает коллизию трёх
		// операций на общей API. Read-only — БЕЗ audit-middleware. Strict-методы
		// ListPermissions/ListEventTypes/ListMyPermissions остаются generated (до
		// финального сноса), из mount сняты.
		//
		// /v1/permissions — каталог RBAC-permissions (источник rbac.catalog.go); UI
		// фетчит реальные имена для назначения прав роли (фикс unknown_permission).
		// /v1/event-types — каталог event-types для Tiding-подписки (источник
		// herald/eventtypes.go; фикс ADR-042 UI-хардкода).
		// /v1/me/permissions — эффективные права ТЕКУЩЕГО Архонта (AID из claims, не
		// query; чужие не отдаёт), для permission-aware UI; nil-claims → 500
		// problem+json (parity доменного Get). Все три handler-а non-nil всегда
		// (статика rbac/herald + enforcer-снимок), поэтому роуты в спеке и роутере
		// совпадают без allowlist (как /v1/modules).
		catalogAPI := newHumaCadenceAPI(r)
		registerHumaPermissionsList(catalogAPI, permCatalogH)
		registerHumaEventTypesList(catalogAPI, eventTypeCatalogH)
		registerHumaMyPermissionsList(catalogAPI, meH)

		// /v1/augur — реестры Augur (omens / rites, ADR-025). Подключается
		// только при non-nil augurH (Deps.AugurSvc прокинут). Selector —
		// NoSelector: omen.*/rite.* оперируют самим реестром, без таргетинга по
		// имени-Omen-а в MVP (как service.*/role.*).
		//
		// Audit на 4 мутирующих роутах (omen create/delete + rite create/delete).
		// list/get — read-only, без audit. payload handler-ы выставляют через
		// SetAuditPayload (name/source_type/endpoint/auth_ref для omen — не
		// секрет; omen/subject/delegate для rite — не секрет; allow / значения
		// секретов НЕ кладутся, augur.md §8).
		//
		// Permission-маппинг: POST omens→omen.create, GET omens(+{name})→omen.list,
		// DELETE omens/{name}→omen.delete; POST rites→rite.create, GET rites→
		// rite.list, DELETE rites/{id}→rite.delete.
		//
		// FULL-TYPED huma (ADR-054, ТИРАЖ-БАТЧ-2b домена augur целиком по эталонам
		// role/operator): omen create/delete + rite create/delete — WRITE+AUDIT
		// вариант B (huma-audit-middleware; full-typed huma САМ пишет ответ, поэтому
		// StatusRecorder из apimiddleware.Audit неприменим — audit держит
		// hctx.Status() + carrier-payload, иначе рецидив S6). omen list/get + rite
		// list — read (БЕЗ audit; list — read-with-typed-query int32-пагинация→400,
		// rite list — обязательный omen=query→422). Каждый write-роут — СВОЯ chi-группа
		// с собственным event-типом (newHumaAugurAPI(evt)). RequirePermission —
		// chi-middleware группы (huma наследует). MCP augur-tools зовут augur.Service
		// напрямую (мимо handler).
		//
		// chi-группа /v1/augur + относительные huma-op-пути /omens[/{name}] и
		// /rites[/{id}] (НЕ вложенные chi.Route /omens //rites): per-route huma-op
		// несёт полный под-/augur путь → chi.Walk видит /v1/augur/omens и т.д.
		// (drift-test зелёный), distinct-path исключает коллизию omen-POST/rite-POST
		// на общей spec-dump-API (оба иначе осели бы на одном пути «/»).
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

		// /v1/vigils + /v1/decrees — реестры Oracle (beacons, ADR-030 S3).
		// Подключается только при non-nil oracleH (Deps.OracleSvc прокинут).
		// Selector — NoSelector: vigil.*/decree.* оперируют самим реестром, без
		// таргетинга по имени в MVP (как augur.*/service.*).
		//
		// Audit на 4 мутирующих роутах (vigil create/delete + decree create/delete).
		// list/get — read-only, без audit. payload handler-ы выставляют через
		// SetAuditPayload (name/check/interval/subject для vigil; name/on_beacon/
		// incarnation/scenario/subject для decree — не секрет; params / where-CEL /
		// action_input НЕ кладутся, action_input может транзитом нести vault-ref).
		//
		// Permission-маппинг: POST vigils→vigil.create, GET vigils(+{name})→vigil.list,
		// DELETE vigils/{name}→vigil.delete; symmetric для decrees.
		//
		// FULL-TYPED huma (ADR-054, ТИРАЖ-БАТЧ-2b домена oracle целиком по эталонам
		// role/operator/augur): vigil create/delete + decree create/delete — WRITE+AUDIT
		// вариант B (huma-audit-middleware; full-typed huma САМ пишет ответ, поэтому
		// StatusRecorder из apimiddleware.Audit неприменим — audit держит
		// hctx.Status() + carrier-payload, иначе рецидив S6). vigil/decree list/get —
		// read (БЕЗ audit; list — read-with-typed-query int32-пагинация→400). Каждый
		// write-роут — СВОЯ chi-группа с собственным event-типом (newHumaOracleAPI(evt)).
		// huma-op несёт ПОЛНЫЙ путь /vigils[/{name}]//decrees[/{name}] → группы
		// монтируются прямо на /v1 (distinct-path для spec-dump, иначе vigil-POST и
		// decree-POST осели бы на одном «/»). RequirePermission — chi-middleware группы
		// (huma наследует). MCP oracle-tools зовут oracle.Service напрямую (мимо handler).
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
		// docs/keeper/push.md). Подключается только при non-nil pushH (Deps.PushRun
		// прокинут). Selector — NoSelector: push.apply/push.read оперируют apply_id-
		// ом, без таргетинга по имени-incarnation/coven в MVP (как augur.*/service.*).
		// Coven-scope-фильтрация по инвентарь-хостам — отдельный slice (RBAC
		// расширение, не покрыто в этом slice по architect-вердикту a58e).
		//
		// Audit на apply (мутирующий): payload handler выставляет через
		// SetAuditPayload (apply_id, destiny-ref, inventory_size, ssh_provider,
		// cleanup_stale); SID-ы целиком НЕ кладутся (могут быть много, лежат в
		// push_runs.inventory_sids). GET — read-only, без audit.
		//
		// Permission-маппинг: POST→push.apply, GET→push.read. push.read — новая
		// permission (см. catalog.go), отдельно от push.apply: read-операция не
		// требует mutate-прав.
		//
		// FULL-TYPED huma (ADR-054, ТИРАЖ-БАТЧ-2e домена push целиком по эталонам
		// operator issue-token + audit-endpoint): apply — WRITE+AUDIT вариант B
		// (newHumaPushAPI(evt) с event push.applied; 202+body async); get/push-runs —
		// read (БЕЗ audit). Apply-группа сохраняет Toll DegradedMiddleware (503 при
		// cluster:degraded) ПЕРВЫМ — huma наследует chi-middleware группы. MCP push-tool
		// keeper.push.apply зовёт pushorch.PushRun напрямую (мимо handler).
		if pushH != nil {
			r.Route("/push", func(r chi.Router) {
				// POST /v1/push/apply — блокируется Toll при cluster:degraded
				// (ADR-038): паритет с POST /v1/incarnations/{name}/scenarios/{scenario},
				// outermost-middleware → 503 ДО RBAC/Audit. GET /v1/push/{apply_id}
				// (ниже) — read-API, НЕ блокируется (recovery-friendly чтение
				// статуса прогона при degraded).
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

			// /v1/push-runs — глобальный list push-прогонов (UI-4 Push-runs page).
			// Отдельная зона от /v1/push/{apply_id} (тот — per-id detail; этот —
			// список с пагинацией/фильтрами). RBAC — incarnation.history (push —
			// история incarnation, parity с list); отдельная permission
			// `push.list` не вводится до запроса оператора. NoSelector — глобальный
			// list без таргета по path-{id}.
			r.With(
				apimiddleware.RequirePermission(enforcer, "incarnation", "history", apimiddleware.NoSelector),
			).Group(func(r chi.Router) {
				registerHumaPushRunsList(newHumaCadenceAPI(r), pushH)
			})
		}

		// /v1/push-providers — CRUD реестра Push-Provider-ов (ADR-032 amendment
		// 2026-05-26, S7-2). Подключается только при non-nil pushProviderH
		// (Deps.PushProviderSvc прокинут). Selector — NoSelector: push-provider.*
		// оперирует самим реестром (как provider.* / service.* / role.*).
		//
		// Audit на 3 мутирующих роутах (create/update/delete). list/get — read-only,
		// без audit. payload handler выставляет через SetAuditPayload (name +
		// params_keys без values; sensitive-инвариант — vault-refs валидируется
		// сервисом).
		//
		// Permission-маппинг: POST→push-provider.create, GET list→push-provider.list,
		// GET {name}→push-provider.read, PUT→push-provider.update, DELETE→
		// push-provider.delete.
		//
		// FULL-TYPED huma (ADR-054, ТИРАЖ-БАТЧ-2b домена push-provider целиком по
		// эталонам role/operator): create/update/delete — WRITE+AUDIT вариант B
		// (huma-audit-middleware; full-typed huma САМ пишет ответ, поэтому
		// StatusRecorder из apimiddleware.Audit неприменим — audit держит
		// hctx.Status() + carrier-payload, иначе рецидив S6). list/get — read (БЕЗ
		// audit; list — read-with-typed-query int32-пагинация→400 + name_pattern;
		// update — PUT replace-семантика, НЕ presence-tier). Каждый write-роут — СВОЯ
		// chi-группа с собственным event-типом (newHumaPushProviderAPI(evt)).
		// RequirePermission — chi-middleware группы (huma наследует). MCP
		// push-provider-tools зовут pushprovider.Service напрямую (мимо handler).
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

		// /v1/heralds + /v1/tidings — CRUD реестров уведомлений Herald (каналы) /
		// Tiding (правила подписки) о событиях прогонов (ADR-052, S4). Подключаются
		// ТОЛЬКО при non-nil heraldH (Deps.HeraldSvc прокинут). Селектор —
		// NoSelector: управление кластер-уровневое (как push-provider.* / omen.* /
		// role.*).
		//
		// Permission-маппинг: POST→herald.create / GET list→herald.list / GET
		// {name}→herald.read / PUT→herald.update / DELETE→herald.delete (и
		// tiding.* симметрично). Audit на 3 мутирующих роутах каждого реестра
		// (create/update/delete); list/get — read-only без audit (паттерн
		// push-provider). payload handler выставляет через SetHumaAuditPayload.
		//
		// FULL-TYPED huma (ADR-054, ТИРАЖ-БАТЧ-2c домена herald целиком по эталонам
		// role/operator/augur/push-provider): create/update/delete — WRITE+AUDIT
		// вариант B (huma-audit-middleware; full-typed huma САМ пишет ответ, поэтому
		// StatusRecorder из apimiddleware.Audit неприменим — audit держит
		// hctx.Status() + carrier-payload, иначе рецидив S6). list/get — read (БЕЗ
		// audit; list — read-with-typed-query int32-пагинация→400, tiding-list +
		// include_ephemeral bool→400; update — PUT replace-семантика, НЕ presence-tier).
		// Каждый write-роут — СВОЯ chi-группа с собственным event-типом
		// (newHumaHeraldAPI(evt)). huma-op несёт ПОЛНЫЙ путь /heralds[/{name}]//tidings
		// [/{name}] → группы монтируются прямо на /v1 (distinct-path для spec-dump,
		// иначе herald-POST и tiding-POST осели бы на одном «/»). RequirePermission —
		// chi-middleware группы (huma наследует). CRUD-мутации дёргают herald.Service,
		// инвалидирующий снимок dispatcher-кэша
		// (in-process + cross-keeper через Redis `herald:invalidate`).
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

		// /v1/errands — реестр Errand-ов (ADR-033). Mutating POST лежит под
		// /v1/souls/{sid}/exec (выше, на huma — registerHumaSoulExec); здесь — Get/List + DELETE
		// (slice E5 cancel). Permission `errand.list` для read, `errand.cancel` для
		// DELETE; селектор для cancel — NoSelector (per-row host=<sid>-scope в RBAC
		// будет добавлен при появлении мульти-тенант-сценария; SID известен только
		// после lookup-а строки errand-а, что несовместимо с pre-handler-middleware-
		// check-ом). Audit на read-эндпоинтах НЕ навешан (паттерн push.read /
		// role.list — read без audit); DELETE пишет EventTypeErrandCancelled.
		//
		// FULL-TYPED huma (ADR-054, ТИРАЖ-БАТЧ-2c домена errand по эталонам augur/
		// audit-endpoint/role): list — read-with-typed-query (started_after date-time→
		// 400 на huma-bind — единственный source, прежний доменный 422 недостижим, ADR-051
		// Amendment 2026-06-10; offset/limit int32→400 через CheckPageBounds; status enum
		// →422; sid format→422); get — read-with-path (200 ErrandResult / 202 running
		// ErrandAccepted, двойной success-код); cancel — WRITE+AUDIT вариант B (huma-
		// audit-middleware; full-typed huma САМ пишет ответ, StatusRecorder из
		// apimiddleware.Audit неприменим — audit держит hctx.Status() + carrier-payload,
		// иначе рецидив S6; dispatcher также пишет свой audit-event внутри Cancel,
		// middleware-event — security navigation-trail). huma-op несёт ПОЛНЫЙ путь
		// /errands[/{errand_id}] → группы монтируются прямо на /v1 (distinct-path для
		// spec-dump). RequirePermission — chi-middleware группы (huma наследует).
		// MCP errand-tools зовут errand.Dispatcher/Store напрямую.
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

		// /v1/voyages — унифицированный батчевый прогон (ADR-043, S5).
		// Подключается только при non-nil voyageH (паттерн errandRunH).
		//
		// RBAC-by-kind (ADR-043 §6, security-критичный fail-closed): POST и DELETE
		// мультиплексируют kind=scenario (incarnation.run) и kind=command
		// (errand.run) — middleware-route выбрать permission ДО декода body не
		// может (kind виден только из тела / из загруженной строки), поэтому
		// permission-проверка живёт ВНУТРИ VoyageHandler.Create / .Cancel. Здесь
		// навешивается только base auth (RequireJWT на уровне /v1) + audit-trail
		// через SetAuditPayload (handler пишет scenario_run.*/command_run.*
		// напрямую, payload зависит от kind/резолва — middleware.Audit не соберёт).
		//
		// GET/list/detail/targets — read о состоянии прогона; permission
		// `incarnation.history` (All-runs vista — read runtime-состояния).
		// Селектор — NoSelector (глобальный read без таргета по path;
		// per-kind/coven-scope read — отложен).
		//
		// FULL-TYPED huma (ADR-054, БАТЧ-2f WRITE-SELF-AUDIT): create/cancel —
		// self-audit ВНУТРИ CreateTyped/CancelTyped (emitCreated/emitCancelled),
		// audit-middleware НЕ навешан. preview — read-like dry-resolve БЕЗ audit.
		// list биндит typed-пагинацию (offset/limit int32) → CheckPageBounds 400;
		// kind/status enum → 422. MCP voyage-tools зовут (w,r)-handler через
		// httptest-recorder.
		if voyageH != nil {
			r.Route("/voyages", func(r chi.Router) {
				// POST — RBAC-by-kind в handler-е (см. выше). Auth (/v1
				// RequireJWT) + Tempo per-AID rate-limit (ADR-050(c)):
				// resolver-тяжёлый create — единственный охват MVP. Middleware
				// идёт ПОСЛЕ RequireJWT (берёт claims.Subject = AID из context);
				// tempoLimiter=nil (нет Redis / Tempo disabled) → passthrough.
				// Навеска ТОЛЬКО на create — GET/list/cancel дёшевы и не лимитятся.
				r.With(
					apimiddleware.RateLimit(tempoLimiter, tempoBucketVoyageCreate, tempoVoyageCreateLimits, tempoMetrics, logger),
				).Group(func(r chi.Router) {
					registerHumaVoyageCreate(newHumaCadenceAPI(r), voyageH)
				})

				// POST /v1/voyages/preview — dry-resolve scope БЕЗ создания Voyage
				// (ADR-043 amendment §4). RBAC-by-kind в handler-е (как Create).
				// Tempo-навеска на ОТДЕЛЬНЫЙ bucket voyage_preview (ADR-050 amendment
				// 2026-06-17): preview read-like по эффекту (без persist/audit) →
				// собственный, более мягкий лимит, не делит квоту с create. Read-like
				// — БЕЗ audit.
				r.With(
					apimiddleware.RateLimit(tempoLimiter, tempoBucketVoyagePreview, tempoVoyagePreviewLimits, tempoMetrics, logger),
				).Group(func(r chi.Router) {
					registerHumaVoyagePreview(newHumaCadenceAPI(r), voyageH)
				})

				// list/get/targets — read (incarnation.history) на ОДНОЙ huma.API
				// (distinct-path исключает коллизию операций на общей spec-dump-API).
				r.With(
					apimiddleware.RequirePermission(enforcer, "incarnation", "history", apimiddleware.NoSelector),
				).Group(func(r chi.Router) {
					voyageReadAPI := newHumaCadenceAPI(r)
					registerHumaVoyageList(voyageReadAPI, voyageH)
					registerHumaVoyageGet(voyageReadAPI, voyageH)
					registerHumaVoyageTargets(voyageReadAPI, voyageH)
				})

				// DELETE — RBAC-by-kind в handler-е (kind виден из строки). Только
				// base auth (/v1 RequireJWT) — отдельная chi-группа без RequirePermission.
				r.Group(func(r chi.Router) {
					registerHumaVoyageCancel(newHumaCadenceAPI(r), voyageH)
				})
			})
		}

		// /v1/cadences — регулярные запуски (Cadence, ADR-046 S4).
		// Подключается только при non-nil cadenceH (паттерн voyageH).
		//
		// Двухуровневый RBAC (ADR-046 §7, security-критичный fail-closed): первый
		// уровень — cadence.* (middleware-route, NoSelector); второй — Voyage-
		// permission по kind рецепта (scenario→incarnation.run / command→errand.run)
		// проверяется ВНУТРИ CadenceHandler.Create (kind виден только из тела). POST
		// навешивает cadence.create через middleware + audit через SetAuditPayload
		// (handler пишет cadence.created/updated/deleted напрямую).
		//
		// PATCH — правка рецепта → cadence.update; enable/disable — toggle →
		// гранулярные cadence.enable/disable ИЛИ backcompat cadence.update
		// (OR-гейт RequireAnyPermission, ADR-046 amendment 2026-06-02); DELETE →
		// cadence.delete; list/get — cadence.list (read). /runs — дочерние Voyage,
		// permission incarnation.history (read runtime-состояния прогонов, parity
		// Voyage-list). Все селекторы — NoSelector (CRUD реестра расписаний без
		// таргета по path; per-name scope — отложен, parity push-provider).
		if cadenceH != nil {
			r.Route("/cadences", func(r chi.Router) {
				// POST /v1/cadences — huma-операция (code-first, ADR-054) на ЭТОЙ
				// chi-группе под навеской RequirePermission(cadence.create). huma-handler
				// делегирует в доменный cadenceH.CreateTyped (tx+notify+invalidation+audit)
				// через тонкий конверт (см. huma_cadence.go HUMA-PATTERN).
				r.With(
					apimiddleware.RequirePermission(enforcer, "cadence", "create", apimiddleware.NoSelector),
				).Group(func(r chi.Router) {
					registerHumaCadence(newHumaCadenceAPI(r), cadenceH)
				})

				// GET /v1/cadences (list) — READ-with-typed-query (cadence.list, БЕЗ
				// audit; Teardown T1 — последний live strict-mount /v1 перенесён на
				// huma). ТОПОЛОГИЯ: GET / на руте группы /v1/cadences — отдельная
				// chi-группа; не конфликтует с POST / (create) — разные методы на одном
				// пути; и не затеняет /{id}-роуты (huma-op на distinct-path). Query
				// (enabled/kind enum → 422; offset/limit int32 → 400/CheckPageBounds).
				r.With(
					apimiddleware.RequirePermission(enforcer, "cadence", "list", apimiddleware.NoSelector),
				).Group(func(r chi.Router) {
					registerHumaCadenceList(newHumaCadenceAPI(r), cadenceH)
				})

				// GET/{id} + GET/{id}/runs — FULL-TYPED huma (ADR-054, БАТЧ-2f, перенос
				// read-роутов завершает cadence-домен целиком). READ (БЕЗ audit). КРИТИЧНО
				// для блокера: read-роуты ТОЖЕ на huma-op с полным путём /{id}[/runs]
				// относительно группы /v1/cadences — sibling-саброутер r.Route("/{id}")
				// СНЯТ. Прежде chi отдавал ВЕСЬ узел /{id} строгому саброутеру (у него
				// только GET / + GET /runs) → PATCH/DELETE huma-op были недостижимы (405).
				// Теперь GET/{id}, GET/{id}/runs, PATCH/{id}, DELETE/{id} — четыре huma-op
				// на одном /{id}-узле группы, без chi.Route на нём. GET/{id} — RBAC
				// cadence.list (read-tier); /runs — incarnation.history (история
				// incarnation, parity legacy). /runs пагинирован (int32 offset/limit →
				// CheckPageBounds→400 в RunsTyped; status[] enum→422).
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

				// PATCH/DELETE/enable/disable — FULL-TYPED huma (ADR-054, БАТЧ-2f
				// self-audit): WRITE-SELF-AUDIT (handler пишет cadence.updated/.deleted
				// САМ через emitWrite/emitDeleted/emitEnabledToggle ВНУТРИ PatchTyped/
				// DeleteTyped/SetEnabledTyped — audit-middleware НЕ навешан, отличие от
				// middleware-audit-доменов role/operator). newHumaCadenceAPI (БЕЗ audit-
				// навески). huma-op несёт ПОЛНЫЙ путь /{id}[/...] относительно группы
				// /v1/cadences (НЕ вложен в chi.Route("/{id}") — иначе chi удвоил бы
				// {id}-префикс, паттерн soul/operator-доменов; huma биндит {id} сам,
				// chi-RBAC-группа наследуется). PATCH — *T omitempty presence (omitted=
				// keep), НЕ presence-tier Optional[T]. MCP cadence НЕТ.
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

				// enable/disable — гранулярные cadence.enable/disable ИЛИ backcompat-
				// грант cadence.update (роли со старым update не теряют toggle, ADR-046
				// amendment 2026-06-02). OR-гейт по набору actions — RequireAnyPermission.
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

		// Catch-all 404 для несуществующих /v1/-путей за auth-chain
		// (без токена 401, с валидным токеном 404).
		r.HandleFunc("/*", func(w http.ResponseWriter, req *http.Request) {
			apimiddleware.WriteNotFound(w, req, "no such endpoint")
		})
	})

	return r
}

// routePatternFromChi возвращает chi RoutePattern (`/v1/operators/{aid}/revoke`)
// для metric-label `path`. Inject-ится в shared/obs middleware, чтобы
// shared/obs не зависел от chi (по [ADR-011] shared/ — поперечный код,
// без привязки к роутеру).
//
// Возвращает пустую строку, если chi-RouteContext не инициализирован
// (запрос не прошёл chi-роутер; не должно случаться в продакшене, но
// возможно в unit-тесте) — это допустимо, label запишется как `path=""`.
func routePatternFromChi(r *http.Request) string {
	rc := chi.RouteContext(r.Context())
	if rc == nil {
		return ""
	}
	return rc.RoutePattern()
}
