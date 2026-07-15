package handlers

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	keeperjwt "github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/provider"
)

// fakeProviderPool — an in-memory PG simulation for provider CRUD: INSERT/SELECT/
// DELETE/COUNT/LIST. Single-thread (unit test). hasProfiles → delete returns an
// FK violation (simulates profiles_provider_fk RESTRICT).
type fakeProviderPool struct {
	entries     map[string]*provider.Provider
	hasProfiles map[string]bool
}

func newFakeProviderPool() *fakeProviderPool {
	return &fakeProviderPool{entries: map[string]*provider.Provider{}, hasProfiles: map[string]bool{}}
}

func (f *fakeProviderPool) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if strings.Contains(sql, "DELETE FROM providers") {
		name := args[0].(string)
		if f.hasProfiles[name] {
			return pgconn.NewCommandTag("DELETE 0"),
				&pgconn.PgError{Code: "23503", ConstraintName: "profiles_provider_fk"}
		}
		if _, ok := f.entries[name]; !ok {
			return pgconn.NewCommandTag("DELETE 0"), nil
		}
		delete(f.entries, name)
		return pgconn.NewCommandTag("DELETE 1"), nil
	}
	return pgconn.NewCommandTag(""), nil
}

func (f *fakeProviderPool) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	switch {
	case strings.Contains(sql, "INSERT INTO providers"):
		name := args[0].(string)
		if _, exists := f.entries[name]; exists {
			return errRowProv{err: &pgconn.PgError{Code: "23505", ConstraintName: "providers_pkey"}}
		}
		now := time.Now()
		var createdBy *string
		if args[4] != nil {
			s := args[4].(string)
			createdBy = &s
		}
		var fqdnSuffix *string
		if len(args) > 5 && args[5] != nil {
			s := args[5].(string)
			fqdnSuffix = &s
		}
		f.entries[name] = &provider.Provider{
			Name:           name,
			Type:           args[1].(string),
			Region:         args[2].(string),
			CredentialsRef: args[3].(string),
			FQDNSuffix:     fqdnSuffix,
			CreatedByAID:   createdBy,
			CreatedAt:      now,
		}
		return scanRowProv{values: []any{now}}
	case strings.Contains(sql, "COUNT(*) FROM providers"):
		return scanRowProv{values: []any{len(f.entries)}}
	case strings.Contains(sql, "FROM providers") && strings.Contains(sql, "WHERE name = $1"):
		name := args[0].(string)
		p, ok := f.entries[name]
		if !ok {
			return errRowProv{err: pgx.ErrNoRows}
		}
		return scanRowProv{values: []any{p.Name, p.Type, p.Region, p.CredentialsRef, p.CreatedByAID, p.CreatedAt, p.FQDNSuffix}}
	}
	return errRowProv{err: pgx.ErrNoRows}
}

func (f *fakeProviderPool) Query(_ context.Context, _ string, _ ...any) (pgx.Rows, error) {
	rows := &fakeProvRows{}
	for _, p := range f.entries {
		rows.data = append(rows.data, []any{p.Name, p.Type, p.Region, p.CredentialsRef, p.CreatedByAID, p.CreatedAt, p.FQDNSuffix})
	}
	return rows, nil
}

type errRowProv struct{ err error }

func (r errRowProv) Scan(_ ...any) error { return r.err }

type scanRowProv struct{ values []any }

func (r scanRowProv) Scan(dst ...any) error { return assignScan(dst, r.values) }

type fakeProvRows struct {
	data [][]any
	idx  int
}

func (r *fakeProvRows) Next() bool                                   { r.idx++; return r.idx <= len(r.data) }
func (r *fakeProvRows) Scan(dst ...any) error                        { return assignScan(dst, r.data[r.idx-1]) }
func (r *fakeProvRows) Close()                                       {}
func (r *fakeProvRows) Err() error                                   { return nil }
func (r *fakeProvRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *fakeProvRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *fakeProvRows) Values() ([]any, error)                       { return nil, nil }
func (r *fakeProvRows) RawValues() [][]byte                          { return nil }
func (r *fakeProvRows) Conn() *pgx.Conn                              { return nil }

// assignScan copies values from src into dst pointers (a minimal scanner for the
// specific provider/profile CRUD types).
func assignScan(dst, src []any) error {
	for i := range dst {
		switch d := dst[i].(type) {
		case *string:
			*d = src[i].(string)
		case **string:
			if src[i] == nil {
				*d = nil
			} else if sp, ok := src[i].(*string); ok {
				*d = sp
			} else {
				s := src[i].(string)
				*d = &s
			}
		case *time.Time:
			*d = src[i].(time.Time)
		case *int:
			*d = src[i].(int)
		case *[]byte:
			if src[i] == nil {
				*d = nil
			} else {
				*d = src[i].([]byte)
			}
		}
	}
	return nil
}

func newProviderHandler(t *testing.T, pool provider.ExecQueryRower) *ProviderHandler {
	t.Helper()
	svc, err := provider.NewService(provider.ServiceDeps{Pool: pool.(*fakeProviderPool)})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return NewProviderHandler(svc, nil)
}

