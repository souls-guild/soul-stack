package pushorch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// PushRunStatus — терминальные/in-flight статусы записи `push_runs`
// (миграция 051). Синхронизировано с CHECK push_runs_status_valid.
type PushRunStatus string

const (
	StatusPending       PushRunStatus = "pending"
	StatusRunning       PushRunStatus = "running"
	StatusSuccess       PushRunStatus = "success"
	StatusPartialFailed PushRunStatus = "partial_failed"
	StatusFailed        PushRunStatus = "failed"
	StatusCancelled     PushRunStatus = "cancelled"
)

// PushRunRow — read-проекция строки `push_runs` (для GET /v1/push/{apply_id}).
// summary хранится как map[string]any (jsonb) — caller сам сериализует в ответ.
// inventory_sids — TEXT[]; ssh_provider/started_by_aid — nullable.
type PushRunRow struct {
	ApplyID       string
	InventorySIDs []string
	DestinyRef    string
	SSHProvider   string
	Input         map[string]any
	CleanupStale  bool
	Status        PushRunStatus
	StartedAt     time.Time
	FinishedAt    *time.Time
	StartedByAID  string
	StartedByKID  string
	Summary       map[string]any
}

// ExecQueryRower — узкая поверхность pgxpool.Pool, нужная Store-у. Симметрично
// soul.ExecQueryRower / topology.Querier: позволяет fake в unit-тестах без PG.
type ExecQueryRower interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// ErrNotFound — запись push_runs по apply_id не найдена (GET handler).
var ErrNotFound = errors.New("pushorch: push_run not found")

// Store — read/write CRUD-поверхность над таблицей `push_runs`.
//
// Concurrent-safe: каждый метод — один запрос; собственного in-memory-состояния
// не держит. БД-уровень атомарности (single UPDATE с RowsAffected) служит
// барьером single-winner-а между orchestrator-горутиной и Reaper-purge-ом.
type Store struct {
	db ExecQueryRower
}

// NewStore конструирует Store. db обязателен.
func NewStore(db ExecQueryRower) *Store {
	return &Store{db: db}
}

const insertPushRunSQL = `
INSERT INTO push_runs (
    apply_id, inventory_sids, destiny_ref, ssh_provider, input,
    cleanup_stale, status, started_at, started_by_aid, started_by_kid
) VALUES (
    $1, $2, $3, NULLIF($4, ''), $5,
    $6, $7, NOW(), $8, $9
)
`

// Insert вставляет новую запись push_runs со статусом `pending`. apply_id — ULID,
// валидируется caller-ом (audit.IsValidULID). Сохраняется ровно один раз
// (orchestrator стартует goroutine после успешного INSERT-а). input/summary —
// jsonb; пустой input → пустой объект (DEFAULT '{}'::jsonb выставит сам PG, но
// мы шлём явно для предсказуемости).
func (s *Store) Insert(ctx context.Context, row PushRunRow) error {
	inputJSON, err := marshalJSONB(row.Input)
	if err != nil {
		return fmt.Errorf("pushorch: marshal input: %w", err)
	}
	var startedByAID any = row.StartedByAID
	if row.StartedByAID == "" {
		startedByAID = nil
	}
	_, err = s.db.Exec(ctx, insertPushRunSQL,
		row.ApplyID,
		row.InventorySIDs,
		row.DestinyRef,
		row.SSHProvider,
		inputJSON,
		row.CleanupStale,
		string(StatusPending),
		startedByAID,
		row.StartedByKID,
	)
	if err != nil {
		return fmt.Errorf("pushorch: insert push_runs: %w", err)
	}
	return nil
}

const selectPushRunSQL = `
SELECT apply_id, inventory_sids, destiny_ref, COALESCE(ssh_provider, ''),
       input, cleanup_stale, status, started_at, finished_at,
       COALESCE(started_by_aid, ''), started_by_kid, COALESCE(summary, '{}'::jsonb)
FROM push_runs
WHERE apply_id = $1
`

// Get читает строку по apply_id. ErrNotFound — записи нет (caller → 404).
func (s *Store) Get(ctx context.Context, applyID string) (*PushRunRow, error) {
	row := s.db.QueryRow(ctx, selectPushRunSQL, applyID)
	var (
		r           PushRunRow
		statusStr   string
		finishedAt  *time.Time
		inputJSON   []byte
		summaryJSON []byte
	)
	if err := row.Scan(
		&r.ApplyID,
		&r.InventorySIDs,
		&r.DestinyRef,
		&r.SSHProvider,
		&inputJSON,
		&r.CleanupStale,
		&statusStr,
		&r.StartedAt,
		&finishedAt,
		&r.StartedByAID,
		&r.StartedByKID,
		&summaryJSON,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("pushorch: select push_run: %w", err)
	}
	r.Status = PushRunStatus(statusStr)
	r.FinishedAt = finishedAt
	if len(inputJSON) > 0 {
		if err := json.Unmarshal(inputJSON, &r.Input); err != nil {
			return nil, fmt.Errorf("pushorch: unmarshal input: %w", err)
		}
	}
	if len(summaryJSON) > 0 {
		if err := json.Unmarshal(summaryJSON, &r.Summary); err != nil {
			return nil, fmt.Errorf("pushorch: unmarshal summary: %w", err)
		}
	}
	return &r, nil
}

