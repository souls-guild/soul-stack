//go:build integration

// NIM-529 — agreement guard for the trait WRITE gate across its two surfaces.
//
// One rule, two writers. Assigning a trait pair to a host GRANTS visibility of
// that host (a trait pair is an RBAC scope dimension, NIM-128/ADR-047), so both
// write surfaces gate the pairs being stamped against the operator's own
// trait-scope:
//
//	REST — POST /v1/souls/traits  → handlers.SoulHandler.AssignTraitsTyped
//	MCP  — keeper.soul.traits-assign tool
//
// The defect the ticket describes is not that either surface got the rule
// wrong; it is that each carried its OWN hand-written copy of the step that
// renders a pair as text. Two copies drift: one gets fixed, the other is
// forgotten, and the two surfaces then disagree about the same payload — the
// operator is refused through one door and admitted through the other. So the
// assertion below is REST against MCP, not each against a written-down
// expectation. A per-surface expectation would pass happily while both copies
// were wrong in the same direction, and would say nothing at all about drift.
//
// Postgres is the neutral third party. The pair a gate must reason about is the
// text that `traits ->> '<key>'` will yield from the row the write CREATES —
// that is the text the read side (rbac.PurviewSQL) later matches a scope value
// against. jsonb does not store the caller's numeric token, it re-canonicalizes
// it (`1e-7` is read back as `0.0000001`), so the only way to know that text is
// to ask Postgres. The oracle here does exactly that, in SQL, using `->>` and
// `jsonb_array_elements_text` directly. It is deliberately NOT a Go rendering of
// the expected text: a Go expectation would be a third copy of the very step
// under test and would agree with a wrong implementation.
//
// Live PG is required for the same reason: no fake reproduces jsonb's
// canonicalization, and the unit-level fakes say so where they stand in for it.
//
// Proven by mutation, in both directions: restoring the pre-fix renderer in the
// MCP copy alone reddens with "REST accepted=true, MCP accepted=false", and in
// the REST copy alone with the same line reversed. The cases that discriminate
// are integer-token and exponent-recanonicalized — the two where Go's rendering
// and the stored text part ways. Both pre-fix copies already walked list
// ELEMENTS, so the list cases do not catch that mutant; they pin the
// container/element rule itself, which nothing else states.
//
// Run:
//
//	cd keeper && SOUL_STACK_INTEGRATION_REQUIRE_DOCKER=1 \
//	    go test -tags=integration -race -count=1 ./internal/mcp/ \
//	    -run TestIntegration_TraitWriteGate

package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
	keeperjwt "github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac/rbactest"
)

const traitGateCoven = "nim529"

// traitGateCase — one payload, one operator trait-scope, one expected verdict.
//
// payload is JSON TEXT, not a Go map, so both surfaces receive what their own
// transport would hand them: MCP embeds the text in the tool arguments, REST
// decodes it into the map[string]any its typed entry point takes. A Go literal
// would let the two paths diverge before the gate ever ran.
type traitGateCase struct {
	name string
	// key — the single trait key in payload; the readback assertions are
	// written for one key, so a case carries exactly one.
	key string
	// payload — the `traits` object, verbatim JSON.
	//
	// NOT every number the read matrix uses can be borrowed here. REST decodes the
	// payload into map[string]any and re-marshals it, so a case only holds if that
	// float64 detour is lossless in Postgres' eyes: `1e+21` survives it,
	// `12345678901234567890` becomes `12345678901234567000` and the stored row
	// would no longer equal the oracle's canonicalization of the payload. Check a
	// candidate against a real jsonb before adding it — the failure looks like a
	// bug in the gate and is not one.
	payload string
	// granted — the trait values the operator's scope carries for key. Each
	// becomes its own `trait.<key>="<v>"` grant; the enforcer unions them.
	granted []string
	// wantAccept — the fixture's own claim about the verdict. Checked against
	// the Postgres oracle before the surfaces are compared, so a mis-stated
	// case fails loudly instead of quietly agreeing with the code.
	wantAccept bool
	// why — what this case would catch, in one line.
	why string
}

