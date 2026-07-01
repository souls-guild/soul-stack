package mcp

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/souls-guild/soul-stack/keeper/internal/operator"
	"github.com/souls-guild/soul-stack/keeper/internal/profile"
	"github.com/souls-guild/soul-stack/keeper/internal/provider"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac/rbactest"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// cloudFakePool — in-memory PG-имитация под provider+profile CRUD для MCP-tools.
// Транспортный фокус (тот же приём, что svcRegFakePool): валидирует, что tool
// зовёт Service, маппит ошибки, кодирует output, пишет audit.
type cloudFakePool struct {
	providers map[string]*provider.Provider
	profiles  map[string]*profile.Profile
}

func newCloudFakePool() *cloudFakePool {
	return &cloudFakePool{
		providers: map[string]*provider.Provider{},
		profiles:  map[string]*profile.Profile{},
	}
}

func (p *cloudFakePool) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	switch {
	case strings.Contains(sql, "DELETE FROM providers"):
		name := args[0].(string)
		if _, ok := p.providers[name]; !ok {
			return pgconn.NewCommandTag("DELETE 0"), nil
		}
		delete(p.providers, name)
		return pgconn.NewCommandTag("DELETE 1"), nil
	case strings.Contains(sql, "DELETE FROM profiles"):
		name := args[0].(string)
		if _, ok := p.profiles[name]; !ok {
			return pgconn.NewCommandTag("DELETE 0"), nil
		}
		delete(p.profiles, name)
		return pgconn.NewCommandTag("DELETE 1"), nil
	}
	return pgconn.CommandTag{}, nil
}

func (p *cloudFakePool) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	switch {
	case strings.Contains(sql, "INSERT INTO providers"):
		name := args[0].(string)
		if _, ok := p.providers[name]; ok {
			return cloudErrRow{&pgconn.PgError{Code: "23505", ConstraintName: "providers_pkey"}}
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
		p.providers[name] = &provider.Provider{
			Name: name, Type: args[1].(string), Region: args[2].(string),
			CredentialsRef: args[3].(string), FQDNSuffix: fqdnSuffix, CreatedByAID: createdBy, CreatedAt: now,
		}
		return cloudRow{[]any{now}}
	case strings.Contains(sql, "INSERT INTO profiles"):
		name := args[0].(string)
		prov := args[1].(string)
		if _, ok := p.profiles[name]; ok {
			return cloudErrRow{&pgconn.PgError{Code: "23505", ConstraintName: "profiles_pkey"}}
		}
		if _, ok := p.providers[prov]; !ok {
			return cloudErrRow{&pgconn.PgError{Code: "23503", ConstraintName: "profiles_provider_fk"}}
		}
		now := time.Now()
		var params map[string]any
		_ = json.Unmarshal(args[2].([]byte), &params)
		p.profiles[name] = &profile.Profile{Name: name, Provider: prov, Params: params, CreatedAt: now}
		return cloudRow{[]any{now}}
	case strings.Contains(sql, "COUNT(*) FROM providers"):
		return cloudRow{[]any{len(p.providers)}}
	case strings.Contains(sql, "COUNT(*) FROM profiles"):
		return cloudRow{[]any{len(p.profiles)}}
	case strings.Contains(sql, "FROM providers") && strings.Contains(sql, "WHERE name = $1"):
		pr, ok := p.providers[args[0].(string)]
		if !ok {
			return cloudErrRow{pgx.ErrNoRows}
		}
		return cloudRow{[]any{pr.Name, pr.Type, pr.Region, pr.CredentialsRef, pr.CreatedByAID, pr.CreatedAt, pr.FQDNSuffix}}
	case strings.Contains(sql, "FROM profiles") && strings.Contains(sql, "WHERE name = $1"):
		pr, ok := p.profiles[args[0].(string)]
		if !ok {
			return cloudErrRow{pgx.ErrNoRows}
		}
		return cloudRow{profileRowValues(pr)}
	}
	return cloudErrRow{pgx.ErrNoRows}
}

