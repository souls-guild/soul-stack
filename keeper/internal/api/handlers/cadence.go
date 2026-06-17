package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	"github.com/souls-guild/soul-stack/keeper/internal/cadence"
	"github.com/souls-guild/soul-stack/keeper/internal/herald"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac"
	"github.com/souls-guild/soul-stack/keeper/internal/soul"
	"github.com/souls-guild/soul-stack/keeper/internal/voyage"
	sharedapi "github.com/souls-guild/soul-stack/shared/api"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// CadenceStore — поверхность пакета [cadence] для S4 HTTP/MCP handler-а.
//
//   - cadence.ExecQueryRower — read + одношаговые write (Get/List/Update/Delete/
//     SetEnabled без транзакции: одна строка cadences, FK voyages.cadence_id на
//     дочерней строке).
//   - voyage.ExecQueryRower — `GET /v1/cadences/{id}/runs` делает read-only
//     voyage.List по тому же пулу (CopyFrom в read-пути не вызывается, но входит
//     в voyage-интерфейс).
//   - BeginTx — atomic Create с блоком notify (ADR-052 §m): Insert Cadence +
//     InsertTiding постоянных правил из notify[] в ОДНОЙ PG-tx (либо обе записи,
//     либо rollback — иначе осиротевшее правило/расписание без второй половины).
//
// Реальный *pgxpool.Pool удовлетворяет всем; unit-тесты — fake.
type CadenceStore interface {
	cadence.ExecQueryRower
	voyage.ExecQueryRower
	BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error)
}

// CadenceHandler — handler-ы endpoints Cadence (ADR-046 §6, S4):
//
//	POST   /v1/cadences              — создать Cadence (двухуровневый RBAC-by-kind).
//	GET    /v1/cadences              — paged list (filter enabled/kind).
//	GET    /v1/cadences/{id}         — деталь.
//	PATCH  /v1/cadences/{id}         — обновить (рецепт/расписание/enabled-toggle).
//	DELETE /v1/cadences/{id}         — снять расписание (дети-Voyage остаются).
//	POST   /v1/cadences/{id}/enable  — включить (пауза/возобновление, ADR-046 §6).
//	POST   /v1/cadences/{id}/disable — выключить.
//	GET    /v1/cadences/{id}/runs    — дочерние Voyage (reuse Voyage-DTO).
//
// Двухуровневый RBAC (ADR-046 §7, security-критичный fail-closed): право
// `cadence.*` управляет самим расписанием, но рецепт спавнит Voyage, поэтому при
// СОЗДАНИИ создатель обязан иметь и Voyage-permission по `kind` рецепта
// (scenario→incarnation.run, command→errand.run, ADR-043 §6) — иначе Cadence
// стала бы privilege-escalation-обходом RBAC. `cadence.create` гейтится в router-е
// (middleware), Voyage-permission по kind виден только из тела → проверяется
// ВНУТРИ Create. list/get/update/delete/enable/disable гейтятся middleware-route-ом
// (cadence.list / cadence.update / cadence.delete). `/runs` — incarnation.history
// (read runtime-состояния прогонов, parity Voyage-list).
//
// store/enforcer обязательны для production-маршрутов; auditW допускает nil
// (dev без audit). Резолв next_run_at — чистая [cadence.NextRun] (без БД).
//
// scenarioResolver/incReader — per-target coven-scope-check рецепта kind=scenario
// (ADR-046 §7, security-критичный fail-closed): target Cadence обязан лежать в
// RBAC-скоупе создателя на момент создания/правки, иначе scoped-Архонт «run on
// coven=A» создал бы Cadence на coven=B (вне scope) и фоновый спавн (от
// created_by_aid) исполнил бы вне scope = RBAC-bypass. Полная parity
// VoyageHandler.createScenario: тот же резолв инкарнаций + per-incarnation
// scope-loop. incReader=nil → fail-closed (как Voyage): scoped-роли отвергаются,
// проходит лишь bare/`*`-право из checkKindPermission. command-kind scope —
// bare-check (parity Voyage errand.run NoSelector в MVP).
type CadenceHandler struct {
	store            CadenceStore
	scenarioResolver VoyageScenarioResolver
	incReader        IncarnationContextReader
	enforcer         middleware.PermissionChecker
	auditW           audit.Writer
	// tidingInvalidator сбрасывает TTL-снимок Tiding-правил dispatcher-а после
	// commit cadence-tx с блоком notify (ADR-052 §m, тот же race-fix, что у Voyage-
	// ephemeral): постоянные правила вставлены прямым herald.InsertTiding в обход
	// herald.Service-CRUD (и его инвалидации), поэтому dispatcher держит их за TTL-
	// снимком (15s) — без явного сброса быстрый/cross-keeper спавн диспетчит
	// терминал против устаревшего снимка и уведомление молча промахивается. Тот же
	// *herald.Service-инстанс, что у VoyageHandler. nil (dev без herald) → no-op.
	tidingInvalidator TidingInvalidator
	// pollFloorSeconds — нижний предел периода interval-Cadence (floor-лимит,
	// ADR-046 Pass B). Единый источник с адаптивным опросом Conductor
	// (cfg.CadenceScheduler.ResolvedPollFloor()). 0 → floor-проверка выключена.
	pollFloorSeconds int
	logger           *slog.Logger
}

// NewCadenceHandler собирает handler. logger=nil → discard. store/enforcer —
// обязательны для production-маршрутов; auditW допускает nil. scenarioResolver/
// incReader — те же экземпляры, что у [VoyageHandler], для per-target scope-check
// рецепта kind=scenario (ADR-046 §7). incReader=nil → fail-closed: scoped-роли
// scenario-create/patch отвергаются (как у Voyage). tidingInvalidator — тот же
// *herald.Service, что у VoyageHandler: сбрасывает TTL-снимок dispatcher-а после
// commit cadence-tx с блоком notify (ADR-052 §m race-fix); nil → no-op (dev без
// herald).
func NewCadenceHandler(
	store CadenceStore,
	scenarioResolver VoyageScenarioResolver,
	incReader IncarnationContextReader,
	enforcer middleware.PermissionChecker,
	auditW audit.Writer,
	tidingInvalidator TidingInvalidator,
	pollFloorSeconds int,
	logger *slog.Logger,
) *CadenceHandler {
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	return &CadenceHandler{
		store:             store,
		scenarioResolver:  scenarioResolver,
		incReader:         incReader,
		enforcer:          enforcer,
		auditW:            auditW,
		tidingInvalidator: tidingInvalidator,
		pollFloorSeconds:  pollFloorSeconds,
		logger:            logger,
	}
}

// --- POST /v1/cadences ---

// cadenceCreateRequest — POST body. snake_case, unknown-поля отвергаем. Рецепт
// прогона — то же множество, что [voyageCreateRequest] (kind/scenario_name|module/
// target/input/batch-настройки) + правило повторения + overlap_policy. target
// сериализуется в jsonb as-is (резолв — на спавне, не на создании, ADR-046 §3).
type cadenceCreateRequest struct {
	Name            string `json:"name"`
	Enabled         *bool  `json:"enabled,omitempty"`
	ScheduleKind    string `json:"schedule_kind"`
	IntervalSeconds *int   `json:"interval_seconds,omitempty"`
	CronExpr        string `json:"cron_expr,omitempty"`
	OverlapPolicy   string `json:"overlap_policy"`

	// Рецепт прогона (parity voyageCreateRequest).
	Kind         string               `json:"kind"`
	ScenarioName string               `json:"scenario_name,omitempty"`
	Module       string               `json:"module,omitempty"`
	Input        map[string]any       `json:"input,omitempty"`
	Target       *voyageTargetRequest `json:"target"`
	// Batch — строковый размер батча ("N" хостов/инкарнаций / "N%" от spawn-scope),
	// parity voyageCreateRequest.Batch (ADR-043 amendment). Маппится на
	// batch_size|batch_percent колонки (см. applyCadenceBatchSpec): "N%" →
	// batch_percent (резолвится на spawn-scope в BuildVoyage), "N" → batch_size.
	// Конфликтует с batch_size/batch_percent → 422.
	Batch        *string `json:"batch,omitempty"`
	BatchSize    *int    `json:"batch_size,omitempty"`
	BatchPercent *int    `json:"batch_percent,omitempty"`
	Concurrency  *int    `json:"concurrency,omitempty"`
	BatchMode    string  `json:"batch_mode,omitempty"`
	// MaxFailures — строковый порог провалов ("N" абсолют / "N%" процент от
	// spawn-scope), parity voyageCreateRequest.MaxFailures (ADR-043 amendment
	// 2026-06-09). Маппится на fail_threshold|fail_threshold_percent колонки (см.
	// applyCadenceMaxFailures): "N%" → fail_threshold_percent (резолвится на
	// spawn-scope в BuildVoyage — у Cadence scope неизвестен на создании, в отличие
	// от Voyage), "N" → fail_threshold. Конфликтует с fail_threshold → 422.
	MaxFailures          *string `json:"max_failures,omitempty"`
	FailThreshold        *int    `json:"fail_threshold,omitempty"`
	InterBatchIntervalMS *int    `json:"inter_batch_interval_ms,omitempty"`
	InterUnitIntervalMS  *int    `json:"inter_unit_interval_ms,omitempty"`
	RequireAlive         *bool   `json:"require_alive,omitempty"`
	OnFailure            string  `json:"on_failure,omitempty"`

	// Notify — подписки на уведомления о прогонах ЭТОГО расписания (ADR-052 §m).
	// В отличие от voyage.notify[] (разовое ephemeral-правило на один прогон),
	// здесь каждый элемент материализуется в ПОСТОЯННЫЙ Tiding (ephemeral=false),
	// привязанный по ULID расписания (cadences.id) — селектором Cadence (фильтр
	// «слать только про прогоны этого расписания») + origin-маркером
	// created_from_cadence_id (каскад-снос при удалении Cadence, ADR-046 §9).
	// Insert правил идёт в ТОЙ ЖЕ tx, что Insert Cadence. nil/пусто ⇒ без
	// уведомлений. Форма элемента — та же [voyageNotifyRequest] (herald/on/
	// only_failures/only_changes/annotations/projection), reuse валидации/RBAC.
	Notify []voyageNotifyRequest `json:"notify,omitempty"`

	// failThresholdPercent — необёрнутый процент из max_failures="N%",
	// заполняется applyCadenceMaxFailures для записи в колонку
	// fail_threshold_percent (резолв в абсолют — на spawn-scope в BuildVoyage).
	// Не JSON-поле: задаётся только через max_failures.
	failThresholdPercent *int
}

