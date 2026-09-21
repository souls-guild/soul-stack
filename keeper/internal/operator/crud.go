package operator

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

// ErrOperatorAlreadyExists — UNIQUE-violation (`23505`) on Insert: AID
// already taken or bootstrap is being inserted again (partial unique index
// `operators_first_archon_idx WHERE created_via='bootstrap'` — migrations
// 084/085, ADR-058(d): invariant moved from `created_by_aid IS NULL`).
var ErrOperatorAlreadyExists = errors.New("operator: AID already exists")

// ErrOperatorNotFound — SELECT did not find a row by AID. Returned by
// SelectByAID — a separate sentinel so the caller can distinguish
// "does not exist" from a transport error.
var ErrOperatorNotFound = errors.New("operator: AID not found")

// ErrOperatorAlreadyRevoked — Revoke was called for an already revoked AID
// (revoked_at != NULL). Sentinel is separated so handler returns
// 409 instead of 404.
var ErrOperatorAlreadyRevoked = errors.New("operator: AID already revoked")

// pgErrCodeUniqueViolation — SQLSTATE for UNIQUE violation, including PK and
// partial unique index. Documented in pgerrcode, but in keeper/go.sum
// there is only an indirect dependency; we keep the constant locally to avoid
// pulling the package into the API.
const pgErrCodeUniqueViolation = "23505"

// pgErrCodeForeignKeyViolation — SQLSTATE for FK violation. For
// operators it occurs on `created_by_aid` (insert references a
// non-existent AID).
const pgErrCodeForeignKeyViolation = "23503"

// ExecQueryRower — a narrow subset of the pgxpool.Pool interface needed
// by CRUD. The narrowing allows unit-testing functions via a fake pool without
// spinning up Postgres; the real pool from keeper/internal/pg satisfies
// the interface automatically.
//
// Query — part of the subset for functions reading multiple rows;
// pgxpool.Pool/Conn/Tx satisfy it automatically.
//
// The type is exported so handlers/operator.go can type the pool
// in OperatorHandler without a dependency on pgxpool in the API layer.
type ExecQueryRower interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// execQueryRower — backwards-compatible alias (package-internal usage in
// crud.go and crud_test.go).
type execQueryRower = ExecQueryRower

// Compile-time check: pgxpool.Pool / pgx.Conn / pgx.Tx satisfy
// execQueryRower. All three are actually used (Pool in `keeper run`,
// Tx in bootstrap.Init, Conn — theoretically for custom snippets).
// pgx.Tx — interface, so `(pgx.Tx)(nil)` form without pointer.
var (
	_ execQueryRower = (*pgx.Conn)(nil)
	_ execQueryRower = (*pgxpool.Pool)(nil)
	_ execQueryRower = (pgx.Tx)(nil)
)

// insertOperatorSQL — INSERT with explicit mapping of all columns of the
// `operators` table (003_create_operators.up.sql). `created_at` is taken from
// DEFAULT NOW() if the caller did not set a value — behavior is symmetric with
// audit_log in `keeper/internal/auditpg`.
const insertOperatorSQL = `
INSERT INTO operators (aid, display_name, auth_method, created_at, created_by_aid, created_via, revoked_at, metadata)
VALUES ($1, $2, $3, COALESCE($4, NOW()), $5, $6, $7, $8)
`

// selectOperatorByAIDSQL — SELECT all columns of operators by PK.
const selectOperatorByAIDSQL = `
SELECT aid, display_name, auth_method, created_at, created_by_aid, created_via, revoked_at, metadata
FROM operators
WHERE aid = $1
`

const countOperatorsSQL = `SELECT COUNT(*) FROM operators`

// Excludes the system archon-system (ADR-013 amendment 2026-07-01).
const countNonSystemOperatorsSQL = `SELECT COUNT(*) FROM operators WHERE created_via <> 'system'`

// ListFilter — parameters for `GET /v1/operators`. Empty fields = "no filter".
// IncludeRevoked=false → SQL adds `revoked_at IS NULL` (returns only
// active; default UI behavior). IncludeRevoked=true → no filter on
// revoked_at (admin-view).
type ListFilter struct {
	AuthMethod     AuthMethod
	IncludeRevoked bool
	// Q — free substring search (ILIKE) by display_name/aid; "" = no filter.
	Q string
}

