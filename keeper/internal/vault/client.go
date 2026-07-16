// Package vault — wraps `github.com/hashicorp/vault/api` for reading
// Vault KV (v1/v2) secrets on the keeper side.
//
// Scope (ADR-014, ADR-017): reads `secret/keeper/jwt-signing-key` for
// the JWT issuer in bootstrap logic, supports `core.vault.kv-read` on
// the keeper side.
//
// Authentication (cfg.Auth.Method):
//   - `token` (default, dev) — static token from cfg.Token;
//   - `approle` (prod, ADR-014) — `auth/approle/login` with role_id + secret_id;
//     returns a renewable client token.
//
// Token auto-renew — see renewer.go (TokenRenewer on the vault LifetimeWatcher);
// a renewable token (approle) is renewed in the background, non-renewable (root/dev)
// degrades without failing.
//
// Post-MVP:
//   - TLS CA (cfg.Addr=https://..., custom CA bundle).
//   - Namespace (Vault Enterprise).
package vault

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	vaultapi "github.com/hashicorp/vault/api"

	"github.com/souls-guild/soul-stack/shared/config"
)

// defaultKVMount — fallback for `cfg.KVMount` when it is empty in keeper.yml.
// This is the default mount for Vault dev-mode (`vault secrets enable -path=secret kv-v2`
// is activated automatically).
const defaultKVMount = "secret"

// ErrVaultKVNotFound — the KV path does not exist or was deleted (soft delete).
// A dedicated sentinel lets calling code distinguish "no such key"
// from transport errors.
var ErrVaultKVNotFound = errors.New("vault: KV path not found")

// kvVersionUnset — `kvVersionOverride` is not set (auto-detect via probe).
const kvVersionUnset = 0

// Client — a thin wrapper over *vaultapi.Client with a fixed KV mount.
//
// Safe for concurrent use: vaultapi.Client holds an internal http.Client
// with a connection pool. The KV mount version cache
// (`kvVersions`) is protected by `kvVersionsMu` — `resolveKVVersion` is concurrent.
type Client struct {
	c       *vaultapi.Client
	kvMount string
	metrics *VaultMetrics

	// kvVersionOverride — forces the KV version from cfg.Vault.KVVersion (1/2);
	// kvVersionUnset → auto-detect via probe (sys/internal/ui/mounts).
	kvVersionOverride int

	// kvVersions — per-mount cache of the resolved KV version (1/2). The probe is lazy
	// (on the first ReadKV/WriteKV for a mount), the result is cached. A map, not a single
	// field, so it can extend to multi-mount scenarios without rewriting.
	kvVersionsMu sync.RWMutex
	kvVersions   map[string]int
}

// SetMetrics wires keeper_vault_*-metrics into the client (ADR-024). The nil-safe
// no-op methods on [VaultMetrics] make it fine to not set metrics at all (the bootstrap path
// keeper init brings up Client before obs.Registry exists). Called once in
// keeper run after [RegisterVaultMetrics]; there are no concurrent writers — by that
// point the client has not yet been handed to render/scenario/CEL.
func (c *Client) SetMetrics(m *VaultMetrics) { c.metrics = m }

// approleLoginPath — the Vault AppRole authentication endpoint.
const approleLoginPath = "auth/approle/login"

