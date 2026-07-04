package handlers

// T5d-2c-full (handler-native): домен incarnation отвязан от legacy-генерата. Доменная бизнес-логика
// каждого роута живёт в *Typed-функциях (incarnation_typed.go), возвращающих ПЛОСКИЕ доменные
// view-структуры (incarnation_view.go); пакет api биндит huma-input/проецирует native reply.
// Прежний (w,r)-слой (тонкие strict-оболочки) и legacy-генерата-конвертеры сняты.
//
// Этот файл несёт: зависимости handler-а (интерфейсы + конструктор), shared-хелперы RBAC-scope
// (ADR-047 S3b-3) — фабрики GetInScopeFor/ResolveListScopeFor + резолв state-CEL-измерения,
// валидацию host-role/state-предикатов, и RBAC scope-селекторы роутов (router.go + MCP-паритет).

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"sync"

	"github.com/go-chi/chi/v5"

	"github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac"
	"github.com/souls-guild/soul-stack/keeper/internal/scenario"
	"github.com/souls-guild/soul-stack/keeper/internal/statemigrate"
	"github.com/souls-guild/soul-stack/keeper/internal/statepredicate"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// IncarnationDB — узкая поверхность над pgxpool.Pool для CRUD-операций
// incarnation. Объединяет [incarnation.ExecQueryRower] (Create / Get / List /
// History) и [incarnation.TxBeginner] (Unlock — atomic FOR UPDATE → mutate →
// commit). Реальный `*pgxpool.Pool` удовлетворяет автоматически; unit-тесты
// передают fake.
type IncarnationDB interface {
	incarnation.ExecQueryRower
	incarnation.TxBeginner
}

// ScenarioStarter — узкая поверхность scenario.Runner, нужная Create-handler-у:
// async-запуск прогона scenario. Интерфейс (а не *scenario.Runner) — чтобы
// unit-тесты handler-а не поднимали весь runner-стек.
type ScenarioStarter interface {
	Start(ctx context.Context, spec scenario.RunSpec) error
}

// AssertPreflighter — узкая поверхность scenario.Runner для pre-flight-гейта
// `assert:` (ADR-009/ADR-027 amendment 2026-06-23, форма A): синхронное
// вычисление assert-предикатов сценария на СОЗДАНИИ прогона (request-путь, ДО
// коммита incarnation). Реализуется *scenario.Runner (метод PreflightAssert);
// Create-handler берёт его опционально type-assertion-ом из runner-а, поэтому
// тестовые ScenarioStarter-fake без этого метода продолжают работать (pre-flight
// тогда пропускается — no-op, как в M0.6c-1 stub-режиме без runner-а).
//
// Возвращает scenario.ErrAssertFailed на провале предиката (handler → 422
// assert_failed); прочие ошибки — внутренний сбой pre-flight (handler → 500).
type AssertPreflighter interface {
	PreflightAssert(ctx context.Context, spec scenario.RunSpec) error
}

// DestroyStarter — узкая поверхность scenario.Runner, нужная Destroy-handler-у:
// async-запуск teardown-прогона scenario `destroy` в режиме TerminalDestroy
// (S-D2b). Отдельный интерфейс от [ScenarioStarter]: Destroy использует
// StartDestroy (фиксирует ScenarioName=destroy + TerminalMode), а не Start.
// Реальный *scenario.Runner удовлетворяет обоим.
type DestroyStarter interface {
	StartDestroy(ctx context.Context, spec scenario.RunSpec) error
}

// DriftChecker — узкая поверхность scenario.Runner для check-drift-handler-а
// (ADR-031, Slice B). CheckDrift sync (не async, в отличие от Start/StartDestroy):
// handler блокируется до сборки DriftReport, чтобы вернуть его оператору в 200-
// ответе. MarkDriftStatus — post-check информационная маркировка
// incarnation.status (drift/ready). Реальный *scenario.Runner удовлетворяет.
type DriftChecker interface {
	CheckDrift(ctx context.Context, spec scenario.CheckDriftSpec) (*scenario.DriftReport, error)
	MarkDriftStatus(ctx context.Context, name string, hasDrift bool) error
}

