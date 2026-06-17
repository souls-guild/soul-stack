//go:build integration

// Integration-тесты `core.vault.kv-read` через testcontainers-go
// (hashicorp/vault dev-mode). End-to-end: модуль вызывается с настоящим
// vault.Client поверх testcontainer-Vault-а, проверяется чтение KV v2.
//
// Паттерн совпадает с keeper/internal/vault/integration_test.go.

package vault_test

import (
	"context"
	"log"
	"os"
	"reflect"
	"sort"
	"testing"
	"time"

	vaultapi "github.com/hashicorp/vault/api"
	tcvault "github.com/testcontainers/testcontainers-go/modules/vault"

	"github.com/souls-guild/soul-stack/keeper/internal/coremod/internaltest"
	coremodvault "github.com/souls-guild/soul-stack/keeper/internal/coremod/vault"
	keepervault "github.com/souls-guild/soul-stack/keeper/internal/vault"
	"github.com/souls-guild/soul-stack/shared/audit"
	"github.com/souls-guild/soul-stack/shared/config"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/protobuf/types/known/structpb"
)

const (
	integrationToken = "root"
	integrationImage = "hashicorp/vault:1.18"
)

var (
	integrationClient *keepervault.Client
	integrationAPI    *vaultapi.Client
)

func TestMain(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) int {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	ctr, err := tcvault.Run(ctx, integrationImage, tcvault.WithToken(integrationToken))
	if err != nil {
		if requireDocker() {
			log.Fatalf("coremod/vault integration: setup failed (REQUIRE_DOCKER): %v", err)
		}
		log.Printf("coremod/vault integration: skipping, docker unavailable: %v", err)
		return 0
	}
	defer func() {
		termCtx, termCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer termCancel()
		_ = ctr.Terminate(termCtx)
	}()

	addr, err := ctr.HttpHostAddress(ctx)
	if err != nil {
		log.Printf("HttpHostAddress: %v", err)
		return 1
	}

	apiCfg := vaultapi.DefaultConfig()
	apiCfg.Address = addr
	api, err := vaultapi.NewClient(apiCfg)
	if err != nil {
		log.Printf("vaultapi.NewClient: %v", err)
		return 1
	}
	api.SetToken(integrationToken)
	integrationAPI = api

	cl, err := keepervault.NewClient(ctx, config.KeeperVault{
		Addr:    addr,
		Token:   integrationToken,
		KVMount: "secret",
	})
	if err != nil {
		log.Printf("keepervault.NewClient: %v", err)
		return 1
	}
	integrationClient = cl

	return m.Run()
}

type capturingAudit struct {
	events []*audit.Event
}

func (c *capturingAudit) Write(_ context.Context, e *audit.Event) error {
	c.events = append(c.events, e)
	return nil
}

func mustStructIT(t *testing.T, m map[string]any) *structpb.Struct {
	t.Helper()
	s, err := structpb.NewStruct(m)
	if err != nil {
		t.Fatalf("structpb.NewStruct: %v", err)
	}
	return s
}

func seedKV(t *testing.T, path string, data map[string]any) {
	t.Helper()
	_, err := integrationAPI.KVv2("secret").Put(context.Background(), path, data)
	if err != nil {
		t.Fatalf("seedKV(%s): %v", path, err)
	}
}

func TestIntegration_VaultKVRead_AllFields(t *testing.T) {
	seedKV(t, "redis/admin", map[string]any{
		"username": "admin",
		"password": "s3cret-it",
	})

	audr := &capturingAudit{}
	m := coremodvault.New(integrationClient, audr)
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: coremodvault.StateRead,
		Params: mustStructIT(t, map[string]any{
			"path": "secret/redis/admin",
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	ev := stream.Last()
	if ev.Failed {
		t.Fatalf("apply failed: %+v", ev)
	}
	out := ev.Output.AsMap()
	data := out["data"].(map[string]any)
	if data["password"] != "s3cret-it" || data["username"] != "admin" {
		t.Errorf("data=%v", data)
	}
	if len(audr.events) != 1 {
		t.Fatalf("audit events=%d, want 1", len(audr.events))
	}
	got := audr.events[0]
	if got.EventType != audit.EventVaultKVRead {
		t.Errorf("event_type=%q", got.EventType)
	}
	if _, has := got.Payload["password"]; has {
		t.Error("audit payload leaked password")
	}
}

func TestIntegration_VaultKVRead_FieldsFilter(t *testing.T) {
	seedKV(t, "redis/filter", map[string]any{
		"username": "admin",
		"password": "filtered-it",
		"extra":    "noise",
	})

	m := coremodvault.New(integrationClient, &capturingAudit{})
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: coremodvault.StateRead,
		Params: mustStructIT(t, map[string]any{
			"path":   "secret/redis/filter",
			"fields": []any{"password"},
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	out := stream.Last().Output.AsMap()
	data := out["data"].(map[string]any)
	if _, has := data["username"]; has {
		t.Error("leak: data contains username outside requested fields")
	}
	if data["password"] != "filtered-it" {
		t.Errorf("data[password]=%v", data["password"])
	}
	keys := sortedKeys(data)
	if !reflect.DeepEqual(keys, []string{"password"}) {
		t.Errorf("data keys=%v", keys)
	}
}

func TestIntegration_VaultKVRead_NotFound(t *testing.T) {
	m := coremodvault.New(integrationClient, &capturingAudit{})
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: coremodvault.StateRead,
		Params: mustStructIT(t, map[string]any{
			"path": "secret/missing/path",
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Failed {
		t.Fatal("expected failed=true on missing path")
	}
}

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
