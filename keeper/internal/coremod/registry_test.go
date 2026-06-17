package coremod_test

import (
	"context"
	"sort"
	"testing"

	keeperchoir "github.com/souls-guild/soul-stack/keeper/internal/choir"
	"github.com/souls-guild/soul-stack/keeper/internal/coremod"
	coremodchoir "github.com/souls-guild/soul-stack/keeper/internal/coremod/choir"
	"github.com/souls-guild/soul-stack/keeper/internal/coremod/cloud"
	"github.com/souls-guild/soul-stack/keeper/internal/coremod/soul"
	"github.com/souls-guild/soul-stack/keeper/internal/coremod/vault"
	keepersoul "github.com/souls-guild/soul-stack/keeper/internal/soul"

	"github.com/souls-guild/soul-stack/keeper/internal/bootstraptoken"
	"github.com/souls-guild/soul-stack/shared/audit"
)

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
