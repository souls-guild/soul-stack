package handlers

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/api/middleware"
	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/pushorch"
	sharedapi "github.com/souls-guild/soul-stack/shared/api"
)

// pushApplier — узкая поверхность orchestrator-а для ApplyTyped: единственный
// метод Apply (accept push-прогон → apply_id). Сужение нужно ради S6-теста
// happy-path: `*pushorch.PushRun` держит реальный `*Store` на PG, поэтому
// happy-path Apply через конкретную структуру требует testcontainers. Этот
// интерфейс позволяет внедрить fake-orchestrator со статичным apply_id и
// доказать 202→audit-путь (carrier→middleware) unit-level. Прод по-прежнему
// передаёт `*pushorch.PushRun` (удовлетворяет интерфейс автоматически) — wire
// и MCP push не меняются.
type pushApplier interface {
	Apply(ctx context.Context, req pushorch.ApplyRequest) (applyID string, err error)
}

// PushHandler — endpoints push-прогона Destiny по SSH (`POST /v1/push/apply` +
// `GET /v1/push/{apply_id}`, ADR-004 push-flow + Variant C orchestrator
// docs/keeper/push.md). svc — `*pushorch.PushRun` (опц. поле api.Deps): при
// nil роуты не подключаются router-ом (паттерн SigilSvc/AugurSvc). Вся бизнес-
// логика — в pushorch. applier — узкая Apply-поверхность (== svc в проде; fake в
// S6-тесте); read-пути (GetTyped/ListRunsTyped) ходят в svc напрямую.
//
// T5d-2c-full (handler-native): домен push отвязан от legacy-генерата. *Typed-функции
// принимают/возвращают NATIVE типы с плоскими wire-полями (PushApplyInput /
// PushApplyResultView / PushRunListPage); native wire-DTO (схему OpenAPI) строит
// пакет api из этих полей (register-func huma_push.go). HTTP обслуживает huma
// full-typed, MCP зовёт pushorch.PushRun напрямую (мимо handler).
type PushHandler struct {
	svc     *pushorch.PushRun
	applier pushApplier
	logger  *slog.Logger
}

// NewPushHandler конструирует handler. svc nil → caller (api.NewServer) сам
// решает не подключать push-роуты (см. router.go), здесь nil допустим только
// для unit-тестов конструктора. applier == svc (orchestrator реализует Apply);
// при nil-svc applier остаётся nil → ApplyTyped отдаёт 500 «not configured».
func NewPushHandler(svc *pushorch.PushRun, logger *slog.Logger) *PushHandler {
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	h := &PushHandler{svc: svc, logger: logger}
	if svc != nil {
		h.applier = svc
	}
	return h
}

// NewPushHandlerWithApplier — test-only конструктор: внедряет fake-orchestrator
// в Apply-путь (S6 RecordsOnSuccess happy-path без PG). svc остаётся nil →
// read-пути недоступны, но Apply идёт через applier. НЕ использовать в проде.
func NewPushHandlerWithApplier(applier pushApplier, logger *slog.Logger) *PushHandler {
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	return &PushHandler{applier: applier, logger: logger}
}

// PushSpecStub — непустой *PushHandler-заглушка для генерации huma-OpenAPI-фрагмента
// (HumaPushSpecYAML): при dump доменный handler не вызывается, но huma.Register
// требует non-nil для nil-проверки (parity [RoleSpecStub]). svc nil — handler в
// spec-режиме не исполняется.
func PushSpecStub() *PushHandler {
	return &PushHandler{logger: slog.New(slog.NewJSONHandler(io.Discard, nil))}
}

// PushApplyInput — NATIVE request-форма POST /v1/push/apply (handler-native T5d-2c-full).
// Заменяет PushApplyRequest: huma-input (пакет api) биндит/валидирует тело и проецирует
// его в эти поля перед вызовом ApplyTyped. Опциональные поля — указатели (Input/SSHProvider/
// CleanupStaleVersions), handler разыменовывает их в pushorch.ApplyRequest.
type PushApplyInput struct {
	Inventory            []string
	Destiny              string
	Input                *map[string]any
	SSHProvider          *string
	CleanupStaleVersions *bool
}

