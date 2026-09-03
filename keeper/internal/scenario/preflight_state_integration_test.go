//go:build integration

package scenario

// THE DIVERGENCE, second instance. [Runner.PreflightAssert] assembled a
// render.RenderInput WITHOUT `State`, while run() has always carried the
// incarnation's state snapshot there. `incarnation.state.<path>` is therefore
// absent from the CEL context of every pre-flight assert — not empty, ABSENT:
// cel_render.go binds the key only `if in.State != nil`.
//
// The symptom split by how the expression was written, and the defensive form is
// the dangerous one (NIM-403, NIM-404):
//
//   - a bare read — `default(incarnation.state.redis_version, '')` — raises a CEL
//     error `no such key: state`, which surfaced to the operator as a 500;
//   - a GUARDED read — `has(incarnation.state) && …` — raises nothing. It
//     evaluates to false and the assert fails with a confident 422 naming a
//     condition that is actually satisfied. A predicate written to be safe turned
//     a missing context into a wrong answer.
//
// Both were caught by the L3b live gate, which is the point: unit coverage of
// pre-flight never supplied a state-reading assert, so the gap was invisible
// until day-2 scenarios of a real service ran against a real incarnation.
//
// Same shape as NIM-271 (pre-flight resolved service vars from a synthetic
// incarnation), and fixed the same way — pre-flight takes the input run() would.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
)

// stateAssertServiceRepo is a service whose day-2 scenarios read
// `incarnation.state` from an assert — once bare, once guarded — so the two
// symptom shapes are exercised by the same fixture.
func stateAssertServiceRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("PlainInit: %v", err)
	}
	write := func(rel, content string) {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}
	write("service.yml", `description: pre-flight asserts that read incarnation.state
state_schema: {}
`)
	write("vars/00-base.yaml", "base_marker: default\n")

	// Bare read: no has() around it. With State absent this is a hard CEL error.
	write("scenario/verify_bare/main.yml", `name: verify_bare
description: assert reading incarnation.state without a has() guard
input: {}
tasks:
  - name: Guard on the recorded version
    assert:
      that:
        - "default(incarnation.state.recorded_version, '') == '7.4'"
      message: "recorded_version is not 7.4"
`)

	// Guarded read: the shape that fails SILENTLY when State is missing.
	write("scenario/verify_guarded/main.yml", `name: verify_guarded
description: assert reading incarnation.state behind has() guards
input: {}
tasks:
  - name: Guard on the tls switch
    assert:
      that:
        - "has(incarnation.state) && has(incarnation.state.tls) && default(incarnation.state.tls.enable, false)"
      message: "tls must be enabled"
`)
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("Worktree: %v", err)
	}
	if err := wt.AddGlob("."); err != nil {
		t.Fatalf("AddGlob: %v", err)
	}
	if _, err := wt.Commit("init state-assert-guard", &git.CommitOptions{
		Author: &object.Signature{Name: "T", Email: "t@example.test", When: time.Now()},
	}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	return "file://" + dir
}

// seedStatefulIncarnation seeds a READY incarnation of THIS fixture's service
// carrying the state a day-2 assert reads, plus its roster. Distinct from
// gap4_test.go's seedIncarnationWithState, which pins service "noop": the
// pre-flight path resolves the service snapshot by name, so the row has to name
// this repo. The state is the point — it exists in the row, and the only
// question is whether pre-flight looks at it.
func seedStatefulIncarnation(t *testing.T, name string, state map[string]any, sids ...string) {
	t.Helper()
	inc := &incarnation.Incarnation{
		ID: name, Service: "state-assert-guard", ServiceVersion: "master",
		StateSchemaVersion: 1, Status: incarnation.StatusReady, State: state,
	}
	if err := incarnation.Create(context.Background(), integrationPool, inc); err != nil {
		t.Fatalf("seedStatefulIncarnation: %v", err)
	}
	for _, sid := range sids {
		seedConnectedSoul(t, sid, []string{name})
	}
}

