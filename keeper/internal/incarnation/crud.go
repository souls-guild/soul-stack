package incarnation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/souls-guild/soul-stack/keeper/internal/artifact"
	"github.com/souls-guild/soul-stack/keeper/internal/statemigrate"
	"github.com/souls-guild/soul-stack/shared/audit"
)

// Sentinel errors of the CRUD layer. The handler side maps:
//   - ErrIncarnationAlreadyExists → 409 incarnation-already-exists.
//   - ErrIncarnationNotFound      → 404 not-found.
//   - ErrIncarnationNotLocked     → 409 incarnation-locked (unlock is possible
//     from error_locked / migration_failed / destroy_failed; applying/ready/
//     destroying → rejected).
//   - ErrIncarnationBusy          → 409 (upgrade rejected: a run is in
//     progress, status=applying).
//   - ErrIncarnationLocked        → 409 (upgrade rejected: status is
//     error_locked / migration_failed — unlock required).
//   - ErrDowngradeUnsupported     → 409 (target schema version below current;
//     ADR-019 forward-only).
//   - ErrSchemaVersionMismatch    → 409 (current schema version didn't match
//     the chain: someone upgraded between resolve and FOR UPDATE).
var (
	ErrIncarnationAlreadyExists = errors.New("incarnation: name already exists")
	ErrIncarnationNotFound      = errors.New("incarnation: name not found")
	ErrIncarnationNotLocked     = errors.New("incarnation: not in unlockable status")
	// ErrIncarnationNotErrorLocked — status is not error_locked: rerun-last is
	// allowed only from error_locked (architecture.md → "Atomicity and
	// error_locked"; migration_failed/destroy_failed/etc. need a plain unlock +
	// manual run). Narrower than [ErrIncarnationNotLocked], which covers all
	// three blocking statuses.
	ErrIncarnationNotErrorLocked = errors.New("incarnation: not in error_locked status (rerun-last requires error_locked)")
	// ErrRerunInputUnavailable — rerun-last cannot recover the failed day-2
	// run's input: the last state_history snapshot points to an apply_run whose
	// recipe (`apply_runs.recipe`) is NULL. Causes: the run failed before
	// dispatch (render_failed/no_hosts/preflight, recipe-less terminal row from
	// ensureTerminalApplyRun); the legacy dispatchWave path (Insert(running)
	// carries no recipe); or the apply_run row was purged by Reaper retention
	// (purge_apply_runs). Fail-closed: rerunning without the saved input would
	// apply defaults or fail input validation, so instead we reject — operator
	// does a plain unlock and runs the scenario manually with explicit input.
	// The create path (last-failed == created_scenario) never hits this sentinel:
	// its input comes from incarnation.spec.input. Handler maps to 409.
	ErrRerunInputUnavailable = errors.New("incarnation: rerun-last cannot recover the failed run's input (recipe unavailable — use unlock + manual run with explicit input)")
	ErrIncarnationBusy       = errors.New("incarnation: run in progress (applying)")
	ErrIncarnationLocked     = errors.New("incarnation: locked — unlock required before upgrade")
	ErrDowngradeUnsupported  = errors.New("incarnation: schema downgrade unsupported (forward-only, ADR-019)")
	ErrSchemaVersionMismatch = errors.New("incarnation: current schema version does not match migration chain")
	// ErrAlreadyFinalized — single-winner state-commit (ADR-027(j), W1): the
	// incarnation row exists but is no longer in a working run status
	// (applying/destroying) — another handler won finalization (RunResult vs.
	// recovery takeover). Not an error: caller (commitSuccess/lockIncarnation)
	// treats it as a no-op (logs "already finalized by another"), same as
	// [DeleteAfterTeardown] treats RowsAffected==0 for DELETE.
	ErrAlreadyFinalized = errors.New("incarnation: already finalized by another committer")
	// ErrOrphanLockNotReleased — releasing an orphaned applying-lock was a
	// no-op: the incarnation is no longer applying, or the orphan apply_id
	// doesn't belong to it. Not a consistency error — caller (voyageorch
	// recovery seam, ADR-027(k)) treats it as "nothing to release" and
	// continues the re-run without releasing.
	ErrOrphanLockNotReleased = errors.New("incarnation: orphan applying-lock not released (no-op)")
)

const (
	pgErrCodeUniqueViolation     = "23505"
	pgErrCodeForeignKeyViolation = "23503"
	pgErrCodeCheckViolation      = "23514"
)

// ExecQueryRower — narrow pgxpool.Pool subset needed by CRUD. Mirrors
// [operator.ExecQueryRower]: unit tests use a fake (no PG needed),
// production passes a real pool/Conn/Tx.
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

// insertSQL — INSERT with RETURNING to get server-side created_at/updated_at
// (DEFAULT NOW()) in one round trip.
const insertSQL = `
INSERT INTO incarnation (
    name, service, service_version, state_schema_version,
    spec, state, status, status_details, created_by_aid, covens, traits,
    created_scenario
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
RETURNING created_at, updated_at
`

// selectByNameSQL — SELECT all columns by PK.
const selectByNameSQL = `
SELECT name, service, service_version, state_schema_version,
       spec, state, status, status_details, created_by_aid,
       created_at, updated_at, covens, traits,
       last_drift_check_at, last_drift_summary, created_scenario,
       applying_apply_id
FROM incarnation
WHERE name = $1
`

// StateOp — comparison operator for a state predicate ([StateEq]). Closed
// set; anything else → [ErrInvalidStateOp].
type StateOp string

const (
	StateOpEq  StateOp = "eq" // equality (text, jsonb ->> = $n)
	StateOpNe  StateOp = "ne" // inequality
	StateOpGt  StateOp = "gt" // greater-than (numeric cast ::numeric)
	StateOpGte StateOp = "gte"
	StateOpLt  StateOp = "lt"
	StateOpLte StateOp = "lte"
)

// numericStateOps — operators requiring a numeric comparison (both sides
// cast to ::numeric). The rest (eq/ne) compare jsonb ->> as text.
var numericStateOps = map[StateOp]string{
	StateOpGt:  ">",
	StateOpGte: ">=",
	StateOpLt:  "<",
	StateOpLte: "<=",
}

// textStateOps — text-comparison operators over jsonb ->>.
var textStateOps = map[StateOp]string{
	StateOpEq: "=",
	StateOpNe: "<>",
}

// StateEq — predicate over a `state` jsonb-column field (phase 1, jsonb
// pushdown). Path is the state object's top-level key (e.g. `redis_version`),
// validated against the [statePathPattern] whitelist — never concatenated as
// a SQL identifier unchecked. Value always goes as a bind param ($n), never
// into SQL text. MVP: top-level keys only (no nested a.b.c path); key
// existence isn't checked against the service's state_schema — a missing key
// yields `state->>'x' = $n` → NULL → empty result, a valid "nothing found".
type StateEq struct {
	Path  string
	Op    StateOp
	Value string
}

// SortDir — sort direction for [ListFilter.SortBy].
type SortDir string

const (
	SortAsc  SortDir = "asc"
	SortDesc SortDir = "desc"
)

// sortableColumns — base columns allowed in [ListFilter.SortBy] (closed
// whitelist; state fields use a separate `state.` prefix). Maps logical
// name → SQL expression (currently 1:1, in case they diverge later).
var sortableColumns = map[string]string{
	"created_at": "created_at",
	"name":       "name",
	"status":     "status",
	"service":    "service",
}

// statePathPrefix — sort-field prefix meaning "sort by jsonb state field"
// (`sort=state.redis_version`).
const statePathPrefix = "state."

// statePathPattern — format whitelist for a jsonb path key: lowercase,
// digits, underscore, first char a letter. Closes SQL injection via
// identifier — anything not matching → [ErrInvalidStatePath]. Key existence
// against state_schema isn't checked (see [StateEq]).
var statePathPattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// Sentinel errors for filter/sort validation. Handler maps all to 422.
var (
	ErrInvalidStatePath  = errors.New("incarnation: invalid state path (must match [a-z][a-z0-9_]*)")
	ErrInvalidStateOp    = errors.New("incarnation: invalid state predicate operator")
	ErrInvalidStateValue = errors.New("incarnation: invalid state predicate value (numeric operator requires a number)")
	ErrInvalidSortField  = errors.New("incarnation: invalid sort field")
	ErrInvalidSortDir    = errors.New("incarnation: invalid sort direction")
)

// ListFilter — filters for [SelectAll]. Empty fields mean "don't filter".
//
//   - Coven — exact any-of match against `incarnation.covens[]` (declared
//     env tags, ADR-008 amendment a); same predicate as `soul.ListFilter.Coven`
//     (souls.coven[]). Here it's an incarnation env tag (`prod`/`staging`),
//     not a host Coven label.
//   - StatePredicates — filters over `state` jsonb-column fields (jsonb
//     pushdown, phase 1), AND-combined with the base filters and each other.
//     Each predicate's Path is validated against [statePathPattern].
//   - SortBy/SortDir — sorting. SortBy is a base column from [sortableColumns]
//     or `state.<field>` (jsonb ->>); empty → legacy `created_at DESC, name
//     ASC`. SortDir defaults to asc. `name ASC` tie-break is always appended
//     (stable pagination).
type ListFilter struct {
	Service         string
	Status          Status
	Coven           string
	StatePredicates []StateEq
	SortBy          string
	SortDir         SortDir
}

// ListScope — RBAC scope visibility boundary (`GET /v1/incarnations`,
// ADR-047 S3b-3), separate from user-facing [ListFilter]: filter is what the
// operator asked to see (query params), scope is what they're allowed to see
// (from JWT, resolved by the handler via [rbac.Purview]). Both intersect with
// AND in WHERE (filter narrows inside scope, never the other way).
//
// Scope dimensions (Covens + StateNames + Traits) combine with OR ("anything
// I can access"): an incarnation is visible if it's in a scope coven, OR its
// state satisfies a scope state-predicate, OR its traits match a scope pair.
//
//   - Covens — coven∪{name} matcher (ADR-008 amendment a): matches both
//     `covens[] && ARRAY[Covens]` and `name = ANY(Covens)` (incarnation name
//     is the root Coven label) — broader than [ListFilter.Coven], which only
//     matches covens[].
//   - StateNames — names of incarnations whose state already satisfied the
//     scope's state-CEL predicates (StateExprs, ADR-047 S2c), resolved before
//     SQL via keeper/internal/statepredicate (no duplicate CEL engine), then
//     pushed down as `name = ANY(StateNames)` (keeps total/offset coherent,
//     no Go post-filter drift).
//   - Traits — `key:value` scalar-equality pairs over `incarnation.traits`
//     (ADR-047 amendment, ADR-060 §7 slice 1); each pair is a separate OR arm
//     `traits->>$key = $value` (scalar-only, not jsonb `@>` containment,
//     which would match list-Traits against a scalar RHS and diverge from the
//     GET path).
//
// Fail-closed semantics (ADR-047): an empty scope (Covens, StateNames, and
// Traits all empty) with !Unrestricted yields an always-false predicate — no
// incarnations, not the whole list. Unrestricted=true drops the scope filter
// entirely. The handler never passes an empty scope here (it short-circuits
// before hitting the DB), but the defensive branch below preserves fail-closed
// regardless.
type ListScope struct {
	Covens       []string
	StateNames   []string
	Traits       []TraitPair
	Unrestricted bool
}

