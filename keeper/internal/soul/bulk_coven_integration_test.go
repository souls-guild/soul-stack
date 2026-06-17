//go:build integration

// Integration-тесты bulk coven-assign (BulkAssignCoven / CountBulkMatched)
// против реального Postgres (keyset-чанкинг, идемпотентность, scope-
// intersection не проверить на fake-pool-е — они SQL-driven). Используют
// общий harness integration_test.go (integrationPool / resetAll).

package soul

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// seedBulkSoul вставляет хост с заданным набором coven (status pending).
func seedBulkSoul(t *testing.T, sid string, coven []string) {
	t.Helper()
	s := &Soul{SID: sid, Status: StatusPending, Coven: coven}
	if err := Insert(context.Background(), integrationPool, s); err != nil {
		t.Fatalf("seedBulkSoul(%s): %v", sid, err)
	}
}

func covenOf(t *testing.T, sid string) []string {
	t.Helper()
	got, err := SelectBySID(context.Background(), integrationPool, sid)
	if err != nil {
		t.Fatalf("SelectBySID(%s): %v", sid, err)
	}
	out := append([]string(nil), got.Coven...)
	sort.Strings(out)
	return out
}

func TestIntegration_Bulk_Append_All_Idempotent(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	seedBulkSoul(t, "a.example.com", []string{"dev"})
	seedBulkSoul(t, "b.example.com", []string{"dev", "edge"})

	sel := BulkSelector{All: true}
	scope := BulkScope{Unrestricted: true}

	rep, err := BulkAssignCoven(ctx, integrationPool, sel, scope, "edge", CovenAppend)
	if err != nil {
		t.Fatalf("BulkAssignCoven: %v", err)
	}
	if rep.Status != BulkCompleted {
		t.Errorf("status = %q, want completed", rep.Status)
	}
	if rep.Matched != 2 {
		t.Errorf("matched = %d, want 2", rep.Matched)
	}
	// b уже имеет edge → идемпотентный отсев → меняется только a.
	if rep.Changed != 1 {
		t.Errorf("changed = %d, want 1 (b already has edge)", rep.Changed)
	}
	if got := covenOf(t, "a.example.com"); !equalStr(got, []string{"dev", "edge"}) {
		t.Errorf("a.coven = %v, want [dev edge]", got)
	}

	// Повторный вызов = no-op (оба уже имеют edge).
	rep2, err := BulkAssignCoven(ctx, integrationPool, sel, scope, "edge", CovenAppend)
	if err != nil {
		t.Fatalf("BulkAssignCoven repeat: %v", err)
	}
	if rep2.Changed != 0 {
		t.Errorf("repeat changed = %d, want 0 (idempotent)", rep2.Changed)
	}
}

func TestIntegration_Bulk_Remove_Idempotent(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	seedBulkSoul(t, "a.example.com", []string{"dev", "old"})
	seedBulkSoul(t, "b.example.com", []string{"dev"})

	sel := BulkSelector{All: true}
	scope := BulkScope{Unrestricted: true}

	rep, err := BulkAssignCoven(ctx, integrationPool, sel, scope, "old", CovenRemove)
	if err != nil {
		t.Fatalf("BulkAssignCoven: %v", err)
	}
	if rep.Changed != 1 {
		t.Errorf("changed = %d, want 1 (only a has old)", rep.Changed)
	}
	if got := covenOf(t, "a.example.com"); !equalStr(got, []string{"dev"}) {
		t.Errorf("a.coven = %v, want [dev]", got)
	}

	rep2, err := BulkAssignCoven(ctx, integrationPool, sel, scope, "old", CovenRemove)
	if err != nil {
		t.Fatalf("repeat: %v", err)
	}
	if rep2.Changed != 0 {
		t.Errorf("repeat changed = %d, want 0", rep2.Changed)
	}
}

