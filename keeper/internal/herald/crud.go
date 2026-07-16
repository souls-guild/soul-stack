package herald

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Sentinel errors of CRUD layer. Handler-side (OpenAPI/MCP, slice S4) maps:
//   - ErrHeraldExists    → 409.
//   - ErrHeraldNotFound  → 404.
//   - ErrTidingExists    → 409.
//   - ErrTidingNotFound  → 404.
//
// ErrHeraldInUse is NOT introduced: tidings.herald is ON DELETE CASCADE (ADR-052(a),
// naming-rules.md), deleting Herald removes its Tiding subscriptions, not
// blocked (difference from RESTRICT-409).
var (
	ErrHeraldExists   = errors.New("herald: name already exists")
	ErrHeraldNotFound = errors.New("herald: name not found")
	ErrTidingExists   = errors.New("herald: tiding name already exists")
	ErrTidingNotFound = errors.New("herald: tiding name not found")

	// ErrValidation is wrapper over any service validation of CRUD input (broken
	// name/type/config/secret_ref/event_types/projection). Handler-side
	// (OpenAPI/MCP) maps it to 422 validation-failed; public-detail is safe
	// (formed by validators without internal SQL/stack — see [PublicMessage]).
	ErrValidation = errors.New("herald: validation failed")

	// ErrEphemeralRequiresVoyage signals invariant violation ephemeral⟺voyage_id
	// (ADR-052(g)): ephemeral rule must carry voyage_id, persistent must not.
	// Duplicates CHECK tidings_ephemeral_voyage_consistent (defence in depth +
	// friendly error before DB call). Wrapped in [ErrValidation].
	ErrEphemeralRequiresVoyage = errors.New("herald: ephemeral tiding requires voyage_id (and non-ephemeral must not set it)")
)

// IsValidationError returns true if err is service validation of CRUD input
// ([ErrValidation] wrapper). Used by handlers to map to 422.
func IsValidationError(err error) bool {
	return errors.Is(err, ErrValidation)
}

// PublicMessage returns client-safe text of validation error: trims wrapper
// `herald: validation failed: ` and internal pkg-prefix `herald: `. For non-validation
// errors caller does not pass them here (checks IsValidationError first).
func PublicMessage(err error) string {
	if err == nil {
		return ""
	}
	// ErrValidation is wrapped as fmt.Errorf("%w: <validator-msg>", ErrValidation),
	// so err.Error() = "herald: validation failed: <validator-msg>". Take
	// original validator-msg (already public), removing wrapper and pkg-prefix.
	msg := strings.TrimPrefix(err.Error(), "herald: validation failed: ")
	return strings.TrimPrefix(msg, "herald: ")
}

// wrapValidation wraps validation error in [ErrValidation], preserving
// original message for public-detail AND errors.Is chain to wrapped sentinel
// (e.g. [ErrEphemeralRequiresVoyage]). nil → nil. Double %w
// (Go 1.20+) makes result comparable with both ErrValidation and wrapped err.
func wrapValidation(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w: %w", ErrValidation, err)
}

const (
	pgErrCodeUniqueViolation     = "23505"
	pgErrCodeForeignKeyViolation = "23503"
	pgErrCodeCheckViolation      = "23514"
)

// ExecQueryRower is narrow subset of pgxpool.Pool needed by CRUD. Symmetric
// to augur/pushprovider: unit tests go through fake without PG, production
// provides real pool / Conn / Tx.
type ExecQueryRower interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Compile-time check.
var (
	_ ExecQueryRower = (*pgx.Conn)(nil)
	_ ExecQueryRower = (*pgxpool.Pool)(nil)
	_ ExecQueryRower = (pgx.Tx)(nil)
)

// --- Herald -----------------------------------------------------------

const heraldColumns = `name, type, config, secret_ref, enabled, created_at, updated_at, created_by_aid`

const heraldInsertSQL = `
INSERT INTO heralds (name, type, config, secret_ref, enabled, created_by_aid)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING created_at, updated_at
`

