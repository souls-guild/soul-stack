package oracle

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/souls-guild/soul-stack/keeper/internal/pgutil"
)

const (
	pgErrCodeUniqueViolation     = "23505"
	pgErrCodeForeignKeyViolation = "23503"
	pgErrCodeCheckViolation      = "23514"
)

// Sentinel-ошибки реестра.
var (
	ErrVigilNotFound  = errors.New("oracle: vigil not found")
	ErrDecreeNotFound = errors.New("oracle: decree not found")
)

const vigilColumns = `name, coven, sid, interval_spec, check_addr, params, enabled, created_at, updated_at, created_by_aid`

// SelectActiveVigilsForSubject возвращает enabled-Vigil-ы, активные для
// субъекта (sid-Vigil с vigils.sid == sid ИЛИ coven-Vigil с пересечением
// vigils.coven ∩ covens). Резолв набора для VigilSnapshot на connect.
//
// Пустой covens допустим (тогда только sid-Vigil-ы). Сортировка `name ASC` —
// детерминированный порядок snapshot-а (ReplaceAll на Soul-е не зависит от
// порядка, но стабильность упрощает тесты/диагностику).
func SelectActiveVigilsForSubject(ctx context.Context, db ExecQueryRower, sid string, covens []string) ([]*Vigil, error) {
	const sql = `SELECT ` + vigilColumns + `
FROM vigils
WHERE enabled AND (sid = $1 OR coven && $2)
ORDER BY name ASC`
	rows, err := db.Query(ctx, sql, sid, covens)
	if err != nil {
		return nil, fmt.Errorf("oracle: list vigils by subject query: %w", err)
	}
	return collectVigils(rows)
}

func collectVigils(rows pgx.Rows) ([]*Vigil, error) {
	defer rows.Close()
	var out []*Vigil
	for rows.Next() {
		v, err := scanVigil(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("oracle: list vigils iter: %w", err)
	}
	return out, nil
}

func scanVigil(row pgx.Row) (*Vigil, error) {
	v := &Vigil{}
	err := row.Scan(
		&v.Name, &v.Coven, &v.SID, &v.IntervalSpec, &v.CheckAddr,
		&v.Params, &v.Enabled, &v.CreatedAt, &v.UpdatedAt, &v.CreatedByAID,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrVigilNotFound
		}
		return nil, fmt.Errorf("oracle: scan vigil: %w", err)
	}
	return v, nil
}

// SelectVigilByName читает Vigil по PK. [ErrVigilNotFound] при pgx.ErrNoRows.
func SelectVigilByName(ctx context.Context, db ExecQueryRower, name string) (*Vigil, error) {
	const sql = `SELECT ` + vigilColumns + `
FROM vigils
WHERE name = $1`
	return scanVigil(db.QueryRow(ctx, sql, name))
}

// SelectAllVigils возвращает страницу Vigil-ов и общее количество (sort
// created_at DESC, name ASC — симметрично [augur.SelectAllOmens]). Total и
// items — двумя запросами вне общей транзакции (eventually consistent).
func SelectAllVigils(ctx context.Context, db ExecQueryRower, offset, limit int) ([]*Vigil, int, error) {
	if offset < 0 {
		return nil, 0, fmt.Errorf("oracle: offset must be >= 0, got %d", offset)
	}
	if limit < 1 {
		return nil, 0, fmt.Errorf("oracle: limit must be >= 1, got %d", limit)
	}

	var total int
	if err := db.QueryRow(ctx, "SELECT COUNT(*) FROM vigils").Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("oracle: count vigils: %w", err)
	}

	const listSQL = `SELECT ` + vigilColumns + `
FROM vigils
ORDER BY created_at DESC, name ASC
OFFSET $1 LIMIT $2`
	rows, err := db.Query(ctx, listSQL, offset, limit)
	if err != nil {
		return nil, 0, fmt.Errorf("oracle: list vigils query: %w", err)
	}
	out, err := collectVigils(rows)
	if err != nil {
		return nil, 0, err
	}
	return out, total, nil
}