const updateStatusRunningSQL = `
UPDATE push_runs
SET status = $2
WHERE apply_id = $1
`

// MarkRunning переводит pending → running. Идемпотентный single-step без
// guard-а на текущий статус — orchestrator вызывает строго один раз ПОСЛЕ
// успешного Insert-а.
func (s *Store) MarkRunning(ctx context.Context, applyID string) error {
	if _, err := s.db.Exec(ctx, updateStatusRunningSQL, applyID, string(StatusRunning)); err != nil {
		return fmt.Errorf("pushorch: mark running: %w", err)
	}
	return nil
}

const updateTerminalSQL = `
UPDATE push_runs
SET status      = $2,
    summary     = $3,
    finished_at = NOW()
WHERE apply_id = $1
`

// MarkTerminal проставляет финальный статус (success/failed/partial_failed/
// cancelled), summary и finished_at=NOW(). Один UPDATE одной транзакцией.
func (s *Store) MarkTerminal(ctx context.Context, applyID string, status PushRunStatus, summary map[string]any) error {
	summaryJSON, err := marshalJSONB(summary)
	if err != nil {
		return fmt.Errorf("pushorch: marshal summary: %w", err)
	}
	if _, err := s.db.Exec(ctx, updateTerminalSQL, applyID, string(status), summaryJSON); err != nil {
		return fmt.Errorf("pushorch: mark terminal: %w", err)
	}
	return nil
}

const cancelOrphanSQL = `
UPDATE push_runs
SET status      = 'cancelled',
    summary     = COALESCE(summary, '{}'::jsonb) || jsonb_build_object(
        'orphan_purged', true,
        'reason', $2
    ),
    finished_at = NOW()
WHERE apply_id = $1
  AND status IN ('pending', 'running')
`

// CancelOrphan переводит in-flight (pending/running) запись в `cancelled` с
// пометкой orphan_purged в summary. Используется Reaper-rule purge_orphan_push_runs
// для прогонов, чей Keeper-инстанс умер во время выполнения. WHERE-guard
// (status IN pending/running) — single-winner: гонка с реальным MarkTerminal
// проигрывает (rows_affected==0).
func (s *Store) CancelOrphan(ctx context.Context, applyID, reason string) (bool, error) {
	tag, err := s.db.Exec(ctx, cancelOrphanSQL, applyID, reason)
	if err != nil {
		return false, fmt.Errorf("pushorch: cancel orphan %s: %w", applyID, err)
	}
	return tag.RowsAffected() > 0, nil
}

const listOrphansSQL = `
SELECT apply_id
FROM push_runs
WHERE status IN ('pending', 'running')
  AND started_at < NOW() - $1::interval
ORDER BY started_at ASC
LIMIT $2
`

// ListOrphans возвращает apply_id-ы in-flight прогонов старше maxAge (TTL для
// purge_orphan_push_runs). Симметрично reaper-purger-функциям: один батч,
// LIMIT $2. Используется Reaper для последующего CancelOrphan по каждому.
func (s *Store) ListOrphans(ctx context.Context, maxAge time.Duration, batchSize int) ([]string, error) {
	rows, err := s.db.Query(ctx, listOrphansSQL, maxAge, batchSize)
	if err != nil {
		return nil, fmt.Errorf("pushorch: list orphans: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("pushorch: scan orphan: %w", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pushorch: orphans iter: %w", err)
	}
	return out, nil
}

// marshalJSONB кодирует map в jsonb-bytes. nil → "{}".
func marshalJSONB(m map[string]any) ([]byte, error) {
	if m == nil {
		return []byte(`{}`), nil
	}
	return json.Marshal(m)
}

// ListFilter — фильтры [Store.SelectAll] глобального list-эндпоинта
// `GET /v1/push-runs` (UI-4). Statuses — multi-value-фильтр (несколько
// значений объединяются через OR); пустой слайс — без фильтрации по статусу.
// SSHProvider — exact-match; пустая строка — все провайдеры. Невалидный статус —
// caller-handler отказывает на 422 до вызова CRUD.
type ListFilter struct {
	Statuses    []PushRunStatus
	SSHProvider string
}

