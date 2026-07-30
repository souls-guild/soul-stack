package rbac

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"

	"github.com/jackc/pgx/v5"
)

// ErrInvalidRoleName is returned when a role name fails [reRoleName]. A
// validation-error sentinel (separate from ErrRoleAlreadyExists /
// ErrRoleNotFound): transport maps it to 422, not 409/404. A specific broken
// permission is returned as a wrapped ParsePermission error instead (also
// 422; no sentinel needed there — the message carries the diagnosis).
var ErrInvalidRoleName = errors.New("rbac: invalid role name")

// ServicePool is the narrow subset of pgxpool.Pool that [Service] needs: the
// [ExecQueryRower] transport surface plus BeginTx for atomic mutations under
// FOR UPDATE. The real `*pgxpool.Pool` satisfies it automatically.
//
// Mirrors [operator.ServicePool]; declared locally so rbac doesn't pull in
// operator (and vice versa — avoiding an import cycle, see
// [ErrWouldLockOutCluster]).
type ServicePool interface {
	ExecQueryRower
	BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error)
}

// Invalidator is the cluster-wide RBAC invalidation surface (ADR-028(d),
// B2). After a role mutation commits successfully, [Service] calls
// Invalidate so other Keeper nodes re-read the snapshot near-instantly
// (instead of waiting for the TTL poll). Implemented in `keeper run` by an
// adapter over [keeperredis.PublishRBACInvalidate]; in single-Keeper/dev mode
// (no Redis) no invalidator is attached — only the TTL poll runs.
//
// Invalidate is best-effort: it does NOT return a publish error (the
// mutation is already committed to the DB); the implementation logs and
// swallows it.
type Invalidator interface {
	Invalidate(ctx context.Context)
}

// ServiceDeps holds [Service]'s dependencies. All fields are immutable after
// construction.
type ServiceDeps struct {
	Pool   ServicePool
	Logger *slog.Logger
}

// Service holds the RBAC CRUD business logic (roles / permissions /
// membership) behind the role.* permissions (ADR-028(e)). The single source
// of truth for the future transport facade (OpenAPI/MCP — Slice 2);
// invariants (the builtin boundary, self-lockout) live here, transport only
// decodes input / encodes output.
//
// Safe for concurrent use: deps are immutable and no state is held;
// mutation atomicity comes from transactions plus FOR UPDATE.
type Service struct {
	pool   ServicePool
	logger *slog.Logger

	// inv is the optional cluster-wide invalidator (B2). Late-bound via
	// [Service.SetInvalidator]: the Redis client in `keeper run` comes up
	// AFTER NewService, so injection is deferred (the same pattern as
	// store.SetAuditWriter / vc.SetMetrics in main.go). atomic.Pointer
	// handles concurrent writes from the setter vs. reads from mutations
	// without a separate mutex.
	inv atomic.Pointer[Invalidator]
}

// NewService assembles the service. Pool is required.
func NewService(d ServiceDeps) (*Service, error) {
	if d.Pool == nil {
		return nil, errors.New("rbac: ServiceDeps.Pool is nil")
	}
	return &Service{pool: d.Pool, logger: d.Logger}, nil
}

// SetInvalidator late-binds the cluster-wide invalidator (B2). Called from
// `keeper run` after the Redis client comes up. nil removes the invalidator
// (falling back to a pure TTL poll). Idempotent, thread-safe.
func (s *Service) SetInvalidator(inv Invalidator) {
	if inv == nil {
		s.inv.Store(nil)
		return
	}
	s.inv.Store(&inv)
}

// invalidate sends the cluster-wide invalidate signal after a role mutation
// commits successfully (B2). No-op when no invalidator is attached
// (single-Keeper/dev). Best-effort: the Invalidate implementation logs and
// swallows any publish error itself — the mutation is already committed, and
// a lost signal is covered by the TTL poll.
func (s *Service) invalidate(ctx context.Context) {
	if p := s.inv.Load(); p != nil {
		(*p).Invalidate(ctx)
	}
}

// CreateRoleInput holds the parameters for CreateRole.
type CreateRoleInput struct {
	Name        string
	Description string
	Permissions []string
	CallerAID   string
	// DefaultScope is the role's default_scope (ADR-047 S1), inherited by the
	// role's permissions that don't have their own selector. nil means the
	// role has no scope restriction (backcompat).
	//
	// On a DERIVED role (ParentRole set) it is the attenuating DELTA instead:
	// effective = the parent's effective scope AND this (ADR-078(b)). Write only
	// the ADDED narrowing — restating the parent's own predicate resolves to the
	// empty set the moment the parent moves.
	DefaultScope *string

	// ParentRole names the role this one derives from (ADR-078); nil creates a
	// plain role, which is every role that existed before derivation. A derived
	// role is bounded by its parent both structurally (`child ⊆ parent`) and by
	// least-privilege (the caller must hold the parent) — see
	// [Service.resolveParentCeiling] and [assertWithinParent].
	ParentRole *string

	// ScopeMode records what DefaultScope means on a derived role (ADR-078(k)):
	// [ScopeModeTrack] (the default, and the empty value here) leaves it a delta
	// that follows the parent; [ScopeModePin] materializes the parent's current
	// effective scope into it, so a later widening of the parent stops at this
	// role. Ignored — and stored as NULL — when ParentRole is nil.
	ScopeMode ScopeMode
}

