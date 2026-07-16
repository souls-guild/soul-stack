package operator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/souls-guild/soul-stack/keeper/internal/rbac"
)

// ErrWouldLockOutCluster — attempt to revoke last active
// cluster-admin. Sentinel isolated for mapping by HTTP/MCP sides to
// `would-lock-out-cluster` (409 / MCP error code) without string sharing.
//
// rbac.md → § Self-lockout invariant: "cannot revoke last AID with
// active wildcard `*`-permission".
var ErrWouldLockOutCluster = errors.New("operator: would lock out cluster (target is the last active cluster-admin)")

// ServiceDeps — dependencies of [Service]. All fields immutable after constructor.
//
// Pool — narrow ExecQueryRower + BeginTx (for atomic Revoke).
// Issuer — JWT issuance.
// RBAC — read-only interface for RolesOf (token issuance with current roles
// from RBAC DB snapshot, ADR-028). Lockout-probe for Revoke does NOT depend on ClusterAdmins() snapshot
// (Slice 3): admin-set taken from DB under FOR UPDATE (rbac.LockEffectiveClusterAdmins).
// TTLDefault — TTL of JWTs issued by Create/IssueToken.
type ServiceDeps struct {
	Pool       ServicePool
	Issuer     JWTIssuer
	RBAC       RBACSource
	TTLDefault time.Duration
	Logger     *slog.Logger
}

// ServicePool — narrow subset of pgxpool.Pool needed by service.
// Real `*pgxpool.Pool` satisfies automatically.
type ServicePool interface {
	ExecQueryRower
	BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error)
}

// JWTIssuer — narrow interface over `*keeper/internal/jwt.Issuer`,
// needed by service. Matches HTTP-handler interface (handlers.JWTIssuer);
// declared here so service doesn't depend on handlers package.
type JWTIssuer interface {
	Issue(aid string, roles []string, ttl time.Duration, bootstrapInitial bool) (string, error)
}

// RBACSource — read-only interface over RBAC enforcer. Implemented by both
// [rbac.Enforcer] and [rbac.Holder] (with hot-reload). Needed only for RolesOf
// (token issuance with current roles from RBAC DB snapshot, ADR-028): lockout-probe
// takes admin-set from DB under FOR UPDATE (rbac.LockEffectiveClusterAdmins,
// Slice 3), not from snapshot.
type RBACSource interface {
	RolesOf(aid string) []string
}

// Invalidator — interface for cluster-wide RBAC invalidation (ADR-014 Amendment
// 2026-05-27, JWT immediate revoke). After successful Revoke commit,
// [Service] calls Invalidate so other Keeper nodes near-instantly
// re-read RBAC snapshot (and pick up `operators.revoked_at` of fresh row)
// instead of waiting for TTL poll. Implemented in `keeper run` via adapter over
// [keeperredis.PublishRBACInvalidate] — same topic `rbac:invalidate` as
// role mutations (ADR-028(d)).
//
// Contract matches [rbac.Invalidator]; local definition needed so
// operator package doesn't depend on rbac.Service. Best-effort: does not
// return publish errors (Revoke already committed to DB), implementation
// logs and swallows.
type Invalidator interface {
	Invalidate(ctx context.Context)
}

// Service — business logic for Operator-CRUD per Operator API (ADR-014) +
// MCP-tools (M0.7). Single source of truth for HTTP handlers and
// MCP tool handlers; transport facade (HTTP/MCP) only decodes input
// and encodes output, business invariants live here.
//
// Safe for concurrent use: deps immutable, no state held;
// cluster-wide invalidation — via atomic late-binding
// [SetInvalidator].
type Service struct {
	pool       ServicePool
	issuer     JWTIssuer
	rbac       RBACSource
	ttlDefault time.Duration
	logger     *slog.Logger

	// inv — optional cluster-wide invalidator (ADR-014 Amendment
	// 2026-05-27). Late-binding via [Service.SetInvalidator]: Redis client
	// in `keeper run` brought up AFTER NewService, so injection deferred
	// (pattern rbac.Service.SetInvalidator / serviceregistry.Service.SetInvalidator).
	// atomic.Pointer — concurrent write by setter vs. read from Revoke without
	// separate mutex.
	inv atomic.Pointer[Invalidator]
}

