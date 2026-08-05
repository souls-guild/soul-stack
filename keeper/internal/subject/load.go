package subject

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// Querier — the narrow registry surface [LoadHost] needs. Narrowing (rather
// than taking a *pgxpool.Pool) keeps the resolver testable against a fake and
// lets a caller pass a transaction.
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// ErrHostUnknown — the SID is not in the souls registry. Every caller treats it
// as a denial, not as a Keeper failure: no authoritative subject, no grant.
var ErrHostUnknown = errors.New("subject: sid not found in souls registry")

// LoadHost reads the authoritative facts a subject match needs: the host's own
// labels plus the incarnations it belongs to, with each incarnation's own
// labels.
//
// One round trip. The host row and its roster memberships come back as a single
// LEFT JOIN so a host with no memberships is still distinguishable from an
// unknown SID (which is [ErrHostUnknown], not an empty Host) — the difference
// between "belongs to nothing" and "is nobody" decides deny-with-reason versus
// deny-as-error.
//
// Reads `incarnation_membership`, the relation (NIM-124) — NEVER a label. A
// coven tag is something anyone with `soul.coven-assign` may attach, so it
// cannot carry a membership decision: a host merely tagged with an
// incarnation's name is not a member of it, and this is the read that keeps
// those two apart (NIM-280).
func LoadHost(ctx context.Context, db Querier, sid string) (Host, error) {
	const sql = `
SELECT s.coven, s.traits, i.service, i.name, i.covens, i.traits
FROM souls s
LEFT JOIN incarnation_membership m ON m.sid = s.sid
LEFT JOIN incarnation i ON i.name = m.incarnation_name
WHERE s.sid = $1
ORDER BY i.service NULLS FIRST, i.name NULLS FIRST`

	rows, err := db.Query(ctx, sql, sid)
	if err != nil {
		return Host{}, fmt.Errorf("subject: load host %q: %w", sid, err)
	}
	defer rows.Close()

	h := Host{SID: sid}
	found := false
	for rows.Next() {
		var (
			hostCovens []string
			hostTraits []byte
			incService *string
			incName    *string
			incCovens  []string
			incTraits  []byte
		)
		if err := rows.Scan(&hostCovens, &hostTraits, &incService, &incName, &incCovens, &incTraits); err != nil {
			return Host{}, fmt.Errorf("subject: scan host %q: %w", sid, err)
		}
		if !found {
			found = true
			h.Covens = hostCovens
			h.Traits, err = decodeTraits(hostTraits)
			if err != nil {
				return Host{}, fmt.Errorf("subject: host %q traits: %w", sid, err)
			}
		}
		if incName == nil || incService == nil {
			continue // LEFT JOIN filler: the host belongs to no incarnation.
		}
		traits, err := decodeTraits(incTraits)
		if err != nil {
			return Host{}, fmt.Errorf("subject: incarnation %q traits: %w", *incName, err)
		}
		h.Member = append(h.Member, Incarnation{
			Service: *incService,
			Name:    *incName,
			Covens:  incCovens,
			Traits:  traits,
		})
	}
	if err := rows.Err(); err != nil {
		return Host{}, fmt.Errorf("subject: load host %q iter: %w", sid, err)
	}
	if !found {
		return Host{}, ErrHostUnknown
	}
	return h, nil
}

// ExistsIncarnation reports whether the pair names a real incarnation.
//
// Called when a rule is WRITTEN, not when it is matched. There is deliberately
// no FK on the subject columns (following `decrees.incarnation_name`) — a rule
// must outlive an incarnation being rebuilt rather than be deleted by cascade —
// but without a check at write time a typo produces a rule that is silently
// never reached, and nothing in the system ever says so. This turns that into a
// 422 the operator reads immediately.
func ExistsIncarnation(ctx context.Context, db Querier, service, name string) (bool, error) {
	const sql = `SELECT EXISTS (SELECT 1 FROM incarnation WHERE service = $1 AND name = $2)`
	var ok bool
	if err := db.QueryRow(ctx, sql, service, name).Scan(&ok); err != nil {
		return false, fmt.Errorf("subject: check incarnation %s.%s: %w", service, name, err)
	}
	return ok, nil
}

// decodeTraits turns a traits JSONB column into a map. The column is
// NOT NULL DEFAULT '{}' (migrations 087 / 088), so an empty payload is normal
// and decodes to an empty map, not to an error.
func decodeTraits(raw []byte) (map[string]any, error) {
	if len(raw) == 0 {
		return map[string]any{}, nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	if m == nil {
		m = map[string]any{}
	}
	return m, nil
}
