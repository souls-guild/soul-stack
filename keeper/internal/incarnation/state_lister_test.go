package incarnation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/souls-guild/soul-stack/keeper/internal/statepredicate"
)

// Слайс S2: StateLister — production-адаптер statepredicate.IncarnationStateLister
// поверх SelectAll. Unit-тесты через fake-db (без PG): проверяют (1) маппинг
// BaseFilter→ListFilter (pushdown service/coven доходит до SQL-args), (2) реальный
// page-by-page дренаж (набор > страницы → все строки обойдены через offset/limit,
// ни одна не потеряна), (3) проброс ошибок. Интеграция с живой PG — отдельный
// integration-тест (build-tag).

// statePageDB — fake-db, моделирующий SelectAll для StateLister: COUNT отдаёт
// total, SELECT — срез строк по offset/limit из args. Захватывает COUNT-args
// (проверка pushdown service/coven). Каждая строка — 14 колонок scanIncarnation.
type statePageDB struct {
	rows       []*Incarnation // полный набор (имитация уже-сужённого SQL-результата)
	countArgs  []any          // args COUNT-запроса (= base-pushdown bind-параметры)
	queryArgs  [][]any        // args каждого SELECT (для контроля offset/limit-цикла)
	queryCalls int
	selectErr  error // если задано — SELECT возвращает ошибку
}

func (d *statePageDB) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, errors.New("statePageDB: Exec не ожидается")
}

func (d *statePageDB) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	if strings.Contains(sql, "COUNT(*)") {
		d.countArgs = args
		return stateCountRow{total: len(d.rows)}
	}
	return errRow{err: pgx.ErrNoRows}
}

func (d *statePageDB) Query(_ context.Context, sql string, args ...any) (pgx.Rows, error) {
	if !strings.Contains(sql, "SELECT name") {
		return nil, fmt.Errorf("statePageDB: неожиданный Query: %s", sql)
	}
	d.queryCalls++
	d.queryArgs = append(d.queryArgs, args)
	if d.selectErr != nil {
		return nil, d.selectErr
	}
	// Последние два bind-параметра SelectAll — OFFSET, LIMIT.
	offset := args[len(args)-2].(int)
	limit := args[len(args)-1].(int)
	end := offset + limit
	if end > len(d.rows) {
		end = len(d.rows)
	}
	var page []staticRow
	if offset < len(d.rows) {
		for _, inc := range d.rows[offset:end] {
			page = append(page, incToStaticRow(inc))
		}
	}
	return &fakeRows{rows: page}, nil
}

// stateCountRow — Row для COUNT(*): scan total в *int.
type stateCountRow struct{ total int }

func (r stateCountRow) Scan(dest ...any) error {
	if len(dest) != 1 {
		return errors.New("stateCountRow: ожидался один dest")
	}
	*dest[0].(*int) = r.total
	return nil
}

// incToStaticRow упаковывает Incarnation в 16-колоночный staticRow в порядке
// scanIncarnation (name, service, service_version, state_schema_version, spec,
// state, status, status_details, created_by_aid, created_at, updated_at, covens,
// traits, last_drift_check_at, last_drift_summary, created_scenario).
func incToStaticRow(inc *Incarnation) staticRow {
	stateBytes, _ := json.Marshal(inc.State)
	specBytes := []byte("{}")
	now := time.Date(2026, 6, 2, 0, 0, 0, 0, time.UTC)
	// nil-колонки передаются нетипизированным nil: assign различает NULL по
	// `src == nil` (типизированный nil-указатель в interface{} != nil).
	return staticRow{values: []any{
		inc.Name,
		inc.Service,
		"v1",
		1,
		specBytes,
		stateBytes,
		string(StatusReady),
		nil, // status_details
		nil, // created_by_aid
		now,
		now,
		nil,          // covens
		[]byte("{}"), // traits
		nil,          // last_drift_check_at
		nil,          // last_drift_summary
		"create",     // created_scenario
		nil,          // applying_apply_id
	}}
}

func newStated(name string, state map[string]any) *Incarnation {
	return &Incarnation{Name: name, Service: "redis", State: state}
}

