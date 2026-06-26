package rbac

import (
	"strings"
	"testing"
)

// ADR-047 amendment / ADR-060 п.7 slice 1 — trait-ключ селектора: exact
// `key:value`-match по incarnation.traits. Параллель state (S2c) / soulprint
// (S2b), но НЕ CEL-предикат, а точное равенство (как coven): Selector["trait"]
// несёт нормализованную строку `key:value`, реальный match — incarnation-list/get
// резолвер (slice 1 п.7). Matches fail-closed (map[string]string-context не несёт
// nested traits). Семантика slice 1 — OR-измерение Purview.

// --- Парсинг key:value ---

// trait=owner:alice парсится в Selector{trait:["owner:alice"]}.
func TestParseSelector_Trait_Simple(t *testing.T) {
	p, err := ParsePermission(`incarnation.run on trait=owner:alice`)
	if err != nil {
		t.Fatalf("ParsePermission: %v", err)
	}
	got := p.Selector["trait"]
	if len(got) != 1 || got[0] != "owner:alice" {
		t.Errorf("Selector[trait] = %v, want [owner:alice]", got)
	}
}

// Точечные/дефисные/подчёркивающие символы в обеих половинах допустимы (reSelValue).
func TestParseSelector_Trait_DottedValues(t *testing.T) {
	p, err := ParsePermission(`incarnation.run on trait=namespace:dba-ns_01.x`)
	if err != nil {
		t.Fatalf("ParsePermission: %v", err)
	}
	if got := p.Selector["trait"]; len(got) != 1 || got[0] != "namespace:dba-ns_01.x" {
		t.Errorf("Selector[trait] = %v, want [namespace:dba-ns_01.x]", got)
	}
}

// Без `:` — отказ (форма обязана быть key:value).
func TestParseSelector_Trait_MissingColonRejected(t *testing.T) {
	_, err := ParsePermission(`incarnation.run on trait=owner`)
	if err == nil {
		t.Fatal("ParsePermission(trait=owner): want error (must be key:value), got nil")
	}
}

// Больше одной `:` — отказ (ровно один разделитель).
func TestParseSelector_Trait_MultipleColonsRejected(t *testing.T) {
	_, err := ParsePermission(`incarnation.run on trait=owner:a:b`)
	if err == nil {
		t.Fatal("ParsePermission(trait=owner:a:b): want error (exactly one ':'), got nil")
	}
}

// Пустой ключ / пустое значение — отказ.
func TestParseSelector_Trait_EmptyHalvesRejected(t *testing.T) {
	cases := []string{
		`incarnation.run on trait=:alice`,
		`incarnation.run on trait=owner:`,
		`incarnation.run on trait=:`,
	}
	for _, in := range cases {
		t.Run(in, func(t *testing.T) {
			if _, err := ParsePermission(in); err == nil {
				t.Fatalf("ParsePermission(%q): want error for empty key/value, got nil", in)
			}
		})
	}
}

// Недопустимые символы (пробел) в значении — отказ (scalar-only, reSelValue).
func TestParseSelector_Trait_BadCharsRejected(t *testing.T) {
	_, err := ParsePermission(`incarnation.run on trait=owner:al ice`)
	if err == nil {
		t.Fatal("ParsePermission(trait с пробелом): want error, got nil")
	}
}

// --- Matches: fail-closed без traits в context ---

// Текущий map[string]string-context не несёт nested traits → trait-измерение
// fail-closed deny (резолвер incarnation-list/get подаст реальный match).
func TestMatches_Trait_FailClosedWithoutTraits(t *testing.T) {
	p, err := ParsePermission(`incarnation.run on trait=owner:alice`)
	if err != nil {
		t.Fatalf("ParsePermission: %v", err)
	}
	if p.Matches("incarnation", "run", map[string]string{"incarnation": "redis-prod", "trait": "owner:alice"}) {
		t.Error("trait-perm без nested traits в context должна давать deny (slice 1 fail-closed)")
	}
	if p.Matches("incarnation", "run", nil) {
		t.Error("trait-perm с nil-context должна давать deny")
	}
}

// --- Purview.TraitExprs ---

