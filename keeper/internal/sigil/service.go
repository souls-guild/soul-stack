package sigil

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/pluginhost"
	sharedplugin "github.com/souls-guild/soul-stack/shared/plugin"
)

// ErrPluginNotInCache signals an allow request for an alias with no readable slot in
// the host cache (`<cacheRoot>/<alias>/` missing, or holding no artifact with a
// readable schema). Transport maps it to 404. Wraps [pluginhost.ErrSlotNotFound] — the
// service boundary must not leak pluginhost sentinels to handlers.
var ErrPluginNotInCache = errors.New("sigil: plugin not found in host cache")

// ErrAliasReserved signals an allow request for an alias that is malformed or on the
// closed reserved list ([sharedplugin.IsReserved]). Transport maps it to 422.
//
// The check exists because NIM-377 moved the choice of the name: an artifact used to
// declare its own namespace, so nobody could call themselves `core` without saying so;
// now an operator picks the word, and `core.file.present` in a diff must keep meaning
// the engine.
var ErrAliasReserved = errors.New("sigil: alias is reserved or malformed")

// SlotReader is the surface for reading plugin slots from the cache by REGISTRATION
// ALIAS. Implemented by [cacheSlotReader] over [pluginhost.ReadSlot] /
// [pluginhost.SlotCommitSHA] (with a fixed cacheRoot); narrowing to an interface allows
// unit-testing Service without a real cache directory.
//
// The alias is the only key there is: the artifact carries no self-name, so nothing
// else could find a slot. `ref` is an operator-asserted label on the grant and takes no
// part in the lookup.
//
// SlotCommitSHA returns the commit_sha of the ACTIVE slot (current-symlink target,
// A1-S1) — the audit provenance marker written to plugin_sigils on allow (ADR-026(g),
// outside the signature). Missing/corrupted current → [ErrSlotNotFound] (fail-closed,
// symmetric to ReadSlot).
type SlotReader interface {
	ReadSlot(alias string) (*pluginhost.SlotContents, error)
	SlotCommitSHA(alias string) (string, error)
}

// cacheSlotReader adapts [pluginhost.ReadSlot] / [pluginhost.SlotCommitSHA]
// (with fixed cacheRoot) to [SlotReader]. Production wire-up in `keeper run`
// binds cacheRoot.
type cacheSlotReader struct {
	cacheRoot string
}

func (r cacheSlotReader) ReadSlot(alias string) (*pluginhost.SlotContents, error) {
	return pluginhost.ReadSlot(r.cacheRoot, alias)
}