// ServiceResolver резолвит git-координаты service-репо по имени сервиса
// (`incarnation.service` → реестр сервисов в БД, ADR-029). Используется
// Create-handler-ом при запуске scenario `create`.
type ServiceResolver interface {
	Resolve(service string) (artifact.ServiceRef, bool)
}

// ServiceSnapshotLoader — узкая поверхность [artifact.ServiceLoader], нужная
// Upgrade- и Destroy-handler-ам: материализовать снапшот целевого service-ref-а
// (для чтения `state_schema_version` из его `service.yml`), собрать цепочку
// state_schema-миграций current→target (Upgrade) и читать файлы снапшота
// (Destroy pre-check наличия scenario `destroy`, [incarnation.PrepareDestroy]).
// Интерфейс (а не *artifact.ServiceLoader) — чтобы unit-тесты handler-а не
// поднимали git-стек. ReadFile роднит контракт с [incarnation.DestroyScenarioReader]
// (Load + ReadFile) — *artifact.ServiceLoader удовлетворяет обоим.
type ServiceSnapshotLoader interface {
	Load(ctx context.Context, ref artifact.ServiceRef) (*artifact.ServiceArtifact, error)
	LoadMigrationChain(art *artifact.ServiceArtifact, from, to int) (statemigrate.Chain, error)
	ReadFile(art *artifact.ServiceArtifact, file string) ([]byte, error)
	// ListUpgrades — скан upgrade/<slug>/ целевого снапшота (ADR-0068): нужен
	// Upgrade-handler-у, чтобы incarnation.PrepareUpgrade резолвил found/legacy.
	ListUpgrades(art *artifact.ServiceArtifact) ([]artifact.Scenario, error)
}

// IncarnationHandler — handler-ы endpoints incarnation:
// Create / Get / List / History.
//
// runner / services — опциональны: при nil Create деградирует до M0.6c-1
// stub-режима (insert row со status=ready, без запуска scenario). Production
// wire-up (`keeper run`) передаёт оба — Create вставляет row и запускает
// scenario `create`, который сам переводит incarnation applying → ready /
// error_locked.
//
// loader — опционален: нужен только Upgrade-handler-у (материализация
// снапшота целевого service-ref-а + сборка migration-chain). При nil Upgrade
// отвечает 500 (endpoint не сконфигурирован), симметрично Run без runner-а.
//
// pool для UpgradeStateSchema — это сам `db`: IncarnationDB встраивает
// [incarnation.TxBeginner], отдельной зависимости не требуется.
//
// destroyer / auditW — для Destroy-handler-а (S-D4): destroyer запускает
// teardown-прогон (StartDestroy), auditW передаётся в [incarnation.Destroy] /
// [incarnation.DeleteAfterTeardown] для записи destroy_started / destroy_completed
// (audit пишет service-слой, а не permission-middleware — см. router.go). Оба
// допускают nil: без destroyer+services+loader Destroy отвечает 500 (endpoint не
// сконфигурирован, симметрично Run/Upgrade); auditW=nil → trail destroy не пишется
// (допустимо в unit-тестах).
//
// Все зависимости immutable; safe for concurrent use.
type IncarnationHandler struct {
	db        IncarnationDB
	runner    ScenarioStarter
	destroyer DestroyStarter
	drift     DriftChecker
	services  ServiceResolver
	loader    ServiceSnapshotLoader
	auditW    audit.Writer
	scoper    PurviewResolver
	logger    *slog.Logger
}