const heraldSelectByNameSQL = `
SELECT ` + heraldColumns + `
FROM heralds
WHERE name = $1
`

const heraldUpdateSQL = `
UPDATE heralds
SET type = $2,
    config = $3,
    secret_ref = $4,
    enabled = $5,
    updated_at = NOW()
WHERE name = $1
`

// InsertHerald inserts new Herald channel.
//
// Pre-conditions (service validation):
//   - h.Name matches [NamePattern];
//   - h.Type ∈ closed enum ([ValidHeraldType]);
//   - h.Config is valid for type ([ValidateConfig] — webhook url + SSRF circuit);
//   - h.SecretRef (if set) is vault-ref ([ValidateSecretRef]).
//
// Returns [ErrHeraldExists] on UNIQUE PK; wrapped fmt.Errorf on FK-/CHECK-
// violation.
func InsertHerald(ctx context.Context, db ExecQueryRower, h *Herald) error {
	if h == nil {
		return fmt.Errorf("herald: nil herald")
	}
	if err := validateHerald(h); err != nil {
		return err
	}

	configBytes, err := marshalConfig(h.Config)
	if err != nil {
		return fmt.Errorf("herald: marshal config: %w", err)
	}

	row := db.QueryRow(ctx, heraldInsertSQL,
		h.Name, string(h.Type), configBytes, secretRefArg(h.SecretRef), h.Enabled, aidArg(h.CreatedByAID),
	)
	if err := row.Scan(&h.CreatedAt, &h.UpdatedAt); err != nil {
		return mapHeraldInsertError(err)
	}
	return nil
}

func mapHeraldInsertError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case pgErrCodeUniqueViolation:
			return fmt.Errorf("%w (constraint %s): %w", ErrHeraldExists, pgErr.ConstraintName, err)
		case pgErrCodeForeignKeyViolation:
			return fmt.Errorf("herald: FK violation on %s: %w", pgErr.ConstraintName, err)
		case pgErrCodeCheckViolation:
			return fmt.Errorf("herald: CHECK violation on %s: %w", pgErr.ConstraintName, err)
		}
	}
	return fmt.Errorf("herald: insert herald: %w", err)
}

// SelectHeraldByName reads Herald by PK. [ErrHeraldNotFound] on pgx.ErrNoRows.
func SelectHeraldByName(ctx context.Context, db ExecQueryRower, name string) (*Herald, error) {
	return scanHerald(db.QueryRow(ctx, heraldSelectByNameSQL, name))
}

func scanHerald(row pgx.Row) (*Herald, error) {
	var (
		h            Herald
		typeStr      string
		configBytes  []byte
		secretRef    *string
		createdByAID *string
	)
	err := row.Scan(
		&h.Name, &typeStr, &configBytes, &secretRef, &h.Enabled,
		&h.CreatedAt, &h.UpdatedAt, &createdByAID,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrHeraldNotFound
		}
		return nil, fmt.Errorf("herald: scan herald: %w", err)
	}
	h.Type = HeraldType(typeStr)
	h.SecretRef = secretRef
	h.CreatedByAID = createdByAID
	if len(configBytes) > 0 {
		if err := json.Unmarshal(configBytes, &h.Config); err != nil {
			return nil, fmt.Errorf("herald: unmarshal config: %w", err)
		}
	}
	return &h, nil
}

// SelectAllHeralds returns page of Heralds and total count.
//
// Sorting is `updated_at DESC, name ASC` (fresh above; tie-break by name for
// stable pagination). Total and items by two queries outside transaction
// (eventually consistent, like augur/pushprovider).
func SelectAllHeralds(ctx context.Context, db ExecQueryRower, offset, limit int) ([]*Herald, int, error) {
	if offset < 0 {
		return nil, 0, fmt.Errorf("herald: offset must be >= 0, got %d", offset)
	}
	if limit < 1 {
		return nil, 0, fmt.Errorf("herald: limit must be >= 1, got %d", limit)
	}

	var total int
	if err := db.QueryRow(ctx, "SELECT COUNT(*) FROM heralds").Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("herald: count heralds: %w", err)
	}

	const listSQL = `SELECT ` + heraldColumns + `
FROM heralds
ORDER BY updated_at DESC, name ASC
OFFSET $1 LIMIT $2`
	rows, err := db.Query(ctx, listSQL, offset, limit)
	if err != nil {
		return nil, 0, fmt.Errorf("herald: list heralds query: %w", err)
	}
	defer rows.Close()

	out := make([]*Herald, 0, limit)
	for rows.Next() {
		h, err := scanHerald(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("herald: list heralds iter: %w", err)
	}
	return out, total, nil
}

