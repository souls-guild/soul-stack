//go:build integration

// Integration test of the Operator API HTTP server: PG + Vault via
// testcontainers-go, a real listener on an ephemeral port.
//
// Run:
//
//	make test-integration
//	# or
//	cd keeper && SOUL_STACK_INTEGRATION_REQUIRE_DOCKER=1 \
//	    go test -tags=integration -race -count=1 ./internal/api/...
//
// One PG + one Vault per-package in TestMain.

package api

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	jwtv5 "github.com/golang-jwt/jwt/v5"
	vaultapi "github.com/hashicorp/vault/api"
	"github.com/jackc/pgx/v5/pgxpool"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	tcvault "github.com/testcontainers/testcontainers-go/modules/vault"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
	"github.com/souls-guild/soul-stack/keeper/internal/api/health"
	"github.com/souls-guild/soul-stack/keeper/internal/api/problem"
	"github.com/souls-guild/soul-stack/keeper/internal/auditpg"
	"github.com/souls-guild/soul-stack/keeper/internal/bootstraptoken"
	"github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	keeperjwt "github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/migrate"
	"github.com/souls-guild/soul-stack/keeper/internal/operator"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac/rbactest"
	"github.com/souls-guild/soul-stack/keeper/internal/serviceregistry"
	"github.com/souls-guild/soul-stack/keeper/internal/soul"
	keepervault "github.com/souls-guild/soul-stack/keeper/internal/vault"
	"github.com/souls-guild/soul-stack/keeper/migrations"
	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/obs"
)

const (
	integrationVaultToken = "root"
	integrationVaultImage = "hashicorp/vault:1.18"
	integrationPGImage    = "postgres:16-alpine"

	integrationSigningKey = "0123456789abcdef0123456789abcdef" // 32 bytes
	integrationIssuer     = "keeper.api.integration"
)

var (
	integrationPool *pgxpool.Pool
	integrationVC   *keepervault.Client
)

func TestMain(m *testing.M) { os.Exit(run(m)) }

func run(m *testing.M) int {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	// --- Postgres ---
	pgCtr, err := tcpostgres.Run(ctx,
		integrationPGImage,
		tcpostgres.WithDatabase("keeper_api_test"),
		tcpostgres.WithUsername("keeper"),
		tcpostgres.WithPassword("keeper"),
		tcpostgres.BasicWaitStrategies(),
	)
	if err != nil {
		if requireDocker() {
			log.Fatalf("api integration: PG setup failed (REQUIRE_DOCKER): %v", err)
		}
		log.Printf("api integration: skipping, docker unavailable: %v", err)
		return 0
	}
	defer func() {
		tctx, tc := context.WithTimeout(context.Background(), 30*time.Second)
		defer tc()
		_ = pgCtr.Terminate(tctx)
	}()
	dsn, err := pgCtr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		log.Printf("ConnectionString: %v", err)
		return 1
	}
	if err := migrate.Apply(ctx, dsn, migrations.FS, "."); err != nil {
		log.Printf("migrate.Apply: %v", err)
		return 1
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		log.Printf("pgxpool.New: %v", err)
		return 1
	}
	defer pool.Close()
	integrationPool = pool

	// --- Vault ---
	vCtr, err := tcvault.Run(ctx, integrationVaultImage, tcvault.WithToken(integrationVaultToken))
	if err != nil {
		log.Printf("vault Run: %v", err)
		return 1
	}
	defer func() {
		tctx, tc := context.WithTimeout(context.Background(), 30*time.Second)
		defer tc()
		_ = vCtr.Terminate(tctx)
	}()
	vAddr, err := vCtr.HttpHostAddress(ctx)
	if err != nil {
		log.Printf("vault HttpHostAddress: %v", err)
		return 1
	}
	apiCfg := vaultapi.DefaultConfig()
	apiCfg.Address = vAddr
	api, err := vaultapi.NewClient(apiCfg)
	if err != nil {
		log.Printf("vaultapi.NewClient: %v", err)
		return 1
	}
	api.SetToken(integrationVaultToken)

	vc, err := keepervault.NewClient(ctx, config.KeeperVault{
		Addr: vAddr, Token: integrationVaultToken, KVMount: "secret",
	})
	if err != nil {
		log.Printf("keepervault.NewClient: %v", err)
		return 1
	}
	integrationVC = vc

	return m.Run()
}

// startServer starts the HTTP server on an ephemeral port and returns the
// actual base URL + a shutdown function. rbacCfg — the RBAC config (nil →
// an empty enforcer, any Check returns deny).
func startServer(t *testing.T, rbacCfg *rbactest.Config) (baseURL string, shutdown func()) {
	t.Helper()

	verifier, err := keeperjwt.NewVerifier([]byte(integrationSigningKey), integrationIssuer)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	issuer, err := keeperjwt.NewIssuer([]byte(integrationSigningKey), integrationIssuer)
	if err != nil {
		t.Fatalf("NewIssuer: %v", err)
	}
	enforcer, err := rbactest.NewEnforcer(rbacCfg)
	if err != nil {
		t.Fatalf("NewEnforcer: %v", err)
	}
	rbacSvc, err := rbac.NewService(rbac.ServiceDeps{Pool: integrationPool})
	if err != nil {
		t.Fatalf("rbac.NewService: %v", err)
	}
	serviceSvc, err := serviceregistry.NewService(serviceregistry.ServiceDeps{Pool: integrationPool})
	if err != nil {
		t.Fatalf("serviceregistry.NewService: %v", err)
	}

	metricsReg := obs.NewRegistry()
	deps := Deps{
		JWTVerifier:   verifier,
		JWTIssuer:     issuer,
		PGPinger:      poolPinger{integrationPool},
		VaultPinger:   integrationVC,
		AuditWriter:   auditpg.NewWriter(integrationPool),
		RBAC:          enforcer,
		RBACSvc:       rbacSvc,
		ServiceSvc:    serviceSvc,
		OperatorDB:    integrationPool,
		IncarnationDB: integrationPool,
		SoulDB:        integrationPool,
		TTLDefault:    time.Hour,
		MetricsHTTP:   obs.RegisterHTTPMetrics(metricsReg),
		// Voyage circuit: wired for the ADR-047 S4 e2e (scoped command target ∩
		// Purview). Resolvers — production-PG over the same pool.
		VoyageDB:               integrationPool,
		VoyageScenarioResolver: handlers.NewVoyageScenarioPGResolver(integrationPool),
		VoyageCommandResolver:  handlers.NewVoyageCommandPGResolver(integrationPool),
	}
	srv, err := NewServer(
		config.KeeperListenSimple{Addr: "127.0.0.1:0"},
		deps,
		slog.New(slog.NewJSONHandler(io.Discard, nil)),
	)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Start(ctx) }()

	// Wait until the listener actually binds. Start updates srv.Addr to the
	// real port; we wait for a non-empty port to appear.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if addr := srv.Addr(); addr != "" && addr != "127.0.0.1:0" {
			baseURL = "http://" + addr
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("server did not bind within 5s")
		}
		time.Sleep(10 * time.Millisecond)
	}

	shutdown = func() {
		cancel()
		select {
		case <-errCh:
		case <-time.After(15 * time.Second):
			t.Fatal("server did not stop within 15s")
		}
	}
	return baseURL, shutdown
}

// poolPinger — adapts pgxpool.Pool to the health.Pinger interface.
type poolPinger struct{ pool *pgxpool.Pool }

func (p poolPinger) Ping(ctx context.Context) error { return p.pool.Ping(ctx) }

func newValidToken(t *testing.T) string {
	t.Helper()
	return newValidTokenFor(t, "archon-test", []string{"cluster-admin"})
}

// newValidTokenFor issues a JWT for an arbitrary AID — needed by the Operator
// endpoint tests, where claims.Subject determines the audit row and the
// permission check.
func newValidTokenFor(t *testing.T, aid string, roles []string) string {
	t.Helper()
	iss, err := keeperjwt.NewIssuer([]byte(integrationSigningKey), integrationIssuer)
	if err != nil {
		t.Fatalf("NewIssuer: %v", err)
	}
	tok, err := iss.Issue(aid, roles, time.Hour, false)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	return tok
}

// truncateOperators cleans operators + audit_log + incarnation +
// state_history + souls + bootstrap_tokens between tests. One pool per
// package → state is shared between tests; a truncate at the start of each test
// makes the order independent.
//
// FK dependencies (ADR-014): incarnation → operators(aid),
// state_history → operators(aid) + incarnation(name), souls →
// operators(aid) (created_by_aid), bootstrap_tokens → souls(sid) +
// operators(aid). CASCADE removes the dependencies automatically.
func truncateOperators(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	if _, err := integrationPool.Exec(ctx,
		`TRUNCATE state_history, incarnation, bootstrap_tokens, souls, voyages, voyage_targets, operators, audit_log RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
}

// truncateRBAC cleans the RBAC tables (roles / permissions / membership) to a
// clean state for the role.* integration tests. Called AFTER truncateOperators
// (which via CASCADE removes only the membership/roles referencing operators;
// the builtin cluster-admin with created_by_aid=NULL survives CASCADE).
//
// CASCADE on rbac_roles removes permissions + membership (ON DELETE CASCADE).
func truncateRBAC(t *testing.T) {
	t.Helper()
	if _, err := integrationPool.Exec(context.Background(),
		`TRUNCATE rbac_roles, rbac_role_permissions, rbac_role_operators RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncate rbac: %v", err)
	}
}

// seedRole creates a role with a set of permissions directly in the DB (bypassing
// the API) — needed by the Delete/Update/Grant/Revoke tests that rely on an existing role.
func seedRole(t *testing.T, name string, builtin bool, permissions ...string) {
	t.Helper()
	ctx := context.Background()
	if _, err := integrationPool.Exec(ctx,
		`INSERT INTO rbac_roles (name, description, builtin) VALUES ($1, '', $2)`, name, builtin); err != nil {
		t.Fatalf("seedRole(%s): %v", name, err)
	}
	for _, p := range permissions {
		if _, err := integrationPool.Exec(ctx,
			`INSERT INTO rbac_role_permissions (role_name, permission) VALUES ($1, $2)`, name, p); err != nil {
			t.Fatalf("seedRole(%s) permission %q: %v", name, p, err)
		}
	}
}

// seedRoleMember binds an AID to a role directly in the DB.
func seedRoleMember(t *testing.T, roleName, aid string) {
	t.Helper()
	if _, err := integrationPool.Exec(context.Background(),
		`INSERT INTO rbac_role_operators (role_name, aid) VALUES ($1, $2)`, roleName, aid); err != nil {
		t.Fatalf("seedRoleMember(%s, %s): %v", roleName, aid, err)
	}
}

// seedClusterAdmin grants the caller membership of the builtin cluster-admin role (`*`)
// in the DB rbac_* tables (model-C). The config-RBAC enforcer (adminRBAC) only
// lets the caller through the permission-gate middleware; the Service itself does the
// subset-check (least-privilege) and the self-lockout check against the REAL membership
// from rbac_role_operators. Without membership the caller holds 0 effective
// permissions → the subset-check refuses, and self-lockout sees 0 admins.
// truncateRBAC also removes roles — so we re-seed cluster-admin with `*` idempotently.
func seedClusterAdmin(t *testing.T, aid string) {
	t.Helper()
	ctx := context.Background()
	for _, stmt := range []struct {
		q    string
		args []any
	}{
		{`INSERT INTO rbac_roles (name, description, builtin) VALUES ('cluster-admin', '', true)
		  ON CONFLICT (name) DO NOTHING`, nil},
		{`INSERT INTO rbac_role_permissions (role_name, permission) VALUES ('cluster-admin', '*')
		  ON CONFLICT (role_name, permission) DO NOTHING`, nil},
		{`INSERT INTO rbac_role_operators (role_name, aid) VALUES ('cluster-admin', $1)
		  ON CONFLICT (role_name, aid) DO NOTHING`, []any{aid}},
	} {
		if _, err := integrationPool.Exec(ctx, stmt.q, stmt.args...); err != nil {
			t.Fatalf("seedClusterAdmin(%s): %v", aid, err)
		}
	}
}

// seedOperator inserts an AID into the registry for tests that rely on an
// existing Archon. Returns CreatedAt from the DB.
func seedOperator(t *testing.T, aid, parent string) {
	t.Helper()
	op := &operator.Operator{
		AID:         aid,
		DisplayName: aid,
		AuthMethod:  operator.AuthMethodJWT,
	}
	if parent != "" {
		p := parent
		op.CreatedByAID = &p
	}
	if err := operator.Insert(context.Background(), integrationPool, op); err != nil {
		t.Fatalf("seedOperator(%s): %v", aid, err)
	}
}

