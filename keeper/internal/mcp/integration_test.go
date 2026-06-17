//go:build integration

// Integration-тест MCP-сервера: реальный PG + Vault через testcontainers-go,
// listener на ephemeral port, end-to-end JSON-RPC через HTTP.
//
// Запуск:
//
//	cd keeper && SOUL_STACK_INTEGRATION_REQUIRE_DOCKER=1 \
//	    go test -tags=integration -race -count=1 ./internal/mcp/...

package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	vaultapi "github.com/hashicorp/vault/api"
	"github.com/jackc/pgx/v5/pgxpool"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	tcvault "github.com/testcontainers/testcontainers-go/modules/vault"

	"github.com/souls-guild/soul-stack/keeper/internal/applybus"
	"github.com/souls-guild/soul-stack/keeper/internal/auditpg"
	keeperjwt "github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/migrate"
	"github.com/souls-guild/soul-stack/keeper/internal/operator"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac/rbactest"
	keepervault "github.com/souls-guild/soul-stack/keeper/internal/vault"
	"github.com/souls-guild/soul-stack/keeper/migrations"
	"github.com/souls-guild/soul-stack/shared/audit"
	"github.com/souls-guild/soul-stack/shared/config"
)

const (
	integrationVaultToken = "root"
	integrationVaultImage = "hashicorp/vault:1.18"
	integrationPGImage    = "postgres:16-alpine"

	integrationSigningKey = "0123456789abcdef0123456789abcdef" // 32 bytes
	integrationIssuer     = "keeper.mcp.integration"
)

var (
	integrationPool *pgxpool.Pool
	integrationVC   *keepervault.Client
)

func TestMain(m *testing.M) { os.Exit(run(m)) }