// NewService assembles service. nil logger allowed — caller not writing
// logs (MCP) can pass zero value; internally we no-op via nil check
// (slog v0.0+: nil logger panics, so we substitute with discard in caller).
func NewService(d ServiceDeps) (*Service, error) {
	if d.Pool == nil {
		return nil, errors.New("operator: ServiceDeps.Pool is nil")
	}
	if d.Issuer == nil {
		return nil, errors.New("operator: ServiceDeps.Issuer is nil")
	}
	if d.RBAC == nil {
		return nil, errors.New("operator: ServiceDeps.RBAC is nil")
	}
	if d.TTLDefault <= 0 {
		return nil, errors.New("operator: ServiceDeps.TTLDefault must be positive")
	}
	return &Service{
		pool:       d.Pool,
		issuer:     d.Issuer,
		rbac:       d.RBAC,
		ttlDefault: d.TTLDefault,
		logger:     d.Logger,
	}, nil
}

// SetInvalidator late-binds cluster-wide invalidator (ADR-014
// Amendment 2026-05-27). Called from `keeper run` after Redis
// client startup. nil — detach invalidator (return to pure TTL-poll).
// Idempotent, thread-safe.
func (s *Service) SetInvalidator(inv Invalidator) {
	if inv == nil {
		s.inv.Store(nil)
		return
	}
	s.inv.Store(&inv)
}

// invalidate sends cluster-wide invalidate signal after successful Revoke
// commit (ADR-014 Amendment 2026-05-27). No-op if invalidator not connected
// (single-Keeper/dev). Best-effort: Invalidate implementation logs and
// swallows publish errors — Revoke already committed, signal loss
// compensated by TTL poll ([rbac.DefaultRefreshInterval]).
func (s *Service) invalidate(ctx context.Context) {
	if p := s.inv.Load(); p != nil {
		(*p).Invalidate(ctx)
	}
}

// CreateInput — parameters for Create. Fields validated by service: AID by
// AIDPattern, DisplayName — non-empty (default = AID if empty), CallerAID —
// non-empty (transport contract: middleware guarantees non-empty Subject).
//
// Roles — optional list of roles to attach to new Archon
// in same transaction as INSERT operator. Any error (nonexistent
// role, FK-violation, duplicate grant) → rollback, operator NOT created
// (atomic create+grant, UX-fix: operator makes one API call instead of
// two-three). Empty/nil — old path without tx (backward-compat).
type CreateInput struct {
	AID         string
	DisplayName string
	CallerAID   string
	Roles       []string
}

// CreateResult — result of Create. JWT and ExpiresAt — issued token returned
// to caller exactly once (no re-issue path without explicit
// issue-token call).
//
// GrantedRoles — deterministic (input order) list of roles attached
// in same transaction as INSERT (CreateInput.Roles). Empty/nil if caller
// did not pass Roles. JWT in this response issued AFTER tx commit — token
// already carries granted roles (RolesOf after commit sees fresh membership).
type CreateResult struct {
	AID          string
	DisplayName  string
	AuthMethod   AuthMethod
	CreatedAt    time.Time
	CreatedByAID string
	GrantedRoles []string
	JWT          string
	ExpiresAt    time.Time
}

