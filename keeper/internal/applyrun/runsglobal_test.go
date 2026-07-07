package applyrun

// Guard-тесты сортировки глобального read-view прогонов (ADR-068 §B1) на чистой
// функции buildRunsOrderBy: whitelist полей + направление + стабильный tie-break
// apply_id DESC + NULLS LAST для finished_at + byte-exact дефолт. Реальный порядок
// строк на PG — в runsglobal_integration_test.go.

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
)

// unrestricted — scope без ограничений (scope-подзапрос не добавляется, args чисты).
var unrestricted = incarnation.ListScope{Unrestricted: true}

// defaultRunsOrderBy — прежний (до ADR-068) хардкод ORDER BY listSQL. Дефолт
// сортировки обязан быть byte-exact ему (guard 6: без sort-параметров ничего не
// меняется).
const defaultRunsOrderBy = "started_at DESC, apply_id DESC"

func TestBuildRunsOrderBy_DefaultByteExact(t *testing.T) {
	got, err := buildRunsOrderBy("", "")
	if err != nil {
		t.Fatalf("buildRunsOrderBy(\"\",\"\"): %v", err)
	}
	if got != defaultRunsOrderBy {
		t.Errorf("дефолт = %q, want byte-exact %q", got, defaultRunsOrderBy)
	}
}

// TestBuildRunsOrderBy_Columns — каждая из 6 whitelist-колонок в обоих
// направлениях даёт корректное выражение со стабильным tie-break (guard 1, 2, 5).
func TestBuildRunsOrderBy_Columns(t *testing.T) {
	cases := []struct {
		sort, dir string
		want      string
	}{
		{"started_at", "asc", "started_at ASC, apply_id DESC"},
		{"started_at", "desc", "started_at DESC, apply_id DESC"},
		{"finished_at", "asc", "finished_at ASC NULLS LAST, apply_id DESC"},
		{"finished_at", "desc", "finished_at DESC NULLS LAST, apply_id DESC"},
		{"status", "asc", "status ASC, apply_id DESC"},
		{"status", "desc", "status DESC, apply_id DESC"},
		{"incarnation", "asc", "incarnation ASC, apply_id DESC"},
		{"incarnation", "desc", "incarnation DESC, apply_id DESC"},
		{"service", "asc", "service ASC, apply_id DESC"},
		{"service", "desc", "service DESC, apply_id DESC"},
		{"scenario", "asc", "scenario ASC, apply_id DESC"},
		{"scenario", "desc", "scenario DESC, apply_id DESC"},
		// Дефолты применяются независимо: пустое поле → started_at, пустое
		// направление → desc.
		{"", "asc", "started_at ASC, apply_id DESC"},
		{"status", "", "status DESC, apply_id DESC"},
	}
	for _, c := range cases {
		got, err := buildRunsOrderBy(c.sort, c.dir)
		if err != nil {
			t.Errorf("buildRunsOrderBy(%q,%q): %v", c.sort, c.dir, err)
			continue
		}
		if got != c.want {
			t.Errorf("buildRunsOrderBy(%q,%q) = %q, want %q", c.sort, c.dir, got, c.want)
		}
		// Tie-break обязателен во всех кейсах (стабильная пагинация).
		if !strings.HasSuffix(got, ", apply_id DESC") {
			t.Errorf("buildRunsOrderBy(%q,%q) = %q: нет tie-break apply_id DESC", c.sort, c.dir, got)
		}
	}
}

// TestBuildRunsOrderBy_FinishedAtNullsLast — applying-прогоны (finished_at IS NULL)
// уходят в конец при ЛЮБОМ направлении (guard 5).
func TestBuildRunsOrderBy_FinishedAtNullsLast(t *testing.T) {
	for _, dir := range []string{"asc", "desc"} {
		got, err := buildRunsOrderBy("finished_at", dir)
		if err != nil {
			t.Fatalf("buildRunsOrderBy(finished_at,%q): %v", dir, err)
		}
		if !strings.Contains(got, "NULLS LAST") {
			t.Errorf("finished_at %s: %q без NULLS LAST", dir, got)
		}
	}
	// NULLS LAST — только для finished_at (у not-null колонок бессмысленно).
	got, _ := buildRunsOrderBy("started_at", "asc")
	if strings.Contains(got, "NULLS LAST") {
		t.Errorf("started_at не должен нести NULLS LAST: %q", got)
	}
}

// TestBuildRunsOrderBy_InvalidField — не-whitelist поле → sentinel (→ 422). Ловим
// и попытку инъекции, и валидную-в-другом-контексте колонку (created_at из
// incarnation-whitelist здесь недопустима) (guard 3).
func TestBuildRunsOrderBy_InvalidField(t *testing.T) {
	for _, bad := range []string{"created_at", "name", "apply_id", "started_at; DROP TABLE apply_runs", "STARTED_AT"} {
		_, err := buildRunsOrderBy(bad, "asc")
		if !errors.Is(err, ErrInvalidRunsSortField) {
			t.Errorf("buildRunsOrderBy(%q): err=%v, want ErrInvalidRunsSortField", bad, err)
		}
	}
}

