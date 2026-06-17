package handlers

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/souls-guild/soul-stack/keeper/internal/applybus"
	"github.com/souls-guild/soul-stack/keeper/internal/errand"
	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

// fakeErrandPool — узкий мок ExecQueryRower для unit-теста ErrandHandler.ListTyped
// в части query-фильтра `module`. Захватывает SQL и args последнего запроса,
// чтобы assert-нуть форму `module IN ($n,…)` и значения.
type fakeErrandPool struct {
	countSQL  string
	countArgs []any

	selectSQL  string
	selectArgs []any
}

func (f *fakeErrandPool) Exec(_ context.Context, _ string, _ ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, errors.New("fakeErrandPool.Exec: unexpected")
}

func (f *fakeErrandPool) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	f.countSQL = sql
	f.countArgs = args
	return staticRow{values: []any{0}}
}

func (f *fakeErrandPool) Query(_ context.Context, sql string, args ...any) (pgx.Rows, error) {
	f.selectSQL = sql
	f.selectArgs = args
	return &emptyRows{}, nil
}

// TestErrandHandler_ListTyped_ModuleFilter_ForwardedToStore — доменная функция
// ListTyped пробрасывает multi-value module-фильтр в store как IN-предикат
// (handler-native: HTTP/bind покрыт huma-integration в пакете api, здесь — чистая
// доменная логика без (w,r)).
func TestErrandHandler_ListTyped_ModuleFilter_ForwardedToStore(t *testing.T) {
	pool := &fakeErrandPool{}
	store := errand.NewStore(pool)
	h := NewErrandHandler(nil, store, nil)

	_, err := h.ListTyped(context.Background(), ErrandListInput{
		Modules: []string{"core.cmd.shell", "core.exec.run"},
		Offset:  0,
		Limit:   50,
	})
	if err != nil {
		t.Fatalf("ListTyped: %v", err)
	}

	// COUNT SQL должен нести IN-предикат с двумя плейсхолдерами и двумя args.
	if !strings.Contains(pool.countSQL, "module IN") {
		t.Errorf("count SQL не содержит module IN: %q", pool.countSQL)
	}
	if len(pool.countArgs) != 2 {
		t.Fatalf("count args = %v, want 2 module-values", pool.countArgs)
	}
	if pool.countArgs[0] != "core.cmd.shell" || pool.countArgs[1] != "core.exec.run" {
		t.Errorf("count args order: %v", pool.countArgs)
	}

	// SELECT SQL — то же IN + LIMIT/OFFSET добавлены справа.
	if !strings.Contains(pool.selectSQL, "module IN") {
		t.Errorf("select SQL не содержит module IN: %q", pool.selectSQL)
	}
	if len(pool.selectArgs) != 4 {
		t.Fatalf("select args = %v (want 2 module + limit + offset)", pool.selectArgs)
	}
}

func TestErrandHandler_ListTyped_NoModule_NoINPredicate(t *testing.T) {
	pool := &fakeErrandPool{}
	store := errand.NewStore(pool)
	h := NewErrandHandler(nil, store, nil)

	if _, err := h.ListTyped(context.Background(), ErrandListInput{Offset: 0, Limit: 50}); err != nil {
		t.Fatalf("ListTyped: %v", err)
	}
	if strings.Contains(pool.countSQL, "module IN") {
		t.Errorf("count SQL не должна содержать module IN без фильтра: %q", pool.countSQL)
	}
}

// --- Cancel (slice E5): sentinel→problem-классификация через CancelTyped напрямую ---
//
// (w,r)-оболочка снята (HTTP обслуживает huma, MCP зовёт dispatcher напрямую). Тесты
// бьют доменную CancelTyped и проверяют problem-Type извлечённой ошибки.

// claimsFor — минимальный jwt.Claims с заданным Subject.
func claimsFor(aid string) *jwt.Claims { return &jwt.Claims{Subject: aid} }

