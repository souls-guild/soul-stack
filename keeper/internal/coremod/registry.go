// Package coremod wires keeper-side core modules (ADR-017,
// docs/keeper/modules.md) into a single Registry.
//
// Modules (Registry key = base name, author form = base + state in address):
// `core.soul` (`core.soul.registered`, docs/keeper/modules.md), `core.cloud`
// (`core.cloud.created`/`core.cloud.destroyed`, ADR-017(a), Plugin.d-pending),
// `core.vault` (`core.vault.kv-read`/`core.vault.kv-present`, ADR-017(b)) and `core.choir`
// (`core.choir.present`/`core.choir.absent`, ADR-044 — membership changes in
// Choir of incarnation, registered when Deps.ChoirStore is present). All
// execute on keeper instance, scenario-runner dispatcher is `on: keeper`.
//
// Symmetrically soul/internal/coremod (Soul-side, ADR-015): same interface
// sdk/module.SoulModule, same Registry pattern. Difference is where step runs
// and which deps (PG-pool / Vault / PluginHost vs apt/systemd).
package coremod

import (
	"github.com/souls-guild/soul-stack/keeper/internal/coremod/bootstrap"
	"github.com/souls-guild/soul-stack/keeper/internal/coremod/cert"
	"github.com/souls-guild/soul-stack/keeper/internal/coremod/choir"
	"github.com/souls-guild/soul-stack/keeper/internal/coremod/cloud"
	"github.com/souls-guild/soul-stack/keeper/internal/coremod/soul"
	"github.com/souls-guild/soul-stack/keeper/internal/coremod/vault"
	"github.com/souls-guild/soul-stack/keeper/internal/push"
	"github.com/souls-guild/soul-stack/sdk/module"
)

// Registry is immutable set of module base-name → SoulModule implementation.
//
// Symmetric to soul/internal/coremod.Registry: key is module base-name WITHOUT
// state suffix (`core.soul`, not `core.soul.registered`). Author form of task
// address is base + state (`core.soul.registered`, `core.cloud.created`);
// config.SplitModuleAddr splits address into (base, state) in keeper_dispatch,
// base goes to Lookup, state goes to pluginv1.ApplyRequest.state and is handled
// inside implementation.
type Registry struct {
	mods map[string]module.SoulModule
}

