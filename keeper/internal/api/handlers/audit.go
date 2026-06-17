// Operator API handler `GET /v1/audit` — read-only-лента audit-events
// для UI iteration 2 (placeholder /audit). Read-only: сам факт чтения в
// audit_log НЕ пишется (избежать рекурсии — каждый запрос удваивал бы
// таблицу). RBAC — `audit.read`, селектор NoSelector (фильтр по
// archon_aid передаётся query-param-ом, не RBAC-scope).
//
// T5d (handler-native): домен audit отвязан от legacy-генерата. ListTyped возвращает
// доменный [AuditListPage] с ПЛОСКИМИ wire-полями; native wire-DTO (схему
// OpenAPI + сериализацию) строит пакет api из этих полей (register-func).
// (w,r)-оболочка снята — HTTP обслуживает huma full-typed (api/huma_audit_endpoint.go).
package handlers

import (
	"context"
	"io"
	"log/slog"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	"github.com/souls-guild/soul-stack/keeper/internal/auditpg"
	"github.com/souls-guild/soul-stack/shared/api"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// AuditHandler — `GET /v1/audit`. Делегирует чтение в [auditpg.Reader]
// (узкий QueryRower-клиент над `audit_log`). Все зависимости immutable;
// safe for concurrent use.
type AuditHandler struct {
	reader *auditpg.Reader
	logger *slog.Logger
}

// NewAuditHandler конструирует handler. reader обязателен; nil-router
// `router.go` подключает audit-роут только при non-nil reader-е (паттерн
// PushHandler/ErrandHandler), поэтому handler сам не валидирует nil-deps.
func NewAuditHandler(reader *auditpg.Reader, logger *slog.Logger) *AuditHandler {
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	return &AuditHandler{reader: reader, logger: logger}
}

// AuditSpecStub — непустой *AuditHandler-заглушка для генерации huma-OpenAPI-
// фрагмента (HumaAuditSpecYAML): при dump доменный handler не вызывается (huma.
// Register его не исполняет), но register-функция требует non-nil для no-op-проверки.
// reader/logger nil — handler никогда не исполняется в spec-режиме.
func AuditSpecStub() *AuditHandler {
	return &AuditHandler{logger: slog.New(slog.NewJSONHandler(io.Discard, nil))}
}

// AuditEventView — ПЛОСКАЯ доменная проекция одной записи audit_log
// (element AuditListPage.Items), handler-native T5d. Пакет api проецирует её в
// native-схему AuditEvent (register-func). ArchonAID/CorrelationID — `*string`
// (nil → ключ опущен в native-wire); Source — RAW string домена (native-тип в api
// держит enum-форму); CreatedAt — уже усечён до секунд (parity легаси-wire).
// `keeper_kid` поля нет (миграция 001 без колонки; always-null — поле опущено).
type AuditEventView struct {
	ArchonAID     *string
	CorrelationID *string
	CreatedAt     time.Time
	ID            string
	Payload       map[string]any
	Source        string
	Type          string
}

// AuditListPage — доменный результат `GET /v1/audit` (handler-native T5d). Пакет
// api проецирует {Items, Offset, Limit, Total} → native AuditEventListReply.
type AuditListPage struct {
	Items  []AuditEventView
	Offset int
	Limit  int
	Total  int
}

// AuditListFilter — доменные параметры `GET /v1/audit` (typed-query четвёртого tier
// ADR-054). Симметрия с auditpg.ListFilter, но несёт ещё пагинацию (Offset/Limit) и
// отделён от read-side слоя: huma-handler (huma_audit_endpoint.go) и legacy
// (w,r)-оболочка собирают ОДИН этот тип, ListTyped — единственная доменная функция.
// Пустые строковые/slice-поля = «фильтр не применять»; zero-time StartedAfter/Before
// = «без временной границы» (parity легаси `if param != ""`).
type AuditListFilter struct {
	Types         []string
	Sources       []string
	ArchonAID     string
	CorrelationID string
	PayloadHerald string
	PayloadVoyage string
	StartedAfter  time.Time
	StartedBefore time.Time
	Offset        int
	Limit         int
}

// ListTyped — доменная функция `GET /v1/audit` (ADR-054 §Pattern шаг 2): валидирует
// source-enum, строит auditpg.ListFilter, читает страницу через Reader, проецирует в
// typed envelope {items, offset, limit, total}. Без http.ResponseWriter/*http.Request
// — общий код huma-handler-а и legacy (w,r)-оболочки.
//
// Ошибки — *problemError (через AsProblemDetails доставляются обоими путями тем же
// error-контрактом): невалидный source → 422 TypeValidationFailed (string-enum-
// семантика остаётся 422 даже когда huma-query enum уже отбил бы её на 422 — defense-
// in-depth для прямого вызова); БД-сбой → 500 TypeInternalError.
func (h *AuditHandler) ListTyped(ctx context.Context, f AuditListFilter) (AuditListPage, error) {
	var zero AuditListPage
	if h.reader == nil {
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "audit reader is not configured")}
	}

	// Диапазон пагинации (offset≥0, limit∈[1,1000]) — ЕДИНЫЙ источник границ
	// api.CheckPageBounds (тот же, что у ParsePage). Out-of-range → 400
	// TypeMalformedRequest (контракт-инвариант: huma typed-int НЕ несёт schema-
	// minimum/maximum, иначе вернул бы 422 — wire-change против легаси/strict 400).
	if err := api.CheckPageBounds(f.Offset, f.Limit); err != nil {
		return zero, &problemError{problem.New(problem.TypeMalformedRequest, "", err.Error())}
	}

	for _, s := range f.Sources {
		if !audit.Source(s).Valid() {
			return zero, &problemError{problem.New(problem.TypeValidationFailed, "",
				"query 'source' must be one of signal/api/mcp/keeper_internal/soul_grpc/background/config_bootstrap")}
		}
	}

	filter := auditpg.ListFilter{
		ArchonAID:     f.ArchonAID,
		CorrelationID: f.CorrelationID,
		PayloadHerald: f.PayloadHerald,
		PayloadVoyage: f.PayloadVoyage,
		StartedAfter:  f.StartedAfter,
		StartedBefore: f.StartedBefore,
	}
	if len(f.Types) > 0 {
		filter.Types = f.Types
	}
	if len(f.Sources) > 0 {
		filter.Sources = f.Sources
	}

	rows, total, err := h.reader.List(ctx, filter, f.Offset, f.Limit)
	if err != nil {
		h.logger.Error("audit.list: reader failed",
			slog.Int("offset", f.Offset),
			slog.Int("limit", f.Limit),
			slog.Any("error", err))
		return zero, &problemError{problem.New(problem.TypeInternalError, "", "list audit failed")}
	}

	items := make([]AuditEventView, 0, len(rows))
	for _, row := range rows {
		items = append(items, auditEventView(row))
	}

	return AuditListPage{
		Items:  items,
		Offset: f.Offset,
		Limit:  f.Limit,
		Total:  total,
	}, nil
}

// auditEventView проецирует [auditpg.Row] в плоскую доменную [AuditEventView].
// Source — RAW string домена ([audit.Source]); CreatedAt — UTC, обрезан до секунд
// (прежний `.Format(time.RFC3339)` тоже отбрасывал дробную часть — секундный wire).
// ArchonAID / CorrelationID — pointer-optional (nil → поле опущено native-типом).
func auditEventView(row *auditpg.Row) AuditEventView {
	return AuditEventView{
		ID:            row.AuditID,
		Type:          row.EventType,
		Source:        string(row.Source),
		ArchonAID:     row.ArchonAID,
		CorrelationID: row.CorrelationID,
		CreatedAt:     row.CreatedAt.UTC().Truncate(time.Second),
		Payload:       row.Payload,
	}
}
