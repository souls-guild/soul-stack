package coremod_test

import (
	"context"
	"sort"
	"testing"
	"time"

	keeperchoir "github.com/souls-guild/soul-stack/keeper/internal/choir"
	"github.com/souls-guild/soul-stack/keeper/internal/coremod"
	coremodbootstrap "github.com/souls-guild/soul-stack/keeper/internal/coremod/bootstrap"
	coremodchoir "github.com/souls-guild/soul-stack/keeper/internal/coremod/choir"
	"github.com/souls-guild/soul-stack/keeper/internal/coremod/soul"
	coremodstate "github.com/souls-guild/soul-stack/keeper/internal/coremod/state"
	"github.com/souls-guild/soul-stack/keeper/internal/coremod/vault"
	keeperincarnation "github.com/souls-guild/soul-stack/keeper/internal/incarnation"
	keepersoul "github.com/souls-guild/soul-stack/keeper/internal/soul"

	"github.com/souls-guild/soul-stack/keeper/internal/bootstraptoken"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// noopStore / noopVault / noopTokens are minimal test-doubles for wire-check
// (Registry builds, Lookup returns all three modules). Real behavior of each
// module checked in its package _test.go.

type noopSoulStore struct{}

func (noopSoulStore) SelectBySID(_ context.Context, sid string) (*keepersoul.Soul, error) {
	return nil, keepersoul.ErrSoulNotFound
}
func (noopSoulStore) Insert(_ context.Context, _ *keepersoul.Soul) error { return nil }
func (noopSoulStore) UpdateCoven(_ context.Context, _ string, c []string) ([]string, error) {
	return c, nil
}
func (noopSoulStore) SoulsWithSoulprint(_ context.Context, _ []string) (map[string]struct{}, error) {
	return map[string]struct{}{}, nil
}

type noopBootstrapIssuer struct{}

func (noopBootstrapIssuer) IssueBatch(_ context.Context, sids []string, _ string) ([]coremodbootstrap.IssuedHost, error) {
	out := make([]coremodbootstrap.IssuedHost, 0, len(sids))
	for _, sid := range sids {
		tok, err := bootstraptoken.Generate()
		if err != nil {
			return nil, err
		}
		out = append(out, coremodbootstrap.IssuedHost{SID: sid, Token: tok, ExpiresAt: time.Now().Add(time.Hour)})
	}
	return out, nil
}

type noopVault struct{}

func (noopVault) ReadKV(_ context.Context, _ string) (map[string]any, error) {
	return map[string]any{"data": map[string]any{}}, nil
}
func (noopVault) WriteKV(_ context.Context, _ string, _ map[string]any) error { return nil }

type noopAudit struct{}

func (noopAudit) Write(_ context.Context, _ *audit.Event) error { return nil }

type noopChoirStore struct{}

func (noopChoirStore) AddVoice(_ context.Context, _ *keeperchoir.Voice) error { return nil }
func (noopChoirStore) RemoveVoice(_ context.Context, _, _, _ string) error    { return nil }
func (noopChoirStore) IncarnationExists(_ context.Context, _ string) (bool, error) {
	return true, nil
}

// noopStateStore is the capture dependency `core.state.*` gained in [ADR-0084]:
// without it the module is not registered, since it can resolve a secret but not
// record the field it resolved.
type noopStateStore struct{}

func (noopStateStore) CaptureState(_ context.Context, _ keeperincarnation.CaptureSpec, mutate func(map[string]any) (map[string]any, error)) (map[string]any, error) {
	return mutate(map[string]any{})
}

func (noopStateStore) ReadState(_ context.Context, _ string) (map[string]any, error) {
	return map[string]any{}, nil
}

func TestDefault_RegistersAllThree(t *testing.T) {
	r := coremod.Default(coremod.Deps{
		SoulStore:  noopSoulStore{},
		Vault:      noopVault{},
		Audit:      noopAudit{},
		StateStore: noopStateStore{},
	})
	got := r.Names()
	sort.Strings(got)
	want := []string{soul.Name, vault.Name, coremodstate.Name}
	sort.Strings(want)
	if len(got) != len(want) {
		t.Fatalf("Names = %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("Names[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestLookup_KnownAndUnknown(t *testing.T) {
	r := coremod.Default(coremod.Deps{
		SoulStore: noopSoulStore{},
		Vault:     noopVault{},
		Audit:     noopAudit{},
	})
	if _, ok := r.Lookup(soul.Name); !ok {
		t.Errorf("Lookup(%q): not found", soul.Name)
	}
	if _, ok := r.Lookup(vault.Name); !ok {
		t.Errorf("Lookup(%q): not found", vault.Name)
	}
	if _, ok := r.Lookup("core.unknown"); ok {
		t.Errorf("Lookup(core.unknown): unexpected hit")
	}
}

func TestDefault_ChoirMember_RegisteredWhenStorePresent(t *testing.T) {
	r := coremod.Default(coremod.Deps{
		SoulStore:  noopSoulStore{},
		Vault:      noopVault{},
		Audit:      noopAudit{},
		ChoirStore: noopChoirStore{},
	})
	if _, ok := r.Lookup(coremodchoir.Name); !ok {
		t.Fatalf("Lookup(%q): not registered with ChoirStore present", coremodchoir.Name)
	}
}

func TestDefault_ChoirMember_AbsentWhenStoreNil(t *testing.T) {
	r := coremod.Default(coremod.Deps{
		SoulStore: noopSoulStore{},
		Vault:     noopVault{},
		Audit:     noopAudit{},
	})
	if _, ok := r.Lookup(coremodchoir.Name); ok {
		t.Errorf("Lookup(%q): unexpected hit with nil ChoirStore", coremodchoir.Name)
	}
}

// baseDeps is a common set for bootstrap-gate tests (minimally sufficient
// for unconditional core-modules).
func baseDeps() coremod.Deps {
	return coremod.Deps{
		SoulStore: noopSoulStore{},
		Vault:     noopVault{},
		Audit:     noopAudit{},
	}
}

// TestDefault_Bootstrap_GateIsTheIssuerAlone: since NIM-834 removed
// `core.bootstrap.delivered`, minting is the module's whole surface, so the
// issuer is its whole dependency set. Registering it without one would give a
// step that fails inside Apply instead of the "unknown keeper-side module"
// refusal every other unconfigured keeper-side core answers with.
func TestDefault_Bootstrap_GateIsTheIssuerAlone(t *testing.T) {
	d := baseDeps()
	d.BootstrapIssuer = noopBootstrapIssuer{}
	if _, ok := coremod.Default(d).Lookup(coremodbootstrap.Name); !ok {
		t.Fatalf("Lookup(%q): must register with an issuer present", coremodbootstrap.Name)
	}

	if _, ok := coremod.Default(baseDeps()).Lookup(coremodbootstrap.Name); ok {
		t.Errorf("Lookup(%q): must NOT register without an issuer", coremodbootstrap.Name)
	}
}