func (p *cloudFakePool) Query(_ context.Context, sql string, _ ...any) (pgx.Rows, error) {
	rows := &cloudRows{}
	if strings.Contains(sql, "FROM providers") {
		for _, pr := range p.providers {
			rows.data = append(rows.data, []any{pr.Name, pr.Type, pr.Region, pr.CredentialsRef, pr.CreatedByAID, pr.CreatedAt, pr.FQDNSuffix})
		}
		return rows, nil
	}
	for _, pr := range p.profiles {
		rows.data = append(rows.data, profileRowValues(pr))
	}
	return rows, nil
}

func profileRowValues(pr *profile.Profile) []any {
	var b []byte
	if pr.Params != nil {
		b, _ = json.Marshal(pr.Params)
	}
	return []any{pr.Name, pr.Provider, b, pr.CloudInit, pr.CreatedByAID, pr.CreatedAt}
}

type cloudErrRow struct{ err error }

func (r cloudErrRow) Scan(_ ...any) error { return r.err }

type cloudRow struct{ values []any }

func (r cloudRow) Scan(dst ...any) error { return cloudScan(dst, r.values) }

type cloudRows struct {
	data [][]any
	idx  int
}

func (r *cloudRows) Next() bool                                   { r.idx++; return r.idx <= len(r.data) }
func (r *cloudRows) Scan(dst ...any) error                        { return cloudScan(dst, r.data[r.idx-1]) }
func (r *cloudRows) Close()                                       {}
func (r *cloudRows) Err() error                                   { return nil }
func (r *cloudRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *cloudRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *cloudRows) Values() ([]any, error)                       { return nil, nil }
func (r *cloudRows) RawValues() [][]byte                          { return nil }
func (r *cloudRows) Conn() *pgx.Conn                              { return nil }

