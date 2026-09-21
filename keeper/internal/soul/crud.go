package soul

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/souls-guild/soul-stack/keeper/internal/rbac"
	"github.com/souls-guild/soul-stack/keeper/internal/soulpurview"
)

// Sentinel errors of the CRUD layer. Handler side maps:
//   - ErrSoulAlreadyExists   → 409 soul-already-exists.
//   - ErrSoulNotFound        → 404 not-found.
//   - ErrSoulCreatorNotFound → 422 validation-failed (creator AID missing
//     from the operators registry). Symmetric to bootstraptoken.ErrTokenSoulNotFound.
//   - ErrSoulprintNotReceived → 410 gone (`GET /v1/souls/{sid}/soulprint`):
//     the Soul record exists but SoulprintReport has never arrived — empty
//     `soulprint_facts IS NULL`. Distinct from 404: the Soul itself exists.
//   - ErrSoulNotProvisionable → 409 (provision re-run over a taken SID, see
//     [EnsureProvisionable]).
var (
	ErrSoulAlreadyExists    = errors.New("soul: SID already exists")
	ErrSoulNotFound         = errors.New("soul: SID not found")
	ErrSoulCreatorNotFound  = errors.New("soul: created_by AID not found in operators registry")
	ErrSoulprintNotReceived = errors.New("soul: soulprint not yet received")
	ErrSoulNotProvisionable = errors.New("soul: SID is held by a registration a provision run must not take over")
)

const (
	pgErrCodeUniqueViolation     = "23505"
	pgErrCodeForeignKeyViolation = "23503"
	pgErrCodeCheckViolation      = "23514"
)

// ExecQueryRower — narrow subset of pgxpool.Pool needed by CRUD. Symmetric
// to [operator.ExecQueryRower] / [incarnation.ExecQueryRower]: unit tests use
// a fake without spinning up PG, production uses the real pool / Conn / Tx.
type ExecQueryRower interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

var (
	_ ExecQueryRower = (*pgx.Conn)(nil)
	_ ExecQueryRower = (*pgxpool.Pool)(nil)
	_ ExecQueryRower = (pgx.Tx)(nil)
)

const insertSQL = `
INSERT INTO souls (
    sid, transport, status, coven, traits,
    registered_at, last_seen_at, last_seen_by_kid,
    created_by_aid, requested_at, note
) VALUES ($1, $2, $3, $4, COALESCE($5, '{}'::jsonb),
    COALESCE($6, NOW()), $7, $8,
    $9, COALESCE($10, NOW()), $11)
RETURNING registered_at, requested_at
`

// insertIfFreeSQL — [insertSQL] that yields no row instead of a PK error when
// the SID is taken (NIM-170, phase 1 of [EnsureProvisionable]): empty RETURNING
// means "already registered", and the caller decides whether that row is a
// leftover it may reuse.
const insertIfFreeSQL = `
INSERT INTO souls (
    sid, transport, status, coven, traits,
    registered_at, last_seen_at, last_seen_by_kid,
    created_by_aid, requested_at, note
) VALUES ($1, $2, $3, $4, COALESCE($5, '{}'::jsonb),
    COALESCE($6, NOW()), $7, $8,
    $9, COALESCE($10, NOW()), $11)
ON CONFLICT (sid) DO NOTHING
RETURNING registered_at, requested_at
`

// ownedByRunSQL — the ownership half of the provision predicate, shared by the
// reuse and pass-through phases of [EnsureProvisionable]. The row is this run's
// own when it belongs to incarnation $2, or to no incarnation at all (souls are
// registered before core.soul.registered binds them). A row that belongs only to
// OTHER incarnations is somebody else's; an empty $2 (incarnation unknown)
// therefore leaves only unbound rows.
const ownedByRunSQL = `
  AND (
      NOT EXISTS (
          SELECT 1 FROM incarnation_membership m WHERE m.sid = souls.sid
      )
      OR EXISTS (
          SELECT 1 FROM incarnation_membership m
           WHERE m.sid = souls.sid AND m.incarnation_name = $2
      )
  )`

// reuseForProvisionSQL — phase 2 of [EnsureProvisionable] (NIM-170): re-arm the
// row an earlier provision attempt left behind, for a fresh onboarding.
// Reusable = the row never completed an onboarding (`pending`) or is a cascade
// tombstone (`destroyed`) AND it is this run's own ([ownedByRunSQL]).
// The predicate lives in the statement itself: an UPDATE that matches nothing
// is the atomic "not a leftover of mine" answer, no read-then-write window.
const reuseForProvisionSQL = `
UPDATE souls
SET status           = 'pending',
    transport        = $3,
    requested_at     = NOW(),
    last_seen_at     = NULL,
    last_seen_by_kid = NULL
WHERE sid = $1
  AND status IN ('pending', 'destroyed')` + ownedByRunSQL + `
RETURNING registered_at, requested_at
`

// existingProvisionSQL — phase 3 of [EnsureProvisionable] (NIM-189): the SID
// already carries a registration this run onboarded itself. Converge semantics —
// re-provisioning is a no-op over it, so this phase only READS: status,
// `last_seen_*` and the host's identity are left exactly as they are (re-arming
// a live host to `pending` would wipe its presence and cost it its token).
// `disconnected` counts as onboarded too — the stream is closed, but the seed on
// the host is valid and the Soul may return.
const existingProvisionSQL = `
SELECT status, registered_at, requested_at
FROM souls
WHERE sid = $1
  AND status IN ('connected', 'disconnected')` + ownedByRunSQL

// describeTakenSIDSQL reports why a SID could not be provisioned: current
// status + every incarnation the row belongs to (M:N, migration 099).
const describeTakenSIDSQL = `
SELECT s.status,
       COALESCE(array_agg(m.incarnation_name ORDER BY m.incarnation_name)
                FILTER (WHERE m.incarnation_name IS NOT NULL), '{}')
FROM souls s
LEFT JOIN incarnation_membership m ON m.sid = s.sid
WHERE s.sid = $1
GROUP BY s.status
`

const selectBySIDSQL = `
SELECT sid, transport, status, coven, traits,
       registered_at, last_seen_at, last_seen_by_kid,
       created_by_aid, requested_at, note
FROM souls
WHERE sid = $1
`

// selectCovenBySIDSQL — the one column the per-host RBAC gate needs
// (see [CovenBySID]).
const selectCovenBySIDSQL = `
SELECT coven
FROM souls
WHERE sid = $1
`

// selectCovenBySIDsSQL — the batch form of [selectCovenBySIDSQL], for the gates
// that authorize a whole resolved target at once (see [CovenBySIDs]).
const selectCovenBySIDsSQL = `
SELECT sid, coven
FROM souls
WHERE sid = ANY($1)
`

const deleteBySIDSQL = `
DELETE FROM souls
WHERE sid = $1
`

// updateStatusSQL — atomic status UPDATE that also records last_seen_by_kid
// (audit: which Keeper last held the stream). last_seen_at is written to
// Redis and flushed to PG separately; untouched here.
const updateStatusSQL = `
UPDATE souls
SET status = $2,
    last_seen_by_kid = COALESCE($3, last_seen_by_kid)
WHERE sid = $1
`

// markConnectedSQL — where `connected` is written (NIM-865). The value is
// documented as "stream alive, Keeper holds lease in Redis" ([Status]), so it is
// written where that becomes true; until then a host onboarding stays
// `pending`, which is what `pending` already says.
//
// **`last_seen_at` is written here too, and deliberately.** It is the predicate
// the burned-token recovery is gated on — a NULL means "this host has never held
// a stream", and while it is NULL an unexpired burned token stays redeemable.
//
// The handshake already flushes that column by treating Hello as an app message
// ([UpdateLastSeen] via touchSeen), so this is not the only writer and usually
// not the first. What it adds is that the gate closes UNCONDITIONALLY and in the
// same statement as the status: that flush is throttled per SID and its failure
// is swallowed at Debug, so there is a narrow path — a reconnect whose previous
// stream never ran its `forget`, or a single failed write — where the row would
// read `connected` with `last_seen_at` still NULL, which is precisely the
// "never onboarded" answer the recovery path would act on for a host that is up.
// Two predicates that can disagree are worse than one, and the security-bearing
// one should not be the one that can be skipped.
//
// `status IN ('pending','disconnected')` rather than an unconditional write:
// `revoked` / `expired` / `destroyed` are lifecycle decisions an operator or a
// cascade made, and a reconnecting stream must not undo one. The `last_seen_at`
// write is deliberately under the same WHERE: a revoked host must not be able to
// close its own recovery gate, and it has no business being marked seen either.
const markConnectedSQL = `
UPDATE souls
SET status           = 'connected',
    last_seen_at     = $2,
    last_seen_by_kid = $3
WHERE sid = $1
  AND status IN ('pending', 'disconnected')
`

// updateCovenSQL — UPDATE of the stable Coven label set. Used by the
// keeper-side core module `core.soul.registered` (docs/keeper/modules.md).
// Returns the final coven set in one round trip; RETURNING avoids an extra
// SelectBySID to build the module output.
const updateCovenSQL = `
UPDATE souls
SET coven = $2
WHERE sid = $1
RETURNING coven
`