// NIM-403. A bare read of incarnation.state must evaluate, not explode. Without
// the fix this fails with `no such key: state` — and that error reached the
// operator as a 500 rather than as anything about their expression.
func TestIntegration_PreflightAssert_BareStateReadResolves(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedStatefulIncarnation(t, "stateful", map[string]any{"recorded_version": "7.4"}, "a.example.com")
	gitURL := stateAssertServiceRepo(t)
	r := newRunner(t, &mockDispatcher{t: t}, gitURL)

	err := r.PreflightAssert(context.Background(), RunSpec{
		IncarnationName: "stateful",
		ServiceRef:      artifact.ServiceRef{Name: "state-assert-guard", Git: gitURL, Ref: "master"},
		ScenarioName:    "verify_bare",
		StartedByAID:    "archon-alice",
	})
	if err != nil {
		t.Fatalf("PreflightAssert: incarnation.state of the REAL incarnation must be visible, got %v", err)
	}
}

// NIM-404, the dangerous half. A has()-guarded read produces NO error when the
// context is missing — it produces `false`, and the assert refuses the run while
// naming a condition the incarnation actually satisfies. Nothing distinguishes
// that 422 from an honest one, which is why this needs its own guard rather
// than riding on the bare case above.
func TestIntegration_PreflightAssert_GuardedStateReadSeesTheValue(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedStatefulIncarnation(t, "tlson", map[string]any{"tls": map[string]any{"enable": true}}, "a.example.com")
	gitURL := stateAssertServiceRepo(t)
	r := newRunner(t, &mockDispatcher{t: t}, gitURL)

	err := r.PreflightAssert(context.Background(), RunSpec{
		IncarnationName: "tlson",
		ServiceRef:      artifact.ServiceRef{Name: "state-assert-guard", Git: gitURL, Ref: "master"},
		ScenarioName:    "verify_guarded",
		StartedByAID:    "archon-alice",
	})
	if err != nil {
		t.Fatalf("PreflightAssert: a has()-guarded read of state must see it, got %v", err)
	}
}

// The other direction, so the fix cannot be "always pass". An incarnation whose
// state genuinely does NOT satisfy the guard must still be refused — with
// ErrAssertFailed, the honest 422.
func TestIntegration_PreflightAssert_GuardedStateStillRefusesWhenFalse(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedStatefulIncarnation(t, "tlsoff", map[string]any{"tls": map[string]any{"enable": false}}, "a.example.com")
	gitURL := stateAssertServiceRepo(t)
	r := newRunner(t, &mockDispatcher{t: t}, gitURL)

	err := r.PreflightAssert(context.Background(), RunSpec{
		IncarnationName: "tlsoff",
		ServiceRef:      artifact.ServiceRef{Name: "state-assert-guard", Git: gitURL, Ref: "master"},
		ScenarioName:    "verify_guarded",
		StartedByAID:    "archon-alice",
	})
	if err == nil {
		t.Fatal("an incarnation with tls disabled passed a guard that requires it enabled")
	}
	if !strings.Contains(err.Error(), "tls must be enabled") {
		t.Errorf("refusal does not name the assert's own message: %v", err)
	}
}

// The create path has no row and therefore no state, and that must stay a
// deferral rather than becoming an error: an assert over state on a scenario
// that creates the incarnation has nothing to read by construction (NIM-124).
func TestIntegration_PreflightAssert_NoRowKeepsStateAbsent(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	gitURL := stateAssertServiceRepo(t)
	r := newRunner(t, &mockDispatcher{t: t}, gitURL)

	err := r.PreflightAssert(context.Background(), RunSpec{
		IncarnationName: "absent",
		ServiceRef:      artifact.ServiceRef{Name: "state-assert-guard", Git: gitURL, Ref: "master"},
		ScenarioName:    "verify_guarded",
		StartedByAID:    "archon-alice",
	})
	// Either outcome is defensible for a missing row; what must NOT happen is a
	// panic or an internal error naming the CEL machinery.
	if err != nil && strings.Contains(err.Error(), "no such key") {
		t.Errorf("a missing incarnation produced a CEL context error rather than a domain answer: %v", err)
	}
}