// NewIncarnationHandler создаёт handler. runner / destroyer / drift / services /
// loader / auditW допускают nil: без runner+services Create деградирует до
// stub-а, без loader Upgrade отвечает 500, без destroyer+services+loader Destroy
// отвечает 500, без drift+services CheckDrift отвечает 500, без auditW
// destroy/drift-trail не пишется.
//
// scoper — read-поверхность scope-границы оператора ([PurviewResolver],
// production-wire-up передаёт rbac.Holder) для scoped-видимости List/Get
// (ADR-047 S3b-3, coven∪{name} + state-CEL измерения Purview). nil допустим
// только в тестах, не использующих List/Get scope: List при nil-scoper
// fail-closed (пустой список — безопасный дефолт, НЕ все incarnation), Get при
// nil-scoper fail-closed (404 — не палим чужую incarnation).
func NewIncarnationHandler(db IncarnationDB, runner ScenarioStarter, destroyer DestroyStarter, drift DriftChecker, services ServiceResolver, loader ServiceSnapshotLoader, auditW audit.Writer, scoper PurviewResolver, logger *slog.Logger) *IncarnationHandler {
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	return &IncarnationHandler{db: db, runner: runner, destroyer: destroyer, drift: drift, services: services, loader: loader, auditW: auditW, scoper: scoper, logger: logger}
}

// ContextReader возвращает read-поверхность БД handler-а для RBAC-экстрактора
// [IncarnationScopeSelector] (приземление service/covens существующей
// incarnation в permission-context). Сам `db` — [IncarnationDB], встраивающий
// [incarnation.ExecQueryRower]; экстрактору достаточно read-части. nil при
// db=nil (stub-конструкция в drift-тесте — экстрактор там не вызывается).
func (h *IncarnationHandler) ContextReader() IncarnationContextReader {
	if h.db == nil {
		return nil
	}
	return h.db
}

// --- host-role валидация (PATCH .../hosts) ----------------------------

// hostsRolePattern — kebab-case role-label (lowercase + дефисы), 1..63 символа.
// declared-роль — operator-asserted строка из `incarnation.spec.hosts[].role`
// (ADR-008): значения не предопределены в коде (master/replica — частые, но не
// исчерпывающие), поэтому валидируем только форму, как у Coven-меток (тот же
// kebab-case-инвариант, нет конфликта с грамматикой scenario-on:).
const hostsRolePattern = `^[a-z][a-z0-9]*(-[a-z0-9]+)*$`

var hostsRoleRe = regexp.MustCompile(hostsRolePattern)

func validHostRole(role string) bool {
	if role == "" {
		return true
	}
	if len(role) > 63 {
		return false
	}
	return hostsRoleRe.MatchString(role)
}

// specHostsToPayload — снимок hosts[] для audit-payload. Симметрично
// jsonb-форме `spec.hosts` (см. [incarnation.readSpecHosts]).
func specHostsToPayload(hosts []incarnation.SpecHost) []map[string]any {
	out := make([]map[string]any, 0, len(hosts))
	for _, h := range hosts {
		obj := map[string]any{"sid": h.SID}
		if h.Role != "" {
			obj["role"] = h.Role
		}
		out = append(out, obj)
	}
	return out
}

// --- RBAC scope (ADR-047 S3b-3) ---------------------------------------

// incStateResolver — общий statepredicate.Resolver для scoped List/Get (state-CEL
// измерение Purview). НЕ дублирует CEL-движок: statepredicate делегирует
// shared/cel (migration-sandbox, корень `state`). Один резолвер на процесс
// (потокобезопасен, общий compile-cache) — лениво один раз, как rbac.stateResolver.
var (
	incStateResolverOnce sync.Once
	incStateResolverInst statepredicate.Resolver
	incStateResolverErr  error
)

func incStateResolver() (statepredicate.Resolver, error) {
	incStateResolverOnce.Do(func() {
		incStateResolverInst, incStateResolverErr = statepredicate.New()
	})
	return incStateResolverInst, incStateResolverErr
}

