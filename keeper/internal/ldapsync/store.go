package ldapsync

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/serviceregistry"
)

// Outcome is what the last attempt for one operator ended with — the
// `ldap_sync_operators.outcome` domain.
type Outcome string

const (
	// OutcomeSynced — the groups were read and the roles reconciled to them.
	OutcomeSynced Outcome = "synced"
	// OutcomeNotFound — the directory holds no entry for the AID; the group
	// roles were dropped, the operator was not revoked.
	OutcomeNotFound Outcome = "not_found"
	// OutcomeUnreachable — the groups could not be read: no connection, a
	// failed service bind, a search error or an ambiguous answer.
	OutcomeUnreachable Outcome = "unreachable"
	// OutcomeLockoutRefused — dropping roles (on an answer, or after the
	// second failed read) would have emptied the cluster's admin set
	// (NIM-320); nothing was changed.
	OutcomeLockoutRefused Outcome = "lockout_refused"
	// OutcomeGuarded — the pass would have stripped every operator holding
	// group roles of all of them, which is read as a broken lookup rather than
	// as everyone having left; nothing was changed.
	OutcomeGuarded Outcome = "guarded"
	// OutcomeReconcileFailed — the directory answered but the roles could not
	// be written; retried on the next pass.
	OutcomeReconcileFailed Outcome = "reconcile_failed"
)

// Outcomes lists the domain in a stable order.
func Outcomes() []Outcome {
	return []Outcome{OutcomeSynced, OutcomeNotFound, OutcomeUnreachable, OutcomeLockoutRefused, OutcomeGuarded, OutcomeReconcileFailed}
}

// DueOperator is an operator whose groups are read in this pass.
type DueOperator struct {
	AID string
	// FailedRuns — consecutive earlier attempts that could not read the groups.
	FailedRuns int
}

// Record is the result of one attempt for one operator.
type Record struct {
	AID     string
	Outcome Outcome
	// Confirmed — the sync accepted an answer from the directory for this
	// operator (its groups, or "no such entry"); moves confirmed_at. Not set
	// for a guarded pass: those answers were set aside as a broken lookup.
	Confirmed  bool
	FailedRuns int
	Error      string
}

// Status summarises the sync over the operators it covers today: active
// operators with `auth_method = ldap`.
type Status struct {
	// LastRunAt — the latest attempt for any of them.
	LastRunAt *time.Time
	// LastSuccessAt — the latest answer the sync accepted for any of them.
	LastSuccessAt *time.Time
	// Operators counts them by last outcome.
	Operators map[Outcome]int
	// Pending — operators not attempted yet.
	Pending int
}

// DB is the narrow pgx surface the store needs (a pool or a transaction).
type DB = serviceregistry.ExecQueryRower

// PGStore is the Postgres side of the sync: its settings row in
// `keeper_settings` and its per-operator state in `ldap_sync_operators`. The
// clock is the database's, so "an interval since the last attempt" is measured
// on one clock whichever instance leads.
type PGStore struct {
	DB DB
}

// LoadSettings reads the settings; set=false means the key is absent and the
// defaults apply.
func (s PGStore) LoadSettings(ctx context.Context) (Settings, bool, error) {
	row, err := serviceregistry.GetSetting(ctx, s.DB, SettingsKey)
	if errors.Is(err, serviceregistry.ErrSettingNotFound) {
		return DefaultSettings(), false, nil
	}
	if err != nil {
		return Settings{}, false, err
	}
	st, err := ParseSettings(row.Value)
	if err != nil {
		return Settings{}, true, err
	}
	return st, true, nil
}

// SaveSettings writes the settings on behalf of callerAID.
func (s PGStore) SaveSettings(ctx context.Context, st Settings, callerAID string) error {
	return serviceregistry.SetSetting(ctx, s.DB, &serviceregistry.Setting{
		Key:          SettingsKey,
		Value:        st.Encode(),
		UpdatedByAID: &callerAID,
	})
}

