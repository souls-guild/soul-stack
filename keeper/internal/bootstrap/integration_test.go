//go:build integration

// Integration-тесты bootstrap.Init через testcontainers-go (PG + Vault).
//
// Запуск:
//
//	make test-integration
//	# или
//	cd keeper && go test -tags=integration -race -count=1 ./internal/bootstrap/
//
// Один Postgres + один Vault per-package в TestMain — поднятие
// контейнеров занимает 3-5 секунд каждый, иначе тесты получаются
// прохибитивно медленными.

package bootstrap

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	jwtv5 "github.com/golang-jwt/jwt/v5"
	vaultapi "github.com/hashicorp/vault/api"
	"github.com/jackc/pgx/v5/pgxpool"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	tcvault "github.com/testcontainers/testcontainers-go/modules/vault"

	"github.com/souls-guild/soul-stack/keeper/internal/auditpg"
	"github.com/souls-guild/soul-stack/keeper/internal/jwt"
	"github.com/souls-guild/soul-stack/keeper/internal/migrate"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac"
	keepervault "github.com/souls-guild/soul-stack/keeper/internal/vault"
	"github.com/souls-guild/soul-stack/keeper/migrations"
	"github.com/souls-guild/soul-stack/shared/config"
)

const (
	integrationVaultToken = "root"
	integrationVaultImage = "hashicorp/vault:1.18"
	integrationPGImage    = "postgres:16-alpine"

	// integrationSigningKey — 32-байтовый ключ для HS256, общий между
	// Issuer-фабрикой и Verify-парсером в тестах. Base64-форма для Vault.
	integrationSigningKey = "0123456789abcdef0123456789abcdef"

	integrationIssuer = "keeper.integration"
)

var (
	integrationPool     *pgxpool.Pool
	integrationVaultC   *keepervault.Client
	integrationVaultAPI *vaultapi.Client // для write-операций в тестах (наш Client read-only)
	integrationKVPath   = "secret/keeper/jwt-signing-key"
)

func TestMain(m *testing.M) { os.Exit(run(m)) }