// Insert inserts a new Archon into the registry.
//
// Pre-conditions:
//   - op.AID matches [AIDPattern] (validated before round-trip).
//   - op.AuthMethod — one of the enums (jwt / mtls / combined / ldap / oidc;
//     ldap/oidc — federated authentication, ADR-058).
//   - op.DisplayName — non-empty (NOT NULL without DEFAULT in schema).
//
// Returns:
//   - [ErrOperatorAlreadyExists] on UNIQUE-violation (PK or
//     partial unique index `operators_first_archon_idx WHERE
//     created_via='bootstrap'` — migrations 084/085, ADR-058(d)).
//   - wrapped fmt.Errorf on FK-violation (`created_by_aid` references a
//     non-existent AID) with SQLSTATE mention — caller can
//     distinguish the case through the message.
//   - other pgx errors — unwrapped, passed as is.
func Insert(ctx context.Context, db execQueryRower, op *Operator) error {
	if op == nil {
		return fmt.Errorf("operator: nil operator")
	}
	if !ValidAID(op.AID) {
		return fmt.Errorf("operator: invalid AID %q (must match %s)", op.AID, AIDPattern)
	}
	if op.DisplayName == "" {
		return fmt.Errorf("operator: display_name is empty")
	}
	switch op.AuthMethod {
	case AuthMethodJWT, AuthMethodMTLS, AuthMethodCombined, AuthMethodLDAP, AuthMethodOIDC:
	default:
		return fmt.Errorf("operator: invalid auth_method %q", op.AuthMethod)
	}

	// created_via defaults to 'user' (ADR-058(d)): Operator API
	// (Service.Create) and legacy calls do not set the field — an operator created
	// via POST /v1/operators is by definition user. Bootstrap/system/ldap/oidc
	// set the value explicitly. Default is set here (not COALESCE in SQL)
	// so application validation below always sees the canonical value.
	createdVia := op.CreatedVia
	if createdVia == "" {
		createdVia = CreatedViaUser
	}
	switch createdVia {
	case CreatedViaBootstrap, CreatedViaUser, CreatedViaLDAP, CreatedViaOIDC, CreatedViaSystem:
	default:
		return fmt.Errorf("operator: invalid created_via %q", createdVia)
	}

	metadataBytes, err := marshalMetadata(op.Metadata)
	if err != nil {
		return fmt.Errorf("operator: marshal metadata: %w", err)
	}

	var createdAt any
	if !op.CreatedAt.IsZero() {
		createdAt = op.CreatedAt.UTC()
	}

	var createdByAID any
	if op.CreatedByAID != nil {
		createdByAID = *op.CreatedByAID
	}

	var revokedAt any
	if op.RevokedAt != nil {
		revokedAt = op.RevokedAt.UTC()
	}

	_, err = db.Exec(ctx, insertOperatorSQL,
		op.AID,
		op.DisplayName,
		string(op.AuthMethod),
		createdAt,
		createdByAID,
		createdVia,
		revokedAt,
		metadataBytes,
	)
	if err != nil {
		return mapInsertError(err)
	}
	return nil
}

// mapInsertError maps pgx errors to package sentinels. UNIQUE → general
// ErrOperatorAlreadyExists (caller knows from context whether this was
// a PK conflict on AID or partial unique on bootstrap invariant).
func mapInsertError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case pgErrCodeUniqueViolation:
			// Multi-wrap (Go 1.20+): both sentinel and original accessible
			// via errors.Is. Constraint name is in message for logs.
			return fmt.Errorf("%w (constraint %s): %w",
				ErrOperatorAlreadyExists, pgErr.ConstraintName, err)
		case pgErrCodeForeignKeyViolation:
			return fmt.Errorf("operator: FK violation on %s: %w", pgErr.ConstraintName, err)
		}
	}
	return fmt.Errorf("operator: insert: %w", err)
}