func cloudClaims() *keeperjwt.Claims { return &keeperjwt.Claims{Subject: "archon-alice"} }

func provProblemType(t *testing.T, err error) string {
	t.Helper()
	if err == nil {
		return ""
	}
	d, ok := AsProblemDetails(err)
	if !ok {
		t.Fatalf("error не *problemError: %v", err)
	}
	return d.Type
}

func TestProviderHandler_CreateGetListDelete(t *testing.T) {
	h := newProviderHandler(t, newFakeProviderPool())
	ctx := context.Background()

	reply, err := h.CreateTyped(ctx, cloudClaims(), ProviderCreateInput{
		Name: "wb-cloud", Type: "wb", Region: "ru-1", CredentialsRef: "vault:secret/cloud/wb",
	})
	if err != nil {
		t.Fatalf("CreateTyped: %v", err)
	}
	if reply.Body.Name != "wb-cloud" || reply.Body.CredentialsRef != "vault:secret/cloud/wb" {
		t.Fatalf("create body = %+v", reply.Body)
	}
	// The audit payload carries credentials_ref as a PATH (not the secret).
	if reply.AuditPayload()["credentials_ref"] != "vault:secret/cloud/wb" {
		t.Errorf("audit credentials_ref = %v", reply.AuditPayload()["credentials_ref"])
	}

	got, err := h.GetTyped(ctx, "wb-cloud")
	if err != nil {
		t.Fatalf("GetTyped: %v", err)
	}
	if got.Type != "wb" || got.Region != "ru-1" {
		t.Fatalf("get = %+v", got)
	}

	page, err := h.ListTyped(ctx, 0, 50)
	if err != nil {
		t.Fatalf("ListTyped: %v", err)
	}
	if page.Total != 1 || len(page.Items) != 1 {
		t.Fatalf("list total=%d items=%d", page.Total, len(page.Items))
	}

	del, err := h.DeleteTyped(ctx, "wb-cloud")
	if err != nil {
		t.Fatalf("DeleteTyped: %v", err)
	}
	if del.Name != "wb-cloud" {
		t.Fatalf("delete name = %q", del.Name)
	}

	if _, err := h.GetTyped(ctx, "wb-cloud"); provProblemType(t, err) != problem.TypeNotFound {
		t.Fatalf("get after delete: %q, want not-found", provProblemType(t, err))
	}
}

func TestProviderHandler_DuplicateConflict(t *testing.T) {
	h := newProviderHandler(t, newFakeProviderPool())
	ctx := context.Background()
	in := ProviderCreateInput{Name: "dup", Type: "wb", Region: "ru-1", CredentialsRef: "vault:secret/x"}
	if _, err := h.CreateTyped(ctx, cloudClaims(), in); err != nil {
		t.Fatalf("first create: %v", err)
	}
	_, err := h.CreateTyped(ctx, cloudClaims(), in)
	if got := provProblemType(t, err); got != problem.TypeProviderExists {
		t.Fatalf("dup create: %q, want %q", got, problem.TypeProviderExists)
	}
}

func TestProviderHandler_Validation(t *testing.T) {
	h := newProviderHandler(t, newFakeProviderPool())
	ctx := context.Background()
	cases := []struct {
		name string
		in   ProviderCreateInput
	}{
		{"empty-name", ProviderCreateInput{Type: "wb", Region: "ru", CredentialsRef: "vault:x"}},
		{"bad-name", ProviderCreateInput{Name: "WB_Cloud", Type: "wb", Region: "ru", CredentialsRef: "vault:x"}},
		{"empty-region", ProviderCreateInput{Name: "wb", Type: "wb", CredentialsRef: "vault:x"}},
		{"plain-creds", ProviderCreateInput{Name: "wb", Type: "wb", Region: "ru", CredentialsRef: "secret/raw"}},
		{"empty-creds-path", ProviderCreateInput{Name: "wb", Type: "wb", Region: "ru", CredentialsRef: "vault:"}},
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

func TestProviderHandler_DeleteNotFound(t *testing.T) {
	h := newProviderHandler(t, newFakeProviderPool())
	_, err := h.DeleteTyped(context.Background(), "ghost")
	if got := provProblemType(t, err); got != problem.TypeNotFound {
		t.Fatalf("delete ghost: %q, want not-found", got)
	}
}

func TestProviderHandler_DeleteBlockedByProfiles(t *testing.T) {
	pool := newFakeProviderPool()
	h := newProviderHandler(t, pool)
	ctx := context.Background()
	if _, err := h.CreateTyped(ctx, cloudClaims(), ProviderCreateInput{
		Name: "wb", Type: "wb", Region: "ru", CredentialsRef: "vault:x",
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	pool.hasProfiles["wb"] = true
	_, err := h.DeleteTyped(ctx, "wb")
	if got := provProblemType(t, err); got != problem.TypeProviderHasProfiles {
		t.Fatalf("delete blocked: %q, want %q", got, problem.TypeProviderHasProfiles)
	}
}
