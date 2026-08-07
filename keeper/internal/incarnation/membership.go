package incarnation

import (
	"context"
	"fmt"
	"sort"
	"time"
)

// Membership is a first-class M:N relation host↔incarnation (ADR-008 amendment
// 2026-07-17, NIM-124): the source of truth for "which incarnation a host
// belongs to", replacing the former derived fact `incarnation.name ∈
// souls.coven[]`. The roster read (topology), the `Incarnation` bulk selector
// (soul), the Choir member check, form-prep host listing, and the Oracle's
// cross-incarnation guard ([IsMember], NIM-224) all resolve membership through
// `incarnation_membership` (migration 099).

const insertMembershipSQL = `
INSERT INTO incarnation_membership (incarnation_name, sid, bound_by_aid)
SELECT $1, s, $3 FROM unnest($2::text[]) AS s
ON CONFLICT DO NOTHING
RETURNING sid
`

// AddMembers binds the given SIDs to incarnation `incName` (idempotent: ON
// CONFLICT DO NOTHING). byAID is the operator that bound them (nil for a
// keeper-internal bind, e.g. `core.soul.registered` without an operator). Empty
// sids → no-op. The souls rows must already exist (FK sid → souls); the bind
// act (`core.soul.registered`) creates the pending rows before calling this.
func AddMembers(ctx context.Context, db ExecQueryRower, incName string, sids []string, byAID *string) error {
	_, err := AddMembersReporting(ctx, db, incName, sids, byAID)
	return err
}

// AddMembersReporting is [AddMembers] that also reports WHICH SIDs it actually
// wrote — the ones absent from the relation before the call, straight out of
// `RETURNING sid` (ON CONFLICT DO NOTHING emits no row for an existing pair).
// The operator bind endpoint uses the split to answer bound vs already_member
// without a second read: the bind stays idempotent, but a re-bind remains
// distinguishable from a first bind in the reply and in the audit trail.
//
// The returned slice is sorted, so the reply and the audit payload are stable
// regardless of insert order.
func AddMembersReporting(ctx context.Context, db ExecQueryRower, incName string, sids []string, byAID *string) ([]string, error) {
	if !ValidName(incName) {
		return nil, fmt.Errorf("incarnation: add members: invalid name %q", incName)
	}
	if len(sids) == 0 {
		return nil, nil
	}
	rows, err := db.Query(ctx, insertMembershipSQL, incName, sids, byAID)
	if err != nil {
		return nil, fmt.Errorf("incarnation: add members to %q: %w", incName, err)
	}
	defer rows.Close()

	var bound []string
	for rows.Next() {
		var sid string
		if err := rows.Scan(&sid); err != nil {
			return nil, fmt.Errorf("incarnation: scan bound sid: %w", err)
		}
		bound = append(bound, sid)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("incarnation: iter bound sids of %q: %w", incName, err)
	}
	sort.Strings(bound)
	return bound, nil
}

const removeOneMembershipSQL = `
DELETE FROM incarnation_membership
WHERE incarnation_name = $1 AND sid = $2
`

// RemoveMember unbinds ONE SID from incarnation `incName` and reports whether a
// row was actually removed. Unbinding a non-member is a silent no-op (false,
// nil) — idempotent, like the bind direction. The caller still audits the
// no-op: the intent was expressed even when nothing changed.
func RemoveMember(ctx context.Context, db ExecQueryRower, incName, sid string) (bool, error) {
	if !ValidName(incName) {
		return false, fmt.Errorf("incarnation: remove member: invalid name %q", incName)
	}
	tag, err := db.Exec(ctx, removeOneMembershipSQL, incName, sid)
	if err != nil {
		return false, fmt.Errorf("incarnation: remove member %q from %q: %w", sid, incName, err)
	}
	return tag.RowsAffected() > 0, nil
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

// Member is one row of an incarnation's roster as the operator sees it: the
// membership facts (`bound_at` / `bound_by_aid`, migration 099) joined with the
// host facts needed to authorize and to display it (status + the scope
// dimensions coven/traits). Not a [soul.Soul] — the read is about membership,
// and the host columns here exist to serve it.
type Member struct {
	SID    string
	Status string
	Covens []string
	// TraitsRaw is the host's `souls.traits` jsonb exactly as Postgres
	// serializes it. It serves the scope check alone (the roster does not
	// display traits) and is kept RAW on purpose: the souls list compares
	// against these very bytes, and decoding them first loses the number
	// token it compares (NIM-401). Project it with
	// [soulpurview.TraitsFromJSON].
	TraitsRaw  []byte
	BoundAt    time.Time
	BoundByAID *string
}

const listMembersSQL = `
SELECT m.sid, s.status, s.coven, s.traits, m.bound_at, m.bound_by_aid
FROM incarnation_membership m
JOIN souls s ON s.sid = m.sid
WHERE m.incarnation_name = $1
ORDER BY m.sid ASC
`

// ListMembers returns the roster of incarnation `incName` — membership rows
// joined with their host's status and scope dimensions, sorted by SID. An
// incarnation with no members → empty slice, not an error. The JOIN is inner by
// design: FK `sid → souls ON DELETE CASCADE` means a membership row cannot
// outlive its host, so there is no orphan to surface.
func ListMembers(ctx context.Context, db ExecQueryRower, incName string) ([]Member, error) {
	if !ValidName(incName) {
		return nil, fmt.Errorf("incarnation: list members: invalid name %q", incName)
	}
	rows, err := db.Query(ctx, listMembersSQL, incName)
	if err != nil {
		return nil, fmt.Errorf("incarnation: list members of %q: %w", incName, err)
	}
	defer rows.Close()

	out := make([]Member, 0)
	for rows.Next() {
		var m Member
		if err := rows.Scan(&m.SID, &m.Status, &m.Covens, &m.TraitsRaw, &m.BoundAt, &m.BoundByAID); err != nil {
			return nil, fmt.Errorf("incarnation: scan member row: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("incarnation: iter members of %q: %w", incName, err)
	}
	return out, nil
}

const isMemberSQL = `
SELECT EXISTS (
    SELECT 1 FROM incarnation_membership
    WHERE incarnation_name = $1 AND sid = $2
)
`

// IsMember reports whether host `sid` is bound to incarnation `incName`. The
// single-host form of the relation, for gates that admit or refuse ONE host
// (the Oracle's cross-incarnation guard, ADR-030(b)).
//
// ★ A membership gate must NOT be answered from the label layer. `souls.coven`
// holds only what an operator attached by hand (NIM-281), so `incName ∈ coven`
// is true for any host somebody tagged with a string spelled like the
// incarnation's name and false for a member nobody tagged — wrong in both
// directions, and the first is exactly the cross-incarnation escalation such
// gates exist to refuse. Labels answer "may this rule see the host"; only this
// relation answers "does the host belong here".
//
// An invalid name is an error, not a false: it means the caller passed
// something that could never be a member, and a gate must not read that as a
// quiet "no".
func IsMember(ctx context.Context, db ExecQueryRower, incName, sid string) (bool, error) {
	if !ValidName(incName) {
		return false, fmt.Errorf("incarnation: is-member: invalid name %q", incName)
	}
	if sid == "" {
		return false, fmt.Errorf("incarnation: is-member: empty sid")
	}
	var member bool
	if err := db.QueryRow(ctx, isMemberSQL, incName, sid).Scan(&member); err != nil {
		return false, fmt.Errorf("incarnation: is-member %q in %q: %w", sid, incName, err)
	}
	return member, nil
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
