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

// IssueBootstrapToken — harness-помощник L3b. Заводит запись в `souls`
// (status='pending', transport='agent') + `bootstrap_tokens` (active) через
// прямой SQL, минуя Operator API.
//
// Почему direct SQL, а не POST /v1/souls/{sid}/issue-token: L3b-цикл проверяет
// именно gRPC Bootstrap-flow (CSR → Keeper.Bootstrap → leaf-cert + audit
// `soul.bootstrapped`); RBAC/admin-API — отдельная сфера, проверяемая L2-
// тестами Operator API. Сам keeper-handler читает `bootstrap_tokens` и
// апгрейдит `souls.status pending → connected` — это и есть тестируемое
// поведение.
//
// Plain-token возвращается caller-у (передаётся в env soul-контейнера).
// В БД хранится только SHA-256 hex от plain (`token_hash`), симметрично
// keeper/internal/bootstraptoken.HashToken.
//
// Side effect: `created_by_aid = NULL` (FK на operators SET NULL — оставляем
// запись без инициатора, т.к. оператор harness-у формально не известен).
// `requested_at = NOW()`, `expires_at = NOW() + 1h` (внутри MVP-стандарта
// DefaultTokenTTL=24h, для теста 1ч с большим запасом).
func IssueBootstrapToken(t *testing.T, stack *Stack, sid string) string {
	t.Helper()
	if stack == nil || stack.db == nil {
		t.Fatal("IssueBootstrapToken: stack.db nil (NewStack не отработал?)")
	}

	plain := generatePlainToken(t)
	tokenHash := sha256HexLower(plain)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// 1. souls (pending). bootstrap_tokens.sid → souls.sid (FK), поэтому
	//    souls-строка должна существовать до INSERT в bootstrap_tokens.
	//    transport='agent' — pull-режим (push был бы 'ssh', там
	//    bootstrap_tokens не создаются).
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

// generatePlainToken возвращает 32 байта crypto-random в base64url-no-padding.
// Формат идентичен bootstraptoken.Generate (keeper-side канон); хеш считается
// той же SHA-256, что и HashToken.
func generatePlainToken(t *testing.T) string {
	t.Helper()
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		t.Fatalf("generatePlainToken: crypto/rand: %v", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf)
}

// sha256HexLower — SHA-256(plain) в lower-hex (64 символа). Симметрично
// bootstraptoken.HashToken.
func sha256HexLower(plain string) string {
	sum := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(sum[:])
}
