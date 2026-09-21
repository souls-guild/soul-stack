//go:build integration

// Guard (NIM-521): the incarnation LIST and the incarnation GET are two READERS
// of one boundary — "which incarnations may this operator see" — implemented
// twice. The list pushes the purview down into SQL ([rbac.PurviewSQL] over
// [incScopeColumns]); the get evaluates the SAME purview in Go
// ([IncarnationHandler.GetInScopeFor] → [rbac.Purview.Match]) over one row.
//
// The defect is not "one screen is wrong": read alone, either answer looks
// plausible — a 404 reads as "no such incarnation", a short list reads as a small
// fleet. The defect is that the two DISAGREE, and it is only visible by putting
// them side by side. So the assertion here is set equality BETWEEN the readers,
// from ONE token in ONE run:
//
//	expected = {names GET /v1/incarnations shows}
//	actual   = {names GET /v1/incarnations/{id} answers 200 for}
//
// Postgres is the neutral third party: `SELECT id FROM incarnation` is the
// registry itself, not a reader of the scope boundary, so the set of names the
// get reader is probed with cannot inherit either implementation's bug. Neither
// reader is compared against a hand-written expectation of who-sees-what — that
// is exactly the shape that lets two readers drift while both their own tests
// stay green.
//
// Why it needs a live PG. The divergence lives in the TRAIT dimension, and the
// trait dimension's answer is a property of jsonb: `->>` emits a number exactly
// as jsonb stores it (`1e6` reads back `1000000`, `1000000.0` keeps its scale),
// and a scope value reaches a WHOLE value: each SCALAR element of an array,
// never the array's own text and never a key of an object (NIM-522). A fake
// cannot be wrong about that on both ends at once, so only a real column can say
// whether the two halves still agree.
//
// Run:
//
//	cd keeper && SOUL_STACK_INTEGRATION_REQUIRE_DOCKER=1 \
//	    go test -tags=integration -count=1 -run IncarnationReadersAgree ./internal/api/

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac/rbactest"
)

const incAgreementSeeder = "archon-inc-agreement-seed"

// --- fixture ---------------------------------------------------------------

// incAgreementRow — one seeded incarnation, described by every dimension an
// incarnation scope can address (NIM-128): its covens, its service, its own name,
// its traits. There is no host dimension on this axis.
type incAgreementRow struct {
	name    string
	service string
	covens  []string
	traits  map[string]any
}