// updateLastSeenSQL — targeted UPDATE of `last_seen_at`/`last_seen_by_kid`
// (ADR-006(a) flush from the Redis cache; Redis is authoritative, PG is a
// snapshot). Called from the throttled flush in touchSeen on every
// EventStream app message, but no more often than stale_after/3 (fix
// 89b4f0a): frequent heartbeats stay in Redis, PG gets a decimated snapshot.
//
// **It also completes what [markConnectedSQL] does at the handshake**, under the
// same `pending`/`disconnected` guard. That write is best-effort, and NOTHING
// reconciles a `pending` row: the Reaper's lease-aware sweep moves
// `connected → disconnected` and `disconnected → connected` (migration 043) and
// selects neither direction for `pending`, while `overlayPresence` and the
// roster both drop the status entirely. So a single failed write at the
// handshake would leave a live, streaming host invisible to every run for as
// long as the stream lasts. Repeating the promotion on each flush makes it
// self-healing, and costs nothing on a row that is already `connected`.
//
// **`revoked` / `destroyed` are excluded from the statement as a whole**, not
// merely from the status arm. `last_seen_at` is no longer only a staleness
// snapshot: it is the predicate the burned-token recovery reads as "this host
// has never held a stream" (see [markConnectedSQL]). Stamping it for a host an
// operator revoked would let that host close its own recovery gate, and the
// claim in markConnectedSQL that it cannot would be false — a revoked seed is
// refused at the interceptor, so such a row should be writing nothing here.
const updateLastSeenSQL = `
UPDATE souls
SET last_seen_at     = $2,
    last_seen_by_kid = $3,
    status           = CASE WHEN status IN ('pending', 'disconnected')
                            THEN 'connected' ELSE status END
WHERE sid = $1
  AND status NOT IN ('revoked', 'destroyed')
`

// updateSoulprintSQL — UPDATE of the typed-soulprint fields (migration 015).
// `facts` is JSON-serialized by the caller (proto → Struct → JSON).
// `received_at` is a Keeper-side timestamp, distinct from `collected_at`
// (the latter comes from the Soul in `SoulprintReport.collected_at`).
const updateSoulprintSQL = `
UPDATE souls
SET soulprint_facts        = $2,
    soulprint_collected_at = $3,
    soulprint_received_at  = $4
WHERE sid = $1
`

// Insert inserts a new Soul into the registry. Used by the Operator API when
// issuing a bootstrap token (creates a row with status `pending`).
//
// Pre-conditions:
//   - s.SID matches [SIDPattern];
//   - s.Transport / s.Status are valid enum values.
//
// Returns:
//   - [ErrSoulAlreadyExists] on UNIQUE violation of the PK.
//   - [ErrSoulCreatorNotFound] on FK violation of `souls_created_by_aid_fk`
//     (`created_by_aid` points at a nonexistent operator).
//   - wrapped fmt.Errorf for other FK/CHECK violations (status / transport /
//     sid format).
//
// `requested_at` defaults on the PG side (`COALESCE($9, NOW())`) when the
// caller leaves s.RequestedAt unset — normative pending-record semantics
// (docs/soul/onboarding.md). After Insert, s.RequestedAt holds the actual
// value (`RETURNING requested_at`).
func Insert(ctx context.Context, db ExecQueryRower, s *Soul) error {
	args, err := insertArgs(s)
	if err != nil {
		return err
	}
	row := db.QueryRow(ctx, insertSQL, args...)
	if err := row.Scan(&s.RegisteredAt, &s.RequestedAt); err != nil {
		return mapInsertError(err)
	}
	return nil
}

// insertArgs validates s, applies the enum defaults in place and builds the
// ordered argument list shared by [Insert] and [EnsureProvisionable].
func insertArgs(s *Soul) ([]any, error) {
	if s == nil {
		return nil, fmt.Errorf("soul: nil soul")
	}
	if !ValidSID(s.SID) {
		return nil, fmt.Errorf("soul: invalid SID %q (must match %s)", s.SID, SIDPattern)
	}
	if s.Transport == "" {
		s.Transport = TransportAgent
	}
	if !validTransport(s.Transport) {
		return nil, fmt.Errorf("soul: invalid transport %q", s.Transport)
	}
	if s.Status == "" {
		s.Status = StatusPending
	}
	if !validStatus(s.Status) {
		return nil, fmt.Errorf("soul: invalid status %q", s.Status)
	}

	coven := s.Coven
	if coven == nil {
		coven = []string{}
	}

	// traits is jsonb (migration 087, ADR-060): marshal the map to []byte
	// (incarnation marshalJSONB pattern; pgx codec-auto for jsonb is
	// deliberately skipped, consistent with the other jsonb columns).
	// nil/empty → arg=nil, SQL's COALESCE($5,'{}') yields an empty object.
	var traitsArg any
	if len(s.Traits) > 0 {
		b, err := json.Marshal(s.Traits)
		if err != nil {
			return nil, fmt.Errorf("soul: marshal traits: %w", err)
		}
		traitsArg = b
	}

	var registeredAtArg any
	if !s.RegisteredAt.IsZero() {
		registeredAtArg = s.RegisteredAt.UTC()
	}
	var lastSeenAtArg any
	if s.LastSeenAt != nil {
		lastSeenAtArg = s.LastSeenAt.UTC()
	}
	var lastSeenByKIDArg any
	if s.LastSeenByKID != nil {
		lastSeenByKIDArg = *s.LastSeenByKID
	}
	var createdByAIDArg any
	if s.CreatedByAID != nil {
		createdByAIDArg = *s.CreatedByAID
	}
	var requestedAtArg any
	if s.RequestedAt != nil {
		requestedAtArg = s.RequestedAt.UTC()
	}
	var noteArg any
	if s.Note != "" {
		noteArg = s.Note
	}

	return []any{
		s.SID,
		string(s.Transport),
		string(s.Status),
		coven,
		traitsArg,
		registeredAtArg,
		lastSeenAtArg,
		lastSeenByKIDArg,
		createdByAIDArg,
		requestedAtArg,
		noteArg,
	}, nil
}

// ProvisionOutcome is what [EnsureProvisionable] did with the SID.
type ProvisionOutcome string

const (
	// ProvisionInserted — the SID was free; a fresh pending record was written.
	ProvisionInserted ProvisionOutcome = "inserted"
	// ProvisionReused — a leftover of an earlier attempt was re-armed for a
	// fresh onboarding. The record predates this run: the caller's
	// orphan-cleanup must NOT delete it, and its still-active bootstrap token
	// has to be invalidated before a replacement is issued.
	ProvisionReused ProvisionOutcome = "reused"
	// ProvisionExisting — the SID already carries a registration this run
	// onboarded; the row was left untouched. The host needs no bootstrap token
	// (it already holds an identity) and must not be rolled back.
	ProvisionExisting ProvisionOutcome = "existing"
)

// EnsureProvisionable registers the SID of a VM being provisioned, tolerating a
// re-run over an incarnation this run already provisioned (NIM-170, NIM-189).
// Plain [Insert] is not idempotent: `destroy`/`unlock` leave the souls rows
// behind, so a second `create` used to die on the PK (`SQLSTATE 23505`) and the
// operator had to clean PG by hand.
//
// Outcomes:
//   - SID free → inserted as a fresh pending record, [ProvisionInserted].
//   - SID held by a leftover of an earlier provision (status `pending` — never
//     onboarded — or `destroyed` — cascade tombstone) that is this run's own
//     (member of incarnationName, or not bound yet) → the row is re-armed for
//     onboarding, [ProvisionReused].
//   - SID held by a host this run already onboarded (`connected` /
//     `disconnected`, same ownership rule) → [ProvisionExisting]: the row is
//     read back into s and left as it is. Converge semantics — provisioning an
//     existing host is a no-op, the rest of the run brings its config to the
//     target state (NIM-189).
//   - SID held by anything else (`revoked` / `expired`, or a row bound only to
//     another incarnation) → [ErrSoulNotProvisionable], row untouched.
//
// `incarnationName` is the incarnation of the current run ("" when unknown,
// which makes any membership block both reuse and pass-through).
func EnsureProvisionable(ctx context.Context, db ExecQueryRower, s *Soul, incarnationName string) (ProvisionOutcome, error) {
	args, err := insertArgs(s)
	if err != nil {
		return "", err
	}
	// Separate statements rather than one upsert: each is atomic on its own, and
	// an empty RETURNING answers exactly one question (phase 1 "was the SID
	// free", phase 2 "was it a leftover of mine", phase 3 "is it a host of mine
	// that is already up"). Retried once because the row may be deleted between
	// the phases (concurrent rollback of another run).
	for attempt := 0; attempt < 2; attempt++ {
		err := db.QueryRow(ctx, insertIfFreeSQL, args...).Scan(&s.RegisteredAt, &s.RequestedAt)
		if err == nil {
			return ProvisionInserted, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return "", mapInsertError(err)
		}

		err = db.QueryRow(ctx, reuseForProvisionSQL, s.SID, incarnationName, string(s.Transport)).
			Scan(&s.RegisteredAt, &s.RequestedAt)
		if err == nil {
			return ProvisionReused, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return "", fmt.Errorf("soul: reuse provision record %q: %w", s.SID, err)
		}

		var status string
		err = db.QueryRow(ctx, existingProvisionSQL, s.SID, incarnationName).
			Scan(&status, &s.RegisteredAt, &s.RequestedAt)
		if err == nil {
			s.Status = Status(status) // s now mirrors the untouched row
			return ProvisionExisting, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return "", fmt.Errorf("soul: read existing provision record %q: %w", s.SID, err)
		}

		takenStatus, incarnations, derr := describeTakenSID(ctx, db, s.SID)
		if errors.Is(derr, ErrSoulNotFound) {
			continue // row vanished between the phases — the SID is free again
		}
		if derr != nil {
			return "", derr
		}
		return "", notProvisionableError(s.SID, takenStatus, incarnations)
	}
	return "", fmt.Errorf("%w: %q kept changing hands during provisioning", ErrSoulNotProvisionable, s.SID)
}

