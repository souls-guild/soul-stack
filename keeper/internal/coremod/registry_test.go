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

// noopDialer — мок push.Dialer для gate-тестов (никогда не вызывается: проверяем
// только факт регистрации модуля, не доставку).
func noopDialer(_ context.Context, _ push.DialConfig) (push.Session, error) { return nil, nil }

// noopStore / noopVault / noopTokens — минимальные test-doubles для wire-проверки
// (Registry строится, Lookup возвращает все три модуля). Реальное поведение
// каждого модуля проверяется в его пакетных _test.go.

type noopSoulStore struct{}

func (noopSoulStore) SelectBySID(_ context.Context, sid string) (*keepersoul.Soul, error) {
	return nil, keepersoul.ErrSoulNotFound
}
func (noopSoulStore) Insert(_ context.Context, _ *keepersoul.Soul) error { return nil }
func (noopSoulStore) UpdateCoven(_ context.Context, _ string, c []string) ([]string, error) {
	return c, nil
}

type noopCloudSouls struct{}

func (noopCloudSouls) Insert(_ context.Context, _ *keepersoul.Soul) error { return nil }
func (noopCloudSouls) UpdateStatus(_ context.Context, _ string, _ keepersoul.Status, _ *string) error {
	return nil
}

type noopCloudTokens struct{}

func (noopCloudTokens) Generate() (bootstraptoken.PlainToken, error) {
	return bootstraptoken.Generate()
}
func (noopCloudTokens) Insert(_ context.Context, sid, _ string, _ *string) (*bootstraptoken.Record, error) {
	return &bootstraptoken.Record{SID: sid}, nil
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

// baseDeps — общий набор для bootstrap-gate тестов (минимально достаточный для
// безусловных core-модулей).
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

// TestDefault_Bootstrap_TeleportRegistersWithEmptyHostCAs — guard #4 (гейт,
// teleport-half): teleport-режим регистрирует `core.bootstrap` при наличии ТОЛЬКО
// dialer-а, с пустыми Providers/HostCAs (ADR-063 amendment: host-verify через
// Teleport, Authorize/Sign не вызываются).
func TestDefault_Bootstrap_TeleportRegistersWithEmptyHostCAs(t *testing.T) {
	d := baseDeps()
	d.BootstrapTransport = coremodbootstrap.TransportTeleport
	d.BootstrapDial = noopDialer
	// Providers/HostCAs намеренно пусты.
	r := coremod.Default(d)
	if _, ok := r.Lookup(coremodbootstrap.Name); !ok {
		t.Fatalf("Lookup(%q): teleport-mode must register with empty Providers/HostCAs", coremodbootstrap.Name)
	}
}

// TestDefault_Bootstrap_TeleportNeedsDialer — guard #4: teleport БЕЗ dialer-а не
// регистрируется (нечем коннектиться).
func TestDefault_Bootstrap_TeleportNeedsDialer(t *testing.T) {
	d := baseDeps()
	d.BootstrapTransport = coremodbootstrap.TransportTeleport
	d.BootstrapDial = nil
	r := coremod.Default(d)
	if _, ok := r.Lookup(coremodbootstrap.Name); ok {
		t.Errorf("Lookup(%q): must NOT register in teleport-mode without dialer", coremodbootstrap.Name)
	}
}

// TestDefault_Bootstrap_DirectStillRequiresHostCAs — guard #4 (гейт, direct-half):
// direct-режим по-прежнему требует полный SSH-набор (dialer + providers + host-CA);
// пустой host-CA → не регистрируется.
func TestDefault_Bootstrap_DirectStillRequiresHostCAs(t *testing.T) {
	prov := map[string]coremodbootstrap.SshProviderHost{"ssh-static": stubProvider{}}
	caSigner, _, err := push.NewEphemeralEd25519()
	if err != nil {
		t.Fatalf("NewEphemeralEd25519: %v", err)
	}
	cas := []push.NamedHostKeyAuthority{{Name: "test", CAPubKey: caSigner.PublicKey()}}

	// dialer + providers + host-CA → регистрируется.
	d := baseDeps()
	d.BootstrapDial = noopDialer
	d.BootstrapProviders = prov
	d.BootstrapHostCAs = cas
	if _, ok := coremod.Default(d).Lookup(coremodbootstrap.Name); !ok {
		t.Fatalf("direct-mode with full set must register %q", coremodbootstrap.Name)
	}

	// dialer + providers, но БЕЗ host-CA → НЕ регистрируется (fail-closed).
	d2 := baseDeps()
	d2.BootstrapDial = noopDialer
	d2.BootstrapProviders = prov
	// HostCAs пуст.
	if _, ok := coremod.Default(d2).Lookup(coremodbootstrap.Name); ok {
		t.Errorf("direct-mode without host-CA must NOT register %q", coremodbootstrap.Name)
	}
}

// stubProvider — пустая реализация bootstrap.SshProviderHost (= push.SshProvider)
// для gate-тестов; методы не вызываются (проверяется только регистрация).
type stubProvider struct{}

func (stubProvider) Authorize(_ context.Context, _ *pluginv1.AuthorizeRequest) (*pluginv1.AuthorizeReply, error) {
	return &pluginv1.AuthorizeReply{Allowed: true}, nil
}
func (stubProvider) Sign(_ context.Context, _ *pluginv1.SignRequest) (*pluginv1.SignReply, error) {
	return &pluginv1.SignReply{}, nil
}
