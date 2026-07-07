//go:build integration

// Integration-тесты глобального read-view прогонов (GET /v1/runs + /v1/runs/stats):
// свёртка apply_runs по apply_id ЧЕРЕЗ ВСЕ инкарнации, SQL-форма AggregateRunStatus,
// фильтры status/incarnation, Purview-scope-подзапрос и сводные счётчики.
// Использует harness из integration_test.go (testcontainers PG).

package applyrun

import (
	"context"
	"testing"
	"time"

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

// TestIntegration_ListRuns_Sort — сортировка по whitelist-колонке на реальном PG
// (ADR-068 §B1): порядок по колонке в обоих направлениях, стабильный tie-break
// apply_id DESC при равных значениях, NULLS LAST для finished_at (applying-прогон
// в конец), total не зависит от sort.
func TestIntegration_ListRuns_Sort(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod", "archon-alice")
	seedIncarnation(t, "redis-staging", "archon-alice")
	ctx := context.Background()

	// apply_id лексикографически возрастают A<B<C<D → tie-break apply_id DESC даёт
	// D,C,B,A. Два прогона в redis-prod (A,B) и redis-staging (C,D); D — applying
	// (running-хост, finished_at IS NULL).
	seedRun(t, ctx, "01HSORTA", "redis-prod", "create", StatusSuccess)
	seedRun(t, ctx, "01HSORTB", "redis-prod", "restart", StatusSuccess)
	seedRun(t, ctx, "01HSORTC", "redis-staging", "create", StatusSuccess)
	seedRun(t, ctx, "01HSORTD", "redis-staging", "scale", StatusRunning)

	ids := func(runs []RunSummary) []string {
		out := make([]string, len(runs))
		for i, r := range runs {
			out[i] = r.ApplyID
		}
		return out
	}
	eq := func(got, want []string) bool {
		if len(got) != len(want) {
			return false
		}
		for i := range got {
			if got[i] != want[i] {
				return false
			}
		}
		return true
	}

	// incarnation ASC: redis-prod < redis-staging; внутри группы tie-break
	// apply_id DESC → [B,A, D,C].
	runs, total, err := ListRuns(ctx, integrationPool,
		RunsFilter{Sort: "incarnation", SortDir: "asc"}, unrestrictedScope, 0, 50)
	if err != nil {
		t.Fatalf("ListRuns(incarnation asc): %v", err)
	}
	if total != 4 {
		t.Errorf("incarnation asc: total=%d, want 4", total)
	}
	if want := []string{"01HSORTB", "01HSORTA", "01HSORTD", "01HSORTC"}; !eq(ids(runs), want) {
		t.Errorf("incarnation asc: порядок %v, want %v", ids(runs), want)
	}

	// incarnation DESC: redis-staging первой → [D,C, B,A].
	runs, totalDesc, err := ListRuns(ctx, integrationPool,
		RunsFilter{Sort: "incarnation", SortDir: "desc"}, unrestrictedScope, 0, 50)
	if err != nil {
		t.Fatalf("ListRuns(incarnation desc): %v", err)
	}
	if want := []string{"01HSORTD", "01HSORTC", "01HSORTB", "01HSORTA"}; !eq(ids(runs), want) {
		t.Errorf("incarnation desc: порядок %v, want %v", ids(runs), want)
	}
	// total не зависит от направления сортировки (guard 4).
	if totalDesc != total {
		t.Errorf("total разошёлся при смене sort_dir: asc=%d desc=%d", total, totalDesc)
	}

	// finished_at: applying-прогон (01HSORTD, finished_at IS NULL) — в конце при
	// ОБОИХ направлениях (NULLS LAST).
	for _, dir := range []string{"asc", "desc"} {
		runs, _, err = ListRuns(ctx, integrationPool,
			RunsFilter{Sort: "finished_at", SortDir: dir}, unrestrictedScope, 0, 50)
		if err != nil {
			t.Fatalf("ListRuns(finished_at %s): %v", dir, err)
		}
		if got := ids(runs); len(got) != 4 || got[len(got)-1] != "01HSORTD" {
			t.Errorf("finished_at %s: applying-прогон не в конце (NULLS LAST): %v", dir, got)
		}
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

// runIDs — apply_id-ы страницы в порядке выдачи (helper сравнения порядка).
func runIDs(runs []RunSummary) []string {
	out := make([]string, len(runs))
	for i, r := range runs {
		out[i] = r.ApplyID
	}
	return out
}

// eqIDs — поэлементное сравнение порядка apply_id.
func eqIDs(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// TestIntegration_ListRuns_ServiceFilter — фильтр по сервису инкарнации-владельца
// (JOIN incarnation): list и total сужаются ОДИНАКОВО (общий подзапрос sub у count
// и list); проекция несёт service; несуществующий сервис → пусто.
func TestIntegration_ListRuns_ServiceFilter(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod", "archon-alice") // service redis (дефолт seed)
	seedIncarnation(t, "pg-main", "archon-alice")
	ctx := context.Background()

	// pg-main → сервис postgres (seedIncarnation ставит redis по умолчанию).
	if _, err := integrationPool.Exec(ctx,
		`UPDATE incarnation SET service = 'postgres' WHERE name = 'pg-main'`); err != nil {
		t.Fatalf("UPDATE service: %v", err)
	}

	seedRun(t, ctx, "01HSVC01", "redis-prod", "create", StatusSuccess)
	seedRun(t, ctx, "01HSVC02", "pg-main", "create", StatusSuccess)

	runs, total, err := ListRuns(ctx, integrationPool, RunsFilter{Service: "redis"}, unrestrictedScope, 0, 50)
	if err != nil {
		t.Fatalf("ListRuns(service=redis): %v", err)
	}
	if total != 1 || len(runs) != 1 || runs[0].ApplyID != "01HSVC01" {
		t.Fatalf("service=redis: total=%d runs=%+v, want только 01HSVC01", total, runs)
	}
	if runs[0].Service != "redis" {
		t.Errorf("Service = %q, want redis (проекция service)", runs[0].Service)
	}

	runs, total, err = ListRuns(ctx, integrationPool, RunsFilter{Service: "postgres"}, unrestrictedScope, 0, 50)
	if err != nil {
		t.Fatalf("ListRuns(service=postgres): %v", err)
	}
	if total != 1 || len(runs) != 1 || runs[0].ApplyID != "01HSVC02" || runs[0].Service != "postgres" {
		t.Fatalf("service=postgres: total=%d runs=%+v, want только 01HSVC02/postgres", total, runs)
	}

	// Несуществующий сервис — list и total оба 0.
	runs, total, err = ListRuns(ctx, integrationPool, RunsFilter{Service: "mongo"}, unrestrictedScope, 0, 50)
	if err != nil {
		t.Fatalf("ListRuns(service=mongo): %v", err)
	}
	if total != 0 || len(runs) != 0 {
		t.Errorf("service=mongo: total=%d len=%d, want 0/0", total, len(runs))
	}
}

// TestIntegration_ListRuns_QFilter — свободный поиск q матчит по 4 полям
// (incarnation/scenario/service/started_by), регистронезависимо (ILIKE); list и
// total консистентны; LIKE-метасимвол '%' экранирован (литеральный, не wildcard).
func TestIntegration_ListRuns_QFilter(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedOperator(t, "archon-zephyr")
	seedIncarnation(t, "alpha-inc", "archon-alice")
	seedIncarnation(t, "beta-inc", "archon-alice")
	seedIncarnation(t, "gamma-inc", "archon-alice")
	seedIncarnation(t, "delta-inc", "archon-alice")
	ctx := context.Background()

	// gamma-inc → уникальный сервис mysvc (у остальных service='redis').
	if _, err := integrationPool.Exec(ctx,
		`UPDATE incarnation SET service = 'mysvc' WHERE name = 'gamma-inc'`); err != nil {
		t.Fatalf("UPDATE service: %v", err)
	}

	seedRun(t, ctx, "01HQAAA", "alpha-inc", "create", StatusSuccess)    // токен в incarnation
	seedRun(t, ctx, "01HQBBB", "beta-inc", "restartxyz", StatusSuccess) // токен в scenario
	seedRun(t, ctx, "01HQCCC", "gamma-inc", "create", StatusSuccess)    // токен в service
	aidZ := "archon-zephyr"
	mustInsertRun(t, ctx, "01HQDDD", "host-z", "delta-inc", "create", StatusRunning, &aidZ) // токен в started_by

	cases := []struct{ q, want string }{
		{"alpha", "01HQAAA"},      // incarnation
		{"restartxyz", "01HQBBB"}, // scenario
		{"mysvc", "01HQCCC"},      // service
		{"zephyr", "01HQDDD"},     // started_by
		{"ALPHA", "01HQAAA"},      // регистронезависимо (ILIKE)
	}
	for _, c := range cases {
		runs, total, err := ListRuns(ctx, integrationPool, RunsFilter{Q: c.q}, unrestrictedScope, 0, 50)
		if err != nil {
			t.Fatalf("ListRuns(q=%q): %v", c.q, err)
		}
		got := ""
		if len(runs) == 1 {
			got = runs[0].ApplyID
		}
		if total != 1 || len(runs) != 1 || got != c.want {
			t.Errorf("q=%q: total=%d len=%d got=%q, want ровно %s", c.q, total, len(runs), got, c.want)
		}
	}

	// '%' в q — литеральный (экранирован), не wildcard «матчит всё».
	runs, total, err := ListRuns(ctx, integrationPool, RunsFilter{Q: "%"}, unrestrictedScope, 0, 50)
	if err != nil {
		t.Fatalf("ListRuns(q=%%): %v", err)
	}
	if total != 0 || len(runs) != 0 {
		t.Errorf("q=%%: total=%d len=%d, want 0/0 (метасимвол экранирован)", total, len(runs))
	}
}

// TestIntegration_ListRuns_QScopeBinding — РЕГРЕСС (security): свободный поиск q
// AND-связан с Purview-scope (скобочная OR-группа). Две инкарнации с прогонами,
// чей scenario матчит ОДИН q-терм; scope ограничен одной. q=<терм> под scope →
// виден ТОЛЬКО in-scope прогон, out-of-scope НЕ утекает, total считает только
// in-scope. Ловит потерю скобок: `scope AND a OR b OR c OR d` = утечка мимо scope.
func TestIntegration_ListRuns_QScopeBinding(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "in-scope-inc", "archon-alice")
	seedIncarnation(t, "out-scope-inc", "archon-alice")
	ctx := context.Background()

	// Обе инкарнации: прогон со scenario, матчащим один и тот же q-терм.
	seedRun(t, ctx, "01HQBIN", "in-scope-inc", "sharedterm", StatusSuccess)
	seedRun(t, ctx, "01HQBOUT", "out-scope-inc", "sharedterm", StatusSuccess)

	// Ограниченный scope (coven∪{name}) — видна только in-scope-inc.
	scope := incarnation.ListScope{Covens: []string{"in-scope-inc"}}
	runs, total, err := ListRuns(ctx, integrationPool, RunsFilter{Q: "sharedterm"}, scope, 0, 50)
	if err != nil {
		t.Fatalf("ListRuns(q под ограниченным scope): %v", err)
	}
	// q-матч out-of-scope прогона НЕ должен обойти scope: ровно один in-scope.
	if total != 1 || len(runs) != 1 || runs[0].ApplyID != "01HQBIN" {
		t.Fatalf("q под scope: total=%d runs=%v, want ровно 01HQBIN (out-of-scope утёк мимо Purview?)",
			total, runIDs(runs))
	}
	if runs[0].Incarnation != "in-scope-inc" {
		t.Errorf("утечка прогона вне Purview-scope: %+v", runs[0])
	}
}

// TestIntegration_ListRuns_TimeRange — фильтр по диапазону времени СТАРТА прогона
// (агрегат MIN(started_at)) в outerWhere: границы inclusive, list и total сужаются
// ОДИНАКОВО (общий sub+outerWhere), стабильный tie-break apply_id DESC при равном
// started_at.
func TestIntegration_ListRuns_TimeRange(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "redis-prod", "archon-alice")
	ctx := context.Background()

	mustTime := func(s string) time.Time {
		tm, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatalf("parse %s: %v", s, err)
		}
		return tm
	}
	// 4 прогона; started_at выставляем явно. B и C — на одно время t2 (tie-break).
	seedRun(t, ctx, "01HTMA", "redis-prod", "create", StatusSuccess)
	seedRun(t, ctx, "01HTMB", "redis-prod", "create", StatusSuccess)
	seedRun(t, ctx, "01HTMC", "redis-prod", "create", StatusSuccess)
	seedRun(t, ctx, "01HTMD", "redis-prod", "create", StatusSuccess)
	tt1, tt2, tt3 := mustTime("2026-01-01T00:00:00Z"), mustTime("2026-06-01T00:00:00Z"), mustTime("2026-12-01T00:00:00Z")
	setStart := func(applyID string, ts time.Time) {
		if _, err := integrationPool.Exec(ctx,
			`UPDATE apply_runs SET started_at = $1 WHERE apply_id = $2`, ts, applyID); err != nil {
			t.Fatalf("UPDATE started_at %s: %v", applyID, err)
		}
	}
	setStart("01HTMA", tt1)
	setStart("01HTMB", tt2)
	setStart("01HTMC", tt2)
	setStart("01HTMD", tt3)

	// started_after=t2 (inclusive): {t2,t2,t3} = B,C,D. Дефолт-сорт started_at DESC,
	// tie-break apply_id DESC внутри t2 → [D, C, B].
	runs, total, err := ListRuns(ctx, integrationPool, RunsFilter{StartedAfter: &tt2}, unrestrictedScope, 0, 50)
	if err != nil {
		t.Fatalf("ListRuns(after=t2): %v", err)
	}
	if total != 3 || len(runs) != 3 {
		t.Fatalf("after=t2: total=%d len=%d, want 3/3 (граница inclusive)", total, len(runs))
	}
	if want := []string{"01HTMD", "01HTMC", "01HTMB"}; !eqIDs(runIDs(runs), want) {
		t.Errorf("after=t2: порядок %v, want %v (tie-break apply_id DESC при равном started_at)", runIDs(runs), want)
	}

	// started_before=t2 (inclusive): {t1,t2,t2} = A,B,C; total=3.
	runs, total, err = ListRuns(ctx, integrationPool, RunsFilter{StartedBefore: &tt2}, unrestrictedScope, 0, 50)
	if err != nil {
		t.Fatalf("ListRuns(before=t2): %v", err)
	}
	if total != 3 || len(runs) != 3 {
		t.Fatalf("before=t2: total=%d len=%d, want 3/3 (граница inclusive)", total, len(runs))
	}

	// Диапазон [t2,t2] — ровно прогоны со started_at == t2 (обе границы inclusive):
	// C,B; list и total одинаковы.
	runs, total, err = ListRuns(ctx, integrationPool,
		RunsFilter{StartedAfter: &tt2, StartedBefore: &tt2}, unrestrictedScope, 0, 50)
	if err != nil {
		t.Fatalf("ListRuns([t2,t2]): %v", err)
	}
	if total != 2 || len(runs) != 2 {
		t.Fatalf("[t2,t2]: total=%d len=%d, want 2/2 (обе границы inclusive)", total, len(runs))
	}
	if want := []string{"01HTMC", "01HTMB"}; !eqIDs(runIDs(runs), want) {
		t.Errorf("[t2,t2]: порядок %v, want %v", runIDs(runs), want)
	}

	// Пустой диапазон (after позже before) — безвредно пусто (after>before не
	// валидируется жёстко на handler-е).
	runs, total, err = ListRuns(ctx, integrationPool,
		RunsFilter{StartedAfter: &tt3, StartedBefore: &tt1}, unrestrictedScope, 0, 50)
	if err != nil {
		t.Fatalf("ListRuns(after>before): %v", err)
	}
	if total != 0 || len(runs) != 0 {
		t.Errorf("after>before: total=%d len=%d, want 0/0 (пустая выдача безвредна)", total, len(runs))
	}
}

// TestIntegration_ListRuns_TimeScopeBinding — РЕГРЕСС (security): фильтр по времени
// AND-связан с Purview-scope и не обходит его. Симметричен
// [TestIntegration_ListRuns_QScopeBinding], но пользовательский фильтр — диапазон
// времени, а не q. Ключевое отличие раскладки bind-параметров: scope сидит в
// подзапросе `sub` (ДО GROUP BY, $1), а время — в `outerWhere` (ПОСЛЕ GROUP BY, $2).
// Пинним, что позиции не разъезжаются и время не «протаскивает» out-of-scope прогон
// мимо scope. Три прогона под ограниченным scope в разных точках времени:
//   - in-scope + в окне     → единственный видимый;
//   - out-of-scope + в окне → отсечён scope ($1), НЕ утечка через время;
//   - in-scope + до окна    → отсечён нижней границей времени ($2), — доказывает,
//     что предикат времени реально активен, а не no-op под scope.
func TestIntegration_ListRuns_TimeScopeBinding(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "in-scope-inc", "archon-alice")
	seedIncarnation(t, "out-scope-inc", "archon-alice")
	ctx := context.Background()

	mustTime := func(s string) time.Time {
		tm, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatalf("parse %s: %v", s, err)
		}
		return tm
	}
	setStart := func(applyID string, ts time.Time) {
		if _, err := integrationPool.Exec(ctx,
			`UPDATE apply_runs SET started_at = $1 WHERE apply_id = $2`, ts, applyID); err != nil {
			t.Fatalf("UPDATE started_at %s: %v", applyID, err)
		}
	}
	// Окно — StartedAfter=tWindow (inclusive); tBefore заведомо раньше окна.
	tBefore, tWindow := mustTime("2026-01-01T00:00:00Z"), mustTime("2026-06-01T00:00:00Z")

	seedRun(t, ctx, "01HTSIN", "in-scope-inc", "create", StatusSuccess)   // in-scope, в окне
	seedRun(t, ctx, "01HTSOUT", "out-scope-inc", "create", StatusSuccess) // out-of-scope, в окне
	seedRun(t, ctx, "01HTSOLD", "in-scope-inc", "create", StatusSuccess)  // in-scope, ДО окна
	setStart("01HTSIN", tWindow)
	setStart("01HTSOUT", tWindow)
	setStart("01HTSOLD", tBefore)

	// Ограниченный scope (coven∪{name}) — видна только in-scope-inc; окно отсекает
	// всё раньше tWindow.
	scope := incarnation.ListScope{Covens: []string{"in-scope-inc"}}
	runs, total, err := ListRuns(ctx, integrationPool,
		RunsFilter{StartedAfter: &tWindow}, scope, 0, 50)
	if err != nil {
		t.Fatalf("ListRuns(время под ограниченным scope): %v", err)
	}
	// (1) out-of-scope прогон в окне НЕ обходит scope через время; (2) total считает
	// ТОЛЬКО in-scope прогоны в окне (out-of-scope и in-scope-до-окна не в счёте).
	if total != 1 || len(runs) != 1 || runs[0].ApplyID != "01HTSIN" {
		t.Fatalf("время под scope: total=%d runs=%v, want ровно 01HTSIN (out-of-scope утёк мимо Purview через время?)",
			total, runIDs(runs))
	}
	if runs[0].Incarnation != "in-scope-inc" {
		t.Errorf("утечка прогона вне Purview-scope через фильтр времени: %+v", runs[0])
	}
}

