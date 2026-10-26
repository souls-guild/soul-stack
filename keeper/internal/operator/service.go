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
	"github.com/souls-guild/soul-stack/shared/config"
)

// ErrWouldLockOutCluster — attempt to revoke last active
// cluster-admin. Sentinel isolated for mapping by HTTP/MCP sides to
// `would-lock-out-cluster` (409 / MCP error code) without string sharing.
//
// rbac.md → § Self-lockout invariant: "cannot revoke last AID with
// active wildcard `*`-permission".
var ErrWouldLockOutCluster = errors.New("operator: would lock out cluster (target is the last active cluster-admin)")

// MinJWTTTL is the shortest lifetime a caller may ask for. Below it a token
// expires before a client can reasonably use it; asking for that is a mistake.
const MinJWTTTL = time.Minute

// ErrInvalidTTL — a requested token lifetime is below [MinJWTTTL] or above
// `auth.jwt.ttl_max` (for a personal token: above the operator's own ceiling).
var ErrInvalidTTL = errors.New("operator: invalid ttl")

// The bounds on personal tokens (ADR-014 amendment NIM-1052). Issuing one needs
// no permission, so both are what any operator can do:
//   - MaxActivePersonalJWTs bounds the live ones — the credentials a person can
//     have exposed at once. A revoke frees a slot.
//   - MaxPersonalJWTsPerDay bounds issuance, revoked ones included. Without it
//     an issue-then-revoke loop would grow `operator_jwts`, grow every node's
//     snapshot of revoked ids until those tokens expire, and reload every
//     node's snapshot once per revoke.
const (
	MaxActivePersonalJWTs = 10
	MaxPersonalJWTsPerDay = 20
)

var (
	// ErrPersonalJWTLimit — the operator already holds [MaxActivePersonalJWTs]
	// active personal tokens.
	ErrPersonalJWTLimit = fmt.Errorf("operator: at most %d active personal tokens; revoke one first", MaxActivePersonalJWTs)
	// ErrPersonalJWTDailyLimit — the operator issued [MaxPersonalJWTsPerDay]
	// personal tokens in the last 24 hours.
	ErrPersonalJWTDailyLimit = fmt.Errorf("operator: at most %d personal tokens a day, revoked ones included; try again later", MaxPersonalJWTsPerDay)
	// ErrPersonalJWTFromPersonal — the request that asks for a personal token is
	// itself made with one (or with a token no row of the caller accounts for),
	// so a personal token cannot mint its own successor. It does not stop an
	// owner who holds `operator.issue-token`: that route mints an `issued`
	// token for anyone, the owner included, from any of its tokens.
	ErrPersonalJWTFromPersonal = errors.New("operator: a personal token cannot issue personal tokens; sign in or use another token")
	// ErrCallerJWTRevoked — the token the request came with is revoked in the
	// database though it still verified here (the RBAC snapshot is behind):
	// nothing may be minted from it (NIM-1051).
	ErrCallerJWTRevoked = errors.New("operator: the token this request was made with is revoked")
	// ErrOwnPasswordViaAdmin — an administrator asked to set its own password
	// through the administrative route; that route needs no current password,
	// so a token holding operator.set-password would be a way to a password
	// without knowing the old one (NIM-1051). PUT /v1/me/password is the way.
	ErrOwnPasswordViaAdmin = errors.New("operator: set your own password with PUT /v1/me/password, which asks for the current one")
)

// ServiceDeps — dependencies of [Service]. All fields immutable after constructor.
//
// Pool — narrow ExecQueryRower + BeginTx (for atomic Revoke).
// Issuer — JWT issuance.
// RBAC — read-only interface for RolesOf (token issuance with current roles
// from RBAC DB snapshot, ADR-028). Lockout-probe for Revoke does NOT depend on ClusterAdmins() snapshot
// (Slice 3): admin-set taken from DB under FOR UPDATE (rbac.LockEffectiveClusterAdmins).
// TTLDefault — TTL of JWTs issued by Create/IssueToken when the caller asks for none.
// TTLMax — the longest TTL a caller may ask for (`auth.jwt.ttl_max`); 0 → TTLDefault.
// OIDCSessionTTL — the lifetime of an OIDC sign-in, the ceiling of an OIDC
// operator's personal token; 0 → TTLDefault.
type ServiceDeps struct {
	Pool           ServicePool
	Issuer         JWTIssuer
	RBAC           RBACSource
	TTLDefault     time.Duration
	TTLMax         time.Duration
	OIDCSessionTTL time.Duration
	Logger         *slog.Logger
}

