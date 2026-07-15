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

// fakeErrandPool — a narrow mock ExecQueryRower for the unit test of ErrandHandler.ListTyped
// covering the `module` query filter. Captures the SQL and args of the last query
// to assert the `module IN ($n,…)` shape and the values.
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

// TestErrandHandler_ListTyped_ModuleFilter_ForwardedToStore — the domain function
// ListTyped forwards the multi-value module filter into the store as an IN predicate
// (handler-native: HTTP/bind is covered by huma-integration in package api, here — pure
// domain logic without (w,r)).
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

	// COUNT SQL must carry an IN predicate with two placeholders and two args.
	if !strings.Contains(pool.countSQL, "module IN") {
		t.Errorf("count SQL не содержит module IN: %q", pool.countSQL)
	}
	if len(pool.countArgs) != 2 {
		t.Fatalf("count args = %v, want 2 module-values", pool.countArgs)
	}
	if pool.countArgs[0] != "core.cmd.shell" || pool.countArgs[1] != "core.exec.run" {
		t.Errorf("count args order: %v", pool.countArgs)
	}

	// SELECT SQL — the same IN + LIMIT/OFFSET appended on the right.
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

// --- Cancel (slice E5): sentinel→problem classification via CancelTyped directly ---
//
// The (w,r) wrapper is removed (HTTP is served by huma, MCP calls the dispatcher directly). The tests
// hit the domain CancelTyped and check the problem-Type of the extracted error.

// claimsFor — a minimal jwt.Claims with the given Subject.
func claimsFor(aid string) *jwt.Claims { return &jwt.Claims{Subject: aid} }

// cancelProblemType extracts problem.Type from a CancelTyped error (nil → "").
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
	d := buildCancelDispatcher(t, nil) // empty store
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

// --- helpers for the DELETE test ---

// fakeCancelStore — an in-memory StoreAPI. Prefilled (map status); Get
// returns Row{SID, Module, Status} or ErrNotFound.
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

// fakeCancelOutbound — an accept-all Outbound. Accepts SendCancelErrand calls.
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

// fakeBusCancel — an applybus stub: Subscribe returns an empty channel. The dispatcher
// Cancel-flow does not use it (only Dispatch-flow), but the interface requires it.
type fakeBusCancel struct{}

func (b fakeBusCancel) Subscribe(ctx context.Context, applyID string) <-chan applybus.Event {
	return b.SubscribeWithBridge(ctx, applyID, true)
}

func (fakeBusCancel) SubscribeWithBridge(_ context.Context, _ string, _ bool) <-chan applybus.Event {
	ch := make(chan applybus.Event)
	return ch
}

// buildCancelDispatcher builds a Dispatcher with in-memory fake dependencies.
// statuses — map errand_id → initial status (if nil → empty store).
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

// --- guard tests for the domain projection (handler-native): rowToErrandResultView /
// dispatchResultView / newErrandAcceptedView build flat views; the exact wire bytes of
// native ErrandResult/ErrandAccepted are pinned by golden in package api (huma_errand_reply_test.go).
// Here we check the domain projection of view fields (status string, second-precision UTC date-time,
// nil-when-empty for optionals). ---

// TestRowToErrandResultView_FieldProjection pins the domain invariants of the projection:
// status — a flat domain-status string; started_at/finished_at — second-precision UTC
// (Truncate(Second)); truncated flags — nil when false; empty stdout/stderr/
// error_message/output — nil; non-empty stdout — a pointer.
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

// TestRowToErrandResultView_TruncatedPresentWhenTrue — when true the truncated flag
// becomes a non-nil pointer (the nil-when-false boundary).
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

// --- exec path sync-200 / 202-Accepted (handler-native): projection of DispatchResult /
// Accepted from request data into a flat view. The exact wire bytes are pinned by golden in package api;
// here — the domain field invariants (status string, real started_at second-precision UTC,
// sid/module/started_by_aid from the request). ---

// TestDispatchResultView_FieldProjection — sync-200 Exec view: status string; started_at —
// the real start moment from DispatchResult.StartedAt (NOT zero, NOT a time.Now() fabrication),
// truncated to the second; sid/module/started_by_aid taken from the request.
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

// TestNewErrandAcceptedView_FieldProjection — 202 view: errand_id + status string.
func TestNewErrandAcceptedView_FieldProjection(t *testing.T) {
	v := newErrandAcceptedView("01J0000000000000000000003", errand.StatusRunning)
	if v.ErrandID != "01J0000000000000000000003" || v.Status != "running" {
		t.Errorf("accepted view: %+v", v)
	}
}