// TestIntegration_Bulk_Chunking — >bulkChunkSize хостов: keyset-итерация
// должна пройти все строки в нескольких чанках без пропусков/дублей.
func TestIntegration_Bulk_Chunking(t *testing.T) {
	resetAll(t)
	ctx := context.Background()

	const n = bulkChunkSize + 137 // > 1 чанка
	for i := 0; i < n; i++ {
		seedBulkSoul(t, fmt.Sprintf("h%05d.example.com", i), []string{"dev"})
	}

	sel := BulkSelector{All: true}
	scope := BulkScope{Unrestricted: true}
	rep, err := BulkAssignCoven(ctx, integrationPool, sel, scope, "batch", CovenAppend)
	if err != nil {
		t.Fatalf("BulkAssignCoven: %v", err)
	}
	if rep.Matched != n {
		t.Errorf("matched = %d, want %d", rep.Matched, n)
	}
	if rep.Changed != n {
		t.Errorf("changed = %d, want %d (no host had batch)", rep.Changed, n)
	}
	if rep.ChunksCommitted < 2 {
		t.Errorf("chunksCommitted = %d, want >= 2 (>1 chunk)", rep.ChunksCommitted)
	}

	// Контрольная сверка: все хосты получили метку.
	var withBatch int
	if err := integrationPool.QueryRow(ctx,
		"SELECT COUNT(*) FROM souls WHERE 'batch' = ANY(coven)").Scan(&withBatch); err != nil {
		t.Fatalf("count: %v", err)
	}
	if withBatch != n {
		t.Errorf("hosts with 'batch' = %d, want %d (keyset skipped/dup rows)", withBatch, n)
	}
}

func TestIntegration_Bulk_DryRun_NoWrite(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	seedBulkSoul(t, "a.example.com", []string{"dev"})
	seedBulkSoul(t, "b.example.com", []string{"dev"})

	sel := BulkSelector{All: true}
	scope := BulkScope{Unrestricted: true}

	matched, err := CountBulkMatched(ctx, integrationPool, sel, scope)
	if err != nil {
		t.Fatalf("CountBulkMatched: %v", err)
	}
	if matched != 2 {
		t.Errorf("matched = %d, want 2", matched)
	}
	// dry_run не должен ничего записать.
	if got := covenOf(t, "a.example.com"); !equalStr(got, []string{"dev"}) {
		t.Errorf("a.coven mutated by dry_run: %v", got)
	}
}

func TestIntegration_Bulk_Selector_SIDs(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	seedBulkSoul(t, "a.example.com", []string{"dev"})
	seedBulkSoul(t, "b.example.com", []string{"dev"})
	seedBulkSoul(t, "c.example.com", []string{"dev"})

	sel := BulkSelector{SIDs: []string{"a.example.com", "c.example.com"}}
	scope := BulkScope{Unrestricted: true}
	rep, err := BulkAssignCoven(ctx, integrationPool, sel, scope, "edge", CovenAppend)
	if err != nil {
		t.Fatalf("BulkAssignCoven: %v", err)
	}
	if rep.Matched != 2 || rep.Changed != 2 {
		t.Errorf("matched/changed = %d/%d, want 2/2", rep.Matched, rep.Changed)
	}
	if got := covenOf(t, "b.example.com"); !equalStr(got, []string{"dev"}) {
		t.Errorf("b mutated though not in sids: %v", got)
	}
}

func TestIntegration_Bulk_Selector_Status(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	seedBulkSoul(t, "a.example.com", []string{"dev"})
	kid := "kid-1"
	if err := UpdateStatus(ctx, integrationPool, "a.example.com", StatusConnected, &kid); err != nil {
		t.Fatalf("UpdateStatus: %v", err)
	}
	seedBulkSoul(t, "b.example.com", []string{"dev"}) // остаётся pending

	sel := BulkSelector{All: true, Status: StatusConnected}
	scope := BulkScope{Unrestricted: true}
	rep, err := BulkAssignCoven(ctx, integrationPool, sel, scope, "edge", CovenAppend)
	if err != nil {
		t.Fatalf("BulkAssignCoven: %v", err)
	}
	if rep.Matched != 1 || rep.Changed != 1 {
		t.Errorf("matched/changed = %d/%d, want 1/1", rep.Matched, rep.Changed)
	}
}

