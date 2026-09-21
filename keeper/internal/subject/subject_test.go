package subject

import (
	"strings"
	"testing"
)

// sel* — constructors for the four legal shapes, so a test says which DIMENSION
// it is about instead of which fields happen to be set.
func selSID(sids ...string) Selector     { return Selector{SIDs: sids} }
func selCoven(covens ...string) Selector { return Selector{Covens: covens} }
func selInc(service, name string) Selector {
	return Selector{Service: service, Incarnation: name}
}
func selTrait(key, value string) Selector { return Selector{TraitKey: key, TraitValue: value} }

func TestDimension(t *testing.T) {
	cases := []struct {
		name string
		sel  Selector
		want Dimension
	}{
		{"empty", Selector{}, DimNone},
		{"sid", selSID("host-a"), DimSID},
		{"incarnation", selInc("redis", "redis-prod"), DimIncarnation},
		{"coven", selCoven("prod"), DimCoven},
		{"trait", selTrait("tier", "gold"), DimTrait},
		// A half-written pair claims its dimension rather than reporting DimNone.
		// Reporting "no dimension at all" would let it slip past Validate's
		// per-dimension switch and be stored as a rule that can never match.
		{"incarnation service only", Selector{Service: "redis"}, DimIncarnation},
		{"incarnation name only", Selector{Incarnation: "redis-prod"}, DimIncarnation},
		{"trait key only", Selector{TraitKey: "tier"}, DimTrait},
		{"trait value only", Selector{TraitValue: "gold"}, DimTrait},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.sel.Dimension(); got != c.want {
				t.Errorf("Dimension() = %q, want %q", got, c.want)
			}
		})
	}
}

// TestString pins the rendering an operator reads back in audit payloads and 422
// diagnostics — the one place the four dimensions become a single line.
func TestString(t *testing.T) {
	cases := []struct {
		sel  Selector
		want string
	}{
		{Selector{}, "<empty>"},
		{selSID("host-a"), "sid=host-a"},
		{selSID("host-a", "host-b"), "sid=host-a,host-b"},
		{selInc("redis", "redis-prod"), "incarnation=redis.redis-prod"},
		{selCoven("prod"), "coven=prod"},
		{selCoven("prod", "eu"), "coven=prod,eu"},
		{selTrait("tier", "gold"), "trait.tier=gold"},
	}
	for _, c := range cases {
		t.Run(c.want, func(t *testing.T) {
			if got := c.sel.String(); got != c.want {
				t.Errorf("String() = %q, want %q", got, c.want)
			}
		})
	}
}

// --- Matches: one dimension at a time ---

func TestMatches_SID(t *testing.T) {
	sel := selSID("host-a", "host-b")

	if !sel.Matches(Host{SID: "host-b"}) {
		t.Error("a listed SID should match")
	}
	// A rule naming hosts reads identity and nothing else: labels, membership and
	// traits are all irrelevant to this dimension.
	unlisted := Host{
		SID:    "host-c",
		Covens: []string{"prod"},
		Traits: map[string]any{"tier": "gold"},
		Member: []Incarnation{{Service: "redis", Name: "redis-prod"}},
	}
	if sel.Matches(unlisted) {
		t.Error("an unlisted SID must not match, however it is labelled")
	}
}

func TestMatches_Incarnation(t *testing.T) {
	sel := selInc("redis", "redis-prod")

	member := Host{SID: "host-a", Member: []Incarnation{{Service: "redis", Name: "redis-prod"}}}
	if !sel.Matches(member) {
		t.Error("a member of the incarnation should match")
	}

	// The name is unique only WITHIN its service, so the address is the pair: the
	// same name under another service is a different incarnation.
	otherService := Host{SID: "host-b", Member: []Incarnation{{Service: "valkey", Name: "redis-prod"}}}
	if sel.Matches(otherService) {
		t.Error("the same name under a different service must not match")
	}

	// A coven tag is something anyone with soul.coven-assign may attach, so a host
	// merely tagged with the incarnation's spelling is not a member of it — that
	// conflation was the escalation NIM-281 closed.
	lookalike := Host{SID: "host-c", Covens: []string{"redis-prod"}}
	if sel.Matches(lookalike) {
		t.Error("a coven spelled like the incarnation must not stand in for membership")
	}

	if sel.Matches(Host{SID: "host-d"}) {
		t.Error("a host on no roster must not match")
	}
}

