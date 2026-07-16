package sigil

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// File keyservice.go — operator-facing business logic for trust-anchor signing key rotation
// of Sigil (ADR-026(h), R3-S7). Single source of truth for transport facades
// (OpenAPI — handlers/sigil_key.go, MCP — mcp/sigil_key.go): handler decodes
// input → KeyService-call → maps sentinels.
//
// KeyService lives SEPARATELY from [Service] (plugin_sigils allow-list): the latter is about
// permissions for specific binaries, this one is about signing keys for those permissions. Shared
// package only because both entities are about Sigil (like keys.go vs store.go).
//
// Security invariant (ADR-026(d)): ed25519 private key is generated
// here, written to Vault KV and IMMEDIATELY forgotten — it NEVER leaks to
// Postgres (only pubkey_pem + vault_ref are written there), responses (API/MCP), and
// logs. Signing with new keys happens via cluster-wide Signer reload (R3-S6),
// which reads the primary private key from Vault via vault_ref.

// VaultWriter — narrow interface for writing to Vault KV, needed for signing key introduction.
// Implemented by [keepervault.Client.WriteKV]; narrowing to an interface allows
// unit-testing key-gen+vault-write without a real Vault (symmetric to KVReader).
type VaultWriter interface {
	WriteKV(ctx context.Context, path string, data map[string]any) error
}

// AnchorsPublisher — interface for cluster-wide invalidation of trust-anchor sets
// (ADR-026(h), R3-S6). After successful key registry mutation (Introduce / Retire /
// SetPrimary), [KeyService] publishes a signal by which EVERY Keeper node
// reloads its Signer/set and rebroadcasts SigilTrustAnchors to its Souls.
// Implemented in `keeper run` via adapter over [keeperredis.PublishAnchorsChanged];
// in single-Keeper/dev (no Redis) it is disconnected — the set is delivered on restart.
//
// Publish is best-effort: [KeyService] does NOT return publication errors (mutation
// is already persisted in DB); the implementation logs and swallows errors.
type AnchorsPublisher interface {
	Publish(ctx context.Context)
}

// KeyServiceDeps — dependencies for [KeyService]. Pool / Vault are required;
// VaultKeyMount — root path for private key secret (see [KeyService.vaultPath]).
type KeyServiceDeps struct {
	Pool          KeyStorePool
	Vault         VaultWriter
	VaultKeyMount string // root of private key secret, e.g. "secret/keeper/sigil-keys"
	Logger        *slog.Logger
	Metrics       *KeyMetrics
}

// defaultSigilKeyMount — fallback for empty [KeyServiceDeps.VaultKeyMount].
// Each key is written to `<mount>/<key_id>` (key_id is unique, secrets do not
// overwrite each other). Decoupling from jwt-/single sigil-signing-key
// (separate paths → separate rotation, decisions.md G-sigil-3).
const defaultSigilKeyMount = "secret/keeper/sigil-keys"

// KeyService — business logic for Sigil signing key rotation (introduce / retire /
// set-primary / list) over the CRUD layer in keys.go. Deps immutable; holds no
// state (atomicity is at keys.go/PG level, signing uses primary private key from
// Vault via R3-S6 reload).
type KeyService struct {
	pool      KeyStorePool
	vault     VaultWriter
	keyMount  string
	publisher AnchorsPublisher
	logger    *slog.Logger
	metrics   *KeyMetrics
}

// NewKeyService assembles the service. Pool / Vault are required.
func NewKeyService(d KeyServiceDeps) (*KeyService, error) {
	if d.Pool == nil {
		return nil, errors.New("sigil: KeyServiceDeps.Pool is nil")
	}
	if d.Vault == nil {
		return nil, errors.New("sigil: KeyServiceDeps.Vault is nil")
	}
	logger := d.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	mount := d.VaultKeyMount
	if mount == "" {
		mount = defaultSigilKeyMount
	}
	return &KeyService{
		pool:     d.Pool,
		vault:    d.Vault,
		keyMount: mount,
		logger:   logger,
		metrics:  d.Metrics,
	}, nil
}

// SetPublisher late-binds the cluster-wide anchors-publisher (R3-S6).
// Called from `keeper run` after Redis client startup (pattern
// [Service.SetInvalidator]). nil removes the publisher. Idempotent.
func (s *KeyService) SetPublisher(p AnchorsPublisher) { s.publisher = p }

