//go:build integration

package soulforget

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"log"
	"log/slog"
	"math/big"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/souls-guild/soul-stack/keeper/internal/bootstraptoken"
	keepergrpc "github.com/souls-guild/soul-stack/keeper/internal/grpc"
	"github.com/souls-guild/soul-stack/keeper/internal/integrationenv"
	"github.com/souls-guild/soul-stack/keeper/internal/migrate"
	"github.com/souls-guild/soul-stack/keeper/internal/soul"
	"github.com/souls-guild/soul-stack/keeper/internal/soulseed"
	"github.com/souls-guild/soul-stack/keeper/migrations"
)

var integrationPool *pgxpool.Pool

func TestMain(m *testing.M) { os.Exit(run(m)) }

func run(m *testing.M) int {
	ctx, cancel := integrationenv.SetupContext()
	defer cancel()

	ctr, err := integrationenv.Start(ctx, "postgres", func(ctx context.Context) (*tcpostgres.PostgresContainer, error) {
		return tcpostgres.Run(ctx,
			"postgres:16-alpine",
			tcpostgres.WithDatabase("keeper_test"),
			tcpostgres.WithUsername("keeper"),
			tcpostgres.WithPassword("keeper"),
			tcpostgres.BasicWaitStrategies(),
		)
	})
	if err != nil {
		if requireDocker() {
			log.Fatalf("soulforget integration: setup failed (REQUIRE_DOCKER): %v", err)
		}
		log.Printf("soulforget integration: skipping, docker unavailable: %v", err)
		return 0
	}
	defer func() {
		termCtx, termCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer termCancel()
		_ = ctr.Terminate(termCtx)
	}()

	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		log.Printf("ConnectionString: %v", err)
		return 1
	}
	if err := migrate.Apply(ctx, dsn, migrations.FS, "."); err != nil {
		log.Printf("migrate.Apply: %v", err)
		return 1
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		log.Printf("pgxpool.New: %v", err)
		return 1
	}
	defer pool.Close()
	integrationPool = pool

	return m.Run()
}

const testSID = "host1.example.com"
const testReason = "forgotten by archon-alice"

func resetAll(t *testing.T) {
	t.Helper()
	_, err := integrationPool.Exec(context.Background(),
		`TRUNCATE TABLE soul_seeds, bootstrap_tokens, incarnation_choir_voices,
		 incarnation_choirs, incarnation_membership, incarnation, souls, operators, audit_log CASCADE`)
	if err != nil {
		t.Fatalf("TRUNCATE: %v", err)
	}
}

func seedSoul(t *testing.T, sid string, status soul.Status) {
	t.Helper()
	s := &soul.Soul{SID: sid, Transport: soul.TransportAgent, Status: status}
	if err := soul.Insert(context.Background(), integrationPool, s); err != nil {
		t.Fatalf("seedSoul(%s): %v", sid, err)
	}
}

func seedActiveSeed(t *testing.T, sid, fingerprint, serial string) {
	t.Helper()
	s := &soulseed.SoulSeed{
		SID:          sid,
		Fingerprint:  fingerprint,
		SerialNumber: serial,
		ExpiresAt:    time.Now().Add(24 * time.Hour),
	}
	if err := soulseed.Insert(context.Background(), integrationPool, s); err != nil {
		t.Fatalf("seed insert(%s): %v", serial, err)
	}
}

func hexFingerprint(c byte) string { return strings.Repeat(string(c), 64) }

