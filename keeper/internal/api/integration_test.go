//go:build integration

// Integration-тест HTTP-сервера Operator API: PG + Vault через
// testcontainers-go, реальный listener на ephemeral port.
//
// Запуск:
//
//	make test-integration
//	# или
//	cd keeper && SOUL_STACK_INTEGRATION_REQUIRE_DOCKER=1 \
//	    go test -tags=integration -race -count=1 ./internal/api/...
//
// Один PG + один Vault per-package в TestMain.

package api

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"log/slog"
	"net/http"
	"os"
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

// startServer запускает HTTP-сервер на ephemeral port и возвращает
// фактический base URL + shutdown-функцию. rbacCfg — конфиг RBAC (nil →
// пустой enforcer, любой Check вернёт deny).
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
		// Voyage-контур: подключаем для ADR-047 S4 e2e (scoped command-таргет ∩
		// Purview). Резолверы — production-PG поверх того же pool.
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

	// Дождаться, пока listener фактически забиндится. Start меняет
	// srv.Addr на актуальный port; ждём появления непустого порта.
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

// poolPinger — адаптер pgxpool.Pool к интерфейсу health.Pinger.
type poolPinger struct{ pool *pgxpool.Pool }

func (p poolPinger) Ping(ctx context.Context) error { return p.pool.Ping(ctx) }

func newValidToken(t *testing.T) string {
	t.Helper()
	return newValidTokenFor(t, "archon-test", []string{"cluster-admin"})
}

// newValidTokenFor выпускает JWT для произвольного AID — нужен Operator
// endpoint-тестам, где claims.Subject определяет audit-row и
// permission-check.
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

// truncateOperators чистит operators + audit_log + incarnation +
// state_history + souls + bootstrap_tokens между тестами. Один pool на
// пакет → состояние шарится между тестами; truncate в начале каждого теста
// делает порядок независимым.
//
// FK-зависимости (ADR-014): incarnation → operators(aid),
// state_history → operators(aid) + incarnation(name), souls →
// operators(aid) (created_by_aid), bootstrap_tokens → souls(sid) +
// operators(aid). CASCADE снимает зависимости автоматически.
func truncateOperators(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	if _, err := integrationPool.Exec(ctx,
		`TRUNCATE state_history, incarnation, bootstrap_tokens, souls, voyages, voyage_targets, operators, audit_log RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
}

// truncateRBAC чистит RBAC-таблицы (роли / permissions / membership) до
// чистого состояния для role.*-integration-тестов. Вызывается ПОСЛЕ
// truncateOperators (тот через CASCADE сносит лишь membership/роли, ссылающиеся
// на operators; builtin cluster-admin с created_by_aid=NULL переживает CASCADE).
//
// CASCADE на rbac_roles снимает permissions + membership (ON DELETE CASCADE).
func truncateRBAC(t *testing.T) {
	t.Helper()
	if _, err := integrationPool.Exec(context.Background(),
		`TRUNCATE rbac_roles, rbac_role_permissions, rbac_role_operators RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncate rbac: %v", err)
	}
}

// seedRole создаёт роль с набором permissions напрямую в БД (минуя API) —
// нужен тестам Delete/Update/Grant/Revoke, опирающимся на существующую роль.
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

// seedRoleMember привязывает AID к роли напрямую в БД.
func seedRoleMember(t *testing.T, roleName, aid string) {
	t.Helper()
	if _, err := integrationPool.Exec(context.Background(),
		`INSERT INTO rbac_role_operators (role_name, aid) VALUES ($1, $2)`, roleName, aid); err != nil {
		t.Fatalf("seedRoleMember(%s, %s): %v", roleName, aid, err)
	}
}

// seedClusterAdmin выдаёт caller-у membership builtin-роли cluster-admin (`*`)
// в rbac_*-таблицах БД (модель-C). Config-RBAC enforcer (adminRBAC) лишь
// пропускает caller-а через permission-gate middleware; сам Service делает
// subset-check (least-privilege) и self-lockout-проверку по РЕАЛЬНОЙ membership
// из rbac_role_operators. Без membership caller держит 0 эффективных
// permissions → subset-check отказывает, а self-lockout видит 0 admin-ов.
// truncateRBAC сносит и роли — поэтому ре-сидим cluster-admin с `*` идемпотентно.
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

// seedOperator вставляет AID в реестр для тестов, опирающихся на
// существующего Архонта. Возвращает CreatedAt из БД.
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

	// GET /openapi.yaml — ЗА JWT (ADR-054 doc-viewer / router.go): спека больше не
	// публична, /docs фетчит её с Bearer-заголовком. Без токена → 401, поэтому шлём
	// валидный JWT (как UI).
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
	// Served-эндпоинт отдаёт runtime-дамп huma-агрегатора (servedOpenAPIHandler) —
	// это OpenAPI 3.1.0. Committed docs/keeper/openapi.yaml (3.0.3 для oapi-codegen)
	// — производный снимок для UI-vendor, НЕ served. huma .YAML() сортирует ключи
	// верхнего уровня по алфавиту (components/info/openapi/paths…), поэтому
	// `openapi: 3.1.0` НЕ первая строка — ищем версию строкой по всему документу.
	if !strings.Contains(string(body), "openapi: 3.1.0") {
		t.Errorf("body не содержит маркер OpenAPI 3.1.0; first 64 bytes: %q", string(body[:min(64, len(body))]))
	}
}

