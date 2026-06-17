// Package auditpg — Postgres-реализация [audit.Writer] поверх pgxpool.Pool.
//
// Вынесен из `shared/audit/` в `keeper/internal/auditpg/` по architect-
// решению M0.4.0: импорт `pgx/v5` тянет ~1.5 MB pgx-кода и
// `pgtype.init.0`-регистрации; включение в `shared/` транзитивно
// затягивало бы их в `soul`-бинарь через будущий путь
// `soul → shared/config → shared/audit`, нарушая ADR-011 «Soul-изоляция
// гарантируется компилятором».
//
// `shared/audit/` остаётся pgx-free: типы, интерфейс [audit.Writer], enum
// [audit.Source], masking и ULID-helper. Реализации write-path-а живут в
// бинарных модулях — этот пакет в `keeper/internal/`, будущие
// (multi-writer + OTel dual-write, ADR-022(f)) — там же.
package auditpg

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/souls-guild/soul-stack/shared/audit"
)

// execer — узкое подмножество интерфейса pgxpool.Pool, нужное для
// INSERT INTO audit_log. Сужение позволяет unit-тестировать writer
// fake-реализацией без поднятия Postgres-а; реальный pool из
// keeper/internal/pg удовлетворяет интерфейсу автоматически.
type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// pgxWriter — Writer-реализация поверх pgxpool.Pool (или совместимого
// pgx.Conn в тестах). Один экземпляр на Keeper-процесс; safe for
// concurrent use — pool сам обеспечивает потокобезопасность.
type pgxWriter struct {
	pool execer
}

// NewWriter оборачивает уже инициализированный pgxpool.Pool в
// [audit.Writer]. Owner-ship пула остаётся у caller-а: writer не
// закрывает пул, lifecycle — keeper/internal/pg → keeper/cmd/keeper.
func NewWriter(pool execer) audit.Writer {
	return &pgxWriter{pool: pool}
}

// insertSQL — single INSERT в audit_log. Колонки строго в порядке
// ADR-022(a); audit_id обязателен (генерируется до Exec, иначе PG не
// сможет применить DEFAULT — его нет на PK).
const insertSQL = `
INSERT INTO audit_log (audit_id, created_at, event_type, source, archon_aid, correlation_id, payload)
VALUES ($1, COALESCE($2, NOW()), $3, $4, $5, $6, $7)
`

// Write фиксирует событие в audit_log. Контракт:
//
//   - event.EventType и event.Source — обязательны; пустые значения →
//     error без INSERT.
//   - event.Source валидируется по [audit.Source.Valid] — закрытый enum
//     по ADR-022(b); cast произвольной строки в audit.Source отлавливается
//     тут.
//   - event.AuditID пуст → генерируется [audit.NewULID].
//   - event.CreatedAt zero → передаётся NULL, PG ставит DEFAULT NOW().
//   - event.ArchonAID / event.CorrelationID пусты → NULL в БД.
//   - event.Payload nil → пустой JSONB `{}`; nan/inf/неcериализуемые
//     значения отдаются json.Marshal как есть и пробрасываются caller-у
//     как error — это контракт инициатора (payload должен быть
//     JSON-сериализуем).
//   - Секреты в Payload маскируются через [audit.MaskSecrets] перед
//     marshal-ом.
//
// На любую error pgxWriter не делает retry — это ответственность
// caller-а (Reaper-цикл / hot-reload иначе обрабатывают сбой).
func (w *pgxWriter) Write(ctx context.Context, event *audit.Event) error {
	if event == nil {
		return fmt.Errorf("audit: nil event")
	}
	if event.EventType == "" {
		return fmt.Errorf("audit: event_type is empty")
	}
	if !event.Source.Valid() {
		return fmt.Errorf("audit: invalid source %q", event.Source)
	}

	auditID := event.AuditID
	if auditID == "" {
		auditID = audit.NewULID()
	}

	var createdAt any
	if !event.CreatedAt.IsZero() {
		createdAt = event.CreatedAt.UTC()
	}

	var archonAID any
	if event.ArchonAID != "" {
		archonAID = event.ArchonAID
	}

	var correlationID any
	if event.CorrelationID != "" {
		correlationID = event.CorrelationID
	}

	payloadBytes, err := marshalPayload(event.Payload)
	if err != nil {
		return fmt.Errorf("audit: marshal payload: %w", err)
	}

	if _, err := w.pool.Exec(ctx, insertSQL,
		auditID,
		createdAt,
		string(event.EventType),
		string(event.Source),
		archonAID,
		correlationID,
		payloadBytes,
	); err != nil {
		return fmt.Errorf("audit: insert: %w", err)
	}
	return nil
}

// marshalPayload маскирует секреты и сериализует payload в JSON-bytes,
// пригодные для прямой подстановки в JSONB-колонку pgx-ом. nil-payload
// → `[]byte("{}")` (валидный JSONB, не NULL — колонка NOT NULL DEFAULT
// '{}'::jsonb).
func marshalPayload(payload map[string]any) ([]byte, error) {
	masked := audit.MaskSecrets(payload)
	if masked == nil {
		return []byte("{}"), nil
	}
	return json.Marshal(masked)
}

// compileTimeAssertExecer — гарантия что *pgx.Conn удовлетворяет
// execer-интерфейсу. Не используется в runtime; ловит поломку контракта
// pgx/v5 при апгрейде версии. (`*pgxpool.Pool` — отдельный кандидат,
// проверка через NewWriter-инициализацию в keeper/cmd/keeper.)
var _ execer = (*pgx.Conn)(nil)