// PushApplyReply — извлечённый результат [PushHandler.ApplyTyped] (handler-native).
// Несёт apply_id (202-тело строит из него native api.PushApplyReply) + audit-payload
// (выставляется middleware варианта B). Apply — async: 202 Accepted, дальше клиент
// опрашивает GET по apply_id.
type PushApplyReply struct {
	// audit-поля + 202-тело (parity легаси SetAuditPayload; SID-ы целиком НЕ кладутся).
	ApplyID       string
	Destiny       string
	InventorySize int
	SSHProvider   string
	CleanupStale  bool
}

// AuditPayload собирает audit-поля 202-Apply (parity легаси).
func (r PushApplyReply) AuditPayload() middleware.AuditPayload {
	return middleware.AuditPayload{
		"apply_id":       r.ApplyID,
		"destiny":        r.Destiny,
		"inventory_size": r.InventorySize,
		"ssh_provider":   r.SSHProvider,
		"cleanup_stale":  r.CleanupStale,
	}
}

// ApplyTyped — доменная функция POST /v1/push/apply (handler-native): orchestrator-call
// без http-границы. claims/req — аргументы; req — native request-форма (huma пакет api
// биндит/валидирует тело и проецирует в неё; huma отбивает unknown → 400 до вызова).
// Ошибки — *problemError (422 пустой inventory / битый destiny-ref; 500 svc nil / PG-сбой);
// успех — [PushApplyReply] (apply_id + audit-поля).
func (h *PushHandler) ApplyTyped(ctx context.Context, claims *jwt.Claims, req PushApplyInput) (PushApplyReply, error) {
	var zero PushApplyReply
	if h.applier == nil {
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "push orchestrator is not configured")}
	}
	if len(req.Inventory) == 0 {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
			"field 'inventory' is required and must be non-empty")}
	}
	if req.Destiny == "" {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", "field 'destiny' is required")}
	}

	var sshProvider string
	if req.SSHProvider != nil {
		sshProvider = *req.SSHProvider
	}
	var input map[string]any
	if req.Input != nil {
		input = *req.Input
	}
	cleanupStale := req.CleanupStaleVersions != nil && *req.CleanupStaleVersions

	applyID, err := h.applier.Apply(ctx, pushorch.ApplyRequest{
		InventorySIDs: req.Inventory,
		DestinyRef:    req.Destiny,
		SSHProvider:   sshProvider,
		Input:         input,
		CleanupStale:  cleanupStale,
		StartedByAID:  claims.Subject,
	})
	if err != nil {
		if errors.Is(err, pushorch.ErrInvalidDestinyRef) {
			return zero, &problemError{problem.New(problem.TypeValidationFailed, "", err.Error())}
		}
		h.logger.Error("push.apply: orchestrator accept failed",
			slog.String("destiny", req.Destiny),
			slog.String("by_aid", claims.Subject),
			slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "push apply failed")}
	}

	return PushApplyReply{
		ApplyID:       applyID,
		Destiny:       req.Destiny,
		InventorySize: len(req.Inventory),
		SSHProvider:   sshProvider,
		CleanupStale:  cleanupStale,
	}, nil
}

// GetTyped — доменная функция GET /v1/push/{apply_id}. Ошибки — *problemError (422 пустой
// id / 404 нет записи / 500 svc nil / PG-сбой); успех — [PushApplyResultView].
func (h *PushHandler) GetTyped(ctx context.Context, applyID string) (PushApplyResultView, error) {
	var zero PushApplyResultView
	if h.svc == nil {
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "push orchestrator is not configured")}
	}
	if applyID == "" {
		return zero, &problemError{problem.New(problem.TypeValidationFailed, "", "path parameter 'apply_id' is required")}
	}
	row, err := h.svc.GetRow(ctx, applyID)
	if err != nil {
		if errors.Is(err, pushorch.ErrNotFound) {
			return zero, &problemError{problem.New(problem.TypeNotFound, "", "push run "+applyID+" not found")}
		}
		h.logger.Error("push.get: orchestrator read failed", slog.String("apply_id", applyID), slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "push get failed")}
	}
	return rowToPushApplyResultView(row), nil
}