func run(m *testing.M) int {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	// --- Postgres ---
	pgCtr, err := tcpostgres.Run(ctx,
		integrationPGImage,
		tcpostgres.WithDatabase("keeper_test"),
		tcpostgres.WithUsername("keeper"),
		tcpostgres.WithPassword("keeper"),
		tcpostgres.BasicWaitStrategies(),
	)
	if err != nil {
		if requireDocker() {
			log.Fatalf("bootstrap integration: PG setup failed (REQUIRE_DOCKER): %v", err)
		}
		log.Printf("bootstrap integration: skipping, docker unavailable: %v", err)
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
	integrationVaultAPI = api
	if _, err := api.KVv2("secret").Put(ctx, "keeper/jwt-signing-key", map[string]any{
		"signing_key": base64.StdEncoding.EncodeToString([]byte(integrationSigningKey)),
	}); err != nil {
		log.Printf("seed signing-key: %v", err)
		return 1
	}

	vc, err := keepervault.NewClient(ctx, config.KeeperVault{
		Addr: vAddr, Token: integrationVaultToken, KVMount: "secret",
	})
	if err != nil {
		log.Printf("keepervault.NewClient: %v", err)
		return 1
	}
	integrationVaultC = vc

	return m.Run()
}

func resetState(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	// TRUNCATE operators CASCADE каскадно truncate-ит и rbac_roles (FK
	// created_by_aid → operators), снося seed-роль cluster-admin. В проде она
	// живёт за счёт миграции 027; в тестах после wipe ре-сидим её идемпотентно,
	// чтобы keeper init (rbac.GrantOperator cluster-admin) нашёл роль.
	if _, err := integrationPool.Exec(ctx,
		`TRUNCATE TABLE operators, audit_log CASCADE`); err != nil {
		t.Fatalf("TRUNCATE: %v", err)
	}
	for _, stmt := range []string{
		`INSERT INTO rbac_roles (name, builtin, created_by_aid)
		 VALUES ('cluster-admin', true, NULL) ON CONFLICT (name) DO NOTHING`,
		`INSERT INTO rbac_role_permissions (role_name, permission)
		 VALUES ('cluster-admin', '*') ON CONFLICT (role_name, permission) DO NOTHING`,
	} {
		if _, err := integrationPool.Exec(ctx, stmt); err != nil {
			t.Fatalf("re-seed cluster-admin: %v", err)
		}
	}
}

func newIssuerFactory() func(signingKey []byte) (JWTIssuer, error) {
	return func(signingKey []byte) (JWTIssuer, error) {
		return jwt.NewIssuer(signingKey, integrationIssuer)
	}
}

func TestIntegration_Init_HappyPath(t *testing.T) {
	resetState(t)
	ctx := context.Background()
	dir := t.TempDir()
	credPath := filepath.Join(dir, "k.token")

	writer := auditpg.NewWriter(integrationPool)
	res, err := Init(ctx, Config{
		ArchonAID:        "archon-alice",
		DisplayName:      "Alice Admin",
		TTLBootstrap:     720 * time.Hour,
		Pool:             integrationPool,
		VaultClient:      integrationVaultC,
		SigningKeyRef:    "vault:" + integrationKVPath,
		IssuerFactory:    newIssuerFactory(),
		AuditWriter:      writer,
		CredentialOutput: credPath,
	})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if res.CredentialPath != credPath {
		t.Errorf("CredentialPath = %q, want %q", res.CredentialPath, credPath)
	}
	if res.AuditID == "" {
		t.Errorf("AuditID empty")
	}
	if res.CorrelationID == "" {
		t.Errorf("CorrelationID empty")
	}

	// Проверка operators.
	var (
		aid          string
		displayName  string
		authMethod   string
		createdByAID *string
	)
	row := integrationPool.QueryRow(ctx,
		`SELECT aid, display_name, auth_method, created_by_aid
		 FROM operators WHERE aid = $1`, "archon-alice")
	if err := row.Scan(&aid, &displayName, &authMethod, &createdByAID); err != nil {
		t.Fatalf("scan operators: %v", err)
	}
	if aid != "archon-alice" || displayName != "Alice Admin" || authMethod != "jwt" {
		t.Errorf("operator row = (%q, %q, %q)", aid, displayName, authMethod)
	}
	if createdByAID != nil {
		t.Errorf("created_by_aid = %v, want NULL (bootstrap)", *createdByAID)
	}

	// Проверка audit_log.
	var (
		eventType    string
		source       string
		archonAID    *string
		corrID       *string
		payloadBytes []byte
	)
	row = integrationPool.QueryRow(ctx,
		`SELECT event_type, source, archon_aid, correlation_id, payload
		 FROM audit_log WHERE audit_id = $1`, res.AuditID)
	if err := row.Scan(&eventType, &source, &archonAID, &corrID, &payloadBytes); err != nil {
		t.Fatalf("scan audit_log: %v", err)
	}
	if eventType != "operator.created" {
		t.Errorf("event_type = %q", eventType)
	}
	if source != "keeper_internal" {
		t.Errorf("source = %q", source)
	}
	if archonAID != nil {
		t.Errorf("archon_aid = %v, want NULL", *archonAID)
	}
	if corrID == nil || *corrID != res.CorrelationID {
		t.Errorf("correlation_id roundtrip mismatch: %v vs %q", corrID, res.CorrelationID)
	}
	var payload map[string]any
	if err := json.Unmarshal(payloadBytes, &payload); err != nil {
		t.Fatalf("payload not JSON: %v", err)
	}
	if payload["bootstrap_initial"] != true {
		t.Errorf("payload.bootstrap_initial = %v", payload["bootstrap_initial"])
	}
	if payload["aid"] != "archon-alice" {
		t.Errorf("payload.aid = %v", payload["aid"])
	}
	if payload["display_name"] != "Alice Admin" {
		t.Errorf("payload.display_name = %v", payload["display_name"])
	}
	if payload["auth_method"] != "jwt" {
		t.Errorf("payload.auth_method = %v", payload["auth_method"])
	}

	// Проверка JWT-файла.
	info, err := os.Stat(credPath)
	if err != nil {
		t.Fatalf("Stat credPath: %v", err)
	}
	if mode := info.Mode().Perm(); mode != credentialFileMode {
		t.Errorf("file mode = %o, want %o", mode, credentialFileMode)
	}
	tokenBytes, err := os.ReadFile(credPath)
	if err != nil {
		t.Fatalf("ReadFile credPath: %v", err)
	}
	tokenStr := string(tokenBytes)
	if len(tokenStr) == 0 {
		t.Fatal("token file empty")
	}
	// Декод JWT и проверка claims.
	parsed, err := jwtv5.Parse(tokenStr, func(_ *jwtv5.Token) (interface{}, error) {
		return []byte(integrationSigningKey), nil
	}, jwtv5.WithLeeway(2*time.Second))
	if err != nil {
		t.Fatalf("Parse JWT: %v", err)
	}
	if !parsed.Valid {
		t.Fatal("JWT invalid")
	}
	claims, ok := parsed.Claims.(jwtv5.MapClaims)
	if !ok {
		t.Fatalf("claims type = %T", parsed.Claims)
	}
	if claims["sub"] != "archon-alice" {
		t.Errorf("sub = %v", claims["sub"])
	}
	if claims["iss"] != integrationIssuer {
		t.Errorf("iss = %v", claims["iss"])
	}
	if claims["bootstrap_initial"] != true {
		t.Errorf("bootstrap_initial = %v", claims["bootstrap_initial"])
	}
	roles, ok := claims["roles"].([]any)
	if !ok || len(roles) != 1 || roles[0] != "cluster-admin" {
		t.Errorf("roles = %v", claims["roles"])
	}
}

// TestIntegration_Init_Concurrent — coverage gap (qa): реальная гонка двух
// и более `keeper init` (HA-кластер, ADR-002/ADR-013). Существующие тесты
// проверяют только ПОСЛЕДОВАТЕЛЬНЫЙ повтор (Init#1 завершён → Init#2). Здесь
// N goroutine стартуют bootstrap ОДНОВРЕМЕННО на одной чистой БД.
//
// Инвариант (ADR-013): `pg_advisory_xact_lock` сериализует транзакции, а
// partial unique index `operators_first_archon_idx` (на `created_by_aid IS
// NULL`) — последний рубеж на уровне БД. Ровно ОДНА goroutine должна успеть,
// остальные — получить ErrAlreadyInitialized (advisory lock освободился после
// COMMIT-а победителя, проигравшие видят непустой реестр). В итоге в
// operators — ровно одна строка первого Архонта (`created_by_aid IS NULL`).
func TestIntegration_Init_Concurrent(t *testing.T) {
	resetState(t)
	ctx := context.Background()
	writer := auditpg.NewWriter(integrationPool)

	const concurrency = 8

	var (
		start     sync.WaitGroup // барьер: все goroutine стартуют Init одновременно
		done      sync.WaitGroup
		succeeded atomic.Int64
		errs      = make([]error, concurrency)
	)
	start.Add(1)
	done.Add(concurrency)

	for i := range concurrency {
		go func(idx int) {
			defer done.Done()
			cfg := Config{
				// AID у каждого свой — без advisory lock-а попытка вставить
				// второго bootstrap-оператора упёрлась бы в partial unique
				// index, а не в PK по AID. Так тест проверяет именно
				// bootstrap-инвариант «ровно один created_by_aid IS NULL»,
				// а не банальную PK-коллизию.
				ArchonAID:        fmt.Sprintf("archon-race-%d", idx),
				TTLBootstrap:     time.Hour,
				Pool:             integrationPool,
				VaultClient:      integrationVaultC,
				SigningKeyRef:    "vault:" + integrationKVPath,
				IssuerFactory:    newIssuerFactory(),
				AuditWriter:      writer,
				CredentialOutput: filepath.Join(t.TempDir(), "k.token"),
			}
			start.Wait() // ждём общий старт — максимизируем фактическую гонку
			_, err := Init(ctx, cfg)
			if err == nil {
				succeeded.Add(1)
			}
			errs[idx] = err
		}(i)
	}

	start.Done() // отпускаем барьер — все Init стартуют ~одновременно
	done.Wait()

	if got := succeeded.Load(); got != 1 {
		t.Errorf("успешных Init = %d, want ровно 1 (advisory lock сериализует гонку)", got)
	}
	for idx, err := range errs {
		if err == nil {
			continue // победитель
		}
		if !errors.Is(err, ErrAlreadyInitialized) {
			t.Errorf("goroutine #%d: err = %v, want ErrAlreadyInitialized", idx, err)
		}
	}

	// В operators ровно одна строка, и она — первый Архонт (created_by_aid IS NULL).
	var (
		total      int64
		bootstrapN int64
	)
	if err := integrationPool.QueryRow(ctx, `SELECT COUNT(*) FROM operators`).Scan(&total); err != nil {
		t.Fatalf("count operators: %v", err)
	}
	if total != 1 {
		t.Errorf("operators count = %d, want 1 (только победитель гонки)", total)
	}
	if err := integrationPool.QueryRow(ctx,
		`SELECT COUNT(*) FROM operators WHERE created_by_aid IS NULL`).Scan(&bootstrapN); err != nil {
		t.Fatalf("count bootstrap operators: %v", err)
	}
	if bootstrapN != 1 {
		t.Errorf("operators с created_by_aid IS NULL = %d, want 1 (bootstrap-инвариант)", bootstrapN)
	}
}

func TestIntegration_Init_AlreadyInitialized(t *testing.T) {
	resetState(t)
	ctx := context.Background()
	dir := t.TempDir()
	writer := auditpg.NewWriter(integrationPool)
	cfg := Config{
		ArchonAID:        "archon-alice",
		TTLBootstrap:     720 * time.Hour,
		Pool:             integrationPool,
		VaultClient:      integrationVaultC,
		SigningKeyRef:    "vault:" + integrationKVPath,
		IssuerFactory:    newIssuerFactory(),
		AuditWriter:      writer,
		CredentialOutput: filepath.Join(dir, "k1.token"),
	}
	if _, err := Init(ctx, cfg); err != nil {
		t.Fatalf("Init#1: %v", err)
	}
	cfg2 := cfg
	cfg2.ArchonAID = "archon-bob"
	cfg2.CredentialOutput = filepath.Join(dir, "k2.token")
	_, err := Init(ctx, cfg2)
	if err == nil {
		t.Fatal("Init#2: expected ErrAlreadyInitialized, got nil")
	}
	if !errors.Is(err, ErrAlreadyInitialized) {
		t.Errorf("err = %v, want ErrAlreadyInitialized", err)
	}
	// Проверка: в operators по-прежнему один Архонт.
	var n int64
	_ = integrationPool.QueryRow(ctx, `SELECT COUNT(*) FROM operators`).Scan(&n)
	if n != 1 {
		t.Errorf("operators count after rejected Init#2 = %d, want 1", n)
	}
}

// TestIntegration_Init_DisplayNameDefaultsToAID проверяет PM-decision
// M0.5c №5: при пустом DisplayName в Config — в operators.display_name
// записывается ArchonAID.
func TestIntegration_Init_DisplayNameDefaultsToAID(t *testing.T) {
	resetState(t)
	ctx := context.Background()
	dir := t.TempDir()
	writer := auditpg.NewWriter(integrationPool)

	const aid = "archon-default-name"
	_, err := Init(ctx, Config{
		ArchonAID:        aid,
		DisplayName:      "", // пустой → должен fallback на AID
		TTLBootstrap:     time.Hour,
		Pool:             integrationPool,
		VaultClient:      integrationVaultC,
		SigningKeyRef:    "vault:" + integrationKVPath,
		IssuerFactory:    newIssuerFactory(),
		AuditWriter:      writer,
		CredentialOutput: filepath.Join(dir, "k.token"),
	})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}

	var displayName string
	if err := integrationPool.QueryRow(ctx,
		`SELECT display_name FROM operators WHERE aid = $1`, aid).Scan(&displayName); err != nil {
		t.Fatalf("scan display_name: %v", err)
	}
	if displayName != aid {
		t.Errorf("display_name = %q, want %q (fallback to AID)", displayName, aid)
	}
}

