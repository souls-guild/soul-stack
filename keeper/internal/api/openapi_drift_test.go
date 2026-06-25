// SHARED-инфраструктура обхода chi-роутера Operator API для route-coverage гейтов.
//
// Здесь живут общие для агрегатор-тестов примитивы: тип [route], нормализация пути,
// [collectRoutes] (chi.Walk по собранному [buildRouter]) + [pathAllowlist] (opt-in
// домены, чьи handler-ы при сборке drift-router передаются nil). Их потребитель —
// [TestFullSpec_CoversAllRoutes] (huma_full_spec_test.go), который сверяет реальные
// chi-роуты с собранной huma-спекой ([buildFullOpenAPISpec]) в обе стороны: «роут
// есть, в спеке нет» (агрегатор забыл домен) и «в спеке есть, роута нет» (лишняя
// операция). Это и есть гарантия «все роуты в спеке» — отдельного источника-рукописи
// больше нет, served-спека и committed-генерат производны от huma-dump.
//
// Тест чистый: роутер собирается через [buildRouter] со stub-зависимостями
// (zero-value embedded-интерфейсы, методы которых при обходе дерева не
// вызываются) — без Postgres/Redis/Vault, без build-tag `integration`.
package api

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
	apimiddleware "github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
	"github.com/souls-guild/soul-stack/keeper/internal/augur"
	"github.com/souls-guild/soul-stack/keeper/internal/oracle"
	"github.com/souls-guild/soul-stack/keeper/internal/pluginhost"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac"
	"github.com/souls-guild/soul-stack/keeper/internal/serviceregistry"
	"github.com/souls-guild/soul-stack/keeper/internal/sigil"
)

// route — нормализованный ключ маршрута для сравнения множеств.
// method — верхний регистр ("GET"/"POST"/…); path — `/v1/roles/{name}` с
// унифицированными `{param}`-плейсхолдерами.
type route struct {
	method string
	path   string
}

func (r route) String() string { return r.method + " " + r.path }

// normalizePath приводит path к каноничному виду для сравнения. chi монтирует
// `r.Route("/operators")` + `.Post("/")` как `/v1/operators/` (с хвостовым
// слешем), тогда как OpenAPI-нотация — `/v1/operators` (без). Это один и тот
// же эндпоинт; убираем хвостовой слеш (кроме корня). `{param}`-плейсхолдеры у
// chi и openapi уже идентичны (фигурные скобки) — отдельной нормализации не
// требуется.
func normalizePath(p string) string {
	if len(p) > 1 {
		p = strings.TrimSuffix(p, "/")
	}
	return p
}

// wildcardSuffix — chi-нотация catch-all-сегмента. Роуты, оканчивающиеся
// на него, — fallback-обработчики (router.go: r.HandleFunc("/*")), а не
// эндпоинты; в спеке им места нет по определению. chi регистрирует такой
// wildcard под ВСЕ HTTP-методы, поэтому фильтруем по суффиксу пути, а не
// перечисляем (method, path) поимённо.
const wildcardSuffix = "/*"