// ListRunsTyped — доменная функция GET /v1/push-runs (handler-native). Фильтры status[]
// (multi-value OR) / ssh_provider (exact) приходят аргументами; offset/limit диапазон
// enforce-ит CheckPageBounds → 400 (НЕ huma min/max — parity легаси ParsePage). Ошибки —
// *problemError (400 out-of-range / 422 невалидный status / 500 svc nil / PG-сбой); успех —
// [PushRunListPage].
func (h *PushHandler) ListRunsTyped(ctx context.Context, statuses []string, sshProvider string, offset, limit int) (PushRunListPage, error) {
	var zero PushRunListPage
	if err := sharedapi.CheckPageBounds(offset, limit); err != nil {
		return zero, &problemError{problem.New(problem.TypeMalformedRequest, "", err.Error())}
	}

	var filter pushorch.ListFilter
	if len(statuses) > 0 {
		filter.Statuses = make([]pushorch.PushRunStatus, 0, len(statuses))
		for _, s := range statuses {
			st := pushorch.PushRunStatus(s)
			if !pushorch.ValidStatus(st) {
				return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
					"invalid 'status' filter: must be one of pending/running/success/partial_failed/failed/cancelled")}
			}
			filter.Statuses = append(filter.Statuses, st)
		}
	}
	if sshProvider != "" {
		filter.SSHProvider = sshProvider
	}

	// nil-check ПОСЛЕ валидации (детерминированный 400/422 независимо от svc).
	if h.svc == nil {
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "push orchestrator is not configured")}
	}

	rows, total, err := h.svc.ListRows(ctx, filter, offset, limit)
	if err != nil {
		h.logger.Error("push.list: orchestrator read failed", slog.Any("filter", filter), slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "list push runs failed")}
	}
	items := make([]PushRunListEntryView, 0, len(rows))
	for _, row := range rows {
		items = append(items, rowToPushRunListEntryView(row))
	}
	return PushRunListPage{Items: items, Offset: offset, Limit: limit, Total: total}, nil
}

// PushApplyResultView — ПЛОСКАЯ wire-форма GET /v1/push/{apply_id} (handler-native,
// заменяет PushApplyView). Зеркало push_runs-строки + summary как opaque jsonb.
// Опц. поля (ssh_provider/input/started_by_aid/summary/finished_at) — nil при пустом;
// status — плоская строка домен-статуса; started_at/finished_at — UTC+Truncate(Second)
// (byte-exact с прежним секундным RFC3339). Пакет api проецирует её в native PushApplyView
// (register-func huma_push.go), порядок полей wire фиксирует native-тип.
type PushApplyResultView struct {
	ApplyID       string
	CleanupStale  bool
	DestinyRef    string
	FinishedAt    *time.Time
	Input         *map[string]interface{}
	InventorySids []string
	SSHProvider   *string
	StartedAt     time.Time
	StartedByAID  *string
	Status        string
	Summary       *map[string]interface{}
}

// PushSummaryCountsView — ПЛОСКИЙ агрегат counts (PushRunListEntryView.SummaryCounts).
// Все поля — `*int` (nil → ключ опущен). Проецируется в native PushSummaryCounts.
type PushSummaryCountsView struct {
	FailCount    *int
	SuccessCount *int
	Total        *int
}

// PushRunListEntryView — ПЛОСКАЯ compact-строка push_runs (element PushRunListPage.Items),
// handler-native (заменяет PushRunListEntry). Compact-форма: вырезаны тяжёлые поля
// (`input` / `summary.hosts[]`), summary редуцирован до агрегированных summary_counts.
type PushRunListEntryView struct {
	ApplyID       string
	CleanupStale  bool
	DestinyRef    string
	FinishedAt    *time.Time
	InventorySids []string
	SSHProvider   *string
	StartedAt     time.Time
	StartedByAID  *string
	Status        string
	SummaryCounts *PushSummaryCountsView
}

// PushRunListPage — доменный paged-результат GET /v1/push-runs (handler-native). Плоские
// offset/limit/total + срез PushRunListEntryView; пакет api проецирует его в native-envelope
// PushRunListReply (register-func huma_push.go).
type PushRunListPage struct {
	Items  []PushRunListEntryView
	Offset int
	Limit  int
	Total  int
}

