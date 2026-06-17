package auditpg

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/souls-guild/soul-stack/shared/audit"
)

// queryRower — узкое подмножество pgxpool.Pool, нужное Reader-у. Сужение
// позволяет unit-тестировать List через fake без поднятия Postgres-а;
// pgxpool.Pool/pgx.Conn/pgx.Tx удовлетворяют автоматически.
type queryRower interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Reader — read-side обёртка над `audit_log`. Concurrent-safe; собственного
// состояния не держит. Симметрично errand.Store / operator.SelectByAID:
// pool владеет caller, Reader — узкий клиент.
type Reader struct {
	pool queryRower
}

// NewReader конструирует Reader; pool обязателен (paник caller-а — NewServer
// уже валидирует non-nil).
func NewReader(pool queryRower) *Reader {
	return &Reader{pool: pool}
}

// ListFilter — параметры `GET /v1/audit`. Пустые поля = «без фильтра»;
// multi-value Types/Sources фильтруются через `IN (…)`. StartedAfter /
// StartedBefore — UTC, zero-time = без фильтра.
//
// Multi-value по convention `?type=X&type=Y` парсится handler-ом из
// `url.Values.Get`-эквивалентного multi-getter-а (см. handler).
type ListFilter struct {
	Types         []string
	Sources       []string
	ArchonAID     string
	CorrelationID string
	// PayloadHerald — exact-match по строковому полю `herald` в JSONB-payload
	// (`payload->>'herald'`). Для истории доставок одного Herald-канала
	// (события herald.delivered/herald.failed несут `payload.herald`). Пусто =
	// без фильтра.
	PayloadHerald string
	// PayloadVoyage — exact-match по строковому полю `voyage_id` в JSONB-payload
	// (`payload->>'voyage_id'`). Для Voyage detail: per-incarnation события
	// incarnation.run_completed несут correlation_id=apply_id (per-incarnation,
	// не voyage_id), поэтому фильтрация по correlation_id не собирает run-события
	// вояжа — нужен payload-фильтр по voyage_id (ADR-052 amend §k). Пусто =
	// без фильтра.
	PayloadVoyage string
	StartedAfter  time.Time
	StartedBefore time.Time
}

// Row — read-проекция строки `audit_log`. Соответствует колонкам таблицы
// (миграция 001_create_audit_log.up.sql). Payload — распакованный JSONB.
// keeper_kid в схеме отсутствует (ADR-022 / 001_-миграция не несёт колонки);
// если поле понадобится — отдельная миграция + расширение Row.
type Row struct {
	AuditID       string
	CreatedAt     time.Time
	EventType     string
	Source        audit.Source
	ArchonAID     *string
	CorrelationID *string
	Payload       map[string]any
}

