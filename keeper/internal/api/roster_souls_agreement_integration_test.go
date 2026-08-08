//go:build integration

// Guard (NIM-401): the incarnation roster and the souls list are two READERS of
// one boundary — "which hosts may this operator see" — implemented twice. The
// list pushes the purview down into SQL ([rbac.PurviewSQL] over the `souls`
// columns); the roster evaluates it in Go ([soulpurview.InScope]) over the rows
// of `incarnation_membership`. Both resolve the SAME purview, ("soul", "list").
//
// The defect is not "one screen is wrong": read alone, either screen looks
// plausible — a short roster reads as a small incarnation, a long list reads as
// a big fleet. The defect is that the two DISAGREE, and it is only visible by
// putting them side by side. So the assertion here is set equality BETWEEN the
// readers, from ONE token in ONE run:
//
//	expected = {SIDs GET /v1/souls shows} ∩ {true members, read straight from PG}
//	actual   = {SIDs GET /v1/incarnations/{name}/members shows}
//
// Postgres is the neutral third party: `incarnation_membership` is the relation
// itself (NIM-124, migration 099), not a reader of the scope boundary, so a bug
// in either implementation cannot hide inside the expectation. Neither reader is
// compared against a hand-written list of hosts — that is exactly the shape that
// lets two readers drift while both their own tests stay green.
//
// Run:
//
//	cd keeper && SOUL_STACK_INTEGRATION_REQUIRE_DOCKER=1 \
//	    go test -tags=integration -count=1 -run RosterAgrees ./internal/api/

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac/rbactest"
	"github.com/souls-guild/soul-stack/keeper/internal/soul"
)

const (
	agreementInc    = "redis-prod"
	agreementIncDev = "redis-dev"
	agreementSeeder = "archon-agreement-seed"
)

// --- fixture ---------------------------------------------------------------

// agreementHost — one seeded host, described by every dimension a scope can
// address (NIM-128): its own covens, its own traits, its SID. Since NIM-281
// nothing is inherited, so what is written here is the whole truth about it.
type agreementHost struct {
	sid    string
	covens []string
	traits map[string]any
	member bool // bound to agreementInc
}