// pathAllowlist — спека-пути, которым законно НЕ иметь реализующего роута.
// Декларации эндпоинтов, чьи handler-ы подключаются только при non-nil домене
// (push / errand / audit / push-provider): drift-test собирает router с этими
// доменами=nil, поэтому в спеке объявлены, но в роутере отсутствуют. Пустой
// набор означал бы «любая декларация без роута = дрейф»; здесь фиксируем
// заведомо известные opt-in-эндпоинты с явным обоснованием. При безусловном
// подключении handler-а соответствующую строку нужно удалить — тогда тест
// начнёт проверять их как обычные роуты.
var pathAllowlist = map[route]string{
	{method: http.MethodPost, path: "/v1/push/apply"}:     "keeper.push apply объявлен в спеке, роут подключён ТОЛЬКО при non-nil pushH (drift-test собирает router с pushH=nil)",
	{method: http.MethodGet, path: "/v1/push/{apply_id}"}: "keeper.push GET по apply_id, аналогично push/apply подключается только при non-nil pushH",
	// errand.*-роуты подключаются ТОЛЬКО при non-nil errandH (ADR-033, slice E2);
	// drift-test собирает router с errandH=nil, поэтому в спеке объявлены, но
	// в роутере отсутствуют — это документированный «opt-in»-блок (паттерн push).
	{method: http.MethodPost, path: "/v1/souls/{sid}/exec"}:      "ADR-033 Errand: роут подключён ТОЛЬКО при non-nil errandH (slice E2 production-wire-up)",
	{method: http.MethodGet, path: "/v1/errands"}:                "ADR-033 Errand list: роут подключён ТОЛЬКО при non-nil errandH",
	{method: http.MethodGet, path: "/v1/errands/{errand_id}"}:    "ADR-033 Errand get: роут подключён ТОЛЬКО при non-nil errandH",
	{method: http.MethodDelete, path: "/v1/errands/{errand_id}"}: "ADR-033 Errand cancel (slice E5): роут подключён ТОЛЬКО при non-nil errandH",
	// audit.read — роут подключён ТОЛЬКО при non-nil auditH (UI iter 2, паттерн
	// errandH/pushH). drift-test собирает router с auditH=nil → объявлен в
	// спеке, но не в роутере.
	{method: http.MethodGet, path: "/v1/audit"}: "UI iter 2 audit: роут подключён ТОЛЬКО при non-nil auditH (production-wire-up через AuditReader)",
	// push-provider.* — роуты подключаются ТОЛЬКО при non-nil pushProviderH
	// (ADR-032 amendment 2026-05-26, S7-2); drift-test собирает router с
	// pushProviderH=nil → объявлены в спеке, но не в роутере.
	{method: http.MethodPost, path: "/v1/push-providers"}:          "S7-2 push-provider CRUD: роут подключён ТОЛЬКО при non-nil pushProviderH",
	{method: http.MethodGet, path: "/v1/push-providers"}:           "S7-2 push-provider list: роут подключён ТОЛЬКО при non-nil pushProviderH",
	{method: http.MethodGet, path: "/v1/push-providers/{name}"}:    "S7-2 push-provider get: роут подключён ТОЛЬКО при non-nil pushProviderH",
	{method: http.MethodPut, path: "/v1/push-providers/{name}"}:    "S7-2 push-provider update: роут подключён ТОЛЬКО при non-nil pushProviderH",
	{method: http.MethodDelete, path: "/v1/push-providers/{name}"}: "S7-2 push-provider delete: роут подключён ТОЛЬКО при non-nil pushProviderH",

	// provider.* / profile.* — Cloud CRUD (ADR-017): роуты подключаются ТОЛЬКО
	// при non-nil providerH/profileH; drift-test собирает router с nil → в спеке
	// объявлены, в роутере отсутствуют (документированный opt-in, паттерн push-provider).
	{method: http.MethodPost, path: "/v1/providers"}:          "ADR-017 provider create: роут подключён ТОЛЬКО при non-nil providerH",
	{method: http.MethodGet, path: "/v1/providers"}:           "ADR-017 provider list: роут подключён ТОЛЬКО при non-nil providerH",
	{method: http.MethodGet, path: "/v1/providers/{name}"}:    "ADR-017 provider get: роут подключён ТОЛЬКО при non-nil providerH",
	{method: http.MethodDelete, path: "/v1/providers/{name}"}: "ADR-017 provider delete: роут подключён ТОЛЬКО при non-nil providerH",
	{method: http.MethodPost, path: "/v1/profiles"}:           "ADR-017 profile create: роут подключён ТОЛЬКО при non-nil profileH",
	{method: http.MethodGet, path: "/v1/profiles"}:            "ADR-017 profile list: роут подключён ТОЛЬКО при non-nil profileH",
	{method: http.MethodGet, path: "/v1/profiles/{name}"}:     "ADR-017 profile get: роут подключён ТОЛЬКО при non-nil profileH",
	{method: http.MethodDelete, path: "/v1/profiles/{name}"}:  "ADR-017 profile delete: роут подключён ТОЛЬКО при non-nil profileH",

	// push-runs list: роут подключён ТОЛЬКО при non-nil pushH (UI-4); drift-test
	// собирает router с pushH=nil. Парный per-id detail (`GET /v1/push/{apply_id}`)
	// и `POST /v1/push/apply` уже в allowlist выше.
	{method: http.MethodGet, path: "/v1/push-runs"}: "UI-4 Push-runs global list: роут подключён ТОЛЬКО при non-nil pushH",

	// auth.ldap login: роут подключён ТОЛЬКО при non-nil LDAPAuth (ADR-058);
	// drift-test собирает router с LDAPAuth=nil → объявлен в спеке (prefix /auth),
	// но в роутере отсутствует. ВНЕ /v1 (публичный вход, RequireJWT неприменим).
	{method: http.MethodPost, path: "/auth/ldap/login"}: "ADR-058 LDAP login: роут подключён ТОЛЬКО при non-nil LDAPAuth (опц. блок auth.ldap в keeper.yml)",

	// auth.oidc эндпоинты (ADR-058 стадия 2): подключены ТОЛЬКО при non-nil
	// OIDCAuth (опц. блок auth.oidc + Redis); drift-test собирает router с
	// OIDCAuth=nil → в спеке объявлены (prefix /auth), в роутере отсутствуют.
	{method: http.MethodGet, path: "/auth/oidc/login"}:    "ADR-058 OIDC login: роут подключён ТОЛЬКО при non-nil OIDCAuth (опц. блок auth.oidc + Redis)",
	{method: http.MethodGet, path: "/auth/oidc/callback"}: "ADR-058 OIDC callback: роут подключён ТОЛЬКО при non-nil OIDCAuth (опц. блок auth.oidc + Redis)",

	// voyage.*-роуты подключаются ТОЛЬКО при non-nil voyageH (ADR-043 S5);
	// drift-test собирает router с voyageH=nil, поэтому в спеке объявлены, но в
	// роутере отсутствуют — документированный «opt-in»-блок (паттерн
	// errandRunH/pushH).
	{method: http.MethodPost, path: "/v1/voyages"}:             "ADR-043 Voyage create: роут подключён ТОЛЬКО при non-nil voyageH",
	{method: http.MethodPost, path: "/v1/voyages/preview"}:     "ADR-043 amendment §4 Voyage preview: роут подключён ТОЛЬКО при non-nil voyageH",
	{method: http.MethodGet, path: "/v1/voyages"}:              "ADR-043 Voyage list: роут подключён ТОЛЬКО при non-nil voyageH",
	{method: http.MethodGet, path: "/v1/voyages/{id}"}:         "ADR-043 Voyage get: роут подключён ТОЛЬКО при non-nil voyageH",
	{method: http.MethodGet, path: "/v1/voyages/{id}/targets"}: "ADR-043 Voyage targets drill: роут подключён ТОЛЬКО при non-nil voyageH",
	{method: http.MethodDelete, path: "/v1/voyages/{id}"}:      "ADR-043 Voyage cancel: роут подключён ТОЛЬКО при non-nil voyageH",

	// cadence.*-роуты подключаются ТОЛЬКО при non-nil cadenceH (ADR-046 S4);
	// drift-test собирает router с cadenceH=nil, поэтому в спеке объявлены, но в
	// роутере отсутствуют — документированный «opt-in»-блок (паттерн voyageH).
	{method: http.MethodPost, path: "/v1/cadences"}:              "ADR-046 Cadence create: роут подключён ТОЛЬКО при non-nil cadenceH",
	{method: http.MethodGet, path: "/v1/cadences"}:               "ADR-046 Cadence list: роут подключён ТОЛЬКО при non-nil cadenceH",
	{method: http.MethodGet, path: "/v1/cadences/{id}"}:          "ADR-046 Cadence get: роут подключён ТОЛЬКО при non-nil cadenceH",
	{method: http.MethodPatch, path: "/v1/cadences/{id}"}:        "ADR-046 Cadence update: роут подключён ТОЛЬКО при non-nil cadenceH",
	{method: http.MethodDelete, path: "/v1/cadences/{id}"}:       "ADR-046 Cadence delete: роут подключён ТОЛЬКО при non-nil cadenceH",
	{method: http.MethodPost, path: "/v1/cadences/{id}/enable"}:  "ADR-046 Cadence enable: роут подключён ТОЛЬКО при non-nil cadenceH",
	{method: http.MethodPost, path: "/v1/cadences/{id}/disable"}: "ADR-046 Cadence disable: роут подключён ТОЛЬКО при non-nil cadenceH",
	{method: http.MethodGet, path: "/v1/cadences/{id}/runs"}:     "ADR-046 Cadence runs drill: роут подключён ТОЛЬКО при non-nil cadenceH",

	// choir.*-роуты подключаются ТОЛЬКО при non-nil choirH (ADR-044 S-T3);
	// drift-test собирает router с choirH=nil, поэтому в спеке объявлены, но
	// в роутере отсутствуют — документированный «opt-in»-блок (паттерн
	// tideH/errandH/pushH).
	{method: http.MethodPost, path: "/v1/incarnations/{name}/choirs"}:                        "ADR-044 Choir create: роут подключён ТОЛЬКО при non-nil choirH",
	{method: http.MethodGet, path: "/v1/incarnations/{name}/choirs"}:                         "ADR-044 Choir list: роут подключён ТОЛЬКО при non-nil choirH",
	{method: http.MethodDelete, path: "/v1/incarnations/{name}/choirs/{choir}"}:              "ADR-044 Choir delete: роут подключён ТОЛЬКО при non-nil choirH",
	{method: http.MethodPost, path: "/v1/incarnations/{name}/choirs/{choir}/voices"}:         "ADR-044 Voice add: роут подключён ТОЛЬКО при non-nil choirH",
	{method: http.MethodGet, path: "/v1/incarnations/{name}/choirs/{choir}/voices"}:          "ADR-044 Voice list: роут подключён ТОЛЬКО при non-nil choirH",
	{method: http.MethodDelete, path: "/v1/incarnations/{name}/choirs/{choir}/voices/{sid}"}: "ADR-044 Voice remove: роут подключён ТОЛЬКО при non-nil choirH",

	// herald.*/tiding.*-роуты подключаются ТОЛЬКО при non-nil heraldH (ADR-052 S4);
	// drift-test собирает router с heraldH=nil, поэтому в спеке объявлены, но в
	// роутере отсутствуют — документированный «opt-in»-блок (паттерн push-provider).
	{method: http.MethodPost, path: "/v1/heralds"}:          "ADR-052 Herald create: роут подключён ТОЛЬКО при non-nil heraldH",
	{method: http.MethodGet, path: "/v1/heralds"}:           "ADR-052 Herald list: роут подключён ТОЛЬКО при non-nil heraldH",
	{method: http.MethodGet, path: "/v1/heralds/{name}"}:    "ADR-052 Herald get: роут подключён ТОЛЬКО при non-nil heraldH",
	{method: http.MethodPut, path: "/v1/heralds/{name}"}:    "ADR-052 Herald update: роут подключён ТОЛЬКО при non-nil heraldH",
	{method: http.MethodDelete, path: "/v1/heralds/{name}"}: "ADR-052 Herald delete: роут подключён ТОЛЬКО при non-nil heraldH",
	{method: http.MethodPost, path: "/v1/tidings"}:          "ADR-052 Tiding create: роут подключён ТОЛЬКО при non-nil heraldH",
	{method: http.MethodGet, path: "/v1/tidings"}:           "ADR-052 Tiding list: роут подключён ТОЛЬКО при non-nil heraldH",
	{method: http.MethodGet, path: "/v1/tidings/{name}"}:    "ADR-052 Tiding get: роут подключён ТОЛЬКО при non-nil heraldH",
	{method: http.MethodPut, path: "/v1/tidings/{name}"}:    "ADR-052 Tiding update: роут подключён ТОЛЬКО при non-nil heraldH",
	{method: http.MethodDelete, path: "/v1/tidings/{name}"}: "ADR-052 Tiding delete: роут подключён ТОЛЬКО при non-nil heraldH",
}