// List возвращает страницу строк под фильтром, отсортированную по created_at
// DESC (свежие сверху, паритет push_runs/incarnations/errands). total —
// COUNT(*) под тем же фильтром без LIMIT/OFFSET (для UI-пагинации).
//
// Read-only; в audit_log сам факт чтения НЕ пишется (избегаем рекурсии:
// каждый GET /v1/audit удваивал бы таблицу).
func (r *Reader) List(ctx context.Context, f ListFilter, offset, limit int) ([]*Row, int, error) {
	whereSQL, args := buildAuditWhere(f)

	var total int
	if err := r.pool.QueryRow(ctx, "SELECT COUNT(*) FROM audit_log"+whereSQL, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("audit: list count: %w", err)
	}

	selectSQL := `SELECT audit_id, created_at, event_type, source, archon_aid, correlation_id, payload
FROM audit_log` + whereSQL + `
ORDER BY created_at DESC
LIMIT $` + strconv.Itoa(len(args)+1) + ` OFFSET $` + strconv.Itoa(len(args)+2)
	args = append(args, limit, offset)

	rows, err := r.pool.Query(ctx, selectSQL, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("audit: list select: %w", err)
	}
	defer rows.Close()

	out := make([]*Row, 0, limit)
	for rows.Next() {
		row, err := scanAuditRow(rows)
		if err != nil {
			return nil, 0, fmt.Errorf("audit: list scan: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("audit: list iter: %w", err)
	}
	return out, total, nil
}

// buildAuditWhere собирает WHERE-предикат под ListFilter. Multi-value поля
// разворачиваются в `IN ($a,$b,…)`. Параметры пишутся инкрементально в args;
// плейсхолдеры $N — позиционные.
func buildAuditWhere(f ListFilter) (string, []any) {
	var (
		conds []string
		args  []any
	)
	if len(f.Types) > 0 {
		conds = append(conds, "event_type IN ("+placeholders(&args, anyOfStrings(f.Types))+")")
	}
	if len(f.Sources) > 0 {
		conds = append(conds, "source IN ("+placeholders(&args, anyOfStrings(f.Sources))+")")
	}
	if f.ArchonAID != "" {
		args = append(args, f.ArchonAID)
		conds = append(conds, "archon_aid = $"+strconv.Itoa(len(args)))
	}
	if f.CorrelationID != "" {
		args = append(args, f.CorrelationID)
		conds = append(conds, "correlation_id = $"+strconv.Itoa(len(args)))
	}
	if f.PayloadHerald != "" {
		args = append(args, f.PayloadHerald)
		conds = append(conds, "payload->>'herald' = $"+strconv.Itoa(len(args)))
	}
	if f.PayloadVoyage != "" {
		args = append(args, f.PayloadVoyage)
		conds = append(conds, "payload->>'voyage_id' = $"+strconv.Itoa(len(args)))
	}
	if !f.StartedAfter.IsZero() {
		args = append(args, f.StartedAfter.UTC())
		conds = append(conds, "created_at >= $"+strconv.Itoa(len(args)))
	}
	if !f.StartedBefore.IsZero() {
		args = append(args, f.StartedBefore.UTC())
		conds = append(conds, "created_at <= $"+strconv.Itoa(len(args)))
	}
	if len(conds) == 0 {
		return "", args
	}
	out := " WHERE "
	for i, c := range conds {
		if i > 0 {
			out += " AND "
		}
		out += c
	}
	return out, args
}

// placeholders добавляет values в args и возвращает строку плейсхолдеров
// "$n,$n+1,…". Хелпер для `IN (…)`-предикатов с multi-value-фильтрами.
func placeholders(args *[]any, values []any) string {
	out := ""
	for i, v := range values {
		*args = append(*args, v)
		if i > 0 {
			out += ","
		}
		out += "$" + strconv.Itoa(len(*args))
	}
	return out
}

// anyOfStrings — конверсия []string → []any для placeholders. Слой адаптации
// между closed-typed filter-полями (типизированный []string у caller-а) и
// pgx-args (variadic any).
func anyOfStrings(s []string) []any {
	out := make([]any, len(s))
	for i, v := range s {
		out[i] = v
	}
	return out
}

// scanAuditRow распаковывает один SELECT-row в *Row. Симметрично
// errand.scanRow / operator.scanOperator.
func scanAuditRow(r pgx.Rows) (*Row, error) {
	var (
		row        Row
		sourceStr  string
		payloadRaw []byte
	)
	if err := r.Scan(
		&row.AuditID,
		&row.CreatedAt,
		&row.EventType,
		&sourceStr,
		&row.ArchonAID,
		&row.CorrelationID,
		&payloadRaw,
	); err != nil {
		return nil, err
	}
	row.Source = audit.Source(sourceStr)
	if len(payloadRaw) > 0 {
		if err := json.Unmarshal(payloadRaw, &row.Payload); err != nil {
			return nil, fmt.Errorf("audit: unmarshal payload: %w", err)
		}
	}
	return &row, nil
}