// cadenceCreateReply — native 201 body (handler-native T5d). Плоская форма 1:1 с
// прежним CadenceCreateReply (все скаляры; next_run_at — *time.Time с omitempty).
// Сериализуется напрямую и проецируется в api.CadenceCreateReply (huma-schema).
type cadenceCreateReply struct {
	CadenceID string     `json:"cadence_id"`
	Enabled   bool       `json:"enabled"`
	Location  string     `json:"location"`
	Name      string     `json:"name"`
	NextRunAt *time.Time `json:"next_run_at,omitempty"`
}

// CadenceCreateReply / CadenceCreateRequest — экспортируемые алиасы доменных
// форм POST /v1/cadences для FULL-TYPED huma-конверта (ADR-054 §Pattern): api-
// пакет (huma_cadence.go) собирает CadenceCreateRequest из typed huma-body и
// зовёт [CadenceHandler.CreateTyped], получая CadenceCreateReply. Алиасы (не
// новые типы) — та же форма, что декодит legacy (w,r)-Create; вложенные
// target/notify — [VoyageTargetRequest]/[VoyageNotifyRequest] (общие с Voyage).
type (
	CadenceCreateReply   = cadenceCreateReply
	CadenceCreateRequest = cadenceCreateRequest
	VoyageTargetRequest  = voyageTargetRequest
	VoyageNotifyRequest  = voyageNotifyRequest
	// CadencePatchRequest / CadenceDTO / CadenceEnabledReply — экспортируемые
	// алиасы доменных форм cadence-rest-роутов (PATCH/DELETE/enable/disable) для
	// FULL-TYPED huma-конверта (ADR-054, батч-2f self-audit): api-пакет
	// (huma_cadence_op.go) собирает CadencePatchRequest из typed huma-body и зовёт
	// [CadenceHandler.PatchTyped], получая CadenceDTO; SetEnabledTyped возвращает
	// CadenceEnabledReply. Алиасы (не новые типы) — та же форма, что декодит легаси
	// (w,r)-Patch/setEnabled.
	CadencePatchRequest = cadencePatchRequest
	CadenceDTO          = cadenceDTO
	CadenceEnabledReply = cadenceEnabledReply
)

// cadenceEnabledReply — native 200 body POST /v1/cadences/{id}/enable|/disable
// (handler-native T5d). Плоская форма 1:1 с прежним CadenceEnabledReply
// (cadence_id + enabled). Сериализуется напрямую и проецируется в api.CadenceEnabledReply.
type cadenceEnabledReply struct {
	CadenceID string `json:"cadence_id"`
	Enabled   bool   `json:"enabled"`
}

// CadenceListReply — typed-выход GET /v1/cadences: paged cadenceDTO той же wire-формы,
// что legacy (w,r)-List (items/offset/limit/total через sharedapi.PagedResponse →
// byte-exact). Alias (не новый тип) — экспортируется для FULL-TYPED huma-конверта
// (huma_cadence_list_op.go).
type CadenceListReply = sharedapi.PagedResponse[cadenceDTO]

// problemError — typed-обёртка над [problem.Details] для возврата из извлечённых
// доменных функций (FULL-TYPED разворот ADR-054, §Pattern (б)). Доменная функция
// (например, [CadenceHandler.CreateTyped]) вместо problem.Write(w, …) возвращает
// (zeroReply, &problemError{<details>}); вызывающий слой решает, как его доставить:
//
//   - huma-конверт (api-пакет) извлекает .Details и отдаёт через humaProblemError
//     (единый problem+json huma-error-override);
//   - тонкая (w,r)-оболочка handler-а (для strict-моста/прочих вызовов) пишет
//     problem.Write(w, pe.Details) с проставленным instance=r.URL.Path.
//
// Тип семейства humaProblemError (тоже несёт problem.Details) — общий контракт
// «ошибка = problem.Details» по обе стороны границы.
type problemError struct {
	Details problem.Details
}

func (e *problemError) Error() string { return e.Details.Detail }

// asProblemError извлекает [problem.Details] из ошибки доменной функции. ok=false —
// ошибка не доменная problem (нештатный путь): вызывающий слой маппит в 500.
func asProblemError(err error) (problem.Details, bool) {
	var pe *problemError
	if errors.As(err, &pe) {
		return pe.Details, true
	}
	return problem.Details{}, false
}

// AsProblemDetails — экспортируемый извлекатель [problem.Details] из ошибки
// извлечённой доменной функции (FULL-TYPED ADR-054 §Pattern): huma-конверт
// (api-пакет) маппит доменный *problemError в problem+json. ok=false — не-problem
// ошибка → caller отдаёт 500.
func AsProblemDetails(err error) (problem.Details, bool) {
	return asProblemError(err)
}

// CadenceSpecStub — непустой *CadenceHandler-заглушка для генерации huma-OpenAPI-
// фрагмента (HumaCadenceSpecYAML): при dump доменный handler не вызывается, но
// huma.Register требует non-nil для no-op-проверки. Все зависимости nil — handler
// никогда не исполняется в spec-режиме.
func CadenceSpecStub() *CadenceHandler {
	return &CadenceHandler{}
}

// CreateTyped — извлечённая доменная функция POST /v1/cadences (FULL-TYPED разворот
// ADR-054 §Pattern (б)): вся бизнес-логика без http.ResponseWriter/*http.Request.
// claims и req приходят аргументами (декод/auth — на вызывающем слое); ошибки
// возвращаются как *problemError (problem.Write → return), успех — cadenceCreateReply.
//
// Шаги (parity прежнего Create(w,r)): RBAC-by-kind + per-target scope → buildCadence
// → floor-лимит → next_run_at → notify[] → persist (tx + notify + invalidation) →
// audit-emit (self-audit ВНУТРИ функции, до возврата reply — huma-обёртка его не
// задевает, §Audit). ctx — request-context (persist/scope-check читают его; audit
// пишется на background-ctx внутри emitWrite, как и прежде).
func (h *CadenceHandler) CreateTyped(ctx context.Context, claims *jwt.Claims, req CadenceCreateRequest) (CadenceCreateReply, error) {
	var zero CadenceCreateReply
	if h.store == nil || h.enforcer == nil {
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "cadence registry is not configured")}
	}
	if req.Target == nil {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", "field 'target' is required")}
	}

	// Строковые batch-поля (ADR-043 amendment): `batch`/`max_failures` транслируем в
	// batch_size|batch_percent / fail_threshold|fail_threshold_percent ДО RBAC и
	// buildCadence. Конфликт строкового формата с числовыми колонками и malformed → 422.
	if bErr := applyCadenceBatchSpec(&req); bErr != "" {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", bErr)}
	}
	if mfErr := applyCadenceMaxFailures(&req); mfErr != "" {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", mfErr)}
	}

	// Двухуровневый guard (ADR-046 §7): Voyage-permission по kind рецепта.
	if err := h.checkKindPermissionErr(claims.Subject, req.Kind); err != nil {
		return zero, err
	}
	// Per-target coven-scope-check (ADR-046 §7, fail-closed).
	if err := h.checkTargetScopeErr(ctx, claims.Subject, req.Kind, req.Target); err != nil {
		return zero, err
	}

	c := h.buildCadence(&req, audit.NewULID(), claims.Subject)

	// Floor-лимит периода interval-Cadence (ADR-046 Pass B).
	if err := cadence.ValidateIntervalFloor(c, h.pollFloorSeconds); err != nil {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", err.Error())}
	}

	// next_run_at — чистая функция от расписания (ADR-046 §4).
	if next, err := cadence.NextRun(c, time.Now().UTC()); err == nil {
		c.NextRunAt = &next
	}

	// notify[] → постоянные Tiding-шаблоны (ADR-052 §m): валидация + herald.read-guard
	// ДО открытия tx.
	notifyTidings, err := h.prepareNotifyErr(ctx, claims, &req, c)
	if err != nil {
		return zero, err
	}

	if err := h.persistErr(ctx, c, notifyTidings); err != nil {
		return zero, err
	}

	h.emitWrite(claims.Subject, middleware.ScenarioInvocationSource(ctx), audit.EventCadenceCreated, c)

	location := "/v1/cadences/" + c.ID
	return cadenceCreateReply{
		CadenceID: c.ID,
		Name:      c.Name,
		Enabled:   c.Enabled,
		NextRunAt: c.NextRunAt,
		Location:  location,
	}, nil
}

// Create — POST /v1/cadences (ADR-046 §6/§7).
//
// Контракт:
//   - 201 + {cadence_id, name, enabled, next_run_at, location}.
//   - 400 — невалидный JSON.
//   - 403 — двухуровневый RBAC deny: нет Voyage-permission по kind рецепта
//     (scenario без incarnation.run / command без errand.run).
//   - 422 — невалидный рецепт/расписание (XOR interval/cron, enum overlap/kind/
//     batch_mode/on_failure, kind↔scenario_name/module, битый cron, sane-bounds) —
//     прогоняется через [cadence.validate] (Insert).
//   - 500 — store/enforcer не сконфигурирован / БД-сбой.
//
// next_run_at вычисляется при создании ([cadence.NextRun] от now). created_by_aid
// = JWT.sub. audit cadence.created.
func (h *CadenceHandler) Create(w http.ResponseWriter, r *http.Request) {
	claims, ok := middleware.ClaimsFromContext(r.Context())
	if !ok {
		problem.Write(w, problem.New(problem.TypeInternalError, r.URL.Path, "missing claims"))
		return
	}

	var req cadenceCreateRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		problem.Write(w, problem.New(problem.TypeMalformedRequest, r.URL.Path, "invalid JSON body: "+err.Error()))
		return
	}

	reply, err := h.CreateTyped(r.Context(), claims, req)
	if err != nil {
		writeProblemError(w, r, err)
		return
	}

	middleware.SetAuditPayload(r, middleware.AuditPayload{
		"cadence_id": reply.CadenceID,
		"name":       reply.Name,
		"kind":       req.Kind,
	})
	w.Header().Set("Location", reply.Location)
	writeJSON(w, http.StatusCreated, reply, h.logger)
}