// UpdateHerald replaces mutable fields of Herald (type/config/secret_ref/enabled,
// replace semantics). name (PK) is immutable. [ErrHeraldNotFound] if PK not found.
func UpdateHerald(ctx context.Context, db ExecQueryRower, h *Herald) error {
	if h == nil {
		return fmt.Errorf("herald: nil herald")
	}
	if err := validateHerald(h); err != nil {
		return err
	}

	configBytes, err := marshalConfig(h.Config)
	if err != nil {
		return fmt.Errorf("herald: marshal config: %w", err)
	}

	tag, err := db.Exec(ctx, heraldUpdateSQL,
		h.Name, string(h.Type), configBytes, secretRefArg(h.SecretRef), h.Enabled,
	)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgErrCodeCheckViolation {
			return fmt.Errorf("herald: CHECK violation on %s: %w", pgErr.ConstraintName, err)
		}
		return fmt.Errorf("herald: update herald: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrHeraldNotFound
	}
	return nil
}

// DeleteHerald deletes Herald by PK. All its Tidings cascade (ON DELETE
// CASCADE, ADR-052(a)). [ErrHeraldNotFound] if row not found.
func DeleteHerald(ctx context.Context, db ExecQueryRower, name string) error {
	tag, err := db.Exec(ctx, "DELETE FROM heralds WHERE name = $1", name)
	if err != nil {
		return fmt.Errorf("herald: delete herald: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrHeraldNotFound
	}
	return nil
}

// --- Tiding -----------------------------------------------------------

const tidingColumns = `name, herald, event_types, only_failures, only_changes, incarnation, cadence, task, ephemeral, voyage_id, created_from_cadence_id, annotations, projection, enabled, created_at, updated_at, created_by_aid`

const tidingInsertSQL = `
INSERT INTO tidings (name, herald, event_types, only_failures, only_changes, incarnation, cadence, task, ephemeral, voyage_id, created_from_cadence_id, annotations, projection, enabled, created_by_aid)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
RETURNING created_at, updated_at
`

const tidingSelectByNameSQL = `
SELECT ` + tidingColumns + `
FROM tidings
WHERE name = $1
`

const tidingUpdateSQL = `
UPDATE tidings
SET herald = $2,
    event_types = $3,
    only_failures = $4,
    only_changes = $5,
    incarnation = $6,
    cadence = $7,
    task = $8,
    ephemeral = $9,
    voyage_id = $10,
    created_from_cadence_id = $11,
    annotations = $12,
    projection = $13,
    enabled = $14,
    updated_at = NOW()
WHERE name = $1
`

// InsertTiding inserts new Tiding rule.
//
// Pre-conditions (service validation):
//   - t.Name matches [NamePattern];
//   - t.Herald is non-empty (FK to heralds — existence checked by DB);
//   - t.EventTypes is valid ([ValidateEventTypes] — non-empty + run-scope).
//
// Returns [ErrTidingExists] on UNIQUE PK; [ErrHeraldNotFound] if Herald
// by FK does not exist (FK-violation on tidings_herald_fk); wrapped fmt.Errorf
// on other FK-/CHECK-violation.
func InsertTiding(ctx context.Context, db ExecQueryRower, t *Tiding) error {
	if t == nil {
		return fmt.Errorf("herald: nil tiding")
	}
	if err := validateTiding(t); err != nil {
		return err
	}

	annotationsBytes, err := marshalAnnotations(t.Annotations)
	if err != nil {
		return fmt.Errorf("herald: marshal annotations: %w", err)
	}

	row := db.QueryRow(ctx, tidingInsertSQL,
		t.Name, t.Herald, t.EventTypes, t.OnlyFailures, t.OnlyChanges,
		optStrArg(t.Incarnation), optStrArg(t.Cadence), optStrArg(t.Task),
		t.Ephemeral, optStrArg(t.VoyageID), optStrArg(t.CreatedFromCadenceID),
		annotationsBytes, projectionArg(t.Projection),
		t.Enabled, aidArg(t.CreatedByAID),
	)
	if err := row.Scan(&t.CreatedAt, &t.UpdatedAt); err != nil {
		return mapTidingInsertError(err)
	}
	return nil
}

func mapTidingInsertError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case pgErrCodeUniqueViolation:
			return fmt.Errorf("%w (constraint %s): %w", ErrTidingExists, pgErr.ConstraintName, err)
		case pgErrCodeForeignKeyViolation:
			if pgErr.ConstraintName == "tidings_herald_fk" {
				return fmt.Errorf("%w (tiding references it): %w", ErrHeraldNotFound, err)
			}
			return fmt.Errorf("herald: tiding FK violation on %s: %w", pgErr.ConstraintName, err)
		case pgErrCodeCheckViolation:
			return fmt.Errorf("herald: tiding CHECK violation on %s: %w", pgErr.ConstraintName, err)
		}
	}
	return fmt.Errorf("herald: insert tiding: %w", err)
}