// agreementFleet — a park deliberately spread across the scope dimensions, so
// that a scoped operator covers PART of the roster and not all or none of it
// (the ticket's own acceptance).
func agreementFleet() []agreementHost {
	return []agreementHost{
		// Members.
		{sid: "db-a.example.com", covens: []string{"dba"}, traits: map[string]any{"tier": "gold"}, member: true},
		{sid: "db-b.example.com", covens: []string{"dba"}, traits: map[string]any{"tier": "silver"}, member: true},
		{sid: "web-a.example.com", covens: []string{"web"}, traits: map[string]any{"tier": "gold", "zone": []any{"eu", "us"}}, member: true},
		// A member wearing NO label at all. Belonging to an incarnation attaches
		// none (NIM-281), so no label-shaped scope reaches it — and the roster has
		// to be exactly as blind as the list, not one host more generous.
		{sid: "bare-a.example.com", member: true},
		// Members whose trait value is NOT a string. One fact, two renderings:
		// the list compares it as Postgres spells jsonb, the roster as Go spells
		// it, and only a live PG can say whether they still agree. Each of these
		// was an observed NIM-401 divergence, so each one earns its place:
		//   num-a  — a plain number Go used to render as "1e+06";
		//   exp-1  — the SAME number written `1e6`: jsonb normalizes it, so it
		//            must match `trait.asn=1000000` exactly like num-a;
		//   scale-1 — `1000000.0`: jsonb keeps the scale, so it must NOT match;
		//   wide-1 / frac-1 — outside float64 entirely, which is what forbids
		//            re-deriving the text from a decoded map;
		//   obj-1  — a nested object: NIM-522 made it reachable by nothing, neither
		//            its key nor its value nor its own text;
		//   arr-1  — a list of numbers: NIM-522 made each ELEMENT reachable, and
		//            the array's own text reachable by nothing;
		//   bool-1 — a bool and a list of bools, the other half of "any scalar
		//            element" — the old `?|` arm skipped these exactly like numbers;
		//   mix-1  — a list mixing scalars with a nested list and a nested object:
		//            the ONE shape where the SQL has to look at each element's own
		//            type, and where the two halves would part ways silently if the
		//            inner `jsonb_typeof` guard and Go's traitScalarText disagreed.
		{sid: "num-a.example.com", covens: []string{"num"}, traits: map[string]any{"asn": 1000000}, member: true},
		{sid: "exp-1.example.com", traits: map[string]any{"asn": json.Number("1e6")}, member: true},
		{sid: "scale-1.example.com", traits: map[string]any{"asn": json.Number("1000000.0")}, member: true},
		{sid: "wide-1.example.com", traits: map[string]any{"asn": json.Number("12345678901234567890")}, member: true},
		{sid: "frac-1.example.com", traits: map[string]any{"ratio": json.Number("0.0000001")}, member: true},
		{sid: "obj-1.example.com", traits: map[string]any{"tier": map[string]any{"k": "gold"}}, member: true},
		{sid: "arr-1.example.com", traits: map[string]any{"ports": []any{6379, 6380}}, member: true},
		{sid: "bool-1.example.com", traits: map[string]any{"enabled": true, "flags": []any{true, false}}, member: true},
		{sid: "mix-1.example.com", traits: map[string]any{"m": []any{"a", []any{"x"}, map[string]any{"k": "v"}, 42}}, member: true},

		// Non-members. db-c wears the members' labels — visibility must not imply
		// membership; impostor wears the incarnation's NAME as its own coven —
		// membership must not be inferred from a label (NIM-124).
		{sid: "db-c.example.com", covens: []string{"dba"}, traits: map[string]any{"tier": "gold"}},
		{sid: "impostor.example.com", covens: []string{agreementInc}},
	}
}

// seedAgreementFleet plants the park, the two incarnations and the membership
// rows. `redis-dev` exists so that a scope covering hosts of BOTH incarnations
// cannot pass by accident: its member must never surface in redis-prod's roster.
func seedAgreementFleet(t *testing.T) {
	t.Helper()
	truncateOperators(t)
	seedOperator(t, agreementSeeder, "")
	seedIncarnationFull(t, agreementInc, "redis", agreementSeeder, []string{"platform"}, map[string]any{})
	seedIncarnationFull(t, agreementIncDev, "redis", agreementSeeder, []string{"platform"}, map[string]any{})

	var members []string
	for _, h := range agreementFleet() {
		seedAgreementSoul(t, h.sid, h.covens, h.traits)
		if h.member {
			members = append(members, h.sid)
		}
	}
	seedAgreementSoul(t, "dev-a.example.com", []string{"dba"}, map[string]any{"tier": "gold"})

	by := agreementSeeder
	ctx := context.Background()
	if err := incarnation.AddMembers(ctx, integrationPool, agreementInc, members, &by); err != nil {
		t.Fatalf("AddMembers(%s): %v", agreementInc, err)
	}
	if err := incarnation.AddMembers(ctx, integrationPool, agreementIncDev, []string{"dev-a.example.com"}, &by); err != nil {
		t.Fatalf("AddMembers(%s): %v", agreementIncDev, err)
	}
}

// seedAgreementSoul inserts one host with its own covens AND traits —
// seedSoulFull carries no traits, and the trait dimension is half of what the
// two readers have to agree about.
func seedAgreementSoul(t *testing.T, sid string, covens []string, traits map[string]any) {
	t.Helper()
	c := agreementSeeder
	s := &soul.Soul{
		SID:          sid,
		Transport:    soul.Transport("agent"),
		Status:       soul.StatusConnected,
		Coven:        covens,
		Traits:       traits,
		CreatedByAID: &c,
	}
	if err := soul.Insert(context.Background(), integrationPool, s); err != nil {
		t.Fatalf("seedAgreementSoul(%s): %v", sid, err)
	}
}

