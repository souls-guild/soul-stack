//go:build integration

package bootstrap_test

import (
	"context"
	"errors"
	"log"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/souls-guild/soul-stack/keeper/internal/bootstraptoken"
	coremodbootstrap "github.com/souls-guild/soul-stack/keeper/internal/coremod/bootstrap"
	"github.com/souls-guild/soul-stack/keeper/internal/integrationenv"
	"github.com/souls-guild/soul-stack/keeper/internal/migrate"
	keepersoul "github.com/souls-guild/soul-stack/keeper/internal/soul"
	"github.com/souls-guild/soul-stack/keeper/migrations"
)

var issuedIntegrationPool *pgxpool.Pool

func TestMain(m *testing.M) { os.Exit(runIssuedIntegration(m)) }

func runIssuedIntegration(m *testing.M) int {
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
		if integrationenv.RequireDocker() {
			log.Fatalf("coremod/bootstrap integration: setup failed (REQUIRE_DOCKER): %v", err)
		}
		log.Printf("coremod/bootstrap integration: skipping, docker unavailable: %v", err)
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
	issuedIntegrationPool, err = pgxpool.New(ctx, dsn)
	if err != nil {
		log.Printf("pgxpool.New: %v", err)
		return 1
	}
	defer issuedIntegrationPool.Close()
	return m.Run()
}

// runIncarnation is the incarnation of the run under test. Ownership is what
// separates a converged pass-through from an identity takeover (NIM-780), so
// every issuance below names one instead of relying on the "" default.
const runIncarnation = "redis-sa"