// SelectTidingByName reads Tiding by PK. [ErrTidingNotFound] on pgx.ErrNoRows.
func SelectTidingByName(ctx context.Context, db ExecQueryRower, name string) (*Tiding, error) {
	return scanTiding(db.QueryRow(ctx, tidingSelectByNameSQL, name))
}

func scanTiding(row pgx.Row) (*Tiding, error) {
	var (
		t                    Tiding
		incarnation          *string
		cadence              *string
		task                 *string
		voyageID             *string
		createdFromCadenceID *string
		annotationsBytes     []byte
		createdByAID         *string
	)
	err := row.Scan(
		&t.Name, &t.Herald, &t.EventTypes, &t.OnlyFailures, &t.OnlyChanges,
		&incarnation, &cadence, &task, &t.Ephemeral, &voyageID, &createdFromCadenceID,
		&annotationsBytes, &t.Projection,
		&t.Enabled, &t.CreatedAt, &t.UpdatedAt, &createdByAID,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrTidingNotFound
		}
		return nil, fmt.Errorf("herald: scan tiding: %w", err)
	}
	t.Incarnation = incarnation
	t.Cadence = cadence
	t.Task = task
	t.VoyageID = voyageID
	t.CreatedFromCadenceID = createdFromCadenceID
	t.CreatedByAID = createdByAID
	if len(annotationsBytes) > 0 {
		if err := json.Unmarshal(annotationsBytes, &t.Annotations); err != nil {
			return nil, fmt.Errorf("herald: unmarshal annotations: %w", err)
		}
	}
	return &t, nil
}