// TraitPair — one `key:value` scope trait pair (ADR-047 amendment, ADR-060
// §7 slice 1). Scalar-only: matches `traits->>'<key>' = '<value>'`, aligned
// with the GET path [traitScalarEquals]. For a list-Trait, `->>` returns the
// array's text form ≠ the scalar value, so lists never match — unlike `@>`
// containment, which would match an array against a scalar RHS. Key/value
// are separate bind params, never concatenated into SQL text.
type TraitPair struct {
	Key   string
	Value string
}

// Create inserts a new incarnation. status is set by the caller (handler
// passes [StatusReady]; the scenario runner sets applying while a run is in
// progress).
//
// Pre-conditions:
//   - inc.Name matches [NamePattern];
//   - inc.Service / inc.ServiceVersion are non-empty;
//   - inc.Status is one of the valid statuses.
//
// Returns:
//   - [ErrIncarnationAlreadyExists] on UNIQUE violation on PK.
//   - wrapped fmt.Errorf on FK violation (`created_by_aid` references a
//     nonexistent AID) and CHECK violation (status/name format).
func Create(ctx context.Context, db ExecQueryRower, inc *Incarnation) error {
	if inc == nil {
		return fmt.Errorf("incarnation: nil incarnation")
	}
	if !ValidName(inc.Name) {
		return fmt.Errorf("incarnation: invalid name %q (must match %s)", inc.Name, NamePattern)
	}
	if inc.Service == "" {
		return fmt.Errorf("incarnation: service is empty")
	}
	if inc.ServiceVersion == "" {
		return fmt.Errorf("incarnation: service_version is empty")
	}
	if !ValidStatus(inc.Status) {
		return fmt.Errorf("incarnation: invalid status %q", inc.Status)
	}
	if inc.StateSchemaVersion <= 0 {
		// 1 is the canonical starting version (ADR-019); 0/negative is a caller
		// bug.
		return fmt.Errorf("incarnation: state_schema_version must be > 0, got %d", inc.StateSchemaVersion)
	}

	specBytes, err := marshalJSONB(inc.Spec)
	if err != nil {
		return fmt.Errorf("incarnation: marshal spec: %w", err)
	}
	stateBytes, err := marshalJSONB(inc.State)
	if err != nil {
		return fmt.Errorf("incarnation: marshal state: %w", err)
	}
	var statusDetailsArg any
	if inc.StatusDetails != nil {
		b, err := json.Marshal(inc.StatusDetails)
		if err != nil {
			return fmt.Errorf("incarnation: marshal status_details: %w", err)
		}
		statusDetailsArg = b
	}
	var createdByAID any
	if inc.CreatedByAID != nil {
		createdByAID = *inc.CreatedByAID
	}

	// covens — NOT NULL DEFAULT '{}': nil-slice encode as empty array
	// (pgx would otherwise pass NULL → violation NOT NULL).
	covens := inc.Covens
	if covens == nil {
		covens = []string{}
	}

	// traits — NOT NULL DEFAULT '{}'::jsonb (ADR-060 amend, R1): nil/empty map →
	// `{}` ([]byte), same as marshalTraitPayload in bulk-write. pgx-codec-auto for
	// jsonb deliberately not used — consistent with Spec/State and souls.traits.
	traitsBytes, err := marshalJSONB(inc.Traits)
	if err != nil {
		return fmt.Errorf("incarnation: marshal traits: %w", err)
	}

	// created_scenario — name of the startup scenario (multi-create mechanism,
	// migrations 089+090). NULLABLE: nil-pointer from caller = bare incarnation
	// (created without bootstrap scenario) → NULL in DB. No normalization ""→'create'
	// — bare and explicit 'create' differ at type level (*string).
	var createdScenario any
	if inc.CreatedScenario != nil {
		createdScenario = *inc.CreatedScenario
	}

	row := db.QueryRow(ctx, insertSQL,
		inc.Name,
		inc.Service,
		inc.ServiceVersion,
		inc.StateSchemaVersion,
		specBytes,
		stateBytes,
		string(inc.Status),
		statusDetailsArg,
		createdByAID,
		covens,
		traitsBytes,
		createdScenario,
	)
	if err := row.Scan(&inc.CreatedAt, &inc.UpdatedAt); err != nil {
		return mapInsertError(err)
	}
	return nil
}

func mapInsertError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case pgErrCodeUniqueViolation:
			return fmt.Errorf("%w (constraint %s): %w",
				ErrIncarnationAlreadyExists, pgErr.ConstraintName, err)
		case pgErrCodeForeignKeyViolation:
			return fmt.Errorf("incarnation: FK violation on %s: %w", pgErr.ConstraintName, err)
		case pgErrCodeCheckViolation:
			return fmt.Errorf("incarnation: CHECK violation on %s: %w", pgErr.ConstraintName, err)
		}
	}
	return fmt.Errorf("incarnation: insert: %w", err)
}

// SelectByName reads incarnation by PK. [ErrIncarnationNotFound] on
// pgx.ErrNoRows.
func SelectByName(ctx context.Context, db ExecQueryRower, name string) (*Incarnation, error) {
	row := db.QueryRow(ctx, selectByNameSQL, name)
	return scanIncarnation(row)
}

func scanIncarnation(row pgx.Row) (*Incarnation, error) {
	var (
		inc                Incarnation
		statusStr          string
		specBytes          []byte
		stateBytes         []byte
		statusDetailsBytes []byte
		createdByAID       *string
		traitsBytes        []byte
		driftSummaryBytes  []byte
	)
	err := row.Scan(
		&inc.Name,
		&inc.Service,
		&inc.ServiceVersion,
		&inc.StateSchemaVersion,
		&specBytes,
		&stateBytes,
		&statusStr,
		&statusDetailsBytes,
		&createdByAID,
		&inc.CreatedAt,
		&inc.UpdatedAt,
		&inc.Covens,
		&traitsBytes,
		&inc.LastDriftCheckAt,
		&driftSummaryBytes,
		&inc.CreatedScenario,
		&inc.ApplyingApplyID, // ADR-068 §A1: non-null while applying, null on terminal
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrIncarnationNotFound
		}
		return nil, fmt.Errorf("incarnation: scan: %w", err)
	}
	inc.Status = Status(statusStr)
	inc.CreatedByAID = createdByAID
	if inc.Spec, err = unmarshalJSONB(specBytes); err != nil {
		return nil, fmt.Errorf("incarnation: unmarshal spec: %w", err)
	}
	if inc.State, err = unmarshalJSONB(stateBytes); err != nil {
		return nil, fmt.Errorf("incarnation: unmarshal state: %w", err)
	}
	// traits jsonb (ADR-060 amend, R1): `{}` (NOT NULL DEFAULT) → empty map, not
	// nil (read/projection path doesn't distinguish "no column" / "no tags").
	if inc.Traits, err = unmarshalJSONB(traitsBytes); err != nil {
		return nil, fmt.Errorf("incarnation: unmarshal traits: %w", err)
	}
	if len(statusDetailsBytes) > 0 {
		if err := json.Unmarshal(statusDetailsBytes, &inc.StatusDetails); err != nil {
			return nil, fmt.Errorf("incarnation: unmarshal status_details: %w", err)
		}
	}
	if len(driftSummaryBytes) > 0 {
		var summary DriftScanSummary
		if err := json.Unmarshal(driftSummaryBytes, &summary); err != nil {
			return nil, fmt.Errorf("incarnation: unmarshal last_drift_summary: %w", err)
		}
		inc.LastDriftSummary = &summary
	}
	return &inc, nil
}

// SelectAll returns a page of incarnations with applied filter and
// total count of items matching the filter (without offset/limit).
//
// Sorting is controlled by [ListFilter.SortBy]/[SortDir]; by default (empty
// SortBy) — legacy order `created_at DESC, name ASC` (latest first; tie-break
// by name, otherwise pagination unstable at same timestamp). Tie-break
// `name ASC` always appended.
//
// State filters ([ListFilter.StatePredicates]) and sort by `state.<field>`
// validated before any DB query ([ErrInvalidStatePath]/[ErrInvalidStateOp]/
// [ErrInvalidSortField]/[ErrInvalidSortDir]): injection via jsonb path
// doesn't reach PG, values go as bind params.
//
// Total and items obtained by two separate queries outside a common
// transaction — total at this endpoint is **eventually consistent**: new
// incarnation appearing between COUNT and SELECT will give total one
// greater than actual items on current page. Deliberate choice: explicit
// transaction (REPEATABLE READ) for consistent count costs more than
// acceptable pagination drift in UI.
func SelectAll(ctx context.Context, db ExecQueryRower, filter ListFilter, scope ListScope, offset, limit int) ([]*Incarnation, int, error) {
	if offset < 0 {
		return nil, 0, fmt.Errorf("incarnation: offset must be >= 0, got %d", offset)
	}
	if limit < 1 {
		return nil, 0, fmt.Errorf("incarnation: limit must be >= 1, got %d", limit)
	}

	whereSQL, args, err := buildListWhere(filter, scope)
	if err != nil {
		return nil, 0, err
	}
	orderSQL, err := buildListOrderBy(filter)
	if err != nil {
		return nil, 0, err
	}

	// Total without offset/limit.
	countSQL := "SELECT COUNT(*) FROM incarnation" + whereSQL
	var total int
	if err := db.QueryRow(ctx, countSQL, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("incarnation: count: %w", err)
	}

	// Items with offset/limit, appended to the same args.
	listSQL := `SELECT name, service, service_version, state_schema_version,
       spec, state, status, status_details, created_by_aid,
       created_at, updated_at, covens, traits,
       last_drift_check_at, last_drift_summary, created_scenario,
       applying_apply_id
FROM incarnation` + whereSQL + orderSQL +
		fmt.Sprintf(" OFFSET $%d LIMIT $%d", len(args)+1, len(args)+2)
	listArgs := append(append([]any{}, args...), offset, limit)

	rows, err := db.Query(ctx, listSQL, listArgs...)
	if err != nil {
		return nil, 0, fmt.Errorf("incarnation: list query: %w", err)
	}
	defer rows.Close()

	var out []*Incarnation
	for rows.Next() {
		inc, err := scanIncarnation(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, inc)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("incarnation: list iter: %w", err)
	}
	return out, total, nil
}

