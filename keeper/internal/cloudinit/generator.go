// Package cloudinit renders cloud-init userdata for VMs created by
// `core.cloud.provisioned` (ADR-017(h) amendment 2026-05-27, B-flat).
//
// Userdata carries ONLY soul bootstrap: installing the `soul` binary via
// pinned-CA HTTPS curl, the `soul.yml` config with `keeper.endpoints`
// (host:port LB), the embedded Keeper CA PEM, and the `soul.service`
// systemd unit. The per-VM bootstrap token is NOT baked into userdata:
// cloud-provider APIs store userdata in plaintext metadata accessible to
// VM processes (security floor). The per-VM token is issued in
// `applyCreated` after Create and put into the task's register output;
// delivery to the VM is a separate scenario step (typically `keeper.push`
// via an SSH provider).
//
// The Keeper CA is resolved from Vault via `tls_ca_ref` (calls `ReadKV` for
// the `ca` field). The CA is public material, but a single source of truth
// in Vault is needed for rotation without touching keeper.yml.
//
// The install blueprint itself (write_files + runcmd, paths/perms) lives in
// shared [keeper/internal/soulinstall] (ADR-063 amendment 2026-06-30): this
// package is just the config resolver (Vault) plus a thin wrapper over
// `soulinstall.RenderCloudInitYAML`. The external contract
// (Config/Resolver/GenerateUserdata) is preserved.
package cloudinit

import (
	"context"
	"errors"
	"fmt"

	"github.com/souls-guild/soul-stack/keeper/internal/soulinstall"
	keepervault "github.com/souls-guild/soul-stack/keeper/internal/vault"
	"github.com/souls-guild/soul-stack/shared/config"
)

// SoulBinaryCA values select which trust store curl uses when downloading
// the soul binary. Re-exported from soulinstall (shared vocabulary for both
// renderers); kept as this package's public API for existing callers/tests.
const (
	// SoulBinaryCAKeeper pins to the Keeper CA PEM (`curl --cacert keeper-ca.pem`).
	SoulBinaryCAKeeper = soulinstall.SoulBinaryCAKeeper
	// SoulBinaryCASystem uses the OS trust bundle (`curl` without `--cacert`);
	// for artifact hosts with a public CA (e.g. Nexus behind GlobalSign).
	SoulBinaryCASystem = soulinstall.SoulBinaryCASystem
)

// Config holds resolved userdata-render parameters. Built from
// [shared/config.KeeperCloudInit] + [keepervault.Client] on every
// GenerateUserdata call (hot-reload-friendly: each apply picks up the
// current config.Store snapshot).
type Config struct {
	// BootstrapEndpoint is the Keeper LB `host:port` (Bootstrap RPC listener).
	// host goes into soul.yml keeper.endpoints[0].host, port into bootstrap_port.
	BootstrapEndpoint string

	// EventStreamPort is the TCP port of the EventStream phase (mTLS) on the
	// same host; soul.yml event_stream_port. 0 → falls back to the
	// bootstrap_endpoint port (back-compat, single-port LB). ADR-063 wall 6.
	EventStreamPort int

	// TLSCAPem is the PEM-encoded Keeper CA (contents of the `ca` field from
	// Vault KV). Baked into userdata under
	// `write_files: /etc/soul/tls/keeper-ca.pem`, then used by curl --cacert
	// when downloading the soul binary.
	TLSCAPem string

	// SoulBinaryURL is the HTTPS URL to download the `soul` binary from.
	// Plain http is rejected (security: TLS-only, regardless of SoulBinaryCA).
	SoulBinaryURL string

	// SoulBinaryCA selects the curl trust store when downloading the binary:
	// SoulBinaryCAKeeper (default/empty) → `--cacert keeper-ca.pem`;
	// SoulBinaryCASystem → the system bundle (no `--cacert`, for public CAs).
	// Weakens ONLY the artifact host's cert verification; the Bootstrap
	// channel and the binary's SHA256 verify are unaffected.
	SoulBinaryCA string

	// SoulVersion is an optional string that ends up in userdata as a
	// comment tag (for diagnostics). Binary sig-verify is deferred
	// (ADR-017(h) amendment).
	SoulVersion string
}

// Blueprint builds a [soulinstall.Blueprint] from Config — the ONLY mapping
// point from cloudinit.Config to the shared blueprint. Exported so the
// install path (core.bootstrap.delivered) builds its blueprint with the
// same mapper instead of duplicating the field list: a new Blueprint field
// can't silently go missing in install.
func (c Config) Blueprint() soulinstall.Blueprint {
	return soulinstall.Blueprint{
		BootstrapEndpoint: c.BootstrapEndpoint,
		EventStreamPort:   c.EventStreamPort,
		KeeperCAPem:       c.TLSCAPem,
		SoulBinaryURL:     c.SoulBinaryURL,
		SoulBinaryCA:      c.SoulBinaryCA,
		SoulVersion:       c.SoulVersion,
	}
}

