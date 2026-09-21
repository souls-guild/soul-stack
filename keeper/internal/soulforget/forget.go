// Package soulforget implements the operator-facing "forget this host"
// operation behind `soul.forget` (NIM-386, `DELETE /v1/souls/{sid}`).
//
// # Why the operation exists at all
//
// A host can die for good — the machine is scrapped, the tenant leaves, the
// lab VM is thrown away. Before NIM-386 there was no operator path to take it
// out of the registry: `/v1/souls/{sid}` had a GET and nothing else, and the
// only code that ever deleted a `souls` row was the cloud-destroy cascade,
// reachable solely from inside a scenario run. An operator was left with a
// permanently `disconnected` row that no rule ever collects.
//
// # What makes a forgotten host stay gone
//
// Seed authentication is an ALLOWLIST, not a deny-list: the EventStream
// interceptor looks the peer's certificate fingerprint up in `soul_seeds` and
// rejects anything it cannot find ([keeper/internal/grpc.SeedAuthenticator]).
// The seed rows hang off `souls.sid` with ON DELETE CASCADE, so erasing the
// registry row erases the allowlist entry, and the host's next connect fails
// closed with `unknown soul seed`. That is the whole reconnect guarantee, and
// it is structural rather than a rule someone has to remember to apply.
//
// # Deleting a row is not the same as releasing what it held
//
// Four foreign keys point at `souls(sid)` and all four are ON DELETE CASCADE:
// `soul_seeds` (009), `bootstrap_tokens` (008), `incarnation_choir_voices`
// (060) and `incarnation_membership` (099). A bare DELETE therefore also
// empties rosters and Choirs — silently, with the operator shown nothing. So
// [Forget] counts what the cascade is about to take BEFORE it fires and
// reports it back; the endpoint never answers a bare 204.
//
// The live EventStream is the other half, and PG cannot reach it: the seed is
// checked once per stream, at open, so a host that is connected right now keeps
// talking to a Keeper that no longer has a row for it. Tearing that stream down
// is the caller's job (see [Teardown] and the handler), not this file's.
package soulforget

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/souls-guild/soul-stack/keeper/internal/bootstraptoken"
	"github.com/souls-guild/soul-stack/keeper/internal/soul"
	"github.com/souls-guild/soul-stack/keeper/internal/soulseed"
)

// TxBeginner is the pool surface [Forget] needs — one transaction. Satisfied
// by `*pgxpool.Pool` and by the handler's own pool interface; unit tests pass
// a fake.
type TxBeginner interface {
	BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error)
}

// Counts is the truthful report of what forgetting one host took with it.
// Every field is measured against the database inside the same transaction
// that does the delete, never inferred: the operator has to be able to read
// "and it dropped 3 memberships" off the reply rather than discover it later
// from an empty roster.
type Counts struct {
	// StatusBefore is the `souls.status` the row carried at the moment of the
	// delete. Reported because forgetting is legal in every state, so the
	// operator's own record of the call should say whether they erased a dead
	// host or a live one.
	StatusBefore string
	// SeedsRevoked is how many still-usable seeds (`active` + `superseded`)
	// were revoked. Measured on the UPDATE, not counted beforehand — see
	// [soulseed.RevokeAllBySID].
	SeedsRevoked int64
	// BootstrapsBurned is how many unused bootstrap tokens were burned. A host
	// forgotten before it ever onboarded had one outstanding; it must not stay
	// redeemable.
	//
	// "Unused" is `used_at IS NULL` and nothing more: unlike [SeedsRevoked],
	// this count does NOT exclude tokens that had already expired, because
	// [bootstraptoken.BurnAllForSID] deliberately does not filter on
	// `expires_at` — burning a token whose window has closed costs nothing and
	// removes any argument about clock skew at the boundary. So the two
	// numbers beside each other answer different questions: seeds says how
	// many credentials were LIVE, bootstraps says how many were UNREDEEMED.
	// Do not "align" them by adding an expiry filter here; the burn is shared
	// with the cloud-destroy cascade, which needs the unfiltered form.
	//
	// Named without a `token` substring on purpose, all the way out to the wire
	// key `bootstraps_burned`. The audit secret-mask matches key names by
	// substring and replaces the value whatever its type ([audit.MaskSecrets]),
	// and this count reaches the operator ONLY through the audit trail — the
	// `bootstrap_tokens` rows it describes cascade away with the `souls` row in
	// the same transaction. A key called `tokens_burned` is stored as
	// `***MASKED***`, which loses the number permanently rather than protecting
	// anything. Guarded on both surfaces; see
	// handlers.TestSoulForgetAuditPayload_SurvivesTheSecretMask.
	BootstrapsBurned int64
	// MembershipsSevered is how many `incarnation_membership` rows the cascade
	// removed — i.e. how many incarnation rosters just lost this host.
	MembershipsSevered int64
	// ChoirVoicesRemoved is how many `incarnation_choir_voices` rows the
	// cascade removed. A Voice carries the host's declared role, and the
	// cascade takes the role with it.
	ChoirVoicesRemoved int64
}