// resolveStateNames возвращает имена incarnation-ов, чей state удовлетворяет
// объединённому OR-предикату state-CEL scope (StateExprs склеены в `p1 || ...`).
// Переиспользует statepredicate.ResolveIncarnations поверх incarnation.StateLister
// (page-by-page pushdown). serviceFilter сужает множество CEL-eval до того же
// сервиса (BaseFilter pushdown), что и основной запрос.
func (h *IncarnationHandler) resolveStateNames(ctx context.Context, exprs []string, serviceFilter string) ([]string, error) {
	resolver, err := incStateResolver()
	if err != nil {
		return nil, fmt.Errorf("incarnation: state-scope CEL engine: %w", err)
	}
	combined := joinStateExprs(exprs)
	lister := incarnation.NewStateLister(h.db)
	return resolver.ResolveIncarnations(ctx, combined, statepredicate.BaseFilter{Service: serviceFilter}, lister)
}

// joinStateExprs склеивает несколько state-CEL-предикатов в одно OR-выражение
// `(p1) || (p2) || ...` (union внутри измерения — «доступно по любому из»). Один
// предикат отдаётся как есть. Каждый оборачивается в скобки: предикат
// провалидирован на load снимка как самостоятельное bool-выражение, скобки
// сохраняют его границу при склейке (приоритет `||`).
func joinStateExprs(exprs []string) string {
	if len(exprs) == 1 {
		return exprs[0]
	}
	parts := make([]string, len(exprs))
	for i, e := range exprs {
		parts[i] = "(" + e + ")"
	}
	return strings.Join(parts, " || ")
}

// scopeEmpty — true для fail-closed Purview: не Unrestricted и ни одного
// введённого измерения, значимого для incarnation-scope (Covens / StateExprs /
// TraitExprs). regex/soulprint — soul-факты, к incarnations НЕ применяются (ТЗ
// S3b-3), потому в счёт «введённых измерений» здесь НЕ идут: Purview только с
// soulprint/regex (без coven/state/trait) для incarnation-scope = пусто (нечего
// матчить) → fail-closed. Deny (заготовка S2) трактуется как fail-closed.
func scopeEmpty(pv rbac.Purview) bool {
	if pv.Deny {
		return true
	}
	return len(pv.Covens) == 0 && len(pv.StateExprs) == 0 && len(pv.TraitExprs) == 0
}

// statePredicateOps — допустимые префиксы оператора в query-значении
// `state.<field>=<op>:<value>`. Маппинг operator-facing-имени → [incarnation.StateOp].
var statePredicateOps = map[string]incarnation.StateOp{
	"eq":  incarnation.StateOpEq,
	"ne":  incarnation.StateOpNe,
	"gt":  incarnation.StateOpGt,
	"gte": incarnation.StateOpGte,
	"lt":  incarnation.StateOpLt,
	"lte": incarnation.StateOpLte,
}

// parseStatePredicatesFromMap — request-free парсер state-предикатов для FULL-TYPED
// ListTyped (huma-слой биндит typed-query, доменная функция не видит *http.Request).
// Принимает уже-собранную карту `state.<field>` → значения (caller фильтрует query по
// префиксу). Field-часть валидируется форматным whitelist-ом [statePathQueryPattern] —
// невалидный path/op → ошибка (handler маппит в 422), инъекция в jsonb-идентификатор не
// доходит до CRUD/БД. Только первое значение каждого ключа (multi-value на один и тот же
// state-ключ — не MVP). Возвращает nil без ошибки, если state-фильтров нет.
func parseStatePredicatesFromMap(stateParams map[string][]string) ([]incarnation.StateEq, error) {
	var preds []incarnation.StateEq
	for field, vals := range stateParams {
		if !statePathQueryPattern.MatchString(field) {
			return nil, fmt.Errorf("query 'state.%s': field must match [a-z][a-z0-9_]*", field)
		}
		raw := ""
		if len(vals) > 0 {
			raw = vals[0]
		}
		op := incarnation.StateOpEq
		value := raw
		if prefix, rest, found := strings.Cut(raw, ":"); found {
			mapped, known := statePredicateOps[prefix]
			if !known {
				return nil, fmt.Errorf("query 'state.%s': unknown operator %q (eq/ne/gt/gte/lt/lte)", field, prefix)
			}
			op = mapped
			value = rest
		}
		preds = append(preds, incarnation.StateEq{Path: field, Op: op, Value: value})
	}
	return preds, nil
}

