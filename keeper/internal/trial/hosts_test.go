package trial

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// hostsSizeMain — scenario that prints size(soulprint.hosts) in command params.
// Render-observable analog of size-guard: roster size appears directly in rendered plan
// (not in flow-control `failed_when:`, which Soul-side does not carry soulprint.hosts
// in flow_context — see buildFlowContext).
const hostsSizeMain = `name: create
input: {}
tasks:
  - name: report roster size
    module: core.exec.run
    params:
      cmd: "echo ${ soulprint.hosts.size() }"
`

// hostsWhereMain — scenario that filters roster by declared-role: cross-host projection
// of soulprint.hosts.where(...) in params (primary-discovery idiom).
const hostsWhereMain = `name: create
input: {}
tasks:
  - name: replicaof primary
    module: core.exec.run
    params:
      cmd: "replicaof ${ soulprint.hosts.where(\"role == 'primary'\")[0].network.primary_ip } 6379"
`

// TestRunCase_MultiHostRoster_Size3 — fixtures.hosts with 3 hosts gives
// size(soulprint.hosts)==3 in rendered plan: multi-host roster really raises
// the ceiling of single-host harness.
func TestRunCase_MultiHostRoster_Size3(t *testing.T) {
	caseDir := writeScenarioTree(t, hostsSizeMain, `name: roster of three
fixtures:
  hosts:
    - sid: node-3.example.com
      covens: [create, redis]
    - sid: node-1.example.com
      covens: [create, redis]
    - sid: node-2.example.com
      covens: [create, redis]
assert:
  rendered_tasks:
    - index: 0
      module: core.exec.run
      params:
        cmd: "echo 3"
`)

	results, err := Run(context.Background(), caseDir)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !results[0].Pass {
		t.Fatalf("expected PASS (size==3 on 3-host roster), got: %v", results[0].Failures)
	}
}

// TestRunCase_MultiHostRoster_WhereProjection — soulprint.hosts.where(...) on
// roster resolves cross-host fact (primary_ip of host with declared-role primary).
func TestRunCase_MultiHostRoster_WhereProjection(t *testing.T) {
	caseDir := writeScenarioTree(t, hostsWhereMain, `name: where over roster
fixtures:
  hosts:
    - sid: replica-1.example.com
      covens: [create]
      role: replica
      soulprint:
        network: { primary_ip: 10.0.0.2 }
    - sid: primary-1.example.com
      covens: [create]
      role: primary
      soulprint:
        network: { primary_ip: 10.0.0.1 }
assert:
  rendered_tasks:
    - index: 0
      module: core.exec.run
      params:
        cmd: "replicaof 10.0.0.1 6379"
`)

	results, err := Run(context.Background(), caseDir)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !results[0].Pass {
		t.Fatalf("expected PASS (where role==primary → 10.0.0.1), got: %v", results[0].Failures)
	}
}

// TestRunCase_MultiHostRoster_DeterministicOrder — order of soulprint.hosts
// is determined by SID sorting regardless of YAML entry order.
// [0]-element after where("role!=") is lexicographically first SID.
func TestRunCase_MultiHostRoster_DeterministicOrder(t *testing.T) {
	main := `name: create
input: {}
tasks:
  - name: first host by sid
    module: core.exec.run
    params:
      cmd: "first=${ soulprint.hosts[0].sid }"
`
	// YAML order (c, a, b) is intentionally unsorted: harness must sort
	// by SID → first = a.example.com.
	caseYML := `name: order by sid
fixtures:
  hosts:
    - sid: c.example.com
      covens: [create]
    - sid: a.example.com
      covens: [create]
    - sid: b.example.com
      covens: [create]
assert:
  rendered_tasks:
    - index: 0
      module: core.exec.run
      params:
        cmd: "first=a.example.com"
`
	caseDir := writeScenarioTree(t, main, caseYML)
	results, err := Run(context.Background(), caseDir)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !results[0].Pass {
		t.Fatalf("expected PASS (deterministic order: first by SID = a.example.com), got: %v", results[0].Failures)
	}
}

// TestRunCase_SingleHostSugar_Size1 — back-compat: fixtures.soulprint
// (single-host sugar) gives size(soulprint.hosts)==1 bit-for-bit as before the fix.
func TestRunCase_SingleHostSugar_Size1(t *testing.T) {
	caseDir := writeScenarioTree(t, hostsSizeMain, `name: single host sugar
fixtures:
  soulprint:
    os:
      family: debian
assert:
  rendered_tasks:
    - index: 0
      module: core.exec.run
      params:
        cmd: "echo 1"
`)

	results, err := Run(context.Background(), caseDir)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !results[0].Pass {
		t.Fatalf("expected PASS (single-host sugar → size==1), got: %v", results[0].Failures)
	}
}

