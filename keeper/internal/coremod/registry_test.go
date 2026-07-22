package coremod_test

import (
	"context"
	"sort"
	"testing"

	keeperchoir "github.com/souls-guild/soul-stack/keeper/internal/choir"
	"github.com/souls-guild/soul-stack/keeper/internal/coremod"
	coremodbootstrap "github.com/souls-guild/soul-stack/keeper/internal/coremod/bootstrap"
	coremodchoir "github.com/souls-guild/soul-stack/keeper/internal/coremod/choir"
	"github.com/souls-guild/soul-stack/keeper/internal/coremod/cloud"
	"github.com/souls-guild/soul-stack/keeper/internal/coremod/soul"
	"github.com/souls-guild/soul-stack/keeper/internal/coremod/vault"
	"github.com/souls-guild/soul-stack/keeper/internal/push"
	keepersoul "github.com/souls-guild/soul-stack/keeper/internal/soul"

	"github.com/souls-guild/soul-stack/keeper/internal/bootstraptoken"
	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// noopDialer is a mock of push.Dialer for gate tests (never called: only check
// module registration, not delivery).
func noopDialer(_ context.Context, _ push.DialConfig) (push.Session, error) { return nil, nil }

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

type noopCloudSouls struct{}

func (noopCloudSouls) Insert(_ context.Context, _ *keepersoul.Soul) error { return nil }
func (noopCloudSouls) UpdateStatus(_ context.Context, _ string, _ keepersoul.Status, _ *string) error {
	return nil
}
func (noopCloudSouls) DeleteBySID(_ context.Context, _ string) error { return nil }

type noopCloudTokens struct{}

func (noopCloudTokens) Generate() (bootstraptoken.PlainToken, error) {
	return bootstraptoken.Generate()
}
func (noopCloudTokens) Insert(_ context.Context, sid, _ string, _ *string) (*bootstraptoken.Record, error) {
	return &bootstraptoken.Record{SID: sid}, nil
}
func (noopCloudTokens) DeleteByTokenID(_ context.Context, _ string) error { return nil }

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

func TestDefault_RegistersAllThree(t *testing.T) {
	r := coremod.Default(coremod.Deps{
		SoulStore:   noopSoulStore{},
		PluginHost:  cloud.StubHost{},
		CloudSouls:  noopCloudSouls{},
		CloudTokens: noopCloudTokens{},
		Vault:       noopVault{},
		Audit:       noopAudit{},
	})
	got := r.Names()
	sort.Strings(got)
	want := []string{cloud.Name, soul.Name, vault.Name}
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
		SoulStore:   noopSoulStore{},
		PluginHost:  cloud.StubHost{},
		CloudSouls:  noopCloudSouls{},
		CloudTokens: noopCloudTokens{},
		Vault:       noopVault{},
		Audit:       noopAudit{},
	})
	if _, ok := r.Lookup(soul.Name); !ok {
		t.Errorf("Lookup(%q): not found", soul.Name)
	}
	if _, ok := r.Lookup(cloud.Name); !ok {
		t.Errorf("Lookup(%q): not found", cloud.Name)
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
		SoulStore:   noopSoulStore{},
		PluginHost:  cloud.StubHost{},
		CloudSouls:  noopCloudSouls{},
		CloudTokens: noopCloudTokens{},
		Vault:       noopVault{},
		Audit:       noopAudit{},
		ChoirStore:  noopChoirStore{},
	})
	if _, ok := r.Lookup(coremodchoir.Name); !ok {
		t.Fatalf("Lookup(%q): not registered with ChoirStore present", coremodchoir.Name)
	}
}

func TestDefault_ChoirMember_AbsentWhenStoreNil(t *testing.T) {
	r := coremod.Default(coremod.Deps{
		SoulStore:   noopSoulStore{},
		PluginHost:  cloud.StubHost{},
		CloudSouls:  noopCloudSouls{},
		CloudTokens: noopCloudTokens{},
		Vault:       noopVault{},
		Audit:       noopAudit{},
	})
	if _, ok := r.Lookup(coremodchoir.Name); ok {
		t.Errorf("Lookup(%q): unexpected hit with nil ChoirStore", coremodchoir.Name)
	}
}