// SelectByAID reads an Operator by PK. Returns [ErrOperatorNotFound]
// on pgx.ErrNoRows.
func SelectByAID(ctx context.Context, db execQueryRower, aid string) (*Operator, error) {
	row := db.QueryRow(ctx, selectOperatorByAIDSQL, aid)
	return scanOperator(row)
}

// scanOperator — common Scan for a single operators row. Extracted so
// SelectByAID and future List functions read columns consistently.
func scanOperator(row pgx.Row) (*Operator, error) {
	var (
		op            Operator
		authMethodStr string
		createdByAID  *string
		metadataBytes []byte
	)
	err := row.Scan(
		&op.AID,
		&op.DisplayName,
		&authMethodStr,
		&op.CreatedAt,
		&createdByAID,
		&op.CreatedVia,
		&op.RevokedAt,
		&metadataBytes,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrOperatorNotFound
		}
		return nil, fmt.Errorf("operator: scan: %w", err)
	}
	op.AuthMethod = AuthMethod(authMethodStr)
	op.CreatedByAID = createdByAID
	if len(metadataBytes) > 0 {
		if err := json.Unmarshal(metadataBytes, &op.Metadata); err != nil {
			return nil, fmt.Errorf("operator: unmarshal metadata: %w", err)
		}
	}
	return &op, nil
}

// Count — total number of operators (including revoked and system).
// For bootstrap invariant, use [CountNonSystem].
func Count(ctx context.Context, db execQueryRower) (int64, error) {
	var n int64
	if err := db.QueryRow(ctx, countOperatorsSQL).Scan(&n); err != nil {
		return 0, fmt.Errorf("operator: count: %w", err)
	}
	return n, nil
}

// CountNonSystem — count of non-system operators (including revoked); basis
// for bootstrap invariant "registry is empty" (ADR-013 amendment 2026-07-01).
func CountNonSystem(ctx context.Context, db execQueryRower) (int64, error) {
	var n int64
	if err := db.QueryRow(ctx, countNonSystemOperatorsSQL).Scan(&n); err != nil {
		return 0, fmt.Errorf("operator: count non-system: %w", err)
	}
	return n, nil
}

// List returns a page of operators rows under filter, sorted by
// created_at DESC (newest first, parity with push_runs/incarnations/errands).
// total — COUNT(*) under the same filter without LIMIT/OFFSET (for UI pagination).
//
// Queries are simple without prepared statements — filter is small, prepared
// statements are reused by the driver.
func List(ctx context.Context, db execQueryRower, f ListFilter, offset, limit int) ([]*Operator, int, error) {
	whereSQL, args := buildOperatorWhere(f)

	var total int
	if err := db.QueryRow(ctx, "SELECT COUNT(*) FROM operators"+whereSQL, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("operator: list count: %w", err)
	}

	selectSQL := `SELECT aid, display_name, auth_method, created_at, created_by_aid, created_via, revoked_at, metadata
FROM operators` + whereSQL + `
ORDER BY created_at DESC
LIMIT $` + intToString(len(args)+1) + ` OFFSET $` + intToString(len(args)+2)
	args = append(args, limit, offset)

	rows, err := db.Query(ctx, selectSQL, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("operator: list select: %w", err)
	}
	defer rows.Close()

	out := make([]*Operator, 0, limit)
	for rows.Next() {
		op, err := scanOperator(rows)
		if err != nil {
			return nil, 0, fmt.Errorf("operator: list scan: %w", err)
		}
		out = append(out, op)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("operator: list iter: %w", err)
	}
	return out, total, nil
}

// buildOperatorWhere builds WHERE predicate for ListFilter. Returns
// string with leading " WHERE …" or "" if filter is empty.
func buildOperatorWhere(f ListFilter) (string, []any) {
	var (
		conds []string
		args  []any
	)
	if f.AuthMethod != "" {
		args = append(args, string(f.AuthMethod))
		conds = append(conds, "auth_method = $"+intToString(len(args)))
	}
	if f.Q != "" {
		args = append(args, "%"+escapeLike(f.Q)+"%")
		n := intToString(len(args))
		conds = append(conds, "(display_name ILIKE $"+n+" OR aid ILIKE $"+n+")")
	}
	if !f.IncludeRevoked {
		conds = append(conds, "revoked_at IS NULL")
	}
	if len(conds) == 0 {
		return "", args
	}
	out := " WHERE "
	for i, c := range conds {
		if i > 0 {
			out += " AND "
		}
		out += c
	}
	return out, args
}

