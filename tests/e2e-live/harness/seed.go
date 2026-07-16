//go:build e2e_live

package harness

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// Direct-seed helpers L3b: write incarnation/soulprint directly to Postgres,
// bypassing the Operator API. A verbatim (adapted for SoulContainers) port of
// the L3a harness (tests/e2e/harness/stack.go::SeedIncarnationReady,
// cert.go::SeedSoulprint). Duplication is sanctioned by architect verdict
// `a0af3d90ec118aafd`: L3a/L3b are independent test frequencies (stub vs real
// soul), the shared harness is unreachable across the module boundary.
//
// Why seeding is needed on L3b: some services have a create scenario that
// isn't applicable offline (cloud-spawn / declared-primary / probe on a
// not-yet-running daemon), so the incarnation is seeded directly with a
// baseline state, and the mutating scenario is tested on top of a live daemon
// started separately.

// SeedIncarnationReady inserts a ready (status='ready') incarnation with a
// baseline state directly into Postgres. spec is an empty `{}` (mutating
// scenarios don't read spec). Used when the regular create flow is unavailable
// on L3b.
func (s *Stack) SeedIncarnationReady(t *testing.T, name, service, serviceVersion string, state map[string]any) {
	t.Helper()
	stateJSON, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("SeedIncarnationReady(%s): marshal state: %v", name, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := s.db.Exec(ctx, `
		INSERT INTO incarnation (name, service, service_version, spec, state, status)
		VALUES ($1, $2, $3, '{}'::jsonb, $4::jsonb, 'ready')
	`, name, service, serviceVersion, string(stateJSON)); err != nil {
		t.Fatalf("SeedIncarnationReady(%s): %v", name, err)
	}
}

// SpecHostDecl - a declared entry of `incarnation.spec.hosts[]` for
// [Stack.SeedIncarnationForCreate]. The shape mirrors the
// topology.parseDeclaredRoles parser (`{sid, role}`, ADR-008); role is
// kebab-case (`primary`/`replica`/...), empty is allowed (host outside the
// declared role).
type SpecHostDecl struct {
	SID  string
	Role string
}

// SeedIncarnationForCreate inserts an incarnation in status='ready' with an
// EMPTY state (`{}`) and declared `spec.hosts[]` (roles host-0/host-1/...).
// Difference from [Stack.SeedIncarnationReady]: state is empty (create fills
// it itself via state_changes), while spec carries the declared roles - read by
// topology.parseDeclaredRoles when resolving
// `soulprint.hosts.where("role == 'primary'")` in the create scenario.
//
// Why a separate helper: POST /v1/incarnations does NOT accept declared
// spec.hosts (ADR-008, exactly as the spec explains), yet bootstrap-create
// scenarios that target primary via
// `soulprint.hosts.where("role == 'primary'")[0]` depend on these declared
// roles. A direct SQL seed of spec.hosts BEFORE RunScenario(create) closes the
// gap of "declared role unavailable offline".
//
// status='ready' -> the regular RunScenario(create) passes the lock gate
// (lockRun starts a normal run from ready, run.go), no FromLocked hack needed.
func (s *Stack) SeedIncarnationForCreate(t *testing.T, name, service, serviceVersion string, hosts []SpecHostDecl) {
	t.Helper()
	specHosts := make([]map[string]any, 0, len(hosts))
	for _, h := range hosts {
		obj := map[string]any{"sid": h.SID}
		if h.Role != "" {
			obj["role"] = h.Role
		}
		specHosts = append(specHosts, obj)
	}
	specJSON, err := json.Marshal(map[string]any{"hosts": specHosts})
	if err != nil {
		t.Fatalf("SeedIncarnationForCreate(%s): marshal spec: %v", name, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := s.db.Exec(ctx, `
		INSERT INTO incarnation (name, service, service_version, spec, state, status)
		VALUES ($1, $2, $3, $4::jsonb, '{}'::jsonb, 'ready')
	`, name, service, serviceVersion, string(specJSON)); err != nil {
		t.Fatalf("SeedIncarnationForCreate(%s): %v", name, err)
	}
}

// SeedSoulprint writes the soulprint facts of the i-th soul container directly
// to `souls.soulprint_facts` (SoulprintFacts-JSON shape, CEL
// `soulprint.self.<path>`, ADR-018). On L3b the real soul sends its own
// SoulprintReport when the session is established (see
// WaitSoulprintReported) - seeding is only needed when a test requires a
// deterministic fact (e.g. a stable primary_ip) independent of the
// container's network address. SID is taken from SoulContainers[soulIndex].
func (s *Stack) SeedSoulprint(t *testing.T, soulIndex int, facts map[string]any) {
	t.Helper()
	if soulIndex < 0 || soulIndex >= len(s.SoulContainers) {
		t.Fatalf("SeedSoulprint(%d): out of range (%d soul containers created)", soulIndex, len(s.SoulContainers))
	}
	sid := s.SoulContainers[soulIndex].SID
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
