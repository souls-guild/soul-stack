//go:build integration

// Integration-тесты render pipeline через testcontainers-go (hashicorp/vault
// dev-режим). Покрывают единственную фазу с внешней зависимостью —
// vault-resolve: `vault:`-ref в params задачи заменяется значением из Vault KV
// до CEL-фазы. Остальные фазы (CEL-render, on:/where: резолв) — чистые,
// проверены в unit-тестах.
//
// Запуск:
//
//	cd keeper && SOUL_STACK_INTEGRATION_REQUIRE_DOCKER=1 go test -tags=integration -race -count=1 ./internal/render/...
//
// Паттерн TestMain → run() → контейнер per-package совпадает с
// keeper/internal/vault/integration_test.go.
package render

import (
	"context"
	"log"
	"os"
	"testing"
	"time"

	vaultapi "github.com/hashicorp/vault/api"
	tcvault "github.com/testcontainers/testcontainers-go/modules/vault"

	"github.com/souls-guild/soul-stack/keeper/internal/topology"
	"github.com/souls-guild/soul-stack/keeper/internal/vault"
	"github.com/souls-guild/soul-stack/shared/cel"
	"github.com/souls-guild/soul-stack/shared/config"
)

const (
	integrationToken = "root"
	integrationImage = "hashicorp/vault:1.18"
)

var (
	integrationVault *vault.Client
	integrationAPI   *vaultapi.Client
)

func TestMain(m *testing.M) { os.Exit(run(m)) }

func run(m *testing.M) int {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	ctr, err := tcvault.Run(ctx, integrationImage, tcvault.WithToken(integrationToken))
	if err != nil {
		if requireDocker() {
			log.Fatalf("render integration: setup failed (SOUL_STACK_INTEGRATION_REQUIRE_DOCKER set): %v", err)
		}
		log.Printf("render integration: skipping, docker unavailable: %v", err)
		return 0
	}
	defer func() {
		termCtx, termCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer termCancel()
		_ = ctr.Terminate(termCtx)
	}()

	addr, err := ctr.HttpHostAddress(ctx)
	if err != nil {
		log.Printf("render integration: HttpHostAddress: %v", err)
		return 1
	}

	apiCfg := vaultapi.DefaultConfig()
	apiCfg.Address = addr
	api, err := vaultapi.NewClient(apiCfg)
	if err != nil {
		log.Printf("render integration: vaultapi.NewClient: %v", err)
		return 1
	}
	api.SetToken(integrationToken)
	integrationAPI = api

	cl, err := vault.NewClient(ctx, config.KeeperVault{Addr: addr, Token: integrationToken, KVMount: "secret"})
	if err != nil {
		log.Printf("render integration: vault.NewClient: %v", err)
		return 1
	}
	integrationVault = cl

	return m.Run()
}

// TestIntegration_VaultResolveInParams — `vault:`-ref в params задачи
// заменяется значением из Vault KV до CEL-фазы; CEL-интерполяция вокруг
// resolved-значения работает.
func TestIntegration_VaultResolveInParams(t *testing.T) {
	ctx := context.Background()
	kv := integrationAPI.KVv2("secret")
	if _, err := kv.Put(ctx, "db/creds", map[string]any{"password": "s3cr3t"}); err != nil {
		t.Fatalf("seed Put: %v", err)
	}

	engine, err := cel.New()
	if err != nil {
		t.Fatalf("cel.New: %v", err)
	}
	p := NewPipeline(integrationVault, engine, nil, nil)

	manifest := &config.ScenarioManifest{
		Name: "secret-task",
		Tasks: []config.Task{
			{
				Name: "set password",
				Module: &config.ModuleTask{
					Module: "core.exec.run",
					Params: map[string]any{
						// Полевая форма ref: один ключ из секрета.
						"password": "vault:secret/db/creds#password",
						// CEL-интерполяция остаётся независимой фазой.
						"label": "creds for ${ input.user }",
					},
				},
			},
		},
	}
	in := RenderInput{
		Scenario:    manifest,
		Input:       map[string]any{"user": "alice"},
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{{SID: "a", Coven: []string{"svc"}}},
	}

	tasks, _, err := p.Render(ctx, in)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	f := tasks[0].Params.GetFields()
	if got := f["password"].GetStringValue(); got != "s3cr3t" {
		t.Errorf("password = %q, want resolved s3cr3t", got)
	}
	if got := f["label"].GetStringValue(); got != "creds for alice" {
		t.Errorf("label = %q, want %q", got, "creds for alice")
	}
}

// TestIntegration_VaultRefNotFound — ref на несуществующий путь → ошибка
// (проброс vault.ErrVaultKVNotFound).
func TestIntegration_VaultRefNotFound(t *testing.T) {
	ctx := context.Background()
	engine, err := cel.New()
	if err != nil {
		t.Fatalf("cel.New: %v", err)
	}
	p := NewPipeline(integrationVault, engine, nil, nil)

	manifest := &config.ScenarioManifest{
		Name: "missing-secret",
		Tasks: []config.Task{
			{
				Name: "t",
				Module: &config.ModuleTask{
					Module: "core.exec.run",
					Params: map[string]any{"password": "vault:secret/db/never-existed#password"},
				},
			},
		},
	}
	in := RenderInput{
		Scenario:    manifest,
		Incarnation: IncarnationMeta{Name: "svc"},
		Hosts:       []*topology.HostFacts{{SID: "a", Coven: []string{"svc"}}},
	}
	if _, _, err := p.Render(ctx, in); err == nil {
		t.Fatal("Render: ожидалась ошибка для несуществующего vault-пути")
	}
}