func (r cacheSlotReader) SlotCommitSHA(alias string) (string, error) {
	return pluginhost.SlotCommitSHA(r.cacheRoot, alias)
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
	Revoke(ctx context.Context, alias, revokedByAID string) error
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

func (s *pgStore) Revoke(ctx context.Context, alias, revokedByAID string) error {
	return Revoke(ctx, s.db, alias, revokedByAID)
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
// Store) and the host cache (SlotReader). Single source of truth for the transport
// facades (OpenAPI — S4a, MCP — S4b): a handler decodes input → service call → maps
// sentinel errors.
//
// Variant C (ADR-026, operator-asserted ref): Allow reads the CURRENT artifact from the
// slot `<cacheRoot>/<alias>/`; `source` and `ref` are what the operator asserts about
// where those bytes came from, and neither takes part in the slot lookup. The integrity
// authority is the sha256 plus the signature, not a git-verified ref.
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
//
// Alias is the registration the operator is creating — address level 1, the slot's
// name, the runtime lookup key. Source and Ref are what they assert about the artifact
// now sitting in that slot, and are the only identity the signature will be over.
type AllowInput struct {
	Alias     string
	Source    string
	Ref       string
	CallerAID string
}

// Allow approves the artifact in the slot registered under Alias, on the identity
// (Source, Ref), and records the grant in plugin_sigils.
//
// Steps:
//  1. the alias must be well-formed and NOT reserved → [ErrAliasReserved]. First,
//     before any disk read: a name that cannot be registered is not worth reading a
//     slot for, and an alias like `core` would shadow an engine address;
//  2. reads the current artifact and its stamped schema document from
//     `<cacheRoot>/<alias>/` — WITHOUT executing it, since at this moment the binary is
//     precisely what is not yet approved. No slot, no artifact, no readable trailer →
//     [ErrPluginNotInCache];
//  3. reads the commit_sha of the ACTIVE slot (the current-symlink target) as the audit
//     provenance mark (ADR-026(g)); a missing or corrupted `current` →
//     [ErrPluginNotInCache] (fail-closed: an approval's provenance must be pinned);
//  4. signs the Sigil block over (source, ref, binary_sha256, schema_sha256) —
//     commit_sha stays outside the block, the alias too;
//  5. inserts the row (commit_sha as a separate audit column). An existing active grant
//     on (source, ref) → [ErrSigilAlreadyActive]; on the alias →
//     [ErrAliasAlreadyRegistered].
//
// Returns the sha256 of the approved artifact (hex) — the handler puts it in the 201.
func (s *Service) Allow(ctx context.Context, in AllowInput) (string, error) {
	if err := ValidateAlias(in.Alias); err != nil {
		return "", err
	}

	slot, err := s.slots.ReadSlot(in.Alias)
	if err != nil {
		if errors.Is(err, pluginhost.ErrSlotNotFound) {
			return "", fmt.Errorf("%w: %s", ErrPluginNotInCache, in.Alias)
		}
		return "", fmt.Errorf("sigil: read plugin slot: %w", err)
	}

	// commit_sha of the ACTIVE slot (current-symlink target). It is the same `current`
	// that ReadSlot followed to read the artifact, so the commit lines up exactly with
	// the bytes being signed. Fail-closed: a slot without `current` yields
	// ErrSlotNotFound → refuse rather than approve with unpinned provenance.
	commitSHA, err := s.slots.SlotCommitSHA(in.Alias)
	if err != nil {
		if errors.Is(err, pluginhost.ErrSlotNotFound) {
			return "", fmt.Errorf("%w: %s (no resolved commit_sha)", ErrPluginNotInCache, in.Alias)
		}
		return "", fmt.Errorf("sigil: read plugin slot commit_sha: %w", err)
	}

	signature, err := s.signer.Load().Sign(in.Source, in.Ref, slot.BinarySHA256, slot.SchemaBytes)
	if err != nil {
		return "", fmt.Errorf("sigil: sign: %w", err)
	}

	rec := &Sigil{
		Alias:     in.Alias,
		Source:    in.Source,
		Ref:       in.Ref,
		SHA256:    slot.BinarySHA256,
		CommitSHA: commitSHA,
		Signature: signature,
		// Schema — the SAME bytes that went into Sign above (one ReadSlot), the
		// byte-exact canon for S6-verify and for the broadcast. There is no second,
		// derived copy: a projection that could disagree with the signed bytes is how
		// the invariant "signed exactly these bytes" decays.
		Schema:       slot.SchemaBytes,
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

// Revoke revokes the active grant registered under alias. No active grant →
// [ErrSigilNotFound].
func (s *Service) Revoke(ctx context.Context, alias, callerAID string) error {
	if err := s.store.Revoke(ctx, alias, callerAID); err != nil {
		return err
	}
	// Cluster-wide re-broadcast of active set (S6c): revoked allow disappears
	// from fresh set → fail-closed on Soul side. Cache-drop semantics —
	// see constraint in [eventStreamHandler.rebroadcastSigils] / connect-time replace.
	s.invalidate(ctx)
	return nil
}

// ValidateAlias checks a registration alias: well-formed ([sharedplugin.AliasPattern])
// and not on the closed reserved list. Exported so the transports can reject early with
// the same rule the service enforces — one list, one shape, no second opinion.
func ValidateAlias(alias string) error {
	switch {
	case alias == "":
		return fmt.Errorf("%w: alias is required", ErrAliasReserved)
	case !sharedplugin.ValidAlias(alias):
		return fmt.Errorf("%w: %q must match %s", ErrAliasReserved, alias, sharedplugin.AliasPattern)
	case sharedplugin.IsReserved(alias):
		return fmt.Errorf("%w: %q is reserved (%s)", ErrAliasReserved, alias,
			strings.Join(sharedplugin.ReservedNames(), ", "))
	}
	return nil
}

// SigilView is a projection of an active grant for the list feed. WITHOUT the signature
// and the schema: the signature is raw crypto material that has no business on an API,
// and the schema is a large document served by the module catalog rather than by the
// allow-list feed. Symmetric to rbac.RoleView.
type SigilView struct {
	Alias        string
	Source       string
	Ref          string
	SHA256       string
	AllowedByAID string
	AllowedAt    time.Time
	RevokedAt    *time.Time
}

// List returns the feed of active grants (newest first) without signature/schema.
func (s *Service) List(ctx context.Context) ([]SigilView, error) {
	recs, err := s.store.ListActive(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]SigilView, 0, len(recs))
	for _, r := range recs {
		out = append(out, SigilView{
			Alias:        r.Alias,
			Source:       r.Source,
			Ref:          r.Ref,
			SHA256:       r.SHA256,
			AllowedByAID: r.AllowedByAID,
			AllowedAt:    r.AllowedAt,
			RevokedAt:    r.RevokedAt,
		})
	}
	return out, nil
}
