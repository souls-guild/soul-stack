package config

import (
	"reflect"
	"testing"
)

func refKey(k string) InputRefStep { return InputRefStep{Key: k} }

var refElement = InputRefStep{Element: true}

// TestValidateRuleInputRefs_FullDepthOutermostOnly — NIM-853. A read is reported at
// full depth and only at the outermost node of its chain: reporting `creds` beside
// `creds.user` would make every member read look like a read of the whole container.
func TestValidateRuleInputRefs_FullDepthOutermostOnly(t *testing.T) {
	cases := []struct {
		expr string
		want []InputRef
	}{
		{`input.creds.user != ''`, []InputRef{{refKey("creds"), refKey("user")}}},
		{`input.creds["user"] != ''`, []InputRef{{refKey("creds"), refKey("user")}}},
		{`input["creds"].user != ''`, []InputRef{{refKey("creds"), refKey("user")}}},
		{`.input.creds.user != ''`, []InputRef{{refKey("creds"), refKey("user")}}},
		{`has(input.creds.user)`, []InputRef{{refKey("creds"), refKey("user")}}},
		{`input.creds.user.startsWith('a')`, []InputRef{{refKey("creds"), refKey("user")}}},
		{`size(input.creds) > 0`, []InputRef{{refKey("creds")}}},
		{`input.users[0].name != ''`, []InputRef{{refKey("users"), refElement, refKey("name")}}},
		{`input.creds[input.k] != ''`, []InputRef{{refKey("k")}, {refKey("creds"), refElement}}},
		{`size(input) > 0`, []InputRef{{}}},
		{`input.users.all(u, u.name != '')`, []InputRef{{refKey("users")}}},
		{`'input.creds.user' != ''`, nil},
		{`incarnation.id != ''`, nil},
	}
	for _, c := range cases {
		got, readable := ValidateRuleInputRefs(c.expr)
		if !readable {
			t.Errorf("%s: reported unreadable", c.expr)
			continue
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s:\n got %#v\nwant %#v", c.expr, got, c.want)
		}
	}
}

// TestValidateRuleInputRefs_LeavesTheProgramCacheAlone — the listing reads rules from
// any branch or tag a `service.list` caller names, and validateProgs never evicts.
func TestValidateRuleInputRefs_LeavesTheProgramCacheAlone(t *testing.T) {
	const expr = `input.nim853_cache_probe != ''`
	if _, readable := ValidateRuleInputRefs(expr); !readable {
		t.Fatalf("%s: reported unreadable", expr)
	}
	validateProgMu.RLock()
	_, cached := validateProgs[expr]
	validateProgMu.RUnlock()
	if cached {
		t.Error("reading a rule's refs put its program in the evaluator's cache")
	}
}

func TestValidateRuleInputRefs_UnparseableIsNotReadable(t *testing.T) {
	if refs, readable := ValidateRuleInputRefs(`input.creds.user != '' &&`); readable {
		t.Errorf("an unparseable predicate came back readable with %#v — the caller would publish it as naming nothing", refs)
	}
}