func collectTidings(rows pgx.Rows) ([]*Tiding, error) {
	defer rows.Close()
	var out []*Tiding
	for rows.Next() {
		t, err := scanTiding(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("herald: list tidings iter: %w", err)
	}
	return out, nil
}

// SelectAllTidings returns page of Tidings and total count. Sorting is
// `updated_at DESC, name ASC`. Total and items by two queries (eventually
// consistent).
//
// includeEphemeral=false (default) hides ephemeral rules (ADR-052(g)):
// listing shows only persistent subscriptions managed by operator; ephemeral rules
// tied to runs are implementation detail (ADR-042 "dumb frontend": filtering on backend,
// not client-side). total computed under same predicate so pagination doesn't "lose"
// pages. includeEphemeral=true returns all (debugging).
func SelectAllTidings(ctx context.Context, db ExecQueryRower, includeEphemeral bool, offset, limit int) ([]*Tiding, int, error) {
	if offset < 0 {
		return nil, 0, fmt.Errorf("herald: offset must be >= 0, got %d", offset)
	}
	if limit < 1 {
		return nil, 0, fmt.Errorf("herald: limit must be >= 1, got %d", limit)
	}

	// Predicate for ephemeral hiding. Partial index tidings_ephemeral_voyage_idx
	// covers only ephemeral rows; for default branch (NOT ephemeral)
	// DB does seq-scan over small rules table — acceptable.
	where := ""
	if !includeEphemeral {
		where = " WHERE NOT ephemeral"
	}

	var total int
	if err := db.QueryRow(ctx, "SELECT COUNT(*) FROM tidings"+where).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("herald: count tidings: %w", err)
	}

	listSQL := `SELECT ` + tidingColumns + `
FROM tidings` + where + `
ORDER BY updated_at DESC, name ASC
OFFSET $1 LIMIT $2`
	rows, err := db.Query(ctx, listSQL, offset, limit)
	if err != nil {
		return nil, 0, fmt.Errorf("herald: list tidings query: %w", err)
	}
	tidings, err := collectTidings(rows)
	if err != nil {
		return nil, 0, err
	}
	return tidings, total, nil
}

// SelectTidingsByHerald returns all Tiding rules of one Herald (CRUD
// list-by-herald). Sorting `updated_at DESC, name ASC`.
func SelectTidingsByHerald(ctx context.Context, db ExecQueryRower, herald string) ([]*Tiding, error) {
	const sql = `SELECT ` + tidingColumns + `
FROM tidings
WHERE herald = $1
ORDER BY updated_at DESC, name ASC`
	rows, err := db.Query(ctx, sql, herald)
	if err != nil {
		return nil, fmt.Errorf("herald: list tidings by herald query: %w", err)
	}
	return collectTidings(rows)
}

// UpdateTiding replaces mutable fields of Tiding (replace semantics). name (PK)
// is immutable. [ErrTidingNotFound] if PK not found; [ErrHeraldNotFound] if
// new herald by FK does not exist.
func UpdateTiding(ctx context.Context, db ExecQueryRower, t *Tiding) error {
	if t == nil {
		return fmt.Errorf("herald: nil tiding")
	}
	if err := validateTiding(t); err != nil {
		return err
	}

	annotationsBytes, err := marshalAnnotations(t.Annotations)
	if err != nil {
		return fmt.Errorf("herald: marshal annotations: %w", err)
	}

	tag, err := db.Exec(ctx, tidingUpdateSQL,
		t.Name, t.Herald, t.EventTypes, t.OnlyFailures, t.OnlyChanges,
		optStrArg(t.Incarnation), optStrArg(t.Cadence), optStrArg(t.Task),
		t.Ephemeral, optStrArg(t.VoyageID), optStrArg(t.CreatedFromCadenceID),
		annotationsBytes, projectionArg(t.Projection),
		t.Enabled,
	)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) {
			switch pgErr.Code {
			case pgErrCodeForeignKeyViolation:
				if pgErr.ConstraintName == "tidings_herald_fk" {
					return fmt.Errorf("%w (tiding references it): %w", ErrHeraldNotFound, err)
				}
				return fmt.Errorf("herald: tiding FK violation on %s: %w", pgErr.ConstraintName, err)
			case pgErrCodeCheckViolation:
				return fmt.Errorf("herald: tiding CHECK violation on %s: %w", pgErr.ConstraintName, err)
			}
		}
		return fmt.Errorf("herald: update tiding: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrTidingNotFound
	}
	return nil
}