func countRows(t *testing.T, table, sid string) int64 {
	t.Helper()
	var n int64
	err := integrationPool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM `+table+` WHERE sid = $1`, sid).Scan(&n)
	if err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// --- G1: a forgotten host cannot come back ------------------------------------

// TestIntegration_ForgottenHostCannotAuthenticate is the guarantee the whole
// operation rests on, checked against the real authenticator rather than
// inferred from an absent row.
//
// The claim is "we revoke its certificate, or some mechanism, so it cannot
// reconnect to us". The mechanism is that there is no deny-list to consult:
// [keepergrpc.SeedAuthenticator] looks the peer's fingerprint up in
// `soul_seeds`, the seed rows hang off `souls.sid` ON DELETE CASCADE, and a
// lookup that finds nothing fails closed. Should the FK ever be relaxed to
// SET NULL, or the authenticator ever start treating an unknown fingerprint as
// anything but a rejection, a forgotten host would reconnect — and this test is
// what says so.
func TestIntegration_ForgottenHostCannotAuthenticate(t *testing.T) {
	resetAll(t)
	ctx := context.Background()

	cert := selfSignedCert(t, testSID)
	fp := soulseed.FingerprintFromCert(cert)
	seedSoul(t, testSID, soul.StatusConnected)
	seedActiveSeed(t, testSID, fp, "01")

	seed, err := soulseed.SelectByFingerprint(ctx, integrationPool, fp)
	if err != nil {
		t.Fatalf("precondition: reading back the seed we just wrote: %v", err)
	}
	seedID := seed.SeedID

	auth := keepergrpc.NewSeedAuthenticator(integrationPool, slog.New(slog.DiscardHandler))
	peerCtx := ctxWithPeerCert(cert)

	sid, err := auth.Authenticate(peerCtx)
	if err != nil {
		t.Fatalf("precondition: the host cannot authenticate even BEFORE being forgotten (%v) — "+
			"the test would prove nothing", err)
	}
	if sid != testSID {
		t.Fatalf("precondition: authenticated as %q, want %q", sid, testSID)
	}

	// A control host whose seed was merely REVOKED, its `soul_seeds` row still
	// in place. The authenticator rejects both with codes.Unauthenticated, so
	// the code alone cannot tell "the allowlist entry is gone" from "the entry
	// is still there, marked dead" — and that difference IS the cascade. This
	// control gives the assertion below something to differ from.
	const revokedHost = "host2.example.com"
	revokedCert := selfSignedCert(t, revokedHost)
	seedSoul(t, revokedHost, soul.StatusConnected)
	seedActiveSeed(t, revokedHost, soulseed.FingerprintFromCert(revokedCert), "02")
	mustExec(t, `UPDATE soul_seeds SET status = 'revoked' WHERE sid = $1`, revokedHost)

	_, revokedErr := auth.Authenticate(ctxWithPeerCert(revokedCert))
	if revokedErr == nil {
		t.Fatal("precondition: a revoked seed authenticated — the control proves nothing")
	}

	if _, err := Erase(ctx, integrationPool, nil, testSID, testReason); err != nil {
		t.Fatalf("Erase: %v", err)
	}

	_, err = auth.Authenticate(peerCtx)
	if err == nil {
		t.Fatal("the forgotten host authenticated with the SAME certificate — it can reconnect, " +
			"which is the one thing forgetting is supposed to prevent")
	}
	if got := grpcstatus.Code(err); got != codes.Unauthenticated {
		t.Errorf("code = %v (%v), want Unauthenticated — anything else (Unavailable, Internal) "+
			"is a transient the agent will retry through", got, err)
	}

	// The rejection must be the unknown-fingerprint one. Getting the control's
	// answer instead would mean the row survived the delete with a status flag
	// flipped on it — a deny-list, kept in a table nothing prunes, which the
	// next `UPDATE ... SET status='active'` undoes.
	got, control := grpcstatus.Convert(err).Message(), grpcstatus.Convert(revokedErr).Message()
	if got == control {
		t.Errorf("the forgotten host was rejected exactly like the merely-revoked one (%q) — "+
			"its seed row is still in `soul_seeds` and the FK cascade did not fire", got)
	}
	if !strings.Contains(got, "unknown") {
		t.Errorf("rejection = %q, want the unknown-fingerprint answer — the seed row is expected "+
			"to be GONE, not merely marked", got)
	}

	// And the row itself, by its own id: the cascade removed it, nothing renamed
	// or re-inserted it.
	var n int64
	if err := integrationPool.QueryRow(ctx,
		`SELECT COUNT(*) FROM soul_seeds WHERE seed_id::text = $1`, seedID).Scan(&n); err != nil {
		t.Fatalf("count seed by id: %v", err)
	}
	if n != 0 {
		t.Errorf("the forgotten host's seed row %s is still in `soul_seeds` — forgetting relies on "+
			"the ON DELETE CASCADE from `souls`, and nothing in Go deletes this row", seedID)
	}
}

// TestIntegration_EveryForeignKeyToSoulsCascades — the whole operation is one
// `DELETE FROM souls`. Not a line of Go removes a seed row, a bootstrap token,
// a membership or a Choir Voice; the schema does, because every FK pointing at
// `souls` carries ON DELETE CASCADE.
//
// So the guarantee lives in `pg_constraint`, and the two ways it breaks are
// both silent from Go. A future migration that writes ON DELETE SET NULL leaves
// the forgotten host's credential in the allowlist with a NULL sid — it
// authenticates and reconnects. NO ACTION (the PostgreSQL default when the
// clause is omitted, 'a' here) makes the DELETE itself fail with a foreign key
// violation, which the endpoint reports as a 500 on a host that still exists.
// The test above checks one row for one table; this one checks the topology,
// including tables added after it was written.
func TestIntegration_EveryForeignKeyToSoulsCascades(t *testing.T) {
	rows, err := integrationPool.Query(context.Background(), `
		SELECT c.conname, c.conrelid::regclass::text, c.confdeltype::text
		FROM pg_constraint c
		WHERE c.contype = 'f' AND c.confrelid = 'souls'::regclass
		ORDER BY 2, 1`)
	if err != nil {
		t.Fatalf("pg_constraint scan: %v", err)
	}
	defer rows.Close()

	seen := make(map[string]bool)
	for rows.Next() {
		var name, child, delAction string
		if err := rows.Scan(&name, &child, &delAction); err != nil {
			t.Fatalf("scan: %v", err)
		}
		seen[child] = true
		// 'c' = CASCADE, 'a' = NO ACTION, 'r' = RESTRICT, 'n' = SET NULL,
		// 'd' = SET DEFAULT in pg_constraint.confdeltype.
		if delAction != "c" {
			t.Errorf("FK %s on %s references souls with ON DELETE %q, want \"c\" (CASCADE) — "+
				"forgetting a host is a plain DELETE and depends on this: SET NULL/SET DEFAULT "+
				"orphans the row instead of removing it, RESTRICT/NO ACTION makes the delete fail",
				name, child, delAction)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}

	// The counted tables must still BE in that list. A migration that drops the
	// FK to move the cleanup into application code would leave every row above
	// green while the rows themselves quietly survive the delete.
	for _, child := range []string{
		"soul_seeds", "bootstrap_tokens", "incarnation_membership", "incarnation_choir_voices",
	} {
		if !seen[child] {
			t.Errorf("%s has no foreign key to souls — the forget path never deletes from it "+
				"by hand, so its rows now outlive the host", child)
		}
	}
	if len(seen) == 0 {
		t.Fatal("no foreign keys to souls at all — the query matched nothing, so every " +
			"assertion above was vacuous")
	}
}

// --- G2: the counts are measured, not guessed ---------------------------------

// TestIntegration_SeedsRevokedCountsOnlyLiveCredentials — the reply says how
// many credentials the host still held. Counting the seed rows before the delete
// instead would include expired and already-revoked history and report a host as
// having held credentials it had not held for months.
func TestIntegration_SeedsRevokedCountsOnlyLiveCredentials(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	seedSoul(t, testSID, soul.StatusConnected)

	// Rotation history: one superseded seed and one current active seed — both
	// still usable credentials, both must be counted.
	seedActiveSeed(t, testSID, hexFingerprint('a'), "01")
	if err := soulseed.SupersedeBySID(ctx, integrationPool, testSID); err != nil {
		t.Fatalf("SupersedeBySID: %v", err)
	}
	seedActiveSeed(t, testSID, hexFingerprint('b'), "02")

	// Dead history: expired, revoked earlier, and orphaned — the state a seed is
	// left in when its host was cascade-deleted by `core.cloud.provisioned
	// destroyed` (ADR-017). None of the three is a credential the host still
	// held, so none may inflate the count, and `orphaned` is the one a status
	// list written from memory forgets.
	mustExec(t, `INSERT INTO soul_seeds (sid, fingerprint, serial_number, issued_at, expires_at, status)
	             VALUES ($1, $2, '03', NOW() - INTERVAL '2 days', NOW() - INTERVAL '1 day', 'expired')`,
		testSID, hexFingerprint('c'))
	mustExec(t, `INSERT INTO soul_seeds (sid, fingerprint, serial_number, expires_at, status)
	             VALUES ($1, $2, '04', NOW() + INTERVAL '1 day', 'revoked')`, testSID, hexFingerprint('d'))
	mustExec(t, `INSERT INTO soul_seeds (sid, fingerprint, serial_number, expires_at, status)
	             VALUES ($1, $2, '05', NOW() + INTERVAL '1 day', 'orphaned')`, testSID, hexFingerprint('f'))

	res, err := Erase(ctx, integrationPool, nil, testSID, testReason)
	if err != nil {
		t.Fatalf("Erase: %v", err)
	}
	if res.SeedsRevoked != 2 {
		t.Errorf("seeds_revoked = %d, want 2 (the active + the superseded one). "+
			"5 means the count is taken over the seed history rather than over the UPDATE; "+
			"0 means it is taken after the DELETE, when everything is already gone.",
			res.SeedsRevoked)
	}
}