// incAgreementFleet — a park deliberately spread across the scope dimensions, so
// that a scoped operator covers PART of it and not all or none.
//
// The trait rows are the point of the fixture. Each one is a shape whose text is
// decided by jsonb and NOT recoverable from a decoded `map[string]any`, which is
// what the get reader used to project from:
//
//	num-a   — a plain integer Go used to render as "1e+06";
//	exp-b   — the SAME number written `1e6`: jsonb normalizes it, so it must be
//	          reachable by `trait.asn=1000000` exactly like num-a;
//	scale-c — `1000000.0`: jsonb KEEPS the scale, so it must NOT be;
//	wide-d / frac-e — outside float64 entirely, which is what forbids
//	          re-deriving the text from a decoded map at all;
//	obj-f   — a nested object, reachable by NOTHING since NIM-522: not by a key
//	          of it, not by a value in it, not by its own text;
//	arr-g   — a list of numbers, whose ELEMENTS became reachable with NIM-522 and
//	          whose own text `[6379, 6380]` stopped being;
//	flag-h  — a bool and a list of bools: the other half of "any scalar element",
//	          which the old `?|` arm skipped exactly like numbers;
//	mix-i   — a list mixing scalars with a nested list and a nested object, the
//	          one shape where the predicate has to read each ELEMENT's own type.
//
// obj-f and arr-g are seeded straight into the registry: `soul.ValidTraitValue`
// refuses a nested object on the write path today, so such a row can only arrive
// from a direct DB write or from before that validation existed. The readers must
// still agree about it — a row the API cannot create is still a row both of them
// read.
func incAgreementFleet() []incAgreementRow {
	return []incAgreementRow{
		{name: "gold-a", service: "redis", covens: []string{"dba"}, traits: map[string]any{"tier": "gold"}},
		{name: "silver-b", service: "redis", covens: []string{"dba"}, traits: map[string]any{"tier": "silver"}},
		{name: "zone-c", service: "redis", covens: []string{"web"}, traits: map[string]any{"tier": "gold", "zone": []any{"eu", "us"}}},
		// An incarnation wearing NO label at all: no scope shaped like a label
		// reaches it, and the get has to be exactly as blind as the list — not one
		// row more generous.
		{name: "bare-d", service: "redis"},
		{name: "flag-h", service: "redis", traits: map[string]any{"managed": true, "flags": []any{true, false}}},
		{name: "mix-i", service: "redis", traits: map[string]any{"m": []any{"a", []any{"x"}, map[string]any{"k": "v"}, 42}}},

		{name: "num-a", service: "postgres", covens: []string{"num"}, traits: map[string]any{"asn": 1000000}},
		{name: "exp-b", service: "postgres", traits: map[string]any{"asn": json.Number("1e6")}},
		{name: "scale-c", service: "postgres", traits: map[string]any{"asn": json.Number("1000000.0")}},
		{name: "wide-d", service: "postgres", traits: map[string]any{"asn": json.Number("12345678901234567890")}},
		{name: "frac-e", service: "postgres", traits: map[string]any{"ratio": json.Number("0.0000001")}},
		{name: "obj-f", service: "postgres", traits: map[string]any{"tier": map[string]any{"k": "gold"}}},
		{name: "arr-g", service: "postgres", traits: map[string]any{"ports": []any{6379, 6380}}},
	}
}

func seedIncAgreementFleet(t *testing.T) {
	t.Helper()
	truncateOperators(t)
	seedOperator(t, incAgreementSeeder, "")
	for _, r := range incAgreementFleet() {
		seedIncAgreementRow(t, r)
	}
}

// seedIncAgreementRow inserts one incarnation with its covens AND traits —
// seedIncarnationFull carries no traits, and the trait dimension is the half the
// two readers used to disagree about.
func seedIncAgreementRow(t *testing.T, r incAgreementRow) {
	t.Helper()
	c := incAgreementSeeder
	inc := &incarnation.Incarnation{
		ID:                 r.name,
		Service:            r.service,
		ServiceVersion:     "v1",
		StateSchemaVersion: 1,
		Status:             incarnation.StatusReady,
		Covens:             r.covens,
		Traits:             r.traits,
		CreatedByAID:       &c,
	}
	if err := incarnation.Create(context.Background(), integrationPool, inc); err != nil {
		t.Fatalf("seedIncAgreementRow(%s): %v", r.name, err)
	}
}

// --- readers ---------------------------------------------------------------

// walkIncarnationNames — the LIST reader: GET /v1/incarnations walked page by
// page, returning the names it showed. A small limit on purpose: the narrowing
// has to survive pagination, not just decorate the first page.
func walkIncarnationNames(t *testing.T, base, tok string, limit int) map[string]struct{} {
	t.Helper()
	seen := map[string]struct{}{}
	total := 0
	for page := 0; ; page++ {
		q := fmt.Sprintf("offset=%d&limit=%d", page*limit, limit)
		p := getIncarnationsPage(t, base, tok, q)
		total = p.Total
		for _, it := range p.Items {
			if _, dup := seen[it.ID]; dup {
				t.Fatalf("DUPLICATE %s during the offset walk of /v1/incarnations", it.ID)
			}
			seen[it.ID] = struct{}{}
		}
		if len(p.Items) < limit {
			break
		}
		if page > 50 {
			t.Fatal("incarnation-list HTTP walk does not converge (>50 pages)")
		}
	}
	if len(seen) != total {
		t.Errorf("walk collected %d names but the server reported total=%d — the total is not exact", len(seen), total)
	}
	return seen
}