// ServicePool — narrow subset of pgxpool.Pool needed by service.
// Real `*pgxpool.Pool` satisfies automatically.
type ServicePool interface {
	ExecQueryRower
	BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error)
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
	pool           ServicePool
	issuer         JWTIssuer
	rbac           RBACSource
	ttlDefault     time.Duration
	ttlMax         time.Duration
	oidcSessionTTL time.Duration
	logger         *slog.Logger

	// inv — optional cluster-wide invalidator (ADR-014 Amendment
	// 2026-05-27). Late-binding via [Service.SetInvalidator]: Redis client
	// in `keeper run` brought up AFTER NewService, so injection deferred
	// (pattern rbac.Service.SetInvalidator / serviceregistry.Service.SetInvalidator).
	// atomic.Pointer — concurrent write by setter vs. read from Revoke without
	// separate mutex.
	inv atomic.Pointer[Invalidator]

	// passwords — the local-password policy (`auth.local`, NIM-1051); nil →
	// this Keeper sets no local passwords ([ErrLocalPasswordsDisabled]).
	passwords atomic.Pointer[PasswordPolicy]
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
	ttlMax := d.TTLMax
	if ttlMax == 0 {
		ttlMax = d.TTLDefault
	}
	if ttlMax < d.TTLDefault {
		return nil, fmt.Errorf("operator: ServiceDeps.TTLMax %s is below TTLDefault %s", ttlMax, d.TTLDefault)
	}
	oidcSessionTTL := d.OIDCSessionTTL
	if oidcSessionTTL < 0 {
		return nil, errors.New("operator: ServiceDeps.OIDCSessionTTL must not be negative")
	}
	if oidcSessionTTL == 0 {
		oidcSessionTTL = d.TTLDefault
	}
	return &Service{
		pool:           d.Pool,
		issuer:         d.Issuer,
		rbac:           d.RBAC,
		ttlDefault:     d.TTLDefault,
		ttlMax:         ttlMax,
		oidcSessionTTL: oidcSessionTTL,
		logger:         d.Logger,
	}, nil
}

// TTLMax is the longest lifetime [Service.Create] and [Service.IssueToken] accept.
func (s *Service) TTLMax() time.Duration { return s.ttlMax }

// ParseTTL reads a requested lifetime as the API carries it, in the same
// `duration` convention as keeper.yml: a Go duration ("2160h") or whole days
// ("90d"); "" → 0, which means `auth.jwt.ttl_default`. The range is checked by
// the service, against the configured ceiling.
func ParseTTL(raw string) (time.Duration, error) {
	if raw == "" {
		return 0, nil
	}
	d, err := config.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%w: %q is not a duration (e.g. \"2160h\" or \"90d\")", ErrInvalidTTL, raw)
	}
	if d <= 0 {
		return 0, fmt.Errorf("%w: %q must be positive", ErrInvalidTTL, raw)
	}
	return d, nil
}