// statePathQueryPattern дублирует CRUD-side whitelist (forматный reject уже на
// handler-уровне → чистый 422 без round-trip-а). Источник правды — CRUD-side
// валидация в SelectAll (defense in depth).
var statePathQueryPattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// GetInScopeFor — request-free фабрика scope-предиката для FULL-TYPED GetTyped/
// HistoryTyped (huma-слой). claims/action приходят явно (вместо чтения из
// *http.Request). Семантика: Unrestricted → true; пустой/Deny Purview → false;
// coven∪{name} ИЛИ state-CEL match → true. nil-claims/nil-scoper → предикат
// всегда false (fail-closed).
func (h *IncarnationHandler) GetInScopeFor(claims *jwt.Claims, action string) func(*incarnation.Incarnation) bool {
	return func(inc *incarnation.Incarnation) bool {
		if claims == nil || h.scoper == nil {
			return false
		}
		pv := h.scoper.ResolvePurview(claims.Subject, "incarnation", action)
		if pv.Unrestricted {
			return true
		}
		if scopeEmpty(pv) {
			return false
		}
		for _, sc := range pv.Covens {
			if sc == inc.Name {
				return true
			}
			for _, c := range inc.Covens {
				if c == sc {
					return true
				}
			}
		}
		for _, expr := range pv.StateExprs {
			matched, err := rbac.EvalStateExpr(expr, inc.State)
			if err != nil {
				h.logger.Warn("incarnation.get: state-CEL eval упал — предикат не даёт доступ",
					slog.String("name", inc.Name), slog.Any("error", err))
				continue
			}
			if matched {
				return true
			}
		}
		// trait-измерение (ADR-047 amendment, ADR-060 п.7 slice 1): scope-пара
		// `key:value` даёт доступ, если incarnation.traits[key] == value (scalar).
		for _, pair := range pv.TraitExprs {
			key, value, ok := splitTraitPair(pair)
			if !ok {
				continue
			}
			if traitScalarEquals(inc.Traits, key, value) {
				return true
			}
		}
		return false
	}
}

// splitTraitPair разбивает trait-scope-строку `key:value` (нормализованную
// [rbac.parseTraitValue] — ровно одна `:`, непустые половины) на ключ и значение.
// ok=false при отсутствии `:` (defensive против рассинхрона с парсером).
func splitTraitPair(pair string) (key, value string, ok bool) {
	return strings.Cut(pair, ":")
}

// traitScalarEquals — true, если traits[key] — скаляр, строковая форма которого
// равна value (slice 1 — scalar-only trait-scope). list-Trait одним равенством
// не покрывается (follow-up), поэтому non-scalar → false. fmt.Sprint даёт
// каноничную строку для string/число/bool (jsonb-числа приходят float64/json.Number).
func traitScalarEquals(traits map[string]any, key, value string) bool {
	v, ok := traits[key]
	if !ok {
		return false
	}
	switch v.(type) {
	case string, float64, bool, json.Number, int, int64:
		return fmt.Sprint(v) == value
	default:
		// map / slice (list-Trait) — не scalar-match (slice 1 не покрывает).
		return false
	}
}

// ResolveListScopeFor — request-free фабрика scope-резолвера для FULL-TYPED ListTyped
// (huma-слой). Семантика: fail-closed при nil-claims/nil-scoper/Empty Purview (caller
// отдаёт пустой список); Unrestricted → весь список; иначе coven∪{name}-pushdown ∪
// предрезолв имён по state-CEL.
func (h *IncarnationHandler) ResolveListScopeFor(ctx context.Context, claims *jwt.Claims) func(serviceFilter string) (incarnation.ListScope, bool) {
	return func(serviceFilter string) (incarnation.ListScope, bool) {
		return h.resolveListScope(ctx, claims, "list", serviceFilter)
	}
}