// TestIntegration_BootstrapsBurnedLeavesNothingRedeemable — a host forgotten before
// it ever onboarded has an outstanding bootstrap token. Reporting a count while
// leaving the token usable would be the "deleted the record, did not release the
// resource" failure in its purest form.
func TestIntegration_BootstrapsBurnedLeavesNothingRedeemable(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	seedSoul(t, testSID, soul.StatusPending)

	tokenHash := hexFingerprint('e')
	if _, err := bootstraptoken.Insert(ctx, integrationPool, testSID, tokenHash, time.Hour, nil); err != nil {
		t.Fatalf("bootstraptoken.Insert: %v", err)
	}

	res, err := Erase(ctx, integrationPool, nil, testSID, testReason)
	if err != nil {
		t.Fatalf("Erase: %v", err)
	}
	if res.BootstrapsBurned != 1 {
		t.Errorf("bootstraps_burned = %d, want 1", res.BootstrapsBurned)
	}
	if _, err := bootstraptoken.SelectByHash(ctx, integrationPool, tokenHash); err == nil {
		t.Error("the bootstrap token is still in the registry after the host was forgotten — " +
			"it would onboard a host that no longer exists")
	}
}

// --- G4: the cascade is reported, never silent --------------------------------

// TestIntegration_CascadeCountsReportWhatTheOperatorDidNotName — the request
// names one SID; the delete also empties incarnation rosters and Choirs, because
// every FK on `souls(sid)` is ON DELETE CASCADE. Those rows are gone by the time
// anyone could look, so the reply is the only place they are ever mentioned.
// A count of zero here reads as "nothing else was touched" and would be a lie.
func TestIntegration_CascadeCountsReportWhatTheOperatorDidNotName(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	seedSoul(t, testSID, soul.StatusConnected)
	seedActiveSeed(t, testSID, hexFingerprint('a'), "01")
	// A bootstrap token too, so the `bootstrap_tokens: 0` assertion below is
	// about a row that was there and went, not about an empty table.
	if _, err := bootstraptoken.Insert(ctx, integrationPool, testSID, hexFingerprint('e'), time.Hour, nil); err != nil {
		t.Fatalf("bootstraptoken.Insert: %v", err)
	}

	mustExec(t, `INSERT INTO incarnation (name, service, service_version, status)
	             VALUES ('inc-one', 'svc', 'v1', 'ready'), ('inc-two', 'svc', 'v1', 'ready')`)
	mustExec(t, `INSERT INTO incarnation_membership (incarnation_name, sid)
	             VALUES ('inc-one', $1), ('inc-two', $1)`, testSID)
	mustExec(t, `INSERT INTO incarnation_choirs (incarnation_name, choir_name)
	             VALUES ('inc-one', 'db'), ('inc-one', 'web')`)
	mustExec(t, `INSERT INTO incarnation_choir_voices (incarnation_name, choir_name, sid, role)
	             VALUES ('inc-one', 'db', $1, 'primary'), ('inc-one', 'web', $1, 'worker')`, testSID)

	// Every table the loop at the end reads must hold something first, or a
	// broken cascade and an empty fixture are the same green.
	cascading := []string{"soul_seeds", "bootstrap_tokens", "incarnation_membership", "incarnation_choir_voices"}
	for _, table := range cascading {
		if countRows(t, table, testSID) == 0 {
			t.Fatalf("precondition: %s has no rows for the host before it is forgotten — "+
				"the cascade assertion would pass over an empty table", table)
		}
	}

	res, err := Erase(ctx, integrationPool, nil, testSID, testReason)
	if err != nil {
		t.Fatalf("Erase: %v", err)
	}
	if res.MembershipsSevered != 2 {
		t.Errorf("memberships_severed = %d, want 2 — two incarnation rosters just lost this host "+
			"and the reply is the only record of it", res.MembershipsSevered)
	}
	if res.ChoirVoicesRemoved != 2 {
		t.Errorf("choir_voices_removed = %d, want 2 — the host's declared roles went with its Voices",
			res.ChoirVoicesRemoved)
	}
	if !res.TouchedFleet() {
		t.Error("TouchedFleet() = false while rosters and Choirs were changed")
	}

	// And the cascade did fire: the counts describe rows that are actually gone,
	// not rows that were counted and left behind.
	for _, table := range cascading {
		if got := countRows(t, table, testSID); got != 0 {
			t.Errorf("%s still holds %d row(s) for the forgotten host — they were counted "+
				"in the reply and left in place", table, got)
		}
	}
	if _, err := soul.SelectBySID(ctx, integrationPool, testSID); !errors.Is(err, soul.ErrSoulNotFound) {
		t.Errorf("souls row lookup err = %v, want ErrSoulNotFound", err)
	}
}