// The tag is `id`, and it has to be: a wire tag that does not match what the
// handler emits decodes to the zero value SILENTLY — `encoding/json` ignores an
// unknown member unless asked not to. A stale `name` here does not fail the
// decode, it makes every item's identifier "" and the walk below then reports
// them as duplicates of each other, which reads like a pagination bug and is
// not one.
type incarnationsPage struct {
	Items []struct {
		ID string `json:"id"`
	} `json:"items"`
	Total int `json:"total"`
}

func getIncarnationsPage(t *testing.T, base, tok, query string) incarnationsPage {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, base+"/v1/incarnations?"+query, nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("getIncarnationsPage(%q): Do: %v", query, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("getIncarnationsPage(%q): status=%d, want 200", query, resp.StatusCode)
	}
	var p incarnationsPage
	if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
		t.Fatalf("getIncarnationsPage(%q): decode: %v", query, err)
	}
	return p
}

// gettableIncarnations — the GET reader, asked about every name the registry
// holds: 200 → in scope, 404 → out of scope. Any other status is a fixture
// problem, not an answer (403 would mean the operator holds no `incarnation.get`
// at all, and then the two readers are not being asked the same question).
func gettableIncarnations(t *testing.T, base, tok string, universe []string) map[string]struct{} {
	t.Helper()
	out := map[string]struct{}{}
	for _, name := range universe {
		switch code := getIncStatus(t, base, tok, name); code {
		case http.StatusOK:
			out[name] = struct{}{}
		case http.StatusNotFound:
		default:
			t.Fatalf("GET /v1/incarnations/%s: status=%d, want 200 or 404 — the fixture is not asking both readers the same question", name, code)
		}
	}
	return out
}

