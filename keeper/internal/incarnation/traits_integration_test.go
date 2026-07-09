//go:build integration

// Integration-guard релокации Trait per-soul → per-incarnation (ADR-060 amend,
// R1): incarnation.traits round-trip + sync-hook проекция в souls.traits
// хостов-членов (на create-эмуляции и на bind нового хоста) + сохранность
// read-слоя souls.traits (projection target) под containment-таргетинг (where:
// soulprint.self.traits.<key> опирается на тот же jsonb).

package incarnation

import (
	"context"
	"fmt"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/soul"
)

// seedSoul вставляет минимальную souls-строку с заданными coven (членство в
// инкарнации = имя инкарнации ∈ coven, ADR-008). traits — пустой `{}` (projection
// target до sync-hook-а).
func seedSoul(t *testing.T, sid string, coven []string) {
	t.Helper()
	_, err := integrationPool.Exec(context.Background(),
		`INSERT INTO souls (sid, transport, status, coven, traits)
		 VALUES ($1, 'agent', 'connected', $2, '{}'::jsonb)`,
		sid, coven)
	if err != nil {
		t.Fatalf("seedSoul(%s): %v", sid, err)
	}
}

func soulTraits(t *testing.T, sid string) map[string]any {
	t.Helper()
	got, err := soul.SelectBySID(context.Background(), integrationPool, sid)
	if err != nil {
		t.Fatalf("SelectBySID(%s): %v", sid, err)
	}
	return got.Traits
}

func resetSouls(t *testing.T) {
	t.Helper()
	if _, err := integrationPool.Exec(context.Background(), `TRUNCATE TABLE souls CASCADE`); err != nil {
		t.Fatalf("TRUNCATE souls: %v", err)
	}
}

// TestIntegration_IncarnationTraits_RoundTrip — incarnation.traits пишется в
// колонку и читается обратно (источник истины Trait, R1).
func TestIntegration_IncarnationTraits_RoundTrip(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	ctx := context.Background()

	creator := "archon-alice"
	inc := &Incarnation{
		Name: "redis-prod", Service: "redis", ServiceVersion: "v1",
		StateSchemaVersion: 1, Status: StatusReady, CreatedByAID: &creator,
		Traits: map[string]any{
			"team":   "dba",
			"owners": []any{"alice", "bob"},
		},
	}
	if err := Create(ctx, integrationPool, inc); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := SelectByName(ctx, integrationPool, "redis-prod")
	if err != nil {
		t.Fatalf("SelectByName: %v", err)
	}
	if got.Traits["team"] != "dba" {
		t.Errorf("Traits.team = %v, want dba", got.Traits["team"])
	}
	owners, ok := got.Traits["owners"].([]any)
	if !ok || len(owners) != 2 || owners[0] != "alice" {
		t.Errorf("Traits.owners = %v", got.Traits["owners"])
	}

	// Пустой traits → `{}` (NOT NULL DEFAULT), не nil.
	inc2 := &Incarnation{
		Name: "redis-dev", Service: "redis", ServiceVersion: "v1",
		StateSchemaVersion: 1, Status: StatusReady, CreatedByAID: &creator,
	}
	if err := Create(ctx, integrationPool, inc2); err != nil {
		t.Fatalf("Create#2: %v", err)
	}
	got2, _ := SelectByName(ctx, integrationPool, "redis-dev")
	if len(got2.Traits) != 0 {
		t.Errorf("empty traits = %v, want empty map", got2.Traits)
	}
}

