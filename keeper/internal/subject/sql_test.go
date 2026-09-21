package subject

import (
	"slices"
	"strings"
	"testing"
)

// TestHostArgs_FoldsBothLevels — [Host.Args] is where the two-level union is
// made explicit for SQL. The predicate then compares against the SAME resolved
// host the Go matcher does, which is what keeps a prefilter from drifting from
// the rule it implements.
func TestHostArgs_FoldsBothLevels(t *testing.T) {
	h := Host{
		SID:    "host-a.example.com",
		Covens: []string{"db"},
		Traits: map[string]any{"tier": "silver"},
		Member: []Incarnation{
			{Service: "redis", Name: "redis-prod", Covens: []string{"prod"}, Traits: map[string]any{"zone": "eu"}},
			{Service: "vault", Name: "vault-main", Covens: []string{"prod", "eu"}},
		},
	}
	a := h.Args()

	if a.SID != "host-a.example.com" {
		t.Errorf("SID = %q", a.SID)
	}
	// The host's own label AND both incarnations' — deduplicated ("prod" appears
	// twice) and sorted, so a query plan and a test assertion do not depend on
	// roster order.
	if want := []string{"db", "eu", "prod"}; !slices.Equal(a.Covens, want) {
		t.Errorf("Covens = %v, want %v", a.Covens, want)
	}
	if want := []string{"tier=silver", "zone=eu"}; !slices.Equal(a.TraitPairs, want) {
		t.Errorf("TraitPairs = %v, want %v", a.TraitPairs, want)
	}
	// Services and names are PARALLEL arrays: unnest() zips them back into pairs,
	// so position i of one belongs with position i of the other. A dedupe or an
	// independent sort here would pair `redis` with `vault-main`.
	if want := []string{"redis", "vault"}; !slices.Equal(a.IncServices, want) {
		t.Errorf("IncServices = %v, want %v", a.IncServices, want)
	}
	if want := []string{"redis-prod", "vault-main"}; !slices.Equal(a.IncNames, want) {
		t.Errorf("IncNames = %v, want %v", a.IncNames, want)
	}
}

// TestHostArgs_TraitListExpandsToOnePairPerItem mirrors [traitHolds]'s list arm:
// `{"role": ["primary","seed"]}` must be reachable through either item, so it
// flattens into two pairs rather than one rendering of the list.
func TestHostArgs_TraitListExpandsToOnePairPerItem(t *testing.T) {
	h := Host{SID: "host-a", Traits: map[string]any{"role": []any{"seed", "primary"}, "n": float64(3)}}

	// Integral numbers render the way Postgres `->>` renders them ("3", not
	// "3e+00") — the same rule scalarText applies on the Go side.
	if want := []string{"n=3", "role=primary", "role=seed"}; !slices.Equal(h.Args().TraitPairs, want) {
		t.Errorf("TraitPairs = %v, want %v", h.Args().TraitPairs, want)
	}
}

func TestHostArgs_EmptyHostYieldsNilSets(t *testing.T) {
	a := Host{SID: "host-a"}.Args()
	// nil rather than []string{} — a bare host binds an empty text[] and every
	// arm of the predicate stays false, which is the default-deny the fail-safe
	// in [Selector.Matches] states in Go.
	if a.Covens != nil || a.TraitPairs != nil || a.IncServices != nil || a.IncNames != nil {
		t.Errorf("bare host args = %+v, want nil sets", a)
	}
}