// TestIntegration_ListRuns_SortService — сортировка по service (ADR-068 whitelist):
// порядок по сервису в обоих направлениях, стабильный tie-break apply_id DESC при
// равном сервисе; total не зависит от направления.
func TestIntegration_ListRuns_SortService(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	seedIncarnation(t, "inc-a", "archon-alice")
	seedIncarnation(t, "inc-b", "archon-alice")
	seedIncarnation(t, "inc-c", "archon-alice")
	ctx := context.Background()

	// inc-a,inc-b → service 'aaa' (равные → tie-break apply_id DESC); inc-c → 'bbb'.
	if _, err := integrationPool.Exec(ctx, `
		UPDATE incarnation SET service = CASE name
			WHEN 'inc-c' THEN 'bbb' ELSE 'aaa' END
		WHERE name IN ('inc-a','inc-b','inc-c')`); err != nil {
		t.Fatalf("UPDATE service: %v", err)
	}

	// apply_id AA<AB<AC → tie-break apply_id DESC внутри service='aaa' даёт AB,AA.
	seedRun(t, ctx, "01HSVAA", "inc-a", "create", StatusSuccess) // service aaa
	seedRun(t, ctx, "01HSVAB", "inc-b", "create", StatusSuccess) // service aaa
	seedRun(t, ctx, "01HSVAC", "inc-c", "create", StatusSuccess) // service bbb

	// service ASC: aaa (tie-break AB,AA), затем bbb (AC).
	runs, total, err := ListRuns(ctx, integrationPool,
		RunsFilter{Sort: "service", SortDir: "asc"}, unrestrictedScope, 0, 50)
	if err != nil {
		t.Fatalf("ListRuns(service asc): %v", err)
	}
	if total != 3 {
		t.Errorf("service asc: total=%d, want 3", total)
	}
	if want := []string{"01HSVAB", "01HSVAA", "01HSVAC"}; !eqIDs(runIDs(runs), want) {
		t.Errorf("service asc: порядок %v, want %v", runIDs(runs), want)
	}

	// service DESC: bbb (AC) первым, затем aaa (tie-break AB,AA).
	runs, totalDesc, err := ListRuns(ctx, integrationPool,
		RunsFilter{Sort: "service", SortDir: "desc"}, unrestrictedScope, 0, 50)
	if err != nil {
		t.Fatalf("ListRuns(service desc): %v", err)
	}
	if want := []string{"01HSVAC", "01HSVAB", "01HSVAA"}; !eqIDs(runIDs(runs), want) {
		t.Errorf("service desc: порядок %v, want %v", runIDs(runs), want)
	}
	if totalDesc != total {
		t.Errorf("total разошёлся при смене sort_dir: asc=%d desc=%d", total, totalDesc)
	}
}