// Create inserts new Archon and issues JWT.
//
// Returns sentinel errors (transport maps to HTTP 4xx / MCP error code):
//   - [ErrOperatorAlreadyExists] — AID taken (409 / `operator-already-exists`).
//   - fmt.Errorf("invalid AID …") — AID fails regex (422).
//   - JWT-issue-failure after successful Insert — fmt.Errorf wrapper, manual
//     reconciliation (operator in DB, audit_log written by transport side).
//
// If in.Roles non-empty — atomic create+grant (UX-fix): Insert operator
// and all GrantOperator calls go in one PostgreSQL tx. Any error
// (nonexistent role/AID, duplicate) → rollback, operator NOT created.
// Sentinels [rbac.ErrRoleNotFound] / [rbac.ErrOperatorNotFound] passed
// as-is for mapping in transport layer (422 / 404).
func (s *Service) Create(ctx context.Context, in CreateInput) (*CreateResult, error) {
	if !ValidAID(in.AID) {
		return nil, fmt.Errorf("operator: invalid AID %q (must match %s)", in.AID, AIDPattern)
	}
	if in.CallerAID == "" {
		return nil, errors.New("operator: CallerAID is empty (transport must populate)")
	}
	// Pre-validation of role names before round-trip (better error, no tx hold).
	// SQL CHECK rbac_roles_name_format will catch garbage anyway, but application
	// check gives clean validation-failed error, not FK/CHECK-violation.
	for _, r := range in.Roles {
		if !rbac.ValidRoleName(r) {
			return nil, fmt.Errorf("operator: invalid role name %q (must match %s)", r, rbac.RoleNamePattern)
		}
	}

	displayName := in.DisplayName
	if displayName == "" {
		displayName = in.AID
	}

	creator := in.CallerAID
	op := &Operator{
		AID:          in.AID,
		DisplayName:  displayName,
		AuthMethod:   AuthMethodJWT,
		CreatedByAID: &creator,
	}

	grantedRoles, err := s.insertWithOptionalRoles(ctx, op, in.Roles)
	if err != nil {
		return nil, err
	}

	// Cluster-wide RBAC invalidation after atomic create+grant (parity with
	// rbac.Service.GrantOperator): other Keeper nodes near-instantly
	// re-read RBAC snapshot and pick up new membership. No-op without roles
	// (without membership changes RBAC snapshot doesn't change — TTL poll sees
	// new AID independently via `loadRevoked` / no, via regular
	// operators read not needed). Call only when grants happened — otherwise
	// unnecessary publish traffic.
	if len(grantedRoles) > 0 {
		s.invalidate(ctx)
	}

	// JWT issued AFTER tx commit — RolesOf sees fresh membership
	// (with TTL-poll delay, but invalidate above accelerates propagation).
	// Take current snapshot: with atomic create+grant local enforcer
	// of first Keeper already picks up update on next reload (best-
	// effort; token carries roles only for information anyway, authz
	// checked against fresh DB snapshot on each request — ADR-028).
	roles := s.rbac.RolesOf(in.AID)
	expiresAt := time.Now().UTC().Add(s.ttlDefault)
	token, err := s.issuer.Issue(in.AID, roles, s.ttlDefault, false)
	if err != nil {
		// Insert already committed. Caller (transport side) logs
		// "manual reconciliation may be needed". Service also logs
		// so trace is independent of transport.
		if s.logger != nil {
			s.logger.Error("operator.Create: issue JWT failed AFTER insert committed; manual reconciliation may be needed",
				slog.String("aid", in.AID),
				slog.String("by_aid", in.CallerAID),
				slog.Any("error", err),
			)
		}
		return nil, fmt.Errorf("operator: issue JWT failed: %w", err)
	}

	// SelectByAID after Insert — to return created_at from DB
	// (DEFAULT NOW()), not local "now". On Select error don't fail
	// Create — Insert + JWT already successful; fall back to
	// local time. Symmetric with HTTP-handler (M0.6b).
	saved, err := SelectByAID(ctx, s.pool, in.AID)
	if err != nil {
		if s.logger != nil {
			s.logger.Warn("operator.Create: post-insert SelectByAID failed",
				slog.String("aid", in.AID),
				slog.String("by_aid", in.CallerAID),
				slog.Any("error", err),
			)
		}
		saved = op
		saved.CreatedAt = time.Now().UTC()
	}

	return &CreateResult{
		AID:          saved.AID,
		DisplayName:  saved.DisplayName,
		AuthMethod:   saved.AuthMethod,
		CreatedAt:    saved.CreatedAt,
		CreatedByAID: creator,
		GrantedRoles: grantedRoles,
		JWT:          token,
		ExpiresAt:    expiresAt,
	}, nil
}