// TestIntegration_Init_AuditWriteFailsAfterCommit — coverage gap
// (qa M0.5c): после COMMIT-а insert-а operator-а audit-write падает →
// БД консистентна (operator в реестре), Init возвращает
// ErrAuditWriteFailed, JWT-файл НЕ создан (caller через main.go увидит
// sentinel и выдаст warning про manual reconciliation).
func TestIntegration_Init_AuditWriteFailsAfterCommit(t *testing.T) {
	resetState(t)
	ctx := context.Background()
	dir := t.TempDir()
	credPath := filepath.Join(dir, "k.token")

	failingWriter := &fakeAuditWriter{err: errors.New("simulated audit fail")}

	const aid = "archon-audit-fail"
	res, err := Init(ctx, Config{
		ArchonAID:        aid,
		TTLBootstrap:     time.Hour,
		Pool:             integrationPool,
		VaultClient:      integrationVaultC,
		SigningKeyRef:    "vault:" + integrationKVPath,
		IssuerFactory:    newIssuerFactory(),
		AuditWriter:      failingWriter,
		CredentialOutput: credPath,
	})
	if err == nil {
		t.Fatal("Init: expected ErrAuditWriteFailed, got nil")
	}
	if !errors.Is(err, ErrAuditWriteFailed) {
		t.Fatalf("err = %v, want errors.Is ErrAuditWriteFailed", err)
	}
	if res != nil {
		t.Errorf("Result = %+v, want nil on ErrAuditWriteFailed", res)
	}

	// operator закоммичен — реестр имеет одну запись.
	var n int64
	if err := integrationPool.QueryRow(ctx, `SELECT COUNT(*) FROM operators`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("operators count = %d, want 1 (committed before audit-fail)", n)
	}

	// JWT-файл НЕ должен быть создан — writeTokenFile вызывается ПОСЛЕ
	// audit-write.
	if _, statErr := os.Stat(credPath); statErr == nil {
		t.Errorf("credPath %q exists; expected NOT to be created on audit-fail", credPath)
	} else if !os.IsNotExist(statErr) {
		t.Errorf("Stat credPath: unexpected err %v (want IsNotExist)", statErr)
	}

	// audit_log пуст (writer симулировал отказ — ничего не записал).
	if err := integrationPool.QueryRow(ctx, `SELECT COUNT(*) FROM audit_log`).Scan(&n); err != nil {
		t.Fatalf("count audit_log: %v", err)
	}
	if n != 0 {
		t.Errorf("audit_log count = %d, want 0", n)
	}
}

