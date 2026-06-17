package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	"github.com/souls-guild/soul-stack/keeper/internal/herald"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac"
	"github.com/souls-guild/soul-stack/keeper/internal/soul"
	"github.com/souls-guild/soul-stack/keeper/internal/soulpurview"
	"github.com/souls-guild/soul-stack/keeper/internal/voyage"
	sharedapi "github.com/souls-guild/soul-stack/shared/api"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// VoyageStore — узкая поверхность CRUD-а пакета [voyage] для S5 HTTP/MCP
// handler-а:
//
//   - ExecQueryRower — read (SelectByID/List/SelectTargets) + cancel-UPDATE без
//     транзакции;
//   - BeginTx — atomic Insert + InsertTargets (snapshot-scope в одной PG-tx,
//     ADR-043: набор единиц не «дрожит» между INSERT-ами).
//
// Claim/Lease/Finalize живут в [voyageorch.VoyageWorker]. Реальный *pgxpool.Pool
// удовлетворяет; unit-тесты — fake.
type VoyageStore interface {
	voyage.ExecQueryRower
	BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error)
}

// TidingInvalidator — узкая поверхность двухуровневой инвалидации снимка
// Tiding-правил dispatcher-а (in-process InvalidateRules + cross-keeper Redis
// publish). Нужна voyage.create-пути, который вставляет ephemeral-Tiding-и из
// notify прямым herald.InsertTiding в свою voyage-tx, В ОБХОД
// herald.Service-CRUD (и его инвалидации). Без явного вызова после commit
// dispatcher держит правило за TTL-снимком (DefaultRuleCacheTTL=15s) и быстрый
// прогон (~5s) диспетчит терминал против устаревшего снимка → разовое
// уведомление молча промахивается (race, подтверждён architect-ом).
//
// Реализуется [*herald.Service] (метод InvalidateTidings) — single source of
// truth по инвалидатору/Redis-publisher. nil → no-op (dev без herald/Redis:
// деградация на TTL-сходимость, как было).
type TidingInvalidator interface {
	InvalidateTidings(ctx context.Context, name string)
}

// VoyageHandler — handler-ы endpoints Voyage (ADR-043, S5):
//
//	POST   /v1/voyages              — создать Voyage (kind=scenario|command).
//	GET    /v1/voyages              — paged list (filter kind/status).
//	GET    /v1/voyages/{id}         — snapshot (detail + summary).
//	GET    /v1/voyages/{id}/targets — All-runs drill (per-target batch/status/back-link).
//	DELETE /v1/voyages/{id}         — cancel pending/scheduled (running-cancel — post-MVP).
//
// RBAC-by-kind (ADR-043 §6, security-критичный fail-closed guard): POST
// выбирает permission ПО kind из ТЕЛА — scenario→incarnation.run,
// command→errand.run. Middleware-route это сделать не может (kind виден только
// после декода body), поэтому permission-проверка живёт ВНУТРИ Create (router
// навешивает только RequireJWT). GET/list — read-permission по соответствующему
// пространству (incarnation.history для scenario-read parity Tide; общий вход —
// см. router.go: list/detail/targets гейтятся incarnation.history, как Tide).
//
// Зависимости опциональны (паттерн TideHandler / ErrandRunHandler): router.go
// проверяет handler==nil. enforcer/store/scenarioResolver/commandResolver —
// обязательны для production-маршрутов; incReader нужен RBAC-by-kind scenario-
// gate-у. auditW может быть nil (dev без audit).
type VoyageHandler struct {
	store            VoyageStore
	scenarioResolver VoyageScenarioResolver
	commandResolver  VoyageCommandResolver
	incReader        IncarnationContextReader
	enforcer         middleware.PermissionChecker
	// scoper — read-поверхность scope-границы оператора (ADR-047 S4). Используется
	// command-путём для пересечения target ∩ Purview (errand.run): тот же резолвер,
	// что фильтрует `GET /v1/souls`. nil → command-резолв деградирует на cluster-
	// wide (backcompat unit-тестов без БД-scope; production-wire-up передаёт
	// rbac.Holder).
	scoper PurviewResolver
	auditW audit.Writer
	// tidingInvalidator сбрасывает TTL-снимок Tiding-правил dispatcher-а после
	// commit voyage-tx с ephemeral-notify (ADR-052(g) race-fix). nil → no-op
	// (dev без herald: деградация на TTL-сходимость).
	tidingInvalidator TidingInvalidator
	// maxScope — верхний лимит размера резолвнутого scope (DoS-guard S-med-3).
	// 0 → безлимит. Резолвится из cfg.Voyage.ResolvedMaxScope() в конструкторе.
	maxScope int
	// maxBatchSize — верхний предел эффективного размера батча/окна (DoS-guard
	// S-W4): batch_size для barrier, concurrency для window. 0 → без предела.
	// Резолвится из cfg.Voyage.ResolvedMaxBatchSize() в конструкторе.
	maxBatchSize int
	logger       *slog.Logger
}

// NewVoyageHandler собирает handler. logger=nil → discard. store /
// scenarioResolver / commandResolver / enforcer — обязательны для production-
// маршрутов; incReader нужен RBAC-by-kind scenario-gate-у (без него scenario-
// create fail-closed отвергает scoped-роли). scoper нужен command-пути для
// target ∩ Purview (ADR-047 S4); nil → command-резолв cluster-wide (backcompat
// unit-тестов). auditW допускает nil. tidingInvalidator сбрасывает TTL-снимок
// dispatcher-а после commit voyage-tx с ephemeral-notify (ADR-052(g) race-fix);
// nil → no-op (dev без herald). maxScope —
// верхний лимит размера резолвнутого scope (DoS-guard S-med-3); 0 → безлимит
// (caller передаёт cfg.Voyage.ResolvedMaxScope()). maxBatchSize — верхний предел
// размера батча/окна (DoS-guard S-W4); 0 → без предела
// (cfg.Voyage.ResolvedMaxBatchSize()).
func NewVoyageHandler(
	store VoyageStore,
	scenarioResolver VoyageScenarioResolver,
	commandResolver VoyageCommandResolver,
	incReader IncarnationContextReader,
	enforcer middleware.PermissionChecker,
	scoper PurviewResolver,
	auditW audit.Writer,
	tidingInvalidator TidingInvalidator,
	maxScope int,
	maxBatchSize int,
	logger *slog.Logger,
) *VoyageHandler {
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	return &VoyageHandler{
		store:             store,
		scenarioResolver:  scenarioResolver,
		commandResolver:   commandResolver,
		incReader:         incReader,
		enforcer:          enforcer,
		scoper:            scoper,
		auditW:            auditW,
		tidingInvalidator: tidingInvalidator,
		maxScope:          maxScope,
		maxBatchSize:      maxBatchSize,
		logger:            logger,
	}
}

// Конфигурационные лимиты POST-валидации (parity ErrandRun / Tide).
const (
	// voyageDefaultConcurrency — default степень параллелизма внутри Leg (parity
	// errandRunDefaultConcurrency).
	voyageDefaultConcurrency = 50
	// voyageMaxConcurrency — верхняя граница concurrency (parity ErrandRun
	// MaxConcurrency = 500; CHECK voyages_concurrency_positive не ограничивает
	// сверху, cap — invariant handler-а).
	voyageMaxConcurrency = 500
	// voyageMaxWhereBytes — DoS-guard для CEL-предиката command-target.where
	// (parity errandRunMaxWhereBytes = 4 KiB).
	voyageMaxWhereBytes = 4096
)

// --- POST /v1/voyages ---

// voyageCreateRequest — POST body. snake_case. unknown-поля отвергаем.
//
// НЕ alias на [VoyageCreateRequest] (ADR-051 — соображения soulCreateRequest):
// несёт server-only computed-поле `maxFailuresPercent` (не wire-поле — стешится в
// applyMaxFailures, резолвится по scope) и мутируется in-place всей цепочкой
// валидации (applyBatchSpec пишет BatchSize/BatchPercent, applyMaxFailures —
// FailThreshold). Pure-alias на gen-тип (typed-enum Kind/BatchMode/OnFailure +
// pointer-optional Target/Input) потребовал бы переписать security-критичный
// kind-RBAC и batch-резолв ради нулевой wire-выгоды — структура побайтово
// совпадает со схемой VoyageCreateRequest. Wire-shape сверен с oapi (categories
// A: kind/scenario_name/module/scheduling/batch* — те же ключи и типы).
type voyageCreateRequest struct {
	Kind         string               `json:"kind"`
	ScenarioName string               `json:"scenario_name,omitempty"`
	Module       string               `json:"module,omitempty"`
	Input        map[string]any       `json:"input,omitempty"`
	Target       *voyageTargetRequest `json:"target"`
	// Batch — строковый размер батча ("N" хостов / "N%" от scope), S1 строковых
	// batch-полей. Маппится на batch_size|batch_percent (см. applyBatchSpec).
	// Конфликтует с batch_size/batch_percent (нельзя оба формата). nil ⇒ старый
	// путь (batch_size/batch_percent как раньше).
	Batch                *string    `json:"batch,omitempty"`
	BatchSize            *int       `json:"batch_size,omitempty"`
	BatchPercent         *int       `json:"batch_percent,omitempty"`
	Concurrency          *int       `json:"concurrency,omitempty"`
	BatchMode            string     `json:"batch_mode,omitempty"`
	DryRun               bool       `json:"dry_run,omitempty"`
	ScheduleAt           *time.Time `json:"schedule_at,omitempty"`
	InterBatchIntervalMS *int       `json:"inter_batch_interval_ms,omitempty"`
	InterUnitIntervalMS  *int       `json:"inter_unit_interval_ms,omitempty"`
	// MaxFailures — строковый порог провалов ("N" абсолют / "N%" процент от единиц
	// прогона), S2 строковых batch-полей (ADR-043 amendment 2026-06-09). На резолве
	// распадается в FailThreshold (см. applyMaxFailures / resolveMaxFailuresPercent).
	// Конфликтует с fail_threshold (нельзя оба формата). nil ⇒ старый путь
	// (fail_threshold как раньше).
	MaxFailures   *string `json:"max_failures,omitempty"`
	FailThreshold *int    `json:"fail_threshold,omitempty"`
	RequireAlive  *bool   `json:"require_alive,omitempty"`
	OnFailure     string  `json:"on_failure,omitempty"`

	// Notify — разовые подписки на ЭТОТ прогон (ADR-052(g) amendment N2). Каждый
	// элемент keeper материализует в ephemeral-Tiding (ephemeral=true, voyage_id=
	// <новый voyage_id>) в ТОЙ ЖЕ транзакции, что создаёт Voyage. Атомарность даёт
	// наличие правила в БД к коммиту, но не его видимость TTL-снимку dispatcher-а
	// — после commit persist явно инвалидирует кэш (см. persist). nil/пусто ⇒ без
	// уведомлений. НЕ alias на [VoyageNotify]: keeper выводит event_types из
	// On по kind прогона (server-side маппинг), хранит annotations как сырой
	// json.RawMessage для object-валидации (ValidateAnnotationsJSON).
	Notify []voyageNotifyRequest `json:"notify,omitempty"`

	// maxFailuresPercent — необёрнутый процент из max_failures="N%", застешенный
	// applyMaxFailures для пост-scope резолва в абсолютный FailThreshold (зависит от
	// числа единиц прогона, известного только после резолва target-а). nil ⇒ percent
	// не задан (max_failures отсутствует, пуст либо абсолют — уже в FailThreshold).
	maxFailuresPercent *int
}