// Deps are external dependencies for keeper-side modules. Fields are required
// (except `Audit` — may be nil in test builds; prod always has
// auditmulti.Writer).
type Deps struct {
	// SoulStore is keeper/internal/coremod/soul.Store.
	SoulStore soul.Store

	// SoulPresence is presence-checker (Redis SID-lease) for onboarding barrier
	// `core.soul.registered` `await_online` (ADR-061). nil is allowed (test builds
	// / dev without Redis): step with `await_online: true` will fail
	// (barrier needs presence source). Prod wraps
	// keeperredis.SoulsStreamAlive (same source as topology.SoulLeaseChecker).
	SoulPresence soul.PresenceChecker

	// MaxAwaitTimeout is string ceiling provider for keeper.yml::max_await_timeout
	// (ADR-061), hot-reload-aware (read on each Apply). nil → defaults to
	// config.DefaultMaxAwaitTimeout. Prod wraps config.Store.Get().
	MaxAwaitTimeout func() string

	// PluginHost is keeper/internal/coremod/cloud.PluginHost. Before Plugin.d
	// completes, caller provides cloud.StubHost{}; interface is fixed early so
	// Registry can build without Plugin.d dependency.
	PluginHost cloud.PluginHost

	// CloudResolver resolves param `provider` to driver name + plain credentials
	// (A-flow): Provider registry + Vault. Prod is cloud.CredentialsResolverPG.
	CloudResolver cloud.ProviderResolver

	// CloudSouls / CloudTokens are narrow PG-adapters for cloud module.
	// Separated from SoulStore: module `core.cloud` calls different methods
	// (Insert + UpdateStatus), non-overlapping with `core.soul`
	// (SelectBySID + Insert + UpdateCoven).
	CloudSouls  cloud.SoulStore
	CloudTokens cloud.TokenStore

	// CloudCascade is cascade handler for `destroyed` state (ADR-017).
	// Implemented via [cloud.CascadePG] on pgxpool.Pool. Allowed to be nil in
	// test builds without destroyed scenarios.
	CloudCascade cloud.Cascader

	// CloudUserdata is cloud-init userdata resolver for scenario param
	// `generate_userdata: true` (ADR-017(h) amendment 2026-05-27, B-flat).
	// Prod implementation wraps cloudinit.Resolver+GenerateUserdata in
	// daemon (reads current KeeperConfig.CloudInit snapshot + Vault.ReadKV).
	// nil is allowed: `generate_userdata: true` returns error,
	// explicit `userdata:` continues to work unchanged.
	CloudUserdata cloud.UserdataProvider

	// Vault is vault-client for `core.vault` (kv-read reads; kv-present
	// generate-if-absent reads+writes). *vault.Client satisfies both;
	// kv-read write-path does not call (read-state).
	Vault vault.VaultWriter

	// ChoirStore is choir-CRUD adapter (ADR-044) for `core.choir`:
	// AddVoice/RemoveVoice on incarnation_choir_voices + incarnation existence
	// check. Prod is choir.NewPGStore(pool). nil is allowed in test builds
	// without choir scenarios (module not registered).
	ChoirStore choir.Store

	// CertStore is warrant-CRUD adapter (cert-rotation Var1, E1) for
	// `core.cert.registered`: SelectActive + RegisterActive on `warrant`. Prod
	// is cert.NewPGStore(pool). nil is allowed in test builds without cert
	// registration (module not registered — like choir). Module reads cert-PEM
	// from Vault (shared Deps.Vault) and extracts metadata itself, so no separate
	// Vault field is needed.
	CertStore cert.Store

	// KID is Keeper instance identifier, passed to
	// `core.cert.registered` (warrant.issued_by_kid). Empty → NULL in registry.
	KID string

	// BootstrapTransport is token delivery mode for `core.bootstrap.delivered`
	// (ADR-063 amendment): bootstrap.TransportDirect ("" → direct) or
	// bootstrap.TransportTeleport. Source is keeper.yml::push.transport.
	// Determines which other Bootstrap* fields are required for module
	// registration (see gate in Default).
	BootstrapTransport string

	// BootstrapProviders / BootstrapHostCAs / BootstrapDial are dependencies
	// for keeper-side core module `core.bootstrap.delivered` (ADR-063, per-VM
	// bootstrap-token delivery over SSH).
	//
	// direct mode: all three are wired from same push infrastructure as
	// SshDispatcher (discovered SshProvider plugins by manifest.Name +
	// host-CA from Vault + push.Dial). Module registered only when
	// BootstrapProviders non-empty AND BootstrapHostCAs non-empty AND BootstrapDial set.
	//
	// teleport mode (ADR-063 amendment): BootstrapDial alone suffices
	// (push.NewTeleportDialer from keeper.yml::push.teleport); BootstrapProviders/
	// BootstrapHostCAs not needed (Authorize/Sign not called, host-verify
	// via Teleport identity-file).
	//
	// Any gap → module not registered, step with that address
	// fails with "unknown keeper-side module" (clear "not configured").
	BootstrapProviders map[string]bootstrap.SshProviderHost
	BootstrapHostCAs   []push.NamedHostKeyAuthority
	BootstrapDial      push.Dialer

	// BootstrapInstall is install-blueprint resolver for install mode
	// `core.bootstrap.delivered` (param `install: true`, teleport only, ADR-063
	// amendment "full-install over SSH"). Prod wrapper reads keeper.yml::cloud_init
	// snapshot + Vault (same cloudinit.Resolver as cloud-init userdata). nil
	// allowed: task with `install: true` returns error, token-only
	// delivery unaffected.
	BootstrapInstall bootstrap.InstallResolver

	// Audit is single audit-writer for keeper-side modules (cloud/vault write
	// audit events; soul/choir do not). nil allowed (modules skip write and
	// continue), but prod wire-up from main should provide real
	// keeper/internal/auditpg or auditmulti.
	Audit AuditWriter
}

// AuditWriter is common type for audit-writing modules (cloud/vault/bootstrap/cert);
// matches shared/audit.Writer.
type AuditWriter interface {
	cloud.AuditWriter
	vault.AuditWriter
	bootstrap.AuditWriter
	cert.AuditWriter
}

