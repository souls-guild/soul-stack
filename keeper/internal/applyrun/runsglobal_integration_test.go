//go:build integration

// Integration-тесты глобального read-view прогонов (GET /v1/runs + /v1/runs/stats):
// свёртка apply_runs по apply_id ЧЕРЕЗ ВСЕ инкарнации, SQL-форма AggregateRunStatus,
// фильтры status/incarnation, Purview-scope-подзапрос и сводные счётчики.
// Использует harness из integration_test.go (testcontainers PG).

package applyrun

import (
	"context"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
)

// unrestrictedScope — scope без ограничений (Unrestricted-оператор).
var unrestrictedScope = incarnation.ListScope{Unrestricted: true}

// seedRun — helper: один прогон applyID в инкарнации inc с заданным НАБОРОМ
// host-статусов (host-0..host-N). Терминальные статусы проставляются через
// UpdateStatus (Insert принимает только planned/running).
func seedRun(t *testing.T, ctx context.Context, applyID, inc, scenario string, statuses ...Status) {
	t.Helper()
	aid := "archon-alice"
	for i, st := range statuses {
		sid := "host-" + string(rune('a'+i)) + "-" + applyID
		mustInsertRun(t, ctx, applyID, sid, inc, scenario, StatusRunning, &aid)
		if st == StatusRunning {
			continue
		}
		if err := UpdateStatus(ctx, integrationPool, applyID, sid, 0, st, nil); err != nil {
			t.Fatalf("seedRun UpdateStatus(%s/%s→%s): %v", applyID, sid, st, err)
		}
	}
}

// TestIntegration_ListRuns_AcrossIncarnations — глобальная свёртка через инкарнации:
// SQL-CASE агрегатного статуса совпадает с [AggregateRunStatus] на всех четырёх
// значениях, Incarnation заполнен, порядок — новейшие сверху.
func TestIntegration_ListRuns_AcrossIncarnations(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod", "archon-alice")
	seedIncarnation(t, "redis-staging", "archon-alice")
	ctx := context.Background()

	seedRun(t, ctx, "01HGSUC", "redis-prod", "create", StatusSuccess, StatusNoMatch)
	seedRun(t, ctx, "01HGAPP", "redis-staging", "restart", StatusSuccess, StatusRunning)
	seedRun(t, ctx, "01HGFAI", "redis-staging", "create", StatusFailed, StatusCancelled)
	seedRun(t, ctx, "01HGCAN", "redis-prod", "scale", StatusSuccess, StatusCancelled)

	runs, total, err := ListRuns(ctx, integrationPool, RunsFilter{}, unrestrictedScope, 0, 50)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if total != 4 || len(runs) != 4 {
		t.Fatalf("total=%d len=%d, want 4/4", total, len(runs))
	}

	want := map[string]struct {
		inc    string
		status RunStatus
	}{
		"01HGSUC": {"redis-prod", RunStatusSuccess},
		"01HGAPP": {"redis-staging", RunStatusApplying},
		"01HGFAI": {"redis-staging", RunStatusFailed},
		"01HGCAN": {"redis-prod", RunStatusCancelled},
	}
	for _, r := range runs {
		w, ok := want[r.ApplyID]
		if !ok {
			t.Errorf("неожиданный apply_id %q", r.ApplyID)
			continue
		}
		if r.Incarnation != w.inc {
			t.Errorf("%s: Incarnation = %q, want %q", r.ApplyID, r.Incarnation, w.inc)
		}
		if r.Status != w.status {
			t.Errorf("%s: Status = %q, want %q (SQL-CASE ≠ AggregateRunStatus)", r.ApplyID, r.Status, w.status)
		}
		if r.StartedAt.IsZero() {
			t.Errorf("%s: StartedAt zero", r.ApplyID)
		}
	}
	// Новейшие сверху: started_at неубывающе сверху вниз.
	for i := 1; i < len(runs); i++ {
		if runs[i].StartedAt.After(runs[i-1].StartedAt) {
			t.Errorf("порядок нарушен: runs[%d].StartedAt (%v) позже runs[%d] (%v)",
				i, runs[i].StartedAt, i-1, runs[i-1].StartedAt)
		}
	}
}

