package audit

import "context"

// Writer — абстракция write-path-а audit-pipeline-а. Принимает [Event],
// заполняет zero-поля (AuditID, CreatedAt), маскирует секреты в Payload
// и фиксирует событие в backing store.
//
// Реализации:
//   - keeper/internal/auditpg.NewWriter — Postgres-impl. Живёт в
//     keeper-модуле, чтобы pgx-зависимость не утекала транзитивно в
//     soul-бинарь через shared/ (ADR-011 «Soul-изоляция гарантируется
//     компилятором»).
//   - Будущий multi-writer для OTel dual-write (M0.4.1, ADR-022(f)) и
//     noop-writer (для unit-тестов инициаторов) — там же, через тот же
//     интерфейс без breaking changes.
type Writer interface {
	Write(ctx context.Context, event *Event) error
}