// TestIntegration_SyncTraitsToHosts_ProjectsToMembers — sync-hook проецирует
// incarnation.traits в souls.traits ВСЕХ хостов-членов (coven ∋ incName) и НЕ
// трогает чужие хосты.
func TestIntegration_SyncTraitsToHosts_ProjectsToMembers(t *testing.T) {
	resetAll(t)
	resetSouls(t)
	ctx := context.Background()

	// Два члена redis-prod + один чужой хост (другая инкарнация).
	seedSoul(t, "host-a.example.com", []string{"redis-prod", "dc1"})
	seedSoul(t, "host-b.example.com", []string{"redis-prod"})
	seedSoul(t, "outsider.example.com", []string{"other-inc"})

	traits := map[string]any{"team": "dba", "env": "prod"}
	if err := SyncTraitsToHosts(ctx, integrationPool, "redis-prod", traits); err != nil {
		t.Fatalf("SyncTraitsToHosts: %v", err)
	}

	for _, sid := range []string{"host-a.example.com", "host-b.example.com"} {
		got := soulTraits(t, sid)
		if got["team"] != "dba" || got["env"] != "prod" {
			t.Errorf("%s souls.traits = %v, want team=dba env=prod (projection)", sid, got)
		}
	}
	// Чужой хост не затронут.
	if got := soulTraits(t, "outsider.example.com"); len(got) != 0 {
		t.Errorf("outsider souls.traits = %v, want empty (вне инкарнации)", got)
	}
}

// TestIntegration_SyncTraitsToHosts_NewHostPicksUp — bind-сценарий: хост,
// привязавшийся к инкарнации ПОСЛЕ её create, при повторном sync подхватывает
// traits своей инкарнации (идемпотентная replace-проекция).
func TestIntegration_SyncTraitsToHosts_NewHostPicksUp(t *testing.T) {
	resetAll(t)
	resetSouls(t)
	ctx := context.Background()

	seedSoul(t, "host-a.example.com", []string{"redis-prod"})
	traits := map[string]any{"team": "dba"}
	if err := SyncTraitsToHosts(ctx, integrationPool, "redis-prod", traits); err != nil {
		t.Fatalf("SyncTraitsToHosts#1: %v", err)
	}

	// Новый хост привязался к инкарнации (bind через core.soul.registered);
	// его souls.traits ещё пуст.
	seedSoul(t, "host-c.example.com", []string{"redis-prod"})
	if got := soulTraits(t, "host-c.example.com"); len(got) != 0 {
		t.Fatalf("new host pre-sync traits = %v, want empty", got)
	}

	// Повторный sync (bind-хук) проецирует на ВСЕХ членов, включая нового.
	if err := SyncTraitsToHosts(ctx, integrationPool, "redis-prod", traits); err != nil {
		t.Fatalf("SyncTraitsToHosts#2: %v", err)
	}
	if got := soulTraits(t, "host-c.example.com"); got["team"] != "dba" {
		t.Errorf("new host post-sync traits = %v, want team=dba", got)
	}
}

// TestIntegration_ProjectedTraits_ContainmentTargeting — read-слой (projection
// target souls.traits) продолжает обслуживать containment-таргетинг по
// спроецированным traits (фундамент where: soulprint.self.traits.<key>, тот же
// jsonb @>). Проверяем сам PG-предикат на спроецированных данных.
func TestIntegration_ProjectedTraits_ContainmentTargeting(t *testing.T) {
	resetAll(t)
	resetSouls(t)
	ctx := context.Background()

	seedSoul(t, "host-a.example.com", []string{"redis-prod"})
	seedSoul(t, "host-b.example.com", []string{"redis-prod"})
	seedSoul(t, "outsider.example.com", []string{"other-inc"})

	if err := SyncTraitsToHosts(ctx, integrationPool, "redis-prod", map[string]any{"team": "dba"}); err != nil {
		t.Fatalf("SyncTraitsToHosts: %v", err)
	}

	var n int
	err := integrationPool.QueryRow(ctx,
		`SELECT count(*) FROM souls WHERE traits @> '{"team":"dba"}'::jsonb`).Scan(&n)
	if err != nil {
		t.Fatalf("containment query: %v", err)
	}
	if n != 2 {
		t.Errorf("containment matched %d souls, want 2 (только спроецированные члены)", n)
	}
}