// TestIntegration_ForgetTouchesOnlyTheNamedHost — a bystander sharing an
// incarnation with the forgotten host keeps its own membership and Voice.
func TestIntegration_ForgetTouchesOnlyTheNamedHost(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	const bystander = "host2.example.com"
	seedSoul(t, testSID, soul.StatusConnected)
	seedSoul(t, bystander, soul.StatusConnected)

	mustExec(t, `INSERT INTO incarnation (name, service, service_version, status)
	             VALUES ('inc-one', 'svc', 'v1', 'ready')`)
	mustExec(t, `INSERT INTO incarnation_membership (incarnation_name, sid)
	             VALUES ('inc-one', $1), ('inc-one', $2)`, testSID, bystander)
	mustExec(t, `INSERT INTO incarnation_choirs (incarnation_name, choir_name) VALUES ('inc-one', 'db')`)
	mustExec(t, `INSERT INTO incarnation_choir_voices (incarnation_name, choir_name, sid, role)
	             VALUES ('inc-one', 'db', $1, 'primary'), ('inc-one', 'db', $2, 'replica')`, testSID, bystander)

	if _, err := Erase(ctx, integrationPool, nil, testSID, testReason); err != nil {
		t.Fatalf("Erase: %v", err)
	}
	if got := countRows(t, "incarnation_membership", bystander); got != 1 {
		t.Errorf("the bystander's membership rows = %d, want 1", got)
	}
	if got := countRows(t, "incarnation_choir_voices", bystander); got != 1 {
		t.Errorf("the bystander's Choir Voices = %d, want 1", got)
	}
	if _, err := soul.SelectBySID(ctx, integrationPool, bystander); err != nil {
		t.Errorf("the bystander was removed too: %v", err)
	}
}

