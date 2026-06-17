// Package errand — Keeper-side оркестратор pull-ad-hoc Errand-ов
// (`POST /v1/souls/{sid}/exec`, ADR-033).
//
// Контур одного Errand-а: Operator (HTTP/MCP) → Dispatcher.Dispatch →
// Outbound.SendErrand → Soul EventStream → ErrandResult в FromSoul →
// events_errand.handleErrandResult → ApplyBus.Publish(KindErrandCompleted) →
// Dispatcher subscribe-waiter завершается → MarkTerminal в errands-table.
//
// Cross-keeper: holder SID-стрима живёт в Redis-lease (`soul:<sid>:lock`).
// Local-holder → Outbound.SendErrand. Remote-holder → publish в
// `outbound:<sid>` pub/sub (тот же канал, что у apply/cancel). Reply
// летит через applybus-канал `apply:<errand_id>` (cluster-bridge).
//
// State-инвариант: Errand НЕ мутирует incarnation.state (ADR-033 §4).
// Здесь нет ApplyRunDB, нет state_changes, нет barrier — только своя
// таблица `errands` с двумя терминальными переводами (running → terminal).
package errand

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Status — терминал/in-flight статус строки `errands`. Совпадает с CHECK
// `errands_status_valid` и enum-конвертацией из proto (см. mapping в
// dispatcher.go::statusFromProto).
type Status string

const (
	StatusRunning          Status = "running"
	StatusSuccess          Status = "success"
	StatusFailed           Status = "failed"
	StatusTimedOut         Status = "timed_out"
	StatusCancelled        Status = "cancelled"
	StatusModuleNotAllowed Status = "module_not_allowed"
)

// ErrNotFound — запись по errand_id отсутствует (handler → 404).
var ErrNotFound = errors.New("errand: not found")

// Row — read/write-проекция строки `errands`. Поля nullable (exit_code,
// finished_at, output, stdout/stderr) — указатели; при NULL в БД значение
// будет nil. Для конструктора Insert не все поля обязательны (см. метод).
type Row struct {
	ErrandID        string
	SID             string
	Module          string
	Input           map[string]any
	Status          Status
	ExitCode        *int32
	Stdout          string
	Stderr          string
	StdoutTruncated bool
	StderrTruncated bool
	DurationMs      *int64
	ErrorMessage    string
	Output          map[string]any
	StartedByAID    string
	StartedByKID    string
	StartedAt       time.Time
	FinishedAt      *time.Time
	TTLAt           time.Time
}

// ListFilter — параметры `GET /v1/errands`. Пустые поля = «без фильтра».
// StartedAfter в UTC; пагинация — снаружи (offset/limit, paged response).
// Modules — multi-value exact-match OR (`module IN (…)`), параметр query
// `?module=X&module=Y`.
type ListFilter struct {
	SID          string
	Status       Status
	StartedAfter time.Time
	Modules      []string
}

// ExecQueryRower — узкая поверхность pgxpool.Pool, нужная Store-у
// (симметрично pushorch / soul / topology). Позволяет fake в unit-тестах
// без PG.
type ExecQueryRower interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Store — CRUD-обёртка над `errands`. Concurrent-safe; собственного
// in-memory-состояния не держит.
type Store struct {
	db ExecQueryRower
}

// NewStore конструирует Store; db обязателен.
func NewStore(db ExecQueryRower) *Store {
	return &Store{db: db}
}

const insertSQL = `
INSERT INTO errands (
    errand_id, sid, module, input, status,
    started_by_aid, started_by_kid, started_at, ttl_at
) VALUES (
    $1, $2, $3, $4, $5,
    $6, $7, $8, $9
)
`

// Insert вставляет начальную строку Errand-а со status='running'. errand_id —
// ULID, валидируется caller-ом. input/output — jsonb; пустой input → '{}'::jsonb
// (явно сериализуем для предсказуемости, как pushorch).
func (s *Store) Insert(ctx context.Context, row Row) error {
	inputJSON, err := marshalJSONB(row.Input)
	if err != nil {
		return fmt.Errorf("errand: marshal input: %w", err)
	}
	if _, err := s.db.Exec(ctx, insertSQL,
		row.ErrandID,
		row.SID,
		row.Module,
		inputJSON,
		string(row.Status),
		row.StartedByAID,
		row.StartedByKID,
		row.StartedAt.UTC(),
		row.TTLAt.UTC(),
	); err != nil {
		return fmt.Errorf("errand: insert: %w", err)
	}
	return nil
}