// TestIntegration_UpdateTraits_PersistsAndReturnsKeys — операционный PUT-путь: целостная
// замена incarnation.traits персистится в колонку, OldKeys/NewKeys корректны.
func TestIntegration_UpdateTraits_PersistsAndReturnsKeys(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	ctx := context.Background()

	creator := "archon-alice"
	if err := Create(ctx, integrationPool, &Incarnation{
		Name: "redis-prod", Service: "redis", ServiceVersion: "v1",
		StateSchemaVersion: 1, Status: StatusReady, CreatedByAID: &creator,
		Traits: map[string]any{"team": "dba"},
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	res, err := UpdateTraits(ctx, integrationPool, "redis-prod",
		map[string]any{"env": "prod", "az": "a"})
	if err != nil {
		t.Fatalf("UpdateTraits: %v", err)
	}
	if len(res.OldKeys) != 1 || res.OldKeys[0] != "team" {
		t.Errorf("OldKeys = %v, want [team]", res.OldKeys)
	}
	if len(res.NewKeys) != 2 || res.NewKeys[0] != "az" || res.NewKeys[1] != "env" {
		t.Errorf("NewKeys = %v, want [az env] (sorted)", res.NewKeys)
	}

	// Колонка заменена ЦЕЛИКОМ (старый ключ team исчез).
	got, _ := SelectByName(ctx, integrationPool, "redis-prod")
	if got.Traits["env"] != "prod" || got.Traits["az"] != "a" {
		t.Errorf("persisted traits = %v, want env=prod az=a", got.Traits)
	}
	if _, stillThere := got.Traits["team"]; stillThere {
		t.Errorf("persisted traits still has team — replace должен затереть весь map: %v", got.Traits)
	}
}

// TestIntegration_UpdateTraits_EmptyClears — пустой map очищает метки (колонка → `{}`).
func TestIntegration_UpdateTraits_EmptyClears(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	ctx := context.Background()

	creator := "archon-alice"
	if err := Create(ctx, integrationPool, &Incarnation{
		Name: "redis-prod", Service: "redis", ServiceVersion: "v1",
		StateSchemaVersion: 1, Status: StatusReady, CreatedByAID: &creator,
		Traits: map[string]any{"team": "dba"},
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	res, err := UpdateTraits(ctx, integrationPool, "redis-prod", map[string]any{})
	if err != nil {
		t.Fatalf("UpdateTraits(empty): %v", err)
	}
	if len(res.NewKeys) != 0 {
		t.Errorf("NewKeys = %v, want [] (очистка)", res.NewKeys)
	}
	got, _ := SelectByName(ctx, integrationPool, "redis-prod")
	if len(got.Traits) != 0 {
		t.Errorf("traits after clear = %v, want empty", got.Traits)
	}
}

// TestIntegration_UpdateTraits_NotFound — несуществующая инкарнация → ErrIncarnationNotFound.
func TestIntegration_UpdateTraits_NotFound(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	_, err := UpdateTraits(ctx, integrationPool, "nope", map[string]any{"team": "dba"})
	if err == nil {
		t.Fatal("UpdateTraits(missing) returned nil")
	}
}

// TestIntegration_PgContainmentMatchesArrayWithScalarRHS — ПОДТВЕРЖДЕНИЕ корня
// BUG #1 на реальном PG (psql-эквивалент из ТЗ): jsonb-containment `@>` со скаляр-
// RHS МАТЧИТ массив (array-contains-primitive, PG §8.14.3). Именно это делало
// старое SQL-плечо `traits @> '{"env":"prod"}'::jsonb` истинным для list-Trait
// `{"env":["prod","stage"]}`, расходясь с GET-путём traitScalarEquals (list→false).
// Фикс заменил containment на scalar-equality `traits->>$ = $`, которое массив НЕ
// матчит — что и проверяет соседний скаляр-плечевой тест ниже.
func TestIntegration_PgContainmentMatchesArrayWithScalarRHS(t *testing.T) {
	ctx := context.Background()
	var containmentMatch, scalarMatch bool
	// @> со скаляр-RHS против массива → TRUE (корень бага).
	if err := integrationPool.QueryRow(ctx,
		`SELECT '{"env":["prod","stage"]}'::jsonb @> '{"env":"prod"}'::jsonb`).Scan(&containmentMatch); err != nil {
		t.Fatalf("containment probe: %v", err)
	}
	if !containmentMatch {
		t.Error("PG @> со скаляр-RHS НЕ сматчил массив — предпосылка BUG #1 не воспроизвелась (ожидался TRUE)")
	}
	// scalar-форма `->>` против массива → текст массива ≠ 'prod' → FALSE (фикс).
	if err := integrationPool.QueryRow(ctx,
		`SELECT ('{"env":["prod","stage"]}'::jsonb)->>'env' = 'prod'`).Scan(&scalarMatch); err != nil {
		t.Fatalf("scalar probe: %v", err)
	}
	if scalarMatch {
		t.Error("scalar `->>'env' = 'prod'` сматчил массив — фикс не закрыл рассинхрон (ожидался FALSE)")
	}
}

// TestIntegration_ScopeTrait_ListVsScalar_ConsistentWithGet — ГЛАВНЫЙ guard
// BUG #1: List-путь (SelectAll trait-scope SQL) и GET-путь (traitScalarEquals,
// scalar-only) дают ОДИНАКОВЫЙ ответ на одних данных. Две инкарнации:
//   - redis-list  traits={env:[prod,stage]}  (list-Trait);
//   - redis-scalar traits={env:prod}          (scalar-Trait).
//
// Scope trait=env:prod: scalar — видна (оба пути), list — НЕ видна (оба пути,
// scalar-only). Раньше List показывал list-инкарнацию (containment @>), а GET
// давал 404 (traitScalarEquals list→false) — рассинхрон. Здесь доказываем, что
// после фикса оба пути согласованы на live-PG.
func TestIntegration_ScopeTrait_ListVsScalar_ConsistentWithGet(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	ctx := context.Background()
	creator := "archon-alice"

	for _, seed := range []struct {
		name   string
		traits map[string]any
	}{
		{"redis-list", map[string]any{"env": []any{"prod", "stage"}}},
		{"redis-scalar", map[string]any{"env": "prod"}},
	} {
		if err := Create(ctx, integrationPool, &Incarnation{
			Name: seed.name, Service: "redis", ServiceVersion: "v1",
			StateSchemaVersion: 1, Status: StatusReady, CreatedByAID: &creator,
			Traits: seed.traits,
		}); err != nil {
			t.Fatalf("Create %s: %v", seed.name, err)
		}
	}

	scope := ListScope{Traits: []TraitPair{{Key: "env", Value: "prod"}}}

	// List-путь: SQL trait-scope. Видна должна быть ровно redis-scalar.
	out, total, err := SelectAll(ctx, integrationPool, ListFilter{}, scope, 0, 50)
	if err != nil {
		t.Fatalf("SelectAll trait-scope: %v", err)
	}
	listVisible := map[string]bool{}
	for _, inc := range out {
		listVisible[inc.Name] = true
	}
	if total != 1 || !listVisible["redis-scalar"] {
		t.Errorf("List: total=%d visible=%v, want только redis-scalar (scalar-only)", total, listVisible)
	}
	if listVisible["redis-list"] {
		t.Error("List ВИДИТ list-Trait инкарнацию через trait-scope — BUG #1 не исправлен (containment-семантика)")
	}

	// GET-путь: тот же scalar-предикат, что traitScalarEquals (scalar-only). Должен
	// совпасть с List на каждой инкарнации.
	for _, name := range []string{"redis-list", "redis-scalar"} {
		inc, err := SelectByName(ctx, integrationPool, name)
		if err != nil {
			t.Fatalf("SelectByName %s: %v", name, err)
		}
		getVisible := traitScalarEqualsLocal(inc.Traits, "env", "prod")
		if getVisible != listVisible[name] {
			t.Errorf("РАССИНХРОН List↔Get для %s: List=%v Get=%v (traits=%v)",
				name, listVisible[name], getVisible, inc.Traits)
		}
	}
}

// traitScalarEqualsLocal дублирует scalar-only семантику handlers.traitScalarEquals
// (тот не экспортируется и живёт в api-пакете) для проверки List↔Get-консистентности
// внутри incarnation-пакета: scalar (string/число/bool) → строковое равенство;
// list/map → false. Сознательная копия (несколько строк против межпакетной
// зависимости incarnation→api, которой быть не должно).
func traitScalarEqualsLocal(traits map[string]any, key, value string) bool {
	v, ok := traits[key]
	if !ok {
		return false
	}
	switch v.(type) {
	case string, float64, bool, int, int64:
		return fmt.Sprint(v) == value
	default:
		return false
	}
}