// TestIntegration_ListRuns_StatusFilter — фильтр по агрегатному статусу прогона:
// total/страница когерентны фильтру (SQL-фильтр, не Go-постфильтр).
func TestIntegration_ListRuns_StatusFilter(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod", "archon-alice")
	ctx := context.Background()

	seedRun(t, ctx, "01HFS01", "redis-prod", "create", StatusSuccess)
	seedRun(t, ctx, "01HFS02", "redis-prod", "create", StatusFailed)
	seedRun(t, ctx, "01HFS03", "redis-prod", "create", StatusSuccess, StatusOrphaned)

	runs, total, err := ListRuns(ctx, integrationPool, RunsFilter{Status: RunStatusFailed}, unrestrictedScope, 0, 50)
	if err != nil {
		t.Fatalf("ListRuns(status=failed): %v", err)
	}
	if total != 2 || len(runs) != 2 {
		t.Fatalf("total=%d len=%d, want 2/2 (failed + orphaned-прогон)", total, len(runs))
	}
	for _, r := range runs {
		if r.Status != RunStatusFailed {
			t.Errorf("%s: Status = %q, want failed", r.ApplyID, r.Status)
		}
	}
}

// TestIntegration_ListRuns_IncarnationFilter — фильтр по имени инкарнации.
func TestIntegration_ListRuns_IncarnationFilter(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod", "archon-alice")
	seedIncarnation(t, "redis-staging", "archon-alice")
	ctx := context.Background()

	seedRun(t, ctx, "01HIF01", "redis-prod", "create", StatusSuccess)
	seedRun(t, ctx, "01HIF02", "redis-staging", "create", StatusSuccess)

	runs, total, err := ListRuns(ctx, integrationPool, RunsFilter{Incarnation: "redis-staging"}, unrestrictedScope, 0, 50)
	if err != nil {
		t.Fatalf("ListRuns(incarnation=redis-staging): %v", err)
	}
	if total != 1 || len(runs) != 1 || runs[0].ApplyID != "01HIF02" {
		t.Fatalf("got total=%d runs=%+v, want только 01HIF02", total, runs)
	}
}

// TestIntegration_ListRuns_Paging — total считает ВСЕ прогоны, страница — limit/offset.
func TestIntegration_ListRuns_Paging(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod", "archon-alice")
	ctx := context.Background()

	for _, id := range []string{"01HPG01", "01HPG02", "01HPG03"} {
		seedRun(t, ctx, id, "redis-prod", "create", StatusSuccess)
	}
	runs, total, err := ListRuns(ctx, integrationPool, RunsFilter{}, unrestrictedScope, 0, 2)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if total != 3 || len(runs) != 2 {
		t.Errorf("total=%d len=%d, want 3/2", total, len(runs))
	}
}

