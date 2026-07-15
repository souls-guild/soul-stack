package augur

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// The Augur registry's management Service (operator-facing CRUD for
// Omen / Rite through OpenAPI / MCP, augur.md §4 / rbac.md §Augur). A single
// source of truth for the REST handler and the MCP tool: both call this
// Service so validation and the error contract don't drift apart (the
// serviceregistry.Service pattern).
//
// DIFFERENCE from the broker (broker*.go / resolve.go): the broker resolves
// an AugurRequest from a Soul (auth + egress); the Service here only manages
// registry records. The broker doesn't use this type.

// ErrValidation — a sentinel for the service validation that the management
// Service runs BEFORE the DB round trip (format of name / source_type /
// auth_ref / allow / subject / token fields). Transport maps it to 422 (REST
// TypeValidationFailed / MCP validation-failed). The specific diagnostic text
// is in the wrapped error (errors.Unwrap), for the log; the client gets the
// whole wrapped message (it's already public — built here, without internal
// SQL/stack details).
var ErrValidation = errors.New("augur: validation failed")

// ServicePool — the narrow subset of pgxpool.Pool that [Service] needs. A
// real `*pgxpool.Pool` satisfies it automatically (through
// [ExecQueryRower]). Declared locally (like serviceregistry/rbac) so the
// package doesn't pull pgxpool into its public surface.
type ServicePool interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Compile-time check: pool/Tx satisfy ServicePool through ExecQueryRower.
var _ ServicePool = (ExecQueryRower)(nil)

// ServiceDeps — [Service]'s dependencies. All fields are immutable after
// the constructor.
type ServiceDeps struct {
	Pool   ServicePool
	Logger *slog.Logger
}

// Service — the business logic for operator-facing CRUD of the Augur
// registry. Safe for concurrent use: deps are immutable, it holds no state
// between calls.
type Service struct {
	pool   ServicePool
	logger *slog.Logger
}

// NewService assembles the management Service. Pool is required.
func NewService(d ServiceDeps) (*Service, error) {
	if d.Pool == nil {
		return nil, errors.New("augur: ServiceDeps.Pool is nil")
	}
	return &Service{pool: d.Pool, logger: d.Logger}, nil
}

// --- Omen -------------------------------------------------------------

// CreateOmenInput — CreateOmen's parameters. CallerAID is optional (nil →
// created_by_aid IS NULL; transport fills it in from the caller's claims).
type CreateOmenInput struct {
	Name       string
	SourceType string
	Endpoint   string
	AuthRef    string
	CallerAID  *string
}

// CreateOmen validates fields BEFORE the round trip (a better error, no
// wasted call on bad input), then inserts the record.
//
// Returns:
//   - [ErrValidation] (wrapped) — bad name / source_type / endpoint / auth_ref (422);
//   - [ErrOmenAlreadyExists] — name already taken (409);
//   - wrapped fmt.Errorf — FK/CHECK/infra failure (500).
func (s *Service) CreateOmen(ctx context.Context, in CreateOmenInput) (*Omen, error) {
	src := SourceType(in.SourceType)
	if !ValidName(in.Name) {
		return nil, fmt.Errorf("%w: invalid omen name %q (must match %s)", ErrValidation, in.Name, NamePattern)
	}
	if !ValidSourceType(src) {
		return nil, fmt.Errorf("%w: invalid source_type %q (must be vault/prometheus/elk)", ErrValidation, in.SourceType)
	}
	if in.Endpoint == "" {
		return nil, fmt.Errorf("%w: endpoint is empty", ErrValidation)
	}
	if !ValidAuthRef(in.AuthRef) {
		return nil, fmt.Errorf("%w: invalid auth_ref %q (must be a vault-ref vault:<mount>/<path>)", ErrValidation, in.AuthRef)
	}

	o := &Omen{
		Name:         in.Name,
		SourceType:   src,
		Endpoint:     in.Endpoint,
		AuthRef:      in.AuthRef,
		CreatedByAID: in.CallerAID,
	}
	if err := InsertOmen(ctx, s.pool, o); err != nil {
		return nil, err
	}
	return o, nil
}

// ListOmens returns a page of Omens and the total count (sorted by
// created_at DESC, name ASC).
func (s *Service) ListOmens(ctx context.Context, offset, limit int) ([]*Omen, int, error) {
	return SelectAllOmens(ctx, s.pool, offset, limit)
}