// --- state, and the absence of a state gate -----------------------------------

// TestIntegration_ForgetIsLegalInEveryStatus — a host can be forgotten whether
// or not the cluster ever met it, and whether or not it is connected right now.
// There is deliberately no decommission-first step: the answer to "can I forget
// a node we do not know?" was yes, and the reconnect guarantee does not depend
// on the host's state. The status it carried is reported instead of enforced.
func TestIntegration_ForgetIsLegalInEveryStatus(t *testing.T) {
	for _, st := range []soul.Status{
		soul.StatusPending, soul.StatusConnected, soul.StatusDisconnected,
		soul.StatusRevoked, soul.StatusExpired, soul.StatusDestroyed,
	} {
		t.Run(string(st), func(t *testing.T) {
			resetAll(t)
			seedSoul(t, testSID, st)
			res, err := Erase(context.Background(), integrationPool, nil, testSID, testReason)
			if err != nil {
				t.Fatalf("forgetting a %s host was refused: %v", st, err)
			}
			if res.StatusBefore != string(st) {
				t.Errorf("status_before = %q, want %q — the operator's own record should say "+
					"whether they erased a dead host or a live one", res.StatusBefore, st)
			}
		})
	}
}

func TestIntegration_ForgetUnknownHostIsNotFound(t *testing.T) {
	resetAll(t)
	_, err := Erase(context.Background(), integrationPool, nil, "ghost.example.com", testReason)
	if !errors.Is(err, soul.ErrSoulNotFound) {
		t.Errorf("err = %v, want soul.ErrSoulNotFound (the endpoint's 404)", err)
	}
}