// writeProblemError доставляет ошибку извлечённой доменной функции через
// problem.Write (тонкая (w,r)-оболочка, FULL-TYPED ADR-054 §Pattern). Доменная
// *problemError → его Details с проставленным instance=r.URL.Path (доменная
// функция оставляет instance пустым — она не знает путь). Не-problem ошибка
// (нештатный путь) → 500 internal.
func writeProblemError(w http.ResponseWriter, r *http.Request, err error) {
	if d, ok := asProblemError(err); ok {
		d.Instance = r.URL.Path
		problem.Write(w, d)
		return
	}
	problem.Write(w, problem.New(problem.TypeInternalError, r.URL.Path, "internal error"))
}

// prepareNotifyErr валидирует и авторизует блок notify[] формы Cadence (ADR-052 §m),
// собирая шаблоны ПОСТОЯННЫХ Tiding-правил ДО открытия транзакции (FULL-TYPED
// ADR-054 §Pattern; при отказе → *problemError, cadence НЕ создаётся). Cap/store-
// проверки — явные *problemError; форму/валидацию/RBAC notify[] делегирует общему
// извлечённому ядру [prepareNotifyTidingsErr] (единый источник с Voyage-ephemeral
// путём). Отличие от Voyage: ephemeral=false, привязка СРАЗУ по ULID расписания
// (c.ID — селектор Cadence + origin-маркер created_from_cadence_id) и
// детерминированное имя <name>-notify[-N]. kind берётся из рецепта Cadence
// (scenario/command — те же значения, что voyage.Kind).
func (h *CadenceHandler) prepareNotifyErr(
	ctx context.Context, claims *jwt.Claims, req *cadenceCreateRequest, c *cadence.Cadence,
) ([]herald.Tiding, error) {
	if len(req.Notify) == 0 {
		return nil, nil
	}
	// Cap на длину notify[] (ADR-052 §m): имя постоянного правила —
	// <prefix>-notify-<N> (permanentNotifyName), prefix усечён cappedNotifyPrefix
	// с запасом под -NNN (3 цифры). Без явного cap массив ≥1000 дал бы суффикс
	// -1000 (4 цифры) → имя > 63 символов NamePattern → validateTiding отвергает
	// внутри tx → мутный rollback-500. Явный cap — чистый 422 ДО открытия tx.
	if len(req.Notify) > maxNotifyChannels {
		return nil, &problemError{problem.New(problem.TypeValidationFailed, "",
			fmt.Sprintf("field 'notify' exceeds %d channels", maxNotifyChannels))}
	}
	if h.store == nil {
		return nil, &problemError{problem.New(problem.TypeInternalError, "",
			"cadence registry is not configured")}
	}
	tidings, perr := prepareNotifyTidingsErr(prepareNotifyDeps{
		store:    h.store,
		enforcer: h.enforcer,
		logName:  "cadence.notify",
		logger:   h.logger,
	}, ctx, claims, req.Notify, voyage.Kind(c.Kind), notifyTidingShape{
		ephemeral:  false,
		cadenceID:  c.ID,
		namePrefix: cappedNotifyPrefix(c.Name),
	})
	if perr != nil {
		return nil, &problemError{*perr}
	}
	return tidings, nil
}

// persistErr пишет Cadence + постоянные Tiding-и (блок notify) в ОДНОЙ PG-tx
// (ADR-052 §m, тот же атомарный паттерн, что voyage.persist; FULL-TYPED ADR-054
// §Pattern — ошибки через *problemError). Cadence сначала (FK
// tidings.created_from_cadence_id → cadences(id)), затем правила. Любой сбой
// (включая FK/коллизию имени Tiding) откатывает весь Create — нет ни Cadence, ни
// правил (атомарность by construction). После commit с notify — двухуровневая
// инвалидация TTL-снимка dispatcher-а (in-process + cross-keeper): постоянные
// правила вставлены прямым InsertTiding в обход herald.Service-CRUD, и без сброса
// быстрый/cross-keeper спавн диспетчит терминал против устаревшего снимка
// (DefaultRuleCacheTTL=15s) → уведомление молча промахивается.
//
// notify=пусто → одна tx с единственным Insert Cadence (поведение, эквивалентное
// прежнему прямому cadence.Insert по пулу — та же 422/404/500-классификация через
// writeWriteErrorPtr). ctx — request-context (rollback на background-ctx).
func (h *CadenceHandler) persistErr(ctx context.Context, c *cadence.Cadence, notifyTidings []herald.Tiding) error {
	tx, err := h.store.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		h.logger.Error("cadence.create: begin tx failed",
			slog.String("cadence_id", c.ID), slog.Any("error", err))
		return &problemError{problem.New(problem.TypeInternalError, "", "cadence create failed")}
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(context.Background())
		}
	}()

	// Insert Cadence сначала (FK tidings.created_from_cadence_id → cadences). Его
	// validate-ошибки (422) и PG-сбои (500) классифицирует writeWriteErrorPtr — та
	// же семантика, что прежний прямой Insert по пулу.
	if err := cadence.Insert(ctx, tx, c); err != nil {
		return &problemError{h.writeWriteErrorPtr("create", c.ID, err)}
	}
	// Постоянные Tiding-и notify-блока — в ТОЙ ЖЕ tx. Сбой (FK/коллизия имени/
	// валидация) откатывает весь Create. herald.InsertTiding принимает ту же
	// pgx.Tx (herald.ExecQueryRower ⊂ интерфейса tx).
	for i := range notifyTidings {
		if err := herald.InsertTiding(ctx, tx, &notifyTidings[i]); err != nil {
			h.logger.Error("cadence.create: insert notify tiding failed",
				slog.String("cadence_id", c.ID),
				slog.String("herald", notifyTidings[i].Herald),
				slog.Any("error", err))
			return &problemError{problem.New(problem.TypeInternalError, "", "cadence create notify failed")}
		}
	}
	if err := tx.Commit(ctx); err != nil {
		h.logger.Error("cadence.create: commit failed",
			slog.String("cadence_id", c.ID), slog.Any("error", err))
		return &problemError{problem.New(problem.TypeInternalError, "", "cadence create failed")}
	}
	committed = true
	// Постоянные правила вставлены прямым InsertTiding в обход herald.Service-CRUD
	// — сбрасываем TTL-снимок dispatcher-а ЯВНО, строго после commit (двухуровнево:
	// in-process + cross-keeper, спавн Cadence идёт на любом keeper-е). Без notify
	// правил не создано — инвалидировать нечего. nil-safe: dev без herald → no-op.
	if len(notifyTidings) > 0 && h.tidingInvalidator != nil {
		h.tidingInvalidator.InvalidateTidings(ctx, c.ID)
	}
	return nil
}

// maxNotifyChannels — потолок числа каналов notify[] на одно расписание (ADR-052
// §m). Cap нужен, чтобы суффикс имени постоянного правила (-<N>, см.
// permanentNotifyName) не раздувался: при 64 каналах максимум — 2 цифры (-64),
// под которые cappedNotifyPrefix держит запас. Превышение → 422 ДО открытия tx
// (без cap имя ≥1000-го правила вышло бы за NamePattern и упало мутным
// rollback-500 внутри транзакции).
const maxNotifyChannels = 64

