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
	"github.com/souls-guild/soul-stack/keeper/internal/profile"
)

// fakeProfilePool — an in-memory PG simulation for profile CRUD. providers is a set
// of existing Provider names (simulates FK profiles_provider_fk): INSERT with a
// nonexistent provider → FK violation.
type fakeProfilePool struct {
	entries   map[string]*profile.Profile
	providers map[string]bool
}

func newFakeProfilePool(providers ...string) *fakeProfilePool {
	f := &fakeProfilePool{entries: map[string]*profile.Profile{}, providers: map[string]bool{}}
	for _, p := range providers {
		f.providers[p] = true
	}
	return f
}

func (f *fakeProfilePool) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if strings.Contains(sql, "DELETE FROM profiles") {
		name := args[0].(string)
		if _, ok := f.entries[name]; !ok {
			return pgconn.NewCommandTag("DELETE 0"), nil
		}
		delete(f.entries, name)
		return pgconn.NewCommandTag("DELETE 1"), nil
	}
	return pgconn.NewCommandTag(""), nil
}

func (f *fakeProfilePool) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	switch {
	case strings.Contains(sql, "INSERT INTO profiles"):
		name := args[0].(string)
		providerName := args[1].(string)
		if _, exists := f.entries[name]; exists {
			return errRowProv{err: &pgconn.PgError{Code: "23505", ConstraintName: "profiles_pkey"}}
		}
		if !f.providers[providerName] {
			return errRowProv{err: &pgconn.PgError{Code: "23503", ConstraintName: "profiles_provider_fk"}}
		}
		now := time.Now()
		var cloudInit *string
		if args[3] != nil {
			s := args[3].(string)
			cloudInit = &s
		}
		var createdBy *string
		if args[4] != nil {
			s := args[4].(string)
			createdBy = &s
		}
		var params map[string]any
		_ = json.Unmarshal(args[2].([]byte), &params)
		f.entries[name] = &profile.Profile{
			Name: name, Provider: providerName, Params: params,
			CloudInit: cloudInit, CreatedByAID: createdBy, CreatedAt: now,
		}
		return scanRowProv{values: []any{now}}
	case strings.Contains(sql, "COUNT(*) FROM profiles"):
		return scanRowProv{values: []any{len(f.entries)}}
	case strings.Contains(sql, "FROM profiles") && strings.Contains(sql, "WHERE name = $1"):
		name := args[0].(string)
		p, ok := f.entries[name]
		if !ok {
			return errRowProv{err: pgx.ErrNoRows}
		}
		return scanRowProv{values: profileScanValues(p)}
	}
	return errRowProv{err: pgx.ErrNoRows}
}

func (f *fakeProfilePool) Query(_ context.Context, _ string, _ ...any) (pgx.Rows, error) {
	rows := &fakeProvRows{}
	for _, p := range f.entries {
		rows.data = append(rows.data, profileScanValues(p))
	}
	return rows, nil
}

func profileScanValues(p *profile.Profile) []any {
	var paramsBytes []byte
	if p.Params != nil {
		paramsBytes, _ = json.Marshal(p.Params)
	}
	return []any{p.Name, p.Provider, paramsBytes, p.CloudInit, p.CreatedByAID, p.CreatedAt}
}

func newProfileHandler(t *testing.T, pool *fakeProfilePool) *ProfileHandler {
	t.Helper()
	svc, err := profile.NewService(pool)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return NewProfileHandler(svc, nil)
}