// voyageTargetRequest — declarative target (резолвится в snapshot единиц).
// scenario-режим читает Incarnations/Service/Coven; command-режим — SIDs/Coven/
// Where. Поля, нерелевантные для kind, игнорируются (handler валидирует
// непустоту нужного набора по kind).
type voyageTargetRequest struct {
	// scenario-режим:
	Incarnations []string `json:"incarnations,omitempty"`
	Service      string   `json:"service,omitempty"`
	// command-режим:
	SIDs  []string `json:"sids,omitempty"`
	Where string   `json:"where,omitempty"`
	// общий (env-тег incarnation для scenario / coven-метка хоста для command):
	Coven []string `json:"coven,omitempty"`
}

// voyageNotifyRequest — один элемент блока notify: разовая подписка на этот
// прогон (ADR-052(g)/(h)). Поля фильтров/тела совпадают с постоянным Tiding;
// event_types НЕ задаётся клиентом — выводится keeper-ом из On по kind прогона
// (см. notifyEventTypes). Annotations держим сырым json.RawMessage, чтобы
// провалидировать «верхний уровень — object» (herald.ValidateAnnotationsJSON)
// ДО распаковки в map.
type voyageNotifyRequest struct {
	Herald       string          `json:"herald"`
	On           []string        `json:"on,omitempty"`
	OnlyFailures bool            `json:"only_failures,omitempty"`
	OnlyChanges  bool            `json:"only_changes,omitempty"`
	Annotations  json.RawMessage `json:"annotations,omitempty"`
	Projection   []string        `json:"projection,omitempty"`
}

// voyageNotifyTerminal — допустимые значения notify.on (терминалы прогона,
// маппятся в event_types по kind, см. notifyEventTypes). Closed-enum, зеркало
// oapi VoyageNotifyOn.
const (
	notifyOnCompleted = "completed"
	notifyOnFailed    = "failed"
	notifyOnPartial   = "partial"
)

// voyageCreateReply — native 202 body (handler-native T5d). Плоская форма 1:1 с
// прежним VoyageCreateReply (все required-скаляры; kind/status — plain string,
// wire-байт идентичен string-named-enum). Сериализуется напрямую (MCP (w,r) writeJSON)
// и проецируется в api.VoyageCreateReply (huma-schema). Конвертация из row — в
// [VoyageHandler.newCreateReply].
type voyageCreateReply struct {
	Kind      string `json:"kind"`
	Location  string `json:"location"`
	ScopeSize int    `json:"scope_size"`
	Status    string `json:"status"`
	VoyageID  string `json:"voyage_id"`
}

// VoyageCreateReply / VoyagePreviewReply — экспортируемые алиасы reply-форм POST
// /v1/voyages[/preview] для FULL-TYPED huma-конверта (ADR-054, батч-2f self-audit):
// api-пакет (huma_voyage_op.go) собирает Body из reply-типа извлечённых CreateTyped/
// PreviewTyped. Алиасы (не новые типы) — те же oapi-формы, что отдаёт легаси (w,r).
type (
	VoyageCreateReply  = voyageCreateReply
	VoyagePreviewReply = voyagePreviewReply
	// VoyageCreateRequest — экспортируемый алиас доменной формы тела POST
	// /v1/voyages[/preview] для FULL-TYPED huma-конверта (ADR-054, батч-2f self-audit):
	// api-пакет собирает её из typed huma-body и зовёт CreateTyped/PreviewTyped. Поля
	// экспортированы (та же форма, что декодит легаси (w,r)); вложенные target/notify —
	// [VoyageTargetRequest]/[VoyageNotifyRequest] (общие с Cadence). Computed-поле
	// maxFailuresPercent — не wire, заполняется applyMaxFailures внутри валидации.
	VoyageCreateRequest = voyageCreateRequest
)

// VoyageSpecStub — непустой *VoyageHandler-заглушка для генерации huma-OpenAPI-
// фрагмента (HumaVoyageSpecYAML): при dump доменный handler не вызывается, но
// huma.Register требует non-nil для no-op-проверки. Все зависимости nil — handler
// никогда не исполняется в spec-режиме.
func VoyageSpecStub() *VoyageHandler { return &VoyageHandler{} }

// Create — POST /v1/voyages (ADR-043 §6 RBAC-by-kind, §4 target-резолв).
//
// Контракт:
//   - 202 + {voyage_id, kind, scope_size, status, location}.
//   - 400 — невалидный JSON.
//   - 403 — RBAC deny по kind (scenario без incarnation.run / command без
//     errand.run, либо хоть одна резолвнутая инкарнация вне scope).
//   - 404 — явная инкарнация (scenario.incarnations[]) не существует.
//   - 422 — невалидный kind / пустой scenario_name|module по kind / нет target /
//     невалидный SID/coven/имя / where > 4 KiB / on_failure не из {abort,
//     continue} / batch_size|concurrency <= 0 либо concurrency > max / пустой
//     резолв (voyage_empty_target) / резолвнутый scope > voyage.max_scope
//     (voyage_scope_too_large, DoS-guard S-med-3).
//   - 500 — store/resolver/enforcer не сконфигурирован / БД-сбой.
//
// RBAC-by-kind — fail-closed (ADR-043 §6): permission выбирается по kind ДО
// резолва target-а (дешёвый bare-check), затем для scenario — per-incarnation
// scope-check над резолвнутым набором (нельзя стартовать на инкарнации вне
// permission-скоупа = privilege escalation).
func (h *VoyageHandler) Create(w http.ResponseWriter, r *http.Request) {
	claims, ok := middleware.ClaimsFromContext(r.Context())
	if !ok {
		problem.Write(w, problem.New(problem.TypeInternalError, r.URL.Path, "missing claims"))
		return
	}

	var req voyageCreateRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		problem.Write(w, problem.New(problem.TypeMalformedRequest, r.URL.Path,
			"invalid JSON body: "+err.Error()))
		return
	}

	reply, err := h.CreateTyped(r.Context(), claims, &req)
	if err != nil {
		writeProblemError(w, r, err)
		return
	}
	w.Header().Set("Location", reply.Location)
	writeJSON(w, http.StatusAccepted, reply, h.logger)
}

// CreateTyped — извлечённая доменная функция POST /v1/voyages (FULL-TYPED ADR-054
// §Pattern, батч-2f self-audit): kind-независимый guard (validateVoyageRequest) +
// kind-ветка (createScenarioTyped/createCommandTyped) без http.ResponseWriter/*http.
// Request. RBAC-by-kind (ADR-043 §6) и self-audit (scenario_run.started / command_run.
// invoked) живут ВНУТРИ kind-веток. Декод тела — на вызывающем слое. req мутируется
// in-place (валидация). *problemError при отказе, успех — voyageCreateReply (202).
func (h *VoyageHandler) CreateTyped(ctx context.Context, claims *jwt.Claims, req *voyageCreateRequest) (voyageCreateReply, error) {
	var zero voyageCreateReply
	if h.store == nil || h.scenarioResolver == nil || h.commandResolver == nil || h.enforcer == nil {
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "voyage orchestrator is not configured")}
	}
	vc, err := h.validateVoyageRequest(req)
	if err != nil {
		return zero, err
	}
	switch vc.kind {
	case voyage.KindScenario:
		return h.createScenarioTyped(ctx, claims, req, vc.onFailure, vc.concurrency, vc.batchMode)
	default: // voyage.KindCommand (validateVoyageRequest гарантировал валидный kind)
		return h.createCommandTyped(ctx, claims, req, vc.onFailure, vc.concurrency, vc.batchMode)
	}
}

// voyageRequestCommon — kind-независимые параметры, резолвнутые на общем prelude
// декода/валидации (см. decodeAndValidateRequest). Шарится Create и Preview.
type voyageRequestCommon struct {
	kind        voyage.Kind
	onFailure   voyage.OnFailure
	concurrency int
	batchMode   voyage.BatchMode
}

// decodeAndValidateRequest — общий prelude POST /v1/voyages и POST
// /v1/voyages/preview: декод body (DisallowUnknownFields), нормализация
// on_failure/batch_mode, трансляция строковых batch/max_failures (S1/S2), весь
// kind-независимый guard (window-несовместимость batch_size/percent, XOR,
// диапазоны, concurrency-cap, max_batch_size для window). Preview переиспользует
// ровно тот же путь — гарантия консистентности 422 (preview отказывает там же,
// где Create). При любой ошибке пишет problem и возвращает ok=false.
//
// Поля, нерелевантные preview (dry_run / schedule_at / inter_*_interval_ms /
// on_failure / input), декодируются как обычно — preview их просто не читает в
// reply (на резолв/арифметику scope они не влияют). target / kind / batch* /
// concurrency / max_failures / require_alive — влияют и учитываются обоими.
// validateVoyageRequest — error-возвращающий kind-независимый guard POST /v1/voyages
// (FULL-TYPED ADR-054 §Pattern, батч-2f self-audit): нормализация on_failure/batch_mode,
// трансляция строковых batch/max_failures (S1/S2), window-несовместимость / XOR /
// диапазоны / concurrency-cap / max_batch_size для window. Декод тела — на вызывающем
// слое (huma typed Body / (w,r) json.Decode). req мутируется in-place (applyBatchSpec/
// applyMaxFailures). Возвращает voyageRequestCommon при успехе, *problemError при отказе
// (та же 400/422-классификация, что (w,r)-вариант).
func (h *VoyageHandler) validateVoyageRequest(req *voyageCreateRequest) (voyageRequestCommon, error) {
	var zero voyageRequestCommon
	kind := voyage.Kind(req.Kind)
	if !voyage.ValidKind(kind) {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
			"field 'kind' must be one of {scenario, command}")}
	}
	if req.Target == nil {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
			"field 'target' is required")}
	}

	onFailure, ofErr := normalizeVoyageOnFailure(req.OnFailure)
	if ofErr != "" {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", ofErr)}
	}
	batchMode, bmErr := normalizeVoyageBatchMode(req.BatchMode)
	if bmErr != "" {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", bmErr)}
	}
	if bErr := applyBatchSpec(req); bErr != "" {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", bErr)}
	}
	if mfErr := applyMaxFailures(req); mfErr != "" {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", mfErr)}
	}
	if batchMode == voyage.BatchModeWindow && req.BatchSize != nil {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
			"field 'batch_size' is not used with batch_mode=window (window width = concurrency)")}
	}
	if batchMode == voyage.BatchModeWindow && req.BatchPercent != nil {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
			"field 'batch_percent' is not used with batch_mode=window (window width = concurrency)")}
	}
	if req.BatchSize != nil && req.BatchPercent != nil {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
			"fields 'batch_size' and 'batch_percent' are mutually exclusive (set exactly one)")}
	}
	if req.BatchSize != nil && *req.BatchSize <= 0 {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", "field 'batch_size' must be > 0")}
	}
	if req.BatchPercent != nil && (*req.BatchPercent < 1 || *req.BatchPercent > 100) {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", "field 'batch_percent' must be in [1, 100]")}
	}
	if req.FailThreshold != nil && *req.FailThreshold <= 0 {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", "field 'fail_threshold' must be > 0")}
	}
	concurrency := voyageDefaultConcurrency
	if req.Concurrency != nil {
		concurrency = *req.Concurrency
	}
	if concurrency < 1 || concurrency > voyageMaxConcurrency {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
			fmt.Sprintf("field 'concurrency' must be in [1, %d]", voyageMaxConcurrency))}
	}
	// max_batch_size для window (ADR-043 amendment §6): потолок — concurrency (=
	// ширина окна), известен до резолва. Для barrier потолок зависит от len(scope) →
	// проверяется после резолва (scopeтого же batchSizeExceedsCapErr).
	if batchMode == voyage.BatchModeWindow {
		if err := h.batchSizeExceedsCapErr(concurrency, "concurrency"); err != nil {
			return zero, err
		}
	}

	return voyageRequestCommon{
		kind:        kind,
		onFailure:   onFailure,
		concurrency: concurrency,
		batchMode:   batchMode,
	}, nil
}