// collectRoutes собирает фактические `(method, path)` из chi-дерева через
// chi.Walk. buildRouter возвращает http.Handler, конкретный тип — *chi.Mux,
// реализующий chi.Routes. path-паттерны chi уже в форме `/v1/roles/{name}` —
// совпадают с openapi-нотацией, отдельной нормализации `{param}` не требуется
// (оба используют фигурные скобки); приводим только метод к верхнему регистру.
func collectRoutes(t *testing.T) map[route]struct{} {
	t.Helper()
	h := buildRouter(
		nil, // verifier — middleware RequireJWT собирается lazily, не разыменовывается при обходе
		nil, // healthH — r.Get(...) только сохраняет method-value-handler
		stubOperatorHandler(t),
		handlers.NewIncarnationHandler(nil, nil, nil, nil, nil, nil, nil, nil, nil),
		handlers.NewSoulHandler(nil, nil, nil, nil),
		stubRoleHandler(t),
		stubSynodHandler(t),
		stubSigilHandler(t),
		stubSigilKeyHandler(t),
		stubServiceHandler(t),
		stubProvisioningPolicyHandler(t),
		stubAugurHandler(t),
		stubOracleHandler(t),
		nil, // pushH — push.*-роуты подключаются только при non-nil pushH (router.go); сейчас в allowlist
		nil, // pushProviderH — push-provider.*-роуты подключаются только при non-nil; в allowlist
		nil, // providerH — provider.*-роуты подключаются только при non-nil; в allowlist
		nil, // profileH — profile.*-роуты подключаются только при non-nil; в allowlist
		nil, // errandH — errand.*-роуты подключаются только при non-nil errandH; в allowlist
		nil, // voyageH — voyage.*-роуты подключаются только при non-nil voyageH (ADR-043 S5); в allowlist
		nil, // cadenceH — cadence.*-роуты подключаются только при non-nil cadenceH (ADR-046 S4); в allowlist
		nil, // auditH — audit-роут подключается только при non-nil auditH; в allowlist
		nil, // choirH — choir.*-роуты подключаются только при non-nil choirH (ADR-044 S-T3); в allowlist
		nil, // heraldH — herald.*/tiding.*-роуты подключаются только при non-nil heraldH (ADR-052 S4); в allowlist
		handlers.NewModuleCatalogHandler(nil, nil),  // moduleCatalogH — /v1/modules монтируется всегда (core-каталог); plugins=nil → только core
		handlers.NewModuleFormPrepHandler(nil, nil), // moduleFormPrepH — non-nil → /v1/modules/{name}/form-prep монтируется (ADR-045 S3); resolver nil не дёргается при обходе
		handlers.NewPermissionCatalogHandler(nil),   // permCatalogH — /v1/permissions монтируется всегда (статика rbac-каталога)
		handlers.NewEventTypeCatalogHandler(nil),    // eventTypeCatalogH — /v1/event-types монтируется всегда (статика herald-каталога)
		handlers.NewMyPermissionsHandler(nil, nil),  // meH — /v1/me/permissions монтируется всегда (зависит лишь от RBAC-снимка); PermissionsOf при обходе дерева не дёргается
		nil,                                  // enforcer — RequirePermission собирается lazily
		nil,                                  // auditWriter — Audit собирается lazily
		nil,                                  // metricsHTTP — nil → metrics-middleware не подключается (router.go)
		nil,                                  // tollDegradedReader — DegradedMiddleware skip при nil (router.go)
		nil,                                  // tempoLimiter — nil → RateLimit middleware passthrough (router.go)
		nil,                                  // tempoMetrics — nil → emit no-op (router.go)
		nil,                                  // tempoVoyageCreateLimits — nil допустим (RateLimit при nil-limiter не вызывает provider)
		nil,                                  // tempoVoyagePreviewLimits — nil допустим (RateLimit при nil-limiter не вызывает provider)
		false,                                // webUIEnabled — /ui вне /v1, drift-walker его не видит; держим выключенным для чистоты периметра
		nil,                                  // ldapAuth (LDAP не сконфигурирован в тесте)
		nil,                                  // oidcAuth (OIDC не сконфигурирован в тесте)
		nil,                                  // loginGuard (anti-bruteforce off в тесте)
		apimiddleware.AuthLoginLimitConfig{}, // loginLimitCfg
		nil,                                  // logger — допустим nil (handler-ы получают io.Discard внутри)
	)

	routes, ok := h.(chi.Routes)
	if !ok {
		t.Fatalf("buildRouter вернул %T, не реализует chi.Routes — обход chi.Walk невозможен", h)
	}

	set := make(map[route]struct{})
	err := chi.Walk(routes, func(method, pattern string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		set[route{method: strings.ToUpper(method), path: normalizePath(pattern)}] = struct{}{}
		return nil
	})
	if err != nil {
		t.Fatalf("chi.Walk: %v", err)
	}
	if len(set) == 0 {
		t.Fatal("chi.Walk не вернул ни одного роута — роутер пуст?")
	}
	return set
}