// CreateRole creates a role along with its permissions. Validating the name
// plus EVERY permission via [ParsePermission] happens BEFORE the tx opens
// (bad input shouldn't hold a transaction open).
//
// Returns:
//   - [ErrInvalidRoleName] — name doesn't match the format (422).
//   - a wrapped ParsePermission error — a broken permission (422).
//   - [ErrRoleAlreadyExists] — name already taken (409).
//   - a wrapped FK violation — CallerAID doesn't exist in operators
//     (unlikely: middleware guarantees a valid caller, but the FK is a
//     backstop).
func (s *Service) CreateRole(ctx context.Context, in CreateRoleInput) error {
	if !reRoleName.MatchString(in.Name) {
		return fmt.Errorf("%w: %q must match %s", ErrInvalidRoleName, in.Name, reRoleName.String())
	}
	for _, raw := range in.Permissions {
		if _, err := ParsePermission(raw); err != nil {
			return fmt.Errorf("rbac: invalid permission %q: %w", raw, err)
		}
	}
	if in.DefaultScope != nil {
		if _, err := ParseDefaultScope(*in.DefaultScope); err != nil {
			return fmt.Errorf("rbac: invalid default_scope %q: %w", *in.DefaultScope, err)
		}
	}
	if in.ParentRole != nil && !reRoleName.MatchString(*in.ParentRole) {
		return fmt.Errorf("%w: parent %q must match %s", ErrInvalidRoleName, *in.ParentRole, reRoleName.String())
	}
	if err := checkScopeMode(in.ScopeMode, in.ParentRole != nil); err != nil {
		return err
	}

	var createdBy *string
	if in.CallerAID != "" {
		createdBy = &in.CallerAID
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("rbac: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Derived role (ADR-078): the structural ceiling, and the floor below applied
	// to the RESOLVED role rather than its stored rows — `child ⊆ parent AND
	// caller-holds-parent`, both on top of least-privilege, never instead of it.
	// The parent resolves first because the floor needs its ceiling (NIM-198).
	var parent *Role
	scope := in.DefaultScope
	if in.ParentRole != nil {
		parent, err = s.resolveParentCeiling(ctx, tx, in.Name, *in.ParentRole, in.CallerAID)
		if err != nil {
			return err
		}
		// Pinning happens BEFORE the gates, because the materialized predicate is
		// what will be stored and therefore what they must judge. It resolves the
		// same either way — `ceiling AND (ceiling AND delta)` is `ceiling AND delta`
		// — so this changes what is written, never what is allowed.
		if in.ScopeMode == ScopeModePin {
			if scope, err = pinnedDelta(parent, scope); err != nil {
				return err
			}
		}
		if _, err := assertWithinParent(parent, in.Name, in.Permissions, scope); err != nil {
			return err
		}
	}

	// Least-privilege subset check (ADR-028, rbac.md → § Least-Privilege
	// Invariant): a caller can't create a role with a permission it doesn't
	// itself hold. Guards against vertical escalation (role.create without
	// `*` → a role with `*` → grant it to self → cluster-admin). Granted
	// bare perms are expanded under the role's own default_scope being
	// created (ADR-047 S1), otherwise a caller scoped to prod could grant a
	// role scoped to staging — and on a derived role under the parent's ceiling
	// too, so the floor judges the rights the role confers (NIM-198).
	required, err := effectiveRoleRights(parent, in.Permissions, scope)
	if err != nil {
		return err
	}
	if err := s.assertCallerMayGrant(ctx, tx, in.CallerAID, required); err != nil {
		return err
	}

	// No parent: the role tracks nothing, so it needs `role.create-root` (NIM-201,
	// root_role.go). Checked AFTER the floor — "you cannot grant that at all" is
	// the more actionable refusal of the two. The derived branch was settled above,
	// where the parent had to resolve before the floor could read the child in the
	// form it will grant (NIM-198).
	if in.ParentRole == nil {
		if err := s.assertCallerMayMintRootRole(ctx, tx, in.CallerAID, required); err != nil {
			return err
		}
	}

	if err := CreateRole(ctx, tx, in.Name, in.Description, in.Permissions, createdBy, scope); err != nil {
		return err
	}
	// After the INSERT, so the chain-guard trigger sees the row it is judging.
	// NO self-lockout check: creating a role only ADDS to the catalog, and a new
	// derived role that grants nothing to nobody cannot remove an admin. NO cascade
	// report either: a role created this instant has no children to surprise.
	if in.ParentRole != nil {
		if err := UpdateRoleParent(ctx, tx, in.Name, in.ParentRole, in.ScopeMode); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("rbac: commit tx: %w", err)
	}
	s.invalidate(ctx)
	return nil
}

// DeleteRole deletes a role (cascading to its permissions and membership).
//
// Order of checks inside the tx (a deterministic lock order against
// deadlock — R2: role → permissions → membership/operators):
//  1. lock the role row (SELECT … FOR UPDATE); missing → [ErrRoleNotFound].
//  2. builtin=true → [ErrRoleBuiltin] (FIRST — builtin takes priority).
//  3. if the role grants `*` — a self-lockout check: will active admins with
//     `*` remain through a role OTHER than the one being deleted; none →
//     [ErrWouldLockOutCluster]. Ahead of the caller gate because it is a
//     precondition rather than a permission check (NIM-319).
//  4. the caller must be able to administer the role — cover what it grants
//     (NIM-214); otherwise → [ErrPermissionNotHeld]. Before this, `role.delete`
//     had no caller-side check of any kind and dropped any non-builtin role.
//  5. DELETE.
func (s *Service) DeleteRole(ctx context.Context, name, callerAID string) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("rbac: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	builtin, err := lockRole(ctx, tx, name)
	if err != nil {
		return err
	}
	if builtin {
		return ErrRoleBuiltin
	}

	perms, err := rolePermissions(ctx, tx, name)
	if err != nil {
		return err
	}
	parent, err := roleParent(ctx, tx, name)
	if err != nil {
		return err
	}
	// Only a PLAIN `*` role is a cluster-admin source (ADR-078(i)) — a derived one
	// was never counted by the probes, so deleting it cannot lock anyone out and
	// checking would only produce a false 409.
	//
	// Ahead of the caller gate below, per the precedence argued at
	// [Service.assertNotLastWildcardRole] (NIM-319).
	if grantsClusterAdmin(perms, parent) {
		if err := s.assertNotLastWildcardRole(ctx, tx, name); err != nil {
			return err
		}
	}

	// Resolved against its chain, so a derived role is judged on what it GRANTS
	// rather than on its stored delta — the same currency every other coverage
	// question reads (NIM-198).
	target, err := resolveRoleChain(ctx, tx, name)
	if err != nil {
		return err
	}
	if err := s.assertCallerMayAdminister(ctx, tx, callerAID, target); err != nil {
		return err
	}

	// Deleting a role that is still someone's parent is refused by the self-FK
	// (ADR-078(g), [ErrRoleHasChildren]) — the orphan policy is fail-closed
	// RESTRICT, since every alternative re-shapes a child's ceiling unasked.
	if err := DeleteRole(ctx, tx, name); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("rbac: commit tx: %w", err)
	}
	s.invalidate(ctx)
	return nil
}

// UpdateRolePermissionsInput holds the parameters for UpdateRolePermissions.
type UpdateRolePermissionsInput struct {
	Name        string
	Permissions []string
	CallerAID   string

	// SetDefaultScope — when true, the role's default_scope is REPLACED with
	// DefaultScope (nil clears the scope). When false, default_scope is left
	// untouched (PATCH semantics: a caller that doesn't send the field
	// doesn't reset the role's scope).
	SetDefaultScope bool
	// DefaultScope is the new default_scope value when SetDefaultScope=true.
	DefaultScope *string

	// SetParentRole — when true, parent_role is REPLACED with ParentRole (nil
	// makes the role plain again). PATCH semantics, mirroring SetDefaultScope: a
	// caller that doesn't send the field doesn't re-root the role.
	SetParentRole bool
	// ParentRole is the new parent_role when SetParentRole=true (ADR-078).
	ParentRole *string

	// SetScopeMode / ScopeMode — the delta's intent (ADR-078(k)), PATCH-presence
	// again. Sending [ScopeModePin] RE-PINS: the parent's effective scope as of
	// now is materialized into the delta, which is the point of sending it a
	// second time. Not sending the field leaves both the mode and the delta alone.
	SetScopeMode bool
	ScopeMode    ScopeMode

	// ConfirmCascade — the caller has seen what this change does to the roles
	// derived from this one and means to do it (ADR-078(k), NIM-199). Without it a
	// mutation that moves a child's rights is refused with the report attached
	// ([CascadeError]); with it the same mutation proceeds. It confirms nothing
	// else: every other gate is unaffected by it.
	ConfirmCascade bool
}

// UpdateRolePermissions replaces a role's permission set (replace
// semantics).
//
// Order inside the tx:
//  1. lock the role; missing → [ErrRoleNotFound].
//  2. builtin=true → [ErrRoleBuiltin].
//  3. validate the new permission set, the new default_scope and the resulting
//     scope_mode.
//  4. if the role STOPS being a cluster-admin source — `*` removed, or the role
//     turned derived — a self-lockout check (will admins with `*` remain through
//     a PLAIN role other than this one); none → [ErrWouldLockOutCluster]. Ahead
//     of every caller gate: a precondition, not a permission check (NIM-319).
//  5. the caller must be able to administer the role as it stands (NIM-214).
//  6. if the resulting role is derived → the caller holds the parent
//     ([Service.resolveParentCeiling]) and `child ⊆ parent`
//     ([assertWithinParent]) — ADR-078(h).
//  7. least-privilege, over the role's rights in RESOLVED form (NIM-198), then
//     the root-role gate if the result tracks nothing (NIM-201).
//  8. the cascade report, last (ADR-078(k), NIM-199).
//  9. replace.
func (s *Service) UpdateRolePermissions(ctx context.Context, in UpdateRolePermissionsInput) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("rbac: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	builtin, err := lockRole(ctx, tx, in.Name)
	if err != nil {
		return err
	}
	if builtin {
		return ErrRoleBuiltin
	}

	for _, raw := range in.Permissions {
		if _, err := ParsePermission(raw); err != nil {
			return fmt.Errorf("rbac: invalid permission %q: %w", raw, err)
		}
	}
	if in.SetDefaultScope && in.DefaultScope != nil {
		if _, err := ParseDefaultScope(*in.DefaultScope); err != nil {
			return fmt.Errorf("rbac: invalid default_scope %q: %w", *in.DefaultScope, err)
		}
	}
	if in.SetParentRole && in.ParentRole != nil && !reRoleName.MatchString(*in.ParentRole) {
		return fmt.Errorf("%w: parent %q must match %s", ErrInvalidRoleName, *in.ParentRole, reRoleName.String())
	}

	// rolePermissions reads the old set without a separate lock on
	// rbac_role_permissions: the role is already locked by lockRole (FOR
	// UPDATE on rbac_roles) in this same tx, and a role's permissions can
	// only be changed through that role row — a concurrent mutation
	// serializes on the same row lock. A separate lock on the permission
	// rows would be redundant.
	oldPerms, err := rolePermissions(ctx, tx, in.Name)
	if err != nil {
		return err
	}

	oldScope, err := roleDefaultScope(ctx, tx, in.Name)
	if err != nil {
		return err
	}
	oldParent, err := roleParent(ctx, tx, in.Name)
	if err != nil {
		return err
	}
	oldMode, err := roleScopeMode(ctx, tx, in.Name)
	if err != nil {
		return err
	}
	// The role as it will look AFTER this PATCH — an untouched field keeps its
	// stored value. Every guard below judges that shape, not the request.
	newScope, newParent, newMode := oldScope, oldParent, oldMode
	rewriteScope := in.SetDefaultScope
	if in.SetDefaultScope {
		newScope = in.DefaultScope
	}
	if in.SetParentRole {
		newParent = in.ParentRole
	}
	if in.SetScopeMode {
		newMode = in.ScopeMode
	}
	if newParent == nil {
		newMode = ScopeModeNone
	} else if newMode == ScopeModeNone {
		newMode = ScopeModeTrack
	}
	if err := checkScopeMode(newMode, newParent != nil); err != nil {
		return err
	}

	// The self-lockout check is needed when the role STOPS being a cluster-admin
	// source. Two ways to stop, and the second one is why ADR-078(i) exists: `*`
	// is removed from the permission set, or the role becomes DERIVED — a derived
	// `*` is capped by its parent, so the probes no longer count it and the
	// cluster could be left with none.
	//
	// First among the guards, per the precedence argued at
	// [Service.assertNotLastWildcardRole] (NIM-319) — it needs nothing but the
	// stored rows and the parent this PATCH resolves to, so nothing forces it later.
	// A PATCH that would also be refused for another reason is refused by this one
	// instead; both are refusals, and this is the one the caller cannot lift.
	if grantsClusterAdmin(oldPerms, oldParent) && !grantsClusterAdmin(in.Permissions, newParent) {
		if err := s.assertNotLastWildcardRole(ctx, tx, in.Name); err != nil {
			return err
		}
	}

	// The role as STORED and RESOLVED before anything changes — the baseline the
	// cascade report is diffed against, read before the write path touches a row.
	before, err := resolveRoleChain(ctx, tx, in.Name)
	if err != nil {
		return err
	}

	// May the caller touch this role at all (NIM-214)? Asked FIRST, and about the
	// role's CURRENT form: "you may not administer this role" is more actionable
	// than any refusal about the change being made, the same ordering
	// [Service.resolveParentCeiling] uses for a parent that is out of reach.
	if err := s.assertCallerMayAdminister(ctx, tx, in.CallerAID, before); err != nil {
		return err
	}

	// Derived role (ADR-078): the resulting role must still fit inside its parent.
	// Re-checked on EVERY update, not just one that sets parent_role — adding a
	// permission or widening the delta of an existing derived role is the same
	// escalation attempt as creating it that way. Resolved BEFORE the floor, which
	// needs the ceiling to judge the rows in the form they will grant (NIM-198).
	var parent, after *Role
	if newParent != nil {
		parent, err = s.resolveParentCeiling(ctx, tx, in.Name, *newParent, in.CallerAID)
		if err != nil {
			return err
		}
		// Re-pin only on an explicit request. A pinned role whose permissions are
		// being edited must NOT quietly re-freeze onto today's parent: that would
		// make an unrelated PATCH an authorization change nobody asked for.
		if in.SetScopeMode && in.ScopeMode == ScopeModePin {
			if newScope, err = pinnedDelta(parent, newScope); err != nil {
				return err
			}
			rewriteScope = true
		}
		if after, err = assertWithinParent(parent, in.Name, in.Permissions, newScope); err != nil {
			return err
		}
	} else if after, err = plainRole(in.Name, in.Permissions, newScope); err != nil {
		return err
	}

	// What the role GRANTS on either side of this PATCH, both in effective form —
	// the currency every least-privilege comparison reads (NIM-198). `before` is
	// what it grants TODAY, so a row its parent stopped covering is absent from it
	// and correctly reads as new if this PATCH brings it back to life.
	wasRights := effectivePermissions(before.Permissions, before.DefaultScope)
	nowRights := effectivePermissions(after.Permissions, after.DefaultScope)

	// The floor's own set: what this PATCH NEWLY hands out. Measured on the rights,
	// never on which rows changed (NIM-130, then NIM-230).
	//
	// A row diff cannot see either thing that matters here. A PATCH that moves the
	// role's CEILING — clears parent_role, replaces default_scope, re-pins the
	// delta — widens every bare permission it leaves untouched, and reports an
	// empty delta while doing it. A PATCH that rewrites rows into a narrower form
	// reports additions that grant nothing new. NIM-130 patched the first of those
	// for `SetDefaultScope` alone, by gating the whole set whenever the scope
	// moved; parent_role kept escaping through the same hole until NIM-230, because
	// a fix shaped as a list of fields is only ever as complete as the list.
	//
	// So there is no list: the two resolved forms are compared, and whatever moved
	// the ceiling shows up. A pure trim, and a narrowing of any kind, resolve to
	// nothing new and stay ungated — removing rights has never been the escalation
	// this floor is about, and an operator allowed to delete a permission outright
	// must not be refused the smaller act of narrowing its scope.
	required := widenedRights(wasRights, nowRights)
	if err := s.assertCallerMayGrant(ctx, tx, in.CallerAID, required); err != nil {
		return err
	}

	// The result has no parent and this PATCH leaves privilege in it — the same
	// minting the create gate refuses (NIM-201). Judged on the RESULTING shape, so
	// it covers both clearing parent_role and growing a role that was already
	// plain.
	//
	// The set is what ends up UNTRACKED and was not untracked already. A role that
	// had a parent tracked everything it granted, so clearing the parent strands
	// the WHOLE resulting set — including when the rights come out numerically
	// unchanged, because what this gate is about is the tracking, and that is
	// exactly what such a PATCH removes. A role that was already plain strands only
	// what it gained, which is why a pure trim stays ungated.
	if newParent == nil {
		var wasUntracked []Permission
		if oldParent == nil {
			wasUntracked = wasRights
		}
		if err := s.assertCallerMayMintRootRole(ctx, tx, in.CallerAID, widenedRights(wasUntracked, nowRights)); err != nil {
			return err
		}
	}

	// The blast radius, LAST among the gates (ADR-078(k), NIM-199): every refusal
	// that is about the caller's own rights should be reported as such before one
	// that asks them to take a position. A change nobody below feels passes
	// silently — the report is about consequences, not about the graph.
	if err := s.assertCascadeConfirmed(ctx, tx, before, after, in.ConfirmCascade); err != nil {
		return err
	}

	if err := UpdateRolePermissions(ctx, tx, in.Name, in.Permissions); err != nil {
		return err
	}
	// newScope rather than in.DefaultScope: a re-pin rewrites the delta even when
	// the request carried no scope of its own.
	if rewriteScope {
		if err := UpdateRoleDefaultScope(ctx, tx, in.Name, newScope); err != nil {
			return err
		}
	}
	if in.SetParentRole || in.SetScopeMode {
		if err := UpdateRoleParent(ctx, tx, in.Name, newParent, newMode); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("rbac: commit tx: %w", err)
	}
	s.invalidate(ctx)
	return nil
}

// RevokeOperatorInput holds the parameters for RevokeOperator.
type RevokeOperatorInput struct {
	RoleName string
	AID      string
	// CallerAID is the operator taking the binding apart. Required (NIM-285):
	// unbinding is measured against the same rights `role.grant-operator` demands
	// to create the binding, so an absent caller is refused rather than trusted.
	CallerAID string
}

// RevokeOperator removes a membership row (RoleName, AID).
//
// builtin boundary: revoke-operator on the builtin cluster-admin role IS
// ALLOWED (otherwise you couldn't remove a mistakenly-assigned admin), but
// with the same self-lockout guard.
//
// Order inside the tx:
//  1. lock the membership row; missing → [ErrRoleOperatorNotFound].
//  2. if the role grants `*` AND the AID being removed holds `*` ONLY
//     through it — a self-lockout check: will active admins with `*` remain
//     after excluding the (RoleName, AID) pair; none →
//     [ErrWouldLockOutCluster]. Ahead of the caller gate because it is a
//     precondition rather than a permission check (NIM-319).
//  3. the caller must be able to unbind — cover what the role grants, the same
//     rights `role.grant-operator` demands to create the binding (NIM-285);
//     otherwise → [ErrPermissionNotHeld].
//  4. DELETE.
func (s *Service) RevokeOperator(ctx context.Context, in RevokeOperatorInput) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("rbac: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := lockRoleOperator(ctx, tx, in.RoleName, in.AID); err != nil {
		return err
	}

	// Lock the role (deterministic order: role → permissions → operators —
	// against deadlock R2) and read its permissions — we need to know
	// whether the role grants `*`.
	if _, err := lockRole(ctx, tx, in.RoleName); err != nil {
		// The role can't disappear after a successful lockRoleOperator (FK
		// plus the row lock on membership), but we guard anyway:
		// ErrRoleNotFound propagates as-is.
		return err
	}
	perms, err := rolePermissions(ctx, tx, in.RoleName)
	if err != nil {
		return err
	}
	parent, err := roleParent(ctx, tx, in.RoleName)
	if err != nil {
		return err
	}
	// Before the caller gate below, per the precedence argued at
	// [Service.assertNotLastWildcardRole] (NIM-319).
	if grantsClusterAdmin(perms, parent) {
		// Are we removing the last admin with `*`? The probe query, run
		// UNDER FOR UPDATE, excludes the target (RoleName, AID) pair: if the
		// AID also holds `*` via other roles, it stays in the result set and
		// lockout doesn't trigger. A DERIVED role is not a source at all
		// (ADR-078(i)), so revoking one never needs the probe.
		if err := s.assertNotLastWildcardOperator(ctx, tx, in.RoleName, in.AID); err != nil {
			return err
		}
	}

	// The rights `role.grant-operator` would have demanded to create this binding
	// (NIM-285) — unbinding is the same authorization surface in reverse.
	required, err := s.roleEffectivePermissions(ctx, tx, in.RoleName)
	if err != nil {
		return err
	}
	if err := s.assertCallerMayUnbind(ctx, tx, in.CallerAID, required); err != nil {
		return err
	}

	if err := RevokeOperator(ctx, tx, in.RoleName, in.AID); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("rbac: commit tx: %w", err)
	}
	s.invalidate(ctx)
	return nil
}