// dueOperatorsSQL is one clock for the whole cluster: the latest attempt for
// any active LDAP operator. A pass reads everyone, so everyone is in it and the
// all-strip guard sees the whole population, not one cohort.
const dueOperatorsSQL = `
WITH last AS (
    SELECT max(s.attempted_at) AS at
    FROM ldap_sync_operators s
    JOIN operators o ON o.aid = s.aid
    WHERE o.auth_method = 'ldap' AND o.revoked_at IS NULL
)
SELECT o.aid, COALESCE(s.failed_runs, 0)
FROM operators o
LEFT JOIN ldap_sync_operators s ON s.aid = o.aid
CROSS JOIN last
WHERE o.auth_method = 'ldap'
  AND o.revoked_at IS NULL
  AND (last.at IS NULL OR last.at <= NOW() - make_interval(secs => $1))
ORDER BY o.aid`

// DueOperators returns every active LDAP operator once the last pass is an
// interval old, and nobody before that.
func (s PGStore) DueOperators(ctx context.Context, interval time.Duration) ([]DueOperator, error) {
	rows, err := s.DB.Query(ctx, dueOperatorsSQL, interval.Seconds())
	if err != nil {
		return nil, fmt.Errorf("ldapsync: select due operators: %w", err)
	}
	defer rows.Close()
	var out []DueOperator
	for rows.Next() {
		var d DueOperator
		if err := rows.Scan(&d.AID, &d.FailedRuns); err != nil {
			return nil, fmt.Errorf("ldapsync: scan due operator: %w", err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ldapsync: select due operators: %w", err)
	}
	return out, nil
}

const recordSQL = `
INSERT INTO ldap_sync_operators (aid, attempted_at, confirmed_at, outcome, failed_runs, error)
VALUES ($1, NOW(), CASE WHEN $2 THEN NOW() END, $3, $4, $5)
ON CONFLICT (aid) DO UPDATE SET
    attempted_at = EXCLUDED.attempted_at,
    confirmed_at = COALESCE(EXCLUDED.confirmed_at, ldap_sync_operators.confirmed_at),
    outcome      = EXCLUDED.outcome,
    failed_runs  = EXCLUDED.failed_runs,
    error        = EXCLUDED.error`

// maxErrorLen mirrors the CHECK on `ldap_sync_operators.error`.
const maxErrorLen = 1000

// Record stores the result of one attempt.
func (s PGStore) Record(ctx context.Context, r Record) error {
	var errText *string
	if r.Error != "" {
		e := truncate(r.Error, maxErrorLen)
		errText = &e
	}
	if _, err := s.DB.Exec(ctx, recordSQL, r.AID, r.Confirmed, string(r.Outcome), r.FailedRuns, errText); err != nil {
		return fmt.Errorf("ldapsync: record %s: %w", r.AID, err)
	}
	return nil
}

const statusSQL = `
SELECT s.outcome, count(*), max(s.attempted_at), max(s.confirmed_at)
FROM operators o
LEFT JOIN ldap_sync_operators s ON s.aid = o.aid
WHERE o.auth_method = 'ldap' AND o.revoked_at IS NULL
GROUP BY s.outcome`

// Status reads the summary.
func (s PGStore) Status(ctx context.Context) (Status, error) {
	rows, err := s.DB.Query(ctx, statusSQL)
	if err != nil {
		return Status{}, fmt.Errorf("ldapsync: select status: %w", err)
	}
	defer rows.Close()
	st := Status{Operators: make(map[Outcome]int, len(Outcomes()))}
	for _, o := range Outcomes() {
		st.Operators[o] = 0
	}
	for rows.Next() {
		var (
			outcome            *string
			count              int
			attempted, confirm *time.Time
		)
		if err := rows.Scan(&outcome, &count, &attempted, &confirm); err != nil {
			return Status{}, fmt.Errorf("ldapsync: scan status: %w", err)
		}
		if outcome == nil {
			st.Pending = count
			continue
		}
		st.Operators[Outcome(*outcome)] = count
		st.LastRunAt = latest(st.LastRunAt, attempted)
		st.LastSuccessAt = latest(st.LastSuccessAt, confirm)
	}
	if err := rows.Err(); err != nil {
		return Status{}, fmt.Errorf("ldapsync: select status: %w", err)
	}
	return st, nil
}

func latest(a, b *time.Time) *time.Time {
	if b == nil || (a != nil && !b.After(*a)) {
		return a
	}
	t := b.UTC()
	return &t
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