// TestBuildRunsOrderBy_InvalidDir — не-asc/desc направление → sentinel (→ 422).
// Верхний регистр не проходит (whitelist строгий) (guard 3).
func TestBuildRunsOrderBy_InvalidDir(t *testing.T) {
	for _, bad := range []string{"sideways", "ASC", "DESC", "ascending", "1"} {
		_, err := buildRunsOrderBy("started_at", bad)
		if !errors.Is(err, ErrInvalidRunsSortDir) {
			t.Errorf("buildRunsOrderBy(started_at,%q): err=%v, want ErrInvalidRunsSortDir", bad, err)
		}
	}
}

// TestBuildRunsQuery_ServiceFilterInSub — фильтр service попадает в ПОДЗАПРОС sub
// (а не в outerWhere): значит и countSQL, и listSQL строятся из него → total и
// страница сужаются одинаково. Точное совпадение по i.service, bind-арг.
func TestBuildRunsQuery_ServiceFilterInSub(t *testing.T) {
	sub, outer, args := buildRunsQuery(RunsFilter{Service: "redis"}, unrestricted)
	if !strings.Contains(sub, "i.service = $1") {
		t.Errorf("service-фильтр не в sub (не попадёт в count):\n%s", sub)
	}
	if strings.Contains(outer, "service") {
		t.Errorf("service-фильтр просочился в outerWhere (разъедет total/list): %q", outer)
	}
	if len(args) != 1 || args[0] != "redis" {
		t.Errorf("args = %v, want [redis]", args)
	}
}

// TestBuildRunsQuery_QFilterInSub — свободный поиск q покрывает все 4 колонки
// (incarnation/scenario/service/started_by) одним bind-аргом `%q%`, в подзапросе
// sub (count и list консистентны).
func TestBuildRunsQuery_QFilterInSub(t *testing.T) {
	sub, outer, args := buildRunsQuery(RunsFilter{Q: "ab"}, unrestricted)
	for _, want := range []string{
		"ar.incarnation_name ILIKE $1", "ar.scenario ILIKE $1",
		"i.service ILIKE $1", "ar.started_by_aid ILIKE $1",
	} {
		if !strings.Contains(sub, want) {
			t.Errorf("q-фильтр не покрывает %q:\n%s", want, sub)
		}
	}
	// q — только в sub (симметрия с service): в outerWhere его быть не должно,
	// иначе count (из sub) и list разъедутся.
	if strings.Contains(outer, "ILIKE") {
		t.Errorf("q просочился в outerWhere (разъедет total/list): %q", outer)
	}
	if len(args) != 1 || args[0] != "%ab%" {
		t.Errorf("args = %v, want [%%ab%%]", args)
	}
}

// TestBuildRunsQuery_QScopeAndBinding — РЕГРЕСС (security): q-OR-группа обёрнута
// скобками И AND-связана со scope-условием. Без скобок precedence AND>OR даёт
// `a OR b OR c OR (d AND scope)` → прогоны, матчащие q, обходят Purview-scope
// (утечка). Behavioral-подтверждение — TestIntegration_ListRuns_QScopeBinding.
func TestBuildRunsQuery_QScopeAndBinding(t *testing.T) {
	restricted := incarnation.ListScope{Covens: []string{"team-x"}}
	sub, _, _ := buildRunsQuery(RunsFilter{Q: "term"}, restricted)
	// Открывающая скобка q-группы на месте.
	if !strings.Contains(sub, "(ar.incarnation_name ILIKE $1 OR ar.scenario ILIKE $1") {
		t.Errorf("нет открывающей скобки q-группы (OR не сгруппирован):\n%s", sub)
	}
	// Закрывающая скобка группы вплотную к ` AND <scope-подзапрос>` — группа
	// целиком AND-связана со scope (не «...OR d AND scope»).
	const wantBinding = "ar.started_by_aid ILIKE $1) AND ar.incarnation_name IN (SELECT name FROM incarnation WHERE"
	if !strings.Contains(sub, wantBinding) {
		t.Errorf("q-группа не в скобках / не AND-связана со scope (риск утечки мимо Purview):\n%s", sub)
	}
}

// TestBuildRunsQuery_QStatusCompose — q и status компонуются: q сужает в sub (до
// агрегации), status — в outerWhere (по агрегатному статусу); оба bind-арга по
// порядку. countSQL/listSQL строятся из sub+outerWhere → total и страница
// консистентны под обоими фильтрами одновременно.
func TestBuildRunsQuery_QStatusCompose(t *testing.T) {
	sub, outer, args := buildRunsQuery(RunsFilter{Q: "x", Status: RunStatusFailed}, unrestricted)
	if !strings.Contains(sub, "ILIKE $1") {
		t.Errorf("q не в sub:\n%s", sub)
	}
	if !strings.Contains(outer, "status = $2") {
		t.Errorf("status не в outerWhere: %q", outer)
	}
	if len(args) != 2 || args[0] != "%x%" || args[1] != string(RunStatusFailed) {
		t.Errorf("args = %v, want [%%x%% failed]", args)
	}
}

