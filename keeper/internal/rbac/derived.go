package rbac

import (
	"errors"
	"fmt"
)

// Derived roles (ADR-078, NIM-179). A role may name another role in
// `rbac_roles.parent_role` (migration 102); the named role is its parent and the
// deriving role is a CHILD, bounded by the parent's rights. A NULL parent is a
// plain role — what every role is today.
//
// This file holds the MODEL-level guards over the parent graph: no self-parent,
// no cycles, a capped chain length. Resolving a chain into effective permissions
// and scope (attenuation, cascade) is NIM-180 — nothing here reads parent_role
// when a permission is checked, so a stored parent grants a child exactly
// nothing extra until then (narrower than the intended semantics, never wider).
//
// The authoritative gate is the DB (a CHECK, a self-FK ON DELETE RESTRICT and the
// rbac_roles_parent_chain_guard trigger), so every write path is covered — not
// just the Go one. The checks here are the read-side half of the same rule: a
// snapshot whose graph is broken (an older binary, a hand-edited row, a restore)
// must not build an enforcer.

// ScopeMode is the recorded intent of a derived role's delta (ADR-078(k),
// migration 105). It answers a question the delta string cannot: whether the
// operator meant to FOLLOW the parent's scope or to freeze it.
//
// Resolution does not branch on it. Both modes resolve as
// `effective_scope(parent) AND default_scope(role)` — they differ only in what
// was written into the delta at write time, so a pinned role still narrows when
// its parent narrows, and the monotone-AND argument of ADR-078(b) is untouched.
type ScopeMode string

const (
	// ScopeModeNone — the role is plain; there is no parent and so no intent.
	// Stored as NULL, and the DB holds it to that (migration 105 CHECKs the two
	// columns are NULL together).
	ScopeModeNone ScopeMode = ""
	// ScopeModeTrack — the delta is the ADDED narrowing only, so the parent's
	// scope cascades in. The ADR-078(b) contract and the default for a new
	// derived role: it is what makes moving a parent move its children.
	ScopeModeTrack ScopeMode = "track"
	// ScopeModePin — the parent's effective scope was materialized INTO the delta
	// when the mode was set, so a later WIDENING of the parent does not reach this
	// role. Deliberately a write-time act rather than a resolve-time rule: a
	// pinned role is a role whose predicate says what it means, readable without
	// consulting its parent.
	ScopeModePin ScopeMode = "pin"
)

// Valid reports whether m is one of the three states. Transport validates before
// the tx opens; the DB CHECK is the authority.
func (m ScopeMode) Valid() bool {
	return m == ScopeModeNone || m == ScopeModeTrack || m == ScopeModePin
}

// ErrInvalidScopeMode — scope_mode is neither `track` nor `pin`, or is set on a
// role with no parent (where it would describe a relationship that does not
// exist). Transport maps it to 422.
var ErrInvalidScopeMode = errors.New("rbac: invalid scope_mode")

// maxRoleChainDepth caps a derivation chain, counted in ROLES: a plain role is
// depth 1, a role with a parent is depth 2. Four roles = at most three parent
// hops. Mirrors [maxScopeDepth] (the scope-nesting cap) and MUST stay equal to
// max_depth in migration 102 — [TestRoleChainDepthCapMatchesMigration] pins them
// together, since the two guards are enforced in different languages.
const maxRoleChainDepth = 4

const (
	// pgErrCodeRoleParentCycle / pgErrCodeRoleChainTooDeep — the custom SQLSTATEs
	// raised by the rbac_roles_parent_chain_guard trigger (migration 102). Class
	// "SS" is outside the standard-reserved range, so it can't collide with a
	// PostgreSQL-defined condition.
	pgErrCodeRoleParentCycle  = "SS001"
	pgErrCodeRoleChainTooDeep = "SS002"
)

// ErrRoleParentCycle — a role's parent chain closes on itself (a role naming
// itself, or A → B → A). A cycle has no root, so there is no ceiling to attenuate
// against and the resolver would not terminate. Transport maps it to 422.
var ErrRoleParentCycle = errors.New("rbac: role parent chain forms a cycle")

// ErrRoleChainTooDeep — a derivation chain longer than [maxRoleChainDepth].
// Depth is bounded so flattening at snapshot build stays cheap and bounded, and
// so an operator can still read the ceiling of a role off a short chain.
// Transport maps it to 422.
var ErrRoleChainTooDeep = errors.New("rbac: role derivation chain too deep")

// ErrRoleParentUnknown — a role names a parent that is not in the catalog. The
// self-FK rules this out in the DB; reaching it means the snapshot is drifted, and
// a child whose ceiling can't be read is denied rather than treated as unbounded
// (fail-closed — the same call as the orphan policy below).
var ErrRoleParentUnknown = errors.New("rbac: role parent not found in the catalog")

// ErrRoleHasChildren — role.delete on a role that is still someone's
// `parent_role`. The orphan policy is fail-closed RESTRICT (ADR-078): the operator
// re-parents or deletes the children explicitly. The alternatives all change
// somebody's rights implicitly — clearing the parent turns the child's delta into
// an absolute scope and drops the parent's narrowing (a WIDENING), re-rooting to
// the grandparent widens by definition, and cascading the delete silently strips
// membership. Transport maps it to 409.
var ErrRoleHasChildren = errors.New("rbac: role is a parent of other roles (delete refused)")

// validateRoleGraph checks the parent graph of a snapshot: every parent exists,
// no chain closes on itself, none exceeds [maxRoleChainDepth]. roles is the set of
// catalog role names; parents maps a child's name to its parent's (roles with no
// parent are simply absent).
//
// Walks up from each child, at most one hop past the cap. A revisited node means
// a cycle — tracked explicitly rather than inferred from the hop count, so a cycle
// that a role merely feeds into (start → a → b → a) is still reported as a cycle
// and not mislabelled as an over-deep chain.
func validateRoleGraph(roles map[string]struct{}, parents map[string]string) error {
	for name := range parents {
		seen := map[string]struct{}{name: {}}
		cur, depth := name, 1
		for {
			parent, derived := parents[cur]
			if !derived {
				break // reached a root
			}
			if _, known := roles[parent]; !known {
				return fmt.Errorf("%w: role %q names parent %q", ErrRoleParentUnknown, cur, parent)
			}
			if _, revisited := seen[parent]; revisited {
				return fmt.Errorf("%w: role %q through %q", ErrRoleParentCycle, name, parent)
			}
			seen[parent] = struct{}{}
			cur, depth = parent, depth+1
			if depth > maxRoleChainDepth {
				return fmt.Errorf("%w: role %q is %d+ roles deep (max %d)",
					ErrRoleChainTooDeep, name, depth, maxRoleChainDepth)
			}
		}
	}
	return nil
}