func run(m *testing.M) int {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	pgCtr, err := tcpostgres.Run(ctx,
		integrationPGImage,
		tcpostgres.WithDatabase("keeper_mcp_test"),
		tcpostgres.WithUsername("keeper"),
		tcpostgres.WithPassword("keeper"),
		tcpostgres.BasicWaitStrategies(),
	)
	if err != nil {
		if requireDocker() {
			log.Fatalf("mcp integration: PG setup failed (REQUIRE_DOCKER): %v", err)
		}
		log.Printf("mcp integration: skipping, docker unavailable: %v", err)
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

func startMCPServer(t *testing.T, rbacCfg *rbactest.Config) (baseURL string, shutdown func()) {
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
	// RBAC-CRUD-фасад против тест-PG-пула — иначе role-tools диспатчатся, но
	// возвращают «role management is not configured» (gap 1). Симметрично REST
	// startServer, который прокидывает Deps.RBACSvc.
	rbacSvc, err := rbac.NewService(rbac.ServiceDeps{
		Pool:   integrationPool,
		Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("rbac.NewService: %v", err)
	}

	svc, err := operator.NewService(operator.ServiceDeps{
		Pool:       integrationPool,
		Issuer:     issuer,
		RBAC:       enforcer,
		TTLDefault: time.Hour,
		Logger:     slog.New(slog.NewJSONHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("operator.NewService: %v", err)
	}

	handler, err := NewHandler(HandlerDeps{
		OperatorSvc:   svc,
		RBAC:          enforcer,
		RBACRoles:     rbacSvc,
		AuditWriter:   auditpg.NewWriter(integrationPool),
		Logger:        slog.New(slog.NewJSONHandler(io.Discard, nil)),
		IncarnationDB: integrationPool,
	})
	if err != nil {
		t.Fatalf("mcp.NewHandler: %v", err)
	}

	srv, err := NewServer(config.KeeperListenSimple{Addr: "127.0.0.1:0"}, ServerDeps{
		JWTVerifier: verifier,
		Handler:     handler,
		Bus:         applybus.NewBus(slog.New(slog.NewJSONHandler(io.Discard, nil))),
		Logger:      slog.New(slog.NewJSONHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("mcp.NewServer: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Start(ctx) }()

	deadline := time.Now().Add(5 * time.Second)
	for {
		if addr := srv.Addr(); addr != "" && addr != "127.0.0.1:0" {
			baseURL = "http://" + addr
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("mcp server did not bind within 5s")
		}
		time.Sleep(10 * time.Millisecond)
	}

	shutdown = func() {
		cancel()
		select {
		case <-errCh:
		case <-time.After(15 * time.Second):
			t.Fatal("mcp server did not stop within 15s")
		}
	}
	return baseURL, shutdown
}

func newToken(t *testing.T, aid string, roles []string) string {
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

func truncateOperators(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	if _, err := integrationPool.Exec(ctx, `TRUNCATE operators, audit_log RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
}

// truncateRBAC чистит RBAC-таблицы (роли / permissions / membership) до
// чистого состояния для role.*-tool-integration-тестов. Вызывается ПОСЛЕ
// truncateOperators (тот через CASCADE сносит лишь membership, ссылающийся на
// operators; builtin cluster-admin из seed-миграции 027 переживает CASCADE).
// CASCADE на rbac_roles снимает permissions + membership (ON DELETE CASCADE).
func truncateRBAC(t *testing.T) {
	t.Helper()
	if _, err := integrationPool.Exec(context.Background(),
		`TRUNCATE rbac_roles, rbac_role_permissions, rbac_role_operators RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncate rbac: %v", err)
	}
}

// seedRole создаёт роль с набором permissions напрямую в БД (минуя tool) —
// нужен тестам Delete/Update/Grant/Revoke и self-lockout-кейсам.
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

// seedRoleMember привязывает AID к роли напрямую в БД (granted_by_aid NULL).
func seedRoleMember(t *testing.T, roleName, aid string) {
	t.Helper()
	if _, err := integrationPool.Exec(context.Background(),
		`INSERT INTO rbac_role_operators (role_name, aid) VALUES ($1, $2)`, roleName, aid); err != nil {
		t.Fatalf("seedRoleMember(%s, %s): %v", roleName, aid, err)
	}
}

// seedClusterAdmin выдаёт caller-у membership builtin-роли cluster-admin (`*`)
// в rbac_*-таблицах БД (модель-C). Config-RBAC enforcer (rbacAllRoleAdmin) лишь
// пропускает caller-а через permission-gate; сам rbac.Service делает subset-check
// (least-privilege) по РЕАЛЬНОЙ membership из rbac_role_operators. Без membership
// caller держит 0 эффективных permissions → subset-check отказывает в create/
// grant вместо ожидаемого доменного исхода. truncateRBAC сносит роли — ре-сидим
// cluster-admin с `*` идемпотентно.
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

// doRPC отправляет JSON-RPC request и декодирует response.
func doRPC(t *testing.T, baseURL, token string, payload any) jsonRPCResponse {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	authHeader := ""
	if token != "" {
		authHeader = "Bearer " + token
	}
	status, raw := doRawHTTP(t, baseURL+"/mcp", authHeader, body)
	if status == http.StatusNoContent {
		t.Fatalf("doRPC got 204 (notification); use doRawHTTP for notification tests")
	}
	var out jsonRPCResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal response: %v; body=%s", err, raw)
	}
	return out
}

// doRawHTTP — низкоуровневый POST /mcp без JSON-RPC-декодирования.
// Используется для тестов, проверяющих HTTP-уровень: 204 на notification,
// 413 / 400 на превышение лимита, и т.п. authHeader — целиком (например,
// `Bearer xyz`, `bearer xyz`, `XBearer broken`); пустая строка = без
// Authorization-header-а.
func doRawHTTP(t *testing.T, url, authHeader string, body []byte) (status int, raw []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	raw, err = io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	return resp.StatusCode, raw
}

func TestIntegration_NoAuth_Unauthenticated(t *testing.T) {
	base, stop := startMCPServer(t, nil)
	defer stop()

	resp := doRPC(t, base, "", map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/list",
	})
	if resp.Error == nil || resp.Error.Code != rpcCodeInvalidRequest {
		t.Errorf("Error = %+v, want InvalidRequest", resp.Error)
	}
}

// TestIntegration_ToolsList_All — tools/list возвращает РОВНО столько tool-ов,
// сколько объявлено в catalogManifest (single source of truth). Ожидаемое число
// берётся динамически (len(catalogManifest)), не литералом — иначе тест дрейфует
// при каждом добавлении tool-а в манифест.
func TestIntegration_ToolsList_All(t *testing.T) {
	base, stop := startMCPServer(t, nil)
	defer stop()
	token := newToken(t, "archon-alice", []string{"cluster-admin"})

	resp := doRPC(t, base, token, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/list",
	})
	if resp.Error != nil {
		t.Fatalf("Error = %+v", resp.Error)
	}
	var res toolsListResult
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if want := len(catalogManifest); len(res.Tools) != want {
		t.Errorf("tool count = %d, want %d (len(catalogManifest))", len(res.Tools), want)
	}
}

func TestIntegration_OperatorCreate_E2E(t *testing.T) {
	truncateOperators(t)
	seedOperator(t, "archon-alice", "")
	base, stop := startMCPServer(t, &rbactest.Config{
		Roles: []rbactest.Role{
			{Name: "creator", Operators: []string{"archon-alice"}, Permissions: []string{"operator.create"}},
		},
	})
	defer stop()
	token := newToken(t, "archon-alice", []string{"creator"})

	resp := doRPC(t, base, token, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{
			"name": "keeper.operator.create",
			"arguments": map[string]any{
				"aid":          "archon-bob",
				"display_name": "Bob",
			},
		},
	})
	if resp.Error != nil {
		t.Fatalf("Error = %+v", resp.Error)
	}
	var res toolsCallResult
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	var out operatorCreateOutput
	if err := json.Unmarshal(res.StructuredContent, &out); err != nil {
		t.Fatalf("unmarshal structured: %v", err)
	}
	if out.AID != "archon-bob" {
		t.Errorf("AID = %q", out.AID)
	}
	if out.JWT == "" {
		t.Error("JWT empty")
	}

	// БД: оператор реально создан.
	op, err := operator.SelectByAID(context.Background(), integrationPool, "archon-bob")
	if err != nil {
		t.Fatalf("SelectByAID: %v", err)
	}
	if op.CreatedByAID == nil || *op.CreatedByAID != "archon-alice" {
		t.Errorf("CreatedByAID = %v", op.CreatedByAID)
	}

	// audit_log получил event operator.created с source='mcp' (ADR-022(b)).
	var count int
	row := integrationPool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM audit_log WHERE event_type='operator.created' AND source=$1`,
		string(audit.SourceMCP))
	if err := row.Scan(&count); err != nil {
		t.Fatalf("count audit: %v", err)
	}
	if count != 1 {
		t.Errorf("audit count (source=mcp) = %d, want 1", count)
	}
}

func TestIntegration_OperatorCreate_RBACForbidden(t *testing.T) {
	truncateOperators(t)
	seedOperator(t, "archon-alice", "")
	// RBAC: alice не имеет permission `operator.create`.
	base, stop := startMCPServer(t, &rbactest.Config{
		Roles: []rbactest.Role{
			{Name: "reader", Operators: []string{"archon-alice"}, Permissions: []string{"soul.list"}},
		},
	})
	defer stop()
	token := newToken(t, "archon-alice", []string{"reader"})

	resp := doRPC(t, base, token, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{
			"name": "keeper.operator.create",
			"arguments": map[string]any{
				"aid": "archon-bob", "display_name": "Bob",
			},
		},
	})
	if resp.Error == nil {
		t.Fatal("expected error")
	}
	// data.code должен быть forbidden.
	rawData, _ := json.Marshal(resp.Error.Data)
	var data mcpToolError
	if err := json.Unmarshal(rawData, &data); err != nil {
		t.Fatalf("unmarshal data: %v", err)
	}
	if data.Code != mcpCodeForbidden {
		t.Errorf("data.code = %q, want forbidden", data.Code)
	}
}

func TestIntegration_Initialize(t *testing.T) {
	base, stop := startMCPServer(t, nil)
	defer stop()
	token := newToken(t, "archon-alice", []string{"cluster-admin"})

	resp := doRPC(t, base, token, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "initialize",
	})
	if resp.Error != nil {
		t.Fatalf("Error = %+v", resp.Error)
	}
	var res initializeResult
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if res.ServerInfo.Name != serverInfoName {
		t.Errorf("ServerInfo.Name = %q", res.ServerInfo.Name)
	}
}

func TestIntegration_OperatorIssueToken_E2E(t *testing.T) {
	truncateOperators(t)
	seedOperator(t, "archon-alice", "")
	seedOperator(t, "archon-bob", "archon-alice")
	base, stop := startMCPServer(t, &rbactest.Config{
		Roles: []rbactest.Role{
			{Name: "issuer", Operators: []string{"archon-alice"}, Permissions: []string{"operator.issue-token"}},
		},
	})
	defer stop()
	token := newToken(t, "archon-alice", []string{"issuer"})

	resp := doRPC(t, base, token, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{
			"name":      "keeper.operator.issue-token",
			"arguments": map[string]any{"aid": "archon-bob"},
		},
	})
	if resp.Error != nil {
		t.Fatalf("Error = %+v", resp.Error)
	}
	var res toolsCallResult
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	var out operatorIssueTokenOutput
	if err := json.Unmarshal(res.StructuredContent, &out); err != nil {
		t.Fatalf("unmarshal structured: %v", err)
	}
	if out.JWT == "" {
		t.Error("JWT empty")
	}
}

func TestIntegration_StubToolReturnsNotImplemented(t *testing.T) {
	truncateOperators(t)
	seedOperator(t, "archon-alice", "")
	base, stop := startMCPServer(t, &rbactest.Config{
		Roles: []rbactest.Role{
			{Name: "ops", Operators: []string{"archon-alice"}, Permissions: []string{"*"}},
		},
	})
	defer stop()
	token := newToken(t, "archon-alice", []string{"ops"})

	// incarnation-tools все toolStatusImplemented (тираж C7 + destroy S-D4) — с
	// nil deps в harness они отдали бы internal-error, а не not-implemented.
	// push.apply теперь реализован (Variant C orchestrator) — берём реально-stub
	// tool из manifest.go (toolStatusStub): push.cleanup.
	resp := doRPC(t, base, token, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{
			"name": "keeper.push.cleanup",
			"arguments": map[string]any{
				"sid": "web-01",
			},
		},
	})
	if resp.Error == nil {
		t.Fatal("expected error")
	}
	rawData, _ := json.Marshal(resp.Error.Data)
	var data mcpToolError
	if err := json.Unmarshal(rawData, &data); err != nil {
		t.Fatalf("unmarshal data: %v", err)
	}
	if data.Code != mcpCodeNotImplemented {
		t.Errorf("data.code = %q, want not-implemented", data.Code)
	}
}

// TestIntegration_Notification_204 — notification (request без id) на
// HTTP-уровне даёт 204 No Content и пустое тело, по spec JSON-RPC 2.0 §4.1.
func TestIntegration_Notification_204(t *testing.T) {
	base, stop := startMCPServer(t, nil)
	defer stop()
	token := newToken(t, "archon-alice", []string{"cluster-admin"})

	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"method":  "notifications/initialized",
	})
	status, raw := doRawHTTP(t, base+"/mcp", "Bearer "+token, body)
	if status != http.StatusNoContent {
		t.Errorf("status = %d, want 204; body=%s", status, raw)
	}
	if len(raw) != 0 {
		t.Errorf("notification response body = %q, want empty", raw)
	}
}

// TestIntegration_Notification_IDNull — id: null равносильно отсутствию
// id (JSON-RPC §4.1 в комбинации с MCP-spec) → 204.
func TestIntegration_Notification_IDNull(t *testing.T) {
	base, stop := startMCPServer(t, nil)
	defer stop()
	token := newToken(t, "archon-alice", []string{"cluster-admin"})

	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      nil,
		"method":  "tools/list",
	})
	status, _ := doRawHTTP(t, base+"/mcp", "Bearer "+token, body)
	if status != http.StatusNoContent {
		t.Errorf("status = %d, want 204 (id=null is notification)", status)
	}
}

// TestIntegration_Batch_Rejected — batch JSON-RPC (массив) явно отвергается
// в MVP с понятным сообщением, чтобы клиент сразу видел отказ.
func TestIntegration_Batch_Rejected(t *testing.T) {
	base, stop := startMCPServer(t, nil)
	defer stop()
	token := newToken(t, "archon-alice", []string{"cluster-admin"})

	body := []byte(`[{"jsonrpc":"2.0","id":1,"method":"tools/list"}]`)
	status, raw := doRawHTTP(t, base+"/mcp", "Bearer "+token, body)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", status, raw)
	}
	var resp jsonRPCResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("unmarshal: %v; body=%s", err, raw)
	}
	if resp.Error == nil || resp.Error.Code != rpcCodeInvalidRequest {
		t.Errorf("Error = %+v, want InvalidRequest", resp.Error)
	}
	if resp.Error != nil && !strings.Contains(resp.Error.Message, "batch") {
		t.Errorf("Error.Message = %q, want substring 'batch'", resp.Error.Message)
	}
}

// TestIntegration_BodyTooLarge — body >1 MiB режется MaxBytesReader, и
// парсер JSON-RPC возвращает ParseError.
func TestIntegration_BodyTooLarge(t *testing.T) {
	base, stop := startMCPServer(t, nil)
	defer stop()
	token := newToken(t, "archon-alice", []string{"cluster-admin"})

	// Превышаем mcpMaxBodyBytes (1 MiB) — заполняем `params` мусором.
	big := make([]byte, mcpMaxBodyBytes+128)
	for i := range big {
		big[i] = 'x'
	}
	body := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"junk":"` + string(big) + `"}}`)
	status, raw := doRawHTTP(t, base+"/mcp", "Bearer "+token, body)
	// MaxBytesReader даёт читателю ошибку; обрабатывается либо как ParseError
	// (если первые байты прочлись и затем оборвалось), либо 400-уровень.
	// Главное — НЕ 200/success и НЕ panic.
	if status == http.StatusNoContent {
		t.Fatalf("status = 204, want non-success; body=%s", raw)
	}
	if status == http.StatusOK {
		var resp jsonRPCResponse
		if err := json.Unmarshal(raw, &resp); err != nil {
			t.Fatalf("unmarshal: %v; body=%s", err, raw)
		}
		if resp.Error == nil {
			t.Errorf("expected JSON-RPC error for oversize body, got success: %s", raw)
		}
		if resp.Error != nil && resp.Error.Code != rpcCodeParseError &&
			resp.Error.Code != rpcCodeInvalidRequest {
			t.Errorf("Error.Code = %d, want ParseError/InvalidRequest", resp.Error.Code)
		}
	}
}

// TestIntegration_EmptyBody — POST с пустым телом → JSON-RPC InvalidRequest.
func TestIntegration_EmptyBody(t *testing.T) {
	base, stop := startMCPServer(t, nil)
	defer stop()
	token := newToken(t, "archon-alice", []string{"cluster-admin"})

	status, raw := doRawHTTP(t, base+"/mcp", "Bearer "+token, []byte{})
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (JSON-RPC over HTTP); body=%s", status, raw)
	}
	var resp jsonRPCResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("unmarshal: %v; body=%s", err, raw)
	}
	if resp.Error == nil || resp.Error.Code != rpcCodeInvalidRequest {
		t.Errorf("Error = %+v, want InvalidRequest", resp.Error)
	}
}

// TestIntegration_BearerCaseInsensitive — RFC 6750 §2.1: scheme сравнивается
// case-insensitive. `bearer <token>` и `BEARER <token>` должны проходить.
func TestIntegration_BearerCaseInsensitive(t *testing.T) {
	base, stop := startMCPServer(t, nil)
	defer stop()
	token := newToken(t, "archon-alice", []string{"cluster-admin"})

	for _, scheme := range []string{"bearer", "Bearer", "BEARER", "BeArEr"} {
		scheme := scheme
		t.Run(scheme, func(t *testing.T) {
			body, _ := json.Marshal(map[string]any{
				"jsonrpc": "2.0", "id": 1, "method": "tools/list",
			})
			status, raw := doRawHTTP(t, base+"/mcp", scheme+" "+token, body)
			if status != http.StatusOK {
				t.Fatalf("status = %d for scheme %q; body=%s", status, scheme, raw)
			}
			var resp jsonRPCResponse
			if err := json.Unmarshal(raw, &resp); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if resp.Error != nil {
				t.Errorf("scheme %q got error: %+v", scheme, resp.Error)
			}
		})
	}
}

// TestIntegration_IDVariants_Echo — id может быть number / string / 0,
// каждый возвращается в response как есть (echo).
func TestIntegration_IDVariants_Echo(t *testing.T) {
	base, stop := startMCPServer(t, nil)
	defer stop()
	token := newToken(t, "archon-alice", []string{"cluster-admin"})

	cases := []struct {
		name string
		id   any
		want string
	}{
		{"int_zero", 0, "0"},
		{"int_positive", 42, "42"},
		{"string", "req-abc", `"req-abc"`},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			body, _ := json.Marshal(map[string]any{
				"jsonrpc": "2.0", "id": tc.id, "method": "tools/list",
			})
			status, raw := doRawHTTP(t, base+"/mcp", "Bearer "+token, body)
			if status != http.StatusOK {
				t.Fatalf("status = %d; body=%s", status, raw)
			}
			var resp jsonRPCResponse
			if err := json.Unmarshal(raw, &resp); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if string(resp.ID) != tc.want {
				t.Errorf("id echo = %s, want %s", resp.ID, tc.want)
			}
		})
	}
}

// TestIntegration_ToolsCall_ArgumentsWrongType — `arguments` обязан быть
// JSON-объектом; string / number / null отдаются с MCP-error
// malformed-request (strictUnmarshal) или validation-failed (если empty).
func TestIntegration_ToolsCall_ArgumentsWrongType(t *testing.T) {
	truncateOperators(t)
	seedOperator(t, "archon-alice", "")
	base, stop := startMCPServer(t, &rbactest.Config{
		Roles: []rbactest.Role{
			{Name: "creator", Operators: []string{"archon-alice"}, Permissions: []string{"operator.create"}},
		},
	})
	defer stop()
	token := newToken(t, "archon-alice", []string{"creator"})

	cases := []struct {
		name     string
		args     json.RawMessage
		wantCode string
	}{
		{"string", json.RawMessage(`"abc"`), mcpCodeMalformedRequest},
		{"number", json.RawMessage(`42`), mcpCodeMalformedRequest},
		{"null_arguments", json.RawMessage(`null`), mcpCodeValidationFailed},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			body, _ := json.Marshal(map[string]any{
				"jsonrpc": "2.0", "id": 1, "method": "tools/call",
				"params": map[string]any{
					"name":      "keeper.operator.create",
					"arguments": tc.args,
				},
			})
			status, raw := doRawHTTP(t, base+"/mcp", "Bearer "+token, body)
			if status != http.StatusOK {
				t.Fatalf("status = %d; body=%s", status, raw)
			}
			var resp jsonRPCResponse
			if err := json.Unmarshal(raw, &resp); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if resp.Error == nil {
				t.Fatalf("expected error for args=%s", tc.args)
			}
			rawData, _ := json.Marshal(resp.Error.Data)
			var data mcpToolError
			if err := json.Unmarshal(rawData, &data); err != nil {
				t.Fatalf("unmarshal data: %v", err)
			}
			if data.Code != tc.wantCode {
				t.Errorf("args=%s: data.code = %q, want %q", tc.args, data.Code, tc.wantCode)
			}
		})
	}
}