// NewClient creates a Vault client from cfg, authenticates with the chosen method,
// and verifies connectivity via Ping.
//
// cfg.Addr — required (e.g. "http://localhost:8200" for dev,
// "https://vault.example.com:8200" for prod).
// cfg.KVMount — path to the KV v2 secrets engine, default "secret".
//
// Authentication via cfg.Auth.ResolvedAuthMethod():
//   - token: cfg.Token is set directly (for dev = "root").
//   - approle: `auth/approle/login` with role_id + secret_id (from file/env);
//     the returned renewable client token is set on the client; further renewal
//     is handled by TokenRenewer (renewer.go).
//
// Post-MVP: TLS CA / namespace move into cfg.
func NewClient(ctx context.Context, cfg config.KeeperVault) (*Client, error) {
	if cfg.Addr == "" {
		return nil, errors.New("vault: addr is empty")
	}

	apiCfg := vaultapi.DefaultConfig()
	apiCfg.Address = cfg.Addr
	if err := apiCfg.Error; err != nil {
		return nil, fmt.Errorf("vault: api default config: %w", err)
	}

	api, err := vaultapi.NewClient(apiCfg)
	if err != nil {
		return nil, fmt.Errorf("vault: NewClient: %w", err)
	}

	switch cfg.Auth.ResolvedAuthMethod() {
	case config.AuthMethodToken:
		if cfg.Token == "" {
			return nil, errors.New("vault: token is empty (auth.method=token)")
		}
		api.SetToken(cfg.Token)

	case config.AuthMethodAppRole:
		if err := loginAppRole(ctx, api, cfg.Auth); err != nil {
			return nil, err
		}

	default:
		// The config schema phase constrains method to an enum; reaching here
		// is only possible if the config schema and this switch drift out of sync.
		return nil, fmt.Errorf("vault: unsupported auth method %q", cfg.Auth.Method)
	}

	// A trailing slash in mount → silent wrong-path when building the URL
	// (`secret//data/...`). Normalize before saving.
	mount := strings.TrimSuffix(cfg.KVMount, "/")
	if mount == "" {
		mount = defaultKVMount
	}

	// kv_version override (escape hatch): empty → kvVersionUnset (probe).
	// An invalid value is rejected by the schema phase; here it's a fail-fast for
	// callers outside the config-load path (tests).
	override := kvVersionUnset
	switch cfg.KVVersion {
	case "":
		// auto-detect
	case "1":
		override = 1
	case "2":
		override = 2
	default:
		return nil, fmt.Errorf("vault: invalid kv_version %q (want \"1\" or \"2\")", cfg.KVVersion)
	}

	cl := &Client{
		c:                 api,
		kvMount:           mount,
		kvVersionOverride: override,
		kvVersions:        make(map[string]int),
	}
	// The KV version probe is strictly lazy (first ReadKV/WriteKV). Only
	// Ping here (sys/health, no KV permissions needed) — the bootstrap path (keeper init) brings up
	// the Client before KV access is granted; probing in the constructor would break that.
	if err := cl.Ping(ctx); err != nil {
		return nil, fmt.Errorf("vault: ping: %w", err)
	}
	return cl, nil
}

// resolveKVVersion determines the KV secrets engine version for a mount (1 or 2).
//
// Order:
//  1. override (cfg.Vault.kv_version) — wins unconditionally, WITHOUT a round trip;
//  2. per-mount cache — version already resolved;
//  3. probe `sys/internal/ui/mounts/<mount>` → `data.type == "kv"` +
//     `data.options.version` ("1"/"2").
//
// Fail-closed: if the probe didn't yield an unambiguous version (a "kv" mount without
// options.version / an unexpected value / type != "kv" / permission-denied
// without an override) — an explicit error with a hint about `vault.kv_version`, NOT a
// silent default to v2. The former mechanism of "guessing from the error class of
// KVv2.Get" was rejected (a plain v1 secret is indistinguishable from v2-missing).
//
// The probe is lazy — called from ReadKV/WriteKV, not from NewClient (see there).
func (c *Client) resolveKVVersion(ctx context.Context, mount string) (int, error) {
	if c.kvVersionOverride != kvVersionUnset {
		return c.kvVersionOverride, nil
	}

	c.kvVersionsMu.RLock()
	v, ok := c.kvVersions[mount]
	c.kvVersionsMu.RUnlock()
	if ok {
		return v, nil
	}

	// Re-check under the write lock before probing: on a cold start several goroutines
	// see the RLock-cache miss simultaneously; without this they would each make duplicate
	// probe round trips. Double-checked locking eliminates redundant probes (single-
	// flight isn't needed — the probe is idempotent, only round-trip savings matter).
	c.kvVersionsMu.Lock()
	defer c.kvVersionsMu.Unlock()
	if v, ok := c.kvVersions[mount]; ok {
		return v, nil
	}

	version, err := c.probeKVVersion(ctx, mount)
	if err != nil {
		return 0, err
	}
	c.kvVersions[mount] = version
	return version, nil
}