func TestIntegration_Bulk_EmptySelector(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	_, err := BulkAssignCoven(ctx, integrationPool, BulkSelector{}, BulkScope{Unrestricted: true}, "edge", CovenAppend)
	if !errors.Is(err, ErrBulkEmptySelector) {
		t.Errorf("err = %v, want ErrBulkEmptySelector", err)
	}
}

// --- scope-intersection (КРИТИЧНО) ---

// TestIntegration_Bulk_Scope_HostsSubset — гейт (a): целевые хосты ⊆ scope.
// Оператор с coven=dev не трогает хосты вне dev даже при all=true.
func TestIntegration_Bulk_Scope_HostsSubset(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	seedBulkSoul(t, "dev-host.example.com", []string{"dev"})
	seedBulkSoul(t, "prod-host.example.com", []string{"prod"})

	sel := BulkSelector{All: true}
	scope := BulkScope{Covens: []string{"dev"}} // Unrestricted=false

	rep, err := BulkAssignCoven(ctx, integrationPool, sel, scope, "dev", CovenAppend)
	if err != nil {
		t.Fatalf("BulkAssignCoven: %v", err)
	}
	// Только dev-host попадает под scope; prod-host исключён предикатом
	// coven && ARRAY['dev']. append-метка 'dev' ∈ scope — допустима.
	if rep.Matched != 1 {
		t.Errorf("matched = %d, want 1 (prod-host out of scope)", rep.Matched)
	}
	if got := covenOf(t, "prod-host.example.com"); !equalStr(got, []string{"prod"}) {
		t.Errorf("prod-host mutated despite out-of-scope: %v", got)
	}
}

// TestIntegration_Bulk_Scope_HostsSubset_ViaSIDs — гейт (a) обходить через
// явный sids-список нельзя: хост вне scope всё равно не попадает в UPDATE.
func TestIntegration_Bulk_Scope_HostsSubset_ViaSIDs(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	seedBulkSoul(t, "prod-host.example.com", []string{"prod"})

	sel := BulkSelector{SIDs: []string{"prod-host.example.com"}}
	scope := BulkScope{Covens: []string{"dev"}}

	// label 'dev' ∈ scope (проходит гейт b), но целевой хост вне scope.
	rep, err := BulkAssignCoven(ctx, integrationPool, sel, scope, "dev", CovenAppend)
	if err != nil {
		t.Fatalf("BulkAssignCoven: %v", err)
	}
	if rep.Matched != 0 || rep.Changed != 0 {
		t.Errorf("matched/changed = %d/%d, want 0/0 (host out of scope)", rep.Matched, rep.Changed)
	}
	if got := covenOf(t, "prod-host.example.com"); !equalStr(got, []string{"prod"}) {
		t.Errorf("out-of-scope host mutated: %v", got)
	}
}

// TestIntegration_Bulk_Scope_LabelOutOfScope — гейт (b): нельзя навесить
// метку вне scope (privilege-escalation на чужой таргет).
func TestIntegration_Bulk_Scope_LabelOutOfScope(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	seedBulkSoul(t, "dev-host.example.com", []string{"dev"})

	sel := BulkSelector{All: true}
	scope := BulkScope{Covens: []string{"dev"}}

	// Оператор с coven=dev пытается навесить 'prod' — отказ ДО UPDATE.
	_, err := BulkAssignCoven(ctx, integrationPool, sel, scope, "prod", CovenAppend)
	if !errors.Is(err, ErrBulkLabelOutOfScope) {
		t.Fatalf("err = %v, want ErrBulkLabelOutOfScope", err)
	}
	if got := covenOf(t, "dev-host.example.com"); !equalStr(got, []string{"dev"}) {
		t.Errorf("host mutated despite out-of-scope label: %v", got)
	}
}