// cappedNotifyPrefix приводит человекочитаемое имя расписания к безопасному
// префиксу имени Tiding-правила (NamePattern ^[a-z0-9-]{1,63}$): lowercase,
// недопустимые символы → `-`, схлопывание повторных `-`, trim краёв. Усекается с
// запасом под суффикс `-notify` (7) + `-<N>` (≤3, индекс < maxNotifyChannels),
// чтобы permanentNotifyName уложился в 63 символа. Пустой/деградировавший в ничто
// результат → "cadence" (детерминированный фолбэк; коллизию имён всё равно
// разрулит суффикс -<N>, а UNIQUE-PK при гонке — rollback всей tx).
func cappedNotifyPrefix(name string) string {
	const maxPrefix = 52 // 63 - len("-notify") - len("-NNN")
	var b strings.Builder
	prevDash := false
	for _, ru := range strings.ToLower(name) {
		switch {
		case (ru >= 'a' && ru <= 'z') || (ru >= '0' && ru <= '9'):
			b.WriteRune(ru)
			prevDash = false
		default:
			if !prevDash {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > maxPrefix {
		out = strings.Trim(out[:maxPrefix], "-")
	}
	if out == "" {
		return "cadence"
	}
	return out
}

// buildCadence собирает *cadence.Cadence из request-а (без валидации — её делает
// cadence.Insert/Update). target сериализуется в jsonb as-is; input — отдельным
// json.Marshal; ms-интервалы → time.Duration. enabled по умолчанию true
// (расписание без планировщика бессмысленно, ADR-046 §4 default-ON). createdByAID
// фиксируется на created_by_aid.
func (h *CadenceHandler) buildCadence(req *cadenceCreateRequest, id, createdByAID string) *cadence.Cadence {
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	targetJSON, _ := json.Marshal(req.Target)
	var inputJSON []byte
	if req.Input != nil {
		inputJSON, _ = json.Marshal(req.Input)
	}

	c := &cadence.Cadence{
		ID:                   id,
		Name:                 req.Name,
		Enabled:              enabled,
		ScheduleKind:         cadence.ScheduleKind(req.ScheduleKind),
		IntervalSeconds:      req.IntervalSeconds,
		OverlapPolicy:        cadence.OverlapPolicy(req.OverlapPolicy),
		Kind:                 cadence.Kind(req.Kind),
		Target:               targetJSON,
		Input:                inputJSON,
		BatchSize:            req.BatchSize,
		BatchPercent:         req.BatchPercent,
		Concurrency:          req.Concurrency,
		FailThreshold:        req.FailThreshold,
		FailThresholdPercent: req.failThresholdPercent,
		RequireAlive:         req.RequireAlive,
		CreatedByAID:         createdByAID,
	}
	if req.CronExpr != "" {
		c.CronExpr = &req.CronExpr
	}
	if req.ScenarioName != "" {
		c.ScenarioName = &req.ScenarioName
	}
	if req.Module != "" {
		c.Module = &req.Module
	}
	if req.BatchMode != "" {
		bm := cadence.BatchMode(req.BatchMode)
		c.BatchMode = &bm
	}
	if req.OnFailure != "" {
		of := cadence.OnFailure(req.OnFailure)
		c.OnFailure = &of
	}
	if req.InterBatchIntervalMS != nil && *req.InterBatchIntervalMS > 0 {
		d := time.Duration(*req.InterBatchIntervalMS) * time.Millisecond
		c.InterBatchInterval = &d
	}
	if req.InterUnitIntervalMS != nil && *req.InterUnitIntervalMS > 0 {
		d := time.Duration(*req.InterUnitIntervalMS) * time.Millisecond
		c.InterUnitInterval = &d
	}
	return c
}

// applyCadenceBatchSpec транслирует строковое поле `batch` рецепта Cadence в
// req.BatchSize / req.BatchPercent (parity handlers.applyBatchSpec для Voyage,
// ADR-043 amendment). Грамматика — fail-closed [voyage.ParseBatchSpec] (`N`|`N%`).
// "N%" → BatchPercent (резолвится на spawn-scope в cadence.BuildVoyage); "N" →
// BatchSize. Возвращает detail 422-ошибки либо "" (ok / поле не задано).
//
// Семантика:
//   - req.Batch == nil → no-op (старый путь batch_size/batch_percent).
//   - trim == "" → «не задано» (no-op, не 422).
//   - непустой + batch_size|batch_percent заданы → конфликт (тот же error-code
//     voyage_batch_spec_conflict, что у Voyage).
//   - malformed → человекочитаемый detail с показом исходной строки.
func applyCadenceBatchSpec(req *cadenceCreateRequest) (detail string) {
	if req.Batch == nil {
		return ""
	}
	mode, value, err := voyage.ParseBatchSpec(*req.Batch)
	if errors.Is(err, voyage.ErrBatchSpecEmpty) {
		return ""
	}
	if req.BatchSize != nil || req.BatchPercent != nil {
		return "voyage_batch_spec_conflict: field 'batch' is mutually exclusive with 'batch_size'/'batch_percent' (set one format)"
	}
	if err != nil {
		return fmt.Sprintf("field 'batch' must be N or N%% (1-100); got %q", *req.Batch)
	}
	switch mode {
	case voyage.BatchSpecPercent:
		req.BatchPercent = &value
	default: // BatchSpecHosts
		req.BatchSize = &value
	}
	return ""
}

// applyCadenceMaxFailures транслирует строковое поле `max_failures` рецепта Cadence
// в req.FailThreshold (абсолют) / req.failThresholdPercent (percent), parity
// handlers.applyMaxFailures (ADR-043 amendment 2026-06-09). Ключевое отличие от
// Voyage: percent НЕ резолвится в абсолют на create — scope Cadence неизвестен,
// процент стешится в req.failThresholdPercent → колонку fail_threshold_percent и
// резолвится на spawn-scope в cadence.BuildVoyage (effectiveFailThreshold).
//
// Семантика:
//   - req.MaxFailures == nil → no-op (старый путь fail_threshold).
//   - trim == "" → «не задано» (no-op, не 422).
//   - непустой + fail_threshold задан → конфликт (тот же error-code
//     voyage_batch_spec_conflict, что у Voyage/batch).
//   - "N" → абсолют: req.FailThreshold = N.
//   - "N%" → percent: req.failThresholdPercent = N (колонка fail_threshold_percent).
//   - malformed → человекочитаемый detail с показом исходной строки.
func applyCadenceMaxFailures(req *cadenceCreateRequest) (detail string) {
	if req.MaxFailures == nil {
		return ""
	}
	mode, value, err := voyage.ParseBatchSpec(*req.MaxFailures)
	if errors.Is(err, voyage.ErrBatchSpecEmpty) {
		return ""
	}
	if req.FailThreshold != nil {
		return "voyage_batch_spec_conflict: field 'max_failures' is mutually exclusive with 'fail_threshold' (set one format)"
	}
	if err != nil {
		return fmt.Sprintf("field 'max_failures' must be N or N%% (1-100); got %q", *req.MaxFailures)
	}
	switch mode {
	case voyage.BatchSpecPercent:
		req.failThresholdPercent = &value
	default: // BatchSpecHosts → абсолютное число провалов
		req.FailThreshold = &value
	}
	return ""
}

// applyCadencePatchBatchSpec — PATCH-вариант [applyCadenceBatchSpec] над
// cadencePatchRequest (та же грамматика/конфликт-семантика, ADR-043 amendment).
func applyCadencePatchBatchSpec(req *cadencePatchRequest) (detail string) {
	if req.Batch == nil {
		return ""
	}
	mode, value, err := voyage.ParseBatchSpec(*req.Batch)
	if errors.Is(err, voyage.ErrBatchSpecEmpty) {
		return ""
	}
	if req.BatchSize != nil || req.BatchPercent != nil {
		return "voyage_batch_spec_conflict: field 'batch' is mutually exclusive with 'batch_size'/'batch_percent' (set one format)"
	}
	if err != nil {
		return fmt.Sprintf("field 'batch' must be N or N%% (1-100); got %q", *req.Batch)
	}
	switch mode {
	case voyage.BatchSpecPercent:
		req.BatchPercent = &value
	default:
		req.BatchSize = &value
	}
	return ""
}

// applyCadencePatchMaxFailures — PATCH-вариант [applyCadenceMaxFailures] над
// cadencePatchRequest. percent → req.failThresholdPercent (колонка
// fail_threshold_percent, резолв на spawn-scope), абсолют → req.FailThreshold.
func applyCadencePatchMaxFailures(req *cadencePatchRequest) (detail string) {
	if req.MaxFailures == nil {
		return ""
	}
	mode, value, err := voyage.ParseBatchSpec(*req.MaxFailures)
	if errors.Is(err, voyage.ErrBatchSpecEmpty) {
		return ""
	}
	if req.FailThreshold != nil {
		return "voyage_batch_spec_conflict: field 'max_failures' is mutually exclusive with 'fail_threshold' (set one format)"
	}
	if err != nil {
		return fmt.Sprintf("field 'max_failures' must be N or N%% (1-100); got %q", *req.MaxFailures)
	}
	switch mode {
	case voyage.BatchSpecPercent:
		req.failThresholdPercent = &value
	default:
		req.FailThreshold = &value
	}
	return ""
}

// checkKindPermissionErr — error-возвращающий guard Voyage-permission по kind
// (FULL-TYPED ADR-054 §Pattern). nil → разрешено. Неизвестный kind → 422,
// revoked → 401, no-perm → 403 (как (w,r)-вариант).
func (h *CadenceHandler) checkKindPermissionErr(aid, kind string) error {
	resource, action := "", ""
	switch cadence.Kind(kind) {
	case cadence.KindScenario:
		resource, action = "incarnation", "run"
	case cadence.KindCommand:
		resource, action = "errand", "run"
	default:
		return &problemError{problem.New(problem.TypeValidationFailed, "",
			"field 'kind' must be one of {scenario, command}")}
	}
	if err := h.enforcer.Check(aid, resource, action, nil); err != nil {
		if errors.Is(err, rbac.ErrOperatorRevoked) {
			return &problemError{problem.New(problem.TypeOperatorRevokedToken, "", "archon "+aid+" has been revoked")}
		}
		return &problemError{problem.New(problem.TypeForbidden, "",
			"cadence recipe requires Voyage-permission "+resource+"."+action+" by kind="+kind)}
	}
	return nil
}

// checkTargetScopeErr — error-возвращающий per-target coven-scope-check рецепта
// Cadence (ADR-046 §7, fail-closed; FULL-TYPED ADR-054 §Pattern). nil → разрешено.
// Полная parity [VoyageHandler.resolveScenarioScope] (scope-loop): для kind=scenario
// резолвит declared target → имена инкарнаций → проверяет, что создатель имеет
// incarnation.run на КАЖДОЙ резолвнутой инкарнации (её covens ∪ {name}). Иначе
// scoped-Архонт «run on coven=A» создал бы Cadence на coven=B (вне scope) → фоновый
// спавн исполнил бы вне scope = privilege escalation. Вызывается ПОСЛЕ
// [checkKindPermissionErr] (bare-check уже пройден).
//
//   - kind=command — bare-check достаточно (parity Voyage errand.run NoSelector в
//     MVP: per-host selectors отложены пост-MVP); scope здесь не уточняется.
//   - incReader=nil → fail-closed: scenario с непустым target отвергается (как
//     Voyage без incReader пропускает per-incarnation scope, но Voyage там уже
//     гарантировал bare-право; для Cadence та же логика — bare-check выше прошёл,
//     scoped-роли без БД-scope deny). scenarioResolver=nil → 500.
//
// target — declarative-форма рецепта (тот же [voyageTargetRequest], что у Voyage).
// ctx — request-context (резолв/scope-select читают его).
func (h *CadenceHandler) checkTargetScopeErr(ctx context.Context, aid, kind string, target *voyageTargetRequest) error {
	if cadence.Kind(kind) != cadence.KindScenario {
		return nil
	}
	if target == nil {
		// kind=scenario без target — поймает cadence.validate (422 на Insert);
		// здесь нечего скоупить.
		return nil
	}

	// Резолв declared target → имена инкарнаций (parity createScenario:
	// incarnations[] ∪ service/coven-фильтр). covenFilter — первая непустая метка
	// (резолвер принимает одну, как Voyage).
	var covenFilter string
	for _, c := range target.Coven {
		if !incarnationCovenLabelValid(c) {
			return &problemError{problem.New(problem.TypeValidationFailed, "",
				"target.coven: label "+c+" must match "+soul.CovenPattern)}
		}
		if covenFilter == "" {
			covenFilter = c
		}
	}
	for _, name := range target.Incarnations {
		if !incarnation.ValidName(name) {
			return &problemError{problem.New(problem.TypeValidationFailed, "",
				"target.incarnations: name "+name+" must match "+incarnation.NamePattern)}
		}
	}

	if h.scenarioResolver == nil {
		return &problemError{problem.New(problem.TypeInternalError, "",
			"cadence registry is not configured")}
	}
	resolved, err := h.scenarioResolver.ResolveIncarnations(ctx, VoyageScenarioFilter{
		Incarnations: target.Incarnations,
		Service:      target.Service,
		Coven:        covenFilter,
	})
	if err != nil {
		if errors.Is(err, incarnation.ErrIncarnationNotFound) {
			return &problemError{problem.New(problem.TypeNotFound, "", err.Error())}
		}
		h.logger.Error("cadence.scope: scenario target resolve failed", slog.Any("error", err))
		return &problemError{problem.New(problem.TypeInternalError, "",
			"resolve cadence scenario target failed")}
	}
	// Пустой резолв declared target (coven/service ни во что не попал) — 422 до
	// создания (parity VoyageHandler voyage_empty_target, voyage.go: тот же
	// TypeValidationFailed). Эскалации нет (фоновый спавн пустой scope отсёк бы),
	// но честный отказ на CREATE/PATCH вместо молчаливого 201 на «мёртвый» рецепт.
	// command-kind сюда не доходит (ранний return для не-scenario выше).
	if len(resolved) == 0 {
		return &problemError{problem.New(problem.TypeValidationFailed, "",
			"cadence_empty_target: resolved target is empty")}
	}

	// Per-incarnation scope-check (fail-closed, parity createScenario): оператор
	// обязан иметь incarnation.run на КАЖДОЙ резолвнутой инкарнации. incReader=nil
	// (unit-тест без БД-scope) → scoped-роли deny (пустой scope), но bare/`*` уже
	// прошёл checkKindPermission — пропускаем per-incarnation проверку (parity
	// Voyage incReader=nil).
	if h.incReader == nil {
		return nil
	}
	for _, name := range resolved {
		inc, sErr := incarnation.SelectByName(ctx, h.incReader, name)
		if sErr != nil {
			h.logger.Error("cadence.scope: scope-check select failed",
				slog.String("incarnation", name), slog.Any("error", sErr))
			return &problemError{problem.New(problem.TypeInternalError, "",
				"cadence scope check failed")}
		}
		contexts := incarnationCovenContexts(inc.Name, inc.Service, inc.Covens)
		if !h.allowedAnyContext(aid, "incarnation", "run", contexts) {
			return &problemError{problem.New(problem.TypeForbidden, "",
				"cadence recipe target outside operator scope: incarnation.run on resolved incarnation "+name)}
		}
	}
	return nil
}

// allowedAnyContext OR-проверяет permission по набору контекстов (parity
// [VoyageHandler.allowedAnyContext] / [middleware.RequirePermissionMulti]).
// Пустой набор → одна попытка с nil-context (bare/`*`).
func (h *CadenceHandler) allowedAnyContext(aid, resource, action string, contexts []map[string]string) bool {
	if len(contexts) == 0 {
		return h.enforcer.Check(aid, resource, action, nil) == nil
	}
	for _, ctx := range contexts {
		if h.enforcer.Check(aid, resource, action, ctx) == nil {
			return true
		}
	}
	return false
}

// --- GET /v1/cadences ---

// cadenceDTO — response-форма для GET/list. target отдаётся as-is (declarative,
// малый объём — в отличие от Voyage target_resolved). input НЕ кладётся (инвариант
// A ADR-027: параметры прогона не светим в read-API).
type cadenceDTO struct {
	CadenceID            string          `json:"cadence_id"`
	Name                 string          `json:"name"`
	Enabled              bool            `json:"enabled"`
	ScheduleKind         string          `json:"schedule_kind"`
	IntervalSeconds      *int            `json:"interval_seconds,omitempty"`
	CronExpr             string          `json:"cron_expr,omitempty"`
	OverlapPolicy        string          `json:"overlap_policy"`
	Kind                 string          `json:"kind"`
	ScenarioName         string          `json:"scenario_name,omitempty"`
	Module               string          `json:"module,omitempty"`
	Target               json.RawMessage `json:"target,omitempty"`
	BatchSize            *int            `json:"batch_size,omitempty"`
	BatchPercent         *int            `json:"batch_percent,omitempty"`
	Concurrency          *int            `json:"concurrency,omitempty"`
	BatchMode            string          `json:"batch_mode,omitempty"`
	FailThreshold        *int            `json:"fail_threshold,omitempty"`
	FailThresholdPercent *int            `json:"fail_threshold_percent,omitempty"`
	RequireAlive         *bool           `json:"require_alive,omitempty"`
	OnFailure            string          `json:"on_failure,omitempty"`
	NextRunAt            *time.Time      `json:"next_run_at,omitempty"`
	LastRunAt            *time.Time      `json:"last_run_at,omitempty"`
	CreatedByAID         string          `json:"created_by_aid"`
	CreatedAt            time.Time       `json:"created_at"`
	UpdatedAt            time.Time       `json:"updated_at"`
}

func toCadenceDTO(c *cadence.Cadence) cadenceDTO {
	dto := cadenceDTO{
		CadenceID:            c.ID,
		Name:                 c.Name,
		Enabled:              c.Enabled,
		ScheduleKind:         string(c.ScheduleKind),
		IntervalSeconds:      c.IntervalSeconds,
		OverlapPolicy:        string(c.OverlapPolicy),
		Kind:                 string(c.Kind),
		Target:               json.RawMessage(c.Target),
		BatchSize:            c.BatchSize,
		BatchPercent:         c.BatchPercent,
		Concurrency:          c.Concurrency,
		FailThreshold:        c.FailThreshold,
		FailThresholdPercent: c.FailThresholdPercent,
		RequireAlive:         c.RequireAlive,
		NextRunAt:            c.NextRunAt,
		LastRunAt:            c.LastRunAt,
		CreatedByAID:         c.CreatedByAID,
		CreatedAt:            c.CreatedAt,
		UpdatedAt:            c.UpdatedAt,
	}
	if c.CronExpr != nil {
		dto.CronExpr = *c.CronExpr
	}
	if c.ScenarioName != nil {
		dto.ScenarioName = *c.ScenarioName
	}
	if c.Module != nil {
		dto.Module = *c.Module
	}
	if c.BatchMode != nil {
		dto.BatchMode = string(*c.BatchMode)
	}
	if c.OnFailure != nil {
		dto.OnFailure = string(*c.OnFailure)
	}
	return dto
}

// ListTyped — извлечённая доменная функция GET /v1/cadences (READ, БЕЗ audit;
// FULL-TYPED ADR-054 §Pattern четвёртый tier). Зеркало (w,r)-List: фильтры enabled
// (true → только enabled; false → без фильтра; иное → 422) + kind (exact, иное → 422);
// offset/limit диапазон enforce-ит CheckPageBounds → 400 (parity legacy ParsePage).
// Ошибки — *problemError: 422 bad enabled/kind / 400 out-of-range pagination / 500
// БД-сбой / не сконфигурирован. Успех — [CadenceListReply] (та же wire-форма
// items/offset/limit/total, что legacy → byte-exact).
func (h *CadenceHandler) ListTyped(ctx context.Context, enabled, kind string, offset, limit int) (CadenceListReply, error) {
	var zero CadenceListReply
	if h.store == nil {
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "cadence registry is not configured")}
	}
	if err := sharedapi.CheckPageBounds(offset, limit); err != nil {
		return zero, &problemError{problem.New(problem.TypeMalformedRequest, "", err.Error())}
	}

	var filter cadence.ListFilter
	switch enabled {
	case "":
		// фильтр по enabled не применять.
	case "true":
		filter.EnabledOnly = true
	case "false":
		// false → без фильтра по enabled (показать все); явный contract.
	default:
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
			"query 'enabled' must be 'true' or 'false'")}
	}
	if kind != "" {
		k := cadence.Kind(kind)
		if !cadence.ValidKind(k) {
			return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
				"invalid 'kind' filter (must be one of scenario/command)")}
		}
		filter.Kind = k
	}

	items, total, err := cadence.List(ctx, h.store, filter, offset, limit)
	if err != nil {
		h.logger.Error("cadence.list: select failed", slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "list cadences failed")}
	}
	dtos := make([]cadenceDTO, 0, len(items))
	for _, c := range items {
		dtos = append(dtos, toCadenceDTO(c))
	}
	return CadenceListReply{Items: dtos, Offset: offset, Limit: limit, Total: total}, nil
}