// ownedByRunProbeSQL asks the ownership half of the provision predicate on its
// own, for a caller that has already located the row and needs only the answer
// [ownedByRunSQL] carries inside the bigger statements. It is built from the
// same fragment on purpose: a second, restated copy of a fail-closed predicate
// is exactly how the two come to disagree about who owns a host.
const ownedByRunProbeSQL = `
SELECT TRUE
FROM souls
WHERE sid = $1` + ownedByRunSQL

// OwnedByRun reports whether the Soul row for sid is the run's own — a member
// of incarnationName, or of no incarnation at all — judged by the same
// predicate [EnsureProvisionable] uses for its reuse and pass-through phases.
// A row that does not exist is not owned; neither is one bound only to other
// incarnations. An empty incarnationName means "unknown", never "no owner", so
// it leaves only unbound rows owned.
//
// It exists for the caller that cannot go through EnsureProvisionable because
// it must not write: `core.bootstrap.issued` decides whether an already
// onboarded SID is its own host to converge over or somebody else's identity to
// refuse (NIM-780), and re-arming the row would cost a live host its presence.
func OwnedByRun(ctx context.Context, db ExecQueryRower, sid, incarnationName string) (bool, error) {
	var owned bool
	if err := db.QueryRow(ctx, ownedByRunProbeSQL, sid, incarnationName).Scan(&owned); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("soul: ownership of %q: %w", sid, err)
	}
	return owned, nil
}

// DescribeTakenSID reads the status + incarnation memberships of an existing
// SID, so a refusal can say WHO holds it rather than only that somebody does.
// [ErrSoulNotFound] when the row is gone.
//
// Exported for the other refusal built on [OwnedByRun]: `core.bootstrap.issued`
// declines a takeover on the same predicate as provisioning and owes the
// operator the same diagnosis.
func DescribeTakenSID(ctx context.Context, db ExecQueryRower, sid string) (Status, []string, error) {
	return describeTakenSID(ctx, db, sid)
}

// describeTakenSID reads the status + incarnation memberships of an existing
// SID, to explain a refused provision. [ErrSoulNotFound] when the row is gone.
func describeTakenSID(ctx context.Context, db ExecQueryRower, sid string) (Status, []string, error) {
	var status string
	var incarnations []string
	if err := db.QueryRow(ctx, describeTakenSIDSQL, sid).Scan(&status, &incarnations); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", nil, ErrSoulNotFound
		}
		return "", nil, fmt.Errorf("soul: describe taken SID %q: %w", sid, err)
	}
	return Status(status), incarnations, nil
}

// notProvisionableError spells out for the operator WHY the SID cannot be
// re-provisioned — status alone doesn't say who holds it.
func notProvisionableError(sid string, status Status, incarnations []string) error {
	owner := "not a member of any incarnation"
	if len(incarnations) > 0 {
		owner = "member of incarnation " + strings.Join(incarnations, ", ")
	}
	return fmt.Errorf("%w: %q is already registered with status %q (%s); a host of THIS incarnation that is simply up is converged in place, this one is not — destroy it first, or run against the existing roster",
		ErrSoulNotProvisionable, sid, status, owner)
}

// DeleteBySID deletes a souls row by SID. FK bootstrap_tokens.sid and
// soul_seeds.sid are ON DELETE CASCADE (migrations 008/009) — related tokens
// and seed records go with the Soul. Returns [ErrSoulNotFound] if no row
// matches (idempotent for a caller rolling back a just-inserted record).
//
// Single-SID rollback only; batch GC of expired Souls is the separate,
// status-filtered Reaper function purge_souls (migration 012).
func DeleteBySID(ctx context.Context, db ExecQueryRower, sid string) error {
	if !ValidSID(sid) {
		return fmt.Errorf("soul: invalid SID %q", sid)
	}
	tag, err := db.Exec(ctx, deleteBySIDSQL, sid)
	if err != nil {
		return fmt.Errorf("soul: delete: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrSoulNotFound
	}
	return nil
}

func mapInsertError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case pgErrCodeUniqueViolation:
			return fmt.Errorf("%w (constraint %s): %w",
				ErrSoulAlreadyExists, pgErr.ConstraintName, err)
		case pgErrCodeForeignKeyViolation:
			if pgErr.ConstraintName == "souls_created_by_aid_fk" {
				return fmt.Errorf("%w (constraint %s): %w",
					ErrSoulCreatorNotFound, pgErr.ConstraintName, err)
			}
			return fmt.Errorf("soul: FK violation on %s: %w", pgErr.ConstraintName, err)
		case pgErrCodeCheckViolation:
			return fmt.Errorf("soul: CHECK violation on %s: %w", pgErr.ConstraintName, err)
		}
	}
	return fmt.Errorf("soul: insert: %w", err)
}

// SelectBySID reads a Soul by PK. Returns [ErrSoulNotFound] on pgx.ErrNoRows.
func SelectBySID(ctx context.Context, db ExecQueryRower, sid string) (*Soul, error) {
	row := db.QueryRow(ctx, selectBySIDSQL, sid)
	return scanSoul(row)
}

// CovenBySID reads the host's own Coven labels alone. Returns [ErrSoulNotFound]
// on pgx.ErrNoRows.
//
// A narrow read rather than [SelectBySID] because the caller is the RBAC gate of
// the per-host routes (NIM-588): it runs BEFORE the handler on every call and
// needs exactly one column. Scanning the other ten would make the cold gate path
// carry the row twice for nothing.
//
// The labels are the host's OWN (`souls.coven`, NIM-281 — a coven is never
// inherited), which is the same column the scoped souls list pushes down on
// ([soulpurview.Columns]). The gate and the list must agree on what "this host is
// in coven X" means, and the only way to guarantee that is to read the same place.
func CovenBySID(ctx context.Context, db ExecQueryRower, sid string) ([]string, error) {
	var coven []string
	if err := db.QueryRow(ctx, selectCovenBySIDSQL, sid).Scan(&coven); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrSoulNotFound
		}
		return nil, fmt.Errorf("soul: select coven by sid: %w", err)
	}
	return coven, nil
}

// CovenBySIDs is [CovenBySID] over a batch: one round-trip for a whole resolved
// target (Voyage / Cadence spawn, NIM-650), where a query per host would put a
// round-trip on every element of a scope that may run to hundreds.
//
// Only the hosts that HAVE a row appear in the result. A caller reads a missing
// key as "the coven is unknown", which is what an absent row means and also what
// [CovenBySID] reports through [ErrSoulNotFound] — so the batch and the single
// read agree, and a `coven=`-scoped grant fails closed either way. An empty
// input is not a query.
func CovenBySIDs(ctx context.Context, db ExecQueryRower, sids []string) (map[string][]string, error) {
	if len(sids) == 0 {
		return map[string][]string{}, nil
	}
	rows, err := db.Query(ctx, selectCovenBySIDsSQL, sids)
	if err != nil {
		return nil, fmt.Errorf("soul: select coven by sids: %w", err)
	}
	defer rows.Close()

	out := make(map[string][]string, len(sids))
	for rows.Next() {
		var (
			sid   string
			coven []string
		)
		if err := rows.Scan(&sid, &coven); err != nil {
			return nil, fmt.Errorf("soul: scan coven by sids: %w", err)
		}
		out[sid] = coven
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("soul: iterate coven by sids: %w", err)
	}
	return out, nil
}

// SoulprintRecord — last received SoulprintReport for one host
// (`GET /v1/souls/{sid}/soulprint`). FactsJSON is raw JSONB, exactly what
// eventstream collects via `protojson.Marshal(SoulprintFacts)` with
// `UseProtoNames` (snake_case keys like `pkg_mgr`/`init_system`, ADR-018).
// Parsing is left to the consumer (handler returns it as `map[string]any`
// for UI symmetry with other jsonb fields like `incarnation.state`).
//
// CollectedAt is the Soul-side collection timestamp (from proto
// `SoulprintReport.collected_at`), ReceivedAt is the Keeper-side moment the
// stream received it (see [UpdateSoulprint]). The gap is a skew diagnostic:
// eventstream logs an OTel warn past 10 minutes (docs/soul/soulprint.md →
// §`received_at`/`collected_at`).
type SoulprintRecord struct {
	SID         string
	FactsJSON   []byte
	CollectedAt time.Time
	ReceivedAt  time.Time
}