// --- the release half: what could not be freed must be visible ----------------

// TestIntegration_UnpurgedCacheIsWarnedAboutNotSwallowed is the guard the ticket
// was written around: deleting the row and releasing the resources are different
// things, and a partial release must never be rendered as a plain success.
//
// The heartbeat hash carries no TTL, so a purge that failed leaves a key nothing
// will ever collect. The delete is already committed and is not undone — the
// host cannot reconnect either way — but the operator has to be told which
// resource is still held, by name.
func TestIntegration_UnpurgedCacheIsWarnedAboutNotSwallowed(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	seedSoul(t, testSID, soul.StatusConnected)

	// The teardown reports a count AND an error — a partial purge. Scripting it
	// as (0, err) would make the assertion on the count read the fake's zero
	// value rather than anything Erase does with it.
	td := &scriptedTeardown{
		broadcastSent: true,
		purged:        2,
		purgeErr:      errors.New("redis went away mid-forget"),
	}
	res, err := Erase(ctx, integrationPool, td, testSID, testReason)
	if err != nil {
		t.Fatalf("Erase: %v", err)
	}
	if len(res.Warnings) == 0 {
		t.Fatal("the Redis keys could not be purged and the reply is a clean success — " +
			"`soul:<sid>:hb` has no TTL, so the operator has just been told a leak did not happen")
	}
	// Named in full: `hb` alone is not something an operator can go and delete,
	// and the audit payload is where they will read it back from.
	wantKey := "soul:" + testSID + ":hb"
	if !strings.Contains(res.Warnings[0], wantKey) {
		t.Errorf("warning = %q, want it to name %q — the operator has to be able to find the "+
			"key that was left behind", res.Warnings[0], wantKey)
	}
	if res.CacheKeysPurged != 0 {
		t.Errorf("cache_keys_purged = %d beside a failed purge, want 0 — the field goes into the "+
			"audit payload on its own, where a number reads as a release that happened",
			res.CacheKeysPurged)
	}
	// The delete stands: reversing a committed transaction over an unreleased
	// cache key would resurrect a host the cluster has already been told to drop.
	if _, err := soul.SelectBySID(ctx, integrationPool, testSID); !errors.Is(err, soul.ErrSoulNotFound) {
		t.Errorf("souls row lookup err = %v, want ErrSoulNotFound", err)
	}
}