// DeleteTiding deletes Tiding by PK. [ErrTidingNotFound] if row not found.
func DeleteTiding(ctx context.Context, db ExecQueryRower, name string) error {
	tag, err := db.Exec(ctx, "DELETE FROM tidings WHERE name = $1", name)
	if err != nil {
		return fmt.Errorf("herald: delete tiding: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrTidingNotFound
	}
	return nil
}

// --- helpers ----------------------------------------------------------

// validateHerald does service validation of Herald fields before write. All errors
// wrapped in [ErrValidation] (handler → 422). Each sub-check carries
// public-message (no internal SQL/stack).
func validateHerald(h *Herald) error {
	if !ValidName(h.Name) {
		return wrapValidation(fmt.Errorf("invalid name %q (must match %s)", h.Name, NamePattern))
	}
	if !ValidHeraldType(h.Type) {
		return wrapValidation(fmt.Errorf("invalid type %q (must be webhook)", h.Type))
	}
	if err := ValidateConfig(h.Type, h.Config); err != nil {
		return wrapValidation(err)
	}
	if err := ValidateSecretRef(h.Type, h.SecretRef); err != nil {
		return wrapValidation(err)
	}
	return nil
}

// validateTiding does service validation of Tiding fields before write. Errors wrapped
// in [ErrValidation]. Herald existence by FK is checked by DB (see mapTidingInsertError).
func validateTiding(t *Tiding) error {
	// Normalize empty VoyageID string to nil BEFORE mapping so domain-guard and
	// SQL-arg (optStrArg) consistently treat it as NULL: otherwise guard below
	// would pass non-ephemeral+&"" (as "not set"), and optStrArg would write ''
	// → CHECK tidings_ephemeral_voyage_consistent would fail with 500 (symmetric
	// to how aidArg treats empty AID as NULL).
	if t.VoyageID != nil && *t.VoyageID == "" {
		t.VoyageID = nil
	}
	// Normalize empty task-selector string to nil: nil = "no filter", empty
	// address should not match changed_tasks (ADR-052 §l). Without this
	// optStrArg would write `''` to column — dead selector matching nothing.
	if t.Task != nil && *t.Task == "" {
		t.Task = nil
	}
	// Normalize empty origin-marker string to nil: nil = "not created by Cadence form",
	// empty '' in TEXT column would violate FK on cadences(id) (parity
	// VoyageID/Task). Non-empty value checked by DB (FK existence on cadences).
	if t.CreatedFromCadenceID != nil && *t.CreatedFromCadenceID == "" {
		t.CreatedFromCadenceID = nil
	}
	if !ValidName(t.Name) {
		return wrapValidation(fmt.Errorf("invalid tiding name %q (must match %s)", t.Name, NamePattern))
	}
	if t.Herald == "" {
		return wrapValidation(fmt.Errorf("tiding herald is empty"))
	}
	if err := ValidateEventTypes(t.EventTypes); err != nil {
		return wrapValidation(err)
	}
	// Invariant ephemeral⟺voyage_id (ADR-052(g), defence in depth over CHECK):
	// ephemeral rule must carry voyage_id, persistent must not. VoyageID
	// is already normalized here (nil instead of empty string), so `!= nil`
	// is sufficient — `*t.VoyageID != ""` is invariantly true here.
	if t.Ephemeral != (t.VoyageID != nil) {
		return wrapValidation(ErrEphemeralRequiresVoyage)
	}
	if err := ValidateProjection(t.Projection); err != nil {
		return wrapValidation(err)
	}
	return nil
}

// secretRefArg maps nil-string → NULL for nullable secret_ref column.
func secretRefArg(ref *string) any {
	if ref == nil {
		return nil
	}
	return *ref
}

// optStrArg maps nil-string → NULL for optional selectors incarnation/cadence/voyage_id.
func optStrArg(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}

// projectionArg maps nil/empty slice → empty TEXT[] (column NOT NULL DEFAULT '{}',
// pgx requires non-nil). Non-empty passed as-is.
func projectionArg(p []string) []string {
	if p == nil {
		return []string{}
	}
	return p
}

// aidArg maps nil/empty AID → NULL for nullable created_by_aid (FK ON DELETE SET
// NULL). Treat empty string as NULL: empty AID would violate FK on operators.
func aidArg(aid *string) any {
	if aid == nil || *aid == "" {
		return nil
	}
	return *aid
}