// baseDeps is a common set for bootstrap-gate tests (minimally sufficient
// for unconditional core-modules).
func baseDeps() coremod.Deps {
	return coremod.Deps{
		SoulStore:   noopSoulStore{},
		PluginHost:  cloud.StubHost{},
		CloudSouls:  noopCloudSouls{},
		CloudTokens: noopCloudTokens{},
		Vault:       noopVault{},
		Audit:       noopAudit{},
	}
}

// TestDefault_Bootstrap_TeleportRegistersWithEmptyHostCAs is guard #4 (gate,
// teleport-half): teleport mode registers `core.bootstrap` with ONLY dialer,
// empty Providers/HostCAs (ADR-063 amendment: host-verify via Teleport,
// Authorize/Sign not called).
func TestDefault_Bootstrap_TeleportRegistersWithEmptyHostCAs(t *testing.T) {
	d := baseDeps()
	d.BootstrapTransport = coremodbootstrap.TransportTeleport
	d.BootstrapDial = noopDialer
	// Providers/HostCAs intentionally empty.
	r := coremod.Default(d)
	if _, ok := r.Lookup(coremodbootstrap.Name); !ok {
		t.Fatalf("Lookup(%q): teleport-mode must register with empty Providers/HostCAs", coremodbootstrap.Name)
	}
}

// TestDefault_Bootstrap_TeleportNeedsDialer is guard #4: teleport WITHOUT dialer
// not registered (nothing to connect with).
func TestDefault_Bootstrap_TeleportNeedsDialer(t *testing.T) {
	d := baseDeps()
	d.BootstrapTransport = coremodbootstrap.TransportTeleport
	d.BootstrapDial = nil
	r := coremod.Default(d)
	if _, ok := r.Lookup(coremodbootstrap.Name); ok {
		t.Errorf("Lookup(%q): must NOT register in teleport-mode without dialer", coremodbootstrap.Name)
	}
}

// TestDefault_Bootstrap_DirectStillRequiresHostCAs is guard #4 (gate, direct-half):
// direct mode still requires full SSH set (dialer + providers + host-CA);
// empty host-CA → not registered.
func TestDefault_Bootstrap_DirectStillRequiresHostCAs(t *testing.T) {
	prov := map[string]coremodbootstrap.SshProviderHost{"ssh-static": stubProvider{}}
	caSigner, _, err := push.NewEphemeralEd25519()
	if err != nil {
		t.Fatalf("NewEphemeralEd25519: %v", err)
	}
	cas := []push.NamedHostKeyAuthority{{Name: "test", CAPubKey: caSigner.PublicKey()}}

	// dialer + providers + host-CA → registers.
	d := baseDeps()
	d.BootstrapDial = noopDialer
	d.BootstrapProviders = prov
	d.BootstrapHostCAs = cas
	if _, ok := coremod.Default(d).Lookup(coremodbootstrap.Name); !ok {
		t.Fatalf("direct-mode with full set must register %q", coremodbootstrap.Name)
	}

	// dialer + providers, but WITHOUT host-CA → NOT registered (fail-closed).
	d2 := baseDeps()
	d2.BootstrapDial = noopDialer
	d2.BootstrapProviders = prov
	// HostCAs empty.
	if _, ok := coremod.Default(d2).Lookup(coremodbootstrap.Name); ok {
		t.Errorf("direct-mode without host-CA must NOT register %q", coremodbootstrap.Name)
	}
}

// stubProvider is an empty implementation of bootstrap.SshProviderHost (= push.SshProvider)
// for gate tests; methods not called (only registration checked).
type stubProvider struct{}

func (stubProvider) Authorize(_ context.Context, _ *pluginv1.AuthorizeRequest) (*pluginv1.AuthorizeReply, error) {
	return &pluginv1.AuthorizeReply{Allowed: true}, nil
}
func (stubProvider) Sign(_ context.Context, _ *pluginv1.SignRequest) (*pluginv1.SignReply, error) {
	return &pluginv1.SignReply{}, nil
}