// GetOmen reads an Omen by PK. [ErrOmenNotFound] if it doesn't exist.
func (s *Service) GetOmen(ctx context.Context, name string) (*Omen, error) {
	return SelectOmenByName(ctx, s.pool, name)
}

// DeleteOmen deletes an Omen by PK (its Rites cascade). [ErrOmenNotFound]
// if the record didn't exist.
func (s *Service) DeleteOmen(ctx context.Context, name string) error {
	return DeleteOmen(ctx, s.pool, name)
}

// --- Rite -------------------------------------------------------------

// CreateRiteInput — CreateRite's parameters. Subject is XOR Coven/SID.
// allow is raw JSONB (its shape is checked against the Omen's source_type).
// TokenTTL / TokenNumUses only make sense for a vault-Omen with
// Delegate=true.
type CreateRiteInput struct {
	Omen         string
	Coven        *string
	SID          *string
	Allow        json.RawMessage
	Delegate     bool
	TokenTTL     *string
	TokenNumUses *int
	CallerAID    *string
}

// CreateRite validates the subject (XOR) BEFORE the round trip, then
// inserts. The allow shape by source_type and the token fields are further
// validated by [InsertRite] (it needs to resolve the Omen on the same db)
// — its errors are also wrapped into [ErrValidation].
//
// Returns:
//   - [ErrValidation] (wrapped) — bad subject / allow / token fields (422);
//   - [ErrOmenNotFound] — the Omen doesn't exist (404);
//   - wrapped fmt.Errorf — FK/CHECK/infra failure (500).
func (s *Service) CreateRite(ctx context.Context, in CreateRiteInput) (*Rite, error) {
	r := &Rite{
		Omen:         in.Omen,
		Coven:        in.Coven,
		SID:          in.SID,
		Allow:        in.Allow,
		Delegate:     in.Delegate,
		TokenTTL:     in.TokenTTL,
		TokenNumUses: in.TokenNumUses,
		CreatedByAID: in.CallerAID,
	}
	if r.Omen == "" {
		return nil, fmt.Errorf("%w: omen is empty", ErrValidation)
	}
	if err := ValidateSubjectXOR(r); err != nil {
		return nil, fmt.Errorf("%w: %s", ErrValidation, err.Error())
	}
	if len(r.Allow) == 0 {
		return nil, fmt.Errorf("%w: allow is empty", ErrValidation)
	}

	if err := InsertRite(ctx, s.pool, r); err != nil {
		// InsertRite resolves the Omen and further validates the allow shape /
		// token fields. These errors are marked with the ErrAllowShape /
		// ErrTokenFields sentinels — this is service validation, not infra; map
		// them to ErrValidation. ErrOmenNotFound / FK-violation are passed through
		// as-is (404 / 500).
		if isRiteValidationError(err) {
			return nil, fmt.Errorf("%w: %s", ErrValidation, err.Error())
		}
		return nil, err
	}
	return r, nil
}

// ListRitesByOmen returns all Rites for one Omen (sorted by created_at
// DESC, id ASC). The CRUD list-by-omen filter (augur.md §6).
func (s *Service) ListRitesByOmen(ctx context.Context, omen string) ([]*Rite, error) {
	return SelectRitesByOmen(ctx, s.pool, omen)
}

// DeleteRite deletes a Rite by its surrogate PK. [ErrRiteNotFound] if the
// record didn't exist.
func (s *Service) DeleteRite(ctx context.Context, id int64) error {
	return DeleteRite(ctx, s.pool, id)
}

// isRiteValidationError distinguishes InsertRite's service validation
// (allow-shape / token fields) from not-found / infra failures. The
// allow/token validators ([ValidateAllow] / [ValidateTokenFields]) live in
// the storage slice and don't know about the management Service's
// [ErrValidation], so they mark their errors with the [ErrAllowShape] /
// [ErrTokenFields] sentinels; we match via errors.Is rather than a string
// prefix of the text (renaming the diagnostic shouldn't silently break the
// 422→500 mapping).
func isRiteValidationError(err error) bool {
	return errors.Is(err, ErrAllowShape) || errors.Is(err, ErrTokenFields)
}