func collectPages(t *testing.T, l *StateLister, base statepredicate.BaseFilter) []statepredicate.Stated {
	t.Helper()
	var all []statepredicate.Stated
	err := l.ListStatePages(context.Background(), base, func(page []statepredicate.Stated) error {
		all = append(all, page...)
		return nil
	})
	if err != nil {
		t.Fatalf("ListStatePages: %v", err)
	}
	return all
}

// --- pushdown: BaseFilter (service/coven) доходит до SQL как bind-args ---

func TestStateLister_PushdownToSQL(t *testing.T) {
	db := &statePageDB{rows: []*Incarnation{
		newStated("a", map[string]any{"k": "v"}),
	}}
	l := NewStateLister(db)

	_ = collectPages(t, l, statepredicate.BaseFilter{Service: "redis", Coven: "prod"})

	// buildListWhere для Service+Coven биндит два параметра: service, coven.
	if len(db.countArgs) != 2 {
		t.Fatalf("COUNT получил %d args, want 2 (service+coven pushdown)", len(db.countArgs))
	}
	if db.countArgs[0] != "redis" {
		t.Errorf("первый bind=%v, want redis (service pushdown)", db.countArgs[0])
	}
	if db.countArgs[1] != "prod" {
		t.Errorf("второй bind=%v, want prod (coven pushdown)", db.countArgs[1])
	}
}

// --- BaseFilter.Covens (ADDITIVE multi-coven) → coven∪{name} scope в SQL ---

// TestStateLister_MultiCovenScope_PushdownCovenUnionName — ADDITIVE-поле
// BaseFilter.Covens (S3b-3) маппится в ListScope coven∪{name}: и covens[]-
// пересечение, и name-равенство доходят до SQL одним bind-ом. Старый путь
// (single Coven через ListFilter) при этом НЕ задействован.
func TestStateLister_MultiCovenScope_PushdownCovenUnionName(t *testing.T) {
	db := &statePageDB{rows: []*Incarnation{newStated("a", map[string]any{"k": "v"})}}
	l := NewStateLister(db)

	_ = collectPages(t, l, statepredicate.BaseFilter{Covens: []string{"redis-prod"}})

	// countArgs = [scope-covens] (один bind для обоих плеч coven∪{name}).
	if len(db.countArgs) != 1 {
		t.Fatalf("COUNT получил %d args, want 1 (coven∪{name} один bind)", len(db.countArgs))
	}
	covs, ok := db.countArgs[0].([]string)
	if !ok || len(covs) != 1 || covs[0] != "redis-prod" {
		t.Errorf("scope-covens bind = %v, want [redis-prod]", db.countArgs[0])
	}
}

// TestStateLister_NoCovens_UnrestrictedScope — без BaseFilter.Covens лист
// дренируется БЕЗ scope-сужения (Unrestricted): state-CEL резолвится по всему
// service-сужённому множеству (типовой S3b-3 List, где coven и state —
// независимые OR-измерения). countArgs пуст (ни service, ни coven, ни scope).
func TestStateLister_NoCovens_UnrestrictedScope(t *testing.T) {
	db := &statePageDB{rows: []*Incarnation{newStated("a", map[string]any{"k": "v"})}}
	l := NewStateLister(db)

	_ = collectPages(t, l, statepredicate.BaseFilter{})

	if len(db.countArgs) != 0 {
		t.Errorf("без Covens scope обязан быть Unrestricted (без bind-args), got %v", db.countArgs)
	}
}

// --- page-by-page: набор больше страницы → несколько SELECT-ов, все строки обойдены ---