func TestMatches_Coven(t *testing.T) {
	sel := selCoven("prod", "eu")

	if !sel.Matches(Host{SID: "host-a", Covens: []string{"eu", "db"}}) {
		t.Error("a rule naming several covens matches on any one of them")
	}
	if sel.Matches(Host{SID: "host-b", Covens: []string{"stage"}}) {
		t.Error("a disjoint label set must not match")
	}
	if sel.Matches(Host{SID: "host-c"}) {
		t.Error("an unlabelled host must not match")
	}
}

// TestMatches_CovenSecondLevel is the NIM-280 reversal itself: a coven attached
// to an INCARNATION reaches every host on its roster, even though the host
// carries no such label of its own.
//
// This is not the inheritance NIM-281 removed — nothing is written to `souls`,
// and no other consumer (an RBAC scope, `soulprint.self.covens`, the push
// provider choice) sees the label on the host. The union exists for the duration
// of one selector match.
func TestMatches_CovenSecondLevel(t *testing.T) {
	sel := selCoven("prod")

	member := Host{
		SID:    "host-a",
		Covens: nil, // the host itself is unlabelled
		Member: []Incarnation{{Service: "redis", Name: "redis-prod", Covens: []string{"prod"}}},
	}
	if !sel.Matches(member) {
		t.Error("a coven on the incarnation should reach its members")
	}

	other := Host{
		SID:    "host-b",
		Member: []Incarnation{{Service: "redis", Name: "redis-stage", Covens: []string{"stage"}}},
	}
	if sel.Matches(other) {
		t.Error("a member of an incarnation carrying a DIFFERENT label must not match")
	}

	// Belonging alone lends nothing: an unlabelled incarnation reaches no one.
	bare := Host{SID: "host-c", Member: []Incarnation{{Service: "redis", Name: "redis-prod"}}}
	if sel.Matches(bare) {
		t.Error("membership without a label must not match")
	}
}

func TestMatches_Trait(t *testing.T) {
	sel := selTrait("tier", "gold")

	if !sel.Matches(Host{SID: "host-a", Traits: map[string]any{"tier": "gold"}}) {
		t.Error("a host carrying the pair should match")
	}
	if sel.Matches(Host{SID: "host-b", Traits: map[string]any{"tier": "silver"}}) {
		t.Error("a different value under the same key must not match")
	}
	if sel.Matches(Host{SID: "host-c", Traits: map[string]any{"zone": "gold"}}) {
		t.Error("the same value under a different key must not match")
	}
	if sel.Matches(Host{SID: "host-d"}) {
		t.Error("a host with no traits must not match")
	}

	// Same two-level reach as coven.
	member := Host{
		SID:    "host-e",
		Member: []Incarnation{{Service: "redis", Name: "redis-prod", Traits: map[string]any{"tier": "gold"}}},
	}
	if !sel.Matches(member) {
		t.Error("a trait on the incarnation should reach its members")
	}
}

// TestMatches_TraitListMembership pins the list arm: a list trait matches on
// MEMBERSHIP, one host reachable through every item. Postgres answers the same
// way, which is what keeps the SQL prefilter and this matcher in step.
func TestMatches_TraitListMembership(t *testing.T) {
	h := Host{SID: "host-a", Traits: map[string]any{"role": []any{"primary", "seed"}}}

	for _, want := range []string{"primary", "seed"} {
		if !selTrait("role", want).Matches(h) {
			t.Errorf("trait.role=%s should reach a host whose list carries it", want)
		}
	}
	if selTrait("role", "replica").Matches(h) {
		t.Error("a value absent from the list must not match")
	}
	// The whole list is not a value: only its items are.
	if selTrait("role", "primary,seed").Matches(h) {
		t.Error("the rendered list must not match as a single value")
	}
}

// TestMatches_TraitScalarRendering pins the non-string arms against the one
// failure they are written for: json.Unmarshal decodes every JSON number into
// float64, so an integral value must render "3", not "3e+00" — the form
// Postgres `->>` produces. A drift here makes a rule match in Go and miss in
// SQL (or the reverse) for the same host.
func TestMatches_TraitScalarRendering(t *testing.T) {
	cases := []struct {
		name  string
		value any
		want  string
	}{
		{"integral float", float64(3), "3"},
		{"negative integral float", float64(-12), "-12"},
		{"fractional float", 1.5, "1.5"},
		{"true", true, "true"},
		{"false", false, "false"},
		{"string", "gold", "gold"},
		{"null", nil, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := scalarText(c.value); got != c.want {
				t.Fatalf("scalarText(%v) = %q, want %q", c.value, got, c.want)
			}
			h := Host{SID: "host-a", Traits: map[string]any{"k": c.value}}
			if !selTrait("k", c.want).Matches(h) {
				t.Errorf("trait.k=%q should reach a host whose value renders to it", c.want)
			}
		})
	}
}