// --- POST /v1/voyages/preview ---

// voyagePreviewReply — 200 body POST /v1/voyages/preview (ADR-043 amendment
// 2026-06-09 §4). Dry-resolve scope БЕЗ создания Voyage и БЕЗ раскрытия SID-
// списка (только числа — предпоказ числа батчей для late-binding target-а).
//
// batch_mode присутствует ВСЕГДА (barrier/window) — он объясняет семантику
// остальных полей и снимает двусмысленность null-а:
//   - barrier → effective_batch_size = резолвнутый размер Leg (ceil scope*pct/100
//     для percent, либо явный batch_size, либо весь scope одним Leg); total_batches
//     = число Leg-ов = ceil(scope/effective_batch_size);
//   - window → effective_batch_size НЕПРИМЕНИМ (ширина окна = concurrency, не Leg) →
//     поле опущено (nil/omitempty), а не null-мусор; total_batches = 1 (плоский
//     прогон одной волной-окном, parity voyageTotalBatches). batch_mode=window
//     явно говорит UI «смотри concurrency, не effective_batch_size».
//
// Handler-native (T5d): плоская форма 1:1 с прежним VoyagePreviewReply.
// kind/batch_mode — plain string (wire-байт идентичен string-named-enum);
// effective_batch_size — *int с omitempty (опущен в window).
type voyagePreviewReply struct {
	BatchMode          string `json:"batch_mode"`
	EffectiveBatchSize *int   `json:"effective_batch_size,omitempty"`
	Kind               string `json:"kind"`
	ScopeSize          int    `json:"scope_size"`
	TotalBatches       int    `json:"total_batches"`
}

// Preview — POST /v1/voyages/preview (ADR-043 amendment 2026-06-09 §4, ADR-050
// amendment 2026-06-17 — собственный bucket voyage_preview, ADR-047 §S4).
// Dry-resolve scope: отвечает РОВНО то, что сделал бы
// Create (та же валидация / резолв / гейты — общий decodeAndValidateRequest +
// resolveScenarioScope/resolveCommandScope), но БЕЗ persist (не зовёт
// BeginTx/Insert) и БЕЗ раскрытия SID-списка.
//
// Контракт (консистентность с Create — preview отказывает там же):
//   - 200 + {kind, scope_size, total_batches, batch_mode, effective_batch_size?}.
//   - 400 — невалидный JSON.
//   - 403 — RBAC deny по kind / явный чужой хост (command) / инкарнация вне scope
//     (scenario).
//   - 404 — явная инкарнация не существует (scenario).
//   - 422 — невалидный kind/target/batch* / пустой резолв (voyage_empty_target) /
//     scope > voyage.max_scope (voyage_scope_too_large) / batch_size-cap.
//   - 429 — Tempo per-AID rate-limit (middleware, bucket voyage_preview).
//   - 500 — store/resolver/enforcer не сконфигурирован / БД-сбой.
func (h *VoyageHandler) Preview(w http.ResponseWriter, r *http.Request) {
	claims, ok := middleware.ClaimsFromContext(r.Context())
	if !ok {
		problem.Write(w, problem.New(problem.TypeInternalError, r.URL.Path, "missing claims"))
		return
	}

	var req voyageCreateRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		problem.Write(w, problem.New(problem.TypeMalformedRequest, r.URL.Path,
			"invalid JSON body: "+err.Error()))
		return
	}

	reply, err := h.PreviewTyped(r.Context(), claims, &req)
	if err != nil {
		writeProblemError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, reply, h.logger)
}

// PreviewTyped — извлечённая доменная функция POST /v1/voyages/preview (FULL-TYPED
// ADR-054 §Pattern, батч-2f): dry-resolve scope БЕЗ persist и БЕЗ audit (read-like
// POST — preview не пишет audit-event, в отличие от Create). Та же валидация/резолв/
// гейты, что Create (validateVoyageRequest + resolveScenarioScopeErr/
// resolveCommandScopeErr) → консистентность 422 (preview отказывает там же). Декод
// тела — на вызывающем слое. *problemError при отказе, успех — voyagePreviewReply (200).
func (h *VoyageHandler) PreviewTyped(ctx context.Context, claims *jwt.Claims, req *voyageCreateRequest) (voyagePreviewReply, error) {
	var zero voyagePreviewReply
	if h.store == nil || h.scenarioResolver == nil || h.commandResolver == nil || h.enforcer == nil {
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "voyage orchestrator is not configured")}
	}
	vc, err := h.validateVoyageRequest(req)
	if err != nil {
		return zero, err
	}

	var resolved []string
	switch vc.kind {
	case voyage.KindScenario:
		resolved, err = h.resolveScenarioScopeErr(ctx, claims, req)
	default: // voyage.KindCommand
		resolved, err = h.resolveCommandScopeErr(ctx, claims, req)
	}
	if err != nil {
		return zero, err
	}

	// Эффективный batch_size + max_batch_size-cap (barrier) — та же арифметика и тот
	// же гейт, что Create (parity воркера). resolveMaxFailuresPercent в preview не
	// нужен: max_failures на число батчей/scope не влияет (порог провалов — runtime).
	effBatchSize := effectiveBatchSize(req.BatchSize, req.BatchPercent, len(resolved))
	if vc.batchMode == voyage.BatchModeBarrier && effBatchSize != nil {
		if err := h.batchSizeExceedsCapErr(*effBatchSize, "batch_size"); err != nil {
			return zero, err
		}
	}

	reply := voyagePreviewReply{
		Kind:         string(vc.kind),
		ScopeSize:    len(resolved),
		TotalBatches: voyageTotalBatches(len(resolved), effBatchSize, vc.batchMode),
		BatchMode:    string(vc.batchMode),
	}
	// effective_batch_size осмыслен только в barrier. В window поле опускаем
	// (ширина окна = concurrency, не Leg-размер) — null-мусора в ответе нет.
	if vc.batchMode == voyage.BatchModeBarrier {
		reply.EffectiveBatchSize = effBatchSize
	}
	return reply, nil
}

// resolveScenarioScope — kind=scenario резолв scope + гейты (RBAC bare-check
// incarnation.run, per-incarnation scope-check fail-closed, max_scope-cap),
// общий для Create и Preview. Возвращает отсортированный snapshot имён
// инкарнаций; при любом отказе пишет problem и возвращает ok=false. Persist —
// дело caller-а (Create), Preview только читает len(resolved).
// resolveScenarioScopeErr — error-возвращающий резолв kind=scenario scope + гейты
// (FULL-TYPED ADR-054 §Pattern, батч-2f self-audit): RBAC bare-check incarnation.run,
// per-incarnation scope-check fail-closed (ADR-043 §6), max_scope-cap. Общий для
// Create и Preview. nil-error → отсортированный snapshot имён инкарнаций; *problemError
// при отказе (та же 403/404/422/500-классификация, что (w,r)-вариант). ctx — request-
// context (резолв/scope-select читают его).
func (h *VoyageHandler) resolveScenarioScopeErr(ctx context.Context, claims *jwt.Claims, req *voyageCreateRequest) ([]string, error) {
	if req.ScenarioName == "" {
		return nil, &problemError{problem.New(problem.TypeValidationFailed, "",
			"kind=scenario requires non-empty 'scenario_name'")}
	}
	if req.Module != "" {
		return nil, &problemError{problem.New(problem.TypeValidationFailed, "",
			"kind=scenario must not carry 'module'")}
	}
	if len(req.Target.Incarnations) == 0 && req.Target.Service == "" && len(req.Target.Coven) == 0 {
		return nil, &problemError{problem.New(problem.TypeValidationFailed, "",
			"kind=scenario target requires one of incarnations[]/service/coven")}
	}
	for _, name := range req.Target.Incarnations {
		if !incarnation.ValidName(name) {
			return nil, &problemError{problem.New(problem.TypeValidationFailed, "",
				"target.incarnations: name "+name+" must match "+incarnation.NamePattern)}
		}
	}
	// scenario использует один env-тег фильтра (coven[0]); список — приём UI,
	// но резолв (ListFilter.Coven exact any-of) принимает одну метку. Берём
	// первую непустую; остальные валидируем по формату.
	var covenFilter string
	for _, c := range req.Target.Coven {
		if !incarnationCovenLabelValid(c) {
			return nil, &problemError{problem.New(problem.TypeValidationFailed, "",
				"target.coven: label "+c+" must match "+soul.CovenPattern)}
		}
		if covenFilter == "" {
			covenFilter = c
		}
	}

	// RBAC bare-check incarnation.run (быстрый отказ до резолва).
	if err := h.checkPermissionErr(claims.Subject, "incarnation", "run", nil); err != nil {
		return nil, err
	}

	resolved, err := h.scenarioResolver.ResolveIncarnations(ctx, VoyageScenarioFilter{
		Incarnations: req.Target.Incarnations,
		Service:      req.Target.Service,
		Coven:        covenFilter,
	})
	if err != nil {
		if errors.Is(err, incarnation.ErrIncarnationNotFound) {
			return nil, &problemError{problem.New(problem.TypeNotFound, "", err.Error())}
		}
		h.logger.Error("voyage: scenario target resolve failed", slog.Any("error", err))
		return nil, &problemError{problem.New(problem.TypeInternalError, "",
			"resolve voyage scenario target failed")}
	}
	if len(resolved) == 0 {
		return nil, &problemError{problem.New(problem.TypeValidationFailed, "",
			"voyage_empty_target: resolved target is empty")}
	}

	// Per-incarnation scope-check (ADR-043 §6 fail-closed): оператор обязан иметь
	// incarnation.run на КАЖДОЙ резолвнутой инкарнации (её covens ∪ {name}).
	// Иначе старт на инкарнации вне permission-скоупа = privilege escalation.
	// incReader=nil (unit-тест без БД-scope) → пропускаем per-incarnation scope,
	// bare-check выше уже гарантировал базовое право (cluster-admin / bare-роль).
	if h.incReader != nil {
		for _, name := range resolved {
			inc, sErr := incarnation.SelectByName(ctx, h.incReader, name)
			if sErr != nil {
				h.logger.Error("voyage: scope-check select failed",
					slog.String("incarnation", name), slog.Any("error", sErr))
				return nil, &problemError{problem.New(problem.TypeInternalError, "",
					"voyage scope check failed")}
			}
			contexts := incarnationCovenContexts(inc.Name, inc.Service, inc.Covens)
			if !h.allowedAnyContext(claims.Subject, "incarnation", "run", contexts) {
				return nil, &problemError{problem.New(problem.TypeForbidden, "",
					"operator lacks incarnation.run on resolved incarnation "+name)}
			}
		}
	}

	if err := h.scopeExceedsCapErr(len(resolved)); err != nil {
		return nil, err
	}
	return resolved, nil
}