// TestIntegration_Bulk_Scope_RemoveOutOfScopeLabel_Allowed — remove метки вне
// scope не расширяет таргет (снятие — не эскалация); гейт (b) на remove не
// действует, но гейт (a) всё равно ограничивает целевые хосты scope-ом.
func TestIntegration_Bulk_Scope_Remove_HostsSubset(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	seedBulkSoul(t, "dev-host.example.com", []string{"dev", "shared"})
	seedBulkSoul(t, "prod-host.example.com", []string{"prod", "shared"})

	sel := BulkSelector{All: true}
	scope := BulkScope{Covens: []string{"dev"}}

	rep, err := BulkAssignCoven(ctx, integrationPool, sel, scope, "shared", CovenRemove)
	if err != nil {
		t.Fatalf("BulkAssignCoven: %v", err)
	}
	// Только dev-host в scope → снимаем shared только с него.
	if rep.Changed != 1 {
		t.Errorf("changed = %d, want 1 (prod-host out of scope)", rep.Changed)
	}
	if got := covenOf(t, "prod-host.example.com"); !equalStr(got, []string{"prod", "shared"}) {
		t.Errorf("prod-host mutated despite out-of-scope: %v", got)
	}
}

// --- partial-семантика (путь «без отката») ---

// cancelAfterChunkPool оборачивает реальный *pgxpool.Pool и отменяет общий
// cancellable-context ПЕРЕД K-м BeginTx. Тогда чанки 1..K-1 коммитятся
// нормально, а на K-м BeginTx(ctx) pgx возвращает context.Canceled →
// bulkUpdateChunk фейлится → BulkAssignCoven отдаёт BulkPartial без отката
// закоммиченных чанков. Test-only seam: production-код не трогаем, инъекция
// сбоя — через cancel переданного контекста, а не через хук в BulkAssignCoven.
//
// CountBulkMatched (первый round-trip в BulkAssignCoven) ходит через QueryRow,
// а не BeginTx, поэтому счётчик BeginTx прямо соответствует номеру чанка.
type cancelAfterChunkPool struct {
	*pgxpool.Pool
	cancel   context.CancelFunc
	failOn   int // номер чанка (1-based), на BeginTx которого отменяем ctx.
	beginCnt int
}

func (p *cancelAfterChunkPool) BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error) {
	p.beginCnt++
	if p.beginCnt == p.failOn {
		// Отменяем ДО передачи в реальный пул: BeginTx с отменённым ctx
		// вернёт ошибку, и этот чанк (K) не закоммитится.
		p.cancel()
	}
	return p.Pool.BeginTx(ctx, opts)
}

