package soul

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/souls-guild/soul-stack/keeper/internal/soulforget"
)

// ForgetDB is the pool surface [PGForgetter] needs: one read and the forget
// transaction. Satisfied by `*pgxpool.Pool`.
type ForgetDB interface {
	soulforget.TxBeginner
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// PGForgetter is the production [Forgetter]: the same [soulforget] path REST and MCP
// use, with the same Teardown — a nil one would erase the row and release nothing.
type PGForgetter struct {
	DB       ForgetDB
	Teardown soulforget.Teardown
}

// NewPGForgetter is a wire helper for the daemon.
func NewPGForgetter(db ForgetDB, td soulforget.Teardown) *PGForgetter {
	return &PGForgetter{DB: db, Teardown: td}
}

// membersOfSQL — the members of $1, each with every incarnation it is tied to: its
// memberships and the incarnations whose Choir holds a Voice of it. Only the outer
// row is filtered on $1; the two arrays bring back the other incarnations so a
// shared host can be refused rather than severed.
const membersOfSQL = `
SELECT m.sid,
    ARRAY(SELECT o.incarnation_name FROM incarnation_membership o WHERE o.sid = m.sid ORDER BY 1),
    ARRAY(SELECT DISTINCT v.incarnation_name FROM incarnation_choir_voices v WHERE v.sid = m.sid ORDER BY 1)
FROM incarnation_membership m
WHERE m.incarnation_name = $1
ORDER BY m.sid
`

func (f *PGForgetter) Members(ctx context.Context, incarnation string) (map[string]soulforget.Ties, error) {
	rows, err := f.DB.Query(ctx, membersOfSQL, incarnation)
	if err != nil {
		return nil, fmt.Errorf("query members: %w", err)
	}
	defer rows.Close()
	out := map[string]soulforget.Ties{}
	for rows.Next() {
		var sid string
		var t soulforget.Ties
		if err := rows.Scan(&sid, &t.MemberOf, &t.VoiceIn); err != nil {
			return nil, fmt.Errorf("scan members: %w", err)
		}
		out[sid] = t
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read members: %w", err)
	}
	return out, nil
}

func (f *PGForgetter) Forget(ctx context.Context, sid, incarnation, reason string) (soulforget.Result, error) {
	return soulforget.EraseOwned(ctx, f.DB, f.Teardown, sid, incarnation, reason)
}
