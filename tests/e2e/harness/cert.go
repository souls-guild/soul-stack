//go:build e2e

package harness

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"testing"
	"time"
)

// RegisterSoulPreAuth — pre-auth registration of a soul-stub in the DB.
//
// Doesn't go through `bootstrap.Bootstrap` (that's L3b territory): the
// harness inserts rows directly into `souls`/`soul_seeds` via pgx, bypassing
// the `soul.bootstrapped` audit event (written by keeper on a real mTLS
// handshake through the gRPC Bootstrap RPC, see ADR-039 Amendment §6).
//
// Returns the client cert+key that the soul-stub uses for the mTLS handshake
// to the EventStream listener. Drift with keeper/migrations must be
// reconciled manually: the column set below is pinned to the current schema;
// on a schema change the harness fails on INSERT and the migration owner
// updates the CRUD here.
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

	// souls FIRST: soul_seeds.sid is an FK to souls(sid) (soul_seeds_sid_fk), so
	// the parent row must exist before the seed. Columns follow migration
	// 007_create_souls (registered_at, not created_at; transport enum =
	// {agent,ssh}, a gRPC Soul is 'agent'). The real presence marker (Redis
	// SID lease) is acquired when the soul-stub opens the EventStream stream
	// (ConnectSoulStub).
	if _, err := tx.Exec(ctx, `
		INSERT INTO souls (sid, status, transport, registered_at, last_seen_at)
		VALUES ($1, 'connected', 'agent', NOW(), NOW())
		ON CONFLICT (sid) DO UPDATE SET status = 'connected', last_seen_at = NOW()
	`, sid); err != nil {
		t.Fatalf("RegisterSoulPreAuth(%s): insert souls: %v", sid, err)
	}

	// soul_seeds: certificate history; unique on (sid) WHERE status='active'.
	// On pre-auth we insert one active seed. Columns match migration
	// 009_create_soul_seeds (NOT NULL: serial_number / expires_at; no
	// created_at). serial_number must be globally unique
	// (soul_seeds_serial_number_idx) — we use the fingerprint hex as a
	// deterministic unique per-cert serial number.
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

// AddSoulToCoven adds a coven label to souls.coven of the i-th pre-auth Soul.
// Needed for scenario-apply: the run's roster is resolved by Coven membership
// (`WHERE <incarnation.name> = ANY(coven)`, ADR-008 — incarnation.name is the
// root Coven label, topology/resolver.go::rosterSQL). Without this, an
// incarnation "has no connected hosts" → no_hosts → error_locked.
//
// Idempotent (array_append only if the label is not already there). Fatal on
// error.
func (s *Stack) AddSoulToCoven(t *testing.T, soulIndex int, coven string) {
	t.Helper()
	if soulIndex < 0 || soulIndex >= len(s.souls) {
		t.Fatalf("AddSoulToCoven(%d): out of range (created %d souls)", soulIndex, len(s.souls))
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

// SeedSoulprint writes souls.soulprint_facts (JSONB) for the i-th pre-auth
// Soul. Needed by services whose render reads soulprint keeper-side:
// redis-exporter takes `arch: soulprint.self.os.arch`, node-exporter uses
// `soulprint.self.os.arch` in the tarball URL (ADR-018).
// RegisterSoulPreAuth does NOT populate the soulprint row (NULL), and without
// a seed the render phase fails on a nil access to `os.arch`. smoke-nginx
// doesn't read soulprint keeper-side and works without a seed — for redis it
// is required.
//
// facts — the `SoulprintFacts` JSON shape (resolver scanHost -> map[string]any,
// CEL `soulprint.self.<path>`): a top-level `os` key with subfields
// arch/family/distro/version/pkg_mgr/init_system. Symmetric with
// fixtures/souls.yaml.
func (s *Stack) SeedSoulprint(t *testing.T, soulIndex int, facts map[string]any) {
	t.Helper()
	if soulIndex < 0 || soulIndex >= len(s.souls) {
		t.Fatalf("SeedSoulprint(%d): out of range (created %d souls)", soulIndex, len(s.souls))
	}
	sid := s.souls[soulIndex].SID
	factsJSON, err := json.Marshal(facts)
	if err != nil {
		t.Fatalf("SeedSoulprint(%s): marshal facts: %v", sid, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := s.db.Exec(ctx, `
		UPDATE souls
		SET soulprint_facts = $2::jsonb,
		    soulprint_collected_at = NOW(),
		    soulprint_received_at = NOW()
		WHERE sid = $1
	`, sid, string(factsJSON)); err != nil {
		t.Fatalf("SeedSoulprint(%s): %v", sid, err)
	}
}

// fingerprintSHA256Hex computes the fingerprint EXACTLY like the keeper-side
// soulseed.FingerprintFromCert: SHA-256 over cert.RawSubjectPublicKeyInfo (NOT
// over the PEM bytes). The Keeper's mTLS auth (grpc/auth.go::peerFingerprint)
// looks up the seed by this value; a mismatch causes "unknown peer
// fingerprint" and the stream is rejected.
func fingerprintSHA256Hex(t *testing.T, certPEM []byte) string {
	t.Helper()
	block, _ := pem.Decode(certPEM)
	if block == nil {
		t.Fatalf("fingerprintSHA256Hex: cert is not a PEM block")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("fingerprintSHA256Hex: parse cert: %v", err)
	}
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	return hex.EncodeToString(sum[:])
}
