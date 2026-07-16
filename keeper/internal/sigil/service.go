package sigil

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/goccy/go-yaml"

	"github.com/souls-guild/soul-stack/keeper/internal/pluginhost"
)

// ErrPluginNotInCache signals an allow request for a plugin not in the host cache
// (slot `<cacheRoot>/<ns>-<name>/` missing or invalid binary/manifest).
// Transport maps to 404. Wraps [pluginhost.ErrSlotNotFound] — service boundary
// must not leak pluginhost sentinels to handlers.
var ErrPluginNotInCache = errors.New("sigil: plugin not found in host cache")

// SlotReader is the surface for reading plugin slots from the cache by (namespace, name).
// Implemented by [cacheSlotReader] over [pluginhost.ReadSlot] /
// [pluginhost.SlotCommitSHA] (with fixed cacheRoot); narrowing to interface
// allows unit-testing Service without a real cache directory. Variant C: ref
// does not participate in lookup (single-active slot via current-symlink).
//
// SlotCommitSHA returns the commit_sha of the ACTIVE slot (current-symlink target,
// A1-S1) — audit provenance marker written to plugin_sigils on allow
// (ADR-026(g), outside signature). Missing/corrupted current → [ErrSlotNotFound]
// (fail-closed, symmetric to ReadSlot).
type SlotReader interface {
	ReadSlot(namespace, name string) (*pluginhost.SlotContents, error)
	SlotCommitSHA(namespace, name string) (string, error)
}

// cacheSlotReader adapts [pluginhost.ReadSlot] / [pluginhost.SlotCommitSHA]
// (with fixed cacheRoot) to [SlotReader]. Production wire-up in `keeper run`
// binds cacheRoot.
type cacheSlotReader struct {
	cacheRoot string
}

func (r cacheSlotReader) ReadSlot(namespace, name string) (*pluginhost.SlotContents, error) {
	return pluginhost.ReadSlot(r.cacheRoot, namespace, name)
}

func (r cacheSlotReader) SlotCommitSHA(namespace, name string) (string, error) {
	return pluginhost.SlotCommitSHA(r.cacheRoot, namespace, name)
}

// NewCacheSlotReader constructs [SlotReader] over the Keeper-host cache
// with fixed cacheRoot (`keeper.yml` / [pluginhost.DefaultCacheRoot]).
func NewCacheSlotReader(cacheRoot string) SlotReader {
	return cacheSlotReader{cacheRoot: cacheRoot}
}

// Store is the surface for the plugin_sigils registry needed by [Service].
// Implemented by package-level CRUD (Insert / Revoke / ListActive) over pgx-pool
// via [NewPGStore]; narrowing to interface isolates Service from direct pgx-pool
// in unit tests.
type Store interface {
	Insert(ctx context.Context, s *Sigil) error
	Revoke(ctx context.Context, namespace, name, ref, revokedByAID string) error
	ListActive(ctx context.Context) ([]*Sigil, error)
}

// pgStore adapts package-level CRUD plugin_sigils to [Store].
// Holds pool (or tx) and delegates to Insert / Revoke / ListActive.
type pgStore struct {
	db ExecQueryRower
}

// NewPGStore wraps pgx-pool (any [ExecQueryRower]) into [Store].
func NewPGStore(db ExecQueryRower) Store {
	return &pgStore{db: db}
}

func (s *pgStore) Insert(ctx context.Context, rec *Sigil) error {
	return Insert(ctx, s.db, rec)
}

func (s *pgStore) Revoke(ctx context.Context, namespace, name, ref, revokedByAID string) error {
	return Revoke(ctx, s.db, namespace, name, ref, revokedByAID)
}

func (s *pgStore) ListActive(ctx context.Context) ([]*Sigil, error) {
	return ListActive(ctx, s.db)
}

// Invalidator is the surface for cluster-wide Sigil invalidation (ADR-026, S6c).
// After successful commit of Allow/Revoke, [Service] calls Invalidate so that
// EVERY Keeper node (including the mutating one) re-broadcasts the active set
// to its connected Souls — otherwise Soul on another node works with stale cache
// (revoked allow still "trusted", new allow not yet arrived). Implemented in
// `keeper run` via adapter over [keeperredis.PublishSigilInvalidate]; in
// single-Keeper/dev mode (no Redis) not wired — allows reach on next Soul reconnect.
//
// Invalidate is best-effort: does not return publication errors (mutation already
// committed to DB); implementation logs and swallows.
type Invalidator interface {
	Invalidate(ctx context.Context)
}

// ServiceDeps are the dependencies for [Service]. All fields immutable after construction.
type ServiceDeps struct {
	Signer *Signer
	Store  Store
	Slots  SlotReader
	Logger *slog.Logger
}