func TestIntegration_Init_VaultPathMissing(t *testing.T) {
	resetState(t)
	ctx := context.Background()
	dir := t.TempDir()
	writer := auditpg.NewWriter(integrationPool)
	_, err := Init(ctx, Config{
		ArchonAID:        "archon-alice",
		TTLBootstrap:     time.Hour,
		Pool:             integrationPool,
		VaultClient:      integrationVaultC,
		SigningKeyRef:    "vault:secret/keeper/never-existed",
		IssuerFactory:    newIssuerFactory(),
		AuditWriter:      writer,
		CredentialOutput: filepath.Join(dir, "k.token"),
	})
	if err == nil {
		t.Fatal("expected error for missing vault path, got nil")
	}
	// operators остался пуст.
	var n int64
	_ = integrationPool.QueryRow(ctx, `SELECT COUNT(*) FROM operators`).Scan(&n)
	if n != 0 {
		t.Errorf("operators count after Vault-miss = %d, want 0", n)
	}
}

// TestIntegration_Init_GrantsClusterAdminMembership — фикс BUG-1 (ADR-028(c)),
// слой БД. После keeper init в rbac_role_operators существует ровно одна
// membership-строка (cluster-admin, <aid>) с granted_by_aid IS NULL
// (bootstrap-membership). Роль cluster-admin приходит из seed-миграции 027,
// init пишет ТОЛЬКО membership.
func TestIntegration_Init_GrantsClusterAdminMembership(t *testing.T) {
	resetState(t)
	ctx := context.Background()
	dir := t.TempDir()
	writer := auditpg.NewWriter(integrationPool)

	const aid = "archon-alice"
	if _, err := Init(ctx, Config{
		ArchonAID:        aid,
		TTLBootstrap:     time.Hour,
		Pool:             integrationPool,
		VaultClient:      integrationVaultC,
		SigningKeyRef:    "vault:" + integrationKVPath,
		IssuerFactory:    newIssuerFactory(),
		AuditWriter:      writer,
		CredentialOutput: filepath.Join(dir, "k.token"),
	}); err != nil {
		t.Fatalf("Init: %v", err)
	}

	// Seed-роль cluster-admin существует (builtin=true) с permission `*`.
	var builtin bool
	if err := integrationPool.QueryRow(ctx,
		`SELECT builtin FROM rbac_roles WHERE name = 'cluster-admin'`).Scan(&builtin); err != nil {
		t.Fatalf("scan rbac_roles cluster-admin: %v", err)
	}
	if !builtin {
		t.Errorf("cluster-admin builtin = false, want true (seed E1)")
	}
	var perm string
	if err := integrationPool.QueryRow(ctx,
		`SELECT permission FROM rbac_role_permissions WHERE role_name = 'cluster-admin'`).Scan(&perm); err != nil {
		t.Fatalf("scan rbac_role_permissions: %v", err)
	}
	if perm != "*" {
		t.Errorf("cluster-admin permission = %q, want *", perm)
	}

	// Membership-строка (cluster-admin, archon-alice) с granted_by_aid IS NULL.
	var (
		roleName    string
		grantedBy   *string
		membershipN int64
	)
	if err := integrationPool.QueryRow(ctx,
		`SELECT COUNT(*) FROM rbac_role_operators`).Scan(&membershipN); err != nil {
		t.Fatalf("count membership: %v", err)
	}
	if membershipN != 1 {
		t.Errorf("rbac_role_operators count = %d, want 1", membershipN)
	}
	if err := integrationPool.QueryRow(ctx,
		`SELECT role_name, granted_by_aid FROM rbac_role_operators WHERE aid = $1`, aid).
		Scan(&roleName, &grantedBy); err != nil {
		t.Fatalf("scan membership row: %v", err)
	}
	if roleName != "cluster-admin" {
		t.Errorf("membership role_name = %q, want cluster-admin", roleName)
	}
	if grantedBy != nil {
		t.Errorf("granted_by_aid = %v, want NULL (bootstrap-membership)", *grantedBy)
	}
}

