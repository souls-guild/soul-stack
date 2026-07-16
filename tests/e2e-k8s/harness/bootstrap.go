//go:build e2e_k8s

package harness

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// bootstrap.go — L3c-3 harness helper that issues a bootstrap token via
// direct SQL to Postgres in the kind cluster. Symmetric to L3b
// harness/bootstrap.go, but PG access goes through `kubectl port-forward`
// (the PG service is ClusterIP-only, host-side access only via forward).
//
// Why direct SQL and not POST /v1/souls/{sid}/issue-token: this specifically
// exercises the gRPC Bootstrap flow (CSR -> Keeper.Bootstrap -> leaf cert +
// `soul.bootstrapped` audit event); the RBAC/admin API is covered by
// L2/L3b tests. The keeper handler itself reads `bootstrap_tokens` and
// upgrades `souls.status pending -> connected` -- that is the behavior
// under test.
//
// `created_by_aid = NULL` (FK to operators(aid) ON DELETE SET NULL) is a
// valid state: keeper-bootstrap-mode starts with an empty operators
// registry, so we don't set up a test Archon.

// IssueBootstrapToken inserts a `souls` row (status='pending',
// transport='agent') plus an active `bootstrap_tokens` row via port-forward
// to the postgres service. Returns the plain token; only the SHA-256 hex is
// stored in the DB.
//
// Plain-token format: base64-url-no-padding of 32 random bytes (symmetric
// with keeper-side bootstraptoken.Generate). Hash: SHA-256 lower-hex
// (symmetric with bootstraptoken.HashToken and L3b sha256HexLower).
func IssueBootstrapToken(t *testing.T, stack *Stack, sid string) string {
	t.Helper()

	plain := generatePlainToken(t)
	tokenHash := sha256HexLower(plain)

	pool := openPGPool(t, stack)
	defer pool.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// souls (pending). bootstrap_tokens.sid -> souls.sid (FK), the souls row
	// must exist before the INSERT into bootstrap_tokens.
	if _, err := pool.Exec(ctx, `
		INSERT INTO souls (sid, transport, status, requested_at, registered_at)
		VALUES ($1, 'agent', 'pending', NOW(), NOW())
		ON CONFLICT (sid) DO NOTHING
	`, sid); err != nil {
		t.Fatalf("IssueBootstrapToken: INSERT souls(%s): %v", sid, err)
	}

	// bootstrap_tokens (active). Schema: token_id UUID DEFAULT, sid FK,
	// token_hash SHA-256 hex (CHECK ^[0-9a-f]{64}$), created_at DEFAULT NOW,
	// expires_at (>created_at, CHECK), used_at NULL, created_by_aid NULL.
	if _, err := pool.Exec(ctx, `
		INSERT INTO bootstrap_tokens (sid, token_hash, expires_at)
		VALUES ($1, $2, NOW() + INTERVAL '1 hour')
	`, sid, tokenHash); err != nil {
		t.Fatalf("IssueBootstrapToken: INSERT bootstrap_tokens(%s): %v", sid, err)
	}

	return plain
}

// WaitForSoulConnected — method wrapper for callers that already hold a
// *Stack (failover test etc). Delegates to the package-level function.
func (s *Stack) WaitForSoulConnected(t *testing.T, sid string, timeout time.Duration) {
	t.Helper()
	WaitForSoulConnected(t, s, sid, timeout)
}

// WaitForSoulConnected polls `souls.status` for sid via port-forward to PG,
// returns on the first 'connected'. Terminal statuses (revoked/expired/
// destroyed) fail immediately without waiting for the timeout.
func WaitForSoulConnected(t *testing.T, stack *Stack, sid string, timeout time.Duration) {
	t.Helper()

	pool := openPGPool(t, stack)
	defer pool.Close()

	deadline := time.Now().Add(timeout)
	var lastStatus string
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := pool.QueryRow(ctx,
			"SELECT status FROM souls WHERE sid = $1", sid).Scan(&lastStatus)
		cancel()
		if err != nil {
			t.Logf("WaitForSoulConnected: query soul(%s): %v", sid, err)
			time.Sleep(1 * time.Second)
			continue
		}
		switch lastStatus {
		case "connected":
			return
		case "revoked", "expired", "destroyed":
			t.Fatalf("WaitForSoulConnected: soul %s reached terminal status %q", sid, lastStatus)
		}
		time.Sleep(1 * time.Second)
	}
	t.Fatalf("WaitForSoulConnected: soul %s did not reach status=connected within %v (last=%q)",
		sid, timeout, lastStatus)
}

// AssertAuditEvent looks in audit_log for at least one row with
// event_type=eventType whose payload contains subset. On failure, prints the
// last-N payloads of the same event_type for diagnostics. Symmetric to L3b
// Stack.AssertAuditEvent.
func (s *Stack) AssertAuditEvent(t *testing.T, eventType string, expectedSubset map[string]any) {
	t.Helper()

	pool := openPGPool(t, s)
	defer pool.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	subsetJSON, err := json.Marshal(expectedSubset)
	if err != nil {
		t.Fatalf("AssertAuditEvent: marshal expected subset: %v", err)
	}

	var count int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM audit_log
		WHERE event_type = $1 AND payload @> $2::jsonb
	`, eventType, string(subsetJSON)).Scan(&count); err != nil {
		t.Fatalf("AssertAuditEvent %s: query: %v", eventType, err)
	}
	if count > 0 {
		return
	}

	rows, derr := pool.Query(ctx,
		"SELECT payload FROM audit_log WHERE event_type = $1 ORDER BY created_at DESC LIMIT 10",
		eventType)
	var dumps []string
	if derr == nil {
		defer rows.Close()
		for rows.Next() {
			var p []byte
			if err := rows.Scan(&p); err == nil {
				dumps = append(dumps, string(p))
			}
		}
	}
	t.Fatalf("AssertAuditEvent %s: payload subset not found\nexpected=%s\nrecent_events=%v",
		eventType, string(subsetJSON), dumps)
}

// openPGPool opens a pgxpool via port-forward to the postgres service. The
// caller must Close() the pool after use; the port-forward is closed via
// t.Cleanup (inside PortForward).
//
// Per-call open: kubectl port-forward is a cheap subprocess (~100ms), while
// sharing a pool across all harness calls would require tying its lifecycle
// to Stack, complicating teardown. L3c-3 does 3-4 SQL operations per test --
// the ms-scale difference is negligible.
func openPGPool(t *testing.T, stack *Stack) *pgxpool.Pool {
	t.Helper()
	pf := stack.Cluster.PortForward(t, "svc/postgres-postgresql", 5432, 60*time.Second)

	dsn := fmt.Sprintf(
		"postgresql://postgres:testpass@127.0.0.1:%d/keeper_test?sslmode=disable",
		pf.LocalPort,
	)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("openPGPool: pgxpool.New(%s): %v", dsn, err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Fatalf("openPGPool: ping: %v", err)
	}
	return pool
}

// generatePlainToken returns 32 crypto-random bytes in base64url-no-padding.
// Format identical to keeper-side bootstraptoken.Generate.
func generatePlainToken(t *testing.T) string {
	t.Helper()
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		t.Fatalf("generatePlainToken: crypto/rand: %v", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf)
}

// sha256HexLower — SHA-256(plain) in lower-hex (64 chars). Symmetric with
// keeper-side bootstraptoken.HashToken.
func sha256HexLower(plain string) string {
	sum := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(sum[:])
}
