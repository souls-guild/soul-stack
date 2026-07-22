package cloud

import (
	"context"
	"fmt"

	"github.com/souls-guild/soul-stack/keeper/internal/profile"
	"github.com/souls-guild/soul-stack/keeper/internal/provider"
	"github.com/souls-guild/soul-stack/keeper/internal/vault"
)

// ResolvedProvider is result of resolving Provider registry into data needed for
// CloudDriver plugin call (credentials A-flow, docs/keeper/cloud.md):
//
//   - Driver is CloudDriver plugin name (Provider.Type), by which plugin is
//     registered in discovery cache (PluginHost lookup).
//   - Credentials is plain secret from Vault (by Provider.CredentialsRef) +
//     `region` from Provider registry, merged in single map. Goes to
//     CreateRequest.credentials / DestroyRequest.credentials as Struct.
//     Driver does NOT access Vault — Keeper resolves secret for it (Variant A).
//
// region is placed inside Credentials, not as separate field: it is provider-specific
// (Proxmox/OpenStack have no `region`), driver decides how to read it.
type ResolvedProvider struct {
	Driver      string
	Credentials map[string]any

	// FQDNSuffix is VM provider FQDN suffix (self-onboard Variant T, ADR-017(h)):
	// keeper predicts SID=FQDN as `<name>-<index>.<FQDNSuffix>`. Empty →
	// provider without predictable FQDN (self_onboard: true returns error).
	// NOT placed in Credentials (driver does not use it — returns FQDN itself;
	// suffix needed only by keeper for prediction).
	FQDNSuffix string
}

// regionKey is the key under which `region` from Provider registry is placed in
// credentials map (alongside plain secret from Vault). Driver reads it as
// regular credentials field.
const regionKey = "region"

// ProviderResolver resolves registry links of step `core.cloud.provisioned` into
// data for CloudDriver call:
//
//   - Resolve(provider-name) → [ResolvedProvider]: driver name + plain credentials
//     (param `provider`, symmetric to credentials A-flow).
//   - ResolveProfile(profile-name) → VM-spec params from registry `profiles`
//     (param `profile`, Variant A: `profile` = NAME of registry entry /v1/profiles,
//     not inline object). Profile must be pre-registered.
//
// Both methods held by one [CredentialsResolverPG] (Provider+Vault+Profile —
// single registry link resolution layer), injected into [Module] via one field. For
// unit tests of module — use fake (see provisioned_test.go).
type ProviderResolver interface {
	Resolve(ctx context.Context, providerName string) (*ResolvedProvider, error)
	// ResolveProfile resolves Profile name to its VM-spec params. Name not found
	// in registry → error (caller returns SendFailed; masking not required —
	// Profile.Params carries no secrets, but caller runs through maskErr
	// uniformly anyway).
	ResolveProfile(ctx context.Context, profileName string) (map[string]any, error)
}

// ProviderReader is narrow subset of provider-CRUD (SelectByName), needed by
// resolver. Narrow interface simplifies unit tests without PG.
type ProviderReader interface {
	SelectByName(ctx context.Context, name string) (*provider.Provider, error)
}

// ProfileReader is narrow subset of profile-CRUD (SelectByName), symmetric to
// [ProviderReader]. Narrow interface simplifies unit tests without PG.
type ProfileReader interface {
	SelectByName(ctx context.Context, name string) (*profile.Profile, error)
}

// VaultReader is narrow subset of keeper/internal/vault.Client (ReadKV),
// symmetric to coremod/vault.VaultReader. Duplicated so resolver does not
// transitively pull full vault pipeline.
type VaultReader interface {
	ReadKV(ctx context.Context, path string) (map[string]any, error)
}

// CredentialsResolverPG is prod implementation of [ProviderResolver]: reads
// Provider from Postgres, resolves `credentials_ref` (vault:<mount>/<path>)
// via Vault KV, returns driver name (Provider.Type) + plain credentials with
// added `region`. Profile registry resolved via [ResolveProfile] (same
// registry link resolution layer).
type CredentialsResolverPG struct {
	Providers ProviderReader
	Profiles  ProfileReader
	Vault     VaultReader
}