// List — GET /v1/cadences (ADR-046 §6). Тонкая (w,r)-оболочка над [ListTyped]
// (FULL-TYPED ADR-054): парсит offset/limit через sharedapi.ParsePage (тот же 400-
// контракт, что CheckPageBounds в ListTyped) и делегирует. Сохранена для прочих
// (w,r)-вызовов; huma-роут зовёт ListTyped напрямую.
func (h *CadenceHandler) List(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	page, err := sharedapi.ParsePage(q)
	if err != nil {
		problem.Write(w, problem.New(problem.TypeMalformedRequest, r.URL.Path, err.Error()))
		return
	}
	reply, err := h.ListTyped(r.Context(), q.Get("enabled"), q.Get("kind"), page.Offset, page.Limit)
	if err != nil {
		if d, ok := AsProblemDetails(err); ok {
			d.Instance = r.URL.Path
			problem.Write(w, d)
			return
		}
		problem.Write(w, problem.New(problem.TypeInternalError, r.URL.Path, "list cadences failed"))
		return
	}
	writeJSON(w, http.StatusOK, reply, h.logger)
}

// Get — GET /v1/cadences/{id} (деталь).
func (h *CadenceHandler) Get(w http.ResponseWriter, r *http.Request) {
	if h.store == nil {
		problem.Write(w, problem.New(problem.TypeInternalError, r.URL.Path, "cadence registry is not configured"))
		return
	}
	id := chi.URLParam(r, "id")
	if !audit.IsValidULID(id) {
		problem.Write(w, problem.New(problem.TypeValidationFailed, r.URL.Path,
			"path 'id' must be a Crockford-base32 ULID (26 chars)"))
		return
	}
	c, err := cadence.Get(r.Context(), h.store, id)
	if err != nil {
		h.writeReadError(w, r, "get", id, err)
		return
	}
	writeJSON(w, http.StatusOK, toCadenceDTO(c), h.logger)
}

