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

// PresenceChecker answers which SIDs have an agent holding a live EventStream —
// the Redis SID-lease, the same source the onboarding barrier polls.
type PresenceChecker interface {
	SoulsStreamAlive(ctx context.Context, sids []string) (map[string]struct{}, error)
}

// IssuerPG implements Issuer over the Keeper identity registries.
type IssuerPG struct {
	Pool TxBeginner
	TTL  time.Duration
	// Presence decides whether a host holding an identity is passed through
	// (NIM-886). nil fails closed the moment a batch holds such a host.
	Presence PresenceChecker
}

// NewIssuerPG wires the public issued-state to Keeper Postgres and to the
// presence lease. A non-positive TTL falls back to the canonical bootstrap-token
// TTL.
func NewIssuerPG(pool TxBeginner, presence PresenceChecker, ttl time.Duration) *IssuerPG {
	if ttl <= 0 {
		ttl = bootstraptoken.DefaultTokenTTL
	}
	return &IssuerPG{Pool: pool, TTL: ttl, Presence: presence}
}

// presenceReadTimeout bounds the lease read, which runs while the batch holds
// FOR UPDATE on every Soul row in it: a Redis that hangs must not hold a
// concurrent Bootstrap of those hosts for the client's whole retry budget.
const presenceReadTimeout = 5 * time.Second

// NoAgentHost is one host [NoAgentError] refuses: what the registry holds for
// it. LastSeenAt nil means no stream on record.
type NoAgentHost struct {
	SID        string
	Status     string
	LastSeenAt *time.Time
}

// NoAgentError refuses a batch holding hosts that own an identity in the
// registry while no agent is connected with it (NIM-886). Passing such a host
// through as onboarded is what made a machine re-created under a known SID skip
// its install, report success, and fail fifteen minutes later at the barrier:
// the registry cannot tell a re-created machine from a stopped agent, and only
// a live stream proves the identity is still held. Every such host that reaches
// the check is named at once, so a re-created group is repaired in one pass.
//
// Which way out applies is the operator's to know, and the wrong one costs: a
// host forgotten while its disk still holds the seed cannot onboard again, as
// the install's seed guard skips `soul init` and the agent presents a seed the
// Keeper no longer has.
type NoAgentError struct {
	Hosts []NoAgentHost
}

func (e *NoAgentError) Error() string {
	parts := make([]string, 0, len(e.Hosts))
	for _, h := range e.Hosts {
		seen := "no stream on record"
		if h.LastSeenAt != nil {
			seen = "last stream on record " + h.LastSeenAt.UTC().Format(time.RFC3339)
		}
		parts = append(parts, fmt.Sprintf("%q (status %s, %s)", h.SID, h.Status, seen))
	}
	return fmt.Sprintf("%d host(s) hold an identity in the registry but no agent is connected with it: %s. "+
		"If a machine was re-created under the same SID it cannot use that identity: forget the record "+
		"(DELETE /v1/souls/{sid}) and repeat the run. If it is the same machine, bring its agent up and repeat — "+
		"forgetting a host whose disk still holds its seed leaves it unable to onboard",
		len(e.Hosts), strings.Join(parts, ", "))
}

const insertPendingSoulSQL = `
INSERT INTO souls (
    sid, transport, status, coven, traits, registered_at, requested_at
) VALUES ($1, 'agent', 'pending', '{}', '{}'::jsonb, NOW(), NOW())
ON CONFLICT (sid) DO NOTHING
`

const selectSoulForIssueSQL = `
SELECT transport, status, last_seen_at
FROM souls
WHERE sid = $1
FOR UPDATE
`

