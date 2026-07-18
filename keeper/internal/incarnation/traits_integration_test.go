//go:build integration

// Integration guard for the Trait per-soul → per-incarnation relocation
// (ADR-060 amend, R1): incarnation.traits round trip + sync-hook projection
// into member hosts' souls.traits (on create-emulation and on binding a new
// host) + the souls.traits read layer (projection target) keeps serving
// containment targeting (where: soulprint.self.traits.<key> relies on the
// same jsonb).

package incarnation

import (
	"context"
	"fmt"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/soul"
)

// seedSoul inserts a minimal souls row with the given stable-tag coven (ADR-008).
// NIM-124: incarnation membership is NO longer coven == incarnation name — it is
// the `incarnation_membership` relation, seeded via seedMembership. traits is
// empty `{}` (projection target before the sync hook).
func seedSoul(t *testing.T, sid string, coven []string) {
	t.Helper()
	_, err := integrationPool.Exec(context.Background(),
		`INSERT INTO souls (sid, transport, status, coven, traits)
		 VALUES ($1, 'agent', 'connected', $2, '{}'::jsonb)`,
		sid, coven)
	if err != nil {
		t.Fatalf("seedSoul(%s): %v", sid, err)
	}
}

// seedIncarnationRow / seedMembership: NIM-124 — the trait sync projects onto
// members resolved via `incarnation_membership` (soul.BulkSelector.Incarnation),
// so member hosts must be bound in that table and the incarnation must exist (FK).
func seedIncarnationRow(t *testing.T, name string) {
	t.Helper()
	inc := &Incarnation{
		Name: name, Service: "redis", ServiceVersion: "v1",
		StateSchemaVersion: 1, Status: StatusReady,
	}
	if err := Create(context.Background(), integrationPool, inc); err != nil {
		t.Fatalf("seedIncarnationRow(%s): %v", name, err)
	}
}

func seedMembership(t *testing.T, incName string, sids ...string) {
	t.Helper()
	if err := AddMembers(context.Background(), integrationPool, incName, sids, nil); err != nil {
		t.Fatalf("seedMembership(%s): %v", incName, err)
	}
}

func soulTraits(t *testing.T, sid string) map[string]any {
	t.Helper()
	got, err := soul.SelectBySID(context.Background(), integrationPool, sid)
	if err != nil {
		t.Fatalf("SelectBySID(%s): %v", sid, err)
	}
	return got.Traits
}

func resetSouls(t *testing.T) {
	t.Helper()
	if _, err := integrationPool.Exec(context.Background(), `TRUNCATE TABLE souls CASCADE`); err != nil {
		t.Fatalf("TRUNCATE souls: %v", err)
	}
}

// TestIntegration_IncarnationTraits_RoundTrip — incarnation.traits is written
// to the column and read back (Trait source of truth, R1).
func TestIntegration_IncarnationTraits_RoundTrip(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	ctx := context.Background()

	creator := "archon-alice"
	inc := &Incarnation{
		Name: "redis-prod", Service: "redis", ServiceVersion: "v1",
		StateSchemaVersion: 1, Status: StatusReady, CreatedByAID: &creator,
		Traits: map[string]any{
			"team":   "dba",
			"owners": []any{"alice", "bob"},
		},
	}
	if err := Create(ctx, integrationPool, inc); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := SelectByName(ctx, integrationPool, "redis-prod")
	if err != nil {
		t.Fatalf("SelectByName: %v", err)
	}
	if got.Traits["team"] != "dba" {
		t.Errorf("Traits.team = %v, want dba", got.Traits["team"])
	}
	owners, ok := got.Traits["owners"].([]any)
	if !ok || len(owners) != 2 || owners[0] != "alice" {
		t.Errorf("Traits.owners = %v", got.Traits["owners"])
	}

	// Empty traits → `{}` (NOT NULL DEFAULT), not nil.
	inc2 := &Incarnation{
		Name: "redis-dev", Service: "redis", ServiceVersion: "v1",
		StateSchemaVersion: 1, Status: StatusReady, CreatedByAID: &creator,
	}
	if err := Create(ctx, integrationPool, inc2); err != nil {
		t.Fatalf("Create#2: %v", err)
	}
	got2, _ := SelectByName(ctx, integrationPool, "redis-dev")
	if len(got2.Traits) != 0 {
		t.Errorf("empty traits = %v, want empty map", got2.Traits)
	}
}

