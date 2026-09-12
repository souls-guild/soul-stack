// Package coremod wires keeper-side core modules (ADR-017,
// docs/keeper/modules.md) into a single Registry.
//
// Modules (Registry key = base name, author form = base + state in address):
// `core.soul` (`core.soul.registered`, docs/keeper/modules.md),
// `core.vault` (`core.vault.kv-read`/`core.vault.kv-present`, ADR-017(b)), `core.state`
// (`core.state.*`, [ADR-0083] §4 — the write of a service state field
// carrying declared secrets, registered when Deps.Vault is present) and `core.choir`
// (`core.choir.present`/`core.choir.absent`, ADR-044 — membership changes in
// Choir of incarnation, registered when Deps.ChoirStore is present). All
// execute on keeper instance, scenario-runner dispatcher is `on: keeper`.
//
// Symmetrically soul/internal/coremod (Soul-side, ADR-015): same interface
// sdk/module.SoulModule, same Registry pattern. Difference is where step runs
// and which deps (PG-pool / Vault / PluginHost vs apt/systemd).
package coremod

import (
	"github.com/souls-guild/soul-stack/keeper/internal/certissue"
	"github.com/souls-guild/soul-stack/keeper/internal/coremod/bootstrap"
	"github.com/souls-guild/soul-stack/keeper/internal/coremod/cert"
	"github.com/souls-guild/soul-stack/keeper/internal/coremod/choir"
	"github.com/souls-guild/soul-stack/keeper/internal/coremod/soul"
	coremodssh "github.com/souls-guild/soul-stack/keeper/internal/coremod/ssh"
	"github.com/souls-guild/soul-stack/keeper/internal/coremod/state"
	"github.com/souls-guild/soul-stack/keeper/internal/coremod/vault"
	"github.com/souls-guild/soul-stack/keeper/internal/push"
	"github.com/souls-guild/soul-stack/sdk/module"
)

// Registry is immutable set of module base-name → SoulModule implementation.
//
// Symmetric to soul/internal/coremod.Registry: key is module base-name WITHOUT
// state suffix (`core.soul`, not `core.soul.registered`). Author form of task
// address is base + state (`core.soul.registered`, `core.vault.kv-read`);
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

	// Vault is vault-client for `core.vault` (kv-read reads; kv-present
	// generate-if-absent reads+writes). *vault.Client satisfies both;
	// kv-read write-path does not call (read-state).
	Vault vault.VaultWriter

	// VaultMount is the KV mount from keeper.yml (`vault.kv_mount`), used by
	// `core.state.*` to DERIVE the path of a declared secret ([ADR-0083]
	// §1). "" means the default mount. A snapshot, not a hot-reload accessor,
	// deliberately: the mount is one segment of a derived path, so re-reading it
	// mid-life would orphan every secret already written under the old one. The
	// ADR-064 write path (secretwrite.NewWriter) snapshots the same value.
	VaultMount string

	// StateStore is the incarnation-state write adapter for `core.state.*`
	// ([ADR-0084]): capture a field mid-run and record the run that caused it.
	// Prod is state.NewPGStore(pool). nil is allowed in test builds without state
	// capture (module not registered — like choir).
	StateStore state.Store

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

	// Cert* — dependencies of state `core.cert.issued` (NIM-99): Keeper ITSELF
	// issues the cert. Set on cert.Module after cert.New; nil in any of them →
	// issued returns failed ("not configured"), registered does not depend on them.
	CertSigner      certissue.Signer         // Vault PKI sign CSR
	CertVaultWriter certissue.KVWriter       // writes cert/key into Vault
	CertPolicy      cert.IssuePolicyResolver // resolves the rotation policy from the manifest
	CertCSRGen      certissue.CSRGenFunc     // generates keypair+CSR (keeper-side, R2)
	CertPKIMount    func() string            // hot-reload keeper.yml Vault.PKIMount
	CertKVMount     func() string            // hot-reload keeper.yml Vault.KVMount

	// BootstrapIssuer is the transactional ready-made-VM onboarding backend for
	// `core.bootstrap.issued`. It needs only Keeper Postgres. nil means the
	// module is not registered and a step with that address fails with "unknown
	// keeper-side module" — the same "not configured" signal as choir and cert.
	BootstrapIssuer bootstrap.Issuer

	// SSHDial opens the SSH session for `core.ssh.run` and is that module's whole
	// registration gate: without a dialer there is no transport, and a step with
	// that address must answer "unknown keeper-side module" like any other
	// unconfigured keeper-side core. Prod is push.Dial (direct) or
	// push.NewTeleportDialer (teleport).
	SSHDial push.Dialer

	// SSHTransport is `keeper.yml::push.transport` — which way this Keeper
	// installation reaches its hosts. "" is direct. It is deliberately not a
	// scenario param: the transport is a property of the installation, and a task
	// that could pick one would be a task that has to know the site's topology.
	SSHTransport string

	// SSHProviders / SSHHostCAs resolve the direct transport's authentication at
	// APPLY time rather than at registration. The push dispatcher spawns the
	// SshProvider plugins and loads the Vault host CAs AFTER the core modules are
	// registered (setupPushDispatchers follows setupCoreModules), and spawning a
	// second copy of every provider here in order to have them earlier would
	// double the plugin processes. nil in teleport mode, where neither is used.
	SSHProviders func() map[string]coremodssh.SshProviderHost
	SSHHostCAs   func() []push.NamedHostKeyAuthority

	// Audit is single audit-writer for keeper-side modules (vault/bootstrap/cert
	// write audit events; soul/choir do not). nil allowed (modules skip write and
	// continue), but prod wire-up from main should provide real
	// keeper/internal/auditpg or auditmulti.
	Audit AuditWriter
}

