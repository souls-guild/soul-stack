package api

// HUMA-NATIVE reply-DTO AUDIT-ENDPOINT-домена (Teardown T5b, тираж по эталону T5a
// huma_incarnation_reply.go). Reply/output Body GET /v1/audit — native Go-struct в пакете
// api, НЕ генерёный legacy-генерата из рукописи. Паттерн (6 шагов) — в шапке
// huma_incarnation_reply.go. Ключевое для audit:
//
//   - ФОРМА байт-в-байт = legacy-генерата (json-теги/omitempty/date-time/nullable категории A-D).
//   - ИМЯ СХЕМЫ = контрактное (AuditEvent / AuditEventListReply): huma DefaultSchemaNamer
//     берёт reflect.Type.Name() → схема под тем же именем, что давал legacy-генерата.
//   - archon_aid/correlation_id — `*string` С omitempty (nil → ключ опущен); payload —
//     `map[string]interface{}` БЕЗ omitempty (всегда объект на wire); created_at —
//     наносекундный time-wire (значение усекает handler-слой до секундной точности, не
//     форма); source — enum-тип AuditEventSource (alias НЕ заведён, рукопись инлайнит
//     `type: string` + enum — schema byte-identical для native и legacy, parity GitRefType).
//   - AuditEventListReply здесь — НЕ generic-envelope (handler возвращает named Audit-
//     EventListReply, не sharedapi.PagedResponse), а обычный reply с полем items[]AuditEvent
//     + offset/limit/total. offset/limit/total — int (parity legacy-генерата) → `type: integer` без
//     format (assertOffsetEnvelopeNoFormat). Переведён как top-level reply-DTO.
//
// OUTPUT-PATTERN (документационный, НЕ рантайм-валидация): huma НЕ валидирует
// response-body (эмпирически 200, не 500). id/correlation_id — машинно ULID (миграция
// 001: audit_id/correlation_id «ULID»); archon_aid ← operator.AIDPattern. Формат для
// клиент-кодогена; pattern не влияет на json.Marshal (golden byte-exact цел).

import (
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
)

// === top-level reply-DTO (форма 1:1 с прежним legacy-генерата) ===

// AuditEvent — native запись audit_log (element AuditEventListReply.items). Форма 1:1 с
// прежним AuditEvent: archon_aid/correlation_id — `*string` С omitempty; payload — `map`
// БЕЗ omitempty (всегда объект); created_at — наносекундный time-wire (значение усекает
// handler-слой до секунд); source — native enum-тип AuditEventSource (huma_enums.go).
type AuditEvent struct {
	ArchonAID     *string                `json:"archon_aid,omitempty" pattern:"^[a-z0-9][a-z0-9._@-]{1,127}$"` // ← operator.AIDPattern
	CorrelationID *string                `json:"correlation_id,omitempty" pattern:"^[0-9A-HJKMNP-TV-Z]{26}$"`  // ULID (миграция 001)
	CreatedAt     time.Time              `json:"created_at"`
	ID            string                 `json:"id" pattern:"^[0-9A-HJKMNP-TV-Z]{26}$"` // ULID (миграция 001)
	Payload       map[string]interface{} `json:"payload"`
	Source        AuditEventSource       `json:"source"`
	Type          string                 `json:"type"`
}

// AuditEventListReply — native 200-тело GET /v1/audit (offset-envelope: items/offset/limit/
// total). items — native AuditEvent; offset/limit/total — int (parity прежнего legacy-генерата).
type AuditEventListReply struct {
	Items  []AuditEvent `json:"items"`
	Limit  int          `json:"limit"`
	Offset int          `json:"offset"`
	Total  int          `json:"total"`
}

// === проекция доменной handlers.AuditListPage (плоские поля) → native wire-DTO ===

// newAuditEvent проецирует плоскую доменную handlers.AuditEventView в native AuditEvent.
// Source — native enum-каст (тот же underlying string). created_at handler уже усёк до
// секунд (byte-exact с легаси-wire).
func newAuditEvent(v handlers.AuditEventView) AuditEvent {
	return AuditEvent{
		ArchonAID:     v.ArchonAID,
		CorrelationID: v.CorrelationID,
		CreatedAt:     v.CreatedAt,
		ID:            v.ID,
		Payload:       v.Payload,
		Source:        AuditEventSource(v.Source),
		Type:          v.Type,
	}
}

// newAuditEventListReply проецирует доменный handlers.AuditListPage в native
// AuditEventListReply. Items сохраняют nil-vs-empty 1:1 (nil → null, [] → []) ради byte-exact
// wire (категория B ADR-051) — ListTyped даёт non-nil [] (пустая лента → `[]`).
func newAuditEventListReply(p handlers.AuditListPage) AuditEventListReply {
	var items []AuditEvent
	if p.Items != nil {
		items = make([]AuditEvent, len(p.Items))
		for i := range p.Items {
			items[i] = newAuditEvent(p.Items[i])
		}
	}
	return AuditEventListReply{
		Items:  items,
		Limit:  p.Limit,
		Offset: p.Offset,
		Total:  p.Total,
	}
}