func TestProfileHandler_CreateGetListDelete(t *testing.T) {
	h := newProfileHandler(t, newFakeProfilePool("wb-cloud"))
	ctx := context.Background()
	params := map[string]any{"image": "ubuntu-22", "ram_mb": float64(2048)}

	reply, err := h.CreateTyped(ctx, cloudClaims(), ProfileCreateInput{
		Name: "web-small", Provider: "wb-cloud", Params: &params,
	})
	if err != nil {
		t.Fatalf("CreateTyped: %v", err)
	}
	if reply.Body.Name != "web-small" || reply.Body.Provider != "wb-cloud" {
		t.Fatalf("create body = %+v", reply.Body)
	}
	// Audit carries params_keys (no values — secret hygiene).
	keys, _ := reply.AuditPayload()["params_keys"].([]string)
	if len(keys) != 2 {
		t.Errorf("params_keys = %v, want 2 keys", keys)
	}

	got, err := h.GetTyped(ctx, "web-small")
	if err != nil {
		t.Fatalf("GetTyped: %v", err)
	}
	if got.Params["image"] != "ubuntu-22" {
		t.Fatalf("get params = %+v", got.Params)
	}

	page, err := h.ListTyped(ctx, "", 0, 50)
	if err != nil {
		t.Fatalf("ListTyped: %v", err)
	}
	if page.Total != 1 {
		t.Fatalf("list total = %d", page.Total)
	}

	if _, err := h.DeleteTyped(ctx, "web-small"); err != nil {
		t.Fatalf("DeleteTyped: %v", err)
	}
	if _, err := h.GetTyped(ctx, "web-small"); provProblemType(t, err) != problem.TypeNotFound {
		t.Fatalf("get after delete: %q, want not-found", provProblemType(t, err))
	}
}

func TestProfileHandler_DuplicateConflict(t *testing.T) {
	h := newProfileHandler(t, newFakeProfilePool("wb-cloud"))
	ctx := context.Background()
	in := ProfileCreateInput{Name: "dup", Provider: "wb-cloud"}
	if _, err := h.CreateTyped(ctx, cloudClaims(), in); err != nil {
		t.Fatalf("first create: %v", err)
	}
	_, err := h.CreateTyped(ctx, cloudClaims(), in)
	if got := provProblemType(t, err); got != problem.TypeProfileExists {
		t.Fatalf("dup create: %q, want %q", got, problem.TypeProfileExists)
	}
}

func TestProfileHandler_MissingProvider422(t *testing.T) {
	h := newProfileHandler(t, newFakeProfilePool()) // no Provider at all
	_, err := h.CreateTyped(context.Background(), cloudClaims(), ProfileCreateInput{
		Name: "orphan", Provider: "ghost",
	})
	if got := provProblemType(t, err); got != problem.TypeValidationFailed {
		t.Fatalf("missing provider: %q, want validation-failed (422)", got)
	}
}

func TestProfileHandler_Validation(t *testing.T) {
	h := newProfileHandler(t, newFakeProfilePool("wb-cloud"))
	ctx := context.Background()
	cases := []struct {
		name string
		in   ProfileCreateInput
	}{
		{"empty-name", ProfileCreateInput{Provider: "wb-cloud"}},
		{"bad-name", ProfileCreateInput{Name: "Web_Small", Provider: "wb-cloud"}},
		{"empty-provider", ProfileCreateInput{Name: "web"}},
		{"bad-provider", ProfileCreateInput{Name: "web", Provider: "WB"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := h.CreateTyped(ctx, cloudClaims(), c.in)
			if got := provProblemType(t, err); got != problem.TypeValidationFailed {
				t.Fatalf("%s: %q, want validation-failed", c.name, got)
			}
		})
	}
}

func TestProfileHandler_ListByProvider(t *testing.T) {
	h := newProfileHandler(t, newFakeProfilePool("wb-cloud"))
	ctx := context.Background()
	if _, err := h.CreateTyped(ctx, cloudClaims(), ProfileCreateInput{Name: "a", Provider: "wb-cloud"}); err != nil {
		t.Fatalf("create a: %v", err)
	}
	page, err := h.ListTyped(ctx, "wb-cloud", 0, 50)
	if err != nil {
		t.Fatalf("ListTyped(provider): %v", err)
	}
	if page.Total != 1 {
		t.Fatalf("filtered total = %d", page.Total)
	}
}