// ============================ role.* tools (RBAC Slice 2b) — integration ============================
//
// Полный E2E role.*-tool через реальный PG: harness прокидывает rbac.Service
// (startMCPServer выше), tool-вызовы идут через JSON-RPC tools/call. Проверяем,
// что транспорт+Service+таблицы согласованы — паритет с REST role-integration
// (api/integration_test.go § /v1/roles).

// callRoleToolOK — tools/call с пустой ролью alice→all-role-permissions через
// enforcer (rbacAllRoleAdmin). Возвращает jsonRPCResponse. Сам RBAC-config
// проверяется отдельными forbidden-тестами; здесь enforcer заведомо разрешает.
func rbacAllRoleAdmin() *rbactest.Config {
	return &rbactest.Config{
		Roles: []rbactest.Role{
			{Name: "role-admin", Operators: []string{"archon-alice"}, Permissions: []string{
				"role.create", "role.delete", "role.list", "role.update",
				"role.grant-operator", "role.revoke-operator",
			}},
		},
	}
}

// callRoleTool — tools/call по имени tool-а с inline-arguments, возвращает
// декодированный JSON-RPC response.
func callRoleTool(t *testing.T, base, token, tool string, args map[string]any) jsonRPCResponse {
	t.Helper()
	return doRPC(t, base, token, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": tool, "arguments": args},
	})
}