// cancelProblemType извлекает problem.Type из ошибки CancelTyped (nil → "").
func cancelProblemType(t *testing.T, err error) string {
	t.Helper()
	if err == nil {
		return ""
	}
	d, ok := AsProblemDetails(err)
	if !ok {
		t.Fatalf("ошибка не *problemError: %T %v", err, err)
	}
	return d.Type
}

func TestErrandHandler_CancelTyped_NotFound(t *testing.T) {
	d := buildCancelDispatcher(t, nil) // пустой store
	h := NewErrandHandler(d, nil, nil)
	_, err := h.CancelTyped(context.Background(), claimsFor("archon-alice"), "MISSING-ID")
	if got := cancelProblemType(t, err); !strings.Contains(got, "not-found") {
		t.Fatalf("problem.Type = %q, ожидался not-found", got)
	}
}

func TestErrandHandler_CancelTyped_TerminalState(t *testing.T) {
	d := buildCancelDispatcher(t, map[string]errand.Status{"ERR-TERM": errand.StatusSuccess})
	h := NewErrandHandler(d, nil, nil)
	_, err := h.CancelTyped(context.Background(), claimsFor("archon-alice"), "ERR-TERM")
	if got := cancelProblemType(t, err); !strings.Contains(got, "errand-not-cancellable") {
		t.Fatalf("problem.Type = %q, ожидался errand-not-cancellable (409 terminal)", got)
	}
}

func TestErrandHandler_CancelTyped_NoDispatcher(t *testing.T) {
	h := NewErrandHandler(nil, nil, nil)
	_, err := h.CancelTyped(context.Background(), claimsFor("archon-alice"), "ANY")
	if got := cancelProblemType(t, err); !strings.Contains(got, "internal") {
		t.Fatalf("problem.Type = %q, ожидался internal (dispatcher nil)", got)
	}
}

func TestErrandHandler_CancelTyped_Happy(t *testing.T) {
	d := buildCancelDispatcher(t, map[string]errand.Status{"ERR-OK": errand.StatusRunning})
	h := NewErrandHandler(d, nil, nil)
	reply, err := h.CancelTyped(context.Background(), claimsFor("archon-alice"), "ERR-OK")
	if err != nil {
		t.Fatalf("CancelTyped: %v", err)
	}
	if reply.ErrandID != "ERR-OK" {
		t.Fatalf("reply.ErrandID = %q, want ERR-OK", reply.ErrandID)
	}
}

// --- helpers для DELETE-теста ---

// fakeCancelStore — in-memory StoreAPI. Заполняется заранее (map status); Get
// возвращает Row{SID, Module, Status} либо ErrNotFound.
type fakeCancelStore struct {
	mu   sync.Mutex
	rows map[string]errand.Row
}

func newFakeCancelStore(statuses map[string]errand.Status) *fakeCancelStore {
	s := &fakeCancelStore{rows: map[string]errand.Row{}}
	now := time.Now()
	for id, st := range statuses {
		s.rows[id] = errand.Row{
			ErrandID:     id,
			SID:          "host.test",
			Module:       "core.cmd.shell",
			Status:       st,
			StartedByAID: "archon-alice",
			StartedByKID: "kid-test",
			StartedAt:    now,
			TTLAt:        now.Add(time.Hour),
		}
	}
	return s
}

func (s *fakeCancelStore) Insert(_ context.Context, row errand.Row) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows[row.ErrandID] = row
	return nil
}
func (s *fakeCancelStore) MarkTerminal(_ context.Context, id string, upd errand.TerminalUpdate) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.rows[id]
	if !ok || r.Status != errand.StatusRunning {
		return false, nil
	}
	r.Status = upd.Status
	s.rows[id] = r
	return true, nil
}
func (s *fakeCancelStore) SweepOrphanRunning(_ context.Context, _ string, _ time.Duration, _ string) ([]string, error) {
	return nil, nil
}
func (s *fakeCancelStore) Get(_ context.Context, id string) (*errand.Row, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.rows[id]
	if !ok {
		return nil, errand.ErrNotFound
	}
	cp := r
	return &cp, nil
}