// TestIntegration_InitThenCheck_BUG1Closed — КЛЮЧЕВОЙ e2e-тест, доказывающий
// закрытие BUG-1. Раньше этот путь был заблокирован: enforcer резолвил
// membership из keeper.yml, куда keeper init ничего не писал → bootstrap-Архонт
// получал 403 на первой же operator-операции.
//
// Теперь: init пишет membership в rbac_role_operators (БД), enforcer строит
// снимок ИЗ ТОЙ ЖЕ БД через rbac.NewHolder(PoolSource) → Check на operator.create
// для выпущенного AID проходит (не ErrPermissionDenied). Чужой AID — отвергается.
func TestIntegration_InitThenCheck_BUG1Closed(t *testing.T) {
	resetState(t)
	ctx := context.Background()
	dir := t.TempDir()
	writer := auditpg.NewWriter(integrationPool)

	const aid = "archon-alice"
	if _, err := Init(ctx, Config{
		ArchonAID:        aid,
		TTLBootstrap:     time.Hour,
		Pool:             integrationPool,
		VaultClient:      integrationVaultC,
		SigningKeyRef:    "vault:" + integrationKVPath,
		IssuerFactory:    newIssuerFactory(),
		AuditWriter:      writer,
		CredentialOutput: filepath.Join(dir, "k.token"),
	}); err != nil {
		t.Fatalf("Init: %v", err)
	}

	// Enforcer-снимок строится ИЗ БД — ровно так, как `keeper run` собирает
	// rbac.Holder. Длинный TTL: на e2e перечит не нужен, проверяем initial-build.
	holder, err := rbac.NewHolder(ctx, rbac.PoolSource{DB: integrationPool}, time.Hour, nil)
	if err != nil {
		t.Fatalf("rbac.NewHolder from DB: %v", err)
	}

	// Фикс BUG-1: bootstrap-Архонт проходит реальный permission Check (не 403).
	if err := holder.Check(aid, "operator", "create", nil); err != nil {
		t.Errorf("BUG-1: bootstrap-Архонт %q должен проходить operator.create, got: %v", aid, err)
	}
	// cluster-admin = `*` → проходит и любую другую операцию.
	if err := holder.Check(aid, "incarnation", "destroy", nil); err != nil {
		t.Errorf("cluster-admin должен проходить incarnation.destroy: %v", err)
	}
	// Чужой AID без membership-а — отвергается (default deny).
	if err := holder.Check("archon-ghost", "operator", "create", nil); err == nil {
		t.Errorf("archon-ghost без membership-а должен быть denied, got nil")
	}

	// ClusterAdmins из БД-снимка — ровно bootstrap-Архонт (нужно operator-service
	// для self-lockout-инварианта).
	admins := holder.ClusterAdmins()
	if len(admins) != 1 || admins[0] != aid {
		t.Errorf("ClusterAdmins = %v, want [%s]", admins, aid)
	}
}

