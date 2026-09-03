//go:build integration

// NIM-587 — agreement guard for the trait WRITE gate across ALL FOUR of its
// surfaces:
//
//	REST — POST /v1/souls/traits                    → handlers.SoulHandler.AssignTraitsTyped
//	MCP  — keeper.soul.traits-assign
//	REST — PUT  /v1/incarnations/{id}/traits      → handlers.IncarnationHandler.SetTraitsTyped
//	MCP  — keeper.incarnation.traits-set
//
// Why this test is not "the NIM-529 guard, again, for incarnations". The NIM-529
// guard compares REST against MCP, because there the two copies of one rule
// drifted apart. That shape would have been GREEN through the whole of NIM-587:
// both incarnation surfaces carried NO gate at all, so they agreed with each
// other perfectly, and one of them even said so in its header comment. Surfaces
// that are wrong in the same direction agree. So the four surfaces are measured
// here against ONE expectation, and the reference for that expectation is the
// pair of surfaces that already had the gate.
//
// The rule being pinned: a trait pair is a GRANT, not a description. `trait.<key>`
// is a live scope dimension on the read side for BOTH objects that carry traits —
// souls (soulScopeColumns.Traits) and incarnations (incScopeColumns.Traits) — so
// stamping `tier=gold` on an incarnation hands every `trait.tier=gold` role sight
// of it. Being allowed to write to the object (gate (a)) is a different question
// from being allowed to attach the label (gate (b)); this test is gate (b), and
// gate (a) is granted to the operator in every case so that it cannot be what
// produces a refusal.
//
// Postgres is the neutral third party, for the reason NIM-529 established: the
// text a gate must reason about is what `traits ->> '<key>'` will yield from the
// row the write CREATES, and jsonb re-canonicalizes numbers (`1e-7` reads back as
// `0.0000001`). A Go rendering of the expectation would be another copy of the
// step under test and would agree with a wrong implementation, so the oracle is
// written in SQL. The cases are [traitGateCases] — the same table NIM-529 checks,
// shared deliberately: one expectation for one rule is the whole point, and a
// second table would be the next thing to drift.
//
// The two CREATE paths (handlers.IncarnationHandler.CreateTyped and
// keeper.incarnation.create) are deliberately absent. Creating an incarnation
// that carries an out-of-scope trait poses a question this gate does not answer —
// whether the creator becomes the owner of a label they do not hold, or the create
// is refused — and that is a permissions-semantics decision, still open. When it
// lands, both create surfaces join the table below together; deciding them apart
// is exactly the defect this file exists to prevent.
//
// Proven by mutation, in the form of real code: deleting the
// handlers.ScreenTraitPairsInScope block from mcp/incarnation_traits_set.go
// reddens on the foreign cases with the MCP incarnation surface admitting what the
// other three refuse; deleting it from incarnation_typed.go SetTraitsTyped reddens
// the same way for the REST incarnation surface. That mutant is the pre-NIM-587
// state of the code, and it is what a REST-vs-MCP-only guard would have missed.
//
// Run:
//
//	cd keeper && SOUL_STACK_INTEGRATION_REQUIRE_DOCKER=1 \
//	    go test -tags=integration -race -count=1 ./internal/mcp/ \
//	    -run TestIntegration_TraitWriteGate_AllSurfacesAgree

package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	keeperjwt "github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac/rbactest"
)

// traitGateService — the incarnation's service. `incarnation.service` has no FK,
// and the value only matters as an RBAC scope dimension the fixture leaves
// unconstrained.
const traitGateService = "nim587-svc"

// traitWriteOutcome — what one surface did with one payload: whether it admitted
// the write, and what the row holds afterwards.
type traitWriteOutcome struct {
	surface  string
	accepted bool
	stored   string
}