// GetTyped — извлечённая доменная функция GET /v1/cadences/{id} (READ, БЕЗ audit;
// FULL-TYPED ADR-054 §Pattern). Зеркало (w,r)-Get: 422 bad id, 404 not-found, 500
// БД-сбой/не сконфигурирован. Успех — [CadenceDTO] (та же wire-форма, что legacy
// toCadenceDTO → byte-exact с GET {id} на strict).
func (h *CadenceHandler) GetTyped(ctx context.Context, id string) (CadenceDTO, error) {
	var zero CadenceDTO
	if h.store == nil {
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "cadence registry is not configured")}
	}
	if !audit.IsValidULID(id) {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
			"path 'id' must be a Crockford-base32 ULID (26 chars)")}
	}
	c, err := cadence.Get(ctx, h.store, id)
	if err != nil {
		return zero, &problemError{h.readErrPtr("get", id, err)}
	}
	return toCadenceDTO(c), nil
}

// --- PATCH /v1/cadences/{id} ---

// cadencePatchRequest — PATCH body. Все поля опциональны: заданное → перезапись,
// опущенное → текущее значение сохраняется (read-modify-write поверх full-replace
// cadence.Update). Указатели на nullable-поля рецепта семантически
// «не различают» «опущено» и «явный null» — для MVP PATCH трактует присутствие
// ключа как перезапись (отсутствие → keep). enabled-toggle тоже здесь (можно и
// через /enable+/disable).
type cadencePatchRequest struct {
	Name            *string              `json:"name,omitempty"`
	Enabled         *bool                `json:"enabled,omitempty"`
	ScheduleKind    *string              `json:"schedule_kind,omitempty"`
	IntervalSeconds *int                 `json:"interval_seconds,omitempty"`
	CronExpr        *string              `json:"cron_expr,omitempty"`
	OverlapPolicy   *string              `json:"overlap_policy,omitempty"`
	ScenarioName    *string              `json:"scenario_name,omitempty"`
	Module          *string              `json:"module,omitempty"`
	Input           map[string]any       `json:"input,omitempty"`
	Target          *voyageTargetRequest `json:"target,omitempty"`
	// Batch/MaxFailures — строковые batch-поля (parity create, ADR-043 amendment).
	// Транслируются applyCadencePatchBatchSpec/applyCadencePatchMaxFailures в
	// batch_size|batch_percent / fail_threshold|fail_threshold_percent перед
	// applyCadencePatch. Конфликт с числовыми колонками в том же PATCH → 422.
	Batch         *string `json:"batch,omitempty"`
	BatchSize     *int    `json:"batch_size,omitempty"`
	BatchPercent  *int    `json:"batch_percent,omitempty"`
	Concurrency   *int    `json:"concurrency,omitempty"`
	BatchMode     *string `json:"batch_mode,omitempty"`
	MaxFailures   *string `json:"max_failures,omitempty"`
	FailThreshold *int    `json:"fail_threshold,omitempty"`
	RequireAlive  *bool   `json:"require_alive,omitempty"`
	OnFailure     *string `json:"on_failure,omitempty"`

	// failThresholdPercent — стешенный процент из max_failures="N%" (parity create);
	// заполняется applyCadencePatchMaxFailures, переносится в строку applyCadencePatch.
	failThresholdPercent *int
}

// scheduleChanged сообщает, затрагивает ли PATCH расписание (требует пересчёта
// next_run_at).
func (p *cadencePatchRequest) scheduleChanged() bool {
	return p.ScheduleKind != nil || p.IntervalSeconds != nil || p.CronExpr != nil
}

// Patch — PATCH /v1/cadences/{id} (ADR-046 §6). Read-modify-write: читает текущую
// строку, накладывает заданные поля, прогоняет cadence.Update (full-replace +
// validate). Пересчёт next_run_at при смене расписания. audit cadence.updated.
//
// Контракт: 200 + cadenceDTO; 400 невалидный JSON; 404 cadence_not_found; 422
// невалидный рецепт/расписание; 500 БД-сбой. kind НЕ меняется (kind-смена = другая
// сущность рецепта; запрещаем неявно — поле не в PATCH-body; смена kind = delete +
// create). created_by_aid фиксирован (cadence.Update не пишет).
func (h *CadenceHandler) Patch(w http.ResponseWriter, r *http.Request) {
	claims, ok := middleware.ClaimsFromContext(r.Context())
	if !ok {
		problem.Write(w, problem.New(problem.TypeInternalError, r.URL.Path, "missing claims"))
		return
	}
	id := chi.URLParam(r, "id")

	var req cadencePatchRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		problem.Write(w, problem.New(problem.TypeMalformedRequest, r.URL.Path, "invalid JSON body: "+err.Error()))
		return
	}

	dto, err := h.PatchTyped(r.Context(), claims, id, req)
	if err != nil {
		writeProblemError(w, r, err)
		return
	}
	middleware.SetAuditPayload(r, middleware.AuditPayload{
		"cadence_id": dto.CadenceID,
		"name":       dto.Name,
		"kind":       dto.Kind,
	})
	writeJSON(w, http.StatusOK, dto, h.logger)
}

// PatchTyped — извлечённая доменная функция PATCH /v1/cadences/{id} (FULL-TYPED
// разворот ADR-054 §Pattern (б), батч-2f self-audit): read-modify-write поверх
// full-replace cadence.Update без http.ResponseWriter/*http.Request. claims/id/req
// приходят аргументами (декод/auth/{id}-bind — на вызывающем слое); ошибки —
// *problemError, успех — cadenceDTO. self-audit cadence.updated пишется ВНУТРИ
// функции (до возврата DTO — huma-обёртка его не задевает, §Audit).
//
// Шаги (parity прежнего Patch(w,r)): id-валидация → строковые batch-поля → Get →
// applyCadencePatch → RBAC PATCH-guard (двухуровневый, ADR-046 §7) → floor-лимит →
// next_run_at → Update → audit-emit.
func (h *CadenceHandler) PatchTyped(ctx context.Context, claims *jwt.Claims, id string, req CadencePatchRequest) (CadenceDTO, error) {
	var zero CadenceDTO
	if h.store == nil {
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "cadence registry is not configured")}
	}
	if !audit.IsValidULID(id) {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
			"path 'id' must be a Crockford-base32 ULID (26 chars)")}
	}

	// Строковые batch-поля PATCH (parity create): транслируем `batch`/`max_failures`
	// в числовые поля до Get/applyCadencePatch. Конфликт строкового формата с
	// числовой колонкой в том же PATCH и malformed → 422.
	if bErr := applyCadencePatchBatchSpec(&req); bErr != "" {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", bErr)}
	}
	if mfErr := applyCadencePatchMaxFailures(&req); mfErr != "" {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", mfErr)}
	}

	c, err := cadence.Get(ctx, h.store, id)
	if err != nil {
		return zero, &problemError{h.readErrPtr("patch", id, err)}
	}

	scheduleChanged := req.scheduleChanged()
	applyCadencePatch(c, &req)

	// RBAC PATCH-guard (ADR-046 §7, security-критичный — была вторая дыра): PATCH
	// меняет target/scenario_name, поэтому требует тот же двухуровневый guard, что
	// CREATE — иначе scoped-Архонт создал бы Cadence на разрешённом coven=A, затем
	// PATCH-ом перенаправил target на coven=B (вне scope) без проверки. kind в PATCH
	// не меняется (берётся из загруженной строки c.Kind). Сначала bare-check
	// Voyage-permission по kind, затем per-target scope нового (пост-patch) target-а.
	if err := h.checkKindPermissionErr(claims.Subject, string(c.Kind)); err != nil {
		return zero, err
	}
	if err := h.checkTargetScopeErr(ctx, claims.Subject, string(c.Kind), cadenceTargetRequest(c.Target)); err != nil {
		return zero, err
	}

	// Floor-лимит периода (ADR-046 Pass B): PATCH может перевести расписание на
	// interval или поменять interval_seconds — тот же floor-инвариант, что Create.
	// После scope-check (RBAC-deny приоритетнее, не светим валидность interval).
	if err := cadence.ValidateIntervalFloor(c, h.pollFloorSeconds); err != nil {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", err.Error())}
	}

	// Пересчёт next_run_at при смене расписания (ADR-046 §6): новое правило
	// → новый next от now. Битый cron вернёт ошибку из NextRun, но cadence.Update
	// её и так поймает (validate.ParseCron) → 422; здесь next просто не трогаем.
	if scheduleChanged {
		if next, nErr := cadence.NextRun(c, time.Now().UTC()); nErr == nil {
			c.NextRunAt = &next
		}
	}

	if err := cadence.Update(ctx, h.store, c); err != nil {
		return zero, &problemError{h.writeWriteErrorPtr("patch", id, err)}
	}

	h.emitWrite(claims.Subject, middleware.ScenarioInvocationSource(ctx), audit.EventCadenceUpdated, c)
	return toCadenceDTO(c), nil
}

// cadenceTargetRequest декодирует declarative-target Cadence (jsonb, тот же shape,
// что [voyageTargetRequest]) для scope-check-а (ADR-046 §7). Пустой/битый jsonb →
// nil (caller трактует как «нечего скоупить» — kind=scenario без валидного target
// поймает cadence.validate). Read-only декод (target в БД пишет
// buildCadence/applyCadencePatch из того же типа).
func cadenceTargetRequest(raw []byte) *voyageTargetRequest {
	if len(raw) == 0 {
		return nil
	}
	var t voyageTargetRequest
	if err := json.Unmarshal(raw, &t); err != nil {
		return nil
	}
	return &t
}