// --- stub-зависимости handler-конструкторов ---
//
// buildRouter при обходе дерева НЕ вызывает методы этих зависимостей —
// нужны лишь non-nil экземпляры, чтобы конструкторы не паниковали и
// зарегистрировали маршруты (в частности /v1/roles регистрируется только
// при non-nil roleH). Embedded zero-value-интерфейс удовлетворяет
// контракту типа без ручной реализации каждого метода; вызов любого из
// них упал бы nil-panic-ом — но обход дерева до вызова не доходит.

// stubOperatorHandler собирает OperatorHandler со stub-pool/issuer/rbac.
func stubOperatorHandler(t *testing.T) *handlers.OperatorHandler {
	t.Helper()
	return handlers.NewOperatorHandler(stubOperatorPool{}, stubIssuer{}, stubRBACSource{}, time.Hour, nil)
}

// stubRoleHandler собирает RoleHandler через rbac.Service со stub-pool.
func stubRoleHandler(t *testing.T) *handlers.RoleHandler {
	t.Helper()
	svc, err := rbac.NewService(rbac.ServiceDeps{Pool: stubRBACPool{}})
	if err != nil {
		t.Fatalf("rbac.NewService(stub): %v", err)
	}
	return handlers.NewRoleHandler(svc, nil)
}