// TestMatches_EmptySelectorFailSafe — the per-table CHECK makes an empty subject
// unstorable, so reaching this arm means a row was written around the
// constraint. Deny rather than let a rule with no subject bind to every host.
func TestMatches_EmptySelectorFailSafe(t *testing.T) {
	full := Host{
		SID:    "host-a",
		Covens: []string{"prod"},
		Traits: map[string]any{"tier": "gold"},
		Member: []Incarnation{{Service: "redis", Name: "redis-prod", Covens: []string{"eu"}}},
	}
	if (Selector{}).Matches(full) {
		t.Error("an empty selector must match nothing (default-deny)")
	}
}

// --- Validate ---

func TestValidate_ExactlyOneDimension(t *testing.T) {
	cases := []struct {
		name string
		sel  Selector
		ok   bool
	}{
		{"sid", selSID("host-a.example.com"), true},
		{"incarnation", selInc("redis", "redis-prod"), true},
		{"coven", selCoven("prod"), true},
		{"trait", selTrait("tier", "gold"), true},

		{"none", Selector{}, false},
		{"sid + coven", Selector{SIDs: []string{"host-a.example.com"}, Covens: []string{"prod"}}, false},
		{"coven + trait", Selector{Covens: []string{"prod"}, TraitKey: "tier", TraitValue: "gold"}, false},
		{"all four", Selector{
			SIDs: []string{"host-a.example.com"}, Service: "redis", Incarnation: "redis-prod",
			Covens: []string{"prod"}, TraitKey: "tier", TraitValue: "gold",
		}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := Validate(c.sel, "oracle: decree")
			if c.ok && err != nil {
				t.Fatalf("Validate() = %v, want nil", err)
			}
			if !c.ok {
				if err == nil {
					t.Fatal("Validate() = nil, want an exactly-one-of diagnostic")
				}
				if !strings.Contains(err.Error(), "oracle: decree") {
					t.Errorf("diagnostic %q does not name the registry", err)
				}
			}
		})
	}
}

// TestValidate_HalfWrittenPairs — an incarnation is service+name and a trait is
// key+value; half of either is the shape that would be stored happily and then
// silently never match anything.
func TestValidate_HalfWrittenPairs(t *testing.T) {
	cases := []struct {
		name string
		sel  Selector
	}{
		{"service without name", Selector{Service: "redis"}},
		{"name without service", Selector{Incarnation: "redis-prod"}},
		{"trait key without value", Selector{TraitKey: "tier"}},
		{"trait value without key", Selector{TraitValue: "gold"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := Validate(c.sel, "augur: rite"); err == nil {
				t.Error("Validate() = nil, want a both-halves diagnostic")
			}
		})
	}
}

func TestValidate_ElementForm(t *testing.T) {
	cases := []struct {
		name string
		sel  Selector
	}{
		{"bad sid", selSID("not a hostname")},
		{"bad service", selInc("Redis", "redis-prod")},
		{"bad incarnation", selInc("redis", "Redis-Prod")},
		{"bad coven", selCoven("PROD")},
		{"bad trait key", selTrait("Tier", "gold")},
		// One bad element in an otherwise valid list still fails: a partially
		// applied rule is worse than a rejected one.
		{"one bad coven among good", selCoven("prod", "EU")},
		{"one bad sid among good", selSID("host-a.example.com", "nope nope")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := Validate(c.sel, "oracle: vigil"); err == nil {
				t.Error("Validate() = nil, want a form diagnostic")
			}
		})
	}
}

func TestValidPatterns(t *testing.T) {
	if !ValidCoven("prod-eu-1") || ValidCoven("Prod") || ValidCoven("-prod") || ValidCoven("") {
		t.Error("ValidCoven accepts kebab-case starting alphanumeric, nothing else")
	}
	if !ValidService("redis") || ValidService("redis_cache") || ValidService(strings.Repeat("a", 64)) {
		t.Error("ValidService accepts kebab-case up to 63 chars")
	}
	if !ValidIncarnation("redis-prod") || ValidIncarnation("redis.prod") {
		t.Error("ValidIncarnation accepts kebab-case; the dot separates the pair, it is not part of a half")
	}
	if !ValidTraitKey("owner") || !ValidTraitKey("net.zone") || ValidTraitKey("1st") || ValidTraitKey("Owner") {
		t.Error("ValidTraitKey mirrors the RBAC scope trait key")
	}
}