// probeKVVersion reads `sys/internal/ui/mounts/<mount>` and extracts the
// KV engine version from `data.options.version`. Does not cache (that's done by
// resolveKVVersion). Errors are fail-closed — see resolveKVVersion.
func (c *Client) probeKVVersion(ctx context.Context, mount string) (int, error) {
	const hint = "specify vault.kv_version: 1|2 to skip auto-detect"

	secret, err := c.c.Logical().ReadWithContext(ctx, "sys/internal/ui/mounts/"+mount)
	if err != nil {
		// 403/permission-denied (ACL blocked the probe endpoint) or any other
		// transport failure — fail-closed: could not determine the version.
		// The transport/ACL error for the probe endpoint (sys/internal/ui/mounts) is safe
		// to include in the text: there's no KV secret here — include `%v` for diagnostics (ACL/network).
		return 0, fmt.Errorf("vault: cannot determine KV version of mount %q via sys/internal/ui/mounts (%v); %s", mount, err, hint)
	}
	if secret == nil || secret.Data == nil {
		return 0, fmt.Errorf("vault: cannot determine KV version of mount %q: empty sys/internal/ui/mounts response; %s", mount, hint)
	}

	if t, _ := secret.Data["type"].(string); t != "kv" {
		return 0, fmt.Errorf("vault: mount %q is not a KV engine (type=%q); %s", mount, t, hint)
	}

	opts, ok := secret.Data["options"].(map[string]any)
	if !ok || opts == nil {
		return 0, fmt.Errorf("vault: mount %q has no KV options.version; %s", mount, hint)
	}
	ver, _ := opts["version"].(string)
	switch ver {
	case "1":
		return 1, nil
	case "2":
		return 2, nil
	default:
		return 0, fmt.Errorf("vault: mount %q has unexpected KV version %q; %s", mount, ver, hint)
	}
}

// requireKVv2 — guard for metadata/list operations (ListKV / ReadKVMetadata):
// KV v1 has no `<mount>/metadata/` path, so these operations are meaningless
// on a v1 mount. Returns a clear error instead of a silently broken round trip.
// op — a human-readable operation name for the error text ("list"/"metadata read").
func (c *Client) requireKVv2(ctx context.Context, op string) error {
	version, err := c.resolveKVVersion(ctx, c.kvMount)
	if err != nil {
		return err
	}
	if version != 2 {
		return fmt.Errorf("vault: %s requires KV v2, but mount %q is v%d", op, c.kvMount, version)
	}
	return nil
}

// loginAppRole performs `auth/approle/login` and sets the returned
// client token on api. role_id comes from the config (not a secret), secret_id
// comes from file/env (read via loadSecretID).
//
// Security: neither role_id, nor secret_id, nor the returned token ever end up
// in error text — login errors are wrapped generically. vaultapi itself may attach
// the response body (without credentials) to an HTTP login error — that's
// diagnostics from the Vault server itself, not our secret.
func loginAppRole(ctx context.Context, api *vaultapi.Client, auth config.KeeperVaultAuth) error {
	if auth.RoleID == "" {
		// Duplicates schema validation, but NewClient is also called outside
		// the config-load path (tests, future callers) — fail-fast here.
		return errors.New("vault: approle login requires role_id")
	}
	secretID, err := loadSecretID(auth)
	if err != nil {
		return err
	}

	secret, err := api.Logical().WriteWithContext(ctx, approleLoginPath, map[string]any{
		"role_id":   auth.RoleID,
		"secret_id": secretID,
	})
	if err != nil {
		return fmt.Errorf("vault: approle login failed: %w", err)
	}
	if secret == nil || secret.Auth == nil || secret.Auth.ClientToken == "" {
		return errors.New("vault: approle login returned no client token")
	}
	api.SetToken(secret.Auth.ClientToken)
	return nil
}