// buildListWhere builds WHERE-clause and args from non-empty filter fields.
// Returns empty string and nil-args if no filters. State predicates
// validated (path-whitelist + closed operator set + numeric value for
// numeric operators); invalid →
// [ErrInvalidStatePath]/[ErrInvalidStateOp]/[ErrInvalidStateValue] without
// DB access.
func buildListWhere(f ListFilter, scope ListScope) (string, []any, error) {
	var (
		clauses []string
		args    []any
	)
	if f.Service != "" {
		args = append(args, f.Service)
		clauses = append(clauses, fmt.Sprintf("service = $%d", len(args)))
	}
	if f.Status != "" {
		args = append(args, string(f.Status))
		clauses = append(clauses, fmt.Sprintf("status = $%d", len(args)))
	}
	if f.Coven != "" {
		args = append(args, f.Coven)
		clauses = append(clauses, fmt.Sprintf("$%d = ANY(covens)", len(args)))
	}
	for _, p := range f.StatePredicates {
		if !statePathPattern.MatchString(p.Path) {
			return "", nil, fmt.Errorf("%w: %q", ErrInvalidStatePath, p.Path)
		}
		// For numeric operators value goes to SQL as `$n::numeric`.
		// Non-numeric value (e.g., typo `gt:abc`) would cause PG cast error
		// 22P02 → 500; catch here before DB query (handler maps
		// ErrInvalidStateValue to 422). eq/ne are text, numeric validation
		// doesn't apply to them.
		if _, numeric := numericStateOps[p.Op]; numeric {
			if _, err := strconv.ParseFloat(p.Value, 64); err != nil {
				return "", nil, fmt.Errorf("%w: %q for operator %q", ErrInvalidStateValue, p.Value, p.Op)
			}
		}
		args = append(args, p.Value)
		placeholder := fmt.Sprintf("$%d", len(args))
		clause, err := stateClause(p.Path, p.Op, placeholder)
		if err != nil {
			return "", nil, err
		}
		clauses = append(clauses, clause)
	}

	// RBAC scope predicate (ADR-047 S3b-3): AND with user filter.
	clauses, args = appendScopeClause(clauses, args, scope)

	if len(clauses) == 0 {
		return "", nil, nil
	}
	where := " WHERE " + clauses[0]
	for _, c := range clauses[1:] {
		where += " AND " + c
	}
	return where, args, nil
}

// appendScopeClause adds RBAC scope predicate (`GET /v1/incarnations`,
// ADR-047 S3b-3 + trait amendment) as single AND-clause to user filter.
// Within scope, dimensions (coven∪{name} ∪ state-names ∪ traits) combined
// with OR — single parenthesized block to avoid leaking through neighboring
// filter AND-clauses:
//
//		((covens && ARRAY[$c] OR name = ANY($c)) OR name = ANY($s) OR traits->>$tk = $tv)
//
//	  - coven∪{name}: scope-coven matches incarnation by both covens[]-intersection
//	    and name equality (ADR-008: incarnation name is root Coven label).
//	  - state-names: pre-resolved names of incarnations whose state satisfied
//	    state-CEL scope (StateExprs) — come as set, matched via `name = ANY`.
//	  - traits: each scope pair (`key:value`, ADR-060 §7 slice 1) — separate
//	    arm `traits->>$tk = $tv` (scalar-equality, scalar-only — NOT containment
//	    `@>`, which with scalar RHS would match list-Trait, diverging from GET path
//	    [traitScalarEquals]); key/value go as separate bind params,
//	    not concatenated to SQL text.
//
// Unrestricted — no restriction (scope removed). fail-closed: empty scope (no
// coven, state-names, or traits) with !Unrestricted gives `FALSE` — zero
// incarnations (not full list). Symmetric with [soul.appendScopeClause].
func appendScopeClause(clauses []string, args []any, scope ListScope) ([]string, []any) {
	cond, args := ScopeCondition(args, scope)
	if cond == "" {
		return clauses, args
	}
	return append(clauses, cond), args
}

// ScopeCondition — reusable SQL form of scope predicate [ListScope] over
// incarnation table columns (covens/name/traits). Exported for embedding
// in other queries as subquery `... IN (SELECT name FROM incarnation
// WHERE <cond>)` (global read-view of runs, applyrun) — single source of
// scope semantics with [SelectAll]. Placeholder numbering continues from
// passed args. Unrestricted → empty condition; empty scope → `FALSE` (fail-closed).
func ScopeCondition(args []any, scope ListScope) (string, []any) {
	if scope.Unrestricted {
		return "", args
	}

	var dims []string
	if len(scope.Covens) > 0 {
		args = append(args, scope.Covens)
		pos := len(args)
		// coven∪{name}: intersection of covens[] OR name ∈ scope-covens.
		dims = append(dims, fmt.Sprintf("(covens && $%d OR name = ANY($%d))", pos, pos))
	}
	if len(scope.StateNames) > 0 {
		args = append(args, scope.StateNames)
		dims = append(dims, fmt.Sprintf("name = ANY($%d)", len(args)))
	}
	for _, tp := range scope.Traits {
		// scalar-equality arm (slice 1 — scalar-only): `traits->>$key = $value`.
		// NOT jsonb-containment `@>`: PG containment with scalar RHS MATCHES arrays
		// (`{"env":["prod","stage"]} @> {"env":"prod"}` = TRUE — array-contains-
		// primitive, PG §8.14.3), so list-Trait entered List but GET path
		// ([traitScalarEquals]) doesn't see it (list → false) — List↔Get out of sync.
		// `traits->>'<key>'` for array gives its TEXT (`["prod", "stage"]`) ≠
		// '<value>' → list does NOT match, same as traitScalarEquals (both scalar-only,
		// semantics aligned). Key and value are separate bind params (not
		// concatenated to SQL text, no injection via key possible).
		args = append(args, tp.Key)
		keyPos := len(args)
		args = append(args, tp.Value)
		dims = append(dims, fmt.Sprintf("traits->>$%d = $%d", keyPos, len(args)))
	}

	if len(dims) == 0 {
		// fail-closed: scope introduced (not Unrestricted) but empty by dimensions —
		// zero visible incarnations. Deterministic FALSE, not "full list".
		return "FALSE", args
	}

	scopeClause := dims[0]
	for _, d := range dims[1:] {
		scopeClause += " OR " + d
	}
	return "(" + scopeClause + ")", args
}

// stateClause builds single jsonb-pushdown predicate. Path already passed
// [statePathPattern] (safe as identifier), placeholder is bind ($n).
// Text operators — `state->>'path' OP $n`; numeric — both operands
// cast to ::numeric (`(state->>'path')::numeric OP $n::numeric`).
func stateClause(path string, op StateOp, placeholder string) (string, error) {
	if sqlOp, ok := textStateOps[op]; ok {
		return fmt.Sprintf("state->>'%s' %s %s", path, sqlOp, placeholder), nil
	}
	if sqlOp, ok := numericStateOps[op]; ok {
		return fmt.Sprintf("(state->>'%s')::numeric %s %s::numeric", path, sqlOp, placeholder), nil
	}
	return "", fmt.Errorf("%w: %q", ErrInvalidStateOp, op)
}

// buildListOrderBy builds ORDER BY-clause from [ListFilter.SortBy]/[SortDir].
// Empty SortBy → legacy `created_at DESC, name ASC`. Otherwise: base column
// from [sortableColumns] or `state.<field>` (jsonb >>, path-whitelist), with
// direction from SortDir (default asc) and mandatory tie-break `name ASC`.
func buildListOrderBy(f ListFilter) (string, error) {
	if f.SortBy == "" {
		return " ORDER BY created_at DESC, name ASC", nil
	}

	dir, err := sortDirSQL(f.SortDir)
	if err != nil {
		return "", err
	}

	var expr string
	if strings.HasPrefix(f.SortBy, statePathPrefix) {
		path := strings.TrimPrefix(f.SortBy, statePathPrefix)
		if !statePathPattern.MatchString(path) {
			return "", fmt.Errorf("%w: %q", ErrInvalidStatePath, path)
		}
		expr = fmt.Sprintf("state->>'%s'", path)
	} else {
		col, ok := sortableColumns[f.SortBy]
		if !ok {
			return "", fmt.Errorf("%w: %q", ErrInvalidSortField, f.SortBy)
		}
		expr = col
	}
	return fmt.Sprintf(" ORDER BY %s %s, name ASC", expr, dir), nil
}

// sortDirSQL validates sort direction. Empty → ASC (default).
func sortDirSQL(d SortDir) (string, error) {
	switch d {
	case "", SortAsc:
		return "ASC", nil
	case SortDesc:
		return "DESC", nil
	}
	return "", fmt.Errorf("%w: %q", ErrInvalidSortDir, d)
}

// finalizableStatuses — working (non-terminal) run statuses from which
// single-winner final commit ([UpdateStateFromRun], ADR-027(j) W1) is allowed:
//   - applying   — normal run (lockRun transitioned here on start);
//   - destroying — teardown scenario `destroy` (incarnation stays in
//     destroying throughout run, S-D1/S-D2b; final write — only
//     failure transition to destroy_failed via lockIncarnation).
//
// Any other (terminal or already-seized) status → row won by another
// committer: final UPDATE gives RowsAffected==0 → [ErrAlreadyFinalized].
var finalizableStatuses = map[Status]struct{}{
	StatusApplying:   {},
	StatusDestroying: {},
}

