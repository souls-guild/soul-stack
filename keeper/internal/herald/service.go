package herald

import (
	"context"
	"errors"
	"log/slog"
)

// Service is business logic for CRUD of `heralds`/`tidings` registries + invalidation
// of dispatcher rule snapshot (ADR-052, S4). Single source of truth for
// HTTP handlers (OpenAPI) and MCP tool handlers: transport facade only
// decodes input and encodes output, invariants and invalidation live here.
//
// Invalidation is two-level (like pushprovider.Service):
//   - in-process — [Invalidator.InvalidateRules] (same process immediately
//     rereads enabled-Tiding snapshot of dispatcher on next match);
//   - cross-keeper — [RedisInvalidator.PublishHeraldInvalidate] (other node
//     by subscription `herald:invalidate` triggers its InvalidateRules).
//
// Mutations affecting match (any CRUD Herald/Tiding) flush cache:
// disabled→enabled transition, change event_types/filters/herald-binding —
// all visible to dispatcher immediately, not via TTL DefaultRuleCacheTTL.
type Service struct {
	pool        ExecQueryRower
	invalidator Invalidator
	redis       RedisInvalidator
	logger      *slog.Logger
	// secretWriter/acceptPlaintext — dual-mode ingestion of plaintext secret (ADR-064,
	// NIM-11). secretWriter=nil OR acceptPlaintext=false → plaintext rejected,
	// only *_ref accepted (secure default).
	secretWriter    SecretWriter
	acceptPlaintext bool
}

// Invalidator is narrow surface for in-process cache flush of dispatcher rules.
// Implemented by [*Dispatcher] (InvalidateRules method). nil-safe nop by default.
type Invalidator interface {
	InvalidateRules()
}

// RedisInvalidator is narrow surface for cross-keeper publication of invalidate
// signal. Implemented by keeperredis wrapper (method adapter in daemon so
// herald package doesn't depend directly on keeperredis). nil → no-op (Redis
// disabled / unit-test without broker): single-instance degrades to TTL+
// in-process invalidate, cluster convergence via DefaultRuleCacheTTL.
type RedisInvalidator interface {
	PublishHeraldInvalidate(ctx context.Context, id string) error
}

// ServiceDeps are dependencies for [Service]. Pool is required; Invalidator/Redis
// optional (nil → no-op invalidation, convergence via TTL).
type ServiceDeps struct {
	Pool        ExecQueryRower
	Invalidator Invalidator
	Redis       RedisInvalidator
	Logger      *slog.Logger
	// SecretWriter materializes plaintext secret to Vault (dual-mode, ADR-064).
	// nil → plaintext ingestion unavailable (only *_ref accepted).
	SecretWriter SecretWriter
	// AcceptPlaintext allows plaintext ingestion (ADR-064 mitigation a: TLS-front).
	// false (default) → plaintext rejected 422.
	AcceptPlaintext bool
}

// NewService constructs service. Pool is required (nil → error). nil Invalidator/
// Redis are substituted with no-op implementations.
func NewService(d ServiceDeps) (*Service, error) {
	if d.Pool == nil {
		return nil, errors.New("herald: ServiceDeps.Pool is nil")
	}
	inv := d.Invalidator
	if inv == nil {
		inv = nopInvalidator{}
	}
	redis := d.Redis
	if redis == nil {
		redis = nopRedisInvalidator{}
	}
	logger := d.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Service{
		pool:            d.Pool,
		invalidator:     inv,
		redis:           redis,
		logger:          logger,
		secretWriter:    d.SecretWriter,
		acceptPlaintext: d.AcceptPlaintext,
	}, nil
}

// --- Herald ----------------------------------------------------------

// CreateHerald inserts Herald channel + invalidates rule snapshot.
// Returns populated record (with created_at/updated_at). Sentinel errors —
// [ErrHeraldExists] (409). Validation of name/type/config/secret_ref is inside
// [InsertHerald] (before write).
func (s *Service) CreateHerald(ctx context.Context, h *Herald) (*Herald, error) {
	// Dual-mode: plaintext secrets → Vault, replace with internal ref (ADR-064).
	// Before Insert (only ref goes to PG); plaintext erased from h.
	if err := materializeHeraldSecrets(ctx, s.secretWriter, s.acceptPlaintext, h); err != nil {
		return nil, err
	}
	if err := InsertHerald(ctx, s.pool, h); err != nil {
		return nil, err
	}
	s.invalidate(ctx, h.ID)
	return h, nil
}

// GetHerald reads Herald by PK. [ErrHeraldNotFound] if missing.
func (s *Service) GetHerald(ctx context.Context, id string) (*Herald, error) {
	return SelectHeraldByID(ctx, s.pool, id)
}

// ListHeralds returns page of channels + total.
func (s *Service) ListHeralds(ctx context.Context, offset, limit int) ([]*Herald, int, error) {
	return SelectAllHeralds(ctx, s.pool, offset, limit)
}

// UpdateHerald replaces mutable fields of channel (replace). Rereads record after
// update to return current timestamps. [ErrHeraldNotFound] if missing.
func (s *Service) UpdateHerald(ctx context.Context, h *Herald) (*Herald, error) {
	// Dual-mode: plaintext → Vault (idempotent write to same path), ADR-064.
	if err := materializeHeraldSecrets(ctx, s.secretWriter, s.acceptPlaintext, h); err != nil {
		return nil, err
	}
	if err := UpdateHerald(ctx, s.pool, h); err != nil {
		return nil, err
	}
	updated, err := SelectHeraldByID(ctx, s.pool, h.ID)
	if err != nil {
		return nil, err
	}
	updated.SecretWritten = h.SecretWritten // write-path marker survives re-read
	s.invalidate(ctx, h.ID)
	return updated, nil
}