// loadSecretID reads secret_id from the configured source:
// secret_id_file (a mode-restricted file) or secret_id_env (an environment variable).
// Trailing whitespace/newline is stripped. There is exactly one source — guaranteed by the
// schema phase; here we re-check for callers outside the config-load path.
//
// Security: the secret_id value never ends up in error messages —
// only the file path / env variable name (these are not secrets).
func loadSecretID(auth config.KeeperVaultAuth) (string, error) {
	switch {
	case auth.SecretIDFile != "":
		raw, err := os.ReadFile(auth.SecretIDFile)
		if err != nil {
			return "", fmt.Errorf("vault: read secret_id_file %q: %w", auth.SecretIDFile, err)
		}
		v := strings.TrimSpace(string(raw))
		if v == "" {
			return "", fmt.Errorf("vault: secret_id_file %q is empty", auth.SecretIDFile)
		}
		return v, nil

	case auth.SecretIDEnv != "":
		v := strings.TrimSpace(os.Getenv(auth.SecretIDEnv))
		if v == "" {
			return "", fmt.Errorf("vault: secret_id_env %q is empty or unset", auth.SecretIDEnv)
		}
		return v, nil

	default:
		return "", errors.New("vault: approle login requires secret_id_file or secret_id_env")
	}
}

// relativeKVPath normalizes the input KV path to its relative form (without the mount
// prefix) and checks it for safety. Common preamble for all KV methods
// (ReadKV / WriteKV / ListKV / ReadKVMetadata).
//
// Steps:
//   - strip the leading slash (otherwise `/secret/foo` wouldn't reduce to `foo`);
//   - strip the mount prefix, if the path is given in logical form;
//   - strip the leading slash again;
//   - reject an empty result (no point in a round trip without a path).
//
// SECURITY (defense-in-depth): fail-closed on a `..` segment. A literal
// `../` collapses via the Go/HTTP client when building the URL and could take
// the request outside the intended mount/scope. Current callers don't
// construct such paths (the orphan scan only reads names from ListKV), but the guard protects
// future ones. We check via the result of path.Clean: if Clean collapsed `..`
// up the tree (`..` remains at the start) — reject; plus an explicit scan of segments
// for the `a/../b` case inside.
func (c *Client) relativeKVPath(input string) (string, error) {
	input = strings.TrimPrefix(input, "/")
	rel := strings.TrimPrefix(input, c.kvMount+"/")
	rel = strings.TrimPrefix(rel, "/")
	if rel == "" {
		return "", fmt.Errorf("vault: empty KV path after stripping mount %q", c.kvMount)
	}
	for _, seg := range strings.Split(rel, "/") {
		if seg == ".." {
			return "", fmt.Errorf("vault: path contains '..' segment: %q", input)
		}
	}
	// Additional safety net: path.Clean collapses escaping sequences;
	// a leading `..` after Clean means it escaped the scope.
	if cleaned := path.Clean(rel); strings.HasPrefix(cleaned, "..") {
		return "", fmt.Errorf("vault: path contains '..' segment: %q", input)
	}
	return rel, nil
}