// stubSynodHandler — non-nil SynodHandler, чтобы synod.*-роуты зарегистрировались
// для drift-проверки (методы service при обходе дерева не вызываются).
func stubSynodHandler(t *testing.T) *handlers.SynodHandler {
	t.Helper()
	svc, err := rbac.NewService(rbac.ServiceDeps{Pool: stubRBACPool{}})
	if err != nil {
		t.Fatalf("rbac.NewService(stub): %v", err)
	}
	return handlers.NewSynodHandler(svc, nil)
}

// stubSigilHandler собирает SigilHandler через sigil.Service со stub-
// Signer/Store/SlotReader. Методы зависимостей при обходе дерева не
// вызываются — нужен лишь non-nil service, чтобы plugin.*-роуты
// зарегистрировались.
func stubSigilHandler(t *testing.T) *handlers.SigilHandler {
	t.Helper()
	signer, err := sigil.NewSigner(ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize)))
	if err != nil {
		t.Fatalf("sigil.NewSigner(stub): %v", err)
	}
	svc, err := sigil.NewService(sigil.ServiceDeps{
		Signer: signer,
		Store:  stubSigilStore{},
		Slots:  stubSlotReader{},
	})
	if err != nil {
		t.Fatalf("sigil.NewService(stub): %v", err)
	}
	return handlers.NewSigilHandler(svc, nil)
}

