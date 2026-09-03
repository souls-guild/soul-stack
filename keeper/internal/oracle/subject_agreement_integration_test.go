//go:build integration

// The subject model on live PG: the SQL prefilter and the Go matcher must give
// the same answer for every (host, rule) pair (NIM-280).
//
// Why this test and not more unit tests. A rule's subject is evaluated TWICE on
// the way to a host — once as a WHERE clause narrowing the registry
// (subject.MatchSQL, running inside Postgres) and once in Go, when the survivors
// are matched against the resolved host (subject.Selector.Matches). The two are
// written in different languages against different data shapes, and a
// disagreement between them is invisible: the SQL side simply returns fewer rows
// and the Go side never learns there was a row to consider. Nothing errors, no
// count is off, a rule just stops firing.
//
// The design that makes agreement possible is that the predicate consumes the
// ALREADY-RESOLVED host (subject.Host.Args) rather than joining to the registries
// itself, so both sides read the same facts. This test is what holds that
// property in place: it resolves each host once with subject.LoadHost and then
// asks both implementations about every rule.
//
// Live PG rather than a fake, because the halves being compared are `text[] &&
// text[]`, `unnest` over parallel arrays and jsonb `->>` on one side, and Go
// slice/map lookups on the other. A fake would have to reimplement the SQL to
// disagree with it, at which point it is testing itself.

package oracle

import (
	"context"
	"slices"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	"github.com/souls-guild/soul-stack/keeper/internal/soul"
	"github.com/souls-guild/soul-stack/keeper/internal/subject"
)

const (
	agreeAID = "archon-test"
	agreeSvc = "redis"
	agreeInc = "redis-prod"
)

// seedAgreementFleet builds the smallest fleet that can tell every dimension
// apart, and — more to the point — can tell each dimension apart from the way it
// used to be spelled:
//
//	member    — on the roster of redis.redis-prod, carrying NO label of its own.
//	            Everything it matches, it matches through the second level.
//	tagged    — carries the incarnation's NAME as an ordinary host tag and is on
//	            no roster. Under NIM-281 a name is not a label and a tag is not a
//	            membership, so it must match the `coven` rule spelled after the
//	            name and NOT the `incarnation` rule.
//	labelled  — carries `prod` and `tier=gold` on itself, no membership at all.
//	stranger  — nothing. The default-deny control.
//
// The incarnation carries `prod` and `tier=gold` too, so `member` and `labelled`
// are reached by the same rules through opposite routes — a resolver that lost
// either level shows up as one of them going quiet.
func seedAgreementFleet(t *testing.T, ctx context.Context) {
	t.Helper()
	resetAll(t)
	seedOperator(t, agreeAID)
	by := agreeAID

	hosts := []*soul.Soul{
		{SID: "member.example.com", Coven: nil},
		{SID: "tagged.example.com", Coven: []string{agreeInc}},
		{SID: "labelled.example.com", Coven: []string{"prod"}, Traits: map[string]any{"tier": "gold"}},
		{SID: "stranger.example.com", Coven: []string{"db"}},
	}
	for _, h := range hosts {
		h.Transport, h.Status, h.CreatedByAID = soul.TransportAgent, soul.StatusConnected, &by
		if err := soul.Insert(ctx, integrationPool, h); err != nil {
			t.Fatalf("soul.Insert(%s): %v", h.SID, err)
		}
	}

	if err := incarnation.Create(ctx, integrationPool, &incarnation.Incarnation{
		ID: agreeInc, Service: agreeSvc, ServiceVersion: "v1",
		StateSchemaVersion: 1, State: map[string]any{}, Status: incarnation.StatusReady,
		Covens: []string{"prod"}, Traits: map[string]any{"tier": "gold"},
		CreatedByAID: &by,
	}); err != nil {
		t.Fatalf("incarnation.Create: %v", err)
	}
	if err := incarnation.AddMembers(ctx, integrationPool, agreeInc, []string{"member.example.com"}, &by); err != nil {
		t.Fatalf("AddMembers: %v", err)
	}
}

// agreementVigils — one enabled Vigil per subject spelling under test.
func agreementVigils(t *testing.T) map[string]subject.Selector {
	t.Helper()
	sels := map[string]subject.Selector{
		"by-sid":         {SIDs: []string{"member.example.com", "stranger.example.com"}},
		"by-incarnation": {Service: agreeSvc, Incarnation: agreeInc},
		// Spelled after the incarnation's name: reaches whoever an operator TAGGED
		// with that string, and nobody else — the incarnation's name is not one of
		// its labels.
		"by-coven-name":  {Covens: []string{agreeInc}},
		"by-coven-label": {Covens: []string{"prod"}},
		"by-trait":       {TraitKey: "tier", TraitValue: "gold"},
		// Reaches nobody: the fail-closed control that keeps a green run from being
		// explained by a predicate that is simply true.
		"by-absent-coven": {Covens: []string{"nowhere"}},
	}
	aid := agreeAID
	for name, sel := range sels {
		v := &Vigil{
			ID: name, IntervalSpec: "30s", CheckAddr: "core.beacon.service_down",
			Enabled: true, CreatedByAID: &aid,
		}
		v.setSubject(sel)
		mustInsertVigil(t, v)
	}
	return sels
}

