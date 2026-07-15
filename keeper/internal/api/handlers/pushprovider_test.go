package handlers

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	"github.com/souls-guild/soul-stack/keeper/internal/pushprovider"
)

// fakePushProviderPool — mock pushprovider.ExecQueryRower for the handler tests.
// A minimal imitation of PG behavior: name → entry in a map, no concurrency
// (unit-test single-thread).
type fakePushProviderPool struct {
	entries     map[string]*pushprovider.PushProvider
	updateErr   error
	deleteErr   error
	insertErr   error
	selectByErr error
}

func newFakePool() *fakePushProviderPool {
	return &fakePushProviderPool{entries: make(map[string]*pushprovider.PushProvider)}
}

func (f *fakePushProviderPool) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	switch {
	case strings.Contains(sql, "UPDATE push_providers"):
		if f.updateErr != nil {
			return pgconn.NewCommandTag("UPDATE 0"), f.updateErr
		}
		name := args[0].(string)
		p, ok := f.entries[name]
		if !ok {
			return pgconn.NewCommandTag("UPDATE 0"), nil
		}
		paramsBytes := args[1].([]byte)
		var params map[string]any
		_ = json.Unmarshal(paramsBytes, &params)
		p.Params = params
		p.UpdatedAt = time.Now()
		if args[2] != nil {
			s := args[2].(string)
			p.UpdatedByAID = &s
		}
		return pgconn.NewCommandTag("UPDATE 1"), nil
	case strings.Contains(sql, "DELETE FROM push_providers"):
		if f.deleteErr != nil {
			return pgconn.NewCommandTag("DELETE 0"), f.deleteErr
		}
		name := args[0].(string)
		if _, ok := f.entries[name]; !ok {
			return pgconn.NewCommandTag("DELETE 0"), nil
		}
		delete(f.entries, name)
		return pgconn.NewCommandTag("DELETE 1"), nil
	}
	return pgconn.NewCommandTag(""), nil
}

func (f *fakePushProviderPool) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	if strings.Contains(sql, "INSERT INTO push_providers") {
		if f.insertErr != nil {
			return errRowPP{err: f.insertErr}
		}
		name := args[0].(string)
		if _, exists := f.entries[name]; exists {
			return errRowPP{err: &pgconn.PgError{Code: "23505", ConstraintName: "push_providers_pkey"}}
		}
		paramsBytes := args[1].([]byte)
		var params map[string]any
		_ = json.Unmarshal(paramsBytes, &params)
		now := time.Now()
		f.entries[name] = &pushprovider.PushProvider{
			Name:         name,
			Params:       params,
			CreatedAt:    now,
			UpdatedAt:    now,
			CreatedByAID: args[2].(string),
		}
		return scanRowPP{values: []any{now, now}}
	}
	if strings.Contains(sql, "SELECT") && strings.Contains(sql, "FROM push_providers") && strings.Contains(sql, "WHERE name = $1") {
		if f.selectByErr != nil {
			return errRowPP{err: f.selectByErr}
		}
		name := args[0].(string)
		p, ok := f.entries[name]
		if !ok {
			return errRowPP{err: pgx.ErrNoRows}
		}
		paramsBytes, _ := json.Marshal(p.Params)
		return scanRowPP{values: []any{
			p.Name, paramsBytes, p.CreatedAt, p.UpdatedAt, p.CreatedByAID, p.UpdatedByAID,
		}}
	}
	if strings.Contains(sql, "SELECT COUNT(*)") {
		return countRowPP{n: len(f.entries)}
	}
	return errRowPP{err: pgx.ErrNoRows}
}

func (f *fakePushProviderPool) Query(_ context.Context, _ string, _ ...any) (pgx.Rows, error) {
	rows := make([][]any, 0, len(f.entries))
	for _, p := range f.entries {
		paramsBytes, _ := json.Marshal(p.Params)
		rows = append(rows, []any{p.Name, paramsBytes, p.CreatedAt, p.UpdatedAt, p.CreatedByAID, p.UpdatedByAID})
	}
	return &fakeRowsPP{rows: rows}, nil
}

type errRowPP struct{ err error }

func (r errRowPP) Scan(_ ...any) error { return r.err }

type scanRowPP struct{ values []any }

func (r scanRowPP) Scan(dest ...any) error {
	for i, d := range dest {
		switch dst := d.(type) {
		case *string:
			*dst = r.values[i].(string)
		case *time.Time:
			*dst = r.values[i].(time.Time)
		case **string:
			if r.values[i] == nil {
				*dst = nil
				continue
			}
			if p, ok := r.values[i].(*string); ok {
				*dst = p
				continue
			}
			s := r.values[i].(string)
			*dst = &s
		case *[]byte:
			*dst = r.values[i].([]byte)
		}
	}
	return nil
}

