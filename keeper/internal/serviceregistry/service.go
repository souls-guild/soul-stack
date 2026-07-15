package serviceregistry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/souls-guild/soul-stack/shared/config"
)

// ServicePool — narrow subset of pgxpool.Pool that [Service] needs. The real
// `*pgxpool.Pool` satisfies it automatically. Declared locally (like
// rbac/augur) so the package doesn't pull operator/pgxpool into its public surface.
type ServicePool interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Compile-time check: pool/Tx satisfy ServicePool via ExecQueryRower.
var _ ServicePool = (ExecQueryRower)(nil)

// Invalidator — cluster-wide registry invalidation surface (S2). After a
// successful commit of a CRUD mutation (Insert/Update/Delete service /
// SetSetting), [Service] calls Invalidate so the other Keeper nodes re-read
// the snapshot near-instantly (instead of waiting for the TTL poll). Implemented
// in `keeper run` by an adapter over [keeperredis.PublishServiceInvalidate];
// in single-Keeper/dev mode (no Redis) the invalidator isn't wired up — only
// TTL-poll works.
//
// Invalidate is best-effort: it does NOT return a publish error (the mutation
// is already committed to the DB); the implementation logs and swallows it.
// Same pattern as [rbac.Invalidator] / [sigil.Invalidator].
type Invalidator interface {
	Invalidate(ctx context.Context)
}

// ServiceDeps — dependencies of [Service]. All fields immutable after the constructor.
type ServiceDeps struct {
	Pool   ServicePool
	Logger *slog.Logger
}

// Service — CRUD business logic for the Service registry and cluster-wide
// settings. Single source of truth for the future transport facade
// (OpenAPI/MCP — slice S3); validation (name format, non-empty git/ref,
// refresh format) lives here, transport only decodes input / encodes output.
//
// Safe for concurrent use: deps are immutable, no state is held; cluster-wide
// invalidation goes through atomic late-binding [SetInvalidator].
type Service struct {
	pool   ServicePool
	logger *slog.Logger

	// inv — optional cluster-wide invalidator (S2). Late-bound via
	// [Service.SetInvalidator]: the Redis client in `keeper run` comes up
	// AFTER NewService, so injection is deferred (same pattern as
	// rbac.Service.SetInvalidator / sigil.Service.SetInvalidator).
	// atomic.Pointer allows concurrent writes by the setter vs. reads from
	// mutations without a separate mutex.
	inv atomic.Pointer[Invalidator]
}

// NewService builds the service. Pool is required.
func NewService(d ServiceDeps) (*Service, error) {
	if d.Pool == nil {
		return nil, errors.New("serviceregistry: ServiceDeps.Pool is nil")
	}
	return &Service{pool: d.Pool, logger: d.Logger}, nil
}

// SetInvalidator wires up the cluster-wide invalidator (S2) via late binding.
// Called from `keeper run` after the Redis client comes up. nil removes the
// invalidator (falls back to pure TTL-poll). Idempotent, thread-safe.
func (s *Service) SetInvalidator(inv Invalidator) {
	if inv == nil {
		s.inv.Store(nil)
		return
	}
	s.inv.Store(&inv)
}

// invalidate sends a cluster-wide invalidate signal after a successful commit
// of a CRUD mutation (S2). No-op if no invalidator is wired up (single-Keeper/
// dev). Best-effort: the Invalidate implementation logs and swallows the
// publish error itself — the mutation is already committed, and a lost signal
// is compensated by TTL-poll.
func (s *Service) invalidate(ctx context.Context) {
	if p := s.inv.Load(); p != nil {
		(*p).Invalidate(ctx)
	}
}

// CreateServiceInput — parameters for CreateService. CallerAID is optional
// (nil → created_by_aid IS NULL for seed/system creation; transport fills it
// from claims).
type CreateServiceInput struct {
	Name      string
	Git       string
	Ref       string
	Refresh   *string
	CallerAID *string
}

// CreateService creates a Service record. All fields are validated BEFORE the
// DB round trip (better error, no wasted call on bad input).
//
// Returns:
//   - [ErrInvalidName] / [ErrInvalidGit] / [ErrInvalidRef] / [ErrInvalidRefresh] — 422;
//   - [ErrAlreadyExists] — name is taken (409);
//   - [ErrOperatorNotFound] — CallerAID doesn't exist in operators (FK).
func (s *Service) CreateService(ctx context.Context, in CreateServiceInput) (*ServiceEntry, error) {
	if err := validateFields(in.Name, in.Git, in.Ref, in.Refresh); err != nil {
		return nil, err
	}
	e := &ServiceEntry{
		Name:         in.Name,
		Git:          in.Git,
		Ref:          in.Ref,
		Refresh:      in.Refresh,
		CreatedByAID: in.CallerAID,
		// Initial creation: updated_by_aid = created_by_aid (the record wasn't
		// "changed" by a separate operator, but updated_at = created_at and
		// there's one author). Symmetric with updated_at DEFAULT NOW() in the schema.
		UpdatedByAID: in.CallerAID,
	}
	if err := InsertService(ctx, s.pool, e); err != nil {
		return nil, err
	}
	s.invalidate(ctx)
	return e, nil
}