// ReadKV reads the secret value at path. The return contract is a single flat
// payload (`map = secret fields`), regardless of the KV mount version: KVv2.Get
// already returns the unwrapped `data.data`, KVv1.Get — `secret.Data` flat.
//
// `path` is accepted in two forms:
//   - logical: "secret/keeper/jwt-signing-key" (with the mount prefix);
//   - relative: "keeper/jwt-signing-key" (mount is substituted automatically).
//
// The mount is resolved via client.kvMount; the version (v1/v2) — via
// resolveKVVersion (override or probe). Returns ErrVaultKVNotFound
// if the path is missing or deleted (Vault returns an empty Secret) — for BOTH
// versions.
func (c *Client) ReadKV(ctx context.Context, path string) (_ map[string]any, err error) {
	// Input-path normalization (strip mount/leading slash) + fail-closed
	// guard on the `..` segment — see relativeKVPath.
	rel, err := c.relativeKVPath(path)
	if err != nil {
		return nil, err
	}

	// keeper_vault_*-metrics (ADR-024): round-trip latency + error counter
	// (notfound/error). The timer starts here to cover the network call and
	// result parsing; the label is mount-only (not the path-with-secret). nil
	// metrics → no-op. The empty-path guard above is a structural caller failure,
	// not a Vault round trip, so it's not measured.
	start := time.Now()
	defer func() { c.metrics.ObserveRead(c.kvMount, time.Since(start), err) }()

	version, err := c.resolveKVVersion(ctx, c.kvMount)
	if err != nil {
		return nil, err
	}

	var secret *vaultapi.KVSecret
	if version == 2 {
		secret, err = c.c.KVv2(c.kvMount).Get(ctx, rel)
	} else {
		secret, err = c.c.KVv1(c.kvMount).Get(ctx, rel)
	}
	if err != nil {
		// vaultapi.KVv{1,2}.Get returns ErrSecretNotFound for missing
		// and tombstoned paths; map it to our sentinel (both versions).
		if errors.Is(err, vaultapi.ErrSecretNotFound) {
			err = fmt.Errorf("%w: %s", ErrVaultKVNotFound, path)
			return nil, err
		}
		err = fmt.Errorf("vault: read %q: %w", path, err)
		return nil, err
	}
	if secret == nil || secret.Data == nil {
		err = fmt.Errorf("%w: %s", ErrVaultKVNotFound, path)
		return nil, err
	}
	return secret.Data, nil
}

// WriteKV writes a secret to KV at path (KV v2 creates a new version). data is
// a flat set of secret fields (`{"signing_key": "<PEM>"}`).
//
// `path` is accepted in the same forms as [Client.ReadKV] (logical with the mount
// prefix, or relative); mount/version are resolved via client.kvMount +
// resolveKVVersion. Symmetric with reading: the leading slash is stripped, the mount prefix
// is stripped.
//
// Scope (ADR-026(h), R3-S7): writes the private ed25519 Sigil signing key when
// introducing a new trust-anchor key (`secret/keeper/sigil-keys/<key_id>`). Before R3
// keeper only read Vault (jwt-/sigil-signing-key, core.vault.kv-read); writing
// is introduced here as the minimal mirror image of the read surface.
//
// SECURITY: secret field values (including the private key) never end up in error
// text — only the path (the secret's name, not its contents).
func (c *Client) WriteKV(ctx context.Context, path string, data map[string]any) (err error) {
	rel, err := c.relativeKVPath(path)
	if err != nil {
		return err
	}
	if len(data) == 0 {
		return fmt.Errorf("vault: empty data for KV write %q", path)
	}

	// keeper_vault_*-metrics: write round-trip latency + error counter.
	// The label is mount-only (not the path-with-secret), like ObserveRead. nil metrics
	// → no-op. The empty-path/data guard above is a structural caller failure, not a
	// Vault round trip, so it's not measured.
	start := time.Now()
	defer func() { c.metrics.ObserveWrite(c.kvMount, time.Since(start), err) }()

	version, err := c.resolveKVVersion(ctx, c.kvMount)
	if err != nil {
		return err
	}

	if version == 2 {
		_, err = c.c.KVv2(c.kvMount).Put(ctx, rel, data)
	} else {
		err = c.c.KVv1(c.kvMount).Put(ctx, rel, data)
	}
	if err != nil {
		err = fmt.Errorf("vault: write %q: %w", path, err)
		return err
	}
	return nil
}

