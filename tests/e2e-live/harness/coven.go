//go:build e2e_live

package harness

import (
	"context"
	"testing"
	"time"
)

// AddMember binds the i-th soul container to incarnation `incName` in
// incarnation_membership.
//
// Why: membership is a first-class M:N relation (ADR-008 amendment
// 2026-07-17/NIM-124), no longer the derived fact
// `incarnation.name ∈ souls.coven[]`. The scenario run's roster resolves
// members via incarnation_membership
// (keeper/internal/topology/resolver.go::rosterSQL). Without this the
// incarnation "has no connected hosts" -> run.go aborts with `no_hosts`
// BEFORE the dispatch phase -> zero apply_runs rows (run.go §3) ->
// WaitApplySuccess spins until timeout. Symmetric with the L3a harness
// (tests/e2e/harness/cert.go::AddMember).
//
// IssueBootstrapToken creates a `souls` row and the Bootstrap flow only
// upgrades status — no membership is bound. This step closes the gap of
// "connected, but not in the incarnation's roster". The incarnation row must
// already exist (FK incarnation_name → incarnation).
//
// Idempotent (ON CONFLICT DO NOTHING; PK (incarnation_name, sid)). Fatal on
// error.
func (s *Stack) AddMember(t *testing.T, soulIndex int, incName string) {
	t.Helper()
	if soulIndex < 0 || soulIndex >= len(s.SoulContainers) {
		t.Fatalf("AddMember(%d): out of range (%d soul containers created)", soulIndex, len(s.SoulContainers))
	}
	sid := s.SoulContainers[soulIndex].SID
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := s.db.Exec(ctx, `
		INSERT INTO incarnation_membership (incarnation_name, sid)
		VALUES ($1, $2)
		ON CONFLICT DO NOTHING
	`, incName, sid); err != nil {
		t.Fatalf("AddMember(%s, %s): %v", incName, sid, err)
	}
}

// WaitSoulprintReported blocks until souls.soulprint_facts becomes non-empty
// for the i-th soul container.
//
// Why: services with keeper-side soulprint resolution (redis-create reads
// soulprint.self.os.arch when rendering the redis-exporter/node-exporter
// release-tarball URLs, ADR-018) require host facts to already be in the DB
// by render time of the create run. A real soul sends its first
// SoulprintReport IMMEDIATELY on session establishment
// (soul/cmd/soul/main.go::handleSession), but that's a separate message
// AFTER status='connected' — there's a window between connected and
// SoulprintReport being processed. Without waiting, the first
// CreateIncarnation could hit render with empty soulprint_facts -> "no such
// key: arch". We wait for non-empty facts BEFORE Create.
func (s *Stack) WaitSoulprintReported(t *testing.T, soulIndex int, timeoutSec int) {
	t.Helper()
	if soulIndex < 0 || soulIndex >= len(s.SoulContainers) {
		t.Fatalf("WaitSoulprintReported(%d): out of range (%d soul containers created)", soulIndex, len(s.SoulContainers))
	}
	sid := s.SoulContainers[soulIndex].SID
	deadline := time.Now().Add(time.Duration(timeoutSec) * time.Second)
	for time.Now().Before(deadline) {
		var facts []byte
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := s.db.QueryRow(ctx,
			"SELECT soulprint_facts FROM souls WHERE sid = $1", sid).Scan(&facts)
		cancel()
		if err != nil {
			t.Fatalf("WaitSoulprintReported(%s): query: %v", sid, err)
		}
		if len(facts) > 0 && string(facts) != "null" {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("WaitSoulprintReported(%s): soulprint_facts not populated within %ds", sid, timeoutSec)
}
