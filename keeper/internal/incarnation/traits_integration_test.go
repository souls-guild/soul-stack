//go:build integration

// Integration guard for the label model of ADR-080: incarnation.traits round
// trip, plus inheritance by membership — a host reads the labels of the
// incarnations it belongs to WITHOUT anything being written to its own row, a
// host in two incarnations inherits from both, a key held on both sides yields
// both values, and an incarnation write never reaches a host row.

package incarnation

import (
	"context"
	"slices"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/soul"
)

// seedSoul inserts a minimal souls row with the given stable-tag coven (ADR-008).
// NIM-124: incarnation membership is NO longer coven == incarnation name — it is
// the `incarnation_membership` relation, seeded via seedMembership. traits starts
// empty; a host only carries what an operator attaches to it directly (ADR-080).
func seedSoul(t *testing.T, sid string, coven []string) {
	t.Helper()
	// souls.coven is NOT NULL; pgx maps a nil slice to NULL.
	if coven == nil {
		coven = []string{}
	}
	_, err := integrationPool.Exec(context.Background(),
		`INSERT INTO souls (sid, transport, status, coven, traits)
		 VALUES ($1, 'agent', 'connected', $2, '{}'::jsonb)`,
		sid, coven)
	if err != nil {
		t.Fatalf("seedSoul(%s): %v", sid, err)
	}
}

// seedIncarnationRow / seedMembership: inheritance resolves through
// `incarnation_membership` (NIM-124), so member hosts must be bound in that table
// and the incarnation must exist (FK).
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

// setIncarnationLabels sets an incarnation's coven tags and traits directly —
// the labels hosts will inherit from it.
func setIncarnationLabels(t *testing.T, name string, covens []string, traitsJSON string) {
	t.Helper()
	if covens == nil {
		covens = []string{}
	}
	_, err := integrationPool.Exec(context.Background(),
		`UPDATE incarnation SET covens = $2, traits = $3::jsonb WHERE name = $1`,
		name, covens, traitsJSON)
	if err != nil {
		t.Fatalf("setIncarnationLabels(%s): %v", name, err)
	}
}