// Service implements Sigil business logic (allow / revoke / list) over S3 (Signer +
// Store) and host cache (SlotReader). Single source of truth for transport
// facades (OpenAPI — S4a, MCP — S4b): handler decodes input → service-call →
// maps sentinel errors.
//
// Variant C (ADR-026, operator-asserted ref): Allow reads CURRENT binary+
// manifest from the single slot `<cacheRoot>/<ns>-<name>/` (key without ref);
// `ref` arrives as operator-provided label and does not participate in slot lookup.
// Integrity authority: sha256+signature, not git-verified ref.
//
// Concurrency-safe: deps immutable, no state held (ed25519.Sign does not mutate key;
// atomicity at Store/PG level).
type Service struct {
	// signer is atomic.Pointer because R3 multi-anchor rotation (S6) replaces
	// the signing Signer at runtime (new primary after Introduce/SetPrimary)
	// concurrently with Allow, which reads it in [Service.Allow]. Replacement is
	// whole-pointer (Signer immutable after construction), lock-free read in
	// hot-path Allow. Always non-nil after [NewService] (constructor checks).
	signer atomic.Pointer[Signer]
	store  Store
	slots  SlotReader
	logger *slog.Logger

	// inv is optional cluster-wide invalidator (S6c). Late-binding via
	// [Service.SetInvalidator]: Redis client in `keeper run` comes up AFTER
	// NewService, so injection is deferred (pattern rbac.Service.inv).
	// atomic.Pointer allows concurrent write by setter vs. read from mutations
	// without separate mutex.
	inv atomic.Pointer[Invalidator]
}

// NewService constructs the service. Signer / Store / Slots required.
func NewService(d ServiceDeps) (*Service, error) {
	if d.Signer == nil {
		return nil, errors.New("sigil: ServiceDeps.Signer is nil")
	}
	if d.Store == nil {
		return nil, errors.New("sigil: ServiceDeps.Store is nil")
	}
	if d.Slots == nil {
		return nil, errors.New("sigil: ServiceDeps.Slots is nil")
	}
	logger := d.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	svc := &Service{store: d.Store, slots: d.Slots, logger: logger}
	svc.signer.Store(d.Signer)
	return svc, nil
}

// SetSigner atomically replaces the signing Signer (R3 multi-anchor "keeper
// Signer hot-reload", S6). Called by daemon-watcher on cluster signal
// `sigil:anchors-changed` after signing key rotation (Introduce / SetPrimary /
// Retire): new Signer carries fresh primary (signature for new allows) and full
// set of active anchors. nil input ignored (defensive: replacing with nil would strip
// Allow's signature) — each build-Signer path in daemon returns non-nil or error.
// Thread-safe for concurrent [Service.Allow].
func (s *Service) SetSigner(signer *Signer) {
	if signer == nil {
		return
	}
	s.signer.Store(signer)
}

// SetInvalidator late-binds cluster-wide invalidator (S6c).
// Called from `keeper run` after Redis-client startup. nil removes
// invalidator (revert to plain connect-time broadcast). Idempotent,
// thread-safe. Pattern identical to [rbac.Service.SetInvalidator].
func (s *Service) SetInvalidator(inv Invalidator) {
	if inv == nil {
		s.inv.Store(nil)
		return
	}
	s.inv.Store(&inv)
}

// invalidate sends cluster-wide invalidate signal after successful commit of
// allow/revoke mutation (S6c). No-op if invalidator not wired (single-Keeper/dev).
// Best-effort: Invalidate implementation itself logs and swallows publish errors —
// mutation already committed; signal loss compensated by connect-time broadcast.
func (s *Service) invalidate(ctx context.Context) {
	if p := s.inv.Load(); p != nil {
		(*p).Invalidate(ctx)
	}
}

// AllowInput are the parameters for [Service.Allow].
type AllowInput struct {
	Namespace string
	Name      string
	Ref       string
	CallerAID string
}