// TestMatchSQL_BindOrderAndPlaceholders pins the contract every caller's args
// assertion depends on: five binds, in this order, spliced through the caller's
// own numbering.
func TestMatchSQL_BindOrderAndPlaceholders(t *testing.T) {
	h := Host{
		SID:    "host-a",
		Covens: []string{"db"},
		Traits: map[string]any{"tier": "gold"},
		Member: []Incarnation{{Service: "redis", Name: "redis-prod"}},
	}
	b := &ArgBinder{}
	sql := MatchSQL(DecreeColumns, h.Args(), b.Bind)

	if len(b.Args) != 5 {
		t.Fatalf("bound %d args, want 5", len(b.Args))
	}
	if got, ok := b.Args[0].(string); !ok || got != "host-a" {
		t.Errorf("args[0] = %v, want the SID", b.Args[0])
	}
	if got, ok := b.Args[1].([]string); !ok || !slices.Equal(got, []string{"db"}) {
		t.Errorf("args[1] = %v, want the covens", b.Args[1])
	}
	if got, ok := b.Args[2].([]string); !ok || !slices.Equal(got, []string{"redis"}) {
		t.Errorf("args[2] = %v, want the incarnation services", b.Args[2])
	}
	if got, ok := b.Args[3].([]string); !ok || !slices.Equal(got, []string{"redis-prod"}) {
		t.Errorf("args[3] = %v, want the incarnation names", b.Args[3])
	}
	if got, ok := b.Args[4].([]string); !ok || !slices.Equal(got, []string{"tier=gold"}) {
		t.Errorf("args[4] = %v, want the trait pairs", b.Args[4])
	}

	for _, ph := range []string{"$1", "$2", "$3", "$4", "$5"} {
		if !strings.Contains(sql, ph) {
			t.Errorf("predicate does not use %s:\n%s", ph, sql)
		}
	}
}

// TestMatchSQL_UsesEachRegistrysOwnColumns — the three registries spell the
// columns differently (`decrees` prefixes them, `vigils` and `rites` do not) but
// share one predicate; a rename has exactly one place to be made.
func TestMatchSQL_UsesEachRegistrysOwnColumns(t *testing.T) {
	cases := []struct {
		name string
		cols SQLColumns
		want []string
	}{
		{"vigils", VigilColumns, []string{"sid", "coven", "service", "incarnation", "trait_key", "trait_value"}},
		{"rites", RiteColumns, []string{"sid", "coven", "service", "incarnation", "trait_key", "trait_value"}},
		{"decrees", DecreeColumns, []string{
			"subject_sid", "subject_coven", "subject_service", "subject_incarnation",
			"subject_trait_key", "subject_trait_value",
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sql := MatchSQL(c.cols, Host{SID: "host-a"}.Args(), (&ArgBinder{}).Bind)
			for _, col := range c.want {
				if !strings.Contains(sql, col) {
					t.Errorf("predicate omits column %q:\n%s", col, sql)
				}
			}
		})
	}
	// The decree predicate must not reach for an unprefixed column: `decrees` has
	// its own `incarnation_name` (the reaction's TARGET), and matching a subject
	// against it would silently swap the two ends of the rule.
	decree := MatchSQL(DecreeColumns, Host{SID: "host-a"}.Args(), (&ArgBinder{}).Bind)
	if strings.Contains(decree, "incarnation_name") {
		t.Errorf("decree subject predicate touches incarnation_name (the target, not the subject):\n%s", decree)
	}
}

// TestMatchSQL_SplicesIntoCallerNumbering — a caller with placeholders already
// in flight passes its own binder, and the predicate continues that numbering
// instead of restarting at $1.
func TestMatchSQL_SplicesIntoCallerNumbering(t *testing.T) {
	b := &ArgBinder{Args: []any{"already", "bound"}}
	sql := MatchSQL(VigilColumns, Host{SID: "host-a"}.Args(), b.Bind)

	if len(b.Args) != 7 {
		t.Fatalf("bound %d args in total, want 2 + 5", len(b.Args))
	}
	if strings.Contains(sql, "$1") || strings.Contains(sql, "$2") {
		t.Errorf("predicate reused the caller's placeholders:\n%s", sql)
	}
	if !strings.Contains(sql, "$3") || !strings.Contains(sql, "$7") {
		t.Errorf("predicate does not span $3..$7:\n%s", sql)
	}
}
