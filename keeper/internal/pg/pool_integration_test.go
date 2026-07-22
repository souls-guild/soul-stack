//go:build integration

// Integration tests for ResolveDSN/NewPool with real Vault via testcontainers.
//
// One Vault + one Postgres per-package in TestMain — container startup takes
// 3-5 seconds each. Vault dev-mode, root-token = "root".
//
// To run:
//
//	make test-integration
//	# or
//	cd keeper && go test -tags=integration -race -count=1 ./internal/pg/

package pg

import (
	"context"
	"errors"
	"log"
	"os"
	"testing"
	"time"

	vaultapi "github.com/hashicorp/vault/api"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	tcvault "github.com/testcontainers/testcontainers-go/modules/vault"

	keepervault "github.com/souls-guild/soul-stack/keeper/internal/vault"
	"github.com/souls-guild/soul-stack/shared/config"
)

const (
	integrationVaultToken = "root"
	integrationVaultImage = "hashicorp/vault:1.18"
	integrationPGImage    = "postgres:16-alpine"
)

var (
	integrationDSN      string
	integrationVault    *keepervault.Client
	integrationVaultAPI *vaultapi.Client
)

func TestMain(m *testing.M) { os.Exit(run(m)) }

func run(m *testing.M) int {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	// --- Postgres ---
	pgCtr, err := tcpostgres.Run(ctx,
		integrationPGImage,
		tcpostgres.WithDatabase("keeper_pg_test"),
		tcpostgres.WithUsername("keeper"),
		tcpostgres.WithPassword("keeper"),
		tcpostgres.BasicWaitStrategies(),
	)
	if err != nil {
		if requireDocker() {
			log.Fatalf("pg integration: PG setup failed (REQUIRE_DOCKER): %v", err)
		}
		log.Printf("pg integration: skipping, docker unavailable: %v", err)
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
	integrationDSN = dsn

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

	vc, err := keepervault.NewClient(ctx, config.KeeperVault{
		Addr: vAddr, Token: integrationVaultToken, KVMount: "secret",
	})
	if err != nil {
		log.Printf("keepervault.NewClient: %v", err)
		return 1
	}
	integrationVault = vc

	return m.Run()
}

// TestIntegration_ResolveDSN_HappyPath writes DSN to Vault KV under the
// `dsn` field, reads it via ResolveDSN, and verifies it matches.
func TestIntegration_ResolveDSN_HappyPath(t *testing.T) {
	ctx := context.Background()
	if _, err := integrationVaultAPI.KVv2("secret").Put(ctx, "keeper/postgres", map[string]any{
		"dsn": integrationDSN,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	got, err := ResolveDSN(ctx, integrationVault, "vault:secret/keeper/postgres")
	if err != nil {
		t.Fatalf("ResolveDSN: %v", err)
	}
	if got != integrationDSN {
		t.Errorf("resolved DSN = %q, want %q", got, integrationDSN)
	}
}

// TestIntegration_NewPool_VaultRef uses NewPool with vault-ref in DSNRef
// to start a real pool and verify Ping succeeds.
func TestIntegration_NewPool_VaultRef(t *testing.T) {
	ctx := context.Background()
	if _, err := integrationVaultAPI.KVv2("secret").Put(ctx, "keeper/postgres", map[string]any{
		"dsn": integrationDSN,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	pool, err := NewPool(ctx, config.KeeperPostgres{
		DSNRef: "vault:secret/keeper/postgres",
		Pool:   config.KeeperPostgresPool{Min: 1, Max: 4},
	}, integrationVault)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer pool.Close()
	if err := Ping(ctx, pool); err != nil {
		t.Errorf("Ping: %v", err)
	}
}

// TestIntegration_ResolveDSN_VaultPathMissing verifies that when the KV
// path is missing, the error from ReadKV is propagated as ErrVaultKVNotFound.
func TestIntegration_ResolveDSN_VaultPathMissing(t *testing.T) {
	_, err := ResolveDSN(context.Background(), integrationVault, "vault:secret/keeper/never-existed")
	if !errors.Is(err, keepervault.ErrVaultKVNotFound) {
		t.Errorf("err = %v, want errors.Is ErrVaultKVNotFound", err)
	}
}

// TestIntegration_ResolveDSN_DSNFieldMissing verifies that when KV exists
// but lacks the `dsn` field, ErrDSNFieldMissing is returned.
func TestIntegration_ResolveDSN_DSNFieldMissing(t *testing.T) {
	ctx := context.Background()
	if _, err := integrationVaultAPI.KVv2("secret").Put(ctx, "keeper/postgres-bad", map[string]any{
		"other_field": "x",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_, err := ResolveDSN(ctx, integrationVault, "vault:secret/keeper/postgres-bad")
	if !errors.Is(err, ErrDSNFieldMissing) {
		t.Errorf("err = %v, want errors.Is ErrDSNFieldMissing", err)
	}
}