// TestIntegration_LoadSnapshot_FromInit — repository-слой: LoadSnapshot читает
// то, что записали seed-миграция + keeper init.
func TestIntegration_LoadSnapshot_FromInit(t *testing.T) {
	resetState(t)
	ctx := context.Background()
	dir := t.TempDir()
	writer := auditpg.NewWriter(integrationPool)

	const aid = "archon-alice"
	if _, err := Init(ctx, Config{
		ArchonAID:        aid,
		TTLBootstrap:     time.Hour,
		Pool:             integrationPool,
		VaultClient:      integrationVaultC,
		SigningKeyRef:    "vault:" + integrationKVPath,
		IssuerFactory:    newIssuerFactory(),
		AuditWriter:      writer,
		CredentialOutput: filepath.Join(dir, "k.token"),
	}); err != nil {
		t.Fatalf("Init: %v", err)
	}

	snap, err := rbac.LoadSnapshot(ctx, integrationPool)
	if err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	perms, ok := snap.Roles["cluster-admin"]
	if !ok {
		t.Fatalf("snapshot Roles missing cluster-admin: %+v", snap.Roles)
	}
	if len(perms) != 1 || perms[0] != "*" {
		t.Errorf("cluster-admin perms = %v, want [*]", perms)
	}
	roleNames, ok := snap.Membership[aid]
	if !ok {
		t.Fatalf("snapshot Membership missing %s: %+v", aid, snap.Membership)
	}
	if len(roleNames) != 1 || roleNames[0] != "cluster-admin" {
		t.Errorf("membership[%s] = %v, want [cluster-admin]", aid, roleNames)
	}
}