// likeEscaper escapes ILIKE metacharacters (%/_/\) in free search Q —
// backslash-escape (PG default); backslash first to avoid double-escaping.
var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

// escapeLike prepares Q for substitution in `%…%` ILIKE pattern.
func escapeLike(s string) string { return likeEscaper.Replace(s) }

// intToString — small helper for building $N placeholders. Symmetric to
// errand.Store::itoa (inline strconv-free there for one place); operator-
// crud uses SCAN function three times, keep inline-impl for zero
// strconv dependency in list hot-path.
func intToString(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [10]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// marshalMetadata serializes metadata to JSON bytes for direct substitution
// into JSONB column (pgx supports this). nil → `{}`, symmetric with audit-payload.
func marshalMetadata(metadata map[string]any) ([]byte, error) {
	if metadata == nil {
		return []byte("{}"), nil
	}
	return json.Marshal(metadata)
}

// revokeOperatorSQL — UPDATE active operator: set revoked_at=NOW() +
// write `revoke_reason` to metadata. WHERE revoked_at IS NULL —
// atomic protection against repeated revoke (rows-affected = 0 → already revoked
// or does not exist, differentiate via SELECT in Revoke).
const revokeOperatorSQL = `
UPDATE operators
SET revoked_at = NOW(),
    metadata = jsonb_set(COALESCE(metadata, '{}'::jsonb), '{revoke_reason}', to_jsonb($2::text))
WHERE aid = $1 AND revoked_at IS NULL
`

// revokeOperatorNoReasonSQL — same without writing to metadata. Used
// when caller did not pass reason — otherwise jsonb_set would add
// `"revoke_reason":""`, cluttering metadata with empty keys.
const revokeOperatorNoReasonSQL = `
UPDATE operators
SET revoked_at = NOW()
WHERE aid = $1 AND revoked_at IS NULL
`

// Revoke sets revoked_at for active operator and saves reason to
// metadata.revoke_reason (only when reason is non-empty — see
// revokeOperatorNoReasonSQL).
//
// Semantics:
//   - aid does not exist in registry → [ErrOperatorNotFound].
//   - aid already revoked (revoked_at != NULL) → [ErrOperatorAlreadyRevoked].
//   - reason is empty — allowed (field is optional), metadata unchanged.
//
// Active JWTs of a revoked Archon continue to work until `exp`
// (ADR-014(d), PM-decision M0.6b #3 — JWT verify does not check revoked_at).
func Revoke(ctx context.Context, db execQueryRower, aid string, reason string) error {
	if !ValidAID(aid) {
		return fmt.Errorf("operator: invalid AID %q (must match %s)", aid, AIDPattern)
	}
	var (
		tag pgconn.CommandTag
		err error
	)
	if reason == "" {
		tag, err = db.Exec(ctx, revokeOperatorNoReasonSQL, aid)
	} else {
		tag, err = db.Exec(ctx, revokeOperatorSQL, aid, reason)
	}
	if err != nil {
		return fmt.Errorf("operator: revoke: %w", err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	// 0 rows — either AID does not exist or already revoked. Differentiate via
	// SelectByAID: important for UX handler to distinguish 404 vs 409.
	op, selErr := SelectByAID(ctx, db, aid)
	if errors.Is(selErr, ErrOperatorNotFound) {
		return ErrOperatorNotFound
	}
	if selErr != nil {
		return fmt.Errorf("operator: revoke probe: %w", selErr)
	}
	if op.RevokedAt != nil {
		return ErrOperatorAlreadyRevoked
	}
	// Should not happen: WHERE-clause or rows-affected returned 0, but
	// row is active. Return generic error as symptom of potential
	// race-condition (e.g., concurrent revoke).
	return fmt.Errorf("operator: revoke: 0 rows affected, but %q is active (concurrent revoke?)", aid)
}
