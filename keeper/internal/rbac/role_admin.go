package rbac

import "context"

// Who may administer a role (NIM-214, ADR-078(m)).
//
// Creating a role was always bounded — you cannot put in what you do not hold —
// but EDITING and DELETING one were cluster-level: `role.update` / `role.delete`
// are NoSelector, so any holder could rewrite or drop any non-builtin role in the
// cluster, including roles far above their own rights. `role.delete` did not even
// take a caller. The floor did not object, and correctly so on its own terms:
// taking rights away grants nothing, which is why trimming had been deliberately
// free since ADR-028. What that leaves open is not escalation but demolition —
// one holder of `role.update` can zero out every team's access, and the
// self-lockout guard only notices when the LAST `*` admin would go.
//
// The rule, from the model that a role belongs to its parent rather than to its
// author (NIM-201):
//
//	a caller may administer a role ⟺ the caller could GRANT what that role grants
//
// which is the containment of subset.go once more — the same predicate the floor,
// the catalog filter (NIM-202) and attenuation (ADR-078(c)) all read. No third
// notion of ownership is introduced, and that is the point: "administer" turns out
// to be an existing question asked about the role's CURRENT form.
//
// The requested model falls out of it rather than being coded separately. Every
// holder of a role P administers the roles derived from P: a derived role's rights
// are contained in its parent's (attenuation), so anyone holding P covers them.
// Roles outside anyone's subtree are not orphaned either — a plain role is
// administered by whoever could have created it, which is what `role.create-root`
// already selects for.
//
// Two consequences worth stating plainly:
//
//   - trimming a role you do not cover is now REFUSED. That reverses "cutting
//     someone else's role is not escalation" — true as far as escalation goes, and
//     precisely the demolition surface this closes. Trimming a role you DO cover
//     is untouched, and remains ungated by the floor and the root-role gate.
//   - an empty role grants nothing, so everyone covers it and anyone with
//     `role.update` may administer it. Same answer the catalog filter gives for
//     visibility, and for the same reason: there is no privilege to protect.
//
// A bare `*` covers everything, so cluster-admins are unaffected. The refusal is
// [ErrPermissionNotHeld]: it IS the least-privilege boundary, applied to the role
// as it stands rather than to the change being made, and a second sentinel for the
// same containment would be a second place to disagree with the decision layer.

// assertCallerMayAdminister refuses a mutation of a role whose rights the caller
// could not grant. role is the target in RESOLVED form — what it actually grants
// today, the only form a coverage question may be asked about (NIM-198).
//
// Read inside the mutation's tx, like every other caller-side read here, so a
// concurrently revoked right is not honoured.
func (s *Service) assertCallerMayAdminister(ctx context.Context, db ExecQueryRower, callerAID string, role *Role) error {
	return s.assertCallerMayGrant(ctx, db, callerAID, effectivePermissions(role.Permissions, role.DefaultScope))
}

// Taking a binding apart is administering it too (NIM-285).
//
// The same hole the gate above closes stayed open one door along: revoking a role
// from an operator, and emptying a Synod of its members or its bundle, carried no
// caller at all — the inputs did not have the field. Any holder of
// `role.revoke-operator` could strip any archon of any role, gated only by
// self-lockout, which fires when the LAST `*` admin would go and says nothing
// about the thousand revocations before it.
//
// The rule is the one already written on the granting side, applied in reverse:
// each revoke asks for exactly what its matching grant asks for. Symmetry rather
// than a new judgement, because the pair is one authorization surface seen from
// two directions, and any asymmetry between them is a gap by construction:
//
//	role.revoke-operator  ⟷  role.grant-operator     the ROLE's effective rights
//	synod.remove-operator ⟷  synod.add-operator      the group's WHOLE bundle
//	synod.revoke-role     ⟷  synod.grant-role        the revoked ROLE's rights
//
// The two Synod rows differ on purpose and inherit that difference from
// ADR-049(f): a member receives the entire bundle, so touching membership is
// measured against all of it, while granting or revoking one role is measured
// against that role.
//
// Removing a right still grants nothing, so none of this is escalation. It is the
// demolition surface, and the argument for closing it is the one NIM-214 already
// accepted for `role.update`.

// assertCallerMayUnbind refuses to take a binding apart when the caller could not
// have created it. required is what the matching grant would have demanded.
func (s *Service) assertCallerMayUnbind(ctx context.Context, db ExecQueryRower, callerAID string, required []Permission) error {
	return s.assertCallerMayGrant(ctx, db, callerAID, required)
}