// SQL глобального list-эндпоинта `GET /v1/push-runs` (UI-4). Параметры:
//
//	$1 — text[] статусов (NULL/empty → не фильтруем; non-empty → status = ANY($1));
//	$2 — ssh_provider (NULL/empty → не фильтруем; non-empty → exact match);
//	$3 — limit, $4 — offset.
//
// `cardinality($1::text[]) = 0` — каноническая «multi-value-фильтр опционален»
// форма (та же, что у tide.SelectAll): пустой массив трактуется как «не задан»,
// не «не матчит ничего».
const selectAllPushRunsSQL = `
SELECT apply_id, inventory_sids, destiny_ref, COALESCE(ssh_provider, ''),
       input, cleanup_stale, status, started_at, finished_at,
       COALESCE(started_by_aid, ''), started_by_kid, COALESCE(summary, '{}'::jsonb)
FROM push_runs
WHERE ($1::text[] IS NULL OR cardinality($1::text[]) = 0 OR status = ANY($1::text[]))
  AND ($2::text IS NULL OR ssh_provider = $2)
ORDER BY started_at DESC
LIMIT $3 OFFSET $4
`

const countAllPushRunsSQL = `
SELECT COUNT(*) FROM push_runs
WHERE ($1::text[] IS NULL OR cardinality($1::text[]) = 0 OR status = ANY($1::text[]))
  AND ($2::text IS NULL OR ssh_provider = $2)
`

// SelectAll возвращает страницу push_runs-строк для глобального list-эндпоинта
// `GET /v1/push-runs` (UI-4). Сортировка — `started_at DESC` (свежие первыми).
//
// Total — общее число строк под фильтр (отдельный COUNT). limit/offset не
// валидируются — caller-handler уже прогнал через sharedapi.ParsePage.
//
// summary в результирующих строках присутствует целиком (forward-compat: list-
// эндпоинт ещё не отрезает hosts[] на client-side; ТЗ UI-4 обещает «subset без
// summary.hosts», но это видимая часть DTO-маппинга в handler-е, не SQL-проекции).
func (s *Store) SelectAll(ctx context.Context, filter ListFilter, offset, limit int) ([]*PushRunRow, int, error) {
	var statusesArg any
	if len(filter.Statuses) > 0 {
		ss := make([]string, len(filter.Statuses))
		for i, st := range filter.Statuses {
			ss[i] = string(st)
		}
		statusesArg = ss
	}
	var providerArg any
	if filter.SSHProvider != "" {
		providerArg = filter.SSHProvider
	}

	var total int
	if err := s.db.QueryRow(ctx, countAllPushRunsSQL, statusesArg, providerArg).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("pushorch: count all: %w", err)
	}

	rows, err := s.db.Query(ctx, selectAllPushRunsSQL, statusesArg, providerArg, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("pushorch: select all: %w", err)
	}
	defer rows.Close()

	out := make([]*PushRunRow, 0, limit)
	for rows.Next() {
		r := &PushRunRow{}
		var (
			statusStr   string
			finishedAt  *time.Time
			inputJSON   []byte
			summaryJSON []byte
		)
		if err := rows.Scan(
			&r.ApplyID,
			&r.InventorySIDs,
			&r.DestinyRef,
			&r.SSHProvider,
			&inputJSON,
			&r.CleanupStale,
			&statusStr,
			&r.StartedAt,
			&finishedAt,
			&r.StartedByAID,
			&r.StartedByKID,
			&summaryJSON,
		); err != nil {
			return nil, 0, fmt.Errorf("pushorch: select all scan: %w", err)
		}
		r.Status = PushRunStatus(statusStr)
		r.FinishedAt = finishedAt
		if len(inputJSON) > 0 {
			if err := json.Unmarshal(inputJSON, &r.Input); err != nil {
				return nil, 0, fmt.Errorf("pushorch: select all unmarshal input: %w", err)
			}
		}
		if len(summaryJSON) > 0 {
			if err := json.Unmarshal(summaryJSON, &r.Summary); err != nil {
				return nil, 0, fmt.Errorf("pushorch: select all unmarshal summary: %w", err)
			}
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("pushorch: select all iter: %w", err)
	}
	return out, total, nil
}

// ValidStatus сообщает, входит ли статус в [PushRunStatus]-enum. Используется
// caller-handler-ом `GET /v1/push-runs` для валидации query-фильтра до вызова
// CRUD (422 ДО SQL-обращения).
func ValidStatus(s PushRunStatus) bool {
	switch s {
	case StatusPending, StatusRunning, StatusSuccess, StatusPartialFailed, StatusFailed, StatusCancelled:
		return true
	}
	return false
}