// GetService reads a Service record by name. [ErrNotFound] if none.
func (s *Service) GetService(ctx context.Context, name string) (*ServiceEntry, error) {
	return GetService(ctx, s.pool, name)
}

// ListServices returns all Service records (sort name ASC).
func (s *Service) ListServices(ctx context.Context) ([]*ServiceEntry, error) {
	return ListServices(ctx, s.pool)
}

// UpdateServiceInput — parameters for UpdateService. Replaces the record's
// mutable fields (git/ref/refresh); name is the key, it doesn't change.
type UpdateServiceInput struct {
	Name      string
	Git       string
	Ref       string
	Refresh   *string
	CallerAID *string
}

// UpdateService replaces the mutable fields of a Service record (replace
// semantics). Validation happens BEFORE the round trip.
//
// CallerAID=nil → updated_by_aid is cleared (set semantics, the previous
// author is NOT kept); transport usually fills CallerAID from claims.
//
// Returns:
//   - validation sentinels (422);
//   - [ErrNotFound] — no record with that name (404);
//   - [ErrOperatorNotFound] — CallerAID doesn't exist (FK).
func (s *Service) UpdateService(ctx context.Context, in UpdateServiceInput) (*ServiceEntry, error) {
	if err := validateFields(in.Name, in.Git, in.Ref, in.Refresh); err != nil {
		return nil, err
	}
	e := &ServiceEntry{
		Name:         in.Name,
		Git:          in.Git,
		Ref:          in.Ref,
		Refresh:      in.Refresh,
		UpdatedByAID: in.CallerAID,
	}
	if err := UpdateService(ctx, s.pool, e); err != nil {
		return nil, err
	}
	s.invalidate(ctx)
	return e, nil
}

// DeleteService deletes a Service record by name. [ErrNotFound] if none.
func (s *Service) DeleteService(ctx context.Context, name string) error {
	if err := DeleteService(ctx, s.pool, name); err != nil {
		return err
	}
	s.invalidate(ctx)
	return nil
}

// GetSetting reads a cluster-wide setting by key. Validates the key format
// before the round trip. [ErrSettingNotFound] if the key doesn't exist.
func (s *Service) GetSetting(ctx context.Context, key string) (*Setting, error) {
	if !ValidSettingKey(key) {
		return nil, fmt.Errorf("%w: %q must match %s", ErrInvalidSettingKey, key, SettingKeyPattern)
	}
	return GetSetting(ctx, s.pool, key)
}

// SetSettingInput — parameters for SetSetting. CallerAID is optional.
type SetSettingInput struct {
	Key       string
	Value     string
	CallerAID *string
}

// SetSetting upserts a cluster-wide setting (key → value). Validates the key
// format before the round trip; value is stored as-is (semantics are up to
// the consumer).
//
// Returns:
//   - [ErrInvalidSettingKey] — key doesn't match the format (422);
//   - [ErrOperatorNotFound] — CallerAID doesn't exist (FK).
func (s *Service) SetSetting(ctx context.Context, in SetSettingInput) (*Setting, error) {
	if !ValidSettingKey(in.Key) {
		return nil, fmt.Errorf("%w: %q must match %s", ErrInvalidSettingKey, in.Key, SettingKeyPattern)
	}
	set := &Setting{Key: in.Key, Value: in.Value, UpdatedByAID: in.CallerAID}
	if err := SetSetting(ctx, s.pool, set); err != nil {
		return nil, err
	}
	s.invalidate(ctx)
	return set, nil
}

// validateFields — shared application-level validation of Service record
// fields (create/update): name format, non-empty git/ref, refresh format via
// config.ParseDuration (if set). Duplicates DB CHECKs for a better error
// before the round trip; refresh isn't caught by a DB CHECK (like augur
// token_ttl) — only here.
func validateFields(name, git, ref string, refresh *string) error {
	if !ValidName(name) {
		return fmt.Errorf("%w: %q must match %s", ErrInvalidName, name, NamePattern)
	}
	if git == "" {
		return ErrInvalidGit
	}
	if ref == "" {
		return ErrInvalidRef
	}
	if refresh != nil {
		if _, err := config.ParseDuration(*refresh); err != nil {
			return fmt.Errorf("%w %q: %w", ErrInvalidRefresh, *refresh, err)
		}
	}
	return nil
}