// UpdateStateFromRun — atomic commit of run result `RunResult`
// (ADR-009, M2.4) with single-winner state-commit (ADR-027(j), W1). Within single
// transaction:
//
//  1. INSERT to `state_history` transition snapshot (state_before/_after,
//     scenario, apply_id).
//  2. Single-winner UPDATE incarnation.state + status + status_details +
//     updated_at = NOW() with guard `WHERE name=$1 AND status IN
//     ('applying','destroying')` + RETURNING. Only one handler wins
//     the row: in race of recovery takeover vs original RunResult no double
//     commit terminal happens (symmetric with single-winner DELETE in
//     [DeleteAfterTeardown]). Guard replaces former SELECT … FOR UPDATE —
//     atomic CAS-UPDATE serializes concurrent commits itself.
//
// When status = `error_locked` state doesn't change (caller typically passes
// stateAfter == stateBefore = current state); state_history still
// written (capture the fact of failed run itself).
//
// `apply_id` — ULID of run (RunResult.apply_id); goes to both state_history
// and audit via caller. Audit write itself done by event handler one level
// up (after commit, so DB consistency doesn't depend on audit).
//
// `changedByAID` = nil — Soul initiates run without Archon identity
// (`source: soul_grpc`, see ADR-022). Non-nil allowed for future
// case when run triggered via Operator API and AID known.
//
// Returns:
//   - [ErrIncarnationNotFound]  — no row with this name at all.
//   - [ErrAlreadyFinalized]     — row exists but status no longer applying/
//     destroying: another committer won finalization (no-op, NOT panic —
//     caller logs and continues). Transaction rolls back (caller
//     via pgx.BeginFunc), orphaned state_history snapshot doesn't remain.
func UpdateStateFromRun(
	ctx context.Context,
	tx ExecQueryRower,
	name, scenario, applyID string,
	stateBefore, stateAfter map[string]any,
	status Status,
	statusDetails map[string]any,
	changedByAID *string,
	historyID string,
) error {
	if !ValidName(name) {
		return fmt.Errorf("incarnation: invalid name %q", name)
	}
	if !ValidStatus(status) {
		return fmt.Errorf("incarnation: invalid status %q", status)
	}
	if applyID == "" {
		return fmt.Errorf("incarnation: empty apply_id")
	}
	if historyID == "" {
		return fmt.Errorf("incarnation: empty history_id")
	}

	stateBeforeBytes, err := marshalJSONB(stateBefore)
	if err != nil {
		return fmt.Errorf("incarnation: marshal state_before: %w", err)
	}
	stateAfterBytes, err := marshalJSONB(stateAfter)
	if err != nil {
		return fmt.Errorf("incarnation: marshal state_after: %w", err)
	}
	var statusDetailsArg any
	if statusDetails != nil {
		b, err := json.Marshal(statusDetails)
		if err != nil {
			return fmt.Errorf("incarnation: marshal status_details: %w", err)
		}
		statusDetailsArg = b
	}
	var changedByArg any
	if changedByAID != nil {
		changedByArg = *changedByAID
	}

	const historyInsertSQL = `
INSERT INTO state_history (
    history_id, incarnation_name, scenario, state_before, state_after,
    changed_by_aid, apply_id
) VALUES ($1, $2, $3, $4, $5, $6, $7)
`
	if _, err := tx.Exec(ctx, historyInsertSQL,
		historyID, name, scenario, stateBeforeBytes, stateAfterBytes, changedByArg, applyID,
	); err != nil {
		return fmt.Errorf("incarnation: insert state_history: %w", err)
	}

	// Single-winner guard: commit succeeds ONLY if row still in working run status
	// (applying / destroying). RETURNING name returns row to winner; empty result
	// (pgx.ErrNoRows) = row doesn't exist OR status already changed — disambiguate
	// via status probe below (MarkDispatched pattern). Run terminal clears applying-flag
	// AND zeroes its epoch (ADR-027 amend (m-S1)): applying_apply_id/applying_attempt/
	// applying_by_kid/applying_since → NULL atomically with status change.
	// This single success/fail/abort/RunResult-terminal point of run (commitSuccess /
	// lockIncarnation / correlateRunResult call UpdateStateFromRun) — cleanup here
	// covers all exits from applying, leaving row without "stale" epoch (else next
	// run/Reaper sees stale applying_since from previous owner). destroying carries no
	// epoch (lockRun writes epoch for applying only), but unconditional zeroing is
	// harmless and maintains invariant "non-applying ⇒ epoch NULL".
	const updateSQL = `
UPDATE incarnation
SET state             = $2,
    status            = $3,
    status_details    = $4,
    applying_apply_id = NULL,
    applying_attempt  = NULL,
    applying_by_kid   = NULL,
    applying_since    = NULL,
    updated_at        = NOW()
WHERE name = $1 AND status IN ('applying', 'destroying')
RETURNING name
`
	var returnedName string
	err = tx.QueryRow(ctx, updateSQL, name, stateAfterBytes, string(status), statusDetailsArg).Scan(&returnedName)
	if err == nil {
		return nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("incarnation: update state: %w", err)
	}

	// 0 rows: either row doesn't exist at all, or status already terminal/seized.
	// Status probe disambiguates not-found (caller contract preserved) and
	// already-finalized (single-winner no-op, someone won the row first).
	const probeStatusSQL = `SELECT status FROM incarnation WHERE name = $1`
	var statusStr string
	if perr := tx.QueryRow(ctx, probeStatusSQL, name).Scan(&statusStr); perr != nil {
		if errors.Is(perr, pgx.ErrNoRows) {
			return ErrIncarnationNotFound
		}
		return fmt.Errorf("incarnation: update state probe: %w", perr)
	}
	if _, ok := finalizableStatuses[Status(statusStr)]; ok {
		// Status still working, but UPDATE touched no rows — only possible reason:
		// read race within single tx (theoretically unreachable under snapshot tx).
		// Return not-found semantics as defensive default, not silent no-op.
		return ErrIncarnationNotFound
	}
	return fmt.Errorf("%w (status=%s)", ErrAlreadyFinalized, statusStr)
}

// TxBeginner — narrow subset of [pgxpool.Pool] needed for transactional
// operations (FOR UPDATE → check → mutate → commit in single atomic block).
// Real `*pgxpool.Pool` satisfies automatically; unit tests provide
// fake returning fake-tx.
type TxBeginner interface {
	BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error)
}

var _ TxBeginner = (*pgxpool.Pool)(nil)

// UnlockResult — result of [Unlock]: status before block removal (for reply / audit) and
// identifier of recorded state_history snapshot.
//
// Scenario filled ONLY by [UnlockForRerun] (for [Unlock] — ""): name
// of scenario caller reruns via runner.Start. This is the last
// failed scenario of incarnation (latest state_history snapshot) — bootstrap
// scenario on create path (== incarnation.created_scenario) OR day-2 scenario
// (add_user / update_acl / …). Replaces former hardcode "rerun only
// created_scenario": rerun-last reruns the actually failed operation.
//
// Input filled ONLY by [UnlockForRerun] (for [Unlock] — nil): input of failed
// run. On create path — saved operator-input incarnation.spec.input
// (read under same FOR UPDATE). On day-2 path — input from recipe of failed
// apply_run (`apply_runs.recipe.input`, invariant A: vault-ref as strings, secrets
// not revealed). Caller passes it to RunSpec.Input — rerun-last
// recovers failure with SAME input values (version/shards/user/…),
// not defaults. nil = scenario without input.
type UnlockResult struct {
	PreviousStatus Status
	HistoryID      string
	Scenario       string
	Input          map[string]any
	// FromUpgrade — failed run was upgrade scenario (recipe.from_upgrade,
	// ADR-0068): rerun-last must rerun it from upgrade/<slug>/, not
	// scenario/<slug>/. Filled only by day-2 path [UnlockForRerun] (create
	// never upgrade → false). Caller passes to RunSpec.FromUpgrade.
	FromUpgrade bool
}

// InputFromSpec extracts `input` key from freeform jsonb object: either
// incarnation.spec (create path) or apply_run recipe (day-2 path, [UnlockForRerun]).
// Missing key / non-object form → nil without error (jsonb freeform).
func InputFromSpec(spec map[string]any) map[string]any {
	if spec == nil {
		return nil
	}
	raw, ok := spec["input"]
	if !ok {
		return nil
	}
	m, ok := raw.(map[string]any)
	if !ok {
		return nil
	}
	return m
}

// unlockScenarioLabel — value for `state_history.scenario` on unlock transition.
// Unlock — not a scenario run, but state_history requires non-null scenario;
// capture the fact of manual block removal under this label.
const unlockScenarioLabel = "unlock"

