//go:build integration

// Integration tests for bootstrap.Init via testcontainers-go (PG + Vault).
//
// Run:
//
//	make test-integration
//	# or
//	cd keeper && go test -tags=integration -race -count=1 ./internal/bootstrap/
//
// One Postgres + one Vault per package in TestMain — spinning up
// containers takes 3-5 seconds each, otherwise tests would be
// prohibitively slow.

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
	"github.com/souls-guild/soul-stack/keeper/internal/operator"
	"github.com/souls-guild/soul-stack/keeper/internal/rbac"
	keepervault "github.com/souls-guild/soul-stack/keeper/internal/vault"
	"github.com/souls-guild/soul-stack/keeper/migrations"
	"github.com/souls-guild/soul-stack/shared/config"
)

const (
	integrationVaultToken = "root"
	integrationVaultImage = "hashicorp/vault:1.18"
	integrationPGImage    = "postgres:16-alpine"

	// integrationSigningKey is a 32-byte key for HS256, shared between
	// the Issuer factory and the Verify parser in tests. Base64 form for
	// Vault.
	integrationSigningKey = "0123456789abcdef0123456789abcdef"

	integrationIssuer = "keeper.integration"
)

var (
	integrationPool     *pgxpool.Pool
	integrationVaultC   *keepervault.Client
	integrationVaultAPI *vaultapi.Client // for write operations in tests (our Client is read-only)
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
	// TRUNCATE operators CASCADE also truncates rbac_roles (FK
	// created_by_aid → operators), wiping out the seeded cluster-admin
	// role. In production it's kept alive by migration 027; in tests we
	// idempotently re-seed it after the wipe so that keeper init
	// (rbac.GrantOperator cluster-admin) can find the role.
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

// seedSystemOperator restores archon-system (migration 086), which
// resetState wipes via TRUNCATE — reproducing a clean DB post-migrations.
func seedSystemOperator(t *testing.T) {
	t.Helper()
	if _, err := integrationPool.Exec(context.Background(),
		`INSERT INTO operators (aid, display_name, auth_method, created_by_aid, created_via, metadata)
		 VALUES ('archon-system', 'System (Soul Stack)', 'jwt', NULL, 'system', '{}'::jsonb)
		 ON CONFLICT (aid) DO NOTHING`); err != nil {
		t.Fatalf("seed archon-system: %v", err)
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

	// Check operators.
	var (
		aid          string
		displayName  string
		authMethod   string
		createdByAID *string
		createdVia   string
	)
	row := integrationPool.QueryRow(ctx,
		`SELECT aid, display_name, auth_method, created_by_aid, created_via
		 FROM operators WHERE aid = $1`, "archon-alice")
	if err := row.Scan(&aid, &displayName, &authMethod, &createdByAID, &createdVia); err != nil {
		t.Fatalf("scan operators: %v", err)
	}
	if aid != "archon-alice" || displayName != "Alice Admin" || authMethod != "jwt" {
		t.Errorf("operator row = (%q, %q, %q)", aid, displayName, authMethod)
	}
	if createdByAID != nil {
		t.Errorf("created_by_aid = %v, want NULL (bootstrap)", *createdByAID)
	}
	// ADR-058(d) guard (case 1): the first Archon is written with
	// created_via='bootstrap' (the bootstrap invariant now lives on this
	// field, migration 085).
	if createdVia != "bootstrap" {
		t.Errorf("created_via = %q, want \"bootstrap\"", createdVia)
	}

	// Check audit_log.
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

	// Check the JWT file.
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
	// Decode the JWT and check claims.
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

// TestIntegration_Init_Concurrent covers a coverage gap (qa): a real race
// between two or more `keeper init` (HA cluster, ADR-002/ADR-013).
// Existing tests only check a SEQUENTIAL repeat (Init#1 finishes →
// Init#2). Here N goroutines start bootstrap SIMULTANEOUSLY against one
// clean DB.
//
// Invariant (ADR-013): `pg_advisory_xact_lock` serializes the
// transactions, and the partial unique index `operators_first_archon_idx`
// (on `created_via = 'bootstrap'`, ADR-058(d)/migration 085) is the last
// line of defense at the DB level. Exactly ONE goroutine should succeed,
// the rest get ErrAlreadyInitialized (the advisory lock is released after
// the winner's COMMIT, the losers then see a non-empty registry). The end
// result: operators has exactly one row for the first Archon
// (`created_via = 'bootstrap'`).
func TestIntegration_Init_Concurrent(t *testing.T) {
	resetState(t)
	ctx := context.Background()
	writer := auditpg.NewWriter(integrationPool)

	const concurrency = 8

	var (
		start     sync.WaitGroup // barrier: all goroutines start Init simultaneously
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
				// Each goroutine gets its own AID — without the advisory
				// lock, inserting a second bootstrap operator would hit the
				// partial unique index rather than the AID PK. This way the
				// test checks the actual bootstrap invariant ("exactly one
				// created_via = 'bootstrap'"), not a trivial PK collision.
				ArchonAID:        fmt.Sprintf("archon-race-%d", idx),
				TTLBootstrap:     time.Hour,
				Pool:             integrationPool,
				VaultClient:      integrationVaultC,
				SigningKeyRef:    "vault:" + integrationKVPath,
				IssuerFactory:    newIssuerFactory(),
				AuditWriter:      writer,
				CredentialOutput: filepath.Join(t.TempDir(), "k.token"),
			}
			start.Wait() // wait for the common start — maximize the actual race
			_, err := Init(ctx, cfg)
			if err == nil {
				succeeded.Add(1)
			}
			errs[idx] = err
		}(i)
	}

	start.Done() // release the barrier — all Init calls start ~simultaneously
	done.Wait()

	if got := succeeded.Load(); got != 1 {
		t.Errorf("успешных Init = %d, want ровно 1 (advisory lock сериализует гонку)", got)
	}
	for idx, err := range errs {
		if err == nil {
			continue // winner
		}
		if !errors.Is(err, ErrAlreadyInitialized) {
			t.Errorf("goroutine #%d: err = %v, want ErrAlreadyInitialized", idx, err)
		}
	}

	// operators holds exactly one row, and it is the first Archon
	// (created_via = 'bootstrap').
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
	// ADR-058(d): the bootstrap invariant moved from `created_by_aid IS
	// NULL` to `created_via = 'bootstrap'` (migration 085). Under race
	// load, the `operators_first_archon_idx` index (now on
	// created_via='bootstrap') let exactly one winner through.
	if err := integrationPool.QueryRow(ctx,
		`SELECT COUNT(*) FROM operators WHERE created_via = 'bootstrap'`).Scan(&bootstrapN); err != nil {
		t.Fatalf("count bootstrap operators: %v", err)
	}
	if bootstrapN != 1 {
		t.Errorf("operators с created_via='bootstrap' = %d, want 1 (bootstrap-инвариант)", bootstrapN)
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
	// Check: operators still has exactly one Archon.
	var n int64
	_ = integrationPool.QueryRow(ctx, `SELECT COUNT(*) FROM operators`).Scan(&n)
	if n != 1 {
		t.Errorf("operators count after rejected Init#2 = %d, want 1", n)
	}
}

// TestIntegration_Init_DisplayNameDefaultsToAID verifies PM-decision
// M0.5c #5: when Config.DisplayName is empty, operators.display_name is
// set to ArchonAID.
func TestIntegration_Init_DisplayNameDefaultsToAID(t *testing.T) {
	resetState(t)
	ctx := context.Background()
	dir := t.TempDir()
	writer := auditpg.NewWriter(integrationPool)

	const aid = "archon-default-name"
	_, err := Init(ctx, Config{
		ArchonAID:        aid,
		DisplayName:      "", // empty → should fall back to AID
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

// TestIntegration_Init_AuditWriteFailsAfterCommit covers a coverage gap
// (qa M0.5c): after the operator insert's COMMIT, the audit write fails
// → the DB is consistent (operator in the registry), Init returns
// ErrAuditWriteFailed, and the JWT file is NOT created (the caller sees
// the sentinel via main.go and issues a manual-reconciliation warning).
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

	// operator is committed — the registry has one record.
	var n int64
	if err := integrationPool.QueryRow(ctx, `SELECT COUNT(*) FROM operators`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("operators count = %d, want 1 (committed before audit-fail)", n)
	}

	// The JWT file must NOT be created — writeTokenFile is called AFTER
	// the audit write.
	if _, statErr := os.Stat(credPath); statErr == nil {
		t.Errorf("credPath %q exists; expected NOT to be created on audit-fail", credPath)
	} else if !os.IsNotExist(statErr) {
		t.Errorf("Stat credPath: unexpected err %v (want IsNotExist)", statErr)
	}

	// audit_log is empty (the writer simulated a failure — nothing was
	// written).
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
	// operators remains empty.
	var n int64
	_ = integrationPool.QueryRow(ctx, `SELECT COUNT(*) FROM operators`).Scan(&n)
	if n != 0 {
		t.Errorf("operators count after Vault-miss = %d, want 0", n)
	}
}

// TestIntegration_Init_GrantsClusterAdminMembership covers the BUG-1 fix
// (ADR-028(c)) at the DB layer. After keeper init, rbac_role_operators
// has exactly one membership row (cluster-admin, <aid>) with
// granted_by_aid IS NULL (bootstrap membership). The cluster-admin role
// comes from seed migration 027; init writes ONLY the membership.
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

	// The seeded cluster-admin role exists (builtin=true) with permission `*`.
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

	// Membership row (cluster-admin, archon-alice) with granted_by_aid IS NULL.
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

// TestIntegration_InitThenCheck_BUG1Closed is the KEY e2e test proving
// the BUG-1 fix. This path used to be blocked: the enforcer resolved
// membership from keeper.yml, which keeper init never wrote to → the
// bootstrap Archon got a 403 on its very first operator operation.
//
// Now: init writes membership to rbac_role_operators (DB), the enforcer
// builds its snapshot FROM THAT SAME DB via rbac.NewHolder(PoolSource) →
// Check on operator.create for the issued AID succeeds (no
// ErrPermissionDenied). A foreign AID is rejected.
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

	// The enforcer snapshot is built FROM the DB — exactly like `keeper
	// run` assembles rbac.Holder. Long TTL: no refresh needed for this
	// e2e test, we're checking the initial build.
	holder, err := rbac.NewHolder(ctx, rbac.PoolSource{DB: integrationPool}, time.Hour, nil)
	if err != nil {
		t.Fatalf("rbac.NewHolder from DB: %v", err)
	}

	// BUG-1 fix: the bootstrap Archon passes a real permission Check (not 403).
	if err := holder.Check(aid, "operator", "create", nil); err != nil {
		t.Errorf("BUG-1: bootstrap-Архонт %q должен проходить operator.create, got: %v", aid, err)
	}
	// cluster-admin = `*` → also passes any other operation.
	if err := holder.Check(aid, "incarnation", "destroy", nil); err != nil {
		t.Errorf("cluster-admin должен проходить incarnation.destroy: %v", err)
	}
	// A foreign AID without membership is rejected (default deny).
	if err := holder.Check("archon-ghost", "operator", "create", nil); err == nil {
		t.Errorf("archon-ghost без membership-а должен быть denied, got nil")
	}

	// ClusterAdmins from the DB snapshot is exactly the bootstrap Archon
	// (needed by operator-service for the self-lockout invariant).
	admins := holder.ClusterAdmins()
	if len(admins) != 1 || admins[0] != aid {
		t.Errorf("ClusterAdmins = %v, want [%s]", admins, aid)
	}
}

// TestIntegration_LoadSnapshot_FromInit is a repository-layer test:
// LoadSnapshot reads what the seed migration + keeper init wrote.
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

// TestIntegration_LoadSigningKey_HappyPath covers coverage gap qa.M0.6a
// #10: a happy-path round-trip against a real Vault.
func TestIntegration_LoadSigningKey_HappyPath(t *testing.T) {
	got, err := LoadSigningKey(context.Background(), integrationVaultC, "vault:"+integrationKVPath)
	if err != nil {
		t.Fatalf("LoadSigningKey: %v", err)
	}
	if string(got) != integrationSigningKey {
		t.Errorf("signing key = %q, want %q", got, integrationSigningKey)
	}
}

// TestIntegration_LoadSigningKey_BadPayload — the KV exists but without
// the `signing_key` field → ErrSigningKeyMissing.
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

// TestIntegration_Init_GrantsRealRBACAccess is an e2e test for "keeper
// init → real RBAC Check + self-lockout invariant" (HIGH coverage gap qa,
// decisions.md 259/267). Before this test, no run verified that the
// first Archon ACTUALLY gets `*` via DB membership (rbac_role_operators)
// and passes a real enforcer Check — the JWT claim `roles` is NOT
// authoritative (model C). This exact gap masked BUG-1 (Init didn't
// write membership → the enforcer saw 0 roles for the first Archon → the
// cluster was locked out from birth).
//
// The test runs the full chain:
//  1. Init creates the first Archon → membership (cluster-admin, aid) in
//     the DB.
//  2. LoadSnapshot → NewEnforcerFromSnapshot → Check on an arbitrary
//     permission (role.create) is ALLOWED — the first Archon genuinely
//     holds `*` via membership.
//  3. Self-lockout invariant: revoking the last `*` operator (their
//     cluster-admin membership) → ErrWouldLockOutCluster, membership
//     remains (tx rolled back).
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

	// Step 2: a real enforcer on top of the DB snapshot. Init writes the
	// membership — if it hadn't (BUG-1), the snapshot would give the
	// first Archon no roles and Check would return
	// ErrPermissionDenied.
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

	// Step 3: self-lockout invariant — cannot revoke the last `*` operator.
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

	// Membership survived (tx rolled back) — the first Archon is still admin.
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

// Guard (ADR-013 amendment 2026-07-01): bootstrap succeeds on a clean DB where archon-system is present.
func TestIntegration_Init_IgnoresSystemArchon(t *testing.T) {
	resetState(t)
	seedSystemOperator(t)
	ctx := context.Background()
	dir := t.TempDir()
	writer := auditpg.NewWriter(integrationPool)

	res, err := Init(ctx, Config{
		ArchonAID:        "archon-alice",
		TTLBootstrap:     time.Hour,
		Pool:             integrationPool,
		VaultClient:      integrationVaultC,
		SigningKeyRef:    "vault:" + integrationKVPath,
		IssuerFactory:    newIssuerFactory(),
		AuditWriter:      writer,
		CredentialOutput: filepath.Join(dir, "k.token"),
	})
	if err != nil {
		t.Fatalf("Init на чистой БД с archon-system: %v (want success — системный оператор не считается)", err)
	}
	if res.CredentialPath == "" {
		t.Errorf("CredentialPath empty")
	}

	// The registry holds archon-system (system) + archon-alice (bootstrap).
	var (
		total     int64
		nonSystem int64
	)
	if err := integrationPool.QueryRow(ctx, `SELECT COUNT(*) FROM operators`).Scan(&total); err != nil {
		t.Fatalf("count operators: %v", err)
	}
	if total != 2 {
		t.Errorf("operators total = %d, want 2 (archon-system + archon-alice)", total)
	}
	if err := integrationPool.QueryRow(ctx,
		`SELECT COUNT(*) FROM operators WHERE created_via <> 'system'`).Scan(&nonSystem); err != nil {
		t.Fatalf("count non-system: %v", err)
	}
	if nonSystem != 1 {
		t.Errorf("non-system operators = %d, want 1 (только archon-alice)", nonSystem)
	}
}

// Once a real (non-system) Archon exists, a repeat Init returns ErrAlreadyInitialized.
func TestIntegration_Init_AlreadyInitialized_WithSystemArchon(t *testing.T) {
	resetState(t)
	seedSystemOperator(t)
	ctx := context.Background()
	dir := t.TempDir()
	writer := auditpg.NewWriter(integrationPool)

	cfg := Config{
		ArchonAID:        "archon-alice",
		TTLBootstrap:     time.Hour,
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
}

// Restart-guard (ADR-013(d)): on a DB with only archon-system, CountNonSystem=0 → "registry empty".
func TestIntegration_CountNonSystem_IgnoresSystemArchon(t *testing.T) {
	resetState(t)
	seedSystemOperator(t)
	ctx := context.Background()

	total, err := operator.Count(ctx, integrationPool)
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if total != 1 {
		t.Fatalf("Count = %d, want 1 (archon-system)", total)
	}
	nonSystem, err := operator.CountNonSystem(ctx, integrationPool)
	if err != nil {
		t.Fatalf("CountNonSystem: %v", err)
	}
	if nonSystem != 0 {
		t.Errorf("CountNonSystem = %d, want 0 (archon-system не считается → restart-guard требует bootstrap)", nonSystem)
	}
}