const selectByIDSQL = `
SELECT errand_id, sid, module, input, status,
       exit_code, COALESCE(stdout, ''), COALESCE(stderr, ''),
       stdout_truncated, stderr_truncated,
       duration_ms, COALESCE(error_message, ''),
       output, started_by_aid, started_by_kid,
       started_at, finished_at, ttl_at
FROM errands
WHERE errand_id = $1
`

// Get читает строку Errand-а по ULID. ErrNotFound на отсутствие.
func (s *Store) Get(ctx context.Context, errandID string) (*Row, error) {
	r := s.db.QueryRow(ctx, selectByIDSQL, errandID)
	row, err := scanRow(r)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("errand: get: %w", err)
	}
	return row, nil
}

const updateTerminalSQL = `
UPDATE errands
SET status            = $2,
    exit_code         = $3,
    stdout            = NULLIF($4, ''),
    stderr            = NULLIF($5, ''),
    stdout_truncated  = $6,
    stderr_truncated  = $7,
    duration_ms       = $8,
    error_message     = NULLIF($9, ''),
    output            = $10,
    finished_at       = NOW()
WHERE errand_id = $1
  AND status = 'running'
`

// TerminalUpdate — payload для перевода Errand-а в терминальный статус.
// stdout/stderr / output уже cap-нуты и маскированы caller-ом (mask.go).
type TerminalUpdate struct {
	Status          Status
	ExitCode        *int32
	Stdout          string
	Stderr          string
	StdoutTruncated bool
	StderrTruncated bool
	DurationMs      *int64
	ErrorMessage    string
	Output          map[string]any
}

