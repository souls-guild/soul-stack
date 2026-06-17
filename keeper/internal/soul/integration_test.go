//go:build integration

// Integration-тесты souls CRUD через testcontainers-go (postgres:16-alpine).
// Паттерн совпадает с keeper/internal/operator/integration_test.go.

package soul

import (
	"context"
	"errors"
	"log"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/souls-guild/soul-stack/keeper/internal/migrate"
	"github.com/souls-guild/soul-stack/keeper/internal/operator"
	"github.com/souls-guild/soul-stack/keeper/migrations"
)

var integrationPool *pgxpool.Pool

func TestMain(m *testing.M) { os.Exit(run(m)) }

func run(m *testing.M) int {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	ctr, err := tcpostgres.Run(ctx,
		"postgres:16-alpine",
		tcpostgres.WithDatabase("keeper_test"),
		tcpostgres.WithUsername("keeper"),
		tcpostgres.WithPassword("keeper"),
		tcpostgres.BasicWaitStrategies(),
	)
	if err != nil {
		if requireDocker() {
			log.Fatalf("soul integration: setup failed (REQUIRE_DOCKER): %v", err)
		}
		log.Printf("soul integration: skipping, docker unavailable: %v", err)
		return 0
	}
	defer func() {
		termCtx, termCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer termCancel()
		_ = ctr.Terminate(termCtx)
	}()

	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		log.Printf("soul integration: ConnectionString: %v", err)
		return 1
	}
	if err := migrate.Apply(ctx, dsn, migrations.FS, "."); err != nil {
		log.Printf("soul integration: migrate.Apply: %v", err)
		return 1
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		log.Printf("soul integration: pgxpool.New: %v", err)
		return 1
	}
	defer pool.Close()
	integrationPool = pool

	return m.Run()
}

func resetAll(t *testing.T) {
	t.Helper()
	// CASCADE: soul_seeds → souls → operators (FK chain).
	_, err := integrationPool.Exec(context.Background(),
		`TRUNCATE TABLE soul_seeds, bootstrap_tokens, souls, operators, audit_log CASCADE`)
	if err != nil {
		t.Fatalf("TRUNCATE: %v", err)
	}
}

func seedOperator(t *testing.T, aid string) {
	t.Helper()
	op := &operator.Operator{
		AID:         aid,
		DisplayName: aid,
		AuthMethod:  operator.AuthMethodJWT,
	}
	if err := operator.Insert(context.Background(), integrationPool, op); err != nil {
		t.Fatalf("seedOperator(%s): %v", aid, err)
	}
}

func TestIntegration_Insert_Pending_AndSelect(t *testing.T) {
	resetAll(t)
	seedOperator(t, "archon-alice")
	ctx := context.Background()

	creator := "archon-alice"
	requestedAt := time.Now().UTC()
	s := &Soul{
		SID:          "host1.example.com",
		Status:       StatusPending,
		Coven:        []string{"prod", "db"},
		CreatedByAID: &creator,
		RequestedAt:  &requestedAt,
		Note:         "test bootstrap",
	}
	if err := Insert(ctx, integrationPool, s); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if s.RegisteredAt.IsZero() {
		t.Errorf("RegisteredAt not populated")
	}

	got, err := SelectBySID(ctx, integrationPool, "host1.example.com")
	if err != nil {
		t.Fatalf("SelectBySID: %v", err)
	}
	if got.SID != "host1.example.com" || got.Status != StatusPending {
		t.Errorf("got = %+v", got)
	}
	if len(got.Coven) != 2 || got.Coven[0] != "prod" {
		t.Errorf("Coven = %v, want [prod db]", got.Coven)
	}
	if got.CreatedByAID == nil || *got.CreatedByAID != "archon-alice" {
		t.Errorf("CreatedByAID = %v", got.CreatedByAID)
	}
	if got.Note != "test bootstrap" {
		t.Errorf("Note = %q", got.Note)
	}
}

