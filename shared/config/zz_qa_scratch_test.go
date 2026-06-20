package config

import (
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/diag"
)

func loadScn(t *testing.T, body string) (*ScenarioManifest, []diag.Diagnostic) {
	t.Helper()
	m, _, diags, err := LoadScenarioManifestFromBytes("main.yml", []byte(body), ValidateOptions{})
	if err != nil {
		t.Logf("LoadScenarioManifest hard-err: %v", err)
	}
	return m, diags
}

func dumpDiags(t *testing.T, diags []diag.Diagnostic) {
	for _, d := range diags {
		t.Logf("[%s] %s :: %s :: %s", d.Level, d.Code, d.YAMLPath, d.Message)
	}
}

// EDGE 5: expect на ADD — должно быть отвергнуто валидатором (только modify/remove).
func TestQA_ExpectOnAddRejected(t *testing.T) {
	body := `name: add_user
tasks: []
state_changes:
  - add: redis_users
    key: "${ input.username }"
    value: { acl: "x" }
    expect: one
`
	_, diags := loadScn(t, body)
	dumpDiags(t, diags)
	rejected := false
	for _, d := range diags {
		if d.Level == diag.LevelError && strings.Contains(d.YAMLPath, "expect") {
			rejected = true
		}
	}
	if !rejected {
		t.Errorf("BUG: expect: on add NOT rejected by validator (ADR-057 §c: expect only modify/remove)")
	}
}

// EDGE 5: expect на SET — тоже не должно приниматься.
func TestQA_ExpectOnSetRejected(t *testing.T) {
	body := `name: s
tasks: []
state_changes:
  - set: foo
    value: "bar"
    expect: one
`
	_, diags := loadScn(t, body)
	dumpDiags(t, diags)
	rejected := false
	for _, d := range diags {
		if d.Level == diag.LevelError && strings.Contains(d.YAMLPath, "expect") {
			rejected = true
		}
	}
	if !rejected {
		t.Errorf("BUG: expect: on set NOT rejected by validator")
	}
}

// EDGE 2: вложенный foreach в do — грамматика запрещает. Валидатор ловит?
func TestQA_NestedForeachRejected(t *testing.T) {
	body := `name: s
tasks: []
state_changes:
  - foreach: "${ input.outer }"
    as: o
    do:
      - foreach: "${ o.inner }"
        as: i
        do:
          - add: hosts
            value: "${ i }"
`
	_, diags := loadScn(t, body)
	dumpDiags(t, diags)
	rejected := false
	for _, d := range diags {
		if d.Level == diag.LevelError {
			rejected = true
		}
	}
	if !rejected {
		t.Errorf("NOTE: nested foreach NOT rejected (ADR-057: do несёт глаголы, не повторный foreach)")
	}
}

// EDGE 2: foreach с on_conflict внутри do (add) — допустимо?
func TestQA_ForeachAddOnConflictInDo(t *testing.T) {
	body := `name: s
tasks: []
state_changes:
  - foreach: "${ input.sids }"
    as: sid
    do:
      - add: redis_hosts
        value: "${ sid }"
        match: "elem == sid"
        on_conflict: error
`
	_, diags := loadScn(t, body)
	dumpDiags(t, diags)
	for _, d := range diags {
		if d.Level == diag.LevelError {
			t.Errorf("foreach+add+on_conflict in do rejected unexpectedly: %s", d.Message)
		}
	}
}

// EDGE 5: expect:any explicitly on add (the validator may allow 'any' anywhere).
func TestQA_ExpectAnyOnAdd(t *testing.T) {
	body := `name: s
tasks: []
state_changes:
  - add: hosts
    value: "x"
    expect: any
`
	_, diags := loadScn(t, body)
	dumpDiags(t, diags)
}
