//go:build e2e

package harness

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"testing"
	"time"
)

// RegisterSoulPreAuth — pre-auth-регистрация soul-stub-а в БД.
//
// Не идёт через `bootstrap.Bootstrap` (это L3b territory): harness прямо
// вставляет строки в `souls`/`soul_seeds` через pgx, минуя audit-event
// `soul.bootstrapped` (его пишет keeper при настоящем mTLS handshake-е через
// gRPC Bootstrap-RPC, см. ADR-039 Amendment §6).
//
// Возвращает client cert+key, которые soul-stub использует для mTLS handshake-а
// к EventStream-listener-у. Drift с keeper/migrations синхронизировать вручную:
// набор колонок ниже зафиксирован для текущей schema; при schema-change harness
// фейлится на INSERT, и владелец миграции обновляет CRUD здесь.
func RegisterSoulPreAuth(t *testing.T, stack *Stack, sid string) (cert, key []byte) {
	t.Helper()

	cert, key = IssueSoulCert(t, stack, sid)
	fpHex := fingerprintSHA256Hex(t, cert)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	tx, err := stack.db.Begin(ctx)
	if err != nil {
		t.Fatalf("RegisterSoulPreAuth: begin tx: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// souls ПЕРВЫМ: soul_seeds.sid — FK на souls(sid) (soul_seeds_sid_fk), поэтому
	// родительская строка обязана существовать до seed-а. Колонки по migration
	// 007_create_souls (registered_at, не created_at; transport enum = {agent,ssh},
	// gRPC-Soul = 'agent'). Реальная presence-метка (Redis SID-lease) захватывается
	// при открытии EventStream-стрима soul-stub-ом (ConnectSoulStub).
	if _, err := tx.Exec(ctx, `
		INSERT INTO souls (sid, status, transport, registered_at, last_seen_at)
		VALUES ($1, 'connected', 'agent', NOW(), NOW())
		ON CONFLICT (sid) DO UPDATE SET status = 'connected', last_seen_at = NOW()
	`, sid); err != nil {
		t.Fatalf("RegisterSoulPreAuth(%s): insert souls: %v", sid, err)
	}

	// soul_seeds: история сертификатов; уникальность по (sid) WHERE status='active'.
	// На pre-auth кладём один active-seed. Колонки соответствуют migration
	// 009_create_soul_seeds (NOT NULL: serial_number / expires_at; нет created_at).
	// serial_number должен быть глобально-уникален (soul_seeds_serial_number_idx) —
	// берём fingerprint-hex как детерминированный уникальный per-cert серийник.
	if _, err := tx.Exec(ctx, `
		INSERT INTO soul_seeds (sid, fingerprint, serial_number, status, issued_at, expires_at)
		VALUES ($1, $2, $3, 'active', NOW(), NOW() + INTERVAL '365 days')
		ON CONFLICT (fingerprint) DO NOTHING
	`, sid, fpHex, fpHex); err != nil {
		t.Fatalf("RegisterSoulPreAuth(%s): insert soul_seeds: %v", sid, err)
	}

	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("RegisterSoulPreAuth(%s): commit: %v", sid, err)
	}
	return cert, key
}

// AddSoulToCoven добавляет coven-метку в souls.coven i-го pre-auth Soul-а.
// Нужно для scenario-apply: roster прогона резолвится по Coven-членству
// (`WHERE <incarnation.name> = ANY(coven)`, ADR-008 — incarnation.name есть
// корневая Coven-метка, topology/resolver.go::rosterSQL). Без этого incarnation
// «не имеет connected-хостов» → no_hosts → error_locked.
//
// Идемпотентно (array_append только если метки ещё нет). Fatal при ошибке.
func (s *Stack) AddSoulToCoven(t *testing.T, soulIndex int, coven string) {
	t.Helper()
	if soulIndex < 0 || soulIndex >= len(s.souls) {
		t.Fatalf("AddSoulToCoven(%d): out of range (создано %d soul-ов)", soulIndex, len(s.souls))
	}
	sid := s.souls[soulIndex].SID
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := s.db.Exec(ctx, `
		UPDATE souls
		SET coven = array_append(coalesce(coven, '{}'), $2)
		WHERE sid = $1 AND NOT ($2 = ANY(coalesce(coven, '{}')))
	`, sid, coven); err != nil {
		t.Fatalf("AddSoulToCoven(%s, %s): %v", sid, coven, err)
	}
}

// fingerprintSHA256Hex вычисляет fingerprint ТОЧНО как keeper-side
// soulseed.FingerprintFromCert: SHA-256 над cert.RawSubjectPublicKeyInfo (НЕ над
// PEM-байтами). mTLS-auth Keeper-а (grpc/auth.go::peerFingerprint) ищет seed по
// этому значению; расхождение → "unknown peer fingerprint" → стрим отвергается.
func fingerprintSHA256Hex(t *testing.T, certPEM []byte) string {
	t.Helper()
	block, _ := pem.Decode(certPEM)
	if block == nil {
		t.Fatalf("fingerprintSHA256Hex: cert не является PEM-блоком")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("fingerprintSHA256Hex: parse cert: %v", err)
	}
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	return hex.EncodeToString(sum[:])
}