func TestIntegration_Insert_RejectsDuplicateSID(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	s := &Soul{SID: "host1.example.com"}
	if err := Insert(ctx, integrationPool, s); err != nil {
		t.Fatalf("first Insert: %v", err)
	}
	dup := &Soul{SID: "host1.example.com"}
	err := Insert(ctx, integrationPool, dup)
	if !errors.Is(err, ErrSoulAlreadyExists) {
		t.Errorf("err = %v, want ErrSoulAlreadyExists", err)
	}
}

func TestIntegration_Insert_RejectsBadSIDViaCHECK(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	// SID проходит ValidSID на Go-уровне, но PG CHECK его всё равно рубит.
	// Здесь подставим SID, который Go-валидация пропустит — пограничный
	// случай: точка в начале запрещена обеими стороной. Используем
	// uppercase, который Go тоже отвергает; этот тест документирует, что
	// validation отрабатывает до round-trip-а (защита от случайной утечки
	// CHECK-violation наружу).
	s := &Soul{SID: "HOST.EXAMPLE.COM"}
	if err := Insert(ctx, integrationPool, s); err == nil {
		t.Fatal("Insert with uppercase SID returned nil err")
	}
}

func TestIntegration_UpdateStatus(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	s := &Soul{SID: "host1.example.com", Status: StatusPending}
	if err := Insert(ctx, integrationPool, s); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	kid := "kid-1"
	if err := UpdateStatus(ctx, integrationPool, "host1.example.com", StatusConnected, &kid); err != nil {
		t.Fatalf("UpdateStatus: %v", err)
	}
	got, err := SelectBySID(ctx, integrationPool, "host1.example.com")
	if err != nil {
		t.Fatalf("SelectBySID: %v", err)
	}
	if got.Status != StatusConnected {
		t.Errorf("Status = %q, want connected", got.Status)
	}
	if got.LastSeenByKID == nil || *got.LastSeenByKID != "kid-1" {
		t.Errorf("LastSeenByKID = %v, want kid-1", got.LastSeenByKID)
	}
}

func TestIntegration_UpdateStatus_NotFound(t *testing.T) {
	resetAll(t)
	err := UpdateStatus(context.Background(), integrationPool, "missing.host.example.com", StatusConnected, nil)
	if !errors.Is(err, ErrSoulNotFound) {
		t.Errorf("err = %v, want ErrSoulNotFound", err)
	}
}

func TestIntegration_SelectAll_FilterAndPaginate(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	for i, host := range []string{"a.example.com", "b.example.com", "c.example.com"} {
		s := &Soul{
			SID:    host,
			Status: StatusPending,
			Coven:  []string{"prod"},
		}
		if i == 0 {
			s.Status = StatusConnected
		}
		if err := Insert(ctx, integrationPool, s); err != nil {
			t.Fatalf("Insert(%s): %v", host, err)
		}
	}

	unrestricted := ListScope{Unrestricted: true}

	all, total, err := SelectAll(ctx, integrationPool, ListFilter{}, unrestricted, 0, 10)
	if err != nil {
		t.Fatalf("SelectAll: %v", err)
	}
	if total != 3 || len(all) != 3 {
		t.Errorf("total/len = %d/%d, want 3/3", total, len(all))
	}

	pending, totalPending, err := SelectAll(ctx, integrationPool, ListFilter{Status: StatusPending}, unrestricted, 0, 10)
	if err != nil {
		t.Fatalf("SelectAll(pending): %v", err)
	}
	if totalPending != 2 || len(pending) != 2 {
		t.Errorf("pending total/len = %d/%d, want 2/2", totalPending, len(pending))
	}

	byCoven, _, err := SelectAll(ctx, integrationPool, ListFilter{Coven: "prod"}, unrestricted, 0, 10)
	if err != nil {
		t.Fatalf("SelectAll(coven): %v", err)
	}
	if len(byCoven) != 3 {
		t.Errorf("by-coven len = %d, want 3", len(byCoven))
	}
}