// rowToPushApplyResultView проецирует [pushorch.PushRunRow] в плоский view
// GET /v1/push/{apply_id}. date-time: прежний wire был секундным (RFC3339 c усечением
// до секунд), поэтому Truncate(Second) сохраняет байт-в-байт. Опциональные поля
// (ssh_provider/input/started_by_aid/summary) — nil при пустом значении (паритет с
// прежним omitempty).
func rowToPushApplyResultView(row *pushorch.PushRunRow) PushApplyResultView {
	view := PushApplyResultView{
		ApplyID:       row.ApplyID,
		InventorySids: row.InventorySIDs,
		DestinyRef:    row.DestinyRef,
		CleanupStale:  row.CleanupStale,
		Status:        string(row.Status),
		StartedAt:     row.StartedAt.UTC().Truncate(time.Second),
		SSHProvider:   ptrIfNotEmpty(row.SSHProvider),
		StartedByAID:  ptrIfNotEmpty(row.StartedByAID),
		Input:         ptrMapIfNotEmpty(row.Input),
		Summary:       ptrMapIfNotEmpty(row.Summary),
	}
	if row.FinishedAt != nil {
		fin := row.FinishedAt.UTC().Truncate(time.Second)
		view.FinishedAt = &fin
	}
	return view
}

// ptrIfNotEmpty возвращает nil для пустой строки (паритет с json omitempty над
// string), иначе указатель на значение.
func ptrIfNotEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// ptrMapIfNotEmpty возвращает nil для пустой/nil-карты (паритет с прежним
// omitempty над map[string]any), иначе указатель на карту в wire-форме.
func ptrMapIfNotEmpty(m map[string]any) *map[string]interface{} {
	if len(m) == 0 {
		return nil
	}
	out := map[string]interface{}(m)
	return &out
}

// extractSummaryCounts вынимает success_count/fail_count/total из summary jsonb
// в плоский [PushSummaryCountsView], возвращает nil, если ни одного поля нет (пусто
// либо pending/running с {}). jsonb-числа приходят как float64 после json.Unmarshal —
// приводим к int с floor-семантикой (orchestrator пишет всегда целые). Все поля —
// *int, чтобы 0 (легитимный счётчик) отличался от «не было записи».
func extractSummaryCounts(summary map[string]any) *PushSummaryCountsView {
	if len(summary) == 0 {
		return nil
	}
	var counts PushSummaryCountsView
	any := false
	if v, ok := summary["total"].(float64); ok {
		i := int(v)
		counts.Total = &i
		any = true
	}
	if v, ok := summary["success_count"].(float64); ok {
		i := int(v)
		counts.SuccessCount = &i
		any = true
	}
	if v, ok := summary["fail_count"].(float64); ok {
		i := int(v)
		counts.FailCount = &i
		any = true
	}
	if !any {
		return nil
	}
	return &counts
}

// rowToPushRunListEntryView — граничный конвертер домен→view для list-эндпоинта
// `GET /v1/push-runs` (UI-4). Мапит [pushorch.PushRunRow] в плоский [PushRunListEntryView],
// сохраняя нативный time.Time. Compact-форма: вырезаны тяжёлые поля (`input` /
// `summary.hosts[]`), summary редуцирован до агрегированных summary_counts
// (extractSummaryCounts). date-time: прежний wire был секундным, поэтому Truncate(Second)
// держит байт-в-байт. Опциональные поля — nil при пустом значении (паритет с прежним
// omitempty). Полная запись — через `GET /v1/push/{apply_id}` (GetTyped).
func rowToPushRunListEntryView(row *pushorch.PushRunRow) PushRunListEntryView {
	entry := PushRunListEntryView{
		ApplyID:       row.ApplyID,
		InventorySids: row.InventorySIDs,
		DestinyRef:    row.DestinyRef,
		CleanupStale:  row.CleanupStale,
		Status:        string(row.Status),
		StartedAt:     row.StartedAt.UTC().Truncate(time.Second),
		SSHProvider:   ptrIfNotEmpty(row.SSHProvider),
		StartedByAID:  ptrIfNotEmpty(row.StartedByAID),
		SummaryCounts: extractSummaryCounts(row.Summary),
	}
	if row.FinishedAt != nil {
		fin := row.FinishedAt.UTC().Truncate(time.Second)
		entry.FinishedAt = &fin
	}
	return entry
}
