package cloud_test

import (
	"context"
	"errors"
	"testing"

	coremodcloud "github.com/souls-guild/soul-stack/keeper/internal/coremod/cloud"
	"github.com/souls-guild/soul-stack/keeper/internal/profile"
	"github.com/souls-guild/soul-stack/keeper/internal/provider"
)

// fakeProviderReader is a ProviderReader stub: returns a preset
// Provider or error.
type fakeProviderReader struct {
	p   *provider.Provider
	err error

	lastName string
}

func (r *fakeProviderReader) SelectByName(_ context.Context, name string) (*provider.Provider, error) {
	r.lastName = name
	if r.err != nil {
		return nil, r.err
	}
	return r.p, nil
}

// fakeProfileReader is a ProfileReader stub: returns a preset Profile
// or error. Mirrors fakeProviderReader.
type fakeProfileReader struct {
	p   *profile.Profile
	err error

	lastName string
}

func (r *fakeProfileReader) SelectByName(_ context.Context, name string) (*profile.Profile, error) {
	r.lastName = name
	if r.err != nil {
		return nil, r.err
	}
	return r.p, nil
}

// fakeVault is a VaultReader stub: maps a logical path to a secret payload.
type fakeVault struct {
	byPath map[string]map[string]any
	err    error

	lastPath string
}

func (v *fakeVault) ReadKV(_ context.Context, path string) (map[string]any, error) {
	v.lastPath = path
	if v.err != nil {
		return nil, v.err
	}
	return v.byPath[path], nil
}

func TestCredentialsResolver_Resolve_OK(t *testing.T) {
	pr := &fakeProviderReader{p: &provider.Provider{
		Name:           "aws-prod",
		Type:           "aws",
		Region:         "eu-west-1",
		CredentialsRef: "vault:secret/cloud/aws-prod",
	}}
	vlt := &fakeVault{byPath: map[string]map[string]any{
		"secret/cloud/aws-prod": {
			"access_key_id":     "AKIA...",
			"secret_access_key": "wJalr...",
		},
	}}
	r := coremodcloud.NewCredentialsResolverPG(pr, &fakeProfileReader{}, vlt)

	got, err := r.Resolve(context.Background(), "aws-prod")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Driver != "aws" {
		t.Errorf("Driver = %q, want aws (Provider.Type)", got.Driver)
	}
	// credentials_ref resolves via ParseRef → logical path without `vault:`.
	if vlt.lastPath != "secret/cloud/aws-prod" {
		t.Errorf("vault path = %q, want secret/cloud/aws-prod", vlt.lastPath)
	}
	if got.Credentials["access_key_id"] != "AKIA..." {
		t.Errorf("access_key_id leaked/lost: %v", got.Credentials["access_key_id"])
	}
	// region is added from the Provider registry into the credentials map.
	if got.Credentials["region"] != "eu-west-1" {
		t.Errorf("region = %v, want eu-west-1 (from Provider registry)", got.Credentials["region"])
	}
}

func TestCredentialsResolver_RegionOverridesSecret(t *testing.T) {
	pr := &fakeProviderReader{p: &provider.Provider{
		Name: "p", Type: "aws", Region: "us-east-1",
		CredentialsRef: "vault:secret/p",
	}}
	vlt := &fakeVault{byPath: map[string]map[string]any{
		"secret/p": {"region": "wrong-from-secret", "k": "v"},
	}}
	r := coremodcloud.NewCredentialsResolverPG(pr, &fakeProfileReader{}, vlt)

	got, err := r.Resolve(context.Background(), "p")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Credentials["region"] != "us-east-1" {
		t.Errorf("region = %v, want registry value us-east-1 (registry wins over secret)", got.Credentials["region"])
	}
}

func TestCredentialsResolver_ProviderNotFound(t *testing.T) {
	pr := &fakeProviderReader{err: provider.ErrProviderNotFound}
	r := coremodcloud.NewCredentialsResolverPG(pr, &fakeProfileReader{}, &fakeVault{})
	if _, err := r.Resolve(context.Background(), "ghost"); err == nil {
		t.Fatal("expected error when provider not found")
	}
}

func TestCredentialsResolver_BadCredentialsRef(t *testing.T) {
	pr := &fakeProviderReader{p: &provider.Provider{
		Name: "p", Type: "aws", Region: "r",
		CredentialsRef: "not-a-vault-ref",
	}}
	r := coremodcloud.NewCredentialsResolverPG(pr, &fakeProfileReader{}, &fakeVault{})
	if _, err := r.Resolve(context.Background(), "p"); err == nil {
		t.Fatal("expected error on malformed credentials_ref")
	}
}

func TestCredentialsResolver_VaultError(t *testing.T) {
	pr := &fakeProviderReader{p: &provider.Provider{
		Name: "p", Type: "aws", Region: "r",
		CredentialsRef: "vault:secret/p",
	}}
	vlt := &fakeVault{err: errors.New("vault down")}
	r := coremodcloud.NewCredentialsResolverPG(pr, &fakeProfileReader{}, vlt)
	if _, err := r.Resolve(context.Background(), "p"); err == nil {
		t.Fatal("expected error when vault read fails")
	}
}

// TestCredentialsResolver_ResolveProfile_OK — Option A: ResolveProfile reads
// a Profile by name and returns its params (VM spec for the driver).
func TestCredentialsResolver_ResolveProfile_OK(t *testing.T) {
	prof := &fakeProfileReader{p: &profile.Profile{
		Name:     "redis-small",
		Provider: "example-dev",
		Params:   map[string]any{"image_id": "ami-0001", "flavor": "s2.medium"},
	}}
	r := coremodcloud.NewCredentialsResolverPG(&fakeProviderReader{}, prof, &fakeVault{})

	params, err := r.ResolveProfile(context.Background(), "redis-small")
	if err != nil {
		t.Fatalf("ResolveProfile: %v", err)
	}
	if prof.lastName != "redis-small" {
		t.Errorf("SelectByName got %q, want redis-small", prof.lastName)
	}
	if params["image_id"] != "ami-0001" || params["flavor"] != "s2.medium" {
		t.Errorf("profile params not returned: %v", params)
	}
}

// TestCredentialsResolver_ResolveProfile_NotFound — name not in the registry →
// error (caller returns SendFailed, not a nil-panic).
func TestCredentialsResolver_ResolveProfile_NotFound(t *testing.T) {
	prof := &fakeProfileReader{err: profile.ErrProfileNotFound}
	r := coremodcloud.NewCredentialsResolverPG(&fakeProviderReader{}, prof, &fakeVault{})
	if _, err := r.ResolveProfile(context.Background(), "ghost"); err == nil {
		t.Fatal("expected error when profile not found")
	}
}