// TestIntegration_SelectAll_Scope — scope-pushdown `coven && ARRAY[scope]`
// (ADR-047 S3b) на реальном PG: coven-scoped оператор видит ТОЛЬКО хосты в своих
// ковенах; пустой scope (fail-closed) → ноль строк (НЕ весь флот); filter ∩ scope
// AND. total когерентен выдаче (COUNT с тем же scope-WHERE).
func TestIntegration_SelectAll_Scope(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	seed := []struct {
		sid   string
		coven []string
	}{
		{"prod-1.example.com", []string{"prod"}},
		{"prod-2.example.com", []string{"prod", "dc-eu"}},
		{"stg-1.example.com", []string{"staging"}},
		{"dev-1.example.com", []string{"dev"}},
	}
	for _, s := range seed {
		if err := Insert(ctx, integrationPool, &Soul{SID: s.sid, Status: StatusPending, Coven: s.coven}); err != nil {
			t.Fatalf("Insert(%s): %v", s.sid, err)
		}
	}

	// scope=[prod] → только prod-хосты (подмножество, не весь флот).
	prodItems, prodTotal, err := SelectAll(ctx, integrationPool, ListFilter{}, ListScope{Covens: []string{"prod"}}, 0, 10)
	if err != nil {
		t.Fatalf("SelectAll(scope=prod): %v", err)
	}
	if prodTotal != 2 || len(prodItems) != 2 {
		t.Errorf("scope=prod total/len = %d/%d, want 2/2 (prod-1, prod-2)", prodTotal, len(prodItems))
	}

	// scope=[prod,staging] → union.
	union, unionTotal, err := SelectAll(ctx, integrationPool, ListFilter{}, ListScope{Covens: []string{"prod", "staging"}}, 0, 10)
	if err != nil {
		t.Fatalf("SelectAll(scope=prod,staging): %v", err)
	}
	if unionTotal != 3 || len(union) != 3 {
		t.Errorf("scope=[prod,staging] total/len = %d/%d, want 3/3", unionTotal, len(union))
	}

	// fail-closed: пустой scope (не Unrestricted) → ноль строк, НЕ весь флот.
	empty, emptyTotal, err := SelectAll(ctx, integrationPool, ListFilter{}, ListScope{Covens: nil}, 0, 10)
	if err != nil {
		t.Fatalf("SelectAll(scope=empty): %v", err)
	}
	if emptyTotal != 0 || len(empty) != 0 {
		t.Errorf("empty scope total/len = %d/%d, want 0/0 (fail-closed, НЕ весь флот)", emptyTotal, len(empty))
	}

	// filter ∩ scope: оператор со scope=[prod] фильтрует coven=staging → пусто
	// (staging вне его scope, AND-пересечение, не расширение).
	cross, crossTotal, err := SelectAll(ctx, integrationPool, ListFilter{Coven: "staging"}, ListScope{Covens: []string{"prod"}}, 0, 10)
	if err != nil {
		t.Fatalf("SelectAll(filter=staging ∩ scope=prod): %v", err)
	}
	if crossTotal != 0 || len(cross) != 0 {
		t.Errorf("filter=staging ∩ scope=prod total/len = %d/%d, want 0/0 (scope не расширяется фильтром)", crossTotal, len(cross))
	}
}

