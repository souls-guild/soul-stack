//go:build e2e_live

package harness

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"testing"
	"time"
)

// IssueBootstrapToken — an L3b harness helper. Creates a row in `souls`
// (status='pending', transport='agent') + `bootstrap_tokens` (active) via
// direct SQL, bypassing the Operator API.
//
// Why direct SQL and not POST /v1/souls/{sid}/issue-token: the L3b cycle
// specifically tests the gRPC Bootstrap flow (CSR → Keeper.Bootstrap →
// leaf-cert + audit `soul.bootstrapped`); RBAC/admin API is a separate area
// covered by L2 Operator API tests. The keeper handler itself reads
// `bootstrap_tokens` and upgrades `souls.status pending → connected` — that
// is the behavior under test.
//
// The plain token is returned to the caller (passed into the soul
// container's env). Only the SHA-256 hex of the plain value is stored in the
// DB (`token_hash`), symmetric with keeper/internal/bootstraptoken.HashToken.
//
// Side effect: `created_by_aid = NULL` (FK to operators SET NULL — the row
// is left without an initiator since no operator is formally known to the
// harness). `requested_at = NOW()`, `expires_at = NOW() + 1h` (within the
// MVP standard DefaultTokenTTL=24h; 1h for the test with plenty of margin).
func IssueBootstrapToken(t *testing.T, stack *Stack, sid string) string {
	t.Helper()
	if stack == nil || stack.db == nil {
		t.Fatal("IssueBootstrapToken: stack.db is nil (NewStack did not run?)")
	}

	plain := generatePlainToken(t)
	tokenHash := sha256HexLower(plain)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// 1. souls (pending). bootstrap_tokens.sid → souls.sid (FK), so the souls
	//    row must exist before the INSERT into bootstrap_tokens.
	//    transport='agent' — pull mode (push would be 'ssh', where
	//    bootstrap_tokens aren't created).
	if _, err := stack.db.Exec(ctx, `
		INSERT INTO souls (sid, transport, status, requested_at, registered_at)
		VALUES ($1, 'agent', 'pending', NOW(), NOW())
		ON CONFLICT (sid) DO NOTHING
	`, sid); err != nil {
		t.Fatalf("IssueBootstrapToken: INSERT souls(%s): %v", sid, err)
	}

	// 2. bootstrap_tokens (active). Schema: token_id (UUID DEFAULT), sid (FK),
	//    token_hash (SHA-256 hex, CHECK ^[0-9a-f]{64}$), created_at DEFAULT NOW,
	//    expires_at (>created_at, CHECK), used_at NULL, created_by_aid NULL.
	if _, err := stack.db.Exec(ctx, `
		INSERT INTO bootstrap_tokens (sid, token_hash, expires_at)
		VALUES ($1, $2, NOW() + INTERVAL '1 hour')
	`, sid, tokenHash); err != nil {
		t.Fatalf("IssueBootstrapToken: INSERT bootstrap_tokens(%s): %v", sid, err)
	}

	return plain
}

// generatePlainToken returns 32 bytes of crypto-random data in
// base64url-no-padding. The format matches bootstraptoken.Generate
// (keeper-side canon); the hash uses the same SHA-256 as HashToken.
func generatePlainToken(t *testing.T) string {
	t.Helper()
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		t.Fatalf("generatePlainToken: crypto/rand: %v", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf)
}

// sha256HexLower — SHA-256(plain) as lower-hex (64 chars). Symmetric with
// bootstraptoken.HashToken.
func sha256HexLower(plain string) string {
	sum := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(sum[:])
}