// stubSigilKeyHandler собирает SigilKeyHandler через sigil.KeyService со stub-
// pool/vault. Методы зависимостей при обходе дерева не вызываются — нужен лишь
// non-nil service, чтобы sigil/keys-роуты зарегистрировались.
func stubSigilKeyHandler(t *testing.T) *handlers.SigilKeyHandler {
	t.Helper()
	svc, err := sigil.NewKeyService(sigil.KeyServiceDeps{
		Pool:  stubKeyStorePool{},
		Vault: stubVaultWriter{},
	})
	if err != nil {
		t.Fatalf("sigil.NewKeyService(stub): %v", err)
	}
	return handlers.NewSigilKeyHandler(svc, nil)
}

type stubKeyStorePool struct{ sigil.KeyStorePool }

type stubVaultWriter struct{}

func (stubVaultWriter) WriteKV(context.Context, string, map[string]any) error { return nil }

// stubServiceHandler собирает ServiceHandler через serviceregistry.Service со
// stub-pool. Методы pool при обходе дерева не вызываются — нужен лишь non-nil
// service, чтобы service.*-роуты зарегистрировались.
func stubServiceHandler(t *testing.T) *handlers.ServiceHandler {
	t.Helper()
	svc, err := serviceregistry.NewService(serviceregistry.ServiceDeps{Pool: stubServicePool{}})
	if err != nil {
		t.Fatalf("serviceregistry.NewService(stub): %v", err)
	}
	return handlers.NewServiceHandler(svc, nil, nil, nil, nil, nil)
}

