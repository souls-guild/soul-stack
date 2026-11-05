//go:build integration

package applyrun

import (
	"context"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
)

// TestIntegration_RunDetailServiceVersion reads the run's service git ref back through
// the real write paths and the real jsonb (NIM-1175): the terminal history row's
// snapshot first, else the recipe of a planned row that is not a linear step's. A
// step row's recipe pins the commit the run loaded and is never the answer; a row
// with no recipe, or a history row with no snapshot, is passed over.
func TestIntegration_RunDetailServiceVersion(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod", "archon-alice")
	ctx := context.Background()
	aid := "archon-alice"
	const commit = "9f1c2b7e4d3a5f6081726354a5b6c7d8e9f0a1b2"
	recipe := func(ref string, step bool) *Recipe {
		r := &Recipe{
			ServiceRef:   artifact.ServiceRef{Name: "redis", Git: "file:///srv/redis", Ref: ref},
			ScenarioName: "create",
		}
		if step {
			r.Step = &RecipeStep{Roster: []string{"host-c"}, Tasks: []int{0}}
		}
		return r
	}
	insert := func(applyID, sid string) {
		t.Helper()
		if err := Insert(ctx, integrationPool, &ApplyRun{
			ApplyID: applyID, SID: sid, IncarnationName: "redis-prod",
			Scenario: "create", Status: StatusRunning, StartedByAID: &aid,
		}); err != nil {
			t.Fatalf("Insert(%s/%s): %v", applyID, sid, err)
		}
	}
	insertPlanned := func(applyID, sid string, r *Recipe) {
		t.Helper()
		if err := InsertPlanned(ctx, integrationPool, &ApplyRun{
			ApplyID: applyID, SID: sid, IncarnationName: "redis-prod",
			Scenario: "create", StartedByAID: &aid, Recipe: r,
		}); err != nil {
			t.Fatalf("InsertPlanned(%s/%s): %v", applyID, sid, err)
		}
	}
	history := func(historyID, applyID string, r *Recipe, at string) {
		t.Helper()
		var snapshot []byte
		if r != nil {
			b, err := MarshalRecipe(r)
			if err != nil {
				t.Fatalf("MarshalRecipe: %v", err)
			}
			snapshot = b
		}
		if _, err := integrationPool.Exec(ctx, `
INSERT INTO state_history (history_id, incarnation_name, scenario, apply_id, run, run_status, at)
VALUES ($1, 'redis-prod', 'create', $2, $3, CASE WHEN $3::jsonb IS NULL THEN NULL ELSE 'ready' END, NOW() + $4::interval)`,
			historyID, applyID, snapshot, at); err != nil {
			t.Fatalf("seed state_history %s: %v", historyID, err)
		}
	}
	version := func(applyID string) string {
		t.Helper()
		d, err := SelectRunDetail(ctx, integrationPool, applyID, "redis-prod")
		if err != nil {
			t.Fatalf("SelectRunDetail(%s): %v", applyID, err)
		}
		return d.ServiceVersion
	}

	// A planned run beside a row with no recipe, which sorts first: the planned
	// row names the ref.
	insert("01HSVCREFPLANNED", "host-a")
	insertPlanned("01HSVCREFPLANNED", "host-b", recipe("v1.4.0", false))
	if got := version("01HSVCREFPLANNED"); got != "v1.4.0" {
		t.Errorf("planned run ServiceVersion = %q, want v1.4.0", got)
	}
	// Once the terminal recorded its snapshot, that is the answer.
	history("01HSVCREFHIST00", "01HSVCREFPLANNED", recipe("v1.3.9", false), "0 seconds")
	if got := version("01HSVCREFPLANNED"); got != "v1.3.9" {
		t.Errorf("planned run with history ServiceVersion = %q, want v1.3.9 from the snapshot", got)
	}

	// A linear run, whose planned rows are all step rows: a commit is not a ref,
	// until the terminal records the run's own.
	insertPlanned("01HSVCREFSTEPS", "host-c", recipe(commit, true))
	insertPlanned("01HSVCREFSTEPS", "host-d", recipe(commit, true))
	if got := version("01HSVCREFSTEPS"); got != "" {
		t.Errorf("linear run ServiceVersion = %q, want empty (a step recipe pins a commit)", got)
	}
	history("01HSVCREFHIST03", "01HSVCREFSTEPS", recipe("v1.4.1", false), "0 seconds")
	if got := version("01HSVCREFSTEPS"); got != "v1.4.1" {
		t.Errorf("finished linear run ServiceVersion = %q, want v1.4.1 from its snapshot", got)
	}

	// An inline run: no recipe anywhere, so nothing before its terminal…
	insert("01HSVCREFINLINE", "host-a")
	if got := version("01HSVCREFINLINE"); got != "" {
		t.Errorf("inline run without history ServiceVersion = %q, want empty", got)
	}
	// …and its snapshot after, which a later marker row without one does not hide.
	history("01HSVCREFHIST01", "01HSVCREFINLINE", recipe("v1.3.0", false), "-1 minute")
	history("01HSVCREFHIST02", "01HSVCREFINLINE", nil, "0 seconds")
	if got := version("01HSVCREFINLINE"); got != "v1.3.0" {
		t.Errorf("inline run ServiceVersion = %q, want v1.3.0 from its history snapshot", got)
	}
}