// TestIntegration_Metrics_NotOnOpenAPI — после ADR-024 / Slice 1 `/metrics`
// снят с openapi-роутера (вынесен на выделенный listener). На openapi-порту
// эндпоинт за auth-chain `/v1/*` не висит — bare `/metrics` отдаёт 404
// (catch-all NotFound), не Prometheus-вывод.
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

// TestIntegration_Metrics_RecordsV1Requests — наводит трафик на /v1/* через
// openapi-порт и проверяет, что keeper_http_*-метрики (записанные middleware
// на /v1/*) видны на ВЫДЕЛЕННОМ metrics-listener-е (тот же *obs.Registry).
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

	// Один registry шарится между HTTP-middleware (инструментация /v1/*) и
	// metrics-listener-ом (exposition) — как в production-wire-up keeper/cmd.
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

	// scrape должен содержать go-collector до любого /v1/-трафика.
	if body := httpGetBody(t, metricsURL); !strings.Contains(body, "go_goroutines") {
		t.Errorf("metrics output не содержит go_goroutines (core-collector); len=%d", len(body))
	}

	// Любой запрос под /v1 без токена → 401, но pipeline проходит до
	// metrics-middleware (он внутри /v1.Use до auth — см. router.go).
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

// httpGetBody — GET URL → string body (для metrics-scrape в тестах).
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

// adminRBAC — RBAC-конфиг с одним cluster-admin (`archon-alice`).
func adminRBAC() *rbactest.Config {
	return &rbactest.Config{
		Roles: []rbactest.Role{
			{Name: "cluster-admin", Operators: []string{"archon-alice"}, Permissions: []string{"*"}},
		},
	}
}

// twoAdminsRBAC — два cluster-admin-а, для теста revoke без self-lockout.
func twoAdminsRBAC() *rbactest.Config {
	return &rbactest.Config{
		Roles: []rbactest.Role{
			{Name: "cluster-admin", Operators: []string{"archon-alice", "archon-bob"}, Permissions: []string{"*"}},
		},
	}
}

// roReader — RBAC без operator.* permissions (для теста 403).
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

	// Audit row + payload-структура (qa-coverage M0.6b): aid /
	// display_name / created_by_aid должны попасть в payload, JWT и
	// expires_at — НЕ должны (sensitive).
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

	// JWT и expires_at — sensitive, в audit-payload НЕ пишутся.
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
	// revoke-контракт требует requestBody (committed openapi.yaml →
	// requestBody.required: true; единственное поле reason — optional). Пустой
	// JSON-объект удовлетворяет huma-валидацию, не задавая reason.
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

	// Bob ревокнут в БД.
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
	// Ревокнем Bob через crud напрямую (минуя HTTP).
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
	// revoke-контракт требует requestBody (см. Revoke_SingleAdmin); пустой JSON-
	// объект проходит валидацию, reason не задаётся.
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

// seedIncarnation вставляет incarnation-row через CRUD-слой для тестов
// Get/History/List, опирающихся на существующие записи.
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
	// Поле `status` НЕ должно быть в response (OpenAPI:
	// IncarnationCreateReply объявляет только apply_id+incarnation).
	if _, hasStatus := out["status"]; hasStatus {
		t.Errorf("response contains 'status' field, want absent: %v", out)
	}
	if s, _ := out["apply_id"].(string); len(s) != 26 {
		t.Errorf("apply_id = %q (len=%d), want ULID 26 chars", s, len(s))
	}

	// Row в БД.
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

	// Audit-event записан с правильным payload (qa-coverage M0.6c-1):
	// apply_id + name + service должны попасть в payload (apply_id для
	// корреляции, name/service для аудит-фильтрации без join).
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
	// created_by_aid — required в OpenAPI, должен присутствовать (не omitted).
	if _, present := dto["created_by_aid"]; !present {
		t.Errorf("response missing 'created_by_aid' (OpenAPI required): %v", dto)
	}
	if dto["created_by_aid"] != "archon-alice" {
		t.Errorf("created_by_aid = %v, want \"archon-alice\"", dto["created_by_aid"])
	}
	// status_details — nullable в OpenAPI; для status=ready ожидаем null
	// присутствующим (а не omitted-ключом).
	v, present := dto["status_details"]
	if !present {
		t.Errorf("response missing 'status_details' (must be null, not omitted)")
	}
	if v != nil {
		t.Errorf("status_details = %v, want null", v)
	}
}