// ResolvePurview с trait-permission заполняет Purview.TraitExprs.
func TestResolvePurview_Trait(t *testing.T) {
	e := mustEnforcer(t, fixtureRole{
		name: "alice-ops", operators: []string{"archon-a"},
		permissions: []string{`incarnation.run on trait=owner:alice`},
	})
	p := e.ResolvePurview("archon-a", "incarnation", "run")
	if p.Unrestricted {
		t.Error("Unrestricted=true, want false (trait-scoped)")
	}
	if len(p.TraitExprs) != 1 || p.TraitExprs[0] != "owner:alice" {
		t.Errorf("TraitExprs = %v, want [owner:alice]", p.TraitExprs)
	}
}

// default_scope=trait наследуется bare-permission-ом (S1 + trait вместе).
func TestResolvePurview_Trait_DefaultScopeInherited(t *testing.T) {
	e := mustEnforcer(t, fixtureRole{
		name: "alice-ops", operators: []string{"archon-a"},
		defaultScope: `trait=owner:alice`,
		permissions:  []string{"incarnation.run"},
	})
	p := e.ResolvePurview("archon-a", "incarnation", "run")
	if p.Unrestricted {
		t.Error("Unrestricted=true, want false (bare наследует trait default_scope)")
	}
	if len(p.TraitExprs) != 1 || p.TraitExprs[0] != "owner:alice" {
		t.Errorf("TraitExprs = %v, want [owner:alice] (наследование default_scope)", p.TraitExprs)
	}
}

// trait-only Purview даёт HoldsAction=true (gate видит scoped-роль с одним
// trait-измерением — иначе оператор получил бы 403 на собственный список).
func TestHoldsAction_TraitOnly(t *testing.T) {
	e := mustEnforcer(t, fixtureRole{
		name: "alice-ops", operators: []string{"archon-a"},
		permissions: []string{`incarnation.list on trait=owner:alice`},
	})
	if !e.HoldsAction("archon-a", "incarnation", "list") {
		t.Error("trait-only scope обязан давать HoldsAction=true (gate-видимость)")
	}
}

// --- subset: trait = string-equality fail-closed (escalation-guard) ---

func TestSubset_Trait_StringEquality(t *testing.T) {
	alice := `incarnation.run on trait=owner:alice`
	bob := `incarnation.run on trait=owner:bob`

	tests := []struct {
		name        string
		callerRaws  []string
		grantedRaws []string
		wantHeld    bool // true → ErrPermissionNotHeld (выдача запрещена)
	}{
		{
			name:        "идентичная trait-пара → выдача ок",
			callerRaws:  []string{alice},
			grantedRaws: []string{alice},
			wantHeld:    false,
		},
		{
			name:        "иная trait-пара → DENY (fail-closed, escalation-guard)",
			callerRaws:  []string{alice},
			grantedRaws: []string{bob},
			wantHeld:    true,
		},
		{
			name:        "caller с * выдаёт любой trait",
			callerRaws:  []string{"*"},
			grantedRaws: []string{alice},
			wantHeld:    false,
		},
		{
			name:        "caller без trait-scope (bare) выдаёт trait → ок (bare покрывает)",
			callerRaws:  []string{"incarnation.run"},
			grantedRaws: []string{alice},
			wantHeld:    false,
		},
		{
			name:        "caller с trait выдаёт bare → DENY (bare шире trait-scope caller-а)",
			callerRaws:  []string{alice},
			grantedRaws: []string{"incarnation.run"},
			wantHeld:    true,
		},
		{
			name:        "caller с trait выдаёт coven (иное измерение) → DENY",
			callerRaws:  []string{alice},
			grantedRaws: []string{"incarnation.run on coven=prod"},
			wantHeld:    true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			caller := mustParse(t, tc.callerRaws...)
			required := mustParse(t, tc.grantedRaws...)
			err := assertCallerCovers(caller, required)
			gotHeld := strings.Contains(errString(err), "least-privilege")
			if gotHeld != tc.wantHeld {
				t.Fatalf("assertCallerCovers err = %v; held=%v, want %v", err, gotHeld, tc.wantHeld)
			}
		})
	}
}