// SetHeraldLabel replaces the display caption of one Herald and returns the row
// as it now reads ([ADR-0085], permission herald.label-set, audit
// herald.label_changed).
//
// No invalidation follows, unlike every other write here: the snapshot the
// channel refreshes carries the delivery rules a dispatcher acts on, and a
// caption is not one of them. Republishing would wake every Keeper instance to
// learn a word no code reads.
//
// [ErrHeraldNotFound] if missing.
func (s *Service) SetHeraldLabel(ctx context.Context, id string, label *string) (*Herald, *string, error) {
	previous, err := UpdateHeraldLabel(ctx, s.pool, id, label)
	if err != nil {
		return nil, nil, err
	}
	h, err := SelectHeraldByID(ctx, s.pool, id)
	return h, previous, err
}

// SetTidingLabel replaces the display caption of one Tiding and returns the row
// as it now reads ([ADR-0085], permission tiding.label-set, audit
// tiding.label_changed). No invalidation, for the same reason as
// [Service.SetHeraldLabel].
//
// [ErrTidingNotFound] if missing.
func (s *Service) SetTidingLabel(ctx context.Context, id string, label *string) (*Tiding, *string, error) {
	previous, err := UpdateTidingLabel(ctx, s.pool, id, label)
	if err != nil {
		return nil, nil, err
	}
	t, err := SelectTidingByID(ctx, s.pool, id)
	return t, previous, err
}

// DeleteHerald deletes channel (its Tidings cascade delete) + invalidates.
// [ErrHeraldNotFound] if missing.
func (s *Service) DeleteHerald(ctx context.Context, id string) error {
	if err := DeleteHerald(ctx, s.pool, id); err != nil {
		return err
	}
	s.invalidate(ctx, id)
	return nil
}

// --- Tiding ----------------------------------------------------------

// CreateTiding inserts Tiding rule + invalidates. Sentinel errors —
// [ErrTidingExists] (409), [ErrHeraldNotFound] (404 — FK to nonexistent
// herald). Validation of name/herald/event_types is inside [InsertTiding].
func (s *Service) CreateTiding(ctx context.Context, t *Tiding) (*Tiding, error) {
	if err := InsertTiding(ctx, s.pool, t); err != nil {
		return nil, err
	}
	s.invalidate(ctx, t.ID)
	return t, nil
}

// GetTiding reads Tiding by PK. [ErrTidingNotFound] if missing.
func (s *Service) GetTiding(ctx context.Context, id string) (*Tiding, error) {
	return SelectTidingByID(ctx, s.pool, id)
}

// ListTidings returns page of rules + total. includeEphemeral=false (default)
// hides ephemeral rules (ADR-052(g)); true returns all (debug).
func (s *Service) ListTidings(ctx context.Context, includeEphemeral bool, offset, limit int) ([]*Tiding, int, error) {
	return SelectAllTidings(ctx, s.pool, includeEphemeral, offset, limit)
}

// UpdateTiding replaces mutable fields of rule (replace). Rereads record.
// [ErrTidingNotFound] (404) if PK missing; [ErrHeraldNotFound] (404) if
// FK to nonexistent herald.
func (s *Service) UpdateTiding(ctx context.Context, t *Tiding) (*Tiding, error) {
	if err := UpdateTiding(ctx, s.pool, t); err != nil {
		return nil, err
	}
	updated, err := SelectTidingByID(ctx, s.pool, t.ID)
	if err != nil {
		return nil, err
	}
	s.invalidate(ctx, t.ID)
	return updated, nil
}

// DeleteTiding deletes rule + invalidates. [ErrTidingNotFound] if missing.
func (s *Service) DeleteTiding(ctx context.Context, id string) error {
	if err := DeleteTiding(ctx, s.pool, id); err != nil {
		return err
	}
	s.invalidate(ctx, id)
	return nil
}

// InvalidateTidings is public entry point for two-level invalidation for external
// write paths that create Tiding rules BYPASSING this Service's CRUD
// (voyage.create inserts ephemeral-Tidings via direct herald.InsertTiding in its
// voyage-tx — ADR-052(g)). Without this call, dispatcher dispatches terminal
// of quick run against stale TTL snapshot, and ephemeral notification silently
// misses. Call STRICTLY AFTER tx.Commit (on rollback rule is not in DB —
// nothing to invalidate). Best-effort, nil-safe (see invalidate).
func (s *Service) InvalidateTidings(ctx context.Context, id string) {
	if s == nil {
		return
	}
	s.invalidate(ctx, id)
}

// --- helpers ---------------------------------------------------------

// invalidate flushes rule cache in this process and publishes cross-keeper
// signal. Both operations best-effort: mutation already committed, loss of invalidate
// is compensated by TTL convergence (DefaultRuleCacheTTL). Redis error
// is logged but not returned to caller.
func (s *Service) invalidate(ctx context.Context, id string) {
	s.invalidator.InvalidateRules()
	if err := s.redis.PublishHeraldInvalidate(ctx, id); err != nil {
		s.logger.Warn("herald: publish herald:invalidate failed",
			slog.String("id", id), slog.Any("error", err))
	}
}

// nopInvalidator / nopRedisInvalidator are no-op implementations for cases without
// dispatcher / without Redis (unit-test / single-instance dev).
type nopInvalidator struct{}

func (nopInvalidator) InvalidateRules() {}

type nopRedisInvalidator struct{}

func (nopRedisInvalidator) PublishHeraldInvalidate(context.Context, string) error { return nil }

// compile-time check: *Dispatcher satisfies Invalidator.
var _ Invalidator = (*Dispatcher)(nil)
