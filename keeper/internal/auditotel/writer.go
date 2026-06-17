// Package auditotel — OTel-реализация [audit.Writer], публикующая
// событие как standalone span.
//
// Используется multi-writer-ом ([keeper/internal/auditmulti]) как
// **secondary** writer в dual-write-pipeline: PG — источник правды
// (sync, обязательный), OTel — transient debugging (async, best-effort).
// Поэтому Write всегда возвращает nil — ошибки экспортёра обрабатываются
// OTel-SDK (BatchSpanProcessor) асинхронно, не блокируют write-path и не
// влияют на consistency audit_log.
//
// Span создаётся standalone (без parent) — audit-event фиксирует факт
// «что произошло», не часть распределённой трассировки текущего request-а.
// Trace в OTel-вьюере покажет один span с длительностью ~0 и набором
// attributes. Это сознательный trade-off против ad-hoc-query-ев по
// `audit_log` (Grafana → Tempo / Jaeger / другой OTel-backend).
//
// ADR-022(f) фиксирует dual-write multi-writer-ом; этот пакет — secondary
// half (Postgres half — `keeper/internal/auditpg`). Lifecycle экспортёра и
// TracerProvider — на стороне `cmd/keeper` (M0.4.2), tracer передаётся в
// [NewWriter] уже сконфигурированным.
package auditotel

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/souls-guild/soul-stack/shared/audit"
)

// otelWriter — [audit.Writer]-реализация поверх [trace.Tracer]. Один
// экземпляр на Keeper-процесс; safe for concurrent use — tracer
// потокобезопасен по контракту OTel.
type otelWriter struct {
	tracer trace.Tracer
}

// NewWriter оборачивает [trace.Tracer] в [audit.Writer]. Owner-ship
// TracerProvider остаётся у caller-а: writer не shutdown-ит провайдер,
// lifecycle — `cmd/keeper`.
func NewWriter(tracer trace.Tracer) audit.Writer {
	return &otelWriter{tracer: tracer}
}

// Write публикует event как standalone span. Контракт:
//
//   - event == nil → no-op, возврат nil без span-а.
//   - event.EventType == "" → ранний возврат с slog.Warn (создавать span
//     с пустым name бессмысленно — он непригоден в OTel-UI).
//   - event.AuditID пуст → генерируется [audit.NewULID] (симметрия с
//     pgxWriter; audit_id одинаков в PG и OTel при использовании внутри
//     multi-writer-а).
//   - Span name = string(event.EventType) (`<area>.<action>`).
//   - Attributes по схеме `audit.*`: source, correlation_id, archon_aid,
//     id; payload через [audit.MaskSecrets] раскладывается в плоские
//     `audit.payload.<key>` (только top-level — вложенные maps/slices
//     отдаются stringify через `fmt.Sprintf("%v", …)`).
//   - End span с явным timestamp = event.CreatedAt (если zero — now).
//   - Возврат nil **всегда** — secondary-writer в dual-write не блокирует
//     primary; ошибки экспорта — async через BatchSpanProcessor, видны
//     через OTel-внутренний log-handler.
func (w *otelWriter) Write(_ context.Context, event *audit.Event) error {
	if event == nil {
		return nil
	}
	if event.EventType == "" {
		slog.Warn(
			"audit otel writer: empty EventType, skipping span",
			slog.String("audit_id", event.AuditID),
			slog.String("source", string(event.Source)),
		)
		return nil
	}

	auditID := event.AuditID
	if auditID == "" {
		auditID = audit.NewULID()
	}

	endTime := event.CreatedAt
	if endTime.IsZero() {
		endTime = time.Now().UTC()
	}

	// context.Background — span standalone, не привязан к request-у.
	_, span := w.tracer.Start(context.Background(), string(event.EventType))

	attrs := make([]attribute.KeyValue, 0, 4+len(event.Payload))
	attrs = append(attrs,
		attribute.String("audit.id", auditID),
		attribute.String("audit.source", string(event.Source)),
	)
	if event.CorrelationID != "" {
		attrs = append(attrs, attribute.String("audit.correlation_id", event.CorrelationID))
	}
	if event.ArchonAID != "" {
		attrs = append(attrs, attribute.String("audit.archon_aid", event.ArchonAID))
	}

	masked := audit.MaskSecrets(event.Payload)
	for k, v := range masked {
		if kv, ok := payloadAttribute(k, v); ok {
			attrs = append(attrs, kv)
		}
	}

	span.SetAttributes(attrs...)
	span.End(trace.WithTimestamp(endTime))
	return nil
}

// payloadAttribute строит OTel-attribute из top-level payload-ключа.
// Тип scalar-значения сохраняется (string/bool/intN/uintN/floatN); сложные
// типы (map / slice) сериализуются в строку через `fmt.Sprintf("%v", …)`,
// чтобы не уронить экспортёр на unsupported attribute-value. payload в
// OTel — debugging-aid, нормативный канал данных — JSONB-колонка в PG.
//
// Второй возврат — флаг «эмитить ли атрибут вообще»: для `nil`-значения
// возвращается `(_, false)`, чтобы caller пропустил его в SetAttributes
// (типовой OTel-pattern — отсутствие лучше пустой строки, путаемой с
// сознательно установленным пустым значением).
//
// uint64 > math.MaxInt64 сериализуется в строку: OTel attribute-int — это
// int64, безболезненного downcast-а нет.
func payloadAttribute(key string, value any) (attribute.KeyValue, bool) {
	akey := "audit.payload." + key
	switch x := value.(type) {
	case nil:
		return attribute.KeyValue{}, false
	case string:
		return attribute.String(akey, x), true
	case bool:
		return attribute.Bool(akey, x), true
	case int:
		return attribute.Int64(akey, int64(x)), true
	case int8:
		return attribute.Int64(akey, int64(x)), true
	case int16:
		return attribute.Int64(akey, int64(x)), true
	case int32:
		return attribute.Int64(akey, int64(x)), true
	case int64:
		return attribute.Int64(akey, x), true
	case uint:
		if uint64(x) > math.MaxInt64 {
			return attribute.String(akey, fmt.Sprintf("%d", x)), true
		}
		return attribute.Int64(akey, int64(x)), true
	case uint8:
		return attribute.Int64(akey, int64(x)), true
	case uint16:
		return attribute.Int64(akey, int64(x)), true
	case uint32:
		return attribute.Int64(akey, int64(x)), true
	case uint64:
		if x > math.MaxInt64 {
			return attribute.String(akey, fmt.Sprintf("%d", x)), true
		}
		return attribute.Int64(akey, int64(x)), true
	case float32:
		return attribute.Float64(akey, float64(x)), true
	case float64:
		return attribute.Float64(akey, x), true
	default:
		return attribute.String(akey, fmt.Sprintf("%v", x)), true
	}
}