// resolveListScope — общий Purview→[incarnation.ListScope] резолв list-подобных
// read-ов (List action=list; глобальные runs/stats action=history). Семантика —
// как у [IncarnationHandler.ResolveListScopeFor] (та же fail-closed граница).
func (h *IncarnationHandler) resolveListScope(ctx context.Context, claims *jwt.Claims, action, serviceFilter string) (incarnation.ListScope, bool) {
	if claims == nil || h.scoper == nil {
		return incarnation.ListScope{}, false
	}
	pv := h.scoper.ResolvePurview(claims.Subject, "incarnation", action)
	if pv.Unrestricted {
		return incarnation.ListScope{Unrestricted: true}, true
	}
	if scopeEmpty(pv) {
		return incarnation.ListScope{}, false
	}
	scope := incarnation.ListScope{Covens: pv.Covens}
	// state-измерение fail-OPEN: резолв упал → НЕ расширяем выдачу его именами,
	// но coven-измерение остаётся в силе (НЕ роняем весь List). Логируем.
	if len(pv.StateExprs) > 0 {
		names, err := h.resolveStateNames(ctx, pv.StateExprs, serviceFilter)
		if err != nil {
			h.logger.Warn("incarnation."+action+": state-scope резолв упал — применяется только coven-измерение (fail-closed по state)",
				slog.String("aid", claims.Subject), slog.Any("error", err))
		} else {
			scope.StateNames = names
		}
	}
	// trait-измерение (ADR-047 amendment, ADR-060 п.7 slice 1): scope-пары
	// `key:value` → SQL-pushdown `traits->>$key = $value` (scalar-equality, без
	// CEL/резолва; НЕ containment `@>` — BUG#1 fix, [incarnation.appendScopeClause]).
	// Битая пара (рассинхрон с парсером) пропускается, не роняет List.
	for _, pair := range pv.TraitExprs {
		key, value, ok := splitTraitPair(pair)
		if !ok {
			continue
		}
		scope.Traits = append(scope.Traits, incarnation.TraitPair{Key: key, Value: value})
	}
	return scope, true
}

// --- RBAC scope-селекторы роутов --------------------------------------

// IncarnationContextReader — read-поверхность для RBAC-экстракторов
// incarnation-роутов: «верни service + declared covens incarnation по имени».
// Реализуется [IncarnationDB] (через [incarnation.SelectByName]); экстрактор
// держит её в замыкании, чтобы приземлить scope-атрибуты самой incarnation в
// RBAC-context (ADR-008 amendment a; architect: контекст одномерный —
// атрибуты incarnation, не bulk по хостам, поэтому доступ к данным в
// экстракторе чище переноса проверки в handler).
type IncarnationContextReader interface {
	incarnation.ExecQueryRower
}

// incarnationCovenContexts разворачивает coven-scope incarnation в набор
// per-кандидат RBAC-контекстов для [middleware.RequirePermissionMulti].
//
// Эффективный coven-scope = covens ∪ {name} (declared env-теги + имя как
// корневая Coven-метка, ADR-008). Каждый кандидат → отдельный context
// `{incarnation, service, coven=<кандидат>}`; permission-проверка OR-ит их.
// service кладётся во ВСЕ контексты (service-only-permission матчит при любой
// coven-итерации). Дедуп — имя может уже быть в covens.
//
// Пустой name → nil (caller вернёт 422 на битый path до RBAC, либо create
// передаёт name=своё-имя).
//
// IncarnationCovenContexts — экспортированная обёртка над
// [incarnationCovenContexts] для переиспользования вне пакета (MCP
// incarnation-tools зеркалят REST coven/service-scope, RBAC-паритет
// endpoint↔MCP).
func IncarnationCovenContexts(name, service string, covens []string) []map[string]string {
	return incarnationCovenContexts(name, service, covens)
}