// TestIntegration_Incarnation_Get_CreatedByAID_NullAfterRevoke — после
// удаления оператора FK `incarnation.created_by_aid` уходит в SET NULL
// (ADR-014), и Get-response должен отдавать `"created_by_aid": null`,
// а не omitted-ключ (qa coverage gap M0.6c-1).
func TestIntegration_Incarnation_Get_CreatedByAID_NullAfterRevoke(t *testing.T) {
	truncateOperators(t)
	seedOperator(t, "archon-alice", "")
	seedOperator(t, "archon-bob", "archon-alice")
	seedIncarnation(t, "redis-test", "redis", "archon-bob")

	// Прямой DELETE operator-row → FK ON DELETE SET NULL на incarnation.created_by_aid.
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

	// Без фильтра — 3 элемента.
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

	// Seed history напрямую — handler-side write придёт в M0.6c-2.
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
	// Timestamp обязан быть заполнен и парситься как RFC3339 (state_history.at
	// через DEFAULT NOW()). Wire-ключ — created_at (общий с остальным Operator
	// API). Регрессия: клиенты получали null/пустой timestamp.
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

	// Два state_history-row с разными apply_id — фильтр должен оставить один.
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

	// Filter — non-matching ULID (валидный синтаксически, но нет такого
	// apply_id в БД): 200 + items=[], total=0. НЕ 404.
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

// TestIntegration_Incarnation_Create_BodyTooLarge_413 — body > v1RequestBodyLimit
// (1 MiB). huma-роут (incarnation Create мигрирован на huma) сам ограничивает тело
// и на превышении отдаёт RFC-корректный 413 Payload Too Large (huma v2:
// "request body is too large limit=… bytes"), а не 400. Тест проверяет именно 413.
func TestIntegration_Incarnation_Create_BodyTooLarge_413(t *testing.T) {
	truncateOperators(t)
	seedOperator(t, "archon-alice", "")

	base, stop := startServer(t, adminRBAC())
	defer stop()

	tok := newValidTokenFor(t, "archon-alice", []string{"cluster-admin"})
	// 2 MiB случайного валидного JSON (огромный input-объект).
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

// soulRBAC — cluster-admin + viewer с soul.create / soul.issue-token у
// admin-а, чтобы покрыть и 201/200, и 403.
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
	// covens из запроса персистятся в souls.coven (привязка при онбординге,
	// GAP #3): без неё хост не таргетится сценарием, нужен прямой UPDATE в БД.
	if covens, _ := out["covens"].([]any); len(covens) != 1 || covens[0] != "prod" {
		t.Errorf("response covens = %v, want [prod]", out["covens"])
	}
	if tokStr, _ := out["bootstrap_token"].(string); tokStr == "" {
		t.Errorf("bootstrap_token missing for transport=agent: %v", out)
	}
	if out["created_by_aid"] != "archon-alice" {
		t.Errorf("created_by_aid = %v", out["created_by_aid"])
	}

	// souls-row создан, status=pending.
	got, err := soulSelectStatus(t, "web-01.example.com")
	if err != nil {
		t.Fatalf("soulSelectStatus: %v", err)
	}
	if got != "pending" {
		t.Errorf("souls.status = %q, want pending", got)
	}

	// requested_at проставлен (B1): нужен для Reaper-правила pending→expired
	// по индексу souls_pending_requested_at_idx (docs/soul/onboarding.md:51).
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

	// souls.coven персистнут из covens-поля запроса (GAP #3).
	var savedCoven []string
	if err := integrationPool.QueryRow(context.Background(),
		`SELECT coven FROM souls WHERE sid='web-01.example.com'`).Scan(&savedCoven); err != nil {
		t.Fatalf("coven probe: %v", err)
	}
	if len(savedCoven) != 1 || savedCoven[0] != "prod" {
		t.Errorf("souls.coven = %v, want [prod]", savedCoven)
	}

	// Активный bootstrap-токен в БД.
	var tokenCount int64
	if err := integrationPool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM bootstrap_tokens WHERE sid='web-01.example.com' AND used_at IS NULL`).
		Scan(&tokenCount); err != nil {
		t.Fatalf("token count: %v", err)
	}
	if tokenCount != 1 {
		t.Errorf("active token count = %d, want 1", tokenCount)
	}

	// Audit: token-plain НЕ должен попасть в payload (substring-mask H1).
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

	// Старый токен инвалидирован (used_at IS NOT NULL), новый — активный.
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

	// Audit: soul.token-issued записан, force=true, expired_previous=true.
	// Идентификаторы токенов в payload отсутствуют (secret-mask H1).
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

// TestIntegration_Soul_Create_UnknownCreator_422 (B2): JWT с AID, которого нет
// в реестре operators, но с RBAC-ролью, дающей soul.create. FK-violation на
// souls_created_by_aid_fk должна маппиться в чистый 422, а не opaque 500.
func TestIntegration_Soul_Create_UnknownCreator_422(t *testing.T) {
	truncateOperators(t)
	// archon-alice намеренно НЕ сидируется в operators — только в RBAC-config.

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

	// Осиротевшей souls-row не должно остаться (insert упал на FK).
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
// когда активного токена нет — recovery-сценарий из onboarding.md. Должно вернуть
// 200 + новый токен (ExpireActiveBySID → no-op, Insert проходит).
func TestIntegration_Soul_IssueToken_ForceNoActive_200(t *testing.T) {
	truncateOperators(t)
	seedOperator(t, "archon-alice", "")
	seedSoul(t, "web-01.example.com", "agent", "archon-alice")
	// Активный токен НЕ сидируется — force-recovery на голом pending-Soul-е.

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

	// expired_previous=false (нечего было истекать).
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

// TestIntegration_Soul_IssueToken_Concurrent (coverage gap 2): два одновременных
// issue-token на один SID без force → ровно 1 успех (200) + 1 отказ (409
// bootstrap-token-active), в БД ровно 1 активный токен. Защита — partial unique
// UNIQUE(sid) WHERE used_at IS NULL.
func TestIntegration_Soul_IssueToken_Concurrent(t *testing.T) {
	truncateOperators(t)
	seedOperator(t, "archon-alice", "")
	seedSoul(t, "web-01.example.com", "agent", "archon-alice")
	// Без предварительного активного токена: гонка двух чистых выписок.

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
	// Три Soul-а: agent/connected coven=[redis-prod], agent/pending coven=[redis-prod,cache],
	// ssh/pending coven=[edge]. Покрывают coven / status / transport фильтры.
	seedSoulFull(t, "redis-01.example.com", "agent", soul.StatusConnected, []string{"redis-prod"}, "archon-alice")
	seedSoulFull(t, "redis-02.example.com", "agent", soul.StatusPending, []string{"redis-prod", "cache"}, "archon-alice")
	seedSoulFull(t, "edge-01.example.com", "ssh", soul.StatusPending, []string{"edge"}, "archon-alice")

	base, stop := startServer(t, soulRBAC())
	defer stop()
	tok := newValidTokenFor(t, "archon-alice", []string{"cluster-admin"})

	// Без фильтра — 3.
	if total, items := listSouls(t, base, tok, ""); total != 3 || items != 3 {
		t.Errorf("no filter: total=%d items=%d, want 3/3", total, items)
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
	// Комбинация: agent + pending + coven=cache → 1 (redis-02).
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

// TestIntegration_Soul_List_NoSecretsLeak — list-response не должен отдавать
// fingerprint / token_hash и прочие секреты (DTO их не объявляет; проверяем
// через raw-map ключи).
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

	// RBAC без soul.list для archon-nobody.
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

// --- ADR-047 S3b-2a: keyset-режим souls-list на реальной PG (regex-scope) ---

// keysetPage — одна HTTP-страница keyset-режима souls-list (next_cursor /
// total_approximate). SID-ы вынимаем для проверки покрытия обхода.
type keysetPage struct {
	Items []struct {
		SID    string `json:"sid"`
		Status string `json:"status"`
	} `json:"items"`
	TotalApproximate bool    `json:"total_approximate"`
	NextCursor       *string `json:"next_cursor"`
}

// getSoulsPage — GET /v1/souls?<query> и decode в keysetPage. Падает на не-200.
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

// walkSouls — полный keyset-обход souls-list через next_cursor round-trip,
// собирает все SID-ы; падает на дубле или превышении лимита страниц. baseQuery —
// фильтры/limit (без cursor).
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

// regexScopeRBAC — роль с регекс-scoped soul.list (`on regex=<pat>`): оператор
// видит только SID, матчащие паттерн (keyset-режим, ADR-047 S3b-2a).
func regexScopeRBAC(aid, pattern string) *rbactest.Config {
	return &rbactest.Config{
		Roles: []rbactest.Role{
			{Name: "web-ops", Operators: []string{aid}, Permissions: []string{"soul.list on regex='" + pattern + "'"}},
		},
	}
}

// covenAndRegexScopeRBAC — две soul.list-permission: coven=<coven> + regex=<pat>.
// Видимость = union (OR): хост в coven ИЛИ матчащий regex (keyset-режим).
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

// TestIntegration_Soul_List_Keyset_RegexScope — keyset HTTP-путь на реальной PG:
// regex-scope `^web-`, флот web-*/db-* → обход через next_cursor собирает РОВНО
// web-* (без дублей/пропусков), total_approximate:true, добор работает (limit
// меньше числа web-хостов → многостраничный обход).
func TestIntegration_Soul_List_Keyset_RegexScope(t *testing.T) {
	// ADR-047 G1: route-gate переведён на RequireAction (existence-gate) —
	// regex-scoped оператор достигает handler-а (раньше rbac.Check(nil-ctx)
	// деньил scoped soul.list ДО handler-а). Достижимо через РЕАЛЬНЫЙ route-gate.
	truncateOperators(t)
	seedOperator(t, "archon-webops", "")
	// 3 web-* (видимы) + 2 db-* (вне scope). Разные registered_at —
	// seedSoulFull проставляет NOW(); порядок вставки задаёт DESC-порядок.
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

	// limit=2 < 3 web-хостов → многостраничный обход с добором.
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

// TestIntegration_Soul_List_Keyset_CovenRegexUnion — смешанный scope coven=prod
// + regex=^db- на реальной PG: видимость = union (OR). Хост-в-prod-не-db и
// хост-db-не-prod оба видны на page-by-page обходе; хост-ни-ни скрыт.
func TestIntegration_Soul_List_Keyset_CovenRegexUnion(t *testing.T) {
	// ADR-047 G1: route-gate RequireAction (existence-gate) пускает scoped-
	// оператора к handler-у; union OR + filter∩scope считаются в handler-е.
	truncateOperators(t)
	seedOperator(t, "archon-mixed", "")
	// app-01: prod, не db → виден по coven. db-01: staging, db-* → виден по regex.
	// db-02: prod, db-* → виден обоими. noise-01: staging, не db → скрыт.
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

// TestIntegration_Soul_List_Keyset_FilterIntersectsScope — BLOCKER-фикс на
// реальной PG: regex-scope `^web-` + `?status=connected` → видны ТОЛЬКО
// connected web-хосты (фильтр ∩ scope, AND). pending web-хост из scope скрыт
// фильтром. До фикса keyset-режим игнорировал бы query-фильтр.
func TestIntegration_Soul_List_Keyset_FilterIntersectsScope(t *testing.T) {
	// ADR-047 G1: route-gate RequireAction (existence-gate) пускает regex-scoped
	// оператора к handler-у; filter∩scope (AND) считается keyset-eval-ом.
	truncateOperators(t)
	seedOperator(t, "archon-webops", "")
	seedSoulFull(t, "web-01.example.com", "agent", soul.StatusConnected, []string{"prod"}, "archon-webops")
	time.Sleep(2 * time.Millisecond)
	seedSoulFull(t, "web-02.example.com", "agent", soul.StatusPending, []string{"prod"}, "archon-webops")
	time.Sleep(2 * time.Millisecond)
	seedSoulFull(t, "web-03.example.com", "agent", soul.StatusConnected, []string{"prod"}, "archon-webops")
	// db-вне-scope с connected — не должен просочиться даже под фильтр.
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

// --- ADR-047 gate-fix: scoped-операторы достигают handler через РЕАЛЬНЫЙ
// route-gate (NoSelector), scope деривируется из JWT, не из query. ---

// covenScopeRBAC — роль с coven-scoped soul.list (`on coven=<label>`): оператор
// видит только хосты своего coven. БЕЗ host/coven-контекста в query — gate
// (NoSelector) пускает, сужает handler.
func covenScopeRBAC(aid, coven string) *rbactest.Config {
	return &rbactest.Config{
		Roles: []rbactest.Role{
			{Name: "coven-ops", Operators: []string{aid}, Permissions: []string{"soul.list on coven=" + coven}},
		},
	}
}

// getSoulStatus — GET /v1/souls/{sid}, возвращает HTTP-статус (для scope-гейта
// single-read: 200 видим / 404 вне scope).
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

// TestIntegration_Soul_List_CovenScope_NoQuery_200 — guard #1 (G1, главный
// выигрыш): coven-scoped оператор делает GET /v1/souls БЕЗ ?coven= → 200 + только
// хосты своего coven (НЕ 403). Раньше scope-aware gate (RequirePermission) деньил
// coven-scoped без ?coven= (пустой context → селектор не сматчил → 403). G1 —
// RequireAction existence-gate пускает, сужает handler. Регресс = scoped-оператор
// снова 403 на собственный список (scoped-видимость через HTTP недостижима).
func TestIntegration_Soul_List_CovenScope_NoQuery_200(t *testing.T) {
	truncateOperators(t)
	seedOperator(t, "archon-coven", "")
	seedSoulFull(t, "prod-01.example.com", "agent", soul.StatusConnected, []string{"prod"}, "archon-coven")
	seedSoulFull(t, "prod-02.example.com", "agent", soul.StatusConnected, []string{"prod"}, "archon-coven")
	seedSoulFull(t, "stg-01.example.com", "agent", soul.StatusConnected, []string{"staging"}, "archon-coven")

	base, stop := startServer(t, covenScopeRBAC("archon-coven", "prod"))
	defer stop()
	tok := newValidTokenFor(t, "archon-coven", []string{"coven-ops"})

	// БЕЗ ?coven= — раньше 403, теперь 200 + ровно 2 prod-хоста (coven-pushdown).
	total, items := listSouls(t, base, tok, "")
	if total != 2 || items != 2 {
		t.Fatalf("coven-scoped list без ?coven=: total=%d items=%d, want 2/2 (только prod)", total, items)
	}
}

// TestIntegration_Soul_Get_CovenScope_InScope_200_OutOfScope_404 — guard #4/#6
// (gate-fix + list↔get): coven-scoped оператор читает хост своего coven по
// прямому GET /{sid} → 200; чужой coven → 404. list↔get консистентны.
func TestIntegration_Soul_Get_CovenScope(t *testing.T) {
	// ADR-047 G1: RequireAction existence-gate пускает coven-scoped к single-get;
	// handler-сужение (readScope/InScope coven-match → 200/404) режет видимость.
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
// + InScope OR-regex): regex-scoped оператор. Хост, видимый в List (regex-eval),
// доступен и по прямому GET /{sid} (200, не 404 — рассинхрон S3b-2a устранён);
// не-матчащий хост → 404. Это ключевой list↔get-консистентность guard.
func TestIntegration_Soul_Get_RegexScope_ListGetConsistency(t *testing.T) {
	// ADR-047 G1: RequireAction existence-gate пускает regex-scoped и на list, и
	// на single-get; list↔get консистентность (InScope OR-regex) считается handler-ом.
	truncateOperators(t)
	seedOperator(t, "archon-webops", "")
	seedSoulFull(t, "web-01.example.com", "agent", soul.StatusConnected, []string{"prod"}, "archon-webops")
	seedSoulFull(t, "db-01.example.com", "agent", soul.StatusConnected, []string{"prod"}, "archon-webops")

	base, stop := startServer(t, regexScopeRBAC("archon-webops", "^web-"))
	defer stop()
	tok := newValidTokenFor(t, "archon-webops", []string{"web-ops"})

	// web-01 виден в keyset-List…
	seen := walkSouls(t, base, tok, "limit=10")
	if _, ok := seen["web-01.example.com"]; !ok {
		t.Fatal("web-01 не виден в regex-scoped List — предусловие consistency-теста нарушено")
	}
	// …и доступен по прямому GET (раньше InScope coven-only → 404 на видимый).
	if code := getSoulStatus(t, base, tok, "web-01.example.com"); code != http.StatusOK {
		t.Errorf("GET web-01 (виден в List по regex ^web-) = %d, want 200 (list↔get консистентны)", code)
	}
	// db-01 НЕ матчит regex → 404 (вне Purview, не палит существование).
	if code := getSoulStatus(t, base, tok, "db-01.example.com"); code != http.StatusNotFound {
		t.Errorf("GET db-01 (не матчит ^web-) = %d, want 404", code)
	}
}

// TestIntegration_Soul_Get_403_NoPermission — guard #5 (security не-регресс):
// оператор БЕЗ soul.list получает 403 и на single-get (gate всё ещё требует
// держать permission — NoSelector ослабил scope-в-query, НЕ требование самого
// permission). Регресс = чтение детали без права.
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

// TestIntegration_Soul_Get_BareList_200 — guard #5 (bare-soul.list не сломан):
// unrestricted-оператор (bare soul.list) видит любой хост по single-get.
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

// getReadStatus — GET по произвольному read-souls-пути, возвращает HTTP-статус.
// Используется revoked/expired-guard-ами для всех четырёх read-роутов
// (list / {sid} / {sid}/soulprint / {sid}/history) единым helper-ом.
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

// newExpiredTokenFor выпускает УЖЕ ИСТЁКШИЙ JWT — для guard-а «revoked-фикс не
// сломал auth-слой»: expired-токен обязан давать 401 на read-souls (auth-слой ДО
// RBAC-gate), а не проскальзывать в revoked-семантику. Issue не даёт negative
// ttl, поэтому крафтим claims напрямую с exp в прошлом (как jwt/verifier_test).
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

// TestIntegration_Soul_Read_Revoked_403 — guard (ADR-047 G1 Фикс 2): revoked
// Архонт С АКТИВНОЙ ролью soul.list НЕ видит флот. ResolvePurview → Deny отрезает
// его в единой точке — на route-gate [RequireAction] (HoldsAction→Deny→false→403),
// который стоит ПЕРЕД handler-ом и ловит ВСЕ четыре read-роута единообразно:
// list / {sid} / soulprint / history → 403. handler-резолверы (readScope→Empty→
// InScope false → 404) остаются недостижимым backstop при revoked — gate срабатывает
// раньше; на покрытие 404-ветки от scope работают coven/regex-scope-тесты ниже.
// 403 vs 404 здесь не утечка: оба = «нет доступа», 403 даже строже (не различает
// существование). Контроль: та же роль БЕЗ revoked видит хост (НЕ 403).
func TestIntegration_Soul_Read_Revoked_403(t *testing.T) {
	truncateOperators(t)
	seedOperator(t, "archon-fired", "")
	seedSoulFull(t, "prod-01.example.com", "agent", soul.StatusConnected, []string{"prod"}, "archon-fired")

	// Контроль: НЕ-revoked с тем же bare soul.list видит хост.
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

	// Revoked: активная роль soul.list, но revoked_at выставлен.
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

// TestIntegration_Soul_Read_Expired_401 — guard (ADR-047 G1): revoked-фикс НЕ
// сломал auth-слой. Истёкший JWT даёт 401 на всех четырёх read-роутах ДО
// RBAC-gate (expired ≠ revoked: 401 от RequireJWT, а не 403/404 от scope).
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

// postCovenAssign — POST /v1/souls/coven с переданным телом, возвращает
// HTTP-статус. Используется coven-assign-revoked guard-ом (Пробел 2). dry_run
// в теле делает запрос не-мутирующим (CountBulkMatched без UPDATE) — happy-path
// 200 не зависит от наличия хостов.
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

// TestIntegration_Soul_CovenAssign_Revoked_401 — guard (ADR-047 G2, Пробел 2):
// revoked Архонт С АКТИВНОЙ ролью soul.coven-assign НЕ может массово менять
// Coven-метки. `POST /v1/souls/coven` — второй потребитель revoked-aware
// ResolvePurview (через handler scope-intersection), но первым срабатывает
// route-gate RequirePermission → scope-aware Check → ErrOperatorRevoked → 401
// (TypeOperatorRevokedToken, StatusUnauthorized), ДО handler-а. Service-слой
// ResolvePurview→Deny — недостижимый fail-closed backstop. Этот тест фиксирует
// фактическое корректное поведение (401, а НЕ 403/200), чтобы регресс
// revoked-shortcut в Check/ResolvePurview не открыл escalation молча. Контроль:
// та же роль БЕЗ revoked проходит gate и handler → 200 (dry_run).
//
// 401 здесь (а не 403 как на read-souls): mutate-роут ходит через scope-aware
// Check (revoked → ErrOperatorRevoked → 401-паритет expired-JWT), тогда как
// read-роуты — через existence-gate RequireAction (revoked → HoldsAction
// false → 403). Разный статус — разные gate-механизмы, оба fail-closed.
func TestIntegration_Soul_CovenAssign_Revoked_401(t *testing.T) {
	truncateOperators(t)
	seedOperator(t, "archon-fired", "")

	// append одной метки под selector all=true; dry_run → не-мутирующий
	// CountBulkMatched, happy-path 200 без зависимости от наличия хостов.
	body := `{"mode":"append","label":"prod","selector":{"all":true},"dry_run":true}`

	// Контроль: НЕ-revoked с активной ролью soul.coven-assign проходит gate +
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

	// Revoked: та же активная роль soul.coven-assign, но revoked_at выставлен.
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

// --- ADR-047 §г (S3b-G2): incarnations read-gate через РЕАЛЬНЫЙ router ---
//
// Тираж souls-G1-паттерна на incarnations: read-роуты (list/get/history)
// переведены с scope-aware RequirePermission(Multi) на existence-only
// RequireAction; сужение по scope — handler (resolveListScope / getInScope).
// Эти тесты бьют ПОЛНЫЙ router (route-gate + handler), а не handler напрямую —
// именно через route-gate проявлялась дыра (scoped-оператор ловил 403/deny ДО
// handler-а; unit-тесты по doIncList/h.History её не видели).

// seedIncarnationFull вставляет incarnation с произвольными covens + state (для
// coven/state-scoped read-gate-тестов). seedIncarnation оставлен минимальным.
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

// incCovenScopeRBAC — роль с coven-scoped incarnation read-правами (list+get+
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

// incStateScopeRBAC — роль с state-scoped incarnation read-правами (CEL по
// incarnation.state). Это измерение НЕ резолвится в request-контексте route-gate
// (state приходит только из строки БД), поэтому до Фикс 1/2 scope-aware gate
// деньил такого оператора в 403 ДО handler-а — главная латентная дыра G2.
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

// listIncarnations — GET /v1/incarnations?<query> → (total, len(items)). Падает
// на не-200.
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

// TestIntegration_Incarnation_List_StateScope_NoContext_200 — ГЛАВНЫЙ G2-выигрыш
// (Фикс 1): state-scoped оператор делает GET /v1/incarnations БЕЗ доп.контекста →
// 200 + только incarnation своего state-scope (НЕ 403). До Фикс 1 route-gate
// RequirePermission(NoSelector) → Check(aid,incarnation,list,nil) → state-
// измерение fail-closed → deny → 403 ДО handler-а. Регресс = scoped-оператор
// снова невидим через HTTP.
func TestIntegration_Incarnation_List_StateScope_NoContext_200(t *testing.T) {
	truncateOperators(t)
	seedOperator(t, "archon-state", "")
	// redis-8: state.redis_version=8.0 (в scope). redis-7: 7.2 (вне scope).
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

// TestIntegration_Incarnation_List_CovenScope_NoContext_200 — coven-scoped через
// HTTP: route-gate existence-only пускает, handler сужает coven-pushdown-ом → 200
// + только prod-incarnation (coven-scoped раньше тоже деньился NoSelector-gate-ом
// при пустом контексте, как state).
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

// getIncStatus — GET /v1/incarnations/{name} → HTTP-статус.
func getIncStatus(t *testing.T, base, tok, name string) int {
	t.Helper()
	return getReadStatus(t, base, tok, "/v1/incarnations/"+name)
}

// historyIncStatus — GET /v1/incarnations/{name}/history → HTTP-статус.
func historyIncStatus(t *testing.T, base, tok, name string) int {
	t.Helper()
	return getReadStatus(t, base, tok, "/v1/incarnations/"+name+"/history")
}

// TestIntegration_Incarnation_Get_StateScope — Фикс 2: state-scoped оператор
// читает get/history матчащей incarnation → 200; вне state-scope → 404. До Фикс 2
// route-gate RequirePermissionMulti(incScope) НЕ нёс state-измерение → deny → 403
// для оператора, который ДОЛЖЕН видеть incarnation. Теперь existence-gate +
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

// TestIntegration_Incarnation_Get_CovenScope — coven-scoped get/history: свой
// coven → 200, чужой → 404 (паритет souls Get_CovenScope; через полный router).
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

// TestIntegration_Incarnation_Read_Revoked — Фикс 3 (revoked-покрытие): revoked
// Архонт С АКТИВНОЙ ролью incarnation-read НЕ видит флот. Единая revoked-aware
// точка ResolvePurview→Deny отрезает на всех путях: route-gate (HoldsAction→Deny→
// false→403 для list/get/history) ПЕРЕД handler-ом. 403 на list, 403/404 на
// get/history — все = «нет доступа». Контроль: та же роль БЕЗ revoked видит.
func TestIntegration_Incarnation_Read_Revoked(t *testing.T) {
	truncateOperators(t)
	seedOperator(t, "archon-fired", "")
	seedIncarnationFull(t, "redis-prod", "redis", "archon-fired", []string{"prod"}, nil)

	// Контроль: НЕ-revoked с bare incarnation-read видит.
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

	// Revoked: те же активные права, но revoked_at выставлен.
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

	// list/get/history — все режутся route-gate-ом (HoldsAction→Deny→403). 403
	// vs 404 не утечка: оба = «нет доступа», 403 строже (не различает существование).
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

// TestIntegration_Incarnation_Read_Expired_401 — auth-слой не сломан: истёкший
// JWT даёт 401 на всех read-роутах ДО RBAC-gate (expired ≠ revoked: 401 от
// RequireJWT, а не 403/404 от scope).
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

// TestIntegration_Incarnation_Read_403_NoPermission — security не-регресс:
// оператор БЕЗ incarnation-read-прав получает 403 на list/get/history (gate всё
// ещё требует держать право — existence-gate ослабил scope-в-контексте, НЕ само
// требование permission).
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

// listSouls — helper: GET /v1/souls с опциональной query-строкой, возвращает
// (total, len(items)). Падает на не-200.
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

// seedSoulFull вставляет souls-row с произвольными status / coven (для
// list-фильтр-тестов). seedSoul оставлен для issue-token-тестов (минимальный).
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

// soulSelectStatus читает souls.status напрямую из БД.
func soulSelectStatus(t *testing.T, sid string) (string, error) {
	t.Helper()
	var status string
	err := integrationPool.QueryRow(context.Background(),
		`SELECT status FROM souls WHERE sid=$1`, sid).Scan(&status)
	return status, err
}

// seedSoul вставляет souls-row через CRUD для тестов issue-token.
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

// seedActiveToken вставляет активный bootstrap-токен и возвращает его token_id.
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

// bytesReader — helper для inline-JSON в request body. Wrapping
// strings.NewReader → io.ReadCloser, чтобы http.NewRequest принял.
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

	// Роль и permissions материализованы в БД.
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

	// Audit row + payload: name / created_by_aid / permissions присутствуют
	// (ADR-022: изменение авторизации обязательно аудируется).
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

	// Audit row: role.deleted с payload.name (ADR-022).
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
	// Replace-семантика: старый soul.list ушёл, два новых пришли.
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

	// Audit row: role.permissions-updated с payload.name + permissions (ADR-022).
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

	// Audit row: role.operator-granted с name / aid / granted_by_aid (ADR-022).
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

	// Audit row: role.operator-revoked с payload name / aid (ADR-022).
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

// TestIntegration_Role_403_AllOperations — оператор без role.<action>-
// permission получает 403 на КАЖДОЙ из шести role-операций (gap 2, REST-
// поверхность; create уже покрыт TestIntegration_Role_Create_403_NoPermission,
// здесь — list/delete/update/grant/revoke). RBAC даёт лишь soul.list →
// любой role.* → deny.
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

// TestIntegration_Role_SelfLockout — три lockout-мутации над последним
// `*`-путём (alice через единственную wildcard-роль) дают 409
// would-lock-out-cluster через полный роутер+middleware+реальный PG (gap 3);
// БД остаётся нетронутой (tx откатилась). enforcer-config (adminRBAC) даёт
// alice право на role.*; сама self-lockout-проверка читает rbac_*-таблицы.
func TestIntegration_Role_SelfLockout(t *testing.T) {
	base, stop := startServer(t, adminRBAC())
	defer stop()
	tok := newValidTokenFor(t, "archon-alice", []string{"cluster-admin"})

	// setup — единственный путь к `*`: alice через wildcard-role в rbac_*-таблицах.
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

// truncateServices чистит service_registry до чистого состояния для service.*-
// integration-тестов. FK service_registry → operators(aid): вызывается ПОСЛЕ
// truncateOperators (через CASCADE он не сносит service_registry-строки с
// created_by_aid=NULL, но новые тесты создают записи с FK на seed-операторов).
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

	// Запись материализована в БД с created_by_aid.
	var createdBy *string
	if err := integrationPool.QueryRow(context.Background(),
		`SELECT created_by_aid FROM service_registry WHERE name='web'`).Scan(&createdBy); err != nil {
		t.Fatalf("created_by: %v", err)
	}
	if createdBy == nil || *createdBy != "archon-alice" {
		t.Errorf("created_by_aid = %v, want archon-alice", createdBy)
	}

	// Audit row + payload {name, git, ref, created_by_aid}. git-URL не секрет.
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
	// 403 не аудируется как service.registered (операция не состоялась).
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

// Прямое использование health.Pinger из integration-теста — sanity check,
// что интерфейс совместим с реальными зависимостями.
var _ health.Pinger = poolPinger{}
var _ health.Pinger = (*keepervault.Client)(nil)
