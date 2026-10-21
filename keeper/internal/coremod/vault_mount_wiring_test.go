package coremod_test

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/coremod"
	"github.com/souls-guild/soul-stack/keeper/internal/coremod/internaltest"
	"github.com/souls-guild/soul-stack/keeper/internal/coremod/util"
	"github.com/souls-guild/soul-stack/keeper/internal/coremod/vault"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/protobuf/types/known/structpb"
)

// mountVault records the prefixes kv-absent lists and the paths it deletes, and
// holds one entry under whatever subtree it is asked about.
type mountVault struct {
	noopVault
	lists   []string
	deletes []string
}

func (v *mountVault) ListKVEntries(_ context.Context, prefix string) ([]string, error) {
	v.lists = append(v.lists, prefix)
	if strings.Count(prefix, "/") == 3 {
		return []string{"admin_password"}, nil
	}
	return nil, nil
}

func (v *mountVault) DeleteKV(_ context.Context, p string) (bool, error) {
	v.deletes = append(v.deletes, p)
	return true, nil
}

// TestDefault_KVAbsentDerivesUnderTheConfiguredMount — `vault.kv_mount: kv` reaches
// kv-absent through coremod.Default. Without it the step derives under the default
// `secret/` mount: on a Keeper whose secrets live under `kv/` it lists a subtree
// that holds nothing and reports a destroyed incarnation's secrets as already gone,
// while they stay where the writes put them.
func TestDefault_KVAbsentDerivesUnderTheConfiguredMount(t *testing.T) {
	v := &mountVault{}
	reg := coremod.Default(coremod.Deps{
		SoulStore:  noopSoulStore{},
		Vault:      v,
		VaultMount: "kv",
	})
	mod, ok := reg.Lookup(vault.Name)
	if !ok {
		t.Fatal("core.vault is not registered")
	}
	ctx := util.WithRunScope(
		util.WithService(util.WithIncarnation(context.Background(), "prod-1"), "dragonfly"),
		util.RunScope{Scenario: "destroy", ApplyID: "01JDESTROY", Destroy: true})
	params, _ := structpb.NewStruct(map[string]any{"scope": "incarnation"})
	stream := internaltest.NewApplyStreamCtx(ctx)
	if err := mod.Apply(&pluginv1.ApplyRequest{State: vault.StateAbsent, Params: params}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if ev := stream.Last(); ev == nil || ev.GetFailed() {
		t.Fatalf("kv-absent failed: %v", ev)
	}
	if !reflect.DeepEqual(v.lists, []string{"kv/dragonfly/prod-1/"}) {
		t.Errorf("listed %v, want exactly kv/dragonfly/prod-1/", v.lists)
	}
	if !reflect.DeepEqual(v.deletes, []string{"kv/dragonfly/prod-1/admin_password"}) {
		t.Errorf("deleted %v, want kv/dragonfly/prod-1/admin_password", v.deletes)
	}
}