// TestIntegration_ListForScopeEval_KeysetNoDupesNoGaps — ГЛАВНЫЙ keyset-тест
// (ADR-047 S3b-2a): флот с ПАЧКОЙ одинаковых registered_at, page_size меньше
// флота → проход всех страниц composite-курсором `(registered_at, sid)` собирает
// КАЖДЫЙ хост РОВНО ОДИН раз (нет дублей, нет пропусков на границах). Голый sid
// в курсоре дал бы дыры на равных registered_at — этот тест ловит регресс.
func TestIntegration_ListForScopeEval_KeysetNoDupesNoGaps(t *testing.T) {
	resetAll(t)
	ctx := context.Background()

	// 7 хостов с ОДИНАКОВЫМ registered_at (пачка), 2 — с другими временами.
	// Пачка вынуждает composite-курсор разрешать ties по sid.
	sameTS := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	earlier := sameTS.Add(-time.Hour)
	later := sameTS.Add(time.Hour)
	seed := []struct {
		sid string
		at  time.Time
	}{
		{"h1.example.com", sameTS}, {"h2.example.com", sameTS}, {"h3.example.com", sameTS},
		{"h4.example.com", sameTS}, {"h5.example.com", sameTS}, {"h6.example.com", sameTS},
		{"h7.example.com", sameTS},
		{"early.example.com", earlier},
		{"late.example.com", later},
	}
	want := map[string]struct{}{}
	for _, s := range seed {
		at := s.at
		if err := Insert(ctx, integrationPool, &Soul{SID: s.sid, Status: StatusPending, RegisteredAt: at}); err != nil {
			t.Fatalf("Insert(%s): %v", s.sid, err)
		}
		want[s.sid] = struct{}{}
	}

	const pageSize = 3 // меньше флота (9) и меньше пачки (7) → много границ.
	got := map[string]struct{}{}
	var cursor *KeysetCursorBound
	pages := 0
	for {
		rows, err := ListForScopeEval(ctx, integrationPool, ListFilter{}, cursor, pageSize)
		if err != nil {
			t.Fatalf("ListForScopeEval (page %d): %v", pages, err)
		}
		if len(rows) == 0 {
			break
		}
		for _, r := range rows {
			if _, dup := got[r.SID]; dup {
				t.Fatalf("ДУБЛЬ: %s встретился дважды (keyset-граница протекла)", r.SID)
			}
			got[r.SID] = struct{}{}
		}
		last := rows[len(rows)-1]
		cursor = &KeysetCursorBound{RegisteredAt: last.RegisteredAt, SID: last.SID}
		pages++
		if len(rows) < pageSize {
			break
		}
		if pages > 20 {
			t.Fatal("курсор не сходится (>20 страниц на 9 хостах)")
		}
	}
	if len(got) != len(want) {
		t.Fatalf("собрано %d хостов, want %d — есть пропуски/дубли", len(got), len(want))
	}
	for sid := range want {
		if _, ok := got[sid]; !ok {
			t.Errorf("хост %s пропущен keyset-обходом", sid)
		}
	}
}

// TestIntegration_ListForScopeEval_OrderStable — порядок registered_at DESC,
// sid ASC устойчив: курсорный обход идёт строго монотонно (каждая следующая
// страница не «возвращается назад»).
func TestIntegration_ListForScopeEval_OrderStable(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	base := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	for _, sid := range []string{"c.example.com", "a.example.com", "b.example.com"} {
		// a,b в одну секунду, c — позже: проверяем и tie-break, и DESC.
		at := base
		if sid == "c.example.com" {
			at = base.Add(time.Minute)
		}
		if err := Insert(ctx, integrationPool, &Soul{SID: sid, Status: StatusPending, RegisteredAt: at}); err != nil {
			t.Fatalf("Insert(%s): %v", sid, err)
		}
	}
	rows, err := ListForScopeEval(ctx, integrationPool, ListFilter{}, nil, 10)
	if err != nil {
		t.Fatalf("ListForScopeEval: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3", len(rows))
	}
	// c (позже) первым; затем a,b (равный TS) по sid ASC.
	wantOrder := []string{"c.example.com", "a.example.com", "b.example.com"}
	for i, w := range wantOrder {
		if rows[i].SID != w {
			t.Errorf("rows[%d] = %s, want %s (порядок registered_at DESC, sid ASC нарушен)", i, rows[i].SID, w)
		}
	}
}