const selectSoulprintSQL = `
SELECT sid, soulprint_facts, soulprint_collected_at, soulprint_received_at
FROM souls
WHERE sid = $1
`

// SelectSoulprint reads the latest typed SoulprintReport for one Soul.
//
// Returns:
//   - [ErrSoulNotFound] — no record in the `souls` registry.
//   - [ErrSoulprintNotReceived] — record exists but SoulprintReport never
//     arrived (`soulprint_facts IS NULL`). Mapped by the handler to HTTP 410.
//
// FactsJSON is returned as-is, without unmarshaling: the storage invariant
// is that the JSONB is already proto-snake_case; the handler forwards it via
// `json.RawMessage`/decoded `map[string]any` rather than duplicating the
// proto schema on the Keeper's Go side.
func SelectSoulprint(ctx context.Context, db ExecQueryRower, sid string) (*SoulprintRecord, error) {
	if !ValidSID(sid) {
		return nil, fmt.Errorf("soul: invalid SID %q", sid)
	}
	var (
		rec         SoulprintRecord
		factsJSON   []byte
		collectedAt *time.Time
		receivedAt  *time.Time
	)
	err := db.QueryRow(ctx, selectSoulprintSQL, sid).Scan(&rec.SID, &factsJSON, &collectedAt, &receivedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrSoulNotFound
		}
		return nil, fmt.Errorf("soul: soulprint select: %w", err)
	}
	if len(factsJSON) == 0 {
		// Soul record exists but no facts yet: typed SoulprintReport
		// hasn't been sent (fresh onboarding, or transport: ssh).
		return nil, ErrSoulprintNotReceived
	}
	rec.FactsJSON = factsJSON
	if collectedAt != nil {
		rec.CollectedAt = collectedAt.UTC()
	}
	if receivedAt != nil {
		rec.ReceivedAt = receivedAt.UTC()
	}
	return &rec, nil
}

const selectSoulsWithSoulprintSQL = `
SELECT sid
FROM souls
WHERE sid = ANY($1) AND soulprint_facts IS NOT NULL
`

// SelectSoulsWithSoulprint returns the subset of sids with a typed soulprint
// recorded (`souls.soulprint_facts IS NOT NULL`). Batch check for the
// `core.soul.registered` onboarding barrier (ADR-061 amendment: presence +
// first soulprint on refresh_soulprint); result shape mirrors
// redis.SoulsStreamAlive. Unknown SIDs are simply absent from the result.
func SelectSoulsWithSoulprint(ctx context.Context, db ExecQueryRower, sids []string) (map[string]struct{}, error) {
	res := make(map[string]struct{}, len(sids))
	if len(sids) == 0 {
		return res, nil
	}
	rows, err := db.Query(ctx, selectSoulsWithSoulprintSQL, sids)
	if err != nil {
		return nil, fmt.Errorf("soul: souls with soulprint select: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var sid string
		if err := rows.Scan(&sid); err != nil {
			return nil, fmt.Errorf("soul: souls with soulprint scan: %w", err)
		}
		res[sid] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("soul: souls with soulprint rows: %w", err)
	}
	return res, nil
}

func scanSoul(row pgx.Row) (*Soul, error) {
	var (
		s             Soul
		transportStr  string
		statusStr     string
		lastSeenAt    *time.Time
		lastSeenByKID *string
		createdByAID  *string
		requestedAt   *time.Time
		note          *string
	)
	err := row.Scan(
		&s.SID,
		&transportStr,
		&statusStr,
		&s.Coven,
		&s.TraitsRaw,
		&s.RegisteredAt,
		&lastSeenAt,
		&lastSeenByKID,
		&createdByAID,
		&requestedAt,
		&note,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrSoulNotFound
		}
		return nil, fmt.Errorf("soul: scan: %w", err)
	}
	s.Transport = Transport(transportStr)
	s.Status = Status(statusStr)
	// traits jsonb (ADR-060): '{}' (NOT NULL DEFAULT) → empty map, not nil.
	if len(s.TraitsRaw) > 0 {
		if err := json.Unmarshal(s.TraitsRaw, &s.Traits); err != nil {
			return nil, fmt.Errorf("soul: unmarshal traits for %q: %w", s.SID, err)
		}
	}
	s.LastSeenAt = lastSeenAt
	s.LastSeenByKID = lastSeenByKID
	s.CreatedByAID = createdByAID
	s.RequestedAt = requestedAt
	if note != nil {
		s.Note = *note
	}
	return &s, nil
}

// selectBySIDsSQL — souls by an explicit SID list. Same column set as the list
// query (scanSoul), ordered by SID. Unknown SIDs are simply absent from the
// result — the caller diffs against what it asked for.
const selectBySIDsSQL = `
SELECT sid, transport, status, coven, traits,
       registered_at, last_seen_at, last_seen_by_kid,
       created_by_aid, requested_at, note
FROM souls
WHERE sid = ANY($1)
ORDER BY sid ASC
`

