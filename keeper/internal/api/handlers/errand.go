package handlers

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	"github.com/souls-guild/soul-stack/keeper/internal/errand"
	keeperjwt "github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/soul"
	"github.com/souls-guild/soul-stack/shared/api"
)

// ErrandHandler — endpoints pull-ad-hoc Errand-ов (ADR-033):
//
//	POST /v1/souls/{sid}/exec       — запуск Errand (sync 200 / async 202)
//	GET  /v1/errands/{errand_id}    — состояние Errand-а (poll)
//	GET  /v1/errands?sid=&status=…  — список Errand-ов (RBAC-фильтр)
//
// dispatcher / store обязательны при non-nil handler-е; nil-handler роутом
// не подключается (см. router.go, паттерн PushHandler/OracleHandler).
//
// T5d-2c-full (handler-native): домен errand отвязан от legacy-генерата. *Typed-функции
// принимают/возвращают NATIVE типы с плоскими wire-полями (ErrandRunInput /
// ErrandResultView / ErrandListPage / ErrandAcceptedView); native wire-DTO (схему
// OpenAPI) строит пакет api из этих полей (register-func huma_errand.go /
// huma_soul.go). (w,r)-оболочки сняты: HTTP обслуживает huma full-typed, MCP зовёт
// errand.Dispatcher/Store напрямую (мимо handler).
type ErrandHandler struct {
	dispatcher *errand.Dispatcher
	store      *errand.Store
	logger     *slog.Logger
}

// NewErrandHandler конструирует handler. dispatcher/store обязательны
// для production-вызовов; в drift-тесте/unit nil допустим только если
// роуты не дёргают handler.
func NewErrandHandler(dispatcher *errand.Dispatcher, store *errand.Store, logger *slog.Logger) *ErrandHandler {
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	return &ErrandHandler{dispatcher: dispatcher, store: store, logger: logger}
}

// ErrandSpecStub — непустой *ErrandHandler-заглушка для генерации huma-OpenAPI-
// фрагмента (HumaErrandSpecYAML): при dump доменный handler не вызывается, но
// huma.Register требует non-nil для no-op-проверки на nil. dispatcher/store nil —
// handler никогда не исполняется в spec-режиме (parity [AugurSpecStub]).
func ErrandSpecStub() *ErrandHandler {
	return &ErrandHandler{logger: slog.New(slog.NewJSONHandler(io.Discard, nil))}
}

// ErrandRunInput — NATIVE request-форма POST /v1/souls/{sid}/exec (handler-native
// T5d-2c-full). Заменяет ErrandRunRequest: huma-input SOUL-домена (huma_soul.go)
// биндит/валидирует тело и проецирует его в эти поля перед вызовом ExecTyped.
// Опциональные поля — указатели (Input/TimeoutSeconds/DryRun), handler разыменовывает
// их в errand.DispatchRequest.
type ErrandRunInput struct {
	Module         string
	Input          *map[string]any
	TimeoutSeconds *int
	DryRun         *bool
}

// ErrandResultView — ПЛОСКАЯ wire-форма результата Errand-а (200-тело errand-get-
// терминал / sync-exec / element list-а), handler-native (заменяет ErrandResult).
// Опц. поля — указатели С nil-при-пустом (паритет omitempty прежней формы); status —
// плоская строка домен-статуса; started_at/finished_at — UTC+Truncate(Second) (byte-exact
// с прежним секундным RFC3339). Пакет api проецирует ErrandResultView → native-схему
// ErrandResult (register-func huma_errand.go), порядок полей wire фиксирует native-тип.
type ErrandResultView struct {
	DurationMs      *int64
	ErrandID        string
	ErrorMessage    *string
	ExitCode        *int32
	FinishedAt      *time.Time
	Module          string
	Output          *map[string]interface{}
	SID             string
	StartedAt       time.Time
	StartedByAID    string
	Status          string
	Stderr          *string
	StderrTruncated *bool
	Stdout          *string
	StdoutTruncated *bool
}