// --- readers ---------------------------------------------------------------

type rosterPage struct {
	Items []struct {
		SID    string `json:"sid"`
		Status string `json:"status"`
	} `json:"items"`
	Limit  int `json:"limit"`
	Offset int `json:"offset"`
	Total  int `json:"total"`
}

// getRoster — GET /v1/incarnations/{name}/members. Returns the decoded page and
// the HTTP status; a non-200 yields the zero page (the caller decides whether
// that is the case under test).
func getRoster(t *testing.T, base, tok, name string) (rosterPage, int) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, base+"/v1/incarnations/"+name+"/members", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("getRoster(%s): Do: %v", name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return rosterPage{}, resp.StatusCode
	}
	var p rosterPage
	if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
		t.Fatalf("getRoster(%s): decode: %v", name, err)
	}
	return p, resp.StatusCode
}

// trueMembers reads the relation itself — the neutral third party. Not a reader
// of the scope boundary, so it cannot inherit either reader's bug.
func trueMembers(t *testing.T, name string) map[string]struct{} {
	t.Helper()
	rows, err := integrationPool.Query(context.Background(),
		`SELECT sid FROM incarnation_membership WHERE incarnation_name = $1`, name)
	if err != nil {
		t.Fatalf("trueMembers(%s): %v", name, err)
	}
	defer rows.Close()
	out := map[string]struct{}{}
	for rows.Next() {
		var sid string
		if err := rows.Scan(&sid); err != nil {
			t.Fatalf("trueMembers(%s): scan: %v", name, err)
		}
		out[sid] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("trueMembers(%s): iter: %v", name, err)
	}
	return out
}

// --- the guard -------------------------------------------------------------

// assertReadersAgree is THE assertion of NIM-401: with ONE token, the roster
// shows EXACTLY the members the souls list shows — not one host fewer, not one
// host more. Both failure modes named in the ticket get their own message,
// because they read completely differently to an operator.
//
// wantListed pins the list reader against the fixture's intent. It is not a
// substitute for the comparison — it is the anti-vacuum guard: two readers that
// both answer ∅ agree perfectly and prove nothing.
func assertReadersAgree(t *testing.T, base, tok, label string, wantListed []string) {
	t.Helper()

	// limit=2 < the visible set: the narrowing has to survive pagination, not
	// just decorate the first page.
	listed, _ := walkSouls(t, base, tok, "", 2)
	if want := setOf(wantListed); !sameSet(listed, want) {
		t.Errorf("[%s] GET /v1/souls returned %v, the fixture expects %v — the list reader itself moved; the comparison below is now against an unintended baseline",
			label, sortedSet(listed), sortedSet(want))
	}

	members := trueMembers(t, agreementInc)
	expected := map[string]struct{}{}
	for sid := range listed {
		if _, ok := members[sid]; ok {
			expected[sid] = struct{}{}
		}
	}

	page, code := getRoster(t, base, tok, agreementInc)
	if code != http.StatusOK {
		t.Fatalf("[%s] GET /v1/incarnations/%s/members: status=%d, want 200", label, agreementInc, code)
	}
	actual := map[string]struct{}{}
	for _, it := range page.Items {
		actual[it.SID] = struct{}{}
	}

	for sid := range expected {
		if _, ok := actual[sid]; !ok {
			t.Errorf("[%s] %s is visible in /v1/souls AND is a member of %s, but the roster hides it — the roster is NARROWER than the list: the operator sees a host that its own incarnation claims not to have (roster=%v, souls∩members=%v)",
				label, sid, agreementInc, sortedSet(actual), sortedSet(expected))
		}
	}
	for sid := range actual {
		if _, ok := expected[sid]; !ok {
			reason := "it is not visible in /v1/souls for this token"
			if _, isMember := members[sid]; !isMember {
				reason = "it is not a member of " + agreementInc + " at all"
			}
			t.Errorf("[%s] %s is in the roster but %s — the roster is WIDER than the list: a host reachable only through the roster (roster=%v, souls∩members=%v)",
				label, sid, reason, sortedSet(actual), sortedSet(expected))
		}
	}

	// The envelope must describe the same set the items do; an operator who
	// cannot tell "narrowed" from "small" gets the silent divergence the ticket
	// calls out by name.
	if page.Total != len(page.Items) {
		t.Errorf("[%s] roster total=%d but items=%d — the envelope disagrees with its own page",
			label, page.Total, len(page.Items))
	}
}