// SelectBySIDs returns the souls for the given SIDs, ordered by SID. Missing
// SIDs are omitted rather than erroring — the caller decides what an unknown
// host means (the membership bind path, [incarnation.AddMembers], turns them
// into a 422 listing them). An empty list → nil, no query.
func SelectBySIDs(ctx context.Context, db ExecQueryRower, sids []string) ([]*Soul, error) {
	if len(sids) == 0 {
		return nil, nil
	}
	rows, err := db.Query(ctx, selectBySIDsSQL, sids)
	if err != nil {
		return nil, fmt.Errorf("soul: select by sids: %w", err)
	}
	defer rows.Close()

	var out []*Soul
	for rows.Next() {
		s, err := scanSoul(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("soul: iter souls by sids: %w", err)
	}
	return out, nil
}

// selectIncarnationMembersSQL — member souls of an incarnation, resolved via the
// membership relation (JOIN incarnation_membership, ADR-008 amendment
// 2026-07-17/NIM-124 — no longer `incarnation.name = ANY(coven)`). Same column
// set as SelectAll's list query (scanSoul), capped by $2, ordered by SID.
const selectIncarnationMembersSQL = `
SELECT s.sid, s.transport, s.status, s.coven, s.traits,
       s.registered_at, s.last_seen_at, s.last_seen_by_kid,
       s.created_by_aid, s.requested_at, s.note
FROM souls s
JOIN incarnation_membership m ON m.sid = s.sid
WHERE m.incarnation_name = $1
ORDER BY s.sid ASC
LIMIT $2
`

// SelectIncarnationMembers returns the member souls of incarnation `incName`
// (via incarnation_membership, NIM-124), capped at `limit` and ordered by SID.
// Used by the host-vitals incarnation aggregate to list members before the
// caller's soul-read-scope filter. A non-member host is not returned even if its
// coven happens to contain a string equal to the incarnation name.
func SelectIncarnationMembers(ctx context.Context, db ExecQueryRower, incName string, limit int) ([]*Soul, error) {
	if limit < 1 {
		limit = 1
	}
	rows, err := db.Query(ctx, selectIncarnationMembersSQL, incName, limit)
	if err != nil {
		return nil, fmt.Errorf("soul: select incarnation members: %w", err)
	}
	defer rows.Close()

	var out []*Soul
	for rows.Next() {
		s, err := scanSoul(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("soul: iter incarnation members: %w", err)
	}
	return out, nil
}

// UpdateStatus transitions a Soul to a new status and updates
// last_seen_by_kid. kid is a pointer: nil keeps the old value (PG
// `COALESCE`), non-nil overwrites it.
//
// **It has no production caller.** The onboarding RPC stopped writing a status
// at all and the EventStream handshake uses [MarkConnected], both in NIM-865;
// the Reaper moves statuses through its own SQL functions. Kept as the general
// CRUD verb, and because a transition that is neither "a stream appeared" nor a
// Reaper sweep has nowhere else to go — but it is unconstrained, so a new caller
// must decide for itself whether `revoked`/`destroyed` should stop it, which is
// exactly what MarkConnected encodes.
//
// Returns [ErrSoulNotFound] if the SID doesn't exist or UPDATE touched no
// rows.
func UpdateStatus(ctx context.Context, db ExecQueryRower, sid string, newStatus Status, kid *string) error {
	if !ValidSID(sid) {
		return fmt.Errorf("soul: invalid SID %q", sid)
	}
	if !validStatus(newStatus) {
		return fmt.Errorf("soul: invalid status %q", newStatus)
	}
	var kidArg any
	if kid != nil {
		kidArg = *kid
	}
	tag, err := db.Exec(ctx, updateStatusSQL, sid, string(newStatus), kidArg)
	if err != nil {
		return fmt.Errorf("soul: update status: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrSoulNotFound
	}
	return nil
}

// MarkConnected records that a stream from this Soul is up, moving a
// `pending` / `disconnected` row to `connected` and stamping `last_seen_at`.
// Called once per EventStream, after the handshake reply has been delivered —
// the first moment the Keeper has evidence the host holds a working credential
// (NIM-865).
//
// Idempotent and lossless: a row in any other status is left alone and the
// call reports no error, because "this stream changed nothing" is the normal
// answer for an already-connected Soul and not a failure to report upwards.
func MarkConnected(ctx context.Context, db ExecQueryRower, sid, kid string, at time.Time) error {
	if !ValidSID(sid) {
		return fmt.Errorf("soul: invalid SID %q", sid)
	}
	if kid == "" {
		return fmt.Errorf("soul: empty kid")
	}
	if _, err := db.Exec(ctx, markConnectedSQL, sid, at.UTC(), kid); err != nil {
		return fmt.Errorf("soul: mark connected: %w", err)
	}
	return nil
}

// UpdateCoven — atomic UPDATE of the stable Coven label set. Used by the
// keeper-side core module `core.soul.registered` (docs/keeper/modules.md).
//
// The caller computes the final set per `mode` (`append`/`replace`/`remove`);
// this function only performs the UPDATE. Returns the actually saved set
// (PG `RETURNING coven`) so the module output is built from the actual
// value, not a client-recomputed one.
//
// Returns [ErrSoulNotFound] if the SID doesn't exist.
func UpdateCoven(ctx context.Context, db ExecQueryRower, sid string, coven []string) ([]string, error) {
	if !ValidSID(sid) {
		return nil, fmt.Errorf("soul: invalid SID %q", sid)
	}
	if coven == nil {
		coven = []string{}
	}
	var saved []string
	err := db.QueryRow(ctx, updateCovenSQL, sid, coven).Scan(&saved)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrSoulNotFound
		}
		return nil, fmt.Errorf("soul: update coven: %w", err)
	}
	return saved, nil
}

// UpdateLastSeen flushes last_seen_at/last_seen_by_kid to PG (ADR-006(a)).
// The real-time value lives in the Redis heartbeat cache; PG holds a
// snapshot needed by the Reaper (`mark_disconnected`) and the Operator API
// (`GET /v1/souls`).
//
// **It also promotes `pending`/`disconnected` to `connected`** — see
// [updateLastSeenSQL] for why the status ride-along is there and why `revoked`
// and `destroyed` rows are skipped entirely. The name predates that; it is kept
// because the flush is still what the call IS, and the promotion is a
// consequence of the same fact (an app message arrived).
//
// Returns [ErrSoulNotFound] if the SID doesn't exist.
func UpdateLastSeen(ctx context.Context, db ExecQueryRower, sid, kid string, at time.Time) error {
	if !ValidSID(sid) {
		return fmt.Errorf("soul: invalid SID %q", sid)
	}
	if kid == "" {
		return fmt.Errorf("soul: empty kid")
	}
	tag, err := db.Exec(ctx, updateLastSeenSQL, sid, at.UTC(), kid)
	if err != nil {
		return fmt.Errorf("soul: update last_seen: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrSoulNotFound
	}
	return nil
}

// UpdateSoulprint persists typed facts to PG (migration 015 → columns
// `soulprint_facts`/`soulprint_collected_at`/`soulprint_received_at`).
//
// `factsJSON` is pre-marshaled proto `SoulprintFacts` bytes; the caller
// calls [protojson.Marshal] (forward-compat proto-default serialization).
// nil / empty slice is allowed: on first connection a Soul has no
// SoulprintReport yet, and we want to be able to clear the column (tests,
// manual reset).
//
// Returns [ErrSoulNotFound] if the SID doesn't exist.
func UpdateSoulprint(ctx context.Context, db ExecQueryRower, sid string, factsJSON []byte, collectedAt, receivedAt time.Time) error {
	if !ValidSID(sid) {
		return fmt.Errorf("soul: invalid SID %q", sid)
	}
	var factsArg any
	if len(factsJSON) > 0 {
		factsArg = factsJSON
	}
	var collectedArg any
	if !collectedAt.IsZero() {
		collectedArg = collectedAt.UTC()
	}
	var receivedArg any
	if !receivedAt.IsZero() {
		receivedArg = receivedAt.UTC()
	}
	tag, err := db.Exec(ctx, updateSoulprintSQL, sid, factsArg, collectedArg, receivedArg)
	if err != nil {
		return fmt.Errorf("soul: update soulprint: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrSoulNotFound
	}
	return nil
}

// BulkPool — the pgxpool.Pool surface [BulkAssignCoven] needs:
// ExecQueryRower (count + per-chunk UPDATE/Scan) + BeginTx (commit per
// chunk). `*pgxpool.Pool` satisfies it automatically; unit tests use a fake.
type BulkPool interface {
	ExecQueryRower
	BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error)
}

// BulkSelector — subset of the `soul.*` targeting vocabulary for bulk
// operations (`POST /v1/souls/coven`). NOT topology.Resolver: a pure PG
// predicate over cold `souls` columns, no presence/soulprint.
//
//   - All         — no host filter (whole registry; still intersects scope).
//   - SIDs        — explicit host list (`sid = ANY($n)`).
//   - Coven       — hosts that already carry this label (`$n = ANY(coven)`).
//   - Incarnation — members of this incarnation, resolved via
//     `incarnation_membership` (ADR-008 amendment 2026-07-17/NIM-124: membership
//     is a first-class relation, no longer `incarnation.name = ANY(coven)`).
//   - Status      — filter by status.
//
// Empty SIDs/Coven/Incarnation/Status means "don't filter". All=false with
// every other criterion empty yields an empty host set (the caller must set
// at least one criterion — no bulk without a target).
//
// Criteria combine with AND (narrowing). E.g. {Incarnation: "redis", Status:
// connected} matches only connected hosts of incarnation `redis`.
type BulkSelector struct {
	All         bool
	SIDs        []string
	Coven       string
	Incarnation string
	Status      Status
}

// BulkScope — operator's coven scope for `soul.coven-assign` (from
// rbac.Enforcer.CovenScope). Unrestricted=true (bare/`*`) lifts both
// scope-intersection constraints.
type BulkScope struct {
	Covens       []string
	Unrestricted bool
}

// BulkStatus — terminal status of a bulk operation.
type BulkStatus string

const (
	// BulkCompleted — all chunks committed (or dry_run).
	BulkCompleted BulkStatus = "completed"
	// BulkPartial — chunk K failed; 1..K-1 are committed and not rolled
	// back (idempotently retried by the operator).
	BulkPartial BulkStatus = "partial"
)

// Report — outcome of a bulk coven-assign.
//
//   - Matched         — hosts matching selector ∩ scope.
//   - Changed         — rows actually changed (sum of RowsAffected across
//     chunks; idempotent no-ops don't count).
//   - ChunksCommitted — number of successfully committed chunks.
//   - Status          — completed | partial.
//   - Err             — reason for a partial failure (nil for completed).
type Report struct {
	Matched         int
	Changed         int
	ChunksCommitted int
	Status          BulkStatus
	Err             error
}

// bulkChunkSize — keyset iteration chunk size (spec: 1-2k SIDs, commit per
// chunk). Smaller means more round trips; larger holds the `souls` row lock
// longer, blocking the hot UpdateLastSeen heartbeat flush. 2000 is the top
// of the recommended window.
const bulkChunkSize = 2000

// ErrBulkEmptySelector — selector has no criteria at all (All=false and
// everything else empty): a targetless bulk call is almost always a caller
// bug, so it's rejected.
var ErrBulkEmptySelector = errors.New("soul: bulk selector matches no hosts (set all/sids/coven/status)")

// ErrBulkLabelOutOfScope — the label being appended is outside the
// operator's coven scope. Privilege-escalation gate (b): an operator can't
// attach a label it doesn't own within its scope.
var ErrBulkLabelOutOfScope = errors.New("soul: label is outside operator coven-scope")

// CountBulkMatched counts hosts matching selector ∩ scope without mutating
// anything (dry_run and Matched precomputation). Returns
// [ErrBulkEmptySelector] on an empty selector.
func CountBulkMatched(ctx context.Context, db ExecQueryRower, sel BulkSelector, scope BulkScope) (int, error) {
	where, args, err := buildBulkWhere(sel, scope)
	if err != nil {
		return 0, err
	}
	var n int
	if err := db.QueryRow(ctx, "SELECT COUNT(*) FROM souls"+where, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("soul: bulk count: %w", err)
	}
	return n, nil
}

// BulkAssignCoven bulk-adds (append) or removes ONE Coven label on hosts
// matching selector ∩ scope (`POST /v1/souls/coven`).
//
// Invariants:
//   - Coven is a COLD PG label: a plain UPDATE souls, no Redis writes.
//   - Keyset iteration over the PK (`sid > cursor ORDER BY sid LIMIT chunk`,
//     NOT OFFSET) + commit per chunk: one giant transaction would hold the
//     `souls` row lock for tens of seconds, blocking the UpdateLastSeen
//     heartbeat flush.
//   - Idempotent filtering in WHERE: append skips hosts that already carry
//     the label; remove skips hosts that don't. An untouched row takes no lock.
//   - scope-intersection: target hosts ⊆ scope (predicate
//     coven && ARRAY[scope]); for append, the label itself ∈ scope (else
//     [ErrBulkLabelOutOfScope]). Unrestricted scope (bare/`*`) lifts both
//     constraints.
//   - On chunk K failure: 1..K-1 stay committed, Status=partial — no
//     rollback (idempotently retried by the operator).
//
// label must be valid ([ValidCoven]) and mode ∈ {append, remove} — the
// caller (handler) checks before calling; this is a defensive re-check.
// mode=replace uses [BulkReplaceCoven] with a label set instead.
func BulkAssignCoven(ctx context.Context, db BulkPool, sel BulkSelector, scope BulkScope, label string, mode CovenMode) (Report, error) {
	if !ValidCoven(label) {
		return Report{}, fmt.Errorf("soul: invalid coven label %q (must match %s)", label, CovenPattern)
	}
	if mode != CovenAppend && mode != CovenRemove {
		return Report{}, fmt.Errorf("soul: bulk mode %q unsupported (want append/remove; use BulkReplaceCoven for replace)", mode)
	}
	// Gate (b): an append label outside scope cannot be assigned.
	if mode == CovenAppend && !scope.Unrestricted && !covenInScope(label, scope.Covens) {
		return Report{}, fmt.Errorf("%w: %q", ErrBulkLabelOutOfScope, label)
	}

	matched, err := CountBulkMatched(ctx, db, sel, scope)
	if err != nil {
		return Report{}, err
	}
	rep := Report{Matched: matched, Status: BulkCompleted}
	if matched == 0 {
		return rep, nil
	}

	cursor := ""
	for {
		tag, lastSID, n, cerr := bulkUpdateChunk(ctx, db, sel, scope, label, mode, cursor)
		if cerr != nil {
			rep.Status = BulkPartial
			rep.Err = cerr
			return rep, cerr
		}
		rep.Changed += int(tag)
		rep.ChunksCommitted++
		if n < bulkChunkSize {
			break
		}
		cursor = lastSID
	}
	return rep, nil
}

// BulkReplaceCoven bulk-REPLACES a host's Coven label set with `labels`
// exactly (dropping the existing set) for hosts matching selector ∩ scope.
//
// Semantic differences from [BulkAssignCoven]:
//   - mode is implicitly replace; SET `coven = $labels` wholesale, not
//     array_append/array_remove on a single label.
//   - Gate (b) checks EVERY label in the set (any label outside scope →
//     [ErrBulkLabelOutOfScope]). Otherwise it'd be a scope bypass: an
//     operator scoped to `dev` could pass [dev, prod] and attach `prod` to
//     someone else's hosts within `dev`.
//   - Idempotent filtering: `coven IS DISTINCT FROM $labels` (a PG predicate
//     that's NULL- and array-safe; order-sensitive — so the caller must pass
//     the set in CANONICAL form via [covenUniqueSorted], otherwise a
//     set-equal but differently ordered list gets rewritten every time).
//   - An empty set (`labels = []`) is allowed — "clear all labels". Gate (b)
//     degenerates to a no-op for an empty set (no out-of-scope label to
//     check), but gate (a)'s scope-intersection WHERE predicate still runs:
//     a coven-scoped operator can't wipe labels off someone else's hosts.
//
// Chunking, partial semantics, and the iteration skeleton are shared with
// [BulkAssignCoven].
func BulkReplaceCoven(ctx context.Context, db BulkPool, sel BulkSelector, scope BulkScope, labels []string) (Report, error) {
	canonical := covenUniqueSorted(labels)
	for _, l := range canonical {
		if !ValidCoven(l) {
			return Report{}, fmt.Errorf("soul: invalid coven label %q (must match %s)", l, CovenPattern)
		}
	}
	// Gate (b): EVERY label in the set must be in scope. Symmetric to
	// append, but looped — extends the privilege-escalation gate to a
	// replace set.
	if !scope.Unrestricted {
		for _, l := range canonical {
			if !covenInScope(l, scope.Covens) {
				return Report{}, fmt.Errorf("%w: %q", ErrBulkLabelOutOfScope, l)
			}
		}
	}

	matched, err := CountBulkMatched(ctx, db, sel, scope)
	if err != nil {
		return Report{}, err
	}
	rep := Report{Matched: matched, Status: BulkCompleted}
	if matched == 0 {
		return rep, nil
	}

	cursor := ""
	for {
		tag, lastSID, n, cerr := bulkReplaceChunk(ctx, db, sel, scope, canonical, cursor)
		if cerr != nil {
			rep.Status = BulkPartial
			rep.Err = cerr
			return rep, cerr
		}
		rep.Changed += int(tag)
		rep.ChunksCommitted++
		if n < bulkChunkSize {
			break
		}
		cursor = lastSID
	}
	return rep, nil
}

// bulkUpdateChunk runs one chunk in its own transaction: a keyset window
// `sid > cursor ORDER BY sid LIMIT chunk` under selector ∩ scope, plus
// idempotent filtering. Returns (changedRows, lastSID, scannedRows, err):
//
//   - changedRows — RETURNING rows (actually changed in this chunk);
//   - lastSID     — max sid in the window (next cursor); empty if the
//     window is empty;
//   - scannedRows — rows in the keyset window BEFORE idempotent filtering
//     (exit condition: scannedRows < chunk → last chunk).
//
// Idempotent filtering and the keyset window are kept separate on purpose:
// RETURNING gives changedRows, but the exit condition is the keyset window
// size, not changedRows (otherwise a chunk where every label is already set
// would end iteration prematurely). So the window is selected by its own
// keyset predicate, and UPDATE applies to a subset of it.
func bulkUpdateChunk(ctx context.Context, db BulkPool, sel BulkSelector, scope BulkScope, label string, mode CovenMode, cursor string) (changed int64, lastSID string, scanned int, err error) {
	tx, err := db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return 0, "", 0, fmt.Errorf("soul: bulk begin tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(context.Background())
		}
	}()

	where, args := buildBulkWhereWithCursor(sel, scope, cursor)
	// $label is the last positional argument (used by array_append/remove
	// and the idempotent predicate).
	labelPos := len(args) + 1
	args = append(args, label)

	var setExpr, idemPred string
	switch mode {
	case CovenAppend:
		setExpr = fmt.Sprintf("array_append(coven, $%d)", labelPos)
		idemPred = fmt.Sprintf("NOT ($%d = ANY(coven))", labelPos)
	case CovenRemove:
		setExpr = fmt.Sprintf("array_remove(coven, $%d)", labelPos)
		idemPred = fmt.Sprintf("$%d = ANY(coven)", labelPos)
	}

	// CTE: the keyset window (window) pins one chunk of hosts by PK; scanned
	// is its size (exit condition); upd mutates only the idempotently
	// filtered subset and returns the changed sids.
	sql := fmt.Sprintf(`
WITH chunk AS (
    SELECT sid FROM souls%s
    ORDER BY sid LIMIT %d
),
upd AS (
    UPDATE souls
    SET coven = %s
    WHERE sid IN (SELECT sid FROM chunk) AND %s
    RETURNING sid
)
SELECT
    (SELECT COUNT(*) FROM chunk),
    (SELECT COUNT(*) FROM upd),
    (SELECT MAX(sid) FROM chunk)
`, where, bulkChunkSize, setExpr, idemPred)

	var (
		scannedN int
		changedN int64
		maxSID   *string
	)
	if err := tx.QueryRow(ctx, sql, args...).Scan(&scannedN, &changedN, &maxSID); err != nil {
		return 0, "", 0, fmt.Errorf("soul: bulk chunk update: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, "", 0, fmt.Errorf("soul: bulk chunk commit: %w", err)
	}
	committed = true

	last := ""
	if maxSID != nil {
		last = *maxSID
	}
	return changedN, last, scannedN, nil
}

// bulkReplaceChunk runs one chunk of replace mode: the same CTE skeleton as
// [bulkUpdateChunk], but UPDATE replaces the whole set (`coven = $labels`)
// with idempotent filtering `coven IS DISTINCT FROM $labels` (PG's
// NULL/array-safe "not equal").
func bulkReplaceChunk(ctx context.Context, db BulkPool, sel BulkSelector, scope BulkScope, labels []string, cursor string) (changed int64, lastSID string, scanned int, err error) {
	tx, err := db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return 0, "", 0, fmt.Errorf("soul: bulk begin tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(context.Background())
		}
	}()

	where, args := buildBulkWhereWithCursor(sel, scope, cursor)
	labelsPos := len(args) + 1
	// pgx maps a nil slice to NULL; replacing with an empty set must yield
	// an empty array (`coven = ARRAY[]::text[]`) or it breaks the column's
	// NOT NULL expectation. Symmetric to [UpdateCoven] above.
	canonical := labels
	if canonical == nil {
		canonical = []string{}
	}
	args = append(args, canonical)

	sql := fmt.Sprintf(`
WITH chunk AS (
    SELECT sid FROM souls%s
    ORDER BY sid LIMIT %d
),
upd AS (
    UPDATE souls
    SET coven = $%d
    WHERE sid IN (SELECT sid FROM chunk) AND coven IS DISTINCT FROM $%d
    RETURNING sid
)
SELECT
    (SELECT COUNT(*) FROM chunk),
    (SELECT COUNT(*) FROM upd),
    (SELECT MAX(sid) FROM chunk)
`, where, bulkChunkSize, labelsPos, labelsPos)

	var (
		scannedN int
		changedN int64
		maxSID   *string
	)
	if err := tx.QueryRow(ctx, sql, args...).Scan(&scannedN, &changedN, &maxSID); err != nil {
		return 0, "", 0, fmt.Errorf("soul: bulk replace chunk update: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, "", 0, fmt.Errorf("soul: bulk replace chunk commit: %w", err)
	}
	committed = true

	last := ""
	if maxSID != nil {
		last = *maxSID
	}
	return changedN, last, scannedN, nil
}

// buildBulkWhere builds the WHERE for selector ∩ scope (no keyset cursor) —
// used by the count path (Matched/dry_run). Returns [ErrBulkEmptySelector]
// if the selector sets no criteria (All=false and everything else empty).
func buildBulkWhere(sel BulkSelector, scope BulkScope) (string, []any, error) {
	clauses, args := bulkSelectorClauses(sel)
	if len(clauses) == 0 && !sel.All {
		return "", nil, ErrBulkEmptySelector
	}
	clauses, args = appendScopeClause(clauses, args, scope)
	return joinWhere(clauses), args, nil
}

// buildBulkWhereWithCursor — same, plus the keyset predicate `sid > $cursor`
// (empty cursor = first chunk, no predicate). Returns no error: the selector
// was already validated by buildBulkWhere in CountBulkMatched before iteration.
func buildBulkWhereWithCursor(sel BulkSelector, scope BulkScope, cursor string) (string, []any) {
	clauses, args := bulkSelectorClauses(sel)
	clauses, args = appendScopeClause(clauses, args, scope)
	if cursor != "" {
		args = append(args, cursor)
		clauses = append(clauses, fmt.Sprintf("sid > $%d", len(args)))
	}
	return joinWhere(clauses), args
}

// bulkSelectorClauses turns a BulkSelector into SQL clauses + args. All adds
// no clause by itself (it means "no host filter").
//
// Coven and Incarnation stay DISTINCT predicates asking DIFFERENT questions
// (ADR-008 amendment 2026-07-17/NIM-124, sharpened by NIM-281):
//
//   - Coven is a LABEL question, resolved over the host's OWN `coven[]` and
//     nothing else ([covenMatchSQL]). A host carrying a tag spelled like an
//     incarnation matches, because it genuinely carries the label; a host merely
//     BOUND to that incarnation does not, because belonging attaches nothing.
//   - Incarnation is a MEMBERSHIP question, and is answered from
//     `incarnation_membership` alone. Neither predicate can stand in for the
//     other: one names what an operator put on the host, the other what the host
//     is bound to.
func bulkSelectorClauses(sel BulkSelector) ([]string, []any) {
	var (
		clauses []string
		args    []any
	)
	if len(sel.SIDs) > 0 {
		args = append(args, sel.SIDs)
		clauses = append(clauses, fmt.Sprintf("sid = ANY($%d)", len(args)))
	}
	if sel.Coven != "" {
		args = append(args, []string{sel.Coven})
		clauses = append(clauses, covenMatchSQL(len(args)))
	}
	if sel.Incarnation != "" {
		args = append(args, sel.Incarnation)
		clauses = append(clauses, fmt.Sprintf("sid IN (SELECT sid FROM incarnation_membership WHERE incarnation_name = $%d)", len(args)))
	}
	if sel.Status != "" {
		args = append(args, string(sel.Status))
		clauses = append(clauses, fmt.Sprintf("status = $%d", len(args)))
	}
	return clauses, args
}

// covenMatchSQL renders "the host carries any of the coven labels held by $pos",
// over the host's OWN `souls.coven` and nothing else.
//
// There is no label inheritance (NIM-281): a coven tag exists only where an
// operator attached it, keeper never mints one, and belonging to an incarnation
// attaches nothing. Reaching an incarnation's hosts is a membership question,
// spelled `incarnation=<name>` — see the `Incarnation` arm above, which joins
// `incarnation_membership` explicitly.
//
// One implementation for all three coven readers of this package — the list
// filter, the bulk selector and the bulk scope gate — and it is literally the
// predicate [rbac.PurviewSQL] pushes down for the `coven` dimension. That is the
// point: the set an operator can find, the set a bulk call selects, and the set
// authorizing that call are answers to the same question, and a rule applied to
// some of them and not others shows up as a filter matching nothing, with no
// error anywhere.
//
// The column is table-qualified so the predicate can be ANDed into any of them;
// every query using it selects `FROM souls` unaliased (the list, the bulk COUNT,
// and the keyset `chunk` CTE of the bulk UPDATEs), so `souls.coven` resolves in
// all of them.
func covenMatchSQL(pos int) string {
	return rbac.CovenScopeSQL("souls.coven", fmt.Sprintf("$%d", pos))
}

// appendScopeClause adds scope predicate (a): target hosts ⊆ operator scope.
// Unrestricted skips the constraint. Empty Covens with non-unrestricted yields a
// predicate that is deterministically false (the operator may touch no coven at
// all), which is correct — an empty array overlaps nothing and `ANY` of it holds
// for no name.
//
// The gate renders the SAME coven predicate the operator's read scope does
// (NIM-250), which is the invariant to keep whatever that predicate is: while
// the two disagreed, a bulk call silently skipped hosts the operator was looking
// at — Matched came back short with nothing to explain it. Both sides now match
// the host's own labels and nothing else (NIM-281). Gate (b) is untouched, so
// the operator still cannot attach a label outside its own scope.
func appendScopeClause(clauses []string, args []any, scope BulkScope) ([]string, []any) {
	if scope.Unrestricted {
		return clauses, args
	}
	covens := scope.Covens
	if covens == nil {
		covens = []string{} // NULL && coven = NULL; an empty array is a deterministic false.
	}
	args = append(args, covens)
	clauses = append(clauses, covenMatchSQL(len(args)))
	return clauses, args
}

func joinWhere(clauses []string) string {
	if len(clauses) == 0 {
		return ""
	}
	where := " WHERE " + clauses[0]
	for _, c := range clauses[1:] {
		where += " AND " + c
	}
	return where
}

func covenInScope(label string, scope []string) bool {
	for _, c := range scope {
		if c == label {
			return true
		}
	}
	return false
}

// ListFilter — filters for [SelectAll]. Empty fields mean "don't filter".
type ListFilter struct {
	Status    Status
	Transport Transport
	// Covens matches a host carrying ANY of these labels on ITSELF — belonging to an
	// incarnation attaches none (NIM-281). Empty = no filter. A
	// single-element slice is the one-label case; the plural exists because a create
	// form declares a SET of covens and asks for hosts in any of them (NIM-371) —
	// with one label per query the caller would have to union pages client-side and
	// would get a total that means nothing.
	Covens []string
	// Unassigned narrows to hosts belonging to NO incarnation
	// (`incarnation_membership`, NIM-124). The "free souls" filter behind the create
	// form's roster picker (NIM-371): offering a host that already runs another
	// service invites the operator to stack two services on it.
	//
	// It is a MEMBERSHIP question, deliberately not a label one: a host may carry a
	// tag spelled like an incarnation without being bound to it, so filtering by
	// labels would leave a free host looking occupied — and would miss a genuine
	// member that nobody bothered to tag.
	Unassigned bool
	// SIDPrefix narrows to SIDs starting with this string (autocomplete). Empty = no
	// filter. Matched as a literal prefix, LIKE metacharacters included — a `%` here
	// finds a SID containing one, it does not widen the match.
	SIDPrefix string
}

// SelectAll returns a page of Souls with the user filter (filter) ∩ RBAC
// purview boundary applied, plus the total count.
//
// The scope predicate is in the WHERE of BOTH queries (COUNT and SELECT) —
// total stays coherent with the results (never counts out-of-scope hosts).
// The full boolean purview (coven/host/trait) pushes down to SQL, so offset
// pagination and total are exact — no Go post-filter, no keyset window (NIM-128).
//
// Sort order is `registered_at DESC, sid ASC` (newest first; SID as
// tie-break, otherwise pagination is unstable on equal timestamps —
// symmetric to incarnation.SelectAll).
func SelectAll(ctx context.Context, db ExecQueryRower, filter ListFilter, scope soulpurview.Scope, offset, limit int) ([]*Soul, int, error) {
	if offset < 0 {
		return nil, 0, fmt.Errorf("soul: offset must be >= 0, got %d", offset)
	}
	if limit < 1 {
		return nil, 0, fmt.Errorf("soul: limit must be >= 1, got %d", limit)
	}

	whereSQL, args := buildListWhere(filter, scope)

	countSQL := "SELECT COUNT(*) FROM souls" + whereSQL
	var total int
	if err := db.QueryRow(ctx, countSQL, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("soul: count: %w", err)
	}

	listSQL := `SELECT sid, transport, status, coven, traits,
       registered_at, last_seen_at, last_seen_by_kid,
       created_by_aid, requested_at, note
FROM souls` + whereSQL +
		fmt.Sprintf(" ORDER BY registered_at DESC, sid ASC OFFSET $%d LIMIT $%d", len(args)+1, len(args)+2)
	listArgs := append(append([]any{}, args...), offset, limit)

	rows, err := db.Query(ctx, listSQL, listArgs...)
	if err != nil {
		return nil, 0, fmt.Errorf("soul: list query: %w", err)
	}
	defer rows.Close()

	var out []*Soul
	for rows.Next() {
		s, err := scanSoul(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("soul: list iter: %w", err)
	}
	return out, total, nil
}

// buildListWhere builds the WHERE for user filter ∩ RBAC scope. The scope
// predicate is the full boolean purview rendered into SQL over the souls columns
// (coven/host/trait) by [soulpurview.Scope.WhereSQL] — all dimensions push down
// to the database, so offset/total stay exact. The scope fragment is always
// atomic ("TRUE"/"FALSE"/parenthesized OR), safe to AND with the user filter;
// its placeholders continue after the filter args. Fail-closed: an empty purview
// renders "FALSE" (zero hosts, never the whole registry).
func buildListWhere(f ListFilter, scope soulpurview.Scope) (string, []any) {
	clauses, args := listFilterClauses(f)
	frag, scopeArgs, _ := scope.WhereSQL(soulpurview.Columns, len(args)+1)
	clauses = append(clauses, frag)
	args = append(args, scopeArgs...)
	return joinWhere(clauses), args
}

// listFilterClauses turns a user [ListFilter] (status/transport/coven) into
// SQL clauses + positional args. Empty fields mean "don't filter". Single
// source of filter semantics: the offset path ([buildListWhere]) and the
// keyset path ([buildScopeEvalSQL]) apply the exact same filter — otherwise
// keyset mode would silently ignore the filter (ADR-047 S3b-2a fix).
func listFilterClauses(f ListFilter) ([]string, []any) {
	var (
		clauses []string
		args    []any
	)
	if f.Status != "" {
		args = append(args, string(f.Status))
		clauses = append(clauses, fmt.Sprintf("status = $%d", len(args)))
	}
	if f.Transport != "" {
		args = append(args, string(f.Transport))
		clauses = append(clauses, fmt.Sprintf("transport = $%d", len(args)))
	}
	if len(f.Covens) > 0 {
		args = append(args, f.Covens)
		clauses = append(clauses, covenMatchSQL(len(args)))
	}
	if f.Unassigned {
		clauses = append(clauses, unassignedSQL)
	}
	if f.SIDPrefix != "" {
		// ESCAPE with a literal backslash: the prefix comes from an operator's
		// keystrokes, so `%`/`_` must match themselves rather than turn an
		// autocomplete query into a wildcard scan.
		args = append(args, escapeLikePrefix(f.SIDPrefix)+"%")
		clauses = append(clauses, fmt.Sprintf(`sid LIKE $%d ESCAPE '\'`, len(args)))
	}
	return clauses, args
}

// unassignedSQL — the host belongs to no incarnation (NIM-371). NOT EXISTS over
// `incarnation_membership`, correlated on `souls.sid`: the column is qualified
// because this fragment also lands in queries that join other tables carrying a
// `sid` (the same reason the purview fragments qualify theirs).
const unassignedSQL = `NOT EXISTS (SELECT 1 FROM incarnation_membership m WHERE m.sid = souls.sid)`

// escapeLikePrefix makes a user-typed string safe as a LIKE prefix: the escape
// character first (otherwise it would double-escape the metacharacters escaped
// below), then the two metacharacters.
func escapeLikePrefix(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, "%", `\%`)
	return strings.ReplaceAll(s, "_", `\_`)
}

// Stats — aggregated summary of the `souls` registry within the operator's
// Purview scope (`GET /v1/souls/stats`, Souls Overview UI). Computed in ONE
// round trip ([SelectStats]) over the same scope predicate as [SelectAll]:
// the aggregate excludes hosts outside operator visibility (otherwise
// dashboard numbers would diverge from the scoped list).
//
//   - ByStatus / ByTransport / ByCoven — dense "axis value → host count"
//     maps. An empty axis (no hosts) still yields an empty (not nil) map.
//     Transport in the model is agent/ssh (NOT pull/push): the UI maps to
//     pull/push labels itself, the storage layer returns domain values as-is.
//   - Total — all visible hosts (= sum of ByStatus).
//   - StaleCount — visible hosts stale by `last_seen_at`
//     (< now()-staleThreshold). Same threshold as the Reaper's
//     `mark_disconnected.stale_after` (reaper.ResolveMarkDisconnectedStale),
//     so the number matches the actual disconnected transition. A host
//     without `last_seen_at` (freshly pending, never connected) is excluded
//     from StaleCount (NULL < X = NULL).
type Stats struct {
	ByStatus    map[Status]int
	ByTransport map[Transport]int
	ByCoven     map[string]int
	Total       int
	StaleCount  int
}

// statsAxisSQL — single CTE aggregate query: `scoped` pins the visible host
// set ONCE (scope WHERE is substituted into %s), then each axis is computed
// via UNION ALL with an `axis` discriminator. The axis='stale' row carries a
// single bucket=COUNT of stale hosts (last_seen_at < now()-$stale); the
// other axes are grouped. One stale-threshold placeholder, last positional
// arg ($%d).
//
// unnest(coven) for by_coven: a host with N labels yields N coven-axis rows
// (sum of by_coven >= total is expected — labels overlap). A host with no
// labels doesn't appear in by_coven (unnest of an empty array = 0 rows).
const statsAxisSQLTemplate = `
WITH scoped AS (
    SELECT status, transport, coven, last_seen_at
    FROM souls%s
)
SELECT 'status'    AS axis, status                     AS bucket, COUNT(*) AS n FROM scoped GROUP BY status
UNION ALL
SELECT 'transport' AS axis, transport                  AS bucket, COUNT(*) AS n FROM scoped GROUP BY transport
UNION ALL
SELECT 'coven'     AS axis, c                          AS bucket, COUNT(*) AS n FROM scoped, unnest(coven) AS c GROUP BY c
UNION ALL
SELECT 'stale'     AS axis, ''                         AS bucket, COUNT(*) AS n FROM scoped WHERE last_seen_at < now() - $%d::interval
`

// SelectStats computes an aggregated summary of the `souls` registry within
// the operator's RBAC scope (`GET /v1/souls/stats`). One round trip: all
// axes (status/transport/coven) + stale count are combined with UNION ALL
// over a shared scope CTE.
//
// The scope predicate is the same full boolean purview rendered by
// [soulpurview.Scope.WhereSQL] as [SelectAll]: a single fail-closed source
// (empty purview → "FALSE" = zero hosts, NOT the entire registry) so the
// aggregate matches the scoped list exactly.
//
// staleThreshold is the StaleCount cutoff (a host is "stale" if
// `last_seen_at < now()-staleThreshold`); passed in from
// reaper.ResolveMarkDisconnectedStale so the number matches the disconnect
// threshold. <= 0 is rejected (a cutoff in the present/future is a
// meaningless aggregate; the caller must pass a real threshold).
func SelectStats(ctx context.Context, db ExecQueryRower, scope soulpurview.Scope, staleThreshold time.Duration) (Stats, error) {
	if staleThreshold <= 0 {
		return Stats{}, fmt.Errorf("soul: stale threshold must be > 0, got %v", staleThreshold)
	}
	stats := Stats{
		ByStatus:    map[Status]int{},
		ByTransport: map[Transport]int{},
		ByCoven:     map[string]int{},
	}

	// The scope fragment is always atomic ("TRUE"/"FALSE"/parenthesized OR),
	// placeholders start at $1; the stale interval is the last positional arg.
	frag, args, _ := scope.WhereSQL(soulpurview.Columns, 1)
	whereSQL := " WHERE " + frag
	// stale threshold is the last positional argument; Go duration → PG
	// interval via the string "<seconds> seconds" (safe for sub-second
	// thresholds).
	args = append(args, fmt.Sprintf("%d seconds", int64(staleThreshold.Seconds())))
	sql := fmt.Sprintf(statsAxisSQLTemplate, whereSQL, len(args))

	rows, err := db.Query(ctx, sql, args...)
	if err != nil {
		return Stats{}, fmt.Errorf("soul: stats query: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			axis   string
			bucket string
			n      int
		)
		if err := rows.Scan(&axis, &bucket, &n); err != nil {
			return Stats{}, fmt.Errorf("soul: stats scan: %w", err)
		}
		switch axis {
		case "status":
			stats.ByStatus[Status(bucket)] = n
			stats.Total += n
		case "transport":
			stats.ByTransport[Transport(bucket)] = n
		case "coven":
			stats.ByCoven[bucket] = n
		case "stale":
			stats.StaleCount = n
		}
	}
	if err := rows.Err(); err != nil {
		return Stats{}, fmt.Errorf("soul: stats iter: %w", err)
	}
	return stats, nil
}
