package rbac

import (
	"context"
	"errors"
	"fmt"
)

// Minting privilege that tracks nothing (NIM-201).
//
// A derived role follows its parent: narrow the parent and the child narrows at
// the next snapshot build (ADR-078(c)/(d)). A PLAIN role follows nothing — it is
// a snapshot of privilege that outlives whatever the operator held when they
// wrote it. Revoke their `coven=dba` role tomorrow and the plain role they minted
// keeps granting `coven=dba` to whoever holds it, with no rule having fired.
//
// The least-privilege floor does not catch this: every permission in that role
// WAS covered at the moment of writing. The floor bounds what you may put in a
// role, not whether the result keeps tracking the rights it came from.
//
// So minting a plain role that grants something is its own action,
// `role.create-root`. The default becomes: derive from a role you hold, and the
// cascade does the rest. The ceiling is then ONE named role — an organisational
// object other holders administer — never the creator, whose union across roles
// is wider than any single role they could point at (ADR-078(e)) and who may be
// revoked or leave.
//
// The gate is on the SHAPE OF THE RESULT, not on the verb: it fires wherever a
// caller puts privilege into a role that will have no parent — on create, on a
// PATCH that clears parent_role, and on a PATCH that grows an already-plain role.
// Gating only creation would leave it one PATCH wide, the same trap ADR-078(h)
// records for the attenuation gate.
//
// A plain role that grants NOTHING is free: there is no privilege to strand, and
// it mirrors the visibility rule (role_visibility.go), where an empty role is
// visible to everyone.

// ErrRootRoleNotPermitted — the caller asked for a role with no parent that
// grants something, without holding `role.create-root`. Separate from
// [ErrPermissionNotHeld]: every permission asked for may well be within the
// caller's rights: what is refused is minting privilege that tracks nothing.
// Transport maps it to 403, alongside the other two "not yours to give" refusals.
var ErrRootRoleNotPermitted = errors.New("rbac: caller may not create a role without a parent")

// rootRolePermission is the right to mint a role that tracks nothing. Bare on
// purpose — [callerHolds] then demands an UNRESTRICTED holder, which is the only
// sound reading: a scope on it would have to select roles, and the grammar has no
// `role=` dimension.
var rootRolePermission = Permission{Resource: "role", Action: "create-root"}

// assertCallerMayMintRootRole refuses a plain role that grants something unless
// the caller holds `role.create-root`. granting is what the caller is putting in
// (the same effective set the least-privilege floor just judged) — empty means
// nothing is being stranded and the gate does not apply.
//
// Read inside the mutation's tx, like every other caller-side read here, so a
// concurrently revoked right is not honoured. `*` and `role.*` cover the action
// for free, so a cluster-admin is unaffected.
func (s *Service) assertCallerMayMintRootRole(ctx context.Context, db ExecQueryRower, callerAID string, granting []Permission) error {
	if len(granting) == 0 {
		return nil
	}
	if callerAID == "" {
		return fmt.Errorf("%w: missing caller", ErrRootRoleNotPermitted)
	}
	callerPerms, err := callerPermissions(ctx, db, callerAID)
	if err != nil {
		return err
	}
	if callerHolds(callerPerms, rootRolePermission) {
		return nil
	}
	return fmt.Errorf("%w: derive from a role you hold, or obtain %s",
		ErrRootRoleNotPermitted, permString(rootRolePermission))
}