// ErrandListPage — доменный paged-результат GET /v1/errands (handler-native). Плоские
// offset/limit/total + срез ErrandResultView; пакет api проецирует его в native-envelope
// ErrandListReply (register-func huma_errand.go).
type ErrandListPage struct {
	Items  []ErrandResultView
	Offset int
	Limit  int
	Total  int
}

// ErrandAcceptedView — плоское 202-тело async-эскалации (sync-cap превышен) /
// errand-get-running. errand_id + строковый status (домен-статус errand.Status). На wire
// сериализуется register-func-ом (api проецирует в native ErrandAccepted byte-exact).
type ErrandAcceptedView struct {
	ErrandID string
	Status   string
}

// newErrandAcceptedView строит плоское 202-тело из errand_id + домен-статуса.
func newErrandAcceptedView(errandID string, status errand.Status) ErrandAcceptedView {
	return ErrandAcceptedView{ErrandID: errandID, Status: string(status)}
}

// ErrandExecReply — извлечённый результат [ErrandHandler.ExecTyped] (handler-native
// T5d-2c-full). Async=true → 202-тело Accepted + Location-header; иначе 200-тело Result
// (терминал получен до server-cap). Несёт audit-поля (event errand.invoked) — дублируются
// для security navigation-trail middleware-я (dispatcher сам пишет audit-event source=api
// внутри Dispatch).
type ErrandExecReply struct {
	Async    bool
	ErrandID string
	Result   ErrandResultView
	Accepted ErrandAcceptedView
	// audit-поля (parity легаси SetAuditPayload).
	sid        string
	module     string
	timeoutSec int
	dryRun     bool
}

// AuditPayload собирает audit-поля errand.invoked-роута (parity легаси:
// sid/module/errand_id/timeout_seconds/dry_run). Источник для huma-варианта B.
func (r ErrandExecReply) AuditPayload() middleware.AuditPayload {
	return middleware.AuditPayload{
		"sid":             r.sid,
		"module":          r.module,
		"errand_id":       r.ErrandID,
		"timeout_seconds": r.timeoutSec,
		"dry_run":         r.dryRun,
	}
}

// ExecTyped — доменная функция POST /v1/souls/{sid}/exec (handler-native): валидация
// SID (soul.ValidSID→422) + dispatcher.Dispatch + sentinel→problem. req — native
// request-форма (SOUL-домен huma_soul.go биндит/валидирует тело и проецирует в неё; huma
// отбивает unknown → 400 до вызова). Ошибки — *problemError (422 невалидный sid / пустой
// module / timeout вне диапазона; 404 soul-not-connected; 400 dry_run verb-модуль; 500
// dispatcher nil / БД-сбой); успех — [ErrandExecReply] (200 sync / 202 async).
func (h *ErrandHandler) ExecTyped(ctx context.Context, claims *keeperjwt.Claims, sid string, req ErrandRunInput) (ErrandExecReply, error) {
	var zero ErrandExecReply
	if h.dispatcher == nil {
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "errand dispatcher is not configured")}
	}
	if !soul.ValidSID(sid) {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", "path 'sid' must match "+soul.SIDPattern)}
	}

	var input map[string]any
	if req.Input != nil {
		input = *req.Input
	}
	var timeoutSec int
	if req.TimeoutSeconds != nil {
		timeoutSec = *req.TimeoutSeconds
	}
	dryRun := req.DryRun != nil && *req.DryRun

	res, err := h.dispatcher.Dispatch(ctx, errand.DispatchRequest{
		SID:          sid,
		Module:       req.Module,
		Input:        input,
		TimeoutSec:   timeoutSec,
		DryRun:       dryRun,
		StartedByAID: claims.Subject,
	})
	if err != nil {
		return zero, h.dispatchError(err)
	}

	reply := ErrandExecReply{
		Async:      res.Async,
		ErrandID:   res.ErrandID,
		sid:        sid,
		module:     req.Module,
		timeoutSec: timeoutSec,
		dryRun:     dryRun,
	}
	if res.Async {
		reply.Accepted = newErrandAcceptedView(res.ErrandID, res.Status)
	} else {
		reply.Result = dispatchResultView(&res, sid, req.Module, claims.Subject)
	}
	return reply, nil
}