// MarkTerminal переводит running → terminal с CAPACITY-маскированным выводом.
// WHERE-guard `status='running'` — single-winner между двумя терминальными
// путями (синхронный waiter и async-эскалация / timed_out): первый победил.
// Возвращает false, если строка уже была не в running (дубль/late-event).
func (s *Store) MarkTerminal(ctx context.Context, errandID string, upd TerminalUpdate) (bool, error) {
	outputJSON, err := marshalJSONBNullable(upd.Output)
	if err != nil {
		return false, fmt.Errorf("errand: marshal output: %w", err)
	}
	tag, err := s.db.Exec(ctx, updateTerminalSQL,
		errandID,
		string(upd.Status),
		upd.ExitCode,
		upd.Stdout,
		upd.Stderr,
		upd.StdoutTruncated,
		upd.StderrTruncated,
		upd.DurationMs,
		upd.ErrorMessage,
		outputJSON,
	)
	if err != nil {
		return false, fmt.Errorf("errand: update terminal: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// List возвращает страницу строк под фильтром, отсортированную по
// started_at DESC (свежие сверху, паритет push_runs/incarnations). total —
// COUNT(*) под тем же фильтром без LIMIT/OFFSET (для UI-пагинации).
//
// Запросы простые без подготовленных выражений — фильтр маленький
// (3 опц. поля), prepared statement переиспользуется драйвером.
func (s *Store) List(ctx context.Context, f ListFilter, offset, limit int) ([]*Row, int, error) {
	whereSQL, args := buildListWhere(f)

	countSQL := "SELECT COUNT(*) FROM errands" + whereSQL
	var total int
	if err := s.db.QueryRow(ctx, countSQL, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("errand: list count: %w", err)
	}

	selectSQL := `
SELECT errand_id, sid, module, input, status,
       exit_code, COALESCE(stdout, ''), COALESCE(stderr, ''),
       stdout_truncated, stderr_truncated,
       duration_ms, COALESCE(error_message, ''),
       output, started_by_aid, started_by_kid,
       started_at, finished_at, ttl_at
FROM errands` + whereSQL + `
ORDER BY started_at DESC
LIMIT $` + itoa(len(args)+1) + ` OFFSET $` + itoa(len(args)+2)
	args = append(args, limit, offset)

	rows, err := s.db.Query(ctx, selectSQL, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("errand: list select: %w", err)
	}
	defer rows.Close()

	out := make([]*Row, 0, limit)
	for rows.Next() {
		row, err := scanRow(rows)
		if err != nil {
			return nil, 0, fmt.Errorf("errand: list scan: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("errand: list iter: %w", err)
	}
	return out, total, nil
}

// buildListWhere собирает WHERE-предикат под ListFilter. Параметры пишутся
// инкрементально в args ([]any), плейсхолдеры $N — позиционные. Возвращает
// строку с ведущим " WHERE …" либо "" если фильтр пуст.
func buildListWhere(f ListFilter) (string, []any) {
	var (
		conds []string
		args  []any
	)
	if f.SID != "" {
		args = append(args, f.SID)
		conds = append(conds, "sid = $"+itoa(len(args)))
	}
	if f.Status != "" {
		args = append(args, string(f.Status))
		conds = append(conds, "status = $"+itoa(len(args)))
	}
	if !f.StartedAfter.IsZero() {
		args = append(args, f.StartedAfter.UTC())
		conds = append(conds, "started_at > $"+itoa(len(args)))
	}
	if len(f.Modules) > 0 {
		// IN ($n,$n+1,…) — exact-match OR; regex/glob не вводим (ТЗ).
		ph := ""
		for i, m := range f.Modules {
			args = append(args, m)
			if i > 0 {
				ph += ","
			}
			ph += "$" + itoa(len(args))
		}
		conds = append(conds, "module IN ("+ph+")")
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

const sweepOrphanRunningSQL = `
UPDATE errands
SET status        = 'timed_out',
    error_message = $2,
    finished_at   = NOW()
WHERE started_by_kid = $1
  AND status = 'running'
  AND started_at < NOW() - $3::interval
RETURNING errand_id
`

// SweepOrphanRunning переводит «осиротевшие» running-Errand-ы этого инстанса
// в timed_out. Источник осиротевших — рестарт keeper-а: каждая running-строка,
// чей `started_by_kid` равен нашему KID и `started_at < now - grace`, точно
// не дождётся ErrandResult (background-горутина умерла вместе с процессом).
// Возвращает список переведённых errand_id для последующего audit-write
// (caller сам решает писать события — Store без зависимостей от Writer).
//
// WHERE-guard `status='running'` — single-winner; параллельный live-handler
// другой ноды перевод не сорвёт.
func (s *Store) SweepOrphanRunning(ctx context.Context, kid string, grace time.Duration, reason string) ([]string, error) {
	rows, err := s.db.Query(ctx, sweepOrphanRunningSQL, kid, reason, grace)
	if err != nil {
		return nil, fmt.Errorf("errand: sweep orphan running: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("errand: sweep scan: %w", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("errand: sweep iter: %w", err)
	}
	return out, nil
}

// scanner — общий интерфейс для pgx.Row и pgx.Rows для функции scanRow.
type scanner interface {
	Scan(dest ...any) error
}

// scanRow распаковывает один SELECT-row в *Row. Используется и Get-ом
// (pgx.Row), и List-ом (pgx.Rows).
func scanRow(r scanner) (*Row, error) {
	var (
		row        Row
		statusStr  string
		exitCode   *int32
		duration   *int64
		finishedAt *time.Time
		inputJSON  []byte
		outputJSON []byte
	)
	if err := r.Scan(
		&row.ErrandID,
		&row.SID,
		&row.Module,
		&inputJSON,
		&statusStr,
		&exitCode,
		&row.Stdout,
		&row.Stderr,
		&row.StdoutTruncated,
		&row.StderrTruncated,
		&duration,
		&row.ErrorMessage,
		&outputJSON,
		&row.StartedByAID,
		&row.StartedByKID,
		&row.StartedAt,
		&finishedAt,
		&row.TTLAt,
	); err != nil {
		return nil, err
	}
	row.Status = Status(statusStr)
	row.ExitCode = exitCode
	row.DurationMs = duration
	row.FinishedAt = finishedAt
	if len(inputJSON) > 0 {
		if err := json.Unmarshal(inputJSON, &row.Input); err != nil {
			return nil, fmt.Errorf("errand: unmarshal input: %w", err)
		}
	}
	if len(outputJSON) > 0 {
		if err := json.Unmarshal(outputJSON, &row.Output); err != nil {
			return nil, fmt.Errorf("errand: unmarshal output: %w", err)
		}
	}
	return &row, nil
}

// marshalJSONB кодирует map в jsonb-bytes. nil → "{}" (input всегда non-null).
func marshalJSONB(m map[string]any) ([]byte, error) {
	if m == nil {
		return []byte(`{}`), nil
	}
	return json.Marshal(m)
}

// marshalJSONBNullable — для output: nil → SQL NULL (returned as nil-bytes),
// иначе сериализованный JSON. shell/exec-модули не возвращают output → NULL
// (а не "{}", чтобы отличать «модуль не положил структурный output» от
// «положил пустой объект»).
func marshalJSONBNullable(m map[string]any) ([]byte, error) {
	if m == nil {
		return nil, nil
	}
	return json.Marshal(m)
}

// itoa — мелкий helper для построения $N плейсхолдеров. strconv.Itoa тащит
// import; для одной маленькой функции в WHERE-builder проще inline.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [10]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
