//go:build e2e_live

package harness

import (
	"context"
	"testing"
	"time"
)

// AddSoulToCoven adds a Coven label to `souls.coven` for the i-th soul
// container.
//
// Why: the scenario run's roster is resolved by Coven membership
// (`WHERE <incarnation.name> = ANY(coven)`, ADR-008 — incarnation.name is
// the root Coven label; keeper/internal/topology/resolver.go::rosterSQL).
// Without this the incarnation "has no connected hosts" -> run.go aborts
// with `no_hosts` BEFORE the dispatch phase -> zero apply_runs rows (run.go
// §3) -> WaitApplySuccess spins until timeout. Symmetric with the L3a
// harness (tests/e2e/harness/cert.go::AddSoulToCoven).
//
// IssueBootstrapToken creates a `souls` row with an empty coven, and the
// Bootstrap flow only upgrades status — coven stays empty. This step closes
// the gap of "connected, but not in the incarnation's roster".
//
// Idempotent (array_append only if the label isn't there yet). Fatal on
// error.
func (s *Stack) AddSoulToCoven(t *testing.T, soulIndex int, coven string) {
	t.Helper()
	if soulIndex < 0 || soulIndex >= len(s.SoulContainers) {
		t.Fatalf("AddSoulToCoven(%d): out of range (%d soul containers created)", soulIndex, len(s.SoulContainers))
	}
	sid := s.SoulContainers[soulIndex].SID
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