// resolveTTL turns a requested lifetime into the one to mint: 0 → the default,
// otherwise within [MinJWTTTL, ttlMax].
func (s *Service) resolveTTL(requested time.Duration) (time.Duration, error) {
	switch {
	case requested == 0:
		return s.ttlDefault, nil
	case requested < MinJWTTTL:
		return 0, fmt.Errorf("%w: %s is below the minimum %s", ErrInvalidTTL, requested, MinJWTTTL)
	case requested > s.ttlMax:
		return 0, fmt.Errorf("%w: %s is above auth.jwt.ttl_max %s", ErrInvalidTTL, requested, s.ttlMax)
	}
	return requested, nil
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

// SetPasswordPolicy turns local passwords on with policy p, or off with nil
// (`auth.local` absent). Called from `keeper run` before serving.
func (s *Service) SetPasswordPolicy(p *PasswordPolicy) {
	if p == nil {
		s.passwords.Store(nil)
		return
	}
	cp := *p
	s.passwords.Store(&cp)
}

func (s *Service) passwordPolicy() (PasswordPolicy, error) {
	p := s.passwords.Load()
	if p == nil {
		return PasswordPolicy{}, ErrLocalPasswordsDisabled
	}
	return *p, nil
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
	// TTL of the token returned with the new operator; 0 → `auth.jwt.ttl_default`.
	TTL    time.Duration
	Label  string
	Client ClientMeta
	// AuthMethod — "" or [AuthMethodJWT]: the operator gets a token in the
	// reply. [AuthMethodLocal]: it gets Password instead and no token, and logs
	// in by password (NIM-1051).
	AuthMethod AuthMethod
	Password   string
	// Owner — the owner of the new operator, only for [AuthMethodJWT]
	// (NIM-1057); zero → none.
	Owner Owner
}

// CreateResult — result of Create. JWT and ExpiresAt — issued token returned
// to caller exactly once (no re-issue path without explicit
// issue-token call). A local operator gets none: JWT, JWTID and ExpiresAt are
// zero.
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
	Owner        Owner
	JWT          string
	JWTID        string
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
	method := in.AuthMethod
	if method == "" {
		method = AuthMethodJWT
	}
	// The token's terms — or, for a local operator, the password — are checked
	// before the INSERT: a bad one must refuse the whole call, not leave an
	// operator behind that cannot log in.
	var (
		ttl  time.Duration
		hash string
		err  error
	)
	switch method {
	case AuthMethodJWT:
		if in.Password != "" {
			return nil, fmt.Errorf("%w: a password is set only for auth_method %q", ErrNotLocalOperator, AuthMethodLocal)
		}
		if ttl, err = s.resolveTTL(in.TTL); err != nil {
			return nil, err
		}
		if !ValidJWTLabel(in.Label) {
			return nil, ErrInvalidJWTLabel
		}
	case AuthMethodLocal:
		policy, perr := s.passwordPolicy()
		if perr != nil {
			return nil, perr
		}
		if in.TTL != 0 || in.Label != "" {
			return nil, ErrLocalOperatorNoToken
		}
		if err := policy.Check(in.AID, in.Password); err != nil {
			return nil, err
		}
		if hash, err = hashPassword(ctx, in.Password); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("operator: invalid auth_method %q for create (want %q or %q)", method, AuthMethodJWT, AuthMethodLocal)
	}
	if !in.Owner.IsZero() && method != AuthMethodJWT {
		return nil, ErrOwnerNotAllowed
	}

	displayName := in.DisplayName
	if displayName == "" {
		displayName = in.AID
	}

	creator := in.CallerAID
	op := &Operator{
		AID:          in.AID,
		DisplayName:  displayName,
		AuthMethod:   method,
		CreatedByAID: &creator,
	}
	op.OwnerAID, op.OwnerSynod = in.Owner.columns()

	grantedRoles, err := s.insertOperator(ctx, op, in.Roles, hash)
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

	if method == AuthMethodLocal {
		return s.createdResult(ctx, op, creator, grantedRoles, MintResult{}), nil
	}

	// JWT issued AFTER tx commit — RolesOf sees fresh membership
	// (with TTL-poll delay, but invalidate above accelerates propagation).
	// Take current snapshot: with atomic create+grant local enforcer
	// of first Keeper already picks up update on next reload (best-
	// effort; token carries roles only for information anyway, authz
	// checked against fresh DB snapshot on each request — ADR-028).
	roles := s.rbac.RolesOf(in.AID)
	minted, err := MintJWT(ctx, s.pool, s.issuer, MintInput{
		AID:          in.AID,
		Roles:        roles,
		TTL:          ttl,
		Kind:         JWTKindIssued,
		Label:        in.Label,
		CreatedByAID: in.CallerAID,
		Client:       in.Client,
	})
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
	return s.createdResult(ctx, op, creator, grantedRoles, minted), nil
}