// Validate checks that Config is filled in enough to render. Delegates to
// [soulinstall.Blueprint.Validate] (one set of checks shared by both paths).
func (c Config) Validate() error {
	return c.Blueprint().Validate()
}

// GenerateUserdata renders cloud-config YAML. Idempotent: same inputs give
// a byte-identical output. A thin wrapper over
// [soulinstall.RenderCloudInitYAML] (install blueprint lives in shared).
//
// Security: the output is checked for absence of the `bootstrap_token` /
// `vault:` substrings (security floor) — inside the soulinstall renderer.
func GenerateUserdata(cfg Config) (string, error) {
	return soulinstall.RenderCloudInitYAML(cfg.Blueprint())
}

// GenerateUserdataSelfOnboard renders cloud-config YAML for the self-onboard
// "Variant T" (ADR-017(h) amendment): userdata carries an FQDN→plain-token
// map and a `soul init` phase (token looked up by hostname). Keeper predicts
// each VM's FQDN BEFORE create and passes the tokens in here. Tokens do end
// up in userdata (test stand) — the `bootstrap_token` security guard is
// lifted inside soulinstall for this mode (see Blueprint.SelfOnboardTokens).
//
// Empty tokens → error (self-onboard with no tokens is meaningless; the
// caller must pass a non-empty map). The vault-ref floor still applies here.
func GenerateUserdataSelfOnboard(cfg Config, tokens map[string]string) (string, error) {
	if len(tokens) == 0 {
		return "", errors.New("cloud_init: self-onboard requires non-empty FQDN→token map")
	}
	bp := cfg.Blueprint()
	bp.SelfOnboardTokens = tokens
	return soulinstall.RenderCloudInitYAML(bp)
}

// Resolver resolves a [config.KeeperCloudInit] into a [Config], loading the
// CA PEM from Vault. Created as a single instance in the daemon and reused
// for every GenerateUserdata call; carries no state of its own (the vault
// client reads a KV snapshot each time — CA rotation applies without a
// restart).
type Resolver struct {
	Vault VaultReader
}

// VaultReader is the narrow slice of [keepervault.Client] needed to resolve
// the CA PEM. Mirrors keeper/internal/coremod/vault.VaultReader: simplifies
// unit tests (a fake, no HTTP needed).
type VaultReader interface {
	ReadKV(ctx context.Context, path string) (map[string]any, error)
}

// NewResolver is a wire helper. nil vc is fine in test builds; a real
// Resolve call then returns an explicit error.
func NewResolver(vc VaultReader) *Resolver {
	return &Resolver{Vault: vc}
}

// Resolve turns the keeper.yml config block into a render-ready [Config]:
// parses the TLSCARef vault-ref and reads the `ca` field from KV.
//
// Returns an error with the vault-ref masked (like the cloud resolver), so a
// failed read doesn't leak the secret's path — resolving ANY vault-ref
// (including the public CA) goes through the keeper vault client, same care
// either way.
func (r *Resolver) Resolve(ctx context.Context, cfg *config.KeeperCloudInit) (Config, error) {
	if cfg == nil {
		return Config{}, errors.New("cloud_init: keeper.yml block is missing (set keeper.cloud_init.* to use generate_userdata)")
	}
	if cfg.BootstrapEndpoint == "" {
		return Config{}, errors.New("cloud_init.bootstrap_endpoint is empty in keeper.yml")
	}
	if cfg.TLSCARef == "" {
		return Config{}, errors.New("cloud_init.tls_ca_ref is empty in keeper.yml")
	}
	if cfg.SoulBinaryURL == "" {
		return Config{}, errors.New("cloud_init.soul_binary_url is empty in keeper.yml")
	}
	if r.Vault == nil {
		return Config{}, errors.New("cloud_init: vault client is not configured (cannot resolve tls_ca_ref)")
	}

	logical, err := keepervault.ParseRef(cfg.TLSCARef)
	if err != nil {
		return Config{}, fmt.Errorf("cloud_init.tls_ca_ref: %w", err)
	}
	kv, err := r.Vault.ReadKV(ctx, logical)
	if err != nil {
		return Config{}, fmt.Errorf("cloud_init.tls_ca_ref: read vault failed")
	}
	caRaw, ok := kv["ca"]
	if !ok {
		return Config{}, fmt.Errorf("cloud_init.tls_ca_ref: vault KV at %q has no field %q", logical, "ca")
	}
	caPem, ok := caRaw.(string)
	if !ok {
		return Config{}, fmt.Errorf("cloud_init.tls_ca_ref: field %q is not a string", "ca")
	}

	return Config{
		BootstrapEndpoint: cfg.BootstrapEndpoint,
		EventStreamPort:   cfg.EventStreamPort,
		TLSCAPem:          caPem,
		SoulBinaryURL:     cfg.SoulBinaryURL,
		SoulBinaryCA:      cfg.SoulBinaryCA,
		SoulVersion:       cfg.SoulVersion,
	}, nil
}