func traitGateCases() []traitGateCase {
	return []traitGateCase{
		{
			name: "string-granted", key: "env",
			payload: `{"env":"prod"}`, granted: []string{"prod"}, wantAccept: true,
			why: "the plain case: a string pair the operator holds goes through",
		},
		{
			name: "string-foreign", key: "env",
			payload: `{"env":"staging"}`, granted: []string{"prod"}, wantAccept: false,
			why: "the gate's reason to exist: stamping a pair outside the operator's scope hands the host to a foreign role",
		},
		{
			name: "integer-token", key: "asn",
			payload: `{"asn":1000000}`, granted: []string{"1000000"}, wantAccept: true,
			why: "fmt over the decoded float64 prints 1e+06, which no scope names — a pair the operator plainly holds gets refused",
		},
		{
			name: "go-spelling-not-honoured", key: "tier",
			payload: `{"tier":1000000}`, granted: []string{"1e+06"}, wantAccept: false,
			why: "integer-token's mirror, and the direction that LEAKS: `1e+06` is Go's spelling of this number and nothing Postgres ever stores, so a scope naming it must reach no row at all — under the old fmt-based gate it reached this one",
		},
		{
			name: "exponent-recanonicalized", key: "ratio",
			payload: `{"ratio":1e-7}`, granted: []string{"0.0000001"}, wantAccept: true,
			why: "jsonb stores 0.0000001; Go renders 1e-7 (marshal) or 1e-07 (fmt) — only asking Postgres reaches the stored text",
		},
		{
			name: "exponent-expanded", key: "asn",
			payload: `{"asn":1e+21}`, granted: []string{"1000000000000000000000"}, wantAccept: true,
			why: "the same re-canonicalization the other way — jsonb expands the exponent, Go keeps it; the doc on rbac.TraitPairTexts states this number as fact, so it is checked rather than asserted",
		},
		{
			name: "bool", key: "managed",
			payload: `{"managed":true}`, granted: []string{"true"}, wantAccept: true,
			why: "a bool renders as its `->>` text, not as Go's",
		},
		{
			name: "list-all-granted", key: "env",
			payload: `{"env":["prod","stage"]}`, granted: []string{"prod", "stage"}, wantAccept: true,
			why: "a list contributes its ELEMENTS, not its own text — demanding a grant for `[\"prod\", \"stage\"]` would refuse every legitimate list write",
		},
		{
			name: "list-one-foreign", key: "env",
			payload: `{"env":["prod","staging"]}`, granted: []string{"prod", "stage"}, wantAccept: false,
			why: "the gate is per ELEMENT: one out-of-scope element grants as much as a whole out-of-scope key would",
		},
		{
			name: "numeric-list", key: "ports",
			payload: `{"ports":[6379,6380]}`, granted: []string{"6379", "6380"}, wantAccept: true,
			why: "the container/element rule on a NUMERIC list — the payload whose stored text `[6379, 6380]` a quoted scope value can name, the read/write asymmetry NIM-522 decides",
		},
	}
}