// ErrandGetReply — извлечённый результат [ErrandHandler.GetTyped] (handler-native).
// Running=true → 202-тело Accepted (Errand ещё бежит, async-poll); иначе 200-тело
// Result (терминал). Один из двух полей значим по флагу Running.
type ErrandGetReply struct {
	Running  bool
	Accepted ErrandAcceptedView
	Result   ErrandResultView
}

// GetTyped — доменная функция GET /v1/errands/{errand_id} (READ-with-path, БЕЗ audit):
// валидация path-id + store.Get + sentinel→problem. running → 202-Accepted, терминал →
// 200-Result. Ошибки — *problemError (404/422/500); store nil → 500.
func (h *ErrandHandler) GetTyped(ctx context.Context, errandID string) (ErrandGetReply, error) {
	var zero ErrandGetReply
	if h.store == nil {
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "errand store is not configured")}
	}
	if errandID == "" {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", "path 'errand_id' is required")}
	}
	row, err := h.store.Get(ctx, errandID)
	if err != nil {
		if errors.Is(err, errand.ErrNotFound) {
			return zero, &problemError{problem.New(problem.TypeNotFound, "", "errand "+errandID+" not found")}
		}
		h.logger.Error("errand.get: store failed",
			slog.String("errand_id", errandID), slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "get errand failed")}
	}
	if row.Status == errand.StatusRunning {
		return ErrandGetReply{Running: true, Accepted: newErrandAcceptedView(row.ErrandID, row.Status)}, nil
	}
	return ErrandGetReply{Result: rowToErrandResultView(row)}, nil
}

// ErrandListInput — провалидированный вход GET /v1/errands. sid/status —
// string-фильтры (валидация формата/enum в ListTyped → 422); StartedAfter —
// time.Time (нулевое = без фильтра, bad value уже отбит на bind-фазе → 400);
// Modules — multi-value exact-match OR; Offset/Limit — пагинация.
type ErrandListInput struct {
	SID          string
	Status       string
	StartedAfter time.Time
	Modules      []string
	Offset       int
	Limit        int
}

// ListTyped — доменная функция GET /v1/errands (READ-with-typed-query, БЕЗ audit).
// offset/limit провалидированы huma-bind; диапазон enforce-ит CheckPageBounds → 400
// (parity ParsePage). sid формат / status enum → 422. StartedAfter — bad value уже отбит
// huma-bind date-time (400). Ошибка чтения → *problemError (500); store nil → 500.
func (h *ErrandHandler) ListTyped(ctx context.Context, in ErrandListInput) (ErrandListPage, error) {
	var zero ErrandListPage
	if h.store == nil {
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "errand store is not configured")}
	}
	if err := api.CheckPageBounds(in.Offset, in.Limit); err != nil {
		return zero, &problemError{problem.New(problem.TypeMalformedRequest, "", err.Error())}
	}

	var filter errand.ListFilter
	if in.SID != "" {
		if !soul.ValidSID(in.SID) {
			return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
				"query 'sid' must match "+soul.SIDPattern)}
		}
		filter.SID = in.SID
	}
	if in.Status != "" {
		if !validErrandStatus(in.Status) {
			return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
				"query 'status' must be one of running/success/failed/timed_out/cancelled/module_not_allowed")}
		}
		filter.Status = errand.Status(in.Status)
	}
	filter.StartedAfter = in.StartedAfter
	if len(in.Modules) > 0 {
		// Exact-match OR, без regex/glob (ТЗ). Дубликаты допустимы — пройдут в
		// IN-предикат как есть, PG нормализует.
		filter.Modules = in.Modules
	}

	rows, total, err := h.store.List(ctx, filter, in.Offset, in.Limit)
	if err != nil {
		h.logger.Error("errand.list: store failed",
			slog.Any("filter", filter), slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "list errands failed")}
	}

	items := make([]ErrandResultView, 0, len(rows))
	for _, r := range rows {
		items = append(items, rowToErrandResultView(r))
	}
	return ErrandListPage{
		Items:  items,
		Offset: in.Offset,
		Limit:  in.Limit,
		Total:  total,
	}, nil
}