// DeleteVigil удаляет Vigil по PK. [ErrVigilNotFound] если строки не было.
// Decree-ы НЕ каскадятся: decrees.on_beacon — text-ссылка без FK (Decree
// managed-реестр, переживает пересоздание Vigil-а), удаление Vigil-а лишь
// перестаёт раздавать его в VigilSnapshot.
func DeleteVigil(ctx context.Context, db ExecQueryRower, name string) error {
	tag, err := db.Exec(ctx, "DELETE FROM vigils WHERE name = $1", name)
	if err != nil {
		return fmt.Errorf("oracle: delete vigil: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrVigilNotFound
	}
	return nil
}

const decreeColumns = `name, on_beacon, where_cel, subject_coven, subject_sid, incarnation_name, action_scenario, action_input, cooldown, enabled, created_at, updated_at, created_by_aid`

// SelectDecreesByBeacon возвращает enabled-Decree-ы, реагирующие на указанный
// Vigil (decrees.on_beacon == beacon). Горячий путь match-флоу: Oracle на
// каждый Portent делает этот SELECT, далее фильтрует субъектом + where-CEL +
// cooldown (см. [Match]). Default-deny: пустой результат → событие игнорируется.
//
// Сортировка `name ASC` — детерминированный порядок обработки нескольких
// матчащих Decree-ов на одно событие.
func SelectDecreesByBeacon(ctx context.Context, db ExecQueryRower, beacon string) ([]*Decree, error) {
	const sql = `SELECT ` + decreeColumns + `
FROM decrees
WHERE enabled AND on_beacon = $1
ORDER BY name ASC`
	rows, err := db.Query(ctx, sql, beacon)
	if err != nil {
		return nil, fmt.Errorf("oracle: list decrees by beacon query: %w", err)
	}
	return collectDecrees(rows)
}

func collectDecrees(rows pgx.Rows) ([]*Decree, error) {
	defer rows.Close()
	var out []*Decree
	for rows.Next() {
		d, err := scanDecree(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("oracle: list decrees iter: %w", err)
	}
	return out, nil
}

func scanDecree(row pgx.Row) (*Decree, error) {
	d := &Decree{}
	err := row.Scan(
		&d.Name, &d.OnBeacon, &d.WhereCEL, &d.SubjectCoven, &d.SubjectSID,
		&d.IncarnationName, &d.ActionScenario, &d.ActionInput, &d.Cooldown,
		&d.Enabled, &d.CreatedAt, &d.UpdatedAt, &d.CreatedByAID,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrDecreeNotFound
		}
		return nil, fmt.Errorf("oracle: scan decree: %w", err)
	}
	return d, nil
}

// SelectDecreeByName читает Decree по PK. [ErrDecreeNotFound] при pgx.ErrNoRows.
func SelectDecreeByName(ctx context.Context, db ExecQueryRower, name string) (*Decree, error) {
	const sql = `SELECT ` + decreeColumns + `
FROM decrees
WHERE name = $1`
	return scanDecree(db.QueryRow(ctx, sql, name))
}

// SelectAllDecrees возвращает страницу Decree-ов и общее количество (sort
// created_at DESC, name ASC). Total и items — двумя запросами вне общей
// транзакции (eventually consistent).
func SelectAllDecrees(ctx context.Context, db ExecQueryRower, offset, limit int) ([]*Decree, int, error) {
	if offset < 0 {
		return nil, 0, fmt.Errorf("oracle: offset must be >= 0, got %d", offset)
	}
	if limit < 1 {
		return nil, 0, fmt.Errorf("oracle: limit must be >= 1, got %d", limit)
	}

	var total int
	if err := db.QueryRow(ctx, "SELECT COUNT(*) FROM decrees").Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("oracle: count decrees: %w", err)
	}

	const listSQL = `SELECT ` + decreeColumns + `
FROM decrees
ORDER BY created_at DESC, name ASC
OFFSET $1 LIMIT $2`
	rows, err := db.Query(ctx, listSQL, offset, limit)
	if err != nil {
		return nil, 0, fmt.Errorf("oracle: list decrees query: %w", err)
	}
	out, err := collectDecrees(rows)
	if err != nil {
		return nil, 0, err
	}
	return out, total, nil
}

// DeleteDecree удаляет Decree по PK. Его cooldown-state в `oracle_fires` уходит
// каскадом (ON DELETE CASCADE, миграция 041). [ErrDecreeNotFound] если строки
// не было.
func DeleteDecree(ctx context.Context, db ExecQueryRower, name string) error {
	tag, err := db.Exec(ctx, "DELETE FROM decrees WHERE name = $1", name)
	if err != nil {
		return fmt.Errorf("oracle: delete decree: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrDecreeNotFound
	}
	return nil
}

// LastFiredAt возвращает время последнего срабатывания пары (decree, subject)
// из `oracle_fires` (cooldown-state, ADR-030(a)). (zero, false, nil) — пара
// ещё не срабатывала (строки нет): cooldown не активен. Используется
// [WithinCooldown].
func LastFiredAt(ctx context.Context, db ExecQueryRower, decree, subject string) (time.Time, bool, error) {
	const sql = `SELECT fired_at FROM oracle_fires WHERE decree = $1 AND subject = $2`
	var firedAt time.Time
	err := db.QueryRow(ctx, sql, decree, subject).Scan(&firedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return time.Time{}, false, nil
		}
		return time.Time{}, false, fmt.Errorf("oracle: last fired query: %w", err)
	}
	return firedAt, true, nil
}