type countRowPP struct{ n int }

func (r countRowPP) Scan(dest ...any) error {
	if p, ok := dest[0].(*int); ok {
		*p = r.n
	}
	return nil
}

type fakeRowsPP struct {
	rows [][]any
	idx  int
}

func (r *fakeRowsPP) Next() bool {
	if r.idx >= len(r.rows) {
		return false
	}
	r.idx++
	return true
}

func (r *fakeRowsPP) Scan(dest ...any) error {
	row := r.rows[r.idx-1]
	for i, d := range dest {
		switch dst := d.(type) {
		case *string:
			*dst = row[i].(string)
		case *time.Time:
			*dst = row[i].(time.Time)
		case **string:
			if row[i] == nil {
				*dst = nil
				continue
			}
			if p, ok := row[i].(*string); ok {
				*dst = p
				continue
			}
			s := row[i].(string)
			*dst = &s
		case *[]byte:
			*dst = row[i].([]byte)
		}
	}
	return nil
}

func (r *fakeRowsPP) Err() error                                   { return nil }
func (r *fakeRowsPP) Close()                                       {}
func (r *fakeRowsPP) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *fakeRowsPP) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *fakeRowsPP) Values() ([]any, error)                       { return nil, nil }
func (r *fakeRowsPP) RawValues() [][]byte                          { return nil }
func (r *fakeRowsPP) Conn() *pgx.Conn                              { return nil }

func newPushProviderHandler(t *testing.T) (*PushProviderHandler, *fakePushProviderPool) {
	t.Helper()
	pool := newFakePool()
	svc, err := pushprovider.NewService(pushprovider.ServiceDeps{Pool: pool})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return NewPushProviderHandler(svc, nil), pool
}