// Default builds Registry with keeper-side core modules: unconditionally
// core.soul / core.cloud / core.vault, plus core.choir if
// Deps.ChoirStore present. Caller provides real deps (PG-pool via
// cloud.NewSoulPG / cloud.NewTokenPG / soul.NewPGStore, vault-client from
// keeper/internal/vault, choir.NewPGStore).
func Default(d Deps) *Registry {
	cloudMod := cloud.New(d.PluginHost, d.CloudResolver, d.CloudSouls, d.CloudTokens, d.CloudCascade, d.Audit)
	if d.CloudUserdata != nil {
		cloudMod = cloudMod.WithUserdata(d.CloudUserdata)
	}
	// core.soul.registered with onboarding barrier (ADR-061): presence-checker +
	// await_timeout ceiling provider optional. nil presence →
	// step with await_online: true fails (test/dev builds without Redis).
	soulMod := soul.New(d.SoulStore).WithPresence(d.SoulPresence, d.MaxAwaitTimeout)
	mods := map[string]module.SoulModule{
		soul.Name:  soulMod,
		cloud.Name: cloudMod,
		vault.Name: vault.New(d.Vault, d.Audit),
	}
	// `core.choir` (ADR-044) registered only when ChoirStore present.
	// nil means build without choir scenarios; step with that module
	// fails with "unknown keeper-side module" (like any unconfigured one).
	if d.ChoirStore != nil {
		mods[choir.Name] = choir.New(d.ChoirStore)
	}
	// `core.cert.registered` (cert-rotation Var1, E1) registered when
	// CertStore AND Vault present (module reads cert-PEM from Vault). nil either
	// means build without cert registration (dev without Vault / PG); step with
	// that address fails with "unknown keeper-side module". Symmetric to
	// conditional core.choir registration.
	if d.CertStore != nil && d.Vault != nil {
		mods[cert.Name] = cert.New(d.Vault, d.CertStore, d.Audit, d.KID)
	}
	// `core.bootstrap.delivered` (ADR-063) registered when required dependency
	// set present; set depends on transport (ADR-063 amendment):
	//   - teleport: BootstrapDial alone suffices (Teleport-Dialer); providers/host-CA
	//     not needed (Authorize/Sign not called, host-verify via Teleport);
	//   - direct (default): providers + host-CA + dialer (full SSH set).
	// Any gap means build without push access: step with that
	// address fails with "unknown keeper-side module" (like any unconfigured one).
	// Symmetric to conditional `core.choir` registration.
	if bootstrapModuleConfigured(d) {
		mods[bootstrap.Name] = &bootstrap.Module{
			Transport: d.BootstrapTransport,
			Providers: d.BootstrapProviders,
			HostCAs:   d.BootstrapHostCAs,
			Dial:      d.BootstrapDial,
			Install:   d.BootstrapInstall,
			Audit:     d.Audit,
		}
	}
	return NewRegistry(mods)
}

// bootstrapModuleConfigured decides whether to register `core.bootstrap.delivered`
// (ADR-063 + amendment). teleport mode requires only dialer; direct requires
// full SSH set (providers + host-CA + dialer).
func bootstrapModuleConfigured(d Deps) bool {
	if d.BootstrapDial == nil {
		return false
	}
	if d.BootstrapTransport == bootstrap.TransportTeleport {
		return true
	}
	return len(d.BootstrapProviders) > 0 && len(d.BootstrapHostCAs) > 0
}

// NewRegistry builds Registry from arbitrary set of implementations.
func NewRegistry(mods map[string]module.SoulModule) *Registry {
	cp := make(map[string]module.SoulModule, len(mods))
	for k, v := range mods {
		cp[k] = v
	}
	return &Registry{mods: cp}
}

// Lookup returns module by base-name (without state suffix) and presence flag.
func (r *Registry) Lookup(name string) (module.SoulModule, bool) {
	m, ok := r.mods[name]
	return m, ok
}

// Names returns list of registered modules in non-deterministic order
// (Go map iteration). Used for diagnostic output / healthz.
func (r *Registry) Names() []string {
	out := make([]string, 0, len(r.mods))
	for k := range r.mods {
		out = append(out, k)
	}
	return out
}