// applyCadencePatch накладывает заданные PATCH-поля на загруженную строку
// (read-modify-write). Опущенное поле → текущее значение сохраняется. Указательные
// nullable-поля рецепта: присутствие ключа → перезапись, в т.ч. на null
// (json-decoder положит nil-указатель при `"field": null` — но omitempty в DTO
// неотличим, поэтому для MVP трактуем непустой указатель как set; cron/scenario/
// module очищаются пустой строкой).
func applyCadencePatch(c *cadence.Cadence, req *cadencePatchRequest) {
	if req.Name != nil {
		c.Name = *req.Name
	}
	if req.Enabled != nil {
		c.Enabled = *req.Enabled
	}
	if req.ScheduleKind != nil {
		c.ScheduleKind = cadence.ScheduleKind(*req.ScheduleKind)
	}
	if req.IntervalSeconds != nil {
		c.IntervalSeconds = req.IntervalSeconds
	}
	if req.CronExpr != nil {
		if *req.CronExpr == "" {
			c.CronExpr = nil
		} else {
			c.CronExpr = req.CronExpr
		}
	}
	if req.OverlapPolicy != nil {
		c.OverlapPolicy = cadence.OverlapPolicy(*req.OverlapPolicy)
	}
	// При смене schedule_kind очищаем «чужое» поле расписания, иначе validate
	// отвергнет (interval не должен нести cron_expr и наоборот).
	switch c.ScheduleKind {
	case cadence.ScheduleKindInterval:
		c.CronExpr = nil
	case cadence.ScheduleKindCron:
		c.IntervalSeconds = nil
	}
	if req.ScenarioName != nil {
		if *req.ScenarioName == "" {
			c.ScenarioName = nil
		} else {
			c.ScenarioName = req.ScenarioName
		}
	}
	if req.Module != nil {
		if *req.Module == "" {
			c.Module = nil
		} else {
			c.Module = req.Module
		}
	}
	if req.Input != nil {
		inputJSON, _ := json.Marshal(req.Input)
		c.Input = inputJSON
	}
	if req.Target != nil {
		targetJSON, _ := json.Marshal(req.Target)
		c.Target = targetJSON
	}
	// batch_size / batch_percent — взаимоисключающая пара. PATCH одного формата
	// поверх хранимого встречного: обнуляем встречное, иначе оператор не сможет
	// переключить формат без явного сброса (ревью Batch S3). nil-req → keep (поле
	// не задано) — обнуление НЕ срабатывает.
	if req.BatchSize != nil {
		c.BatchSize = req.BatchSize
		c.BatchPercent = nil
	}
	if req.BatchPercent != nil {
		c.BatchPercent = req.BatchPercent
		c.BatchSize = nil
	}
	if req.Concurrency != nil {
		c.Concurrency = req.Concurrency
	}
	if req.BatchMode != nil {
		if *req.BatchMode == "" {
			c.BatchMode = nil
		} else {
			bm := cadence.BatchMode(*req.BatchMode)
			c.BatchMode = &bm
		}
	}
	// fail_threshold / fail_threshold_percent — взаимоисключающая пара (validate
	// бьёт XOR). PATCH одного формата (max_failures="N" → absolute, "N%" → percent)
	// поверх хранимого встречного: обнуляем встречное, иначе validate вернёт 422 и
	// оператор не переключит формат без явного сброса (ревью Batch S3). nil-req →
	// keep — обнуление НЕ срабатывает.
	if req.FailThreshold != nil {
		c.FailThreshold = req.FailThreshold
		c.FailThresholdPercent = nil
	}
	if req.failThresholdPercent != nil {
		c.FailThresholdPercent = req.failThresholdPercent
		c.FailThreshold = nil
	}
	if req.RequireAlive != nil {
		c.RequireAlive = req.RequireAlive
	}
	if req.OnFailure != nil {
		if *req.OnFailure == "" {
			c.OnFailure = nil
		} else {
			of := cadence.OnFailure(*req.OnFailure)
			c.OnFailure = &of
		}
	}
}

// --- POST /v1/cadences/{id}/enable | /disable ---

// Enable — POST /v1/cadences/{id}/enable (возобновление расписания).
func (h *CadenceHandler) Enable(w http.ResponseWriter, r *http.Request) {
	h.setEnabled(w, r, true)
}

// Disable — POST /v1/cadences/{id}/disable (пауза расписания).
func (h *CadenceHandler) Disable(w http.ResponseWriter, r *http.Request) {
	h.setEnabled(w, r, false)
}

// setEnabled — общая ветка enable/disable: lightweight toggle без перезаписи
// рецепта ([cadence.SetEnabled]). audit cadence.updated (изменение состояния
// расписания). 200 + {cadence_id, enabled}.
func (h *CadenceHandler) setEnabled(w http.ResponseWriter, r *http.Request, enabled bool) {
	claims, ok := middleware.ClaimsFromContext(r.Context())
	if !ok {
		problem.Write(w, problem.New(problem.TypeInternalError, r.URL.Path, "missing claims"))
		return
	}
	id := chi.URLParam(r, "id")
	reply, err := h.SetEnabledTyped(r.Context(), claims, id, enabled)
	if err != nil {
		writeProblemError(w, r, err)
		return
	}
	middleware.SetAuditPayload(r, middleware.AuditPayload{
		"cadence_id": reply.CadenceID,
		"enabled":    reply.Enabled,
	})
	writeJSON(w, http.StatusOK, reply, h.logger)
}

// SetEnabledTyped — извлечённая доменная функция enable/disable (FULL-TYPED
// ADR-054 §Pattern, батч-2f self-audit): lightweight toggle без перезаписи рецепта
// ([cadence.SetEnabled]) без http.ResponseWriter/*http.Request. self-audit
// cadence.updated (изменение состояния расписания) пишется ВНУТРИ функции.
// 200-body — сгенерированный CadenceEnabledReply.
func (h *CadenceHandler) SetEnabledTyped(ctx context.Context, claims *jwt.Claims, id string, enabled bool) (CadenceEnabledReply, error) {
	var zero CadenceEnabledReply
	if h.store == nil {
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "cadence registry is not configured")}
	}
	if !audit.IsValidULID(id) {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
			"path 'id' must be a Crockford-base32 ULID (26 chars)")}
	}
	if err := cadence.SetEnabled(ctx, h.store, id, enabled); err != nil {
		return zero, &problemError{h.writeWriteErrorPtr("set-enabled", id, err)}
	}
	h.emitEnabledToggle(claims.Subject, middleware.ScenarioInvocationSource(ctx), id, enabled)
	return cadenceEnabledReply{CadenceID: id, Enabled: enabled}, nil
}

// --- DELETE /v1/cadences/{id} ---

// Delete — DELETE /v1/cadences/{id} (ADR-046 §9). Снимает расписание; порождённые
// Voyage остаются (FK voyages.cadence_id ON DELETE SET NULL — ручные прогоны и
// история детей сохраняются). audit cadence.deleted. 204 No Content; 404
// cadence_not_found.
func (h *CadenceHandler) Delete(w http.ResponseWriter, r *http.Request) {
	claims, ok := middleware.ClaimsFromContext(r.Context())
	if !ok {
		problem.Write(w, problem.New(problem.TypeInternalError, r.URL.Path, "missing claims"))
		return
	}
	id := chi.URLParam(r, "id")
	if err := h.DeleteTyped(r.Context(), claims, id); err != nil {
		writeProblemError(w, r, err)
		return
	}
	middleware.SetAuditPayload(r, middleware.AuditPayload{"cadence_id": id})
	w.WriteHeader(http.StatusNoContent)
}

// DeleteTyped — извлечённая доменная функция DELETE /v1/cadences/{id} (FULL-TYPED
// ADR-054 §Pattern, батч-2f self-audit): снимает расписание; порождённые Voyage
// остаются (FK voyages.cadence_id ON DELETE SET NULL). self-audit cadence.deleted
// пишется ВНУТРИ функции. ctx — request-context (delete + инвалидация TTL-снимка
// dispatcher-а читают его).
func (h *CadenceHandler) DeleteTyped(ctx context.Context, claims *jwt.Claims, id string) error {
	if h.store == nil {
		return &problemError{problem.New(problem.TypeInternalError, "", "cadence registry is not configured")}
	}
	if !audit.IsValidULID(id) {
		return &problemError{problem.New(problem.TypeValidationFailed, "",
			"path 'id' must be a Crockford-base32 ULID (26 chars)")}
	}
	if err := cadence.Delete(ctx, h.store, id); err != nil {
		return &problemError{h.writeWriteErrorPtr("delete", id, err)}
	}

	// Каскад FK (tidings.created_from_cadence_id ON DELETE CASCADE, миграция 074)
	// снёс постоянные notify-правила на БД-уровне В ОБХОД herald.Service-CRUD —
	// сбрасываем TTL-снимок dispatcher-а ЯВНО, строго после удаления (parity с
	// Create-инвалидацией после notify-insert и HeraldHandler.DeleteHerald).
	// Инвалидация БЕЗУСЛОВНА: handler не знает, были ли form-rules (каскад
	// БД-side), а InvalidateRules() сбрасывает весь снимок — id лишь
	// диагностический лейбл для cross-keeper publish; delete редкий, лишний
	// сброс TTL-снимка дёшев. nil-safe: dev без herald → no-op.
	if h.tidingInvalidator != nil {
		h.tidingInvalidator.InvalidateTidings(ctx, id)
	}

	h.emitDeleted(claims.Subject, middleware.ScenarioInvocationSource(ctx), id)
	return nil
}

// --- GET /v1/cadences/{id}/runs ---