// fakeCancelOutbound — accept-all Outbound. Зачисляет SendCancelErrand-вызовы.
type fakeCancelOutbound struct{}

func (fakeCancelOutbound) SendErrand(_ context.Context, _ string, _ *keeperv1.ErrandRequest) error {
	return nil
}
func (fakeCancelOutbound) PublishErrand(_ context.Context, _ string, _ *keeperv1.ErrandRequest) error {
	return nil
}
func (fakeCancelOutbound) SendCancelErrand(_ context.Context, _ string, _ string) error {
	return nil
}
func (fakeCancelOutbound) PublishCancelErrand(_ context.Context, _ string, _ string) error {
	return nil
}

// fakeBusCancel — applybus-stub: Subscribe возвращает empty-чанал. dispatcher
// Cancel-flow его не использует (только Dispatch-flow), но интерфейс требует.
type fakeBusCancel struct{}

func (b fakeBusCancel) Subscribe(ctx context.Context, applyID string) <-chan applybus.Event {
	return b.SubscribeWithBridge(ctx, applyID, true)
}

func (fakeBusCancel) SubscribeWithBridge(_ context.Context, _ string, _ bool) <-chan applybus.Event {
	ch := make(chan applybus.Event)
	return ch
}

// buildCancelDispatcher собирает Dispatcher с in-memory fake-зависимостями.
// statuses — map errand_id → начальный статус (если nil → пустой store).
func buildCancelDispatcher(t *testing.T, statuses map[string]errand.Status) *errand.Dispatcher {
	t.Helper()
	store := newFakeCancelStore(statuses)
	d, err := errand.NewDispatcher(errand.Deps{
		Store:    store,
		Outbound: fakeCancelOutbound{},
		ApplyBus: fakeBusCancel{},
		Logger:   slog.New(slog.NewJSONHandler(io.Discard, nil)),
		KID:      "kid-test",
	})
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}
	return d
}

// --- guard-тесты доменной проекции (handler-native): rowToErrandResultView /
// dispatchResultView / newErrandAcceptedView строят плоские view-ы; точные wire-байты
// native ErrandResult/ErrandAccepted пинит golden в пакете api (huma_errand_reply_test.go).
// Здесь проверяем доменную проекцию полей view-а (status-строка, секундный UTC date-time,
// nil-при-пустом для опц.). ---

// TestRowToErrandResultView_FieldProjection фиксирует доменные инварианты проекции:
// status — плоская строка домен-статуса; started_at/finished_at — секундный UTC
// (Truncate(Second)); truncated-флаги — nil при false; пустые stdout/stderr/
// error_message/output — nil; непустой stdout — указатель.
func TestRowToErrandResultView_FieldProjection(t *testing.T) {
	fin := time.Date(2026, 6, 9, 10, 0, 5, 987654321, time.UTC)
	row := &errand.Row{
		ErrandID:     "01J0000000000000000000000",
		SID:          "host.test",
		Module:       "core.cmd.shell",
		Status:       errand.StatusSuccess,
		Stdout:       "ok\n",
		StartedByAID: "archon-alice",
		StartedAt:    time.Date(2026, 6, 9, 10, 0, 0, 123456789, time.UTC),
		FinishedAt:   &fin,
	}
	v := rowToErrandResultView(row)

	if v.Status != "success" {
		t.Errorf("status не плоская строка домен-статуса: %q", v.Status)
	}
	if !v.StartedAt.Equal(time.Date(2026, 6, 9, 10, 0, 0, 0, time.UTC)) {
		t.Errorf("started_at не усечён до секунды UTC: %v", v.StartedAt)
	}
	if v.FinishedAt == nil || !v.FinishedAt.Equal(time.Date(2026, 6, 9, 10, 0, 5, 0, time.UTC)) {
		t.Errorf("finished_at не усечён до секунды UTC: %v", v.FinishedAt)
	}
	if v.StdoutTruncated != nil || v.StderrTruncated != nil {
		t.Errorf("truncated-флаги должны быть nil при false: %v %v", v.StdoutTruncated, v.StderrTruncated)
	}
	if v.Stderr != nil || v.ErrorMessage != nil || v.Output != nil {
		t.Errorf("пустые опциональные поля должны быть nil: %v %v %v", v.Stderr, v.ErrorMessage, v.Output)
	}
	if v.Stdout == nil || *v.Stdout != "ok\n" {
		t.Errorf("непустой stdout должен быть указателем на значение: %v", v.Stdout)
	}
}