// --- cases -----------------------------------------------------------------

// agreementRBAC — one catalog for every case. `incarnation.get` is bare
// (unrestricted) on purpose: the roster route's own permission must not be the
// variable, the soul-axis scope is.
func agreementRBAC() *rbactest.Config {
	role := func(name, aid string, soulScopes ...string) rbactest.Role {
		perms := []string{"incarnation.get"}
		for _, s := range soulScopes {
			perms = append(perms, "soul.list on "+s)
		}
		return rbactest.Role{Name: name, Operators: []string{aid}, Permissions: perms}
	}
	return &rbactest.Config{Roles: []rbactest.Role{
		{Name: "cluster-admin", Operators: []string{"archon-agree-all"}, Permissions: []string{"*"}},
		role("agree-coven", "archon-agree-coven", "coven=dba"),
		role("agree-trait", "archon-agree-trait", "trait.tier=gold"),
		role("agree-trait-list", "archon-agree-trait-list", "trait.zone=eu"),
		role("agree-trait-num", "archon-agree-trait-num", "trait.asn=1000000"),
		role("agree-trait-wide", "archon-agree-trait-wide", "trait.asn=12345678901234567890"),
		role("agree-trait-frac", "archon-agree-trait-frac", "trait.ratio=0.0000001"),
		role("agree-trait-key", "archon-agree-trait-key", "trait.tier=k"),
		role("agree-trait-arrnum", "archon-agree-trait-arrnum", "trait.ports=6379"),
		role("agree-trait-arrtext", "archon-agree-trait-arrtext", `trait.ports="[6379, 6380]"`),
		role("agree-trait-bool", "archon-agree-trait-bool", "trait.enabled=true"),
		role("agree-trait-boolarr", "archon-agree-trait-boolarr", "trait.flags=false"),
		role("agree-trait-mixhit", "archon-agree-trait-mixhit", "trait.m=a"),
		role("agree-trait-mixnest", "archon-agree-trait-mixnest", "trait.m=x"),
		role("agree-glob", "archon-agree-glob", "host matches *-a.example.com"),
		role("agree-union", "archon-agree-union", "coven=dba", "host matches bare-*"),
		role("agree-nothing", "archon-agree-nothing", "coven=no-such-label"),
	}}
}