func TestIntegration_TraitWriteGate_SurfacesAgree(t *testing.T) {
	for _, tc := range traitGateCases() {
		t.Run(tc.name, func(t *testing.T) {
			truncateOperators(t)
			seedOperator(t, "archon-alice", "")

			sid := "nim529-" + tc.name + ".test"
			seedTraitGateSoul(t, sid)

			// The oracle first: if Postgres and the fixture disagree, the case
			// is mis-stated and comparing the surfaces to it proves nothing.
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

			cfg := traitGateRBAC(tc.key, tc.granted)

			restAccepted := runTraitAssignREST(t, cfg, tc.payload)
			restStored := readTraits(t, sid)
			resetTraits(t, sid)

			mcpAccepted := runTraitAssignMCP(t, cfg, tc.payload)
			mcpStored := readTraits(t, sid)

			// (1) The two writers against EACH OTHER. This is the defect the
			// ticket describes: one copy fixed, one forgotten.
			if restAccepted != mcpAccepted {
				t.Errorf("[%s] the two write surfaces disagree on %q under scope trait.%s=%v: "+
					"REST accepted=%v, MCP accepted=%v — one copy of the pair-rendering step was changed "+
					"and the other was not, so the same operator is refused through one door and admitted "+
					"through the other (%s)",
					tc.name, tc.payload, tc.key, tc.granted, restAccepted, mcpAccepted, tc.why)
			}

			// (2) Both against Postgres' own `->>`. This is what makes the
			// claim "the gate speaks the text `->>` yields" a checked one
			// rather than an assertion in a comment.
			if restAccepted != oracleAccept {
				t.Errorf("[%s] REST accepted=%v, but `->>` over the payload yields the pairs %v and the "+
					"operator's scope carries %v (%s)",
					tc.name, restAccepted, oraclePairs, tc.granted, tc.why)
			}
			if mcpAccepted != oracleAccept {
				t.Errorf("[%s] MCP accepted=%v, but `->>` over the payload yields the pairs %v and the "+
					"operator's scope carries %v (%s)",
					tc.name, mcpAccepted, oraclePairs, tc.granted, tc.why)
			}

			// (3) The row that now exists is the row the gate reasoned about.
			// A gate that admits one payload while the write stores another
			// would pass (1) and (2) and still hand out a foreign pair.
			wantStored := "{}"
			if tc.wantAccept {
				wantStored = pgCanonicalJSONB(t, tc.payload)
			}
			if restStored != wantStored {
				t.Errorf("[%s] after the REST call souls.traits = %s, want %s — the gate and the write "+
					"disagree about what is being stored", tc.name, restStored, wantStored)
			}
			if mcpStored != wantStored {
				t.Errorf("[%s] after the MCP call souls.traits = %s, want %s — the gate and the write "+
					"disagree about what is being stored", tc.name, mcpStored, wantStored)
			}
		})
	}
}

// --- surfaces ---

// runTraitAssignREST drives POST /v1/souls/traits through the typed entry point
// the HTTP layer calls, with the live pool and a real enforcer. Returns whether
// the write was admitted; anything that is not the scope refusal is fatal, so a
// 500 cannot be mistaken for a "no" and quietly agree with the other surface.
func runTraitAssignREST(t *testing.T, cfg *rbactest.Config, payload string) bool {
	t.Helper()
	enf, err := rbactest.NewEnforcer(cfg)
	if err != nil {
		t.Fatalf("NewEnforcer: %v", err)
	}
	h := handlers.NewSoulHandler(integrationPool, enf, nil, nil)

	var traits map[string]any
	if err := json.Unmarshal([]byte(payload), &traits); err != nil {
		t.Fatalf("decode payload %q: %v", payload, err)
	}

	_, err = h.AssignTraitsTyped(context.Background(), &keeperjwt.Claims{Subject: "archon-alice"},
		handlers.SoulTraitsAssignInput{
			Mode:     "merge",
			Traits:   traits,
			Selector: handlers.SoulCovenAssignSelectorInput{Coven: traitGateCoven},
		}, false)
	if err == nil {
		return true
	}
	d, ok := handlers.AsProblemDetails(err)
	if !ok {
		t.Fatalf("REST: non-problem error: %T %v", err, err)
	}
	if d.Status != 422 || !strings.Contains(d.Detail, "outside operator trait-scope") {
		t.Fatalf("REST: unexpected refusal (status=%d type=%s detail=%q) — expected either success or the "+
			"trait-scope refusal; anything else would be counted as a 'no' and could match the other "+
			"surface by accident", d.Status, d.Type, d.Detail)
	}
	return false
}

// runTraitAssignMCP drives keeper.soul.traits-assign end-to-end over JSON-RPC
// against a server wired with the same pool and the same RBAC fixture. Same
// contract as the REST driver: only the scope refusal counts as "no".
func runTraitAssignMCP(t *testing.T, cfg *rbactest.Config, payload string) bool {
	t.Helper()
	base, stop := startMCPServer(t, cfg)
	defer stop()
	token := newToken(t, "archon-alice", []string{traitGateRole})

	resp := doRPC(t, base, token, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{
			"name": "keeper.soul.traits-assign",
			"arguments": json.RawMessage(fmt.Sprintf(
				`{"mode":"merge","traits":%s,"selector":{"coven":%q}}`, payload, traitGateCoven)),
		},
	})
	if resp.Error == nil {
		return true
	}
	rawData, _ := json.Marshal(resp.Error.Data)
	var data mcpToolError
	if err := json.Unmarshal(rawData, &data); err != nil {
		t.Fatalf("MCP: unmarshal error data: %v", err)
	}
	if data.Code != mcpCodeValidationFailed || !strings.Contains(resp.Error.Message, "outside operator trait-scope") {
		t.Fatalf("MCP: unexpected refusal (code=%q message=%q) — expected either success or the "+
			"trait-scope refusal; anything else would be counted as a 'no' and could match the other "+
			"surface by accident", data.Code, resp.Error.Message)
	}
	return false
}