// AuditWriter is common type for audit-writing modules (vault/bootstrap/cert);
// matches shared/audit.Writer.
type AuditWriter interface {
	vault.AuditWriter
	bootstrap.AuditWriter
	cert.AuditWriter
	coremodssh.AuditWriter
}

// Default builds Registry with keeper-side core modules: unconditionally
// core.soul / core.vault, plus core.choir if Deps.ChoirStore present. Caller
// provides real deps (PG-pool via soul.NewPGStore, vault-client from
// keeper/internal/vault, choir.NewPGStore).
func Default(d Deps) *Registry {
	// core.soul.registered with onboarding barrier (ADR-061): presence-checker +
	// await_timeout ceiling provider optional. nil presence →
	// step with await_online: true fails (test/dev builds without Redis).
	soulMod := soul.New(d.SoulStore).WithPresence(d.SoulPresence, d.MaxAwaitTimeout)
	mods := map[string]module.SoulModule{
		soul.Name:  soulMod,
		vault.Name: vault.New(d.Vault, d.Audit),
	}
	// `core.state.*` ([ADR-0083] §4) needs BOTH: the Vault client, because
	// its job is to put a declared secret in Vault and hand back a reference, and
	// the state store, because since [ADR-0084] the same step also WRITES the
	// captured field. Missing either one and a step with that address fails with
	// "unknown keeper-side module" — the same "not configured" signal as choir
	// and cert.
	if d.Vault != nil && d.StateStore != nil {
		mods[state.Name] = state.New(d.Vault, d.Audit, d.VaultMount).WithStore(d.StateStore)
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
		m := cert.New(d.Vault, d.CertStore, d.Audit, d.KID)
		m.Signer, m.VaultWriter, m.Policy = d.CertSigner, d.CertVaultWriter, d.CertPolicy
		m.CSRGen, m.PKIMount, m.KVMount = d.CertCSRGen, d.CertPKIMount, d.CertKVMount
		mods[cert.Name] = m
	}
	// `core.bootstrap` is registered when the transactional issuer is available.
	// Since NIM-834 the module has only the `issued` state, so the issuer is the
	// whole dependency set; nil means a build without Keeper Postgres and a step
	// with that address fails with "unknown keeper-side module" (like any
	// unconfigured one). Symmetric to conditional `core.choir` registration.
	if d.BootstrapIssuer != nil {
		mods[bootstrap.Name] = &bootstrap.Module{
			Issuer: d.BootstrapIssuer,
			Audit:  d.Audit,
		}
	}
	// `core.ssh.run` (NIM-849) is registered when there is a dialer, which is the
	// whole of its transport. It is the engine's only way to execute anything on
	// a host that has no agent yet — `core.exec.run` and `core.file.present` are
	// Soul-side, and a bare VM has no Soul — so a build without it mints tokens
	// nobody can redeem and blocks at the onboarding barrier. The direct
	// transport's providers and host CAs arrive through accessors read at Apply,
	// not here: they are spawned and loaded after this runs.
	if d.SSHDial != nil {
		mods[coremodssh.Name] = &coremodssh.Module{
			Transport: d.SSHTransport,
			Providers: d.SSHProviders,
			HostCAs:   d.SSHHostCAs,
			Dial:      d.SSHDial,
			Audit:     d.Audit,
		}
	}
	return NewRegistry(mods)
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
