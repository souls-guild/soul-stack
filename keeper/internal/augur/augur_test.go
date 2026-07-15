package augur

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestValidName(t *testing.T) {
	good := []string{"a", "vault-prod", "prom-main", "elk-logs", "1cloud"}
	bad := []string{"", "Upper", "with_underscore", "x:colon", strings.Repeat("a", 64)}
	for _, n := range good {
		if !ValidName(n) {
			t.Errorf("ValidName(%q) = false, want true", n)
		}
	}
	for _, n := range bad {
		if ValidName(n) {
			t.Errorf("ValidName(%q) = true, want false", n)
		}
	}
}

func TestValidSourceType(t *testing.T) {
	for _, s := range []SourceType{SourceVault, SourcePrometheus, SourceELK} {
		if !ValidSourceType(s) {
			t.Errorf("ValidSourceType(%q) = false", s)
		}
	}
	for _, s := range []SourceType{"", "mysql", "Vault", "redis"} {
		if ValidSourceType(s) {
			t.Errorf("ValidSourceType(%q) = true", s)
		}
	}
}

func TestValidCoven(t *testing.T) {
	good := []string{"web", "db-1", "0abc", "a"}
	bad := []string{"", "-bad", "_x", "Web", "x.y"}
	for _, c := range good {
		if !ValidCoven(c) {
			t.Errorf("ValidCoven(%q) = false", c)
		}
	}
	for _, c := range bad {
		if ValidCoven(c) {
			t.Errorf("ValidCoven(%q) = true", c)
		}
	}
}

func TestValidAuthRef(t *testing.T) {
	good := []string{"vault:secret/keeper/x", "vault:secret/a/b/c"}
	bad := []string{"", "vault:", "vault:onlymount", "secret/x", "env:FOO", "vault:secret/../x"}
	for _, r := range good {
		if !ValidAuthRef(r) {
			t.Errorf("ValidAuthRef(%q) = false, want true", r)
		}
	}
	for _, r := range bad {
		if ValidAuthRef(r) {
			t.Errorf("ValidAuthRef(%q) = true, want false", r)
		}
	}
}

func TestValidateAllow(t *testing.T) {
	cases := []struct {
		name  string
		src   SourceType
		allow string
		ok    bool
	}{
		{"vault-paths", SourceVault, `{"paths":["secret/x"]}`, true},
		{"vault-policies", SourceVault, `{"policies":["read-db"]}`, true},
		{"vault-both", SourceVault, `{"paths":["a"],"policies":["b"]}`, true},
		{"vault-empty-obj", SourceVault, `{}`, false},
		{"vault-empty-arrays", SourceVault, `{"paths":[],"policies":[]}`, false},
		{"vault-unknown-key", SourceVault, `{"queries":["x"]}`, false},
		{"prom-ok", SourcePrometheus, `{"queries":["up"]}`, true},
		{"prom-empty", SourcePrometheus, `{"queries":[]}`, false},
		{"prom-wrong-shape", SourcePrometheus, `{"paths":["x"]}`, false},
		{"elk-ok", SourceELK, `{"indices":["logs-*"]}`, true},
		{"elk-empty", SourceELK, `{"indices":[]}`, false},
		{"empty-json", SourceVault, ``, false},
		{"not-object", SourceVault, `["x"]`, false},
		{"unknown-src", SourceType("redis"), `{"x":1}`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := ValidateAllow(c.src, json.RawMessage(c.allow))
			if c.ok && err != nil {
				t.Errorf("ValidateAllow ok-case returned %v", err)
			}
			if !c.ok && err == nil {
				t.Errorf("ValidateAllow bad-case returned nil")
			}
		})
	}
}

func TestValidateTokenFields(t *testing.T) {
	mk := func(delegate bool, ttl *string, uses *int) *Rite {
		return &Rite{Delegate: delegate, TokenTTL: ttl, TokenNumUses: uses}
	}
	p := func(s string) *string { return &s }
	n := func(i int) *int { return &i }

	// no token fields — always ok, regardless of src/delegate.
	if err := ValidateTokenFields(SourcePrometheus, mk(false, nil, nil)); err != nil {
		t.Errorf("no token fields: %v", err)
	}
	// vault + delegate + valid ttl/uses — ok.
	if err := ValidateTokenFields(SourceVault, mk(true, p("5m"), n(3))); err != nil {
		t.Errorf("vault delegate happy: %v", err)
	}
	// token fields without delegate — fail.
	if err := ValidateTokenFields(SourceVault, mk(false, p("5m"), nil)); err == nil {
		t.Error("token without delegate accepted")
	}
	// token fields + delegate, but not vault — fail.
	if err := ValidateTokenFields(SourcePrometheus, mk(true, p("5m"), nil)); err == nil {
		t.Error("token on prometheus accepted")
	}
	// bad ttl format — fail.
	if err := ValidateTokenFields(SourceVault, mk(true, p("5banana"), nil)); err == nil {
		t.Error("bad ttl accepted")
	}
	// negative num_uses — fail.
	if err := ValidateTokenFields(SourceVault, mk(true, nil, n(-1))); err == nil {
		t.Error("negative num_uses accepted")
	}
	// num_uses without ttl — ok (uses only).
	if err := ValidateTokenFields(SourceVault, mk(true, nil, n(5))); err != nil {
		t.Errorf("uses-only: %v", err)
	}
}

func TestValidateSubjectXOR(t *testing.T) {
	p := func(s string) *string { return &s }
	// coven only — ok.
	if err := ValidateSubjectXOR(&Rite{Coven: p("web")}); err != nil {
		t.Errorf("coven-only: %v", err)
	}
	// sid only — ok.
	if err := ValidateSubjectXOR(&Rite{SID: p("host.example.com")}); err != nil {
		t.Errorf("sid-only: %v", err)
	}
	// both — fail.
	if err := ValidateSubjectXOR(&Rite{Coven: p("web"), SID: p("h")}); err == nil {
		t.Error("both subjects accepted")
	}
	// none — fail.
	if err := ValidateSubjectXOR(&Rite{}); err == nil {
		t.Error("no subject accepted")
	}
	// empty-string coven counts as absent.
	if err := ValidateSubjectXOR(&Rite{Coven: p("")}); err == nil {
		t.Error("empty coven accepted as present")
	}
	// bad coven format — fail.
	if err := ValidateSubjectXOR(&Rite{Coven: p("-bad")}); err == nil {
		t.Error("bad coven format accepted")
	}
}
