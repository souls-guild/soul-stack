package scenario

import (
	"context"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/structpb"

	"github.com/souls-guild/soul-stack/keeper/internal/bootstraptoken"
	keeperchoir "github.com/souls-guild/soul-stack/keeper/internal/choir"
	"github.com/souls-guild/soul-stack/keeper/internal/coremod"
	coremodbootstrap "github.com/souls-guild/soul-stack/keeper/internal/coremod/bootstrap"
	"github.com/souls-guild/soul-stack/keeper/internal/render"
	keepersoul "github.com/souls-guild/soul-stack/keeper/internal/soul"
)

// L0 integration test of the real dispatch path: applyKeeperTask parses the
// author address `core.choir.present` via config.SplitModuleAddr, does a Lookup
// by base key on the real coremod.Default Registry, and calls Apply on the real
// module with the resolved state. Catches a regression of the contract fix
// (qa-blocker): before the fix, keeper derived the wire state from the LAST
// segment + Lookup by the FULL address — a multi-state module (choir
// present/absent) was unreachable.
//
// PG is not started: soul-Store and choir-Store are fake; this verifies exactly
// the address resolution → correct module → correct state, NOT the modules' full
// side-effect behavior (covered by the package _test.go files for choir/soul).

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

type fakeBootstrapIssuer struct{}

func (fakeBootstrapIssuer) IssueBatch(_ context.Context, sids []string, _ string) ([]coremodbootstrap.IssuedHost, error) {
	out := make([]coremodbootstrap.IssuedHost, 0, len(sids))
	for _, sid := range sids {
		tok, err := bootstraptoken.Generate()
		if err != nil {
			return nil, err
		}
		out = append(out, coremodbootstrap.IssuedHost{
			SID: sid, Token: tok, ExpiresAt: time.Now().Add(time.Hour), Created: true,
		})
	}
	return out, nil
}

// --- fake choir-Store ---------------------------------------------------------

type fakeChoirStore struct{}

func (fakeChoirStore) AddVoice(_ context.Context, _ *keeperchoir.Voice) error { return nil }
func (fakeChoirStore) RemoveVoice(_ context.Context, _, _, _ string) error    { return nil }
func (fakeChoirStore) IncarnationExists(_ context.Context, _ string) (bool, error) {
	return true, nil
}

func realKeeperRegistry() *coremod.Registry {
	return coremod.Default(coremod.Deps{
		SoulStore:       fakeSoulStore{},
		BootstrapIssuer: fakeBootstrapIssuer{},
		ChoirStore:      fakeChoirStore{},
	})
}

// TestApplyKeeperTask_RealBootstrap_IssuedResolves is the L0 contract guard
// for the public address. It traverses the real author-address split and the
// real core registry, and proves core.bootstrap.issued is independent of any
// delivery dialer.
func TestApplyKeeperTask_RealBootstrap_IssuedResolves(t *testing.T) {
	r := &Runner{keeperModules: realKeeperRegistry()}
	rt := &render.RenderedTask{
		Index:  0,
		Module: "core.bootstrap.issued",
		Params: mustStructI(t, map[string]any{
			"sids": []any{"vm1.example.com", "vm2.example.com"},
		}),
	}
	changed, failed, output, msg := r.applyKeeperTask(context.Background(), RunSpec{}, nil, rt, nil)
	if failed {
		t.Fatalf("core.bootstrap.issued failed: %q", msg)
	}
	if !changed || output["action"] != coremodbootstrap.StateIssued {
		t.Fatalf("changed=%v output=%v, want issued success", changed, output)
	}
	hosts, _ := output["hosts"].([]any)
	if len(hosts) != 2 {
		t.Fatalf("output hosts=%v, want one delivery record per SID", output["hosts"])
	}
}

func mustStructI(t *testing.T, m map[string]any) *structpb.Struct {
	t.Helper()
	s, err := structpb.NewStruct(m)
	if err != nil {
		t.Fatalf("structpb.NewStruct: %v", err)
	}
	return s
}

// A known base with an unknown state must FAIL, not fall through to some
// default: base core.choir is found, but the module knows only present/absent,
// so `core.choir.provisioned` has to come back as an explicit unknown-state
// event. Without this the address split would silently swallow a typo.
func TestApplyKeeperTask_RealChoir_BadStateFails(t *testing.T) {
	r := &Runner{keeperModules: realKeeperRegistry()}
	rt := &render.RenderedTask{Index: 0, Module: "core.choir.provisioned", Params: mustStructI(t, map[string]any{
		"incarnation": "redis-prod",
		"choir":       "masters",
		"sid":         "h1.example.com",
	})}
	_, failed, _, msg := r.applyKeeperTask(context.Background(), RunSpec{}, nil, rt, nil)
	if !failed {
		t.Fatalf("core.choir.provisioned must fail (unknown state provisioned), got success")
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
	changed, failed, output, msg := r.applyKeeperTask(context.Background(), RunSpec{}, nil, rt, nil)
	if failed {
		t.Fatalf("core.choir.present failed: %q (Lookup(core.choir) hit + state=present should have passed)", msg)
	}
	if !changed {
		t.Fatalf("expected changed=true on AddVoice, msg=%q", msg)
	}
	if output["state"] != "present" {
		t.Errorf("output[state] = %v, want present (state resolved to present)", output["state"])
	}
}