// TestIntegration_ListForScopeEval_FilterPushdown — user-filter (status/
// transport/coven) применяется как SQL WHERE внутри keyset-окна (ADR-047 S3b-2a
// fix). Без pushdown-а keyset-режим вернул бы все строки, и handler-Go-eval не
// смог бы отличить «отфильтровано» от «вне scope». Тест на CRUD-уровне фиксирует,
// что фильтр режет выборку В БД, а не игнорируется.
func TestIntegration_ListForScopeEval_FilterPushdown(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	base := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	seed := []*Soul{
		{SID: "web-01.example.com", Transport: TransportAgent, Status: StatusConnected, Coven: []string{"prod"}, RegisteredAt: base},
		{SID: "web-02.example.com", Transport: TransportAgent, Status: StatusPending, Coven: []string{"dev"}, RegisteredAt: base.Add(-time.Second)},
		{SID: "ssh-03.example.com", Transport: TransportSSH, Status: StatusConnected, Coven: []string{"prod"}, RegisteredAt: base.Add(-2 * time.Second)},
	}
	for _, s := range seed {
		if err := Insert(ctx, integrationPool, s); err != nil {
			t.Fatalf("Insert(%s): %v", s.SID, err)
		}
	}

	collect := func(f ListFilter) map[string]struct{} {
		rows, err := ListForScopeEval(ctx, integrationPool, f, nil, 100)
		if err != nil {
			t.Fatalf("ListForScopeEval(%+v): %v", f, err)
		}
		out := map[string]struct{}{}
		for _, r := range rows {
			out[r.SID] = struct{}{}
		}
		return out
	}

	// status=connected → web-01, ssh-03 (НЕ web-02 pending).
	got := collect(ListFilter{Status: StatusConnected})
	if len(got) != 2 {
		t.Errorf("status=connected: got %d, want 2 (%v)", len(got), got)
	}
	if _, ok := got["web-02.example.com"]; ok {
		t.Error("status=connected пропустил pending web-02 — фильтр не применён в SQL")
	}
	// transport=ssh → только ssh-03.
	got = collect(ListFilter{Transport: TransportSSH})
	if len(got) != 1 {
		t.Errorf("transport=ssh: got %d, want 1 (%v)", len(got), got)
	}
	if _, ok := got["ssh-03.example.com"]; !ok {
		t.Error("transport=ssh не вернул ssh-03")
	}
	// coven=dev → только web-02.
	got = collect(ListFilter{Coven: "dev"})
	if len(got) != 1 {
		t.Errorf("coven=dev: got %d, want 1 (%v)", len(got), got)
	}
	if _, ok := got["web-02.example.com"]; !ok {
		t.Error("coven=dev не вернул web-02")
	}
	// Комбинация (AND): connected + prod + agent → только web-01.
	got = collect(ListFilter{Status: StatusConnected, Coven: "prod", Transport: TransportAgent})
	if len(got) != 1 {
		t.Errorf("combined: got %d, want 1 (%v)", len(got), got)
	}
	if _, ok := got["web-01.example.com"]; !ok {
		t.Error("combined фильтр не вернул web-01")
	}
}

func TestIntegration_UpdateStatus_DestroyedAcceptedByCHECK(t *testing.T) {
	// ADR-017 cascade: миграция 016 расширила CHECK `souls_status_valid`
	// значением `destroyed`. Проверяем, что PG принимает новый enum.
	resetAll(t)
	ctx := context.Background()
	s := &Soul{SID: "destroyed-host.example.com"}
	if err := Insert(ctx, integrationPool, s); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if err := UpdateStatus(ctx, integrationPool, "destroyed-host.example.com", StatusDestroyed, nil); err != nil {
		t.Fatalf("UpdateStatus(destroyed): %v", err)
	}
	got, err := SelectBySID(ctx, integrationPool, "destroyed-host.example.com")
	if err != nil {
		t.Fatalf("SelectBySID: %v", err)
	}
	if got.Status != StatusDestroyed {
		t.Errorf("status = %q, want destroyed", got.Status)
	}
}

func TestIntegration_FKToOperators_SetNullOnDelete(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	seedOperator(t, "archon-alice")
	creator := "archon-alice"
	s := &Soul{SID: "host1.example.com", CreatedByAID: &creator}
	if err := Insert(ctx, integrationPool, s); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	// Удаляем оператора — souls.created_by_aid должен стать NULL (ON DELETE SET NULL).
	if _, err := integrationPool.Exec(ctx, `DELETE FROM operators WHERE aid = $1`, "archon-alice"); err != nil {
		t.Fatalf("DELETE operator: %v", err)
	}
	got, err := SelectBySID(ctx, integrationPool, "host1.example.com")
	if err != nil {
		t.Fatalf("SelectBySID: %v", err)
	}
	if got.CreatedByAID != nil {
		t.Errorf("CreatedByAID = %v, want nil after operator delete", got.CreatedByAID)
	}
}