// TestIntegration_SubjectSQLAndGoAgree — the headline. For every (host, rule)
// pair: the SQL prefilter selected it iff the Go matcher accepts it.
func TestIntegration_SubjectSQLAndGoAgree(t *testing.T) {
	ctx := context.Background()
	seedAgreementFleet(t, ctx)
	sels := agreementVigils(t)

	for _, sid := range []string{
		"member.example.com", "tagged.example.com", "labelled.example.com", "stranger.example.com",
	} {
		t.Run(sid, func(t *testing.T) {
			host, err := subject.LoadHost(ctx, integrationPool, sid)
			if err != nil {
				t.Fatalf("LoadHost(%s): %v", sid, err)
			}

			got, err := SelectActiveVigilsForSubject(ctx, integrationPool, host)
			if err != nil {
				t.Fatalf("SelectActiveVigilsForSubject: %v", err)
			}
			selected := map[string]bool{}
			for _, v := range got {
				selected[v.ID] = true
			}

			for name, sel := range sels {
				sqlSaidYes, goSaidYes := selected[name], sel.Matches(host)
				if sqlSaidYes != goSaidYes {
					t.Errorf("%s vs %s: SQL=%v Go=%v — the prefilter and the matcher disagree; "+
						"host=%+v selector=%s", sid, name, sqlSaidYes, goSaidYes, host, sel)
				}
			}
		})
	}
}

// TestIntegration_SubjectDimensionsReachTheRightHosts — the agreement test above
// is satisfied by two implementations that are wrong in the same way, so this one
// states the expected answer independently, host by host.
func TestIntegration_SubjectDimensionsReachTheRightHosts(t *testing.T) {
	ctx := context.Background()
	seedAgreementFleet(t, ctx)
	agreementVigils(t)

	// The full truth table. Read the `member` row as the NIM-280 feature (own
	// labels: none, every match arrives through the incarnation) and the `tagged`
	// row as the NIM-281 boundary (a tag spelled like an incarnation's name buys
	// membership nothing).
	want := map[string][]string{
		"member.example.com":   {"by-sid", "by-incarnation", "by-coven-label", "by-trait"},
		"tagged.example.com":   {"by-coven-name"},
		"labelled.example.com": {"by-coven-label", "by-trait"},
		"stranger.example.com": {"by-sid"},
	}
	for sid, expected := range want {
		t.Run(sid, func(t *testing.T) {
			got := keysOf(selectedVigilNames(t, ctx, sid))
			slices.Sort(got)
			slices.Sort(expected)
			if !slices.Equal(got, expected) {
				t.Errorf("%s reached by %v, want %v", sid, got, expected)
			}
		})
	}
}

// TestIntegration_SubjectSecondLevelFollowsTheRelation — unbinding the member
// takes its incarnation-derived matches away on the next resolve, and leaves the
// host's own labels (it has none) exactly where they were.
//
// This is the difference between the union NIM-280 added and the inheritance
// NIM-281 removed, stated as an observation rather than as prose: nothing was
// written to `souls`, so nothing has to be unwritten.
func TestIntegration_SubjectSecondLevelFollowsTheRelation(t *testing.T) {
	ctx := context.Background()
	seedAgreementFleet(t, ctx)
	agreementVigils(t)
	const sid = "member.example.com"

	before := selectedVigilNames(t, ctx, sid)
	for _, name := range []string{"by-incarnation", "by-coven-label", "by-trait"} {
		if !before[name] {
			t.Fatalf("precondition: %q must reach the bound member, got %v", name, keysOf(before))
		}
	}

	if _, err := incarnation.RemoveMember(ctx, integrationPool, agreeInc, sid); err != nil {
		t.Fatalf("RemoveMember: %v", err)
	}

	after := selectedVigilNames(t, ctx, sid)
	for _, name := range []string{"by-incarnation", "by-coven-label", "by-trait"} {
		if after[name] {
			t.Errorf("%q still reaches an unbound host — the second level must follow the relation", name)
		}
	}
	// It keeps what it always had on its own: its SID.
	if !after["by-sid"] {
		t.Error("unbinding took away a match that never came from the incarnation")
	}

	var own []string
	if err := integrationPool.QueryRow(ctx, `SELECT coven FROM souls WHERE sid = $1`, sid).Scan(&own); err != nil {
		t.Fatalf("read souls.coven: %v", err)
	}
	if len(own) != 0 {
		t.Errorf("souls.coven = %v, want empty — matching must never write a label onto the host", own)
	}
}

func selectedVigilNames(t *testing.T, ctx context.Context, sid string) map[string]bool {
	t.Helper()
	host, err := subject.LoadHost(ctx, integrationPool, sid)
	if err != nil {
		t.Fatalf("LoadHost(%s): %v", sid, err)
	}
	got, err := SelectActiveVigilsForSubject(ctx, integrationPool, host)
	if err != nil {
		t.Fatalf("SelectActiveVigilsForSubject(%s): %v", sid, err)
	}
	names := map[string]bool{}
	for _, v := range got {
		names[v.ID] = true
	}
	return names
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
