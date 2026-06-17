package toll

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

// BaselineReader — узкая поверхность для Leader: вернуть текущий снимок числа
// `souls.status='connected'`. Реализация по умолчанию — [PGBaselineReader]
// поверх pgxpool; интерфейс позволяет fake-у в unit-тестах Leader-а без живого
// PG (см. leader_test.go::fakeBaseline).
type BaselineReader interface {
	BaselineConnected(ctx context.Context) (int64, error)
}

// PGQuerier — узкий контракт `QueryRow` поверх *pgxpool.Pool / pgx.Conn.
// Сужение до одного метода держит [PGBaselineReader] лёгким на test-fake и
// принимает любой источник pgx (pool/conn/tx). Соответствует сигнатуре
// pgxpool.Pool.QueryRow.
type PGQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// cachedBaseline — Leader-side кеш с TTL. SELECT COUNT(*) FROM souls дёшев
// (индекс по status), но Leader тикает каждые 5s — кеш на 60s ([Config.WindowSize])
// уменьшает PG-нагрузку в 12 раз. Мутекс — потому что кеш живёт между тиками
// (один Leader-loop), но Leader-loop одна горутина, мутекс — на случай
// расширения (например, expose чтения baseline на /metrics).
type cachedBaseline struct {
	reader BaselineReader
	ttl    time.Duration

	mu        sync.Mutex
	value     int64
	fetchedAt time.Time
	hasValue  bool
}

func newCachedBaseline(reader BaselineReader, ttl time.Duration) *cachedBaseline {
	return &cachedBaseline{reader: reader, ttl: ttl}
}

// get возвращает cached-значение если оно свежее, иначе делает fetch. На
// ошибке fetch-а — возвращает err И stale-значение (если есть): leader сам
// решит, использовать stale (лучше, чем баг false-positive на пустом baseline-е)
// или пропустить тик. Параметр now — для тестируемости (test инжектит clock).
func (c *cachedBaseline) get(ctx context.Context, now time.Time) (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.hasValue && now.Sub(c.fetchedAt) < c.ttl {
		return c.value, nil
	}
	v, err := c.reader.BaselineConnected(ctx)
	if err != nil {
		// Stale-fallback: если уже было значение — возвращаем его И ошибку.
		// Caller (Leader) различит по err != nil и решит сам.
		if c.hasValue {
			return c.value, err
		}
		return 0, err
	}
	c.value = v
	c.fetchedAt = now
	c.hasValue = true
	return v, nil
}

// PGBaselineReader — production-impl поверх pgxpool. SELECT COUNT(*) FROM
// souls WHERE status='connected'. Колонка status — read-only в этой query
// (ADR-006 amend: presence теперь Redis-lease, но souls.status последний
// known-good — достаточная аппроксимация baseline для cluster-level metric-а).
type PGBaselineReader struct {
	pool PGQuerier
}

// NewPGBaselineReader собирает reader поверх pgxpool (через адаптер pgxToQuerier).
// Принимает узкий [PGQuerier]: caller (daemon) оборачивает *pgxpool.Pool через
// тривиальный adapter, чтобы пакет toll не тянул pgx как direct dep.
func NewPGBaselineReader(pool PGQuerier) (*PGBaselineReader, error) {
	if pool == nil {
		return nil, errors.New("toll.NewPGBaselineReader: nil pool")
	}
	return &PGBaselineReader{pool: pool}, nil
}

// BaselineConnected — SELECT COUNT(*) FROM souls WHERE status='connected'.
//
// Возвращает 0 без ошибки на пустой таблице (нормальный путь для свежего
// кластера) — Leader интерпретирует baseline=0 как «делить не на что, ratio
// неопределённый, degraded не взводим» (ADR-038 защита от деления на ноль).
func (r *PGBaselineReader) BaselineConnected(ctx context.Context) (int64, error) {
	var n int64
	row := r.pool.QueryRow(ctx, `SELECT COUNT(*) FROM souls WHERE status = 'connected'`)
	if err := row.Scan(&n); err != nil {
		return 0, fmt.Errorf("toll.BaselineConnected: scan: %w", err)
	}
	return n, nil
}
