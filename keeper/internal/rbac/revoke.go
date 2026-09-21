package rbac

import "context"

// Revoking a membership is guarded by default, and the raw row delete is the one
// you have to ask for by name (NIM-320).
//
// The self-lockout invariant used to live in [Service.RevokeOperator] alone, and
// the package-level revoke documented that arrangement: "the self-lockout check
// lives in Service.RevokeOperator (this is DELETE only)". A comment is not a
// boundary. `keeper/internal/auth`'s federated reconciler took the package-level
// function — it needs to revoke inside its own transaction, alongside the grants,
// so it could not use the Service at all — and thereby stripped memberships with
// no probe of any kind. Any managed role could be revoked from the last operator
// holding `*`, and the cluster would be left with no administrator: a state with
// no way back through the API.
//
// So the names now carry the guarantee instead of a comment. [RevokeOperator]
// keeps the obvious name and runs the probe; [RevokeOperatorRow] is the bare
// DELETE, named so that reaching for it is a decision rather than the default.
// Only [Service.RevokeOperator] uses it, because it runs the probe earlier for
// ordering reasons (ADR-078(n)).
//
// What this is NOT: a permission check. The reconciler has no caller — it applies
// an external identity provider's decision, and its grants are equally caller-less
// (`granted_by_aid IS NULL`). Measuring it against an operator's rights would be
// meaningless, and inventing a system caller to satisfy the least-privilege floor
// would create a subject with unlimited rights for no reason. What the reconciler
// must respect is the cluster-state precondition, which is exactly the thing
// ADR-078(n) established as answerable with no subject present.

// assertRevokeKeepsClusterAdmin refuses to remove the (roleName, aid) membership
// when it would leave the cluster with no active operator holding an effective
// `*`. The one definition of that question: [Service.RevokeOperator] calls it at
// the position ADR-078(n) fixes, and [RevokeOperator] calls it for everyone else.
//
// Only a PLAIN `*` role is a cluster-admin source (ADR-078(i)) — a derived one was
// never counted by the probes, so revoking it cannot strand anyone and the probe
// is skipped. That skip is not only an optimisation: the probe reports lockout
// whenever the surviving set is empty, so running it for a role that is not a
// source would refuse ordinary revokes in a cluster that already has no admin.
//
// Read inside the caller's transaction. The probe takes row locks (FOR UPDATE),
// which is what makes two concurrent revokes unable to both pass.
func assertRevokeKeepsClusterAdmin(ctx context.Context, db ExecQueryRower, roleName, aid string) error {
	perms, err := rolePermissions(ctx, db, roleName)
	if err != nil {
		return err
	}
	parent, err := roleParent(ctx, db, roleName)
	if err != nil {
		return err
	}
	if !grantsClusterAdmin(perms, parent) {
		return nil
	}
	// The probe excludes exactly the (roleName, aid) pair: if the AID also holds
	// `*` through another role it stays in the surviving set, and if another AID
	// holds `*` through this role it stays too — only this one membership is
	// going away, not the role.
	return assertNotLastWildcardOperator(ctx, db, roleName, aid)
}

// RevokeOperator removes the membership row (roleName, aid) unless doing so would
// leave the cluster without an administrator.
//
// The guarded entry point, and the one to use. Errors:
//   - [ErrWouldLockOutCluster] — the last active operator with an effective `*`
//     would lose it (ADR-028(f), ADR-049(f) for the Synod path).
//   - [ErrRoleOperatorNotFound] — the pair does not exist.
//   - a wrapped pgx error on a transport failure.
//
// Callers that have already run [assertRevokeKeepsClusterAdmin] — meaning
// [Service.RevokeOperator], which runs it before its caller-rights gate — use
// [RevokeOperatorRow] instead, so the probe and its row locks are not taken twice.
func RevokeOperator(ctx context.Context, db ExecQueryRower, roleName, aid string) error {
	if err := assertRevokeKeepsClusterAdmin(ctx, db, roleName, aid); err != nil {
		return err
	}
	return RevokeOperatorRow(ctx, db, roleName, aid)
}