// createScenarioTyped — kind=scenario ветка Create (FULL-TYPED ADR-054 §Pattern,
// батч-2f self-audit). Резолв scope + гейты — resolveScenarioScopeErr; затем
// batch-арифметика, persist, self-audit scenario_run.started ВНУТРИ функции, reply.
// ctx — request-context. *problemError при отказе.
func (h *VoyageHandler) createScenarioTyped(
	ctx context.Context, claims *jwt.Claims,
	req *voyageCreateRequest, onFailure voyage.OnFailure, concurrency int, batchMode voyage.BatchMode,
) (voyageCreateReply, error) {
	var zero voyageCreateReply
	resolved, err := h.resolveScenarioScopeErr(ctx, claims, req)
	if err != nil {
		return zero, err
	}

	// notify → ephemeral-Tiding-шаблоны (ADR-052(g)): валидация + herald.read-guard
	// ДО открытия tx. voyage_id/name стемпятся в persist после генерации row.
	notifyTidings, err := h.prepareNotifyErr(ctx, claims, req, voyage.KindScenario)
	if err != nil {
		return zero, err
	}

	// Эффективный batch_size: batch_percent → ceil(scope * pct/100); иначе
	// req.BatchSize (как есть). Зависит от len(scope), поэтому считается после
	// резолва (ADR-043 amendment §2).
	effBatchSize := effectiveBatchSize(req.BatchSize, req.BatchPercent, len(resolved))
	// max_failures="N%" → абсолютный fail_threshold по числу ИНКАРНАЦИЙ (единица
	// прогона scenario): ceil(scope*pct/100), clamp [1,scope] (ADR-043 amendment
	// 2026-06-09 §2). Та же база scope=len(resolved), что и effectiveBatchSize.
	resolveMaxFailuresPercent(req, len(resolved))
	// max_batch_size для barrier — потолок эффективного batch_size (S-W4).
	if batchMode == voyage.BatchModeBarrier && effBatchSize != nil {
		if err := h.batchSizeExceedsCapErr(*effBatchSize, "batch_size"); err != nil {
			return zero, err
		}
	}

	targets := make([]voyage.VoyageTarget, len(resolved))
	for i, name := range resolved {
		targets[i] = voyage.VoyageTarget{
			TargetKind: voyage.TargetKindIncarnation,
			TargetID:   name,
			BatchIndex: voyageBatchIndex(i, effBatchSize, batchMode),
			Status:     voyage.TargetStatusAwaiting,
		}
	}

	row := h.buildVoyageRow(voyage.KindScenario, req, claims.Subject, &req.ScenarioName, nil, resolved, onFailure, concurrency, batchMode, effBatchSize)
	stampEphemeralTidings(notifyTidings, row.VoyageID)
	if err := h.persistErr(ctx, row, targets, notifyTidings); err != nil {
		return zero, err
	}

	h.emitCreated(claims.Subject, middleware.ScenarioInvocationSource(ctx), audit.EventScenarioRunStarted, row, req.Target, len(resolved))
	return h.newCreateReply(row, len(resolved)), nil
}

// resolveCommandScope — kind=command резолв scope + гейты (RBAC errand.run +
// target ∩ Purview гибрид-семантика, max_scope-cap), общий для Create и Preview.
// Возвращает SID-snapshot (AND-merge sids/coven, урезанный до Purview оператора);
// при отказе пишет problem и возвращает ok=false. Гибрид-ветки (ADR-047 S4):
// явный чужой SID → 403, широкий target урезан в ноль → 422 voyage_empty_target.
// Persist — дело caller-а (Create), Preview только читает len(resolved).
// resolveCommandScopeErr — error-возвращающий резолв kind=command scope + гейты
// (FULL-TYPED ADR-054 §Pattern, батч-2f self-audit): RBAC errand.run + target ∩
// Purview гибрид-семантика, max_scope-cap. Общий для Create и Preview. nil-error → SID-
// snapshot; *problemError при отказе. Гибрид-ветки (ADR-047 S4): явный чужой SID →
// 403, широкий target урезан в ноль → 422 voyage_empty_target. ctx — request-context.
func (h *VoyageHandler) resolveCommandScopeErr(ctx context.Context, claims *jwt.Claims, req *voyageCreateRequest) ([]string, error) {
	if req.Module == "" {
		return nil, &problemError{problem.New(problem.TypeValidationFailed, "",
			"kind=command requires non-empty 'module'")}
	}
	if req.ScenarioName != "" {
		return nil, &problemError{problem.New(problem.TypeValidationFailed, "",
			"kind=command must not carry 'scenario_name'")}
	}
	if len(req.Target.SIDs) == 0 && len(req.Target.Coven) == 0 && req.Target.Where == "" {
		return nil, &problemError{problem.New(problem.TypeValidationFailed, "",
			"kind=command target requires one of sids[]/coven/where")}
	}
	// S-med-1 (security): where: пока НЕ вычисляется (MVP, no-op в резолвере —
	// сохраняется лишь в target_origin). where-only target (нет ни sids, ни coven)
	// поэтому молча резолвится на ВЕСЬ флот — сужающий предикат превращается в
	// расширитель, нарушая инвариант «invocation сужает scope, не расширяет».
	// Отвергаем единственный опасный кейс. where как ДОПОЛНЕНИЕ к sids/coven
	// допустимо: scope уже сужен ими, where не расширяет.
	if req.Target.Where != "" && len(req.Target.SIDs) == 0 && len(req.Target.Coven) == 0 {
		return nil, &problemError{problem.New(problem.TypeValidationFailed, "",
			"voyage_where_not_evaluated: target where: пока не вычисляется (MVP); "+
				"укажите sids или coven для сужения scope")}
	}
	for _, sid := range req.Target.SIDs {
		if !soul.ValidSID(sid) {
			return nil, &problemError{problem.New(problem.TypeValidationFailed, "",
				"target.sids: SID "+sid+" must match "+soul.SIDPattern)}
		}
	}
	for _, label := range req.Target.Coven {
		if !soul.ValidCoven(label) {
			return nil, &problemError{problem.New(problem.TypeValidationFailed, "",
				"target.coven: label "+label+" must match "+soul.CovenPattern)}
		}
	}
	if len(req.Target.Where) > voyageMaxWhereBytes {
		return nil, &problemError{problem.New(problem.TypeValidationFailed, "",
			fmt.Sprintf("target.where exceeds %d bytes", voyageMaxWhereBytes))}
	}

	filter := VoyageCommandFilter{
		SIDs:         req.Target.SIDs,
		Covens:       req.Target.Coven,
		Where:        req.Target.Where,
		RequireAlive: voyage.ResolveRequireAlive(req.RequireAlive),
	}

	var resolved []string
	// RBAC + target ∩ Purview (ADR-047 S4, security-fix). scoper=nil (unit-тест без
	// БД-scope) → cluster-wide резолв + nil-context bare-check (backcompat). Иначе:
	// один ResolvePurview(errand.run) даёт И existence-gate (держит ли право хоть в
	// каком scope — иначе nil-context Check ложно денит scoped-роль, как souls G1),
	// И scope-границу для пересечения с резолвнутым target-ом.
	if h.scoper == nil {
		if err := h.checkPermissionErr(claims.Subject, "errand", "run", nil); err != nil {
			return nil, err
		}
		out, err := h.commandResolver.ResolveSIDs(ctx, filter)
		if err != nil {
			h.logger.Error("voyage: command target resolve failed", slog.Any("error", err))
			return nil, &problemError{problem.New(problem.TypeInternalError, "",
				"resolve voyage command target failed")}
		}
		resolved = out
	} else {
		// Existence-gate errand.run: оператор обязан держать право хоть в КАКОМ-ТО
		// scope (или Unrestricted). Пустой Purview (нет права / ревокнут → Deny) →
		// 403. Сужение по scope делает резолвер (parity souls HoldsAction).
		pv := h.scoper.ResolvePurview(claims.Subject, "errand", "run")
		scope := soulpurview.Resolve(pv)
		if scope.Empty {
			// scope.Empty не несёт причину (revoke vs no-perm слиты в Resolve).
			// Классифицируем причину через enforcer для парити error-семантики со
			// scenario-путём и scoper==nil-веткой (revoked → TypeOperatorRevokedToken,
			// no-perm → 403). nil-context здесь безопасен: scope пуст в ЛЮБОМ
			// контексте (Empty это уже доказал), ложного деная scoped-роли быть не
			// может — checkPermissionErr тут только КЛАССИФИКАТОР причины, не второй gate.
			if err := h.checkPermissionErr(claims.Subject, "errand", "run", nil); err != nil {
				return nil, err
			}
			// Недостижимая подстраховка: Empty без deny от enforcer невозможен.
			return nil, &problemError{problem.New(problem.TypeForbidden, "",
				"operator lacks required permission errand.run")}
		}
		scoped, err := h.commandResolver.ResolveSIDsInScope(ctx, filter, scope)
		if err != nil {
			h.logger.Error("voyage: command target resolve failed", slog.Any("error", err))
			return nil, &problemError{problem.New(problem.TypeInternalError, "",
				"resolve voyage command target failed")}
		}
		// Гибрид ветка 1 (anti-escalation): явно-указанный чужой хост в sids[] →
		// 403 (parity per-incarnation scope-check scenario-пути). Молчаливое
		// урезание тут было бы маскировкой попытки эскалации.
		if len(scoped.DeniedExplicit) > 0 {
			return nil, &problemError{problem.New(problem.TypeForbidden, "",
				"operator lacks errand.run on target host "+scoped.DeniedExplicit[0])}
		}
		resolved = scoped.SIDs
	}
	// Гибрид ветка 3: пустое пересечение (широкий target урезан в ноль) → 422,
	// отличаем от 403-эскалации (валидный запрос, но нечего исполнять).
	if len(resolved) == 0 {
		return nil, &problemError{problem.New(problem.TypeValidationFailed, "",
			"voyage_empty_target: resolved target is empty")}
	}

	if err := h.scopeExceedsCapErr(len(resolved)); err != nil {
		return nil, err
	}
	return resolved, nil
}

// createCommandTyped — kind=command ветка Create (FULL-TYPED ADR-054 §Pattern,
// батч-2f self-audit). Резолв scope + гейты — resolveCommandScopeErr; затем
// batch-арифметика, persist, self-audit command_run.invoked ВНУТРИ функции, reply.
func (h *VoyageHandler) createCommandTyped(
	ctx context.Context, claims *jwt.Claims,
	req *voyageCreateRequest, onFailure voyage.OnFailure, concurrency int, batchMode voyage.BatchMode,
) (voyageCreateReply, error) {
	var zero voyageCreateReply
	resolved, err := h.resolveCommandScopeErr(ctx, claims, req)
	if err != nil {
		return zero, err
	}

	// notify → ephemeral-Tiding-шаблоны (ADR-052(g)): валидация + herald.read-guard
	// ДО открытия tx. voyage_id/name стемпятся в persist после генерации row.
	notifyTidings, err := h.prepareNotifyErr(ctx, claims, req, voyage.KindCommand)
	if err != nil {
		return zero, err
	}

	// Эффективный batch_size: batch_percent → ceil(scope * pct/100); иначе
	// req.BatchSize. Зависит от len(scope) (ADR-043 amendment §2).
	effBatchSize := effectiveBatchSize(req.BatchSize, req.BatchPercent, len(resolved))
	// max_failures="N%" → абсолютный fail_threshold по числу ХОСТОВ (единица
	// прогона command): ceil(scope*pct/100), clamp [1,scope] (ADR-043 amendment
	// 2026-06-09 §2). Та же база scope=len(resolved), что и effectiveBatchSize.
	resolveMaxFailuresPercent(req, len(resolved))
	if batchMode == voyage.BatchModeBarrier && effBatchSize != nil {
		if err := h.batchSizeExceedsCapErr(*effBatchSize, "batch_size"); err != nil {
			return zero, err
		}
	}

	targets := make([]voyage.VoyageTarget, len(resolved))
	for i, sid := range resolved {
		targets[i] = voyage.VoyageTarget{
			TargetKind: voyage.TargetKindSID,
			TargetID:   sid,
			BatchIndex: voyageBatchIndex(i, effBatchSize, batchMode),
			Status:     voyage.TargetStatusAwaiting,
		}
	}

	row := h.buildVoyageRow(voyage.KindCommand, req, claims.Subject, nil, &req.Module, resolved, onFailure, concurrency, batchMode, effBatchSize)
	stampEphemeralTidings(notifyTidings, row.VoyageID)
	if err := h.persistErr(ctx, row, targets, notifyTidings); err != nil {
		return zero, err
	}

	h.emitCreated(claims.Subject, middleware.ScenarioInvocationSource(ctx), audit.EventCommandRunInvoked, row, req.Target, len(resolved))
	return h.newCreateReply(row, len(resolved)), nil
}