// TestIntegration_SyncTraitsToHosts_ProjectsToMembers — the sync hook projects
// incarnation.traits into souls.traits of ALL member hosts (coven ∋ incName)
// and does NOT touch foreign hosts.
func TestIntegration_SyncTraitsToHosts_ProjectsToMembers(t *testing.T) {
	resetAll(t)
	resetSouls(t)
	ctx := context.Background()

	// Two redis-prod members + one foreign host (a different incarnation).
	seedIncarnationRow(t, "redis-prod")
	seedSoul(t, "host-a.example.com", []string{"dc1"})
	seedSoul(t, "host-b.example.com", nil)
	seedSoul(t, "outsider.example.com", []string{"other-inc"})
	seedMembership(t, "redis-prod", "host-a.example.com", "host-b.example.com")

	traits := map[string]any{"team": "dba", "env": "prod"}
	if err := SyncTraitsToHosts(ctx, integrationPool, "redis-prod", traits); err != nil {
		t.Fatalf("SyncTraitsToHosts: %v", err)
	}

	for _, sid := range []string{"host-a.example.com", "host-b.example.com"} {
		got := soulTraits(t, sid)
		if got["team"] != "dba" || got["env"] != "prod" {
			t.Errorf("%s souls.traits = %v, want team=dba env=prod (projection)", sid, got)
		}
	}
	// The foreign host is untouched.
	if got := soulTraits(t, "outsider.example.com"); len(got) != 0 {
		t.Errorf("outsider souls.traits = %v, want empty (outside the incarnation)", got)
	}
}

// TestIntegration_SyncTraitsToHosts_NewHostPicksUp — bind scenario: a host
// that bound to the incarnation AFTER its create picks up its incarnation's
// traits on a repeat sync (idempotent replace projection).
func TestIntegration_SyncTraitsToHosts_NewHostPicksUp(t *testing.T) {
	resetAll(t)
	resetSouls(t)
	ctx := context.Background()

	seedIncarnationRow(t, "redis-prod")
	seedSoul(t, "host-a.example.com", nil)
	seedMembership(t, "redis-prod", "host-a.example.com")
	traits := map[string]any{"team": "dba"}
	if err := SyncTraitsToHosts(ctx, integrationPool, "redis-prod", traits); err != nil {
		t.Fatalf("SyncTraitsToHosts#1: %v", err)
	}

	// A new host bound to the incarnation (bind via core.soul.registered);
	// its souls.traits is still empty.
	seedSoul(t, "host-c.example.com", nil)
	seedMembership(t, "redis-prod", "host-c.example.com")
	if got := soulTraits(t, "host-c.example.com"); len(got) != 0 {
		t.Fatalf("new host pre-sync traits = %v, want empty", got)
	}

	// A repeat sync (bind hook) projects onto ALL members, including the new one.
	if err := SyncTraitsToHosts(ctx, integrationPool, "redis-prod", traits); err != nil {
		t.Fatalf("SyncTraitsToHosts#2: %v", err)
	}
	if got := soulTraits(t, "host-c.example.com"); got["team"] != "dba" {
		t.Errorf("new host post-sync traits = %v, want team=dba", got)
	}
}

// TestIntegration_ProjectedTraits_ContainmentTargeting — the read layer
// (projection target souls.traits) keeps serving containment targeting over
// projected traits (the foundation of where: soulprint.self.traits.<key>, the
// same jsonb @>). Checks the PG predicate itself on projected data.
func TestIntegration_ProjectedTraits_ContainmentTargeting(t *testing.T) {
	resetAll(t)
	resetSouls(t)
	ctx := context.Background()

	seedIncarnationRow(t, "redis-prod")
	seedSoul(t, "host-a.example.com", nil)
	seedSoul(t, "host-b.example.com", nil)
	seedSoul(t, "outsider.example.com", []string{"other-inc"})
	seedMembership(t, "redis-prod", "host-a.example.com", "host-b.example.com")

	if err := SyncTraitsToHosts(ctx, integrationPool, "redis-prod", map[string]any{"team": "dba"}); err != nil {
		t.Fatalf("SyncTraitsToHosts: %v", err)
	}

	var n int
	err := integrationPool.QueryRow(ctx,
		`SELECT count(*) FROM souls WHERE traits @> '{"team":"dba"}'::jsonb`).Scan(&n)
	if err != nil {
		t.Fatalf("containment query: %v", err)
	}
	if n != 2 {
		t.Errorf("containment matched %d souls, want 2 (only projected members)", n)
	}
}

