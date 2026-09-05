//go:build e2e

package harness

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/shared/api/wire"
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

// AddMember binds the i-th pre-auth Soul to incarnation `incName` through the
// OPERATOR path — `POST /v1/incarnations/{name}/members` (ADR-008 amendment
// 2026-07-28/NIM-209). Needed for scenario-apply: the run's roster resolves
// members via incarnation_membership (topology/resolver.go::rosterSQL), so
// without this an incarnation "has no connected hosts" → no_hosts →
// error_locked.
//
// It used to INSERT into incarnation_membership directly, because until NIM-209
// there was no route to call — the harness was working around a genuine product
// gap. Now that the route exists, the direct INSERT would be worse than
// redundant (NIM-231): it bypasses BOTH authorization gates (the incarnation
// selector and the caller's soul purview) and the `connected` status rule, so a
// suite would go green along a path no operator can take. Tests are supposed to
// fail where an operator would.
//
// A consequence worth stating: this call now REQUIRES the soul to be
// `connected`, exactly as the operator's does. Connect the stub before binding.
//
// ORDER: the incarnation must already exist. That is still true, but it is no
// longer diagnosed by an FK violation — the route resolves `{name}` before
// writing anything and answers 404, which this turns into the instruction. For
// bootstrapping a NEW incarnation use [Stack.CreateIncarnationOnRoster], which
// owns the whole order in one place.
//
// Idempotent server-side (ON CONFLICT DO NOTHING; the reply splits `bound` from
// `already_member`). Fatal on any non-200 — see [Stack.AddMemberRaw] when the
// status itself is the subject of the test.
func (s *Stack) AddMember(t *testing.T, soulIndex int, incName string) {
	t.Helper()
	body, status := s.AddMemberRaw(t, soulIndex, incName)
	if status == http.StatusNotFound {
		t.Fatalf("AddMember(%s, soul %d): 404 — the incarnation does not exist yet; membership is bound AFTER "+
			"the incarnation is created. Bootstrap a new incarnation with Stack.CreateIncarnationOnRoster. body=%s",
			incName, soulIndex, string(body))
	}
	if status != http.StatusOK {
		t.Fatalf("AddMember(%s, soul %d): status %d, body=%s", incName, soulIndex, status, string(body))
	}
}

// AddMemberRaw — low-level `POST /v1/incarnations/{name}/members`: returns
// (responseBody, statusCode) without checking it. For tests where the code IS
// the subject — binding a host that is not `connected` must be refused the same
// way it is refused an operator (422), not silently accepted as the old direct
// INSERT did (NIM-231).
func (s *Stack) AddMemberRaw(t *testing.T, soulIndex int, incName string) ([]byte, int) {
	t.Helper()
	if soulIndex < 0 || soulIndex >= len(s.souls) {
		t.Fatalf("AddMemberRaw(%d): out of range (created %d souls)", soulIndex, len(s.souls))
	}
	sid := s.souls[soulIndex].SID
	c := s.opClient(t)
	path := fmt.Sprintf("/v1/incarnations/%s/members", incName)
	resp, status, err := c.post(context.Background(), path, wire.IncarnationMemberBindRequest{SIDs: []string{sid}})
	if err != nil {
		t.Fatalf("AddMemberRaw(%s, %s): http: %v", incName, sid, err)
	}
	return resp, status
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