// ppProblemType extracts problem.Type from a *Typed-function error (nil → "").
func ppProblemType(t *testing.T, err error) string {
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

// pmap — a pointer to a map for request Params (huma gives *map for an optional field).
func pmap(m map[string]any) *map[string]any { return &m }

func TestPushProviderHandler_CreateTyped_Success(t *testing.T) {
	h, _ := newPushProviderHandler(t)
	reply, err := h.CreateTyped(context.Background(), claimsFor("archon-alice"),
		PushProviderCreateInput{Name: "vault-bastion", Params: pmap(map[string]any{"vault_addr": "https://vault.example.com"})})
	if err != nil {
		t.Fatalf("CreateTyped: %v", err)
	}
	if reply.Body.Name != "vault-bastion" {
		t.Errorf("name = %v", reply.Body.Name)
	}
	if reply.Body.CreatedByAID != "archon-alice" {
		t.Errorf("created_by_aid = %v", reply.Body.CreatedByAID)
	}
}

func TestPushProviderHandler_CreateTyped_RejectsPlainSensitive_422(t *testing.T) {
	h, _ := newPushProviderHandler(t)
	_, err := h.CreateTyped(context.Background(), claimsFor("archon-alice"),
		PushProviderCreateInput{Name: "vault", Params: pmap(map[string]any{"secret_id": "plain-leaked"})})
	if got := ppProblemType(t, err); got != problem.TypeValidationFailed {
		t.Fatalf("problem.Type = %q, want %q (sensitive plain → 422)", got, problem.TypeValidationFailed)
	}
}

func TestPushProviderHandler_CreateTyped_InvalidName_422(t *testing.T) {
	h, _ := newPushProviderHandler(t)
	_, err := h.CreateTyped(context.Background(), claimsFor("archon-alice"),
		PushProviderCreateInput{Name: "1bad-name"})
	if got := ppProblemType(t, err); got != problem.TypeValidationFailed {
		t.Errorf("problem.Type = %q, want %q", got, problem.TypeValidationFailed)
	}
}

func TestPushProviderHandler_CreateTyped_DuplicateName_409(t *testing.T) {
	h, pool := newPushProviderHandler(t)
	pool.entries["vault"] = &pushprovider.PushProvider{Name: "vault", CreatedByAID: "archon-alice"}

	_, err := h.CreateTyped(context.Background(), claimsFor("archon-alice"),
		PushProviderCreateInput{Name: "vault"})
	if got := ppProblemType(t, err); got != problem.TypePushProviderExists {
		t.Errorf("problem.Type = %q, want %q (409 duplicate)", got, problem.TypePushProviderExists)
	}
}

func TestPushProviderHandler_GetTyped_Success(t *testing.T) {
	h, pool := newPushProviderHandler(t)
	now := time.Now()
	pool.entries["vault"] = &pushprovider.PushProvider{
		Name:         "vault",
		Params:       map[string]any{"role": "keeper"},
		CreatedAt:    now,
		UpdatedAt:    now,
		CreatedByAID: "archon-alice",
	}

	view, err := h.GetTyped(context.Background(), "vault")
	if err != nil {
		t.Fatalf("GetTyped: %v", err)
	}
	if view.Name != "vault" {
		t.Errorf("name = %v", view.Name)
	}
	if view.Params["role"] != "keeper" {
		t.Errorf("params: %v", view.Params)
	}
}

func TestPushProviderHandler_GetTyped_NotFound_404(t *testing.T) {
	h, _ := newPushProviderHandler(t)
	_, err := h.GetTyped(context.Background(), "missing")
	if got := ppProblemType(t, err); got != problem.TypeNotFound {
		t.Errorf("problem.Type = %q, want %q", got, problem.TypeNotFound)
	}
}

func TestPushProviderHandler_UpdateTyped_Success(t *testing.T) {
	h, pool := newPushProviderHandler(t)
	now := time.Now()
	pool.entries["vault"] = &pushprovider.PushProvider{
		Name:         "vault",
		Params:       map[string]any{"role": "old"},
		CreatedAt:    now,
		UpdatedAt:    now,
		CreatedByAID: "archon-alice",
	}

	_, err := h.UpdateTyped(context.Background(), claimsFor("archon-bob"), "vault",
		PushProviderUpdateInput{Params: map[string]any{"role": "new", "vault_addr": "https://new.example.com"}})
	if err != nil {
		t.Fatalf("UpdateTyped: %v", err)
	}
	updated := pool.entries["vault"]
	if updated.Params["role"] != "new" {
		t.Errorf("params not updated: %v", updated.Params)
	}
	if updated.UpdatedByAID == nil || *updated.UpdatedByAID != "archon-bob" {
		t.Errorf("UpdatedByAID = %v", updated.UpdatedByAID)
	}
}

func TestPushProviderHandler_UpdateTyped_NotFound_404(t *testing.T) {
	h, _ := newPushProviderHandler(t)
	_, err := h.UpdateTyped(context.Background(), claimsFor("archon-bob"), "missing",
		PushProviderUpdateInput{Params: map[string]any{}})
	if got := ppProblemType(t, err); got != problem.TypeNotFound {
		t.Errorf("problem.Type = %q, want %q", got, problem.TypeNotFound)
	}
}

func TestPushProviderHandler_UpdateTyped_RejectsPlainSensitive_422(t *testing.T) {
	h, pool := newPushProviderHandler(t)
	pool.entries["vault"] = &pushprovider.PushProvider{Name: "vault", CreatedByAID: "archon-alice"}

	_, err := h.UpdateTyped(context.Background(), claimsFor("archon-bob"), "vault",
		PushProviderUpdateInput{Params: map[string]any{"token": "plain"}})
	if got := ppProblemType(t, err); got != problem.TypeValidationFailed {
		t.Errorf("problem.Type = %q, want %q", got, problem.TypeValidationFailed)
	}
}

func TestPushProviderHandler_DeleteTyped_Success(t *testing.T) {
	h, pool := newPushProviderHandler(t)
	pool.entries["vault"] = &pushprovider.PushProvider{Name: "vault", CreatedByAID: "archon-alice"}

	reply, err := h.DeleteTyped(context.Background(), "vault")
	if err != nil {
		t.Fatalf("DeleteTyped: %v", err)
	}
	if reply.Name != "vault" {
		t.Errorf("reply.Name = %q, want vault", reply.Name)
	}
	if _, exists := pool.entries["vault"]; exists {
		t.Error("entry not deleted")
	}
}

func TestPushProviderHandler_DeleteTyped_NotFound_404(t *testing.T) {
	h, _ := newPushProviderHandler(t)
	_, err := h.DeleteTyped(context.Background(), "missing")
	if got := ppProblemType(t, err); got != problem.TypeNotFound {
		t.Errorf("problem.Type = %q, want %q", got, problem.TypeNotFound)
	}
}

func TestPushProviderHandler_ListTyped_Success(t *testing.T) {
	h, pool := newPushProviderHandler(t)
	now := time.Now()
	pool.entries["vault"] = &pushprovider.PushProvider{Name: "vault", CreatedAt: now, UpdatedAt: now, CreatedByAID: "archon-alice"}
	pool.entries["static"] = &pushprovider.PushProvider{Name: "static", CreatedAt: now, UpdatedAt: now, CreatedByAID: "archon-alice"}

	page, err := h.ListTyped(context.Background(), "", 0, 10)
	if err != nil {
		t.Fatalf("ListTyped: %v", err)
	}
	if page.Total != 2 {
		t.Errorf("total = %d, want 2", page.Total)
	}
	if len(page.Items) != 2 {
		t.Errorf("items len = %d", len(page.Items))
	}
}