// createdResult reads the new row back for its created_at (DEFAULT NOW(), not
// local "now"). A failed read does not fail Create — the insert, and the token
// if any, already succeeded — and falls back to local time.
func (s *Service) createdResult(ctx context.Context, op *Operator, creator string, grantedRoles []string, minted MintResult) *CreateResult {
	saved, err := SelectByAID(ctx, s.pool, op.AID)
	if err != nil {
		if s.logger != nil {
			s.logger.Warn("operator.Create: post-insert SelectByAID failed",
				slog.String("aid", op.AID),
				slog.String("by_aid", creator),
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
		Owner:        OwnerOf(saved),
		JWT:          minted.JWT,
		JWTID:        minted.JWTID,
		ExpiresAt:    minted.ExpiresAt,
	}
}

// insertOperator — internal path of Create. With no roles, no password hash
// and no owner — fast-path without tx (Insert via s.pool, zero regression risk
// for existing calls without Roles). Otherwise — atomic tx: BeginTx → owner
// check → Insert → password hash → rbac.GrantOperator x N → Commit; any error
// → rollback, operator not created.
//
// Returns actually-granted roles (input order). Sentinel errors
// (ErrOperatorAlreadyExists / ErrInvalidOwner / rbac.ErrRoleNotFound /
// rbac.ErrOperatorNotFound) passed as-is for mapping in transport layer.
func (s *Service) insertOperator(ctx context.Context, op *Operator, roles []string, passwordHash string) ([]string, error) {
	owner := OwnerOf(op)
	if len(roles) == 0 && passwordHash == "" && owner.IsZero() {
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

	if !owner.IsZero() {
		if err := validateOwner(ctx, tx, op.AID, owner); err != nil {
			return nil, err
		}
	}
	if err := Insert(ctx, tx, op); err != nil {
		return nil, err
	}
	if passwordHash != "" {
		if err := storePasswordHash(ctx, tx, op.AID, passwordHash, *op.CreatedByAID); err != nil {
			return nil, err
		}
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
	if len(roles) == 0 {
		return nil, nil
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
	// TTL — 0 → `auth.jwt.ttl_default`; otherwise within [MinJWTTTL, `auth.jwt.ttl_max`].
	TTL    time.Duration
	Label  string
	Client ClientMeta
}

// IssueTokenResult — issued token.
type IssueTokenResult struct {
	AID       string
	JWT       string
	JWTID     string
	TTL       time.Duration
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
	ttl, err := s.resolveTTL(in.TTL)
	if err != nil {
		return nil, err
	}
	if !ValidJWTLabel(in.Label) {
		return nil, ErrInvalidJWTLabel
	}
	op, err := SelectByAID(ctx, s.pool, in.AID)
	if err != nil {
		return nil, err
	}
	if op.IsRevoked() {
		return nil, ErrOperatorAlreadyRevoked
	}
	roles := s.rbac.RolesOf(in.AID)
	minted, err := MintJWT(ctx, s.pool, s.issuer, MintInput{
		AID:          in.AID,
		Roles:        roles,
		TTL:          ttl,
		Kind:         JWTKindIssued,
		Label:        in.Label,
		CreatedByAID: in.CallerAID,
		Client:       in.Client,
	})
	if err != nil {
		return nil, fmt.Errorf("operator: issue JWT failed: %w", err)
	}
	return &IssueTokenResult{
		AID:       in.AID,
		JWT:       minted.JWT,
		JWTID:     minted.JWTID,
		TTL:       ttl,
		ExpiresAt: minted.ExpiresAt,
	}, nil
}

// ListJWTs returns a page of the operator's accounted tokens under f, newest
// first. [ErrOperatorNotFound] for an unknown AID, so an empty list always
// means an operator with no tokens (under f).
func (s *Service) ListJWTs(ctx context.Context, aid string, f JWTFilter, offset, limit int) ([]JWTRecord, int, error) {
	if !ValidAID(aid) {
		return nil, 0, fmt.Errorf("operator: invalid AID %q (must match %s)", aid, AIDPattern)
	}
	if _, err := SelectByAID(ctx, s.pool, aid); err != nil {
		return nil, 0, err
	}
	return ListJWTs(ctx, s.pool, aid, f, offset, limit)
}

// personalTTLMax is the longest personal token op may ask for. An OIDC
// operator's roles are reconciled from its groups only at sign-in, so its token
// must not outlive a sign-in: a person removed from a group at the IdP keeps
// the roles of the last sign-in for as long as the token lives (ADR-014
// amendment NIM-1052, lifted by NIM-1054). LDAP gets the general ceiling on the
// strength of the background group sync (NIM-1053); without that sync an LDAP
// operator would be in the OIDC operator's position.
func (s *Service) personalTTLMax(m AuthMethod) time.Duration {
	if m == AuthMethodOIDC && s.oidcSessionTTL < s.ttlMax {
		return s.oidcSessionTTL
	}
	return s.ttlMax
}

// resolvePersonalTTL is [Service.resolveTTL] under the operator's own ceiling.
// signInLeft > 0 is the time left in the OIDC sign-in the request is made
// with: the token is cut to expire with it, so a token minted late in a
// sign-in does not carry that sign-in's roles past the next one.
func (s *Service) resolvePersonalTTL(requested time.Duration, m AuthMethod, signInLeft time.Duration) (time.Duration, error) {
	ceiling := s.personalTTLMax(m)
	ttl := requested
	switch {
	case requested == 0:
		ttl = min(s.ttlDefault, ceiling)
	case requested < MinJWTTTL:
		return 0, fmt.Errorf("%w: %s is below the minimum %s", ErrInvalidTTL, requested, MinJWTTTL)
	case requested > ceiling && ceiling < s.ttlMax:
		return 0, fmt.Errorf("%w: %s is above %s, the lifetime of an OIDC sign-in: OIDC group membership is re-checked only at sign-in", ErrInvalidTTL, requested, ceiling)
	case requested > ceiling:
		return 0, fmt.Errorf("%w: %s is above auth.jwt.ttl_max %s", ErrInvalidTTL, requested, ceiling)
	}
	if signInLeft > 0 {
		// The mint takes its own `now` a few milliseconds after signInLeft was
		// measured; the second off the end keeps the token inside the sign-in.
		left := (signInLeft - time.Second).Truncate(time.Second)
		if left < MinJWTTTL {
			return 0, fmt.Errorf("%w: your OIDC sign-in ends too soon for a token of at least %s; sign in again", ErrInvalidTTL, MinJWTTTL)
		}
		ttl = min(ttl, left)
	}
	return ttl, nil
}

// IssuePersonalJWTInput — parameters for [Service.IssuePersonalJWT]. AID is the
// caller and the owner; CallerJWTID is the `jti` of the token the request is
// made with ("" for a token minted before migration 127).
type IssuePersonalJWTInput struct {
	AID         string
	CallerJWTID string
	// TTL — 0 → `auth.jwt.ttl_default`, capped at the operator's ceiling.
	TTL    time.Duration
	Label  string
	Client ClientMeta
}

// operatorTokensLockClass is the first key of the advisory lock below; the
// second is hashtext(aid). The two-key form shares no key space with the
// one-key locks of keeper init and the migrator.
const operatorTokensLockClass int32 = 0x4f505457

const lockOperatorTokensSQL = `SELECT pg_advisory_xact_lock($1, hashtext($2))`

// lockOperatorTokens serializes, for one AID, the password replacements and
// personal token issues of it, and the changes of its owner against the
// owner's issues and revokes of its tokens (NIM-1057) — and the same for any
// AID whose hashtext collides, an extra wait, nothing more. A personal token
// being minted from a session a replacement revokes either commits first — and
// the revocation sees it — or waits and then finds its session revoked. Not the operators
// row: the replacement writes rows whose foreign keys share-lock the acting
// administrator's row, and an operator revoke holds the administrators' rows
// FOR UPDATE while it waits for the target's — a lock on the target's row here
// would close that cycle (NIM-1051).
func lockOperatorTokens(ctx context.Context, db ExecQueryRower, aid string) error {
	if _, err := db.Exec(ctx, lockOperatorTokensSQL, operatorTokensLockClass, aid); err != nil {
		return fmt.Errorf("operator: lock operator tokens: %w", err)
	}
	return nil
}

const lockOperatorForPersonalJWTSQL = `SELECT auth_method, revoked_at IS NOT NULL FROM operators WHERE aid = $1 FOR NO KEY UPDATE`

// IssuePersonalJWT mints a token the operator issues to itself (kind
// `personal`). It needs no permission: the token's rights are the owner's at
// each request, read from the RBAC snapshot like every token's, so it can do
// nothing its owner cannot do at that moment. What it adds is a lifetime, and
// that is what is bounded here — the ceiling ([Service.resolvePersonalTTL]),
// the counts ([MaxActivePersonalJWTs], [MaxPersonalJWTsPerDay]) and the
// credential that may ask ([ErrPersonalJWTFromPersonal]).
//
// The operator row is locked for the counts and the insert, so two concurrent
// requests cannot both take the last free slot. Before it, [lockOperatorTokens]
// waits out a password replacement in flight, so the caller's token is read
// after that replacement has revoked it.
func (s *Service) IssuePersonalJWT(ctx context.Context, in IssuePersonalJWTInput) (*IssueTokenResult, error) {
	if !ValidAID(in.AID) {
		return nil, fmt.Errorf("operator: invalid AID %q (must match %s)", in.AID, AIDPattern)
	}
	if !ValidJWTLabel(in.Label) {
		return nil, ErrInvalidJWTLabel
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("operator: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockOperatorTokens(ctx, tx, in.AID); err != nil {
		return nil, err
	}

	var (
		method  string
		revoked bool
	)
	switch err := tx.QueryRow(ctx, lockOperatorForPersonalJWTSQL, in.AID).Scan(&method, &revoked); {
	case errors.Is(err, pgx.ErrNoRows):
		return nil, ErrOperatorNotFound
	case err != nil:
		return nil, fmt.Errorf("operator: lock operator: %w", err)
	case revoked:
		return nil, ErrOperatorAlreadyRevoked
	}

	signInLeft, err := s.callerSignInLeft(ctx, tx, in.AID, in.CallerJWTID, AuthMethod(method))
	if err != nil {
		return nil, err
	}

	ttl, err := s.resolvePersonalTTL(in.TTL, AuthMethod(method), signInLeft)
	if err != nil {
		return nil, err
	}

	counts, err := CountPersonalJWTs(ctx, tx, in.AID)
	if err != nil {
		return nil, err
	}
	// The daily limit is reported first: revoking a token does not lift it.
	if counts.LastDay >= MaxPersonalJWTsPerDay {
		return nil, ErrPersonalJWTDailyLimit
	}
	if counts.Active >= MaxActivePersonalJWTs {
		return nil, ErrPersonalJWTLimit
	}

	minted, err := MintJWT(ctx, tx, s.issuer, MintInput{
		AID:          in.AID,
		Roles:        s.rbac.RolesOf(in.AID),
		TTL:          ttl,
		Kind:         JWTKindPersonal,
		Label:        in.Label,
		CreatedByAID: in.AID,
		Client:       in.Client,
	})
	if err != nil {
		return nil, fmt.Errorf("operator: issue personal JWT failed: %w", err)
	}
	// The token was signed inside the transaction; a failed commit drops its
	// row, so it is discarded here and never leaves the process unaccounted.
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("operator: commit tx: %w", err)
	}
	return &IssueTokenResult{
		AID:       in.AID,
		JWT:       minted.JWT,
		JWTID:     minted.JWTID,
		TTL:       ttl,
		ExpiresAt: minted.ExpiresAt,
	}, nil
}

// checkCallerToken refuses to mint from the token a request is made with when
// it is a personal token or a `jti` that is not a row of aid
// ([ErrPersonalJWTFromPersonal]) or is revoked in the database
// ([ErrCallerJWTRevoked]), and returns its kind and expiry. An empty `jti` is a
// token from before migration 127, which is neither: kind "" and no error.
func checkCallerToken(ctx context.Context, db ExecQueryRower, aid, callerJWTID string) (JWTKind, time.Time, error) {
	if callerJWTID == "" {
		return "", time.Time{}, nil
	}
	kind, expiresAt, revoked, found, err := LookupJWT(ctx, db, aid, callerJWTID)
	if err != nil {
		return "", time.Time{}, err
	}
	if !found || kind == JWTKindPersonal {
		return "", time.Time{}, ErrPersonalJWTFromPersonal
	}
	// A password reset may have revoked the session the request came with a
	// moment ago, before this node's snapshot heard of it: a token minted from
	// it would outlive the reset (NIM-1051).
	if revoked {
		return "", time.Time{}, ErrCallerJWTRevoked
	}
	return kind, expiresAt, nil
}

// callerSignInLeft checks the token the request is made with
// ([checkCallerToken]) and returns the time left in it when it is the OIDC
// sign-in a personal token must not outlive (0 otherwise).
func (s *Service) callerSignInLeft(ctx context.Context, db ExecQueryRower, aid, callerJWTID string, m AuthMethod) (time.Duration, error) {
	kind, expiresAt, err := checkCallerToken(ctx, db, aid, callerJWTID)
	if err != nil || kind == "" {
		return 0, err
	}
	if m != AuthMethodOIDC || kind != JWTKindSession {
		return 0, nil
	}
	left := time.Until(expiresAt)
	if left <= 0 {
		// Expired since it was verified; anything below MinJWTTTL is refused.
		left = time.Nanosecond
	}
	return left, nil
}

// PersonalJWTTerms — what [Service.IssuePersonalJWT] would allow aid right now.
// TTLMax is the longest ttl it accepts; ExpiresBy, set for a request made with
// an OIDC sign-in, is when a token issued now expires at the latest.
type PersonalJWTTerms struct {
	TTLMax     time.Duration
	ExpiresBy  *time.Time
	Active     int
	Limit      int
	LastDay    int
	DailyLimit int
}

// PersonalJWTTerms reads aid's ceiling and its personal-token counts, so a
// client can offer only what will be accepted. callerJWTID is the request's
// `jti`.
func (s *Service) PersonalJWTTerms(ctx context.Context, aid, callerJWTID string) (PersonalJWTTerms, error) {
	if !ValidAID(aid) {
		return PersonalJWTTerms{}, fmt.Errorf("operator: invalid AID %q (must match %s)", aid, AIDPattern)
	}
	op, err := SelectByAID(ctx, s.pool, aid)
	if err != nil {
		return PersonalJWTTerms{}, err
	}
	counts, err := CountPersonalJWTs(ctx, s.pool, aid)
	if err != nil {
		return PersonalJWTTerms{}, err
	}
	terms := PersonalJWTTerms{
		TTLMax:     s.personalTTLMax(op.AuthMethod),
		Active:     counts.Active,
		Limit:      MaxActivePersonalJWTs,
		LastDay:    counts.LastDay,
		DailyLimit: MaxPersonalJWTsPerDay,
	}
	// A refusal of the caller's own token is IssuePersonalJWT's to report; the
	// terms only lack the cut-off then.
	left, err := s.callerSignInLeft(ctx, s.pool, aid, callerJWTID, op.AuthMethod)
	switch {
	case errors.Is(err, ErrPersonalJWTFromPersonal), errors.Is(err, ErrCallerJWTRevoked):
	case err != nil:
		return PersonalJWTTerms{}, err
	case left > 0:
		by := time.Now().Add(left - time.Second).UTC().Truncate(time.Second)
		terms.ExpiresBy = &by
	}
	return terms, nil
}

// RevokeJWTInput — parameters for RevokeJWT.
type RevokeJWTInput struct {
	AID       string
	JWTID     string
	CallerAID string
}

// RevokeJWT revokes one token of an operator and invalidates the RBAC snapshot
// cluster-wide, so the token is refused on its next request on every node. The
// operator and its other tokens are untouched.
//
// Returns [ErrJWTNotFound] (also for a token of another operator) or
// [ErrJWTAlreadyRevoked].
func (s *Service) RevokeJWT(ctx context.Context, in RevokeJWTInput) (JWTRecord, error) {
	if !ValidAID(in.AID) {
		return JWTRecord{}, fmt.Errorf("operator: invalid AID %q (must match %s)", in.AID, AIDPattern)
	}
	if !ValidJWTID(in.JWTID) {
		return JWTRecord{}, ErrJWTNotFound
	}
	rec, err := RevokeJWT(ctx, s.pool, in.AID, in.JWTID, in.CallerAID)
	if err != nil {
		return JWTRecord{}, err
	}
	s.invalidate(ctx)
	return rec, nil
}

// RevokeSessions ends aid's web sessions — tokens of kind `session` that may
// still verify — on the operator's own request (ADR-058 amendment NIM-1050).
// keepJWTID, when non-empty, is the session to leave alive: "all but this
// one". Personal and issued tokens are untouched. Returns how many were
// revoked; any revocation invalidates the RBAC snapshot cluster-wide, so a
// revoked session and every Bearer exchanged from it are refused on their next
// request on every node.
func (s *Service) RevokeSessions(ctx context.Context, aid, keepJWTID string) (int64, error) {
	if !ValidAID(aid) {
		return 0, fmt.Errorf("operator: invalid AID %q (must match %s)", aid, AIDPattern)
	}
	n, err := RevokeTokensOfKinds(ctx, s.pool, aid, keepJWTID, aid, []JWTKind{JWTKindSession})
	if err != nil {
		return 0, err
	}
	if n > 0 {
		s.invalidate(ctx)
	}
	return n, nil
}

// SetPasswordInput — parameters for SetPassword (an administrator sets or
// resets another operator's local password).
type SetPasswordInput struct {
	AID       string
	Password  string
	CallerAID string
}

// SetPassword replaces a local operator's password and revokes all of its web
// sessions and personal tokens ([resetRevokesKinds]): whoever held the old
// password is logged out everywhere, a token it issued itself included.
//
// Returns [ErrLocalPasswordsDisabled], [ErrOwnPasswordViaAdmin],
// [ErrOperatorNotFound], [ErrOperatorAlreadyRevoked], [ErrNotLocalOperator] or
// [ErrWeakPassword].
func (s *Service) SetPassword(ctx context.Context, in SetPasswordInput) (revokedTokens int64, err error) {
	policy, err := s.passwordPolicy()
	if err != nil {
		return 0, err
	}
	if in.AID == in.CallerAID {
		return 0, ErrOwnPasswordViaAdmin
	}
	if !ValidAID(in.AID) {
		return 0, fmt.Errorf("operator: invalid AID %q (must match %s)", in.AID, AIDPattern)
	}
	op, err := SelectByAID(ctx, s.pool, in.AID)
	if err != nil {
		return 0, err
	}
	if op.IsRevoked() {
		return 0, ErrOperatorAlreadyRevoked
	}
	if op.AuthMethod != AuthMethodLocal {
		return 0, ErrNotLocalOperator
	}
	if err := policy.Check(in.AID, in.Password); err != nil {
		return 0, err
	}
	return s.replacePassword(ctx, in.AID, in.Password, in.CallerAID, "", nil, resetRevokesKinds)
}

// ChangePasswordInput — parameters for ChangePassword (an operator changes its
// own password). CurrentJWTID is the token the request came with; its session
// survives the change.
type ChangePasswordInput struct {
	AID          string
	Current      string
	New          string
	CurrentJWTID string
}

// ChangePassword replaces the caller's own password after checking the
// current one, and revokes the caller's other web sessions. The write is bound
// to the password that was checked: a reset committed between the check and
// the write refuses the change rather than being overwritten by it.
//
// Returns [ErrLocalPasswordsDisabled], [ErrNotLocalOperator],
// [ErrPasswordMismatch] (the current password is wrong, or no longer current),
// [ErrOperatorAlreadyRevoked] or [ErrWeakPassword].
func (s *Service) ChangePassword(ctx context.Context, in ChangePasswordInput) (revokedTokens int64, err error) {
	policy, err := s.passwordPolicy()
	if err != nil {
		return 0, err
	}
	check, err := VerifyLocalPassword(ctx, s.pool, in.AID, in.Current)
	switch {
	case errors.Is(err, ErrOperatorNotFound):
		return 0, ErrNotLocalOperator
	case err != nil:
		return 0, err
	}
	if err := policy.Check(in.AID, in.New); err != nil {
		return 0, err
	}
	return s.replacePassword(ctx, in.AID, in.New, in.AID, in.CurrentJWTID, &check, changeRevokesKinds)
}

// replacePassword takes [lockOperatorTokens], writes the new hash and revokes
// aid's tokens of kinds. With check non-nil the old hash is re-read FOR UPDATE
// first and must still be the one that verified.
func (s *Service) replacePassword(ctx context.Context, aid, password, byAID, keepJWTID string, check *PasswordCheck, kinds []JWTKind) (int64, error) {
	hash, err := hashPassword(ctx, password)
	if err != nil {
		return 0, err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return 0, fmt.Errorf("operator: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockOperatorTokens(ctx, tx, aid); err != nil {
		return 0, err
	}
	if check != nil {
		current, err := stillCurrent(ctx, tx, *check, "UPDATE")
		if err != nil {
			return 0, err
		}
		if !current {
			return 0, ErrPasswordMismatch
		}
	}
	// The upsert must come before the revocation: it takes the row lock a
	// password login's mint holds FOR SHARE, so a mint in progress commits its
	// session first and the UPDATE below — a later statement, a fresh snapshot —
	// sees and revokes it. Revoking first would miss that uncommitted row.
	if err := storePasswordHash(ctx, tx, aid, hash, byAID); err != nil {
		return 0, err
	}
	n, err := RevokeTokensOfKinds(ctx, tx, aid, keepJWTID, byAID, kinds)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("operator: commit tx: %w", err)
	}
	if n > 0 {
		s.invalidate(ctx)
	}
	return n, nil
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