// TestLoadCase_RejectsSoulprintAndHosts — fixtures.soulprint (single) and
// fixtures.hosts (multi) in one case → strict-error (mutually exclusive,
// in spirit of strict-decode harness).
func TestLoadCase_RejectsSoulprintAndHosts(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, caseFileName)
	if err := os.WriteFile(file, []byte(`name: both
fixtures:
  soulprint:
    os: { family: debian }
  hosts:
    - sid: node-1.example.com
      covens: [both]
assert:
  rendered_tasks:
    - index: 0
      module: core.exec.run
`), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, _, err := LoadCase(file)
	if err == nil {
		t.Fatalf("expected error on soulprint+hosts mutual exclusion")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("expected message about mutual exclusion, got: %v", err)
	}
}

// TestLoadCase_RejectsHostWithoutSID — host-entry in roster without sid →
// strict-error (sid is mandatory).
func TestLoadCase_RejectsHostWithoutSID(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, caseFileName)
	if err := os.WriteFile(file, []byte(`name: no sid
fixtures:
  hosts:
    - covens: [create]
assert:
  rendered_tasks:
    - index: 0
      module: core.exec.run
`), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, _, err := LoadCase(file)
	if err == nil {
		t.Fatalf("expected error on host without sid")
	}
	if !strings.Contains(err.Error(), "sid") {
		t.Fatalf("expected message about mandatory sid, got: %v", err)
	}
}

// TestLoadCase_RejectsDuplicateSID — two host-entries in roster with same sid →
// strict-error. Duplicate collapses RegisterByHost (map by SID) and makes order of
// soulprint.hosts non-deterministic → unacceptable.
func TestLoadCase_RejectsDuplicateSID(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, caseFileName)
	if err := os.WriteFile(file, []byte(`name: dup sid
fixtures:
  hosts:
    - sid: node-1.example.com
      covens: [create]
    - sid: node-1.example.com
      covens: [create]
assert:
  rendered_tasks:
    - index: 0
      module: core.exec.run
`), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, _, err := LoadCase(file)
	if err == nil {
		t.Fatalf("expected error on duplicate sid")
	}
	if !strings.Contains(err.Error(), "duplicate sid") {
		t.Fatalf("expected message about duplicate sid, got: %v", err)
	}
}

// TestRunCase_MultiHostSizeGuard_PassAndFail — demo size-guard at render-level:
// param checks size(soulprint.hosts) against expected N. Pass-branch (roster exactly
// N) and fail-branch (roster != N) — both observable in rendered plan.
//
// L0-form of guard is render-observable (size in params), NOT flow-control
// `failed_when:`: flow_context Soul-side does not carry soulprint.hosts
// (buildFlowContext), therefore real failed_when-size-guard for service is
// scenario-layer of next slice, not L0-render.
func TestRunCase_MultiHostSizeGuard_PassAndFail(t *testing.T) {
	main := `name: create
input: {}
tasks:
  - name: size guard
    module: core.exec.run
    params:
      cmd: "size=${ soulprint.hosts.size() }"
`
	threeHosts := `fixtures:
  hosts:
    - sid: node-1.example.com
      covens: [create]
    - sid: node-2.example.com
      covens: [create]
    - sid: node-3.example.com
      covens: [create]
`

	// Pass: assert expects exactly size==3 on 3-host roster.
	passDir := writeScenarioTree(t, main, "name: guard pass\n"+threeHosts+`assert:
  rendered_tasks:
    - index: 0
      module: core.exec.run
      params:
        cmd: "size=3"
`)
	passRes, err := Run(context.Background(), passDir)
	if err != nil {
		t.Fatalf("Run pass: %v", err)
	}
	if !passRes[0].Pass {
		t.Fatalf("pass-branch: expected PASS (size==3), got: %v", passRes[0].Failures)
	}

	// Fail: assert expects size==4, but roster has 3 hosts → mismatch.
	failDir := writeScenarioTree(t, main, "name: guard fail\n"+threeHosts+`assert:
  rendered_tasks:
    - index: 0
      module: core.exec.run
      params:
        cmd: "size=4"
`)
	failRes, err := Run(context.Background(), failDir)
	if err != nil {
		t.Fatalf("Run fail: %v", err)
	}
	if failRes[0].Pass {
		t.Fatalf("fail-branch: expected FAIL (roster=3, assert expected size=4)")
	}
}
