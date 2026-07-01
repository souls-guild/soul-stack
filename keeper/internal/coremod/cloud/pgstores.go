package cloud

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/souls-guild/soul-stack/keeper/internal/bootstraptoken"
	keepersoul "github.com/souls-guild/soul-stack/keeper/internal/soul"
	"github.com/souls-guild/soul-stack/keeper/internal/soulseed"
)

// SoulPG — adapter поверх keeper/internal/soul для интерфейса [SoulStore].
type SoulPG struct {
	DB keepersoul.ExecQueryRower
}

func NewSoulPG(db keepersoul.ExecQueryRower) *SoulPG { return &SoulPG{DB: db} }

func (s *SoulPG) Insert(ctx context.Context, soul *keepersoul.Soul) error {
	return keepersoul.Insert(ctx, s.DB, soul)
}

func (s *SoulPG) UpdateStatus(ctx context.Context, sid string, status keepersoul.Status, kid *string) error {
	return keepersoul.UpdateStatus(ctx, s.DB, sid, status, kid)
}

func (s *SoulPG) DeleteBySID(ctx context.Context, sid string) error {
	return keepersoul.DeleteBySID(ctx, s.DB, sid)
}

// TokenPG — adapter поверх keeper/internal/bootstraptoken с фиксированным TTL.
// TTL берётся из cfg-поля keeper-config (через ctor); MVP — 24h, согласуется
// с рекомендацией docs/soul/onboarding.md (часы, не дни — токен это
// одноразовый capability для первого подключения).
type TokenPG struct {
	DB  bootstraptoken.ExecQueryRower
	TTL time.Duration
}

const DefaultBootstrapTokenTTL = 24 * time.Hour

func NewTokenPG(db bootstraptoken.ExecQueryRower, ttl time.Duration) *TokenPG {
	if ttl <= 0 {
		ttl = DefaultBootstrapTokenTTL
	}
	return &TokenPG{DB: db, TTL: ttl}
}

func (t *TokenPG) Generate() (bootstraptoken.PlainToken, error) {
	return bootstraptoken.Generate()
}

func (t *TokenPG) Insert(ctx context.Context, sid, tokenHash string, createdByAID *string) (*bootstraptoken.Record, error) {
	return bootstraptoken.Insert(ctx, t.DB, sid, tokenHash, t.TTL, createdByAID)
}

func (t *TokenPG) DeleteByTokenID(ctx context.Context, tokenID string) error {
	return bootstraptoken.DeleteByTokenID(ctx, t.DB, tokenID)
}

// PoolBeginner — узкое подмножество `*pgxpool.Pool`, нужное cascade-tx
// (`core.cloud.provisioned destroyed`). pgx.BeginFunc принимает интерфейс
// с одним методом Begin — этого хватает.
//
// Реализуется `*pgxpool.Pool` напрямую; для unit-тестов делается fake.
type PoolBeginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

var _ PoolBeginner = (*pgxpool.Pool)(nil)

// CascadePG — cascade-обработчик `core.cloud.provisioned destroyed` (ADR-017):
// в одной PG-транзакции переводит souls→destroyed, активные soul_seeds→orphaned,
// активные bootstrap_tokens→burned. Обособлен от SoulPG / TokenPG, потому что
// атомарность всей цепочки требует pool с BeginFunc, а не узкий ExecQueryRower.
type CascadePG struct {
	Pool PoolBeginner
}

// NewCascadePG строит adapter.
func NewCascadePG(pool PoolBeginner) *CascadePG {
	return &CascadePG{Pool: pool}
}

// CascadeCounts — агрегированные результаты [CascadePG.CascadeDestroy]
// для audit-payload и observability.
type CascadeCounts struct {
	SoulsUpdated  int64
	SeedsOrphaned int64
	TokensBurned  int64
}

// CascadeDestroy — в одной PG-транзакции (ADR-017 cascade):
//
//	UPDATE souls           SET status='destroyed'
//	   WHERE sid IN (sids)
//	UPDATE soul_seeds      SET status='orphaned'
//	   WHERE sid IN (sids) AND status='active'
//	UPDATE bootstrap_tokens SET used_at=NOW(), used_by_kid=$usedByKID
//	   WHERE sid IN (sids) AND used_at IS NULL
//
// Возвращает агрегированные счётчики (для audit-payload и observability).
//
// Семантика частичных совпадений: пустой `sids` → no-op (counts всё 0).
// Несуществующий sid просто не затронет ни одной строки — это допустимо
// (caller передаёт sids от CloudDriver-а, не проверяя их регистрацию).
func (c *CascadePG) CascadeDestroy(ctx context.Context, sids []string, usedByKID string) (CascadeCounts, error) {
	var counts CascadeCounts
	if len(sids) == 0 {
		return counts, nil
	}
	if usedByKID == "" {
		return counts, fmt.Errorf("cloud: cascade destroy: usedByKID is empty")
	}

	err := pgx.BeginFunc(ctx, c.Pool, func(tx pgx.Tx) error {
		for _, sid := range sids {
			if err := keepersoul.UpdateStatus(ctx, tx, sid, keepersoul.StatusDestroyed, nil); err != nil {
				if errors.Is(err, keepersoul.ErrSoulNotFound) {
					// Допустимо: SID отсутствует в реестре (race с manual-cleanup
					// или повторная destroy). Не двигаем counts, не валим tx.
					continue
				}
				return fmt.Errorf("souls destroyed for %q: %w", sid, err)
			}
			counts.SoulsUpdated++
		}
		for _, sid := range sids {
			n, err := soulseed.OrphanActiveBySID(ctx, tx, sid)
			if err != nil {
				return fmt.Errorf("soul_seeds orphaned for %q: %w", sid, err)
			}
			counts.SeedsOrphaned += n
		}
		for _, sid := range sids {
			n, err := bootstraptoken.BurnAllForSID(ctx, tx, sid, usedByKID)
			if err != nil {
				return fmt.Errorf("bootstrap_tokens burned for %q: %w", sid, err)
			}
			counts.TokensBurned += n
		}
		return nil
	})
	if err != nil {
		return CascadeCounts{}, err
	}
	return counts, nil
}
