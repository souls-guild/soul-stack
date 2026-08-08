package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/souls-guild/soul-stack/keeper/internal/bootstraptoken"
	keepersoul "github.com/souls-guild/soul-stack/keeper/internal/soul"
)

// TxBeginner is the Postgres surface needed to make issuance atomic across the
// whole requested batch. A real *pgxpool.Pool satisfies it.
type TxBeginner interface {
	BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error)
}

var _ TxBeginner = (*pgxpool.Pool)(nil)

// IssuerPG implements Issuer over the Keeper identity registries.
type IssuerPG struct {
	Pool TxBeginner
	TTL  time.Duration
}

// NewIssuerPG wires the public issued-state to Keeper Postgres. A non-positive
// TTL falls back to the canonical bootstrap-token TTL.
func NewIssuerPG(pool TxBeginner, ttl time.Duration) *IssuerPG {
	if ttl <= 0 {
		ttl = bootstraptoken.DefaultTokenTTL
	}
	return &IssuerPG{Pool: pool, TTL: ttl}
}

const insertPendingSoulSQL = `
INSERT INTO souls (
    sid, transport, status, coven, traits, registered_at, requested_at
) VALUES ($1, 'agent', 'pending', '{}', '{}'::jsonb, NOW(), NOW())
ON CONFLICT (sid) DO NOTHING
`

const selectSoulForIssueSQL = `
SELECT transport, status
FROM souls
WHERE sid = $1
FOR UPDATE
`

const refreshPendingSoulSQL = `
UPDATE souls
SET status           = 'pending',
    requested_at     = NOW(),
    last_seen_at     = NULL,
    last_seen_by_kid = NULL
WHERE sid = $1
`

// IssueBatch creates/checks every pending agent Soul and issues exactly one
// fresh token per SID in one transaction. The row lock closes the race with a
// concurrent Bootstrap RPC: after waiting, issuance either observes an
// onboarded status and refuses, or invalidates the old unused token before a
// fresh one is committed.
//
// Reissue semantics:
//   - absent SID: create pending/agent + issue;
//   - pending/expired agent SID: refresh pending requested_at, invalidate any
//     unused token (expired or not), then issue a fresh default-TTL token;
//   - connected/disconnected/revoked/destroyed or transport=ssh: fail closed.
//
// Any failure rolls back the entire batch. Plain tokens are returned only after
// Commit succeeds and are never stored in Postgres (only SHA-256 hashes are).
func (i *IssuerPG) IssueBatch(ctx context.Context, sids []string) ([]IssuedHost, error) {
	if i == nil || i.Pool == nil {
		return nil, fmt.Errorf("bootstrap issuer: postgres pool is nil")
	}
	if i.TTL <= 0 {
		return nil, fmt.Errorf("bootstrap issuer: ttl must be positive")
	}
	if len(sids) == 0 {
		return nil, fmt.Errorf("bootstrap issuer: empty SID batch")
	}

	tx, err := i.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("bootstrap issuer: begin batch: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(context.Background())
		}
	}()

	out := make([]IssuedHost, 0, len(sids))
	for _, sid := range sids {
		host, err := i.issueOne(ctx, tx, sid)
		if err != nil {
			return nil, &SIDIssueError{SID: sid, Err: err}
		}
		out = append(out, host)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("bootstrap issuer: commit batch: %w", err)
	}
	committed = true
	return out, nil
}

func (i *IssuerPG) issueOne(ctx context.Context, tx pgx.Tx, sid string) (IssuedHost, error) {
	if !keepersoul.ValidSID(sid) || keepersoul.IsReservedSID(sid) {
		return IssuedHost{}, fmt.Errorf("invalid sid")
	}
	tag, err := tx.Exec(ctx, insertPendingSoulSQL, sid)
	if err != nil {
		return IssuedHost{}, fmt.Errorf("create pending Soul: %w", err)
	}
	created := tag.RowsAffected() == 1

	var transport, status string
	if err := tx.QueryRow(ctx, selectSoulForIssueSQL, sid).Scan(&transport, &status); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return IssuedHost{}, fmt.Errorf("Soul disappeared while locking it")
		}
		return IssuedHost{}, fmt.Errorf("lock Soul: %w", err)
	}
	if transport != string(keepersoul.TransportAgent) {
		return IssuedHost{}, fmt.Errorf("transport is %q, want %q", transport, keepersoul.TransportAgent)
	}
	switch keepersoul.Status(status) {
	case keepersoul.StatusPending, keepersoul.StatusExpired:
		// A fresh token gets a fresh pending window too. This also re-arms an
		// explicitly expired-but-never-onboarded record without touching identity.
		if _, err := tx.Exec(ctx, refreshPendingSoulSQL, sid); err != nil {
			return IssuedHost{}, fmt.Errorf("refresh pending Soul: %w", err)
		}
	case keepersoul.StatusConnected, keepersoul.StatusDisconnected:
		return IssuedHost{}, fmt.Errorf("Soul is already onboarded (status %q); refusing identity takeover", status)
	default:
		return IssuedHost{}, fmt.Errorf("Soul status %q is not eligible for bootstrap issuance", status)
	}

	_, reissued, err := bootstraptoken.ExpireActiveBySID(
		ctx, tx, sid, bootstraptoken.SystemKIDBootstrapIssuedReissue,
	)
	if err != nil {
		return IssuedHost{}, fmt.Errorf("invalidate previous bootstrap token: %w", err)
	}
	plain, err := bootstraptoken.Generate()
	if err != nil {
		return IssuedHost{}, fmt.Errorf("generate bootstrap token: %w", err)
	}
	rec, err := bootstraptoken.Insert(ctx, tx, sid, plain.Hash(), i.TTL, nil)
	if err != nil {
		return IssuedHost{}, fmt.Errorf("insert bootstrap token: %w", err)
	}
	return IssuedHost{
		SID:       sid,
		Token:     plain,
		ExpiresAt: rec.ExpiresAt,
		Created:   created,
		Reissued:  reissued,
	}, nil
}