// TestIntegration_RosterAgreesWithSoulsList — the matrix. One server, one fleet,
// one scope shape per case; every case asks both readers the same question with
// the same token.
func TestIntegration_RosterAgreesWithSoulsList(t *testing.T) {
	seedAgreementFleet(t)

	base, stop := startServer(t, agreementRBAC())
	defer stop()

	cases := []struct {
		label  string
		aid    string
		role   string
		listed []string // what /v1/souls must show this operator (anti-vacuum)
	}{
		{
			label: "unrestricted", aid: "archon-agree-all", role: "cluster-admin",
			listed: []string{
				"db-a.example.com", "db-b.example.com", "web-a.example.com",
				"bare-a.example.com", "num-a.example.com",
				"exp-1.example.com", "scale-1.example.com", "wide-1.example.com",
				"frac-1.example.com", "obj-1.example.com", "arr-1.example.com",
				"bool-1.example.com", "mix-1.example.com",
				"db-c.example.com", "impostor.example.com", "dev-a.example.com",
			},
		},
		{
			// The ticket's own acceptance: a scope covering PART of the roster.
			label: "coven=dba", aid: "archon-agree-coven", role: "agree-coven",
			listed: []string{"db-a.example.com", "db-b.example.com", "db-c.example.com", "dev-a.example.com"},
		},
		{
			// obj-1 carries `tier: {"k": "gold"}` and is deliberately NOT here: a
			// scope value names a whole value, and an object is not one — neither
			// through the word inside it nor through its own text (NIM-522).
			label: "trait.tier=gold", aid: "archon-agree-trait", role: "agree-trait",
			listed: []string{"db-a.example.com", "web-a.example.com", "db-c.example.com", "dev-a.example.com"},
		},
		{
			// A trait whose value is a LIST: PG matches it element-wise.
			label: "trait.zone=eu (list-valued)", aid: "archon-agree-trait-list", role: "agree-trait-list",
			listed: []string{"web-a.example.com"},
		},
		{
			// A trait whose value is a NUMBER: one fact, two renderings. `1e6`
			// is the same jsonb number and must be listed; `1000000.0` is a
			// different one (jsonb keeps the scale) and must not.
			label: "trait.asn=1000000 (number-valued)", aid: "archon-agree-trait-num", role: "agree-trait-num",
			listed: []string{"num-a.example.com", "exp-1.example.com"},
		},
		{
			// Beyond float64: a reader that re-derives the text from a decoded
			// map cannot produce these digits at all.
			label: "trait.asn=12345678901234567890 (beyond float64)", aid: "archon-agree-trait-wide", role: "agree-trait-wide",
			listed: []string{"wide-1.example.com"},
		},
		{
			label: "trait.ratio=0.0000001 (small number)", aid: "archon-agree-trait-frac", role: "agree-trait-frac",
			listed: []string{"frac-1.example.com"},
		},
		{
			// NIM-522, the new "no": an object's KEY is not a value. The old `?|`
			// arm matched keys, so this scope handed obj-1 to anyone who could name
			// a key of its `tier` object, whatever that key mapped to. It now
			// reaches nothing — and the roster must not reach further.
			label: "trait.tier=k (object key — reaches nothing)", aid: "archon-agree-trait-key", role: "agree-trait-key",
			listed: nil,
		},
		{
			// NIM-522, the new "yes": a list element is a value whatever its JSON
			// type. `?|` skipped numbers, so an operator granted `trait.ports=6379`
			// was denied a host that plainly carries 6379. arr-1 IS a member, so a
			// roster answering from its own stringification would disagree here.
			label: "trait.ports=6379 (number in a list)", aid: "archon-agree-trait-arrnum", role: "agree-trait-arrnum",
			listed: []string{"arr-1.example.com"},
		},
		{
			// NIM-522, the other new "no": `->>` over an array yields `[6379, 6380]`,
			// and that text is a rendering of the container, not a value in it. It
			// was the ONLY way to address arr-1 before, and the only container text
			// a scope can even spell — a string list's `["prod", "stage"]` carries
			// `"`, which no scope value can hold.
			label: `trait.ports="[6379, 6380]" (array's own text — reaches nothing)`, aid: "archon-agree-trait-arrtext", role: "agree-trait-arrtext",
			listed: nil,
		},
		{
			// A bool scalar: unchanged by NIM-522, pinned because the rule now
			// speaks about "any scalar" and a rule is only as good as its cases.
			label: "trait.enabled=true (bool scalar)", aid: "archon-agree-trait-bool", role: "agree-trait-bool",
			listed: []string{"bool-1.example.com"},
		},
		{
			// The other half of "any scalar element": `?|` skipped bools exactly
			// like numbers, so this was a "no" for the same wrong reason.
			label: "trait.flags=false (bool in a list)", aid: "archon-agree-trait-boolarr", role: "agree-trait-boolarr",
			listed: []string{"bool-1.example.com"},
		},
		{
			// The nested-container guard, positive half: mix-1's list carries "a"
			// next to a nested list and a nested object. A scalar sibling is still
			// a value — an implementation that gave up on the whole list the moment
			// it met a container would hide this host from one reader only.
			label: "trait.m=a (scalar beside nested containers)", aid: "archon-agree-trait-mixhit", role: "agree-trait-mixhit",
			listed: []string{"mix-1.example.com"},
		},
		{
			// Negative half, and the reason the SQL tests each ELEMENT's type
			// rather than only the value's: `x` lives one level deeper than a
			// value, so it reaches nothing. This is the only case that separates
			// "walk the elements" from "walk everything underneath".
			label: "trait.m=x (inside a nested list — reaches nothing)", aid: "archon-agree-trait-mixnest", role: "agree-trait-mixnest",
			listed: nil,
		},
		{
			label: "host matches *-a.example.com", aid: "archon-agree-glob", role: "agree-glob",
			listed: []string{
				"db-a.example.com", "web-a.example.com", "bare-a.example.com",
				"num-a.example.com", "dev-a.example.com",
			},
		},
		{
			label: "coven=dba OR host matches bare-*", aid: "archon-agree-union", role: "agree-union",
			listed: []string{
				"db-a.example.com", "db-b.example.com", "db-c.example.com",
				"dev-a.example.com", "bare-a.example.com",
			},
		},
		{
			// Fail-closed: a scope that reaches nothing must leave the roster
			// empty too, rather than falling back to "show the whole roster".
			label: "coven=no-such-label", aid: "archon-agree-nothing", role: "agree-nothing",
			listed: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			tok := newValidTokenFor(t, tc.aid, []string{tc.role})
			assertReadersAgree(t, base, tok, tc.label, tc.listed)
		})
	}
}