func TestIntegration_Healthz(t *testing.T) {
	base, stop := startServer(t, nil)
	defer stop()

	resp, err := http.Get(base + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["status"] != "ok" {
		t.Errorf("status field = %q, want ok", body["status"])
	}
}

func TestIntegration_Readyz_AllUp(t *testing.T) {
	base, stop := startServer(t, nil)
	defer stop()

	resp, err := http.Get(base + "/readyz")
	if err != nil {
		t.Fatalf("GET /readyz: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	var body struct {
		Status string            `json:"status"`
		Checks map[string]string `json:"checks"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Status != "ok" {
		t.Errorf("status = %q, want ok (checks=%v)", body.Status, body.Checks)
	}
	if body.Checks["postgres"] != "ok" {
		t.Errorf("postgres = %q", body.Checks["postgres"])
	}
	if body.Checks["vault"] != "ok" {
		t.Errorf("vault = %q", body.Checks["vault"])
	}
}

func TestIntegration_V1_NoAuth_401(t *testing.T) {
	base, stop := startServer(t, nil)
	defer stop()

	resp, err := http.Get(base + "/v1/anything")
	if err != nil {
		t.Fatalf("GET /v1/anything: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); got != problem.ContentType {
		t.Errorf("Content-Type = %q, want %q", got, problem.ContentType)
	}
	var p problem.Details
	if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if p.Type != problem.TypeUnauthenticated {
		t.Errorf("Type = %q", p.Type)
	}
}

func TestIntegration_V1_BadJWT_401(t *testing.T) {
	base, stop := startServer(t, nil)
	defer stop()

	req, _ := http.NewRequest(http.MethodGet, base+"/v1/anything", nil)
	req.Header.Set("Authorization", "Bearer not-a-jwt")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

func TestIntegration_V1_ValidJWT_404(t *testing.T) {
	base, stop := startServer(t, nil)
	defer stop()

	tok := newValidToken(t)
	req, _ := http.NewRequest(http.MethodGet, base+"/v1/anything", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); got != problem.ContentType {
		t.Errorf("Content-Type = %q, want %q", got, problem.ContentType)
	}
}

func TestIntegration_OpenAPI_200(t *testing.T) {
	base, stop := startServer(t, nil)
	defer stop()

	// GET /openapi.yaml — BEHIND the JWT (ADR-054 doc-viewer / router.go): the spec is
	// no longer public, /docs fetches it with a Bearer header. Without a token → 401, so we
	// send a valid JWT (like the UI).
	req, _ := http.NewRequest(http.MethodGet, base+"/openapi.yaml", nil)
	req.Header.Set("Authorization", "Bearer "+newValidToken(t))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /openapi.yaml: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "application/yaml") {
		t.Errorf("Content-Type = %q, want application/yaml prefix", got)
	}
	body, _ := io.ReadAll(resp.Body)
	// The served endpoint returns a runtime dump of the huma aggregator (servedOpenAPIHandler)
	// — this is OpenAPI 3.1.0. The committed docs/keeper/openapi.yaml (3.0.3 for oapi-codegen)
	// — a derived snapshot for the UI vendor, NOT served. huma .YAML() sorts the top-level
	// keys alphabetically (components/info/openapi/paths…), so
	// `openapi: 3.1.0` is NOT the first line — we search for the version as a substring across the document.
	if !strings.Contains(string(body), "openapi: 3.1.0") {
		t.Errorf("body не содержит маркер OpenAPI 3.1.0; first 64 bytes: %q", string(body[:min(64, len(body))]))
	}
}

// TestIntegration_Metrics_NotOnOpenAPI — after ADR-024 / Slice 1 `/metrics`
// is removed from the openapi router (moved to a dedicated listener). On the openapi port
// no endpoint hangs behind the `/v1/*` auth-chain — a bare `/metrics` returns 404
// (catch-all NotFound), not Prometheus output.
func TestIntegration_Metrics_NotOnOpenAPI(t *testing.T) {
	base, stop := startServer(t, nil)
	defer stop()

	resp, err := http.Get(base + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("openapi /metrics status = %d, want 404 (эндпоинт ушёл на выделенный listener)", resp.StatusCode)
	}
}

// TestIntegration_Metrics_RecordsV1Requests — drives traffic to /v1/* via the
// openapi port and checks that the keeper_http_* metrics (recorded by middleware
// on /v1/*) are visible on the DEDICATED metrics listener (the same *obs.Registry).
func TestIntegration_Metrics_RecordsV1Requests(t *testing.T) {
	verifier, err := keeperjwt.NewVerifier([]byte(integrationSigningKey), integrationIssuer)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	issuer, err := keeperjwt.NewIssuer([]byte(integrationSigningKey), integrationIssuer)
	if err != nil {
		t.Fatalf("NewIssuer: %v", err)
	}
	enforcer, err := rbactest.NewEnforcer(nil)
	if err != nil {
		t.Fatalf("NewEnforcer: %v", err)
	}

	// One registry is shared between the HTTP middleware (instrumentation of /v1/*) and
	// the metrics listener (exposition) — as in the production wire-up of keeper/cmd.
	metricsReg := obs.NewRegistry()
	srv, err := NewServer(
		config.KeeperListenSimple{Addr: "127.0.0.1:0"},
		Deps{
			JWTVerifier:   verifier,
			JWTIssuer:     issuer,
			PGPinger:      poolPinger{integrationPool},
			VaultPinger:   integrationVC,
			AuditWriter:   auditpg.NewWriter(integrationPool),
			RBAC:          enforcer,
			OperatorDB:    integrationPool,
			IncarnationDB: integrationPool,
			SoulDB:        integrationPool,
			TTLDefault:    time.Hour,
			MetricsHTTP:   obs.RegisterHTTPMetrics(metricsReg),
		},
		slog.New(slog.NewJSONHandler(io.Discard, nil)),
	)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Start(ctx) }()
	defer func() {
		cancel()
		<-errCh
	}()

	deadline := time.Now().Add(5 * time.Second)
	var base string
	for {
		if addr := srv.Addr(); addr != "" && addr != "127.0.0.1:0" {
			base = "http://" + addr
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("server did not bind within 5s")
		}
		time.Sleep(10 * time.Millisecond)
	}

	metricsSrv, err := obs.ServeMetrics("127.0.0.1:0", metricsReg, nil)
	if err != nil {
		t.Fatalf("ServeMetrics: %v", err)
	}
	defer func() {
		shutCtx, shutCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutCancel()
		_ = metricsSrv.Shutdown(shutCtx)
	}()
	metricsURL := "http://" + metricsSrv.Addr() + "/metrics"

	// the scrape must contain the go-collector before any /v1/ traffic.
	if body := httpGetBody(t, metricsURL); !strings.Contains(body, "go_goroutines") {
		t.Errorf("metrics output не содержит go_goroutines (core-collector); len=%d", len(body))
	}

	// Any request under /v1 without a token → 401, but the pipeline runs up to the
	// metrics middleware (it is inside /v1.Use before auth — see router.go).
	for i := 0; i < 3; i++ {
		resp, err := http.Get(base + "/v1/anything")
		if err != nil {
			t.Fatalf("GET /v1/anything: %v", err)
		}
		_ = resp.Body.Close()
	}

	body := httpGetBody(t, metricsURL)
	if !strings.Contains(body, "keeper_http_requests_total") {
		t.Errorf("metrics output не содержит keeper_http_requests_total; len=%d", len(body))
	}
	if !strings.Contains(body, `status="401"`) {
		t.Errorf("metrics output не содержит status=\"401\" sample; got=\n%s", body)
	}
}

// httpGetBody — GET URL → string body (for the metrics scrape in tests).
func httpGetBody(t *testing.T, url string) string {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status = %d, want 200", url, resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/plain") {
		t.Errorf("GET %s: Content-Type = %q, want text/plain prefix", url, got)
	}
	body, _ := io.ReadAll(resp.Body)
	return string(body)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// --- M0.6b: Operator endpoints ---

// adminRBAC — an RBAC config with one cluster-admin (`archon-alice`).
func adminRBAC() *rbactest.Config {
	return &rbactest.Config{
		Roles: []rbactest.Role{
			{Name: "cluster-admin", Operators: []string{"archon-alice"}, Permissions: []string{"*"}},
		},
	}
}

// twoAdminsRBAC — two cluster-admins, for the revoke-without-self-lockout test.
func twoAdminsRBAC() *rbactest.Config {
	return &rbactest.Config{
		Roles: []rbactest.Role{
			{Name: "cluster-admin", Operators: []string{"archon-alice", "archon-bob"}, Permissions: []string{"*"}},
		},
	}
}

// roReader — RBAC without operator.* permissions (for the 403 test).
func roReaderRBAC() *rbactest.Config {
	return &rbactest.Config{
		Roles: []rbactest.Role{
			{Name: "cluster-admin", Operators: []string{"archon-alice"}, Permissions: []string{"*"}},
			{Name: "viewer", Operators: []string{"archon-viewer"}, Permissions: []string{"soul.list"}},
		},
	}
}

func TestIntegration_Operator_Create_201(t *testing.T) {
	truncateOperators(t)
	seedOperator(t, "archon-alice", "")

	base, stop := startServer(t, adminRBAC())
	defer stop()

	tok := newValidTokenFor(t, "archon-alice", []string{"cluster-admin"})
	body := bytesReader(`{"aid":"archon-bob","display_name":"Bob"}`)
	req, _ := http.NewRequest(http.MethodPost, base+"/v1/operators", body)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 201, body=%s", resp.StatusCode, raw)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out["aid"] != "archon-bob" || out["display_name"] != "Bob" {
		t.Errorf("response = %v", out)
	}
	if out["jwt"] == nil || out["jwt"] == "" {
		t.Errorf("jwt missing in response")
	}

	// Audit row + payload structure (qa-coverage M0.6b): aid /
	// display_name / created_by_aid must appear in the payload, JWT and
	// expires_at — must NOT (sensitive).
	var (
		auditCount   int64
		payloadAID   string
		payloadName  string
		payloadByAID string
	)
	if err := integrationPool.QueryRow(context.Background(),
		`SELECT COUNT(*),
		        MAX(payload->>'aid'),
		        MAX(payload->>'display_name'),
		        MAX(payload->>'created_by_aid')
		   FROM audit_log
		  WHERE event_type='operator.created' AND archon_aid='archon-alice'`).
		Scan(&auditCount, &payloadAID, &payloadName, &payloadByAID); err != nil {
		t.Fatalf("audit count+payload: %v", err)
	}
	if auditCount != 1 {
		t.Errorf("audit count = %d, want 1", auditCount)
	}
	if payloadAID != "archon-bob" {
		t.Errorf("payload.aid = %q, want archon-bob", payloadAID)
	}
	if payloadName != "Bob" {
		t.Errorf("payload.display_name = %q, want Bob", payloadName)
	}
	if payloadByAID != "archon-alice" {
		t.Errorf("payload.created_by_aid = %q, want archon-alice", payloadByAID)
	}

	// JWT and expires_at — sensitive, NOT written to the audit payload.
	var jwtPresent, expPresent bool
	if err := integrationPool.QueryRow(context.Background(),
		`SELECT payload ? 'jwt', payload ? 'expires_at'
		   FROM audit_log
		  WHERE event_type='operator.created' AND archon_aid='archon-alice'`).
		Scan(&jwtPresent, &expPresent); err != nil {
		t.Fatalf("audit payload sensitive-keys probe: %v", err)
	}
	if jwtPresent {
		t.Errorf("audit payload leaked 'jwt' key")
	}
	if expPresent {
		t.Errorf("audit payload leaked 'expires_at' key")
	}
}

func TestIntegration_Operator_Create_403_NoPermission(t *testing.T) {
	truncateOperators(t)
	seedOperator(t, "archon-viewer", "")

	base, stop := startServer(t, roReaderRBAC())
	defer stop()

	tok := newValidTokenFor(t, "archon-viewer", []string{"viewer"})
	body := bytesReader(`{"aid":"archon-bob"}`)
	req, _ := http.NewRequest(http.MethodPost, base+"/v1/operators", body)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); got != problem.ContentType {
		t.Errorf("Content-Type = %q", got)
	}
}

func TestIntegration_Operator_Revoke_SingleAdmin_409Lockout(t *testing.T) {
	truncateOperators(t)
	truncateRBAC(t)
	seedOperator(t, "archon-alice", "")
	seedClusterAdmin(t, "archon-alice")

	base, stop := startServer(t, adminRBAC())
	defer stop()

	tok := newValidTokenFor(t, "archon-alice", []string{"cluster-admin"})
	// the revoke contract requires a requestBody (committed openapi.yaml →
	// requestBody.required: true; the single field reason — optional). An empty
	// JSON object satisfies huma validation without setting reason.
	req, _ := http.NewRequest(http.MethodPost, base+"/v1/operators/archon-alice/revoke", bytesReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 409, body=%s", resp.StatusCode, raw)
	}
	var p problem.Details
	if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if p.Type != problem.TypeWouldLockOutCluster {
		t.Errorf("Type = %q, want %q", p.Type, problem.TypeWouldLockOutCluster)
	}
}

func TestIntegration_Operator_Revoke_204(t *testing.T) {
	truncateOperators(t)
	seedOperator(t, "archon-alice", "")
	seedOperator(t, "archon-bob", "archon-alice")

	base, stop := startServer(t, twoAdminsRBAC())
	defer stop()

	tok := newValidTokenFor(t, "archon-alice", []string{"cluster-admin"})
	req, _ := http.NewRequest(http.MethodPost, base+"/v1/operators/archon-bob/revoke",
		bytesReader(`{"reason":"left team"}`))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 204, body=%s", resp.StatusCode, raw)
	}

	// Bob is revoked in the DB.
	op, err := operator.SelectByAID(context.Background(), integrationPool, "archon-bob")
	if err != nil {
		t.Fatalf("SelectByAID: %v", err)
	}
	if !op.IsRevoked() {
		t.Errorf("bob should be revoked")
	}

	// Audit row.
	var n int64
	if err := integrationPool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM audit_log WHERE event_type='operator.revoked'`).Scan(&n); err != nil {
		t.Fatalf("audit count: %v", err)
	}
	if n != 1 {
		t.Errorf("audit count = %d, want 1", n)
	}
}

func TestIntegration_Operator_IssueToken_RevokedOperator_409(t *testing.T) {
	truncateOperators(t)
	seedOperator(t, "archon-alice", "")
	seedOperator(t, "archon-bob", "archon-alice")
	// Revoke Bob via crud directly (bypassing HTTP).
	if err := operator.Revoke(context.Background(), integrationPool, "archon-bob", "test"); err != nil {
		t.Fatalf("Revoke seed: %v", err)
	}

	base, stop := startServer(t, twoAdminsRBAC())
	defer stop()

	tok := newValidTokenFor(t, "archon-alice", []string{"cluster-admin"})
	req, _ := http.NewRequest(http.MethodPost, base+"/v1/operators/archon-bob/issue-token", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 409, body=%s", resp.StatusCode, raw)
	}
	var p problem.Details
	if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if p.Type != problem.TypeOperatorRevoked {
		t.Errorf("Type = %q", p.Type)
	}
}

func TestIntegration_Operator_IssueToken_200(t *testing.T) {
	truncateOperators(t)
	seedOperator(t, "archon-alice", "")
	seedOperator(t, "archon-bob", "archon-alice")

	base, stop := startServer(t, twoAdminsRBAC())
	defer stop()

	tok := newValidTokenFor(t, "archon-alice", []string{"cluster-admin"})
	req, _ := http.NewRequest(http.MethodPost, base+"/v1/operators/archon-bob/issue-token", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200, body=%s", resp.StatusCode, raw)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out["aid"] != "archon-bob" {
		t.Errorf("aid = %v", out["aid"])
	}
	if out["jwt"] == nil || out["jwt"] == "" {
		t.Errorf("jwt missing")
	}

	// Audit row.
	var n int64
	if err := integrationPool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM audit_log WHERE event_type='operator.token-issued'`).Scan(&n); err != nil {
		t.Fatalf("audit count: %v", err)
	}
	if n != 1 {
		t.Errorf("audit count = %d, want 1", n)
	}
}

func TestIntegration_Operator_Create_DuplicateAID_409(t *testing.T) {
	truncateOperators(t)
	seedOperator(t, "archon-alice", "")
	seedOperator(t, "archon-bob", "archon-alice")

	base, stop := startServer(t, adminRBAC())
	defer stop()

	tok := newValidTokenFor(t, "archon-alice", []string{"cluster-admin"})
	body := bytesReader(`{"aid":"archon-bob"}`)
	req, _ := http.NewRequest(http.MethodPost, base+"/v1/operators", body)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 409, body=%s", resp.StatusCode, raw)
	}
}

func TestIntegration_Operator_Revoke_AlreadyRevoked_409(t *testing.T) {
	truncateOperators(t)
	seedOperator(t, "archon-alice", "")
	seedOperator(t, "archon-bob", "archon-alice")
	_ = operator.Revoke(context.Background(), integrationPool, "archon-bob", "")

	base, stop := startServer(t, twoAdminsRBAC())
	defer stop()

	tok := newValidTokenFor(t, "archon-alice", []string{"cluster-admin"})
	// the revoke contract requires a requestBody (see Revoke_SingleAdmin); an empty
	// JSON object passes validation, reason is not set.
	req, _ := http.NewRequest(http.MethodPost, base+"/v1/operators/archon-bob/revoke", bytesReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("status = %d, want 409", resp.StatusCode)
	}
}

// --- M0.6c-1: Incarnation endpoints ---

// seedIncarnation inserts an incarnation row via the CRUD layer for the
// Get/History/List tests that rely on existing records.
func seedIncarnation(t *testing.T, name, service, creator string) {
	t.Helper()
	ctx := context.Background()
	c := creator
	inc := &incarnation.Incarnation{
		Name:               name,
		Service:            service,
		ServiceVersion:     "v1",
		StateSchemaVersion: 1,
		Status:             incarnation.StatusReady,
		CreatedByAID:       &c,
	}
	if err := incarnation.Create(ctx, integrationPool, inc); err != nil {
		t.Fatalf("seedIncarnation(%s): %v", name, err)
	}
}

func TestIntegration_Incarnation_Create_202(t *testing.T) {
	truncateOperators(t)
	seedOperator(t, "archon-alice", "")

	base, stop := startServer(t, adminRBAC())
	defer stop()

	tok := newValidTokenFor(t, "archon-alice", []string{"cluster-admin"})
	body := bytesReader(`{"name":"redis-test","service":"redis","input":{}}`)
	req, _ := http.NewRequest(http.MethodPost, base+"/v1/incarnations", body)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 202, body=%s", resp.StatusCode, raw)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out["incarnation"] != "redis-test" {
		t.Errorf("incarnation = %v", out["incarnation"])
	}
	// The `status` field must NOT be in the response (OpenAPI:
	// IncarnationCreateReply declares only apply_id+incarnation).
	if _, hasStatus := out["status"]; hasStatus {
		t.Errorf("response contains 'status' field, want absent: %v", out)
	}
	if s, _ := out["apply_id"].(string); len(s) != 26 {
		t.Errorf("apply_id = %q (len=%d), want ULID 26 chars", s, len(s))
	}

	// Row in the DB.
	got, err := incarnation.SelectByName(context.Background(), integrationPool, "redis-test")
	if err != nil {
		t.Fatalf("SelectByName: %v", err)
	}
	if got.Service != "redis" || got.Status != incarnation.StatusReady {
		t.Errorf("row = %+v", got)
	}
	if got.CreatedByAID == nil || *got.CreatedByAID != "archon-alice" {
		t.Errorf("CreatedByAID = %v", got.CreatedByAID)
	}

	// The audit event is written with the correct payload (qa-coverage M0.6c-1):
	// apply_id + name + service must appear in the payload (apply_id for
	// correlation, name/service for audit filtering without a join).
	var (
		auditCount   int64
		auditApplyID string
		auditName    string
		auditService string
	)
	if err := integrationPool.QueryRow(context.Background(),
		`SELECT COUNT(*),
		        MAX(payload->>'apply_id'),
		        MAX(payload->>'name'),
		        MAX(payload->>'service')
		   FROM audit_log
		  WHERE event_type='incarnation.created' AND archon_aid='archon-alice'`).
		Scan(&auditCount, &auditApplyID, &auditName, &auditService); err != nil {
		t.Fatalf("audit query: %v", err)
	}
	if auditCount != 1 {
		t.Errorf("audit count = %d, want 1", auditCount)
	}
	if auditApplyID != out["apply_id"] {
		t.Errorf("audit apply_id %q != response apply_id %v", auditApplyID, out["apply_id"])
	}
	if auditName != "redis-test" {
		t.Errorf("audit payload.name = %q, want redis-test", auditName)
	}
	if auditService != "redis" {
		t.Errorf("audit payload.service = %q, want redis", auditService)
	}
}

func TestIntegration_Incarnation_Create_DuplicateName_409(t *testing.T) {
	truncateOperators(t)
	seedOperator(t, "archon-alice", "")
	seedIncarnation(t, "redis-test", "redis", "archon-alice")

	base, stop := startServer(t, adminRBAC())
	defer stop()

	tok := newValidTokenFor(t, "archon-alice", []string{"cluster-admin"})
	body := bytesReader(`{"name":"redis-test","service":"redis"}`)
	req, _ := http.NewRequest(http.MethodPost, base+"/v1/incarnations", body)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 409, body=%s", resp.StatusCode, raw)
	}
}

func TestIntegration_Incarnation_Get_200(t *testing.T) {
	truncateOperators(t)
	seedOperator(t, "archon-alice", "")
	seedIncarnation(t, "redis-test", "redis", "archon-alice")

	base, stop := startServer(t, adminRBAC())
	defer stop()

	tok := newValidTokenFor(t, "archon-alice", []string{"cluster-admin"})
	req, _ := http.NewRequest(http.MethodGet, base+"/v1/incarnations/redis-test", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200, body=%s", resp.StatusCode, raw)
	}
	var dto map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&dto); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if dto["name"] != "redis-test" {
		t.Errorf("name = %v", dto["name"])
	}
	if dto["status"] != "ready" {
		t.Errorf("status = %v", dto["status"])
	}
	// created_by_aid — required in OpenAPI, must be present (not omitted).
	if _, present := dto["created_by_aid"]; !present {
		t.Errorf("response missing 'created_by_aid' (OpenAPI required): %v", dto)
	}
	if dto["created_by_aid"] != "archon-alice" {
		t.Errorf("created_by_aid = %v, want \"archon-alice\"", dto["created_by_aid"])
	}
	// status_details — nullable in OpenAPI; for status=ready we expect null
	// present (rather than an omitted key).
	v, present := dto["status_details"]
	if !present {
		t.Errorf("response missing 'status_details' (must be null, not omitted)")
	}
	if v != nil {
		t.Errorf("status_details = %v, want null", v)
	}
}

// TestIntegration_Incarnation_Get_CreatedByAID_NullAfterRevoke — after an
// operator is deleted the FK `incarnation.created_by_aid` goes to SET NULL
// (ADR-014), and the Get response must return `"created_by_aid": null`,
// not an omitted key (qa coverage gap M0.6c-1).
func TestIntegration_Incarnation_Get_CreatedByAID_NullAfterRevoke(t *testing.T) {
	truncateOperators(t)
	seedOperator(t, "archon-alice", "")
	seedOperator(t, "archon-bob", "archon-alice")
	seedIncarnation(t, "redis-test", "redis", "archon-bob")

	// Direct DELETE of the operator row → FK ON DELETE SET NULL on incarnation.created_by_aid.
	if _, err := integrationPool.Exec(context.Background(),
		`DELETE FROM operators WHERE aid = 'archon-bob'`); err != nil {
		t.Fatalf("DELETE operator: %v", err)
	}

	base, stop := startServer(t, adminRBAC())
	defer stop()

	tok := newValidTokenFor(t, "archon-alice", []string{"cluster-admin"})
	req, _ := http.NewRequest(http.MethodGet, base+"/v1/incarnations/redis-test", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200, body=%s", resp.StatusCode, raw)
	}
	var dto map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&dto); err != nil {
		t.Fatalf("decode: %v", err)
	}
	v, present := dto["created_by_aid"]
	if !present {
		t.Errorf("response missing 'created_by_aid' (must be null, not omitted): %v", dto)
	}
	if v != nil {
		t.Errorf("created_by_aid = %v, want null after operator delete", v)
	}
}

func TestIntegration_Incarnation_Get_404(t *testing.T) {
	truncateOperators(t)
	seedOperator(t, "archon-alice", "")

	base, stop := startServer(t, adminRBAC())
	defer stop()

	tok := newValidTokenFor(t, "archon-alice", []string{"cluster-admin"})
	req, _ := http.NewRequest(http.MethodGet, base+"/v1/incarnations/ghost", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestIntegration_Incarnation_List_200(t *testing.T) {
	truncateOperators(t)
	seedOperator(t, "archon-alice", "")
	seedIncarnation(t, "redis-a", "redis", "archon-alice")
	time.Sleep(2 * time.Millisecond)
	seedIncarnation(t, "redis-b", "redis", "archon-alice")
	time.Sleep(2 * time.Millisecond)
	seedIncarnation(t, "mysql-c", "mysql", "archon-alice")

	base, stop := startServer(t, adminRBAC())
	defer stop()

	tok := newValidTokenFor(t, "archon-alice", []string{"cluster-admin"})

	// Without a filter — 3 items.
	req, _ := http.NewRequest(http.MethodGet, base+"/v1/incarnations", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, body=%s", resp.StatusCode, raw)
	}
	var out struct {
		Items  []map[string]any `json:"items"`
		Total  int              `json:"total"`
		Offset int              `json:"offset"`
		Limit  int              `json:"limit"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Total != 3 || len(out.Items) != 3 {
		t.Errorf("Total=%d len=%d, want 3/3", out.Total, len(out.Items))
	}

	// GUARD contract-wire: the items[] element carries snake_case keys (OpenAPI
	// snake_case schema), NOT the PascalCase of the domain *View. Positive — the key is present;
	// negative — the PascalCase twin is ABSENT (without it the test stays green when both
	// keys are present). Regression: the list Body was serialized via the untagged
	// IncarnationGetView → ApplyID/CreatedAt keys (contract bug #7).
	first := out.Items[0]
	if _, ok := first["created_at"]; !ok {
		t.Errorf("items[0] missing snake_case ключ 'created_at': %v", first)
	}
	if _, ok := first["CreatedAt"]; ok {
		t.Fatalf("items[0] несёт PascalCase ключ 'CreatedAt' (контракт-wire сломан): %v", first)
	}
	if _, ok := first["StateSchemaVersion"]; ok {
		t.Fatalf("items[0] несёт PascalCase ключ 'StateSchemaVersion' (контракт-wire сломан): %v", first)
	}

	// service-filter.
	req2, _ := http.NewRequest(http.MethodGet, base+"/v1/incarnations?service=redis", nil)
	req2.Header.Set("Authorization", "Bearer "+tok)
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp2.Body.Close()
	var out2 struct {
		Items []map[string]any `json:"items"`
		Total int              `json:"total"`
	}
	_ = json.NewDecoder(resp2.Body).Decode(&out2)
	if out2.Total != 2 {
		t.Errorf("filtered total = %d, want 2", out2.Total)
	}
}

func TestIntegration_Incarnation_List_Pagination(t *testing.T) {
	truncateOperators(t)
	seedOperator(t, "archon-alice", "")
	for _, n := range []string{"a", "b", "c", "d", "e"} {
		seedIncarnation(t, "redis-"+n, "redis", "archon-alice")
		time.Sleep(2 * time.Millisecond)
	}

	base, stop := startServer(t, adminRBAC())
	defer stop()

	tok := newValidTokenFor(t, "archon-alice", []string{"cluster-admin"})
	req, _ := http.NewRequest(http.MethodGet, base+"/v1/incarnations?offset=2&limit=2", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, body=%s", resp.StatusCode, raw)
	}
	var out struct {
		Items  []map[string]any `json:"items"`
		Total  int              `json:"total"`
		Offset int              `json:"offset"`
		Limit  int              `json:"limit"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Total != 5 || out.Offset != 2 || out.Limit != 2 {
		t.Errorf("pagination meta = %+v, want total=5 offset=2 limit=2", out)
	}
	if len(out.Items) != 2 {
		t.Errorf("len(Items) = %d, want 2", len(out.Items))
	}
}

func TestIntegration_Incarnation_History_200(t *testing.T) {
	truncateOperators(t)
	seedOperator(t, "archon-alice", "")
	seedIncarnation(t, "redis-test", "redis", "archon-alice")

	// Seed history directly — the handler-side write comes in M0.6c-2.
	ctx := context.Background()
	if _, err := integrationPool.Exec(ctx, `
INSERT INTO state_history (history_id, incarnation_name, scenario,
    state_before, state_after, changed_by_aid, apply_id)
VALUES ('01HFIRST', 'redis-test', 'create', '{}', '{"x":1}', 'archon-alice', '01HAPPLY')`); err != nil {
		t.Fatalf("seed history: %v", err)
	}

	base, stop := startServer(t, adminRBAC())
	defer stop()

	tok := newValidTokenFor(t, "archon-alice", []string{"cluster-admin"})
	req, _ := http.NewRequest(http.MethodGet, base+"/v1/incarnations/redis-test/history", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, body=%s", resp.StatusCode, raw)
	}
	var out struct {
		Items []map[string]any `json:"items"`
		Total int              `json:"total"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if out.Total != 1 || len(out.Items) != 1 {
		t.Errorf("history Total=%d len=%d", out.Total, len(out.Items))
	}
	if out.Items[0]["history_id"] != "01HFIRST" {
		t.Errorf("items[0].history_id = %v", out.Items[0]["history_id"])
	}
	// GUARD contract-wire: positive snake_case + negative PascalCase twin
	// (an untagged StateHistoryView would give HistoryID/ApplyID — contract bug #7).
	if _, ok := out.Items[0]["apply_id"]; !ok {
		t.Errorf("items[0] missing snake_case ключ 'apply_id': %v", out.Items[0])
	}
	if _, ok := out.Items[0]["HistoryID"]; ok {
		t.Fatalf("items[0] несёт PascalCase ключ 'HistoryID' (контракт-wire сломан): %v", out.Items[0])
	}
	if _, ok := out.Items[0]["ApplyID"]; ok {
		t.Fatalf("items[0] несёт PascalCase ключ 'ApplyID' (контракт-wire сломан): %v", out.Items[0])
	}
	// The timestamp must be filled and parse as RFC3339 (state_history.at
	// via DEFAULT NOW()). The wire key is created_at (shared with the rest of the Operator
	// API). Regression: clients received a null/empty timestamp.
	atRaw, ok := out.Items[0]["created_at"].(string)
	if !ok || atRaw == "" {
		t.Fatalf("items[0].created_at = %v, want непустой RFC3339 timestamp", out.Items[0]["created_at"])
	}
	ts, err := time.Parse(time.RFC3339, atRaw)
	if err != nil {
		t.Errorf("items[0].created_at = %q не парсится как RFC3339: %v", atRaw, err)
	}
	if ts.IsZero() {
		t.Errorf("items[0].created_at = %q — zero time, ожидался реальный момент записи", atRaw)
	}
}

func TestIntegration_Incarnation_History_FilterByApplyID(t *testing.T) {
	truncateOperators(t)
	seedOperator(t, "archon-alice", "")
	seedIncarnation(t, "redis-test", "redis", "archon-alice")

	// Two state_history rows with different apply_id — the filter must keep one.
	ctx := context.Background()
	if _, err := integrationPool.Exec(ctx, `
INSERT INTO state_history (history_id, incarnation_name, scenario,
    state_before, state_after, changed_by_aid, apply_id)
VALUES
  ('01HFRST000000000000000000A', 'redis-test', 'create', '{}',      '{"v":1}', 'archon-alice', '01HAPPYAAAAAAAAAAAAAAAAA00'),
  ('01HSCND000000000000000000B', 'redis-test', 'scale',  '{"v":1}', '{"v":2}', 'archon-alice', '01HAPPYBBBBBBBBBBBBBBBBB00')
`); err != nil {
		t.Fatalf("seed history: %v", err)
	}

	base, stop := startServer(t, adminRBAC())
	defer stop()

	tok := newValidTokenFor(t, "archon-alice", []string{"cluster-admin"})

	// Filter matches second row.
	req, _ := http.NewRequest(http.MethodGet,
		base+"/v1/incarnations/redis-test/history?apply_id=01HAPPYBBBBBBBBBBBBBBBBB00", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, body=%s", resp.StatusCode, raw)
	}
	var out struct {
		Items []map[string]any `json:"items"`
		Total int              `json:"total"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if out.Total != 1 || len(out.Items) != 1 {
		t.Fatalf("filter matched: Total=%d items=%d, want 1/1", out.Total, len(out.Items))
	}
	if out.Items[0]["history_id"] != "01HSCND000000000000000000B" {
		t.Errorf("items[0].history_id = %v, want 01HSCND000000000000000000B", out.Items[0]["history_id"])
	}

	// Filter — a non-matching ULID (syntactically valid, but there is no such
	// apply_id in the DB): 200 + items=[], total=0. NOT 404.
	req2, _ := http.NewRequest(http.MethodGet,
		base+"/v1/incarnations/redis-test/history?apply_id=01HGHST00000000000000000ZZ", nil)
	req2.Header.Set("Authorization", "Bearer "+tok)
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp2.Body)
		t.Fatalf("non-matching apply_id: status = %d, body=%s (want 200)", resp2.StatusCode, raw)
	}
	var out2 struct {
		Items []map[string]any `json:"items"`
		Total int              `json:"total"`
	}
	_ = json.NewDecoder(resp2.Body).Decode(&out2)
	if out2.Total != 0 || len(out2.Items) != 0 {
		t.Errorf("non-matching apply_id: Total=%d items=%d, want 0/0", out2.Total, len(out2.Items))
	}

	// Invalid ULID → 400.
	req3, _ := http.NewRequest(http.MethodGet,
		base+"/v1/incarnations/redis-test/history?apply_id=not-a-ulid", nil)
	req3.Header.Set("Authorization", "Bearer "+tok)
	resp3, err := http.DefaultClient.Do(req3)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp3.Body.Close()
	if resp3.StatusCode != http.StatusBadRequest {
		raw, _ := io.ReadAll(resp3.Body)
		t.Errorf("invalid apply_id: status = %d, body=%s (want 400)", resp3.StatusCode, raw)
	}
}

func TestIntegration_Incarnation_History_404(t *testing.T) {
	truncateOperators(t)
	seedOperator(t, "archon-alice", "")

	base, stop := startServer(t, adminRBAC())
	defer stop()

	tok := newValidTokenFor(t, "archon-alice", []string{"cluster-admin"})
	req, _ := http.NewRequest(http.MethodGet, base+"/v1/incarnations/ghost/history", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

// snakeCaseKeyRe — the contract wire format of a JSON key (snake_case): starts with
// a lowercase letter, then [a-z0-9_]. Any uppercase (PascalCase untagged View) → fail.
var snakeCaseKeyRe = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// TestIntegration_ListEndpoints_SnakeCaseWire — a contract-wire class guard: for
// each paged-list endpoint the first items[] element must carry ONLY snake_case
// keys (OpenAPI snake_case schema). Catches the whole class of "a domain *View without json tags
// goes into Body directly, bypassing the projection → PascalCase keys" (contract bug #7) and for
// future list endpoints too. Each sub-test seeds its own data in isolation.
func TestIntegration_ListEndpoints_SnakeCaseWire(t *testing.T) {
	cases := []struct {
		name  string
		rbac  *rbactest.Config
		seed  func(t *testing.T)
		path  string
		minOk int // minimum expected items (>=1 — otherwise the guard is a no-op)
	}{
		{
			name: "incarnation_list",
			rbac: adminRBAC(),
			seed: func(t *testing.T) {
				seedIncarnation(t, "redis-a", "redis", "archon-alice")
			},
			path:  "/v1/incarnations",
			minOk: 1,
		},
		{
			name: "incarnation_history",
			rbac: adminRBAC(),
			seed: func(t *testing.T) {
				seedIncarnation(t, "redis-h", "redis", "archon-alice")
				if _, err := integrationPool.Exec(context.Background(), `
INSERT INTO state_history (history_id, incarnation_name, scenario,
    state_before, state_after, changed_by_aid, apply_id)
VALUES ('01HHIST000000000000000000A', 'redis-h', 'create', '{}', '{"x":1}', 'archon-alice', '01HAPPLY00000000000000000A')`); err != nil {
					t.Fatalf("seed history: %v", err)
				}
			},
			path:  "/v1/incarnations/redis-h/history",
			minOk: 1,
		},
		{
			name: "soul_list",
			rbac: soulRBAC(),
			seed: func(t *testing.T) {
				seedSoulFull(t, "redis-01.example.com", "agent", soul.StatusConnected, []string{"redis-prod"}, "archon-alice")
			},
			path:  "/v1/souls",
			minOk: 1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			truncateOperators(t)
			seedOperator(t, "archon-alice", "")
			tc.seed(t)

			base, stop := startServer(t, tc.rbac)
			defer stop()

			tok := newValidTokenFor(t, "archon-alice", []string{"cluster-admin"})
			req, _ := http.NewRequest(http.MethodGet, base+tc.path, nil)
			req.Header.Set("Authorization", "Bearer "+tok)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("Do: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				raw, _ := io.ReadAll(resp.Body)
				t.Fatalf("status = %d, body=%s", resp.StatusCode, raw)
			}
			var out struct {
				Items []map[string]any `json:"items"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if len(out.Items) < tc.minOk {
				t.Fatalf("items len = %d, want >= %d (guard вхолостую без элемента)", len(out.Items), tc.minOk)
			}
			for key := range out.Items[0] {
				if !snakeCaseKeyRe.MatchString(key) {
					t.Errorf("items[0] ключ %q не snake_case (контракт-wire сломан): %v", key, out.Items[0])
				}
			}
		})
	}
}

// TestIntegration_Incarnation_Create_BodyTooLarge_413 — body > v1RequestBodyLimit
// (1 MiB). The huma route (incarnation Create migrated to huma) limits the body itself
// and on overflow returns an RFC-correct 413 Payload Too Large (huma v2:
// "request body is too large limit=… bytes"), not 400. The test checks exactly 413.
func TestIntegration_Incarnation_Create_BodyTooLarge_413(t *testing.T) {
	truncateOperators(t)
	seedOperator(t, "archon-alice", "")

	base, stop := startServer(t, adminRBAC())
	defer stop()

	tok := newValidTokenFor(t, "archon-alice", []string{"cluster-admin"})
	// 2 MiB of random valid JSON (a huge input object).
	big := strings.Repeat("a", 2<<20)
	body := bytesReader(`{"name":"redis-test","service":"redis","input":{"x":"` + big + `"}}`)
	req, _ := http.NewRequest(http.MethodPost, base+"/v1/incarnations", body)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 413, body=%s", resp.StatusCode, raw)
	}
}

func TestIntegration_Incarnation_403_NoPermission(t *testing.T) {
	truncateOperators(t)
	seedOperator(t, "archon-viewer", "")

	base, stop := startServer(t, roReaderRBAC())
	defer stop()

	tok := newValidTokenFor(t, "archon-viewer", []string{"viewer"})
	body := bytesReader(`{"name":"redis-test","service":"redis"}`)
	req, _ := http.NewRequest(http.MethodPost, base+"/v1/incarnations", body)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403", resp.StatusCode)
	}
}

// --- M2.x: Soul onboarding endpoints ---

// soulRBAC — cluster-admin + viewer with soul.create / soul.issue-token on the
// admin, to cover both 201/200 and 403.
func soulRBAC() *rbactest.Config {
	return &rbactest.Config{
		Roles: []rbactest.Role{
			{Name: "cluster-admin", Operators: []string{"archon-alice"}, Permissions: []string{"*"}},
			{Name: "viewer", Operators: []string{"archon-viewer"}, Permissions: []string{"soul.list"}},
		},
	}
}

func TestIntegration_Soul_Create_201(t *testing.T) {
	truncateOperators(t)
	seedOperator(t, "archon-alice", "")

	base, stop := startServer(t, soulRBAC())
	defer stop()

	tok := newValidTokenFor(t, "archon-alice", []string{"cluster-admin"})
	body := bytesReader(`{"sid":"web-01.example.com","transport":"agent","covens":["prod"]}`)
	req, _ := http.NewRequest(http.MethodPost, base+"/v1/souls", body)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 201, body=%s", resp.StatusCode, raw)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out["sid"] != "web-01.example.com" || out["status"] != "pending" {
		t.Errorf("response = %v", out)
	}
	// covens from the request are persisted into souls.coven (binding at onboarding,
	// GAP #3): without it the host is not targeted by a scenario, a direct UPDATE in the DB is needed.
	if covens, _ := out["covens"].([]any); len(covens) != 1 || covens[0] != "prod" {
		t.Errorf("response covens = %v, want [prod]", out["covens"])
	}
	if tokStr, _ := out["bootstrap_token"].(string); tokStr == "" {
		t.Errorf("bootstrap_token missing for transport=agent: %v", out)
	}
	if out["created_by_aid"] != "archon-alice" {
		t.Errorf("created_by_aid = %v", out["created_by_aid"])
	}

	// souls row created, status=pending.
	got, err := soulSelectStatus(t, "web-01.example.com")
	if err != nil {
		t.Fatalf("soulSelectStatus: %v", err)
	}
	if got != "pending" {
		t.Errorf("souls.status = %q, want pending", got)
	}

	// requested_at is set (B1): needed for the Reaper rule pending→expired
	// via the index souls_pending_requested_at_idx (docs/soul/onboarding.md:51).
	var requestedAt *time.Time
	if err := integrationPool.QueryRow(context.Background(),
		`SELECT requested_at FROM souls WHERE sid='web-01.example.com'`).Scan(&requestedAt); err != nil {
		t.Fatalf("requested_at probe: %v", err)
	}
	if requestedAt == nil {
		t.Fatalf("souls.requested_at is NULL, want NOW() (Reaper pending→expired would miss this row)")
	}
	if skew := time.Since(*requestedAt); skew < -time.Minute || skew > 5*time.Minute {
		t.Errorf("requested_at skew from now = %s, want within ~now", skew)
	}

	// souls.coven is persisted from the request's covens field (GAP #3).
	var savedCoven []string
	if err := integrationPool.QueryRow(context.Background(),
		`SELECT coven FROM souls WHERE sid='web-01.example.com'`).Scan(&savedCoven); err != nil {
		t.Fatalf("coven probe: %v", err)
	}
	if len(savedCoven) != 1 || savedCoven[0] != "prod" {
		t.Errorf("souls.coven = %v, want [prod]", savedCoven)
	}

	// An active bootstrap token in the DB.
	var tokenCount int64
	if err := integrationPool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM bootstrap_tokens WHERE sid='web-01.example.com' AND used_at IS NULL`).
		Scan(&tokenCount); err != nil {
		t.Fatalf("token count: %v", err)
	}
	if tokenCount != 1 {
		t.Errorf("active token count = %d, want 1", tokenCount)
	}

	// Audit: the plaintext token must NOT appear in the payload (substring-mask H1).
	var auditCount int64
	var tokenLeaked bool
	if err := integrationPool.QueryRow(context.Background(),
		`SELECT COUNT(*), bool_or(payload ? 'bootstrap_token')
		   FROM audit_log
		  WHERE event_type='soul.created' AND archon_aid='archon-alice'`).
		Scan(&auditCount, &tokenLeaked); err != nil {
		t.Fatalf("audit query: %v", err)
	}
	if auditCount != 1 {
		t.Errorf("audit count = %d, want 1", auditCount)
	}
	if tokenLeaked {
		t.Errorf("audit payload leaked 'bootstrap_token' key")
	}
}

func TestIntegration_Soul_Create_SSH_NoToken(t *testing.T) {
	truncateOperators(t)
	seedOperator(t, "archon-alice", "")

	base, stop := startServer(t, soulRBAC())
	defer stop()

	tok := newValidTokenFor(t, "archon-alice", []string{"cluster-admin"})
	body := bytesReader(`{"sid":"ssh-01.example.com","transport":"ssh"}`)
	req, _ := http.NewRequest(http.MethodPost, base+"/v1/souls", body)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 201, body=%s", resp.StatusCode, raw)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if tokStr, _ := out["bootstrap_token"].(string); tokStr != "" {
		t.Errorf("bootstrap_token should be empty for transport=ssh, got %q", tokStr)
	}

	var tokenCount int64
	if err := integrationPool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM bootstrap_tokens WHERE sid='ssh-01.example.com'`).Scan(&tokenCount); err != nil {
		t.Fatalf("token count: %v", err)
	}
	if tokenCount != 0 {
		t.Errorf("ssh-host token count = %d, want 0", tokenCount)
	}
}

func TestIntegration_Soul_Create_Duplicate_409(t *testing.T) {
	truncateOperators(t)
	seedOperator(t, "archon-alice", "")
	seedSoul(t, "web-01.example.com", "agent", "archon-alice")

	base, stop := startServer(t, soulRBAC())
	defer stop()

	tok := newValidTokenFor(t, "archon-alice", []string{"cluster-admin"})
	body := bytesReader(`{"sid":"web-01.example.com","transport":"agent"}`)
	req, _ := http.NewRequest(http.MethodPost, base+"/v1/souls", body)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 409, body=%s", resp.StatusCode, raw)
	}
	var p problem.Details
	if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if p.Type != problem.TypeSoulExists {
		t.Errorf("Type = %q, want %q", p.Type, problem.TypeSoulExists)
	}
}

func TestIntegration_Soul_Create_InvalidTransport_422(t *testing.T) {
	truncateOperators(t)
	seedOperator(t, "archon-alice", "")

	base, stop := startServer(t, soulRBAC())
	defer stop()

	tok := newValidTokenFor(t, "archon-alice", []string{"cluster-admin"})
	body := bytesReader(`{"sid":"web-01.example.com","transport":"carrier-pigeon"}`)
	req, _ := http.NewRequest(http.MethodPost, base+"/v1/souls", body)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 422, body=%s", resp.StatusCode, raw)
	}
}

func TestIntegration_Soul_Create_403_NoPermission(t *testing.T) {
	truncateOperators(t)
	seedOperator(t, "archon-viewer", "")

	base, stop := startServer(t, soulRBAC())
	defer stop()

	tok := newValidTokenFor(t, "archon-viewer", []string{"viewer"})
	body := bytesReader(`{"sid":"web-01.example.com","transport":"agent"}`)
	req, _ := http.NewRequest(http.MethodPost, base+"/v1/souls", body)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403", resp.StatusCode)
	}
}

func TestIntegration_Soul_IssueToken_NotFound_404(t *testing.T) {
	truncateOperators(t)
	seedOperator(t, "archon-alice", "")

	base, stop := startServer(t, soulRBAC())
	defer stop()

	tok := newValidTokenFor(t, "archon-alice", []string{"cluster-admin"})
	req, _ := http.NewRequest(http.MethodPost, base+"/v1/souls/ghost.example.com/issue-token", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 404, body=%s", resp.StatusCode, raw)
	}
}

func TestIntegration_Soul_IssueToken_SSH_422(t *testing.T) {
	truncateOperators(t)
	seedOperator(t, "archon-alice", "")
	seedSoul(t, "ssh-01.example.com", "ssh", "archon-alice")

	base, stop := startServer(t, soulRBAC())
	defer stop()

	tok := newValidTokenFor(t, "archon-alice", []string{"cluster-admin"})
	req, _ := http.NewRequest(http.MethodPost, base+"/v1/souls/ssh-01.example.com/issue-token", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 422, body=%s", resp.StatusCode, raw)
	}
}

func TestIntegration_Soul_IssueToken_ActiveToken_409(t *testing.T) {
	truncateOperators(t)
	seedOperator(t, "archon-alice", "")
	seedSoul(t, "web-01.example.com", "agent", "archon-alice")
	seedActiveToken(t, "web-01.example.com", "archon-alice")

	base, stop := startServer(t, soulRBAC())
	defer stop()

	tok := newValidTokenFor(t, "archon-alice", []string{"cluster-admin"})
	req, _ := http.NewRequest(http.MethodPost, base+"/v1/souls/web-01.example.com/issue-token", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 409, body=%s", resp.StatusCode, raw)
	}
	var p problem.Details
	if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if p.Type != problem.TypeBootstrapTokenActive {
		t.Errorf("Type = %q, want %q", p.Type, problem.TypeBootstrapTokenActive)
	}
}

func TestIntegration_Soul_IssueToken_Force_200(t *testing.T) {
	truncateOperators(t)
	seedOperator(t, "archon-alice", "")
	seedSoul(t, "web-01.example.com", "agent", "archon-alice")
	oldTokenID := seedActiveToken(t, "web-01.example.com", "archon-alice")

	base, stop := startServer(t, soulRBAC())
	defer stop()

	tok := newValidTokenFor(t, "archon-alice", []string{"cluster-admin"})
	req, _ := http.NewRequest(http.MethodPost,
		base+"/v1/souls/web-01.example.com/issue-token?force=true", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200, body=%s", resp.StatusCode, raw)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if tokStr, _ := out["bootstrap_token"].(string); tokStr == "" {
		t.Errorf("bootstrap_token missing in force-reissue: %v", out)
	}

	// The old token is invalidated (used_at IS NOT NULL), the new one — active.
	var oldUsed bool
	if err := integrationPool.QueryRow(context.Background(),
		`SELECT used_at IS NOT NULL FROM bootstrap_tokens WHERE token_id=$1`, oldTokenID).
		Scan(&oldUsed); err != nil {
		t.Fatalf("old token probe: %v", err)
	}
	if !oldUsed {
		t.Errorf("old token should be invalidated (used_at set) after force-reissue")
	}
	var activeCount int64
	if err := integrationPool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM bootstrap_tokens WHERE sid='web-01.example.com' AND used_at IS NULL`).
		Scan(&activeCount); err != nil {
		t.Fatalf("active count: %v", err)
	}
	if activeCount != 1 {
		t.Errorf("active token count after force = %d, want 1", activeCount)
	}

	// Audit: soul.token-issued written, force=true, expired_previous=true.
	// Token identifiers are absent from the payload (secret-mask H1).
	var auditCount int64
	var force, expiredPrev bool
	if err := integrationPool.QueryRow(context.Background(),
		`SELECT COUNT(*),
		        bool_or((payload->>'force')::bool),
		        bool_or((payload->>'expired_previous')::bool)
		   FROM audit_log WHERE event_type='soul.token-issued'`).
		Scan(&auditCount, &force, &expiredPrev); err != nil {
		t.Fatalf("audit query: %v", err)
	}
	if auditCount != 1 || !force || !expiredPrev {
		t.Errorf("audit = count:%d force:%v expired_previous:%v (want 1/true/true)", auditCount, force, expiredPrev)
	}
}

// TestIntegration_Soul_Create_UnknownCreator_422 (B2): a JWT with an AID that is not
// in the operators registry, but with an RBAC role granting soul.create. An FK violation on
// souls_created_by_aid_fk must map to a clean 422, not an opaque 500.
func TestIntegration_Soul_Create_UnknownCreator_422(t *testing.T) {
	truncateOperators(t)
	// archon-alice is deliberately NOT seeded in operators — only in the RBAC config.

	rbac := &rbactest.Config{
		Roles: []rbactest.Role{
			{Name: "creator", Operators: []string{"archon-alice"}, Permissions: []string{"soul.create"}},
		},
	}
	base, stop := startServer(t, rbac)
	defer stop()

	tok := newValidTokenFor(t, "archon-alice", []string{"creator"})
	body := bytesReader(`{"sid":"web-01.example.com","transport":"agent"}`)
	req, _ := http.NewRequest(http.MethodPost, base+"/v1/souls", body)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 422 (FK on creator AID), body=%s", resp.StatusCode, raw)
	}
	var p problem.Details
	if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if p.Type != problem.TypeValidationFailed {
		t.Errorf("Type = %q, want %q", p.Type, problem.TypeValidationFailed)
	}

	// No orphaned souls row must remain (the insert failed on the FK).
	var soulCount int64
	if err := integrationPool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM souls WHERE sid='web-01.example.com'`).Scan(&soulCount); err != nil {
		t.Fatalf("soul count: %v", err)
	}
	if soulCount != 0 {
		t.Errorf("orphan souls-row count = %d, want 0", soulCount)
	}
}

// TestIntegration_Soul_IssueToken_ForceNoActive_200 (coverage gap 1): force=true
// when there is no active token — the recovery scenario from onboarding.md. Must return
// 200 + a new token (ExpireActiveBySID → no-op, Insert succeeds).
func TestIntegration_Soul_IssueToken_ForceNoActive_200(t *testing.T) {
	truncateOperators(t)
	seedOperator(t, "archon-alice", "")
	seedSoul(t, "web-01.example.com", "agent", "archon-alice")
	// No active token is seeded — force-recovery on a bare pending Soul.

	base, stop := startServer(t, soulRBAC())
	defer stop()

	tok := newValidTokenFor(t, "archon-alice", []string{"cluster-admin"})
	req, _ := http.NewRequest(http.MethodPost,
		base+"/v1/souls/web-01.example.com/issue-token?force=true", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200, body=%s", resp.StatusCode, raw)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if tokStr, _ := out["bootstrap_token"].(string); tokStr == "" {
		t.Errorf("bootstrap_token missing in force-no-active reissue: %v", out)
	}

	var activeCount int64
	if err := integrationPool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM bootstrap_tokens WHERE sid='web-01.example.com' AND used_at IS NULL`).
		Scan(&activeCount); err != nil {
		t.Fatalf("active count: %v", err)
	}
	if activeCount != 1 {
		t.Errorf("active token count = %d, want 1", activeCount)
	}

	// expired_previous=false (there was nothing to expire).
	var expiredPrev bool
	if err := integrationPool.QueryRow(context.Background(),
		`SELECT bool_or((payload->>'expired_previous')::bool)
		   FROM audit_log WHERE event_type='soul.token-issued'`).Scan(&expiredPrev); err != nil {
		t.Fatalf("audit query: %v", err)
	}
	if expiredPrev {
		t.Errorf("expired_previous = true, want false (no active token existed)")
	}
}

// TestIntegration_Soul_IssueToken_Concurrent (coverage gap 2): two concurrent
// issue-token calls on one SID without force → exactly 1 success (200) + 1 refusal (409
// bootstrap-token-active), exactly 1 active token in the DB. Protection — a partial unique
// UNIQUE(sid) WHERE used_at IS NULL.
func TestIntegration_Soul_IssueToken_Concurrent(t *testing.T) {
	truncateOperators(t)
	seedOperator(t, "archon-alice", "")
	seedSoul(t, "web-01.example.com", "agent", "archon-alice")
	// Without a prior active token: a race of two clean issuances.

	base, stop := startServer(t, soulRBAC())
	defer stop()

	tok := newValidTokenFor(t, "archon-alice", []string{"cluster-admin"})

	const n = 2
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		codes   []int
		startCh = make(chan struct{})
	)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, _ := http.NewRequest(http.MethodPost,
				base+"/v1/souls/web-01.example.com/issue-token", nil)
			req.Header.Set("Authorization", "Bearer "+tok)
			<-startCh
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				mu.Lock()
				codes = append(codes, -1)
				mu.Unlock()
				return
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			mu.Lock()
			codes = append(codes, resp.StatusCode)
			mu.Unlock()
		}()
	}
	close(startCh)
	wg.Wait()

	var ok, conflict int
	for _, c := range codes {
		switch c {
		case http.StatusOK:
			ok++
		case http.StatusConflict:
			conflict++
		default:
			t.Errorf("unexpected status %d", c)
		}
	}
	if ok != 1 || conflict != 1 {
		t.Errorf("got %d×200 + %d×409, want exactly 1 each (codes=%v)", ok, conflict, codes)
	}

	var activeCount int64
	if err := integrationPool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM bootstrap_tokens WHERE sid='web-01.example.com' AND used_at IS NULL`).
		Scan(&activeCount); err != nil {
		t.Fatalf("active count: %v", err)
	}
	if activeCount != 1 {
		t.Errorf("active token count = %d, want exactly 1 (partial unique guard)", activeCount)
	}
}

func TestIntegration_Soul_List_200_Filters(t *testing.T) {
	truncateOperators(t)
	seedOperator(t, "archon-alice", "")
	// Three Souls: agent/connected coven=[redis-prod], agent/pending coven=[redis-prod,cache],
	// ssh/pending coven=[edge]. Cover the coven / status / transport filters.
	seedSoulFull(t, "redis-01.example.com", "agent", soul.StatusConnected, []string{"redis-prod"}, "archon-alice")
	seedSoulFull(t, "redis-02.example.com", "agent", soul.StatusPending, []string{"redis-prod", "cache"}, "archon-alice")
	seedSoulFull(t, "edge-01.example.com", "ssh", soul.StatusPending, []string{"edge"}, "archon-alice")

	base, stop := startServer(t, soulRBAC())
	defer stop()
	tok := newValidTokenFor(t, "archon-alice", []string{"cluster-admin"})

	// Without a filter — 3.
	if total, items := listSouls(t, base, tok, ""); total != 3 || items != 3 {
		t.Errorf("no filter: total=%d items=%d, want 3/3", total, items)
	}

	// GUARD contract-wire: the soul-list items[] element carries snake_case keys
	// (sid/last_seen_at/registered_at), NOT the PascalCase of the domain SoulListView.
	// Positive — the key is present; negative — the PascalCase twin is ABSENT.
	// Regression: the list Body was serialized via the untagged SoulListView →
	// SID/LastSeenAt/RegisteredAt keys (contract bug #7).
	first := firstSoulItem(t, base, tok)
	for _, key := range []string{"sid", "last_seen_at", "registered_at"} {
		if _, ok := first[key]; !ok {
			t.Errorf("items[0] missing snake_case ключ %q: %v", key, first)
		}
	}
	for _, key := range []string{"SID", "LastSeenAt", "RegisteredAt"} {
		if _, ok := first[key]; ok {
			t.Fatalf("items[0] несёт PascalCase ключ %q (контракт-wire сломан): %v", key, first)
		}
	}
	// coven=redis-prod → 2.
	if total, items := listSouls(t, base, tok, "coven=redis-prod"); total != 2 || items != 2 {
		t.Errorf("coven filter: total=%d items=%d, want 2/2", total, items)
	}
	// status=pending → 2.
	if total, items := listSouls(t, base, tok, "status=pending"); total != 2 || items != 2 {
		t.Errorf("status filter: total=%d items=%d, want 2/2", total, items)
	}
	// transport=ssh → 1.
	if total, items := listSouls(t, base, tok, "transport=ssh"); total != 1 || items != 1 {
		t.Errorf("transport filter: total=%d items=%d, want 1/1", total, items)
	}
	// Combination: agent + pending + coven=cache → 1 (redis-02).
	if total, items := listSouls(t, base, tok, "transport=agent&status=pending&coven=cache"); total != 1 || items != 1 {
		t.Errorf("combined filter: total=%d items=%d, want 1/1", total, items)
	}
}

func TestIntegration_Soul_List_Empty_200(t *testing.T) {
	truncateOperators(t)
	seedOperator(t, "archon-alice", "")

	base, stop := startServer(t, soulRBAC())
	defer stop()
	tok := newValidTokenFor(t, "archon-alice", []string{"cluster-admin"})

	req, _ := http.NewRequest(http.MethodGet, base+"/v1/souls", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200, body=%s", resp.StatusCode, raw)
	}
	var out struct {
		Items []map[string]any `json:"items"`
		Total int              `json:"total"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Total != 0 || len(out.Items) != 0 {
		t.Errorf("empty list: total=%d items=%d, want 0/0", out.Total, len(out.Items))
	}
}

// TestIntegration_Soul_List_NoSecretsLeak — the list response must not return
// fingerprint / token_hash and other secrets (the DTO does not declare them; we check
// via raw-map keys).
func TestIntegration_Soul_List_NoSecretsLeak(t *testing.T) {
	truncateOperators(t)
	seedOperator(t, "archon-alice", "")
	seedSoulFull(t, "web-01.example.com", "agent", soul.StatusConnected, []string{"prod"}, "archon-alice")

	base, stop := startServer(t, soulRBAC())
	defer stop()
	tok := newValidTokenFor(t, "archon-alice", []string{"cluster-admin"})

	req, _ := http.NewRequest(http.MethodGet, base+"/v1/souls", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200, body=%s", resp.StatusCode, raw)
	}
	var out struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Items) != 1 {
		t.Fatalf("items len = %d, want 1", len(out.Items))
	}
	for _, leak := range []string{"fingerprint", "soul_seed", "token_hash", "bootstrap_token"} {
		if _, present := out.Items[0][leak]; present {
			t.Errorf("list item leaked sensitive key %q: %v", leak, out.Items[0])
		}
	}
}

func TestIntegration_Soul_List_403_NoPermission(t *testing.T) {
	truncateOperators(t)
	seedOperator(t, "archon-nobody", "")

	// RBAC without soul.list for archon-nobody.
	rbacCfg := &rbactest.Config{
		Roles: []rbactest.Role{
			{Name: "no-list", Operators: []string{"archon-nobody"}, Permissions: []string{"soul.create"}},
		},
	}
	base, stop := startServer(t, rbacCfg)
	defer stop()

	tok := newValidTokenFor(t, "archon-nobody", []string{"no-list"})
	req, _ := http.NewRequest(http.MethodGet, base+"/v1/souls", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 403, body=%s", resp.StatusCode, raw)
	}
	if got := resp.Header.Get("Content-Type"); got != problem.ContentType {
		t.Errorf("Content-Type = %q", got)
	}
}

// --- ADR-047 S3b-2a: keyset mode of souls-list on a real PG (regex-scope) ---

// keysetPage — one HTTP page of the keyset mode of souls-list (next_cursor /
// total_approximate). We extract the SIDs to verify the walk's coverage.
type keysetPage struct {
	Items []struct {
		SID    string `json:"sid"`
		Status string `json:"status"`
	} `json:"items"`
	TotalApproximate bool    `json:"total_approximate"`
	NextCursor       *string `json:"next_cursor"`
}

// getSoulsPage — GET /v1/souls?<query> and decode into keysetPage. Fails on non-200.
func getSoulsPage(t *testing.T, base, tok, query string) keysetPage {
	t.Helper()
	url := base + "/v1/souls"
	if query != "" {
		url += "?" + query
	}
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("getSoulsPage(%q): Do: %v", query, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("getSoulsPage(%q): status = %d, body=%s", query, resp.StatusCode, raw)
	}
	var page keysetPage
	if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
		t.Fatalf("getSoulsPage(%q): decode: %v", query, err)
	}
	return page
}

// walkSouls — a full keyset walk of souls-list via the next_cursor round-trip,
// collects all SIDs; fails on a duplicate or on exceeding the page limit. baseQuery —
// filters/limit (without cursor).
func walkSouls(t *testing.T, base, tok, baseQuery string) map[string]struct{} {
	t.Helper()
	seen := map[string]struct{}{}
	cursor := ""
	for page := 0; ; page++ {
		q := baseQuery
		if cursor != "" {
			if q != "" {
				q += "&"
			}
			q += "cursor=" + cursor
		}
		p := getSoulsPage(t, base, tok, q)
		if !p.TotalApproximate {
			t.Errorf("keyset-страница %d: total_approximate=false, want true (regex-scope)", page)
		}
		for _, it := range p.Items {
			if _, dup := seen[it.SID]; dup {
				t.Fatalf("ДУБЛЬ %s при keyset-обходе через HTTP", it.SID)
			}
			seen[it.SID] = struct{}{}
		}
		if p.NextCursor == nil {
			break
		}
		cursor = *p.NextCursor
		if page > 50 {
			t.Fatal("keyset HTTP-обход не сходится (>50 страниц)")
		}
	}
	return seen
}

// regexScopeRBAC — a role with regex-scoped soul.list (`on regex=<pat>`): the operator
// sees only SIDs matching the pattern (keyset mode, ADR-047 S3b-2a).
func regexScopeRBAC(aid, pattern string) *rbactest.Config {
	return &rbactest.Config{
		Roles: []rbactest.Role{
			{Name: "web-ops", Operators: []string{aid}, Permissions: []string{"soul.list on regex='" + pattern + "'"}},
		},
	}
}

// covenAndRegexScopeRBAC — two soul.list permissions: coven=<coven> + regex=<pat>.
// Visibility = union (OR): a host in the coven OR matching the regex (keyset mode).
func covenAndRegexScopeRBAC(aid, coven, pattern string) *rbactest.Config {
	return &rbactest.Config{
		Roles: []rbactest.Role{
			{Name: "mixed-ops", Operators: []string{aid}, Permissions: []string{
				"soul.list on coven=" + coven,
				"soul.list on regex='" + pattern + "'",
			}},
		},
	}
}

// TestIntegration_Soul_List_Keyset_RegexScope — the keyset HTTP path on a real PG:
// regex-scope `^web-`, souls web-*/db-* → the walk via next_cursor collects EXACTLY
// web-* (no duplicates/gaps), total_approximate:true, top-up works (limit
// less than the number of web hosts → a multi-page walk).
func TestIntegration_Soul_List_Keyset_RegexScope(t *testing.T) {
	// ADR-047 G1: the route-gate is switched to RequireAction (existence-gate) —
	// a regex-scoped operator reaches the handler (previously rbac.Check(nil-ctx)
	// denied scoped soul.list BEFORE the handler). Reachable through the REAL route-gate.
	truncateOperators(t)
	seedOperator(t, "archon-webops", "")
	// 3 web-* (visible) + 2 db-* (out of scope). Different registered_at —
	// seedSoulFull sets NOW(); the insertion order determines the DESC order.
	for _, s := range []string{"web-01.example.com", "web-02.example.com", "web-03.example.com"} {
		seedSoulFull(t, s, "agent", soul.StatusConnected, []string{"prod"}, "archon-webops")
		time.Sleep(2 * time.Millisecond)
	}
	for _, s := range []string{"db-01.example.com", "db-02.example.com"} {
		seedSoulFull(t, s, "agent", soul.StatusConnected, []string{"prod"}, "archon-webops")
		time.Sleep(2 * time.Millisecond)
	}

	base, stop := startServer(t, regexScopeRBAC("archon-webops", "^web-"))
	defer stop()
	tok := newValidTokenFor(t, "archon-webops", []string{"web-ops"})

	// limit=2 < 3 web hosts → a multi-page walk with top-up.
	got := walkSouls(t, base, tok, "limit=2")
	want := map[string]struct{}{
		"web-01.example.com": {}, "web-02.example.com": {}, "web-03.example.com": {},
	}
	if len(got) != len(want) {
		t.Fatalf("keyset regex-scope собрал %d хостов, want %d: %v", len(got), len(want), got)
	}
	for sid := range want {
		if _, ok := got[sid]; !ok {
			t.Errorf("web-хост %s пропущен keyset-обходом", sid)
		}
	}
	for _, leak := range []string{"db-01.example.com", "db-02.example.com"} {
		if _, ok := got[leak]; ok {
			t.Errorf("db-хост %s виден regex-scoped оператору — утечка за границу Purview", leak)
		}
	}
}

// TestIntegration_Soul_List_Keyset_CovenRegexUnion — a mixed scope coven=prod
// + regex=^db- on a real PG: visibility = union (OR). A host-in-prod-not-db and a
// host-db-not-prod are both visible on the page-by-page walk; a host-neither is hidden.
func TestIntegration_Soul_List_Keyset_CovenRegexUnion(t *testing.T) {
	// ADR-047 G1: the route-gate RequireAction (existence-gate) admits the scoped
	// operator to the handler; union OR + filter∩scope are computed in the handler.
	truncateOperators(t)
	seedOperator(t, "archon-mixed", "")
	// app-01: prod, not db → visible by coven. db-01: staging, db-* → visible by regex.
	// db-02: prod, db-* → visible by both. noise-01: staging, not db → hidden.
	seedSoulFull(t, "app-01.example.com", "agent", soul.StatusConnected, []string{"prod"}, "archon-mixed")
	time.Sleep(2 * time.Millisecond)
	seedSoulFull(t, "db-01.example.com", "agent", soul.StatusConnected, []string{"staging"}, "archon-mixed")
	time.Sleep(2 * time.Millisecond)
	seedSoulFull(t, "db-02.example.com", "agent", soul.StatusConnected, []string{"prod"}, "archon-mixed")
	time.Sleep(2 * time.Millisecond)
	seedSoulFull(t, "noise-01.example.com", "agent", soul.StatusConnected, []string{"staging"}, "archon-mixed")

	base, stop := startServer(t, covenAndRegexScopeRBAC("archon-mixed", "prod", "^db-"))
	defer stop()
	tok := newValidTokenFor(t, "archon-mixed", []string{"mixed-ops"})

	got := walkSouls(t, base, tok, "limit=2")
	for _, sid := range []string{"app-01.example.com", "db-01.example.com", "db-02.example.com"} {
		if _, ok := got[sid]; !ok {
			t.Errorf("union: %s скрыт (должен быть виден по coven ИЛИ regex)", sid)
		}
	}
	if _, ok := got["noise-01.example.com"]; ok {
		t.Error("union: noise-01 (ни prod, ни db-*) виден — over-show за границу Purview")
	}
	if len(got) != 3 {
		t.Fatalf("union собрал %d, want 3 (app-01, db-01, db-02): %v", len(got), got)
	}
}

// TestIntegration_Soul_List_Keyset_FilterIntersectsScope — a BLOCKER fix on
// a real PG: regex-scope `^web-` + `?status=connected` → ONLY the
// connected web hosts are visible (filter ∩ scope, AND). A pending web host in scope is hidden
// by the filter. Before the fix the keyset mode would have ignored the query filter.
func TestIntegration_Soul_List_Keyset_FilterIntersectsScope(t *testing.T) {
	// ADR-047 G1: the route-gate RequireAction (existence-gate) admits the regex-scoped
	// operator to the handler; filter∩scope (AND) is computed by the keyset eval.
	truncateOperators(t)
	seedOperator(t, "archon-webops", "")
	seedSoulFull(t, "web-01.example.com", "agent", soul.StatusConnected, []string{"prod"}, "archon-webops")
	time.Sleep(2 * time.Millisecond)
	seedSoulFull(t, "web-02.example.com", "agent", soul.StatusPending, []string{"prod"}, "archon-webops")
	time.Sleep(2 * time.Millisecond)
	seedSoulFull(t, "web-03.example.com", "agent", soul.StatusConnected, []string{"prod"}, "archon-webops")
	// a db-out-of-scope host with connected — must not leak through even under the filter.
	seedSoulFull(t, "db-01.example.com", "agent", soul.StatusConnected, []string{"prod"}, "archon-webops")

	base, stop := startServer(t, regexScopeRBAC("archon-webops", "^web-"))
	defer stop()
	tok := newValidTokenFor(t, "archon-webops", []string{"web-ops"})

	got := walkSouls(t, base, tok, "status=connected&limit=2")
	want := map[string]struct{}{"web-01.example.com": {}, "web-03.example.com": {}}
	if len(got) != len(want) {
		t.Fatalf("filter∩scope собрал %d, want 2 (connected web-*): %v", len(got), got)
	}
	for sid := range want {
		if _, ok := got[sid]; !ok {
			t.Errorf("connected web-хост %s пропущен", sid)
		}
	}
	if _, ok := got["web-02.example.com"]; ok {
		t.Error("pending web-02 виден под ?status=connected — фильтр НЕ применён в keyset-режиме (BLOCKER регресс)")
	}
	if _, ok := got["db-01.example.com"]; ok {
		t.Error("db-01 вне scope виден — утечка за Purview")
	}
}

// --- ADR-047 gate-fix: scoped operators reach the handler through the REAL
// route-gate (NoSelector), scope is derived from the JWT, not from the query. ---

// covenScopeRBAC — a role with coven-scoped soul.list (`on coven=<label>`): the operator
// sees only the hosts of its own coven. WITHOUT host/coven context in the query — the gate
// (NoSelector) admits, the handler narrows.
func covenScopeRBAC(aid, coven string) *rbactest.Config {
	return &rbactest.Config{
		Roles: []rbactest.Role{
			{Name: "coven-ops", Operators: []string{aid}, Permissions: []string{"soul.list on coven=" + coven}},
		},
	}
}

// getSoulStatus — GET /v1/souls/{sid}, returns the HTTP status (for the single-read
// scope gate: 200 visible / 404 out of scope).
func getSoulStatus(t *testing.T, base, tok, sid string) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, base+"/v1/souls/"+sid, nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("getSoulStatus(%s): Do: %v", sid, err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

// TestIntegration_Soul_List_CovenScope_NoQuery_200 — guard #1 (G1, the main
// win): a coven-scoped operator does GET /v1/souls WITHOUT ?coven= → 200 + only
// the hosts of its own coven (NOT 403). Previously the scope-aware gate (RequirePermission) denied
// coven-scoped without ?coven= (empty context → the selector did not match → 403). G1 —
// RequireAction existence-gate admits, the handler narrows. Regression = a scoped operator
// again 403 on its own list (scoped visibility unreachable via HTTP).
func TestIntegration_Soul_List_CovenScope_NoQuery_200(t *testing.T) {
	truncateOperators(t)
	seedOperator(t, "archon-coven", "")
	seedSoulFull(t, "prod-01.example.com", "agent", soul.StatusConnected, []string{"prod"}, "archon-coven")
	seedSoulFull(t, "prod-02.example.com", "agent", soul.StatusConnected, []string{"prod"}, "archon-coven")
	seedSoulFull(t, "stg-01.example.com", "agent", soul.StatusConnected, []string{"staging"}, "archon-coven")

	base, stop := startServer(t, covenScopeRBAC("archon-coven", "prod"))
	defer stop()
	tok := newValidTokenFor(t, "archon-coven", []string{"coven-ops"})

	// WITHOUT ?coven= — previously 403, now 200 + exactly 2 prod hosts (coven-pushdown).
	total, items := listSouls(t, base, tok, "")
	if total != 2 || items != 2 {
		t.Fatalf("coven-scoped list без ?coven=: total=%d items=%d, want 2/2 (только prod)", total, items)
	}
}

// TestIntegration_Soul_Get_CovenScope_InScope_200_OutOfScope_404 — guard #4/#6
// (gate-fix + list↔get): a coven-scoped operator reads a host of its own coven by
// a direct GET /{sid} → 200; a foreign coven → 404. list↔get are consistent.
func TestIntegration_Soul_Get_CovenScope(t *testing.T) {
	// ADR-047 G1: the RequireAction existence-gate admits coven-scoped to single-get;
	// the handler narrowing (readScope/InScope coven-match → 200/404) cuts visibility.
	truncateOperators(t)
	seedOperator(t, "archon-coven", "")
	seedSoulFull(t, "prod-01.example.com", "agent", soul.StatusConnected, []string{"prod"}, "archon-coven")
	seedSoulFull(t, "stg-01.example.com", "agent", soul.StatusConnected, []string{"staging"}, "archon-coven")

	base, stop := startServer(t, covenScopeRBAC("archon-coven", "prod"))
	defer stop()
	tok := newValidTokenFor(t, "archon-coven", []string{"coven-ops"})

	if code := getSoulStatus(t, base, tok, "prod-01.example.com"); code != http.StatusOK {
		t.Errorf("GET prod-01 (свой coven) = %d, want 200", code)
	}
	if code := getSoulStatus(t, base, tok, "stg-01.example.com"); code != http.StatusNotFound {
		t.Errorf("GET stg-01 (чужой coven) = %d, want 404 (не палит существование)", code)
	}
}

// TestIntegration_Soul_Get_RegexScope_ListGetConsistency — guard #3/#6 (gate-fix
// + InScope OR-regex): a regex-scoped operator. A host visible in List (regex-eval)
// is also reachable by a direct GET /{sid} (200, not 404 — the S3b-2a mismatch is fixed);
// a non-matching host → 404. This is the key list↔get consistency guard.
func TestIntegration_Soul_Get_RegexScope_ListGetConsistency(t *testing.T) {
	// ADR-047 G1: the RequireAction existence-gate admits regex-scoped both to list and
	// to single-get; the list↔get consistency (InScope OR-regex) is computed by the handler.
	truncateOperators(t)
	seedOperator(t, "archon-webops", "")
	seedSoulFull(t, "web-01.example.com", "agent", soul.StatusConnected, []string{"prod"}, "archon-webops")
	seedSoulFull(t, "db-01.example.com", "agent", soul.StatusConnected, []string{"prod"}, "archon-webops")

	base, stop := startServer(t, regexScopeRBAC("archon-webops", "^web-"))
	defer stop()
	tok := newValidTokenFor(t, "archon-webops", []string{"web-ops"})

	// web-01 is visible in the keyset List…
	seen := walkSouls(t, base, tok, "limit=10")
	if _, ok := seen["web-01.example.com"]; !ok {
		t.Fatal("web-01 не виден в regex-scoped List — предусловие consistency-теста нарушено")
	}
	// …and reachable by a direct GET (previously InScope coven-only → 404 on a visible host).
	if code := getSoulStatus(t, base, tok, "web-01.example.com"); code != http.StatusOK {
		t.Errorf("GET web-01 (виден в List по regex ^web-) = %d, want 200 (list↔get консистентны)", code)
	}
	// db-01 does NOT match the regex → 404 (outside Purview, does not reveal existence).
	if code := getSoulStatus(t, base, tok, "db-01.example.com"); code != http.StatusNotFound {
		t.Errorf("GET db-01 (не матчит ^web-) = %d, want 404", code)
	}
}

// TestIntegration_Soul_Get_403_NoPermission — guard #5 (security non-regression):
// an operator WITHOUT soul.list gets 403 on single-get too (the gate still requires
// holding the permission — NoSelector relaxed scope-in-query, NOT the requirement of the
// permission itself). Regression = reading the detail without the right.
func TestIntegration_Soul_Get_403_NoPermission(t *testing.T) {
	truncateOperators(t)
	seedOperator(t, "archon-nobody", "")
	seedSoulFull(t, "prod-01.example.com", "agent", soul.StatusConnected, []string{"prod"}, "archon-nobody")

	rbacCfg := &rbactest.Config{
		Roles: []rbactest.Role{
			{Name: "no-list", Operators: []string{"archon-nobody"}, Permissions: []string{"soul.create"}},
		},
	}
	base, stop := startServer(t, rbacCfg)
	defer stop()
	tok := newValidTokenFor(t, "archon-nobody", []string{"no-list"})

	if code := getSoulStatus(t, base, tok, "prod-01.example.com"); code != http.StatusForbidden {
		t.Fatalf("GET /{sid} без soul.list = %d, want 403 (gate требует permission)", code)
	}
}

// TestIntegration_Soul_Get_BareList_200 — guard #5 (bare soul.list not broken):
// an unrestricted operator (bare soul.list) sees any host via single-get.
func TestIntegration_Soul_Get_BareList_200(t *testing.T) {
	truncateOperators(t)
	seedOperator(t, "archon-viewer", "")
	seedSoulFull(t, "prod-01.example.com", "agent", soul.StatusConnected, []string{"prod"}, "archon-viewer")

	rbacCfg := &rbactest.Config{
		Roles: []rbactest.Role{
			{Name: "viewer", Operators: []string{"archon-viewer"}, Permissions: []string{"soul.list"}},
		},
	}
	base, stop := startServer(t, rbacCfg)
	defer stop()
	tok := newValidTokenFor(t, "archon-viewer", []string{"viewer"})

	if code := getSoulStatus(t, base, tok, "prod-01.example.com"); code != http.StatusOK {
		t.Fatalf("GET /{sid} с bare soul.list = %d, want 200 (unrestricted видит всё)", code)
	}
}

// getReadStatus — GET on an arbitrary read-souls path, returns the HTTP status.
// Used by the revoked/expired guards for all four read routes
// (list / {sid} / {sid}/soulprint / {sid}/history) via a single helper.
func getReadStatus(t *testing.T, base, tok, path string) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, base+path, nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("getReadStatus(%s): Do: %v", path, err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

// newExpiredTokenFor issues an ALREADY EXPIRED JWT — for the guard "the revoked-fix did
// not break the auth layer": an expired token must give 401 on read-souls (the auth layer BEFORE
// the RBAC gate), rather than slipping into revoked semantics. Issue does not allow a negative
// ttl, so we craft the claims directly with exp in the past (as in jwt/verifier_test).
func newExpiredTokenFor(t *testing.T, aid string, roles []string) string {
	t.Helper()
	claims := jwtv5.MapClaims{
		"iss":   integrationIssuer,
		"sub":   aid,
		"roles": roles,
		"iat":   time.Now().Add(-2 * time.Hour).Unix(),
		"exp":   time.Now().Add(-time.Hour).Unix(),
	}
	tok, err := jwtv5.NewWithClaims(jwtv5.SigningMethodHS256, claims).SignedString([]byte(integrationSigningKey))
	if err != nil {
		t.Fatalf("sign expired token: %v", err)
	}
	return tok
}

// TestIntegration_Soul_Read_Revoked_403 — guard (ADR-047 G1 Fix 2): a revoked
// Archon WITH AN ACTIVE soul.list role does NOT see the souls. ResolvePurview → Deny cuts
// it off at a single point — the route-gate [RequireAction] (HoldsAction→Deny→false→403),
// which sits BEFORE the handler and catches ALL four read routes uniformly:
// list / {sid} / soulprint / history → 403. The handler resolvers (readScope→Empty→
// InScope false → 404) remain an unreachable backstop when revoked — the gate fires
// earlier; the coven/regex-scope tests below cover the 404 branch from scope.
// 403 vs 404 here is not a leak: both = "no access", 403 is even stricter (does not distinguish
// existence). Control: the same role WITHOUT revoked sees the host (NOT 403).
func TestIntegration_Soul_Read_Revoked_403(t *testing.T) {
	truncateOperators(t)
	seedOperator(t, "archon-fired", "")
	seedSoulFull(t, "prod-01.example.com", "agent", soul.StatusConnected, []string{"prod"}, "archon-fired")

	// Control: a non-revoked operator with the same bare soul.list sees the host.
	notRevoked := &rbactest.Config{
		Roles: []rbactest.Role{
			{Name: "viewer", Operators: []string{"archon-fired"}, Permissions: []string{"soul.list"}},
		},
	}
	baseOK, stopOK := startServer(t, notRevoked)
	tokOK := newValidTokenFor(t, "archon-fired", []string{"viewer"})
	if code := getReadStatus(t, baseOK, tokOK, "/v1/souls"); code != http.StatusOK {
		t.Errorf("контроль (НЕ revoked) GET /v1/souls = %d, want 200", code)
	}
	if code := getReadStatus(t, baseOK, tokOK, "/v1/souls/prod-01.example.com"); code != http.StatusOK {
		t.Errorf("контроль (НЕ revoked) GET /{sid} = %d, want 200", code)
	}
	stopOK()

	// Revoked: an active soul.list role, but revoked_at is set.
	revokedCfg := &rbactest.Config{
		Roles: []rbactest.Role{
			{Name: "viewer", Operators: []string{"archon-fired"}, Permissions: []string{"soul.list"}},
		},
		Revoked: map[string]time.Time{"archon-fired": time.Now()},
	}
	base, stop := startServer(t, revokedCfg)
	defer stop()
	tok := newValidTokenFor(t, "archon-fired", []string{"viewer"})

	for _, path := range []string{
		"/v1/souls",
		"/v1/souls/prod-01.example.com",
		"/v1/souls/prod-01.example.com/soulprint",
		"/v1/souls/prod-01.example.com/history",
	} {
		if code := getReadStatus(t, base, tok, path); code != http.StatusForbidden {
			t.Errorf("revoked GET %s = %d, want 403 (gate HoldsAction→Deny→false, не данные/факты/timeline)", path, code)
		}
	}
}

// TestIntegration_Soul_Read_Expired_401 — guard (ADR-047 G1): the revoked-fix did NOT
// break the auth layer. An expired JWT gives 401 on all four read routes BEFORE
// the RBAC gate (expired ≠ revoked: 401 from RequireJWT, not 403/404 from scope).
func TestIntegration_Soul_Read_Expired_401(t *testing.T) {
	truncateOperators(t)
	seedOperator(t, "archon-viewer", "")
	seedSoulFull(t, "prod-01.example.com", "agent", soul.StatusConnected, []string{"prod"}, "archon-viewer")

	rbacCfg := &rbactest.Config{
		Roles: []rbactest.Role{
			{Name: "viewer", Operators: []string{"archon-viewer"}, Permissions: []string{"soul.list"}},
		},
	}
	base, stop := startServer(t, rbacCfg)
	defer stop()
	tok := newExpiredTokenFor(t, "archon-viewer", []string{"viewer"})

	for _, path := range []string{
		"/v1/souls",
		"/v1/souls/prod-01.example.com",
		"/v1/souls/prod-01.example.com/soulprint",
		"/v1/souls/prod-01.example.com/history",
	} {
		if code := getReadStatus(t, base, tok, path); code != http.StatusUnauthorized {
			t.Errorf("expired JWT GET %s = %d, want 401 (auth-слой не сломан revoked-фиксом)", path, code)
		}
	}
}

// postCovenAssign — POST /v1/souls/coven with the given body, returns the
// HTTP status. Used by the coven-assign-revoked guard (Gap 2). dry_run
// in the body makes the request non-mutating (CountBulkMatched without UPDATE) — the happy-path
// 200 does not depend on the presence of hosts.
func postCovenAssign(t *testing.T, base, tok, body string) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, base+"/v1/souls/coven", bytesReader(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("postCovenAssign: Do: %v", err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

// TestIntegration_Soul_CovenAssign_Revoked_401 — guard (ADR-047 G2, Gap 2): a
// revoked Archon WITH AN ACTIVE soul.coven-assign role can NOT bulk-change
// Coven labels. `POST /v1/souls/coven` — the second consumer of revoked-aware
// ResolvePurview (via handler scope-intersection), but the first to fire is
// route-gate RequirePermission → scope-aware Check → ErrOperatorRevoked → 401
// (TypeOperatorRevokedToken, StatusUnauthorized), BEFORE the handler. The service-layer
// ResolvePurview→Deny — an unreachable fail-closed backstop. This test pins the
// actual correct behavior (401, NOT 403/200), so that a regression of the
// revoked-shortcut in Check/ResolvePurview does not silently open an escalation. Control:
// the same role WITHOUT revoked passes the gate and handler → 200 (dry_run).
//
// 401 here (not 403 as on read-souls): the mutate route goes through the scope-aware
// Check (revoked → ErrOperatorRevoked → 401, parity with an expired JWT), whereas
// the read routes — through the existence-gate RequireAction (revoked → HoldsAction
// false → 403). Different status — different gate mechanisms, both fail-closed.
func TestIntegration_Soul_CovenAssign_Revoked_401(t *testing.T) {
	truncateOperators(t)
	seedOperator(t, "archon-fired", "")

	// append a single label under selector all=true; dry_run → non-mutating
	// CountBulkMatched, happy-path 200 with no dependence on the presence of hosts.
	body := `{"mode":"append","label":"prod","selector":{"all":true},"dry_run":true}`

	// Control: a non-revoked operator with an active soul.coven-assign role passes the gate +
	// handler → 200.
	notRevoked := &rbactest.Config{
		Roles: []rbactest.Role{
			{Name: "coven-op", Operators: []string{"archon-fired"}, Permissions: []string{"soul.coven-assign"}},
		},
	}
	baseOK, stopOK := startServer(t, notRevoked)
	tokOK := newValidTokenFor(t, "archon-fired", []string{"coven-op"})
	if code := postCovenAssign(t, baseOK, tokOK, body); code != http.StatusOK {
		t.Errorf("контроль (НЕ revoked) POST /v1/souls/coven = %d, want 200", code)
	}
	stopOK()

	// Revoked: the same active soul.coven-assign role, but revoked_at is set.
	revokedCfg := &rbactest.Config{
		Roles: []rbactest.Role{
			{Name: "coven-op", Operators: []string{"archon-fired"}, Permissions: []string{"soul.coven-assign"}},
		},
		Revoked: map[string]time.Time{"archon-fired": time.Now()},
	}
	base, stop := startServer(t, revokedCfg)
	defer stop()
	tok := newValidTokenFor(t, "archon-fired", []string{"coven-op"})

	if code := postCovenAssign(t, base, tok, body); code != http.StatusUnauthorized {
		t.Errorf("revoked POST /v1/souls/coven = %d, want 401 (gate Check→ErrOperatorRevoked, fail-closed)", code)
	}
}

// --- ADR-047 §d (S3b-G2): incarnations read-gate through the REAL router ---
//
// Rollout of the souls-G1 pattern to incarnations: the read routes (list/get/history)
// are switched from scope-aware RequirePermission(Multi) to existence-only
// RequireAction; scope narrowing — the handler (resolveListScope / getInScope).
// These tests hit the FULL router (route-gate + handler), not the handler directly —
// the hole appeared exactly through the route-gate (a scoped operator caught 403/deny BEFORE
// the handler; the unit tests on doIncList/h.History did not see it).

// seedIncarnationFull inserts an incarnation with arbitrary covens + state (for the
// coven/state-scoped read-gate tests). seedIncarnation is kept minimal.
func seedIncarnationFull(t *testing.T, name, service, creator string, covens []string, state map[string]any) {
	t.Helper()
	c := creator
	inc := &incarnation.Incarnation{
		Name:               name,
		Service:            service,
		ServiceVersion:     "v1",
		StateSchemaVersion: 1,
		Status:             incarnation.StatusReady,
		Covens:             covens,
		State:              state,
		CreatedByAID:       &c,
	}
	if err := incarnation.Create(context.Background(), integrationPool, inc); err != nil {
		t.Fatalf("seedIncarnationFull(%s): %v", name, err)
	}
}

// incCovenScopeRBAC — a role with coven-scoped incarnation read rights (list+get+
// history `on coven=<label>`).
func incCovenScopeRBAC(aid, coven string) *rbactest.Config {
	return &rbactest.Config{
		Roles: []rbactest.Role{
			{Name: "inc-coven-ops", Operators: []string{aid}, Permissions: []string{
				"incarnation.list on coven=" + coven,
				"incarnation.get on coven=" + coven,
				"incarnation.history on coven=" + coven,
			}},
		},
	}
}

// incStateScopeRBAC — a role with state-scoped incarnation read rights (CEL over
// incarnation.state). This dimension does NOT resolve in the route-gate request context
// (state comes only from the DB row), so before Fix 1/2 the scope-aware gate
// denied such an operator with 403 BEFORE the handler — the main latent hole of G2.
func incStateScopeRBAC(aid, expr string) *rbactest.Config {
	return &rbactest.Config{
		Roles: []rbactest.Role{
			{Name: "inc-state-ops", Operators: []string{aid}, Permissions: []string{
				"incarnation.list on state='" + expr + "'",
				"incarnation.get on state='" + expr + "'",
				"incarnation.history on state='" + expr + "'",
			}},
		},
	}
}

// listIncarnations — GET /v1/incarnations?<query> → (total, len(items)). Fails
// on non-200.
func listIncarnations(t *testing.T, base, tok, query string) (total, items int) {
	t.Helper()
	url := base + "/v1/incarnations"
	if query != "" {
		url += "?" + query
	}
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("listIncarnations(%q): Do: %v", query, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("listIncarnations(%q): status = %d, body=%s", query, resp.StatusCode, raw)
	}
	var out struct {
		Items []map[string]any `json:"items"`
		Total int              `json:"total"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("listIncarnations(%q): decode: %v", query, err)
	}
	return out.Total, len(out.Items)
}

// TestIntegration_Incarnation_List_StateScope_NoContext_200 — the MAIN G2 win
// (Fix 1): a state-scoped operator does GET /v1/incarnations WITHOUT extra context →
// 200 + only the incarnations of its own state-scope (NOT 403). Before Fix 1 the route-gate
// RequirePermission(NoSelector) → Check(aid,incarnation,list,nil) → the state
// dimension fail-closed → deny → 403 BEFORE the handler. Regression = a scoped operator
// again invisible via HTTP.
func TestIntegration_Incarnation_List_StateScope_NoContext_200(t *testing.T) {
	truncateOperators(t)
	seedOperator(t, "archon-state", "")
	// redis-8: state.redis_version=8.0 (in scope). redis-7: 7.2 (out of scope).
	seedIncarnationFull(t, "redis-8", "redis", "archon-state", []string{"prod"}, map[string]any{"redis_version": "8.0"})
	seedIncarnationFull(t, "redis-7", "redis", "archon-state", []string{"prod"}, map[string]any{"redis_version": "7.2"})

	base, stop := startServer(t, incStateScopeRBAC("archon-state", `state.redis_version == "8.0"`))
	defer stop()
	tok := newValidTokenFor(t, "archon-state", []string{"inc-state-ops"})

	total, items := listIncarnations(t, base, tok, "")
	if total != 1 || items != 1 {
		t.Fatalf("state-scoped list без контекста: total=%d items=%d, want 1/1 (только redis-8)", total, items)
	}
}

// TestIntegration_Incarnation_List_CovenScope_NoContext_200 — coven-scoped via
// HTTP: the existence-only route-gate admits, the handler narrows via coven-pushdown → 200
// + only the prod incarnation (coven-scoped was also previously denied by the NoSelector gate
// on an empty context, like state).
func TestIntegration_Incarnation_List_CovenScope_NoContext_200(t *testing.T) {
	truncateOperators(t)
	seedOperator(t, "archon-coven", "")
	seedIncarnationFull(t, "prod-a", "redis", "archon-coven", []string{"prod"}, nil)
	seedIncarnationFull(t, "prod-b", "redis", "archon-coven", []string{"prod"}, nil)
	seedIncarnationFull(t, "stg-c", "redis", "archon-coven", []string{"staging"}, nil)

	base, stop := startServer(t, incCovenScopeRBAC("archon-coven", "prod"))
	defer stop()
	tok := newValidTokenFor(t, "archon-coven", []string{"inc-coven-ops"})

	total, items := listIncarnations(t, base, tok, "")
	if total != 2 || items != 2 {
		t.Fatalf("coven-scoped list без ?coven=: total=%d items=%d, want 2/2 (только prod)", total, items)
	}
}

// getIncStatus — GET /v1/incarnations/{name} → HTTP status.
func getIncStatus(t *testing.T, base, tok, name string) int {
	t.Helper()
	return getReadStatus(t, base, tok, "/v1/incarnations/"+name)
}

// historyIncStatus — GET /v1/incarnations/{name}/history → HTTP status.
func historyIncStatus(t *testing.T, base, tok, name string) int {
	t.Helper()
	return getReadStatus(t, base, tok, "/v1/incarnations/"+name+"/history")
}

// TestIntegration_Incarnation_Get_StateScope — Fix 2: a state-scoped operator
// reads get/history of a matching incarnation → 200; out of state-scope → 404. Before Fix 2
// the route-gate RequirePermissionMulti(incScope) did NOT carry the state dimension → deny → 403
// for an operator who SHOULD see the incarnation. Now the existence-gate +
// getInScope(state-CEL).
func TestIntegration_Incarnation_Get_StateScope(t *testing.T) {
	truncateOperators(t)
	seedOperator(t, "archon-state", "")
	seedIncarnationFull(t, "redis-8", "redis", "archon-state", []string{"prod"}, map[string]any{"redis_version": "8.0"})
	seedIncarnationFull(t, "redis-7", "redis", "archon-state", []string{"prod"}, map[string]any{"redis_version": "7.2"})

	base, stop := startServer(t, incStateScopeRBAC("archon-state", `state.redis_version == "8.0"`))
	defer stop()
	tok := newValidTokenFor(t, "archon-state", []string{"inc-state-ops"})

	if code := getIncStatus(t, base, tok, "redis-8"); code != http.StatusOK {
		t.Errorf("GET redis-8 (state в scope) = %d, want 200 (state-scoped видит get)", code)
	}
	if code := getIncStatus(t, base, tok, "redis-7"); code != http.StatusNotFound {
		t.Errorf("GET redis-7 (state вне scope) = %d, want 404", code)
	}
	if code := historyIncStatus(t, base, tok, "redis-8"); code != http.StatusOK {
		t.Errorf("history redis-8 (state в scope) = %d, want 200 (state-scoped видит history)", code)
	}
	if code := historyIncStatus(t, base, tok, "redis-7"); code != http.StatusNotFound {
		t.Errorf("history redis-7 (state вне scope) = %d, want 404", code)
	}
}

// TestIntegration_Incarnation_Get_CovenScope — coven-scoped get/history: own
// coven → 200, foreign → 404 (parity with souls Get_CovenScope; through the full router).
func TestIntegration_Incarnation_Get_CovenScope(t *testing.T) {
	truncateOperators(t)
	seedOperator(t, "archon-coven", "")
	seedIncarnationFull(t, "prod-a", "redis", "archon-coven", []string{"prod"}, nil)
	seedIncarnationFull(t, "stg-c", "redis", "archon-coven", []string{"staging"}, nil)

	base, stop := startServer(t, incCovenScopeRBAC("archon-coven", "prod"))
	defer stop()
	tok := newValidTokenFor(t, "archon-coven", []string{"inc-coven-ops"})

	if code := getIncStatus(t, base, tok, "prod-a"); code != http.StatusOK {
		t.Errorf("GET prod-a (свой coven) = %d, want 200", code)
	}
	if code := getIncStatus(t, base, tok, "stg-c"); code != http.StatusNotFound {
		t.Errorf("GET stg-c (чужой coven) = %d, want 404 (не палим существование)", code)
	}
	if code := historyIncStatus(t, base, tok, "prod-a"); code != http.StatusOK {
		t.Errorf("history prod-a (свой coven) = %d, want 200", code)
	}
	if code := historyIncStatus(t, base, tok, "stg-c"); code != http.StatusNotFound {
		t.Errorf("history stg-c (чужой coven) = %d, want 404", code)
	}
}

// TestIntegration_Incarnation_Read_Revoked — Fix 3 (revoked coverage): a revoked
// Archon WITH AN ACTIVE incarnation-read role does NOT see the souls. The single revoked-aware
// point ResolvePurview→Deny cuts off on all paths: the route-gate (HoldsAction→Deny→
// false→403 for list/get/history) BEFORE the handler. 403 on list, 403/404 on
// get/history — all = "no access". Control: the same role WITHOUT revoked sees.
func TestIntegration_Incarnation_Read_Revoked(t *testing.T) {
	truncateOperators(t)
	seedOperator(t, "archon-fired", "")
	seedIncarnationFull(t, "redis-prod", "redis", "archon-fired", []string{"prod"}, nil)

	// Control: a non-revoked operator with bare incarnation-read sees.
	notRevoked := &rbactest.Config{
		Roles: []rbactest.Role{
			{Name: "inc-viewer", Operators: []string{"archon-fired"}, Permissions: []string{
				"incarnation.list", "incarnation.get", "incarnation.history",
			}},
		},
	}
	baseOK, stopOK := startServer(t, notRevoked)
	tokOK := newValidTokenFor(t, "archon-fired", []string{"inc-viewer"})
	if code := getReadStatus(t, baseOK, tokOK, "/v1/incarnations"); code != http.StatusOK {
		t.Errorf("контроль (НЕ revoked) GET /v1/incarnations = %d, want 200", code)
	}
	if code := getIncStatus(t, baseOK, tokOK, "redis-prod"); code != http.StatusOK {
		t.Errorf("контроль (НЕ revoked) GET /{name} = %d, want 200", code)
	}
	if code := historyIncStatus(t, baseOK, tokOK, "redis-prod"); code != http.StatusOK {
		t.Errorf("контроль (НЕ revoked) history = %d, want 200", code)
	}
	stopOK()

	// Revoked: the same active rights, but revoked_at is set.
	revokedCfg := &rbactest.Config{
		Roles: []rbactest.Role{
			{Name: "inc-viewer", Operators: []string{"archon-fired"}, Permissions: []string{
				"incarnation.list", "incarnation.get", "incarnation.history",
			}},
		},
		Revoked: map[string]time.Time{"archon-fired": time.Now()},
	}
	base, stop := startServer(t, revokedCfg)
	defer stop()
	tok := newValidTokenFor(t, "archon-fired", []string{"inc-viewer"})

	// list/get/history — all cut off by the route-gate (HoldsAction→Deny→403). 403
	// vs 404 is not a leak: both = "no access", 403 is stricter (does not distinguish existence).
	if code := getReadStatus(t, base, tok, "/v1/incarnations"); code != http.StatusForbidden {
		t.Errorf("revoked GET /v1/incarnations = %d, want 403 (gate HoldsAction→Deny)", code)
	}
	if code := getIncStatus(t, base, tok, "redis-prod"); code != http.StatusForbidden {
		t.Errorf("revoked GET /{name} = %d, want 403 (gate HoldsAction→Deny, не данные)", code)
	}
	if code := historyIncStatus(t, base, tok, "redis-prod"); code != http.StatusForbidden {
		t.Errorf("revoked history = %d, want 403 (gate HoldsAction→Deny, не timeline)", code)
	}
}

// TestIntegration_Incarnation_Read_Expired_401 — the auth layer is not broken: an expired
// JWT gives 401 on all read routes BEFORE the RBAC gate (expired ≠ revoked: 401 from
// RequireJWT, not 403/404 from scope).
func TestIntegration_Incarnation_Read_Expired_401(t *testing.T) {
	truncateOperators(t)
	seedOperator(t, "archon-viewer", "")
	seedIncarnationFull(t, "redis-prod", "redis", "archon-viewer", []string{"prod"}, nil)

	rbacCfg := &rbactest.Config{
		Roles: []rbactest.Role{
			{Name: "inc-viewer", Operators: []string{"archon-viewer"}, Permissions: []string{
				"incarnation.list", "incarnation.get", "incarnation.history",
			}},
		},
	}
	base, stop := startServer(t, rbacCfg)
	defer stop()
	tok := newExpiredTokenFor(t, "archon-viewer", []string{"inc-viewer"})

	for _, path := range []string{
		"/v1/incarnations",
		"/v1/incarnations/redis-prod",
		"/v1/incarnations/redis-prod/history",
	} {
		if code := getReadStatus(t, base, tok, path); code != http.StatusUnauthorized {
			t.Errorf("expired JWT GET %s = %d, want 401 (auth-слой не сломан)", path, code)
		}
	}
}

// TestIntegration_Incarnation_Read_403_NoPermission — security non-regression:
// an operator WITHOUT incarnation-read rights gets 403 on list/get/history (the gate still
// requires holding the right — the existence-gate relaxed scope-in-context, NOT the
// permission requirement itself).
func TestIntegration_Incarnation_Read_403_NoPermission(t *testing.T) {
	truncateOperators(t)
	seedOperator(t, "archon-nobody", "")
	seedIncarnationFull(t, "redis-prod", "redis", "archon-nobody", []string{"prod"}, nil)

	rbacCfg := &rbactest.Config{
		Roles: []rbactest.Role{
			{Name: "no-read", Operators: []string{"archon-nobody"}, Permissions: []string{"incarnation.create"}},
		},
	}
	base, stop := startServer(t, rbacCfg)
	defer stop()
	tok := newValidTokenFor(t, "archon-nobody", []string{"no-read"})

	if code := getReadStatus(t, base, tok, "/v1/incarnations"); code != http.StatusForbidden {
		t.Errorf("GET /v1/incarnations без incarnation.list = %d, want 403", code)
	}
	if code := getIncStatus(t, base, tok, "redis-prod"); code != http.StatusForbidden {
		t.Errorf("GET /{name} без incarnation.get = %d, want 403", code)
	}
	if code := historyIncStatus(t, base, tok, "redis-prod"); code != http.StatusForbidden {
		t.Errorf("history без incarnation.history = %d, want 403", code)
	}
}

// listSouls — helper: GET /v1/souls with an optional query string, returns
// (total, len(items)). Fails on non-200.
func listSouls(t *testing.T, base, tok, query string) (total, items int) {
	t.Helper()
	url := base + "/v1/souls"
	if query != "" {
		url += "?" + query
	}
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("listSouls(%q): Do: %v", query, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("listSouls(%q): status = %d, body=%s", query, resp.StatusCode, raw)
	}
	var out struct {
		Items []map[string]any `json:"items"`
		Total int              `json:"total"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("listSouls(%q): decode: %v", query, err)
	}
	return out.Total, len(out.Items)
}

// firstSoulItem returns the first items[] element of GET /v1/souls as a raw map (for
// the wire-key guard asserts). Fatal if the list is empty.
func firstSoulItem(t *testing.T, base, tok string) map[string]any {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, base+"/v1/souls", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("firstSoulItem: Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("firstSoulItem: status = %d, body=%s", resp.StatusCode, raw)
	}
	var out struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("firstSoulItem: decode: %v", err)
	}
	if len(out.Items) == 0 {
		t.Fatalf("firstSoulItem: пустой список souls")
	}
	return out.Items[0]
}

// seedSoulFull inserts a souls row with arbitrary status / coven (for the
// list-filter tests). seedSoul is kept for the issue-token tests (minimal).
func seedSoulFull(t *testing.T, sid, transport string, status soul.Status, coven []string, creator string) {
	t.Helper()
	c := creator
	s := &soul.Soul{
		SID:          sid,
		Transport:    soul.Transport(transport),
		Status:       status,
		Coven:        coven,
		CreatedByAID: &c,
	}
	if err := soul.Insert(context.Background(), integrationPool, s); err != nil {
		t.Fatalf("seedSoulFull(%s): %v", sid, err)
	}
}

// soulSelectStatus reads souls.status directly from the DB.
func soulSelectStatus(t *testing.T, sid string) (string, error) {
	t.Helper()
	var status string
	err := integrationPool.QueryRow(context.Background(),
		`SELECT status FROM souls WHERE sid=$1`, sid).Scan(&status)
	return status, err
}

// seedSoul inserts a souls row via CRUD for the issue-token tests.
func seedSoul(t *testing.T, sid, transport, creator string) {
	t.Helper()
	c := creator
	s := &soul.Soul{
		SID:          sid,
		Transport:    soul.Transport(transport),
		Status:       soul.StatusPending,
		CreatedByAID: &c,
	}
	if err := soul.Insert(context.Background(), integrationPool, s); err != nil {
		t.Fatalf("seedSoul(%s): %v", sid, err)
	}
}

// seedActiveToken inserts an active bootstrap token and returns its token_id.
func seedActiveToken(t *testing.T, sid, creator string) string {
	t.Helper()
	plain, err := bootstraptoken.Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	c := creator
	rec, err := bootstraptoken.Insert(context.Background(), integrationPool, sid, plain.Hash(),
		bootstraptoken.DefaultTokenTTL, &c)
	if err != nil {
		t.Fatalf("seedActiveToken(%s): %v", sid, err)
	}
	return rec.TokenID
}

// bytesReader — helper for inline JSON in the request body. Wraps
// strings.NewReader → io.ReadCloser so http.NewRequest accepts it.
func bytesReader(s string) io.Reader { return io.NopCloser(strings.NewReader(s)) }

// ============================ /v1/roles (RBAC Slice 2a) ============================

func TestIntegration_Role_Create_201(t *testing.T) {
	truncateOperators(t)
	truncateRBAC(t)
	seedOperator(t, "archon-alice", "")
	seedClusterAdmin(t, "archon-alice")

	base, stop := startServer(t, adminRBAC())
	defer stop()

	tok := newValidTokenFor(t, "archon-alice", []string{"cluster-admin"})
	body := bytesReader(`{"name":"ops","description":"ops team","permissions":["soul.list","incarnation.get"]}`)
	req, _ := http.NewRequest(http.MethodPost, base+"/v1/roles", body)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 201, body=%s", resp.StatusCode, raw)
	}

	// The role and permissions are materialized in the DB.
	var permCount int64
	if err := integrationPool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM rbac_role_permissions WHERE role_name='ops'`).Scan(&permCount); err != nil {
		t.Fatalf("perm count: %v", err)
	}
	if permCount != 2 {
		t.Errorf("permissions = %d, want 2", permCount)
	}
	var createdBy *string
	if err := integrationPool.QueryRow(context.Background(),
		`SELECT created_by_aid FROM rbac_roles WHERE name='ops'`).Scan(&createdBy); err != nil {
		t.Fatalf("created_by: %v", err)
	}
	if createdBy == nil || *createdBy != "archon-alice" {
		t.Errorf("created_by_aid = %v, want archon-alice", createdBy)
	}

	// Audit row + payload: name / created_by_aid / permissions are present
	// (ADR-022: an authorization change is always audited).
	var (
		cnt          int64
		payloadName  string
		payloadByAID string
		permsPresent bool
	)
	if err := integrationPool.QueryRow(context.Background(),
		`SELECT COUNT(*), MAX(payload->>'name'), MAX(payload->>'created_by_aid'),
		        bool_or(payload ? 'permissions')
		   FROM audit_log
		  WHERE event_type='role.created' AND archon_aid='archon-alice'`).
		Scan(&cnt, &payloadName, &payloadByAID, &permsPresent); err != nil {
		t.Fatalf("audit probe: %v", err)
	}
	if cnt != 1 {
		t.Errorf("audit count = %d, want 1", cnt)
	}
	if payloadName != "ops" {
		t.Errorf("payload.name = %q, want ops", payloadName)
	}
	if payloadByAID != "archon-alice" {
		t.Errorf("payload.created_by_aid = %q, want archon-alice", payloadByAID)
	}
	if !permsPresent {
		t.Errorf("payload missing 'permissions'")
	}
}

func TestIntegration_Role_Create_Duplicate_409(t *testing.T) {
	truncateOperators(t)
	truncateRBAC(t)
	seedOperator(t, "archon-alice", "")
	seedRole(t, "ops", false, "soul.list")

	base, stop := startServer(t, adminRBAC())
	defer stop()

	tok := newValidTokenFor(t, "archon-alice", []string{"cluster-admin"})
	body := bytesReader(`{"name":"ops"}`)
	req, _ := http.NewRequest(http.MethodPost, base+"/v1/roles", body)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 409, body=%s", resp.StatusCode, raw)
	}
	var p problem.Details
	_ = json.NewDecoder(resp.Body).Decode(&p)
	if p.Type != problem.TypeRoleExists {
		t.Errorf("Type = %q, want %q", p.Type, problem.TypeRoleExists)
	}
}

func TestIntegration_Role_Create_403_NoPermission(t *testing.T) {
	truncateOperators(t)
	truncateRBAC(t)
	seedOperator(t, "archon-viewer", "")

	base, stop := startServer(t, roReaderRBAC())
	defer stop()

	tok := newValidTokenFor(t, "archon-viewer", []string{"viewer"})
	body := bytesReader(`{"name":"ops"}`)
	req, _ := http.NewRequest(http.MethodPost, base+"/v1/roles", body)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403", resp.StatusCode)
	}
}

func TestIntegration_Role_List_200(t *testing.T) {
	truncateOperators(t)
	truncateRBAC(t)
	seedOperator(t, "archon-alice", "")
	seedRole(t, "ops", false, "soul.list", "incarnation.get")
	seedRole(t, "viewers", false, "soul.list")
	seedRoleMember(t, "ops", "archon-alice")

	base, stop := startServer(t, adminRBAC())
	defer stop()

	tok := newValidTokenFor(t, "archon-alice", []string{"cluster-admin"})
	req, _ := http.NewRequest(http.MethodGet, base+"/v1/roles", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200, body=%s", resp.StatusCode, raw)
	}
	var out struct {
		Items []struct {
			Name        string   `json:"name"`
			Builtin     bool     `json:"builtin"`
			Permissions []string `json:"permissions"`
			Operators   []string `json:"operators"`
		} `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	byName := make(map[string]int, len(out.Items))
	for i, it := range out.Items {
		byName[it.Name] = i
	}
	if _, ok := byName["ops"]; !ok {
		t.Fatalf("role 'ops' missing in list: %+v", out.Items)
	}
	ops := out.Items[byName["ops"]]
	if len(ops.Permissions) != 2 {
		t.Errorf("ops permissions = %v, want 2", ops.Permissions)
	}
	if len(ops.Operators) != 1 || ops.Operators[0] != "archon-alice" {
		t.Errorf("ops operators = %v, want [archon-alice]", ops.Operators)
	}
}

func TestIntegration_Role_Delete_204(t *testing.T) {
	truncateOperators(t)
	truncateRBAC(t)
	seedOperator(t, "archon-alice", "")
	seedRole(t, "ops", false, "soul.list")

	base, stop := startServer(t, adminRBAC())
	defer stop()

	tok := newValidTokenFor(t, "archon-alice", []string{"cluster-admin"})
	req, _ := http.NewRequest(http.MethodDelete, base+"/v1/roles/ops", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 204, body=%s", resp.StatusCode, raw)
	}
	var cnt int64
	if err := integrationPool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM rbac_roles WHERE name='ops'`).Scan(&cnt); err != nil {
		t.Fatalf("count: %v", err)
	}
	if cnt != 0 {
		t.Errorf("role still present after delete")
	}

	// Audit row: role.deleted with payload.name (ADR-022).
	var auditCnt int64
	var payloadName string
	if err := integrationPool.QueryRow(context.Background(),
		`SELECT COUNT(*), MAX(payload->>'name') FROM audit_log
		  WHERE event_type='role.deleted' AND archon_aid='archon-alice'`).
		Scan(&auditCnt, &payloadName); err != nil {
		t.Fatalf("audit probe: %v", err)
	}
	if auditCnt != 1 || payloadName != "ops" {
		t.Errorf("audit role.deleted count=%d name=%q, want 1/ops", auditCnt, payloadName)
	}
}

func TestIntegration_Role_Delete_NotFound_404(t *testing.T) {
	truncateOperators(t)
	truncateRBAC(t)
	seedOperator(t, "archon-alice", "")

	base, stop := startServer(t, adminRBAC())
	defer stop()

	tok := newValidTokenFor(t, "archon-alice", []string{"cluster-admin"})
	req, _ := http.NewRequest(http.MethodDelete, base+"/v1/roles/ghost", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	var p problem.Details
	_ = json.NewDecoder(resp.Body).Decode(&p)
	if p.Type != problem.TypeRoleNotFound {
		t.Errorf("Type = %q, want %q", p.Type, problem.TypeRoleNotFound)
	}
}

func TestIntegration_Role_Delete_Builtin_409(t *testing.T) {
	truncateOperators(t)
	truncateRBAC(t)
	seedOperator(t, "archon-alice", "")
	seedRole(t, "cluster-admin", true, "*")

	base, stop := startServer(t, adminRBAC())
	defer stop()

	tok := newValidTokenFor(t, "archon-alice", []string{"cluster-admin"})
	req, _ := http.NewRequest(http.MethodDelete, base+"/v1/roles/cluster-admin", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 409, body=%s", resp.StatusCode, raw)
	}
	var p problem.Details
	_ = json.NewDecoder(resp.Body).Decode(&p)
	if p.Type != problem.TypeRoleBuiltin {
		t.Errorf("Type = %q, want %q", p.Type, problem.TypeRoleBuiltin)
	}
}

func TestIntegration_Role_UpdatePermissions_204(t *testing.T) {
	truncateOperators(t)
	truncateRBAC(t)
	seedOperator(t, "archon-alice", "")
	seedClusterAdmin(t, "archon-alice")
	seedRole(t, "ops", false, "soul.list")

	base, stop := startServer(t, adminRBAC())
	defer stop()

	tok := newValidTokenFor(t, "archon-alice", []string{"cluster-admin"})
	body := bytesReader(`{"permissions":["incarnation.get","incarnation.list"]}`)
	req, _ := http.NewRequest(http.MethodPatch, base+"/v1/roles/ops/permissions", body)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 204, body=%s", resp.StatusCode, raw)
	}
	// Replace semantics: the old soul.list is gone, two new ones arrived.
	rows, err := integrationPool.Query(context.Background(),
		`SELECT permission FROM rbac_role_permissions WHERE role_name='ops' ORDER BY permission`)
	if err != nil {
		t.Fatalf("query perms: %v", err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var p string
		_ = rows.Scan(&p)
		got = append(got, p)
	}
	if len(got) != 2 || got[0] != "incarnation.get" || got[1] != "incarnation.list" {
		t.Errorf("permissions after update = %v, want [incarnation.get incarnation.list]", got)
	}

	// Audit row: role.permissions-updated with payload.name + permissions (ADR-022).
	var auditCnt int64
	var payloadName string
	var permsPresent bool
	if err := integrationPool.QueryRow(context.Background(),
		`SELECT COUNT(*), MAX(payload->>'name'), bool_or(payload ? 'permissions')
		   FROM audit_log
		  WHERE event_type='role.permissions-updated' AND archon_aid='archon-alice'`).
		Scan(&auditCnt, &payloadName, &permsPresent); err != nil {
		t.Fatalf("audit probe: %v", err)
	}
	if auditCnt != 1 || payloadName != "ops" || !permsPresent {
		t.Errorf("audit role.permissions-updated count=%d name=%q perms=%v, want 1/ops/true",
			auditCnt, payloadName, permsPresent)
	}
}

func TestIntegration_Role_GrantThenRevokeOperator(t *testing.T) {
	truncateOperators(t)
	truncateRBAC(t)
	seedOperator(t, "archon-alice", "")
	seedClusterAdmin(t, "archon-alice")
	seedOperator(t, "archon-bob", "archon-alice")
	seedRole(t, "ops", false, "soul.list")

	base, stop := startServer(t, adminRBAC())
	defer stop()
	tok := newValidTokenFor(t, "archon-alice", []string{"cluster-admin"})

	// grant.
	grantBody := bytesReader(`{"aid":"archon-bob"}`)
	greq, _ := http.NewRequest(http.MethodPost, base+"/v1/roles/ops/operators", grantBody)
	greq.Header.Set("Authorization", "Bearer "+tok)
	greq.Header.Set("Content-Type", "application/json")
	gresp, err := http.DefaultClient.Do(greq)
	if err != nil {
		t.Fatalf("grant Do: %v", err)
	}
	defer gresp.Body.Close()
	if gresp.StatusCode != http.StatusNoContent {
		raw, _ := io.ReadAll(gresp.Body)
		t.Fatalf("grant status = %d, want 204, body=%s", gresp.StatusCode, raw)
	}
	var grantedBy *string
	if err := integrationPool.QueryRow(context.Background(),
		`SELECT granted_by_aid FROM rbac_role_operators WHERE role_name='ops' AND aid='archon-bob'`).
		Scan(&grantedBy); err != nil {
		t.Fatalf("granted_by probe: %v", err)
	}
	if grantedBy == nil || *grantedBy != "archon-alice" {
		t.Errorf("granted_by_aid = %v, want archon-alice", grantedBy)
	}

	// Audit row: role.operator-granted with name / aid / granted_by_aid (ADR-022).
	var gCnt int64
	var gName, gAID, gBy string
	if err := integrationPool.QueryRow(context.Background(),
		`SELECT COUNT(*), MAX(payload->>'name'), MAX(payload->>'aid'), MAX(payload->>'granted_by_aid')
		   FROM audit_log
		  WHERE event_type='role.operator-granted' AND archon_aid='archon-alice'`).
		Scan(&gCnt, &gName, &gAID, &gBy); err != nil {
		t.Fatalf("audit grant probe: %v", err)
	}
	if gCnt != 1 || gName != "ops" || gAID != "archon-bob" || gBy != "archon-alice" {
		t.Errorf("audit role.operator-granted = count=%d name=%q aid=%q by=%q, want 1/ops/archon-bob/archon-alice",
			gCnt, gName, gAID, gBy)
	}

	// revoke.
	rreq, _ := http.NewRequest(http.MethodDelete, base+"/v1/roles/ops/operators/archon-bob", nil)
	rreq.Header.Set("Authorization", "Bearer "+tok)
	rresp, err := http.DefaultClient.Do(rreq)
	if err != nil {
		t.Fatalf("revoke Do: %v", err)
	}
	defer rresp.Body.Close()
	if rresp.StatusCode != http.StatusNoContent {
		raw, _ := io.ReadAll(rresp.Body)
		t.Fatalf("revoke status = %d, want 204, body=%s", rresp.StatusCode, raw)
	}
	var cnt int64
	if err := integrationPool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM rbac_role_operators WHERE role_name='ops' AND aid='archon-bob'`).
		Scan(&cnt); err != nil {
		t.Fatalf("membership count: %v", err)
	}
	if cnt != 0 {
		t.Errorf("membership still present after revoke")
	}

	// Audit row: role.operator-revoked with payload name / aid (ADR-022).
	var rCnt int64
	var rName, rAID string
	if err := integrationPool.QueryRow(context.Background(),
		`SELECT COUNT(*), MAX(payload->>'name'), MAX(payload->>'aid')
		   FROM audit_log
		  WHERE event_type='role.operator-revoked' AND archon_aid='archon-alice'`).
		Scan(&rCnt, &rName, &rAID); err != nil {
		t.Fatalf("audit revoke probe: %v", err)
	}
	if rCnt != 1 || rName != "ops" || rAID != "archon-bob" {
		t.Errorf("audit role.operator-revoked = count=%d name=%q aid=%q, want 1/ops/archon-bob",
			rCnt, rName, rAID)
	}
}

func TestIntegration_Role_GrantOperator_OperatorNotFound_404(t *testing.T) {
	truncateOperators(t)
	truncateRBAC(t)
	seedOperator(t, "archon-alice", "")
	seedClusterAdmin(t, "archon-alice")
	seedRole(t, "ops", false, "soul.list")

	base, stop := startServer(t, adminRBAC())
	defer stop()

	tok := newValidTokenFor(t, "archon-alice", []string{"cluster-admin"})
	body := bytesReader(`{"aid":"archon-ghost"}`)
	req, _ := http.NewRequest(http.MethodPost, base+"/v1/roles/ops/operators", body)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 404, body=%s", resp.StatusCode, raw)
	}
}

func TestIntegration_Role_RevokeOperator_NotFound_404(t *testing.T) {
	truncateOperators(t)
	truncateRBAC(t)
	seedOperator(t, "archon-alice", "")
	seedRole(t, "ops", false, "soul.list")

	base, stop := startServer(t, adminRBAC())
	defer stop()

	tok := newValidTokenFor(t, "archon-alice", []string{"cluster-admin"})
	req, _ := http.NewRequest(http.MethodDelete, base+"/v1/roles/ops/operators/archon-alice", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	var p problem.Details
	_ = json.NewDecoder(resp.Body).Decode(&p)
	if p.Type != problem.TypeNotFound {
		t.Errorf("Type = %q, want %q", p.Type, problem.TypeNotFound)
	}
}

// TestIntegration_Role_403_AllOperations — an operator without a role.<action>
// permission gets 403 on EACH of the six role operations (gap 2, the REST
// surface; create is already covered by TestIntegration_Role_Create_403_NoPermission,
// here — list/delete/update/grant/revoke). RBAC grants only soul.list →
// any role.* → deny.
func TestIntegration_Role_403_AllOperations(t *testing.T) {
	truncateOperators(t)
	truncateRBAC(t)
	seedOperator(t, "archon-viewer", "")
	seedRole(t, "ops", false, "soul.list")

	base, stop := startServer(t, roReaderRBAC())
	defer stop()
	tok := newValidTokenFor(t, "archon-viewer", []string{"viewer"})

	cases := []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{"list", http.MethodGet, "/v1/roles", ""},
		{"delete", http.MethodDelete, "/v1/roles/ops", ""},
		{"update", http.MethodPatch, "/v1/roles/ops/permissions", `{"permissions":["soul.list"]}`},
		{"grant", http.MethodPost, "/v1/roles/ops/operators", `{"aid":"archon-viewer"}`},
		{"revoke", http.MethodDelete, "/v1/roles/ops/operators/archon-viewer", ""},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			var body io.Reader
			if tc.body != "" {
				body = bytesReader(tc.body)
			}
			req, _ := http.NewRequest(tc.method, base+tc.path, body)
			req.Header.Set("Authorization", "Bearer "+tok)
			if tc.body != "" {
				req.Header.Set("Content-Type", "application/json")
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("Do: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusForbidden {
				raw, _ := io.ReadAll(resp.Body)
				t.Errorf("%s %s: status = %d, want 403, body=%s", tc.method, tc.path, resp.StatusCode, raw)
			}
		})
	}
}

// TestIntegration_Role_SelfLockout — three lockout mutations on the last
// `*` path (alice via the single wildcard role) give 409
// would-lock-out-cluster through the full router+middleware+real PG (gap 3);
// the DB stays untouched (the tx rolled back). The enforcer config (adminRBAC) grants
// alice the role.* right; the self-lockout check itself reads the rbac_* tables.
func TestIntegration_Role_SelfLockout(t *testing.T) {
	base, stop := startServer(t, adminRBAC())
	defer stop()
	tok := newValidTokenFor(t, "archon-alice", []string{"cluster-admin"})

	// setup — the only path to `*`: alice via a wildcard-role in the rbac_* tables.
	setup := func() {
		truncateOperators(t)
		truncateRBAC(t)
		seedOperator(t, "archon-alice", "")
		seedRole(t, "wildcard-role", false, "*")
		seedRoleMember(t, "wildcard-role", "archon-alice")
	}

	assert409Lockout := func(t *testing.T, method, path, body string) {
		t.Helper()
		var r io.Reader
		if body != "" {
			r = bytesReader(body)
		}
		req, _ := http.NewRequest(method, base+path, r)
		req.Header.Set("Authorization", "Bearer "+tok)
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("Do: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusConflict {
			raw, _ := io.ReadAll(resp.Body)
			t.Fatalf("%s %s: status = %d, want 409, body=%s", method, path, resp.StatusCode, raw)
		}
		var p problem.Details
		if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if p.Type != problem.TypeWouldLockOutCluster {
			t.Errorf("Type = %q, want %q", p.Type, problem.TypeWouldLockOutCluster)
		}
	}

	t.Run("delete", func(t *testing.T) {
		setup()
		assert409Lockout(t, http.MethodDelete, "/v1/roles/wildcard-role", "")
		var n int64
		if err := integrationPool.QueryRow(context.Background(),
			`SELECT COUNT(*) FROM rbac_roles WHERE name='wildcard-role'`).Scan(&n); err != nil {
			t.Fatalf("role probe: %v", err)
		}
		if n != 1 {
			t.Error("role deleted despite lockout (tx not rolled back)")
		}
	})

	t.Run("update-remove-wildcard", func(t *testing.T) {
		setup()
		assert409Lockout(t, http.MethodPatch, "/v1/roles/wildcard-role/permissions",
			`{"permissions":["soul.list"]}`)
		var n int64
		if err := integrationPool.QueryRow(context.Background(),
			`SELECT COUNT(*) FROM rbac_role_permissions WHERE role_name='wildcard-role' AND permission='*'`).
			Scan(&n); err != nil {
			t.Fatalf("perm probe: %v", err)
		}
		if n != 1 {
			t.Error("wildcard permission removed despite lockout (tx not rolled back)")
		}
	})

	t.Run("revoke-last-admin", func(t *testing.T) {
		setup()
		assert409Lockout(t, http.MethodDelete, "/v1/roles/wildcard-role/operators/archon-alice", "")
		var n int64
		if err := integrationPool.QueryRow(context.Background(),
			`SELECT COUNT(*) FROM rbac_role_operators WHERE role_name='wildcard-role' AND aid='archon-alice'`).
			Scan(&n); err != nil {
			t.Fatalf("membership probe: %v", err)
		}
		if n != 1 {
			t.Error("membership revoked despite lockout (tx not rolled back)")
		}
	})
}

// --- Service registry endpoints (ADR-028 S3) ---

// truncateServices cleans service_registry to a clean state for the service.*
// integration tests. FK service_registry → operators(aid): called AFTER
// truncateOperators (via CASCADE it does not remove service_registry rows with
// created_by_aid=NULL, but new tests create records with an FK to the seed operators).
func truncateServices(t *testing.T) {
	t.Helper()
	if _, err := integrationPool.Exec(context.Background(),
		`TRUNCATE service_registry RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncate service_registry: %v", err)
	}
}

func TestIntegration_Service_Register_201(t *testing.T) {
	truncateOperators(t)
	truncateServices(t)
	seedOperator(t, "archon-alice", "")

	base, stop := startServer(t, adminRBAC())
	defer stop()

	tok := newValidTokenFor(t, "archon-alice", []string{"cluster-admin"})
	body := bytesReader(`{"name":"web","git":"https://git/web.git","ref":"v1.0.0"}`)
	req, _ := http.NewRequest(http.MethodPost, base+"/v1/services", body)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 201, body=%s", resp.StatusCode, raw)
	}

	// The record is materialized in the DB with created_by_aid.
	var createdBy *string
	if err := integrationPool.QueryRow(context.Background(),
		`SELECT created_by_aid FROM service_registry WHERE name='web'`).Scan(&createdBy); err != nil {
		t.Fatalf("created_by: %v", err)
	}
	if createdBy == nil || *createdBy != "archon-alice" {
		t.Errorf("created_by_aid = %v, want archon-alice", createdBy)
	}

	// Audit row + payload {name, git, ref, created_by_aid}. The git URL is not a secret.
	var (
		cnt          int64
		payloadName  string
		payloadGit   string
		payloadByAID string
	)
	if err := integrationPool.QueryRow(context.Background(),
		`SELECT COUNT(*), MAX(payload->>'name'), MAX(payload->>'git'), MAX(payload->>'created_by_aid')
		   FROM audit_log
		  WHERE event_type='service.registered' AND archon_aid='archon-alice'`).
		Scan(&cnt, &payloadName, &payloadGit, &payloadByAID); err != nil {
		t.Fatalf("audit probe: %v", err)
	}
	if cnt != 1 {
		t.Errorf("audit count = %d, want 1", cnt)
	}
	if payloadName != "web" {
		t.Errorf("payload.name = %q, want web", payloadName)
	}
	if payloadGit != "https://git/web.git" {
		t.Errorf("payload.git = %q", payloadGit)
	}
	if payloadByAID != "archon-alice" {
		t.Errorf("payload.created_by_aid = %q, want archon-alice", payloadByAID)
	}
}

func TestIntegration_Service_Register_Duplicate_409(t *testing.T) {
	truncateOperators(t)
	truncateServices(t)
	seedOperator(t, "archon-alice", "")

	base, stop := startServer(t, adminRBAC())
	defer stop()

	tok := newValidTokenFor(t, "archon-alice", []string{"cluster-admin"})
	do := func() *http.Response {
		req, _ := http.NewRequest(http.MethodPost, base+"/v1/services",
			bytesReader(`{"name":"web","git":"g","ref":"v1"}`))
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("Do: %v", err)
		}
		return resp
	}
	first := do()
	first.Body.Close()
	if first.StatusCode != http.StatusCreated {
		t.Fatalf("first register status = %d, want 201", first.StatusCode)
	}
	second := do()
	defer second.Body.Close()
	if second.StatusCode != http.StatusConflict {
		raw, _ := io.ReadAll(second.Body)
		t.Fatalf("status = %d, want 409, body=%s", second.StatusCode, raw)
	}
	var p problem.Details
	_ = json.NewDecoder(second.Body).Decode(&p)
	if p.Type != problem.TypeServiceExists {
		t.Errorf("Type = %q, want %q", p.Type, problem.TypeServiceExists)
	}
}

func TestIntegration_Service_Register_403_NoPermission(t *testing.T) {
	truncateOperators(t)
	truncateServices(t)
	seedOperator(t, "archon-viewer", "")

	base, stop := startServer(t, roReaderRBAC())
	defer stop()

	tok := newValidTokenFor(t, "archon-viewer", []string{"viewer"})
	req, _ := http.NewRequest(http.MethodPost, base+"/v1/services",
		bytesReader(`{"name":"web","git":"g","ref":"v1"}`))
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 403, body=%s", resp.StatusCode, raw)
	}
	// 403 is not audited as service.registered (the operation did not happen).
	var cnt int64
	if err := integrationPool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM audit_log WHERE event_type='service.registered'`).Scan(&cnt); err != nil {
		t.Fatalf("audit probe: %v", err)
	}
	if cnt != 0 {
		t.Errorf("audit count = %d, want 0 (403 must not register)", cnt)
	}
}

func TestIntegration_Service_List_And_Get_200(t *testing.T) {
	truncateOperators(t)
	truncateServices(t)
	seedOperator(t, "archon-alice", "")

	base, stop := startServer(t, adminRBAC())
	defer stop()

	tok := newValidTokenFor(t, "archon-alice", []string{"cluster-admin"})
	for _, name := range []string{"api", "web"} {
		req, _ := http.NewRequest(http.MethodPost, base+"/v1/services",
			bytesReader(`{"name":"`+name+`","git":"g","ref":"v1"}`))
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("Do register %s: %v", name, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("register %s status = %d", name, resp.StatusCode)
		}
	}

	// List → 200 + 2 items.
	listReq, _ := http.NewRequest(http.MethodGet, base+"/v1/services", nil)
	listReq.Header.Set("Authorization", "Bearer "+tok)
	listResp, err := http.DefaultClient.Do(listReq)
	if err != nil {
		t.Fatalf("Do list: %v", err)
	}
	defer listResp.Body.Close()
	if listResp.StatusCode != http.StatusOK {
		t.Fatalf("list status = %d, want 200", listResp.StatusCode)
	}
	var listBody struct {
		Items []struct {
			Name string `json:"name"`
		} `json:"items"`
	}
	if err := json.NewDecoder(listResp.Body).Decode(&listBody); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(listBody.Items) != 2 {
		t.Errorf("items = %d, want 2", len(listBody.Items))
	}

	// Get one → 200.
	getReq, _ := http.NewRequest(http.MethodGet, base+"/v1/services/web", nil)
	getReq.Header.Set("Authorization", "Bearer "+tok)
	getResp, err := http.DefaultClient.Do(getReq)
	if err != nil {
		t.Fatalf("Do get: %v", err)
	}
	defer getResp.Body.Close()
	if getResp.StatusCode != http.StatusOK {
		t.Fatalf("get status = %d, want 200", getResp.StatusCode)
	}
}

func TestIntegration_Service_Update_200(t *testing.T) {
	truncateOperators(t)
	truncateServices(t)
	seedOperator(t, "archon-alice", "")

	base, stop := startServer(t, adminRBAC())
	defer stop()

	tok := newValidTokenFor(t, "archon-alice", []string{"cluster-admin"})
	regReq, _ := http.NewRequest(http.MethodPost, base+"/v1/services",
		bytesReader(`{"name":"web","git":"g","ref":"v1"}`))
	regReq.Header.Set("Authorization", "Bearer "+tok)
	regResp, err := http.DefaultClient.Do(regReq)
	if err != nil {
		t.Fatalf("Do register: %v", err)
	}
	regResp.Body.Close()

	patchReq, _ := http.NewRequest(http.MethodPatch, base+"/v1/services/web",
		bytesReader(`{"git":"https://git/web.git","ref":"v2.0.0"}`))
	patchReq.Header.Set("Authorization", "Bearer "+tok)
	patchResp, err := http.DefaultClient.Do(patchReq)
	if err != nil {
		t.Fatalf("Do patch: %v", err)
	}
	defer patchResp.Body.Close()
	if patchResp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(patchResp.Body)
		t.Fatalf("patch status = %d, want 200, body=%s", patchResp.StatusCode, raw)
	}

	var ref string
	if err := integrationPool.QueryRow(context.Background(),
		`SELECT ref FROM service_registry WHERE name='web'`).Scan(&ref); err != nil {
		t.Fatalf("ref probe: %v", err)
	}
	if ref != "v2.0.0" {
		t.Errorf("ref = %q, want v2.0.0", ref)
	}

	var cnt int64
	if err := integrationPool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM audit_log WHERE event_type='service.updated' AND archon_aid='archon-alice'`).
		Scan(&cnt); err != nil {
		t.Fatalf("audit probe: %v", err)
	}
	if cnt != 1 {
		t.Errorf("audit count = %d, want 1", cnt)
	}
}

func TestIntegration_Service_Deregister_204(t *testing.T) {
	truncateOperators(t)
	truncateServices(t)
	seedOperator(t, "archon-alice", "")

	base, stop := startServer(t, adminRBAC())
	defer stop()

	tok := newValidTokenFor(t, "archon-alice", []string{"cluster-admin"})
	regReq, _ := http.NewRequest(http.MethodPost, base+"/v1/services",
		bytesReader(`{"name":"web","git":"g","ref":"v1"}`))
	regReq.Header.Set("Authorization", "Bearer "+tok)
	regResp, err := http.DefaultClient.Do(regReq)
	if err != nil {
		t.Fatalf("Do register: %v", err)
	}
	regResp.Body.Close()

	delReq, _ := http.NewRequest(http.MethodDelete, base+"/v1/services/web", nil)
	delReq.Header.Set("Authorization", "Bearer "+tok)
	delResp, err := http.DefaultClient.Do(delReq)
	if err != nil {
		t.Fatalf("Do delete: %v", err)
	}
	defer delResp.Body.Close()
	if delResp.StatusCode != http.StatusNoContent {
		raw, _ := io.ReadAll(delResp.Body)
		t.Fatalf("delete status = %d, want 204, body=%s", delResp.StatusCode, raw)
	}

	var n int64
	if err := integrationPool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM service_registry WHERE name='web'`).Scan(&n); err != nil {
		t.Fatalf("row probe: %v", err)
	}
	if n != 0 {
		t.Errorf("row count = %d, want 0 (deleted)", n)
	}

	var cnt int64
	if err := integrationPool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM audit_log WHERE event_type='service.deregistered' AND archon_aid='archon-alice'`).
		Scan(&cnt); err != nil {
		t.Fatalf("audit probe: %v", err)
	}
	if cnt != 1 {
		t.Errorf("audit count = %d, want 1", cnt)
	}
}

func TestIntegration_Service_Deregister_NotFound_404(t *testing.T) {
	truncateOperators(t)
	truncateServices(t)
	seedOperator(t, "archon-alice", "")

	base, stop := startServer(t, adminRBAC())
	defer stop()

	tok := newValidTokenFor(t, "archon-alice", []string{"cluster-admin"})
	req, _ := http.NewRequest(http.MethodDelete, base+"/v1/services/ghost", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 404, body=%s", resp.StatusCode, raw)
	}
}

// Direct use of health.Pinger from the integration test — a sanity check
// that the interface is compatible with the real dependencies.
var _ health.Pinger = poolPinger{}
var _ health.Pinger = (*keepervault.Client)(nil)