// --- fixtures ---

const traitGateRole = "nim529-traits-op"

// traitGateRBAC grants soul.traits-assign twice over: once on the coven, which
// is gate (a) — the hosts the operator may touch at all — and once per trait
// value, which is gate (b), the one under test. The two must stay in SEPARATE
// grants: rbac.traitsFromPurview only counts a disjunct that constrains trait
// ALONE, so `coven=X AND trait.k=v` would contribute no trait pair and every
// case would refuse for the wrong reason.
func traitGateRBAC(key string, values []string) *rbactest.Config {
	perms := []string{"soul.traits-assign on coven=" + traitGateCoven}
	for _, v := range values {
		perms = append(perms, fmt.Sprintf("soul.traits-assign on trait.%s=%q", key, v))
	}
	return &rbactest.Config{
		Roles: []rbactest.Role{
			{Name: traitGateRole, Operators: []string{"archon-alice"}, Permissions: perms},
		},
	}
}

func seedTraitGateSoul(t *testing.T, sid string) {
	t.Helper()
	_, err := integrationPool.Exec(context.Background(),
		`INSERT INTO souls (sid, transport, status, coven, traits)
		 VALUES ($1, 'agent', 'pending', ARRAY[$2]::TEXT[], '{}'::jsonb)
		 ON CONFLICT (sid) DO UPDATE SET coven = EXCLUDED.coven, traits = '{}'::jsonb`,
		sid, traitGateCoven)
	if err != nil {
		t.Fatalf("seed soul %s: %v", sid, err)
	}
	t.Cleanup(func() {
		_, _ = integrationPool.Exec(context.Background(), `DELETE FROM souls WHERE sid = $1`, sid)
	})
}

func resetTraits(t *testing.T, sid string) {
	t.Helper()
	if _, err := integrationPool.Exec(context.Background(),
		`UPDATE souls SET traits = '{}'::jsonb WHERE sid = $1`, sid); err != nil {
		t.Fatalf("reset traits on %s: %v", sid, err)
	}
}

func readTraits(t *testing.T, sid string) string {
	t.Helper()
	var out string
	if err := integrationPool.QueryRow(context.Background(),
		`SELECT traits::text FROM souls WHERE sid = $1`, sid).Scan(&out); err != nil {
		t.Fatalf("read traits of %s: %v", sid, err)
	}
	return out
}

// --- the Postgres oracle ---

// pgTraitPairTexts asks Postgres which pairs a payload contributes for one key,
// using the same two operators the read side's SQL pushdown uses: `->>` for a
// scalar, and the element texts for an array. Written in SQL on purpose — a Go
// implementation of this would be another copy of the step under test.
func pgTraitPairTexts(t *testing.T, payload, key string) []string {
	t.Helper()
	var out []string
	err := integrationPool.QueryRow(context.Background(), `
SELECT CASE
         WHEN jsonb_typeof(v -> $2) = 'array'
           THEN ARRAY(SELECT jsonb_array_elements_text(v -> $2))
         ELSE ARRAY[v ->> $2]
       END
FROM (SELECT $1::jsonb AS v) s`, payload, key).Scan(&out)
	if err != nil {
		t.Fatalf("oracle for %q key %q: %v", payload, key, err)
	}
	return out
}

// pgCanonicalJSONB returns the payload as Postgres spells it after a jsonb
// round-trip — the exact text a stored row will show.
func pgCanonicalJSONB(t *testing.T, payload string) string {
	t.Helper()
	var out string
	if err := integrationPool.QueryRow(context.Background(),
		`SELECT ($1::jsonb)::text`, payload).Scan(&out); err != nil {
		t.Fatalf("canonicalize %q: %v", payload, err)
	}
	return out
}
