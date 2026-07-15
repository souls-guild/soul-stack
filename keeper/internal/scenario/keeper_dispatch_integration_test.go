package scenario

import (
	"context"
	"testing"

	"google.golang.org/protobuf/types/known/structpb"

	"github.com/souls-guild/soul-stack/keeper/internal/bootstraptoken"
	keeperchoir "github.com/souls-guild/soul-stack/keeper/internal/choir"
	"github.com/souls-guild/soul-stack/keeper/internal/coremod"
	"github.com/souls-guild/soul-stack/keeper/internal/coremod/cloud"
	"github.com/souls-guild/soul-stack/keeper/internal/render"
	keepersoul "github.com/souls-guild/soul-stack/keeper/internal/soul"
	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
)

// L0 integration test of the real dispatch path: applyKeeperTask parses the
// author address `core.cloud.created` / `core.choir.present` via
// config.SplitModuleAddr, does a Lookup by base key on the real
// coremod.Default Registry, and calls Apply on the real module with the
// resolved state. Catches a regression of the contract fix (qa-blocker):
// before the fix, keeper derived the wire state from the LAST segment +
// Lookup by the FULL address — multi-state modules (cloud created/destroyed,
// choir present/absent) were unreachable.
//
// PG is not started: cloud dependencies (Resolver/Host/Souls/Tokens),
// soul-Store and choir-Store are fake; this verifies exactly the address
// resolution → correct module → correct state, NOT the modules' full
// side-effect behavior (covered by the package _test.go files for
// cloud/choir/soul).

// --- fake soul-Store (coremod.Deps.SoulStore) ---------------------------------

type fakeSoulStore struct{}

func (fakeSoulStore) SelectBySID(_ context.Context, _ string) (*keepersoul.Soul, error) {
	return nil, keepersoul.ErrSoulNotFound
}
func (fakeSoulStore) Insert(_ context.Context, _ *keepersoul.Soul) error { return nil }
func (fakeSoulStore) UpdateCoven(_ context.Context, _ string, c []string) ([]string, error) {
	return c, nil
}
func (fakeSoulStore) SoulsWithSoulprint(_ context.Context, _ []string) (map[string]struct{}, error) {
	return map[string]struct{}{}, nil
}

// --- fake cloud dependencies (happy-path created, no PG) ---------------------

type fakeResolver struct{}

func (fakeResolver) Resolve(_ context.Context, _ string) (*cloud.ResolvedProvider, error) {
	return &cloud.ResolvedProvider{Driver: "fake-driver", Credentials: map[string]any{}}, nil
}

func (fakeResolver) ResolveProfile(_ context.Context, _ string) (map[string]any, error) {
	return nil, nil
}

type fakeHost struct{ cloud.StubHost }

func (fakeHost) Create(_ context.Context, _ string, _, _ map[string]any, count int32, _, _ string) ([]*pluginv1.VmInfo, error) {
	out := make([]*pluginv1.VmInfo, 0, count)
	for i := int32(0); i < count; i++ {
		out = append(out, &pluginv1.VmInfo{VmId: "vm-1", Fqdn: "vm1.example.com", PrimaryIp: "10.0.0.1"})
	}
	return out, nil
}

type fakeCloudSouls struct{}

func (fakeCloudSouls) Insert(_ context.Context, _ *keepersoul.Soul) error { return nil }
func (fakeCloudSouls) UpdateStatus(_ context.Context, _ string, _ keepersoul.Status, _ *string) error {
	return nil
}
func (fakeCloudSouls) DeleteBySID(_ context.Context, _ string) error { return nil }

type fakeCloudTokens struct{}

func (fakeCloudTokens) Generate() (bootstraptoken.PlainToken, error) {
	return bootstraptoken.Generate()
}
func (fakeCloudTokens) Insert(_ context.Context, sid, _ string, _ *string) (*bootstraptoken.Record, error) {
	return &bootstraptoken.Record{SID: sid}, nil
}
func (fakeCloudTokens) DeleteByTokenID(_ context.Context, _ string) error { return nil }

// --- fake choir-Store ---------------------------------------------------------

type fakeChoirStore struct{}

func (fakeChoirStore) AddVoice(_ context.Context, _ *keeperchoir.Voice) error { return nil }
func (fakeChoirStore) RemoveVoice(_ context.Context, _, _, _ string) error    { return nil }
func (fakeChoirStore) IncarnationExists(_ context.Context, _ string) (bool, error) {
	return true, nil
}

func realKeeperRegistry() *coremod.Registry {
	return coremod.Default(coremod.Deps{
		SoulStore:     fakeSoulStore{},
		PluginHost:    fakeHost{},
		CloudResolver: fakeResolver{},
		CloudSouls:    fakeCloudSouls{},
		CloudTokens:   fakeCloudTokens{},
		ChoirStore:    fakeChoirStore{},
	})
}

func mustStructI(t *testing.T, m map[string]any) *structpb.Struct {
	t.Helper()
	s, err := structpb.NewStruct(m)
	if err != nil {
		t.Fatalf("structpb.NewStruct: %v", err)
	}
	return s
}

func TestApplyKeeperTask_RealCloud_CreatedResolves(t *testing.T) {
	r := &Runner{keeperModules: realKeeperRegistry()}
	rt := &render.RenderedTask{
		Index:  0,
		Module: "core.cloud.created",
		Params: mustStructI(t, map[string]any{
			"provider": "fake",
			"count":    float64(1),
		}),
	}
	changed, failed, output, msg := r.applyKeeperTask(context.Background(), rt)
	if failed {
		t.Fatalf("core.cloud.created failed: %q (Lookup(core.cloud) hit + state=created должны были пройти)", msg)
	}
	if !changed {
		t.Fatalf("expected changed=true on created, msg=%q", msg)
	}
	if output["action"] != "created" {
		t.Errorf("output[action] = %v, want created (state резолвлен в created)", output["action"])
	}
}

// core.cloud.provisioned is the old (incorrect) form: base core.cloud is
// found, but the module doesn't know state "provisioned" → failed event
// "unknown state". Confirms the author form is now created/destroyed, not
// provisioned.
func TestApplyKeeperTask_RealCloud_BadStateFails(t *testing.T) {
	r := &Runner{keeperModules: realKeeperRegistry()}
	rt := &render.RenderedTask{Index: 0, Module: "core.cloud.provisioned", Params: mustStructI(t, map[string]any{"provider": "fake"})}
	_, failed, _, msg := r.applyKeeperTask(context.Background(), rt)
	if !failed {
		t.Fatalf("core.cloud.provisioned must fail (unknown state provisioned), got success")
	}
	if msg == "" {
		t.Fatalf("expected unknown-state message")
	}
}

func TestApplyKeeperTask_RealChoir_PresentResolves(t *testing.T) {
	r := &Runner{keeperModules: realKeeperRegistry()}
	rt := &render.RenderedTask{
		Index:  0,
		Module: "core.choir.present",
		Params: mustStructI(t, map[string]any{
			"incarnation": "redis-prod",
			"choir":       "masters",
			"sid":         "h1.example.com",
		}),
	}
	changed, failed, output, msg := r.applyKeeperTask(context.Background(), rt)
	if failed {
		t.Fatalf("core.choir.present failed: %q (Lookup(core.choir) hit + state=present должны были пройти)", msg)
	}
	if !changed {
		t.Fatalf("expected changed=true on AddVoice, msg=%q", msg)
	}
	if output["state"] != "present" {
		t.Errorf("output[state] = %v, want present (state резолвлен в present)", output["state"])
	}
}