// RecordFire фиксирует срабатывание пары (decree, subject) в `oracle_fires`
// (cooldown-state, ADR-030(a)). UPSERT: одна строка на пару, fired_at
// обновляется на now (НЕ append-only — таблица остаётся ограниченной размером
// числа уникальных пар). Вызывается ПОСЛЕ успешной постановки scenario.
//
// firedAt — момент срабатывания (caller передаёт единое время, согласованное
// с cooldown-check-ом и audit-ом). FK-violation на отсутствующий decree
// маппится в обёрнутую ошибку (программная ошибка caller-а: Decree был прочитан
// match-ем, но удалён до record).
func RecordFire(ctx context.Context, db ExecQueryRower, decree, subject string, firedAt time.Time) error {
	const sql = `
INSERT INTO oracle_fires (decree, subject, fired_at)
VALUES ($1, $2, $3)
ON CONFLICT (decree, subject) DO UPDATE SET fired_at = EXCLUDED.fired_at`
	if _, err := db.Exec(ctx, sql, decree, subject, firedAt); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgErrCodeForeignKeyViolation {
			return fmt.Errorf("oracle: record fire FK violation on %s: %w", pgErr.ConstraintName, err)
		}
		return fmt.Errorf("oracle: record fire: %w", err)
	}
	return nil
}

// BumpCircuit атомарно инкрементирует per-decree fixed-window счётчик
// срабатываний в `oracle_circuit` (circuit-breaker, ADR-030(a), beacons S4) и
// возвращает счётчик ПОСЛЕ инкремента. window — длина окна; now — момент
// срабатывания (тот же, что cooldown/audit).
//
// Семантика fixed-window: если окно текущей строки истекло
// (window_start ≤ now - window), оно сбрасывается (window_start = now,
// fire_count = 1); иначе fire_count += 1. Первое срабатывание (строки нет) —
// INSERT с fire_count = 1.
//
// Cluster-safe: один statement INSERT … ON CONFLICT DO UPDATE … RETURNING под
// row-lock-ом одной строки сериализует read-modify-write — конкурентные
// BumpCircuit с разных Keeper-инстансов на один Decree не теряют инкремент.
// FK-violation на отсутствующий Decree — программная ошибка caller-а (Decree
// прочитан match-ем, но удалён до bump-а): обёрнутая ошибка.
func BumpCircuit(ctx context.Context, db ExecQueryRower, decree string, now time.Time, window time.Duration) (int, error) {
	const sql = `
INSERT INTO oracle_circuit (decree, window_start, fire_count)
VALUES ($1, $2, 1)
ON CONFLICT (decree) DO UPDATE SET
  window_start = CASE WHEN oracle_circuit.window_start <= $2 - $3::interval THEN $2 ELSE oracle_circuit.window_start END,
  fire_count   = CASE WHEN oracle_circuit.window_start <= $2 - $3::interval THEN 1 ELSE oracle_circuit.fire_count + 1 END
RETURNING fire_count`
	var fireCount int
	err := db.QueryRow(ctx, sql, decree, now, pgutil.Interval(window)).Scan(&fireCount)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgErrCodeForeignKeyViolation {
			return 0, fmt.Errorf("oracle: bump circuit FK violation on %s: %w", pgErr.ConstraintName, err)
		}
		return 0, fmt.Errorf("oracle: bump circuit: %w", err)
	}
	return fireCount, nil
}

// TripDecree авто-disable-ит Decree circuit-breaker-ом: переводит enabled
// true→false (ADR-030(a)). Возвращает tripped=true, если эта операция фактически
// выключила правило (RowsAffected==1) — single-winner: при конкурентном trip-е
// с нескольких Keeper-инстансов ровно один выигрывает (`WHERE enabled=true`
// сериализует через row-lock), остальные получают RowsAffected==0 и НЕ дублируют
// alert/audit/metric. now пишется в updated_at (мутация Decree).
func TripDecree(ctx context.Context, db ExecQueryRower, decree string, now time.Time) (bool, error) {
	const sql = `UPDATE decrees SET enabled = false, updated_at = $2 WHERE name = $1 AND enabled = true`
	tag, err := db.Exec(ctx, sql, decree, now)
	if err != nil {
		return false, fmt.Errorf("oracle: trip decree: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}