// mcpErrCode извлекает data.code из error-ответа tools/call.
func mcpErrCode(t *testing.T, resp jsonRPCResponse) string {
	t.Helper()
	if resp.Error == nil {
		t.Fatalf("expected error response, got result=%s", resp.Result)
	}
	rawData, _ := json.Marshal(resp.Error.Data)
	var data mcpToolError
	if err := json.Unmarshal(rawData, &data); err != nil {
		t.Fatalf("unmarshal data: %v", err)
	}
	return data.Code
}

// rbacRoleCount / rbacPermCount / rbacMemberCount — point-пробы rbac_*-таблиц.
func rbacRoleCount(t *testing.T, name string) int64 {
	t.Helper()
	var n int64
	if err := integrationPool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM rbac_roles WHERE name=$1`, name).Scan(&n); err != nil {
		t.Fatalf("rbacRoleCount(%s): %v", name, err)
	}
	return n
}

func rbacPermCount(t *testing.T, name string) int64 {
	t.Helper()
	var n int64
	if err := integrationPool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM rbac_role_permissions WHERE role_name=$1`, name).Scan(&n); err != nil {
		t.Fatalf("rbacPermCount(%s): %v", name, err)
	}
	return n
}

func rbacMemberCount(t *testing.T, name, aid string) int64 {
	t.Helper()
	var n int64
	if err := integrationPool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM rbac_role_operators WHERE role_name=$1 AND aid=$2`, name, aid).Scan(&n); err != nil {
		t.Fatalf("rbacMemberCount(%s,%s): %v", name, aid, err)
	}
	return n
}

// TestIntegration_RoleTool_HappyCycle — полный цикл через реальный PG:
// create → list → grant-operator → revoke-operator → delete, с проверкой
// rbac_*-таблиц после каждого шага.
func TestIntegration_RoleTool_HappyCycle(t *testing.T) {
	truncateOperators(t)
	truncateRBAC(t)
	seedOperator(t, "archon-alice", "")
	seedClusterAdmin(t, "archon-alice")
	seedOperator(t, "archon-bob", "archon-alice")

	base, stop := startMCPServer(t, rbacAllRoleAdmin())
	defer stop()
	token := newToken(t, "archon-alice", []string{"role-admin"})

	// create.
	resp := callRoleTool(t, base, token, "keeper.role.create", map[string]any{
		"name": "ops", "description": "ops team", "permissions": []string{"soul.list", "incarnation.get"},
	})
	if resp.Error != nil {
		t.Fatalf("create: %+v", resp.Error)
	}
	if rbacRoleCount(t, "ops") != 1 {
		t.Fatalf("role 'ops' not materialized")
	}
	if rbacPermCount(t, "ops") != 2 {
		t.Errorf("ops permissions = %d, want 2", rbacPermCount(t, "ops"))
	}
	// created_by_aid = caller.
	var createdBy *string
	if err := integrationPool.QueryRow(context.Background(),
		`SELECT created_by_aid FROM rbac_roles WHERE name='ops'`).Scan(&createdBy); err != nil {
		t.Fatalf("created_by probe: %v", err)
	}
	if createdBy == nil || *createdBy != "archon-alice" {
		t.Errorf("created_by_aid = %v, want archon-alice", createdBy)
	}

	// list — роль видна с развёрнутыми permissions.
	resp = callRoleTool(t, base, token, "keeper.role.list", map[string]any{})
	if resp.Error != nil {
		t.Fatalf("list: %+v", resp.Error)
	}
	var lres toolsCallResult
	if err := json.Unmarshal(resp.Result, &lres); err != nil {
		t.Fatalf("unmarshal list result: %v", err)
	}
	var out roleListOutput
	if err := json.Unmarshal(lres.StructuredContent, &out); err != nil {
		t.Fatalf("unmarshal structured: %v", err)
	}
	var opsView *roleView
	for i := range out.Roles {
		if out.Roles[i].Name == "ops" {
			opsView = &out.Roles[i]
		}
	}
	if opsView == nil {
		t.Fatalf("role 'ops' missing in list: %+v", out.Roles)
	}
	if len(opsView.Permissions) != 2 {
		t.Errorf("ops permissions in list = %v, want 2", opsView.Permissions)
	}

	// grant-operator → membership-строка с granted_by_aid=caller.
	resp = callRoleTool(t, base, token, "keeper.role.grant-operator", map[string]any{
		"role": "ops", "aid": "archon-bob",
	})
	if resp.Error != nil {
		t.Fatalf("grant: %+v", resp.Error)
	}
	if rbacMemberCount(t, "ops", "archon-bob") != 1 {
		t.Fatalf("membership ops→archon-bob not created")
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

	// revoke-operator → membership снят.
	resp = callRoleTool(t, base, token, "keeper.role.revoke-operator", map[string]any{
		"role": "ops", "aid": "archon-bob",
	})
	if resp.Error != nil {
		t.Fatalf("revoke: %+v", resp.Error)
	}
	if rbacMemberCount(t, "ops", "archon-bob") != 0 {
		t.Errorf("membership still present after revoke")
	}

	// delete → роль снесена каскадом.
	resp = callRoleTool(t, base, token, "keeper.role.delete", map[string]any{"name": "ops"})
	if resp.Error != nil {
		t.Fatalf("delete: %+v", resp.Error)
	}
	if rbacRoleCount(t, "ops") != 0 {
		t.Errorf("role 'ops' still present after delete")
	}
	if rbacPermCount(t, "ops") != 0 {
		t.Errorf("permissions not cascade-deleted")
	}
}

// TestIntegration_RoleTool_UpdatePermissions — replace-семантика через реальный
// PG: старый набор снят, новый материализован.
func TestIntegration_RoleTool_UpdatePermissions(t *testing.T) {
	truncateOperators(t)
	truncateRBAC(t)
	seedOperator(t, "archon-alice", "")
	seedClusterAdmin(t, "archon-alice")
	seedRole(t, "ops", false, "soul.list")

	base, stop := startMCPServer(t, rbacAllRoleAdmin())
	defer stop()
	token := newToken(t, "archon-alice", []string{"role-admin"})

	resp := callRoleTool(t, base, token, "keeper.role.update", map[string]any{
		"name": "ops", "permissions": []string{"incarnation.get", "incarnation.list"},
	})
	if resp.Error != nil {
		t.Fatalf("update: %+v", resp.Error)
	}
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
}

// TestIntegration_RoleTool_SelfLockout — каждая из трёх lockout-мутаций над
// последним `*`-путём (alice через единственную wildcard-роль) даёт
// would-lock-out-cluster через реальный tool-вызов; БД остаётся нетронутой
// (tx откатилась). Паритет с rbac-self-lockout-матрицей (gap 1 + gap 4).
func TestIntegration_RoleTool_SelfLockout(t *testing.T) {
	base, stop := startMCPServer(t, rbacAllRoleAdmin())
	defer stop()
	token := newToken(t, "archon-alice", []string{"role-admin"})

	// setup — единственный путь к `*`: alice через wildcard-role.
	setup := func() {
		truncateOperators(t)
		truncateRBAC(t)
		seedOperator(t, "archon-alice", "")
		seedRole(t, "wildcard-role", false, "*")
		seedRoleMember(t, "wildcard-role", "archon-alice")
	}

	t.Run("delete", func(t *testing.T) {
		setup()
		resp := callRoleTool(t, base, token, "keeper.role.delete", map[string]any{"name": "wildcard-role"})
		if code := mcpErrCode(t, resp); code != mcpCodeWouldLockOutCluster {
			t.Errorf("code = %q, want would-lock-out-cluster", code)
		}
		if rbacRoleCount(t, "wildcard-role") != 1 {
			t.Error("role deleted despite lockout (tx not rolled back)")
		}
	})

	t.Run("update-remove-wildcard", func(t *testing.T) {
		setup()
		resp := callRoleTool(t, base, token, "keeper.role.update", map[string]any{
			"name": "wildcard-role", "permissions": []string{"soul.list"},
		})
		if code := mcpErrCode(t, resp); code != mcpCodeWouldLockOutCluster {
			t.Errorf("code = %q, want would-lock-out-cluster", code)
		}
		// `*` остался — tx откатилась.
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
		resp := callRoleTool(t, base, token, "keeper.role.revoke-operator", map[string]any{
			"role": "wildcard-role", "aid": "archon-alice",
		})
		if code := mcpErrCode(t, resp); code != mcpCodeWouldLockOutCluster {
			t.Errorf("code = %q, want would-lock-out-cluster", code)
		}
		if rbacMemberCount(t, "wildcard-role", "archon-alice") != 1 {
			t.Error("membership revoked despite lockout (tx not rolled back)")
		}
	})
}

// TestIntegration_RoleTool_ErrorMapping — sentinel-ошибки rbac.Service через
// реальный tool-вызов маппятся в стабильные data.code (паритет REST problem-type).
func TestIntegration_RoleTool_ErrorMapping(t *testing.T) {
	base, stop := startMCPServer(t, rbacAllRoleAdmin())
	defer stop()
	token := newToken(t, "archon-alice", []string{"role-admin"})

	t.Run("create-already-exists", func(t *testing.T) {
		truncateOperators(t)
		truncateRBAC(t)
		seedOperator(t, "archon-alice", "")
		seedClusterAdmin(t, "archon-alice")
		seedRole(t, "ops", false, "soul.list")
		resp := callRoleTool(t, base, token, "keeper.role.create", map[string]any{
			"name": "ops", "permissions": []string{"soul.list"},
		})
		if code := mcpErrCode(t, resp); code != mcpCodeRoleExists {
			t.Errorf("code = %q, want role-already-exists", code)
		}
	})

	t.Run("create-bad-permission", func(t *testing.T) {
		truncateOperators(t)
		truncateRBAC(t)
		seedOperator(t, "archon-alice", "")
		resp := callRoleTool(t, base, token, "keeper.role.create", map[string]any{
			"name": "ops", "permissions": []string{"keeper.incarnation.get"},
		})
		if code := mcpErrCode(t, resp); code != mcpCodeValidationFailed {
			t.Errorf("code = %q, want validation-failed", code)
		}
		if rbacRoleCount(t, "ops") != 0 {
			t.Error("role created despite bad permission (validation must be pre-tx)")
		}
	})

	t.Run("delete-not-found", func(t *testing.T) {
		truncateOperators(t)
		truncateRBAC(t)
		seedOperator(t, "archon-alice", "")
		resp := callRoleTool(t, base, token, "keeper.role.delete", map[string]any{"name": "ghost"})
		if code := mcpErrCode(t, resp); code != mcpCodeNotFound {
			t.Errorf("code = %q, want not-found", code)
		}
	})

	t.Run("delete-builtin", func(t *testing.T) {
		truncateOperators(t)
		truncateRBAC(t)
		seedOperator(t, "archon-alice", "")
		seedRole(t, "cluster-admin", true, "*")
		resp := callRoleTool(t, base, token, "keeper.role.delete", map[string]any{"name": "cluster-admin"})
		if code := mcpErrCode(t, resp); code != mcpCodeRoleBuiltin {
			t.Errorf("code = %q, want role-builtin", code)
		}
	})

	t.Run("grant-operator-not-found", func(t *testing.T) {
		truncateOperators(t)
		truncateRBAC(t)
		seedOperator(t, "archon-alice", "")
		seedClusterAdmin(t, "archon-alice")
		seedRole(t, "ops", false, "soul.list")
		resp := callRoleTool(t, base, token, "keeper.role.grant-operator", map[string]any{
			"role": "ops", "aid": "archon-ghost",
		})
		if code := mcpErrCode(t, resp); code != mcpCodeNotFound {
			t.Errorf("code = %q, want not-found", code)
		}
	})

	t.Run("revoke-operator-not-found", func(t *testing.T) {
		truncateOperators(t)
		truncateRBAC(t)
		seedOperator(t, "archon-alice", "")
		seedRole(t, "ops", false, "soul.list")
		resp := callRoleTool(t, base, token, "keeper.role.revoke-operator", map[string]any{
			"role": "ops", "aid": "archon-alice",
		})
		if code := mcpErrCode(t, resp); code != mcpCodeNotFound {
			t.Errorf("code = %q, want not-found", code)
		}
	})
}

// TestIntegration_RoleTool_403_AllOperations — оператор без role.<action>-
// permission получает forbidden на КАЖДОЙ из шести role-операций (gap 2,
// MCP-поверхность). RBAC-config даёт лишь soul.list → любой role.* → deny.
func TestIntegration_RoleTool_403_AllOperations(t *testing.T) {
	truncateOperators(t)
	truncateRBAC(t)
	seedOperator(t, "archon-viewer", "")
	seedRole(t, "ops", false, "soul.list")

	base, stop := startMCPServer(t, &rbactest.Config{
		Roles: []rbactest.Role{
			{Name: "viewer", Operators: []string{"archon-viewer"}, Permissions: []string{"soul.list"}},
		},
	})
	defer stop()
	token := newToken(t, "archon-viewer", []string{"viewer"})

	cases := []struct {
		tool string
		args map[string]any
	}{
		{"keeper.role.create", map[string]any{"name": "x", "permissions": []string{"soul.list"}}},
		{"keeper.role.list", map[string]any{}},
		{"keeper.role.delete", map[string]any{"name": "ops"}},
		{"keeper.role.update", map[string]any{"name": "ops", "permissions": []string{"soul.list"}}},
		{"keeper.role.grant-operator", map[string]any{"role": "ops", "aid": "archon-viewer"}},
		{"keeper.role.revoke-operator", map[string]any{"role": "ops", "aid": "archon-viewer"}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.tool, func(t *testing.T) {
			resp := callRoleTool(t, base, token, tc.tool, tc.args)
			if code := mcpErrCode(t, resp); code != mcpCodeForbidden {
				t.Errorf("%s: code = %q, want forbidden", tc.tool, code)
			}
		})
	}
}