// ErrandCancelReply — извлечённый результат [ErrandHandler.CancelTyped] (handler-native).
// Несёт audit-поля (HTTP-ответ — пустое 204-тело).
type ErrandCancelReply struct {
	ErrandID string
}

// AuditPayload собирает audit-payload errand.cancel-роута (parity легаси: errand_id).
// dispatcher уже пишет audit-event source=api сам — payload дублируется для security-
// navigation-trail middleware-я. Источник для huma-варианта B.
func (r ErrandCancelReply) AuditPayload() middleware.AuditPayload {
	return middleware.AuditPayload{"errand_id": r.ErrandID}
}

// CancelTyped — доменная функция DELETE /v1/errands/{errand_id} (handler-native):
// валидация path-id + dispatcher.Cancel + sentinel→problem. Ошибки — *problemError;
// успех — [ErrandCancelReply]. dispatcher nil → 500.
func (h *ErrandHandler) CancelTyped(ctx context.Context, claims *keeperjwt.Claims, errandID string) (ErrandCancelReply, error) {
	var zero ErrandCancelReply
	if h.dispatcher == nil {
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "errand dispatcher is not configured")}
	}
	if errandID == "" {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", "path 'errand_id' is required")}
	}
	if err := h.dispatcher.Cancel(ctx, errand.CancelRequest{
		ErrandID:    errandID,
		RequestedBy: claims.Subject,
	}); err != nil {
		return zero, h.cancelError(errandID, err)
	}
	return ErrandCancelReply{ErrandID: errandID}, nil
}

// cancelError маппит sentinel-ы errand.Dispatcher.Cancel в *problemError (доставляются
// huma-обёрткой через AsProblemDetails).
func (h *ErrandHandler) cancelError(errandID string, err error) error {
	switch {
	case errors.Is(err, errand.ErrEmptyErrandID):
		return &problemError{problem.New(problem.TypeValidationFailed, "", "path 'errand_id' is required")}
	case errors.Is(err, errand.ErrNotFound):
		return &problemError{problem.New(problem.TypeNotFound, "", "errand "+errandID+" not found")}
	case errors.Is(err, errand.ErrErrandTerminal):
		return &problemError{problem.New(problem.TypeErrandNotCancellable, "",
			"errand "+errandID+" is already in a terminal state")}
	case errors.Is(err, errand.ErrSoulNotConnected):
		return &problemError{problem.New(problem.TypeNotFound, "", "target soul is not connected to the cluster")}
	default:
		h.logger.Error("errand.cancel: dispatcher failed",
			slog.String("errand_id", errandID), slog.Any("error", err))
		return &problemError{problem.New(problem.TypeInternalError, "", "errand cancel failed")}
	}
}

// rowToErrandResultView проецирует [errand.Row] в плоский [ErrandResultView]
// (GET /v1/errands/{id} терминал + элемент list-а).
//
// date-time: прежний wire был секундным (`.Format(time.RFC3339)`), поэтому
// `.UTC().Truncate(time.Second)` сохраняет байт-в-байт. Опциональные поля
// (stdout/stderr/error_message — string omitempty; truncated-флаги — bool omitempty
// в спеке) проецируются через nil-при-пустом-значении (паритет с прежним omitempty).
func rowToErrandResultView(row *errand.Row) ErrandResultView {
	res := ErrandResultView{
		ErrandID:        row.ErrandID,
		SID:             row.SID,
		Module:          row.Module,
		Status:          string(row.Status),
		ExitCode:        row.ExitCode,
		Stdout:          ptrIfNotEmpty(row.Stdout),
		Stderr:          ptrIfNotEmpty(row.Stderr),
		StdoutTruncated: ptrBoolIfTrue(row.StdoutTruncated),
		StderrTruncated: ptrBoolIfTrue(row.StderrTruncated),
		DurationMs:      row.DurationMs,
		ErrorMessage:    ptrIfNotEmpty(row.ErrorMessage),
		Output:          ptrMapIfNotEmpty(row.Output),
		StartedByAID:    row.StartedByAID,
		StartedAt:       row.StartedAt.UTC().Truncate(time.Second),
	}
	if row.FinishedAt != nil {
		fin := row.FinishedAt.UTC().Truncate(time.Second)
		res.FinishedAt = &fin
	}
	return res
}