// allIncarnationNames reads the registry itself — the neutral third party. Not a
// reader of the scope boundary, so it cannot inherit either implementation's bug.
func allIncarnationNames(t *testing.T) []string {
	t.Helper()
	rows, err := integrationPool.Query(context.Background(), `SELECT id FROM incarnation ORDER BY id`)
	if err != nil {
		t.Fatalf("allIncarnationNames: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("allIncarnationNames: scan: %v", err)
		}
		out = append(out, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("allIncarnationNames: iter: %v", err)
	}
	return out
}

// --- the guard -------------------------------------------------------------

// assertIncReadersAgree is THE assertion of NIM-521: with ONE token, GET answers
// 200 for EXACTLY the incarnations the list showed — not one row fewer, not one
// row more. Both failure modes get their own message, because they read
// completely differently to an operator.
//
// wantListed pins the list reader against the fixture's intent. It is not a
// substitute for the comparison — it is the anti-vacuum guard: two readers that
// both answer ∅ agree perfectly and prove nothing.
func assertIncReadersAgree(t *testing.T, base, tok, label string, wantListed []string) {
	t.Helper()

	listed := walkIncarnationNames(t, base, tok, 2)
	if want := setOf(wantListed); !sameSet(listed, want) {
		t.Errorf("[%s] GET /v1/incarnations returned %v, the fixture expects %v — the list reader itself moved; the comparison below is now against an unintended baseline",
			label, sortedSet(listed), sortedSet(want))
	}

	gettable := gettableIncarnations(t, base, tok, allIncarnationNames(t))

	for name := range listed {
		if _, ok := gettable[name]; !ok {
			t.Errorf("[%s] %s is IN GET /v1/incarnations but GET /v1/incarnations/%s answers 404 — the get is NARROWER than the list: the operator is told a row it is looking at does not exist (list=%v, gettable=%v)",
				label, name, name, sortedSet(listed), sortedSet(gettable))
		}
	}
	for name := range gettable {
		if _, ok := listed[name]; !ok {
			t.Errorf("[%s] GET /v1/incarnations/%s answers 200 but the list never showed %s — the get is WIDER than the list: an incarnation reachable only by guessing its name (list=%v, gettable=%v)",
				label, name, name, sortedSet(listed), sortedSet(gettable))
		}
	}
}

// --- cases -----------------------------------------------------------------

// incAgreementRBAC — one catalog for every case. Both permissions of a role carry
// the SAME scope string by construction: the variable under test is the scope
// SHAPE, and a role whose list and get were scoped differently would compare two
// different questions.
func incAgreementRBAC() *rbactest.Config {
	role := func(name, aid, scope string) rbactest.Role {
		return rbactest.Role{Name: name, Operators: []string{aid}, Permissions: []string{
			"incarnation.list on " + scope,
			"incarnation.get on " + scope,
		}}
	}
	return &rbactest.Config{Roles: []rbactest.Role{
		{Name: "cluster-admin", Operators: []string{"archon-inc-all"}, Permissions: []string{"*"}},
		role("inc-coven", "archon-inc-coven", "coven=dba"),
		role("inc-service", "archon-inc-service", "service=redis"),
		role("inc-name", "archon-inc-name", "incarnation matches num-*"),
		role("inc-trait", "archon-inc-trait", "trait.tier=gold"),
		role("inc-trait-list", "archon-inc-trait-list", "trait.zone=eu"),
		role("inc-trait-num", "archon-inc-trait-num", "trait.asn=1000000"),
		role("inc-trait-scale", "archon-inc-trait-scale", "trait.asn=1000000.0"),
		role("inc-trait-wide", "archon-inc-trait-wide", "trait.asn=12345678901234567890"),
		role("inc-trait-frac", "archon-inc-trait-frac", "trait.ratio=0.0000001"),
		role("inc-trait-bool", "archon-inc-trait-bool", "trait.managed=true"),
		role("inc-trait-key", "archon-inc-trait-key", "trait.tier=k"),
		role("inc-trait-arrnum", "archon-inc-trait-arrnum", "trait.ports=6379"),
		role("inc-trait-arrtext", "archon-inc-trait-arrtext", `trait.ports="[6379, 6380]"`),
		role("inc-trait-boolarr", "archon-inc-trait-boolarr", "trait.flags=false"),
		role("inc-trait-mixhit", "archon-inc-trait-mixhit", "trait.m=a"),
		role("inc-trait-mixnest", "archon-inc-trait-mixnest", "trait.m=x"),
		role("inc-union", "archon-inc-union", "coven=dba OR trait.zone=eu"),
		role("inc-nothing", "archon-inc-nothing", "coven=no-such-label"),
	}}
}

// TestIntegration_IncarnationReadersAgree — the matrix. One server, one fleet, one
// scope shape per case; every case asks both readers the same question with the
// same token.
func TestIntegration_IncarnationReadersAgree(t *testing.T) {
	seedIncAgreementFleet(t)

	base, stop := startServer(t, incAgreementRBAC())
	defer stop()

	cases := []struct {
		label  string
		aid    string
		role   string
		listed []string // what the LIST must show this operator (anti-vacuum)
	}{
		{
			label: "unrestricted", aid: "archon-inc-all", role: "cluster-admin",
			listed: []string{
				"gold-a", "silver-b", "zone-c", "bare-d", "flag-h", "mix-i",
				"num-a", "exp-b", "scale-c", "wide-d", "frac-e", "obj-f", "arr-g",
			},
		},
		{
			label: "coven=dba", aid: "archon-inc-coven", role: "inc-coven",
			listed: []string{"gold-a", "silver-b"},
		},
		{
			label: "service=redis", aid: "archon-inc-service", role: "inc-service",
			listed: []string{"gold-a", "silver-b", "zone-c", "bare-d", "flag-h", "mix-i"},
		},
		{
			label: "incarnation matches num-*", aid: "archon-inc-name", role: "inc-name",
			listed: []string{"num-a"},
		},
		{
			// A string trait — and the negative that matters: obj-f carries
			// `{"tier": {"k": "gold"}}`, and an object is not a value, so neither
			// the word "gold" inside it nor its own text reaches it (NIM-522).
			label: "trait.tier=gold", aid: "archon-inc-trait", role: "inc-trait",
			listed: []string{"gold-a", "zone-c"},
		},
		{
			// A trait whose value is a LIST: matched element-wise.
			label: "trait.zone=eu (list-valued)", aid: "archon-inc-trait-list", role: "inc-trait-list",
			listed: []string{"zone-c"},
		},
		{
			// One fact, two renderings. `1e6` is the same jsonb number and must be
			// listed; `1000000.0` is a different one (jsonb keeps the scale).
			label: "trait.asn=1000000 (number-valued)", aid: "archon-inc-trait-num", role: "inc-trait-num",
			listed: []string{"num-a", "exp-b"},
		},
		{
			// The mirror: only the row whose token carries the scale.
			label: "trait.asn=1000000.0 (scale kept)", aid: "archon-inc-trait-scale", role: "inc-trait-scale",
			listed: []string{"scale-c"},
		},
		{
			// Beyond float64: a reader that re-derives the text from a decoded map
			// cannot produce these digits at all.
			label: "trait.asn=12345678901234567890 (beyond float64)", aid: "archon-inc-trait-wide", role: "inc-trait-wide",
			listed: []string{"wide-d"},
		},
		{
			label: "trait.ratio=0.0000001 (small number)", aid: "archon-inc-trait-frac", role: "inc-trait-frac",
			listed: []string{"frac-e"},
		},
		{
			label: "trait.managed=true (bool)", aid: "archon-inc-trait-bool", role: "inc-trait-bool",
			listed: []string{"flag-h"},
		},
		{
			// NIM-522, the new "no": an object's KEY is not a value. The old `?|`
			// arm matched keys, so this scope handed obj-f to anyone who could name
			// a key of its `tier` object. It now reaches nothing — and the get must
			// not reach further.
			label: "trait.tier=k (object key — reaches nothing)", aid: "archon-inc-trait-key", role: "inc-trait-key",
			listed: nil,
		},
		{
			// NIM-522, the new "yes": a list element is a value whatever its JSON
			// type. arr-g EXISTS, so a get answering from its own stringification
			// would disagree with the list here.
			label: "trait.ports=6379 (number in a list)", aid: "archon-inc-trait-arrnum", role: "inc-trait-arrnum",
			listed: []string{"arr-g"},
		},
		{
			// NIM-522, the other new "no": `->>` over an array yields `[6379, 6380]`,
			// which renders the container rather than naming a value in it. It was
			// the ONLY way to address arr-g before.
			label: `trait.ports="[6379, 6380]" (array's own text — reaches nothing)`, aid: "archon-inc-trait-arrtext", role: "inc-trait-arrtext",
			listed: nil,
		},
		{
			// The other half of "any scalar element": a bool inside a list. `?|`
			// skipped these exactly as it skipped numbers.
			label: "trait.flags=false (bool in a list)", aid: "archon-inc-trait-boolarr", role: "inc-trait-boolarr",
			listed: []string{"flag-h"},
		},
		{
			// The nested-container guard, positive half: mix-i's list carries "a"
			// next to a nested list and a nested object. A scalar sibling is still
			// a value — an implementation that gave up on the whole list the moment
			// it met a container would hide this row from one reader only.
			label: "trait.m=a (scalar beside nested containers)", aid: "archon-inc-trait-mixhit", role: "inc-trait-mixhit",
			listed: []string{"mix-i"},
		},
		{
			// Negative half, and the reason each ELEMENT's type is tested rather
			// than only the value's: `x` lives one level deeper than a value, so it
			// reaches nothing. This is the only case separating "walk the elements"
			// from "walk everything underneath".
			label: "trait.m=x (inside a nested list — reaches nothing)", aid: "archon-inc-trait-mixnest", role: "inc-trait-mixnest",
			listed: nil,
		},
		{
			label: "coven=dba OR trait.zone=eu", aid: "archon-inc-union", role: "inc-union",
			listed: []string{"gold-a", "silver-b", "zone-c"},
		},
		{
			// Fail-closed: a scope that reaches nothing must leave the get answering
			// 404 everywhere too, rather than falling back to "show it all".
			label: "coven=no-such-label", aid: "archon-inc-nothing", role: "inc-nothing",
			listed: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			tok := newValidTokenFor(t, tc.aid, []string{tc.role})
			assertIncReadersAgree(t, base, tok, tc.label, tc.listed)
		})
	}
}
