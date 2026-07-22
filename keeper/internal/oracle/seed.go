package oracle

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"
)

// Sentinel errors for seeding.
var (
	ErrVigilAlreadyExists  = errors.New("oracle: vigil already exists")
	ErrDecreeAlreadyExists = errors.New("oracle: decree already exists")
)

// InsertVigil inserts a row into the `vigils` registry. Used by the management Service
// (S3 CRUD) and by seeding in tests/dev provisioning (S2). Params nil → writes '{}'
// (the column's DEFAULT). created_at/updated_at are filled back into v via
// RETURNING (the create response of OpenAPI/MCP carries real timestamps).
//
// Return:
//   - [ErrVigilAlreadyExists] on a UNIQUE violation on the PK (name);
//   - a wrapped fmt.Errorf on a CHECK violation (subject_xor / name_format) and
//     an FK violation (created_by_aid).
func InsertVigil(ctx context.Context, db ExecQueryRower, v *Vigil) error {
	if v == nil {
		return fmt.Errorf("oracle: nil vigil")
	}
	const sql = `
INSERT INTO vigils (name, coven, sid, interval_spec, check_addr, params, enabled, created_by_aid)
VALUES ($1, $2, $3, $4, $5, COALESCE($6, '{}'::jsonb), $7, $8)
RETURNING created_at, updated_at`
	var paramsArg any
	if len(v.Params) > 0 {
		paramsArg = []byte(v.Params)
	}
	row := db.QueryRow(ctx, sql,
		v.Name, v.Coven, v.SID, v.IntervalSpec, v.CheckAddr,
		paramsArg, v.Enabled, v.CreatedByAID,
	)
	if err := row.Scan(&v.CreatedAt, &v.UpdatedAt); err != nil {
		return mapInsertErr(err, ErrVigilAlreadyExists, "vigil")
	}
	return nil
}

// InsertDecree inserts a row into the `decrees` registry. Used by the
// management Service (S3 CRUD) and by seeding in tests/dev provisioning (S2).
// ActionInput nil → writes '{}'. An empty Cooldown → writes '0s' (the DEFAULT,
// cooldown disabled). created_at/updated_at + the normalized cooldown
// are filled back into d via RETURNING (the create response of OpenAPI/MCP carries
// real values).
//
// Return:
//   - [ErrDecreeAlreadyExists] on a UNIQUE violation on the PK (name);
//   - a wrapped fmt.Errorf on a CHECK violation (subject_xor / name_format /
//     scenario_format) and an FK violation (created_by_aid).
func InsertDecree(ctx context.Context, db ExecQueryRower, d *Decree) error {
	if d == nil {
		return fmt.Errorf("oracle: nil decree")
	}
	cooldown := d.Cooldown
	if cooldown == "" {
		cooldown = "0s"
	}
	const sql = `
INSERT INTO decrees (name, on_beacon, where_cel, subject_coven, subject_sid, incarnation_name, action_scenario, action_input, cooldown, enabled, created_by_aid)
VALUES ($1, $2, $3, $4, $5, $6, $7, COALESCE($8, '{}'::jsonb), $9, $10, $11)
RETURNING cooldown, created_at, updated_at`
	var inputArg any
	if len(d.ActionInput) > 0 {
		inputArg = []byte(d.ActionInput)
	}
	row := db.QueryRow(ctx, sql,
		d.Name, d.OnBeacon, d.WhereCEL, d.SubjectCoven, d.SubjectSID,
		d.IncarnationName, d.ActionScenario, inputArg, cooldown, d.Enabled, d.CreatedByAID,
	)
	if err := row.Scan(&d.Cooldown, &d.CreatedAt, &d.UpdatedAt); err != nil {
		return mapInsertErr(err, ErrDecreeAlreadyExists, "decree")
	}
	return nil
}

func mapInsertErr(err error, dup error, what string) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case pgErrCodeUniqueViolation:
			return fmt.Errorf("%w (constraint %s): %w", dup, pgErr.ConstraintName, err)
		case pgErrCodeForeignKeyViolation:
			return fmt.Errorf("oracle: %s FK violation on %s: %w", what, pgErr.ConstraintName, err)
		case pgErrCodeCheckViolation:
			return fmt.Errorf("oracle: %s CHECK violation on %s: %w", what, pgErr.ConstraintName, err)
		}
	}
	return fmt.Errorf("oracle: insert %s: %w", what, err)
}