// NewCredentialsResolverPG is wire helper. profiles required: param `profile`
// of step `core.cloud.created` resolved via ResolveProfile (Variant A).
func NewCredentialsResolverPG(p ProviderReader, profiles ProfileReader, v VaultReader) *CredentialsResolverPG {
	return &CredentialsResolverPG{Providers: p, Profiles: profiles, Vault: v}
}

// providerReaderFunc adapts package function provider.SelectByName
// (free function, not method) to [ProviderReader]. db fixed at wire-up.
type providerReaderFunc struct {
	db provider.ExecQueryRower
}

// NewProviderReaderPG wraps pgxpool.Pool (or Conn/Tx) into [ProviderReader]
// using free function provider.SelectByName.
func NewProviderReaderPG(db provider.ExecQueryRower) ProviderReader {
	return providerReaderFunc{db: db}
}

func (r providerReaderFunc) SelectByName(ctx context.Context, name string) (*provider.Provider, error) {
	return provider.SelectByName(ctx, r.db, name)
}

// profileReaderFunc adapts package function profile.SelectByName to
// [ProfileReader], symmetric to [providerReaderFunc].
type profileReaderFunc struct {
	db profile.ExecQueryRower
}

// NewProfileReaderPG wraps pgxpool.Pool (or Conn/Tx) into [ProfileReader]
// using free function profile.SelectByName.
func NewProfileReaderPG(db profile.ExecQueryRower) ProfileReader {
	return profileReaderFunc{db: db}
}

func (r profileReaderFunc) SelectByName(ctx context.Context, name string) (*profile.Profile, error) {
	return profile.SelectByName(ctx, r.db, name)
}

// Resolve reads Provider by name, resolves credentials_ref via Vault and
// assembles credentials map. region added under key [regionKey].
//
// Security: returned Credentials contain plain secret — caller must
// run it through audit.MaskSecrets on ANY output (see provisioned.go).
func (r *CredentialsResolverPG) Resolve(ctx context.Context, providerName string) (*ResolvedProvider, error) {
	p, err := r.Providers.SelectByName(ctx, providerName)
	if err != nil {
		return nil, fmt.Errorf("resolve provider %q: %w", providerName, err)
	}

	logical, err := vault.ParseRef(p.CredentialsRef)
	if err != nil {
		return nil, fmt.Errorf("provider %q credentials_ref: %w", providerName, err)
	}

	secret, err := r.Vault.ReadKV(ctx, logical)
	if err != nil {
		return nil, fmt.Errorf("provider %q vault read: %w", providerName, err)
	}

	creds := make(map[string]any, len(secret)+1)
	for k, v := range secret {
		creds[k] = v
	}
	// region from Provider registry takes priority over same-named secret field:
	// registry value is authoritative source, secret holds auth data only.
	creds[regionKey] = p.Region

	// fqdn_suffix (self-onboard Variant T) optional; empty string when nil.
	var suffix string
	if p.FQDNSuffix != nil {
		suffix = *p.FQDNSuffix
	}

	return &ResolvedProvider{Driver: p.Type, Credentials: creds, FQDNSuffix: suffix}, nil
}

// ResolveProfile reads Profile by name and returns its VM-spec params
// (Variant A: param `profile` of step `core.cloud.created` = NAME of registry entry
// /v1/profiles). Name not found → [profile.ErrProfileNotFound] (caller returns
// SendFailed). Params may be nil (profile without VM-spec — valid).
func (r *CredentialsResolverPG) ResolveProfile(ctx context.Context, profileName string) (map[string]any, error) {
	p, err := r.Profiles.SelectByName(ctx, profileName)
	if err != nil {
		return nil, fmt.Errorf("resolve profile %q: %w", profileName, err)
	}
	return p.Params, nil
}