// scopeExceedsCap проверяет резолвнутый scope против maxScope (DoS-guard
// S-med-3). maxScope=0 → безлимит. При превышении пишет 422
// voyage_scope_too_large и возвращает true (caller прекращает обработку).
// Вызывается ПОСЛЕ резолва target-а в []VoyageTarget, ДО InsertTargets: один
// POST не должен резолвнуть весь флот (100k per-row INSERT в одной транзакции +
// неконтролируемый blast-radius).
// scopeExceedsCapErr — DoS-guard размера резолвнутого scope против maxScope (S-med-3,
// FULL-TYPED ADR-054 §Pattern). nil → в пределах лимита; иначе 422 voyage_scope_too_large.
func (h *VoyageHandler) scopeExceedsCapErr(size int) error {
	if h.maxScope <= 0 || size <= h.maxScope {
		return nil
	}
	return &problemError{problem.New(problem.TypeValidationFailed, "",
		fmt.Sprintf("voyage_scope_too_large: scope %d превышает лимит %d; "+
			"сузьте target (sids/coven/incarnations) или поднимите voyage.max_scope",
			size, h.maxScope))}
}

// batchSizeExceedsCap проверяет размер батча/окна против maxBatchSize (DoS-guard
// S-W4, ADR-043 amendment §6). maxBatchSize=0 → без предела. При превышении пишет
// 422 voyage_batch_size_too_large (parity voyage_scope_too_large) и возвращает
// true. field — имя поля в detail ("batch_size" для barrier / "concurrency" для
// window).
// batchSizeExceedsCapErr — DoS-guard размера батча/окна против maxBatchSize (S-W4,
// FULL-TYPED ADR-054 §Pattern). nil → в пределах; иначе 422 voyage_batch_size_too_large.
func (h *VoyageHandler) batchSizeExceedsCapErr(size int, field string) error {
	if h.maxBatchSize <= 0 || size <= h.maxBatchSize {
		return nil
	}
	return &problemError{problem.New(problem.TypeValidationFailed, "",
		fmt.Sprintf("voyage_batch_size_too_large: %s %d превышает лимит %d; "+
			"уменьшите %s или поднимите voyage.max_batch_size",
			field, size, h.maxBatchSize, field))}
}

// effectiveBatchSize резолвит эффективный размер пачки (ADR-043 amendment §2):
//   - batch_percent задан → ceil(scope * pct/100), но не меньше 1 и не больше
//     scope (пачка не может превышать весь scope);
//   - иначе → batchSize как есть (включая nil = весь прогон одним Leg).
//
// scope <= 0 (защита, в норме caller отсёк пустой резолв) → возвращает batchSize.
func effectiveBatchSize(batchSize, batchPercent *int, scope int) *int {
	if batchPercent == nil {
		return batchSize
	}
	if scope <= 0 {
		return batchSize
	}
	// ceil(scope * pct / 100) целочисленно.
	eff := (scope*(*batchPercent) + 99) / 100
	if eff < 1 {
		eff = 1
	}
	if eff > scope {
		eff = scope
	}
	return &eff
}

// buildVoyageRow собирает *voyage.Voyage из request-а. target_resolved
// сериализуется в JSONB-массив единиц (имена / SID-ы); target_origin —
// declarative-форма для audit/UI. total_batches вычисляется из числа единиц и
// эффективного batch_size. effBatchSize — резолвнутый batch_size (из batch_size
// или batch_percent, ADR-043 amendment §2); nil → весь прогон одним Leg.
func (h *VoyageHandler) buildVoyageRow(
	kind voyage.Kind, req *voyageCreateRequest, startedByAID string,
	scenarioName, module *string, resolved []string, onFailure voyage.OnFailure, concurrency int, batchMode voyage.BatchMode,
	effBatchSize *int,
) *voyage.Voyage {
	resolvedJSON, _ := json.Marshal(resolved) // []string всегда сериализуется
	originJSON, _ := json.Marshal(req.Target)

	var inputJSON []byte
	if req.Input != nil {
		inputJSON, _ = json.Marshal(req.Input)
	}

	row := &voyage.Voyage{
		VoyageID:       audit.NewULID(),
		Kind:           kind,
		ScenarioName:   scenarioName,
		Module:         module,
		Input:          inputJSON,
		TargetResolved: resolvedJSON,
		TargetOrigin:   originJSON,
		Concurrency:    &concurrency,
		DryRun:         req.DryRun,
		ScheduleAt:     req.ScheduleAt,
		OnFailure:      &onFailure,
		TotalBatches:   voyageTotalBatches(len(resolved), effBatchSize, batchMode),
		StartedByAID:   startedByAID,
		FailThreshold:  req.FailThreshold,
		RequireAlive:   req.RequireAlive,
	}
	// batch_mode пишем только при window — barrier остаётся NULL (forward-compat:
	// «не задано» = barrier, отличимо от явного значения в audit/UI).
	if batchMode == voyage.BatchModeWindow {
		bm := batchMode
		row.BatchMode = &bm
	}
	// window: batch_size / batch_percent не используются (ширина окна =
	// concurrency) — не пишем. barrier: пишем эффективный batch_size (резолвнутый
	// из percent или явный), а batch_percent сохраняем как есть для audit/UI.
	if batchMode != voyage.BatchModeWindow {
		row.BatchSize = effBatchSize
		row.BatchPercent = req.BatchPercent
	}
	if req.InterBatchIntervalMS != nil && *req.InterBatchIntervalMS > 0 {
		d := time.Duration(*req.InterBatchIntervalMS) * time.Millisecond
		row.InterBatchInterval = &d
	}
	// inter_unit_interval — per-unit пауза, осмыслена только в window (parity:
	// inter_batch_interval только в barrier). В barrier не пишем.
	if batchMode == voyage.BatchModeWindow && req.InterUnitIntervalMS != nil && *req.InterUnitIntervalMS > 0 {
		d := time.Duration(*req.InterUnitIntervalMS) * time.Millisecond
		row.InterUnitInterval = &d
	}
	return row
}

// persist пишет targets + voyage + ephemeral-Tiding-и (notify, ADR-052(g)) в
// ОДНОЙ PG-транзакции (snapshot-scope не «дрожит» между INSERT-ами, ADR-043).
// Атомарность гарантирует НАЛИЧИЕ ephemeral-правил в БД к моменту коммита, но
// НЕ их видимость TTL-снимку dispatcher-а (DefaultRuleCacheTTL=15s) — поэтому
// после commit при наличии notify-правил нужна явная двухуровневая инвалидация
// (in-process + cross-keeper), иначе быстрый прогон диспетчит терминал против
// устаревшего снимка и разовое уведомление молча промахивается.
// Возвращает false и пишет problem при ошибке. При откате tx — нет ни Voyage,
// ни ephemeral-правил (и инвалидировать нечего — звать СТРОГО после commit).
// persistErr — error-возвращающий persist Voyage (FULL-TYPED ADR-054 §Pattern,
// батч-2f self-audit): targets + voyage + ephemeral-Tiding-и (notify, ADR-052(g)) в
// ОДНОЙ PG-транзакции. nil-error → успех; *problemError при сбое (500). При откате tx
// — нет ни Voyage, ни ephemeral-правил. ctx — request-context (rollback на
// background-ctx, инвалидация TTL-снимка читает ctx).
func (h *VoyageHandler) persistErr(ctx context.Context, row *voyage.Voyage, targets []voyage.VoyageTarget, notifyTidings []herald.Tiding) error {
	tx, err := h.store.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		h.logger.Error("voyage.create: begin tx failed",
			slog.String("voyage_id", row.VoyageID), slog.Any("error", err))
		return &problemError{problem.New(problem.TypeInternalError, "", "insert voyage failed")}
	}
	// Insert самого Voyage сначала (FK voyage_targets → voyages); затем targets.
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(context.Background())
		}
	}()

	if err := voyage.Insert(ctx, tx, row); err != nil {
		h.logger.Error("voyage.create: insert failed",
			slog.String("voyage_id", row.VoyageID), slog.Any("error", err))
		return &problemError{problem.New(problem.TypeInternalError, "", "insert voyage failed")}
	}
	if err := voyage.InsertTargets(ctx, tx, row.VoyageID, targets); err != nil {
		h.logger.Error("voyage.create: insert targets failed",
			slog.String("voyage_id", row.VoyageID), slog.Any("error", err))
		return &problemError{problem.New(problem.TypeInternalError, "", "insert voyage targets failed")}
	}
	// Ephemeral-Tiding-и notify-блока — в ТОЙ ЖЕ tx (ADR-052(g)). Любой сбой
	// (включая нарушение инварианта ephemeral⟺voyage_id или гонку имён) откатывает
	// весь Voyage — атомарность by construction. herald.InsertTiding принимает
	// ту же pgx.Tx (herald.ExecQueryRower ⊂ интерфейса tx).
	for i := range notifyTidings {
		if err := herald.InsertTiding(ctx, tx, &notifyTidings[i]); err != nil {
			h.logger.Error("voyage.create: insert ephemeral tiding failed",
				slog.String("voyage_id", row.VoyageID),
				slog.String("herald", notifyTidings[i].Herald),
				slog.Any("error", err))
			return &problemError{problem.New(problem.TypeInternalError, "", "insert voyage notify failed")}
		}
	}
	if err := tx.Commit(ctx); err != nil {
		h.logger.Error("voyage.create: commit failed",
			slog.String("voyage_id", row.VoyageID), slog.Any("error", err))
		return &problemError{problem.New(problem.TypeInternalError, "", "insert voyage failed")}
	}
	committed = true
	// Ephemeral-Tiding-и вставлены прямым InsertTiding в обход herald.Service-CRUD
	// (и его инвалидации) — сбрасываем TTL-снимок dispatcher-а ЯВНО, строго после
	// commit (двухуровнево: in-process + cross-keeper, HA-финализация прогона может
	// идти на другом keeper-е). Без notify правил не создано — инвалидировать
	// нечего. nil-safe: dev без herald → no-op, деградация на TTL.
	if len(notifyTidings) > 0 && h.tidingInvalidator != nil {
		h.tidingInvalidator.InvalidateTidings(ctx, row.VoyageID)
	}
	return nil
}