// Unlock removes blocking status (ADR-009 / ADR-019): transitions
// incarnation error_locked → ready, migration_failed → ready OR
// destroy_failed → ready and writes snapshot-row to state_history (state does NOT
// change — last known-good preserved; unlock doesn't roll back or complete
// hosts, operator takes responsibility for consistency, architecture.md →
// "Atomicity and error_locked").
//
// migration_failed removed as safely as error_locked: migration
// is atomic in single tx, on failure rollback leaves pre-reform
// (consistent) state, so unlock returns incarnation to working
// state without risk of half-applied migration (ADR-019, atomicity).
//
// destroy_failed (S-D2a) removed same way: teardown works with hosts,
// not jsonb-state, so on failed teardown state remains
// last known-good — unlock returns incarnation to ready without risk
// of state-graph divergence. Operator thereby rejects destroy and takes
// instance back to work (alternatives — retry destroy / force-remove —
// appear in S-D2b/S-D3).
//
// Atomicity: single transaction SELECT … FOR UPDATE → status check →
// INSERT state_history → UPDATE status. FOR UPDATE serializes unlock
// relative to concurrent scenario-runner (its lockRun locks same
// row).
//
// Returns:
//   - [ErrIncarnationNotFound] — name doesn't exist (404).
//   - [ErrIncarnationNotLocked] — status not error_locked, not migration_failed
//     and not destroy_failed (409): can't unlock ready/applying/destroying.
//
// reason written to audit-payload by caller (state_history schema MVP doesn't
// carry metadata columns); previous_status returned in [UnlockResult].
func Unlock(ctx context.Context, pool TxBeginner, name, reason, unlockedByAID, historyID string) (*UnlockResult, error) {
	if !ValidName(name) {
		return nil, fmt.Errorf("incarnation: invalid name %q", name)
	}
	if reason == "" {
		return nil, fmt.Errorf("incarnation: unlock reason is empty")
	}
	if historyID == "" {
		return nil, fmt.Errorf("incarnation: empty history_id")
	}

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("incarnation: begin unlock tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	const selectForUpdateSQL = `
SELECT state, status
FROM incarnation
WHERE name = $1
FOR UPDATE
`
	var (
		stateBytes []byte
		statusStr  string
	)
	if err := tx.QueryRow(ctx, selectForUpdateSQL, name).Scan(&stateBytes, &statusStr); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrIncarnationNotFound
		}
		return nil, fmt.Errorf("incarnation: unlock select: %w", err)
	}
	previous := Status(statusStr)
	if previous != StatusErrorLocked && previous != StatusMigrationFailed && previous != StatusDestroyFailed {
		return nil, ErrIncarnationNotLocked
	}

	var changedByArg any
	if unlockedByAID != "" {
		changedByArg = unlockedByAID
	}

	// state_before == state_after: unlock doesn't change state (ADR-009).
	// apply_id = history_id ($1): unlock not tied to apply run (schema
	// requires NOT NULL, no FK to apply_runs) — substitute history_id as
	// unique non-null marker.
	const historyInsertSQL = `
INSERT INTO state_history (
    history_id, incarnation_name, scenario, state_before, state_after,
    changed_by_aid, apply_id
) VALUES ($1, $2, $3, $4, $4, $5, $1)
`
	if _, err := tx.Exec(ctx, historyInsertSQL,
		historyID, name, unlockScenarioLabel, stateBytes, changedByArg,
	); err != nil {
		return nil, fmt.Errorf("incarnation: insert unlock state_history: %w", err)
	}

	// status → ready, status_details reset (block removed).
	const updateSQL = `
UPDATE incarnation
SET status = $2, status_details = NULL, updated_at = NOW()
WHERE name = $1
`
	if _, err := tx.Exec(ctx, updateSQL, name, string(StatusReady)); err != nil {
		return nil, fmt.Errorf("incarnation: unlock update: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("incarnation: commit unlock tx: %w", err)
	}
	return &UnlockResult{PreviousStatus: previous, HistoryID: historyID}, nil
}

// orphanReleaseScenarioLabel — value for `state_history.scenario` on orphan lock release
// transition (ADR-027(k) recovery seam). Separate label from unlockScenarioLabel: this
// is NOT manual operator-unlock, but automatic reconciliation of dangling lock by
// reclaimed Voyage owner — history trace should differ for triage.
const orphanReleaseScenarioLabel = "voyage-orphan-release"

// ReleaseApplyingOrphan atomically releases an orphaned applying-lock from an incarnation
// remaining from a scenario-run of a dead Keeper-owner Voyage (recovery seam,
// ADR-027(k) / ADR-043). A reclaimed VoyageWorker calls this BEFORE re-spawning
// per-incarnation scenario-run: without release, lockRun rejects re-run
// (incarnation already applying), and Voyage would hang forever.
//
// Release = applying → ready (state NOT touched — last known-good preserved,
// symmetric to [Unlock]; orphan-run of dead owner didn't reach state-commit,
// so last-good = pre-run state). status_details are reset.
//
// orphanApplyID — apply_id of the orphaned run (back-link from voyage_targets
// of this Voyage from the previous attempt — caller already proved apply_id↔
// incarnation↔voyage binding by retrieving it from voyage_targets[name]). Caller
// (voyageorch) must perform fencing-checks (reclaimed-attempt +
// VerifyOwnership) BEFORE calling; here — FENCING-1 (protection against alien live-lock) +
// single-winner CAS under one FOR UPDATE:
//
//   - FENCING-1 (no-live-rival): release lock ONLY if incarnation HAS NO
//     active (non-terminal) apply_run with apply_id ≠ orphanApplyID. Alien
//     live run (direct run / other Voyage, started between crash and
//     reclaim) holds active apply_runs-row with its own apply_id → its presence
//     blocks release ([ErrOrphanLockNotReleased]). Protection against releasing alien
//     live-lock. IMPORTANT: check does NOT require that apply_run orphanApplyID
//     exists — orphanApplyID row may not exist at all: deleted by
//     retention-purge `purge_apply_runs` (migration 021, removes TERMINAL
//     apply_runs older than 30d) OR previous owner crashed BEFORE Insert(apply_runs)
//     (lockRun already committed applying, apply_run not yet inserted). incarnation-
//     lock — unclaimed flag (no owner/attempt/lease) — remains in both
//     cases; binding to orphan given by voyage_targets back-link on caller side,
//     not by apply_run presence. (NB: reaper-rule reclaim_apply_runs does NOT delete
//     orphaned row — it does claimed→planned reset of stale plans, not touching
//     dispatched/running/terminal; source of "row doesn't exist"
//     here — exactly purge-retention or pre-Insert-crash.)
//   - SINGLE-WINNER: guard `status='applying'` (CAS). If honest RunResult
//     of previous owner / concurrent finalization already moved row out of applying —
//     UPDATE gives RowsAffected==0 → [ErrOrphanLockNotReleased] (no-op, race
//     closed, we do NOT overwrite alien terminal).
//
// Atomicity: single transaction SELECT … FOR UPDATE → checks → INSERT
// state_history → CAS-UPDATE. FOR UPDATE serializes release relative to
// concurrent scenario-runner (its lockRun locks the same row).
//
// Returns:
//   - nil                        — lock released (applying → ready), re-run can
//     start.
//   - [ErrIncarnationNotFound]   — name doesn't exist (incarnation deleted between
//     reclaim and release).
//   - [ErrOrphanLockNotReleased] — nothing to release (not applying / orphan apply_id
//     not ours): caller continues re-run without release.
func ReleaseApplyingOrphan(ctx context.Context, pool TxBeginner, name, orphanApplyID, historyID string) error {
	if !ValidName(name) {
		return fmt.Errorf("incarnation: invalid name %q", name)
	}
	if orphanApplyID == "" {
		return fmt.Errorf("incarnation: empty orphan apply_id")
	}
	if historyID == "" {
		return fmt.Errorf("incarnation: empty history_id")
	}

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("incarnation: begin orphan-release tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	const selectForUpdateSQL = `
SELECT state, status
FROM incarnation
WHERE name = $1
FOR UPDATE
`
	var (
		stateBytes []byte
		statusStr  string
	)
	if err := tx.QueryRow(ctx, selectForUpdateSQL, name).Scan(&stateBytes, &statusStr); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrIncarnationNotFound
		}
		return fmt.Errorf("incarnation: orphan-release select: %w", err)
	}
	// SINGLE-WINNER fast-path: not in applying — nothing to release (honest RunResult
	// of previous owner / concurrent finalization already won the row). FOR UPDATE
	// already serialized us — status is authoritative in this tx.
	if Status(statusStr) != StatusApplying {
		return ErrOrphanLockNotReleased
	}

	// FENCING-1 (no-live-rival): incarnation MUST NOT have active
	// (non-terminal) apply_run of alien run (apply_id ≠ orphanApplyID).
	// If exists — between crash and reclaim a live run started (direct run /
	// other Voyage), its applying-lock is NOT our orphan → do NOT release. Terminal
	// statuses (success/failed/cancelled/orphaned/no_match) are not held by live run —
	// ignore. Missing orphanApplyID row is acceptable: deleted by
	// retention-purge purge_apply_runs (terminal >30d) OR previous owner
	// crashed BEFORE Insert(apply_runs); lock — unclaimed flag — remains.
	const liveRivalSQL = `
SELECT EXISTS (
    SELECT 1 FROM apply_runs
    WHERE incarnation_name = $1
      AND apply_id <> $2
      AND status NOT IN ('success', 'failed', 'cancelled', 'orphaned', 'no_match')
)
`
	var hasRival bool
	if err := tx.QueryRow(ctx, liveRivalSQL, name, orphanApplyID).Scan(&hasRival); err != nil {
		return fmt.Errorf("incarnation: orphan-release live-rival check: %w", err)
	}
	if hasRival {
		return ErrOrphanLockNotReleased
	}

	// state_before == state_after: releasing orphan-lock does not change state (orphan-
	// run didn't reach commit — state remains pre-run last-good).
	// apply_id = orphanApplyID: transition snapshot correlates with orphaned
	// run (schema requires non-null apply_id).
	const historyInsertSQL = `
INSERT INTO state_history (
    history_id, incarnation_name, scenario, state_before, state_after,
    changed_by_aid, apply_id
) VALUES ($1, $2, $3, $4, $4, NULL, $5)
`
	if _, err := tx.Exec(ctx, historyInsertSQL,
		historyID, name, orphanReleaseScenarioLabel, stateBytes, orphanApplyID,
	); err != nil {
		return fmt.Errorf("incarnation: insert orphan-release state_history: %w", err)
	}

	// SINGLE-WINNER CAS: applying → ready. RowsAffected==0 is impossible under
	// FOR UPDATE+early status check (we already saw applying in this tx), but
	// keep guard as explicit transition invariant. Epoch of applying-flag (ADR-027
	// amend (m-S1)) is zeroed together with release of applying — symmetric to
	// UpdateStateFromRun terminal: after orphan-lock release row must not carry
	// dead owner's epoch (otherwise Reaper would see stale applying_by_kid on
	// already-released row). Covers both release paths — Voyage (l) and standalone (m).
	const updateSQL = `
UPDATE incarnation
SET status            = $2,
    status_details    = NULL,
    applying_apply_id = NULL,
    applying_attempt  = NULL,
    applying_by_kid   = NULL,
    applying_since    = NULL,
    updated_at        = NOW()
WHERE name = $1 AND status = 'applying'
`
	tag, err := tx.Exec(ctx, updateSQL, name, string(StatusReady))
	if err != nil {
		return fmt.Errorf("incarnation: orphan-release update: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrOrphanLockNotReleased
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("incarnation: commit orphan-release tx: %w", err)
	}
	return nil
}

// rerunLastScenarioLabel — value for `state_history.scenario` on unlock portion
// of rerun-last. By unlock-transition convention (state_history requires non-null
// scenario): capture the fact of error_locked release under this label, separate
// from unlockScenarioLabel — rerun releases block AND restarts the last failed
// scenario, its trace in history differs from normal manual unlock.
const rerunLastScenarioLabel = "rerun-last"

// UnlockForRerun — unlock portion of rerun-last (architecture.md → "Atomicity and
// error_locked"). Atomically releases error_locked and transitions incarnation
// error_locked → applying BYPASSING ready: window where concurrent run could slip
// into freed ready does not occur (transition under single FOR UPDATE). State NOT
// touched — last known-good preserved (snapshot in state_history, state_before == state_after,
// symmetric to [Unlock]).
//
// Admission STRICTLY from error_locked: migration_failed / destroy_failed / ready /
// applying / destroying → [ErrIncarnationNotErrorLocked] (for them — normal
// unlock + manual run).
//
// Scope=last-failed: restarts LAST FAILED scenario of incarnation (last
// state_history snapshot: run.go::abort → lockIncarnation →
// UpdateStateFromRun writes failed scenario name and its apply_id). This can be
// bootstrap creator (`create`/`create_cluster`/…) OR day-2-scenario
// (add_user / update_acl / …) — both restarted identically.
//
// Recovery of failed run's input (so restart proceeds with SAME
// values, not defaults):
//   - create-path (last failed == incarnation.created_scenario): input from
//     incarnation.spec.input, read under same FOR UPDATE (lives with incarnation).
//   - day-2-path (else, including bare-incarnation with created_scenario IS NULL):
//     input from recipe of failed apply_run (`apply_runs.recipe.input` by apply_id
//     of last snapshot; invariant A — vault-ref as strings, secrets not
//     revealed). Recipe unavailable → fail-closed [ErrRerunInputUnavailable]
//     (reasons and semantics — see sentinel), transaction NOT committed.
//
// Caller (handler / MCP-tool) AFTER successful commit starts
// [UnlockResult.Scenario] via runner.Start with same applyID passed here:
// status already applying, lockRun of starting run locks same row and sees
// applying as valid start status. Passing applyID here needed for write to
// state_history.apply_id — unlock-transition snapshot correlates with run being started.
//
// Atomicity: single transaction SELECT … FOR UPDATE → gate error_locked →
// last-failed probe → (day-2) recipe probe → INSERT state_history →
// UPDATE status=applying → commit. FOR UPDATE serializes rerun relative to
// concurrent scenario-runner (its lockRun locks same row).
//
// Returns:
//   - [ErrIncarnationNotFound]       — name doesn't exist (404).
//   - [ErrIncarnationNotErrorLocked] — status not error_locked (409).
//   - [ErrRerunInputUnavailable]     — day-2-path but failed run's input
//     unavailable (recipe IS NULL: early abort without recipe / legacy / apply_run
//     purged — full list at sentinel) (409).
//
// reason written to audit-payload by caller (state_history-schema MVP doesn't carry
// metadata-columns); previous_status returned in [UnlockResult].
func UnlockForRerun(ctx context.Context, pool TxBeginner, name, reason, rerunByAID, historyID, applyID string) (*UnlockResult, error) {
	if !ValidName(name) {
		return nil, fmt.Errorf("incarnation: invalid name %q", name)
	}
	if reason == "" {
		return nil, fmt.Errorf("incarnation: rerun-last reason is empty")
	}
	if historyID == "" {
		return nil, fmt.Errorf("incarnation: empty history_id")
	}
	if applyID == "" {
		return nil, fmt.Errorf("incarnation: empty apply_id")
	}

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("incarnation: begin rerun-last tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// read spec under same FOR UPDATE-snapshot (column at end — order of state,
	// status, created_scenario NOT shifted, plain Unlock/Destroy scan first
	// two, as before): on create-path extract saved operator-
	// input from spec.input for passing to restarted bootstrap-run (rerun-last
	// recovers failure with same version/shards/…, not defaults).
	const selectForUpdateSQL = `
SELECT state, status, created_scenario, spec
FROM incarnation
WHERE name = $1
FOR UPDATE
`
	var (
		stateBytes      []byte
		statusStr       string
		createdScenario *string
		specBytes       []byte
	)
	if err := tx.QueryRow(ctx, selectForUpdateSQL, name).Scan(&stateBytes, &statusStr, &createdScenario, &specBytes); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrIncarnationNotFound
		}
		return nil, fmt.Errorf("incarnation: rerun-last select: %w", err)
	}
	previous := Status(statusStr)
	if previous != StatusErrorLocked {
		return nil, ErrIncarnationNotErrorLocked
	}

	// Scope=last-failed: restart LAST FAILED scenario of incarnation.
	// Last state_history snapshot carries failed scenario name AND apply_id of that
	// run (run.go::abort → lockIncarnation → UpdateStateFromRun). apply_id —
	// authoritative correlation with recipe (day-2 input), more precise than matching by
	// scenario name. Same FOR UPDATE-tx: read serialized relative to
	// concurrent scenario-runner.
	const lastRunSQL = `
SELECT scenario, apply_id
FROM state_history
WHERE incarnation_name = $1
ORDER BY history_id DESC
LIMIT 1
`
	var (
		lastScenario string
		lastApplyID  string
	)
	if err := tx.QueryRow(ctx, lastRunSQL, name).Scan(&lastScenario, &lastApplyID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// error_locked without single snapshot — unachievable normally (lockIncarnation
			// always writes state_history on failure). Fail-closed: without trace of failed
			// run nothing to restart.
			return nil, ErrRerunInputUnavailable
		}
		return nil, fmt.Errorf("incarnation: rerun-last last-run probe: %w", err)
	}

	// Recovery of failed run's input: create-path vs day-2-path.
	// fromUpgrade — day-2 only (recipe.from_upgrade); create-path always false.
	var (
		rerunInput  map[string]any
		fromUpgrade bool
	)
	if createdScenario != nil && lastScenario == *createdScenario {
		// create-path: last failed == bootstrap creator. Operator input
		// stored in incarnation.spec.input (read under same FOR UPDATE):
		// null/malformed spec jsonb — not consistency error (spec freeform;
		// unmarshal returns nil-map), input — nil if key absent.
		spec, _ := unmarshalJSONB(specBytes)
		rerunInput = InputFromSpec(spec)
	} else {
		// day-2-path (including bare-incarnation, created_scenario IS NULL): failed
		// day-2-run input lives only in apply_run recipe. Read recipe by
		// apply_id of last snapshot (any passage/sid-row of run — recipe
		// one per run). Recipe unavailable → fail-closed
		// [ErrRerunInputUnavailable] (reasons — see sentinel), tx NOT committed.
		const recipeSQL = `
SELECT recipe
FROM apply_runs
WHERE apply_id = $1 AND recipe IS NOT NULL
LIMIT 1
`
		var recipeBytes []byte
		if err := tx.QueryRow(ctx, recipeSQL, lastApplyID).Scan(&recipeBytes); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, ErrRerunInputUnavailable
			}
			return nil, fmt.Errorf("incarnation: rerun-last recipe probe: %w", err)
		}
		recipe, uerr := unmarshalJSONB(recipeBytes)
		if uerr != nil {
			// Malformed recipe-jsonb — inconsistent with invariant A (recipe written
			// by json.Marshal). Fail-closed, not silent bootstrap-input.
			return nil, ErrRerunInputUnavailable
		}
		rerunInput = InputFromSpec(recipe)
		// recipe.FromUpgrade as freeform-jsonb string (like InputFromSpec): upgrade-
		// run restarts from upgrade/, not scenario/ (ADR-0068).
		fromUpgrade, _ = recipe["from_upgrade"].(bool)
	}

	var changedByArg any
	if rerunByAID != "" {
		changedByArg = rerunByAID
	}

	// state_before == state_after: rerun does not change state (last known-good).
	// apply_id = $6: unlock-transition snapshot correlates with run being started
	// (same applyID used by runner.Start in caller).
	const historyInsertSQL = `
INSERT INTO state_history (
    history_id, incarnation_name, scenario, state_before, state_after,
    changed_by_aid, apply_id
) VALUES ($1, $2, $3, $4, $4, $5, $6)
`
	if _, err := tx.Exec(ctx, historyInsertSQL,
		historyID, name, rerunLastScenarioLabel, stateBytes, changedByArg, applyID,
	); err != nil {
		return nil, fmt.Errorf("incarnation: insert rerun-last state_history: %w", err)
	}

	// status → applying (BYPASSING ready), status_details reset (block released).
	// Concurrent run can't slip through: ready never materializes, and
	// FOR UPDATE holds row until commit.
	const updateSQL = `
UPDATE incarnation
SET status = $2, status_details = NULL, updated_at = NOW()
WHERE name = $1
`
	if _, err := tx.Exec(ctx, updateSQL, name, string(StatusApplying)); err != nil {
		return nil, fmt.Errorf("incarnation: rerun-last update: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("incarnation: commit rerun-last tx: %w", err)
	}

	return &UnlockResult{
		PreviousStatus: previous,
		HistoryID:      historyID,
		Scenario:       lastScenario,
		Input:          rerunInput,
		FromUpgrade:    fromUpgrade,
	}, nil
}

// migrationScenarioLabel — value for `state_history.scenario` on state_schema-
// migration step (docs/migrations.md §Atomicity: scenario="migration").
// Migration — not scenario run through Soul, but state_history requires non-null
// scenario; capture each chain step under this label.
const migrationScenarioLabel = "migration"

// upgradeDriftScenarioLabel — value for `state_history.scenario` on upgrade's
// final transition to drift (ADR-031, amendment to upgrade behavior). Upgrade
// changes only incarnation.state + version in single PG-tx — hosts remain on
// OLD deployment, actual state diverges from new state. Separate label
// (not `migration`, under which migration steps go) captures REASON
// of transition to drift in history — "new version awaits deployment on hosts", so
// triage distinguishes upgrade-drift from drift found by Scry-scan. Symmetric
// to other transition-labels (unlock / rerun-last / voyage-orphan-release).
const upgradeDriftScenarioLabel = "upgrade-pending-apply"

// UpgradeInput — input to [UpgradeStateSchema]. Caller (Slice 2) resolves
// TargetSchemaVer from service.yml of target snapshot, collects Chain via
// [artifact.ServiceLoader.LoadMigrationChain], and generates ApplyID (ULID of single
// upgrade operation, common to all chain steps).
type UpgradeInput struct {
	Name             string
	TargetServiceVer string                 // git-ref of target service version (ADR-007)
	TargetSchemaVer  int                    // state_schema_version from service.yml snapshot
	Chain            statemigrate.Chain     // chain current→target (empty = no-op ref-bump)
	Evaluator        statemigrate.Evaluator // migration-CEL ([statemigrate.NewEvaluator])
	ApplyID          string                 // ULID of upgrade operation (M, common to migration chain)
	ChangedByAID     *string                // Archon initiator (nil — no identity)

	// UpgradeSlug — name of found upgrade-scenario for from→to transition (ADR-0068
	// §5). Filled by [PrepareUpgrade] (scan upgrade/ of target snapshot); empty →
	// legacy (drift + host deployment manual).
	UpgradeSlug string
	// RunApplyID — ULID of Runner-run of upgrade-scenario (R), SEPARATE from ApplyID
	// of migration (M). Found-mode (applying + linkage-snapshot under R) enabled ONLY
	// when BOTH UpgradeSlug AND RunApplyID non-empty: R — caller's commit to auto-start.
	// Caller without auto-start (MCP) leaves empty → legacy-drift, to not
	// reserve applying without run (else incarnation would hang). ADR-0068 §5/B.
	RunApplyID string
	// TargetRef — git-coordinates of target version (Ref=to_version), resolved
	// by [PrepareUpgrade]. For runner.Start(ServiceRef) auto-start of upgrade-scenario.
	TargetRef artifact.ServiceRef
}

// UpgradeResult — result of [UpgradeStateSchema]: schema before/after and count
// of applied migration steps (0 for no-op ref-bump).
type UpgradeResult struct {
	FromSchemaVer int
	ToSchemaVer   int
	Steps         int
}

// UpgradeStateSchema atomically applies state_schema-migration to incarnation
// (ADR-019, docs/migrations.md §Atomicity). Same transactional pattern as
// [Unlock] / scenario.lockRun: single tx FOR UPDATE → status gate → sanity
// against chain → in-memory [statemigrate.Apply] → per-step INSERT state_history
// → UPDATE incarnation → commit.
//
// Upgrade allowed ONLY from ready:
//   - applying        → [ErrIncarnationBusy] (run in progress);
//   - error_locked    → [ErrIncarnationLocked] (unlock needed);
//   - migration_failed → [ErrIncarnationLocked] (unlock needed).
//
// Sanity against chain (protection from resolve↔FOR UPDATE race):
//   - current schema-version must equal chain[0].FromVersion;
//   - TargetSchemaVer must equal chain[last].ToVersion;
//     else [ErrSchemaVersionMismatch] (someone upgraded between resolve and
//     row lock).
//   - TargetSchemaVer < current → [ErrDowngradeUnsupported] (forward-only).
//
// No-op (empty Chain — ref changed but schema_version same): [Apply]
// returns FinalState = state copy and empty Steps; still writes single
// zero-diff state_history-record (symmetric to unlock) and UPDATE service_version.
//
// On successful upgrade incarnation transitions to status=drift, NOT ready (ADR-031,
// amendment to upgrade behavior): DB-state updated, but hosts remain on old
// deployment — actual state diverges from new state, operator needs signal
// to "deploy new version". drift informational, not blocking (ADR-031(d));
// remediation — normal apply (drift→ready). Transition captured by separate
// zero-diff history-record with reason ([writeUpgradeDriftHistory]). Also
// applies to no-op ref-bump (git-ref change without migration can also change
// deployment — e.g., templates/packages of same schema-version).
//
// On error in [Apply] / any write tx is rolled back; then SEPARATE
// background-tx marks incarnation status=migration_failed with masked
// status_details (pattern from scenario.lockIncarnation; migration failure →
// migration_failed, NOT error_locked, ADR-019). Error in such marking
// wrapped in return, but primary cause preserved via %w.
//
// Returns [ErrIncarnationNotFound] if row doesn't exist.
func UpgradeStateSchema(ctx context.Context, pool TxBeginner, in UpgradeInput) (*UpgradeResult, error) {
	if !ValidName(in.Name) {
		return nil, fmt.Errorf("incarnation: invalid name %q", in.Name)
	}
	if in.TargetServiceVer == "" {
		return nil, fmt.Errorf("incarnation: empty target service_version")
	}
	if in.TargetSchemaVer <= 0 {
		return nil, fmt.Errorf("incarnation: target state_schema_version must be > 0, got %d", in.TargetSchemaVer)
	}
	if in.Evaluator == nil {
		return nil, fmt.Errorf("incarnation: nil evaluator")
	}
	if in.ApplyID == "" {
		return nil, fmt.Errorf("incarnation: empty apply_id")
	}

	res, err := upgradeTx(ctx, pool, in)
	if err == nil {
		return res, nil
	}
	// Sentinel rejections (gate / sanity / not-found) — NOT migration_failed:
	// state untouched, incarnation remains in original status.
	if isUpgradeRejection(err) {
		return nil, err
	}
	// Apply / write failure inside tx (rollback already done) → mark
	// migration_failed with separate background-tx.
	markErr := markMigrationFailed(pool, in, err)
	if markErr != nil {
		return nil, fmt.Errorf("incarnation: upgrade failed (%w); marking migration_failed failed: %v", err, markErr)
	}
	return nil, err
}

// isUpgradeRejection — true for sentinel rejections that do NOT transition
// incarnation to migration_failed (state unchanged, status preserved).
func isUpgradeRejection(err error) bool {
	return errors.Is(err, ErrIncarnationNotFound) ||
		errors.Is(err, ErrIncarnationBusy) ||
		errors.Is(err, ErrIncarnationLocked) ||
		errors.Is(err, ErrDowngradeUnsupported) ||
		errors.Is(err, ErrSchemaVersionMismatch)
}

// upgradeTx performs all upgrade-logic in single PG-transaction. Separated from
// [UpgradeStateSchema] so failure-handling (migration_failed) lives outside,
// after guaranteed rollback (defer Rollback in this function).
func upgradeTx(ctx context.Context, pool TxBeginner, in UpgradeInput) (*UpgradeResult, error) {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("incarnation: begin upgrade tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	const selectForUpdateSQL = `
SELECT state, state_schema_version, status
FROM incarnation
WHERE name = $1
FOR UPDATE
`
	var (
		stateBytes []byte
		currentVer int
		statusStr  string
	)
	if err := tx.QueryRow(ctx, selectForUpdateSQL, in.Name).Scan(&stateBytes, &currentVer, &statusStr); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrIncarnationNotFound
		}
		return nil, fmt.Errorf("incarnation: upgrade select: %w", err)
	}

	switch Status(statusStr) {
	case StatusReady, StatusDrift:
		// ready — normal path; drift (ADR-031, Scry) — informational status,
		// upgrade does NOT block it (same as normal apply). After upgrade-tx
		// status — back to drift (hosts await new version deployment, see
		// final UPDATE), not ready: DB-state change not yet deployed to hosts.
	case StatusApplying:
		return nil, ErrIncarnationBusy
	case StatusErrorLocked, StatusMigrationFailed:
		return nil, ErrIncarnationLocked
	default:
		return nil, fmt.Errorf("incarnation: upgrade from unknown status %q", statusStr)
	}

	if in.TargetSchemaVer < currentVer {
		return nil, ErrDowngradeUnsupported
	}

	// Sanity against chain (only for non-empty chain): protection from race
	// between resolve(service.yml snapshot) ↔ FOR UPDATE — someone could have upgraded
	// row between these points.
	if len(in.Chain) > 0 {
		if in.Chain[0].FromVersion != currentVer || in.Chain[len(in.Chain)-1].ToVersion != in.TargetSchemaVer {
			return nil, ErrSchemaVersionMismatch
		}
	} else if in.TargetSchemaVer != currentVer {
		// Empty chain must be ref-bump without schema-version change.
		return nil, ErrSchemaVersionMismatch
	}

	currentState, err := unmarshalJSONB(stateBytes)
	if err != nil {
		return nil, fmt.Errorf("incarnation: upgrade unmarshal state: %w", err)
	}

	applyRes, err := statemigrate.Apply(ctx, currentState, in.Chain, in.Evaluator)
	if err != nil {
		return nil, fmt.Errorf("incarnation: migration %q: %w", in.Name, err)
	}

	if err := writeMigrationHistory(ctx, tx, in, currentState, applyRes); err != nil {
		return nil, err
	}

	finalBytes, err := marshalJSONB(applyRes.FinalState)
	if err != nil {
		return nil, fmt.Errorf("incarnation: marshal migrated state: %w", err)
	}

	// found-branch (ADR-0068 §5/B): both slug AND R non-empty (R = caller's commit to
	// auto-start; without R can't reserve applying — incarnation would hang without
	// run, so caller without auto-start, e.g. MCP, goes legacy).
	found := in.UpgradeSlug != "" && in.RunApplyID != ""

	// legacy → drift (ADR-031: hosts behind DB-state, remediation = manual apply),
	// also no-op ref-bump; found → applying (ADR-0068: control goes to
	// Runner of upgrade-scenario auto-start, mirror of UnlockForRerun;
	// applying_apply_id=R filled by lockRun on FromLocked). status — controlled
	// constant (not user input), no Sprintf injection; legacy-text
	// byte-for-byte same (regression tests on literal 'drift'). Gate above allows
	// UPDATE only from ready/drift — both validly transition to both drift and applying.
	finalStatus := StatusDrift
	if found {
		finalStatus = StatusApplying
	}
	updateSQL := fmt.Sprintf(`
UPDATE incarnation
SET state                = $2,
    state_schema_version = $3,
    service_version      = $4,
    status               = '%s',
    status_details       = NULL,
    updated_at           = NOW()
WHERE name = $1
`, finalStatus)
	if _, err := tx.Exec(ctx, updateSQL, in.Name, finalBytes, in.TargetSchemaVer, in.TargetServiceVer); err != nil {
		return nil, fmt.Errorf("incarnation: upgrade update: %w", err)
	}

	// Zero-diff transition-record (state_before == state_after = post-migration
	// state): legacy → drift-transition under M (label upgrade-pending-apply,
	// distinguishes upgrade-drift from Scry-drift on triage); found → linkage-snapshot under
	// R with scenario=slug (mirror of UnlockForRerun-snapshot — links auto-start run
	// with this upgrade).
	if found {
		if err := writeUpgradeRunHistory(ctx, tx, in, applyRes.FinalState); err != nil {
			return nil, err
		}
	} else if err := writeUpgradeDriftHistory(ctx, tx, in, applyRes.FinalState); err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("incarnation: commit upgrade tx: %w", err)
	}

	return &UpgradeResult{
		FromSchemaVer: currentVer,
		ToSchemaVer:   in.TargetSchemaVer,
		Steps:         len(applyRes.Steps),
	}, nil
}

// writeUpgradeDriftHistory writes zero-diff snapshot of incarnation transition to drift
// following upgrade completion (state_before == state_after = post-migration final state).
// scenario label [upgradeDriftScenarioLabel] captures the transition reason (hosts await
// new version deployment); common ApplyID links it to step-snapshots of this upgrade.
func writeUpgradeDriftHistory(ctx context.Context, tx ExecQueryRower, in UpgradeInput, finalState map[string]any) error {
	stateBytes, err := marshalJSONB(finalState)
	if err != nil {
		return fmt.Errorf("incarnation: marshal state (drift-transition history): %w", err)
	}
	var changedByArg any
	if in.ChangedByAID != nil {
		changedByArg = *in.ChangedByAID
	}
	const historyInsertSQL = `
INSERT INTO state_history (
    history_id, incarnation_name, scenario, state_before, state_after,
    changed_by_aid, apply_id
) VALUES ($1, $2, $3, $4, $4, $5, $6)
`
	if _, err := tx.Exec(ctx, historyInsertSQL,
		audit.NewULID(), in.Name, upgradeDriftScenarioLabel, stateBytes, changedByArg, in.ApplyID,
	); err != nil {
		return fmt.Errorf("incarnation: insert drift-transition state_history: %w", err)
	}
	return nil
}

// writeUpgradeRunHistory writes zero-diff linkage-snapshot of control handoff to
// upgrade-scenario auto-start (found-branch ADR-0068 §5/B): apply_id = R
// ([UpgradeInput.RunApplyID]), scenario = discovered slug, state_before ==
// state_after = post-migration final state. Mirror of UnlockForRerun-snapshot
// (applying + zero-diff under run's apply_id) — links R to this upgrade in history,
// separate from M-labels of the migration itself.
func writeUpgradeRunHistory(ctx context.Context, tx ExecQueryRower, in UpgradeInput, finalState map[string]any) error {
	stateBytes, err := marshalJSONB(finalState)
	if err != nil {
		return fmt.Errorf("incarnation: marshal state (upgrade-run history): %w", err)
	}
	var changedByArg any
	if in.ChangedByAID != nil {
		changedByArg = *in.ChangedByAID
	}
	const historyInsertSQL = `
INSERT INTO state_history (
    history_id, incarnation_name, scenario, state_before, state_after,
    changed_by_aid, apply_id
) VALUES ($1, $2, $3, $4, $4, $5, $6)
`
	if _, err := tx.Exec(ctx, historyInsertSQL,
		audit.NewULID(), in.Name, in.UpgradeSlug, stateBytes, changedByArg, in.RunApplyID,
	); err != nil {
		return fmt.Errorf("incarnation: insert upgrade-run state_history: %w", err)
	}
	return nil
}

// writeMigrationHistory writes per-step snapshot of the chain to state_history.
// One common ApplyID per upgrade, distinct history_id (ULID) per step. For no-op
// (empty chain), writes one zero-diff record (state_before == state_after),
// symmetric to unlock — captures the fact of the ref-bump itself.
func writeMigrationHistory(ctx context.Context, tx ExecQueryRower, in UpgradeInput, before map[string]any, res statemigrate.Result) error {
	const historyInsertSQL = `
INSERT INTO state_history (
    history_id, incarnation_name, scenario, state_before, state_after,
    changed_by_aid, apply_id
) VALUES ($1, $2, $3, $4, $5, $6, $7)
`
	var changedByArg any
	if in.ChangedByAID != nil {
		changedByArg = *in.ChangedByAID
	}

	if len(res.Steps) == 0 {
		// No-op ref-bump: zero-diff snapshot (state unchanged).
		stateBytes, err := marshalJSONB(before)
		if err != nil {
			return fmt.Errorf("incarnation: marshal state (no-op history): %w", err)
		}
		if _, err := tx.Exec(ctx, historyInsertSQL,
			audit.NewULID(), in.Name, migrationScenarioLabel, stateBytes, stateBytes, changedByArg, in.ApplyID,
		); err != nil {
			return fmt.Errorf("incarnation: insert migration state_history (no-op): %w", err)
		}
		return nil
	}

	for i := range res.Steps {
		beforeBytes, err := marshalJSONB(res.Steps[i].StateBefore)
		if err != nil {
			return fmt.Errorf("incarnation: marshal step state_before: %w", err)
		}
		afterBytes, err := marshalJSONB(res.Steps[i].StateAfter)
		if err != nil {
			return fmt.Errorf("incarnation: marshal step state_after: %w", err)
		}
		if _, err := tx.Exec(ctx, historyInsertSQL,
			audit.NewULID(), in.Name, migrationScenarioLabel, beforeBytes, afterBytes, changedByArg, in.ApplyID,
		); err != nil {
			return fmt.Errorf("incarnation: insert migration state_history (step %d): %w", i, err)
		}
	}
	return nil
}

// markMigrationFailed marks incarnation status=migration_failed after
// migration failure. Separate background-tx (pattern from scenario.lockIncarnation):
// original ctx may have been cancelled, but recording failure must happen regardless.
// status_details masked via [audit.MaskSecrets] (migration-CEL has no vault,
// but cause may carry vault-refs from old state in transit — defense in depth,
// status_details read outbound without masking).
func markMigrationFailed(pool TxBeginner, in UpgradeInput, cause error) error {
	wctx := context.Background()
	details := audit.MaskSecrets(map[string]any{
		"reason":   "migration_failed",
		"apply_id": in.ApplyID,
		"error":    cause.Error(),
	})
	detailsBytes, err := json.Marshal(details)
	if err != nil {
		return fmt.Errorf("incarnation: marshal migration_failed details: %w", err)
	}

	tx, err := pool.BeginTx(wctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("incarnation: begin migration_failed tx: %w", err)
	}
	defer func() { _ = tx.Rollback(wctx) }()

	const updateSQL = `
UPDATE incarnation
SET status = 'migration_failed', status_details = $2, updated_at = NOW()
WHERE name = $1
`
	if _, err := tx.Exec(wctx, updateSQL, in.Name, detailsBytes); err != nil {
		return fmt.Errorf("incarnation: migration_failed update: %w", err)
	}
	if err := tx.Commit(wctx); err != nil {
		return fmt.Errorf("incarnation: commit migration_failed tx: %w", err)
	}
	return nil
}

// HistoryFilter — filters for [HistorySelectByName]. Empty fields mean "don't filter".
// ApplyID — ULID of specific run; typically matches 0 or 1 row (one state_history-snapshot
// per apply), but contract doesn't forbid multiple on future extension.
//
// IncludeArchived — flag to include soft-deleted snapshots (ADR-Q19 retention,
// `state_history.archived_at` column). Default false: history registry returns only
// active snapshots (`archived_at IS NULL`) — typical Operator API / MCP scenario.
// When true, returns all records (including those marked by Reaper `archive_state_history` rule)
// — needed to investigate "where did snapshot N days ago go".
type HistoryFilter struct {
	ApplyID         string
	IncludeArchived bool
}

// HistorySelectByName returns a page of `state_history` records for a specific
// incarnation in reverse chronological order + total count (without offset/limit).
// When `filter.ApplyID` is non-empty, result is additionally filtered by `apply_id`.
//
// By default (filter.IncludeArchived = false), only active snapshots are returned
// (`archived_at IS NULL`, ADR-Q19 retention). Total is also counted over active set —
// Operator API pagination doesn't "jump" through soft-deleted gaps.
//
// Return ([], 0, nil) for non-existent incarnation — no need to check existence
// with separate query: caller (handler) should first call [SelectByName] to return 404,
// or accept empty history as valid for an existing incarnation.
func HistorySelectByName(ctx context.Context, db ExecQueryRower, name string, filter HistoryFilter, offset, limit int) ([]*HistoryEntry, int, error) {
	if offset < 0 {
		return nil, 0, fmt.Errorf("incarnation: history offset must be >= 0, got %d", offset)
	}
	if limit < 1 {
		return nil, 0, fmt.Errorf("incarnation: history limit must be >= 1, got %d", limit)
	}

	args := []any{name}
	where := "WHERE incarnation_name = $1"
	if !filter.IncludeArchived {
		where += " AND archived_at IS NULL"
	}
	if filter.ApplyID != "" {
		args = append(args, filter.ApplyID)
		where += fmt.Sprintf(" AND apply_id = $%d", len(args))
	}

	countSQL := "SELECT COUNT(*) FROM state_history " + where
	var total int
	if err := db.QueryRow(ctx, countSQL, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("incarnation: history count: %w", err)
	}

	listSQL := `SELECT history_id, scenario, state_before, state_after,
       changed_by_aid, apply_id, at
FROM state_history
` + where +
		fmt.Sprintf(`
ORDER BY at DESC, history_id ASC
OFFSET $%d LIMIT $%d`, len(args)+1, len(args)+2)
	listArgs := append(append([]any{}, args...), offset, limit)

	rows, err := db.Query(ctx, listSQL, listArgs...)
	if err != nil {
		return nil, 0, fmt.Errorf("incarnation: history list query: %w", err)
	}
	defer rows.Close()

	var out []*HistoryEntry
	for rows.Next() {
		entry, err := scanHistoryEntry(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("incarnation: history list iter: %w", err)
	}
	return out, total, nil
}

func scanHistoryEntry(row pgx.Row) (*HistoryEntry, error) {
	var (
		entry            HistoryEntry
		stateBeforeBytes []byte
		stateAfterBytes  []byte
		changedByAID     *string
	)
	err := row.Scan(
		&entry.HistoryID,
		&entry.Scenario,
		&stateBeforeBytes,
		&stateAfterBytes,
		&changedByAID,
		&entry.ApplyID,
		&entry.At,
	)
	if err != nil {
		return nil, fmt.Errorf("incarnation: history scan: %w", err)
	}
	entry.ChangedByAID = changedByAID
	if entry.StateBefore, err = unmarshalJSONB(stateBeforeBytes); err != nil {
		return nil, fmt.Errorf("incarnation: history unmarshal state_before: %w", err)
	}
	if entry.StateAfter, err = unmarshalJSONB(stateAfterBytes); err != nil {
		return nil, fmt.Errorf("incarnation: history unmarshal state_after: %w", err)
	}
	return &entry, nil
}

// ValidStatus — closed enum check for status. Mirrors CHECK
// incarnation_status_valid from migration (005 + 031 + 036 + 047) to reject
// invalid status in Go before round-trip to DB. Exported for list-filter in handler layer.
func ValidStatus(s Status) bool {
	switch s {
	case StatusReady, StatusApplying, StatusErrorLocked, StatusMigrationFailed,
		StatusDestroying, StatusDestroyFailed, StatusDrift:
		return true
	}
	return false
}

// marshalJSONB serializes map to bytes for JSONB column. nil → `{}`,
// symmetric with shared/audit and operator.marshalMetadata.
func marshalJSONB(m map[string]any) ([]byte, error) {
	if m == nil {
		return []byte("{}"), nil
	}
	return json.Marshal(m)
}

// unmarshalJSONB parses JSONB bytes into map. Empty bytes / `null` → nil-map.
func unmarshalJSONB(b []byte) (map[string]any, error) {
	if len(b) == 0 {
		return nil, nil
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return m, nil
}