// setSoulTraits attaches traits directly to a host — the per-soul write path
// (POST /v1/souls/traits) reduced to its effect on the row.
func setSoulTraits(t *testing.T, sid, traitsJSON string) {
	t.Helper()
	_, err := integrationPool.Exec(context.Background(),
		`UPDATE souls SET traits = $2::jsonb WHERE sid = $1`, sid, traitsJSON)
	if err != nil {
		t.Fatalf("setSoulTraits(%s): %v", sid, err)
	}
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

// TestIntegration_InheritedLabels_FromMembership — a host picks up the labels of
// the incarnation it belongs to WITHOUT anything having been written to its own
// row: nothing is copied down, the union is resolved on read (ADR-080). The
// incarnation's NAME comes along on the coven axis, mirroring the
// incarnation-side resolver.
func TestIntegration_InheritedLabels_FromMembership(t *testing.T) {
	resetAll(t)
	resetSouls(t)
	ctx := context.Background()

	seedIncarnationRow(t, "redis-prod")
	setIncarnationLabels(t, "redis-prod", []string{"dba"}, `{"team":"dba","env":"prod"}`)
	seedSoul(t, "host-a.example.com", []string{"dc1"})
	seedSoul(t, "outsider.example.com", []string{"other-inc"})
	seedMembership(t, "redis-prod", "host-a.example.com")

	got, err := soul.LoadInheritedLabels(ctx, integrationPool, "host-a.example.com")
	if err != nil {
		t.Fatalf("LoadInheritedLabels: %v", err)
	}
	if got.Traits["team"] != "dba" || got.Traits["env"] != "prod" {
		t.Errorf("inherited traits = %v, want team=dba env=prod", got.Traits)
	}
	if !slices.Contains(got.Covens, "dba") {
		t.Errorf("inherited covens = %v, want the incarnation's tag 'dba'", got.Covens)
	}
	if !slices.Contains(got.Covens, "redis-prod") {
		t.Errorf("inherited covens = %v, want the incarnation NAME as a coven tag", got.Covens)
	}
	// The host's own row was never written to.
	if own := soulTraits(t, "host-a.example.com"); len(own) != 0 {
		t.Errorf("souls.traits = %v, want empty — inheritance must not write to the host", own)
	}
	// A non-member inherits nothing.
	outsider, err := soul.LoadInheritedLabels(ctx, integrationPool, "outsider.example.com")
	if err != nil {
		t.Fatalf("LoadInheritedLabels(outsider): %v", err)
	}
	if len(outsider.Traits) != 0 || len(outsider.Covens) != 0 {
		t.Errorf("outsider inherited %v / %v, want nothing", outsider.Covens, outsider.Traits)
	}
}

// TestIntegration_InheritedLabels_TwoIncarnations — membership is M:N (migration
// 099), and a host in two incarnations inherits from BOTH. This is what the
// removed projection could not express: it replaced souls.traits wholesale per
// incarnation, so syncing one erased what the other had projected.
func TestIntegration_InheritedLabels_TwoIncarnations(t *testing.T) {
	resetAll(t)
	resetSouls(t)
	ctx := context.Background()

	seedIncarnationRow(t, "redis-prod")
	seedIncarnationRow(t, "metrics-prod")
	setIncarnationLabels(t, "redis-prod", nil, `{"team":"dba"}`)
	setIncarnationLabels(t, "metrics-prod", nil, `{"tier":"gold"}`)
	seedSoul(t, "host-a.example.com", nil)
	seedMembership(t, "redis-prod", "host-a.example.com")
	seedMembership(t, "metrics-prod", "host-a.example.com")

	got, err := soul.LoadInheritedLabels(ctx, integrationPool, "host-a.example.com")
	if err != nil {
		t.Fatalf("LoadInheritedLabels: %v", err)
	}
	if got.Traits["team"] != "dba" || got.Traits["tier"] != "gold" {
		t.Fatalf("inherited traits = %v, want BOTH incarnations (team=dba, tier=gold)", got.Traits)
	}
}

// TestIntegration_InheritedLabels_ContestedKey_Unions — the same key on the host
// and on its incarnation yields both values, in no order of precedence: either
// one grants, so neither may be dropped.
func TestIntegration_InheritedLabels_ContestedKey_Unions(t *testing.T) {
	resetAll(t)
	resetSouls(t)
	ctx := context.Background()

	seedIncarnationRow(t, "redis-prod")
	setIncarnationLabels(t, "redis-prod", nil, `{"owner":"dba"}`)
	seedSoul(t, "host-a.example.com", nil)
	seedMembership(t, "redis-prod", "host-a.example.com")
	setSoulTraits(t, "host-a.example.com", `{"owner":"bobik"}`)

	inherited, err := soul.LoadInheritedLabels(ctx, integrationPool, "host-a.example.com")
	if err != nil {
		t.Fatalf("LoadInheritedLabels: %v", err)
	}
	effective := soul.UnionTraits(soulTraits(t, "host-a.example.com"), inherited.Traits)

	values, ok := effective["owner"].([]any)
	if !ok || len(values) != 2 {
		t.Fatalf("effective owner = %#v, want both values", effective["owner"])
	}
	if values[0] != "bobik" || values[1] != "dba" {
		t.Errorf("effective owner = %v, want [bobik dba] (own first, neither dropped)", values)
	}
}

// TestIntegration_UpdateTraits_LeavesHostRowAlone — the guard against the failure
// mode ADR-080 removes: replacing an incarnation's traits must not write to any
// host, so a label an operator attached to a host is still there afterwards.
func TestIntegration_UpdateTraits_LeavesHostRowAlone(t *testing.T) {
	resetAll(t)
	resetSouls(t)
	ctx := context.Background()

	seedIncarnationRow(t, "redis-prod")
	seedSoul(t, "host-a.example.com", nil)
	seedMembership(t, "redis-prod", "host-a.example.com")
	setSoulTraits(t, "host-a.example.com", `{"owner":"bobik"}`)

	if _, err := UpdateTraits(ctx, integrationPool, "redis-prod", map[string]any{"team": "dba"}); err != nil {
		t.Fatalf("UpdateTraits: %v", err)
	}
	if got := soulTraits(t, "host-a.example.com"); got["owner"] != "bobik" {
		t.Fatalf("souls.traits = %v, want owner=bobik intact — an incarnation write must not reach the host", got)
	}

	// Clearing the incarnation's labels likewise leaves the host's own alone —
	// the old projection cleared every member here.
	if _, err := UpdateTraits(ctx, integrationPool, "redis-prod", nil); err != nil {
		t.Fatalf("UpdateTraits(clear): %v", err)
	}
	if got := soulTraits(t, "host-a.example.com"); got["owner"] != "bobik" {
		t.Fatalf("souls.traits = %v after clearing the incarnation, want owner=bobik intact", got)
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