// GrantOperatorInput holds the parameters for GrantOperator. CallerAID is
// optional (nil → granted_by_aid IS NULL, for bootstrap membership;
// transport fills it in from the caller's claims).
type GrantOperatorInput struct {
	RoleName  string
	AID       string
	CallerAID *string
}

// GrantOperator binds an AID to a role — inserts a membership row
// (RoleName, AID) with granted_by_aid = CallerAID. A facade over the
// package-level [GrantOperator] (repository.go), mirroring
// [Service.RevokeOperator].
//
// Order inside the tx (a deterministic lock order against deadlock — R2:
// role → operators; the same as RevokeOperator):
//  1. lock the role row (SELECT … FOR UPDATE); missing → [ErrRoleNotFound].
//  2. INSERT the membership row; an FK violation on a nonexistent AID →
//     [ErrOperatorNotFound] (via [mapGrantError]).
//
// NO self-lockout check: a grant only adds membership (even for an admin)
// — expanding the admin set can never lock the cluster out. This is the
// only membership mutation without a lockout boundary (revoke/delete/update
// all require one).
//
// Idempotent: regranting the same pair is a no-op (ON CONFLICT DO NOTHING
// in insertRoleOperatorSQL).
func (s *Service) GrantOperator(ctx context.Context, in GrantOperatorInput) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("rbac: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Lock the role as an existence check: gives a clean ErrRoleNotFound
	// instead of an FK violation on role_name (indistinguishable from an FK
	// on aid in a single mapper).
	if _, err := lockRole(ctx, tx, in.RoleName); err != nil {
		return err
	}

	// Least-privilege subset check: can't grant a role that contains a
	// permission outside the caller's own set — otherwise a bypass (a
	// cluster-admin creates a powerful role, a sub-operator with
	// role.grant-operator binds it to itself/others and escalates). Checks
	// the permissions of the role BEING GRANTED.
	//
	// CallerAID == nil — a system/bootstrap grant (keeper init binds the
	// first Archon to cluster-admin inside its advisory-lock tx):
	// least-privilege doesn't apply here (there's no caller Archon as a
	// subject). A subject-initiated grant from transport always carries a
	// CallerAID (claims.Subject).
	if in.CallerAID != nil {
		// The rights the binding actually confers: bare perms under the role's
		// default_scope (ADR-047 S1), and on a DERIVED role the whole set resolved
		// against its chain (NIM-198) — binding a child confers what the child
		// grants, which is never more than its parent allows.
		required, err := s.roleEffectivePermissions(ctx, tx, in.RoleName)
		if err != nil {
			return err
		}
		if err := s.assertCallerMayGrant(ctx, tx, *in.CallerAID, required); err != nil {
			return err
		}
	}

	if _, err := tx.Exec(ctx, insertRoleOperatorSQL, in.RoleName, in.AID, grantedByArg(in.CallerAID)); err != nil {
		return mapGrantError(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("rbac: commit tx: %w", err)
	}
	s.invalidate(ctx)
	return nil
}

// grantedByArg converts a *string CallerAID into an args value for
// granted_by_aid: nil → nil (PG NULL), otherwise the dereferenced string.
func grantedByArg(callerAID *string) any {
	if callerAID == nil {
		return nil
	}
	return *callerAID
}

// ListRoles returns the API role catalog (name / description / builtin plus
// expanded permissions and operator AIDs) AS callerAID is entitled to see it.
// Read-only, no tx — assembled with three SELECTs, no N+1 ([LoadRoleViews],
// mirrors [LoadSnapshot]). This is an API view, not an enforcer snapshot: it
// carries description/builtin for role.list.
//
// The catalog is filtered to the roles the caller could grant (NIM-202,
// role_visibility.go): `role.list` is the right to read the catalog, not the
// right to read the cluster's whole privilege map. A caller holding a bare `*`
// covers every role and so still sees everything, as does one holding
// `role.list-all` — the explicit right to the full catalog (NIM-203).
//
// callerAID is required — filtering has no basis without it, and an unfiltered
// catalog is exactly the leak this closes.
func (s *Service) ListRoles(ctx context.Context, callerAID string) ([]RoleView, error) {
	if callerAID == "" {
		return nil, fmt.Errorf("%w: missing caller", ErrPermissionNotHeld)
	}
	views, err := LoadRoleViews(ctx, s.pool)
	if err != nil {
		return nil, err
	}
	callerPerms, err := callerPermissions(ctx, s.pool, callerAID)
	if err != nil {
		return nil, err
	}
	if callerHoldsFullCatalog(callerPerms) {
		return views, nil
	}
	return visibleRoleViews(callerPerms, views)
}

// A self-lockout guard is a precondition, not a permission check (NIM-319).
//
// The six guards below answer one question — "would the cluster be left with no
// effective `*` admin?" — about the state the mutation would produce. They read
// that state under FOR UPDATE and say nothing about who is asking. So they run
// BEFORE every gate that requires a caller, and their refusal takes precedence
// over one.
//
// This is not a stylistic ordering. It is the only refusal on the write path that
// no caller, however privileged, may waive: a cluster-admin holding `*` is
// refused by it exactly as an unprivileged caller is. Everything else in the chain
// — the least-privilege floor, the root-role gate, [Service.assertCallerMayAdminister],
// [Service.assertCallerMayUnbind] — is a statement about the caller's rights and
// is satisfiable by holding more of them. A check that cannot be satisfied at all
// belongs ahead of checks that can, and a check about cluster state must be
// answerable when there is no subject in the picture.
//
// NIM-319 was the consequence of getting that backwards. Placing a caller gate
// first made the guarantee depend on that gate's incidental strictness rather than
// on the probe: "you cannot remove the last admin" held only because unbinding a
// `*`-granting role happens to demand `*` from the caller. Nothing was exploitable
// through the API — every handler passes its claims — but the invariant had stopped
// being enforced in its own right, and the caller-less path (`keeper init`, and any
// reconciler added later) reached a least-privilege refusal instead of the guard.
//
// ADR-078(k) places the cascade report last, so that a refusal about the caller's
// own rights is reported before one asking them to take a position. That ordering
// stands; it simply never assigned these guards a place, and they ended up between
// its two elements. The full order on the write path is now:
//
//	syntactic validation → self-lockout → caller's rights → cascade confirmation
//
// One consequence is accepted deliberately: a caller who may reach the endpoint but
// could not administer the role now learns from the refusal that the target is the
// last `*` holder. That audience already passed the enforcer's `Check` for a
// role-administration right, and the row locks above already disclose whether the
// membership exists at all. Locking a live cluster out has no path back through the
// API; this does not.
//
// assertNotLastWildcardRole is the self-lockout guard for delete/
// update-that-removes-`*`: lock the effective-cluster-admins core (DB + FOR
// UPDATE) and check that ≥1 active AID with `*` remains via a role OTHER
// than excludeRole. None → [ErrWouldLockOutCluster].
//
// excludeRole is being mutated/deleted in this same tx — its contribution to
// the effective `*` is about to vanish. So we count "survivors" via OTHER
// roles. For accuracy we use per-role membership (we can't just exclude the
// role from a flat AID list: an AID may hold `*` via both excludeRole and
// another role).
func (s *Service) assertNotLastWildcardRole(ctx context.Context, tx ExecQueryRower, excludeRole string) error {
	survivors, err := s.lockWildcardAdminsExcludingRole(ctx, tx, excludeRole)
	if err != nil {
		return err
	}
	if len(survivors) == 0 {
		return ErrWouldLockOutCluster
	}
	return nil
}

// assertNotLastWildcardOperator is the self-lockout guard for
// revoke-operator: lock the effective-cluster-admins core and check that ≥1
// active AID with `*` remains AFTER excluding exactly the (excludeRole,
// excludeAID) pair. None → [ErrWouldLockOutCluster].
//
// Only the pair's contribution is excluded: if excludeAID also holds `*` via
// another role, it stays; if another AID holds `*` via excludeRole, it stays
// too (only excludeAID's membership is being removed, not the whole role).
func (s *Service) assertNotLastWildcardOperator(ctx context.Context, tx ExecQueryRower, excludeRole, excludeAID string) error {
	survivors, err := s.lockWildcardAdminsExcludingPair(ctx, tx, excludeRole, excludeAID)
	if err != nil {
		return err
	}
	if len(survivors) == 0 {
		return ErrWouldLockOutCluster
	}
	return nil
}

// lockWildcardAdminsExcludingRole locks the self-lockout core (FOR UPDATE)
// and returns active AIDs with effective `*` via a role ≠ excludeRole —
// accounting for BOTH paths (direct ∪ via Synod, ADR-049(f)). excludeRole is
// being deleted/losing `*` in this same tx, so its contribution is excluded
// from both branches: direct `ro.role_name <> $1` and Synod
// `sr.role_name <> $1` (a role bundled into a group also stops granting
// `*`). Without the Synod branch, an admin holding `*` ONLY via a group
// would be counted as "nonexistent" → a false lockout OR (worse) a false
// pass that lets the last `*`-granting role be deleted.
//
// Two locking queries in a fixed order (direct → Synod, see
// [directClusterAdminsForUpdateSQL]) — PostgreSQL forbids UNION with FOR
// UPDATE.
func (s *Service) lockWildcardAdminsExcludingRole(ctx context.Context, tx ExecQueryRower, excludeRole string) ([]string, error) {
	// No DISTINCT: FOR UPDATE forbids it (SQLSTATE 0A000). Dedup isn't needed
	// for an empty/non-empty check; scanAIDs dedups anyway for cleanliness.
	const directQ = `
SELECT ro.aid
FROM rbac_role_operators ro
JOIN rbac_role_permissions rp ON rp.role_name = ro.role_name
JOIN rbac_roles r ON r.name = rp.role_name
JOIN operators o ON o.aid = ro.aid
WHERE rp.permission = '*' AND r.parent_role IS NULL AND o.revoked_at IS NULL AND ro.role_name <> $1
FOR UPDATE OF ro, rp, r, o
`
	const synodQ = `
SELECT so.aid
FROM synod_operators so
JOIN synod_roles sr ON sr.synod_name = so.synod_name
JOIN rbac_role_permissions rp ON rp.role_name = sr.role_name
JOIN rbac_roles r ON r.name = sr.role_name
JOIN operators o ON o.aid = so.aid
WHERE rp.permission = '*' AND r.parent_role IS NULL AND o.revoked_at IS NULL AND sr.role_name <> $1
FOR UPDATE OF so, sr, rp, r, o
`
	direct, err := scanAIDs(ctx, tx, directQ, excludeRole)
	if err != nil {
		return nil, err
	}
	synod, err := scanAIDs(ctx, tx, synodQ, excludeRole)
	if err != nil {
		return nil, err
	}
	return dedupAIDs(append(direct, synod...)), nil
}

// lockWildcardAdminsExcludingPair locks the self-lockout core and returns
// active AIDs with effective `*` AFTER excluding exactly one DIRECT
// membership row (excludeRole, excludeAID) — the exact row that
// `role.revoke-operator` removes from rbac_role_operators.
//
// The Synod branch (ADR-049(f)) is NOT filtered by the pair: revoke-operator
// doesn't touch synod_operators/synod_roles — if excludeAID holds `*` via
// Synod, it stays an admin even after the direct row is removed. This is
// semantically correct: one path is removed, the group path is still alive.
// (Mirror case: another AID holding `*` via excludeRole either directly OR
// through a Synod bundling that role also stays — only excludeAID's
// membership is being removed, not the role itself.)
//
// The direct branch excludes the pair: `NOT (role_name=$1 AND aid=$2)`. Two
// locking queries in a fixed order (direct → Synod, see
// [directClusterAdminsForUpdateSQL]).
func (s *Service) lockWildcardAdminsExcludingPair(ctx context.Context, tx ExecQueryRower, excludeRole, excludeAID string) ([]string, error) {
	// No DISTINCT (see lockWildcardAdminsExcludingRole).
	const directQ = `
SELECT ro.aid
FROM rbac_role_operators ro
JOIN rbac_role_permissions rp ON rp.role_name = ro.role_name
JOIN rbac_roles r ON r.name = rp.role_name
JOIN operators o ON o.aid = ro.aid
WHERE rp.permission = '*' AND r.parent_role IS NULL AND o.revoked_at IS NULL
  AND NOT (ro.role_name = $1 AND ro.aid = $2)
FOR UPDATE OF ro, rp, r, o
`
	const synodQ = `
SELECT so.aid
FROM synod_operators so
JOIN synod_roles sr ON sr.synod_name = so.synod_name
JOIN rbac_role_permissions rp ON rp.role_name = sr.role_name
JOIN rbac_roles r ON r.name = sr.role_name
JOIN operators o ON o.aid = so.aid
WHERE rp.permission = '*' AND r.parent_role IS NULL AND o.revoked_at IS NULL
FOR UPDATE OF so, sr, rp, r, o
`
	direct, err := scanAIDs(ctx, tx, directQ, excludeRole, excludeAID)
	if err != nil {
		return nil, err
	}
	synod, err := scanAIDs(ctx, tx, synodQ)
	if err != nil {
		return nil, err
	}
	return dedupAIDs(append(direct, synod...)), nil
}

// assertNotLastWildcardSynod is the self-lockout guard for synod.delete:
// lock the effective-cluster-admins core and check that ≥1 active AID with
// `*` remains AFTER the whole excludeSynod group disappears. None →
// [ErrWouldLockOutCluster].
//
// The group is removed via CASCADE in this same tx → its bundled roles stop
// granting `*` to ALL its members. So the Synod branch of the probe query
// excludes that group's rows (`so.synod_name <> excludeSynod`); the direct
// branch is untouched (deleting a group doesn't remove direct membership).
// Survivors are admins via OTHER groups OR directly.
func (s *Service) assertNotLastWildcardSynod(ctx context.Context, tx ExecQueryRower, excludeSynod string) error {
	survivors, err := s.lockWildcardAdminsExcludingSynod(ctx, tx, excludeSynod)
	if err != nil {
		return err
	}
	if len(survivors) == 0 {
		return ErrWouldLockOutCluster
	}
	return nil
}

// assertNotLastWildcardSynodRole is the self-lockout guard for
// synod.revoke-role: lock the effective-cluster-admins core and check that
// ≥1 active AID with `*` remains AFTER excludeRole is removed from
// excludeSynod's bundle. None → [ErrWouldLockOutCluster].
//
// The role leaves exactly THAT group's bundle (the synod_roles row is
// deleted in this same tx) → it stops granting `*` via excludeSynod, but
// still grants it via other groups / the same AID directly. The Synod branch
// excludes the (excludeSynod, excludeRole) pair; the direct branch is
// untouched.
func (s *Service) assertNotLastWildcardSynodRole(ctx context.Context, tx ExecQueryRower, excludeSynod, excludeRole string) error {
	survivors, err := s.lockWildcardAdminsExcludingSynodRole(ctx, tx, excludeSynod, excludeRole)
	if err != nil {
		return err
	}
	if len(survivors) == 0 {
		return ErrWouldLockOutCluster
	}
	return nil
}

// assertNotLastWildcardSynodOperator is the self-lockout guard for
// synod.remove-operator: lock the effective-cluster-admins core and check
// that ≥1 active AID with `*` remains AFTER excluding exactly the
// (excludeSynod, excludeAID) pair from the group's membership. None →
// [ErrWouldLockOutCluster].
//
// One synod_operators row is removed → excludeAID loses excludeSynod's
// roles, but if it holds `*` via ANOTHER group OR directly, it stays; other
// members of excludeSynod are unaffected. The Synod branch excludes the
// (excludeSynod, excludeAID) pair; the direct branch is untouched
// (excludeAID may hold `*` directly — in which case it remains an admin).
func (s *Service) assertNotLastWildcardSynodOperator(ctx context.Context, tx ExecQueryRower, excludeSynod, excludeAID string) error {
	survivors, err := s.lockWildcardAdminsExcludingSynodOperator(ctx, tx, excludeSynod, excludeAID)
	if err != nil {
		return err
	}
	if len(survivors) == 0 {
		return ErrWouldLockOutCluster
	}
	return nil
}

// lockWildcardAdminsExcludingSynod — the admin set with `*` after the whole
// excludeSynod group disappears (synod.delete). The direct branch is
// untouched; the Synod branch excludes the group's rows
// (`so.synod_name <> $1`). Two locking queries in a fixed order (direct →
// Synod, see [directClusterAdminsForUpdateSQL]).
func (s *Service) lockWildcardAdminsExcludingSynod(ctx context.Context, tx ExecQueryRower, excludeSynod string) ([]string, error) {
	const synodQ = `
SELECT so.aid
FROM synod_operators so
JOIN synod_roles sr ON sr.synod_name = so.synod_name
JOIN rbac_role_permissions rp ON rp.role_name = sr.role_name
JOIN rbac_roles r ON r.name = sr.role_name
JOIN operators o ON o.aid = so.aid
WHERE rp.permission = '*' AND r.parent_role IS NULL AND o.revoked_at IS NULL AND so.synod_name <> $1
FOR UPDATE OF so, sr, rp, r, o
`
	direct, err := scanAIDs(ctx, tx, directClusterAdminsForUpdateSQL)
	if err != nil {
		return nil, err
	}
	synod, err := scanAIDs(ctx, tx, synodQ, excludeSynod)
	if err != nil {
		return nil, err
	}
	return dedupAIDs(append(direct, synod...)), nil
}

// lockWildcardAdminsExcludingSynodRole — the admin set with `*` after
// excludeRole is removed from excludeSynod's bundle (synod.revoke-role). The
// direct branch is untouched; the Synod branch excludes the pair
// (`NOT (sr.synod_name=$1 AND sr.role_name=$2)`).
func (s *Service) lockWildcardAdminsExcludingSynodRole(ctx context.Context, tx ExecQueryRower, excludeSynod, excludeRole string) ([]string, error) {
	const synodQ = `
SELECT so.aid
FROM synod_operators so
JOIN synod_roles sr ON sr.synod_name = so.synod_name
JOIN rbac_role_permissions rp ON rp.role_name = sr.role_name
JOIN rbac_roles r ON r.name = sr.role_name
JOIN operators o ON o.aid = so.aid
WHERE rp.permission = '*' AND r.parent_role IS NULL AND o.revoked_at IS NULL
  AND NOT (sr.synod_name = $1 AND sr.role_name = $2)
FOR UPDATE OF so, sr, rp, r, o
`
	direct, err := scanAIDs(ctx, tx, directClusterAdminsForUpdateSQL)
	if err != nil {
		return nil, err
	}
	synod, err := scanAIDs(ctx, tx, synodQ, excludeSynod, excludeRole)
	if err != nil {
		return nil, err
	}
	return dedupAIDs(append(direct, synod...)), nil
}

// lockWildcardAdminsExcludingSynodOperator — the admin set with `*` after
// excluding the (excludeSynod, excludeAID) pair from the group's membership
// (synod.remove-operator). The direct branch is untouched; the Synod branch
// excludes the pair (`NOT (so.synod_name=$1 AND so.aid=$2)`).
func (s *Service) lockWildcardAdminsExcludingSynodOperator(ctx context.Context, tx ExecQueryRower, excludeSynod, excludeAID string) ([]string, error) {
	const synodQ = `
SELECT so.aid
FROM synod_operators so
JOIN synod_roles sr ON sr.synod_name = so.synod_name
JOIN rbac_role_permissions rp ON rp.role_name = sr.role_name
JOIN rbac_roles r ON r.name = sr.role_name
JOIN operators o ON o.aid = so.aid
WHERE rp.permission = '*' AND r.parent_role IS NULL AND o.revoked_at IS NULL
  AND NOT (so.synod_name = $1 AND so.aid = $2)
FOR UPDATE OF so, sr, rp, r, o
`
	direct, err := scanAIDs(ctx, tx, directClusterAdminsForUpdateSQL)
	if err != nil {
		return nil, err
	}
	synod, err := scanAIDs(ctx, tx, synodQ, excludeSynod, excludeAID)
	if err != nil {
		return nil, err
	}
	return dedupAIDs(append(direct, synod...)), nil
}

// scanAIDs collects the single-column AID result shared by the self-lockout
// queries.
func scanAIDs(ctx context.Context, tx ExecQueryRower, sql string, args ...any) ([]string, error) {
	rows, err := tx.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("rbac: self-lockout probe: %w", wrapPgErr(err))
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var aid string
		if err := rows.Scan(&aid); err != nil {
			return nil, fmt.Errorf("rbac: self-lockout scan: %w", err)
		}
		out = append(out, aid)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rbac: self-lockout iter: %w", err)
	}
	return dedupAIDs(out), nil
}