// dispatchResultView строит sync-200-body POST /v1/souls/{sid}/exec из
// [errand.DispatchResult] (терминал получен до server-cap). sid/module/aid
// приходят из request-а — DispatchResult их не несёт. StartedAt — реальный
// момент INSERT-а errands-строки (DispatchResult.StartedAt, тот же Clock()-now,
// что персистирован в Row.StartedAt), а не время формирования ответа. Так
// sync-`started_at` совпадает с тем, что вернёт последующий GET /v1/errands/{id}.
func dispatchResultView(res *errand.DispatchResult, sid, module, startedByAID string) ErrandResultView {
	return ErrandResultView{
		ErrandID:        res.ErrandID,
		SID:             sid,
		Module:          module,
		Status:          string(res.Status),
		ExitCode:        res.ExitCode,
		Stdout:          ptrIfNotEmpty(res.Stdout),
		Stderr:          ptrIfNotEmpty(res.Stderr),
		StdoutTruncated: ptrBoolIfTrue(res.StdoutTruncated),
		StderrTruncated: ptrBoolIfTrue(res.StderrTruncated),
		DurationMs:      res.DurationMs,
		ErrorMessage:    ptrIfNotEmpty(res.ErrorMessage),
		Output:          ptrMapIfNotEmpty(res.Output),
		StartedByAID:    startedByAID,
		StartedAt:       res.StartedAt.UTC().Truncate(time.Second),
	}
}

// ptrBoolIfTrue возвращает nil для false (паритет с omitempty над bool — поле
// опускается), иначе указатель на true. truncated-флаги в спеке optional.
func ptrBoolIfTrue(b bool) *bool {
	if !b {
		return nil
	}
	return &b
}

// dispatchError маппит sentinel-ошибки dispatcher.Dispatch в *problemError
// (доставляются huma-обёрткой через AsProblemDetails). Path в problem.Details
// пустой — заполняется на выводе.
func (h *ErrandHandler) dispatchError(err error) error {
	switch {
	case errors.Is(err, errand.ErrSIDEmpty):
		return &problemError{problem.New(problem.TypeValidationFailed, "", "sid is empty")}
	case errors.Is(err, errand.ErrModuleEmpty):
		return &problemError{problem.New(problem.TypeValidationFailed, "", "field 'module' is required")}
	case errors.Is(err, errand.ErrTimeoutOutOfRange):
		return &problemError{problem.New(problem.TypeValidationFailed, "",
			"field 'timeout_seconds' must be in ["+strconv.Itoa(errand.MinTimeoutSeconds)+", "+strconv.Itoa(errand.MaxTimeoutSeconds)+"]")}
	case errors.Is(err, errand.ErrSoulNotConnected):
		return &problemError{problem.New(problem.TypeNotFound, "", "target soul is not connected to the cluster")}
	default:
		h.logger.Error("errand.exec: dispatcher failed", slog.Any("error", err))
		return &problemError{problem.New(problem.TypeInternalError, "", "errand dispatch failed")}
	}
}

// validErrandStatus — closed enum для query-фильтра. Совпадает со
// списком CHECK errands_status_valid + StatusXxx-константами errand-
// пакета. Один источник правды (валидируем сразу при парсинге query).
func validErrandStatus(s string) bool {
	switch errand.Status(s) {
	case errand.StatusRunning, errand.StatusSuccess, errand.StatusFailed,
		errand.StatusTimedOut, errand.StatusCancelled, errand.StatusModuleNotAllowed:
		return true
	}
	return false
}

// ErrandSIDSelector — middleware-helper для RBAC: извлекает SID из
// path-параметра `/v1/souls/{sid}/exec` для permission-проверки
// (rbac.md §Errand → селекторы `host=<sid>`).
//
// Симметрично SoulSIDSelector — отдельный helper, чтобы router.go не
// зависел от внутренних деталей errand-пакета.
func ErrandSIDSelector(r *http.Request) map[string]string {
	sid := chi.URLParam(r, "sid")
	if sid == "" {
		return nil
	}
	return map[string]string{"host": sid}
}
