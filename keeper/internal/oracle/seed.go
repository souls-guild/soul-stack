package oracle

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/souls-guild/soul-stack/keeper/internal/registrylabel"
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
//   - a wrapped fmt.Errorf on a CHECK violation (subject_one_of / name_format)
//     and an FK violation (created_by_aid).
func InsertVigil(ctx context.Context, db ExecQueryRower, v *Vigil) error {
	if v == nil {
		return fmt.Errorf("oracle: nil vigil")
	}
	const sql = `
INSERT INTO vigils (id, sid, service, incarnation, coven, trait_key, trait_value, interval_spec, check_addr, params, enabled, created_by_aid, label)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, COALESCE($10, '{}'::jsonb), $11, $12, $13)
RETURNING created_at, updated_at`
	var paramsArg any
	if len(v.Params) > 0 {
		paramsArg = []byte(v.Params)
	}
	// The label is canonicalised, never validated: free text is the point
	// ([ADR-0085]). Blank collapses to NULL so "absent" has one spelling.
	v.Label = registrylabel.Normalize(v.Label)
	row := db.QueryRow(ctx, sql,
		v.ID, v.SID, v.Service, v.Incarnation, v.Coven, v.TraitKey, v.TraitValue,
		v.IntervalSpec, v.CheckAddr,
		paramsArg, v.Enabled, v.CreatedByAID, v.Label,
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
//   - a wrapped fmt.Errorf on a CHECK violation (subject_one_of / name_format /
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
INSERT INTO decrees (id, on_beacon, where_cel, subject_sid, subject_service, subject_incarnation, subject_coven, subject_trait_key, subject_trait_value, incarnation_name, action_scenario, action_input, cooldown, enabled, created_by_aid, label)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, COALESCE($12, '{}'::jsonb), $13, $14, $15, $16)
RETURNING cooldown, created_at, updated_at`
	var inputArg any
	if len(d.ActionInput) > 0 {
		inputArg = []byte(d.ActionInput)
	}
	// The label is canonicalised, never validated ([ADR-0085]).
	d.Label = registrylabel.Normalize(d.Label)
	row := db.QueryRow(ctx, sql,
		d.ID, d.OnBeacon, d.WhereCEL,
		d.SubjectSID, d.SubjectService, d.SubjectIncarnation, d.SubjectCoven,
		d.SubjectTraitKey, d.SubjectTraitValue,
		d.IncarnationName, d.ActionScenario, inputArg, cooldown, d.Enabled, d.CreatedByAID,
		d.Label,
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