// TestIntegration_SecondBroadcastFailureIsWarnedAboutNotSwallowed — the
// post-delete notice catches a stream reopened during the transaction. Losing it
// is not fatal (the pre-flight notice already went out and the host can no longer
// authenticate) but it is not nothing either, and it is not the operator's job to
// guess which.
func TestIntegration_SecondBroadcastFailureIsWarnedAboutNotSwallowed(t *testing.T) {
	resetAll(t)
	seedSoul(t, testSID, soul.StatusConnected)

	td := &scriptedTeardown{broadcastErr: []error{nil, errors.New("redis went away mid-forget")}}
	res, err := Erase(context.Background(), integrationPool, td, testSID, testReason)
	if err != nil {
		t.Fatalf("Erase: %v", err)
	}
	if len(res.Warnings) == 0 {
		t.Error("the second teardown notice failed and the reply is a clean success")
	}
	if res.Broadcast {
		t.Error("broadcast = true although the second notice failed to go out")
	}
}

// TestIntegration_BroadcastNotSentIsNeverReportedAsSent — a Keeper with no Redis
// is single-instance by construction, so there is nobody to notify and no failure
// either. Recording that as a broadcast would put a notice in the operator's
// record that was never published — a small lie of exactly the kind this
// operation exists to avoid.
func TestIntegration_BroadcastNotSentIsNeverReportedAsSent(t *testing.T) {
	resetAll(t)
	seedSoul(t, testSID, soul.StatusConnected)

	td := &scriptedTeardown{broadcastSent: false, closeLocal: true}
	res, err := Erase(context.Background(), integrationPool, td, testSID, testReason)
	if err != nil {
		t.Fatalf("Erase: %v", err)
	}
	if res.Broadcast {
		t.Error("broadcast = true although the teardown reported that no notice went out")
	}
	if !res.LocalStreamClosed {
		t.Error("local_stream_closed = false although this instance held a stream and closed it")
	}
	if len(res.Warnings) != 0 {
		t.Errorf("warnings = %v, want none — having no cluster to tell is not a failed release", res.Warnings)
	}
}

// TestIntegration_FullReleaseReportsNoWarnings — the mirror of the two guards
// above. If a clean run ever started emitting warnings they would stop meaning
// anything, and a real unreleased resource would read as noise.
func TestIntegration_FullReleaseReportsNoWarnings(t *testing.T) {
	resetAll(t)
	seedSoul(t, testSID, soul.StatusConnected)

	td := &scriptedTeardown{broadcastSent: true, closeLocal: true, purged: 3}
	res, err := Erase(context.Background(), integrationPool, td, testSID, testReason)
	if err != nil {
		t.Fatalf("Erase: %v", err)
	}
	if len(res.Warnings) != 0 {
		t.Errorf("warnings = %v, want none on a fully released host", res.Warnings)
	}
	if !res.Broadcast || !res.LocalStreamClosed || res.CacheKeysPurged != 3 {
		t.Errorf("release = %+v, want everything released", res.Release)
	}
}

// --- helpers ------------------------------------------------------------------

func mustExec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := integrationPool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("exec %.60s…: %v", sql, err)
	}
}

// selfSignedCert makes a leaf whose SubjectPublicKeyInfo fingerprint is what
// `soul_seeds.fingerprint` holds — the same value the onboarding path writes via
// [soulseed.FingerprintFromCert].
func selfSignedCert(t *testing.T, cn string) *x509.Certificate {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		t.Fatalf("CreateCertificate: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("ParseCertificate: %v", err)
	}
	return cert
}

// ctxWithPeerCert builds the context an mTLS EventStream handler sees.
func ctxWithPeerCert(cert *x509.Certificate) context.Context {
	return peer.NewContext(context.Background(), &peer.Peer{
		Addr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1},
		AuthInfo: credentials.TLSInfo{State: tls.ConnectionState{
			PeerCertificates: []*x509.Certificate{cert},
		}},
	})
}