// TestIntegration_ListRuns_Scope — Purview-scope-подзапрос: coven∪{name}-измерение
// сужает выборку видимыми инкарнациями; пустой scope (не Unrestricted) — fail-closed
// пусто; covens[]-метка тоже матчится.
func TestIntegration_ListRuns_Scope(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod", "archon-alice")
	seedIncarnation(t, "redis-staging", "archon-alice")
	ctx := context.Background()

	// redis-staging получает env-тег team-x (covens[]-плечо scope).
	if _, err := integrationPool.Exec(ctx,
		`UPDATE incarnation SET covens = ARRAY['team-x'] WHERE name = 'redis-staging'`); err != nil {
		t.Fatalf("UPDATE covens: %v", err)
	}

	seedRun(t, ctx, "01HSC01", "redis-prod", "create", StatusSuccess)
	seedRun(t, ctx, "01HSC02", "redis-staging", "create", StatusSuccess)

	// scope по ИМЕНИ инкарнации (coven∪{name}): видна только redis-prod.
	runs, total, err := ListRuns(ctx, integrationPool,
		RunsFilter{}, incarnation.ListScope{Covens: []string{"redis-prod"}}, 0, 50)
	if err != nil {
		t.Fatalf("ListRuns(scope name): %v", err)
	}
	if total != 1 || len(runs) != 1 || runs[0].Incarnation != "redis-prod" {
		t.Fatalf("scope name: total=%d runs=%+v, want только redis-prod", total, runs)
	}

	// scope по covens[]-метке: видна только redis-staging.
	runs, total, err = ListRuns(ctx, integrationPool,
		RunsFilter{}, incarnation.ListScope{Covens: []string{"team-x"}}, 0, 50)
	if err != nil {
		t.Fatalf("ListRuns(scope coven): %v", err)
	}
	if total != 1 || len(runs) != 1 || runs[0].Incarnation != "redis-staging" {
		t.Fatalf("scope coven: total=%d runs=%+v, want только redis-staging", total, runs)
	}

	// fail-closed: пустой scope (не Unrestricted) → ни одного прогона.
	runs, total, err = ListRuns(ctx, integrationPool, RunsFilter{}, incarnation.ListScope{}, 0, 50)
	if err != nil {
		t.Fatalf("ListRuns(empty scope): %v", err)
	}
	if total != 0 || len(runs) != 0 {
		t.Errorf("empty scope: total=%d len=%d, want 0/0 (fail-closed)", total, len(runs))
	}
}

// TestIntegration_SelectRunsStats — сводные счётчики: total + по каждому агрегатному
// статусу, за всё время и за последние 24 часа (старый прогон выпадает из 24h-корзины).
func TestIntegration_SelectRunsStats(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod", "archon-alice")
	seedIncarnation(t, "redis-staging", "archon-alice")
	ctx := context.Background()

	seedRun(t, ctx, "01HST01", "redis-prod", "create", StatusSuccess)
	seedRun(t, ctx, "01HST02", "redis-staging", "create", StatusSuccess)
	seedRun(t, ctx, "01HST03", "redis-prod", "create", StatusFailed)
	seedRun(t, ctx, "01HST04", "redis-staging", "restart", StatusRunning)
	seedRun(t, ctx, "01HST05", "redis-prod", "scale", StatusCancelled)

	// 01HST02 — старше 24 часов: в All есть, в Last24h нет.
	if _, err := integrationPool.Exec(ctx,
		`UPDATE apply_runs SET started_at = now() - interval '2 days' WHERE apply_id = '01HST02'`); err != nil {
		t.Fatalf("UPDATE started_at: %v", err)
	}

	stats, err := SelectRunsStats(ctx, integrationPool, unrestrictedScope)
	if err != nil {
		t.Fatalf("SelectRunsStats: %v", err)
	}
	wantAll := RunsStatusCounts{Total: 5, Applying: 1, Success: 2, Failed: 1, Cancelled: 1}
	if stats.All != wantAll {
		t.Errorf("All = %+v, want %+v", stats.All, wantAll)
	}
	want24h := RunsStatusCounts{Total: 4, Applying: 1, Success: 1, Failed: 1, Cancelled: 1}
	if stats.Last24h != want24h {
		t.Errorf("Last24h = %+v, want %+v", stats.Last24h, want24h)
	}
}

// TestIntegration_SelectRunsStats_Scoped — счётчики в границах Purview-scope:
// только прогоны видимых инкарнаций.
func TestIntegration_SelectRunsStats_Scoped(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod", "archon-alice")
	seedIncarnation(t, "redis-staging", "archon-alice")
	ctx := context.Background()

	seedRun(t, ctx, "01HSS01", "redis-prod", "create", StatusSuccess)
	seedRun(t, ctx, "01HSS02", "redis-staging", "create", StatusFailed)

	stats, err := SelectRunsStats(ctx, integrationPool,
		incarnation.ListScope{Covens: []string{"redis-prod"}})
	if err != nil {
		t.Fatalf("SelectRunsStats(scoped): %v", err)
	}
	want := RunsStatusCounts{Total: 1, Success: 1}
	if stats.All != want {
		t.Errorf("All = %+v, want %+v (чужая инкарнация вне scope)", stats.All, want)
	}
}