// TestRowToErrandResultView_TruncatedPresentWhenTrue — при true truncated-флаг
// становится non-nil указателем (граница nil-при-false).
func TestRowToErrandResultView_TruncatedPresentWhenTrue(t *testing.T) {
	row := &errand.Row{
		ErrandID:        "01J0000000000000000000001",
		SID:             "host.test",
		Module:          "core.cmd.shell",
		Status:          errand.StatusSuccess,
		Stdout:          "x",
		StdoutTruncated: true,
		StartedByAID:    "archon-alice",
		StartedAt:       time.Now(),
	}
	v := rowToErrandResultView(row)
	if v.StdoutTruncated == nil || !*v.StdoutTruncated {
		t.Errorf("stdout_truncated=true должен быть non-nil true: %v", v.StdoutTruncated)
	}
	if v.StderrTruncated != nil {
		t.Errorf("stderr_truncated=false должен быть nil: %v", v.StderrTruncated)
	}
}

// --- exec-путь sync-200 / 202-Accepted (handler-native): проекция DispatchResult /
// Accepted из request-данных в плоский view. Точные wire-байты пинит golden в пакете api;
// здесь — доменные инварианты полей (status-строка, реальный started_at секундный UTC,
// sid/module/started_by_aid из request-а). ---

// TestDispatchResultView_FieldProjection — sync-200 Exec-view: status-строка; started_at —
// реальный момент старта из DispatchResult.StartedAt (НЕ zero, НЕ time.Now()-фабрикация),
// усечённый до секунды; sid/module/started_by_aid берутся из request-а.
func TestDispatchResultView_FieldProjection(t *testing.T) {
	started := time.Date(2026, 6, 9, 10, 0, 0, 123456789, time.UTC)
	res := &errand.DispatchResult{
		ErrandID:  "01J0000000000000000000002",
		Status:    errand.StatusSuccess,
		Stdout:    "done",
		StartedAt: started,
	}
	v := dispatchResultView(res, "host.test", "core.cmd.shell", "archon-alice")

	if v.Status != "success" {
		t.Errorf("status не плоская строка домен-статуса: %q", v.Status)
	}
	if v.SID != "host.test" || v.Module != "core.cmd.shell" || v.StartedByAID != "archon-alice" {
		t.Errorf("sid/module/aid из request не проброшены: %+v", v)
	}
	if v.StartedAt.IsZero() {
		t.Errorf("started_at должен быть заполнен (не zero time)")
	}
	if v.StartedAt.Nanosecond() != 0 {
		t.Errorf("started_at должен быть усечён до секунды: %v", v.StartedAt)
	}
	if !v.StartedAt.Equal(time.Date(2026, 6, 9, 10, 0, 0, 0, time.UTC)) {
		t.Errorf("started_at должен быть = DispatchResult.StartedAt (секундный UTC): %v", v.StartedAt)
	}
	if v.Stdout == nil || *v.Stdout != "done" {
		t.Errorf("stdout должен быть указателем на значение: %v", v.Stdout)
	}
}

// TestNewErrandAcceptedView_FieldProjection — 202-view: errand_id + status-строка.
func TestNewErrandAcceptedView_FieldProjection(t *testing.T) {
	v := newErrandAcceptedView("01J0000000000000000000003", errand.StatusRunning)
	if v.ErrandID != "01J0000000000000000000003" || v.Status != "running" {
		t.Errorf("accepted view: %+v", v)
	}
}