// TestIntegration_Bulk_Partial_NoRollback — чанк K падает в середине большой
// выборки: чанки 1..K-1 закоммичены (метка на их строках), возврат partial с
// Err и changed < matched; идемпотентный повтор добивает остаток без дублей.
func TestIntegration_Bulk_Partial_NoRollback(t *testing.T) {
	resetAll(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 3 полных чанка + хвост: достаточно, чтобы упасть на 2-м чанке и иметь
	// непустой остаток (чанки 2..3 + хвост) для до-повтора.
	const n = bulkChunkSize*3 + 91
	for i := 0; i < n; i++ {
		seedBulkSoul(t, fmt.Sprintf("h%05d.example.com", i), []string{"dev"})
	}

	sel := BulkSelector{All: true}
	scope := BulkScope{Unrestricted: true}

	const failChunk = 2 // упасть на BeginTx 2-го чанка → закоммичен ровно 1.
	pool := &cancelAfterChunkPool{Pool: integrationPool, cancel: cancel, failOn: failChunk}

	rep, err := BulkAssignCoven(ctx, pool, sel, scope, "batch", CovenAppend)
	if err == nil {
		t.Fatal("BulkAssignCoven: err = nil, want partial-failure error")
	}
	if rep.Status != BulkPartial {
		t.Errorf("status = %q, want partial", rep.Status)
	}
	if rep.Err == nil {
		t.Errorf("rep.Err = nil, want non-nil on partial")
	}
	if rep.Matched != n {
		t.Errorf("matched = %d, want %d (count до итерации)", rep.Matched, n)
	}
	if rep.ChunksCommitted != failChunk-1 {
		t.Errorf("chunksCommitted = %d, want %d", rep.ChunksCommitted, failChunk-1)
	}
	if rep.Changed >= rep.Matched {
		t.Errorf("changed = %d, want < matched (%d) on partial", rep.Changed, rep.Matched)
	}
	if rep.Changed != bulkChunkSize {
		t.Errorf("changed = %d, want %d (ровно 1 закоммиченный чанк)", rep.Changed, bulkChunkSize)
	}

	// Чанки 1..K-1 ДЕЙСТВИТЕЛЬНО закоммичены в реальном PG (метка на строках).
	var withBatch int
	if err := integrationPool.QueryRow(context.Background(),
		"SELECT COUNT(*) FROM souls WHERE 'batch' = ANY(coven)").Scan(&withBatch); err != nil {
		t.Fatalf("count after partial: %v", err)
	}
	if withBatch != bulkChunkSize {
		t.Errorf("hosts with 'batch' after partial = %d, want %d (commit чанка 1 уцелел)", withBatch, bulkChunkSize)
	}

	// Идемпотентный повтор ДОБИВАЕТ остаток (свежий неотменённый ctx).
	rep2, err := BulkAssignCoven(context.Background(), integrationPool, sel, scope, "batch", CovenAppend)
	if err != nil {
		t.Fatalf("BulkAssignCoven repeat: %v", err)
	}
	if rep2.Status != BulkCompleted {
		t.Errorf("repeat status = %q, want completed", rep2.Status)
	}
	// Повтор трогает только ещё-не-помеченные строки (idem-отсев): n - уже-помеченные.
	if rep2.Changed != n-bulkChunkSize {
		t.Errorf("repeat changed = %d, want %d (только остаток)", rep2.Changed, n-bulkChunkSize)
	}

	// Итог консистентен: метка ровно у всех n, без дублей в массиве.
	if err := integrationPool.QueryRow(context.Background(),
		"SELECT COUNT(*) FROM souls WHERE 'batch' = ANY(coven)").Scan(&withBatch); err != nil {
		t.Fatalf("final count: %v", err)
	}
	if withBatch != n {
		t.Errorf("final hosts with 'batch' = %d, want %d", withBatch, n)
	}
	var dupHosts int
	if err := integrationPool.QueryRow(context.Background(),
		// cardinality(array) > длины множества уникальных → дубль метки в массиве.
		`SELECT COUNT(*) FROM souls
		 WHERE cardinality(coven) <> cardinality(ARRAY(SELECT DISTINCT unnest(coven)))`).Scan(&dupHosts); err != nil {
		t.Fatalf("dup-check: %v", err)
	}
	if dupHosts != 0 {
		t.Errorf("hosts с дублями меток в coven = %d, want 0 (idem-отсев не дал двойного append)", dupHosts)
	}
}

// TestIntegration_Bulk_ExactMultipleOfChunk — число подходящих строк ТОЧНО
// кратно bulkChunkSize: проверка off-by-one в keyset-итерации. Последнее
// keyset-окно ровно заполнено (scanned == chunk), поэтому итерация делает ещё
// один проход с пустым окном и корректно выходит — без лишних изменений/паники.
func TestIntegration_Bulk_ExactMultipleOfChunk(t *testing.T) {
	resetAll(t)
	ctx := context.Background()

	const n = bulkChunkSize * 2 // ровно 2 полных чанка.
	for i := 0; i < n; i++ {
		seedBulkSoul(t, fmt.Sprintf("h%05d.example.com", i), []string{"dev"})
	}

	sel := BulkSelector{All: true}
	scope := BulkScope{Unrestricted: true}
	rep, err := BulkAssignCoven(ctx, integrationPool, sel, scope, "exact", CovenAppend)
	if err != nil {
		t.Fatalf("BulkAssignCoven: %v", err)
	}
	if rep.Matched != n || rep.Changed != n {
		t.Errorf("matched/changed = %d/%d, want %d/%d", rep.Matched, rep.Changed, n, n)
	}
	// 2 полных чанка → 2-й чанк имеет scanned == chunk, значит цикл делает 3-й
	// проход с пустым окном (scanned 0 < chunk → выход). ChunksCommitted
	// считает и пустой финальный проход (он коммитит no-op-транзакцию).
	if rep.ChunksCommitted != 3 {
		t.Errorf("chunksCommitted = %d, want 3 (2 полных + пустой финальный)", rep.ChunksCommitted)
	}

	var withExact int
	if err := integrationPool.QueryRow(ctx,
		"SELECT COUNT(*) FROM souls WHERE 'exact' = ANY(coven)").Scan(&withExact); err != nil {
		t.Fatalf("count: %v", err)
	}
	if withExact != n {
		t.Errorf("hosts with 'exact' = %d, want %d (off-by-one в keyset)", withExact, n)
	}
}

func equalStr(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// --- mode=replace integration ---

// TestIntegration_Bulk_Replace_MutatesSet — replace задаёт coven ровно как
// набор, выкидывая существующие метки.
func TestIntegration_Bulk_Replace_MutatesSet(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	seedBulkSoul(t, "a.example.com", []string{"dev", "old"})
	seedBulkSoul(t, "b.example.com", []string{"stage"})

	sel := BulkSelector{All: true}
	scope := BulkScope{Unrestricted: true}

	rep, err := BulkReplaceCoven(ctx, integrationPool, sel, scope, []string{"prod", "edge"})
	if err != nil {
		t.Fatalf("BulkReplaceCoven: %v", err)
	}
	if rep.Status != BulkCompleted {
		t.Errorf("status = %q, want completed", rep.Status)
	}
	if rep.Matched != 2 || rep.Changed != 2 {
		t.Errorf("matched/changed = %d/%d, want 2/2", rep.Matched, rep.Changed)
	}
	if got := covenOf(t, "a.example.com"); !equalStr(got, []string{"edge", "prod"}) {
		t.Errorf("a.coven = %v, want [edge prod]", got)
	}
	if got := covenOf(t, "b.example.com"); !equalStr(got, []string{"edge", "prod"}) {
		t.Errorf("b.coven = %v, want [edge prod]", got)
	}

	// Idem: повтор с тем же набором → 0 changed (coven IS DISTINCT FROM
	// $labels = false на каждом хосте).
	rep2, err := BulkReplaceCoven(ctx, integrationPool, sel, scope, []string{"prod", "edge"})
	if err != nil {
		t.Fatalf("BulkReplaceCoven repeat: %v", err)
	}
	if rep2.Changed != 0 {
		t.Errorf("repeat changed = %d, want 0 (idem-set-equal)", rep2.Changed)
	}
}

// TestIntegration_Bulk_Replace_EmptySet_ClearsAll — replace с пустым набором
// чистит coven у хостов под selector.
func TestIntegration_Bulk_Replace_EmptySet_ClearsAll(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	seedBulkSoul(t, "a.example.com", []string{"dev", "edge"})

	rep, err := BulkReplaceCoven(ctx, integrationPool, BulkSelector{All: true},
		BulkScope{Unrestricted: true}, []string{})
	if err != nil {
		t.Fatalf("BulkReplaceCoven: %v", err)
	}
	if rep.Changed != 1 {
		t.Errorf("changed = %d, want 1", rep.Changed)
	}
	if got := covenOf(t, "a.example.com"); len(got) != 0 {
		t.Errorf("a.coven = %v, want []", got)
	}
}

// TestIntegration_Bulk_Replace_ScopeIntersection — гейт (a) на replace:
// хосты вне scope не попадают в UPDATE даже при all=true.
func TestIntegration_Bulk_Replace_ScopeIntersection(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	seedBulkSoul(t, "dev-host.example.com", []string{"dev"})
	seedBulkSoul(t, "prod-host.example.com", []string{"prod"})

	rep, err := BulkReplaceCoven(ctx, integrationPool, BulkSelector{All: true},
		BulkScope{Covens: []string{"dev"}}, []string{"dev"})
	if err != nil {
		t.Fatalf("BulkReplaceCoven: %v", err)
	}
	// Только dev-host попадает под scope; набор {dev} равен текущему {dev} →
	// idem-отсев → 0 changed, но matched=1.
	if rep.Matched != 1 {
		t.Errorf("matched = %d, want 1 (prod-host out of scope)", rep.Matched)
	}
	if got := covenOf(t, "prod-host.example.com"); !equalStr(got, []string{"prod"}) {
		t.Errorf("prod-host mutated despite out-of-scope: %v", got)
	}
}

// TestIntegration_Bulk_Replace_LabelOutOfScope — гейт (b) на replace: набор
// с меткой вне scope отвергнут ДО UPDATE.
func TestIntegration_Bulk_Replace_LabelOutOfScope(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	seedBulkSoul(t, "dev-host.example.com", []string{"dev"})

	_, err := BulkReplaceCoven(ctx, integrationPool, BulkSelector{All: true},
		BulkScope{Covens: []string{"dev"}}, []string{"dev", "prod"})
	if !errors.Is(err, ErrBulkLabelOutOfScope) {
		t.Fatalf("err = %v, want ErrBulkLabelOutOfScope", err)
	}
	if got := covenOf(t, "dev-host.example.com"); !equalStr(got, []string{"dev"}) {
		t.Errorf("host mutated despite out-of-scope label set: %v", got)
	}
}

// --- selector.incarnation integration ---

// TestIntegration_Bulk_Selector_Incarnation_Match — bulk по incarnation матчит
// её хосты через `incarnation-name = ANY(coven)`.
func TestIntegration_Bulk_Selector_Incarnation_Match(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	seedBulkSoul(t, "r1.example.com", []string{"redis"})
	seedBulkSoul(t, "r2.example.com", []string{"redis", "dc-eu"})
	seedBulkSoul(t, "other.example.com", []string{"nginx"})

	sel := BulkSelector{Incarnation: "redis"}
	scope := BulkScope{Unrestricted: true}
	rep, err := BulkAssignCoven(ctx, integrationPool, sel, scope, "patched", CovenAppend)
	if err != nil {
		t.Fatalf("BulkAssignCoven: %v", err)
	}
	if rep.Matched != 2 || rep.Changed != 2 {
		t.Errorf("matched/changed = %d/%d, want 2/2 (только redis-хосты)", rep.Matched, rep.Changed)
	}
	if got := covenOf(t, "other.example.com"); !equalStr(got, []string{"nginx"}) {
		t.Errorf("other mutated though not in incarnation: %v", got)
	}
}

// TestIntegration_Bulk_Selector_Incarnation_NoMatch — несуществующая
// incarnation → 0 matched/changed.
func TestIntegration_Bulk_Selector_Incarnation_NoMatch(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	seedBulkSoul(t, "r1.example.com", []string{"redis"})

	sel := BulkSelector{Incarnation: "ghost-incarnation"}
	rep, err := BulkAssignCoven(ctx, integrationPool, sel, BulkScope{Unrestricted: true},
		"patched", CovenAppend)
	if err != nil {
		t.Fatalf("BulkAssignCoven: %v", err)
	}
	if rep.Matched != 0 || rep.Changed != 0 {
		t.Errorf("matched/changed = %d/%d, want 0/0", rep.Matched, rep.Changed)
	}
}

// TestIntegration_Bulk_Selector_Incarnation_ScopeIntersection — incarnation
// + scope (a): scope продолжает резать целевые хосты.
func TestIntegration_Bulk_Selector_Incarnation_ScopeIntersection(t *testing.T) {
	resetAll(t)
	ctx := context.Background()
	// redis-incarnation охватывает оба хоста, но prod-host ВНЕ scope=dev.
	seedBulkSoul(t, "redis-dev.example.com", []string{"redis", "dev"})
	seedBulkSoul(t, "redis-prod.example.com", []string{"redis", "prod"})

	sel := BulkSelector{Incarnation: "redis"}
	scope := BulkScope{Covens: []string{"dev"}}
	rep, err := BulkAssignCoven(ctx, integrationPool, sel, scope, "dev", CovenAppend)
	if err != nil {
		t.Fatalf("BulkAssignCoven: %v", err)
	}
	if rep.Matched != 1 {
		t.Errorf("matched = %d, want 1 (только redis-dev в scope)", rep.Matched)
	}
	if got := covenOf(t, "redis-prod.example.com"); !equalStr(got, []string{"prod", "redis"}) {
		t.Errorf("redis-prod mutated despite out-of-scope: %v", got)
	}
}