// ListKV lists secret names under prefix at the KV v2 metadata path
// (`<mount>/metadata/<rel>`). Returns only the last segment of each name
// (key_id for `keeper/sigil-keys/<key_id>`), NOT the full path.
//
// `prefix` is accepted in the same two forms as [Client.ReadKV] (logical with the
// mount prefix, or relative); mount is resolved via client.kvMount.
//
// An empty/missing prefix (Vault returns a nil Secret for a nonexistent
// subfolder) → (nil, nil): this is the valid "no secrets under prefix", NOT an error.
// This lets orphan-reconcile distinguish "no orphans" from a transport failure.
//
// Scope (ADR-026(h), reap_orphan_vault_keys): lists
// `secret/keeper/sigil-keys/` to find orphaned signing private keys.
// Requires a Vault policy with `list` on `secret/metadata/keeper/sigil-keys/*`.
// Secret values are NOT read — only the names from the metadata path.
func (c *Client) ListKV(ctx context.Context, prefix string) (_ []string, err error) {
	rel, err := c.relativeKVPath(prefix)
	if err != nil {
		return nil, err
	}

	// keeper_vault_list_*-metrics: LIST round-trip latency + error
	// counter. The label is mount-only (not the path-with-secret-names), like
	// ObserveRead. A nil secret (empty/missing subfolder) is NOT an error
	// (kind is not incremented). The empty-prefix guard above is a structural
	// caller failure, not a Vault round trip, so it's not measured.
	start := time.Now()
	defer func() { c.metrics.ObserveList(c.kvMount, time.Since(start), err) }()

	if err = c.requireKVv2(ctx, "list"); err != nil {
		return nil, err
	}

	secret, err := c.c.Logical().ListWithContext(ctx, c.kvMount+"/metadata/"+rel)
	if err != nil {
		err = fmt.Errorf("vault: list %q: %w", prefix, err)
		return nil, err
	}
	// A nonexistent subfolder → nil Secret or empty Data. No orphans.
	if secret == nil || secret.Data == nil {
		return nil, nil
	}
	rawKeys, ok := secret.Data["keys"].([]any)
	if !ok {
		// A missing `keys` key or unexpected type — an empty LIST response.
		return nil, nil
	}

	names := make([]string, 0, len(rawKeys))
	for _, rk := range rawKeys {
		name, ok := rk.(string)
		if !ok || name == "" {
			continue
		}
		// Trailing slash → subfolder. A flat `sigil-keys/` shouldn't have
		// any; filtered defensively so a subfolder is never mistaken for a key_id.
		if strings.HasSuffix(name, "/") {
			continue
		}
		names = append(names, name)
	}
	return names, nil
}

// ReadKVMetadata reads a secret's version-agnostic metadata (`created_time`) from the
// KV v2 metadata path (`<mount>/metadata/<rel>`). Needed for age-based grace
// in orphan-reconcile — without reading the secret itself.
//
// `path` is accepted in the same forms as [Client.ReadKV]; mount is resolved
// via client.kvMount. Returns [ErrVaultKVNotFound] if the metadata does not exist.
//
// SECURITY: ONLY the metadata path is read — the secret value (the private key)
// is never requested. Metrics reuse [VaultMetrics.ObserveRead]
// (metadata-read is a special case of reading).
func (c *Client) ReadKVMetadata(ctx context.Context, path string) (_ time.Time, err error) {
	rel, err := c.relativeKVPath(path)
	if err != nil {
		return time.Time{}, err
	}

	start := time.Now()
	defer func() { c.metrics.ObserveRead(c.kvMount, time.Since(start), err) }()

	if err = c.requireKVv2(ctx, "metadata read"); err != nil {
		return time.Time{}, err
	}

	secret, err := c.c.Logical().ReadWithContext(ctx, c.kvMount+"/metadata/"+rel)
	if err != nil {
		err = fmt.Errorf("vault: read metadata %q: %w", path, err)
		return time.Time{}, err
	}
	if secret == nil || secret.Data == nil {
		err = fmt.Errorf("%w: %s", ErrVaultKVNotFound, path)
		return time.Time{}, err
	}
	rawCreated, ok := secret.Data["created_time"].(string)
	if !ok || rawCreated == "" {
		err = fmt.Errorf("vault: metadata %q has no created_time", path)
		return time.Time{}, err
	}
	created, perr := time.Parse(time.RFC3339Nano, rawCreated)
	if perr != nil {
		err = fmt.Errorf("vault: parse created_time for %q: %w", path, perr)
		return time.Time{}, err
	}
	return created, nil
}

// Ping — a health check via `sys/health`. Does not require a token with KV
// permissions, so it's also suitable for bootstrap checks.
func (c *Client) Ping(ctx context.Context) error {
	_, err := c.c.Sys().HealthWithContext(ctx)
	if err != nil {
		return fmt.Errorf("vault: sys/health: %w", err)
	}
	return nil
}