// hasActiveSeedSQL asks the question `souls.status` only approximates: does
// this host already hold a machine identity?
//
// It is the seed, not the status, that decides whether issuing a token is safe
// (NIM-865). Bootstrap used to flip the row to `connected` the instant it
// signed a certificate, so the status happened to answer this — but it answered
// by asserting a stream that did not exist, and a host whose Bootstrap reply
// was lost then read `connected` forever. With the status left honest at
// `pending` until a stream really opens, a status-only test would send exactly
// that host — and every host in the ordinary window between onboarding and its
// first connect — down the re-arm path below, minting it a fresh token while an
// active seed is on file. That token supersedes the seed for whoever redeems
// it, which is the identity takeover the `connected` arm refuses by name.
const hasActiveSeedSQL = `
SELECT EXISTS (
    SELECT 1 FROM soul_seeds
     WHERE sid = $1 AND status = 'active'
)
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
//   - pending/expired agent SID holding an ACTIVE (unused, unexpired) token, with
//     reissue false: nothing is written at all, [IssuedHost.TokenHeld] (NIM-900);
//   - pending/expired agent SID otherwise: refresh pending requested_at,
//     invalidate any unused token (expired or not), then issue a fresh
//     default-TTL token;
//   - connected/disconnected agent SID that is this run's own: passed through
//     untouched and tokenless, [IssuedHost.Onboarded] (NIM-780) — but only while
//     an agent holds a live stream with it; otherwise the batch is refused with
//     [NoAgentError] (NIM-886);
//   - connected/disconnected held by another incarnation, revoked/destroyed, or
//     transport=ssh: fail closed.
//
// incarnationName is the run's incarnation, from [util.IncarnationFrom]. It is
// what separates the last two cases, so "" (unknown) leaves only unbound rows
// eligible for pass-through — see [keepersoul.OwnedByRun].
//
// reissue reaches only the second case. The onboarding guard is decided before
// it and is not relaxed by it: a host holding an identity is judged identically
// under either value.
//
// Any failure rolls back the entire batch. Plain tokens are returned only after
// Commit succeeds and are never stored in Postgres (only SHA-256 hashes are).
func (i *IssuerPG) IssueBatch(ctx context.Context, sids []string, incarnationName string, reissue bool) ([]IssuedHost, error) {
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
	var onboarded []NoAgentHost
	for _, sid := range sids {
		host, err := i.issueOne(ctx, tx, sid, incarnationName, reissue)
		if err != nil {
			return nil, &SIDIssueError{SID: sid, Err: err}
		}
		if host.Onboarded {
			onboarded = append(onboarded, host.registered)
		}
		out = append(out, host)
	}
	if err := i.requireLiveAgents(ctx, onboarded); err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("bootstrap issuer: commit batch: %w", err)
	}
	committed = true
	return out, nil
}

// requireLiveAgents is the second half of the onboarded arm: an identity in the
// registry is not enough to be passed through, an agent has to hold it now. It
// runs before COMMIT, so a refusal rolls back every write the batch made.
func (i *IssuerPG) requireLiveAgents(ctx context.Context, hosts []NoAgentHost) error {
	if len(hosts) == 0 {
		return nil
	}
	if i.Presence == nil {
		return fmt.Errorf("bootstrap issuer: %d host(s) hold an identity, and with no presence checker configured nothing can tell whether their agents are connected", len(hosts))
	}
	sids := make([]string, len(hosts))
	for k, h := range hosts {
		sids[k] = h.SID
	}
	pctx, cancel := context.WithTimeout(ctx, presenceReadTimeout)
	defer cancel()
	alive, err := i.Presence.SoulsStreamAlive(pctx, sids)
	if err != nil {
		return fmt.Errorf("bootstrap issuer: check agent presence: %w", err)
	}
	var absent []NoAgentHost
	for _, h := range hosts {
		if _, ok := alive[h.SID]; !ok {
			absent = append(absent, h)
		}
	}
	if len(absent) > 0 {
		return &NoAgentError{Hosts: absent}
	}
	return nil
}

func (i *IssuerPG) issueOne(ctx context.Context, tx pgx.Tx, sid, incarnationName string, reissue bool) (IssuedHost, error) {
	if !keepersoul.ValidSID(sid) || keepersoul.IsReservedSID(sid) {
		return IssuedHost{}, fmt.Errorf("invalid sid")
	}
	tag, err := tx.Exec(ctx, insertPendingSoulSQL, sid)
	if err != nil {
		return IssuedHost{}, fmt.Errorf("create pending Soul: %w", err)
	}
	created := tag.RowsAffected() == 1

	var transport, status string
	var lastSeenAt *time.Time
	if err := tx.QueryRow(ctx, selectSoulForIssueSQL, sid).Scan(&transport, &status, &lastSeenAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return IssuedHost{}, fmt.Errorf("Soul disappeared while locking it")
		}
		return IssuedHost{}, fmt.Errorf("lock Soul: %w", err)
	}
	if transport != string(keepersoul.TransportAgent) {
		return IssuedHost{}, fmt.Errorf("transport is %q, want %q", transport, keepersoul.TransportAgent)
	}
	// Read under the same row lock as the status, so nothing can onboard between
	// the two reads and slip past the onboarded arm below.
	var hasActiveSeed bool
	if err := tx.QueryRow(ctx, hasActiveSeedSQL, sid).Scan(&hasActiveSeed); err != nil {
		return IssuedHost{}, fmt.Errorf("check active seed: %w", err)
	}

	// An active seed means the host holds an identity, so it is judged by the
	// onboarded arm even while it is still `pending`, waiting to open its first
	// stream. See [hasActiveSeedSQL].
	//
	// Only those two statuses are redirected, deliberately: `revoked` and
	// `destroyed` are operator and cascade decisions that must keep falling to
	// the refusal below, and a leftover active seed under one of them is a
	// registry inconsistency, not a reason to call the host onboarded.
	effective := keepersoul.Status(status)
	if hasActiveSeed && (effective == keepersoul.StatusPending || effective == keepersoul.StatusExpired) {
		effective = keepersoul.StatusConnected
	}

	switch effective {
	case keepersoul.StatusPending, keepersoul.StatusExpired:
		// Idempotence, and it has to come before every write below (NIM-900). With
		// `reissue: false` a host already holding a redeemable token is left exactly
		// as it is — not re-armed, not re-marked, not re-minted — and reported by the
		// flag. Issuing anyway would have killed a capability the operator may have
		// already put on the machine, and it is not recoverable afterwards: only the
		// SHA-256 is on file, so a second run cannot hand back the plaintext the
		// first one did. The read is under the Soul's row lock taken above, so a
		// concurrent Bootstrap cannot burn the token between the answer and the act.
		if !reissue {
			held, herr := bootstraptoken.HasActiveBySID(ctx, tx, sid)
			if herr != nil {
				return IssuedHost{}, herr
			}
			if held {
				return IssuedHost{SID: sid, TokenHeld: true}, nil
			}
		}
		// A fresh token gets a fresh pending window too. This also re-arms an
		// explicitly expired-but-never-onboarded record without touching identity.
		if _, err := tx.Exec(ctx, refreshPendingSoulSQL, sid); err != nil {
			return IssuedHost{}, fmt.Errorf("refresh pending Soul: %w", err)
		}
		// That statement sets `last_seen_at = NULL`, which is the predicate the
		// reply-loss recovery reads as "this host has never held a stream"
		// (NIM-865). A host that HAD streamed, and still carries an unexpired
		// token burned by a real KID, would therefore have its recovery window
		// re-opened by the re-arm — so the burned tokens are disarmed in the
		// same transaction. ExpireActiveBySID below cannot do it: it matches
		// `used_at IS NULL`.
		if _, err := bootstraptoken.CloseRecoveryBySID(
			ctx, tx, sid, bootstraptoken.SystemKIDBootstrapIssuedReissue,
		); err != nil {
			return IssuedHost{}, fmt.Errorf("close recovery window: %w", err)
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
		//
		// Owned is not yet passed through: [IssuerPG.requireLiveAgents] still asks
		// whether an agent holds this identity now, once for the whole batch.
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
			return IssuedHost{}, fmt.Errorf("Soul is already onboarded (status %q, active seed %t) and is not this run's own (%s); refusing identity takeover", status, hasActiveSeed, owner)
		}
		return IssuedHost{
			SID:        sid,
			Onboarded:  true,
			registered: NoAgentHost{SID: sid, Status: status, LastSeenAt: lastSeenAt},
		}, nil
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