func TestIntegration_TraitWriteGate_AllSurfacesAgree(t *testing.T) {
	for _, tc := range traitGateCases() {
		t.Run(tc.name, func(t *testing.T) {
			// Order matters: TRUNCATE operators … CASCADE reaches every table
			// with an FK to operators, incarnation among them, so both objects
			// are seeded AFTER it.
			truncateOperators(t)
			seedOperator(t, "archon-alice", "")

			sid := "nim587-" + tc.name + ".test"
			seedTraitGateSoul(t, sid)

			incName := "nim587-" + tc.name
			seedTraitGateIncarnation(t, incName)

			// The oracle first: if Postgres and the fixture disagree, the case is
			// mis-stated and comparing four surfaces to it proves nothing.
			oraclePairs := pgTraitPairTexts(t, tc.payload, tc.key)
			oracleAccept := true
			for _, p := range oraclePairs {
				if !slices.Contains(tc.granted, p) {
					oracleAccept = false
					break
				}
			}
			if oracleAccept != tc.wantAccept {
				t.Fatalf("[%s] the fixture claims accept=%v, but Postgres renders %q as the pairs %v "+
					"and the operator's scope carries %v — the CASE is wrong, not the code (%s)",
					tc.name, tc.wantAccept, tc.payload, oraclePairs, tc.granted, tc.why)
			}

			cfg := traitWriteGateRBAC(tc.key, tc.granted)

			outcomes := []traitWriteOutcome{}

			accepted := runTraitAssignREST(t, cfg, tc.payload)
			outcomes = append(outcomes, traitWriteOutcome{"REST /v1/souls/traits", accepted, readTraits(t, sid)})
			resetTraits(t, sid)

			accepted = runTraitAssignMCP(t, cfg, tc.payload)
			outcomes = append(outcomes, traitWriteOutcome{"MCP keeper.soul.traits-assign", accepted, readTraits(t, sid)})
			resetTraits(t, sid)

			accepted = runIncarnationTraitsSetREST(t, cfg, incName, tc.payload)
			outcomes = append(outcomes, traitWriteOutcome{"REST /v1/incarnations/{id}/traits", accepted, readIncTraits(t, incName)})
			resetIncTraits(t, incName)

			accepted = runIncarnationTraitsSetMCP(t, cfg, incName, tc.payload)
			outcomes = append(outcomes, traitWriteOutcome{"MCP keeper.incarnation.traits-set", accepted, readIncTraits(t, incName)})
			resetIncTraits(t, incName)

			// (1) The four writers against EACH OTHER. A surface that stops
			// calling the shared gate — or a new one that never started — shows
			// up here as the odd one out, named.
			var admitted, refused []string
			for _, o := range outcomes {
				if o.accepted {
					admitted = append(admitted, o.surface)
				} else {
					refused = append(refused, o.surface)
				}
			}
			if len(admitted) > 0 && len(refused) > 0 {
				t.Errorf("[%s] the trait write surfaces disagree on %q under scope trait.%s=%v:\n"+
					"  admitted: %s\n  refused:  %s\n"+
					"one surface is not calling handlers.ScreenTraitPairsInScope, so the same operator is "+
					"refused through one door and admitted through another (%s)",
					tc.name, tc.payload, tc.key, tc.granted,
					strings.Join(admitted, ", "), strings.Join(refused, ", "), tc.why)
			}

			// (2) All of them against Postgres' own `->>`. Agreement alone would
			// be satisfied by four surfaces that are wrong the same way — which is
			// precisely the state NIM-587 found the incarnation pair in.
			for _, o := range outcomes {
				if o.accepted != oracleAccept {
					t.Errorf("[%s] %s accepted=%v, but `->>` over the payload yields the pairs %v and the "+
						"operator's scope carries %v (%s)",
						tc.name, o.surface, o.accepted, oraclePairs, tc.granted, tc.why)
				}
			}

			// (3) The row that now exists is the row the gate reasoned about. A
			// gate that admits one payload while the write stores another would
			// pass (1) and (2) and still hand out a foreign pair; a gate that runs
			// AFTER the write would leave the pair behind on a refusal.
			wantStored := "{}"
			if tc.wantAccept {
				wantStored = pgCanonicalJSONB(t, tc.payload)
			}
			for _, o := range outcomes {
				if o.stored != wantStored {
					t.Errorf("[%s] after %s the stored traits are %s, want %s — the gate and the write "+
						"disagree about what is being stored, or the gate runs after it",
						tc.name, o.surface, o.stored, wantStored)
				}
			}
		})
	}
}

// --- surfaces ---

// runIncarnationTraitsSetREST drives PUT /v1/incarnations/{id}/traits through
// the typed entry point the HTTP layer calls, with the live pool and a real
// enforcer. Same contract as the soul drivers: anything that is not the scope
// refusal is fatal, so a 500 cannot be counted as a "no" and match another
// surface by accident.
//
// The route's own middleware — gate (a) — is not in the path here and does not
// need to be: the fixture grants incarnation.traits-set on the incarnation's
// coven, so gate (a) would pass, and the MCP driver below exercises it for real.
func runIncarnationTraitsSetREST(t *testing.T, cfg *rbactest.Config, name, payload string) bool {
	t.Helper()
	enf, err := rbactest.NewEnforcer(cfg)
	if err != nil {
		t.Fatalf("NewEnforcer: %v", err)
	}
	h := handlers.NewIncarnationHandler(integrationPool, nil, nil, nil, nil, nil, enf, nil)

	var traits map[string]any
	if err := json.Unmarshal([]byte(payload), &traits); err != nil {
		t.Fatalf("decode payload %q: %v", payload, err)
	}

	_, err = h.SetTraitsTyped(context.Background(),
		&keeperjwt.Claims{Subject: "archon-alice"}, name, traits)
	if err == nil {
		return true
	}
	d, ok := handlers.AsProblemDetails(err)
	if !ok {
		t.Fatalf("REST incarnation: non-problem error: %T %v", err, err)
	}
	if d.Status != 422 || !strings.Contains(d.Detail, "outside operator trait-scope") {
		t.Fatalf("REST incarnation: unexpected refusal (status=%d type=%s detail=%q) — expected either "+
			"success or the trait-scope refusal; anything else would be counted as a 'no' and could match "+
			"another surface by accident", d.Status, d.Type, d.Detail)
	}
	return false
}