func resetIssuedIntegration(t *testing.T) {
	t.Helper()
	if _, err := issuedIntegrationPool.Exec(context.Background(),
		`TRUNCATE TABLE soul_seeds, bootstrap_tokens, incarnation_membership,
		 incarnation, souls, audit_log CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
}

func seedIssuedIncarnation(t *testing.T, name string) {
	t.Helper()
	if _, err := issuedIntegrationPool.Exec(context.Background(),
		`INSERT INTO incarnation (id, service, service_version, status)
		 VALUES ($1, 'redis', 'main', 'ready')`, name); err != nil {
		t.Fatalf("seed incarnation %q: %v", name, err)
	}
}

func seedIssuedMembership(t *testing.T, incarnation, sid string) {
	t.Helper()
	if _, err := issuedIntegrationPool.Exec(context.Background(),
		`INSERT INTO incarnation_membership (incarnation_name, sid) VALUES ($1, $2)`,
		incarnation, sid); err != nil {
		t.Fatalf("seed membership %s/%s: %v", incarnation, sid, err)
	}
}

func TestIntegration_IssuedBatch_CreateReissueAndExpiry(t *testing.T) {
	resetIssuedIntegration(t)
	ctx := context.Background()
	issuer := coremodbootstrap.NewIssuerPG(issuedIntegrationPool, time.Hour)
	sids := []string{"vm1.example.com", "vm2.example.com"}

	first, err := issuer.IssueBatch(ctx, sids, runIncarnation)
	if err != nil {
		t.Fatalf("first IssueBatch: %v", err)
	}
	if len(first) != 2 || !first[0].Created || !first[1].Created {
		t.Fatalf("first issuance count/created flags = %d/%t/%t, want 2/true/true",
			len(first), len(first) > 0 && first[0].Created, len(first) > 1 && first[1].Created)
	}
	firstHashes := map[string]string{}
	for _, h := range first {
		var transport, status, hash string
		if err := issuedIntegrationPool.QueryRow(ctx, `
SELECT s.transport, s.status, t.token_hash
FROM souls s JOIN bootstrap_tokens t USING (sid)
WHERE s.sid=$1 AND t.used_at IS NULL`, h.SID).Scan(&transport, &status, &hash); err != nil {
			t.Fatalf("read %s: %v", h.SID, err)
		}
		if transport != string(keepersoul.TransportAgent) || status != string(keepersoul.StatusPending) {
			t.Fatalf("%s transport/status = %s/%s", h.SID, transport, status)
		}
		if hash != h.Token.Hash() || hash == h.Token.Reveal() {
			t.Fatalf("%s token persistence violated: hash=%q", h.SID, hash)
		}
		firstHashes[h.SID] = hash
	}

	// Make vm1's still-unused token expired. Reissue must invalidate it and
	// create a fresh TTL instead of colliding with the used_at partial index.
	if _, err := issuedIntegrationPool.Exec(ctx, `
UPDATE bootstrap_tokens
SET created_at=NOW()-INTERVAL '2 hours', expires_at=NOW()-INTERVAL '1 hour'
WHERE sid='vm1.example.com' AND used_at IS NULL`); err != nil {
		t.Fatalf("expire fixture token: %v", err)
	}

	second, err := issuer.IssueBatch(ctx, sids, runIncarnation)
	if err != nil {
		t.Fatalf("second IssueBatch: %v", err)
	}
	for _, h := range second {
		if h.Created || !h.Reissued {
			t.Errorf("second host %q created/reissued = %t/%t, want false/true", h.SID, h.Created, h.Reissued)
		}
		if h.Token.Hash() == firstHashes[h.SID] || !h.ExpiresAt.After(time.Now()) {
			t.Errorf("%s did not receive a fresh non-expired token", h.SID)
		}
		var active, marked int
		if err := issuedIntegrationPool.QueryRow(ctx, `
SELECT count(*) FILTER (WHERE used_at IS NULL),
       count(*) FILTER (WHERE used_by_kid=$2)
FROM bootstrap_tokens WHERE sid=$1`, h.SID, bootstraptoken.SystemKIDBootstrapIssuedReissue).
			Scan(&active, &marked); err != nil {
			t.Fatalf("token counters %s: %v", h.SID, err)
		}
		if active != 1 || marked != 1 {
			t.Errorf("%s active/marked = %d/%d, want 1/1", h.SID, active, marked)
		}
	}
}

// A connected host that belongs to ANOTHER incarnation is still an identity
// takeover, and refusing it still rolls the whole batch back. The pass-through
// NIM-780 added is scoped by exactly the ownership predicate the cloud path used
// (NIM-189), so this is the half of the old fail-closed rule that survived it.
func TestIntegration_IssuedBatch_ConnectedRefusalRollsBackWholeBatch(t *testing.T) {
	resetIssuedIntegration(t)
	ctx := context.Background()
	issuer := coremodbootstrap.NewIssuerPG(issuedIntegrationPool, time.Hour)
	sids := []string{"good.example.com", "onboarded.example.com"}
	first, err := issuer.IssueBatch(ctx, sids, runIncarnation)
	if err != nil {
		t.Fatalf("seed IssueBatch: %v", err)
	}
	before := map[string]string{}
	for _, h := range first {
		before[h.SID] = h.Token.Hash()
	}
	if _, err := issuedIntegrationPool.Exec(ctx,
		`UPDATE souls SET status='connected' WHERE sid='onboarded.example.com'`); err != nil {
		t.Fatalf("mark connected: %v", err)
	}
	seedIssuedIncarnation(t, "someone-else")
	seedIssuedMembership(t, "someone-else", "onboarded.example.com")

	_, err = issuer.IssueBatch(ctx, sids, runIncarnation)
	if err == nil || !strings.Contains(err.Error(), "onboarded.example.com") || !strings.Contains(err.Error(), "identity takeover") {
		t.Fatalf("connected IssueBatch error = %v", err)
	}
	for _, sid := range sids {
		var hash string
		var active int
		if err := issuedIntegrationPool.QueryRow(ctx, `
SELECT min(token_hash), count(*) FROM bootstrap_tokens
WHERE sid=$1 AND used_at IS NULL`, sid).Scan(&hash, &active); err != nil {
			t.Fatalf("active token %s: %v", sid, err)
		}
		if active != 1 || hash != before[sid] {
			t.Errorf("batch rollback failed for %s: active=%d hash=%q want original", sid, active, hash)
		}
	}
}

func TestIntegration_IssuedBatch_StatusAndTransportFailClosed(t *testing.T) {
	cases := []struct {
		name      string
		transport keepersoul.Transport
		status    keepersoul.Status
		// foreignMember binds the row to another incarnation. Needed only where
		// the status is one NIM-780 converges over for a host of this run — there
		// ownership is the whole difference between a pass-through and a refusal.
		foreignMember bool
	}{
		{name: "disconnected is onboarded", transport: keepersoul.TransportAgent, status: keepersoul.StatusDisconnected, foreignMember: true},
		{name: "revoked", transport: keepersoul.TransportAgent, status: keepersoul.StatusRevoked},
		{name: "destroyed", transport: keepersoul.TransportAgent, status: keepersoul.StatusDestroyed},
		{name: "ssh transport", transport: keepersoul.TransportSSH, status: keepersoul.StatusPending},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetIssuedIntegration(t)
			ctx := context.Background()
			s := &keepersoul.Soul{SID: "guard.example.com", Transport: tc.transport, Status: tc.status}
			if err := keepersoul.Insert(ctx, issuedIntegrationPool, s); err != nil {
				t.Fatalf("insert soul: %v", err)
			}
			if tc.foreignMember {
				seedIssuedIncarnation(t, "someone-else")
				seedIssuedMembership(t, "someone-else", s.SID)
			}
			_, err := coremodbootstrap.NewIssuerPG(issuedIntegrationPool, time.Hour).
				IssueBatch(ctx, []string{s.SID}, runIncarnation)
			if err == nil {
				t.Fatal("IssueBatch succeeded across fail-closed boundary")
			}
			var count int
			if qerr := issuedIntegrationPool.QueryRow(ctx,
				`SELECT count(*) FROM bootstrap_tokens WHERE sid=$1`, s.SID).Scan(&count); qerr != nil {
				t.Fatalf("count tokens: %v", qerr)
			}
			if count != 0 {
				t.Fatalf("refused SID got %d token rows", count)
			}
		})
	}
}

func TestIntegration_IssuedBatch_ExpiredSoulRearmed(t *testing.T) {
	resetIssuedIntegration(t)
	ctx := context.Background()
	s := &keepersoul.Soul{SID: "expired.example.com", Transport: keepersoul.TransportAgent, Status: keepersoul.StatusExpired}
	if err := keepersoul.Insert(ctx, issuedIntegrationPool, s); err != nil {
		t.Fatalf("insert expired Soul: %v", err)
	}
	hosts, err := coremodbootstrap.NewIssuerPG(issuedIntegrationPool, time.Hour).
		IssueBatch(ctx, []string{s.SID}, runIncarnation)
	if err != nil {
		t.Fatalf("IssueBatch expired Soul: %v", err)
	}
	if len(hosts) != 1 || hosts[0].Created || hosts[0].Reissued {
		t.Fatalf("expired Soul issuance count/created/reissued = %d/%t/%t, want 1/false/false",
			len(hosts), len(hosts) > 0 && hosts[0].Created, len(hosts) > 0 && hosts[0].Reissued)
	}
	got, err := keepersoul.SelectBySID(ctx, issuedIntegrationPool, s.SID)
	if err != nil {
		t.Fatalf("SelectBySID: %v", err)
	}
	if got.Status != keepersoul.StatusPending {
		t.Fatalf("status = %q, want pending", got.Status)
	}
}

func TestIntegration_IssuedBatch_ErrorTypeKeepsSID(t *testing.T) {
	resetIssuedIntegration(t)
	ctx := context.Background()
	if err := keepersoul.Insert(ctx, issuedIntegrationPool, &keepersoul.Soul{
		SID: "connected.example.com", Transport: keepersoul.TransportAgent, Status: keepersoul.StatusConnected,
	}); err != nil {
		t.Fatal(err)
	}
	seedIssuedIncarnation(t, "someone-else")
	seedIssuedMembership(t, "someone-else", "connected.example.com")
	_, err := coremodbootstrap.NewIssuerPG(issuedIntegrationPool, time.Hour).
		IssueBatch(ctx, []string{"connected.example.com"}, runIncarnation)
	var sidErr *coremodbootstrap.SIDIssueError
	if !errors.As(err, &sidErr) || sidErr.SID != "connected.example.com" {
		t.Fatalf("error = %v, want SIDIssueError", err)
	}
}

// ★ NIM-780. The run this reproduces: `create` built the VMs, minted, delivered
// and onboarded them, then failed LATER (rollout, cluster assembly), and the
// operator repeats `create`. The cloud plugin idempotently hands back the same
// machines and therefore the same SIDs, whose Souls are now `connected`. Before
// this, issuance refused them as an identity takeover and rolled the batch back,
// so the run could never be finished without deleting Soul rows by hand.
//
// The two sub-cases are the two points a run can die past onboarding, and the
// pass-through has to cover both: after `core.soul.registered` bound the host to
// the incarnation, and before it — where the host is up but still unbound, which
// `ownedByRunSQL` counts as this run's own for exactly that reason.
func TestIntegration_IssuedBatch_OnboardedHostOfThisRunIsConverged(t *testing.T) {
	cases := []struct {
		name  string
		bind  bool
		state keepersoul.Status
		// caller is the incarnation the issuance names. "" is a call with no run
		// context, which may still claim an UNBOUND row — the other half of
		// `ownedByRunSQL`'s rule from the refusal pinned below.
		caller string
	}{
		{name: "connected and bound to this incarnation", bind: true, state: keepersoul.StatusConnected, caller: runIncarnation},
		{name: "connected but not bound yet", bind: false, state: keepersoul.StatusConnected, caller: runIncarnation},
		{name: "disconnected and bound to this incarnation", bind: true, state: keepersoul.StatusDisconnected, caller: runIncarnation},
		{name: "unbound row, no run context — the unbound branch is what answers", bind: false, state: keepersoul.StatusConnected, caller: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetIssuedIntegration(t)
			ctx := context.Background()
			seedIssuedIncarnation(t, runIncarnation)
			issuer := coremodbootstrap.NewIssuerPG(issuedIntegrationPool, time.Hour)

			const live, fresh = "live.example.com", "fresh.example.com"
			if err := keepersoul.Insert(ctx, issuedIntegrationPool, &keepersoul.Soul{
				SID: live, Transport: keepersoul.TransportAgent, Status: tc.state,
			}); err != nil {
				t.Fatalf("insert live Soul: %v", err)
			}
			if tc.bind {
				seedIssuedMembership(t, runIncarnation, live)
			}
			var requestedBefore time.Time
			if err := issuedIntegrationPool.QueryRow(ctx,
				`SELECT requested_at FROM souls WHERE sid=$1`, live).Scan(&requestedBefore); err != nil {
				t.Fatalf("read requested_at: %v", err)
			}

			// The mixed batch is the point: the run must go on for the host that
			// still needs a token, not merely stop failing on the one that does not.
			hosts, err := issuer.IssueBatch(ctx, []string{live, fresh}, tc.caller)
			if err != nil {
				t.Fatalf("repeat create over an onboarded host failed: %v — a re-run past onboarding must converge, not refuse", err)
			}
			if len(hosts) != 2 || hosts[0].SID != live || hosts[1].SID != fresh {
				t.Fatalf("hosts = %+v, want both requested SIDs in order", hosts)
			}
			if !hosts[0].Onboarded {
				t.Errorf("live host Onboarded = false, want true — delivery has no other way to know it needs no token")
			}
			if hosts[0].Token.Reveal() != "" || !hosts[0].ExpiresAt.IsZero() || hosts[0].Created || hosts[0].Reissued {
				t.Errorf("live host carries issuance data (token=%t expires=%t created=%t reissued=%t), want none",
					hosts[0].Token.Reveal() != "", !hosts[0].ExpiresAt.IsZero(), hosts[0].Created, hosts[0].Reissued)
			}
			if hosts[1].Onboarded || hosts[1].Token.Reveal() == "" || !hosts[1].Created {
				t.Errorf("fresh host = onboarded:%t token:%t created:%t, want a normal first issuance",
					hosts[1].Onboarded, hosts[1].Token.Reveal() != "", hosts[1].Created)
			}

			// Nothing was written for the live host. A token row would be a
			// capability it cannot redeem, and a `pending` refresh would wipe its
			// presence and hand it to the Reaper's pending sweep while its stream
			// is still up.
			var tokens int
			if err := issuedIntegrationPool.QueryRow(ctx,
				`SELECT count(*) FROM bootstrap_tokens WHERE sid=$1`, live).Scan(&tokens); err != nil {
				t.Fatalf("count tokens: %v", err)
			}
			if tokens != 0 {
				t.Errorf("live host got %d token rows, want 0", tokens)
			}
			var status string
			var requestedAfter time.Time
			if err := issuedIntegrationPool.QueryRow(ctx,
				`SELECT status, requested_at FROM souls WHERE sid=$1`, live).Scan(&status, &requestedAfter); err != nil {
				t.Fatalf("re-read live Soul: %v", err)
			}
			if status != string(tc.state) || !requestedAfter.Equal(requestedBefore) {
				t.Errorf("live Soul was mutated: status %q→%q, requested_at moved: %t",
					tc.state, status, !requestedAfter.Equal(requestedBefore))
			}
		})
	}
}

// An unknown incarnation ("" — a direct call, or a keeper task outside a run)
// means "unknown", never "no owner": a BOUND onboarded host stays a refusal,
// because nothing here can claim it. This is `ownedByRunSQL`'s own rule, and the
// case where getting it backwards would let any caller with no run context
// converge over somebody's live fleet.
func TestIntegration_IssuedBatch_UnknownIncarnationCannotClaimABoundHost(t *testing.T) {
	resetIssuedIntegration(t)
	ctx := context.Background()
	seedIssuedIncarnation(t, runIncarnation)
	if err := keepersoul.Insert(ctx, issuedIntegrationPool, &keepersoul.Soul{
		SID: "bound.example.com", Transport: keepersoul.TransportAgent, Status: keepersoul.StatusConnected,
	}); err != nil {
		t.Fatalf("insert bound Soul: %v", err)
	}
	seedIssuedMembership(t, runIncarnation, "bound.example.com")

	_, err := coremodbootstrap.NewIssuerPG(issuedIntegrationPool, time.Hour).
		IssueBatch(ctx, []string{"bound.example.com"}, "")
	if err == nil || !strings.Contains(err.Error(), "identity takeover") {
		t.Fatalf("issuance without an incarnation = %v, want a takeover refusal", err)
	}
}