// TouchedFleet reports whether forgetting this host changed anything beyond
// the host itself — a roster or a Choir lost a member. Callers use it to
// decide how loudly to speak, not whether to proceed.
func (c Counts) TouchedFleet() bool {
	return c.MembershipsSevered > 0 || c.ChoirVoicesRemoved > 0
}

const lockSoulSQL = `
SELECT status FROM souls WHERE sid = $1 FOR UPDATE
`

const countMembershipsSQL = `
SELECT COUNT(*) FROM incarnation_membership WHERE sid = $1
`

const countChoirVoicesSQL = `
SELECT COUNT(*) FROM incarnation_choir_voices WHERE sid = $1
`

// Forget erases one host from the registry in a single transaction and returns
// what that cost. It is the PG half of the operation; the stream teardown and
// the Redis key purge live in the handler, which runs them around this call.
//
// Order inside the transaction, and why:
//
//  1. `SELECT … FOR UPDATE` — locks the row and reads the status about to be
//     erased. Missing row → [soul.ErrSoulNotFound] (the endpoint's 404).
//  2. count memberships and Choir Voices — AFTER the delete they are all zero,
//     and a report of zero would read as "nothing else was touched".
//  3. revoke the live seeds, naming the reason.
//  4. burn the unused bootstrap tokens under [bootstraptoken.SystemKIDSoulForget].
//  5. DELETE the row, taking the four cascades with it.
//
// reason is written to `soul_seeds.revocation_reason` and should name the
// operator, since after the delete the seed rows are gone and the audit event
// is the only place the act survives.
func Forget(ctx context.Context, db TxBeginner, sid, reason string) (Counts, error) {
	if db == nil {
		return Counts{}, errors.New("soulforget: nil pool")
	}
	if !soul.ValidSID(sid) {
		return Counts{}, fmt.Errorf("soulforget: invalid SID %q", sid)
	}
	if reason == "" {
		return Counts{}, errors.New("soulforget: reason is empty")
	}

	var counts Counts
	err := pgx.BeginTxFunc(ctx, db, pgx.TxOptions{}, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, lockSoulSQL, sid).Scan(&counts.StatusBefore); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return soul.ErrSoulNotFound
			}
			return fmt.Errorf("soulforget: lock souls row: %w", err)
		}

		if err := tx.QueryRow(ctx, countMembershipsSQL, sid).Scan(&counts.MembershipsSevered); err != nil {
			return fmt.Errorf("soulforget: count memberships: %w", err)
		}
		if err := tx.QueryRow(ctx, countChoirVoicesSQL, sid).Scan(&counts.ChoirVoicesRemoved); err != nil {
			return fmt.Errorf("soulforget: count choir voices: %w", err)
		}

		n, err := soulseed.RevokeAllBySID(ctx, tx, sid, reason)
		if err != nil {
			return fmt.Errorf("soulforget: revoke seeds: %w", err)
		}
		counts.SeedsRevoked = n

		n, err = bootstraptoken.BurnAllForSID(ctx, tx, sid, bootstraptoken.SystemKIDSoulForget)
		if err != nil {
			return fmt.Errorf("soulforget: burn bootstrap tokens: %w", err)
		}
		counts.BootstrapsBurned = n

		if err := soul.DeleteBySID(ctx, tx, sid); err != nil {
			return fmt.Errorf("soulforget: delete souls row: %w", err)
		}
		return nil
	})
	if err != nil {
		return Counts{}, err
	}
	return counts, nil
}