// Runs — GET /v1/cadences/{id}/runs (ADR-046 §6). Дочерние Voyage расписания
// (voyages WHERE cadence_id=$1), reuse Voyage-DTO/list. Существование Cadence
// проверяется probe-ом (404 если нет — пустой список неотличим от
// несуществующего id). Pagination + status-фильтр (parity VoyageHandler.List).
func (h *CadenceHandler) Runs(w http.ResponseWriter, r *http.Request) {
	if h.store == nil {
		problem.Write(w, problem.New(problem.TypeInternalError, r.URL.Path, "cadence registry is not configured"))
		return
	}
	id := chi.URLParam(r, "id")
	if !audit.IsValidULID(id) {
		problem.Write(w, problem.New(problem.TypeValidationFailed, r.URL.Path,
			"path 'id' must be a Crockford-base32 ULID (26 chars)"))
		return
	}
	if _, err := cadence.Get(r.Context(), h.store, id); err != nil {
		h.writeReadError(w, r, "runs", id, err)
		return
	}

	q := r.URL.Query()
	page, err := sharedapi.ParsePage(q)
	if err != nil {
		problem.Write(w, problem.New(problem.TypeMalformedRequest, r.URL.Path, err.Error()))
		return
	}
	filter := voyage.ListFilter{CadenceID: id}
	if statuses := q["status"]; len(statuses) > 0 {
		filter.Statuses = make([]voyage.Status, 0, len(statuses))
		for _, s := range statuses {
			st := voyage.Status(s)
			if !voyage.ValidStatus(st) {
				problem.Write(w, problem.New(problem.TypeValidationFailed, r.URL.Path,
					"invalid 'status' filter (scheduled/pending/running/succeeded/failed/partial_failed/cancelled)"))
				return
			}
			filter.Statuses = append(filter.Statuses, st)
		}
	}

	items, total, err := voyage.List(r.Context(), h.store, filter, page.Offset, page.Limit)
	if err != nil {
		h.logger.Error("cadence.runs: voyage list failed", slog.String("cadence_id", id), slog.Any("error", err))
		problem.Write(w, problem.New(problem.TypeInternalError, r.URL.Path, "list cadence runs failed"))
		return
	}
	dtos := make([]voyageDTO, 0, len(items))
	for _, v := range items {
		dtos = append(dtos, toVoyageDTO(v))
	}
	writeJSON(w, http.StatusOK, sharedapi.PagedResponse[voyageDTO]{
		Items:  dtos,
		Offset: page.Offset,
		Limit:  page.Limit,
		Total:  total,
	}, h.logger)
}

// CadenceRunsReply — typed-выход GET /v1/cadences/{id}/runs: paged voyageDTO той же
// wire-формы, что legacy (w,r)-Runs (items/offset/limit/total через
// sharedapi.PagedResponse → byte-exact). voyageDTO = Voyage.
type CadenceRunsReply = sharedapi.PagedResponse[voyageDTO]

// RunsTyped — извлечённая доменная функция GET /v1/cadences/{id}/runs (READ, БЕЗ
// audit; FULL-TYPED ADR-054 §Pattern). Зеркало (w,r)-Runs: проверяет существование
// расписания (404, если нет), фильтрует Voyage по cadence_id + опц. status[]. offset/
// limit диапазон enforce-ит CheckPageBounds → 400 (parity legacy ParsePage). Ошибки —
// *problemError: 422 bad id / 400 out-of-range pagination / 422 bad status / 404 нет
// расписания / 500 БД-сбой. Успех — [CadenceRunsReply].
func (h *CadenceHandler) RunsTyped(ctx context.Context, id string, statuses []string, offset, limit int) (CadenceRunsReply, error) {
	var zero CadenceRunsReply
	if h.store == nil {
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "cadence registry is not configured")}
	}
	if !audit.IsValidULID(id) {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
			"path 'id' must be a Crockford-base32 ULID (26 chars)")}
	}
	if err := sharedapi.CheckPageBounds(offset, limit); err != nil {
		return zero, &problemError{problem.New(problem.TypeMalformedRequest, "", err.Error())}
	}
	if _, err := cadence.Get(ctx, h.store, id); err != nil {
		return zero, &problemError{h.readErrPtr("runs", id, err)}
	}

	filter := voyage.ListFilter{CadenceID: id}
	if len(statuses) > 0 {
		filter.Statuses = make([]voyage.Status, 0, len(statuses))
		for _, s := range statuses {
			st := voyage.Status(s)
			if !voyage.ValidStatus(st) {
				return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
					"invalid 'status' filter (scheduled/pending/running/succeeded/failed/partial_failed/cancelled)")}
			}
			filter.Statuses = append(filter.Statuses, st)
		}
	}

	items, total, err := voyage.List(ctx, h.store, filter, offset, limit)
	if err != nil {
		h.logger.Error("cadence.runs: voyage list failed", slog.String("cadence_id", id), slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "list cadence runs failed")}
	}
	dtos := make([]voyageDTO, 0, len(items))
	for _, v := range items {
		dtos = append(dtos, toVoyageDTO(v))
	}
	return CadenceRunsReply{Items: dtos, Offset: offset, Limit: limit, Total: total}, nil
}

// --- error mapping ---

// writeReadError маппит ошибку read-операции (Get): not-found → 404, иначе 500.
func (h *CadenceHandler) writeReadError(w http.ResponseWriter, r *http.Request, op, id string, err error) {
	d := h.readErrPtr(op, id, err)
	d.Instance = r.URL.Path
	problem.Write(w, d)
}

// readErrPtr — классификатор read-ошибки (Get) → [problem.Details] (FULL-TYPED
// ADR-054 §Pattern): not-found→404, иначе 500. instance пуст (caller проставит).
// Извлечённое ядро [writeReadError] для error-возвращающих путей (PatchTyped).
func (h *CadenceHandler) readErrPtr(op, id string, err error) problem.Details {
	if errors.Is(err, cadence.ErrCadenceNotFound) {
		return problem.New(problem.TypeNotFound, "", "cadence_not_found: "+id)
	}
	h.logger.Error("cadence."+op+": select failed", slog.String("cadence_id", id), slog.Any("error", err))
	return problem.New(problem.TypeInternalError, "", "cadence "+op+" failed")
}

// writeWriteErrorPtr — классификатор write-ошибки → [problem.Details] (FULL-TYPED
// ADR-054 §Pattern): not-found→404, validate→422, PG→500. instance пуст (caller
// проставит). Извлечённое ядро [writeWriteError] для error-возвращающих путей
// (persistErr). Семантика классификации идентична (w,r)-варианту.
func (h *CadenceHandler) writeWriteErrorPtr(op, id string, err error) problem.Details {
	switch {
	case errors.Is(err, cadence.ErrCadenceNotFound):
		return problem.New(problem.TypeNotFound, "", "cadence_not_found: "+id)
	case isCadenceValidationError(err):
		return problem.New(problem.TypeValidationFailed, "", err.Error())
	default:
		h.logger.Error("cadence."+op+": write failed", slog.String("cadence_id", id), slog.Any("error", err))
		return problem.New(problem.TypeInternalError, "", "cadence "+op+" failed")
	}
}

// isCadenceValidationError отличает validate-ошибку рецепта (422) от PG-сбоя
// (500). cadence.validate возвращает голые fmt.Errorf("cadence: …") ДО SQL;
// PG-сбои оборачиваются mapWriteError в "cadence: write: …" / FK / CHECK / Exists.
// Сигнатура validate-ошибки — НЕ Exists/NotFound и не несёт PG-обёртку. Простой и
// надёжный признак: это не sentinel-ошибки CRUD и не "cadence: write:"-обёртка.
func isCadenceValidationError(err error) bool {
	if errors.Is(err, cadence.ErrCadenceExists) || errors.Is(err, cadence.ErrCadenceNotFound) {
		return false
	}
	// PG-CHECK/FK/write-ошибки прошли через mapWriteError → их текст начинается с
	// "cadence: write:" / "cadence: FK violation" / "cadence: CHECK violation".
	// validate-ошибки — любой другой "cadence: …" из validate (нет SQL-обёртки).
	msg := err.Error()
	for _, pgPrefix := range []string{"cadence: write:", "cadence: FK violation", "cadence: CHECK violation"} {
		if len(msg) >= len(pgPrefix) && msg[:len(pgPrefix)] == pgPrefix {
			return false
		}
	}
	return true
}

// --- audit emitters ---

// emitWrite пишет cadence.created / cadence.updated (source=api/mcp, archon_aid=
// JWT.sub). Background-ctx — HTTP-server мог отменить r.Context() после write-
// response. `input` рецепта НЕ кладётся (инвариант A ADR-027).
func (h *CadenceHandler) emitWrite(aid string, source audit.Source, eventType audit.EventType, c *cadence.Cadence) {
	if h.auditW == nil {
		return
	}
	payload := map[string]any{
		"cadence_id":     c.ID,
		"name":           c.Name,
		"schedule_kind":  string(c.ScheduleKind),
		"kind":           string(c.Kind),
		"overlap_policy": string(c.OverlapPolicy),
		"enabled":        c.Enabled,
	}
	if c.ScenarioName != nil {
		payload["scenario_name"] = *c.ScenarioName
	}
	if c.Module != nil {
		payload["module"] = *c.Module
	}
	h.writeAudit(&audit.Event{
		EventType:     eventType,
		Source:        source,
		ArchonAID:     aid,
		CorrelationID: c.ID,
		Payload:       payload,
	})
}

// emitEnabledToggle пишет cadence.updated для enable/disable toggle.
func (h *CadenceHandler) emitEnabledToggle(aid string, source audit.Source, id string, enabled bool) {
	if h.auditW == nil {
		return
	}
	h.writeAudit(&audit.Event{
		EventType:     audit.EventCadenceUpdated,
		Source:        source,
		ArchonAID:     aid,
		CorrelationID: id,
		Payload:       map[string]any{"cadence_id": id, "enabled": enabled},
	})
}

// emitDeleted пишет cadence.deleted.
func (h *CadenceHandler) emitDeleted(aid string, source audit.Source, id string) {
	if h.auditW == nil {
		return
	}
	h.writeAudit(&audit.Event{
		EventType:     audit.EventCadenceDeleted,
		Source:        source,
		ArchonAID:     aid,
		CorrelationID: id,
		Payload:       map[string]any{"cadence_id": id},
	})
}

func (h *CadenceHandler) writeAudit(ev *audit.Event) {
	if err := h.auditW.Write(context.Background(), ev); err != nil {
		h.logger.Error("cadence: audit write failed",
			slog.String("event_type", string(ev.EventType)),
			slog.String("cadence_id", ev.CorrelationID), slog.Any("error", err))
	}
}
