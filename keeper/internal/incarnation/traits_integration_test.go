//go:build integration

// Integration guard for the label model (NIM-281): incarnation.traits round
// trip, plus the boundary that model draws — an incarnation's labels are the
// incarnation's, they never reach the hosts that belong to it, neither by being
// written to their rows nor by being read back into them.

package incarnation

import (
	"context"
	"slices"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/rbac"
	"github.com/souls-guild/soul-stack/keeper/internal/soul"
	"github.com/souls-guild/soul-stack/keeper/internal/soulpurview"
)

// seedSoul inserts a minimal souls row with the given stable-tag coven (ADR-008).
// NIM-124: incarnation membership is NO longer coven == incarnation name — it is
// the `incarnation_membership` relation, seeded via seedMembership. traits starts
// empty; a host only carries what an operator attaches to it directly (NIM-281).
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

// seedIncarnationRow / seedMembership: membership is the `incarnation_membership`
// relation (NIM-124), so member hosts must be bound in that table and the
// incarnation must exist (FK).
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
// the labels that describe the incarnation itself.
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

// TestIntegration_IncarnationLabels_DoNotReachMembers — the labels an operator
// attaches to an incarnation describe the incarnation. A host that belongs to it
// carries exactly what was attached to the host, before and after: the
// incarnation's traits are not readable from its row, its coven tag is not on the
// row, and neither is its NAME. Membership is answered from
// `incarnation_membership`, and asking for the members is spelled
// `incarnation=<name>` — never `coven=<name>` (NIM-281).
func TestIntegration_IncarnationLabels_DoNotReachMembers(t *testing.T) {
	resetAll(t)
	resetSouls(t)
	ctx := context.Background()

	seedIncarnationRow(t, "redis-prod")
	setIncarnationLabels(t, "redis-prod", []string{"dba"}, `{"team":"dba","env":"prod"}`)
	seedSoul(t, "host-a.example.com", []string{"dc1"})
	seedMembership(t, "redis-prod", "host-a.example.com")

	got, err := soul.SelectBySID(ctx, integrationPool, "host-a.example.com")
	if err != nil {
		t.Fatalf("SelectBySID: %v", err)
	}
	if len(got.Traits) != 0 {
		t.Errorf("souls.traits = %v, want empty — an incarnation's traits are not its hosts'", got.Traits)
	}
	if len(got.Coven) != 1 || got.Coven[0] != "dc1" {
		t.Errorf("souls.coven = %v, want [dc1] — neither the incarnation's tag nor its name belongs here", got.Coven)
	}

	// The same on the list path, which is where a widened label would have shown
	// up as access: a scope on the incarnation's tag, and one on its name, reach
	// no host — only the host's own `dc1` does.
	for _, tc := range []struct {
		scope string
		want  int
	}{
		{"coven=dba", 0},        // the incarnation's tag
		{"coven=redis-prod", 0}, // the incarnation's name
		{"coven=dc1", 1},        // the host's own tag
		{"trait.team=dba", 0},   // the incarnation's trait
	} {
		expr, err := rbac.ParseScopeExpr(tc.scope)
		if err != nil {
			t.Fatalf("ParseScopeExpr(%q): %v", tc.scope, err)
		}
		scope := soulpurview.Resolve(rbac.Purview{Exprs: []*rbac.ScopeExpr{expr}})
		_, total, err := soul.SelectAll(ctx, integrationPool, soul.ListFilter{}, scope, 0, 50)
		if err != nil {
			t.Fatalf("SelectAll(%s): %v", tc.scope, err)
		}
		if total != tc.want {
			t.Errorf("SelectAll(%s) = %d hosts, want %d", tc.scope, total, tc.want)
		}
	}
}

// TestIntegration_UpdateTraits_LeavesHostRowAlone — an incarnation write must not
// write to any host, so a label an operator attached to a host is still there
// afterwards. (Guard against the removed projection, which replaced souls.traits
// wholesale per incarnation.)
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

// TestIntegration_UpdateTraits_ReturnsPostgresSpelling — the struct [UpdateTraits]
// hands back must not carry two disagreeing renderings of the same column
// (NIM-521).
//
// [Incarnation.TraitsRaw] is defined as the column exactly as POSTGRES
// serializes it, and it is what the in-Go scope half reads
// ([handlers.IncarnationHandler.GetInScopeFor] → [rbac.TraitValues]). `inc` here
// is scanned BEFORE the UPDATE, so there are two ways to get this wrong and this
// test pins both:
//
//   - leave TraitsRaw alone → it keeps the OLD labels beside the NEW map, and a
//     caller that gates on the returned object grants by labels that are gone;
//   - assign the bytes we sent (`marshalJSONB`) → the field holds GO's spelling
//     under a name documented to hold Postgres'. That is NIM-521 itself, one
//     layer up: `1e6` would sit there where the column reads `1000000`, and the
//     SQL half of the same boundary would disagree with it about `trait.asn=…`.
//
// The number below is chosen so the two spellings cannot coincide, and most
// numbers do NOT have that property: encoding/json and jsonb both render 1e6 as
// `1000000`, so a test using it would pass with the field filled from either
// source and prove nothing. `1e-7` is outside the range encoding/json writes
// positionally — Go emits `1e-7`, Postgres stores `0.0000001` — so the TOKEN,
// not merely the whitespace, says which side the bytes came from.
func TestIntegration_UpdateTraits_ReturnsPostgresSpelling(t *testing.T) {
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
		map[string]any{"ratio": 1e-7, "env": "prod"})
	if err != nil {
		t.Fatalf("UpdateTraits: %v", err)
	}

	// What the column actually holds, read back independently of the struct.
	var stored string
	if err := integrationPool.QueryRow(ctx,
		`SELECT traits::text FROM incarnation WHERE name = $1`, "redis-prod").Scan(&stored); err != nil {
		t.Fatalf("read back traits: %v", err)
	}
	if got := string(res.Incarnation.TraitsRaw); got != stored {
		t.Errorf("TraitsRaw = %s, but the column holds %s.\n"+
			"The returned struct disagrees with Postgres about the very column the in-Go "+
			"scope half reads, so an operator gated on this object is judged by labels the "+
			"database does not have.", got, stored)
	}

	// And the consequence, stated in the terms the boundary is about: the scope
	// projection off TraitsRaw must name the token Postgres stores.
	texts := rbac.TraitValues(res.Incarnation.TraitsRaw)
	if !slices.Contains(texts["ratio"], "0.0000001") {
		t.Errorf("rbac.TraitValues(TraitsRaw)[ratio] = %q, want it to contain %q — a role scoped "+
			"trait.ratio=0.0000001 matches this incarnation in the LIST (Postgres renders the "+
			"column) and must not miss it in the single read.", texts["ratio"], "0.0000001")
	}
	if slices.Contains(texts["ratio"], "1e-7") || slices.Contains(texts["ratio"], "1e-07") {
		t.Errorf("rbac.TraitValues(TraitsRaw)[ratio] = %q — that is Go's spelling of the number, "+
			"which means TraitsRaw was filled from the payload we sent rather than from the "+
			"column. No scope value an operator can write will ever match it.", texts["ratio"])
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