// TestIntegration_RosterAgreesWithSoulsList_NoSoulRights — the boundary case
// the matrix cannot express: an operator holding `incarnation.get` and no
// soul.* at all. /v1/souls refuses it outright (403), so the roster's honest
// answer is the empty set. A roster that still lists hosts would be a way to
// enumerate the fleet without the right to list it.
func TestIntegration_RosterAgreesWithSoulsList_NoSoulRights(t *testing.T) {
	seedAgreementFleet(t)

	base, stop := startServer(t, &rbactest.Config{Roles: []rbactest.Role{
		{Name: "inc-only", Operators: []string{"archon-inc-only"}, Permissions: []string{"incarnation.get"}},
	}})
	defer stop()
	tok := newValidTokenFor(t, "archon-inc-only", []string{"inc-only"})

	if code := getReadStatus(t, base, tok, "/v1/souls"); code != http.StatusForbidden {
		t.Fatalf("GET /v1/souls without soul.list: status=%d, want 403 (the fixture no longer models 'no soul rights')", code)
	}

	page, code := getRoster(t, base, tok, agreementInc)
	if code != http.StatusOK {
		t.Fatalf("GET roster: status=%d, want 200", code)
	}
	if len(page.Items) != 0 {
		t.Errorf("roster returned %d hosts to an operator that /v1/souls refuses entirely: %v — the roster is a second way to enumerate the fleet",
			len(page.Items), page.Items)
	}
}

// --- set helpers -----------------------------------------------------------

func setOf(xs []string) map[string]struct{} {
	out := make(map[string]struct{}, len(xs))
	for _, x := range xs {
		out[x] = struct{}{}
	}
	return out
}

func sameSet(a, b map[string]struct{}) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if _, ok := b[k]; !ok {
			return false
		}
	}
	return true
}

func sortedSet(s map[string]struct{}) []string {
	out := make([]string, 0, len(s))
	for k := range s {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