// SetMetrics late-binds the gauge for active keys (R3-S7). Called
// from `keeper run` in setupMetricsRegistry (obs.Registry is raised AFTER
// setupSigil). nil-safe (pattern [vault.Client.SetMetrics]).
func (s *KeyService) SetMetrics(m *KeyMetrics) { s.metrics = m }

// IntroduceResult — outcome of [KeyService.Introduce]. Contains ONLY public data:
// key_id, public key (SPKI PEM), flags. Private key is never included
// (security invariant ADR-026(d)).
type IntroduceResult struct {
	KeyID        string
	PubkeyPEM    string
	IsPrimary    bool
	Status       string
	IntroducedAt time.Time
}

// Introduce generates a new ed25519 key pair, writes the private key to Vault KV, and introduces
// the public part into the sigil_signing_keys registry as an active trust-anchor (ADR-026(h),
// R3-S7).
//
// Steps:
//  1. ed25519.GenerateKey — new key pair;
//  2. key_id = SHA-256(SPKI-DER of public key), hex — stable id, independent
//     of PEM wrapper;
//  3. WriteKV(`<keyMount>/<key_id>`, {signing_key: <PKCS#8 PEM private key>}) —
//     private key in Vault (NOT in PG, NOT in logs, NOT in response);
//  4. keys.Introduce(key_id, pubkey_pem, vault_ref, makePrimary, callerAID).
//
// If PG insert (4) fails, the Vault write (3) remains "dangling" — this is harmless
// (private key without an anchor record is not used by anyone); the operator can retry
// Introduce (different keypair → different key_id). Reverse-cleanup of Vault secret
// is DELIBERATELY omitted: deleting the private key on error path is more dangerous
// than a "dangling" unused secret.
//
// On success, publishes anchors-changed (R3-S6 reload across cluster) and updates
// the active keys gauge.
func (s *KeyService) Introduce(ctx context.Context, makePrimary bool, callerAID string) (*IntroduceResult, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("sigil: generate ed25519 key: %w", err)
	}

	keyID, err := keyIDFromPublic(pub)
	if err != nil {
		return nil, fmt.Errorf("sigil: derive key_id: %w", err)
	}
	pubPEM, err := publicKeyToPEM(pub)
	if err != nil {
		return nil, fmt.Errorf("sigil: encode public key PEM: %w", err)
	}
	privPEM, err := privateKeyToPEM(priv)
	if err != nil {
		return nil, fmt.Errorf("sigil: encode private key PEM: %w", err)
	}

	path := s.vaultPath(keyID)
	if err := s.vault.WriteKV(ctx, path, map[string]any{vaultSigningKeyField: string(privPEM)}); err != nil {
		// SECURITY: WriteKV does not leak secret value in error text; here
		// we also log/return only key_id, not the private key.
		return nil, fmt.Errorf("sigil: write private key to vault (key_id=%s): %w", keyID, err)
	}

	var callerPtr *string
	if callerAID != "" {
		callerPtr = &callerAID
	}
	key, err := Introduce(ctx, s.pool, keyID, string(pubPEM), vaultRefForPath(path), makePrimary, callerPtr)
	if err != nil {
		return nil, err
	}

	s.afterMutation(ctx)
	s.logger.Info("sigil: signing key introduced",
		slog.String("key_id", keyID),
		slog.Bool("is_primary", key.IsPrimary),
		slog.String("by_aid", callerAID),
	)
	return &IntroduceResult{
		KeyID:        key.KeyID,
		PubkeyPEM:    key.PubkeyPEM,
		IsPrimary:    key.IsPrimary,
		Status:       key.Status,
		IntroducedAt: key.IntroducedAt,
	}, nil
}

// SetPrimary makes an active key primary (new Sigils will use it after R3-S6
// reload). On success, publishes anchors-changed. CRUD errors from keys.go
// are passed through as-is ([ErrKeyNotFound] / [ErrKeyRetired] / [ErrConcurrentPrimary]).
func (s *KeyService) SetPrimary(ctx context.Context, keyID, callerAID string) error {
	if err := SetPrimary(ctx, s.pool, keyID, callerAID); err != nil {
		return err
	}
	s.afterMutation(ctx)
	s.logger.Info("sigil: signing key set primary",
		slog.String("key_id", keyID), slog.String("by_aid", callerAID))
	return nil
}