// TestBuildRunsQuery_TimeFilterInOuter — фильтр по времени старта — по АГРЕГАТУ
// started_at, значит в outerWhere (после GROUP BY), НЕ в sub: иначе фильтр по
// host-строкам расщепил бы группу apply_id. Границы inclusive (>=/<=); count и
// list строятся из sub+outerWhere → total/страница консистентны.
func TestBuildRunsQuery_TimeFilterInOuter(t *testing.T) {
	after := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	before := time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC)
	sub, outer, args := buildRunsQuery(
		RunsFilter{StartedAfter: &after, StartedBefore: &before}, unrestricted)
	if !strings.Contains(outer, "started_at >= $1") || !strings.Contains(outer, "started_at <= $2") {
		t.Errorf("время не в outerWhere с inclusive-границами: %q", outer)
	}
	// В sub времени быть не должно (агрегатный фильтр после GROUP BY).
	if strings.Contains(sub, "started_at >=") || strings.Contains(sub, "started_at <=") {
		t.Errorf("время просочилось в sub (расщепит группу apply_id):\n%s", sub)
	}
	if len(args) != 2 {
		t.Fatalf("args len = %d, want 2", len(args))
	}
	if got, ok := args[0].(time.Time); !ok || !got.Equal(after) {
		t.Errorf("args[0] = %v, want %v (after)", args[0], after)
	}
	if got, ok := args[1].(time.Time); !ok || !got.Equal(before) {
		t.Errorf("args[1] = %v, want %v (before)", args[1], before)
	}
}

// TestBuildRunsQuery_TimeComposeAllFilters — композиция service+q+status+время:
// service/q сужают в sub (до агрегации), status/started_after/started_before — в
// outerWhere (по агрегату), все клаузы AND, позиции bind-аргов сквозные. countSQL/
// listSQL из sub+outerWhere → total консистентен под всеми фильтрами сразу.
func TestBuildRunsQuery_TimeComposeAllFilters(t *testing.T) {
	after := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	before := time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC)
	sub, outer, args := buildRunsQuery(RunsFilter{
		Service: "redis", Q: "x", Status: RunStatusFailed,
		StartedAfter: &after, StartedBefore: &before,
	}, unrestricted)
	// sub-фильтры: service ($1) и q ($2) до GROUP BY, AND-связаны.
	if !strings.Contains(sub, "i.service = $1") || !strings.Contains(sub, "ILIKE $2") {
		t.Errorf("service/q не в sub с правильными позициями:\n%s", sub)
	}
	if !strings.Contains(sub, " AND ") {
		t.Errorf("sub-клаузы не AND-связаны:\n%s", sub)
	}
	// outer-фильтры: status ($3), started_at >= ($4), started_at <= ($5), все AND.
	for _, want := range []string{"status = $3", "started_at >= $4", "started_at <= $5"} {
		if !strings.Contains(outer, want) {
			t.Errorf("outerWhere без %q: %q", want, outer)
		}
	}
	if strings.Count(outer, " AND ") != 2 {
		t.Errorf("outer-клаузы не полностью AND-связаны (want 2 AND): %q", outer)
	}
	if len(args) != 5 {
		t.Errorf("args len = %d, want 5 (service,q,status,after,before)", len(args))
	}
}

// TestBuildRunsQuery_QEscapesLikeMeta — LIKE-метасимволы %/_/\ в q экранируются
// (литеральные, не wildcard): паттерн `%…%` несёт `\%`, `\_`, `\\`.
func TestBuildRunsQuery_QEscapesLikeMeta(t *testing.T) {
	_, _, args := buildRunsQuery(RunsFilter{Q: `a%b_c\d`}, unrestricted)
	if len(args) != 1 || args[0] != `%a\%b\_c\\d%` {
		t.Errorf("q-арг = %q, want %q", args[0], `%a\%b\_c\\d%`)
	}
}

// TestBuildRunsQuery_ServiceAndQArgOrder — service и q — оба bind-арга по порядку
// (service=$1, q=$2), не путаются позиции при совместном использовании.
func TestBuildRunsQuery_ServiceAndQArgOrder(t *testing.T) {
	sub, _, args := buildRunsQuery(RunsFilter{Service: "redis", Q: "x"}, unrestricted)
	if !strings.Contains(sub, "i.service = $1") || !strings.Contains(sub, "ILIKE $2") {
		t.Errorf("порядок bind-аргов нарушен (service=$1, q=$2):\n%s", sub)
	}
	if len(args) != 2 || args[0] != "redis" || args[1] != "%x%" {
		t.Errorf("args = %v, want [redis %%x%%]", args)
	}
}

// TestEscapeLike — экранирование LIKE-метасимволов одним проходом (backslash
// первым — без двойного экранирования уже вставленных).
func TestEscapeLike(t *testing.T) {
	cases := map[string]string{
		"plain": "plain",
		"50%":   `50\%`,
		"a_b":   `a\_b`,
		`c\d`:   `c\\d`,
		`%_\`:   `\%\_\\`,
		"":      "",
	}
	for in, want := range cases {
		if got := escapeLike(in); got != want {
			t.Errorf("escapeLike(%q) = %q, want %q", in, got, want)
		}
	}
}