// newCreateReply собирает 202-reply (voyageCreateReply) из row + scope_size
// (FULL-TYPED ADR-054 §Pattern). Location = относительный URL ресурса (его же ставит
// Create(w,r)/huma в Location-header).
func (h *VoyageHandler) newCreateReply(row *voyage.Voyage, scopeSize int) voyageCreateReply {
	location := "/v1/voyages/" + row.VoyageID
	return voyageCreateReply{
		VoyageID:  row.VoyageID,
		Kind:      string(row.Kind),
		ScopeSize: scopeSize,
		Status:    string(row.Status),
		Location:  location,
	}
}

// checkPermissionErr — error-возвращающий RBAC.Check (FULL-TYPED ADR-054 §Pattern,
// батч-2f self-audit). nil → разрешено. revoked → 401-эквивалент
// (TypeOperatorRevokedToken), no-perm → 403. Маппинг как middleware.RequirePermission.
func (h *VoyageHandler) checkPermissionErr(aid, resource, action string, ctx map[string]string) error {
	if err := h.enforcer.Check(aid, resource, action, ctx); err != nil {
		if errors.Is(err, rbac.ErrOperatorRevoked) {
			return &problemError{problem.New(problem.TypeOperatorRevokedToken, "", "archon "+aid+" has been revoked")}
		}
		detail := "operator lacks required permission"
		if errors.Is(err, rbac.ErrPermissionDenied) {
			detail = "operator lacks required permission " + resource + "." + action
		}
		return &problemError{problem.New(problem.TypeForbidden, "", detail)}
	}
	return nil
}

