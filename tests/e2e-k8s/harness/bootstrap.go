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

// bootstrap.go — harness-помощник L3c-3 для выдачи bootstrap-токена через
// прямой SQL к Postgres-у в kind-cluster. Симметрично L3b harness/bootstrap.go,
// но доступ к PG идёт через `kubectl port-forward` (PG-сервис ClusterIP-only,
// host-side доступ только через forward).
//
// Почему direct SQL, а не POST /v1/souls/{sid}/issue-token: тестируется именно
// gRPC Bootstrap-flow (CSR → Keeper.Bootstrap → leaf-cert + audit
// `soul.bootstrapped`); RBAC/admin-API проверяется L2/L3b-тестами. Сам keeper-
// handler читает `bootstrap_tokens` + апгрейдит `souls.status pending →
// connected` — это и есть тестируемое поведение.
//
// `created_by_aid = NULL` (FK на operators(aid) ON DELETE SET NULL) —
// валидное состояние: keeper-bootstrap-mode стартует с пустым operators-
// registry, тестового Архонта не заводим.

// IssueBootstrapToken заводит запись в `souls` (status='pending',
// transport='agent') + `bootstrap_tokens` (active) через port-forward к
// postgres-сервису. Возвращает plain-token; в БД хранится только SHA-256 hex.
//
// Формат plain-token — base64-url-no-padding от 32 случайных байт (симметрично
// keeper-side bootstraptoken.Generate). hash — SHA-256 lower-hex (симметрично
// bootstraptoken.HashToken и L3b sha256HexLower).
func IssueBootstrapToken(t *testing.T, stack *Stack, sid string) string {
	t.Helper()

	plain := generatePlainToken(t)
	tokenHash := sha256HexLower(plain)

	pool := openPGPool(t, stack)
	defer pool.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// souls (pending). bootstrap_tokens.sid → souls.sid (FK), souls-строка
	// должна существовать до INSERT в bootstrap_tokens.
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

// WaitForSoulConnected — метод-обёртка для удобства каллеров, у которых уже
// есть *Stack (failover-тест и т.п.). Делегирует package-level функцию.
func (s *Stack) WaitForSoulConnected(t *testing.T, sid string, timeout time.Duration) {
	t.Helper()
	WaitForSoulConnected(t, s, sid, timeout)
}

// WaitForSoulConnected поллит `souls.status` для sid через port-forward к PG,
// возвращает nil при первом 'connected'. Терминальные статусы
// (revoked/expired/destroyed) — немедленный fail без ожидания timeout.
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

// AssertAuditEvent ищет в audit_log хотя бы одну строку с event_type=eventType
// и payload, содержащим subset. На fail печатает last-N payload-ов того же
// event_type для диагностики. Симметрично L3b Stack.AssertAuditEvent.
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
	t.Fatalf("AssertAuditEvent %s: payload subset не найден\nexpected=%s\nrecent_events=%v",
		eventType, string(subsetJSON), dumps)
}

// openPGPool открывает pgxpool через port-forward к postgres-сервису. Caller
// обязан Close() pool после использования; port-forward закрывается через
// t.Cleanup (внутри PortForward).
//
// Per-call open: kubectl port-forward — дешёвый subprocess (~100ms), а
// шерить pool через все harness-вызовы потребовало бы lifecycle-привязки к
// Stack, что усложнит teardown. L3c-3 делает 3-4 SQL-операции на тест —
// разница в ms незаметна.
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

// generatePlainToken возвращает 32 байта crypto-random в base64url-no-padding.
// Формат идентичен keeper-side bootstraptoken.Generate.
func generatePlainToken(t *testing.T) string {
	t.Helper()
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		t.Fatalf("generatePlainToken: crypto/rand: %v", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf)
}

// sha256HexLower — SHA-256(plain) в lower-hex (64 символа). Симметрично
// keeper-side bootstraptoken.HashToken.
func sha256HexLower(plain string) string {
	sum := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(sum[:])
}
