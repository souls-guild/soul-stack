package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"strings"
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
// onboarded status and decides on it, or invalidates the old unused token
// before a fresh one is committed.
//
// Reissue semantics:
//   - absent SID: create pending/agent + issue;
//   - pending/expired agent SID: refresh pending requested_at, invalidate any
//     unused token (expired or not), then issue a fresh default-TTL token;
//   - connected/disconnected agent SID that is this run's own: passed through
//     untouched and tokenless, [IssuedHost.Onboarded] (NIM-780);
//   - connected/disconnected held by another incarnation, revoked/destroyed, or
//     transport=ssh: fail closed.
//
// incarnationName is the run's incarnation, from [util.IncarnationFrom]. It is
// what separates the last two cases, so "" (unknown) leaves only unbound rows
// eligible for pass-through — see [keepersoul.OwnedByRun].
//
// Any failure rolls back the entire batch. Plain tokens are returned only after
// Commit succeeds and are never stored in Postgres (only SHA-256 hashes are).
func (i *IssuerPG) IssueBatch(ctx context.Context, sids []string, incarnationName string) ([]IssuedHost, error) {
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
		host, err := i.issueOne(ctx, tx, sid, incarnationName)
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

func (i *IssuerPG) issueOne(ctx context.Context, tx pgx.Tx, sid, incarnationName string) (IssuedHost, error) {
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
		// Converge in place rather than refuse, for a host this run already
		// onboarded (NIM-780): a `create` that failed AFTER onboarding — on the
		// rollout, on cluster assembly — is repeated to finish, and the cloud
		// plugin idempotently hands back the same machines. Refusing here made
		// that run unfixable, since the scenario has no roster to narrow the
		// list with and an empty list is refused too.
		//
		// This is the exemption core.cloud.created carried as ProvisionExisting
		// (NIM-189), moved to the side that mints now that the cloud driver is an
		// ordinary plugin knowing nothing about Souls (epic NIM-757). Ownership is
		// judged by the same predicate: a row bound only to other incarnations is
		// still an identity takeover, and is still refused.
		//
		// Nothing is written: no token to issue (a bootstrap token is a one-time
		// capability and this host has a seed to authenticate with), and no
		// pending refresh — re-arming a live host would wipe its presence and
		// expose it to the Reaper's pending sweep while its stream is up.
		owned, oerr := keepersoul.OwnedByRun(ctx, tx, sid, incarnationName)
		if oerr != nil {
			return IssuedHost{}, oerr
		}
		if !owned {
			// Name the holder, not just the fact of one. The provision path gives
			// exactly this for exactly this predicate (keepersoul's
			// notProvisionableError), and an operator repeating `create` onto a
			// foreign SID otherwise has nothing to go on but the SID they already
			// typed. A read failure here must not mask the refusal, so the owner
			// clause is simply dropped when it cannot be read.
			owner := "owner unknown"
			if _, incarnations, derr := keepersoul.DescribeTakenSID(ctx, tx, sid); derr == nil {
				if len(incarnations) > 0 {
					owner = "member of incarnation " + strings.Join(incarnations, ", ")
				} else {
					owner = "not a member of any incarnation"
				}
			}
			return IssuedHost{}, fmt.Errorf("Soul is already onboarded (status %q) and is not this run's own (%s); refusing identity takeover", status, owner)
		}
		return IssuedHost{SID: sid, Onboarded: true}, nil
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
