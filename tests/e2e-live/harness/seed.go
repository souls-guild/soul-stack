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
// baseline state directly into Postgres. Used when the regular create flow is
// unavailable on L3b.
func (s *Stack) SeedIncarnationReady(t *testing.T, name, service, serviceVersion string, state map[string]any) {
	t.Helper()
	stateJSON, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("SeedIncarnationReady(%s): marshal state: %v", name, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := s.db.Exec(ctx, `
		INSERT INTO incarnation (id, service, service_version, state, status)
		VALUES ($1, $2, $3, $4::jsonb, 'ready')
	`, name, service, serviceVersion, string(stateJSON)); err != nil {
		t.Fatalf("SeedIncarnationReady(%s): %v", name, err)
	}
}

// AllSoulIndexes — every soul container index, for the common
// "bind the whole stack to one incarnation" case of
// [Stack.CreateIncarnationOnRoster].
func (s *Stack) AllSoulIndexes() []int {
	idx := make([]int, len(s.SoulContainers))
	for i := range s.SoulContainers {
		idx[i] = i
	}
	return idx
}

// soulprintBootstrapWaitSec — how long [Stack.CreateIncarnationOnRoster] waits
// for each member's first SoulprintReport. A real soul sends it right after the
// session is established, so this is a ceiling for container cold-start, not a
// budget that gets spent.
const soulprintBootstrapWaitSec = 60

// CreateIncarnationOnRoster bootstraps an incarnation onto an ALREADY-onboarded
// roster and returns (incarnationName, applyID of the create run).
//
// THE BOOTSTRAP ORDER LIVES HERE (NIM-192) — one place, not per test:
//
//  1. seed the `incarnation` row (status='ready', empty state) — direct SQL;
//  2. bind each soul as a member (incarnation_membership);
//  3. wait for each member's first SoulprintReport;
//  4. run the create scenario as an ordinary explicit run.
//
// Why not POST /v1/incarnations. Since NIM-124 membership is a first-class
// relation guarded by FK incarnation_membership_incarnation_fk (migration 099),
// a host CANNOT be bound before the incarnation row exists. POST
// /v1/incarnations inserts that row AND starts the create run in the same call
// (lifecycle.auto_create), leaving no window in between — and a create run that
// rolls onto a ready roster (`provision: {enabled: false}`) resolves the roster
// at run start, so an unbound roster aborts with `no_hosts` before dispatch
// (run.go §3). Seeding the row first is the only order that satisfies both
// constraints; it is the same direct-SQL escape the harness already uses for
// paths unreachable offline (see [Stack.SeedIncarnationReady]).
//
// Coverage note: this path deliberately does NOT exercise POST /v1/incarnations
// (input validation, create-scenario resolution, the `incarnation.created`
// audit event) — that surface belongs to the handler unit tests and, for the
// BARE variant (no starting scenario), to L3a (tests/e2e). L3a hit the same FK
// ordering and moved to the same helper (NIM-210), so the AUTO-STARTED create
// run is no longer covered end-to-end at either tier. Here create is an
// explicit run, so it writes `incarnation.scenario_started`, not
// `incarnation.created`.
//
// serviceRef — `<service>@<ref>`; the ref is stored in
// incarnation.service_version for readability only (the run path resolves the
// service ref from the registry, incarnation_typed.go::RunTyped). createScenario
// must carry `create: true`; scenarios that compose their own name via
// id.template (ADR-0079) are NOT usable here — the name is fixed by the seed.
func (s *Stack) CreateIncarnationOnRoster(t *testing.T, name, serviceRef, createScenario string, soulIndexes []int, input map[string]any) (string, string) {
	t.Helper()
	if len(soulIndexes) == 0 {
		t.Fatalf("CreateIncarnationOnRoster(%s): no soul indexes — an empty roster aborts the create run with no_hosts", name)
	}
	s.SeedIncarnationReady(t, name, stripServiceRef(serviceRef), serviceRefVersion(serviceRef), map[string]any{})
	for _, idx := range soulIndexes {
		s.AddMember(t, idx, name)
	}
	for _, idx := range soulIndexes {
		s.WaitSoulprintReported(t, idx, soulprintBootstrapWaitSec)
	}
	return name, s.RunScenario(t, name, createScenario, input)
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