// allowedAnyContext OR-проверяет permission по набору контекстов (parity
// RequirePermissionMulti). Пустой набор → одна попытка с nil-context (bare/`*`).
func (h *VoyageHandler) allowedAnyContext(aid, resource, action string, contexts []map[string]string) bool {
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

// --- GET /v1/voyages ---

// voyageDTO — native response-форма для GET/List (handler-native T5d). Плоская
// форма 1:1 с прежним Voyage: ПОРЯДОК ПОЛЕЙ алфавитный (как oapi-codegen),
// чтобы json.Marshal эмитил ключи byte-exact; kind/status/batch_mode/on_failure —
// plain string (wire идентичен string-named-enum); pointer-optional с omitempty —
// все nullable. target_resolved (имена/SID-ы) НЕ кладётся (UI читает scope_size).
// Сериализуется напрямую (MCP/cadence-runs writeJSON), проецируется в api.Voyage
// (huma-schema). Конвертация из row — [toVoyageDTO].
type voyageDTO struct {
	Attempt           int               `json:"attempt"`
	BatchMode         *string           `json:"batch_mode,omitempty"`
	BatchPercent      *int              `json:"batch_percent,omitempty"`
	BatchSize         *int              `json:"batch_size,omitempty"`
	Concurrency       *int              `json:"concurrency,omitempty"`
	CreatedAt         time.Time         `json:"created_at"`
	CurrentBatchIndex int               `json:"current_batch_index"`
	DryRun            bool              `json:"dry_run"`
	FailThreshold     *int              `json:"fail_threshold,omitempty"`
	FinishedAt        *time.Time        `json:"finished_at,omitempty"`
	Kind              string            `json:"kind"`
	Module            *string           `json:"module,omitempty"`
	OnFailure         *string           `json:"on_failure,omitempty"`
	RequireAlive      *bool             `json:"require_alive,omitempty"`
	ScenarioName      *string           `json:"scenario_name,omitempty"`
	ScheduleAt        *time.Time        `json:"schedule_at,omitempty"`
	ScopeSize         int               `json:"scope_size"`
	StartedAt         *time.Time        `json:"started_at,omitempty"`
	StartedByAID      string            `json:"started_by_aid"`
	Status            string            `json:"status"`
	Summary           *voyageSummaryDTO `json:"summary,omitempty"`
	Target            *voyageTargetDTO  `json:"target,omitempty"`
	TotalBatches      int               `json:"total_batches"`
	VoyageID          string            `json:"voyage_id"`
}

// voyageTargetDTO — native declarative-target reply-форма (origin, handler-native
// T5d). Все поля pointer-optional с omitempty (1:1 VoyageTarget); ПОРЯДОК
// алфавитный (coven/incarnations/service/sids/where), wire-ключ `sids` совпадает.
// Заполняется unmarshal-ом target_origin JSONB.
type voyageTargetDTO struct {
	Coven        *[]string `json:"coven,omitempty"`
	Incarnations *[]string `json:"incarnations,omitempty"`
	Service      *string   `json:"service,omitempty"`
	Sids         *[]string `json:"sids,omitempty"`
	Where        *string   `json:"where,omitempty"`
}

// voyageSummaryDTO — native агрегаты прогона (handler-native T5d). no_match — *int
// с omitempty (0 → ключ опущен, как noMatchPtr); прочие — int required.
type voyageSummaryDTO struct {
	Cancelled int  `json:"cancelled"`
	Failed    int  `json:"failed"`
	NoMatch   *int `json:"no_match,omitempty"`
	Succeeded int  `json:"succeeded"`
	Total     int  `json:"total"`
}

// voyageTargetEntryDTO — native строка voyage_targets (All-runs drill, handler-native
// T5d). target_kind/status — plain string; apply_id/errand_id/finished_at —
// pointer-optional с omitempty. ПОРЯДОК полей алфавитный (как oapi-codegen).
type voyageTargetEntryDTO struct {
	ApplyID    *string    `json:"apply_id,omitempty"`
	BatchIndex int        `json:"batch_index"`
	ErrandID   *string    `json:"errand_id,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	Status     string     `json:"status"`
	TargetID   string     `json:"target_id"`
	TargetKind string     `json:"target_kind"`
}

// scopeSizeOf декодирует target_resolved JSONB-массив и считает его длину.
// Невалидный/пустой → 0 (graceful; терминальные счётчики берутся из summary).
func scopeSizeOf(raw json.RawMessage) int {
	if len(raw) == 0 {
		return 0
	}
	var items []string
	if err := json.Unmarshal(raw, &items); err != nil {
		return 0
	}
	return len(items)
}

// toVoyageDTO проецирует domain [voyage.Voyage] в wire-тип [Voyage]
// (категория C). Typed-enum Kind/Status [VoyageKind]/[VoyageStatus] —
// wire та же строка. scenario_name/module/batch_mode/on_failure — pointer-optional
// с omitempty: домен несёт `*string`/`*BatchMode`/`*OnFailure`, gen — те же
// `*string`/`*enum` (nil → ключ опущен, байт-в-байт со старым `string ...,omitempty`
// при пустом значении). target_origin — typed-struct JSONB (НЕ byte-passthrough):
// unmarshal в [VoyageTarget] (домен-`SIDs` → gen-`Sids`, wire-ключ `sids`
// совпадает); невалидный → target опущен (graceful, как раньше). date-time
// created_at/started_at/finished_at/schedule_at — наносекундный wire (домен —
// голый time.Time): присваиваем как есть БЕЗ .UTC()/Truncate, сохраняя байт-в-байт
// с прежней прямой сериализацией v.CreatedAt.
func toVoyageDTO(v *voyage.Voyage) voyageDTO {
	dto := voyageDTO{
		VoyageID:          v.VoyageID,
		Kind:              string(v.Kind),
		Status:            string(v.Status),
		ScopeSize:         scopeSizeOf(v.TargetResolved),
		BatchSize:         v.BatchSize,
		BatchPercent:      v.BatchPercent,
		Concurrency:       v.Concurrency,
		DryRun:            v.DryRun,
		TotalBatches:      v.TotalBatches,
		CurrentBatchIndex: v.CurrentBatchIndex,
		FailThreshold:     v.FailThreshold,
		RequireAlive:      v.RequireAlive,
		ScheduleAt:        v.ScheduleAt,
		Attempt:           v.Attempt,
		StartedByAID:      v.StartedByAID,
		CreatedAt:         v.CreatedAt,
		StartedAt:         v.StartedAt,
		FinishedAt:        v.FinishedAt,
	}
	if v.BatchMode != nil {
		bm := string(*v.BatchMode)
		dto.BatchMode = &bm
	}
	dto.ScenarioName = v.ScenarioName
	dto.Module = v.Module
	if v.OnFailure != nil {
		of := string(*v.OnFailure)
		dto.OnFailure = &of
	}
	if len(v.TargetOrigin) > 0 {
		var t voyageTargetDTO
		if err := json.Unmarshal(v.TargetOrigin, &t); err == nil {
			dto.Target = &t
		}
	}
	if v.Summary != nil {
		dto.Summary = &voyageSummaryDTO{
			Total:     v.Summary.Total,
			Succeeded: v.Summary.Succeeded,
			Failed:    v.Summary.Failed,
			Cancelled: v.Summary.Cancelled,
			NoMatch:   noMatchPtr(v.Summary.NoMatch),
		}
	}
	return dto
}

// noMatchPtr оборачивает summary.no_match в pointer-optional [VoyageSummary].
// Старый DTO нёс `int json:"no_match,omitempty"` (0 → ключ опущен); gen — `*int`
// с omitempty. Сохраняем байт-в-байт: 0 → nil-указатель (ключ опущен), иначе —
// указатель на значение.
func noMatchPtr(n int) *int {
	if n == 0 {
		return nil
	}
	return &n
}

// List — GET /v1/voyages (ADR-043 §5).
//
// Query-фильтры: kind (exact), status (multi-value OR). Pagination —
// sharedapi.ParsePage (max=1000). Sort created_at DESC.
func (h *VoyageHandler) List(w http.ResponseWriter, r *http.Request) {
	page, err := sharedapi.ParsePage(r.URL.Query())
	if err != nil {
		problem.Write(w, problem.New(problem.TypeMalformedRequest, r.URL.Path, err.Error()))
		return
	}
	q := r.URL.Query()
	reply, perr := h.ListTyped(r.Context(), VoyageListInput{
		Kind:     q.Get("kind"),
		Statuses: q["status"],
		Page:     page,
	})
	if perr != nil {
		writeProblemError(w, r, perr)
		return
	}
	writeJSON(w, http.StatusOK, reply, h.logger)
}

// VoyageListInput — typed-вход [VoyageHandler.ListTyped] (FULL-TYPED ADR-054 §Pattern,
// батч-2f). Kind/Statuses — query-фильтры (валидация enum — в ListTyped → 422); Page —
// уже распарсенная пагинация (диапазон проверяет caller через ParsePage/CheckPageBounds
// → 400). Пустые → фильтр не применять.
type VoyageListInput struct {
	Kind     string
	Statuses []string
	Page     sharedapi.Page
}

// VoyageListReply — native typed-выход list (handler-native T5d). Плоская форма 1:1
// с прежним VoyageListReply (items/offset/limit/total). items — native voyageDTO.
type VoyageListReply struct {
	Items  []voyageDTO `json:"items"`
	Limit  int         `json:"limit"`
	Offset int         `json:"offset"`
	Total  int         `json:"total"`
}

// ListTyped — извлечённая доменная функция GET /v1/voyages (READ, БЕЗ audit; FULL-TYPED
// ADR-054 §Pattern). enum-валидация kind/status → 422; БД-сбой → 500.
func (h *VoyageHandler) ListTyped(ctx context.Context, in VoyageListInput) (VoyageListReply, error) {
	var zero VoyageListReply
	if h.store == nil {
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "voyage orchestrator is not configured")}
	}
	var filter voyage.ListFilter
	if in.Kind != "" {
		kind := voyage.Kind(in.Kind)
		if !voyage.ValidKind(kind) {
			return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
				"invalid 'kind' filter (must be one of scenario/command)")}
		}
		filter.Kind = kind
	}
	if len(in.Statuses) > 0 {
		filter.Statuses = make([]voyage.Status, 0, len(in.Statuses))
		for _, s := range in.Statuses {
			st := voyage.Status(s)
			if !voyage.ValidStatus(st) {
				return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
					"invalid 'status' filter (scheduled/pending/running/succeeded/failed/partial_failed/cancelled)")}
			}
			filter.Statuses = append(filter.Statuses, st)
		}
	}
	items, total, err := voyage.List(ctx, h.store, filter, in.Page.Offset, in.Page.Limit)
	if err != nil {
		h.logger.Error("voyage.list: select failed", slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "list voyages failed")}
	}
	dtos := make([]voyageDTO, 0, len(items))
	for _, v := range items {
		dtos = append(dtos, toVoyageDTO(v))
	}
	return VoyageListReply{Items: dtos, Offset: in.Page.Offset, Limit: in.Page.Limit, Total: total}, nil
}

// Get — GET /v1/voyages/{id} (detail + summary).
func (h *VoyageHandler) Get(w http.ResponseWriter, r *http.Request) {
	dto, err := h.GetTyped(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeProblemError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, dto, h.logger)
}

// VoyageDTO / VoyageSummaryDTO / VoyageTargetDTO / VoyageTargetEntryDTO — экспортируемые
// alias-ы native handler-DTO (handler-native T5d). Нужны huma-projection (huma_voyage_reply.go
// строит api-native из этих типов) и cadence-runs envelope-alias (PagedResponse[VoyageDTO]).
type (
	VoyageDTO            = voyageDTO
	VoyageSummaryDTO     = voyageSummaryDTO
	VoyageTargetDTO      = voyageTargetDTO
	VoyageTargetEntryDTO = voyageTargetEntryDTO
)

// GetTyped — извлечённая доменная функция GET /v1/voyages/{id} (READ, БЕЗ audit;
// FULL-TYPED ADR-054 §Pattern). 404 not-found, 422 bad id, 500 БД-сбой.
func (h *VoyageHandler) GetTyped(ctx context.Context, id string) (VoyageDTO, error) {
	var zero VoyageDTO
	if h.store == nil {
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "voyage orchestrator is not configured")}
	}
	if !audit.IsValidULID(id) {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
			"path 'id' must be a Crockford-base32 ULID (26 chars)")}
	}
	v, err := voyage.SelectByID(ctx, h.store, id)
	if err != nil {
		if errors.Is(err, voyage.ErrVoyageNotFound) {
			return zero, &problemError{problem.New(problem.TypeNotFound, "", "voyage "+id+" not found")}
		}
		h.logger.Error("voyage.get: select failed", slog.String("voyage_id", id), slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "get voyage failed")}
	}
	return toVoyageDTO(v), nil
}

// Targets — GET /v1/voyages/{id}/targets (All-runs drill).
func (h *VoyageHandler) Targets(w http.ResponseWriter, r *http.Request) {
	reply, err := h.TargetsTyped(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeProblemError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, reply, h.logger)
}

// VoyageTargetsReply — native typed-выход targets (handler-native T5d). Плоская форма
// 1:1 с прежним VoyageTargetsReply (voyage_id + targets[], оба required).
type VoyageTargetsReply struct {
	Targets  []voyageTargetEntryDTO `json:"targets"`
	VoyageID string                 `json:"voyage_id"`
}

// TargetsTyped — извлечённая доменная функция GET /v1/voyages/{id}/targets (READ, БЕЗ
// audit; FULL-TYPED ADR-054 §Pattern). Existence-probe → 404; 422 bad id; 500 БД-сбой.
func (h *VoyageHandler) TargetsTyped(ctx context.Context, id string) (VoyageTargetsReply, error) {
	var zero VoyageTargetsReply
	if h.store == nil {
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "voyage orchestrator is not configured")}
	}
	if !audit.IsValidULID(id) {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
			"path 'id' must be a Crockford-base32 ULID (26 chars)")}
	}
	// Existence-probe: 404 если Voyage не существует (иначе пустой список
	// неотличим от «несуществующего id»).
	if _, err := voyage.SelectByID(ctx, h.store, id); err != nil {
		if errors.Is(err, voyage.ErrVoyageNotFound) {
			return zero, &problemError{problem.New(problem.TypeNotFound, "", "voyage "+id+" not found")}
		}
		h.logger.Error("voyage.targets: probe failed", slog.String("voyage_id", id), slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "get voyage targets failed")}
	}
	targets, err := voyage.SelectTargets(ctx, h.store, id)
	if err != nil {
		h.logger.Error("voyage.targets: select failed", slog.String("voyage_id", id), slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "get voyage targets failed")}
	}
	out := make([]voyageTargetEntryDTO, 0, len(targets))
	for i := range targets {
		t := &targets[i]
		out = append(out, voyageTargetEntryDTO{
			TargetKind: string(t.TargetKind),
			TargetID:   t.TargetID,
			BatchIndex: t.BatchIndex,
			Status:     string(t.Status),
			ApplyID:    t.ApplyID,
			ErrandID:   t.ErrandID,
			FinishedAt: t.FinishedAt,
		})
	}
	return VoyageTargetsReply{VoyageID: id, Targets: out}, nil
}

// --- DELETE /v1/voyages/{id} ---

// Cancel — DELETE /v1/voyages/{id} (ADR-043 §«отложено»: cancel pending/
// scheduled — простой перевод в cancelled; running-cancel — post-MVP, см. ниже).
//
// Контракт:
//   - 202 + {voyage_id, status:"cancelled"} — pending/scheduled-прогон отменён.
//   - 404 — voyage_id не существует.
//   - 409 — running-прогон (`voyage_running_cancel_unsupported`) либо уже
//     терминальный (`voyage_already_terminal`). Сложный abort running-прогона
//     отложен post-MVP (наследует deferred Tide/ErrandRun).
//   - 500 — store не сконфигурирован / БД-сбой.
func (h *VoyageHandler) Cancel(w http.ResponseWriter, r *http.Request) {
	claims, ok := middleware.ClaimsFromContext(r.Context())
	if !ok {
		problem.Write(w, problem.New(problem.TypeInternalError, r.URL.Path, "missing claims"))
		return
	}
	reply, err := h.CancelTyped(r.Context(), claims, chi.URLParam(r, "id"))
	if err != nil {
		writeProblemError(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, reply, h.logger)
}

// VoyageCancelReply — native typed-выход cancel (handler-native T5d). Плоская форма
// 1:1 с прежним VoyageCancelReply (voyage_id + status:cancelled, оба required).
type VoyageCancelReply struct {
	Status   string `json:"status"`
	VoyageID string `json:"voyage_id"`
}

// CancelTyped — извлечённая доменная функция DELETE /v1/voyages/{id} (FULL-TYPED
// ADR-054 §Pattern, батч-2f self-audit): cancel pending/scheduled. RBAC-by-kind
// (ADR-043 §6 fail-closed) ПОСЛЕ загрузки строки (kind виден из БД). self-audit
// scenario_run.cancelled / command_run.cancelled пишется ВНУТРИ функции. *problemError
// при отказе (404 нет, 409 running/terminal, 422 bad id), успех — 202 reply.
func (h *VoyageHandler) CancelTyped(ctx context.Context, claims *jwt.Claims, id string) (VoyageCancelReply, error) {
	var zero VoyageCancelReply
	if h.store == nil {
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "voyage orchestrator is not configured")}
	}
	if !audit.IsValidULID(id) {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
			"path 'id' must be a Crockford-base32 ULID (26 chars)")}
	}
	v, err := voyage.SelectByID(ctx, h.store, id)
	if err != nil {
		if errors.Is(err, voyage.ErrVoyageNotFound) {
			return zero, &problemError{problem.New(problem.TypeNotFound, "", "voyage "+id+" not found")}
		}
		h.logger.Error("voyage.cancel: select failed", slog.String("voyage_id", id), slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "cancel voyage failed")}
	}
	// RBAC-by-kind (ADR-043 §6, fail-closed): cancel — mutating, требует то же
	// право, что create по kind. Проверяем ПОСЛЕ загрузки строки (kind виден
	// только из БД). scenario→incarnation.run, command→errand.run.
	resource, action := "errand", "run"
	if v.Kind == voyage.KindScenario {
		resource, action = "incarnation", "run"
	}
	if h.enforcer != nil {
		if err := h.checkPermissionErr(claims.Subject, resource, action, nil); err != nil {
			return zero, err
		}
	}

	if voyage.IsTerminal(v.Status) {
		return zero, &problemError{problem.New(problem.TypeErrandNotCancellable, "",
			"voyage_already_terminal: status="+string(v.Status))}
	}
	if v.Status == voyage.StatusRunning {
		return zero, &problemError{problem.New(problem.TypeErrandNotCancellable, "",
			"voyage_running_cancel_unsupported: running-Voyage cancel is post-MVP")}
	}

	// pending / scheduled → cancelled (простой перевод; running-abort — post-MVP).
	prev := string(v.Status)
	if err := cancelNonRunningVoyage(ctx, h.store, id); err != nil {
		if errors.Is(err, errVoyageCancelRaceLost) {
			// Между SELECT и UPDATE Voyage подобран VoyageWorker-ом (стал running).
			return zero, &problemError{problem.New(problem.TypeErrandNotCancellable, "",
				"voyage_running_cancel_unsupported: voyage was claimed by a worker")}
		}
		h.logger.Error("voyage.cancel: update failed", slog.String("voyage_id", id), slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "cancel voyage failed")}
	}

	eventType := audit.EventScenarioRunCancelled
	if v.Kind == voyage.KindCommand {
		eventType = audit.EventCommandRunCancelled
	}
	h.emitCancelled(claims.Subject, middleware.ScenarioInvocationSource(ctx), eventType, v, prev)

	return VoyageCancelReply{
		VoyageID: id,
		Status:   string(voyage.StatusCancelled),
	}, nil
}

// --- audit emitters ---

// emitCreated пишет scenario_run.started / command_run.invoked (source=api/mcp,
// archon_aid=JWT.sub). Background-ctx — HTTP-server мог отменить r.Context()
// после write-response. `input` НЕ кладётся (инвариант A ADR-027).
func (h *VoyageHandler) emitCreated(aid string, source audit.Source, eventType audit.EventType, row *voyage.Voyage, target *voyageTargetRequest, scopeSize int) {
	if h.auditW == nil {
		return
	}
	payload := map[string]any{
		"voyage_id":   row.VoyageID,
		"kind":        string(row.Kind),
		"scope_size":  scopeSize,
		"concurrency": derefInt(row.Concurrency),
		"dry_run":     row.DryRun,
		"on_failure":  derefOnFailure(row.OnFailure),
	}
	if row.ScenarioName != nil {
		payload["scenario_name"] = *row.ScenarioName
	}
	if row.Module != nil {
		payload["module"] = *row.Module
	}
	if row.BatchSize != nil {
		payload["batch_size"] = *row.BatchSize
	}
	if t := targetAuditPayload(target); len(t) > 0 {
		payload["target"] = t
	}
	ev := &audit.Event{
		EventType:     eventType,
		Source:        source,
		ArchonAID:     aid,
		CorrelationID: row.VoyageID,
		Payload:       payload,
	}
	if err := h.auditW.Write(context.Background(), ev); err != nil {
		h.logger.Error("voyage.create: audit created write failed",
			slog.String("voyage_id", row.VoyageID), slog.Any("error", err))
	}
}

// emitCancelled пишет scenario_run.cancelled / command_run.cancelled.
func (h *VoyageHandler) emitCancelled(aid string, source audit.Source, eventType audit.EventType, v *voyage.Voyage, prevStatus string) {
	if h.auditW == nil {
		return
	}
	ev := &audit.Event{
		EventType:     eventType,
		Source:        source,
		ArchonAID:     aid,
		CorrelationID: v.VoyageID,
		Payload: map[string]any{
			"voyage_id":       v.VoyageID,
			"kind":            string(v.Kind),
			"previous_status": prevStatus,
		},
	}
	if err := h.auditW.Write(context.Background(), ev); err != nil {
		h.logger.Error("voyage.cancel: audit cancelled write failed",
			slog.String("voyage_id", v.VoyageID), slog.Any("error", err))
	}
}

// targetAuditPayload собирает declarative-target-форму для audit (без чувств.).
func targetAuditPayload(t *voyageTargetRequest) map[string]any {
	if t == nil {
		return nil
	}
	out := map[string]any{}
	if len(t.Incarnations) > 0 {
		out["incarnations"] = t.Incarnations
	}
	if t.Service != "" {
		out["service"] = t.Service
	}
	if len(t.SIDs) > 0 {
		out["sids"] = t.SIDs
	}
	if t.Where != "" {
		out["where"] = t.Where
	}
	if len(t.Coven) > 0 {
		out["coven"] = t.Coven
	}
	return out
}

// --- helpers ---

func derefOnFailure(p *voyage.OnFailure) string {
	if p == nil {
		return ""
	}
	return string(*p)
}

// normalizeVoyageOnFailure: "" → continue (parity ErrandRun default), валидное →
// само, иначе detail-ошибка.
func normalizeVoyageOnFailure(s string) (voyage.OnFailure, string) {
	switch s {
	case "", string(voyage.OnFailureContinue):
		return voyage.OnFailureContinue, ""
	case string(voyage.OnFailureAbort):
		return voyage.OnFailureAbort, ""
	default:
		return "", "field 'on_failure' must be one of {abort, continue}"
	}
}

// applyBatchSpec транслирует строковое поле `batch` (S1) в req.BatchSize /
// req.BatchPercent, переиспользуя весь существующий downstream (window-guard /
// XOR / диапазоны / effectiveBatchSize / max_batch_size). Возвращает detail
// 422-ошибки либо "" (ok / поле не задано).
//
// Семантика:
//   - req.Batch == nil → no-op (старый путь batch_size/batch_percent).
//   - trim(*req.Batch) == "" → «не задано»: весь scope одним Leg (no-op, не 422).
//   - непустой + (batch_size|batch_percent заданы) → конфликт (нельзя оба формата).
//   - "N%" → percent (как batch_percent=N); "N" → hosts (как batch_size=N).
//   - malformed → человекочитаемый detail с показом исходной строки.
//
// Поле уже разобрано через [voyage.ParseBatchSpec] (fail-closed грамматика);
// window-несовместимость и потолок max_batch_size проверяются ниже по общему пути.
func applyBatchSpec(req *voyageCreateRequest) (detail string) {
	if req.Batch == nil {
		return ""
	}
	raw := *req.Batch
	mode, value, err := voyage.ParseBatchSpec(raw)
	if errors.Is(err, voyage.ErrBatchSpecEmpty) {
		// Пустая строка-явно — «не задано», весь scope одним Leg. Не ошибка.
		return ""
	}
	// Конфликт двух форматов отвергаем ДО разбора значения: непустой batch вместе
	// с числовыми batch_size/batch_percent — противоречивый payload.
	if req.BatchSize != nil || req.BatchPercent != nil {
		return "voyage_batch_spec_conflict: field 'batch' is mutually exclusive with 'batch_size'/'batch_percent' (set one format)"
	}
	if err != nil {
		return fmt.Sprintf("field 'batch' must be N or N%% (1-100); got %q", raw)
	}
	switch mode {
	case voyage.BatchSpecPercent:
		req.BatchPercent = &value
	default: // BatchSpecHosts
		req.BatchSize = &value
	}
	return ""
}

// applyMaxFailures транслирует строковое поле `max_failures` (S2) в порог провалов
// req.FailThreshold, переиспользуя fail-closed грамматику [voyage.ParseBatchSpec]
// (`N`|`N%`, та же что у batch). Возвращает detail 422-ошибки либо "" (ok / поле не
// задано). ADR-043 amendment 2026-06-09 §2/§3.
//
// Семантика (симметрия с applyBatchSpec):
//   - req.MaxFailures == nil → no-op (старый путь fail_threshold).
//   - trim(*req.MaxFailures) == "" → «не задано» (no-op, не 422).
//   - непустой + fail_threshold задан → конфликт (нельзя оба формата).
//   - "N" → абсолют: пишет req.FailThreshold = N (ведёт себя как fail_threshold:N).
//   - "N%" → percent: стешит N в req.maxFailuresPercent; абсолютный порог
//     резолвится позже по scope (resolveMaxFailuresPercent после резолва target-а).
//   - malformed → человекочитаемый detail с показом исходной строки.
//
// Грамматика batch разделяет с порогом hosts/percent-семантику: hosts-режим здесь =
// абсолютное число провалов, percent-режим = процент от единиц прогона.
func applyMaxFailures(req *voyageCreateRequest) (detail string) {
	if req.MaxFailures == nil {
		return ""
	}
	raw := *req.MaxFailures
	mode, value, err := voyage.ParseBatchSpec(raw)
	if errors.Is(err, voyage.ErrBatchSpecEmpty) {
		// Пустая строка — «не задано» (no-op, не ошибка ввода).
		return ""
	}
	// Конфликт двух форматов отвергаем ДО разбора значения: непустой max_failures
	// вместе с int-овым fail_threshold — противоречивый payload (тот же error-code,
	// что у batch, ADR-043 amendment 2026-06-09 §3).
	if req.FailThreshold != nil {
		return "voyage_batch_spec_conflict: field 'max_failures' is mutually exclusive with 'fail_threshold' (set one format)"
	}
	if err != nil {
		return fmt.Sprintf("field 'max_failures' must be N or N%% (1-100); got %q", raw)
	}
	switch mode {
	case voyage.BatchSpecPercent:
		req.maxFailuresPercent = &value
	default: // BatchSpecHosts → абсолютное число провалов
		req.FailThreshold = &value
	}
	return ""
}

// resolveMaxFailuresPercent дорезолвивает max_failures="N%" в абсолютный
// req.FailThreshold ПОСЛЕ резолва scope (ADR-043 amendment 2026-06-09 §2): порог
// считается по единицам прогона (инкарнации для scenario / хосты для command — та
// же база scope, что у effectiveBatchSize). ceil(scope*pct/100), clamp [1, scope].
// No-op, если percent не задан (req.maxFailuresPercent == nil) либо scope <= 0.
func resolveMaxFailuresPercent(req *voyageCreateRequest, scope int) {
	if req.maxFailuresPercent == nil || scope <= 0 {
		return
	}
	eff := (scope*(*req.maxFailuresPercent) + 99) / 100 // ceil
	if eff < 1 {
		eff = 1
	}
	if eff > scope {
		eff = scope
	}
	req.FailThreshold = &eff
}

// normalizeVoyageBatchMode: "" → barrier (default, ADR-043 amendment), валидное →
// само, иначе detail-ошибка.
func normalizeVoyageBatchMode(s string) (voyage.BatchMode, string) {
	switch s {
	case "", string(voyage.BatchModeBarrier):
		return voyage.BatchModeBarrier, ""
	case string(voyage.BatchModeWindow):
		return voyage.BatchModeWindow, ""
	default:
		return "", "field 'batch_mode' must be one of {barrier, window}"
	}
}

// voyageBatchIndex — batch_index единицы при Insert-е. barrier → Leg-индекс
// (chunk по batch_size); window → 0 для всех (плоский прогон, нет Leg-ов; ADR-043
// amendment §7).
func voyageBatchIndex(i int, batchSize *int, batchMode voyage.BatchMode) int {
	if batchMode == voyage.BatchModeWindow {
		return 0
	}
	return batchIndexFor(i, batchSize)
}

// batchIndexFor — 0-based индекс Leg-а для i-й единицы (chunk по batch_size).
// batch_size nil/<=0 → весь прогон один Leg (batch_index=0), parity
// chunkIncarnations/chunkSIDs воркера.
func batchIndexFor(i int, batchSize *int) int {
	if batchSize == nil || *batchSize <= 0 {
		return 0
	}
	return i / *batchSize
}

// voyageTotalBatches — total_batches при Insert-е. barrier → число Leg-ов; window
// → 1 (плоский прогон одной волной-окном, batch_index=0 у всех единиц).
func voyageTotalBatches(n int, batchSize *int, batchMode voyage.BatchMode) int {
	if n == 0 {
		return 0
	}
	if batchMode == voyage.BatchModeWindow {
		return 1
	}
	return totalBatches(n, batchSize)
}

// totalBatches — число Leg-ов для n единиц при заданном batch_size.
func totalBatches(n int, batchSize *int) int {
	if n == 0 {
		return 0
	}
	if batchSize == nil || *batchSize <= 0 {
		return 1
	}
	bs := *batchSize
	return (n + bs - 1) / bs
}

// incarnationCovenLabelValid — формат env-тега incarnation совпадает с
// coven-меткой хоста (ADR-008 amendment a использует тот же предикат).
func incarnationCovenLabelValid(label string) bool { return soul.ValidCoven(label) }

// errVoyageCancelRaceLost — CAS-cancel вернул 0 строк: между SELECT и UPDATE
// Voyage подобран VoyageWorker-ом (pending/scheduled → running). Caller трактует
// как 409 «running-cancel unsupported».
var errVoyageCancelRaceLost = errors.New("voyage: cancel race lost (claimed by worker)")

// cancelNonRunningVoyageSQL — CAS-перевод pending/scheduled → cancelled.
// WHERE сужено до non-running-статусов (idempotent + race-safe): если Voyage
// успел стать running между SELECT и UPDATE, 0 строк → errVoyageCancelRaceLost.
// finished_at = NOW() (CHECK voyages_terminal_finished_at: cancelled — terminal).
const cancelNonRunningVoyageSQL = `
UPDATE voyages
SET status      = 'cancelled',
    finished_at = NOW()
WHERE voyage_id = $1
  AND status IN ('pending', 'scheduled')
`

// cancelNonRunningVoyage переводит pending/scheduled Voyage в cancelled. 0 строк
// (Voyage стал running / уже terminal — последнее caller отсёк probe-ом) →
// errVoyageCancelRaceLost. running-abort — post-MVP (наследует deferred
// Tide/ErrandRun).
func cancelNonRunningVoyage(ctx context.Context, db voyage.ExecQueryRower, id string) error {
	tag, err := db.Exec(ctx, cancelNonRunningVoyageSQL, id)
	if err != nil {
		return fmt.Errorf("voyage: cancel: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return errVoyageCancelRaceLost
	}
	return nil
}