// insertWithOptionalRoles — internal path of Create. If roles empty —
// fast-path without tx (Insert via s.pool, zero regression risk for
// existing calls without Roles). Otherwise — atomic tx: BeginTx → Insert →
// rbac.GrantOperator x N → Commit; any error → rollback, operator not
// created.
//
// Returns actually-granted roles (input order). Sentinel errors
// (ErrOperatorAlreadyExists / rbac.ErrRoleNotFound / rbac.ErrOperatorNotFound)
// passed as-is for mapping in transport layer.
func (s *Service) insertWithOptionalRoles(ctx context.Context, op *Operator, roles []string) ([]string, error) {
	if len(roles) == 0 {
		if err := Insert(ctx, s.pool, op); err != nil {
			return nil, err
		}
		return nil, nil
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("operator: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := Insert(ctx, tx, op); err != nil {
		return nil, err
	}

	// GrantOperator called as repository function (rbac.GrantOperator), not
	// rbac.Service.GrantOperator facade: latter opens its own tx and
	// does least-privilege check we don't need (Create — privileged
	// path `operator.create` permission; operator who could create
	// Archon can also attach roles).
	//
	// granted_by_aid = CallerAID — track initiator in audit trail.
	creator := op.CreatedByAID
	for _, role := range roles {
		if err := rbac.GrantOperator(ctx, tx, role, op.AID, creator); err != nil {
			return nil, mapGrantRoleError(err, role)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("operator: commit tx: %w", err)
	}
	return append([]string(nil), roles...), nil
}

// mapGrantRoleError converts rbac-repository error from GrantOperator to
// application sentinels: FK-violation on role_name → [rbac.ErrRoleNotFound],
// FK on aid → [rbac.ErrOperatorNotFound] (latter impossible here —
// operator just inserted in same tx, but we defend explicitly for
// diagnostics). Repository function already maps via [rbac.wrapPgErr], but
// specific sentinel `role-not-found` not extracted; we match SQLSTATE 23503
// and constraint name in message text.
//
// role parameter goes in message for UX ("role X not found"), this is not
// machine-parseable — transport layer takes sentinel.
func mapGrantRoleError(err error, role string) error {
	if err == nil {
		return nil
	}
	// rbac.GrantOperator wraps original via wrapPgErr — pg code
	// accessible inside. Pull pgconn.PgError directly, like mapInsertError.
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == pgErrCodeForeignKeyViolation {
		// Constraint name contains reference to parent table:
		// `rbac_role_operators_role_name_fkey` → rbac_roles (role),
		// `rbac_role_operators_aid_fkey` → operators (AID). Distinguish
		// by substring — constraint names fixed by migration.
		if strings.Contains(pgErr.ConstraintName, "role_name") {
			return fmt.Errorf("%w: %q: %w", rbac.ErrRoleNotFound, role, err)
		}
		return fmt.Errorf("%w (constraint %s): %w", rbac.ErrOperatorNotFound, pgErr.ConstraintName, err)
	}
	return fmt.Errorf("operator: grant role %q: %w", role, err)
}

// RevokeInput — parameters for Revoke.
type RevokeInput struct {
	AID       string
	Reason    string
	CallerAID string
}

// Revoke clears active flag on operator (RevokedAt=NOW), saving reason
// in metadata.revoke_reason (if non-empty).
//
// Atomicity (architect-verdict M0.6b §1): self-lockout probe and UPDATE
// go in one transaction with SELECT … FOR UPDATE on admin-set. This
// serializes concurrent revoke calls.
//
// Returns sentinel errors:
//   - [ErrOperatorNotFound] — AID does not exist (404).
//   - [ErrOperatorAlreadyRevoked] — already revoked (409 / `operator-revoked`).
//   - [ErrWouldLockOutCluster] — target is only active
//     cluster-admin (409 / `would-lock-out-cluster`).
//   - fmt.Errorf("invalid AID …") — AID fails regex (422).
//   - others — wrap pgx errors (500).
func (s *Service) Revoke(ctx context.Context, in RevokeInput) error {
	if !ValidAID(in.AID) {
		return fmt.Errorf("operator: invalid AID %q (must match %s)", in.AID, AIDPattern)
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("operator: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Admin-set source — DB under FOR UPDATE (rbac.LockEffectiveClusterAdmins),
	// NOT in-memory ClusterAdmins() snapshot (Slice 3, architect-verdict §2).
	// Detailed "why DB not snapshot" (staleness gap + serialization with
	// role mutations) — in [rbac.directClusterAdminsForUpdateSQL]; don't duplicate.
	//
	// Lock order (deterministic, against deadlock with role mutations):
	// first ro/rp/o (FOR UPDATE OF in LockEffectiveClusterAdmins), then
	// UPDATE operators row of target in Revoke. Single core = single order.
	admins, err := rbac.LockEffectiveClusterAdmins(ctx, tx)
	if err != nil {
		return fmt.Errorf("operator: lock effective cluster-admins: %w", err)
	}

	// Invariant: after revocation target will have ≥1 active operator with
	// effective `*`. Target after revoke stops being active admin
	// (revoked_at != NULL), so we exclude it from admin-set by AID filter;
	// if target is not admin at all — filter changes nothing and lockout impossible.
	survivors := excludeAID(admins, in.AID)
	if isInSet(admins, in.AID) && len(survivors) == 0 {
		return ErrWouldLockOutCluster
	}

	if err := Revoke(ctx, tx, in.AID, in.Reason); err != nil {
		// ErrOperatorNotFound / ErrOperatorAlreadyRevoked — pass as-is
		// for mapping in transport layer.
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("operator: commit tx: %w", err)
	}
	// Cluster-wide RBAC invalidation (ADR-014 Amendment 2026-05-27): publish
	// `rbac:invalidate` after successful commit — other Keeper nodes
	// near-instantly re-read RBAC snapshot and pick up `operators.revoked_at`
	// of this row (Snapshot.Revoked). Best-effort, Invalidate implementation
	// swallows publish errors.
	s.invalidate(ctx)
	return nil
}

// IssueTokenInput — parameters for IssueToken.
type IssueTokenInput struct {
	AID       string
	CallerAID string
}

// IssueTokenResult — issued token.
type IssueTokenResult struct {
	AID       string
	JWT       string
	ExpiresAt time.Time
}

// IssueToken issues new JWT for existing active Archon.
// Roles resolved from RBAC DB snapshot at time of issuance (s.rbac.RolesOf,
// ADR-028). JWT-roles — informational and NOT authoritative for authz: on
// each request Keeper re-checks rights against current DB snapshot.
//
// Returns:
//   - [ErrOperatorNotFound] — AID does not exist (404).
//   - [ErrOperatorRevoked-sentinel] — operator revoked (409).
//   - fmt.Errorf("invalid AID …") — AID fails regex (422).
//
// ErrOperatorRevoked here — re-use [ErrOperatorAlreadyRevoked] for mapping
// to `operator-revoked` error (transport).
func (s *Service) IssueToken(ctx context.Context, in IssueTokenInput) (*IssueTokenResult, error) {
	if !ValidAID(in.AID) {
		return nil, fmt.Errorf("operator: invalid AID %q (must match %s)", in.AID, AIDPattern)
	}
	op, err := SelectByAID(ctx, s.pool, in.AID)
	if err != nil {
		return nil, err
	}
	if op.IsRevoked() {
		return nil, ErrOperatorAlreadyRevoked
	}
	roles := s.rbac.RolesOf(in.AID)
	expiresAt := time.Now().UTC().Add(s.ttlDefault)
	token, err := s.issuer.Issue(in.AID, roles, s.ttlDefault, false)
	if err != nil {
		return nil, fmt.Errorf("operator: issue JWT failed: %w", err)
	}
	return &IssueTokenResult{
		AID:       in.AID,
		JWT:       token,
		ExpiresAt: expiresAt,
	}, nil
}

// List returns page of Archons under filter. Thin wrapper over [List];
// service layer needed for symmetry with Create/Revoke/IssueToken (single
// source of truth for HTTP/MCP, M0.7 #6).
func (s *Service) List(ctx context.Context, f ListFilter, offset, limit int) ([]*Operator, int, error) {
	return List(ctx, s.pool, f, offset, limit)
}

// Get reads Archon by AID. Thin wrapper over [SelectByAID]; sentinel
// errors passed as-is for mapping in transport layer.
func (s *Service) Get(ctx context.Context, aid string) (*Operator, error) {
	if !ValidAID(aid) {
		return nil, fmt.Errorf("operator: invalid AID %q (must match %s)", aid, AIDPattern)
	}
	return SelectByAID(ctx, s.pool, aid)
}

// isInSet — linear membership check (admin-set tens of AIDs).
func isInSet(set []string, target string) bool {
	for _, a := range set {
		if a == target {
			return true
		}
	}
	return false
}

// excludeAID returns copy of set without occurrences of target. Used by
// Revoke lockout-probe: target after revocation stops being active
// admin, remaining set — "surviving" effective `*`-admins.
func excludeAID(set []string, target string) []string {
	out := make([]string, 0, len(set))
	for _, a := range set {
		if a != target {
			out = append(out, a)
		}
	}
	return out
}