func TestStateLister_PageByPage(t *testing.T) {
	// 2.5 страницы (statePageSize=1000) — проверяем многостраничный дренаж и что
	// ни одна строка не потеряна на границах. Каждая инкарнация уникальна по имени.
	const n = statePageSize*2 + 17
	rows := make([]*Incarnation, n)
	for i := range rows {
		rows[i] = newStated(fmt.Sprintf("inc-%05d", i), map[string]any{"idx": i})
	}
	db := &statePageDB{rows: rows}
	l := NewStateLister(db)

	got := collectPages(t, l, statepredicate.BaseFilter{Service: "redis"})

	if len(got) != n {
		t.Fatalf("обойдено %d строк, want %d (ни одна не потеряна)", len(got), n)
	}
	// Три страницы (1000 + 1000 + 17).
	if db.queryCalls != 3 {
		t.Fatalf("SELECT-ов %d, want 3 (offset/limit-цикл по страницам)", db.queryCalls)
	}
	// Offset-ы возрастают на размер страницы.
	wantOffsets := []int{0, statePageSize, statePageSize * 2}
	for i, args := range db.queryArgs {
		off := args[len(args)-2].(int)
		if off != wantOffsets[i] {
			t.Errorf("страница %d: offset=%d, want %d", i, off, wantOffsets[i])
		}
	}
	// Все имена уникальны и присутствуют.
	seen := make(map[string]bool, n)
	for _, s := range got {
		if seen[s.Name] {
			t.Fatalf("дубликат %q (страница перечитана)", s.Name)
		}
		seen[s.Name] = true
		if s.State == nil {
			t.Fatalf("%q: state не пробросился", s.Name)
		}
	}
}

// --- ровно одна полная страница → один SELECT, без лишнего запроса ---

func TestStateLister_ExactPage(t *testing.T) {
	rows := make([]*Incarnation, statePageSize)
	for i := range rows {
		rows[i] = newStated(fmt.Sprintf("inc-%05d", i), map[string]any{"idx": i})
	}
	db := &statePageDB{rows: rows}
	l := NewStateLister(db)

	got := collectPages(t, l, statepredicate.BaseFilter{})
	if len(got) != statePageSize {
		t.Fatalf("got %d, want %d", len(got), statePageSize)
	}
	// total==statePageSize: offset+len >= total на первой странице → второй SELECT
	// не нужен (offset+1000 >= 1000).
	if db.queryCalls != 1 {
		t.Errorf("SELECT-ов %d, want 1 (ровно страница, без лишнего round-trip)", db.queryCalls)
	}
}

// --- пустой набор → один SELECT, пустой yield (callback не падает) ---

func TestStateLister_EmptySet(t *testing.T) {
	db := &statePageDB{rows: nil}
	l := NewStateLister(db)

	yields := 0
	err := l.ListStatePages(context.Background(), statepredicate.BaseFilter{}, func(page []statepredicate.Stated) error {
		yields++
		return nil
	})
	if err != nil {
		t.Fatalf("ListStatePages: %v", err)
	}
	if yields != 0 {
		t.Errorf("пустой набор: yield вызван %d раз, want 0 (пустые страницы не отдаются)", yields)
	}
}

// --- ошибка SELECT пробрасывается ---

func TestStateLister_SelectError(t *testing.T) {
	sentinel := errors.New("db down")
	db := &statePageDB{rows: []*Incarnation{newStated("a", map[string]any{"k": 1})}, selectErr: sentinel}
	l := NewStateLister(db)

	err := l.ListStatePages(context.Background(), statepredicate.BaseFilter{}, func([]statepredicate.Stated) error { return nil })
	if !errors.Is(err, sentinel) {
		t.Fatalf("ошибка SELECT должна проброситься: got %v", err)
	}
}

// --- ошибка yield прерывает дренаж и пробрасывается (lazy: лишние страницы не тянутся) ---

func TestStateLister_YieldErrorStops(t *testing.T) {
	const n = statePageSize + 10 // две страницы
	rows := make([]*Incarnation, n)
	for i := range rows {
		rows[i] = newStated(fmt.Sprintf("inc-%05d", i), map[string]any{"idx": i})
	}
	db := &statePageDB{rows: rows}
	l := NewStateLister(db)

	sentinel := errors.New("not-bool on full state")
	err := l.ListStatePages(context.Background(), statepredicate.BaseFilter{}, func([]statepredicate.Stated) error {
		return sentinel // падаем на первой же странице
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("ошибка yield должна проброситься: got %v", err)
	}
	if db.queryCalls != 1 {
		t.Errorf("после ошибки yield SELECT-ов %d, want 1 (вторая страница не тянется)", db.queryCalls)
	}
}