// Allow permits plugin (namespace, name) under operator-asserted label ref
// in the allow-list plugin_sigils.
//
// Steps (variant C):
//  1. reads current binary+manifest from slot `<cacheRoot>/<ns>-<name>/`
//     (ref does not participate in lookup); no slot → [ErrPluginNotInCache];
//  2. reads commit_sha of ACTIVE slot (current-symlink target) — audit provenance
//     mark (ADR-026(g)); missing/corrupted current → [ErrPluginNotInCache]
//     (fail-closed: allow provenance must be fixed);
//  3. signs Sigil block via Signer over (ns, name, ref, binary_sha256,
//     manifest_bytes) — commit_sha not in block, signature unchanged;
//  4. inserts record into registry (commit_sha as separate audit column); existing
//     active record on (ns, name, ref) → [ErrSigilAlreadyActive].
//
// Returns sha256 of allowed binary (hex) — handler places in 201 response.
func (s *Service) Allow(ctx context.Context, in AllowInput) (string, error) {
	slot, err := s.slots.ReadSlot(in.Namespace, in.Name)
	if err != nil {
		if errors.Is(err, pluginhost.ErrSlotNotFound) {
			return "", fmt.Errorf("%w: %s-%s", ErrPluginNotInCache, in.Namespace, in.Name)
		}
		return "", fmt.Errorf("sigil: read plugin slot: %w", err)
	}

	// commit_sha of ACTIVE slot (current-symlink target). Source is the same
	// current that ReadSlot follows when reading binary/manifest, so commit_sha
	// aligns exactly with signed binary. fail-closed: legacy slot without current
	// yields ErrSlotNotFound → reject without fixed provenance (same contract as slot absence).
	commitSHA, err := s.slots.SlotCommitSHA(in.Namespace, in.Name)
	if err != nil {
		if errors.Is(err, pluginhost.ErrSlotNotFound) {
			return "", fmt.Errorf("%w: %s-%s (no resolved commit_sha)", ErrPluginNotInCache, in.Namespace, in.Name)
		}
		return "", fmt.Errorf("sigil: read plugin slot commit_sha: %w", err)
	}

	signature, err := s.signer.Load().Sign(in.Namespace, in.Name, in.Ref, slot.BinarySHA256, slot.ManifestBytes)
	if err != nil {
		return "", fmt.Errorf("sigil: sign: %w", err)
	}

	manifestJSON, err := manifestYAMLToJSON(slot.ManifestBytes)
	if err != nil {
		return "", fmt.Errorf("sigil: convert manifest to JSON: %w", err)
	}

	rec := &Sigil{
		Namespace: in.Namespace,
		Name:      in.Name,
		Ref:       in.Ref,
		SHA256:    slot.BinarySHA256,
		CommitSHA: commitSHA,
		Signature: signature,
		// ManifestRaw — SAME bytes that went into Sign above (single ReadSlot),
		// byte-exact canon for S6-verify/broadcast. Manifest is derived JSONB
		// projection for query/audit. Sources must not diverge: invariant
		// "signed exactly these bytes" will decay.
		ManifestRaw:  slot.ManifestBytes,
		Manifest:     manifestJSON,
		AllowedByAID: in.CallerAID,
	}
	if err := s.store.Insert(ctx, rec); err != nil {
		return "", err
	}
	// Cluster-wide re-broadcast of active set to all connected Souls (S6c):
	// new allow must arrive near-instant, not waiting for reconnect.
	s.invalidate(ctx)
	return slot.BinarySHA256, nil
}

// Revoke revokes active allow (namespace, name, ref). No active record →
// [ErrSigilNotFound].
func (s *Service) Revoke(ctx context.Context, namespace, name, ref, callerAID string) error {
	if err := s.store.Revoke(ctx, namespace, name, ref, callerAID); err != nil {
		return err
	}
	// Cluster-wide re-broadcast of active set (S6c): revoked allow disappears
	// from fresh set → fail-closed on Soul side. Cache-drop semantics —
	// see constraint in [eventStreamHandler.rebroadcastSigils] / connect-time replace.
	s.invalidate(ctx)
	return nil
}

// SigilView is a projection of active record for list delivery. WITHOUT signature and
// manifest: signature is raw crypto material (not for API), manifest is large
// JSONB query/audit layer (not allow-list feed). Symmetric to rbac.RoleView.
type SigilView struct {
	Namespace    string
	Name         string
	Ref          string
	SHA256       string
	AllowedByAID string
	AllowedAt    time.Time
	RevokedAt    *time.Time
}

// List returns feed of active allows (newest first) without signature/manifest.
func (s *Service) List(ctx context.Context) ([]SigilView, error) {
	recs, err := s.store.ListActive(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]SigilView, 0, len(recs))
	for _, r := range recs {
		out = append(out, SigilView{
			Namespace:    r.Namespace,
			Name:         r.Name,
			Ref:          r.Ref,
			SHA256:       r.SHA256,
			AllowedByAID: r.AllowedByAID,
			AllowedAt:    r.AllowedAt,
			RevokedAt:    r.RevokedAt,
		})
	}
	return out, nil
}

// manifestYAMLToJSON converts raw manifest.yaml bytes to JSON for JSONB
// plugin_sigils.manifest column (query/audit layer, NOT canon for verify —
// canon held on raw bytes via NormalizeManifestBytes, S3↔S6).
// Uses same goccy/go-yaml as shared/plugin parser.
func manifestYAMLToJSON(yamlBytes []byte) ([]byte, error) {
	var v any
	if err := yaml.Unmarshal(yamlBytes, &v); err != nil {
		return nil, fmt.Errorf("yaml unmarshal: %w", err)
	}
	out, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("json marshal: %w", err)
	}
	return out, nil
}