// Retire removes a key from the trust-anchor set (Soul forgets it at next
// SigilTrustAnchors delivery). On success, publishes anchors-changed. Invariants from keys.go
// (≥1 active, not primary) are passed through as [ErrLastActiveKey] / [ErrRetirePrimary] /
// [ErrKeyNotFound].
//
// Retire is SAFE only when the set has already propagated across the cluster AND the bootstrap
// source is live (architect af7d): the former is ensured by R3-S6 (PublishAnchorsChanged →
// reloadAnchors on every node), the latter by a live TrustAnchorSource in the bootstrap
// reply (daemon orchestration). Without them, a new Soul between bootstrap and connect would
// receive a stale set and reject a signature by a retired anchor (or accept an extra one).
func (s *KeyService) Retire(ctx context.Context, keyID, callerAID string) error {
	if err := Retire(ctx, s.pool, keyID, callerAID); err != nil {
		return err
	}
	s.afterMutation(ctx)
	s.logger.Warn("sigil: signing key retired — distributed anchor set has been reduced",
		slog.String("key_id", keyID), slog.String("by_aid", callerAID))
	return nil
}

// List returns active trust-anchor keys (primary first). Read-only, no
// publication or audit.
func (s *KeyService) List(ctx context.Context) ([]*SigningKey, error) {
	return ListActiveKeys(ctx, s.pool)
}

// afterMutation — common tail of successful mutation (Introduce/SetPrimary/Retire):
// (1) cluster-wide publish anchors-changed (R3-S6 reload), (2) refresh of active
// keys gauge. Both are best-effort: publish swallows errors internally,
// gauge-refresh on registry read error only logs (metric stays at previous
// value until next mutation/restart).
func (s *KeyService) afterMutation(ctx context.Context) {
	if s.publisher != nil {
		s.publisher.Publish(ctx)
	}
	s.refreshActiveGauge(ctx)
}

// PrimeActiveGauge sets the initial value of the active keys gauge (R3-S7).
// Called once in `keeper run` after metrics registration to ensure the gauge holds
// an accurate count BEFORE the first mutation (otherwise it would remain 0 until first
// Introduce/Retire). nil-metrics → no-op.
func (s *KeyService) PrimeActiveGauge(ctx context.Context) { s.refreshActiveGauge(ctx) }

// refreshActiveGauge re-reads the active keys count and updates the gauge.
// nil-metrics → no-op. Registry read errors are not critical (metric is not authoritative).
func (s *KeyService) refreshActiveGauge(ctx context.Context) {
	if s.metrics == nil {
		return
	}
	keys, err := ListActiveKeys(ctx, s.pool)
	if err != nil {
		s.logger.Warn("sigil: refresh active-keys gauge failed", slog.Any("error", err))
		return
	}
	s.metrics.SetActive(len(keys))
}

// vaultPath constructs the logical path for private key secret by key_id:
// `<keyMount>/<key_id>`. key_id is hex SHA-256 (closed charset, no `..`/slashes),
// safe to use as a path segment.
func (s *KeyService) vaultPath(keyID string) string {
	return s.keyMount + "/" + keyID
}

// vaultRefForPath wraps the logical path in a `vault:` ref, expected by
// [LoadSigningKey] / [keepervault.ParseRef] (vault_ref format in registry).
func vaultRefForPath(path string) string { return "vault:" + path }

// keyIDFromPublic computes the stable key_id = SHA-256(SPKI-DER of public key), hex.
// Matches migration 037 convention (key_id is independent of PEM wrapper:
// whitespace/line breaks have no effect).
func keyIDFromPublic(pub ed25519.PublicKey) (string, error) {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return "", fmt.Errorf("marshal SPKI: %w", err)
	}
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:]), nil
}

// privateKeyToPEM encodes an ed25519 private key into a PKCS#8 PEM block "PRIVATE KEY".
// This form is understood by [parseEd25519Key] (PEM → PKCS#8) when later loading
// the primary private key from Vault (R3-S6 reload).
func privateKeyToPEM(priv ed25519.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, fmt.Errorf("marshal PKCS#8: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}