func incarnationCovenContexts(name, service string, covens []string) []map[string]string {
	if name == "" {
		return nil
	}
	seen := make(map[string]struct{}, len(covens)+1)
	candidates := make([]string, 0, len(covens)+1)
	add := func(c string) {
		if c == "" {
			return
		}
		if _, ok := seen[c]; ok {
			return
		}
		seen[c] = struct{}{}
		candidates = append(candidates, c)
	}
	for _, c := range covens {
		add(c)
	}
	add(name) // имя — корневая Coven-метка (ADR-008).

	out := make([]map[string]string, 0, len(candidates))
	for _, c := range candidates {
		ctx := map[string]string{"incarnation": name, "coven": c}
		if service != "" {
			ctx["service"] = service
		}
		out = append(out, ctx)
	}
	return out
}

// IncarnationScopeSelector конструирует [middleware.MultiSelectorExtractor] для
// роутов над СУЩЕСТВУЮЩЕЙ incarnation (get / history / run / unlock / upgrade /
// destroy): читает строку incarnation по path-`{name}` через reader и
// приземляет в RBAC-context `incarnation=<name>`, `service=<inc.service>` и
// multi-value `coven=` (covens ∪ {name}) — закрывает docs↔code drift, когда
// роли `incarnation.* on coven=…` / `on service=…` молча НЕ матчили
// (ADR-008 amendment a).
//
// Тот же [incarnation.SelectByName], что эти роуты и так делают в handler-е
// (двойной select — холодный путь RBAC-гейта, не hot path; альтернатива —
// тащить inc из middleware в handler через context — лишняя связность ради
// одного round-trip-а на не-bulk-операцию).
//
// Fail-closed: невалидный/пустой name или incarnation не найдена → nil-набор;
// [middleware.RequirePermissionMulti] тогда пропускает только bare-/`*`-роли
// (scoped — deny). 404 для несуществующей incarnation handler вернёт сам после
// прохождения bare-/`*`-оператора (паритет прежнего поведения: RBAC раньше
// тоже не знал о существовании incarnation).
func IncarnationScopeSelector(reader IncarnationContextReader) middleware.MultiSelectorExtractor {
	return func(r *http.Request) []map[string]string {
		name := chi.URLParam(r, "name")
		if !incarnation.ValidName(name) {
			return nil
		}
		inc, err := incarnation.SelectByName(r.Context(), reader, name)
		if err != nil {
			// Не найдена / ошибка БД → fail-closed для scoped-ролей. bare/`*`
			// пройдут пустой набор, handler вернёт 404 / 500 как раньше.
			return nil
		}
		return incarnationCovenContexts(inc.Name, inc.Service, inc.Covens)
	}
}

// IncarnationCreateScopeSelector — [middleware.MultiSelectorExtractor] для
// `POST /v1/incarnations` (incarnation ещё нет): scope из ТЕЛА запроса —
// `service=<body.service>` + multi-value `coven=` из declared `body.covens` ∪
// `{body.name}`. Не даёт coven-scoped оператору создать incarnation с тегом
// вне своего scope (least-privilege; иначе create = privilege-escalation).
//
// Body читается под уже навешенным `/v1/*` MaxBytesReader-лимитом и
// восстанавливается для handler-а (паттерн [SoulCovenLabelSelector]): handler
// декодирует тело повторно (strict-декодер). Невалидный/пустой body или
// битый name → nil-набор: scoped-роли — deny, bare/`*` — pass (handler затем
// вернёт 400/422 на теле). covens из тела НЕ валидируются по формату здесь
// (это делает handler перед insert); невалидная метка просто не сматчит ни
// одну корректную permission (scoped → deny), bare/`*` — pass, handler вернёт 422.
func IncarnationCreateScopeSelector(r *http.Request) []map[string]string {
	if r.Body == nil {
		return nil
	}
	body, err := io.ReadAll(r.Body)
	_ = r.Body.Close()
	r.Body = io.NopCloser(bytes.NewReader(body))
	if err != nil || len(body) == 0 {
		return nil
	}
	var probe struct {
		Name    string   `json:"name"`
		Service string   `json:"service"`
		Covens  []string `json:"covens"`
	}
	if err := json.Unmarshal(body, &probe); err != nil || probe.Name == "" {
		return nil
	}
	return incarnationCovenContexts(probe.Name, probe.Service, probe.Covens)
}