// TestIntegration_UpdateTraits_PersistsAndReturnsKeys — the operational PUT
// path: a wholesale replace of incarnation.traits is persisted to the column,
// OldKeys/NewKeys are correct.
func TestIntegration_UpdateTraits_PersistsAndReturnsKeys(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	ctx := context.Background()

	creator := "archon-alice"
	if err := Create(ctx, integrationPool, &Incarnation{
		Name: "redis-prod", Service: "redis", ServiceVersion: "v1",
		StateSchemaVersion: 1, Status: StatusReady, CreatedByAID: &creator,
		Traits: map[string]any{"team": "dba"},
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	res, err := UpdateTraits(ctx, integrationPool, "redis-prod",
		map[string]any{"env": "prod", "az": "a"})
	if err != nil {
		t.Fatalf("UpdateTraits: %v", err)
	}
	if len(res.OldKeys) != 1 || res.OldKeys[0] != "team" {
		t.Errorf("OldKeys = %v, want [team]", res.OldKeys)
	}
	if len(res.NewKeys) != 2 || res.NewKeys[0] != "az" || res.NewKeys[1] != "env" {
		t.Errorf("NewKeys = %v, want [az env] (sorted)", res.NewKeys)
	}

	// The column is replaced WHOLESALE (the old team key is gone).
	got, _ := SelectByName(ctx, integrationPool, "redis-prod")
	if got.Traits["env"] != "prod" || got.Traits["az"] != "a" {
		t.Errorf("persisted traits = %v, want env=prod az=a", got.Traits)
	}
	if _, stillThere := got.Traits["team"]; stillThere {
		t.Errorf("persisted traits still has team - replace must overwrite the whole map: %v", got.Traits)
	}
}

// TestIntegration_UpdateTraits_EmptyClears — an empty map clears the labels
// (column → `{}`).
func TestIntegration_UpdateTraits_EmptyClears(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	ctx := context.Background()

	creator := "archon-alice"
	if err := Create(ctx, integrationPool, &Incarnation{
		Name: "redis-prod", Service: "redis", ServiceVersion: "v1",
		StateSchemaVersion: 1, Status: StatusReady, CreatedByAID: &creator,
		Traits: map[string]any{"team": "dba"},
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	res, err := UpdateTraits(ctx, integrationPool, "redis-prod", map[string]any{})
	if err != nil {
		t.Fatalf("UpdateTraits(empty): %v", err)
	}
	if len(res.NewKeys) != 0 {
		t.Errorf("NewKeys = %v, want [] (cleared)", res.NewKeys)
	}
	got, _ := SelectByName(ctx, integrationPool, "redis-prod")
	if len(got.Traits) != 0 {
		t.Errorf("traits after clear = %v, want empty", got.Traits)
	}
}

// TestIntegration_UpdateTraits_NotFound — a nonexistent incarnation →
// ErrIncarnationNotFound.
func TestIntegration_UpdateTraits_NotFound(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	_, err := UpdateTraits(ctx, integrationPool, "nope", map[string]any{"team": "dba"})
	if err == nil {
		t.Fatal("UpdateTraits(missing) returned nil")
	}
}

// TestIntegration_PgContainmentMatchesArrayWithScalarRHS — CONFIRMS the root
// cause of BUG #1 on real PG (the psql equivalent from the spec): jsonb
// containment `@>` with a scalar RHS MATCHES an array (array-contains-primitive,
// PG §8.14.3). This is exactly what made the old SQL leg
// `traits @> '{"env":"prod"}'::jsonb` true for the list-Trait
// `{"env":["prod","stage"]}`, diverging from the GET path's traitScalarEquals
// (list→false). The fix replaced containment with scalar-equality
// `traits->>$ = $`, which does NOT match an array — checked by the adjacent
// scalar-leg test below.
func TestIntegration_PgContainmentMatchesArrayWithScalarRHS(t *testing.T) {
	ctx := context.Background()
	var containmentMatch, scalarMatch bool
	// @> with a scalar RHS against an array → TRUE (root cause of the bug).
	if err := integrationPool.QueryRow(ctx,
		`SELECT '{"env":["prod","stage"]}'::jsonb @> '{"env":"prod"}'::jsonb`).Scan(&containmentMatch); err != nil {
		t.Fatalf("containment probe: %v", err)
	}
	if !containmentMatch {
		t.Error("PG @> with a scalar-RHS did NOT match the array - the BUG #1 premise did not reproduce (expected TRUE)")
	}
	// the scalar form `->>` against an array → array text ≠ 'prod' → FALSE (the fix).
	if err := integrationPool.QueryRow(ctx,
		`SELECT ('{"env":["prod","stage"]}'::jsonb)->>'env' = 'prod'`).Scan(&scalarMatch); err != nil {
		t.Fatalf("scalar probe: %v", err)
	}
	if scalarMatch {
		t.Error("scalar `->>'env' = 'prod'` matched the array - the fix did not close the desync (expected FALSE)")
	}
}

// TestIntegration_ScopeTrait_ListVsScalar_ConsistentWithGet — the MAIN guard
// for BUG #1: the List path (SelectAll trait-scope SQL) and the GET path
// (traitScalarEquals, scalar-only) give the SAME answer on the same data. Two
// incarnations:
//   - redis-list  traits={env:[prod,stage]}  (list-Trait);
//   - redis-scalar traits={env:prod}          (scalar-Trait).
//
// Scope trait=env:prod: scalar is visible (both paths), list is NOT visible
// (both paths, scalar-only). Previously List showed the list-incarnation
// (containment @>) while GET returned 404 (traitScalarEquals list→false) — a
// mismatch. Here we prove that after the fix both paths agree on live PG.
func TestIntegration_ScopeTrait_ListVsScalar_ConsistentWithGet(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	ctx := context.Background()
	creator := "archon-alice"

	for _, seed := range []struct {
		name   string
		traits map[string]any
	}{
		{"redis-list", map[string]any{"env": []any{"prod", "stage"}}},
		{"redis-scalar", map[string]any{"env": "prod"}},
	} {
		if err := Create(ctx, integrationPool, &Incarnation{
			Name: seed.name, Service: "redis", ServiceVersion: "v1",
			StateSchemaVersion: 1, Status: StatusReady, CreatedByAID: &creator,
			Traits: seed.traits,
		}); err != nil {
			t.Fatalf("Create %s: %v", seed.name, err)
		}
	}

	scope := ListScope{Traits: []TraitPair{{Key: "env", Value: "prod"}}}

	// List path: SQL trait-scope. Exactly redis-scalar must be visible.
	out, total, err := SelectAll(ctx, integrationPool, ListFilter{}, scope, 0, 50)
	if err != nil {
		t.Fatalf("SelectAll trait-scope: %v", err)
	}
	listVisible := map[string]bool{}
	for _, inc := range out {
		listVisible[inc.Name] = true
	}
	if total != 1 || !listVisible["redis-scalar"] {
		t.Errorf("List: total=%d visible=%v, want only redis-scalar (scalar-only)", total, listVisible)
	}
	if listVisible["redis-list"] {
		t.Error("List SEES the list-Trait incarnation via trait-scope - BUG #1 not fixed (containment semantics)")
	}

	// GET path: the same scalar predicate as traitScalarEquals (scalar-only).
	// Must agree with List on every incarnation.
	for _, name := range []string{"redis-list", "redis-scalar"} {
		inc, err := SelectByName(ctx, integrationPool, name)
		if err != nil {
			t.Fatalf("SelectByName %s: %v", name, err)
		}
		getVisible := traitScalarEqualsLocal(inc.Traits, "env", "prod")
		if getVisible != listVisible[name] {
			t.Errorf("DESYNC List<->Get for %s: List=%v Get=%v (traits=%v)",
				name, listVisible[name], getVisible, inc.Traits)
		}
	}
}

// traitScalarEqualsLocal duplicates the scalar-only semantics of
// handlers.traitScalarEquals (unexported, lives in the api package) to check
// List↔Get consistency within the incarnation package: scalar
// (string/number/bool) → string equality; list/map → false. A deliberate copy
// (a few lines vs. an incarnation→api cross-package dependency that shouldn't
// exist).
func traitScalarEqualsLocal(traits map[string]any, key, value string) bool {
	v, ok := traits[key]
	if !ok {
		return false
	}
	switch v.(type) {
	case string, float64, bool, int, int64:
		return fmt.Sprint(v) == value
	default:
		return false
	}
}