// TestIntegration_LoadSigningKey_HappyPath — coverage gap qa.M0.6a #10:
// happy round-trip против реального Vault.
func TestIntegration_LoadSigningKey_HappyPath(t *testing.T) {
	got, err := LoadSigningKey(context.Background(), integrationVaultC, "vault:"+integrationKVPath)
	if err != nil {
		t.Fatalf("LoadSigningKey: %v", err)
	}
	if string(got) != integrationSigningKey {
		t.Errorf("signing key = %q, want %q", got, integrationSigningKey)
	}
}

// TestIntegration_LoadSigningKey_BadPayload — KV есть, но без поля
// `signing_key` → ErrSigningKeyMissing.
func TestIntegration_LoadSigningKey_BadPayload(t *testing.T) {
	ctx := context.Background()
	if _, err := integrationVaultAPI.KVv2("secret").Put(ctx, "keeper/no-signing-key", map[string]any{
		"other": "x",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_, err := LoadSigningKey(ctx, integrationVaultC, "vault:secret/keeper/no-signing-key")
	if !errors.Is(err, ErrSigningKeyMissing) {
		t.Errorf("err = %v, want errors.Is ErrSigningKeyMissing", err)
	}
}

// TestIntegration_Init_GrantsRealRBACAccess — e2e «keeper init → реальный RBAC
// Check + self-lockout-инвариант» (HIGH coverage-gap qa, decisions.md 259/267).
// До этого теста ни один прогон не проверял, что первый Архонт ФАКТИЧЕСКИ
// получает `*` через БД-membership (rbac_role_operators) и проходит реальный
// enforcer Check — JWT-claim `roles` авторитетом НЕ является (модель-C).
// Именно этот пробел маскировал BUG-1 (Init не писал membership → enforcer
// видел 0 ролей у первого Архонта → cluster залочен от рождения).
//
// Тест прогоняет полную цепочку:
//  1. Init создаёт первого Архонта → membership (cluster-admin, aid) в БД.
//  2. LoadSnapshot → NewEnforcerFromSnapshot → Check на произвольный permission
//     (role.create) РАЗРЕШЁН — первый Архонт реально держит `*` через membership.
//  3. Self-lockout-инвариант: revoke последнего `*`-оператора (его membership в
//     cluster-admin) → ErrWouldLockOutCluster, membership остаётся (tx откат).
func TestIntegration_Init_GrantsRealRBACAccess(t *testing.T) {
	resetState(t)
	ctx := context.Background()
	dir := t.TempDir()

	writer := auditpg.NewWriter(integrationPool)
	if _, err := Init(ctx, Config{
		ArchonAID:        "archon-root",
		TTLBootstrap:     720 * time.Hour,
		Pool:             integrationPool,
		VaultClient:      integrationVaultC,
		SigningKeyRef:    "vault:" + integrationKVPath,
		IssuerFactory:    newIssuerFactory(),
		AuditWriter:      writer,
		CredentialOutput: filepath.Join(dir, "k.token"),
	}); err != nil {
		t.Fatalf("Init: %v", err)
	}

	// Шаг 2: реальный enforcer поверх БД-снимка. Membership пишет Init — если бы
	// он его не записал (BUG-1), snapshot не дал бы первому Архонту ни одной роли
	// и Check вернул бы ErrPermissionDenied.
	snap, err := rbac.LoadSnapshot(ctx, integrationPool)
	if err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	enforcer, err := rbac.NewEnforcerFromSnapshot(snap)
	if err != nil {
		t.Fatalf("NewEnforcerFromSnapshot: %v", err)
	}
	for _, tc := range []struct{ resource, action string }{
		{"role", "create"},
		{"operator", "create"},
		{"soul", "list"},
	} {
		if err := enforcer.Check("archon-root", tc.resource, tc.action, nil); err != nil {
			t.Errorf("первый Архонт должен пройти %s.%s через `*`-membership: %v", tc.resource, tc.action, err)
		}
	}

	// Шаг 3: self-lockout-инвариант — нельзя ревокнуть последнего `*`-оператора.
	svc, err := rbac.NewService(rbac.ServiceDeps{Pool: integrationPool})
	if err != nil {
		t.Fatalf("rbac.NewService: %v", err)
	}
	err = svc.RevokeOperator(ctx, rbac.RevokeOperatorInput{
		RoleName: BootstrapRoleClusterAdmin, AID: "archon-root",
	})
	if !errors.Is(err, rbac.ErrWouldLockOutCluster) {
		t.Fatalf("revoke последнего админа: err = %v, want ErrWouldLockOutCluster", err)
	}

	// Membership уцелел (tx откатилась) — первый Архонт всё ещё admin.
	var n int64
	if err := integrationPool.QueryRow(ctx,
		`SELECT COUNT(*) FROM rbac_role_operators WHERE role_name = $1 AND aid = $2`,
		BootstrapRoleClusterAdmin, "archon-root").Scan(&n); err != nil {
		t.Fatalf("membership probe: %v", err)
	}
	if n != 1 {
		t.Errorf("membership rows = %d, want 1 (revoke-lockout откатился)", n)
	}
}