func cloudScan(dst, src []any) error {
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

// --- harness ---

func newCloudToolHandler(t *testing.T, rbacCfg *rbactest.Config, pool *cloudFakePool) (*Handler, *recordingAudit) {
	t.Helper()
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	enf, err := rbactest.NewEnforcer(rbacCfg)
	if err != nil {
		t.Fatalf("NewEnforcer: %v", err)
	}
	opSvc, err := operator.NewService(operator.ServiceDeps{
		Pool: &fakePool{}, Issuer: &fakeIssuer{}, RBAC: enf, TTLDefault: time.Hour, Logger: logger,
	})
	if err != nil {
		t.Fatalf("operator.NewService: %v", err)
	}
	rec := &recordingAudit{}
	deps := HandlerDeps{
		OperatorSvc: opSvc, RBAC: enf, AuditWriter: rec, Logger: logger, IncarnationDB: &fakePool{},
	}
	if pool != nil {
		provSvc, err := provider.NewService(pool)
		if err != nil {
			t.Fatalf("provider.NewService: %v", err)
		}
		profSvc, err := profile.NewService(pool)
		if err != nil {
			t.Fatalf("profile.NewService: %v", err)
		}
		deps.ProviderSvc = provSvc
		deps.ProfileSvc = profSvc
	}
	h, err := NewHandler(deps)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	return h, rec
}

func cloudAdminCfg() *rbactest.Config {
	return &rbactest.Config{
		Roles: []rbactest.Role{
			{Name: "cloud-admin", Operators: []string{"archon-alice"}, Permissions: []string{
				"provider.create", "provider.read", "provider.delete",
				"profile.create", "profile.read", "profile.delete",
			}},
		},
	}
}

// --- manifest ---

func TestCloudTools_InManifest(t *testing.T) {
	want := []string{
		"keeper.provider.create", "keeper.provider.read", "keeper.provider.list", "keeper.provider.delete",
		"keeper.profile.create", "keeper.profile.read", "keeper.profile.list", "keeper.profile.delete",
	}
	for _, name := range want {
		e, ok := toolByName(name)
		if !ok {
			t.Errorf("%s missing from catalogManifest", name)
			continue
		}
		if e.status != toolStatusImplemented {
			t.Errorf("%s status = %d, want Implemented", name, e.status)
		}
	}
}

// --- nil-guard ---

func TestCloudTools_NilGuard(t *testing.T) {
	h, _ := newCloudToolHandler(t, cloudAdminCfg(), nil) // Svc == nil
	cases := []struct{ tool, args string }{
		{"keeper.provider.create", `{"name":"wb","type":"wb","region":"ru","credentials_ref":"vault:x"}`},
		{"keeper.provider.read", `{"name":"wb"}`},
		{"keeper.provider.list", `{}`},
		{"keeper.provider.delete", `{"name":"wb"}`},
		{"keeper.profile.create", `{"name":"p","provider":"wb"}`},
		{"keeper.profile.read", `{"name":"p"}`},
		{"keeper.profile.list", `{}`},
		{"keeper.profile.delete", `{"name":"p"}`},
	}
	for _, tc := range cases {
		t.Run(tc.tool, func(t *testing.T) {
			resp := callTool(t, h, "archon-alice", tc.tool, tc.args)
			if resp.Error == nil {
				t.Fatal("expected error")
			}
			if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeInternalError {
				t.Errorf("code = %q, want internal-error", data.Code)
			}
		})
	}
}

// --- RBAC ---

func TestCloudTools_RBACForbidden(t *testing.T) {
	h, _ := newCloudToolHandler(t, nil, newCloudFakePool()) // нет permissions → deny
	resp := callTool(t, h, "archon-alice", "keeper.provider.create",
		`{"name":"wb","type":"wb","region":"ru","credentials_ref":"vault:x"}`)
	if resp.Error == nil {
		t.Fatal("expected forbidden")
	}
	if data := mustToolErrorData(t, resp.Error.Data); data.Code != mcpCodeForbidden {
		t.Errorf("code = %q, want forbidden", data.Code)
	}
}

// --- provider success lifecycle + audit ---

func TestProviderTool_CreateReadListDelete(t *testing.T) {
	h, rec := newCloudToolHandler(t, cloudAdminCfg(), newCloudFakePool())

	resp := callTool(t, h, "archon-alice", "keeper.provider.create",
		`{"name":"wb","type":"wb","region":"ru","credentials_ref":"vault:secret/cloud/wb"}`)
	if resp.Error != nil {
		t.Fatalf("create error: %+v", resp.Error)
	}
	var res toolsCallResult
	_ = json.Unmarshal(resp.Result, &res)
	var out providerViewOut
	_ = json.Unmarshal(res.StructuredContent, &out)
	if out.Name != "wb" || out.CredentialsRef != "vault:secret/cloud/wb" {
		t.Fatalf("output = %+v", out)
	}
	// Audit credentials_ref как ПУТЬ (не секрет).
	if len(rec.events) != 1 || rec.events[0].EventType != audit.EventProviderCreated {
		t.Fatalf("audit = %+v", rec.events)
	}
	assertPayload(t, rec.events[0].Payload, "credentials_ref", "vault:secret/cloud/wb")

	if r := callTool(t, h, "archon-alice", "keeper.provider.read", `{"name":"wb"}`); r.Error != nil {
		t.Fatalf("read error: %+v", r.Error)
	}
	if r := callTool(t, h, "archon-alice", "keeper.provider.list", `{}`); r.Error != nil {
		t.Fatalf("list error: %+v", r.Error)
	}
	if r := callTool(t, h, "archon-alice", "keeper.provider.delete", `{"name":"wb"}`); r.Error != nil {
		t.Fatalf("delete error: %+v", r.Error)
	}
	// read после delete → not-found.
	r := callTool(t, h, "archon-alice", "keeper.provider.read", `{"name":"wb"}`)
	if data := mustToolErrorData(t, r.Error.Data); data.Code != mcpCodeNotFound {
		t.Errorf("read after delete code = %q, want not-found", data.Code)
	}
}

func TestProviderTool_Duplicate409(t *testing.T) {
	pool := newCloudFakePool()
	h, _ := newCloudToolHandler(t, cloudAdminCfg(), pool)
	args := `{"name":"wb","type":"wb","region":"ru","credentials_ref":"vault:x"}`
	if r := callTool(t, h, "archon-alice", "keeper.provider.create", args); r.Error != nil {
		t.Fatalf("first create: %+v", r.Error)
	}
	r := callTool(t, h, "archon-alice", "keeper.provider.create", args)
	if data := mustToolErrorData(t, r.Error.Data); data.Code != mcpCodeProviderExists {
		t.Errorf("dup code = %q, want provider-already-exists", data.Code)
	}
}

func TestProviderTool_Validation(t *testing.T) {
	h, _ := newCloudToolHandler(t, cloudAdminCfg(), newCloudFakePool())
	cases := []struct{ name, args, want string }{
		{"no-name", `{"type":"wb","region":"ru","credentials_ref":"vault:x"}`, mcpCodeValidationFailed},
		{"plain-creds", `{"name":"wb","type":"wb","region":"ru","credentials_ref":"raw"}`, mcpCodeValidationFailed},
		{"unknown-field", `{"name":"wb","type":"wb","region":"ru","credentials_ref":"vault:x","z":1}`, mcpCodeMalformedRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := callTool(t, h, "archon-alice", "keeper.provider.create", tc.args)
			if data := mustToolErrorData(t, r.Error.Data); data.Code != tc.want {
				t.Errorf("code = %q, want %q", data.Code, tc.want)
			}
		})
	}
}