type stubServicePool struct{ serviceregistry.ServicePool }

// stubProvisioningPolicyHandler собирает ProvisioningPolicyHandler через
// serviceregistry.Service со stub-pool + stub-reader. Методы при обходе дерева не
// вызываются — нужен лишь non-nil handler, чтобы provisioning-policy-роуты
// зарегистрировались (drift роутер↔full-spec). ADR-058 Часть B.
func stubProvisioningPolicyHandler(t *testing.T) *handlers.ProvisioningPolicyHandler {
	t.Helper()
	svc, err := serviceregistry.NewService(serviceregistry.ServiceDeps{Pool: stubServicePool{}})
	if err != nil {
		t.Fatalf("serviceregistry.NewService(stub): %v", err)
	}
	return handlers.NewProvisioningPolicyHandler(stubProvisioningReader{}, svc, nil)
}

type stubProvisioningReader struct{}

func (stubProvisioningReader) ProvisioningPolicy() ([]string, bool) { return nil, false }

// stubAugurHandler собирает AugurHandler через augur.Service со stub-pool.
// Методы pool при обходе дерева не вызываются — нужен лишь non-nil service,
// чтобы augur.*-роуты зарегистрировались.
func stubAugurHandler(t *testing.T) *handlers.AugurHandler {
	t.Helper()
	svc, err := augur.NewService(augur.ServiceDeps{Pool: stubAugurPool{}})
	if err != nil {
		t.Fatalf("augur.NewService(stub): %v", err)
	}
	return handlers.NewAugurHandler(svc, nil)
}

type stubAugurPool struct{ augur.ServicePool }

// stubOracleHandler собирает OracleHandler через oracle.Service со stub-pool +
// реальным WhereEvaluator (compile-проверка where-CEL; конструктор требует
// non-nil Where). Методы pool при обходе дерева не вызываются — нужен лишь
// non-nil service, чтобы vigil.*/decree.*-роуты зарегистрировались.
func stubOracleHandler(t *testing.T) *handlers.OracleHandler {
	t.Helper()
	where, err := oracle.NewWhereEvaluator()
	if err != nil {
		t.Fatalf("oracle.NewWhereEvaluator(stub): %v", err)
	}
	svc, err := oracle.NewService(oracle.ServiceDeps{Pool: stubOraclePool{}, Where: where})
	if err != nil {
		t.Fatalf("oracle.NewService(stub): %v", err)
	}
	return handlers.NewOracleHandler(svc, nil)
}

type stubOraclePool struct{ oracle.ServicePool }

type stubSigilStore struct{}

func (stubSigilStore) Insert(context.Context, *sigil.Sigil) error { return nil }
func (stubSigilStore) Revoke(context.Context, string, string, string, string) error {
	return nil
}
func (stubSigilStore) ListActive(context.Context) ([]*sigil.Sigil, error) { return nil, nil }

type stubSlotReader struct{}

func (stubSlotReader) ReadSlot(string, string) (*pluginhost.SlotContents, error) {
	return nil, sigil.ErrPluginNotInCache
}

func (stubSlotReader) SlotCommitSHA(string, string) (string, error) {
	return "", pluginhost.ErrSlotNotFound
}

type stubOperatorPool struct{ handlers.OperatorPool }

type stubIssuer struct{}

func (stubIssuer) Issue(string, []string, time.Duration, bool) (string, error) {
	return "", fmt.Errorf("stub issuer: не должен вызываться в drift-тесте")
}

type stubRBACSource struct{}

func (stubRBACSource) RolesOf(string) []string { return nil }

type stubRBACPool struct{ rbac.ServicePool }
