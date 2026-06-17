// Package audit — структурированный audit-log Soul Stack: канонические
// EventType-имена (ADR-022), сериализация payload в jsonb, маскирование
// секретов через MaskSecrets (см. mask.go).
//
// Канон wire-format-имён событий — docs/naming-rules.md → Audit-events.
// Каждое событие пишет через audit.Writer интерфейс; для тестов есть
// in-memory fake (audittest пакет).
package audit

import "time"

// Event — одна запись audit-pipeline-а. Соответствует строке `audit_log`
// в Postgres (ADR-022(a)).
//
// Заполняется write-path-инициатором (HTTP-middleware, MCP-handler,
// hot-reload pipeline, Reaper, bootstrap, Soul gRPC forwarder) и
// передаётся `Writer.Write` (см. [writer.go]). [Writer]-реализация
// (`keeper/internal/auditpg`) маскирует секреты в `Payload` через
// [MaskSecrets] и заполняет zero-поля (`AuditID`, `CreatedAt`) перед
// INSERT.
type Event struct {
	// AuditID — ULID (26 chars). Если пуст, write-path-реализация
	// сгенерирует через [NewULID] перед INSERT.
	AuditID string

	// EventType — `<area>.<action>` (см. [EventType]).
	EventType EventType

	// Source — кто инициировал событие (closed enum [Source]).
	Source Source

	// ArchonAID — AID Архонта, инициировавшего событие. Пустая строка
	// означает «не применимо» (для `signal` / `keeper_internal` /
	// `soul_grpc`) — write-path-реализация запишет NULL.
	ArchonAID string

	// CorrelationID — ULID цепочки связанных событий. Опционально (см.
	// ADR-022(c)). Пустая строка → NULL в `audit_log.correlation_id`.
	CorrelationID string

	// Payload — kind-specific полезная нагрузка для `audit_log.payload`
	// (JSONB). Может быть nil — write-path-реализация запишет
	// `'{}'::jsonb`. Секреты в Payload (по known-keys list и
	// `vault:`-prefix value) маскируются перед INSERT — см. [MaskSecrets].
	Payload map[string]any

	// CreatedAt — момент возникновения события. Zero-value → DEFAULT
	// `NOW()` Postgres-а (через nil-параметр в INSERT).
	CreatedAt time.Time
}
