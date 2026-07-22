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

// SoulPG is adapter over keeper/internal/soul for [SoulStore] interface.
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

// TokenPG is adapter over keeper/internal/bootstraptoken with fixed TTL.
// TTL taken from cfg-field keeper-config (via ctor); MVP is 24h, aligns
// with docs/soul/onboarding.md recommendation (hours, not days — token is
// one-time capability for first connection).
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

// PoolBeginner is narrow subset of `*pgxpool.Pool` needed for cascade-tx
// (`core.cloud.provisioned destroyed`). pgx.BeginFunc takes interface
// with one Begin method — suffices.
//
// Implemented by `*pgxpool.Pool` directly; for unit tests use fake.
type PoolBeginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

var _ PoolBeginner = (*pgxpool.Pool)(nil)

// CascadePG is cascade handler for `core.cloud.provisioned destroyed` (ADR-017):
// in single PG transaction moves souls→destroyed, active soul_seeds→orphaned,
// active bootstrap_tokens→burned. Separate from SoulPG / TokenPG because
// atomicity of whole chain requires pool with BeginFunc, not narrow ExecQueryRower.
type CascadePG struct {
	Pool PoolBeginner
}

// NewCascadePG builds adapter.
func NewCascadePG(pool PoolBeginner) *CascadePG {
	return &CascadePG{Pool: pool}
}

// CascadeCounts is aggregated results of [CascadePG.CascadeDestroy]
// for audit-payload and observability.
type CascadeCounts struct {
	SoulsUpdated  int64
	SeedsOrphaned int64
	TokensBurned  int64
}

// CascadeDestroy in single PG transaction (ADR-017 cascade):
//
//	UPDATE souls           SET status='destroyed'
//	   WHERE sid IN (sids)
//	UPDATE soul_seeds      SET status='orphaned'
//	   WHERE sid IN (sids) AND status='active'
//	UPDATE bootstrap_tokens SET used_at=NOW(), used_by_kid=$usedByKID
//	   WHERE sid IN (sids) AND used_at IS NULL
//
// Returns aggregated counters (for audit-payload and observability).
//
// Partial match semantics: empty `sids` → no-op (all counts 0).
// Non-existent sid simply doesn't affect any rows — allowed
// (caller provides sids from CloudDriver without verification).
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
					// Allowed: SID missing from registry (race with manual-cleanup
					// or re-destroy). Don't move counts, don't fail tx.
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