// --- profile success + FK 422 ---

func TestProfileTool_CreateAndMissingProvider(t *testing.T) {
	pool := newCloudFakePool()
	h, rec := newCloudToolHandler(t, cloudAdminCfg(), pool)

	// нет Provider → profile.create отдаёт validation-failed (FK).
	r := callTool(t, h, "archon-alice", "keeper.profile.create", `{"name":"p","provider":"ghost"}`)
	if data := mustToolErrorData(t, r.Error.Data); data.Code != mcpCodeValidationFailed {
		t.Fatalf("missing provider code = %q, want validation-failed", data.Code)
	}

	// заводим Provider, потом Profile.
	if r := callTool(t, h, "archon-alice", "keeper.provider.create",
		`{"name":"wb","type":"wb","region":"ru","credentials_ref":"vault:x"}`); r.Error != nil {
		t.Fatalf("provider create: %+v", r.Error)
	}
	rec.events = nil
	if r := callTool(t, h, "archon-alice", "keeper.profile.create",
		`{"name":"web","provider":"wb","params":{"image":"ubuntu"}}`); r.Error != nil {
		t.Fatalf("profile create: %+v", r.Error)
	}
	// Audit profile.created с params_keys (без values).
	if len(rec.events) != 1 || rec.events[0].EventType != audit.EventProfileCreated {
		t.Fatalf("audit = %+v", rec.events)
	}
}

func TestProfileTool_Duplicate409(t *testing.T) {
	pool := newCloudFakePool()
	h, _ := newCloudToolHandler(t, cloudAdminCfg(), pool)
	if r := callTool(t, h, "archon-alice", "keeper.provider.create",
		`{"name":"wb","type":"wb","region":"ru","credentials_ref":"vault:x"}`); r.Error != nil {
		t.Fatalf("provider create: %+v", r.Error)
	}
	args := `{"name":"web","provider":"wb"}`
	if r := callTool(t, h, "archon-alice", "keeper.profile.create", args); r.Error != nil {
		t.Fatalf("first profile: %+v", r.Error)
	}
	r := callTool(t, h, "archon-alice", "keeper.profile.create", args)
	if data := mustToolErrorData(t, r.Error.Data); data.Code != mcpCodeProfileExists {
		t.Errorf("dup code = %q, want profile-already-exists", data.Code)
	}
}
