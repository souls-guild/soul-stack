package incarnation

import (
	"context"
	"fmt"
)

// ServiceStats is one row of [SelectServiceStats]. Hosts counts DISTINCT members:
// membership is M:N, so a host bound to two incarnations of one service is one
// host of that service, not two.
type ServiceStats struct {
	Service      string
	Incarnations int
	Hosts        int
}

// SelectServiceStats counts, per service, the incarnations inside scope and the
// distinct hosts bound to them (GET /v1/stats/services). Same incarnation scope as
// [SelectAll] via [ScopeCondition]. hosts renders the caller's Soul visibility
// over the unaliased `souls` table; a member outside it is not counted, and nil
// counts no host at all. A service with no incarnation in scope has no row; an
// incarnation without a visible member still counts, with 0 hosts. Sorted by service.
func SelectServiceStats(ctx context.Context, db ExecQueryRower, scope ListScope, hosts ScopeSQLFunc) ([]ServiceStats, error) {
	visible := "SELECT id, service FROM incarnation"
	cond, args := ScopeCondition(nil, scope)
	if cond != "" {
		visible += " WHERE " + cond
	}
	hostCond := "FALSE"
	if hosts != nil {
		var extra []any
		hostCond, extra, _ = hosts(len(args) + 1)
		args = append(args, extra...)
	}
	sql := `SELECT v.service, COUNT(DISTINCT v.id), COUNT(DISTINCT vm.sid)
FROM (` + visible + `) v
LEFT JOIN (
    SELECT m.incarnation_name, m.sid
    FROM incarnation_membership m
    JOIN souls ON souls.sid = m.sid
    WHERE ` + hostCond + `
) vm ON vm.incarnation_name = v.id
GROUP BY v.service
ORDER BY v.service`

	rows, err := db.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("incarnation: service stats: %w", err)
	}
	defer rows.Close()

	out := []ServiceStats{}
	for rows.Next() {
		var s ServiceStats
		if err := rows.Scan(&s.Service, &s.Incarnations, &s.Hosts); err != nil {
			return nil, fmt.Errorf("incarnation: scan service stats: %w", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("incarnation: iterate service stats: %w", err)
	}
	return out, nil
}
