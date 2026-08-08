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
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	ctr, err := tcpostgres.Run(ctx,
		"postgres:16-alpine",
		tcpostgres.WithDatabase("keeper_test"),
		tcpostgres.WithUsername("keeper"),
		tcpostgres.WithPassword("keeper"),
		tcpostgres.BasicWaitStrategies(),
	)
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

func resetIssuedIntegration(t *testing.T) {
	t.Helper()
	if _, err := issuedIntegrationPool.Exec(context.Background(),
		`TRUNCATE TABLE soul_seeds, bootstrap_tokens, incarnation_membership, souls, audit_log CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
}

func TestIntegration_IssuedBatch_CreateReissueAndExpiry(t *testing.T) {
	resetIssuedIntegration(t)
	ctx := context.Background()
	issuer := coremodbootstrap.NewIssuerPG(issuedIntegrationPool, time.Hour)
	sids := []string{"vm1.example.com", "vm2.example.com"}

	first, err := issuer.IssueBatch(ctx, sids)
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

	second, err := issuer.IssueBatch(ctx, sids)
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

func TestIntegration_IssuedBatch_ConnectedRefusalRollsBackWholeBatch(t *testing.T) {
	resetIssuedIntegration(t)
	ctx := context.Background()
	issuer := coremodbootstrap.NewIssuerPG(issuedIntegrationPool, time.Hour)
	sids := []string{"good.example.com", "onboarded.example.com"}
	first, err := issuer.IssueBatch(ctx, sids)
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

	_, err = issuer.IssueBatch(ctx, sids)
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
	}{
		{name: "disconnected is onboarded", transport: keepersoul.TransportAgent, status: keepersoul.StatusDisconnected},
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
			_, err := coremodbootstrap.NewIssuerPG(issuedIntegrationPool, time.Hour).
				IssueBatch(ctx, []string{s.SID})
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
		IssueBatch(ctx, []string{s.SID})
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
	_, err := coremodbootstrap.NewIssuerPG(issuedIntegrationPool, time.Hour).
		IssueBatch(ctx, []string{"connected.example.com"})
	var sidErr *coremodbootstrap.SIDIssueError
	if !errors.As(err, &sidErr) || sidErr.SID != "connected.example.com" {
		t.Fatalf("error = %v, want SIDIssueError", err)
	}
}