// runIncarnationTraitsSetMCP drives keeper.incarnation.traits-set end-to-end over
// JSON-RPC against a server wired with the same pool and the same RBAC fixture —
// gate (a) included, since the tool runs its own OR-Check before gate (b).
func runIncarnationTraitsSetMCP(t *testing.T, cfg *rbactest.Config, name, payload string) bool {
	t.Helper()
	base, stop := startMCPServer(t, cfg)
	defer stop()
	token := newToken(t, "archon-alice", []string{traitGateRole})

	resp := doRPC(t, base, token, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{
			"name": "keeper.incarnation.traits-set",
			"arguments": json.RawMessage(fmt.Sprintf(
				`{"id":%q,"traits":%s}`, name, payload)),
		},
	})
	if resp.Error == nil {
		return true
	}
	rawData, _ := json.Marshal(resp.Error.Data)
	var data mcpToolError
	if err := json.Unmarshal(rawData, &data); err != nil {
		t.Fatalf("MCP incarnation: unmarshal error data: %v", err)
	}
	if data.Code != mcpCodeValidationFailed || !strings.Contains(resp.Error.Message, "outside operator trait-scope") {
		t.Fatalf("MCP incarnation: unexpected refusal (code=%q message=%q) — expected either success or the "+
			"trait-scope refusal; anything else would be counted as a 'no' and could match another surface "+
			"by accident", data.Code, resp.Error.Message)
	}
	return false
}

// --- fixtures ---

// traitWriteGateRBAC is [traitGateRBAC] widened to both permissions the four
// surfaces ask for. Each is granted twice over: once on the coven, which is
// gate (a) — the objects the operator may touch at all — and once per trait
// value, which is gate (b), the one under test.
//
// The two must stay in SEPARATE grants: rbac.traitsFromPurview only counts a
// disjunct that constrains trait ALONE, so `coven=X AND trait.k=v` would
// contribute no trait pair and every case would refuse for the wrong reason.
//
// Both permissions carry the same trait values on purpose. The pairs an operator
// holds are per (resource, action), so granting them apart would let the two
// halves of the table diverge and turn a real disagreement between the soul and
// incarnation surfaces into an expected one.
func traitWriteGateRBAC(key string, values []string) *rbactest.Config {
	perms := []string{
		"soul.traits-assign on coven=" + traitGateCoven,
		"incarnation.traits-set on coven=" + traitGateCoven,
	}
	for _, v := range values {
		perms = append(perms,
			fmt.Sprintf("soul.traits-assign on trait.%s=%q", key, v),
			fmt.Sprintf("incarnation.traits-set on trait.%s=%q", key, v))
	}
	return &rbactest.Config{
		Roles: []rbactest.Role{
			{Name: traitGateRole, Operators: []string{"archon-alice"}, Permissions: perms},
		},
	}
}

// seedTraitGateIncarnation creates the target row through incarnation.Create
// rather than raw SQL, so the seed cannot drift from what the production write
// path expects of a row. The declared coven is what gate (a) matches on.
func seedTraitGateIncarnation(t *testing.T, name string) {
	t.Helper()
	ctx := context.Background()
	inc := &incarnation.Incarnation{
		ID:                 name,
		Service:            traitGateService,
		ServiceVersion:     "v1.0.0",
		StateSchemaVersion: 1,
		State:              map[string]any{},
		Status:             incarnation.StatusReady,
		Covens:             []string{traitGateCoven},
		Traits:             map[string]any{},
	}
	if err := incarnation.Create(ctx, integrationPool, inc); err != nil {
		t.Fatalf("seed incarnation %s: %v", name, err)
	}
	t.Cleanup(func() {
		_, _ = integrationPool.Exec(ctx, `DELETE FROM incarnation WHERE id = $1`, name)
	})
}

func resetIncTraits(t *testing.T, name string) {
	t.Helper()
	if _, err := integrationPool.Exec(context.Background(),
		`UPDATE incarnation SET traits = '{}'::jsonb WHERE id = $1`, name); err != nil {
		t.Fatalf("reset traits on incarnation %s: %v", name, err)
	}
}

func readIncTraits(t *testing.T, name string) string {
	t.Helper()
	var out string
	if err := integrationPool.QueryRow(context.Background(),
		`SELECT traits::text FROM incarnation WHERE id = $1`, name).Scan(&out); err != nil {
		t.Fatalf("read traits of incarnation %s: %v", name, err)
	}
	return out
}
