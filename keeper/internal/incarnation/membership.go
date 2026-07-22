package incarnation

import (
	"context"
	"fmt"
)

// Membership is a first-class M:N relation host↔incarnation (ADR-008 amendment
// 2026-07-17, NIM-124): the source of truth for "which incarnation a host
// belongs to", replacing the former derived fact `incarnation.name ∈
// souls.coven[]`. The roster read (topology), the `Incarnation` bulk selector
// (soul), the Choir member check, and form-prep host listing all resolve
// membership through `incarnation_membership` (migration 099).

const insertMembershipSQL = `
INSERT INTO incarnation_membership (incarnation_name, sid, bound_by_aid)
SELECT $1, s, $3 FROM unnest($2::text[]) AS s
ON CONFLICT DO NOTHING
`

// AddMembers binds the given SIDs to incarnation `incName` (idempotent: ON
// CONFLICT DO NOTHING). byAID is the operator that bound them (nil for a
// keeper-internal bind, e.g. `core.soul.registered` without an operator). Empty
// sids → no-op. The souls rows must already exist (FK sid → souls); the bind
// act (`core.soul.registered`) creates the pending rows before calling this.
func AddMembers(ctx context.Context, db ExecQueryRower, incName string, sids []string, byAID *string) error {
	if !ValidName(incName) {
		return fmt.Errorf("incarnation: add members: invalid name %q", incName)
	}
	if len(sids) == 0 {
		return nil
	}
	if _, err := db.Exec(ctx, insertMembershipSQL, incName, sids, byAID); err != nil {
		return fmt.Errorf("incarnation: add members to %q: %w", incName, err)
	}
	return nil
}

const removeMembershipSQL = `
DELETE FROM incarnation_membership
WHERE incarnation_name = $1 AND sid = ANY($2)
`

// RemoveMembers unbinds the given SIDs from incarnation `incName`. Empty sids →
// no-op. Removing a non-member is a silent no-op (idempotent).
func RemoveMembers(ctx context.Context, db ExecQueryRower, incName string, sids []string) error {
	if !ValidName(incName) {
		return fmt.Errorf("incarnation: remove members: invalid name %q", incName)
	}
	if len(sids) == 0 {
		return nil
	}
	if _, err := db.Exec(ctx, removeMembershipSQL, incName, sids); err != nil {
		return fmt.Errorf("incarnation: remove members from %q: %w", incName, err)
	}
	return nil
}

const listMemberSIDsSQL = `
SELECT sid FROM incarnation_membership
WHERE incarnation_name = $1
ORDER BY sid ASC
`

// ListMemberSIDs returns the SIDs bound to incarnation `incName`, sorted. An
// incarnation with no members → empty slice, not an error.
func ListMemberSIDs(ctx context.Context, db ExecQueryRower, incName string) ([]string, error) {
	if !ValidName(incName) {
		return nil, fmt.Errorf("incarnation: list members: invalid name %q", incName)
	}
	rows, err := db.Query(ctx, listMemberSIDsSQL, incName)
	if err != nil {
		return nil, fmt.Errorf("incarnation: list members of %q: %w", incName, err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var sid string
		if err := rows.Scan(&sid); err != nil {
			return nil, fmt.Errorf("incarnation: scan member sid: %w", err)
		}
		out = append(out, sid)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("incarnation: iter members of %q: %w", incName, err)
	}
	return out, nil
}
